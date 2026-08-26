package scaler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/whywaita/actions-runner-processor/internal/runner"
)

type fakeScaleSetClient struct {
	jitRunnerID int
	removedIDs  []int64
}

func (f *fakeScaleSetClient) GenerateJitRunnerConfig(context.Context, *scaleset.RunnerScaleSetJitRunnerSetting, int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	return &scaleset.RunnerScaleSetJitRunnerConfig{
		Runner:           &scaleset.RunnerReference{ID: f.jitRunnerID},
		EncodedJITConfig: "jit-config",
	}, nil
}

func (f *fakeScaleSetClient) RemoveRunner(_ context.Context, runnerID int64) error {
	f.removedIDs = append(f.removedIDs, runnerID)
	return nil
}

func TestStartRunnerRemovesRegistrationWhenLaunchFails(t *testing.T) {
	t.Parallel()

	client := &fakeScaleSetClient{jitRunnerID: 42}
	s := New(client, 1, 1, 0, nil, "/srv/image")
	s.launch = func(context.Context, *runner.Runner) error {
		return errors.New("launch failed")
	}

	err := s.startRunner(context.Background())
	if err == nil {
		t.Fatal("startRunner() error = nil, want launch failure")
	}
	if len(client.removedIDs) != 1 || client.removedIDs[0] != 42 {
		t.Fatalf("removed runner IDs = %v, want [42]", client.removedIDs)
	}
}

func TestStartRunnerSetsImageAndEntrypoint(t *testing.T) {
	t.Parallel()

	client := &fakeScaleSetClient{jitRunnerID: 7}
	s := New(client, 1, 1, 0, nil, "/srv/image")

	var got *runner.Runner
	s.launch = func(_ context.Context, r *runner.Runner) error {
		got = r
		return nil
	}

	if err := s.startRunner(context.Background()); err != nil {
		t.Fatalf("startRunner() error = %v", err)
	}
	if got.ImagePath != "/srv/image" {
		t.Errorf("ImagePath = %q, want /srv/image", got.ImagePath)
	}
	if got.JITConfig != "jit-config" || got.Name == "" {
		t.Errorf("runner fields not populated: %+v", got)
	}
}

func TestHandleDesiredRunnerCountLogsOnlyWhenAssignedJobsIncrease(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	s := New(nil, 1, 0, 0, nil, "/srv/image")
	s.logger = slog.New(slog.NewJSONHandler(&output, nil))

	for _, count := range []int{0, 0, 1, 1, 0, 1} {
		if _, err := s.HandleDesiredRunnerCount(context.Background(), count); err != nil {
			t.Fatalf("HandleDesiredRunnerCount(%d) error = %v", count, err)
		}
	}

	if got := strings.Count(output.String(), `"msg":"scaling"`); got != 2 {
		t.Fatalf("scaling log count = %d, want 2; output = %s", got, output.String())
	}
}

func TestHandleJobStartedMarksBusy(t *testing.T) {
	t.Parallel()

	s := New(nil, 1, 1, 0, nil, "/srv/image")
	r := &runner.Runner{Name: "runner-busy"}
	s.mu.Lock()
	s.runners["runner-busy"] = r
	s.mu.Unlock()

	if r.IsBusy() {
		t.Fatal("runner busy before JobStarted")
	}
	if err := s.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: "runner-busy"}); err != nil {
		t.Fatalf("HandleJobStarted error = %v", err)
	}
	if !r.IsBusy() {
		t.Fatal("runner not busy after JobStarted")
	}

	if err := s.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: "runner-busy", Result: "success"}); err != nil {
		t.Fatalf("HandleJobCompleted error = %v", err)
	}
	if r.IsBusy() {
		t.Fatal("runner still busy after JobCompleted")
	}
}

func TestShutdownNoRunners(t *testing.T) {
	t.Parallel()

	s := New(nil, 1, 1, 0, nil, "/srv/image")

	// Must return immediately and not hang when there is nothing tracked.
	done := make(chan struct{})
	go func() {
		s.Shutdown(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown with no runners did not return")
	}
}

func TestShutdownForceKillsOnExpiredContext(t *testing.T) {
	t.Parallel()

	s := New(nil, 1, 1, 0, nil, "/srv/image")

	// Seed a runner that is idle (no output yet -> RunningJob() == false).
	// Its cmd is nil, so Kill() is a no-op. With an already-cancelled context
	// the grace loop must not spin forever: it force-kills and returns.
	s.mu.Lock()
	s.runners["runner-idle"] = &runner.Runner{Name: "runner-idle"}
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		s.Shutdown(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown with expired context did not return")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.runners) != 0 {
		t.Fatalf("runners not cleared after force-kill shutdown, len = %d", len(s.runners))
	}
}
