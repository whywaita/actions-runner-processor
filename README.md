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
| **sudo works** | The runner runs as root inside the container, so `sudo` is available. |

## Quick Start

### Prerequisites

```bash
apt install systemd-container

# Build a custom runner image (root filesystem) and place it at /opt/runner/image.
# The image must contain actions/runner preinstalled at /opt/actions-runner
# (the default entrypoint is /opt/actions-runner/run.sh).
```

### Building a custom runner image

`runner.image_path` must point to a root filesystem tree that contains the
`actions/runner` binary. A quick way to build one:

```bash
# 1. debootstrap a base rootfs
sudo debootstrap --variant=minbase noble /opt/runner/work-rootfs

# 2. download and extract actions/runner
sudo mkdir -p /opt/runner/work-rootfs/opt/actions-runner
curl -L "https://github.com/actions/runner/releases/download/v2.326.0/actions-runner-linux-x64-2.326.0.tar.gz" \
  | sudo tar xz -C /opt/runner/work-rootfs/opt/actions-runner

# 3. atomically place it (so nspawn never sees a partial tree)
sudo rm -rf /opt/runner/image
sudo mv /opt/runner/work-rootfs /opt/runner/image
```

For a full GitHub-hosted-compatible toolset, build with `distrobuilder` from
the actions-runner-images recipe and copy the resulting rootfs to
`/opt/runner/image`.

### Configuration

Create `/etc/actions-runner-processor/config.yaml`:

```yaml
github:
  client_id: "123456"                             # GitHub App App ID
  private_key_path: "/etc/actions-runner-processor/github-app.pem"

scale_set_name: "actions-runner-processor"

runner:
  mode: "nspawn"                                   # sandbox backend: "nspawn" (default) or "bwrap"
  image_path: "/opt/runner/image"                  # custom runner image rootfs
  entrypoint: "/opt/actions-runner/run.sh"         # in-container boot command
  max_runners: 4                                   # 0 = runtime.NumCPU()
  min_runners: 0                                   # warm idle runners

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

| Permission | Reason |
|-----------|--------|
| `administration:read` | Read installation info, manage runner groups/scale sets |
| `organization_self_hosted_runners:write` | Issue runner registration tokens |

### Install

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
  --directory=/opt/runner/image \      # custom image (read-only lower)
  --volatile=overlay \                 # ephemeral overlay root (changes discarded on exit)
  --as-pid2 \                          # run entrypoint as PID 2
  --setenv=ACTIONS_RUNNER_INPUT_JITCONFIG=<jit> \
  --machine=runner-<name> \
  --bind-ro=/etc/resolv.conf \
  /opt/actions-runner/run.sh
```

- **Custom image** — the image directory is the base (lower) layer.
- **Ephemeral** — all writes go to a private overlay layer discarded on exit, so jobs can `apt install` and mutate `/usr` without affecting other jobs or the host.
- **Network** — shares the host network namespace (no `--private-network`), so the runner reaches GitHub over outbound HTTPS only.
- **sudo** — runs as root inside the container; `sudo` works as expected in job steps.
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
- **release** — tagpr + GoReleaser on main push

GoReleaser produces `actions-runner-processor_<version>_linux_<arch>.tar.gz`
artifacts for `amd64` and `arm64`.

## License

MIT