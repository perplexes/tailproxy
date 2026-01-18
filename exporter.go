package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"tailscale.com/tsnet"
)

// ExporterManager manages port exports over tsnet
type ExporterManager struct {
	config    *Config
	server    *tsnet.Server
	mu        sync.Mutex
	exporters map[int]*portExporter // port -> exporter
	ctx       context.Context
	cancel    context.CancelFunc
}

type portExporter struct {
	port      int
	listener  net.Listener
	refcount  int
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// NewExporterManager creates a new exporter manager
func NewExporterManager(config *Config, server *tsnet.Server) *ExporterManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &ExporterManager{
		config:    config,
		server:    server,
		exporters: make(map[int]*portExporter),
		ctx:       ctx,
		cancel:    cancel,
	}
}

// StartControlSocket starts the Unix socket control server
func (em *ExporterManager) StartControlSocket(socketPath string) error {
	// Remove existing socket if it exists
	os.Remove(socketPath)

	// Ensure directory exists
	dir := filepath.Dir(socketPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create control socket directory: %w", err)
	}

	// Create Unix domain socket listener
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("failed to create control socket: %w", err)
	}

	// Set permissions
	if err := os.Chmod(socketPath, 0600); err != nil {
		listener.Close()
		return fmt.Errorf("failed to set socket permissions: %w", err)
	}

	if em.config.Verbose {
		log.Printf("Control socket listening on %s", socketPath)
	}

	// Accept connections in background
	go func() {
		defer listener.Close()
		for {
			select {
			case <-em.ctx.Done():
				return
			default:
			}

			conn, err := listener.Accept()
			if err != nil {
				if em.ctx.Err() != nil {
					return
				}
				if em.config.Verbose {
					log.Printf("Control socket accept error: %v", err)
				}
				continue
			}

			go em.handleControlConnection(conn)
		}
	}()

	return nil
}

func (em *ExporterManager) handleControlConnection(conn net.Conn) {
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) < 3 {
			if em.config.Verbose {
				log.Printf("Invalid control message: %s", line)
			}
			continue
		}

		cmd := parts[0]
		// family := parts[1] // tcp4 or tcp6
		portStr := parts[2]

		port, err := strconv.Atoi(portStr)
		if err != nil {
			if em.config.Verbose {
				log.Printf("Invalid port in control message: %s", portStr)
			}
			continue
		}

		switch cmd {
		case "LISTEN":
			em.handleListen(port)
		case "CLOSE":
			em.handleClose(port)
		default:
			if em.config.Verbose {
				log.Printf("Unknown control command: %s", cmd)
			}
		}
	}
}

func (em *ExporterManager) handleListen(port int) {
	em.mu.Lock()
	defer em.mu.Unlock()

	// Check if port is allowed
	if !em.isPortAllowed(port) {
		if em.config.Verbose {
			log.Printf("Port %d not allowed by export policy", port)
		}
		return
	}

	// Check if already exported
	if exp, exists := em.exporters[port]; exists {
		exp.refcount++
		if em.config.Verbose {
			log.Printf("Port %d already exported, refcount now %d", port, exp.refcount)
		}
		return
	}

	// Check max exports
	if len(em.exporters) >= em.config.ExportMax {
		if em.config.Verbose {
			log.Printf("Cannot export port %d: max exports (%d) reached", port, em.config.ExportMax)
		}
		return
	}

	// Create new exporter
	if err := em.startExporter(port); err != nil {
		log.Printf("Failed to export port %d: %v", port, err)
	}
}

func (em *ExporterManager) handleClose(port int) {
	em.mu.Lock()
	defer em.mu.Unlock()

	exp, exists := em.exporters[port]
	if !exists {
		return
	}

	exp.refcount--
	if em.config.Verbose {
		log.Printf("Port %d refcount decreased to %d", port, exp.refcount)
	}

	if exp.refcount <= 0 {
		em.stopExporter(port)
	}
}

func (em *ExporterManager) startExporter(port int) error {
	// Listen on tailnet
	listener, err := em.server.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("failed to listen on tailnet port %d: %w", port, err)
	}

	ctx, cancel := context.WithCancel(em.ctx)
	exp := &portExporter{
		port:     port,
		listener: listener,
		refcount: 1,
		ctx:      ctx,
		cancel:   cancel,
	}

	em.exporters[port] = exp

	if em.config.Verbose {
		log.Printf("Exporting port %d on tailnet", port)
	}

	// Start accept loop
	exp.wg.Add(1)
	go func() {
		defer exp.wg.Done()
		em.acceptLoop(exp)
	}()

	return nil
}

func (em *ExporterManager) stopExporter(port int) {
	exp, exists := em.exporters[port]
	if !exists {
		return
	}

	if em.config.Verbose {
		log.Printf("Stopping export of port %d", port)
	}

	exp.cancel()
	exp.listener.Close()
	delete(em.exporters, port)

	// Wait for accept loop to finish
	go exp.wg.Wait()
}

func (em *ExporterManager) acceptLoop(exp *portExporter) {
	for {
		conn, err := exp.listener.Accept()
		if err != nil {
			if exp.ctx.Err() != nil {
				return
			}
			if em.config.Verbose {
				log.Printf("Accept error on port %d: %v", exp.port, err)
			}
			continue
		}

		go em.forwardConnection(exp.ctx, conn, exp.port)
	}
}

func (em *ExporterManager) forwardConnection(ctx context.Context, tsConn net.Conn, port int) {
	defer tsConn.Close()

	// Try IPv4 loopback first
	localConn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		// Try IPv6 loopback
		localConn, err = net.Dial("tcp", fmt.Sprintf("[::1]:%d", port))
		if err != nil {
			if em.config.Verbose {
				log.Printf("Failed to connect to local port %d: %v", port, err)
			}
			return
		}
	}
	defer localConn.Close()

	if em.config.Verbose {
		log.Printf("Forwarding connection to local port %d", port)
	}

	// Bidirectional copy
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(localConn, tsConn)
		localConn.(*net.TCPConn).CloseWrite()
	}()

	go func() {
		defer wg.Done()
		io.Copy(tsConn, localConn)
		tsConn.(*net.TCPConn).CloseWrite()
	}()

	wg.Wait()
}

func (em *ExporterManager) isPortAllowed(port int) bool {
	// Check deny list first
	if em.config.ExportDenyPorts != "" {
		if em.matchesPortSpec(port, em.config.ExportDenyPorts) {
			return false
		}
	}

	// Check allow list (if specified)
	if em.config.ExportAllowPorts != "" {
		return em.matchesPortSpec(port, em.config.ExportAllowPorts)
	}

	// No allow list specified, allow by default (subject to deny list)
	return true
}

func (em *ExporterManager) matchesPortSpec(port int, spec string) bool {
	parts := strings.Split(spec, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)

		// Check for range
		if strings.Contains(part, "-") {
			rangeParts := strings.Split(part, "-")
			if len(rangeParts) != 2 {
				continue
			}
			start, err1 := strconv.Atoi(strings.TrimSpace(rangeParts[0]))
			end, err2 := strconv.Atoi(strings.TrimSpace(rangeParts[1]))
			if err1 == nil && err2 == nil && port >= start && port <= end {
				return true
			}
		} else {
			// Single port
			p, err := strconv.Atoi(part)
			if err == nil && p == port {
				return true
			}
		}
	}
	return false
}

// Stop stops all exporters and the control socket
func (em *ExporterManager) Stop() {
	em.cancel()

	em.mu.Lock()
	defer em.mu.Unlock()

	for port := range em.exporters {
		em.stopExporter(port)
	}
}
