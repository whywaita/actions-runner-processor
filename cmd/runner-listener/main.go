// runner-listener is a lightweight GitHub Actions self-hosted runner processor.
// It discovers GitHub App installations, creates runner scale sets, and
// launches ephemeral runners in bubblewrap sandboxes.
package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"github.com/whywaita/actions-runner-processor/internal/client"
	"github.com/whywaita/actions-runner-processor/internal/config"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	installations, err := client.DiscoverInstallations(ctx, client.GitHubAuth{
		ClientID:   cfg.GitHub.ClientID,
		PrivateKey: cfg.GitHub.PrivateKey,
	})
	if err != nil {
		log.Fatalf("failed to discover installations: %v", err)
	}

	log.Printf("discovered %d installation(s)", len(installations))
	for _, inst := range installations {
		log.Printf("  installation_id=%d scope=%s", inst.ID, inst.Scope)
	}

	// TODO: Phase 2 — create scale sets, message sessions, listeners
	<-ctx.Done()
	log.Println("shutting down")
}
