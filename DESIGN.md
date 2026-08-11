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
│              │ bubblewrap sandboxes (×N×M)   │            │
│              │  namespace 隔離、1job→消滅     │            │
│              └───────────────────────────────┘            │
└──────────────────────────────────────────────────────────┘
```

### Data Flow

```
1. [GitHub]  workflow triggered → job queued
       │
2. [Message Session]  job_available メッセージが Long-Poll 接続上で push
       │                 （メッセージがない場合は HTTP 202 → すぐに再ポーリング）
       │
3. [Listener]  AcquireJobs() でジョブを獲得
       │
4. [Listener]  job_started メッセージ受信
       │
5. [Scaler]    HandleDesiredRunnerCount(count=TotalAssignedJobs)
       │          → target = min(maxRunners, minRunners + count)
       │          → 不足分の runner を startRunner() で起動
       │
6. [Scaler]    startRunner() → GenerateJitRunnerConfig(name)
       │          → bwrap で runner 起動（JIT Config + temporary overlay）
       │
7. [Runner]    JIT Config で GitHub に自身を登録 → WebSocket でジョブ受信 → 実行
       │
8. [Runner]    ジョブ完了 → 自動登録解除 → プロセス終了 → sandbox 消滅
       │
9. [Listener]  job_completed メッセージ受信（RunnerName を含む）
       │
10. [Scaler]   HandleJobCompleted() → RunnerName で runner を特定 → workspace と runner 登録をクリーンアップ
```

### Tech Stack

| Layer | Technology | Rationale |
|-------|-----------|-----------|
| Language | Go 1.23+ | シングルバイナリ、静的リンク、低リソース |
| Message Session | `github.com/actions/scaleset` | GitHub 公式 SDK。ARC と同一プロトコル |
| Listener Loop | `github.com/actions/scaleset/listener` | メッセージループ、ack、再試行を内包 |
| Auth | GitHub App (installation token) | 既存ポリシー。PAT より安全、スコープ限定可 |
| Sandbox | bubblewrap (`bwrap`) | rootless、依存極小（1 バイナリ）、namespace 隔離 |
| Writable system | bubblewrap `--tmp-overlay` | ジョブごとの一時 CoW layer。外部 mount プロセス不要 |
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

type Installation struct {
    ID    int64
    Scope string // "https://github.com/org" or "https://github.com/org/repo"
}

func DiscoverInstallations(ctx context.Context, clientID, privateKey string) ([]Installation, error)
func NewClient(ctx context.Context, scope string, auth GitHubAuth) (*Client, error)
func (c *Client) CreateOrGetScaleSet(ctx context.Context, name string) (*scaleset.RunnerScaleSet, error)
func (c *Client) CreateMessageSession(ctx context.Context, owner string) (*scaleset.MessageSessionClient, error)
```

**責務**: GitHub App 認証、Installation 自動検出、Scale Set の作成/取得、Message Session の確立。

### 3.2 BwrapScaler

`internal/scaler/` — `listener.Scaler` インターフェースの実装。

```go
type BwrapScaler struct {
    client     *scaleset.Client
    scaleSetID int
    maxRunners int
    minRunners int    // 常に維持する idle runner 数（デフォルト 0）
    mu         sync.Mutex
    runners    map[string]*runner.Runner  // RunnerName → Runner
}

func (s *BwrapScaler) HandleJobStarted(ctx context.Context, job *scaleset.JobStarted) error
func (s *BwrapScaler) HandleJobCompleted(ctx context.Context, job *scaleset.JobCompleted) error
func (s *BwrapScaler) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error)
func (s *BwrapScaler) Shutdown(ctx context.Context)  // graceful shutdown: 全 runner を kill
```

**責務**: runner のライフサイクル管理（起動、追跡、クリーンアップ）。

**`HandleDesiredRunnerCount` のセマンティクス**:

`count` は `RunnerScaleSetStatistic.TotalAssignedJobs`（現在割り当て済みのジョブ総数）。
Scaler は `minRunners + count` を目標に runner を増減させる（`maxRunners` が上限）。

```go
func (s *BwrapScaler) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
    current := len(s.runners)
    target := min(s.maxRunners, s.minRunners + count)

    if target > current {
        for i := 0; i < target - current; i++ {
            s.startRunner(ctx)  // JIT Config 生成 + bwrap 起動
        }
    }
    return len(s.runners), nil
}
```

Runner の追跡には `RunnerName`（JIT Config 発行時に指定した名前）を使用。
`HandleJobCompleted` が呼ばれたら、該当 Runner のプロセスをクリーンアップする。

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

ジョブごとに bubblewrap の `--tmp-overlay` でシステムディレクトリと runner バイナリの writable layer を作成する。

```bash
JOB_ID="runner-$(uuidgen | cut -c1-8)"
mkdir -p "/opt/runner/workspaces/${JOB_ID}"

bwrap \
  --overlay-src /usr --tmp-overlay /usr \
  --overlay-src /lib --tmp-overlay /lib \
  --overlay-src /lib64 --tmp-overlay /lib64 \
  --overlay-src /bin --tmp-overlay /bin \
  --overlay-src /sbin --tmp-overlay /sbin \
  --overlay-src /etc --tmp-overlay /etc \
  --overlay-src /var --tmp-overlay /var \
  --overlay-src /opt/runner/actions-runner --tmp-overlay /actions-runner \
  --tmpfs /run \
  --dir /run/systemd \
  --dir /run/systemd/resolve \
  --ro-bind /etc/resolv.conf /etc/resolv.conf \
  --ro-bind /etc/hosts /etc/hosts \
  --ro-bind /dev/null /etc/actions-runner-processor/config.yaml \
  --ro-bind /dev/null /etc/actions-runner-processor/github-app.pem \
  --dev /dev \
  --proc /proc \
  --tmpfs /home/runner \
  --tmpfs /tmp \
  --bind /opt/runner/workspaces/${JOB_ID} /actions-runner/_work \
  --unshare-all \
  --share-net \
  --uid 0 --gid 0 \
  --die-with-parent \
  --new-session \
  /actions-runner/run.sh

# ジョブ完了後に workspace を削除
rm -rf "/opt/runner/workspaces/${JOB_ID}"
```

- システムディレクトリと runner バイナリを **両方 temporary overlay** 化。変更はプロセス終了時に破棄される
- クリーンアップは Scaler の `HandleJobCompleted` 内で実行
- 設定ファイルと GitHub App の秘密鍵は `/dev/null` を bind して sandbox から隠す
- runner プロセス終了時は JIT 応答の runner ID を使って GitHub 側の登録も削除する
- processor 起動時は `runner-xxxxxxxx` 形式の stale workspace を削除する

### 3.4 main entrypoint

`cmd/actions-runner-processor/main.go` — entrypoint. Spawns a goroutine per Installation, each running an independent Listener/MessageSession loop.

```go
func main() {
    cfg := config.Load()
    auth := cfg.GitHub

    // ① API から全 Installation を自動検出
    installations, err := client.DiscoverInstallations(ctx, auth.ClientID, auth.PrivateKey)
    if err != nil {
        log.Fatal(err)
    }

    // ② 全 listener の集約ビュー
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

1 プロセスで N 個の listener（goroutine）を並行稼働させる設計上のポイント：

| 項目 | 設計 |
|------|------|
| **スケジューリング** | 各 listener は独立した goroutine。Go ランタイムが M:N スケジュール |
| **障害分離** | 1 つの listener がエラーで死んでも他は継続。`errgroup` で集約エラーハンドリング |
| **リソース共有** | `max_runners` の合計が CPU コア数を超える可能性があるが、runner は ephemeral でジョブ時のみ起動するため問題ない（オーバーコミット可） |
| **メトリクス** | 全 Scaler を 1 つの Registry に登録し、Prometheus endpoint で集約表示 |
| **Web UI** | 全 Scale Set の状態を 1 画面にタブ/カードで表示 |

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

`config_url` は GitHub App の Installation API (`GET /app/installations`) から全 Installation を自動取得し、それぞれの scope を解決する。

```go
func discoverInstallations(ctx context.Context, clientID, privateKey string) ([]Installation, error) {
    // JWT で GET /app/installations → 全 Installation を列挙
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
  client_id: "123456"               # GitHub App の App ID（JWT iss として使用）
  private_key_path: "/etc/runner-listener/github-app.pem"

scale_set_name: "runner-listener"  # 全 Installation で共通の Scale Set 名

runner:
  version: "latest"                # actions/runner のバージョン。"latest" で自動解決
  actions_runner_path: "/opt/runner/actions-runner"
  workspace_root: "/opt/runner/workspaces"  # tmpfs per job, no persistence

metrics:
  enabled: true
  addr: ":9090"

webui:
  enabled: true
  addr: ":8080"
```

### max_runners / min_runners

```go
// cfg.MaxRunners = 0 → 各 Installation が runtime.NumCPU() を使う
// cfg.MinRunners = 0 → warm idle runner なし
func (cfg Config) ResolveMaxRunners() int {
    if cfg.MaxRunners == 0 {
        return runtime.NumCPU()
    }
    return cfg.MaxRunners
}
```

全 Installation で同じ `maxRunners` / `minRunners` が適用される。runner はジョブ実行時のみプロセスが存在するため、N 個の Installation で N×NumCPU が起動しても実際の負荷はジョブ数次第。

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
| ジョブがホストファイルを改ざん | `/usr`, `/lib`, `/lib64`, `/bin`, `/sbin`, `/etc`, `/var` は bubblewrap temporary overlay。変更はジョブ終了時に破棄 |
| runner プロセスが残存 | `--die-with-parent` (listener が死んだら全 sandbox も死ぬ) |
| 認証情報漏洩 | GitHub App private key は権限 600 かつ sandbox 内で mask。JIT Config はワンタイム |
| ネットワーク経由の攻撃 | `--share-net` のみ（GitHub との通信に必要）。他の network namespace は隔離 |
| ジョブのリソース食い潰し | `--new-session` + プロセスグループに cgroup 制限を適用可能（将来） |

### JIT Config Security

- JIT Config は **ワンタイム**。発行後 1 回のみ使用可能
- runner がジョブを完了すると自動で無効化
- 仮に漏洩しても、使用済みまたは期限切れのため再利用不可

## 6. Design Decisions & Edge Cases

### Message Queue Semantics

- `GetMessage()` はメッセージがない場合 **HTTP 202 → `(nil, nil)`** を返す
- `listener.Listener` はこれを受けて即座に再ポーリングする（ビジーループにはならない）
- `DeleteMessage()` はメッセージの **ack**。呼ばないと同じメッセージが再送される
- Message Session のアクセストークンは期限切れがあり、SDK が自動リフレッシュ

### Session Lifecycle

- `MessageSessionClient` は作成時に `POST .../sessions` でセッションを確立
- **必ず `Close()` を呼ぶこと**（`DELETE .../sessions/{id}` でセッション削除）
- プロセス終了時は `defer session.Close(context.Background())` で確実に後始末

### Scale Set Lifecycle

- Scale Set は起動時に作成（または既存を取得）、終了時に削除
- 削除しないと孤儿 Scale Set が残り、GitHub 側のジョブ割当に影響
- 1VM 1ScaleSet 運用のため、終了時削除が安全

### Runner Naming & Tracking

- JIT Config 発行時の `Name` と `JobStarted.JobCompleted.RunnerName` が一致する
- この `RunnerName` をキーに runner プロセスを追跡する
- `HandleJobCompleted` で該当 runner のプロセスツリーを `Kill()` + sandbox 削除

### Stuck Runner 対策

- Runner がジョブを掴んだまま応答しなくなった場合のタイムアウトが必要
- 将来的に `context.WithTimeout` で runner プロセス全体を kill する機構を追加
- v1 では GitHub Actions のデフォルトジョブタイムアウト（6h）に任せる

### Runner バイナリの更新

- `DisableUpdate: true` を Scale Set 設定で指定（自動更新を無効化）
- 更新は VM イメージの再ビルド + systemd 再起動で行う
- `actions/runner` 自体の更新は `runner-listener` のデプロイフローに組み込む

## 7. Deployment

### VM Setup (one-time)

```bash
# 1. 依存パッケージ
apt install bubblewrap

# 2. actions/runner の展開（起動時に runner-listener が自動で行う）
#    version: "latest" → GitHub API で最新バージョンを解決 → ダウンロード
#    手動で事前展開する場合は:
#   RUNNER_VERSION="v2.326.0"
#   curl -L "https://github.com/actions/runner/releases/download/${RUNNER_VERSION}/actions-runner-linux-x64-${RUNNER_VERSION#v}.tar.gz" | tar xz -C /opt/runner/actions-runner

# 3. 専用ユーザー作成
useradd -r -s /bin/false runner-listener
mkdir -p /opt/runner/{actions-runner,workspaces}
chown -R runner-listener:runner-listener /opt/runner

# 4. runner-listener の配置
cp runner-listener /opt/runner-listener/
cp config.yaml /etc/runner-listener/
cp github-app.pem /etc/runner-listener/ && chmod 600 /etc/runner-listener/github-app.pem

# 5. systemd unit
cat > /etc/systemd/system/runner-listener.service << 'EOF'
[Unit]
Description=GitHub Actions Runner Listener
After=network-online.target
Wants=network-online.target

[Service]
User=runner-listener
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

### Runner バージョン管理

- デフォルト `"latest"` を指定すると、起動時に GitHub API から最新リリースを取得
- 明示的なバージョン（`"v2.326.0"`）も指定可能
- 解決は `actions/runner` の [GitHub Releases](https://github.com/actions/runner/releases) から行う

```go
func resolveRunnerVersion(cfgVersion string) (string, error) {
    if cfgVersion == "" || cfgVersion == "latest" {
        release, _, _ := gh.Repositories.GetLatestRelease(ctx, "actions", "runner")
        return release.GetTagName(), nil // "v2.326.0"
    }
    return cfgVersion, nil
}
```

- 起動時に解決したバージョンの tarball が未キャッシュならダウンロード
- `DisableUpdate: true` により runner の自動更新は無効化（更新は runner-listener 再起動時）

## 8. Project Structure

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
├── .goreleaser.yaml              # GoReleaser 設定
├── .tagpr                        # tagpr 設定（自動バージョニング）
├── DESIGN.md                     # this file
├── SPEC.md                       # 実装詳細（別途）
└── README.md
```

## 9. Development Roadmap

| Phase | Scope | Effort |
|-------|-------|--------|
| **Phase 1: Core** | `go mod init`, 設定読み込み, Installation scope 自動検出, scaleset.Client 初期化, Scale Set 作成 | 小 |
| **Phase 2: Listener** | Message Session 確立, listener.Run(), 空の Scaler | 小 |
| **Phase 3: Runner** | JIT Config 生成, bwrap runner 起動, プロセス管理 | 中 |
| **Phase 4: Scaler** | HandleDesiredRunnerCount (スケールアップ/ダウン), HandleJobCompleted (クリーンアップ) | 中 |
| **Phase 5: Metrics** | Prometheus exporter, runner/job メトリクス公開 | 小 |
| **Phase 6: Web UI** | embed 簡易ダッシュボード, `/api/status`, `/api/jobs` JSON API | 中 |
| **Phase 7: Ops** | systemd unit, ログ, ヘルスチェック, graceful shutdown | 小 |
| **Phase 8: CI/CD** | GitHub Actions workflow, GoReleaser でクロスコンパイル + GitHub Release, tagpr で自動バージョニング | 中 |

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
          fetch-depth: 0      # 全履歴 + 全タグを取得（tagpr が打ったタグを GoReleaser が検出できるように）
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

# tagpr 設定（.tagpr ファイル or .github/tagpr.yaml）
#   PR ラベルに応じて major/minor/patch を自動判定
```

- **tagpr**: main マージ時に PR ラベルを見てバージョンを自動決定・タグ付け
- **GoReleaser**: タグが打たれたら静的リンクバイナリをビルドし GitHub Release を作成
- 成果物: `runner-listener_linux_amd64.tar.gz`, `runner-listener_linux_arm64.tar.gz`

## 10. Future Extensions (Out of Scope for v1)

- 複数 VM へのスケールアウト（Message Session がもともとマルチ listener 対応なので、同じ Scale Set を別 VM でも listen するだけで実現）
- cgroup によるリソース制限（CPU/メモリ）
- runner イメージの自動更新
