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
# Use the default variant (not minbase) so systemd is installed in the rootfs;
# systemd-nspawn --boot requires systemd as PID 1 inside the container.
debootstrap --arch="$ARCH" "$CODENAME" "$WORK/rootfs" "http://archive.ubuntu.com/ubuntu/"

cp /etc/resolv.conf "$WORK/rootfs/etc/resolv.conf" 2>/dev/null || true

echo ">>> boot base in systemd-nspawn and provision"
cat > "$WORK/provision.sh" <<'PROVISION'
#!/bin/bash
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
# Write the final status to the bind-mounted /provision/status so the host
# can tell success from failure after the container powers itself off.
STATUS_FILE=/provision/status
poweroff_self() {
  # Ensure the container always powers itself off so systemd-nspawn on the host
  # returns and the CI job fails promptly instead of hanging until timeout.
  sync
  systemctl poweroff 2>/dev/null || true
}
on_fail() {
  echo ">>> PROVISION FAILED at line $LINENO: $BASH_COMMAND" >&2
  echo "FAILED:$LINENO" > "$STATUS_FILE" 2>/dev/null || true
  poweroff_self
}
finish_ok() {
  echo "PROVISION DONE" >&2
  echo "OK" > "$STATUS_FILE" 2>/dev/null || true
  poweroff_self
}
# Whatever path this script exits by (command failure, child-script failure, a
# stray 'exit', or a signal), make sure the container powers off so the host
# nspawn call returns and the job does not run until timeout. We only power off
# on exit when provisioning did not already complete OK (finish_ok calls
# poweroff itself and then exits, so this is idempotent).
trap 'on_fail' ERR
trap 'trap - EXIT; [ "$(cat "$STATUS_FILE" 2>/dev/null)" = "OK" ] || poweroff_self' EXIT

# Print filesystem usage before each build script so we can catch disk
# exhaustion (ENOSPC). GitHub-hosted runners have ~14GB; full toolchains are
# tight, so knowing the budget is essential when a download starts failing with
# curl: (23) (a write failed, almost always disk full).
df -h / /tmp 2>/dev/null | sed 's/^/DISK /' >&2

# systemd-nspawn mounts /tmp as a small tmpfs (RAM, typically ~1.6G). The
# runner-images build scripts download large tarballs (Swift ~500MB, clang,
# dotnet, etc.) into /tmp and this tiny tmpfs fills up, surfacing as
# "curl: (23) Failure writing output to destination". Unmount the tmpfs so the
# rootfs's real (145G) disk /tmp is used instead.
if mountpoint -q /tmp && awk '$2=="/tmp" && $3=="tmpfs" {found=1; exit} END{exit !found}' /proc/mounts; then
  echo ">>> /tmp is tmpfs; switching to real disk" >&2
  umount /tmp || { echo "warning: could not unmount /tmp tmpfs" >&2; }
fi
mkdir -p /tmp
df -h /tmp 2>/dev/null | sed 's/^/DISK(tmp)/' >&2

# Prevent apt package postinst hooks from trying to start system services.
# We are provisioning a headless nspawn container with no running systemd, so
# invoke-rc.d would block on dbus/systemd sockets (seen as a silent hang after
# package unpack). Returning 101 makes postinst skip service (re)start.
printf '#!/bin/sh\n# policy-rc.d: never start services during provisioning\nexit 101\n' > /usr/sbin/policy-rc.d
chmod +x /usr/sbin/policy-rc.d

# Make apt non-interactive for the whole build: several runner-images scripts
# call apt-get without -y, and with stdin closed that would abort on the
# "[Y/n] continue?" prompt. Force --assume-yes and quiet log level globally.
printf 'APT::Get::Assume-Yes "true";\nAPT::Get::Quiet "1";\n' > /etc/apt/apt.conf.d/90assumeyes

# Pin the debconf frontend to noninteractive so dpkg-reconfigure calls in
# runner-images scripts (e.g. configure-environment.sh reconfigures man-db)
# never block on an interactive prompt, and pre-seed the man-db answer so
# dpkg-reconfigure man-db succeeds without input.
export DEBIAN_FRONTEND=noninteractive
echo "man-db man-db/auto-update boolean false" | debconf-set-selections

# Basic tooling the runner-images scripts assume.
apt-get update -y -qq
apt-get install -y -qq sudo systemd systemd-sysv dbus git curl jq ca-certificates locales wget lsb-release software-properties-common gnupg apt-transport-https build-essential cloud-init needrestart man-db

# configure-environment.sh edits Azure-specific /etc/waagent.conf (swap
# settings). The nspawn rootfs has no Azure agent, so create an empty config
# for the sed -i edits to succeed as a no-op.
touch /etc/waagent.conf

# configure-environment.sh disables motd news metadata
# (sed ENABLED=1 -> ENABLED=0 on /etc/default/motd-news). update-motd is not
# installed, so create an empty file for the sed -i to succeed as a no-op.
touch /etc/default/motd-news

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
# packer sets both HELPER_SCRIPTS and HELPER_SCRIPT_FOLDER to the helper dir:
# some scripts (configure-environment.sh) read HELPER_SCRIPTS, others
# (configure-system.sh) read HELPER_SCRIPT_FOLDER.
export HELPER_SCRIPTS="$INSTALLER_SCRIPT_FOLDER/helpers"
export HELPER_SCRIPT_FOLDER="$INSTALLER_SCRIPT_FOLDER/helpers"
mkdir -p "$HELPER_SCRIPTS"
cp -R /runner-images/images/ubuntu/scripts/helpers/* "$HELPER_SCRIPTS/"
# Disable the Pester test harness: install-* scripts call invoke_tests (which
# runs PowerShell Pester tests) at the end. We build the image in a bare nspawn
# container with no test framework, so these would fail spuriously. Overwrite
# the helper with a no-op; configure-environment.sh symlinks it to PATH.
cat > "$HELPER_SCRIPTS/invoke-tests.sh" <<'INVA'
#!/bin/bash
# Test skeleton disabled for nspawn builds; image provisioning is the goal.
exit 0
INVA
chmod +x "$HELPER_SCRIPTS/invoke-tests.sh"
# packer places the per-release toolset at $INSTALLER_SCRIPT_FOLDER/toolset.json.
cp "/runner-images/images/ubuntu/toolsets/toolset-${RELEASE//./}.json" \
  "$INSTALLER_SCRIPT_FOLDER/toolset.json"
export IMAGE_OS=ubuntu
export IMAGE_VERSION="${RELEASE}"
# packer sets IMAGE_FOLDER=/imagegeneration; configure-system.sh chmods it.
export IMAGE_FOLDER=/imagegeneration
# packer passes IMAGEDATA_FILE to configure-image-data.sh.
export IMAGEDATA_FILE=/imagegeneration/imagedata.json
mkdir -p /imagegeneration

# packer uploads assets/post-gen to the image folder and renames it to
# post-generation; configure-system.sh moves it to /opt. Reproduce that here
# so the mv in configure-system.sh does not fail under set -e.
cp -R /runner-images/images/ubuntu/assets/post-gen /imagegeneration/post-gen
mv /imagegeneration/post-gen /imagegeneration/post-generation

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
      # Budget disk: apt archives from the previous install script are no
      # longer needed, and the runner-images scripts don't clean them. Free them
      # each step so ENOSPC doesn't hit on the next big download.
      apt-get clean 2>/dev/null || true
      df -h / 2>/dev/null | awk 'NR==1 || /\/$/' | sed 's/^/[disk] /' >&2
      # Run with bash -e so the script's shebang fail-fast semantics are honoured
      # even when invoked explicitly (plain "bash \$script" strips #! options).
      # The parent's trap ERR does NOT propagate into a child shell, so for
      # line-level diagnostics we launch with -E (errtrace) and inject an ERR
      # trap via BASH_ENV (read by non-interactive bash at startup), reporting
      # the failing line/command. We exec the script directly (not source) so
      # \$0 and function scoping behave exactly as a normal invocation.
      # This heredoc becomes $WORK/provision.sh and runs inside the container
      # where $WORK is unset, so use an absolute path under /tmp.
      export BASH_ENV=/tmp/bash-env-trap.sh
      cat > "$BASH_ENV" <<'TRAPEOF'
trap 'echo ">>> FAILED line $LINENO: $BASH_COMMAND" >&2; exit 1' ERR
TRAPEOF
      bash -eE "$script" || { echo "FAILED: $script" >&2; echo "FAILED:$script" > "$STATUS_FILE" 2>/dev/null || true; exit 1; }
    else
      echo "### (skip) $script not present"
    fi
done

echo "PROVISION DONE"
# Power the container off so systemd-nspawn returns on the host; the host reads
# the shared status file to learn success/failure.
finish_ok
PROVISION
chmod +x "$WORK/provision.sh"

# Register the provision script as a one-shot systemd unit so it runs once at
# container boot (--boot). The unit powers the machine off when done.
MKDIR_P="$WORK/rootfs/etc/systemd/system"
mkdir -p "$MKDIR_P"
cat > "$MKDIR_P/runner-provision.service" <<UNIT
[Unit]
Description=actions-runner-processor full image provisioning
After=multi-user.target

[Service]
Type=oneshot
Environment=DEBIAN_FRONTEND=noninteractive
Environment=RELEASE=${RELEASE}
Environment=ARCH=${ARCH}
Environment=RUNNER_VERSION=${RUNNER_VERSION}
ExecStart=/runner-provision.sh
TimeoutStopSec=300
StandardOutput=journal+console
StandardError=journal+console

[Install]
WantedBy=multi-user.target
UNIT
ln -sf /etc/systemd/system/runner-provision.service \
  "$WORK/rootfs/etc/systemd/system/multi-user.target.wants/runner-provision.service"

# Host-side status dir bind-mounted into the container.
mkdir -p "$WORK/status"

# Boot the container with real systemd (--boot) so package postinst hooks that
# call invoke-rc.d/systemctl operate against a running init (with policy-rc.d
# still short-circuiting service starts during provisioning).
#
# Note: snap/configure-snap.sh is intentionally skipped. snapd must mount
# squashfs images, which systemd-nspawn cannot do without CAP_SYS_ADMIN (and
# loop devices); we deliberately keep the container sandboxed. Snap-dependent
# tools are simply not installed in the full image.
systemd-nspawn \
  --directory="$WORK/rootfs" \
  --machine="$MACHINE" \
  --bind="$WORK/provision.sh:/runner-provision.sh" \
  --bind="$WORK/runner-images:/runner-images" \
  --bind="$WORK/status:/provision" \
  --setenv=RELEASE="$RELEASE" \
  --setenv=ARCH="$ARCH" \
  --setenv=RUNNER_VERSION="$RUNNER_VERSION" \
  --boot

# Verify the provisioning result via the bind-mounted status file written by
# the provision script (OK / FAILED:<line>) before exporting the rootfs.
if [ -f "$WORK/status/status" ]; then
  STATUS="$(cat "$WORK/status/status")"
  echo "provision status: $STATUS"
  case "$STATUS" in
    OK) : ;;
    FAILED:*) echo "error: provisioning failed in container ($STATUS)" >&2; exit 1 ;;
    *)  echo "error: unexpected provision status '$STATUS'" >&2; exit 1 ;;
  esac
else
  echo "error: container did not write a provision status file" >&2
  exit 1
fi

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