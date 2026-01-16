# Guest Agent Setup

This directory contains the guest-side agent that runs inside the Firecracker VM to communicate execution results back to the host via Unix Domain Socket.

## Building

```bash
./build.sh
```

Or manually:
```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o guest-agent .
```

## Installing into Rootfs

```bash
sudo mount /home/milan/fc/rootfs.ext4 /mnt
sudo mkdir -p /mnt/usr/bin
sudo cp guest-agent /mnt/usr/bin/guest-agent
sudo chmod +x /mnt/usr/bin/guest-agent
sudo umount /mnt
```

## Updating /sbin/init

The guest init script must be updated to use the guest-agent. Replace the current `/sbin/init` with:

```sh
#!/bin/sh
set -e

# Mount the agent drive at /run/agent
mkdir -p /run/agent
mount /dev/vdb /run/agent

# Run the command and capture output
sh -c "$CMD" > /tmp/stdout 2> /tmp/stderr
echo $? > /tmp/exitcode

# Send results via guest-agent
/usr/bin/guest-agent

# Cleanup and shutdown
poweroff -f
```

To install:
```bash
sudo mount /home/milan/fc/rootfs.ext4 /mnt
sudo cp init /mnt/sbin/init
sudo chmod +x /mnt/sbin/init
sudo umount /mnt
```

## How It Works

1. The host creates a small ext4 image with a Unix socket inside
2. Firecracker mounts this image as `/dev/vdb` in the guest
3. Guest init runs the command, captures stdout/stderr/exitcode
4. Guest agent connects to the socket and sends JSON response
5. Host receives structured `{stdout, stderr, exit_code}` response
