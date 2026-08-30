// actions-runner-processor is a lightweight GitHub Actions self-hosted runner processor.
// It discovers GitHub App installations, creates runner scale sets, and
// launches ephemeral runners in systemd-nspawn containers.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/whywaita/actions-runner-processor/internal/client"
	"github.com/whywaita/actions-runner-processor/internal/config"
	"github.com/whywaita/actions-runner-processor/internal/metrics"
	"github.com/whywaita/actions-runner-processor/internal/scaler"
	"github.com/whywaita/actions-runner-processor/internal/webui"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "image":
			os.Exit(runImageCmd(os.Args[2:]))
		case "-h", "--help", "help":
			usage()
			os.Exit(0)
		}
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Set up structured logging with configurable format.
	setupLogging(cfg.LogFormat)

	// Verify runtime prerequisites before starting listeners.
	if perr := preflight(cfg); perr != nil {
		slog.Error("preflight check failed", "error", perr)
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	auth := client.GitHubAuth{
		ClientID:   cfg.GitHub.ClientID,
		PrivateKey: cfg.GitHub.PrivateKey,
		APIURL:     cfg.GitHub.APIURL,
	}

	installations, err := client.DiscoverInstallations(ctx, auth, cfg.GitHub.URL)
	if err != nil {
		slog.Error("failed to discover installations", "error", err)
		os.Exit(1)
	}

	slog.Info("discovered installations", "count", len(installations))

	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	maxRunners := cfg.ResolveMaxRunners()

	// Metrics registry shared across all scalers
	registry := metrics.NewRegistry()
	webRegistry := webui.NewRegistry()

	if cfg.Metrics.Enabled {
		go func() {
			if err := metrics.Serve(ctx, cfg.Metrics.Addr, registry); err != nil {
				slog.Error("metrics server error", "error", err)
			}
		}()
	}

	if cfg.WebUI.Enabled {
		go func() {
			if err := webui.Serve(ctx, cfg.WebUI.Addr, webRegistry); err != nil {
				slog.Error("webui server error", "error", err)
			}
		}()
	}

	var wg sync.WaitGroup
	var scalersMu sync.Mutex
	var scalers []*scaler.Scaler

	for _, inst := range installations {
		wg.Add(1)
		go func(inst client.Installation) {
			defer wg.Done()

			logger := slog.With("installationID", inst.ID, "scope", inst.Scope)
			logger.Info("creating scale set", "name", cfg.ScaleSetName)

			sClient, err := scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
				GitHubConfigURL: inst.Scope,
				GitHubAppAuth: scaleset.GitHubAppAuth{
					ClientID:       auth.ClientID,
					InstallationID: inst.ID,
					PrivateKey:     auth.PrivateKey,
				},
				SystemInfo: scaleset.SystemInfo{
					System:    "actions-runner-processor",
					Subsystem: "listener",
				},
			})
			if err != nil {
				logger.Error("failed to create scaleset client", "error", err)
				return
			}

			// Reuse an existing scale set if one exists, to avoid orphaning
			// queued jobs that were assigned before a restart.
			scaleSet, err := sClient.GetRunnerScaleSet(ctx, 1, cfg.ScaleSetName)
			if err != nil {
				logger.Error("failed to get scale set", "error", err)
				return
			}
			if scaleSet == nil {
				scaleSet, err = sClient.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
					Name:          cfg.ScaleSetName,
					RunnerGroupID: 1,
					Labels:        []scaleset.Label{{Name: cfg.ScaleSetName, Type: "System"}},
					RunnerSetting: scaleset.RunnerSetting{
						DisableUpdate: true,
					},
				})
				if err != nil {
					logger.Error("failed to create scale set", "error", err)
					return
				}
				logger.Info("scale set created", "scaleSetID", scaleSet.ID)
			} else {
				logger.Info("reusing existing scale set", "scaleSetID", scaleSet.ID)
			}

			sClient.SetSystemInfo(scaleset.SystemInfo{
				System:     "actions-runner-processor",
				Subsystem:  "listener",
				ScaleSetID: scaleSet.ID,
			})

			session, err := sClient.MessageSessionClient(ctx, scaleSet.ID, hostname)
			if err != nil {
				logger.Error("failed to create message session", "error", err)
				return
			}
			defer func() { _ = session.Close(context.Background()) }()

			s := scaler.New(
				sClient,
				scaleSet.ID,
				maxRunners,
				cfg.Runner.MinRunners,
				[]string{configPath(), cfg.GitHub.PrivateKeyPath},
				cfg.Runner.ImagePath,
			)
			registry.Register(inst.Scope, s)
			webRegistry.Register(inst.Scope, s)
			scalersMu.Lock()
			scalers = append(scalers, s)
			scalersMu.Unlock()

			l, err := listener.New(session, listener.Config{
				ScaleSetID: scaleSet.ID,
				MaxRunners: maxRunners,
			})
			if err != nil {
				logger.Error("failed to create listener", "error", err)
				return
			}

			logger.Info("listener started", "scaleSetID", scaleSet.ID, "maxRunners", maxRunners)

			if err := l.Run(ctx, s); err != nil && err != context.Canceled {
				logger.Error("listener error", "error", err)
			}
		}(inst)
	}

	wg.Wait()
	slog.Info("all listeners stopped")

	// Graceful shutdown: listeners have stopped (no new job acquisition), so now
	// drain the runners that may still be executing in-flight jobs. Without this
	// wait the process would exit immediately and systemd would SIGKILL the whole
	// cgroup -- including any nspawn containers mid-job, losing the job. See
	// scaler.Scaler.Shutdown for the drain semantics.
	drainCtx, drainCancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.Runner.ShutdownGraceTimeout.Duration)
	defer drainCancel()

	scalersMu.Lock()
	toDrain := append([]*scaler.Scaler(nil), scalers...)
	scalersMu.Unlock()

	var drainWG sync.WaitGroup
	for _, s := range toDrain {
		drainWG.Add(1)
		go func(s *scaler.Scaler) {
			defer drainWG.Done()
			s.Shutdown(drainCtx)
		}(s)
	}
	drainWG.Wait()
	slog.Info("all runners drained")
}

// setupLogging configures the default slog handler based on logFormat.
// Supported values: "text" (default), "json".
func setupLogging(format string) {
	opts := &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}

	var handler slog.Handler
	switch format {
	case "text":
		handler = slog.NewTextHandler(os.Stderr, opts)
	default:
		handler = slog.NewJSONHandler(os.Stderr, opts)
	}

	slog.SetDefault(slog.New(handler))
}

// preflight verifies that all runtime prerequisites are met before
// starting any listeners. Returns an error describing the first failure.
func preflight(cfg *config.Config) error {
	checks := []struct {
		name string
		fn   func() error
	}{
		{"systemd-nspawn binary", checkBinary("systemd-nspawn")},
		{"image directory " + cfg.Runner.ImagePath, checkDir(cfg.Runner.ImagePath)},
		{"image btrfs subvolume " + cfg.Runner.ImagePath, checkBtrfs(cfg.Runner.ImagePath)},
	}
	for _, c := range checks {
		if err := c.fn(); err != nil {
			return fmt.Errorf("%s: %w", c.name, err)
		}
		slog.Info("preflight check passed", "check", c.name)
	}
	return nil
}

func checkBinary(name string) func() error {
	return func() error {
		_, err := exec.LookPath(name)
		if err != nil {
			return fmt.Errorf("%s not found in PATH", name)
		}
		return nil
	}
}

func checkDir(path string) func() error {
	return func() error {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("%s does not exist: %w", path, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", path)
		}
		return nil
	}
}

// btrfsSuperMagic is BTRFS_SUPER_MAGIC (0x9123683E). The custom image MUST be a
// btrfs subvolume so systemd-nspawn --ephemeral CoW-snapshots it cheaply; a
// plain directory on an ext4 (or btrfs-non-subvolume) backing would fall back
// to a full copy per job. This check enforces the btrfs requirement at startup.
const btrfsSuperMagic = 0x9123683E

// checkBtrfs verifies that path resides on a btrfs filesystem and is a btrfs
// subvolume. Enforced so the image always gets cheap CoW snapshots.
func checkBtrfs(path string) func() error {
	return func() error {
		var fs syscall.Statfs_t
		if err := syscall.Statfs(path, &fs); err != nil {
			return fmt.Errorf("statfs %s: %w", path, err)
		}
		if fs.Type != btrfsSuperMagic {
			return fmt.Errorf("%s is not on a btrfs filesystem (fstype=%d); the runner image must be a btrfs subvolume (see deploy/setup.sh)", path, fs.Type)
		}
		if out, err := exec.Command("btrfs", "subvolume", "show", path).CombinedOutput(); err != nil {
			return fmt.Errorf("%s is not a btrfs subvolume: %s", path, strings.TrimSpace(string(out)))
		}
		return nil
	}
}

func configPath() string {
	if path := os.Getenv("CONFIG_PATH"); path != "" {
		return path
	}
	return "/etc/actions-runner-processor/config.yaml"
}

// usage prints the command-line help.
func usage() {
	fmt.Fprint(os.Stderr, `actions-runner-processor — a lightweight self-hosted GitHub Actions runner processor

Usage:
  actions-runner-processor                     run the processor daemon
  actions-runner-processor image install-full  download + expand the full runner image
    [--release <tag|release-url>]             split parts of a specific release
                                               (default: newest release, no auth)
    --url <tarball-url>                        URL of a single full image tar.gz
    --from-actions [--owner <o>] [--repo <r>]  pull the latest build-image-full artifact
                                               (GitHub App auth from config; defaults to
                                               whywaita/actions-runner-processor; optional
                                               --artifact-prefix <p>)
    --image-path <path>                        image subvolume (default: config image_path or /opt/runner-btrfs/image)
  actions-runner-processor help                show this help

The runner image must be a btrfs subvolume (btrfs is enforced). See deploy/setup.sh.
`)
}
