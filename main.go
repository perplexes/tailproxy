package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

var (
	exitNode   = flag.String("exit-node", "", "Tailscale exit node to use (hostname or IP)")
	configFile = flag.String("config", "", "Path to configuration file")
	hostname   = flag.String("hostname", "tailproxy", "Hostname for this tsnet node")
	authKey    = flag.String("authkey", "", "Tailscale auth key (optional, for unattended setup)")
	proxyPort  = flag.Int("port", 1080, "SOCKS5 proxy port")
	verbose    = flag.Bool("verbose", false, "Verbose logging")
)

func init() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] [command [args...]]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Modes:\n")
		fmt.Fprintf(os.Stderr, "  Proxy-only:     %s [options]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "                  Run SOCKS5 proxy server only\n\n")
		fmt.Fprintf(os.Stderr, "  Command mode:   %s [options] <command> [args...]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "                  Execute command with transparent proxying via LD_PRELOAD\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  # Run proxy server only\n")
		fmt.Fprintf(os.Stderr, "  %s -exit-node=exit-node-1\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  # Execute command with transparent proxying\n")
		fmt.Fprintf(os.Stderr, "  %s -exit-node=exit-node-1 curl https://ifconfig.me\n", os.Args[0])
	}
}

func main() {
	flag.Parse()

	// Check if running in proxy-only mode (no command provided)
	proxyOnly := flag.NArg() == 0

	// Load config if provided
	var config *Config
	if *configFile != "" {
		var err error
		config, err = LoadConfig(*configFile)
		if err != nil {
			log.Fatalf("Failed to load config: %v", err)
		}
	} else {
		config = &Config{
			ExitNode: *exitNode,
			Hostname: *hostname,
			AuthKey:  *authKey,
			ProxyPort: *proxyPort,
			Verbose:  *verbose,
		}
	}

	// Override config with command-line flags if provided
	if *exitNode != "" {
		config.ExitNode = *exitNode
	}
	if *hostname != "tailproxy" {
		config.Hostname = *hostname
	}
	if *authKey != "" {
		config.AuthKey = *authKey
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("Received interrupt signal, shutting down...")
		cancel()
	}()

	// Start the proxy server
	proxy, err := NewProxyServer(config)
	if err != nil {
		log.Fatalf("Failed to create proxy server: %v", err)
	}

	proxyChan := make(chan error, 1)
	readyChan := make(chan struct{})
	go func() {
		proxyChan <- proxy.StartWithReady(ctx, readyChan)
	}()

	// Wait for proxy to be ready or error
	select {
	case err := <-proxyChan:
		log.Fatalf("Proxy failed to start: %v", err)
	case <-readyChan:
		// Proxy is ready
	}

	if proxyOnly {
		// Proxy-only mode: just wait for interrupt
		fmt.Fprintf(os.Stderr, "SOCKS5 proxy running on 127.0.0.1:%d\n", config.ProxyPort)
		if config.ExitNode != "" {
			fmt.Fprintf(os.Stderr, "Using exit node: %s\n", config.ExitNode)
		}
		fmt.Fprintf(os.Stderr, "Press Ctrl+C to stop\n")

		// Wait for interrupt
		<-sigChan
		cancel()

		// Wait for proxy to finish
		select {
		case err := <-proxyChan:
			if err != nil && err != context.Canceled {
				log.Printf("Proxy server error: %v", err)
			}
		case <-time.After(2 * time.Second):
			if config.Verbose {
				log.Println("Timeout waiting for proxy to stop")
			}
		}
		return
	}

	// Command execution mode
	// Find the preload library
	exePath, err := os.Executable()
	if err != nil {
		log.Fatalf("Failed to get executable path: %v", err)
	}
	exeDir := filepath.Dir(exePath)
	preloadLib := filepath.Join(exeDir, "libtailproxy.so")

	// Check if library exists
	if _, err := os.Stat(preloadLib); os.IsNotExist(err) {
		log.Fatalf("Preload library not found: %s\nPlease run 'make' to build it", preloadLib)
	}

	// Execute the command with LD_PRELOAD
	cmd := exec.CommandContext(ctx, flag.Arg(0), flag.Args()[1:]...)

	// Set up environment with LD_PRELOAD and proxy configuration
	env := os.Environ()
	env = append(env,
		fmt.Sprintf("LD_PRELOAD=%s", preloadLib),
		fmt.Sprintf("TAILPROXY_HOST=127.0.0.1"),
		fmt.Sprintf("TAILPROXY_PORT=%d", config.ProxyPort),
	)

	if config.Verbose {
		env = append(env, "TAILPROXY_VERBOSE=1")
	}

	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if config.Verbose {
		log.Printf("Executing command: %v", flag.Args())
		log.Printf("LD_PRELOAD: %s", preloadLib)
		log.Printf("Proxy configured on 127.0.0.1:%d", config.ProxyPort)
	}

	cmdErr := cmd.Run()

	// Cancel context to stop proxy
	cancel()

	// Wait for proxy to finish
	select {
	case err := <-proxyChan:
		if err != nil && err != context.Canceled {
			log.Printf("Proxy server error: %v", err)
		}
	case <-time.After(2 * time.Second):
		if config.Verbose {
			log.Println("Timeout waiting for proxy to stop")
		}
	}

	if cmdErr != nil {
		if exitErr, ok := cmdErr.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		log.Fatalf("Command failed: %v", cmdErr)
	}
}
