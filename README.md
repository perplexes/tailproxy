# TailProxy

A Go-based proxychains alternative that routes **any** application's traffic through a Tailscale network and exit node using `tsnet` and `LD_PRELOAD`.

## Features

- Routes **any** application's traffic through Tailscale (not just proxy-aware apps)
- Support for Tailscale exit nodes
- SOCKS5 proxy implementation with tsnet
- LD_PRELOAD syscall interception (like proxychains)
- Works with applications that don't support proxies
- Configuration file support
- Automatic Tailscale authentication
- Transparent operation - no application modification needed

## How It Works

TailProxy uses the same technique as proxychains: **LD_PRELOAD syscall interception**. When you run a command through TailProxy, it:

1. Starts an embedded Tailscale instance using `tsnet`
2. Configures the specified exit node (if provided)
3. Launches a local SOCKS5 proxy server on localhost
4. Injects `libtailproxy.so` via `LD_PRELOAD` to intercept network syscalls
5. Intercepts `connect()`, `getaddrinfo()`, and other network calls
6. Redirects all TCP connections through the SOCKS5 proxy
7. Routes all traffic through the Tailscale network

### Architecture

```
[Your Application]
        ↓
[libtailproxy.so] (LD_PRELOAD intercepts connect())
        ↓
[SOCKS5 Proxy] (localhost:1080)
        ↓
[tsnet] (Embedded Tailscale)
        ↓
[Exit Node] (Optional)
        ↓
[Destination]
```

The C library (`libtailproxy.so`) intercepts all `connect()` syscalls at runtime and redirects them to the local SOCKS5 proxy, which then routes traffic through Tailscale.

## Installation

### Build from Source

```bash
# Clone the repository
git clone <repository-url>
cd tailproxy

# Build both components
make build

# This creates:
# - tailproxy (main binary)
# - libtailproxy.so (preload library)
```

### System-wide Installation

```bash
# Install to /usr/local/bin
sudo make install
```

### Requirements

- Go 1.21+ (for tsnet)
- GCC (for compiling C library)
- Linux with glibc (for LD_PRELOAD)
- Tailscale account

**Important**: Both `tailproxy` and `libtailproxy.so` must be in the same directory!

## Usage

### Basic Usage

```bash
tailproxy curl https://ifconfig.me
```

### With Exit Node

```bash
tailproxy -exit-node=exit-node-hostname curl https://ifconfig.me
```

### With Authentication Key

For unattended setup (useful in CI/CD or scripts):

```bash
tailproxy -authkey=tskey-auth-xxxxx curl https://api.example.com
```

### Custom Hostname

```bash
tailproxy -hostname=my-proxy-node curl https://ifconfig.me
```

### Verbose Logging

```bash
tailproxy -verbose curl https://ifconfig.me
```

### Using Configuration File

Create a `config.json`:

```json
{
  "exit_node": "exit-node-hostname",
  "hostname": "tailproxy",
  "authkey": "tskey-auth-xxxxx",
  "proxy_port": 1080,
  "verbose": false
}
```

Then run:

```bash
tailproxy -config=config.json curl https://ifconfig.me
```

## Command-Line Options

```
-exit-node string
    Tailscale exit node to use (hostname or IP)
-config string
    Path to configuration file
-hostname string
    Hostname for this tsnet node (default "tailproxy")
-authkey string
    Tailscale auth key (optional, for unattended setup)
-port int
    SOCKS5 proxy port (default 1080)
-verbose
    Verbose logging
```

## Configuration File Format

```json
{
  "exit_node": "exit-node-hostname",
  "hostname": "tailproxy",
  "authkey": "tskey-auth-xxxxx",
  "proxy_port": 1080,
  "verbose": false
}
```

All fields are optional. Command-line flags override configuration file values.

## Examples

### Test your exit IP

```bash
tailproxy -exit-node=us-exit curl https://ifconfig.me
```

### Run a web scraper through Tailscale

```bash
tailproxy -exit-node=eu-exit python scraper.py
```

### Use with wget

```bash
tailproxy -exit-node=asia-exit wget https://example.com
```

### Use with custom applications

```bash
tailproxy -verbose ./my-app
```

## Supported Applications

TailProxy works with **any dynamically-linked application** that makes TCP connections, including:

- Command-line tools (curl, wget, ssh, git, etc.)
- Programming language runtimes (Python, Node.js, Ruby, etc.)
- Network utilities (telnet, nc, nmap, etc.)
- Custom applications
- Browsers and GUI applications

### Limitations

- Only works with dynamically-linked binaries (not statically-linked Go binaries)
- Only intercepts TCP connections (UDP requires different approach)
- Doesn't work with applications that use raw sockets or custom network stacks
- Some security-sensitive programs may block LD_PRELOAD

## Exit Node Configuration

Exit nodes must be configured in your Tailscale network first. To set up an exit node:

1. On the exit node machine:
   ```bash
   sudo tailscale up --advertise-exit-node
   ```

2. Approve the exit node in the Tailscale admin console

3. Use the exit node's hostname or IP in TailProxy:
   ```bash
   tailproxy -exit-node=exit-node-hostname curl https://ifconfig.me
   ```

## Authentication

On first run, TailProxy will authenticate with Tailscale. You can either:

1. **Interactive**: Follow the authentication URL printed to the console (first run only)
2. **Unattended**: Provide an auth key with `-authkey` flag

To generate an auth key:
1. Go to https://login.tailscale.com/admin/settings/keys
2. Generate a new auth key
3. Use it with the `-authkey` flag

## State Directory

TailProxy stores its state in `/tmp/tailproxy-<hostname>/`. Each unique hostname creates a separate Tailscale node.

## Troubleshooting

### "Connection refused" errors

Make sure the proxy port (default 1080) is not already in use:
```bash
tailproxy -port=1081 curl https://ifconfig.me
```

### Exit node not working

Verify the exit node is approved and online in your Tailscale admin console.

### Application not using proxy

Some applications may not respect proxy environment variables. Check the application's proxy configuration documentation.

### Authentication issues

Use `-verbose` flag to see detailed authentication logs:
```bash
tailproxy -verbose curl https://ifconfig.me
```

## Comparison with Proxychains

| Feature | TailProxy | Proxychains |
|---------|-----------|-------------|
| Method | LD_PRELOAD | LD_PRELOAD |
| Platform | Linux/Unix | Linux/Unix |
| Network | Tailscale (WireGuard) | Any SOCKS/HTTP proxy chain |
| Exit nodes | Built-in Tailscale | Manual proxy chain config |
| Transparency | Fully transparent | Fully transparent |
| Proxy server | Embedded (tsnet) | External |
| Authentication | Tailscale auth | Per-proxy auth |
| Encryption | WireGuard | Depends on proxy |

## Security Notes

- State directory contains authentication tokens - protect it appropriately
- Auth keys should be kept secret and rotated regularly
- Exit node traffic is encrypted via WireGuard
- Local SOCKS5 proxy only listens on 127.0.0.1

## License

MIT

## Contributing

Contributions welcome! Please open an issue or PR.

## See Also

- [Tailscale](https://tailscale.com/)
- [tsnet documentation](https://pkg.go.dev/tailscale.com/tsnet)
- [SOCKS5 RFC](https://www.rfc-editor.org/rfc/rfc1928)
