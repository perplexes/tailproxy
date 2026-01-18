package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"tailscale.com/client/tailscale"
	"tailscale.com/ipn"
	"tailscale.com/tsnet"
)

type ProxyServer struct {
	config  *Config
	server  *tsnet.Server
	mu      sync.Mutex
	dialer  *net.Dialer
}

func getStateDir(hostname string) string {
	// Check for explicit state directory from environment
	if dir := os.Getenv("TAILPROXY_STATE_DIR"); dir != "" {
		return dir
	}

	// Use XDG_STATE_HOME if set, otherwise ~/.local/state
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			// Fall back to temp directory if we can't get home dir
			return filepath.Join(os.TempDir(), "tailproxy-"+hostname)
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "tailproxy", hostname)
}

func NewProxyServer(config *Config) (*ProxyServer, error) {
	// Create state directory - use persistent location for stable node ID
	stateDir := getStateDir(config.Hostname)
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create state directory: %w", err)
	}

	srv := &tsnet.Server{
		Hostname: config.Hostname,
		Dir:      stateDir,
		Logf:     func(format string, args ...any) {
			if config.Verbose {
				log.Printf("[tsnet] "+format, args...)
			}
		},
	}

	if config.AuthKey != "" {
		srv.AuthKey = config.AuthKey
	}

	return &ProxyServer{
		config: config,
		server: srv,
	}, nil
}

func (p *ProxyServer) waitForAuth(ctx context.Context, lc *tailscale.LocalClient) error {
	// If we have an auth key, tsnet handles it automatically
	if p.config.AuthKey != "" {
		if p.config.Verbose {
			log.Println("Using provided auth key...")
		}
		// Wait for the server to be ready with the auth key
		_, err := p.server.Up(ctx)
		return err
	}

	// For interactive auth, we need to check status and wait
	authURLPrinted := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		status, err := lc.Status(ctx)
		if err != nil {
			// Server might not be ready yet, wait a bit
			time.Sleep(500 * time.Millisecond)
			continue
		}

		// Check if we're already authenticated
		if status.BackendState == "Running" {
			if p.config.Verbose {
				log.Println("Tailscale connected and authenticated")
			}
			return nil
		}

		// Check if we need to print an auth URL
		if status.AuthURL != "" && !authURLPrinted {
			// Print the auth URL to stderr so user can click it
			fmt.Fprintf(os.Stderr, "\nTo authenticate, visit:\n\n\t%s\n\n", status.AuthURL)
			authURLPrinted = true
		}

		// Wait a bit before checking again
		time.Sleep(time.Second)
	}
}

func (p *ProxyServer) Start(ctx context.Context) error {
	return p.StartWithReady(ctx, nil)
}

func (p *ProxyServer) StartWithReady(ctx context.Context, ready chan<- struct{}) error {
	// Suppress noisy tsnet startup messages unless verbose
	var originalOutput io.Writer
	if !p.config.Verbose {
		originalOutput = log.Writer()
		log.SetOutput(io.Discard)
	}

	// Start tsnet
	if p.config.Verbose {
		log.Println("Starting Tailscale network...")
	}

	// Get local client to configure exit node
	lc, err := p.server.LocalClient()
	if err != nil {
		return fmt.Errorf("failed to get local client: %w", err)
	}

	// Wait for authentication to complete
	if err := p.waitForAuth(ctx, lc); err != nil {
		if originalOutput != nil {
			log.SetOutput(originalOutput)
		}
		return fmt.Errorf("authentication failed: %w", err)
	}

	// Restore log output after tsnet startup noise
	if originalOutput != nil {
		log.SetOutput(originalOutput)
	}

	// Set exit node if specified
	if p.config.ExitNode != "" {
		if p.config.Verbose {
			log.Printf("Configuring exit node: %s", p.config.ExitNode)
		}

		// Get status to find the exit node peer
		status, err := lc.Status(ctx)
		if err != nil {
			return fmt.Errorf("failed to get status: %w", err)
		}

		// Find the exit node by hostname or IP
		var exitNodeIP string
		for _, peer := range status.Peer {
			if peer.HostName == p.config.ExitNode || peer.DNSName == p.config.ExitNode ||
				peer.DNSName == p.config.ExitNode+"."+status.MagicDNSSuffix {
				if len(peer.TailscaleIPs) > 0 {
					exitNodeIP = peer.TailscaleIPs[0].String()
					break
				}
			}
			// Also check by IP address
			for _, ip := range peer.TailscaleIPs {
				if ip.String() == p.config.ExitNode {
					exitNodeIP = ip.String()
					break
				}
			}
		}

		if exitNodeIP == "" {
			return fmt.Errorf("exit node %q not found in peers", p.config.ExitNode)
		}

		if p.config.Verbose {
			log.Printf("Setting exit node to %s (IP: %s)", p.config.ExitNode, exitNodeIP)
		}

		// Set the exit node using EditPrefs
		prefs := &ipn.MaskedPrefs{
			Prefs: ipn.Prefs{
				ExitNodeIP: netip.MustParseAddr(exitNodeIP),
			},
			ExitNodeIPSet: true,
		}
		if _, err := lc.EditPrefs(ctx, prefs); err != nil {
			return fmt.Errorf("failed to set exit node: %w", err)
		}

		if p.config.Verbose {
			log.Printf("Exit node configured successfully")
		}
	}

	// Listen on localhost for SOCKS5 connections
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p.config.ProxyPort))
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}

	// Close listener when context is canceled to unblock Accept()
	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	if p.config.Verbose {
		log.Printf("SOCKS5 proxy listening on 127.0.0.1:%d", p.config.ProxyPort)
	}

	// Signal that we're ready
	if ready != nil {
		close(ready)
	}

	// Accept connections
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if p.config.Verbose {
				log.Printf("Accept error: %v", err)
			}
			continue
		}

		go p.handleConnection(ctx, conn)
	}
}

func (p *ProxyServer) handleConnection(ctx context.Context, clientConn net.Conn) {
	defer clientConn.Close()

	// SOCKS5 handshake
	buf := make([]byte, 256)

	// Read version and methods
	n, err := clientConn.Read(buf)
	if err != nil {
		if p.config.Verbose {
			log.Printf("Failed to read SOCKS5 greeting: %v", err)
		}
		return
	}

	if n < 2 || buf[0] != 0x05 {
		if p.config.Verbose {
			log.Printf("Invalid SOCKS5 version: %d", buf[0])
		}
		return
	}

	// Send "no authentication required" response
	_, err = clientConn.Write([]byte{0x05, 0x00})
	if err != nil {
		return
	}

	// Read request
	n, err = clientConn.Read(buf)
	if err != nil {
		if p.config.Verbose {
			log.Printf("Failed to read SOCKS5 request: %v", err)
		}
		return
	}

	if n < 7 || buf[0] != 0x05 {
		return
	}

	cmd := buf[1]
	if cmd != 0x01 { // Only support CONNECT
		clientConn.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	// Parse address
	addrType := buf[3]
	var host string
	var port uint16

	switch addrType {
	case 0x01: // IPv4
		if n < 10 {
			return
		}
		host = fmt.Sprintf("%d.%d.%d.%d", buf[4], buf[5], buf[6], buf[7])
		port = uint16(buf[8])<<8 | uint16(buf[9])
	case 0x03: // Domain name
		if n < 5 {
			return
		}
		addrLen := int(buf[4])
		if n < 5+addrLen+2 {
			return
		}
		host = string(buf[5 : 5+addrLen])
		port = uint16(buf[5+addrLen])<<8 | uint16(buf[5+addrLen+1])
	case 0x04: // IPv6
		if n < 22 {
			return
		}
		host = net.IP(buf[4:20]).String()
		port = uint16(buf[20])<<8 | uint16(buf[21])
	default:
		clientConn.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	target := net.JoinHostPort(host, fmt.Sprintf("%d", port))

	if p.config.Verbose {
		log.Printf("Connecting to %s via Tailscale", target)
	}

	// Dial through Tailscale
	var remoteConn net.Conn
	if p.config.ExitNode != "" {
		// Use tsnet's dialer which routes through the Tailscale network
		remoteConn, err = p.server.Dial(ctx, "tcp", target)
	} else {
		// Direct connection through Tailscale network
		remoteConn, err = p.server.Dial(ctx, "tcp", target)
	}

	if err != nil {
		if p.config.Verbose {
			log.Printf("Failed to connect to %s: %v", target, err)
		}
		clientConn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer remoteConn.Close()

	// Send success response
	_, err = clientConn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	if err != nil {
		return
	}

	// Bidirectional copy
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(remoteConn, clientConn)
	}()

	go func() {
		defer wg.Done()
		io.Copy(clientConn, remoteConn)
	}()

	wg.Wait()
}
