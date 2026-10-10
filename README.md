# Domain Forwarder

A high-performance HTTP/HTTPS forward proxy server written in Go.

The forwarding engine and its configuration parameters are shared with the
sibling project `domain-dispatcher`: identical parameter names, defaults and
streaming/timeout behavior.

## Features

- **HTTP & HTTPS**: Plain HTTP forwarding plus HTTPS tunneling via the CONNECT method
- **High concurrency**: Connection pooling with configurable pool sizes
- **Buffer pooling**: Memory-efficient data transfer using `sync.Pool`
- **Hop-by-hop header handling**: RFC 7230-compliant header stripping, including `Connection`-listed tokens
- **X-Forwarded-For**: Optionally tracks the original client address (`add_proxy_headers`)
- **SSE-aware forwarding**: Streaming responses are flushed chunk by chunk and pass through uncompressed
- **Timeouts**: Upstream dial/header/body-idle/body-duration and server read-header/idle timeouts are all configurable
- **Source IP blacklist**: Reject individual IPs or CIDR ranges with 403
- **Graceful shutdown**: SIGINT/SIGTERM drains in-flight requests within `server.shutdown_timeout`
- **Optional file logging**: Size-based rotation via `log`
- **YAML configuration**: All values optional; built-in defaults apply when no config file is given

## Quick Start

```bash
# Build all binaries
make build

# Create your local config from the tracked template (config.yaml is gitignored)
cp config.example.yaml config.yaml

# Run the proxy server with the local config (address :8080)
make run

# Or run with a config file / custom address
./build/domain-forwarder -c config.yaml
./build/domain-forwarder -addr :3128
```

### Using the Proxy

```bash
# HTTP request via proxy
curl -x http://localhost:8080 http://example.com

# HTTPS request via proxy
curl -x http://localhost:8080 https://example.com

# Set as environment variable
export http_proxy=http://localhost:8080
export https_proxy=http://localhost:8080
```

## Command-line Options

| Flag | Default | Description |
|------|---------|-------------|
| `-c, --config` | - | Path to YAML config file; built-in defaults apply when omitted |
| `-addr` | `:8080` | Proxy listen address (overrides config) |
| `--version` | - | Print version and build time, then exit |

Exit codes: `0` after a graceful SIGINT/SIGTERM shutdown, `1` when the listener
fails or the configuration is invalid.

## Configuration

See `config.example.yaml` for the fully annotated reference. Every value shown there is
the built-in default, so an omitted key keeps its default. A minimal example:

```yaml
addr: ":8080"
debug: false
add_proxy_headers: false

source_ip_blacklist:
  - 203.0.113.7
  - 192.168.0.0/16

forward:
  dial_timeout: 10s              # also used for CONNECT tunnel dialing
  dial_keep_alive: 30s
  request_timeout: 60s           # max wait for upstream response headers
  stream_idle_timeout: 120s      # abort a body that stalls this long (0s = off)
  response_body_timeout: 300s    # total cap for non-streaming bodies (0s = off)
  disable_compression: true

server:
  idle_timeout: 120s
  shutdown_timeout: 30s

log:                             # optional; omit for stderr
  dir: /var/log/domain-forwarder
  filename: app.log

tls:                             # serve proxy clients over HTTPS
  enabled: false
  cert_file: /path/to/server.crt
  key_file: /path/to/server.key
```

### Configuration Fields

- `addr`: Proxy listen address
- `debug`: Print extra per-request detail logs (the summary line is always printed)
- `add_proxy_headers`: Add `X-Forwarded-For`/`X-Forwarded-Proto`/`X-Forwarded-Host` to forwarded requests
- `source_ip_blacklist`: Client IPs or CIDR ranges rejected with 403
- `forward`: Shared forwarding-engine parameters (connection pool, dial/TLS
  timeouts, `request_timeout`, `stream_idle_timeout`, `response_body_timeout`,
  `streaming_flush`, `disable_compression`, `force_attempt_http2`,
  `buffer_size`) — same names and defaults as `domain-dispatcher`
- `server`: HTTP server settings (`read_header_timeout`, `idle_timeout`,
  `max_header_bytes`, `shutdown_timeout`)
- `log`: Optional file logging with rotation (`dir`, `filename`, `max_size` MB,
  `max_backups`); logs go to stderr when `dir`/`filename` are empty
- `tls`: TLS for the proxy's own listener (`enabled`, `cert_file`, `key_file`);
  startup validation fails if the files do not exist

Precedence: command-line flags > config file > built-in defaults.

## Request Forwarding Details

- **Method**: Preserved for HTTP; `CONNECT` opens a raw TCP tunnel
- **Headers**: Hop-by-hop headers are stripped in both directions, including the
  tokens listed in a `Connection` header. The client's `Accept-Encoding` is
  forwarded as-is (a forward proxy stays transparent)
- **Body**: Forwarded as-is, preserving `Content-Length`
- **Streaming**: `text/event-stream` responses are flushed chunk by chunk
- **Tunnels**: CONNECT tunnels use `forward.dial_timeout` /
  `forward.dial_keep_alive` and reuse the engine's buffer pool
- **Failures**: A forwarding failure before the response starts returns 502 Bad
  Gateway; failures after the response has started are only logged

## Tools

### request-printer

A utility service for testing and debugging proxy requests. It logs all
incoming request details in JSON format.

```bash
# Start with default settings (port 9090)
make run-printer

# Start with config file / custom address
./build/request-printer -c printer.yaml
./build/request-printer -addr :9999
```

**Output includes:** timestamp, source IP/port, method, URL, protocol, all
headers, body size, and a body preview (first/last 50 chars if >100 chars).

## Building

### Local Build

```bash
# Build all binaries for the current platform
make build

# Build only the proxy
make forwarder

# Build only request-printer
make request-printer

# Clean build artifacts
make clean

# View all available commands
make help
```

### Cross-Platform Build

```bash
# Build for Linux (amd64)
make build-linux

# Build for Linux (arm64)
make build-linux-arm64

# Build for macOS (Intel)
make build-macos

# Build for macOS (Apple Silicon)
make build-macos-arm64

# Build for Windows
make build-windows

# Build for all platforms at once
make build-all

# Create release archives (tar.gz for Linux/macOS, zip for Windows)
make release
```

Build artifacts:

- Local: `build/domain-forwarder`, `build/request-printer`
- Platform-specific: `dist/<platform>-<arch>/`
- Release archives: `dist/domain-forwarder-<version>-<platform>-<arch>.tar.gz`

Version and build time are injected via `-ldflags` (`git describe` /
UTC timestamp) and printed by `--version` and at startup.

## Testing

```bash
# Run automated integration tests (starts both services, sends test requests)
make test
```

## Deployment

Scripted installation on Linux (systemd service, dedicated user):

```bash
make build-linux
sudo ./scripts/install.sh
```

See [DEPLOY.md](DEPLOY.md) for manual deployment, service management and
troubleshooting. Removal: `sudo ./scripts/uninstall.sh`.

## Project Structure

```
domain-forwarder/
├── main.go                      # Flags, startup, graceful shutdown
├── config.go                    # Configuration types, defaults, loading, validation
├── pkg/
│   ├── proxy/proxy.go           # Forward proxy handler (CONNECT tunnel + HTTP)
│   └── forwarder/forwarder.go   # Shared forwarding engine (kept in sync with domain-dispatcher)
├── tools/request-printer/       # Test/debug request logger
├── config.example.yaml          # Annotated config template (copy to config.yaml)
├── Makefile                     # Build automation
├── scripts/                     # install.sh / uninstall.sh / service file
├── DEPLOY.md                    # Deployment guide
└── README.md                    # This file
```

## Performance Tuning

The engine ships with these defaults:

| Setting | Default | Description |
|---------|---------|-------------|
| `buffer_size` | 32KB | Data transfer buffer size |
| `max_idle_conns` | 1000 | Total idle connections in pool |
| `max_idle_conns_per_host` | 100 | Idle connections per host |
| `max_conns_per_host` | 0 | Total connections per host (0 = unlimited) |
| `idle_conn_timeout` | 90s | How long connections stay idle |
| `tls_handshake_timeout` | 10s | TLS handshake timeout |
| `force_attempt_http2` | true | Enable HTTP/2 support |

Adjust these values in the `forward` section of `config.yaml` based on your
workload.

## License

MIT
