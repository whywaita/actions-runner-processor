#!/bin/bash
# Build a full GitHub-hosted-compatible runner rootfs for systemd-nspawn.
#
# This mirrors actions-runner-images-lxd: clone actions/runner-images, apply
# the LXD-adapted packer patch (lxd.patch), build the image as an LXD
# container with Packer, then export the container's rootfs directory as a
# .tar.gz for systemd-nspawn (--directory=).
#
# Heavy build: many toolchains, ~50GB+, roughly an hour per image. Trigger via
# CI (workflow_dispatch) rather than on every commit.
#
# Requires: root, LXD (snap), packer. On CI this runs on ubuntu-latest with
# setup-lxd. Locally: install lxd + packer and run as root.
#
# Usage:
#   bash image/build-image-full.sh [output-dir]

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFEST="${SCRIPT_DIR}/image-full.yaml"
OUTPUT_DIR="${OUTPUT_DIR:-${1:-/tmp/runner-image-full}}"

# --- Parse manifest ----------------------------------------------------------
get() {
  awk -v key="$1" '$1 == key ":" { sub(/^[^:]*:[[:space:]]*/, ""); gsub(/^"|"$/, ""); print; exit }' "$MANIFEST"
}

RELEASE="$(get release)"
ARCH="$(get arch)"
RUNNER_IMAGES_REF="$(get runner_images_ref)"
LXD_PATCH_REPO="$(get lxd_patch_repo)"
LXD_PATCH_REF="$(get lxd_patch_ref)"
LXD_PATCH_FILE="$(get lxd_patch_file)"

if [[ -z "$RELEASE" || "$ARCH" != "amd64" ]]; then
  echo "error: manifest must set release and arch=amd64" >&2
  exit 1
fi
if [[ "$(id -u)" == "0" ]]; then
  :
else
  # Non-root is fine as long as the caller has lxc access (socket group).
  # The build itself is done by the packer lxd builder.
  :
fi

NORM="${RELEASE//./_}"
WORK="$OUTPUT_DIR/work"
rm -rf "$WORK"; mkdir -p "$WORK" "$OUTPUT_DIR"

echo ">>> clone actions/runner-images"
git clone --quiet --depth 1 "https://github.com/actions/runner-images" "$WORK/runner-images"
cd "$WORK/runner-images"
if [[ "$RUNNER_IMAGES_REF" != "latest" ]]; then
  git fetch --quiet --depth 1 "origin" "tag/${RUNNER_IMAGES_REF}" 2>/dev/null \
    || git checkout --quiet "$RUNNER_IMAGES_REF"
fi

echo ">>> fetch and apply lxd.patch"
curl -sL "${LXD_PATCH_REPO}/raw/${LXD_PATCH_REF}/${LXD_PATCH_FILE}" -o lxd.patch
patch -p1 < lxd.patch >/dev/null

echo ">>> packer init + build (ubuntu ${RELEASE})"
cd ./images/ubuntu/templates
packer init .
packer validate -syntax-only -only "ubuntu-${RELEASE}.lxd.build_image_${NORM}" .
packer build -only "ubuntu-${RELEASE}.lxd.build_image_${NORM}" .

echo ">>> locate the packer-lxd container rootfs"
# The snap LXD container for the default project lives under this path
# (same path actions-runner-images-lxd passes to distrobuilder).
ROOTFS_CANDIDATES=(
  "/var/snap/lxd/common/lxd/storage-pools/default/containers/packer-lxd"
  "/var/lib/lxd/storage-pools/default/containers/packer-lxd"
)
FOUND=""
for cand in "${ROOTFS_CANDIDATES[@]}"; do
  if [ -d "$cand" ]; then FOUND="$cand"; break; fi
done
if [[ -z "$FOUND" ]]; then
  echo "error: could not locate packer-lxd container rootfs" >&2
  echo "  looked in: ${ROOTFS_CANDIDATES[*]}" >&2
  exit 1
fi
echo ">>> rootfs dir: $FOUND"

TARBALL="$OUTPUT_DIR/actions-runner-image-full-${ARCH}.tar.gz"
tar -czf "$TARBALL" -C "$FOUND" .

echo ""
echo "Done. Full image: $TARBALL"
echo "On the host:"
echo "  sudo rm -rf /opt/runner/image && sudo mkdir -p /opt/runner/image"
echo "  sudo tar -xzf $TARBALL -C /opt/runner/image"
echo "  config: runner.image_path=/opt/runner/image, runner.entrypoint=/opt/actions-runner/run.sh"