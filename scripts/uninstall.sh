#!/bin/bash

set -e

# Configuration variables
INSTALL_DIR="/opt/domain-forwarder"
CONFIG_DIR="/etc/domain-forwarder"
LOG_DIR="/var/log/domain-forwarder"
SERVICE_USER="domain-forwarder"
SERVICE_FILE="/etc/systemd/system/domain-forwarder.service"

echo "=== Domain Forwarder Uninstallation Script ==="
echo

# Check if running as root
if [ "$EUID" -ne 0 ]; then 
    echo "Error: Please run this script with sudo"
    exit 1
fi

# Confirm uninstallation
read -p "Are you sure you want to uninstall Domain Forwarder? This will delete all files and configurations. [y/N] " -n 1 -r
echo
if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo "Uninstallation cancelled"
    exit 0
fi

# 1. Stop service
if systemctl is-active --quiet domain-forwarder; then
    echo "1. Stopping service..."
    systemctl stop domain-forwarder
else
    echo "1. Service not running"
fi

# 2. Disable service
if systemctl is-enabled --quiet domain-forwarder 2>/dev/null; then
    echo "2. Disabling service..."
    systemctl disable domain-forwarder
else
    echo "2. Service not enabled"
fi

# 3. Remove service file
if [ -f "$SERVICE_FILE" ]; then
    echo "3. Removing service file..."
    rm -f "$SERVICE_FILE"
else
    echo "3. Service file does not exist"
fi

# 4. Reload systemd
echo "4. Reloading systemd configuration..."
systemctl daemon-reload

# 5. Remove installation directory
if [ -d "$INSTALL_DIR" ]; then
    echo "5. Removing installation directory..."
    rm -rf "$INSTALL_DIR"
else
    echo "5. Installation directory does not exist"
fi

# 6. Remove configuration directory (optional)
read -p "Remove configuration directory $CONFIG_DIR? [y/N] " -n 1 -r
echo
if [[ $REPLY =~ ^[Yy]$ ]]; then
    if [ -d "$CONFIG_DIR" ]; then
        echo "6. Removing configuration directory..."
        rm -rf "$CONFIG_DIR"
    else
        echo "6. Configuration directory does not exist"
    fi
else
    echo "6. Keeping configuration directory"
fi

# 7. Remove log directory (optional)
read -p "Remove log directory $LOG_DIR? [y/N] " -n 1 -r
echo
if [[ $REPLY =~ ^[Yy]$ ]]; then
    if [ -d "$LOG_DIR" ]; then
        echo "7. Removing log directory..."
        rm -rf "$LOG_DIR"
    else
        echo "7. Log directory does not exist"
    fi
else
    echo "7. Keeping log directory"
fi

# 8. Remove system user (optional)
read -p "Remove system user $SERVICE_USER? [y/N] " -n 1 -r
echo
if [[ $REPLY =~ ^[Yy]$ ]]; then
    if id "$SERVICE_USER" &>/dev/null; then
        echo "8. Removing system user..."
        userdel "$SERVICE_USER"
    else
        echo "8. System user does not exist"
    fi
else
    echo "8. Keeping system user"
fi

echo
echo "=== Uninstallation Complete ==="
echo
