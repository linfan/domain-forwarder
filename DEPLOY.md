# Domain Forwarder Deployment Guide

This document provides detailed steps for deploying Domain Forwarder as a systemd service on Linux systems.

## Table of Contents

- [Building Binaries](#building-binaries)
- [Linux Service Deployment](#linux-service-deployment)
  - [Method 1: Manual Deployment](#method-1-manual-deployment)
  - [Method 2: Using Installation Script](#method-2-using-installation-script)
- [Service Management](#service-management)
- [Log Viewing](#log-viewing)
- [Troubleshooting](#troubleshooting)

---

## Building Binaries

### Local Build

Execute the following commands in the project root directory to build binaries for different platforms:

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

# Build and package release version
make release
```

Build output locations:
- Platform binaries: `dist/<platform>-<arch>/`
- Release packages: `dist/domain-forwarder-<version>-<platform>-<arch>.tar.gz`

---

## Linux Service Deployment

### Method 1: Manual Deployment

#### 1. Prepare Binary Files

```bash
# Build Linux version
make build-linux

# Or extract from release package
tar -xzf domain-forwarder-v1.0.0-linux-amd64.tar.gz
```

#### 2. Create System User

For security reasons, it is recommended to create a dedicated system user to run the service:

```bash
# Create system user (no login permission)
sudo useradd -r -s /bin/false -M domain-forwarder

# Or use an existing user, such as nobody
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

# Copy configuration file
sudo cp config.yaml /etc/domain-forwarder/

# Set permissions
sudo chmod +x /opt/domain-forwarder/domain-forwarder
sudo chmod +x /opt/domain-forwarder/request-printer
sudo chown -R domain-forwarder:domain-forwarder /opt/domain-forwarder
sudo chown -R domain-forwarder:domain-forwarder /etc/domain-forwarder
sudo chown -R domain-forwarder:domain-forwarder /var/log/domain-forwarder
```

#### 4. Configuration File Adjustment

Edit the configuration file `/etc/domain-forwarder/config.yaml`:

```yaml
proxy:
  # Listen address - can be modified as needed
  addr: ":8080"

  # Other configurations can be kept default or adjusted as needed
  buffer_size: 32768

  connection:
    dial_timeout: 30s
    keep_alive: 30s
    target_dial_timeout: 30s

  transport:
    max_idle_conns: 1000
    max_idle_conns_per_host: 100
    max_conns_per_host: 0
    idle_conn_timeout: 90s
    tls_handshake_timeout: 10s
    expect_continue_timeout: 1s
    response_header_timeout: 30s
    force_attempt_http2: true
    disable_compression: false

  server:
    read_header_timeout: 10s
    idle_timeout: 120s
    max_header_bytes: 1048576
    shutdown_timeout: 30s
```

#### 5. Create systemd Service File

Create the file `/etc/systemd/system/domain-forwarder.service`:

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

# Start command
ExecStart=/opt/domain-forwarder/domain-forwarder -config /etc/domain-forwarder/config.yaml

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

The project provides an automated installation script `scripts/install.sh`:

```bash
#!/bin/bash

set -e

# Configuration variables
INSTALL_DIR="/opt/domain-forwarder"
CONFIG_DIR="/etc/domain-forwarder"
LOG_DIR="/var/log/domain-forwarder"
SERVICE_USER="domain-forwarder"
SERVICE_FILE="/etc/systemd/system/domain-forwarder.service"

echo "=== Domain Forwarder Installation Script ==="
echo

# Check if running as root
if [ "$EUID" -ne 0 ]; then
    echo "Error: Please run this script with sudo"
    exit 1
fi

# Check if binary file exists
if [ ! -f "dist/linux-amd64/domain-forwarder" ]; then
    echo "Error: Binary file not found, please run 'make build-linux' first"
    exit 1
fi

# 1. Create system user
echo "1. Creating system user..."
if ! id "$SERVICE_USER" &>/dev/null; then
    useradd -r -s /bin/false -M "$SERVICE_USER"
    echo "   User $SERVICE_USER created successfully"
else
    echo "   User $SERVICE_USER already exists"
fi

# 2. Create directories
echo "2. Creating installation directories..."
mkdir -p "$INSTALL_DIR"
mkdir -p "$CONFIG_DIR"
mkdir -p "$LOG_DIR"

# 3. Stop existing service (if exists)
if systemctl is-active --quiet domain-forwarder; then
    echo "3. Stopping existing service..."
    systemctl stop domain-forwarder
else
    echo "3. No service to stop (not running)"
fi

# 4. Copy files
echo "4. Copying program files..."
cp dist/linux-amd64/domain-forwarder "$INSTALL_DIR/"
cp dist/linux-amd64/request-printer "$INSTALL_DIR/"
cp config.yaml "$CONFIG_DIR/"

# 5. Set permissions
echo "5. Setting file permissions..."
chmod +x "$INSTALL_DIR/domain-forwarder"
chmod +x "$INSTALL_DIR/request-printer"
chown -R "$SERVICE_USER:$SERVICE_USER" "$INSTALL_DIR"
chown -R "$SERVICE_USER:$SERVICE_USER" "$CONFIG_DIR"
chown -R "$SERVICE_USER:$SERVICE_USER" "$LOG_DIR"

# 6. Create systemd service file
echo "6. Creating systemd service..."
cat > "$SERVICE_FILE" << 'EOF'
[Unit]
Description=Domain Forwarder HTTP/HTTPS Proxy Server
Documentation=https://github.com/freyrlin/domain-forwarder
After=network.target network-online.target
Wants=network-online.target

[Service]
Type=simple
User=domain-forwarder
Group=domain-forwarder
WorkingDirectory=/opt/domain-forwarder
ExecStart=/opt/domain-forwarder/domain-forwarder -config /etc/domain-forwarder/config.yaml
Restart=on-failure
RestartSec=5s
LimitNOFILE=65536
LimitNPROC=4096
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/log/domain-forwarder
StandardOutput=journal
StandardError=journal
SyslogIdentifier=domain-forwarder

[Install]
WantedBy=multi-user.target
EOF

# 7. Reload systemd
echo "7. Reloading systemd configuration..."
systemctl daemon-reload

# 8. Enable and start service
echo "8. Enabling and starting service..."
systemctl enable domain-forwarder
systemctl start domain-forwarder

# 9. Wait for service to start
sleep 2

# 10. Check service status
echo "9. Checking service status..."
if systemctl is-active --quiet domain-forwarder; then
    echo "   ✓ Service started successfully!"
else
    echo "   ✗ Service failed to start, please check logs:"
    echo "   sudo journalctl -u domain-forwarder -n 50"
    exit 1
fi

echo
echo "=== Installation Complete ==="
echo
echo "Service management commands:"
echo "  Start service: sudo systemctl start domain-forwarder"
echo "  Stop service: sudo systemctl stop domain-forwarder"
echo "  Restart service: sudo systemctl restart domain-forwarder"
echo "  View status: sudo systemctl status domain-forwarder"
echo "  View logs: sudo journalctl -u domain-forwarder -f"
echo
echo "Configuration file location: $CONFIG_DIR/config.yaml"
echo "Program installation location: $INSTALL_DIR/"
echo "Log directory: $LOG_DIR/"
echo
```

Using the installation script:

```bash
# Grant execution permission
chmod +x scripts/install.sh

# Run installation script
sudo ./scripts/install.sh
```

---

## Service Management

### Basic Commands

```bash
# Start service
sudo systemctl start domain-forwarder

# Stop service
sudo systemctl stop domain-forwarder

# Restart service
sudo systemctl restart domain-forwarder

# Reload configuration (requires modifying config file first)
sudo systemctl restart domain-forwarder

# View service status
sudo systemctl status domain-forwarder

# Enable auto-start on boot
sudo systemctl enable domain-forwarder

# Disable auto-start on boot
sudo systemctl disable domain-forwarder

# Check if service is enabled
sudo systemctl is-enabled domain-forwarder
```

### Configuration Hot Reload

After modifying the configuration file, restart the service:

```bash
# Edit configuration
sudo vim /etc/domain-forwarder/config.yaml

# Restart service to apply configuration
sudo systemctl restart domain-forwarder
```

---

## Log Viewing

### View Logs Using journalctl

```bash
# View latest logs (real-time scrolling)
sudo journalctl -u domain-forwarder -f

# View last 100 lines of logs
sudo journalctl -u domain-forwarder -n 100

# View logs for specific time period
sudo journalctl -u domain-forwarder --since "2024-01-01" --until "2024-01-02"
sudo journalctl -u domain-forwarder --since "1 hour ago"

# View detailed logs (includes all fields)
sudo journalctl -u domain-forwarder -o verbose

# Export logs to file
sudo journalctl -u domain-forwarder > domain-forwarder.log
```

### Configure Log Rotation

If you need to write logs to separate files and rotate them, you can configure rsyslog and logrotate.

Edit `/etc/rsyslog.d/domain-forwarder.conf`:

```
if $programname == 'domain-forwarder' then /var/log/domain-forwarder/domain-forwarder.log
& stop
```

Edit `/etc/logrotate.d/domain-forwarder`:

```
/var/log/domain-forwarder/*.log {
    daily
    rotate 7
    compress
    delaycompress
    missingok
    notifempty
    create 0640 domain-forwarder domain-forwarder
    sharedscripts
    postrotate
        systemctl reload domain-forwarder > /dev/null 2>&1 || true
    endscript
}
```

Restart rsyslog:

```bash
sudo systemctl restart rsyslog
```

---

## Troubleshooting

### 1. Service Fails to Start

Check service status:

```bash
sudo systemctl status domain-forwarder
```

View detailed error logs:

```bash
sudo journalctl -u domain-forwarder -n 50 --no-pager
```

Common issues:
- **Permission issues**: Check file ownership and permissions
- **Port in use**: Use `sudo netstat -tlnp | grep 8080` to check port
- **Configuration file errors**: Check if YAML syntax is correct

### 2. Port in Use

```bash
# Check port usage
sudo netstat -tlnp | grep 8080
# or
sudo ss -tlnp | grep 8080

# Find process using the port
sudo lsof -i :8080

# Terminate the process (use with caution)
sudo kill <PID>
```

### 3. Performance Issues

View service resource usage:

```bash
# CPU and memory usage
systemctl status domain-forwarder

# Detailed resource statistics
sudo systemd-cgtop
```

Adjust service resource limits by editing `/etc/systemd/system/domain-forwarder.service`:

```ini
[Service]
# Limit CPU usage (percentage)
CPUQuota=50%

# Limit memory usage
MemoryLimit=512M

# Limit file descriptor count
LimitNOFILE=65536
```

Reload and restart:

```bash
sudo systemctl daemon-reload
sudo systemctl restart domain-forwarder
```

### 4. Network Connection Issues

Check firewall rules:

```bash
# View firewall status (firewalld)
sudo firewall-cmd --list-all

# Open port
sudo firewall-cmd --permanent --add-port=8080/tcp
sudo firewall-cmd --reload

# Or use iptables
sudo iptables -A INPUT -p tcp --dport 8080 -j ACCEPT
sudo iptables-save
```

Test proxy connection:

```bash
# Test HTTP proxy
curl -x http://localhost:8080 http://httpbin.org/get

# Test HTTPS proxy
curl -x http://localhost:8080 https://httpbin.org/get
```

### 5. Uninstall Service

```bash
# Stop and disable service
sudo systemctl stop domain-forwarder
sudo systemctl disable domain-forwarder

# Remove service file
sudo rm /etc/systemd/system/domain-forwarder.service

# Reload systemd
sudo systemctl daemon-reload

# Remove installation files (optional)
sudo rm -rf /opt/domain-forwarder
sudo rm -rf /etc/domain-forwarder
sudo rm -rf /var/log/domain-forwarder

# Remove user (optional)
sudo userdel domain-forwarder
```

---

## Advanced Configuration

### Multi-Instance Deployment

If you need to run multiple instances, you can create multiple service files:

1. Copy configuration file:

```bash
sudo cp /etc/domain-forwarder/config.yaml /etc/domain-forwarder/config-8081.yaml
```

2. Modify port:

```yaml
# /etc/domain-forwarder/config-8081.yaml
proxy:
  addr: ":8081"
```

3. Create new service file `/etc/systemd/system/domain-forwarder@.service`:

```ini
[Unit]
Description=Domain Forwarder HTTP/HTTPS Proxy Server (Port %i)
After=network.target

[Service]
Type=simple
User=domain-forwarder
Group=domain-forwarder
WorkingDirectory=/opt/domain-forwarder
ExecStart=/opt/domain-forwarder/domain-forwarder -config /etc/domain-forwarder/config-%i.yaml
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
```

4. Start multiple instances:

```bash
sudo systemctl start domain-forwarder@8080
sudo systemctl start domain-forwarder@8081
sudo systemctl enable domain-forwarder@8080
sudo systemctl enable domain-forwarder@8081
```

### Reverse Proxy Configuration (Nginx)

If you need to use Nginx as a reverse proxy:

```nginx
upstream domain_forwarder {
    server 127.0.0.1:8080;
}

server {
    listen 80;
    server_name proxy.example.com;

    location / {
        proxy_pass http://domain_forwarder;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # For CONNECT method, enable long connections
        proxy_connect_timeout 60s;
        proxy_send_timeout 300s;
        proxy_read_timeout 300s;
    }
}
```

---

## Monitoring and Alerting

### Configure Prometheus Monitoring (Optional)

If metrics endpoints are added later, you can use Prometheus for monitoring.

### Simple Health Check Script

Create `/opt/domain-forwarder/health-check.sh`:

```bash
#!/bin/bash

PROXY_HOST="localhost"
PROXY_PORT="8080"

# Test if proxy responds
if curl -x http://$PROXY_HOST:$PROXY_PORT \
    --max-time 5 \
    --silent \
    --fail \
    http://httpbin.org/get > /dev/null 2>&1; then
    echo "OK: Proxy is healthy"
    exit 0
else
    echo "CRITICAL: Proxy is not responding"
    exit 2
fi
```

Add to crontab for periodic checks:

```bash
# Check every 5 minutes
*/5 * * * * /opt/domain-forwarder/health-check.sh || systemctl restart domain-forwarder
```

---

## Security Recommendations

1. **Run User**: Always use a non-privileged user to run the service
2. **Firewall**: Only open necessary ports
3. **Access Control**: Consider adding authentication mechanisms or IP whitelists
4. **Log Auditing**: Regularly check access logs
5. **System Updates**: Keep system and dependencies up to date
6. **Resource Limits**: Use systemd to limit service resource usage
7. **SELinux/AppArmor**: Enable mandatory access control (if supported by the system)

---

## References

- [systemd Service Management Documentation](https://www.freedesktop.org/software/systemd/man/systemd.service.html)
- [journalctl Log Management](https://www.freedesktop.org/software/systemd/man/journalctl.html)
- [Go Cross Compilation](https://golang.org/doc/install/source#environment)

---

## License

This project follows the MIT License.
