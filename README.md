# actions-runner-processor

A lightweight, single-binary GitHub Actions self-hosted runner processor
for homelab Linux VMs.

It uses the [actions/scaleset](https://github.com/actions/scaleset) Message
Session API (the same protocol as ARC) to detect queued jobs and launch
ephemeral runners inside [bubblewrap](https://github.com/containers/bubblewrap)
sandboxes.

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
 │  └── bubblewrap sandboxes (ephemeral, 1 job each) │
 └──────────────────────────────────────────────────┘
```

### Key Design

| Principle | Description |
|-----------|-------------|
| **Single Binary** | One Go binary. `scp` it to a VM, write a systemd unit, done. |
| **No Inbound** | All communication is outbound HTTPS. Works behind NAT/firewall. |
| **Ephemeral Runners** | Each runner lives for exactly one job, then self-destructs. |
| **JIT Config** | No registration token needed. Runners boot directly from a JIT config. |
| **Sandboxed** | bubblewrap namespace isolation (`--unshare-all`). Zero cross-job interference. |

## Quick Start

### Prerequisites

```bash
# Dependencies
apt install bubblewrap fuse-overlayfs

# Download and extract GitHub Actions runner binary
mkdir -p /opt/runner/actions-runner
curl -L "https://github.com/actions/runner/releases/download/v2.326.0/actions-runner-linux-x64-2.326.0.tar.gz" \
  | tar xz -C /opt/runner/actions-runner
```

### Configuration

Create `/etc/actions-runner-processor/config.yaml`:

```yaml
github:
  client_id: "123456"                             # GitHub App App ID
  private_key_path: "/etc/actions-runner-processor/github-app.pem"

scale_set_name: "actions-runner-processor"

runner:
  version: "latest"
  actions_runner_path: "/opt/runner/actions-runner"
  workspace_root: "/opt/runner/workspaces"
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
# Create dedicated user
useradd -r -s /bin/false actions-runner-processor
mkdir -p /opt/actions-runner-processor /opt/runner/{actions-runner,workspaces,overlays}
chown -R actions-runner-processor:actions-runner-processor /opt/runner

# Install binary
cp actions-runner-processor /opt/actions-runner-processor/
cp config.yaml /etc/actions-runner-processor/
cp github-app.pem /etc/actions-runner-processor/ && chmod 600 /etc/actions-runner-processor/github-app.pem

# Install systemd unit
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
6. Scaler → fuse-overlayfs (CoW layers) + GenerateJitRunnerConfig()
7. Runner → bwrap sandbox starts → JIT config registers with GitHub → runs job
8. Runner → job completes → auto-deregister → sandbox destroyed
9. Listener → job_completed received
10. Scaler → HandleJobCompleted() → cleanup overlay/workspace
```

### Sandboxing

Each runner is launched inside a bubblewrap sandbox:

- `--unshare-all` — isolated PID, IPC, UTS, mount namespaces
- `fuse-overlayfs` — per-job writable CoW layers for `/usr`, `/lib`, `/lib64`, `/bin`, `/etc`
- `--die-with-parent` — sandbox auto-dies if the processor exits
- `--share-net` only — runner needs outbound HTTPS to GitHub

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
