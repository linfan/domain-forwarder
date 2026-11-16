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
  a same-name header is overwritten. HTTPS requests covered by a `mitm` group
  are editable too
- **HTTPS interception**: Per-group CAs and host rules terminate TLS locally for
  matching HTTP/1.1 CONNECTs, edit the decrypted requests and re-originate them
  over HTTPS; supports exact hosts, `*.example.com` and a `*` catch-all
- **Traffic mirroring**: Optionally delivers an asynchronous copy of every
  forwarded request to a second HTTP service (`mirror`); failures are silent
  and never affect the original request. HTTPS requests decrypted by `mitm` are
  mirrored as well
- **SSE-aware forwarding**: Streaming responses are flushed chunk by chunk and pass through uncompressed
- **Timeouts**: Upstream dial/header/body-idle/body-duration and server read-header/idle timeouts are all configurable
- **Access control**: Reject blacklisted source IPs or target hosts with 403;
  a whitelist, when set, admits only listed entries
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

- Top level: `addr`, `debug`, `add_proxy_headers`,
  `access_control` (source-IP and target-domain blacklists/whitelists),
  `auth` (HTTP Basic authentication for proxy clients; single user, password
  plaintext or bcrypt hash), `edit` (adds headers before forwarding),
  `mirror` (asynchronous traffic mirroring),
  `mitm` (HTTPS interception groups and host rules)
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

- **Method**: Preserved for HTTP; `CONNECT` opens a raw TCP tunnel unless its
  target matches a `mitm` group
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
- **HTTPS interception**: An HTTP/1.1 CONNECT matching a `mitm` group is
  terminated locally, serving HTTP/1.1 and HTTP/2 inside the tunnel; WebSocket
  upgrades have their handshake edited and are then relayed raw. Decrypted
  requests run through `add_proxy_headers`, `edit` and `mirror` before being
  re-originated over HTTPS to the original target. Groups are matched in order;
  `hosts` accepts exact hosts, `*.example.com` (subdomains only) and `*` (every
  host). Unmatched CONNECTs stay opaque tunnels. Clients must trust the group's
  CA; certificate pinning, upstream client certificates (mTLS) and interception
  of proxy-side HTTP/2 CONNECT are unsupported, with the last case returning 501
- **Mirroring**: With `mirror` enabled, forwarded requests are also delivered
  asynchronously to the mirror target; undecrypted CONNECTs are not mirrored and
  delivery failures are logged only when `debug` is on
- **Access control**: `access_control.source_ip` and
  `access_control.target_domain` each take a `blacklist` and a `whitelist`; a
  blacklist match gets 403, and a non-empty whitelist admits only listed
  requests. Domain matching ignores case and ports, and subdomains must be
  listed explicitly. Plain HTTP and CONNECT are both checked before any
  upstream connection is made
- **Failures**: A forwarding failure before the response starts returns 502 Bad
  Gateway; failures after the response has started are only logged

## Bundled Tools

`tools/request-printer` is a companion service that logs every request it
receives in JSON. With `output_dir` (or `-output-dir` / `-o`) it saves each
request as `<microsecond timestamp>.header` (request line and headers as text)
and `<microsecond timestamp>.body` (raw body, binary), appending `-2`, `-3`, ...
on collisions, and drops the JSON log line in favor of a short "Saved request"
message. Useful for inspecting what the proxy forwards upstream
(`make run-printer`). See [DEPLOY.md](DEPLOY.md#testing-utilities) for its
configuration and the `make test` integration suite.

## Project Structure

```
domain-forwarder/
├── main.go                      # Flags, startup, graceful shutdown
├── config.go                    # Configuration types, defaults, loading, validation
├── pkg/
│   ├── mitm/mitm.go             # Per-host certificates from group CAs; host matching
│   ├── proxy/proxy.go           # Forward proxy handler (CONNECT tunnel + HTTP)
│   ├── proxy/mitm.go            # CONNECT TLS termination, HTTP/2 and WebSocket relay
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
