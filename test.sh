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

# Source .env for authkey if it exists
if [ -f ".env" ]; then
    source .env
    echo "Loaded .env (AUTHKEY set: ${AUTHKEY:+yes})"
fi

# Build authkey flag if available
AUTHKEY_FLAG=""
if [ -n "$AUTHKEY" ]; then
    AUTHKEY_FLAG="-authkey=$AUTHKEY"
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
echo "   Running: ./tailproxy -verbose $AUTHKEY_FLAG curl -s https://ifconfig.me"
echo "   Note: This will show your Tailscale exit IP if configured"
./tailproxy -verbose $AUTHKEY_FLAG curl -s https://ifconfig.me 2>&1 || echo "Test completed (may have failed if not authenticated)"

echo
echo "3. Testing export listeners..."
echo "   This test starts a server with -export-listeners and connects via tailnet."
echo "   Logs will be written to /tmp/tailproxy-test-*.log"
echo

# Clean up any previous test logs
rm -f /tmp/tailproxy-test-server.log /tmp/tailproxy-test-client.log

# Generate unique hostname for this test run
TEST_HOSTNAME="exporttest-$$"
SERVER_PORT=18090
SERVER_PROXY_PORT=19080
CLIENT_PROXY_PORT=19081

echo "   Starting server: ./tailproxy -export-listeners -hostname=$TEST_HOSTNAME -port=$SERVER_PROXY_PORT -verbose $AUTHKEY_FLAG python -m http.server $SERVER_PORT"
./tailproxy -export-listeners -hostname="$TEST_HOSTNAME" -port="$SERVER_PROXY_PORT" -verbose $AUTHKEY_FLAG python -m http.server "$SERVER_PORT" \
    > /tmp/tailproxy-test-server.log 2>&1 &
SERVER_PID=$!

echo "   Server PID: $SERVER_PID"
echo "   Waiting for server to initialize (checking for tailnet ready)..."

# Wait for server to be ready - look for tsnet to be up
MAX_WAIT=60
WAITED=0
SERVER_READY=0
while [ $WAITED -lt $MAX_WAIT ]; do
    if grep -q "Exporting port $SERVER_PORT on tailnet" /tmp/tailproxy-test-server.log 2>/dev/null; then
        SERVER_READY=1
        break
    fi
    if ! kill -0 $SERVER_PID 2>/dev/null; then
        echo "   ERROR: Server process died unexpectedly"
        echo "   Server log:"
        cat /tmp/tailproxy-test-server.log
        exit 1
    fi
    sleep 1
    WAITED=$((WAITED + 1))
    if [ $((WAITED % 10)) -eq 0 ]; then
        echo "   Still waiting... ($WAITED seconds)"
    fi
done

if [ $SERVER_READY -eq 0 ]; then
    echo "   ERROR: Server did not become ready within $MAX_WAIT seconds"
    echo "   Server log tail:"
    tail -50 /tmp/tailproxy-test-server.log
    kill $SERVER_PID 2>/dev/null || true
    exit 1
fi

echo "   Server ready after $WAITED seconds"

# Extract server's Tailscale IP from logs (tsnet doesn't configure system DNS, so we need the IP)
SERVER_IP=$(strings /tmp/tailproxy-test-server.log | grep -oP 'netstack: registered IP \K[0-9.]+' | head -1)
if [ -z "$SERVER_IP" ]; then
    echo "   ERROR: Could not extract server Tailscale IP from logs"
    kill $SERVER_PID 2>/dev/null || true
    exit 1
fi
echo "   Server Tailscale IP: $SERVER_IP"
echo

# Give it a moment to fully stabilize
sleep 2

echo "   Running client: ./tailproxy -hostname=testclient-$$ -port=$CLIENT_PROXY_PORT -verbose $AUTHKEY_FLAG curl -s http://$SERVER_IP:$SERVER_PORT/"
./tailproxy -hostname="testclient-$$" -port="$CLIENT_PROXY_PORT" -verbose $AUTHKEY_FLAG curl -s --max-time 30 "http://$SERVER_IP:$SERVER_PORT/" \
    > /tmp/tailproxy-test-client.log 2>&1
CLIENT_EXIT=$?

echo "   Client exit code: $CLIENT_EXIT"
echo

# Check results
if [ $CLIENT_EXIT -eq 0 ] && grep -q "Directory listing" /tmp/tailproxy-test-client.log 2>/dev/null; then
    echo "   SUCCESS: Export listeners test passed!"
    EXPORT_TEST_PASSED=1
else
    echo "   FAILED: Export listeners test failed"
    EXPORT_TEST_PASSED=0
fi

# Clean up server
echo "   Stopping server..."
kill $SERVER_PID 2>/dev/null || true
wait $SERVER_PID 2>/dev/null || true

echo
echo "   Log files saved to:"
echo "     Server: /tmp/tailproxy-test-server.log"
echo "     Client: /tmp/tailproxy-test-client.log"

if [ $EXPORT_TEST_PASSED -eq 0 ]; then
    echo
    echo "   === Server log (last 30 lines) ==="
    tail -30 /tmp/tailproxy-test-server.log
    echo
    echo "   === Client log ==="
    cat /tmp/tailproxy-test-client.log
fi

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
