# Domain Forwarder

[中文](README.md)

A high-performance HTTP/HTTPS forward proxy server written in Go.

> Build, configuration reference, performance tuning, deployment and
> operations are covered in [DEPLOY.md](DEPLOY.md).

## Features

- **HTTP & HTTPS**: Plain HTTP forwarding plus HTTPS tunneling via the CONNECT method
- **HTTPS proxy listener**: Serve TLS on the proxy port using a Let's Encrypt `fullchain.pem`, so Chrome can use it as an `https://` proxy
- **HTTP/2 CONNECT**: Tunnels work over both HTTP/1.1 and HTTP/2; Chrome multiplexes all its tunnels over one TLS connection
- **High concurrency**: Connection pooling with configurable pool sizes
- **Buffer pooling**: Memory-efficient data transfer using `sync.Pool`
- **Hop-by-hop header handling**: RFC 7230-compliant header stripping, including `Connection`-listed tokens
- **X-Forwarded-For**: Optionally tracks the original client address (`add_proxy_headers`)
- **Request editing**: Adds custom headers to forwarded requests (`edit`);
  a same-name header is overwritten
- **SSE-aware forwarding**: Streaming responses are flushed chunk by chunk and pass through uncompressed
- **Timeouts**: Upstream dial/header/body-idle/body-duration and server read-header/idle timeouts are all configurable
- **Source IP blacklist**: Reject individual IPs or CIDR ranges with 403
- **Proxy authentication**: Require HTTP Basic authentication (single user,
  plaintext or bcrypt-hashed password); unauthenticated requests get 407
- **Graceful shutdown**: SIGINT/SIGTERM drains in-flight requests within `server.shutdown_timeout`
- **Optional file logging**: Size-based rotation via the `log` section
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

# If tls.enabled is on, the proxy itself is reached over https://
curl -x https://proxy.example.com:8080 https://example.com

# With proxy authentication (-U, or credentials in the proxy URL)
curl -U user:pass -x http://localhost:8080 https://example.com
curl -x http://user:pass@localhost:8080 https://example.com

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

## Configuration

`config.example.yaml` is the fully annotated reference: every value there is
the built-in default, so an omitted key keeps its default. Precedence is
command-line flags > config file > built-in defaults.

The config is grouped into these sections:

- Top level: `addr`, `debug`, `add_proxy_headers`, `source_ip_blacklist`,
  `auth` (HTTP Basic authentication for proxy clients; single user, password
  plaintext or bcrypt hash), `edit` (adds headers before forwarding)
- `forward`: forwarding-engine parameters (pool sizes, dial/TLS/header
  timeouts, `request_timeout`, `stream_idle_timeout`, `response_body_timeout`,
  `streaming_flush`, `disable_compression`, `force_attempt_http2`,
  `buffer_size`)
- `server`: HTTP server settings for the proxy listener
- `log`: optional file logging with size-based rotation (stderr when unset)
- `tls`: TLS for the proxy's own listener (Let's Encrypt `fullchain.pem` supported)

Field-by-field defaults, validation rules and tuning advice live in
[DEPLOY.md](DEPLOY.md).

## Request Forwarding Behavior

- **Method**: Preserved for HTTP; `CONNECT` opens a raw TCP tunnel
- **Headers**: Hop-by-hop headers are stripped in both directions, including the
  tokens listed in a `Connection` header. The client's `Accept-Encoding` is
  forwarded as-is (a forward proxy stays transparent); `Proxy-Authorization` is
  consumed by the proxy and never forwarded upstream
- **Body**: Forwarded as-is, preserving `Content-Length`
- **Redirects**: Upstream 3xx responses are returned to the client as-is; the
  proxy does not follow them
- **Streaming**: `text/event-stream` responses are flushed chunk by chunk
- **Tunnels**: HTTP/1.1 CONNECT is hijacked into a raw TCP tunnel, while
  HTTP/2 CONNECT relays data inside the stream; both use
  `forward.dial_timeout` / `forward.dial_keep_alive` and reuse the engine's
  buffer pool
- **Failures**: A forwarding failure before the response starts returns 502 Bad
  Gateway; failures after the response has started are only logged

## Bundled Tools

`tools/request-printer` is a companion service that logs every request it
receives in JSON, useful for inspecting what the proxy forwards upstream
(`make run-printer`). See [DEPLOY.md](DEPLOY.md#testing-utilities) for its
configuration and the `make test` integration suite.

## Project Structure

```
domain-forwarder/
├── main.go                      # Flags, startup, graceful shutdown
├── config.go                    # Configuration types, defaults, loading, validation
├── pkg/
│   ├── proxy/proxy.go           # Forward proxy handler (CONNECT tunnel + HTTP)
│   └── forwarder/forwarder.go   # HTTP forwarding engine (pooling, timeouts, streaming relay)
├── tools/request-printer/       # Test/debug request logger
├── config.example.yaml          # Annotated config template (copy to config.yaml)
├── Makefile                     # Build automation
├── scripts/                     # install.sh / uninstall.sh / service file
├── DEPLOY.md                    # Deployment guide
├── README.md                    # Chinese README
└── README_EN.md                 # This file
```

## Deployment

Scripted installation on Linux (systemd service, dedicated user):

```bash
make build-linux
sudo ./scripts/install.sh
```

Manual deployment, service management, log viewing and troubleshooting are
described in [DEPLOY.md](DEPLOY.md). Removal: `sudo ./scripts/uninstall.sh`.

## License

MIT
