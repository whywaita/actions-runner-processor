#!/bin/sh
set -e

# Install systemd-container if not present (provides systemd-nspawn).
if ! command -v systemd-nspawn >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -y -qq
    apt-get install -y -qq systemd-container
fi

# Ensure the btrfs backing for the runner image. The image MUST be a btrfs
# subvolume so systemd-nspawn --ephemeral CoW-snapshots it cheaply (a plain
# directory on an ext4/loop backing falls back to a full copy per job). The
# processor also enforces this at startup (preflight btrfs check).
ensure_btrfs() {
    [ -x /usr/sbin/mkfs.btrfs ] || {
        command -v btrfs >/dev/null 2>&1 || {
            export DEBIAN_FRONTEND=noninteractive
            apt-get update -y -qq
            apt-get install -y -qq btrfs-progs
        }
    }

    MOUNT=/opt/runner-btrfs
    IMG=/var/lib/actions-runner-processor/runner-btrfs.img
    if [ -d "$MOUNT" ] && findmnt -rn -o FSTYPE "$MOUNT" 2>/dev/null | grep -qx btrfs; then
        echo ">>> $MOUNT is already a btrfs mount"
    else
        echo ">>> creating loopback btrfs backing at $MOUNT (${BTRFS_SIZE:-20G})"
        mkdir -p "$(dirname "$IMG")" "$MOUNT"
        if [ ! -f "$IMG" ]; then
            truncate -s "${BTRFS_SIZE:-20G}" "$IMG"
            mkfs.btrfs -f "$IMG" >/dev/null 2>&1 || {
                echo "error: mkfs.btrfs failed; install btrfs-progs" >&2
                exit 1
            }
        fi
        cat > /etc/systemd/system/actions-runner-btrfs.mount <<UNIT
[Unit]
Description=actions-runner-processor runner image btrfs backing
Before=actions-runner-processor.service

[Mount]
What=$IMG
Where=$MOUNT
Type=btrfs
Options=loop,noatime
UNIT
        systemctl daemon-reload
        systemctl enable actions-runner-btrfs.mount 2>/dev/null || true
        systemctl start actions-runner-btrfs.mount 2>/dev/null \
            || mount -o loop "$IMG" "$MOUNT" \
            || { echo "error: could not mount $MOUNT" >&2; exit 1; }
    fi

    if ! btrfs subvolume show "$MOUNT/image" >/dev/null 2>&1; then
        btrfs subvolume create "$MOUNT/image" 2>/dev/null \
            || mkdir -p "$MOUNT/image"
    fi
    echo ">>> runner image subvolume: $MOUNT/image"
}

ensure_btrfs

# Set permissions
chmod 600 /etc/actions-runner-processor/config.yaml 2>/dev/null || true

# Reload systemd and enable service
systemctl daemon-reload
systemctl enable actions-runner-processor.service 2>/dev/null || true
systemctl try-restart actions-runner-processor.service 2>/dev/null || true