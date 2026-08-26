#!/bin/bash
# deploy.sh — end-to-end deploy of actions-runner-processor on a host.
#
# Idempotent. Run on the target Ubuntu host as a user with passwordless sudo.
# It takes the stack to the latest source in this repo, applying the systemd-boot
# runtime (--boot + private-netns --network-zone + JIT-via-protected-file):
#
#   1. host networking: ip_forward + outbound NAT for the private runner zone
#   2. build/install the processor binary (from this repo's Go source)
#   3. (optional) rebuild the runner image via CI and install it as a btrfs
#      subvolume — required after an image-layout change (e.g. this systemd
#      runtime). ~1h because it is the full GitHub-hosted-compatible image.
#   4. configure runner.image_path
#   5. restart the service and verify it comes up with a green preflight
#
# Flags / env:
#   UPDATE_IMAGE=1   dispatch + download + install a freshly built full image
#                    (skip with SKIP_IMAGE for code-only deploys). off by default.
#   GITHUB_REPO=…    repo whose CI builds the image (default whywaita/actions-runner-processor)
#   RUNNER_IMAGE_PATH=…  image subvolume path (default /opt/runner-btrfs/image)
#   UPLINK=eth0      host egress interface for NAT (default: first default-route iface)
#   ZONE=runner      per-runner nspawn zones share the NAT/ip_forward (host NATs
#                    all of them outbound; each runner gets its own bridge)
#   DNS1/DNS2=…      resolvers baked/verified into the container resolv.conf
#   BINARY_PATH=…    use a prebuilt binary instead of compiling
#   SKIP_IMAGE=1     no image work (binary+network+config+restart only)
#
# Run:
#   sudo bash deploy/deploy.sh          # code-only: network + binary + restart
#   UPDATE_IMAGE=1 sudo bash deploy/deploy.sh   # + rebuild full image via CI
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

BINARY="${BINARY:-/usr/bin/actions-runner-processor}"
SERVICE="${SERVICE:-actions-runner-processor.service}"
CONFIG="${CONFIG:-/etc/actions-runner-processor/config.yaml}"
IMAGE_MOUNT="${IMAGE_MOUNT:-/opt/runner-btrfs}"
IMAGE_SUBVOL="${RUNNER_IMAGE_PATH:-$IMAGE_MOUNT/image}"

ZONE="${ZONE:-runner}"
UPLINK="${UPLINK:-}"
DNS1="${DNS1:-8.8.8.8}"
DNS2="${DNS2:-1.1.1.1}"

GITHUB_REPO="${GITHUB_REPO:-whywaita/actions-runner-processor}"

if [[ "$(id -u)" != "0" ]]; then
  echo "error: run as root (or with passwordless sudo)" >&2
  exit 1
fi

log() { echo ">>> $*"; }
die() { echo "error: $*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# 1. Host networking for the private network zone
# ---------------------------------------------------------------------------
ensure_network() {
  log "configure host networking (ip_forward + NAT for zone '$ZONE')"

  # ip_forward persisted.
  printf 'net.ipv4.ip_forward=1\n' > /etc/sysctl.d/99-actions-runner-forward.conf
  sysctl -w net.ipv4.ip_forward=1 >/dev/null

  # Egress interface for MASQUERADE.
  if [[ -z "$UPLINK" ]]; then
    UPLINK="$(ip route | awk '/default/{print $5; exit}')"
    [[ -n "$UPLINK" ]] || die "could not detect the default-route interface (set UPLINK=)"
  fi

  # A persistent oneshot that (re-)adds the masquerade rule on every boot.
  cat > /etc/systemd/system/actions-runner-nat.service <<UNIT
[Unit]
Description=actions-runner-processor: NAT for the nspawn runner zone
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/bash -c 'iptables -t nat -C POSTROUTING -o "$UPLINK" -j MASQUERADE 2>/dev/null || iptables -t nat -A POSTROUTING -o "$UPLINK" -j MASQUERADE'

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  systemctl enable --now actions-runner-nat.service >/dev/null

  echo "   ip_forward=1, MASQUERADE on $UPLINK"
}

# ---------------------------------------------------------------------------
# 2. Build / install the processor binary
# ---------------------------------------------------------------------------
ensure_binary() {
  log "install processor binary from this repo"

  local want_sha=""
  if [[ -f "$BINARY" ]]; then
    want_sha="$(sha256sum "$BINARY" | awk '{print $1}')"
  fi
  local built_sha=""
  if [[ -n "${BINARY_PATH:-}" && -f "$BINARY_PATH" ]]; then
    log "   installing supplied binary from $BINARY_PATH"
    cp "$BINARY_PATH" "$BINARY.tmp"
    mv "$BINARY.tmp" "$BINARY"; chmod 755 "$BINARY"
    built_sha="$(sha256sum "$BINARY" | awk '{print $1}')"
  elif [[ -d "$REPO_ROOT/cmd/actions-runner-processor" ]]; then
    command -v go >/dev/null 2>&1 || die "go not installed; build the binary and pass BINARY_PATH=, or install go"
    log "   building $BINARY from $REPO_ROOT"
    ( cd "$REPO_ROOT" && go build -o "$BINARY.tmp" ./cmd/actions-runner-processor )
    mv "$BINARY.tmp" "$BINARY"; chmod 755 "$BINARY"
    built_sha="$(sha256sum "$BINARY" | awk '{print $1}')"
  else
    die "no source under $REPO_ROOT and no BINARY_PATH=; nothing to install"
  fi

  if [[ -n "$want_sha" && "$want_sha" == "$built_sha" ]]; then
    log "   binary unchanged ($built_sha); no restart"
    return 0
  fi
  BINARY_SHA="$built_sha"
  log "   installed $BINARY  sha256=$built_sha"
}

# ---------------------------------------------------------------------------
# 3. (optional) rebuild + install the runner image
# ---------------------------------------------------------------------------
ensure_image() {
  if [[ "${SKIP_IMAGE:-0}" == "1" ]]; then
    log "SKIP_IMAGE=1: leaving image as-is"
    return 0
  fi
  if [[ -z "$(ls -A "$IMAGE_SUBVOL" 2>/dev/null)" ]]; then
    if [[ "${UPDATE_IMAGE:-0}" != "1" ]]; then
      die "image subvolume $IMAGE_SUBVOL is empty; set UPDATE_IMAGE=1 to build it"
    fi
  elif [[ "${UPDATE_IMAGE:-0}" != "1" ]]; then
    log "image present at $IMAGE_SUBVOL; skipping (UPDATE_IMAGE=1 to rebuild)"
    return 0
  fi

  log "dispatch full image build on $GITHUB_REPO via GitHub Actions (≈1h)"
  command -v gh >/dev/null 2>&1 || die "gh CLI required for UPDATE_IMAGE=1"
  gh workflow run build-image-full.yaml --repo "$GITHUB_REPO" --field release=24.04
  echo "   waiting for the full image build to finish… (this takes ~1h)"
  sleep 30
  local run_id
  run_id="$(gh run list --repo "$GITHUB_REPO" --workflow build-image-full.yaml --limit 1 --json databaseId --jq '.[0].databaseId')"
  gh run watch "$run_id" --repo "$GITHUB_REPO" --exit-status || die "image build failed ($run_id)"

  local artdir
  artdir="$(mktemp -d)"
  gh run download "$run_id" --repo "$GITHUB_REPO" -D "$artdir"
  local tarball
  tarball="$(find "$artdir" -name 'actions-runner-image-full-*.tar.gz' | head -1)"
  [[ -n "$tarball" ]] || die "full image artifact not found in run $run_id"

  install_image_rootfs "$tarball"
  rm -rf "$artdir"
}

# Expand the rootfs tarball into the image subvolume (replace contents).
install_image_rootfs() {
  local tarball="$1" tmpdir
  # Provision the btrfs backing mount + image subvolume before extracting, so a
  # clean host never runs mktemp against a nonexistent directory. The image MUST
  # be a btrfs subvolume for systemd-nspawn --ephemeral CoW snapshots -- btrfs is
  # enforced (no ext4 copy-per-job fallback); see deploy/setup.sh which creates a
  # loopback btrfs backing at $IMAGE_MOUNT.
  mkdir -p "$IMAGE_MOUNT"
  if ! btrfs subvolume show "$IMAGE_SUBVOL" >/dev/null 2>&1; then
    btrfs subvolume create "$IMAGE_SUBVOL" 2>/dev/null \
      || die "$IMAGE_MOUNT is not on a btrfs filesystem; the runner image must be a btrfs subvolume (btrfs is enforced). Create a btrfs mount at $IMAGE_MOUNT first (see deploy/setup.sh)."
  fi
  tmpdir="$(mktemp -d "$IMAGE_MOUNT/.extract.XXXXXX")"
  log "expanding $tarball into $IMAGE_SUBVOL"
  tar -xzf "$tarball" -C "$tmpdir"
  # swap the subvolume contents (the old snapshot dirs are discarded by nspawn)
  find "$IMAGE_SUBVOL" -mindepth 1 -maxdepth 1 -exec rm -rf {} +
  cp -a "$tmpdir"/. "$IMAGE_SUBVOL"/
  rm -rf "$tmpdir"
  log "   image installed at $IMAGE_SUBVOL ($(du -sh "$IMAGE_SUBVOL" | cut -f1))"
}

# ---------------------------------------------------------------------------
# 4. Configuration
# ---------------------------------------------------------------------------
ensure_config() {
  log "ensure config image_path=$IMAGE_SUBVOL"
  mkdir -p "$(dirname "$CONFIG")"
  if [[ ! -f "$CONFIG" ]]; then
    install -m 0640 "$SCRIPT_DIR/config.example.yaml" "$CONFIG"
  fi
  # set/insert runner.image_path under the `runner:` key.
  python3 - "$CONFIG" "$IMAGE_SUBVOL" <<'PY' || die "failed to update $CONFIG"
import sys
p, img = sys.argv[1], sys.argv[2]
src = open(p).read().splitlines()
out, in_runner = [], False
have = False
for ln in src:
    if ln.rstrip() == "runner:":
        in_runner = True
        out.append(ln); continue
    # End the runner section at the next non-indented, non-blank (top-level)
    # key. Previously the condition never matched top-level keys like
    # "metrics:", so image_path was appended under the wrong section and
    # silently ignored.
    if in_runner and ln.strip() and not ln.startswith(" "):
        # Insert runner.image_path before leaving the section, otherwise an
        # existing config that lacks image_path would never get it added (the
        # runner would fall back to the wrong default image).
        if not have:
            out.append('  image_path: "%s"' % img)
            have = True
        in_runner = False
    if not in_runner:
        out.append(ln); continue
    if ln.strip().startswith("image_path:"):
        out.append('  image_path: "%s"' % img); have = True; continue
    out.append(ln)
if in_runner and not have:
    out.append('  image_path: "%s"' % img)
open(p, "w").write("\n".join(out) + "\n")
PY
}

# ---------------------------------------------------------------------------
# 4b. Install the systemd unit (source deployments only). The packaged .deb
# handles this via postinst; a fresh host flow of deploy.sh never had it, so
# `systemctl restart "$SERVICE"` would fail with unit-not-found.
# ---------------------------------------------------------------------------
ensure_unit() {
  local src="$SCRIPT_DIR/actions-runner-processor.service"
  [[ -f "$src" ]] || die "systemd unit not found: $src"
  install -m 0644 "$src" "/etc/systemd/system/$SERVICE"
  log "installed systemd unit $SERVICE"
}

# ---------------------------------------------------------------------------
# 5. Restart + verify
# ---------------------------------------------------------------------------
restart_service() {
  log "restart $SERVICE"
  systemctl daemon-reload
  systemctl enable "$SERVICE" >/dev/null 2>&1 || true
  systemctl restart "$SERVICE"
  sleep 3
  systemctl is-active "$SERVICE" >/dev/null || die "$SERVICE not active"
  # preflight should confirm the image dir exists.
  journalctl -u "$SERVICE" --no-pager -n 40 | grep -q 'preflight check passed.*image directory' \
    || echo "   (warning: image-directory preflight not seen yet)"
  echo "   $SERVICE active"
}

main() {
  ensure_network
  ensure_binary
  ensure_unit
  ensure_image
  ensure_config
  restart_service
  echo ""
  echo "Deploy complete."
  echo "  binary:  $BINARY"
  [[ -n "${BINARY_SHA:-}" ]] && echo "  sha256:  $BINARY_SHA"
  echo "  image:   $IMAGE_SUBVOL"
  echo "  watch:   journalctl -u $SERVICE -f"
}
main "$@"