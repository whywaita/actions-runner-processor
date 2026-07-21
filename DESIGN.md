# DESIGN.md — actions-runner-processor

> **Status**: Draft | **Version**: 0.1.0 | **Date**: 2026-07-21

## 1. Overview

**actions-runner-processor** は、GitHub Actions のセルフホストランナーを Kubernetes なしで運用するための軽量 Go バイナリです。

単一の Linux VM 上で動作し、[actions/scaleset](https://github.com/actions/scaleset)（GitHub 公式 SDK）の Message Session API を使ってジョブを検知し、[bubblewrap](https://github.com/containers/bubblewrap) で隔離された ephemeral runner を動的に起動・破棄します。

### Design Principles

| 原則 | 説明 |
|------|------|
| **Single Binary** | Go 製シングルバイナリ。VM に scp して systemd unit を書くだけ |
| **No Inbound** | 通信はすべて outbound。NAT/FW 内側から 443 だけ開いていれば動く |
| **Ephemeral** | runner は 1 ジョブ実行後、自動的に登録解除 + sandbox ごと消滅 |
| **JIT Runner** | Registration Token 不要。JIT Config で runner を直接起動 |
| **Sandboxed** | bubblewrap による namespace 隔離。ジョブ間の干渉なし |
| **ARC-Compatible** | Message Session は ARC と同一プロトコル。GitHub 公式 SDK を利用 |

## 2. Architecture

### System Diagram

```
                        GitHub Actions Service
                              │
                              │ Message Session (HTTPS Long-Poll, outbound)
                              │
┌─────────────────────────────┴─────────────────────────────┐
│ Linux VM                                                     │
│                                                              │
│  ┌──────────────────────────────────────────────────────┐  │
│  │ runner-listener (single Go binary)                     │  │
│  │                                                        │  │
│  │  ┌──────────────────┐   ┌───────────────────────┐    │  │
│  │  │ scaleset.Client   │   │ listener.Listener      │    │  │
│  │  │                    │   │                        │    │  │
│  │  │ • CreateScaleSet() │   │ • Run(scaler)          │    │  │
│  │  │ • MessageSession() │──▶│   ├ GetMessage()       │    │  │
│  │  │ • GenerateJIT()    │   │   ├ AcquireJobs()      │    │  │
│  │  │ • DeleteScaleSet() │   │   └ DeleteMessage()    │    │  │
│  │  └──────────────────┘   └───────────┬─────────────┘    │  │
│  │                                      │                   │  │
│  │                           ┌─────────▼──────────┐       │  │
│  │                           │ BwrapScaler          │       │  │
│  │                           │                      │       │  │
│  │                           │ HandleJobStarted()   │       │  │
│  │                           │ HandleJobCompleted() │       │  │
│  │                           │ HandleDesiredCount() │       │  │
│  │                           └──┬───────┬───────────┘       │  │
│  │                              │       │                    │  │
│  │                    ┌─────────┘       └──────────┐        │  │
│  │                    ▼                             ▼        │  │
│  │  ┌────────────────────┐        ┌────────────────────┐   │  │
│  │  │ Metrics Exporter    │        │ Web UI              │   │  │
│  │  │ :9090/metrics       │        │ :8080               │   │  │
│  │  │ • active_runners    │        │ • dashboard         │   │  │
│  │  │ • jobs_total        │        │ • /api/status       │   │  │
│  │  │ • job_duration      │        │ • /api/jobs         │   │  │
│  │  └────────────────────┘        └────────────────────┘   │  │
│  │                                                          │  │
│  └────────────────────────────────────┼────────────────────┘  │
│                                        │                       │
│                          ┌─────────────▼──────────────┐       │
│                          │ bubblewrap sandbox (×N)     │       │
│                          │                              │       │
│                          │  /usr, /lib, /bin → ro-bind │       │
│                          │  /actions-runner/  → rw-bind│       │
│                          │  /home/runner/     → tmpfs  │       │
│                          │                              │       │
│                          │  $ ./run.sh                  │       │
│                          │  env: JITCONFIG=<encoded>   │       │
│                          └──────────────────────────────┘       │
└──────────────────────────────────────────────────────────────┘
```

### Data Flow

```
1. [GitHub]  workflow triggered → job queued
       │
2. [Message Session]  job_available メッセージが Long-Poll 接続上で push
       │
3. [Listener]  AcquireJobs() でジョブを獲得
       │
4. [Listener]  job_started メッセージ受信
       │
5. [Scaler]    HandleDesiredRunnerCount() → GenerateJitRunnerConfig()
       │
6. [Scaler]    bwrap で runner 起動（JIT Config を環境変数で渡す）
       │
7. [Runner]    JIT Config で GitHub に自身を登録 → WebSocket でジョブ受信 → 実行
       │
8. [Runner]    ジョブ完了 → 自動登録解除 → プロセス終了 → sandbox 消滅
       │
9. [Listener]  job_completed メッセージ受信
       │
10. [Scaler]   HandleJobCompleted() → runner 状態をクリーンアップ
```

### Tech Stack

| Layer | Technology | Rationale |
|-------|-----------|-----------|
| Language | Go 1.23+ | シングルバイナリ、静的リンク、低リソース |
| Message Session | `github.com/actions/scaleset` | GitHub 公式 SDK。ARC と同一プロトコル |
| Listener Loop | `github.com/actions/scaleset/listener` | メッセージループ、ack、再試行を内包 |
| Auth | GitHub App (installation token) | 既存ポリシー。PAT より安全、スコープ限定可 |
| Sandbox | bubblewrap (`bwrap`) | rootless、依存極小（1 バイナリ）、namespace 隔離 |
| Runner | `actions/runner` (GitHub 公式) | `--once --ephemeral` 不要。JIT Config モードで起動 |
| Process Manager | systemd | VM 再起動時の自動起動、ログ管理 |
| Metrics | `prometheus/client_golang` | Prometheus exporter |
| Web UI | `embed` + `net/http` | 静的ファイル埋め込み、ゼロ依存 |

## 3. Components

### 3.1 scaleset.Client Wrapper

`internal/client/` — scaleset.Client のラッパー。

```go
type Client struct {
    *scaleset.Client
    scaleSetID int
}

func NewClient(ctx context.Context, cfg Config) (*Client, error)
func (c *Client) CreateOrGetScaleSet(ctx context.Context, name string, labels []string) (*scaleset.RunnerScaleSet, error)
func (c *Client) CreateMessageSession(ctx context.Context, owner string) (*scaleset.MessageSessionClient, error)
```

**責務**: GitHub App 認証、Scale Set の作成/取得、Message Session の確立。

### 3.2 BwrapScaler

`internal/scaler/` — `listener.Scaler` インターフェースの実装。

```go
type BwrapScaler struct {
    client     *scaleset.Client
    scaleSetID int
    maxRunners int
    // runner state management
}

func (s *BwrapScaler) HandleJobStarted(ctx context.Context, job *scaleset.JobStarted) error
func (s *BwrapScaler) HandleJobCompleted(ctx context.Context, job *scaleset.JobCompleted) error
func (s *BwrapScaler) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error)
```

**責務**: runner のライフサイクル管理（起動、追跡、クリーンアップ）。

### 3.3 Runner Launcher

`internal/runner/` — bubblewrap 経由での runner 起動。

```go
type Runner struct {
    Name       string
    JITConfig  string
    WorkDir    string
}

func Launch(ctx context.Context, r Runner) error
func (r *Runner) Wait() error
func (r *Runner) Kill() error
```

**bubblewrap 実行コマンド**:

```bash
bwrap \
  --ro-bind /usr /usr \
  --ro-bind /lib /lib \
  --ro-bind /lib64 /lib64 \
  --ro-bind /bin /bin \
  --ro-bind /etc/resolv.conf /etc/resolv.conf \
  --ro-bind /etc/ssl /etc/ssl \
  --dev /dev \
  --proc /proc \
  --tmpfs /home/runner \
  --tmpfs /tmp \
  --bind /opt/runner/actions-runner /actions-runner \
  --bind /opt/runner/workspaces/{name} /actions-runner/_work \
  --unshare-all \
  --share-net \
  --die-with-parent \
  --new-session \
  /actions-runner/run.sh
```

### 3.4 main entrypoint

`cmd/runner-listener/main.go` — エントリーポイント。

```go
func main() {
    cfg := config.Load()

    // Auto-detect scope from GitHub App installation
    scope := client.DetectScope(ctx, cfg.GitHub.AppID, cfg.GitHub.InstallationID)
    sClient := client.New(ctx, scope, cfg.GitHub)
    scaleSet := sClient.CreateOrGetScaleSet(ctx, cfg.ScaleSetName, cfg.Labels)
    session := sClient.CreateMessageSession(ctx, hostname)
    scaler := scaler.New(sClient, scaleSet.ID, cfg.ScaleSet.MaxRunners)

    // Start metrics exporter
    if cfg.Metrics.Enabled {
        go metrics.Serve(ctx, cfg.Metrics.Addr, scaler)
    }

    // Start Web UI
    if cfg.WebUI.Enabled {
        go webui.Serve(ctx, cfg.WebUI.Addr, scaler, scaleSet)
    }

    listener := listener.New(session, listener.Config{...})
    listener.Run(ctx, scaler)
}
```

### 3.5 Metrics Exporter

`internal/metrics/` — Prometheus metrics exporter。

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

**責務**: Prometheus `/metrics` エンドポイントの提供。runner 状態、ジョブ実行数、所要時間を公開。

### 3.6 Web UI

`internal/webui/` — 簡易ダッシュボード。Go の `embed` で静的ファイルをバイナリに埋め込み。

```
/               → ダッシュボード（active runners, job queue, recent jobs）
/api/status     → JSON API（scaler 状態）
/api/jobs       → JSON API（ジョブ履歴）
```

**責務**: 現在の runner 状態とジョブ履歴の可視化。1 画面の簡易ダッシュボード。

## 4. Configuration

### Scope Auto-Detection

`config_url` は GitHub App の Installation から自動解決する。ユーザーが手動で scope（org/repo）を指定する必要はない。

```go
// Installation API のレスポンスから scope を自動判別
installation, _ := gh.Apps.GetInstallation(ctx, installationID)

switch {
case installation.RepositorySelection == "selected" && len(installation.Repositories) == 1:
    configURL = fmt.Sprintf("https://github.com/%s", installation.Repositories[0].FullName)
case installation.Account.Type == "Organization":
    configURL = fmt.Sprintf("https://github.com/%s", installation.Account.Login)
default:
    // GHES or unexpected — fallback to explicit config
}
```

### Environment Variables / Config File

```yaml
# /opt/runner-listener/config.yaml
github:
  app_id: 123456
  installation_id: 789012
  private_key_path: "/etc/runner-listener/github-app.pem"

scale_set:
  name: "my-scale-set"
  max_runners: 0          # 0 = auto-detect from CPU cores (runtime.NumCPU())

runner:
  actions_runner_path: "/opt/runner/actions-runner"
  workspace_root: "/opt/runner/workspaces"  # tmpfs per job, no persistence

metrics:
  enabled: true
  addr: ":9090"

webui:
  enabled: true
  addr: ":8080"
```

### max_runners Default

```go
import "runtime"

if cfg.ScaleSet.MaxRunners == 0 {
    cfg.ScaleSet.MaxRunners = runtime.NumCPU()
}
```

### Required Permissions (GitHub App)

| Permission | Reason |
|-----------|--------|
| `administration:read` | Installation 情報の取得、Runner Group / Scale Set の管理 |
| `organization_self_hosted_runners:write` | Runner 登録トークン発行 |

## 5. Security Design

### Threat Model

| Threat | Countermeasure |
|--------|---------------|
| ジョブ間の横断アクセス | bubblewrap `--unshare-all` (PID, IPC, UTS, mount namespace 分離) |
| ジョブがホストファイルを改ざん | `/usr`, `/lib`, `/bin`, `/etc` は `--ro-bind` (read-only mount) |
| runner プロセスが残存 | `--die-with-parent` (listener が死んだら全 sandbox も死ぬ) |
| 認証情報漏洩 | GitHub App private key はファイルシステム権限 600。JIT Config はワンタイム |
| ネットワーク経由の攻撃 | `--share-net` のみ（GitHub との通信に必要）。他の network namespace は隔離 |
| ジョブのリソース食い潰し | `--new-session` + プロセスグループに cgroup 制限を適用可能（将来） |

### JIT Config Security

- JIT Config は **ワンタイム**。発行後 1 回のみ使用可能
- runner がジョブを完了すると自動で無効化
- 仮に漏洩しても、使用済みまたは期限切れのため再利用不可

## 6. Deployment

### VM Setup (one-time)

```bash
# 1. 依存パッケージ
apt install bubblewrap

# 2. actions/runner の展開
mkdir -p /opt/runner/actions-runner
cd /opt/runner/actions-runner
curl -L https://github.com/actions/runner/releases/download/v2.326.0/...tar.gz | tar xz

# 3. runner-listener の配置
cp runner-listener /opt/runner-listener/
cp config.yaml /etc/runner-listener/
cp github-app.pem /etc/runner-listener/ && chmod 600 /etc/runner-listener/github-app.pem

# 4. systemd unit
cat > /etc/systemd/system/runner-listener.service << 'EOF'
[Unit]
Description=GitHub Actions Runner Listener
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/opt/runner-listener/runner-listener
Restart=always
RestartSec=10
Environment=CONFIG_PATH=/etc/runner-listener/config.yaml

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now runner-listener
```

### Workflow Targeting

```yaml
# .github/workflows/build.yml
jobs:
  build:
    runs-on: self-hosted  # Scale Set のラベルにマッチ
    steps:
      - uses: actions/checkout@v4
      - run: make build
```

## 7. Project Structure

```
actions-runner-processor/
├── cmd/
│   └── runner-listener/
│       └── main.go               # エントリーポイント
├── internal/
│   ├── client/
│   │   └── client.go             # scaleset.Client ラッパー、scope 自動検出
│   ├── config/
│   │   └── config.go             # 設定読み込み
│   ├── metrics/
│   │   └── metrics.go            # Prometheus exporter
│   ├── runner/
│   │   └── runner.go             # bwrap runner 起動
│   ├── scaler/
│   │   └── scaler.go             # listener.Scaler 実装
│   └── webui/
│       ├── server.go             # HTTP handler
│       └── templates/            # embed FS: dashboard HTML
├── go.mod
├── go.sum
├── DESIGN.md                     # this file
├── SPEC.md                       # 実装詳細（別途）
└── README.md
```

## 8. Development Roadmap

| Phase | Scope | Effort |
|-------|-------|--------|
| **Phase 1: Core** | `go mod init`, 設定読み込み, Installation scope 自動検出, scaleset.Client 初期化, Scale Set 作成 | 小 |
| **Phase 2: Listener** | Message Session 確立, listener.Run(), 空の Scaler | 小 |
| **Phase 3: Runner** | JIT Config 生成, bwrap runner 起動, プロセス管理 | 中 |
| **Phase 4: Scaler** | HandleDesiredRunnerCount (スケールアップ/ダウン), HandleJobCompleted (クリーンアップ) | 中 |
| **Phase 5: Metrics** | Prometheus exporter, runner/job メトリクス公開 | 小 |
| **Phase 6: Web UI** | embed 簡易ダッシュボード, `/api/status`, `/api/jobs` JSON API | 中 |
| **Phase 7: Ops** | systemd unit, ログ, ヘルスチェック, graceful shutdown | 小 |
| **Phase 8: CI/CD** | GitHub Actions workflow, GoReleaser, GHCR へのコンテナイメージ push | 中 |

## 9. Future Extensions (Out of Scope for v1)

- 複数 VM へのスケールアウト（Message Session がもともとマルチ listener 対応なので、同じ Scale Set を別 VM でも listen するだけで実現）
- cgroup によるリソース制限（CPU/メモリ）
- runner イメージの自動更新
