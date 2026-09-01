#!/bin/sh
set -e

# Runtime dependencies for the processor: systemd-container provides
# systemd-nspawn (the sandbox backend); btrfs-progs is required by
# `actions-runner-processor setup`, which provisions the loopback btrfs backing
# and image subvolume (the previous ensure_btrfs() here moved into that command).
for pkg in systemd-container btrfs-progs; do
    if ! dpkg -s "$pkg" >/dev/null 2>&1; then
        export DEBIAN_FRONTEND=noninteractive
        apt-get update -y -qq
        apt-get install -y -qq "$pkg"
    fi
done

# Set permissions
chmod 600 /etc/actions-runner-processor/config.yaml 2>/dev/null || true

# Reload systemd and enable service
systemctl daemon-reload
systemctl enable actions-runner-processor.service 2>/dev/null || true
systemctl try-restart actions-runner-processor.service 2>/dev/null || true
