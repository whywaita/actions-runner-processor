// runner-listener is a lightweight GitHub Actions self-hosted runner processor.
// It discovers GitHub App installations, creates runner scale sets, and
// launches ephemeral runners in bubblewrap sandboxes.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/whywaita/actions-runner-processor/internal/client"
	"github.com/whywaita/actions-runner-processor/internal/config"
	"github.com/whywaita/actions-runner-processor/internal/metrics"
	"github.com/whywaita/actions-runner-processor/internal/scaler"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	auth := client.GitHubAuth{
		ClientID:   cfg.GitHub.ClientID,
		PrivateKey: cfg.GitHub.PrivateKey,
	}

	installations, err := client.DiscoverInstallations(ctx, auth)
	if err != nil {
		log.Fatalf("failed to discover installations: %v", err)
	}

	log.Printf("discovered %d installation(s)", len(installations))

	hostname, _ := os.Hostname()
	maxRunners := cfg.ResolveMaxRunners()

	// Metrics registry shared across all scalers
	registry := metrics.NewRegistry()

	if cfg.Metrics.Enabled {
		go func() {
			if err := metrics.Serve(ctx, cfg.Metrics.Addr, registry); err != nil {
				log.Printf("metrics server: %v", err)
			}
		}()
	}

	var wg sync.WaitGroup

	for _, inst := range installations {
		wg.Add(1)
		go func(inst client.Installation) {
			defer wg.Done()

			log.Printf("[%d] creating scale set %q in %s", inst.ID, cfg.ScaleSetName, inst.Scope)

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
				log.Printf("[%d] failed to create scaleset client: %v", inst.ID, err)
				return
			}

			scaleSet, err := sClient.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
				Name:          cfg.ScaleSetName,
				RunnerGroupID: 1,
				Labels:        []scaleset.Label{{Name: cfg.ScaleSetName, Type: "System"}},
				RunnerSetting: scaleset.RunnerSetting{
					DisableUpdate: true,
				},
			})
			if err != nil {
				log.Printf("[%d] failed to create scale set: %v", inst.ID, err)
				return
			}
			defer func() {
				if err := sClient.DeleteRunnerScaleSet(context.Background(), scaleSet.ID); err != nil {
					log.Printf("[%d] failed to delete scale set: %v", inst.ID, err)
				}
			}()

			sClient.SetSystemInfo(scaleset.SystemInfo{
				System:    "actions-runner-processor",
				Subsystem: "listener",
				ScaleSetID: scaleSet.ID,
			})

			session, err := sClient.MessageSessionClient(ctx, scaleSet.ID, hostname)
			if err != nil {
				log.Printf("[%d] failed to create message session: %v", inst.ID, err)
				return
			}
			defer session.Close(context.Background())

			s := scaler.New(sClient, scaleSet.ID, maxRunners, cfg.Runner.MinRunners)
			registry.Register(inst.Scope, s)

			l, err := listener.New(session, listener.Config{
				ScaleSetID: scaleSet.ID,
				MaxRunners: maxRunners,
			})
			if err != nil {
				log.Printf("[%d] failed to create listener: %v", inst.ID, err)
				return
			}

			log.Printf("[%d] listener started (scaleSetID=%d, maxRunners=%d)", inst.ID, scaleSet.ID, maxRunners)

			if err := l.Run(ctx, s); err != nil && err != context.Canceled {
				log.Printf("[%d] listener error: %v", inst.ID, err)
			}
		}(inst)
	}

	wg.Wait()
	log.Println("all listeners stopped")
}
