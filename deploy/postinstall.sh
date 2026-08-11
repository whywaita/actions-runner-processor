#!/bin/sh
set -e

# Create user if not exists
if ! id -u actions-runner-processor >/dev/null 2>&1; then
    useradd -r -s /bin/false actions-runner-processor
fi

# Remove the global AppArmor relaxation created by older package versions.
# Ubuntu's bubblewrap package includes a dedicated AppArmor profile.
rm -f /etc/sysctl.d/99-fuse-userns.conf
if [ -w /proc/sys/kernel/apparmor_restrict_unprivileged_userns ]; then
    sysctl -w kernel.apparmor_restrict_unprivileged_userns=1 2>/dev/null || true
fi

# Create directories
mkdir -p /opt/runner/actions-runner /opt/runner/workspaces

# Clean up stale FUSE mounts left by older package versions.
if [ -d /opt/runner/overlays ]; then
    for d in /opt/runner/overlays/*/merged; do
        fusermount -u "$d" 2>/dev/null || umount -l "$d" 2>/dev/null || true
    done
    rm -rf /opt/runner/overlays
fi

chown -R actions-runner-processor:actions-runner-processor /opt/runner

# Set permissions
chmod 600 /etc/actions-runner-processor/config.yaml 2>/dev/null || true
chown -R actions-runner-processor:actions-runner-processor /etc/actions-runner-processor 2>/dev/null || true

# Reload systemd and enable service
systemctl daemon-reload
systemctl enable actions-runner-processor.service 2>/dev/null || true
systemctl try-restart actions-runner-processor.service 2>/dev/null || true
