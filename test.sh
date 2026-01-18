#!/bin/bash
# Simple test script for TailProxy

set -e

echo "=== TailProxy Test Script ==="
echo

# Check if built
if [ ! -f "./tailproxy" ] || [ ! -f "./libtailproxy.so" ]; then
    echo "Error: tailproxy or libtailproxy.so not found. Run 'make build' first."
    exit 1
fi

echo "1. Testing verbose output..."
echo "   Running: ./tailproxy -verbose echo 'Hello from TailProxy'"
./tailproxy -verbose echo "Hello from TailProxy" 2>&1 | head -5
echo

echo "2. Testing with network command (requires Tailscale auth)..."
echo "   This will require you to authenticate with Tailscale on first run."
echo "   Press Ctrl+C to skip this test, or Enter to continue..."
read -t 5 || echo "Continuing..."

echo
echo "   Running: ./tailproxy -verbose curl -s https://ifconfig.me"
echo "   Note: This will show your Tailscale exit IP if configured"
./tailproxy -verbose curl -s https://ifconfig.me 2>&1 || echo "Test completed (may have failed if not authenticated)"

echo
echo "=== Test completed ==="
echo
echo "To use TailProxy:"
echo "  ./tailproxy [options] <command>"
echo
echo "To use with exit node:"
echo "  ./tailproxy -exit-node=your-exit-node curl https://ifconfig.me"
echo
echo "For more info, see README.md"
