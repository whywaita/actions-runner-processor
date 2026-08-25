#!/bin/sh
#
# setup.sh — install actions-runner-processor from a GitHub Release and wire it
# up as a systemd service.
#
# Usage:
#   sudo bash deploy/setup.sh [VERSION]
#
# Without VERSION the latest published release is used. This script:
#   1. Detects the architecture and downloads the matching .deb from
#      the GitHub Release (actions-runner-processor_<ver>_<arch>.deb).
#   2. Installs it with apt (pulls in systemd-container + config + unit file).
#   3. Creates /etc/actions-runner-processor/config.yaml from the packaged
#      example if none exists yet.
#   4. Masks nothing; enables and starts the actions-runner-processor service.
#
# Configuration (GitHub App ID + private key path) must be filled in at
# /etc/actions-runner-processor/config.yaml before the service can register.

set -eu

REPO="whywaita/actions-runner-processor"
VERSION="${1:-}"

if [ "$(id -u)" -ne 0 ]; then
  echo "error: run as root (installs packages and systemd units): sudo bash $0" >&2
  exit 1
fi

# --- Resolve version (latest release if unspecified) -------------------------
if [ -z "$VERSION" ]; then
  VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep -oP '"tag_name":\s*"\K[^"]+')"
  echo ">>> latest release: $VERSION"
else
  case "$VERSION" in
    v*) : ;; *) VERSION="v${VERSION}" ;; esac
fi

# --- Architecture -------------------------------------------------------------
case "$(uname -m)" in
  x86_64)  ARCH="amd64" ;;
  aarch64) ARCH="arm64" ;;
  *) echo "error: unsupported arch $(uname -m)" >&2; exit 1 ;;
esac

# --- Download + install .deb ---------------------------------------------------
DEB="actions-runner-processor_${VERSION#v}_${ARCH}.deb"
URL="https://github.com/${REPO}/releases/download/${VERSION}/${DEB}"
echo ">>> downloading ${URL}"
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT
curl -fL "${URL}" -o "${TMPDIR}/${DEB}"

echo ">>> installing ${DEB} (pulls systemd-container as a dependency)"
DEBIAN_FRONTEND=noninteractive apt-get update -y -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "${TMPDIR}/${DEB}"

# --- Config ---------------------------------------------------------------------
# The .deb package (nfpm) installs an example config at
# /etc/actions-runner-processor/config.yaml (noreplace), which the operator
# edits with their GitHub App credentials.
CONF="/etc/actions-runner-processor/config.yaml"
chmod 600 "$CONF" 2>/dev/null || true

# --- Install a runner image if missing ----------------------------------------
if [ ! -d /opt/runner-btrfs/image ] || [ -z "$(ls -A /opt/runner-btrfs/image 2>/dev/null)" ]; then
  echo ">>> /opt/runner-btrfs/image is empty — the processor cannot boot runners until"
  echo "    you build a custom image. See DESIGN.md §7 (image/build-image.sh)."
fi

# --- Host networking for the private runner zone ------------------------------
# Runners boot with --network-zone=runner (private netns). Outbound internet
# requires ip_forward + a MASQUERADE rule on this host, otherwise the container
# cannot reach GitHub. deploy/deploy.sh (source deploys) does the same; keep
# this installer in lockstep.
UPLINK="$(ip route | awk '/default/{print $5; exit}')"
if [ -n "$UPLINK" ]; then
  echo ">>> configuring host NAT for runner zone (egress ${UPLINK})"
  printf 'net.ipv4.ip_forward=1\n' > /etc/sysctl.d/99-actions-runner-forward.conf
  sysctl -w net.ipv4.ip_forward=1 >/dev/null
  cat > /etc/systemd/system/actions-runner-nat.service <<UNIT
[Unit]
Description=actions-runner-processor: NAT for the nspawn runner zone
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh -c 'iptables -t nat -C POSTROUTING -o "$UPLINK" -j MASQUERADE 2>/dev/null || iptables -t nat -A POSTROUTING -o "$UPLINK" -j MASQUERADE'

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  systemctl enable --now actions-runner-nat.service >/dev/null
  echo "   ip_forward=1, MASQUERADE on ${UPLINK}"
else
  echo ">>> WARNING: no default-route interface detected; skipped NAT setup" >&2
fi

# --- Systemd unit ----------------------------------------------------------------
systemctl daemon-reload
systemctl enable actions-runner-processor.service
echo
echo "Done. Next steps:"
echo "  1. Edit ${CONF}"
echo "       github.client_id        = your GitHub App ID"
echo "       github.private_key_path = path to the App's .pem key"
echo "  2. Place the .pem key there, e.g."
echo "       mkdir -p /etc/actions-runner-processor"
echo "       install -m 600 /path/github-app.pem /etc/actions-runner-processor/github-app.pem"
echo "  3. Start the service:"
echo "       systemctl start actions-runner-processor"
echo "  4. Check it: systemctl status actions-runner-processor"