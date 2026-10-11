# Domain Forwarder

[中文](README.md)

A high-performance HTTP/HTTPS forward proxy server written in Go.

The forwarding engine and its configuration parameters are shared with the
sibling project `domain-dispatcher`: identical parameter names, defaults and
streaming/timeout behavior.

> Build, configuration reference, performance tuning, deployment and
> operations are covered in [DEPLOY.md](DEPLOY.md).

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

- Top level: `addr`, `debug`, `add_proxy_headers`, `source_ip_blacklist`
- `forward`: forwarding-engine parameters (pool sizes, dial/TLS/header
  timeouts, `request_timeout`, `stream_idle_timeout`, `response_body_timeout`,
  `streaming_flush`, `disable_compression`, `force_attempt_http2`,
  `buffer_size`)
- `server`: HTTP server settings for the proxy listener
- `log`: optional file logging with size-based rotation (stderr when unset)
- `tls`: TLS for the proxy's own listener

Field-by-field defaults, validation rules and tuning advice live in
[DEPLOY.md](DEPLOY.md).

## Request Forwarding Behavior

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
│   └── forwarder/forwarder.go   # Shared forwarding engine (kept in sync with domain-dispatcher)
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
