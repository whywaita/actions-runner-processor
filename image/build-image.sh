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
# GitHub names runner linux binaries with x64/arm64 (not amd64).
case "$ARCH" in
  amd64) RUNNER_ARCH="x64" ;;
  arm64) RUNNER_ARCH="arm64" ;;
  *)     RUNNER_ARCH="$ARCH" ;;
esac
RUNNER_FILE="actions-runner-linux-${RUNNER_ARCH}-${TRIM}.tar.gz"
mkdir -p "$ROOTFS/opt/actions-runner"
curl -sL "https://github.com/actions/runner/releases/download/${RUNNER_VERSION}/${RUNNER_FILE}" \
  | tar xz -C "$ROOTFS/opt/actions-runner"

# The runner's official ./run.sh reads ACTIONS_RUNNER_INPUT_JITCONFIG (passed
# via systemd-nspawn --setenv) and boots the listener.
chmod +x "$ROOTFS/opt/actions-runner/run.sh"

# actions/runner is .NET-based and needs native system deps (libicu, libssl,
# libkrb5, zlib, liblttng-ust) at startup. The runner ships the official
# installdependencies.sh that installs exactly these for the current release;
# call it inside the chroot so the listener boots and stays correct across
# runner upgrades.
echo ">>> install actions/runner native dependencies"
chroot "$ROOTFS" /bin/bash -c "
  export DEBIAN_FRONTEND=noninteractive
  cd /opt/actions-runner
  if [ -x bin/installdependencies.sh ]; then
    ./bin/installdependencies.sh
  else
    echo 'warning: bin/installdependencies.sh not found; installing fallback set'
    apt-get install -y -qq libkrb5-3 zlib1g liblttng-ust1t64 libssl3 libicu74
  fi
"

# Create a dedicated runner user, mirroring the GitHub-hosted / full-image
# convention (uid 1001, home /home/runner, passwordless sudo). launchNspawn
# boots the container with --user=runner, so a `runner` account must exist in
# the rootfs or systemd-nspawn fails with "Failed to resolve user 'runner'".
echo ">>> create runner user (uid 1001, passwordless sudo)"
chroot "$ROOTFS" /bin/bash -c "
  export DEBIAN_FRONTEND=noninteractive
  apt-get install -y -qq whois
  runner_pass=\$(echo runner | mkpasswd -s -m sha-512)
  useradd -m -d /home/runner -s /bin/bash -u 1001 -U -p \"\$runner_pass\" runner
  gpasswd -a runner sudo
  echo 'runner ALL=(ALL) NOPASSWD:ALL' >> /etc/sudoers
"
# Ensure runtime dirs exist so state lands on the ephemeral overlay, then hand
# the runner-owned paths to the runner user.
mkdir -p "$ROOTFS/opt/actions-runner/_diag" "$ROOTFS/opt/actions-runner/_work"
chown -R 1001:1001 "$ROOTFS/opt/actions-runner"

# --- systemd-boot runtime (--boot): runner unit + JIT entrypoint + private-net DNS ---
# The container boots with systemd as PID 1 so job steps can `systemctl start
# docker`, and systemd-networkd DHCPs the private-netns host0 veth.
echo ">>> bake systemd runtime (--boot): runner.service + entrypoint + networkd"
mkdir -p "$ROOTFS/etc/systemd/network" "$ROOTFS/etc/systemd/system/multi-user.target.wants"

# systemd-networkd: DHCP on the nspawn host0 veth under --network-zone.
cat > "$ROOTFS/etc/systemd/network/10-host0.network" <<'NF'
[Match]
Name=host0

[Network]
DHCP=ipv4
NF

# Disable systemd-resolved so it does not clobber the bind-mounted /etc/resolv.conf.
rm -f "$ROOTFS/etc/systemd/system/multi-user.target.wants/systemd-resolved.service"
ln -sf /dev/null "$ROOTFS/etc/systemd/system/systemd-resolved.service"

# Deterministic DNS: in a private netns the old host bind-ro /etc/resolv.conf
# would point at 127.0.0.53 (the container's own loopback), so bake real
# resolvers that reach the internet via the host NAT instead. $ROOTFS/etc/hosts
# already provides localhost via the finalize step below.
rm -f "$ROOTFS/etc/resolv.conf"
printf 'nameserver 8.8.8.8\nnameserver 1.1.1.1\n' > "$ROOTFS/etc/resolv.conf"

# entrypoint: pull the JIT config from the protected bind-mounted file
# (argv-free) and run the official runner run.sh.
cat > "$ROOTFS/opt/actions-runner/entrypoint.sh" <<'EP'
#!/bin/bash
set -euo pipefail
if [[ -n "${JITCONFIG_FILE:-}" && -f "${JITCONFIG_FILE}" ]]; then
  export ACTIONS_RUNNER_INPUT_JITCONFIG="$(cat "${JITCONFIG_FILE}")"
fi
exec /opt/actions-runner/run.sh
EP
chmod 755 "$ROOTFS/opt/actions-runner/entrypoint.sh"

# actions-runner.service: one job per container; powers off (tears down nspawn)
# when the runner exits.
cat > "$ROOTFS/etc/systemd/system/actions-runner.service" <<'UN'
[Unit]
Description=GitHub Actions runner (single job)
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=runner
Group=runner
Environment=JITCONFIG_FILE=/opt/actions-runner/.jitconfig
ExecStart=/opt/actions-runner/entrypoint.sh
TimeoutStopSec=7200
ExecStopPost=/usr/bin/systemctl poweroff

[Install]
WantedBy=multi-user.target
UN

# Enable networkd + the runner unit (symlinks work in both chroot and --boot).
ln -sf /lib/systemd/system/systemd-networkd.service "$ROOTFS/etc/systemd/system/multi-user.target.wants/systemd-networkd.service"
ln -sf /etc/systemd/system/actions-runner.service "$ROOTFS/etc/systemd/system/multi-user.target.wants/actions-runner.service"
chown -R 1001:1001 "$ROOTFS/opt/actions-runner"

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