#!/bin/bash
# Build the guest-agent as a static Linux binary
set -e

cd "$(dirname "$0")"

echo "Building guest-agent for Linux (static)..."
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o guest-agent .

echo "Built: $(pwd)/guest-agent"
echo ""
echo "To install into rootfs:"
echo "  sudo mount /home/milan/fc/rootfs.ext4 /mnt"
echo "  sudo cp guest-agent /mnt/usr/bin/guest-agent"
echo "  sudo chmod +x /mnt/usr/bin/guest-agent"
echo "  sudo umount /mnt"
