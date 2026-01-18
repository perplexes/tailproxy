# TailProxy Technical Design

## Overview

TailProxy combines LD_PRELOAD syscall interception (like proxychains) with Tailscale's tsnet library to create a transparent proxy that routes any application's traffic through a Tailscale network.

## Components

### 1. C Shared Library (`libtailproxy.so`)

**Purpose**: Intercept network syscalls and redirect to SOCKS5 proxy

**Intercepted Functions**:
- `connect()` - Main interception point for TCP connections
- `getaddrinfo()` - DNS resolution (passed through, not intercepted)
- `gethostbyname()` - Legacy DNS resolution (passed through)

**How it works**:
1. Uses `dlsym(RTLD_NEXT, "connect")` to get the original syscall
2. When `connect()` is called, checks if it's a TCP socket
3. Skips localhost connections (to avoid intercepting proxy connection)
4. Connects to local SOCKS5 proxy instead of original destination
5. Performs SOCKS5 handshake with original destination info
6. Returns to application as if connected to original destination

**SOCKS5 Protocol Implementation**:
```c
// 1. Greeting
[0x05, 0x01, 0x00]  // Version 5, 1 method, no auth

// 2. Connect request
[0x05, 0x01, 0x00, ATYP, ADDR, PORT]
// ATYP: 0x01 (IPv4), 0x03 (domain), 0x04 (IPv6)

// 3. Response
[0x05, 0x00, ...]  // Version 5, success
```

### 2. Go Proxy Server (`main.go`, `proxy.go`)

**Purpose**: SOCKS5 proxy that routes through Tailscale

**Key Operations**:
1. Creates embedded Tailscale node using `tsnet.Server`
2. Listens on `127.0.0.1:1080` for SOCKS5 connections
3. Accepts SOCKS5 handshake from preload library
4. Uses `tsnet.Server.Dial()` to connect through Tailscale
5. Bidirectional data forwarding between client and remote

**tsnet Integration**:
```go
srv := &tsnet.Server{
    Hostname: "tailproxy",
    Dir:      "/tmp/tailproxy-<hostname>",
}
conn, err := srv.Dial(ctx, "tcp", "destination:port")
```

The `srv.Dial()` automatically routes through:
- Tailscale network (WireGuard encrypted)
- Configured exit node (if specified)
- Internet or private network destination

### 3. Main Coordinator (`main.go`)

**Purpose**: Orchestrate proxy server and command execution

**Workflow**:
1. Parse command-line flags
2. Start tsnet SOCKS5 proxy server in background
3. Wait for proxy to be ready (~2 seconds)
4. Set `LD_PRELOAD` environment variable
5. Set `TAILPROXY_*` configuration env vars
6. Execute user command with modified environment
7. On command exit, stop proxy server

## Data Flow

```
Application calls connect("example.com", 80)
           ↓
[LD_PRELOAD intercepts]
           ↓
libtailproxy.so: connect("127.0.0.1", 1080)
           ↓
libtailproxy.so: SOCKS5 handshake(example.com, 80)
           ↓
Go Proxy Server receives SOCKS5 request
           ↓
srv.Dial("example.com:80") via tsnet
           ↓
Tailscale routes through:
  - WireGuard tunnel
  - Exit node (if configured)
  - Internet
           ↓
Connection established to example.com:80
           ↓
Bidirectional forwarding
           ↓
Application reads/writes as normal
```

## Exit Node Configuration

Exit nodes are configured at the Tailscale network level, not in the SOCKS5 protocol:

1. User specifies `-exit-node=hostname` flag
2. Main program passes this to proxy server config
3. tsnet uses Tailscale's routing preferences
4. All `srv.Dial()` calls automatically route through exit node

**Note**: Current implementation stores exit node preference but relies on tsnet's default routing. Full exit node support requires configuring Tailscale preferences via the LocalClient API.

## Security Considerations

### LD_PRELOAD Security

- Requires dynamic linking (vulnerable to interception by design)
- Security-sensitive programs may ignore LD_PRELOAD (SUID binaries)
- All intercepted connections visible to preload library

### Tailscale Security

- WireGuard encryption for all tunneled traffic
- Tailscale authentication required
- Auth tokens stored in state directory (protect with file permissions)
- Exit node must be trusted (sees cleartext traffic)

### SOCKS5 Security

- No authentication between preload library and proxy (localhost only)
- Assumes localhost is trusted
- Proxy binds to 127.0.0.1 only (not accessible remotely)

## Performance

### Latency
- LD_PRELOAD: ~microseconds (function call overhead)
- SOCKS5 handshake: ~1-2ms (localhost)
- Tailscale routing: ~10-100ms (depends on exit node location)
- Total overhead: ~10-100ms per connection

### Throughput
- Limited by Tailscale/WireGuard throughput
- Typically 100-500 Mbps depending on CPU and network
- Go copy loop is efficient (io.Copy uses splice on Linux)

### Memory
- Go proxy server: ~50-100MB (tsnet + dependencies)
- Preload library: ~100KB
- Per-connection overhead: ~16KB (buffers)

## Limitations

### Application Compatibility
- **Works**: Dynamically-linked binaries using standard libc
- **Doesn't work**:
  - Statically-linked binaries (no libc to intercept)
  - Applications using raw sockets
  - UDP traffic (different syscalls)
  - Kernel-level networking

### DNS Handling
- DNS queries are NOT intercepted (by design)
- DNS resolution happens via system resolver
- IP addresses passed to SOCKS5 proxy
- Privacy: DNS queries visible to local DNS server

To intercept DNS, would need to:
- Intercept `getaddrinfo()` and return fake IPs
- Map fake IPs to real hostnames in proxy
- Send hostnames (not IPs) in SOCKS5 request

### Exit Node Support
Current implementation has basic exit node support. Full support requires:
- Setting Tailscale preferences via LocalClient
- Waiting for exit node route to be established
- Verifying exit node is online and approved
- Handling exit node failover

## Build Process

1. **C Library**:
   ```bash
   gcc -shared -fPIC -O2 -Wall -o libtailproxy.so preload.c -ldl
   ```
   - `-shared`: Create shared library
   - `-fPIC`: Position-independent code (required for shared libs)
   - `-ldl`: Link against libdl for dlsym()

2. **Go Binary**:
   ```bash
   go build -o tailproxy main.go config.go proxy.go
   ```
   - Must specify files explicitly (avoid compiling .c file)
   - Large binary (~32MB) due to tsnet dependencies

## Future Enhancements

### Potential Improvements
1. **DNS Interception**: Intercept DNS for better privacy
2. **UDP Support**: Intercept sendto/recvfrom for UDP
3. **Dynamic Exit Node**: Switch exit nodes mid-session
4. **Connection Pooling**: Reuse Tailscale connections
5. **IPv6 Support**: Full IPv6 interception
6. **Performance Monitoring**: Track latency, bandwidth
7. **Config Profiles**: Saved configurations for different scenarios
8. **GUI**: Graphical interface for non-technical users

### Known Issues
- No proper exit node preference configuration (needs LocalClient integration)
- 2-second startup delay for proxy readiness (should wait properly)
- No connection failure retry logic
- Limited error messages from preload library
- No IPv6 SOCKS5 support verification

## Testing

### Unit Tests
- C library: Test SOCKS5 handshake logic
- Go proxy: Test SOCKS5 server implementation
- Integration: Test end-to-end with real Tailscale

### Manual Testing
```bash
# Test basic functionality
./tailproxy echo "hello"

# Test network interception
./tailproxy -verbose curl https://ifconfig.me

# Test with exit node
./tailproxy -exit-node=us-exit curl https://ipinfo.io

# Test with non-proxy-aware app
./tailproxy python3 -c "import urllib.request; print(urllib.request.urlopen('https://ifconfig.me').read())"
```

## References

- [proxychains](https://github.com/haad/proxychains) - Original LD_PRELOAD proxy tool
- [tsnet documentation](https://pkg.go.dev/tailscale.com/tsnet) - Tailscale embedded networking
- [SOCKS5 RFC 1928](https://www.rfc-editor.org/rfc/rfc1928) - SOCKS Protocol Version 5
- [LD_PRELOAD technique](https://man7.org/linux/man-pages/man8/ld.so.8.html) - Dynamic linker documentation
