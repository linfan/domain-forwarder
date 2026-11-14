# Domain Forwarder Deployment Guide

This document covers building, configuring, deploying and operating Domain
Forwarder, including running it as a systemd service on Linux. For an
introduction to the project itself, see [README.md](README.md)
(English: [README_EN.md](README_EN.md)).

## Build

```bash
make build-linux    # dist/linux-amd64/domain-forwarder + request-printer
```

Other platforms: `make build-linux-arm64`, `build-macos`, `build-macos-arm64`,
`build-windows`, `build-all` (outputs under `dist/`, each platform directory
also containing `config.example.yaml`); `make release` builds and packages
archives. Version and build time are injected via `-ldflags` and printed by
`--version` and at startup.

## Configuration

The full annotated reference — defaults, semantics and tuning notes for every
setting — is `config.example.yaml`, bundled with every build and release
archive. An omitted key keeps its built-in default, so running without `-c`
starts with all defaults; precedence: command-line flags > config file >
built-in defaults.

Startup validation exits with code `1` and a precise message for: an
ill-formed `addr` or out-of-range port, a non-positive `forward.buffer_size`
or `server.max_header_bytes`, a negative duration or pool size, an invalid
blacklist entry (neither an IP nor a CIDR range), `auth` enabled without a
username or password, a username containing `:`, a password starting with `$2`
that is not a valid bcrypt hash, an `edit.add_headers` entry whose name is not
an RFC 7230 token or whose value contains CR/LF/NUL, `mirror` enabled with a
missing, non-http(s), host-less or query/fragment-bearing `mirror.target` or
with non-positive `timeout`/`queue_size`, or missing, unreadable or mismatched
certificate/key files when `tls.enabled` is true. `0` is returned after a
graceful SIGINT/SIGTERM shutdown.

## HTTPS Proxy with Let's Encrypt

The proxy listener can serve TLS directly, so browsers can use it as an
`https://` proxy. Any PEM certificate/key pair works (`server.crt` +
`server.key`); a full chain is recommended so clients receive the
intermediates. Browsers reject proxy TLS errors without an interstitial, so
Chrome needs a publicly trusted certificate; with Let's Encrypt, point
`tls.cert_file` at `fullchain.pem` (the intermediate chain must be included,
`cert.pem` alone is not enough) and `tls.key_file` at `privkey.pem`:

```yaml
addr: ":8888"
tls:
  enabled: true
  cert_file: /etc/letsencrypt/live/proxy.example.com/fullchain.pem
  key_file: /etc/letsencrypt/live/proxy.example.com/privkey.pem
```

### Permissions and renewal

Certbot keeps `privkey.pem` readable by root only, and `/etc/letsencrypt/archive`
is not traversable by other users. The service runs as `domain-forwarder`, so
copy the certificate somewhere the service can read, and let a certbot deploy
hook refresh the copy and restart the service on every renewal (the proxy has
no hot reload):

```bash
sudo install -d -m 750 -o domain-forwarder -g domain-forwarder /etc/domain-forwarder/tls
sudo tee /etc/letsencrypt/renewal-hooks/deploy/domain-forwarder.sh >/dev/null <<'EOF'
#!/bin/sh
set -e
install -m 640 -o domain-forwarder -g domain-forwarder \
  /etc/letsencrypt/live/proxy.example.com/fullchain.pem /etc/domain-forwarder/tls/fullchain.pem
install -m 600 -o domain-forwarder -g domain-forwarder \
  /etc/letsencrypt/live/proxy.example.com/privkey.pem /etc/domain-forwarder/tls/privkey.pem
systemctl try-restart domain-forwarder
EOF
sudo chmod +x /etc/letsencrypt/renewal-hooks/deploy/domain-forwarder.sh
sudo /etc/letsencrypt/renewal-hooks/deploy/domain-forwarder.sh
```

Then set `cert_file: /etc/domain-forwarder/tls/fullchain.pem` and
`key_file: /etc/domain-forwarder/tls/privkey.pem` in the configuration. To use
the certbot directories directly instead, grant the service user access to
them (`setfacl -R -m u:domain-forwarder:rx /etc/letsencrypt/archive /etc/letsencrypt/live`)
and restart the service after every renewal yourself.

### Client settings

- Configure the proxy as `https://proxy.example.com:8888`. The host name must
  match a SAN of the certificate; an IP address will not match a Let's Encrypt
  certificate.
- The listener offers `h2` and `http/1.1` via ALPN. Chrome negotiates HTTP/2 and
  multiplexes its CONNECT tunnels over a single TLS connection; curl uses
  HTTP/1.1 unless `--proxy-http2` is given, which is a convenient way to test
  both tunnel paths.
- Verify from the command line with
  `curl -x https://proxy.example.com:8888 https://example.com`; self-signed
  certificates additionally need `--proxy-cacert`.

## Proxy Authentication

Set `auth.enabled: true` with a single `auth.username` / `auth.password` pair to
require HTTP Basic authentication. Requests without valid credentials get
`407 Proxy Authentication Required` with a `Proxy-Authenticate: Basic`
challenge, covering both plain HTTP forwarding and CONNECT tunnels.
`Proxy-Authorization` is consumed by the proxy and never forwarded upstream.

```yaml
auth:
  enabled: true
  username: alice
  password: s3cret
```

`password` may instead be a bcrypt hash beginning with `$2` (`$2a$`, `$2b$` and
`$2y$` are accepted), keeping the plaintext out of the config file; a malformed
hash fails startup validation. Generate one with any bcrypt tool:

```bash
htpasswd -nbB alice 's3cret' | cut -d: -f2
```

Clients can pass the credentials explicitly or embed them in the proxy URL:

```bash
curl -U alice:s3cret -x http://proxy.example.com:8888 https://example.com
curl -x http://alice:s3cret@proxy.example.com:8888 https://example.com
```

Basic credentials are only base64-encoded, so anyone on the network path of a
plain `http://` proxy listener can read them; pair authentication with
`tls.enabled`. Browsers prompt for the credentials after the first 407 and
reuse them for later requests and CONNECT tunnels. Configuration changes
require a restart (there is no hot reload).

## Manual Deployment

### 1. Install files

```bash
sudo useradd -r -s /bin/false -M domain-forwarder
sudo mkdir -p /opt/domain-forwarder /etc/domain-forwarder /var/log/domain-forwarder

sudo cp dist/linux-amd64/domain-forwarder dist/linux-amd64/request-printer /opt/domain-forwarder/
sudo cp config.example.yaml /etc/domain-forwarder/config.yaml
sudo chmod +x /opt/domain-forwarder/domain-forwarder /opt/domain-forwarder/request-printer
sudo chown -R domain-forwarder:domain-forwarder /opt/domain-forwarder /etc/domain-forwarder /var/log/domain-forwarder
```

### 2. Edit the configuration

The configuration is overlaid on built-in defaults, so
`/etc/domain-forwarder/config.yaml` only needs the values you want to change;
nothing is required (see [Configuration](#configuration)).

### 3. Create the systemd service

Create `/etc/systemd/system/domain-forwarder.service`:

```ini
[Unit]
Description=Domain Forwarder HTTP/HTTPS Proxy Server
Documentation=https://github.com/linfan/domain-forwarder
After=network-online.target
Wants=network-online.target

[Service]
User=domain-forwarder
Group=domain-forwarder
ExecStart=/opt/domain-forwarder/domain-forwarder -c /etc/domain-forwarder/config.yaml
Restart=on-failure
RestartSec=5s
LimitNOFILE=65536

NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/log/domain-forwarder

[Install]
WantedBy=multi-user.target
```

### 4. Start the service

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now domain-forwarder
```

## Installation Script

`scripts/install.sh` automates the steps above — it creates the service user
and directories, copies the binaries, installs `config.example.yaml` as
`/etc/domain-forwarder/config.yaml` (kept if it already exists), and installs
the systemd unit shown above:

```bash
make build-linux
sudo ./scripts/install.sh
```

Edit `/etc/domain-forwarder/config.yaml`, then
`sudo systemctl restart domain-forwarder`.

## Service Management

```bash
sudo systemctl start|stop|restart|status domain-forwarder
sudo systemctl enable|disable domain-forwarder
```

Configuration changes are applied by restarting the service (there is no hot
reload).

## Logs

Logs go to journald by default; file logging with size-based rotation can be
enabled via `log.dir` and `log.filename` in the configuration.

```bash
sudo journalctl -u domain-forwarder -f         # follow in real time
sudo journalctl -u domain-forwarder -n 100     # last 100 lines
```

Every request prints exactly one summary line. CONNECT tunnels include the
proxy-side protocol (`proto=h1` or `proto=h2`), the `sent=`/`recv=` byte
counters, the resolved `upstream=` address, and `duration=`. Aborted tunnels
print `outcome=aborted` instead of a duration
(that is the later direction's end time) and add `upstream_local=` — the proxy's
source address and ephemeral port for the upstream connection, needed to match
NAT/conntrack state — plus per-direction detail: `c2t=`/`t2c=` name the error and
when that direction stopped (`ok` means a clean EOF of the other side;
`reset`/`eio` mean the connection was torn down by a peer or intermediary;
`closed`/`timeout` mean a local close or deadline). An error the kind cannot
classify also prints its message as `c2t_err=`/`t2c_err=`. An HTTP request that
fails before its response starts additionally logs the error; failures after
that point — usually the client hanging up mid-body — are covered by the
summary line alone. Set `debug: true` to also log those late failures and the
client-side local address.

## Testing Utilities

`tools/request-printer` is a companion service that logs every incoming
request in JSON and echoes a minimal response — point the proxy at it to
inspect what is actually sent upstream:

```bash
make run-printer                                # start on :9090
curl -x http://localhost:8080 http://localhost:9090/test?foo=bar
```

It takes its settings from `tools/request-printer/config.yaml` (`addr`,
`max_body_read`, server timeouts) or the `-addr` flag. `make test` starts both
services, sends test requests and stops them.

## Troubleshooting

```bash
sudo systemctl status domain-forwarder
sudo journalctl -u domain-forwarder -n 50 --no-pager
```

Startup failures (missing/invalid config, a bad `addr`, negative durations,
an invalid blacklist entry, missing TLS files, a port already in use) are
printed with a precise message. Run the binary manually to see it directly:

```bash
sudo /opt/domain-forwarder/domain-forwarder -c /etc/domain-forwarder/config.yaml
```

Port conflicts: `sudo ss -tlnp | grep <port>`. Remember to open the proxy port
in the firewall (e.g. `sudo ufw allow 8080/tcp`).

Permission problems are usually fixed by re-running the `chown`/`chmod`
commands from step 1.

## Uninstallation

```bash
sudo ./scripts/uninstall.sh
```

Or manually: stop and disable the service, remove
`/etc/systemd/system/domain-forwarder.service` and run `daemon-reload`,
delete `/opt/domain-forwarder`, `/etc/domain-forwarder` and
`/var/log/domain-forwarder`, then `sudo userdel domain-forwarder`.
