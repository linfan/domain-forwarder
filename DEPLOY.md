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
ill-formed `addr` or out-of-range port, a non-positive `transport.buffer_size`
or `server.max_header_bytes`, a negative duration or pool size, an invalid
`access_control.source_ip` entry (neither an IP nor a CIDR range), an invalid
`access_control.target_domain` entry (empty, or carrying whitespace, a scheme,
port or path, or a wildcard other than `*` or a leading `*.`), `auth` enabled
without a username or password, a username containing `:`, a password starting with `$2`
that is not a valid bcrypt hash, an `edit.add_headers` entry whose name is not
an RFC 7230 token or whose value contains CR/LF/NUL, `mirror` enabled with a
missing, non-http(s), host-less or query/fragment-bearing `mirror.target` or
with non-positive `timeout`/`queue_size`, `mitm` enabled with no groups, a group
without `ca_cert_file`/`ca_key_file`/`hosts` or with missing CA files, a `mitm`
host pattern that is not `*`, `*.example.com`, a bare host name or an IP literal,
or missing, unreadable or mismatched certificate/key files when `tls.enabled` is
true. `0` is returned after a graceful SIGINT/SIGTERM shutdown.

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

## HTTPS Interception

`mitm` makes matching HTTP/1.1 CONNECT tunnels editable: the proxy terminates
TLS locally with a certificate minted by the group's CA, runs the decrypted
request through `add_proxy_headers` / `edit` / `mirror`, then opens its own HTTPS
connection to the original target. CONNECT targets matching no group stay opaque
TCP tunnels.

### 1. Generate a CA per group

```bash
openssl ecparam -name prime256v1 -genkey -noout -out mitm-ca.key
openssl req -x509 -new -key mitm-ca.key -sha256 -days 3650 \
  -subj "/CN=domain-forwarder MITM CA" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" \
  -out mitm-ca.crt
chmod 600 mitm-ca.key
```

The CA key signs short-lived (up to 7 days), per-host leaf certificates on
demand; leaves are cached by host and re-minted before they expire, so the proxy
can run for months without presenting a certificate a client rejects. The cache
is bounded by `mitm.max_cached_hosts`. Keep each key readable only by the proxy
user and install the matching `.crt` on every client that should trust the
interception.

### 2. Configure groups

```yaml
mitm:
  enabled: true
  tunnel_connect:
    - "*:25"                     # SMTP greets before the client speaks
  groups:
    - name: internal
      ca_cert_file: /etc/domain-forwarder/mitm-internal-ca.crt
      ca_key_file: /etc/domain-forwarder/mitm-internal-ca.key
      hosts:
        - api.example.com
        - "*.internal.example.com"
    - name: all
      ca_cert_file: /etc/domain-forwarder/mitm-ca.crt
      ca_key_file: /etc/domain-forwarder/mitm-ca.key
      hosts:
        - "*"
```

- Groups are evaluated in configuration order; the first match wins.
- `hosts` entries are `*` (every host), `*.example.com` (subdomains only, not
  the bare domain), a bare host name, or an IP literal. Ports are ignored.
- A `*` group makes every later group unreachable; startup logs a warning, as it
  does for hosts listed in more than one group.
- Non-TLS CONNECTs matched by a group (for example to port 80 or SSH) fall back
  to a plain tunnel automatically.
- `tunnel_connect` entries are checked before any group and keep the listed
  CONNECT authorities raw: `*` or `*.example.com` or a bare host, optionally with
  `:port`. Use it for protocols whose server speaks first (SMTP 25, IMAP 143,
  FTP 21) — with no ClientHello to sniff, interception could only wait and then
  close the tunnel. Such a tunnel's summary line carries `excluded_by="<entry>"`.
  An entry that names a port does not match an authority without one.

### 3. Trust the CA on clients

```bash
curl -x http://proxy:8080 https://api.example.com --cacert mitm-internal-ca.crt
```

Add the CA to the operating system or browser trust store for transparent use.
Tools with their own trust stores (Go, Java, Node.js) need the CA configured
there.

### Limitations

- The intercepted CONNECT itself must use HTTP/1.1. A matching CONNECT received
  over proxy-side HTTP/2 returns 501; unmatched HTTP/2 CONNECT tunnels are
  unaffected. Inside an intercepted tunnel, both HTTP/1.1 and HTTP/2 work.
- Clients that pin certificates will reject the minted certificates.
- Upstream client certificates (mTLS) are not forwarded; such hosts cannot be
  intercepted.
- Intercepted connections are not tracked by the main server's graceful
  shutdown, the same as raw HTTP/1.1 CONNECT tunnels.
- A CONNECT authority without a port is dialed as port 443. Group matching
  ignores ports altogether, so one pattern covers every port of a host.
- Decrypted request bodies pass through `mirror` when it is enabled.

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

Intercepted CONNECTs print one summary line per decrypted request plus a
`[MITM] source=... target=... group=... requested=N duration=...` line when the
connection ends. Three outcomes are logged on their own, because no summary line
would explain them: a CONNECT that matches a group but arrives over proxy-side
HTTP/2 logs `outcome=mitm_not_supported`, a ClientHello naming a host outside
that group logs `sni=` with `outcome=sni_outside_group`, and a CONNECT whose peer
sends nothing within `transport.tls_handshake_timeout` logs
`outcome=clienthello_timeout` — that is a server-speaks-first protocol, so its
port belongs in `mitm.tunnel_connect`. Other handshake and
relay errors are logged only with `debug: true`.

## Testing Utilities

`tools/request-printer` is a companion service that logs every incoming
request in JSON and echoes a minimal response — point the proxy at it to
inspect what is actually sent upstream:

```bash
make run-printer                                # start on :9090
curl -x http://localhost:8080 http://localhost:9090/test?foo=bar
```

It takes its settings from `tools/request-printer/config.yaml` (`addr`,
`max_body_read`, `output_dir`, server timeouts) or the `-addr` / `-output-dir`
(`-o`) flags. Set `output_dir` to save every request on disk instead of printing
its JSON to the console — one pair of files per request, plus a single
"Saved request" log line:

```
<timestamp>.header   # request line + headers, text
<timestamp>.body     # raw body, binary (may still be text)
```

`<timestamp>` has microsecond precision (`20060102-150405.000000`); on a name
collision `-2`, `-3`, ... is appended to both files. `make test` starts both
services, sends test requests and stops them.

## Troubleshooting

```bash
sudo systemctl status domain-forwarder
sudo journalctl -u domain-forwarder -n 50 --no-pager
```

Startup failures (missing/invalid config, a bad `addr`, negative durations,
an invalid access-control entry, missing TLS or mitm CA files, a port already in
use) are printed with a precise message. Run the binary manually to see it directly:

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
