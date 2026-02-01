# Domain Forwarder

A high-performance HTTP/HTTPS forward proxy server written in Go.

## Features

- **High Concurrency**: Optimized for handling thousands of concurrent connections
- **Connection Pooling**: Efficient connection reuse with configurable pool sizes
- **Buffer Pooling**: Memory-efficient data transfer using `sync.Pool`
- **HTTP & HTTPS Support**: Full support for both HTTP forwarding and HTTPS tunneling (CONNECT method)
- **Hop-by-hop Header Handling**: Properly removes proxy-specific headers
- **X-Forwarded-For**: Tracks original client IP addresses
- **Graceful Shutdown**: Clean shutdown on SIGINT/SIGTERM
- **YAML Configuration**: Flexible configuration via YAML file

## Quick Start

```bash
# Build all binaries
make build

# Run the proxy server (default port: 8080)
make run-proxy

# Or run with config file
./build/proxy -c config.yaml

# Or run directly with custom port
./build/proxy -addr :3128
```

## Usage

### Proxy Server

```bash
# Start proxy with default settings
./build/proxy

# Start proxy with config file
./build/proxy --config config.yaml
./build/proxy -c config.yaml

# Override config with command-line flag
./build/proxy -c config.yaml -addr :3128
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

## Configuration

Both services support configuration via YAML file. Use `--config` or `-c` to specify the config file path.

### Config File (config.yaml)

```yaml
# Proxy server configuration
proxy:
  addr: ":8080"
  buffer_size: 32768  # 32KB
  
  connection:
    dial_timeout: 30s
    keep_alive: 30s
    target_dial_timeout: 30s
  
  transport:
    max_idle_conns: 1000
    max_idle_conns_per_host: 100
    max_conns_per_host: 0  # No limit
    idle_conn_timeout: 90s
    tls_handshake_timeout: 10s
    expect_continue_timeout: 1s
    response_header_timeout: 30s
    force_attempt_http2: true
    disable_compression: false
  
  server:
    read_header_timeout: 10s
    idle_timeout: 120s
    max_header_bytes: 1048576  # 1MB
    shutdown_timeout: 30s

# Request printer configuration
request_printer:
  addr: ":9090"
  max_body_read: 10485760  # 10MB
  
  server:
    read_timeout: 30s
    write_timeout: 30s
    idle_timeout: 60s
    read_header_timeout: 10s
    max_header_bytes: 1048576  # 1MB
    shutdown_timeout: 10s
```

### Command-line Options

#### Proxy Server

| Flag | Default | Description |
|------|---------|-------------|
| `-c, --config` | - | Path to YAML config file |
| `-addr` | `:8080` | Listen address (overrides config) |

#### Request Printer

| Flag | Default | Description |
|------|---------|-------------|
| `-c, --config` | - | Path to YAML config file |
| `-addr` | `:9090` | Listen address (overrides config) |

### Configuration Precedence

1. Command-line flags (highest priority)
2. Config file values
3. Default values (lowest priority)

## Tools

### request-printer

A utility service for testing and debugging proxy requests. It logs all incoming request details in JSON format.

```bash
# Start with default settings
make run-printer

# Start with config file
./build/request-printer -c config.yaml

# Override listen address
./build/request-printer -c config.yaml -addr :9999
```

**Output includes:**
- Timestamp
- Source IP and port
- HTTP method, URL, protocol
- All request headers
- Body size
- Body preview (first/last 50 chars if >100 characters)

**Example output:**
```json
{
  "timestamp": "2026-02-01T12:00:00.000000+08:00",
  "source_ip": "127.0.0.1",
  "source_port": "50681",
  "method": "POST",
  "url": "/test?foo=bar",
  "proto": "HTTP/1.1",
  "host": "localhost:9090",
  "headers": {
    "Content-Type": "application/json",
    "User-Agent": "curl/8.7.1"
  },
  "body_size": 150,
  "body_preview": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA...AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
}
```

## Building

```bash
# Build all
make build

# Build only proxy
make proxy

# Build only request-printer
make request-printer

# Clean build artifacts
make clean
```

## Testing

```bash
# Run automated tests (starts both services, sends test requests)
make test
```

## Project Structure

```
tiny-http-proxy/
├── main.go                      # Proxy server implementation
├── go.mod                       # Go module definition
├── go.sum                       # Go module checksums
├── config.yaml                  # Configuration file
├── Makefile                     # Build automation
├── README.md                    # This file
└── tools/
    └── request-printer/
        └── main.go              # Request printer service
```

## Performance Tuning

The proxy is configured with these defaults for high performance:

| Setting | Default | Description |
|---------|---------|-------------|
| `max_idle_conns` | 1000 | Total idle connections in pool |
| `max_idle_conns_per_host` | 100 | Idle connections per host |
| `idle_conn_timeout` | 90s | How long connections stay idle |
| `tls_handshake_timeout` | 10s | TLS handshake timeout |
| `force_attempt_http2` | true | Enable HTTP/2 support |
| `buffer_size` | 32KB | Data transfer buffer size |

Adjust these values in `config.yaml` based on your workload.

## License

MIT
