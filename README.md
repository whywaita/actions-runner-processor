# actions-runner-processor

A lightweight, single-binary GitHub Actions self-hosted runner processor
for homelab Linux VMs.

It uses the [actions/scaleset](https://github.com/actions/scaleset) Message
Session API (the same protocol as ARC) to detect queued jobs and launch
ephemeral runners inside [systemd-nspawn](https://systemd.io/) containers.

## Overview

```
                    GitHub Actions Service
                          │
          ┌───────────────┼───────────────┐
          │ Message Session│ Message Session│
          │ (Org A)        │ (Org B)        │
          │                │                │
 ┌────────┴────────────────┴────────────────┴──────┐
 │ Single Linux VM                                  │
 │                                                  │
 │  actions-runner-processor (one binary)            │
 │  ├── Listener goroutines (one per installation)   │
 │  ├── Prometheus /metrics (:9090)                  │
 │  ├── Web UI dashboard (:8080)                     │
 │  │                                                │
 │  └── systemd-nspawn containers (ephemeral, 1 job) │
 └──────────────────────────────────────────────────┘
```

### Key Design

| Principle | Description |
|-----------|-------------|
| **Single Binary** | One Go binary. `scp` it to a VM, write a systemd unit, done. |
| **No Inbound** | All communication is outbound HTTPS. Works behind NAT/firewall. |
| **Ephemeral Runners** | Each runner lives for exactly one job, then self-destructs. |
| **JIT Config** | No registration token needed. Runners boot directly from a JIT config. |
| **Custom Image** | Boot runners from your own root filesystem image (runner + tools). |
| **sudo works** | The runner boots as the `runner` user (GitHub layout) with passwordless sudo, so `sudo` is available in job steps. |

## Quick Start

### Prerequisites

```bash
apt install systemd-container

# Build a custom runner image (root filesystem) and place it at /opt/runner-btrfs/image
# (or a btrfs subvolume — see below). The image must contain actions/runner
# preinstalled at /opt/actions-runner and a systemd unit that boots it; the
# bundled image/build-*.sh scripts bake this in.
```

### Building a custom runner image

`runner.image_path` must point to a root filesystem tree that contains the
`actions/runner` binary. This repo ships a declarative image build under
`image/`:

- `image/image.yaml` — the manifest: base distro, architecture, `actions/runner`
  version, and the extra apt packages baked in.
- `image/build-image.sh` — debootsraps the base, provisions the packages and
  runner in a chroot, and packs the resulting rootfs into a `.tar.gz`.

**What the (lightweight) image is / contains:**

| Component | Where | Notes |
|---|---|---|
| Minimal Debian/Ubuntu base | `/` | `debootstrap --variant=minbase`; booted with **systemd as PID 1** (`--boot`) so `systemctl` works in jobs |
| `actions/runner` binary | `/opt/actions-runner` | Booted by a baked `actions-runner.service` (via `/opt/actions-runner/entrypoint.sh`) that reads the JIT config and runs the official `run.sh` |
| Extra apt packages | `/usr` | `build-essential`, git, curl, jq, python3, `sudo`, etc. — declared in `image.yaml`, baked in so jobs don't install them per run |
| Dedicated `runner` user | uid 1001, `/home/runner` | Passwordless sudo, mirrors the GitHub-hosted layout |

The image is booted **read-only** with an **ephemeral overlay**: jobs can
`apt install` and mutate `/usr` freely, but every write lands on a private
upper layer that is **discarded when the runner exits**, so one job never
affects another job or the host. Because the container boots as the `runner`
user with passwordless sudo, `sudo` works inside job steps.

**Build it locally** (needs root for debootstrap/chroot):

```bash
sudo OUTPUT_DIR=/tmp/img bash image/build-image.sh
# → /tmp/img/actions-runner-image-amd64.tar.gz
```

**Or let CI bake it** — `.github/workflows/build-image.yaml` runs
`image/build-image.sh` (on `workflow_dispatch`, or on push/PR touching
`image/**`) and uploads the rootfs tarball as an artifact. The full image uses
a separate workflow: `.github/workflows/build-image-full.yaml` (dispatch, or
auto-run from the release workflow so each Release ships a fresh full image).

**Expand to the host** (`runner.image_path`, default `/opt/runner-btrfs/image`):

```bash
sudo rm -rf /opt/runner-btrfs/image && sudo mkdir -p /opt/runner-btrfs/image
sudo tar -xzf actions-runner-image-amd64.tar.gz -C /opt/runner-btrfs/image
```

A quick manual way to prepare a base rootfs (equivalent to what the script does):

```bash
# 1. debootstrap a base rootfs
sudo debootstrap --variant=minbase noble /opt/runner/work-rootfs
# 2. download and extract actions/runner
sudo mkdir -p /opt/runner/work-rootfs/opt/actions-runner
curl -L "https://github.com/actions/runner/releases/download/v2.326.0/actions-runner-linux-x64-2.326.0.tar.gz" \
  | sudo tar xz -C /opt/runner/work-rootfs/opt/actions-runner
# 3. atomically place it (so nspawn never sees a partial tree)
sudo rm -rf /opt/runner-btrfs/image
sudo mv /opt/runner/work-rootfs /opt/runner-btrfs/image
```

For a full GitHub-hosted-compatible toolset (option A), this repo ships
`image/build-image-full.sh`: it debootstraps a base, boots it in a
systemd-nspawn container, and runs the `actions/runner-images`
`scripts/build/*.sh` scripts directly inside — no LXD or Packer needed
(`actions/runner-images` build scripts are just a sequence of shell scripts;
its Packer templates only loop over them). Trigger it via CI
(`workflow_dispatch` on the `build-image-full` workflow) or locally:

```bash
sudo bash image/build-image-full.sh /tmp/runner-image-full
# → /tmp/runner-image-full/actions-runner-image-full-amd64.tar.gz
```

The full image is too large for a single GitHub Release asset (2GB/file cap),
so it is built on demand (`build-image-full` workflow), split into <2GB parts,
and published to the repository's **newest release** as
`actions-runner-image-full-<arch>.tar.gz.part-000, .part-001, ...`.
Install it onto the runner host with:

```bash
# Default: newest release (no GitHub credentials needed)
sudo actions-runner-processor image install-full

# A specific release, by tag or release page URL
sudo actions-runner-processor image install-full --release v0.0.5
sudo actions-runner-processor image install-full --release https://github.com/whywaita/actions-runner-processor/releases/tag/v0.0.5

# Bound the maximum number of simultaneous part downloads (0 = all at once)
sudo actions-runner-processor image install-full --concurrency 4
```

There are also two lower-level sources:

```bash
# A single operator-hosted tarball
sudo actions-runner-processor image install-full --url https://example.com/actions-runner-image-full-amd64.tar.gz

# The latest build-image-full action artifact (GitHub App auth from config)
sudo actions-runner-processor image install-full --from-actions
```

All modes download, concatenate/reconstruct, expand into the btrfs runner-image
subvolume (`/opt/runner-btrfs/image`), and enforce the btrfs requirement.

Expand it to `runner.image_path` the same way as the lightweight image.

#### Preparing the btrfs backing (fresh machine)

`image install-full` and the processor both require the image path
(`--image-path`, default `/opt/runner-btrfs/image`) to live on, and be a
subvolume of, a **btrfs** filesystem. systemd-nspawn boots each runner with
`--ephemeral`, which CoW-snapshots the image subvolume per job; on an
ext4/non-subvolume backing that would degrade to a full copy per job, so it is
enforced rather than silently slow. Running it on a host where
`/opt/runner-btrfs` is not a btrfs mount fails with:

```
error: parent /opt/runner-btrfs is not on a btrfs filesystem (btrfs is enforced); mount a btrfs backing there (see deploy/setup.sh)
```

**Easiest path — install the `.deb` via `deploy/setup.sh`.** Its postinst
`ensure_btrfs()` provisions the backing for you: creates a loopback image
(`/var/lib/actions-runner-processor/runner-btrfs.img`), mounts it at
`/opt/runner-btrfs` through a systemd `.mount` unit (persistent across
reboot), and creates the `image` subvolume.

**Manual path — binary only.** Set the backing up yourself before running
`image install-full`:

```bash
sudo apt-get install -y btrfs-progs

# Create the loopback image (size it per the note below) and mount it at /opt/runner-btrfs.
sudo truncate -s 60G /var/lib/actions-runner-processor/runner-btrfs.img
sudo mkfs.btrfs /var/lib/actions-runner-processor/runner-btrfs.img
sudo mkdir -p /opt/runner-btrfs
sudo mount -o loop /var/lib/actions-runner-processor/runner-btrfs.img /opt/runner-btrfs

# Persist across reboot (systemd mount unit).
echo '/var/lib/actions-runner-processor/runner-btrfs.img /opt/runner-btrfs btrfs loop,nofail 0 0' \
  | sudo tee /etc/systemd/system/actions-runner-btrfs.mount
systemctl daemon-reload && systemctl enable actions-runner-btrfs.mount

# Create the runner-image subvolume that actions-runner-processor boots from.
sudo mkdir -p /opt/runner-btrfs/image
sudo btrfs subvolume create /opt/runner-btrfs/image
```

**Sizing matters.** `image install-full` reconstructs the split tarball
(~21G for the full image) then expands it (lightweight ~1.5G, **full
GitHub-hosted ~50G+**), plus per-job CoW upper layers. The deb postinstall's
default backing size is 20G — fine for the lightweight image but **too small
for the full image**. For the full image, provision a larger loopback (e.g.
60–80G) via the manual path above, or enlarge before installing.

Verify the backing before retrying:

```bash
findmnt -t btrfs /opt/runner-btrfs                # must show btrfs
sudo btrfs subvolume show /opt/runner-btrfs/image # must be a subvolume
```

### Configuration

Create `/etc/actions-runner-processor/config.yaml`:

```yaml
github:
  client_id: "123456"                             # GitHub App App ID
  private_key_path: "/etc/actions-runner-processor/github-app.pem"

scale_set_name: "actions-runner-processor"

runner:
  image_path: "/opt/runner-btrfs/image"            # custom runner image (btrfs subvolume rootfs)
  max_runners: 4                                   # 0 = runtime.NumCPU()
  min_runners: 0                                   # warm idle runners
  # How long to keep running on SIGTERM/SIGINT so in-flight jobs finish before
  # force-killing remaining runner containers (default 10m). Must be <= the
  # unit's TimeoutStopSec (default 660s).
  shutdown_grace_timeout: "10m"

metrics:
  enabled: true
  addr: ":9090"

webui:
  enabled: true
  addr: ":8080"
```

### GitHub Enterprise Server (GHES)

Set `github.api_url` and `github.url` to your enterprise server:

```yaml
github:
  client_id: "123456"
  private_key_path: "/etc/actions-runner-processor/github-app.pem"
  api_url: "https://github.mycompany.com/api/v3"
  url: "https://github.mycompany.com"
```

These default to `https://api.github.com` and `https://github.com` respectively
when unset.

### GitHub App Permissions

This app uses the same runner-scale-set + JIT model as ARC. The GitHub App
permissions needed depend on the **scope** the listener resolves for an
installation (the listener keys off the *URL path*, not the account type):

- **Organization / org-scoped** (`https://github.com/<org>`) — scale set for the
  whole org:

  | Permission | Access | Reason |
  |-----------|--------|--------|
  | `administration` | **Read** | Read installation info; create / read runner scale sets (`_apis/runtime/runnerscalesets`) |
  | `organization_self_hosted_runners` | **Write** | Generate JIT runner configs (`.../generatejitconfig`), acquire job messages (`.../sessions`), remove runner registrations |
  | `metadata` | Read | Standard minimum App permission (installations / repositories discovery) — usually present by default |

- **Repository-scoped** (`https://github.com/<user>/<repo>`, e.g. a personal
  account like `whywaita/sandbox`) — a per-repo runner:

  | Permission | Access | Reason |
  |-----------|--------|--------|
  | `administration` | **Write** | Manage the repo's self-hosted runners (runner-scale-set registrations for the repo) |
  | `metadata` | Read | Repositories discovery — present by default |

The listener auto-detects installations by account type (`Account.Type`):
- **User / personal** installations are expanded to **per-repository scopes**
  (`https://github.com/<user>/<repo>`), so each repo is served by its own
  repo-scoped runner → needs `administration: write`.
- **Organization** installations use the **org scope**
  (`https://github.com/<org>`) → needs `organization_self_hosted_runners: write`.

Note: unlike the legacy registration-token model, this app uses **JIT config**
(`generatejitconfig`), so no `actions/runners/registration-token` permission is
needed — the runner boots directly from a short-lived JIT token.

> **Repository-selected installs (a subset of repos) are still org-scoped.**
> When you install the app on an organization and select only *some*
> repositories (`repository_selection: "selected"`), the account type is still
> `Organization`, so the app keys off the org scope and provisions a scale set
> at the org level — the `repository_selection` field is not read. Control which
> repos get the runner via runner groups / repo-level self-hosted-runner access
> rather than the app's repository selection. This matches ARC behavior.

### Install

The easiest path is the setup script, which installs the latest release from
GitHub Releases as a `.deb` (pulls `systemd-container`, the example config, the
btrfs backing, and the systemd unit), provisions a **btrfs** runner-image
subvolume, fetches the prebuilt **lightweight** image from the Release, wires
host NAT for the runner zone, and enables the service:

```bash
curl -fsSL https://raw.githubusercontent.com/whywaita/actions-runner-processor/main/deploy/setup.sh | sudo bash
# or install a specific version:
curl -fsSL https://raw.githubusercontent.com/whywaita/actions-runner-processor/main/deploy/setup.sh | sudo bash -s v0.0.5
```

The image is **required to be a btrfs subvolume** (the processor enforces this
at startup and errors out otherwise), so running the runner on an ordinary ext4
path is not supported. `setup.sh` creates a loopback btrfs backing at
`/opt/runner-btrfs` automatically.

Then edit `/etc/actions-runner-processor/config.yaml` (set `github.client_id` /
`github.private_key_path`), place the GitHub App `.pem`, and:
`systemctl start actions-runner-processor`.

Manual install (from a binary you built yourself):

```bash
# Install binary
cp actions-runner-processor /opt/actions-runner-processor/
cp config.yaml /etc/actions-runner-processor/
cp github-app.pem /etc/actions-runner-processor/ && chmod 600 /etc/actions-runner-processor/github-app.pem

# Install systemd unit (runs as root so systemd-nspawn can create containers)
cp deploy/actions-runner-processor.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now actions-runner-processor
```

## Metrics

Prometheus-compatible metrics exposed at `:9090/metrics` (configurable):

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `actions_runner_processor_active_runners` | Gauge | `scope` | Active runners per installation |
| `actions_runner_processor_max_runners` | Gauge | `scope` | Configured maximum per installation |

## Web UI

A minimal embedded dashboard at `:8080` (configurable):

- `/` — HTML dashboard showing runner status per installation
- `/api/status` — JSON API

## Architecture

For detailed architecture documentation, see [DESIGN.md](DESIGN.md).

### Data Flow

```
1. GitHub → job queued
2. Message Session → job_available pushed via long-poll
3. Listener → AcquireJobs()
4. Listener → job_started received
5. Scaler → HandleDesiredRunnerCount() → startRunner()
6. Scaler → GenerateJitRunnerConfig() + systemd-nspawn container from image
7. Runner → JIT config boot in container → registers with GitHub → runs job
8. Runner → job completes → auto-deregister → container (ephemeral overlay) destroyed
9. Listener → job_completed received
10. Scaler → HandleJobCompleted() → cleanup runner registration
```

### Sandboxing

Each runner is booted in a `systemd-nspawn` container:

```bash
systemd-nspawn \
  --directory=/opt/runner-btrfs/image \   # custom image (a btrfs subvolume)
  --ephemeral \                            # CoW-snapshotted root; changes discarded on exit
  --boot \                                 # systemd as PID1: systemctl/dockerd work in jobs
--network-zone=rn-<runner-id> \             # per-runner private bridge; isolated from host and other jobs
  --capability=CAP_SYS_ADMIN,CAP_NET_ADMIN \   # dockerd (safe: netns is private)
  --bind-ro=/run/actions-runner-processor/runner-<name>.jitconfig:/opt/actions-runner/.jitconfig \
  --machine=runner-<name>
```

> `--ephemeral` CoW-snapshots the image directory onto real disk, boots the writable snapshot, and discards it on exit — so job writes (including `sudo apt install` into `/usr`) never touch a RAM overlay and can't hit `ENOSPC`. **btrfs is enforced**: the image must be a btrfs subvolume (the processor fails startup otherwise), and `deploy/setup.sh` provisions a loopback btrfs at `/opt/runner-btrfs` automatically. Set `runner.image_path` accordingly.

> **Privacy / DNS / networking**: the container is booted with `--boot` so job steps can `systemctl start docker`. `--network-zone=rn-<runner-id>` (a per-runner zone) puts it in its own network namespace on its own nspawn-managed bridge, so concurrent jobs are isolated from each other over L2; the host NATs outbound HTTPS out (see `deploy/deploy.sh`), and the image bakes a real `resolv.conf` (the old host `--bind-ro=/etc/resolv.conf` would point at the container's own loopback). The JIT credential is passed via a ro bind-mounted root-only file, never on the command line.

- **Custom image** — the image directory is a btrfs subvolume (the base, CoW layer).
- **Ephemeral** — each job boots a disposable CoW snapshot discarded on exit, so jobs can `apt install` and mutate `/usr` without affecting other jobs or the host.
- **Network** — `--network-zone=rn-<runner-id>` (per-runner zone) gives each runner a private network namespace on its own nspawn-managed bridge, isolated from the host AND from other concurrent jobs; the host NATs outbound HTTPS out. `CAP_NET_ADMIN`/`CAP_SYS_ADMIN` (for dockerd) can only touch the runner's own netns.
- **sudo** — the runner boots (via the baked `actions-runner.service`) as the `runner` user (GitHub layout); passwordless sudo makes `sudo` work as expected in job steps.
- The configuration file and GitHub App private key are masked with `/dev/null` inside the sandbox.
- Runner registrations are removed by ID whenever a runner process exits, including startup failures.

## Build

```bash
go build ./cmd/actions-runner-processor/
go test ./... -race
go vet ./...
golangci-lint run
```

## Deployment

GitHub Actions workflows (`build` + `release`) are in `.github/workflows/`:

- **build** — `go build`, `go test -race`, `go vet`, `golangci-lint` on PR
- **build-image** — lightweight image artifact on push/PR (`image/**`)
- **build-image-full** — heavy (1h, 50GB+) full-image build; dispatch-only,
  and auto-triggered from the `release` workflow after goreleaser so every
  GitHub Release ships with a fresh full image.
  Splits the tarball into <2GB parts and publishes them to the newest release
  via `gh release upload --clobber`
- **release** — tagpr + GoReleaser on main push or `v*` tag push

GoReleaser produces `actions-runner-processor_<version>_linux_<arch>.tar.gz`
artifacts for `amd64` and `arm64`. Pushing a prerelease tag (e.g.
`v0.0.5-rc2`) triggers the same release pipeline and GoReleaser marks it as a
pre-release automatically, packaging a freshly-built lightweight runner image
with it — useful for verifying a candidate on a real host before the final
tagpr release.

## License

MIT