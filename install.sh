#!/bin/bash
# Build and install MyVacControl on the Raspberry Pi.
set -e

INSTALL_USER="myhomeservices"
INSTALL_DIR="/home/$INSTALL_USER/vac-control"
SERVICE_NAME="vac-control"

echo "Building binary..."
go build -o vac-control .

sudo mkdir -p "$INSTALL_DIR/static"
sudo cp vac-control "$INSTALL_DIR/"
sudo cp static/* "$INSTALL_DIR/static/"
if [ ! -f "$INSTALL_DIR/config.yaml" ]; then
    if [ -f config.yaml ]; then
        sudo cp config.yaml "$INSTALL_DIR/"
    else
        sudo cp config.example.yaml "$INSTALL_DIR/config.yaml"
        echo "⚠️  Edit $INSTALL_DIR/config.yaml with your vacuum's details before starting."
    fi
fi
sudo chown -R "$INSTALL_USER:$INSTALL_USER" "$INSTALL_DIR"
sudo chmod 600 "$INSTALL_DIR/config.yaml"

sudo cp "$SERVICE_NAME.service" /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable "$SERVICE_NAME"
sudo systemctl restart "$SERVICE_NAME"

echo "MyVacControl installed. Logs: sudo journalctl -u $SERVICE_NAME -f"
