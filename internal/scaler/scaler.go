// Package scaler implements the listener.Scaler interface for launching
// ephemeral runners in a systemd-nspawn container.
package scaler

import (
	"context"
	"fmt"
	"log/slog"
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

// Scaler manages runner lifecycle: launch, track, cleanup.
type Scaler struct {
	client      scaleSetClient
	scaleSetID  int
	maxRunners  int
	minRunners  int
	maskedPaths []string

	imagePath  string
	entrypoint string

	launch launchRunnerFunc

	mu           sync.Mutex
	runners      map[string]*runner.Runner
	assignedJobs int
	logger       *slog.Logger
}

// New creates a new Scaler.
func New(client scaleSetClient, scaleSetID, maxRunners, minRunners int, maskedPaths []string, imagePath, entrypoint string) *Scaler {
	return &Scaler{
		client:      client,
		scaleSetID:  scaleSetID,
		maxRunners:  maxRunners,
		minRunners:  minRunners,
		maskedPaths: maskedPaths,
		imagePath:   imagePath,
		entrypoint:  entrypoint,
		launch:      runner.Launch,
		runners:     make(map[string]*runner.Runner),
		logger:      slog.Default().With("component", "scaler", "scaleSetID", scaleSetID),
	}
}

// HandleJobStarted tracks a job that has started on a runner.
func (s *Scaler) HandleJobStarted(_ context.Context, job *scaleset.JobStarted) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.logger.Info("job started",
		slog.Int64("runnerRequestID", job.RunnerRequestID),
		slog.String("runnerName", job.RunnerName),
	)
	return nil
}

// HandleJobCompleted cleans up after a completed job.
func (s *Scaler) HandleJobCompleted(_ context.Context, job *scaleset.JobCompleted) error {
	s.logger.Info("job completed",
		slog.Int64("runnerRequestID", job.RunnerRequestID),
		slog.String("runnerName", job.RunnerName),
		slog.String("result", job.Result),
	)
	return nil
}

// HandleDesiredRunnerCount adjusts the number of runners.
func (s *Scaler) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current := len(s.runners)
	target := min(s.maxRunners, s.minRunners+count)

	if count > s.assignedJobs {
		s.logger.Info("scaling",
			slog.Int("current", current),
			slog.Int("target", target),
			slog.Int("assignedJobs", count),
		)
	}
	s.assignedJobs = count

	for i := 0; i < target-current; i++ {
		if err := s.startRunner(ctx); err != nil {
			return current, fmt.Errorf("start runner: %w", err)
		}
	}

	return len(s.runners), nil
}

// Shutdown gracefully stops all runners.
func (s *Scaler) Shutdown(_ context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.logger.Info("shutting down", slog.Int("runners", len(s.runners)))
	for _, r := range s.runners {
		_ = r.Kill()
	}
	clear(s.runners)
}

// MaxRunners returns the configured maximum.
func (s *Scaler) MaxRunners() int { return s.maxRunners }

// ActiveRunners returns the current number of tracked runners.
func (s *Scaler) ActiveRunners() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runners)
}

func (s *Scaler) startRunner(ctx context.Context) error {
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

	r := &runner.Runner{
		Name:        name,
		JITConfig:   jit.EncodedJITConfig,
		MaskedPaths: s.maskedPaths,
		ImagePath:   s.imagePath,
		Entrypoint:  s.entrypoint,
	}

	if err := s.launch(ctx, r); err != nil {
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

		s.removeRunnerRegistration(ctx, runnerID, name)
	}()
	return nil
}

func (s *Scaler) removeRunnerRegistration(ctx context.Context, runnerID int64, name string) {
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
