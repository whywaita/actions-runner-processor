#!/bin/sh
set -e

# Install systemd-container if not present (provides systemd-nspawn).
if ! command -v systemd-nspawn >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -y -qq
    apt-get install -y -qq systemd-container
fi

# Create the custom runner image directory.
# For nspawn mode this must be a root filesystem tree with actions/runner
# preinstalled (e.g. built via distrobuilder or debootstrap + actions/runner).
mkdir -p /opt/runner/image

# Set permissions
chmod 600 /etc/actions-runner-processor/config.yaml 2>/dev/null || true

# Reload systemd and enable service
systemctl daemon-reload
systemctl enable actions-runner-processor.service 2>/dev/null || true
systemctl try-restart actions-runner-processor.service 2>/dev/null || true