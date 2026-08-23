#!/bin/bash
# Build a full GitHub-hosted-compatible runner rootfs for systemd-nspawn.
#
# actions/runner-images is built by running images/ubuntu/scripts/build/*.sh
# in order (its packer templates are just a loop over these scripts). So we
# boot a debootstrap base in a systemd-nspawn container, run the same build
# scripts directly inside it, and export the resulting rootfs .tar.gz for
# systemd-nspawn (--directory=). No LXD, no packer, no setup-lxd needed.
#
# Heavy build: many toolchains, ~50GB+, roughly an hour per image. Trigger via
# CI (workflow_dispatch) rather than on every commit.
#
# Requires: root, systemd-container, debootstrap, curl, git.
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

RELEASE="$(get release)"          # 22.04 / 24.04
ARCH="$(get arch)"                # amd64
RUNNER_IMAGES_REF="$(get runner_images_ref)"
RUNNER_VERSION="$(get runner_version)"

if [[ -z "$RELEASE" || "$ARCH" != "amd64" ]]; then
  echo "error: manifest must set release and arch=amd64" >&2
  exit 1
fi
if [[ "$(id -u)" != "0" ]]; then
  echo "error: must run as root (debootstrap + systemd-nspawn)" >&2
  exit 1
fi

command -v systemd-nspawn >/dev/null 2>&1 || { echo "error: systemd-nspawn (systemd-container) not installed"; exit 1; }
command -v debootstrap >/dev/null 2>&1 || { echo "error: debootstrap not installed"; exit 1; }
command -v curl >/dev/null 2>&1 || { echo "error: curl not installed"; exit 1; }
command -v git >/dev/null 2>&1 || { echo "error: git not installed"; exit 1; }

WORK="$OUTPUT_DIR/work"
MACHINE="runner-build-$(date +%s)"
rm -rf "$WORK"; mkdir -p "$WORK" "$OUTPUT_DIR"

# Map release codename for debootstrap.
CODENAME="noble"
[[ "$RELEASE" == "22.04" ]] && CODENAME="jammy"

echo ">>> clone actions/runner-images"
if [[ -d "$WORK/runner-images" ]]; then
  rm -rf "$WORK/runner-images"
fi
git clone --quiet --depth 1 "https://github.com/actions/runner-images" "$WORK/runner-images"
if [[ "$RUNNER_IMAGES_REF" != "latest" ]]; then
  git -C "$WORK/runner-images" fetch --quiet --depth 1 origin "tag/${RUNNER_IMAGES_REF}" 2>/dev/null \
    || git -C "$WORK/runner-images" checkout --quiet "$RUNNER_IMAGES_REF"
fi

echo ">>> debootstrap base ($RELEASE, $CODENAME, $ARCH)"
debootstrap --variant=minbase --arch="$ARCH" "$CODENAME" "$WORK/rootfs" "http://archive.ubuntu.com/ubuntu/"

cp /etc/resolv.conf "$WORK/rootfs/etc/resolv.conf" 2>/dev/null || true

echo ">>> boot base in systemd-nspawn and provision"
cat > "$WORK/provision.sh" <<'PROVISION'
#!/bin/bash
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
# Print the failing line on error so a silent `set -e` exit is diagnosable.
trap 'echo ">>> PROVISION FAILED at line $LINENO: $BASH_COMMAND" >&2' ERR

# Basic tooling the runner-images scripts assume.
apt-get update -y -qq
apt-get install -y -qq sudo git curl jq ca-certificates locales wget lsb-release software-properties-common gnupg apt-transport-https build-essential

# GitHub-hosted Ubuntu 24.04 (noble) images manage apt sources through the
# deb822 file /etc/apt/sources.list.d/ubuntu.sources; debootstrap's minbase
# leaves a legacy /etc/apt/sources.list instead. configure-apt-sources.sh and
# configure-apt.sh operate on ubuntu.sources for >=24.04, so mirror the
# GitHub layout: write the deb822 ubuntu.sources and drop the legacy one.
if [ "${RELEASE}" != "22.04" ]; then
  CODENAME_REL="$(grep VERSION_CODENAME /etc/os-release | cut -d= -f2)"
  cat > /etc/apt/sources.list.d/ubuntu.sources <<APTEOF
Types: deb
URIs: http://archive.ubuntu.com/ubuntu/
Suites: ${CODENAME_REL} ${CODENAME_REL}-updates ${CODENAME_REL}-backports
Components: main restricted universe multiverse
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg

Types: deb
URIs: http://security.ubuntu.com/ubuntu/
Suites: ${CODENAME_REL}-security
Components: main restricted universe multiverse
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
APTEOF
  rm -f /etc/apt/sources.list
  apt-get update -y || { echo ">>> apt-get update failed after writing deb822 sources" >&2; false; }
fi

# Install the GitHub Actions runner (used as the boot entrypoint).
if [ -n "${RUNNER_VERSION:-}" ] && [ "${RUNNER_VERSION}" != "latest" ]; then
  VER="${RUNNER_VERSION}"
else
  VER="$(curl -sL https://api.github.com/repos/actions/runner/releases/latest | grep -oP '"tag_name":\s*"\K[^"]+')"
fi
TRIM="${VER#v}"
# GitHub names runner linux binaries with x64/arm64 (not amd64).
case "$ARCH" in
  amd64) RUNNER_ARCH="x64" ;;
  arm64) RUNNER_ARCH="arm64" ;;
  *)     RUNNER_ARCH="$ARCH" ;;
esac
mkdir -p /opt/actions-runner
curl -sL "https://github.com/actions/runner/releases/download/${VER}/actions-runner-linux-${RUNNER_ARCH}-${TRIM}.tar.gz" \
  | tar xz -C /opt/actions-runner
chmod +x /opt/actions-runner/run.sh

# Fresh OS inside the container; run every runner-images build script in a
# stable, conventional order, mirroring what packer does in
# images/ubuntu/templates/build.ubuntu-*.pkr.hcl:
#   INSTALLER_SCRIPT_FOLDER <- scripts/build
#   HELPER_SCRIPTS          <- scripts/helpers
#   INSTALLER_SCRIPT_FOLDER/toolset.json <- toolsets/toolset-<release>.json
# Build scripts are run from the runner-images checkout; INSTALLER_SCRIPT_FOLDER
# is where tools are installed to and where toolset.json is read from.
export INSTALLER_SCRIPT_FOLDER=/opt/runner-images-scripts
mkdir -p "$INSTALLER_SCRIPT_FOLDER"
cp -R /runner-images/images/ubuntu/scripts/build/* "$INSTALLER_SCRIPT_FOLDER/"
export HELPER_SCRIPTS="$INSTALLER_SCRIPT_FOLDER/helpers"
mkdir -p "$HELPER_SCRIPTS"
cp -R /runner-images/images/ubuntu/scripts/helpers/* "$HELPER_SCRIPTS/"
# packer places the per-release toolset at $INSTALLER_SCRIPT_FOLDER/toolset.json.
cp "/runner-images/images/ubuntu/toolsets/toolset-${RELEASE//./}.json" \
  "$INSTALLER_SCRIPT_FOLDER/toolset.json"
export IMAGE_OS=ubuntu
export IMAGE_VERSION="${RELEASE}"

cd /runner-images/images/ubuntu/scripts/build
for script in \
  configure-apt-mock.sh \
  install-ms-repos.sh \
  configure-apt-sources.sh \
  configure-apt.sh \
  configure-limits.sh \
  configure-image-data.sh \
  configure-environment.sh \
  configure-system.sh \
  configure-snap.sh \
  configure-pipx.sh \
  install-apt-vital.sh \
  install-powershell.sh \
  install-actions-cache.sh \
  install-apt-common.sh \
  install-azcopy.sh \
  install-azure-cli.sh \
  install-azure-devops-cli.sh \
  install-bicep.sh \
  install-apache.sh \
  install-aws-tools.sh \
  install-clang.sh \
  install-swift.sh \
  install-cmake.sh \
  install-codeql-bundle.sh \
  install-awf.sh \
  install-container-tools.sh \
  install-dotnetcore-sdk.sh \
  install-microsoft-edge.sh \
  install-gcc-compilers.sh \
  install-firefox.sh \
  install-gfortran.sh \
  install-git.sh \
  install-git-lfs.sh \
  install-github-cli.sh \
  install-google-chrome.sh \
  install-google-cloud-cli.sh \
  install-haskell.sh \
  install-java-tools.sh \
  install-kubernetes-tools.sh \
  install-miniconda.sh \
  install-kotlin.sh \
  install-mysql.sh \
  install-nginx.sh \
  install-nvm.sh \
  install-nodejs.sh \
  install-bazel.sh \
  install-php.sh \
  install-postgresql.sh \
  install-pulumi.sh \
  install-ruby.sh \
  install-rust.sh \
  install-julia.sh \
  install-selenium.sh \
  install-packer.sh \
  install-vcpkg.sh \
  configure-dpkg.sh \
  install-yq.sh \
  install-android-sdk.sh \
  install-pypy.sh \
  install-python.sh \
  install-zstd.sh \
  install-ninja.sh \
  install-docker.sh \
  ; do
    if [ -f "$script" ]; then
      echo "### running $script"
      # Run with bash -e so the script's shebang fail-fast semantics are honoured
      # even when invoked explicitly (plain "bash \$script" strips #! options).
      bash -e "$script" || { echo "FAILED: $script" >&2; exit 1; }
    else
      echo "### (skip) $script not present"
    fi
done

echo "PROVISION DONE"
# Ensure the container shuts down so systemd-nspawn returns on the host.
# (--as-pid2 boots a stub PID 1 that should terminate on exit; poweroff is
# belt-and-suspenders. Ignore failure so it can't mask a real build error.)
sync
poweroff || true
PROVISION
chmod +x "$WORK/provision.sh"

# Bind the repo into the container and run the provision script as PID 2.
systemd-nspawn \
  --directory="$WORK/rootfs" \
  --machine="$MACHINE" \
  --bind="$WORK/provision.sh:/runner-provision.sh" \
  --bind="$WORK/runner-images:/runner-images" \
  --setenv=RELEASE="$RELEASE" \
  --setenv=ARCH="$ARCH" \
  --setenv=RUNNER_VERSION="$RUNNER_VERSION" \
  --as-pid2 \
  /runner-provision.sh

echo ">>> export rootfs"
# Remove nspawn runtime mountpoints so they don't leak into the image.
rm -rf "$WORK/rootfs/proc" "$WORK/rootfs/sys"
mkdir -p "$WORK/rootfs/proc" "$WORK/rootfs/sys"

TARBALL="$OUTPUT_DIR/actions-runner-image-full-${ARCH}.tar.gz"
tar -czf "$TARBALL" -C "$WORK/rootfs" .

echo ""
echo "Done. Full image: $TARBALL"
echo "On the host:"
echo "  sudo rm -rf /opt/runner/image && sudo mkdir -p /opt/runner/image"
echo "  sudo tar -xzf $TARBALL -C /opt/runner/image"
echo "  config: runner.image_path=/opt/runner/image, runner.entrypoint=/opt/actions-runner/run.sh"