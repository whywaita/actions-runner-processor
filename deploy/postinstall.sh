#!/bin/sh
set -e

# Create user if not exists
if ! id -u actions-runner-processor >/dev/null 2>&1; then
    useradd -r -s /bin/false actions-runner-processor
fi

# Add user to fuse group for /dev/fuse access
if getent group fuse >/dev/null 2>&1; then
    usermod -a -G fuse actions-runner-processor 2>/dev/null || true
fi

# Enable user_allow_other in fuse.conf so bwrap (root) can access
# fuse-overlayfs mounts created by the actions-runner-processor user.
if [ -f /etc/fuse.conf ]; then
    sed -i 's/^#user_allow_other/user_allow_other/' /etc/fuse.conf
fi

# Create directories
mkdir -p /opt/runner/{actions-runner,workspaces,overlays}
chown -R actions-runner-processor:actions-runner-processor /opt/runner

# Set permissions
chmod 600 /etc/actions-runner-processor/config.yaml 2>/dev/null || true
chown -R actions-runner-processor:actions-runner-processor /etc/actions-runner-processor 2>/dev/null || true

# Reload systemd and enable service
systemctl daemon-reload
systemctl enable actions-runner-processor.service 2>/dev/null || true
