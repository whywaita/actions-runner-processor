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
				cfg.Runner.Entrypoint,
			)
			registry.Register(inst.Scope, s)
			webRegistry.Register(inst.Scope, s)

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

func configPath() string {
	if path := os.Getenv("CONFIG_PATH"); path != "" {
		return path
	}
	return "/etc/actions-runner-processor/config.yaml"
}
