// Package scaler implements the listener.Scaler interface for launching
// ephemeral runners in a systemd-nspawn container.
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

// Scaler manages runner lifecycle: launch, track, cleanup.
type Scaler struct {
	client      scaleSetClient
	scaleSetID  int
	maxRunners  int
	minRunners  int
	maskedPaths []string

	imagePath     string
	entrypoint    string
	workspacePath string

	launch launchRunnerFunc

	mu           sync.Mutex
	runners      map[string]*runner.Runner
	assignedJobs int
	logger       *slog.Logger
}

// New creates a new Scaler.
func New(client scaleSetClient, scaleSetID, maxRunners, minRunners int, maskedPaths []string, imagePath, entrypoint, workspacePath string) *Scaler {
	return &Scaler{
		client:        client,
		scaleSetID:    scaleSetID,
		maxRunners:    maxRunners,
		minRunners:    minRunners,
		maskedPaths:   maskedPaths,
		imagePath:     imagePath,
		entrypoint:    entrypoint,
		workspacePath: workspacePath,
		launch:        runner.Launch,
		runners:       make(map[string]*runner.Runner),
		logger:        slog.Default().With("component", "scaler", "scaleSetID", scaleSetID),
	}
}

// HandleJobStarted tracks a job that has started on a runner. The runner is
// marked busy so graceful shutdown can tell an in-flight runner (must drain)
// from an idle one. Lookups are best-effort by RunnerName -- if a runner is
// un-tracked it's logged but this is not fatal, and the RunningJob() output
// check still applies as a fallback in Shutdown.
func (s *Scaler) HandleJobStarted(_ context.Context, job *scaleset.JobStarted) error {
	s.mu.Lock()
	r, ok := s.runners[job.RunnerName]
	if ok {
		r.SetBusy(true)
	}
	s.mu.Unlock()

	s.logger.Info("job started",
		slog.Int64("runnerRequestID", job.RunnerRequestID),
		slog.String("runnerName", job.RunnerName),
	)
	return nil
}

// HandleJobCompleted cleans up after a completed job. The runner is un-marked
// busy so a later shutdown no longer treats it as in-flight.
func (s *Scaler) HandleJobCompleted(_ context.Context, job *scaleset.JobCompleted) error {
	s.mu.Lock()
	r, ok := s.runners[job.RunnerName]
	if ok {
		r.SetBusy(false)
	}
	s.mu.Unlock()

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

// Shutdown gracefully stops all runners. The processor's per-runner monitor
// goroutine (started in startRunner) already waits on each runner and removes
// it from the tracked set the moment it exits -- so graceful shutdown is simply
// "make idle runners exit now, then wait for the set to drain to zero".
//
// A runner that is currently executing a job (RunningJob) is left to finish
// naturally; an idle runner is killed since nothing is at stake. If ctx expires
// (the configured shutdown grace timeout) before the drain completes, the
// remaining runners are force-killed. Waiting on the map (instead of calling
// Wait() again) avoids double-Wait panics on the underlying exec.Cmd.
func (s *Scaler) Shutdown(ctx context.Context) {
	s.mu.Lock()
	if len(s.runners) == 0 {
		s.mu.Unlock()
		s.logger.Info("shutting down", slog.Int("runners", 0))
		return
	}
	runners := make([]*runner.Runner, 0, len(s.runners))
	for _, r := range s.runners {
		runners = append(runners, r)
	}
	s.mu.Unlock()

	s.logger.Info("shutting down", slog.Int("runners", len(runners)))

	// Kick any idle runners so they drain promptly; job-executing runners stay.
	// A runner is considered in-flight if it is either tracked-busy (SetBusy via
	// JobStarted) or its captured output shows "Running job:" -- the union covers
	// both the pre-flush window and the rare name-mismatch case.
	for _, r := range runners {
		if !r.IsBusy() && !r.RunningJob() {
			_ = r.Kill()
		}
	}

	// Wait for the monitor goroutines to remove every runner, or until ctx
	// expires and we force-kill the remainder.
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		remaining := len(s.runners)
		s.mu.Unlock()
		if remaining == 0 {
			break
		}

		select {
		case <-ctx.Done():
			s.logger.Warn("shutdown grace timeout exceeded, force-killing remaining runners",
				slog.Int("remaining", remaining))
			s.forceKillAll()
			// The monitor goroutines will pick up the kills and remove them.
			s.waitDrain(ctx)
			return
		case <-ticker.C:
		}
	}

	s.logger.Info("shutdown complete")
}

// forceKillAll kills every runner still tracked by the scaler.
func (s *Scaler) forceKillAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runners {
		_ = r.Kill()
	}
}

// waitDrain blocks until every runner has been removed from the tracked set,
// or ctx expires (whichever comes first), then clears the set.
func (s *Scaler) waitDrain(ctx context.Context) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		remaining := len(s.runners)
		s.mu.Unlock()
		if remaining == 0 {
			break
		}
		select {
		case <-ctx.Done():
			// Give the monitor goroutines a moment to observe the kills before
			// we clear the set and exit.
			time.Sleep(1 * time.Second)
			s.mu.Lock()
			clear(s.runners)
			s.mu.Unlock()
			return
		case <-ticker.C:
		}
	}
	s.mu.Lock()
	clear(s.runners)
	s.mu.Unlock()
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
		Name:         name,
		JITConfig:    jit.EncodedJITConfig,
		MaskedPaths:  s.maskedPaths,
		ImagePath:    s.imagePath,
		Entrypoint:   s.entrypoint,
		WorkspaceDir: filepath.Join(s.workspacePath, name),
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

		// Remove the bind-mounted workspace + tool dirs now that the container has
		// exited and they are no longer in use. These live on real disk and would
		// otherwise accumulate per runner.
		if r.WorkspaceDir != "" {
			s.removeWorkspace(r.WorkspaceDir)
		}

		s.removeRunnerRegistration(ctx, runnerID, name)
	}()
	return nil
}

// removeWorkspace removes a runner's bind-mounted workspace and its tool-cache
// sibling from the host disk. Called after the container has exited and the
// dirs are no longer in use.
func (s *Scaler) removeWorkspace(workspaceDir string) {
	for _, dir := range []string{workspaceDir, workspaceDir + "-tool"} {
		if err := os.RemoveAll(dir); err != nil {
			s.logger.Warn("failed to remove workspace", "dir", dir, "error", err.Error())
		}
	}
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
