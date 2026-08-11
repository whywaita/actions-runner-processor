// Package scaler implements the listener.Scaler interface for launching
// ephemeral runners in bubblewrap sandboxes.
package scaler

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/google/uuid"
	"github.com/whywaita/actions-runner-processor/internal/runner"
)

type scaleSetClient interface {
	GenerateJitRunnerConfig(context.Context, *scaleset.RunnerScaleSetJitRunnerSetting, int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	RemoveRunner(context.Context, int64) error
}

type launchRunnerFunc func(context.Context, *runner.Runner) error

// BwrapScaler manages runner lifecycle: launch, track, cleanup.
type BwrapScaler struct {
	client            scaleSetClient
	scaleSetID        int
	maxRunners        int
	minRunners        int
	actionsRunnerPath string
	workspaceRoot     string
	maskedPaths       []string
	launch            launchRunnerFunc

	mu      sync.Mutex
	runners map[string]*runner.Runner
	logger  *slog.Logger
}

// New creates a new BwrapScaler.
func New(client scaleSetClient, scaleSetID, maxRunners, minRunners int, actionsRunnerPath, workspaceRoot string, maskedPaths []string) *BwrapScaler {
	return &BwrapScaler{
		client:            client,
		scaleSetID:        scaleSetID,
		maxRunners:        maxRunners,
		minRunners:        minRunners,
		actionsRunnerPath: actionsRunnerPath,
		workspaceRoot:     workspaceRoot,
		maskedPaths:       maskedPaths,
		launch:            runner.Launch,
		runners:           make(map[string]*runner.Runner),
		logger:            slog.Default().With("component", "scaler", "scaleSetID", scaleSetID),
	}
}

// HandleJobStarted tracks a job that has started on a runner.
func (s *BwrapScaler) HandleJobStarted(_ context.Context, job *scaleset.JobStarted) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.logger.Info("job started",
		slog.Int64("runnerRequestID", job.RunnerRequestID),
		slog.String("runnerName", job.RunnerName),
	)
	return nil
}

// HandleJobCompleted cleans up after a completed job.
func (s *BwrapScaler) HandleJobCompleted(_ context.Context, job *scaleset.JobCompleted) error {
	s.logger.Info("job completed",
		slog.Int64("runnerRequestID", job.RunnerRequestID),
		slog.String("runnerName", job.RunnerName),
		slog.String("result", job.Result),
	)
	return nil
}

// HandleDesiredRunnerCount adjusts the number of runners.
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

	for i := 0; i < target-current; i++ {
		if err := s.startRunner(ctx); err != nil {
			return current, fmt.Errorf("start runner: %w", err)
		}
	}

	return len(s.runners), nil
}

// Shutdown gracefully stops all runners.
func (s *BwrapScaler) Shutdown(_ context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.logger.Info("shutting down", slog.Int("runners", len(s.runners)))
	for name, r := range s.runners {
		_ = r.Kill()
		_ = os.RemoveAll(filepath.Join(s.workspaceRoot, name))
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

func (s *BwrapScaler) startRunner(ctx context.Context) error {
	name := fmt.Sprintf("runner-%s", uuid.NewString()[:8])

	jit, err := s.client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{
		Name: name,
	}, s.scaleSetID)
	if err != nil {
		return fmt.Errorf("generate JIT config: %w", err)
	}
	if jit.Runner == nil {
		return fmt.Errorf("generate JIT config: response did not include runner reference")
	}
	runnerID := int64(jit.Runner.ID)

	workspaceDir := filepath.Join(s.workspaceRoot, name)
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		return fmt.Errorf("mkdir workspace: %w", err)
	}

	r := &runner.Runner{
		Name:              name,
		JITConfig:         jit.EncodedJITConfig,
		ActionsRunnerPath: s.actionsRunnerPath,
		WorkDir:           workspaceDir,
		MaskedPaths:       s.maskedPaths,
	}

	if err := s.launch(ctx, r); err != nil {
		_ = os.RemoveAll(workspaceDir)
		s.removeRunnerRegistration(ctx, runnerID, name)
		return fmt.Errorf("launch runner: %w", err)
	}

	s.runners[name] = r
	s.logger.Info("runner started", slog.String("name", name))

	// Monitor the runner process. If it exits without completing a job
	// (crash, OOM, JIT expiry, etc.), remove it from the tracked set so
	// HandleDesiredRunnerCount can launch a replacement.
	go func() {
		err := r.Wait()
		output := r.Output()
		if err != nil {
			s.logger.Warn("runner exited with error",
				slog.String("name", name),
				slog.String("error", err.Error()),
				slog.String("output", output),
			)
		} else {
			s.logger.Info("runner exited normally",
				slog.String("name", name),
				slog.String("output", output),
			)
		}

		s.mu.Lock()
		delete(s.runners, name)
		s.mu.Unlock()

		_ = os.RemoveAll(workspaceDir)
		s.removeRunnerRegistration(ctx, runnerID, name)
	}()
	return nil
}

func (s *BwrapScaler) removeRunnerRegistration(ctx context.Context, runnerID int64, name string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	if err := s.client.RemoveRunner(cleanupCtx, runnerID); err != nil {
		s.logger.Warn("failed to remove runner registration",
			slog.String("name", name),
			slog.Int64("runnerID", runnerID),
			slog.String("error", err.Error()),
		)
		return
	}
	s.logger.Info("runner registration removed",
		slog.String("name", name),
		slog.Int64("runnerID", runnerID),
	)
}
