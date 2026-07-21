// Package scaler implements the listener.Scaler interface for launching
// ephemeral runners in bubblewrap sandboxes.
package scaler

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"

	"github.com/actions/scaleset"
	"github.com/google/uuid"
	"github.com/whywaita/actions-runner-processor/internal/runner"
)

// BwrapScaler manages runner lifecycle: launch, track, cleanup.
type BwrapScaler struct {
	client     *scaleset.Client
	scaleSetID int
	maxRunners int
	minRunners int

	mu      sync.Mutex
	runners map[string]*runner.Runner // RunnerName → Runner
	logger  *slog.Logger
}

// New creates a new BwrapScaler.
func New(client *scaleset.Client, scaleSetID, maxRunners, minRunners int) *BwrapScaler {
	return &BwrapScaler{
		client:     client,
		scaleSetID: scaleSetID,
		maxRunners: maxRunners,
		minRunners: minRunners,
		runners:    make(map[string]*runner.Runner),
		logger:     slog.Default().With("component", "scaler", "scaleSetID", scaleSetID),
	}
}

// HandleJobStarted tracks a job that has started on a runner.
func (s *BwrapScaler) HandleJobStarted(ctx context.Context, job *scaleset.JobStarted) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.logger.Info("job started",
		slog.Int64("runnerRequestID", job.RunnerRequestID),
		slog.String("runnerName", job.RunnerName),
	)
	return nil
}

// HandleJobCompleted cleans up after a completed job.
func (s *BwrapScaler) HandleJobCompleted(ctx context.Context, job *scaleset.JobCompleted) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.logger.Info("job completed",
		slog.Int64("runnerRequestID", job.RunnerRequestID),
		slog.String("runnerName", job.RunnerName),
		slog.String("result", job.Result),
	)

	// Kill the runner process and clean up overlay/workspace
	if r, ok := s.runners[job.RunnerName]; ok {
		r.Kill()

		// Clean up overlay dirs
		overlayDir := fmt.Sprintf("/opt/runner/overlays/%s", job.RunnerName)
		runnerOverlayDir := fmt.Sprintf("/opt/runner/overlays/%s-runner", job.RunnerName)
		workspaceDir := fmt.Sprintf("/opt/runner/workspaces/%s", job.RunnerName)

		// Unmount fuse-overlayfs
		exec.Command("fusermount", "-u", overlayDir+"/merged").Run()
		exec.Command("fusermount", "-u", runnerOverlayDir+"/merged").Run()

		os.RemoveAll(overlayDir)
		os.RemoveAll(runnerOverlayDir)
		os.RemoveAll(workspaceDir)
	}
	delete(s.runners, job.RunnerName)
	return nil
}

// HandleDesiredRunnerCount adjusts the number of runners.
// count is TotalAssignedJobs from the scale set statistics.
func (s *BwrapScaler) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current := len(s.runners)
	target := min(s.maxRunners, s.minRunners+count)

	s.logger.Info("scaling",
		slog.Int("current", current),
		slog.Int("target", target),
		slog.Int("assignedJobs", count),
	)

	if target > current {
		for i := 0; i < target-current; i++ {
			if err := s.startRunner(ctx); err != nil {
				return current, fmt.Errorf("start runner: %w", err)
			}
		}
	}

	return len(s.runners), nil
}

// Shutdown gracefully stops all runners.
func (s *BwrapScaler) Shutdown(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.logger.Info("shutting down", slog.Int("runners", len(s.runners)))
	for name, r := range s.runners {
		r.Kill()
		// Quick cleanup on shutdown
		os.RemoveAll(fmt.Sprintf("/opt/runner/overlays/%s", name))
		os.RemoveAll(fmt.Sprintf("/opt/runner/overlays/%s-runner", name))
		os.RemoveAll(fmt.Sprintf("/opt/runner/workspaces/%s", name))
	}
	clear(s.runners)
}

// MaxRunners returns the configured maximum.
func (s *BwrapScaler) MaxRunners() int { return s.maxRunners }

// ActiveRunners returns the current number of tracked runners.
func (s *BwrapScaler) ActiveRunners() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runners)
}

// startRunner generates a JIT config and starts a runner sandbox.
func (s *BwrapScaler) startRunner(ctx context.Context) error {
	name := fmt.Sprintf("runner-%s", uuid.NewString()[:8])

	// Generate JIT runner config
	jit, err := s.client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{
		Name: name,
	}, s.scaleSetID)
	if err != nil {
		return fmt.Errorf("generate JIT config: %w", err)
	}

	// Set up overlayfs directories
	overlayDir := fmt.Sprintf("/opt/runner/overlays/%s", name)
	runnerOverlayDir := fmt.Sprintf("/opt/runner/overlays/%s-runner", name)
	workspaceDir := fmt.Sprintf("/opt/runner/workspaces/%s", name)

	for _, d := range []string{overlayDir, runnerOverlayDir} {
		for _, sub := range []string{"upper", "work", "merged"} {
			os.MkdirAll(d+"/"+sub, 0o755)
		}
	}
	os.MkdirAll(workspaceDir, 0o755)

	// Start fuse-overlayfs for system dirs
	overlayCmd := exec.CommandContext(ctx, "fuse-overlayfs",
		"-o", "lowerdir=/usr:/lib:/lib64:/bin:/etc,upperdir="+overlayDir+"/upper,workdir="+overlayDir+"/work",
		overlayDir+"/merged",
	)
	if err := overlayCmd.Start(); err != nil {
		return fmt.Errorf("start system overlayfs: %w", err)
	}

	// Start fuse-overlayfs for runner binaries
	runnerOverlayCmd := exec.CommandContext(ctx, "fuse-overlayfs",
		"-o", "lowerdir=/opt/runner/actions-runner,upperdir="+runnerOverlayDir+"/upper,workdir="+runnerOverlayDir+"/work",
		runnerOverlayDir+"/merged",
	)
	if err := runnerOverlayCmd.Start(); err != nil {
		overlayCmd.Process.Kill()
		return fmt.Errorf("start runner overlayfs: %w", err)
	}

	r := &runner.Runner{
		Name:      name,
		JITConfig: jit.EncodedJITConfig,
		WorkDir:   workspaceDir,
	}

	if err := runner.Launch(ctx, r); err != nil {
		overlayCmd.Process.Kill()
		runnerOverlayCmd.Process.Kill()
		return fmt.Errorf("launch runner: %w", err)
	}

	s.runners[name] = r
	s.logger.Info("runner started", slog.String("name", name))
	return nil
}
