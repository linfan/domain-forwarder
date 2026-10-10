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
if [ -f "$CONFIG_DIR/config.yaml" ]; then
    echo "   Existing configuration kept: $CONFIG_DIR/config.yaml"
else
    cp config.example.yaml "$CONFIG_DIR/config.yaml"
fi

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
ExecStart=/opt/domain-forwarder/domain-forwarder -c /etc/domain-forwarder/config.yaml
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
echo "  Start service:   sudo systemctl start domain-forwarder"
echo "  Stop service:    sudo systemctl stop domain-forwarder"
echo "  Restart service: sudo systemctl restart domain-forwarder"
echo "  Check status:    sudo systemctl status domain-forwarder"
echo "  View logs:       sudo journalctl -u domain-forwarder -f"
echo
echo "Configuration file location: $CONFIG_DIR/config.yaml"
echo "Program installation location: $INSTALL_DIR/"
echo "Log directory: $LOG_DIR/"
echo
