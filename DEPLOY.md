# Domain Forwarder Deployment Guide

This document describes deploying Domain Forwarder as a systemd service on
Linux systems.

## Table of Contents

- [Building Binaries](#building-binaries)
- [Linux Service Deployment](#linux-service-deployment)
  - [Method 1: Manual Deployment](#method-1-manual-deployment)
  - [Method 2: Using Installation Script](#method-2-using-installation-script)
- [Service Management](#service-management)
- [Log Viewing](#log-viewing)
- [Troubleshooting](#troubleshooting)
- [Uninstallation](#uninstallation)

---

## Building Binaries

Run the following in the project root to build for each platform:

```bash
# Build Linux (amd64) version
make build-linux

# Build Linux (arm64) version
make build-linux-arm64

# Build macOS (Intel) version
make build-macos

# Build macOS (Apple Silicon) version
make build-macos-arm64

# Build Windows version
make build-windows

# Build all platform versions at once
make build-all

# Build and package release archives
make release
```

Build output locations:

- Platform binaries: `dist/<platform>-<arch>/domain-forwarder` and
  `dist/<platform>-<arch>/request-printer` (each directory also contains `config.yaml`)
- Release archives: `dist/domain-forwarder-<version>-<platform>-<arch>.tar.gz` (zip for Windows)

---

## Linux Service Deployment

### Method 1: Manual Deployment

#### 1. Prepare Binary Files

```bash
# Build Linux version
make build-linux

# Or extract from a release package
tar -xzf domain-forwarder-v1.0.0-linux-amd64.tar.gz
```

#### 2. Create System User

For security reasons, run the service as a dedicated system user:

```bash
# Create system user (no login permission, no home directory)
sudo useradd -r -s /bin/false -M domain-forwarder
```

#### 3. Install Files to System Directories

```bash
# Create installation directories
sudo mkdir -p /opt/domain-forwarder
sudo mkdir -p /etc/domain-forwarder
sudo mkdir -p /var/log/domain-forwarder

# Copy binary files
sudo cp dist/linux-amd64/domain-forwarder /opt/domain-forwarder/
sudo cp dist/linux-amd64/request-printer /opt/domain-forwarder/

# Install the configuration template and edit it
sudo cp config.example.yaml /etc/domain-forwarder/config.yaml
sudo nano /etc/domain-forwarder/config.yaml

# Set permissions
sudo chmod +x /opt/domain-forwarder/domain-forwarder
sudo chmod +x /opt/domain-forwarder/request-printer
sudo chmod 644 /etc/domain-forwarder/config.yaml
sudo chown -R domain-forwarder:domain-forwarder /opt/domain-forwarder
sudo chown -R domain-forwarder:domain-forwarder /var/log/domain-forwarder
```

#### 4. Configuration File Adjustment

The configuration is overlaid on built-in defaults, so
`/etc/domain-forwarder/config.yaml` only needs the values you want to change.
A typical production config:

```yaml
addr: ":8080"
debug: false
add_proxy_headers: true

source_ip_blacklist: []

forward:
  dial_timeout: 10s
  dial_keep_alive: 30s
  request_timeout: 60s
  stream_idle_timeout: 120s
  response_body_timeout: 300s
  idle_conn_timeout: 90s

server:
  idle_timeout: 120s
  shutdown_timeout: 30s

log:                               # optional; omit for stderr
  dir: /var/log/domain-forwarder
  filename: app.log
  max_size: 100
  max_backups: 10

tls:                               # serve proxy clients over HTTPS
  enabled: false
  cert_file: /etc/domain-forwarder/server.crt
  key_file: /etc/domain-forwarder/server.key
```

Configuration errors (a bad `addr`, negative or ill-formed durations, an
invalid blacklist entry, missing TLS files when `tls.enabled` is true) make the
process exit with code 1 and a precise error message — check `journalctl`
after editing.

#### 5. Create systemd Service File

Create `/etc/systemd/system/domain-forwarder.service`:

```ini
[Unit]
Description=Domain Forwarder HTTP/HTTPS Proxy Server
Documentation=https://github.com/freyrlin/domain-forwarder
After=network.target network-online.target
Wants=network-online.target

[Service]
Type=simple
User=domain-forwarder
Group=domain-forwarder

# Working directory
WorkingDirectory=/opt/domain-forwarder

# Startup command
ExecStart=/opt/domain-forwarder/domain-forwarder -c /etc/domain-forwarder/config.yaml

# Restart policy
Restart=on-failure
RestartSec=5s

# Resource limits
LimitNOFILE=65536
LimitNPROC=4096

# Security hardening options
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/log/domain-forwarder

# Standard output and error output
StandardOutput=journal
StandardError=journal
SyslogIdentifier=domain-forwarder

[Install]
WantedBy=multi-user.target
```

#### 6. Enable and Start Service

```bash
# Reload systemd configuration
sudo systemctl daemon-reload

# Enable service (auto-start on boot)
sudo systemctl enable domain-forwarder

# Start service
sudo systemctl start domain-forwarder

# Check service status
sudo systemctl status domain-forwarder
```

---

### Method 2: Using Installation Script

The project provides `scripts/install.sh`, which creates the service user and
directories, copies the binaries, installs `config.example.yaml` as
`/etc/domain-forwarder/config.yaml` (kept if it already exists), and installs
the systemd unit shown above:

```bash
# Build the Linux binaries first
make build-linux

# Run the installer
sudo ./scripts/install.sh
```

Afterwards edit `/etc/domain-forwarder/config.yaml` and restart:

```bash
sudo systemctl restart domain-forwarder
```

---

## Service Management

```bash
# Start / stop / restart
sudo systemctl start domain-forwarder
sudo systemctl stop domain-forwarder
sudo systemctl restart domain-forwarder

# Status and boot behaviour
sudo systemctl status domain-forwarder
sudo systemctl enable domain-forwarder
sudo systemctl disable domain-forwarder
```

Configuration changes are applied by restarting the service (there is no hot
reload):

```bash
sudo vim /etc/domain-forwarder/config.yaml
sudo systemctl restart domain-forwarder
```

---

## Log Viewing

The service logs to journald by default. Log rotation is handled by journald;
alternatively enable file logging via `log.dir` and `log.filename`, which uses
lumberjack size-based rotation (see the `log` section above).

```bash
# Follow logs in real time
sudo journalctl -u domain-forwarder -f

# Last 100 lines
sudo journalctl -u domain-forwarder -n 100

# Since a point in time
sudo journalctl -u domain-forwarder --since "1 hour ago"
```

Every forwarded request prints one summary line as `[METHOD] source=<ip:port>
target=<url> req_size=<n> resp_status=<code> resp_size=<n> duration=<d>`;
CONNECT tunnels print `sent=`/`recv=` byte counters. Set `debug: true` for
additional detail lines.

---

## Troubleshooting

### Service fails to start

```bash
sudo systemctl status domain-forwarder
sudo journalctl -u domain-forwarder -n 50 --no-pager
```

The startup log points at the exact problem: missing/invalid config file,
a bad `addr`, negative durations, an invalid blacklist entry, missing TLS
files, or a port already in use. Run the binary manually to see the error
directly:

```bash
/opt/domain-forwarder/domain-forwarder -c /etc/domain-forwarder/config.yaml
```

### Port already in use

```bash
sudo ss -tlnp | grep 8080
sudo lsof -i :8080
```

Change `addr` in the configuration or stop the conflicting process.

### Permission denied errors

```bash
sudo chown -R domain-forwarder:domain-forwarder /opt/domain-forwarder
sudo chown domain-forwarder:domain-forwarder /var/log/domain-forwarder
sudo chmod 755 /opt/domain-forwarder
sudo chmod 644 /etc/domain-forwarder/config.yaml
```

### Testing the proxy

```bash
# Test HTTP proxy
curl -x http://localhost:8080 http://example.com

# Test HTTPS proxy (CONNECT)
curl -x http://localhost:8080 https://example.com
```

### Firewall

If using a firewall, open the proxy port:

```bash
# ufw (Ubuntu/Debian)
sudo ufw allow 8080/tcp

# firewalld (CentOS/RHEL)
sudo firewall-cmd --permanent --add-port=8080/tcp
sudo firewall-cmd --reload

# iptables
sudo iptables -A INPUT -p tcp --dport 8080 -j ACCEPT
```

---

## Uninstallation

Using the script (prompts before removing config, logs and the system user):

```bash
sudo ./scripts/uninstall.sh
```

Manual removal:

```bash
# Stop and disable the service
sudo systemctl stop domain-forwarder
sudo systemctl disable domain-forwarder

# Remove the service file and reload systemd
sudo rm /etc/systemd/system/domain-forwarder.service
sudo systemctl daemon-reload

# Remove files
sudo rm -rf /opt/domain-forwarder
sudo rm -rf /etc/domain-forwarder
sudo rm -rf /var/log/domain-forwarder

# Remove the system user
sudo userdel domain-forwarder
```

## File Locations Summary

| File/Directory | Purpose |
|----------------|---------|
| `/opt/domain-forwarder/domain-forwarder` | Proxy binary |
| `/opt/domain-forwarder/request-printer` | Request printer utility (testing) |
| `/etc/domain-forwarder/config.yaml` | Configuration file |
| `/var/log/domain-forwarder/` | Log directory (when file logging is enabled) |
| `/etc/systemd/system/domain-forwarder.service` | Systemd service file |

## References

- [systemd Service Management Documentation](https://www.freedesktop.org/software/systemd/man/systemd.service.html)
- [journalctl Log Management](https://www.freedesktop.org/software/systemd/man/journalctl.html)
