# DESIGN.md — actions-runner-processor

> **Status**: Draft | **Version**: 0.1.0 | **Date**: 2026-07-21

## 1. Overview

**actions-runner-processor** is a lightweight Go binary to run GitHub Actions self-hosted runners without Kubernetes.

It runs on a single Linux VM, detects jobs using the [actions/scaleset](https://github.com/actions/scaleset) (official GitHub SDK) Message Session API, and dynamically starts/destroys ephemeral runners isolated by [systemd-nspawn](https://systemd.io/). Each runner is booted from a custom image (a root filesystem), is writable during the job (`sudo` available), and is discarded on exit. The legacy bubblewrap backend is kept for backward compatibility under `runner.mode: "bwrap"`.

### Design Principles

| Principle | Description |
|------|------|
| **Single Binary** | Go single binary. `scp` it to a VM and write a systemd unit — done. |
| **No Inbound** | All communication is outbound. Only port 443 open behind NAT/FW. |
| **Ephemeral** | After one job, the runner auto-deregisters and its sandbox disappears. |
| **JIT Runner** | No registration token. Boot the runner directly from a JIT Config. |
| **Sandboxed** | Isolated per job in a systemd-nspawn container. Zero cross-job interference. |
| **Custom Image** | Boot the runner from a custom rootfs image. `sudo` works inside the job. |
| **ARC-Compatible** | Message Session uses the same protocol as ARC. Official GitHub SDK. |

## 2. Architecture

### System Diagram

```
                        GitHub Actions Service
                              │
          ┌───────────────────┼───────────────────┐
          │ Message Session   │ Message Session    │
          │ (Installation A)  │ (Installation B)    │ ...
          │                   │                    │
┌─────────┴───────────────────┴────────────────────┴─────┐
│ Linux VM                                                  │
│                                                           │
│  ┌───────────────────────────────────────────────────┐  │
│ │  actions-runner-processor (single Go binary, multi-listener)   ││
│  │                                                     │  │
│  │  ┌──────────────┐  ┌──────────────┐               │  │
│  │  │ Listener A    │  │ Listener B    │  ... (×N)     │  │
│  │  │ (goroutine)   │  │ (goroutine)   │               │  │
│  │  │               │  │               │               │  │
│  │  │ scaleset. Client BwrapScaler     │  │ scaleset. Client BwrapScaler    │
│  │  └──────┬───────┘  └──────┬────────┘               │  │
│  │         │                 │                          │  │
│  │  ┌──────┴─────────────────┴──────────┐              │  │
│  │  │ Metrics Exporter (:9090)           │              │  │
│  │  │ Web UI (:8080)                     │              │  │
│  │  └────────────────────────────────────┘              │  │
│  └──────────────────────────┬─────────────────────────┘  │
│                              │                             │
│              ┌───────────────┴───────────────┐            │
│              │ systemd-nspawn containers (×N×M) │            │
│              │  ephemeral overlay, 1job→gone   │            │
│              └───────────────────────────────┘            │
└──────────────────────────────────────────────────────────┘
```

### Data Flow

```
1. [GitHub]  workflow triggered → job queued
       │
2. [Message Session]  job_available pushed over the Long-Poll connection
       │                 (if no message: HTTP 202 → re-poll immediately)
       │
3. [Listener]  AcquireJobs() to acquire a job
       │
4. [Listener]  job_started message received
       │
5. [Scaler]    HandleDesiredRunnerCount(count=TotalAssignedJobs)
       │          → target = min(maxRunners, minRunners + count)
       │          → start the missing runners via startRunner()
       │
6. [Scaler]    startRunner() → GenerateJitRunnerConfig(name)
       │          → start the runner with systemd-nspawn (JIT Config + ephemeral overlay)
       │
7. [Runner]    registers itself with GitHub via the JIT Config → receives job over WebSocket → runs it
       │
8. [Runner]    job completes → auto-deregister → process exits → sandbox gone
       │
9. [Listener]  job_completed message received (includes RunnerName)
       │
10. [Scaler]   HandleJobCompleted() → locate the runner by RunnerName → clean up registration
```

### Tech Stack

| Layer | Technology | Rationale |
|-------|-----------|-----------|
| Language | Go 1.23+ | Single binary, static link, low resource usage |
| Message Session | `github.com/actions/scaleset` | Official GitHub SDK. Same protocol as ARC |
| Listener Loop | `github.com/actions/scaleset/listener` | Message loop, ack, and retry built in |
| Auth | GitHub App (installation token) | Existing policy. Safer than PAT, scope-limited |
| Sandbox | systemd-nspawn | Isolated container from a custom rootfs image. Ephemeral CoW root |
| Custom Image | directory rootfs (`/opt/runner/image`) | Lightweight (debootstrap) or full (systemd-nspawn + runner-images scripts) built by `image/build-*.sh`, baked in CI. Writable during job (sudo) |
| Ephemeral Root | `--volatile=overlay` | Image as read-only lower; writes land on an overlay discarded on exit |
| Runner | `actions/runner` (official) | No `--once --ephemeral` needed. Boot from JIT Config mode |
| Process Manager | systemd | Auto-start on reboot, log management |
| Metrics | `prometheus/client_golang` | Prometheus exporter |
| Web UI | `embed` + `net/http` | Static files embedded, zero dependencies |

## 3. Components

### 3.1 scaleset.Client Wrapper

`internal/client/` — a wrapper around `scaleset.Client`.

```go
type Client struct {
    *scaleset.Client
    scaleSetID int
}

type Installation struct {
    ID    int64
    Scope string // "https://github.com/org" or "https://github.com/org/repo"
}

func DiscoverInstallations(ctx context.Context, clientID, privateKey string) ([]Installation, error)
func NewClient(ctx context.Context, scope string, auth GitHubAuth) (*Client, error)
func (c *Client) CreateOrGetScaleSet(ctx context.Context, name string) (*scaleset.RunnerScaleSet, error)
func (c *Client) CreateMessageSession(ctx context.Context, owner string) (*scaleset.MessageSessionClient, error)
```

**Responsibilities**: GitHub App auth, installation auto-discovery, scale set create/get, message session establishment.

### 3.2 BwrapScaler

`internal/scaler/` — implements the `listener.Scaler` interface.

```go
type BwrapScaler struct {
    client     *scaleset.Client
    scaleSetID int
    maxRunners int
    minRunners int    // idle runners to always keep (default 0)
    mu         sync.Mutex
    runners    map[string]*runner.Runner  // RunnerName → Runner
}

func (s *BwrapScaler) HandleJobStarted(ctx context.Context, job *scaleset.JobStarted) error
func (s *BwrapScaler) HandleJobCompleted(ctx context.Context, job *scaleset.JobCompleted) error
func (s *BwrapScaler) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error)
func (s *BwrapScaler) Shutdown(ctx context.Context)  // graceful shutdown: kill all runners
```

**Responsibilities**: runner lifecycle management (launch, track, cleanup).

**`HandleDesiredRunnerCount` semantics**:

`count` is `RunnerScaleSetStatistic.TotalAssignedJobs` (the total number of currently assigned jobs). The scaler scales runners toward `minRunners + count` (`maxRunners` is the upper bound).

```go
func (s *BwrapScaler) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
    current := len(s.runners)
    target := min(s.maxRunners, s.minRunners + count)

    if target > current {
        for i := 0; i < target - current; i++ {
            s.startRunner(ctx)  // JIT Config + nspawn launch
        }
    }
    return len(s.runners), nil
}
```

Runners are tracked by `RunnerName` (the name specified when issuing the JIT Config). When `HandleJobCompleted` is called, the corresponding Runner process is cleaned up.

### 3.3 Runner Launcher

`internal/runner/` — starts the runner via systemd-nspawn (the legacy bubblewrap path remains under `mode: "bwrap"`).

```go
type Runner struct {
    Name       string
    JITConfig  string
    WorkDir    string
    Mode       string
    ImagePath  string
    Entrypoint string
}

func Launch(ctx context.Context, r Runner) error
func (r *Runner) Wait() error
func (r *Runner) Kill() error
```

**systemd-nspawn launch command**:

`ImagePath` (a custom rootfs directory) is booted as a read-only lower layer, and all job writes land on a private overlay injected via `--volatile=overlay`. This overlay is automatically discarded when the container exits, so a job can freely rewrite `/usr` with e.g. `sudo apt install` without affecting other jobs or the host.

```bash
# Boot a runner per job in an nspawn container
R_NAME="runner-$(uuidgen | cut -c1-8)"
systemd-nspawn \
  --directory=/opt/runner/image \                    # custom image (read-only lower)
  --volatile=overlay \                                # ephemeral overlay root (discarded)
  --as-pid2 \                                         # run entrypoint as PID 2
  --user=runner \                                     # boot runner process as `runner` (sudo available)
  --capability=CAP_SYS_ADMIN,CAP_NET_ADMIN \            # dockerd: storage + netfilter (iptables bridge)
  --setenv=ACTIONS_RUNNER_INPUT_JITCONFIG=... \
  --machine="${R_NAME}" \
  --bind-ro=/etc/resolv.conf \
  --bind-ro=/etc/hosts \
  --bind-ro=/dev/null /etc/actions-runner-processor/config.yaml \
  --bind-ro=/dev/null /etc/actions-runner-processor/github-app.pem \
  /opt/actions-runner/run.sh

# On job completion, just Kill(); systemd-nspawn cleans up the overlay and container
```

- Custom image lives at `/opt/runner/image` on the host (`runner.image_path`). Built by `image/build-image.sh` (lightweight) or `image/build-image-full.sh` (full).
- Networking is shared with the host (no `--private-network`) → only outbound HTTPS to reach GitHub.
- The runner boots as the `runner` user inside the container (passwordless sudo) → `sudo` works in job steps. systemd-nspawn itself runs as host root.
- The config file and the GitHub App private key are hidden from the sandbox by binding `/dev/null` over them.
- When a runner process exits, its GitHub registration is removed using the runner ID from the JIT response.

### 3.3.1 Legacy bubblewrap backend (backward compatible)

Setting `runner.mode: "bwrap"` boots with the legacy bubblewrap (`--tmp-overlay`). System directories and the runner binary are temporary overlays and changes are discarded when the process exits. `/dev/null` masking and runner registration removal are shared with nspawn. In bwrap mode the processor removes `runner-xxxxxxxx` stale workspaces on startup.

### 3.3.2 Custom runner image generation

`image/` ships declarative builders for the nspawn rootfs. There are two:

```
image/
├── image.yaml           # manifest for the lightweight image
├── build-image.sh       # lightweight: debootstrap + chroot + tar.gz
├── image-full.yaml      # manifest for the full image
├── build-image-full.sh  # full: debootstrap + nspawn + runner-images scripts
└── (built by) .github/workflows/build-image.yaml
```

**Lightweight (option B, default)** — `image/build-image.sh` debootstraps the
base, applies the apt packages from `image/image.yaml` and installs
`actions/runner` at `/opt/actions-runner` inside a chroot, then packs the
rootfs into `actions-runner-image-<arch>.tar.gz`. Fast (minutes), runs on
PRs/pushes and `workflow_dispatch`.

**Full (option A)** — `image/build-image-full.sh` produces a
GitHub-hosted-compatible toolset. `actions/runner-images` is built by running
`images/ubuntu/scripts/build/*.sh` in order (its Packer templates are just a
loop over these shell scripts), so this script debootstraps a base, boots it
in a `systemd-nspawn` container (`--as-pid2`), and runs the same build scripts
directly inside with the repo bind-mounted. **No LXD or Packer is needed.**
Heavy (~1h, 50GB+), gated on `workflow_dispatch` → Type: **full**.

The tarball is expanded to `runner.image_path` (default `/opt/runner/image`):

```bash
sudo tar -xzf actions-runner-image-amd64.tar.gz -C /opt/runner/image     # B
sudo tar -xzf actions-runner-image-full-amd64.tar.gz -C /opt/runner/image # A
```

### 3.4 main entrypoint

`cmd/actions-runner-processor/main.go` — entrypoint. Spawns a goroutine per Installation, each running an independent Listener/MessageSession loop.

```go
func main() {
    cfg := config.Load()
    auth := cfg.GitHub

    // ① Auto-discover all installations via the API
    installations, err := client.DiscoverInstallations(ctx, auth.ClientID, auth.PrivateKey)
    if err != nil {
        log.Fatal(err)
    }

    // ② Aggregate view across all listeners
    registry := metrics.NewRegistry()
    var wg sync.WaitGroup

    for _, inst := range installations {
        wg.Add(1)
        go func(inst client.Installation) {
            defer wg.Done()

            sClient := client.New(ctx, inst.Scope, auth)
            scaleSet := sClient.CreateOrGetScaleSet(ctx, cfg.ScaleSetName)
            defer sClient.DeleteScaleSet(context.Background(), scaleSet.ID)

            session := sClient.CreateMessageSession(ctx, hostname)
            defer session.Close(context.Background())

            scaler := scaler.New(
                sClient,
                scaleSet.ID,
                cfg.MaxRunners,
                cfg.MinRunners,
                cfg.Runner.ActionsRunnerPath,
                cfg.Runner.WorkspaceRoot,
                []string{configPath(), cfg.GitHub.PrivateKeyPath},
                cfg.Runner.Mode,
                cfg.Runner.ImagePath,
                cfg.Runner.Entrypoint,
            )
            defer scaler.Shutdown(context.Background())

            registry.Register(inst.Scope, scaler)

            l := listener.New(session, listener.Config{
                ScaleSetID: scaleSet.ID,
                MaxRunners: cfg.MaxRunners,
            })
            l.Run(ctx, scaler)
        }(inst)
    }

    if cfg.Metrics.Enabled {
        go metrics.Serve(ctx, cfg.Metrics.Addr, registry)
    }
    if cfg.WebUI.Enabled {
        go webui.Serve(ctx, cfg.WebUI.Addr, registry)
    }

    wg.Wait()
}
```

### 3.4.1 Multi-Listener Architecture

Design points for running N listeners (goroutines) concurrently in one process:

| Item | Design |
|------|------|
| **Scheduling** | Each listener is an independent goroutine. The Go runtime schedules them M:N |
| **Fault isolation** | If one listener dies from an error, the others continue. Aggregated error handling via `errgroup` |
| **Resource sharing** | The sum of `max_runners` can exceed the CPU core count, but runners are ephemeral and only start during jobs, so this is fine (overcommit is OK) |
| **Metrics** | Register all Scalers in one Registry, aggregate via the Prometheus endpoint |
| **Web UI** | Show all Scale Sets in a single view as tabs/cards |

### 3.5 Metrics Exporter

`internal/metrics/` — Prometheus metrics exporter.

```go
type Exporter struct {
    scaler *scaler.BwrapScaler
}

// Exposed metrics:
//   runner_listener_active_runners     (gauge)
//   runner_listener_total_jobs_started (counter)
//   runner_listener_total_jobs_completed (counter)
//   runner_listener_job_duration_seconds (histogram)
```

**Responsibilities**: Provide the Prometheus `/metrics` endpoint. Expose runner state, job execution count, and duration.

### 3.6 Web UI

`internal/webui/` — a simple dashboard. Static files embedded in the binary via Go `embed`.

```
/               → dashboard (active runners, job queue, recent jobs)
/api/status     → JSON API (scaler state)
/api/jobs       → JSON API (job history)
```

**Responsibilities**: Visualize current runner state and job history. A simple one-screen dashboard.

## 4. Configuration

### Scope Auto-Detection

`config_url` auto-fetches all installations from the GitHub App Installation API (`GET /app/installations`) and resolves each one's scope.

```go
func discoverInstallations(ctx context.Context, clientID, privateKey string) ([]Installation, error) {
    // GET /app/installations with JWT → enumerate all installations
    installations, _ := gh.Apps.ListInstallations(ctx)

    var result []Installation
    for _, inst := range installations {
        scope := resolveScope(inst) // org all / org selected / user
        result = append(result, Installation{
            ID:    inst.ID,
            Scope: scope, // "https://github.com/org" or "https://github.com/org/repo"
        })
    }
    return result, nil
}
```

### Environment Variables / Config File

```yaml
# /opt/runner-listener/config.yaml
github:
  client_id: "123456"               # GitHub App App ID (used as JWT iss)
  private_key_path: "/etc/runner-listener/github-app.pem"

scale_set_name: "runner-listener"  # Scale Set name shared across all installations

runner:
  mode: "nspawn"                   # sandbox backend: "nspawn" (default) or "bwrap"
  image_path: "/opt/runner/image"  # custom runner image (rootfs directory)
  entrypoint: "/opt/actions-runner/run.sh"  # in-container boot command
  version: "latest"                # actions/runner version; "latest" auto-resolves (bwrap mode)
  actions_runner_path: "/opt/runner/actions-runner"   # bwrap mode only
  workspace_root: "/opt/runner/workspaces"  # bwrap mode only

metrics:
  enabled: true
  addr: ":9090"

webui:
  enabled: true
  addr: ":8080"
```

### max_runners / min_runners

```go
// cfg.MaxRunners = 0 → each Installation uses runtime.NumCPU()
// cfg.MinRunners = 0 → no warm idle runners
func (cfg Config) ResolveMaxRunners() int {
    if cfg.MaxRunners == 0 {
        return runtime.NumCPU()
    }
    return cfg.MaxRunners
}
```

The same `maxRunners` / `minRunners` apply across all Installations. Runners only exist as processes during job execution, so even if N Installations start N×NumCPU, the real load depends on the job count.

### Required Permissions (GitHub App)

| Permission | Reason |
|-----------|--------|
| `administration:read` | Fetch installation info, manage Runner Groups / Scale Sets |
| `organization_self_hosted_runners:write` | Issue runner registration tokens |

## 5. Security Design

### Threat Model

| Threat | Countermeasure |
|--------|---------------|
| Cross-job access | systemd-nspawn container (PID, IPC, UTS, mount namespace isolation). The runner is the `runner` user inside the container (passwordless sudo), in a separate namespace from the host |
| Job mutating host files | Custom image is a read-only lower; writes go to the `--volatile=overlay` ephemeral upper. Discarded on job exit |
| Runner process lingering | systemd-nspawn container terminated via `Kill()`. The ephemeral overlay is discarded at the same time |
| Credential leak | GitHub App private key chmod 600 and masked inside the sandbox. JIT Config is one-time use |
| Network-based attack | Host network shared (outbound only). Only the necessary connections are possible |
| Job exhausting resources | `--new-session` + cgroup limits on the process group (future) |

### JIT Config Security

- JIT Config is **one-time use**. Can be used only once after issuance.
- Automatically invalidated after the runner completes its job.
- Even if leaked, it cannot be reused because it is used-up or expired.

## 6. Design Decisions & Edge Cases

### Message Queue Semantics

- `GetMessage()` returns **HTTP 202 → `(nil, nil)`** when there are no messages.
- `listener.Listener` re-polls immediately in that case (no busy loop).
- `DeleteMessage()` is the message **ack**. If not called, the same message is redelivered.
- Message Session access tokens expire and the SDK auto-refreshes them.

### Session Lifecycle

- `MessageSessionClient` establishes a session via `POST .../sessions` on creation.
- **Must call `Close()`** (`DELETE .../sessions/{id}` removes the session).
- Ensure cleanup with `defer session.Close(context.Background())` on process exit.

### Scale Set Lifecycle

- Scale Set is created at startup (or an existing one is reused) and deleted on exit.
- Leaving it undeleted orphans the Scale Set and affects job assignment on GitHub's side.
- Because this is a 1VM-1ScaleSet operation, deleting on exit is safe.

### Runner Naming & Tracking

- The `Name` at JIT Config issuance matches `JobStarted.JobCompleted.RunnerName`.
- Track the runner process by this `RunnerName` as the key.
- In `HandleJobCompleted`, `Kill()` the corresponding runner's process tree and remove the sandbox.

### Stuck Runner Handling

- A timeout is needed for runners that grab a job but stop responding.
- A mechanism to kill the whole runner process via `context.WithTimeout` will be added later.
- v1 relies on GitHub Actions' default job timeout (6h).

### Runner Binary Updates

- Set `DisableUpdate: true` in Scale Set settings to disable auto-update.
- Updates are done by rebuilding the VM image + systemd restart.
- Updating `actions/runner` itself is folded into the deployment flow of the runner.

## 7. Deployment

### VM Setup (one-time)

```bash
# 1. Dependencies
apt install systemd-container

# 2. Build the custom runner image (place at /opt/runner/image)
#    Use the repo's builders: image/build-image.sh (lightweight) or
#    image/build-image-full.sh (full GitHub-hosted-compatible toolset).
#    Manual lightweight equivalent:
RUNNER_VERSION="v2.326.0"
sudo debootstrap --variant=minbase noble /opt/runner/work-rootfs
sudo mkdir -p /opt/runner/work-rootfs/opt/actions-runner
curl -L "https://github.com/actions/runner/releases/download/${RUNNER_VERSION}/actions-runner-linux-x64-${RUNNER_VERSION#v}.tar.gz" \
  | sudo tar xz -C /opt/runner/work-rootfs/opt/actions-runner
sudo rm -rf /opt/runner/image
sudo mv /opt/runner/work-rootfs /opt/runner/image

# 3. systemd-nspawn must run as root on the host; the runner inside the image
#    boots as the dedicated `runner` user

# 4. Install the processor
cp actions-runner-processor /opt/actions-runner-processor/
cp config.yaml /etc/actions-runner-processor/
cp github-app.pem /etc/actions-runner-processor/ && chmod 600 /etc/actions-runner-processor/github-app.pem

# 5. systemd unit (runs as root)
cat > /etc/systemd/system/actions-runner-processor.service << 'EOF'
[Unit]
Description=GitHub Actions Runner Processor
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/bin/actions-runner-processor
Restart=always
RestartSec=10
Environment=CONFIG_PATH=/etc/actions-runner-processor/config.yaml

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now actions-runner-processor
```

### Workflow Targeting

```yaml
# .github/workflows/build.yml
jobs:
  build:
    runs-on: self-hosted  # matches the Scale Set label
    steps:
      - uses: actions/checkout@v4
      - run: make build
```

### Runner Version Management

- The default `"latest"` fetches the latest release from the GitHub API at startup.
- An explicit version (e.g. `"v2.326.0"`) can also be specified.
- Resolution uses `actions/runner` [GitHub Releases](https://github.com/actions/runner/releases).

```go
func resolveRunnerVersion(cfgVersion string) (string, error) {
    if cfgVersion == "" || cfgVersion == "latest" {
        release, _, _ := gh.Repositories.GetLatestRelease(ctx, "actions", "runner")
        return release.GetTagName(), nil // "v2.326.0"
    }
    return cfgVersion, nil
}
```

- If the tarball for the resolved version is not cached at startup, it is downloaded.
- `DisableUpdate: true` disables runner auto-update (updates happen on runner restart).

## 8. Project Structure

```
actions-runner-processor/
├── cmd/
│   └── runner-listener/
│       └── main.go               # entrypoint
├── internal/
│   ├── client/
│   │   └── client.go             # scaleset.Client wrapper, scope auto-detection
│   ├── config/
│   │   └── config.go             # config loading
│   ├── metrics/
│   │   └── metrics.go            # Prometheus exporter
│   ├── runner/
│   │   └── runner.go             # nspawn (default) / bwrap runner launch
│   ├── scaler/
│   │   └── scaler.go             # listener.Scaler implementation
│   └── webui/
│       ├── server.go             # HTTP handler
│       └── templates/            # embed FS: dashboard HTML
├── image/
│   ├── image.yaml                # custom image manifest (base distro, arch, packages)
│   └── build-image.sh            # debootstrap + chroot rootfs builder
├── go.mod
├── go.sum
├── .goreleaser.yaml              # GoReleaser config
├── .tagpr                        # tagpr config (auto versioning)
├── DESIGN.md                     # this file
├── SPEC.md                       # implementation details (separate)
└── README.md
```

## 9. Development Roadmap

| Phase | Scope | Effort |
|-------|-------|--------|
| **Phase 1: Core** | `go mod init`, config loading, installation scope auto-detection, scaleset.Client init, Scale Set creation | Small |
| **Phase 2: Listener** | Message Session setup, listener.Run(), empty Scaler | Small |
| **Phase 3: Runner** | JIT Config generation, systemd-nspawn runner launch, process management | Medium |
| **Phase 4: Scaler** | HandleDesiredRunnerCount (scale up/down), HandleJobCompleted (cleanup) | Medium |
| **Phase 5: Metrics** | Prometheus exporter, expose runner/job metrics | Small |
| **Phase 6: Web UI** | embed simple dashboard, `/api/status`, `/api/jobs` JSON API | Medium |
| **Phase 7: Ops** | systemd unit, logs, health check, graceful shutdown | Small |
| **Phase 8: CI/CD** | GitHub Actions workflow, GoReleaser cross-compile + GitHub Release, tagpr auto versioning | Medium |

### CI/CD Pipeline

```yaml
# .github/workflows/release.yaml
name: release
on:
  push:
    branches: [main]

permissions:
  contents: write
  pull-requests: write
  issues: read

jobs:
  tagpr:
    runs-on: ubuntu-latest
    permissions:
      contents: write
      pull-requests: write
      issues: read
    outputs:
      tag: ${{ steps.tagpr.outputs.tag }}
    steps:
      - uses: actions/checkout@v4
        with:
          persist-credentials: false
      - id: tagpr
        uses: Songmu/tagpr@v1
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}

  goreleaser:
    needs: tagpr
    if: needs.tagpr.outputs.tag != ''
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0      # fetch full history + all tags (so GoReleaser detects the tagpr tag)
      - uses: goreleaser/goreleaser-action@v6
        with:
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

```yaml
# .goreleaser.yaml
builds:
  - env: [CGO_ENABLED=0]
    goos: [linux]
    goarch: [amd64, arm64]
    main: ./cmd/runner-listener/

archives:
  - format: tar.gz
    name_template: "runner-listener_{{ .Version }}_{{ .Os }}_{{ .Arch }}"

# tagpr config (.tagpr file or .github/tagpr.yaml)
#   determines major/minor/patch automatically from PR labels
```

- **tagpr**: on the main merge, looks at PR labels, auto-decides the version, and tags.
- **GoReleaser**: when a tag is created, builds statically-linked binaries and creates a GitHub Release.
- Artifacts: `runner-listener_linux_amd64.tar.gz`, `runner-listener_linux_arm64.tar.gz`.

## 10. Future Extensions (Out of Scope for v1)

- Scale-out across multiple VMs (Message Session is already multi-listener capable, so listening on the same Scale Set from another VM is enough).
- Resource limits via cgroup (CPU/memory).
- Automatic runner image updates.