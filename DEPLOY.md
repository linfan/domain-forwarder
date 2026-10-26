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
blacklist entry (neither an IP nor a CIDR range), or missing certificate/key
files when `tls.enabled` is true. `0` is returned after a graceful
SIGINT/SIGTERM shutdown.

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
`sent=`/`recv=` byte counters, the resolved `upstream=` address, and
`duration=`. Aborted tunnels print `outcome=aborted` instead of a duration
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
