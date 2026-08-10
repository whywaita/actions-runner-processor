// Package scaler implements the listener.Scaler interface for launching
// ephemeral runners in bubblewrap sandboxes.
package scaler

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

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
	runners map[string]*runner.Runner
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
	s.mu.Lock()
	defer s.mu.Unlock()

	s.logger.Info("job completed",
		slog.Int64("runnerRequestID", job.RunnerRequestID),
		slog.String("runnerName", job.RunnerName),
		slog.String("result", job.Result),
	)

	if r, ok := s.runners[job.RunnerName]; ok {
		_ = r.Kill()

		overlayDir := fmt.Sprintf("/opt/runner/overlays/%s", job.RunnerName)
		runnerOverlayDir := fmt.Sprintf("/opt/runner/overlays/%s-runner", job.RunnerName)
		workspaceDir := fmt.Sprintf("/opt/runner/workspaces/%s", job.RunnerName)

		_ = exec.Command("fusermount", "-u", overlayDir+"/merged").Run()
		_ = exec.Command("fusermount", "-u", runnerOverlayDir+"/merged").Run()

		_ = os.RemoveAll(overlayDir)
		_ = os.RemoveAll(runnerOverlayDir)
		_ = os.RemoveAll(workspaceDir)
	}
	delete(s.runners, job.RunnerName)
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
		_ = os.RemoveAll(fmt.Sprintf("/opt/runner/overlays/%s", name))
		_ = os.RemoveAll(fmt.Sprintf("/opt/runner/overlays/%s-runner", name))
		_ = os.RemoveAll(fmt.Sprintf("/opt/runner/workspaces/%s", name))
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

	overlayDir := fmt.Sprintf("/opt/runner/overlays/%s", name)
	runnerOverlayDir := fmt.Sprintf("/opt/runner/overlays/%s-runner", name)
	workspaceDir := fmt.Sprintf("/opt/runner/workspaces/%s", name)

	for _, d := range []string{overlayDir, runnerOverlayDir} {
		for _, sub := range []string{"upper", "work", "merged"} {
			if err := os.MkdirAll(d+"/"+sub, 0o755); err != nil {
				return fmt.Errorf("mkdir overlay: %w", err)
			}
		}
	}
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		return fmt.Errorf("mkdir workspace: %w", err)
	}

	overlayCmd := exec.CommandContext(ctx, "fuse-overlayfs",
		"-o", "lowerdir=/usr:/lib:/lib64:/bin:/etc,upperdir="+overlayDir+"/upper,workdir="+overlayDir+"/work",
		"-o", "allow_other",
		overlayDir+"/merged",
	)
	var overlayStderr bytes.Buffer
	overlayCmd.Stderr = &overlayStderr
	if err := overlayCmd.Start(); err != nil {
		return fmt.Errorf("start system overlayfs: %w", err)
	}

	runnerOverlayCmd := exec.CommandContext(ctx, "fuse-overlayfs",
		"-o", "lowerdir=/opt/runner/actions-runner,upperdir="+runnerOverlayDir+"/upper,workdir="+runnerOverlayDir+"/work",
		"-o", "allow_other",
		runnerOverlayDir+"/merged",
	)
	var runnerOverlayStderr bytes.Buffer
	runnerOverlayCmd.Stderr = &runnerOverlayStderr
	if err := runnerOverlayCmd.Start(); err != nil {
		_ = overlayCmd.Process.Kill()
		_ = overlayCmd.Wait() // reap to avoid zombies
		return fmt.Errorf("start runner overlayfs: %w", err)
	}

	// Wait for fuse-overlayfs mounts to become available before launching the runner.
	// fuse-overlayfs daemonizes after cmd.Start() and the FUSE mount may take a
	// moment to become ready.
	if err := waitForPath(overlayDir+"/merged/usr", 10*time.Second); err != nil {
		s.logger.Error("system overlay mount failed",
			slog.String("stderr", overlayStderr.String()),
		)
		_ = overlayCmd.Process.Kill()
		_ = runnerOverlayCmd.Process.Kill()
		return fmt.Errorf("wait for system overlay mount: %w", err)
	}
	if err := waitForPath(runnerOverlayDir+"/merged/run.sh", 10*time.Second); err != nil {
		s.logger.Error("runner overlay mount failed",
			slog.String("stderr", runnerOverlayStderr.String()),
		)
		_ = overlayCmd.Process.Kill()
		_ = runnerOverlayCmd.Process.Kill()
		return fmt.Errorf("wait for runner overlay mount: %w", err)
	}

	r := &runner.Runner{
		Name:      name,
		JITConfig: jit.EncodedJITConfig,
		WorkDir:   workspaceDir,
	}

	if err := runner.Launch(ctx, r); err != nil {
		_ = overlayCmd.Process.Kill()
		_ = runnerOverlayCmd.Process.Kill()
		return fmt.Errorf("launch runner: %w", err)
	}

	s.runners[name] = r
	s.logger.Info("runner started", slog.String("name", name))

	// Monitor the runner process. If it exits without completing a job
	// (crash, OOM, JIT expiry, etc.), remove it from the tracked set so
	// HandleDesiredRunnerCount can launch a replacement.
	go func() {
		if err := r.Wait(); err != nil {
			stderr := r.StderrOutput()
			s.logger.Warn("runner exited with error",
				slog.String("name", name),
				slog.String("error", err.Error()),
				slog.String("stderr", stderr),
			)
		} else {
			s.logger.Info("runner exited normally", slog.String("name", name))
		}

		s.mu.Lock()
		delete(s.runners, name)
		s.mu.Unlock()

		// Clean up overlay processes that may still be mounted.
		_ = overlayCmd.Process.Kill()
		_ = runnerOverlayCmd.Process.Kill()
	}()
	return nil
}

// waitForPath polls until the given path exists or the timeout expires.
func waitForPath(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s after %v", path, timeout)
}
