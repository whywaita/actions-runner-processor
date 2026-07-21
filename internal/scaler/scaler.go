// Package scaler implements the listener.Scaler interface for launching
// ephemeral runners in bubblewrap sandboxes.
package scaler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/actions/scaleset"
)

// BwrapScaler manages runner lifecycle: launch, track, cleanup.
type BwrapScaler struct {
	client     *scaleset.Client
	scaleSetID int
	maxRunners int
	minRunners int

	mu      sync.Mutex
	runners map[string]string // RunnerName → container/job ID
	logger  *slog.Logger
}

// New creates a new BwrapScaler.
func New(client *scaleset.Client, scaleSetID, maxRunners, minRunners int) *BwrapScaler {
	return &BwrapScaler{
		client:     client,
		scaleSetID: scaleSetID,
		maxRunners: maxRunners,
		minRunners: minRunners,
		runners:    make(map[string]string),
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

	// Mark the runner as busy (move from idle if tracked)
	if _, ok := s.runners[job.RunnerName]; ok {
		// already tracked — was idle, now busy
	}
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

	// Remove runner from tracking
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
	// TODO: kill all running runner processes
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
	// TODO: Phase 3 — GenerateJitRunnerConfig + bwrap
	s.logger.Info("startRunner (stub)")
	return nil
}
