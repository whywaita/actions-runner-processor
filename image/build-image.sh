#!/bin/bash
# Build a lightweight systemd-nspawn runner rootfs for actions-runner-processor.
#
# Produces a rootfs .tar.gz that you expand to runner.image_path (default
# /opt/runner/image). The manifest image/image.yaml declares the base distro,
# extra apt packages, and the actions/runner version.
#
# Requirements: debootstrap, apt, curl, and root (sudo). On CI this runs as
# root on ubuntu-latest; locally it needs `sudo bash image/build-image.sh`.
#
# Usage:
#   sudo bash image/build-image.sh [output-dir]
#   OUTPUT_DIR=/tmp/img sudo bash image/build-image.sh
#
# Artifacts:
#   $OUTPUT/actions-runner-image-<arch>.tar.gz — expand this to runner.image_path

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFEST="${SCRIPT_DIR}/image.yaml"
OUTPUT_DIR="${OUTPUT_DIR:-${1:-/tmp/runner-image}}"

# --- Parse manifest (YAML subset is overkill; read key: value lines) ---------
get() {
  awk -v key="$1" '$1 == key ":" { sub(/^[^:]*:[[:space:]]*/, ""); gsub(/^"|"$/, ""); print; exit }' "$MANIFEST"
}

DISTRO="$(get distribution)"
RELEASE="$(get release)"
ARCH="$(get arch)"
RUNNER_VERSION="$(get runner_version)"

# packages: YAML list -> space-separated
PACKAGES="$(
  awk '/^[[:space:]]+-[[:space:]]+/{ gsub(/^[[:space:]]*-[[:space:]]*/, ""); printf "%s ", $0 }' "$MANIFEST"
)"

if [[ -z "$DISTRO" || -z "$RELEASE" || -z "$ARCH" ]]; then
  echo "error: manifest missing distribution/release/arch" >&2
  exit 1
fi

if [[ "$(id -u)" != "0" ]]; then
  echo "error: must run as root (nspawn image build needs debootstrap + chroot)" >&2
  exit 1
fi

command -v debootstrap >/dev/null 2>&1 || { echo "error: debootstrap not installed"; exit 1; }
command -v curl >/dev/null 2>&1 || { echo "error: curl not installed"; exit 1; }

ROOTFS="$OUTPUT_DIR/rootfs"
rm -rf "$ROOTFS"
mkdir -p "$ROOTFS" "$OUTPUT_DIR"

echo ">>> debootstrap $DISTRO:$RELEASE ($ARCH)"
debootstrap --variant=minbase --arch="$ARCH" "$RELEASE" "$ROOTFS" "http://archive.ubuntu.com/ubuntu/"

# Provide the run-time DNS config so the container can resolve GitHub.
cp /etc/resolv.conf "$ROOTFS/etc/resolv.conf" 2>/dev/null || true

echo ">>> apt update + install packages"
chroot "$ROOTFS" /bin/bash -c "
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -y -qq
  apt-get install -y -qq ${PACKAGES}
  apt-get clean -y -qq
"

echo ">>> install actions/runner"
if [[ "$RUNNER_VERSION" == "latest" ]]; then
  RUNNER_VERSION="$(curl -sL https://api.github.com/repos/actions/runner/releases/latest | grep -oP '"tag_name":\s*"\K[^"]+')"
fi
TRIM="${RUNNER_VERSION#v}"
RUNNER_FILE="actions-runner-linux-${ARCH}-${TRIM}.tar.gz"
mkdir -p "$ROOTFS/opt/actions-runner"
curl -sL "https://github.com/actions/runner/releases/download/${RUNNER_VERSION}/${RUNNER_FILE}" \
  | tar xz -C "$ROOTFS/opt/actions-runner"

# The runner's official ./run.sh reads ACTIONS_RUNNER_INPUT_JITCONFIG (passed
# via systemd-nspawn --setenv) and boots the listener. Runner runs as root
# inside the container, so `sudo` works in job steps.
chmod +x "$ROOTFS/opt/actions-runner/run.sh"

# Ensure runtime dirs exist so state lands on the ephemeral overlay.
mkdir -p "$ROOTFS/opt/actions-runner/_diag" "$ROOTFS/opt/actions-runner/_work"

echo ">>> finalize /etc/hosts"
chroot "$ROOTFS" /bin/bash -c "
  printf '127.0.0.1 localhost\n::1 localhost ip6-localhost ip6-loopback\n' > /etc/hosts
"

echo ">>> archive rootfs"
TARBALL="$OUTPUT_DIR/actions-runner-image-${ARCH}.tar.gz"
tar -czf "$TARBALL" -C "$ROOTFS" .

echo ""
echo "Done. Image: $TARBALL"
echo "On the host:"
echo "  sudo rm -rf /opt/runner/image && sudo mkdir -p /opt/runner/image"
echo "  sudo tar -xzf $TARBALL -C /opt/runner/image"
echo "  config: runner.image_path=/opt/runner/image, runner.entrypoint=/opt/actions-runner/run.sh"