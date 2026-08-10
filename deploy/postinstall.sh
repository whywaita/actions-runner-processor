#!/bin/sh
set -e

# Create user if not exists
if ! id -u actions-runner-processor >/dev/null 2>&1; then
    useradd -r -s /bin/false actions-runner-processor
fi

# Add user to fuse group for /dev/fuse access
if ! getent group fuse >/dev/null 2>&1; then
    groupadd fuse
fi
usermod -a -G fuse actions-runner-processor 2>/dev/null || true

# Enable user_allow_other in fuse.conf so bwrap (root) can access
# fuse-overlayfs mounts created by the actions-runner-processor user.
if [ -f /etc/fuse.conf ]; then
    sed -i 's/^#user_allow_other/user_allow_other/' /etc/fuse.conf
fi

# Relax AppArmor unprivileged user namespace restriction (Ubuntu 24.04+ / kernel 6.8+).
# fuse-overlayfs needs this for non-root FUSE mounts.
if [ -w /proc/sys/kernel/apparmor_restrict_unprivileged_userns ]; then
    sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
fi
if [ -d /etc/sysctl.d ]; then
    echo 'kernel.apparmor_restrict_unprivileged_userns=0' > /etc/sysctl.d/99-fuse-userns.conf
fi

# Create directories
mkdir -p /opt/runner/actions-runner /opt/runner/workspaces /opt/runner/overlays

# Clean up stale FUSE mounts from previous crashes before chown
for d in /opt/runner/overlays/*/merged; do
    fusermount -u "$d" 2>/dev/null || umount -l "$d" 2>/dev/null || true
done
# Remove stale overlay directories from crashed runs
find /opt/runner/overlays -maxdepth 1 -name 'runner-*' -exec rm -rf {} + 2>/dev/null || true

chown -R actions-runner-processor:actions-runner-processor /opt/runner

# Set permissions
chmod 600 /etc/actions-runner-processor/config.yaml 2>/dev/null || true
chown -R actions-runner-processor:actions-runner-processor /etc/actions-runner-processor 2>/dev/null || true

# Reload systemd and enable service
systemctl daemon-reload
systemctl enable actions-runner-processor.service 2>/dev/null || true
