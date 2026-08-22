package scaler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestHandleJobCompletedWaitsForRunnerToExit(t *testing.T) {
	t.Parallel()

	workspaceRoot := t.TempDir()
	runnerName := "runner-test"
	workspaceDir := filepath.Join(workspaceRoot, runnerName)
	if err := os.Mkdir(workspaceDir, 0o755); err != nil {
		t.Fatal(err)
	}

	s := New(nil, 1, 1, 0, "/srv/actions-runner", workspaceRoot, nil, "bwrap", "", "")
	s.runners[runnerName] = &runner.Runner{Name: runnerName}

	err := s.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: runnerName})
	if err != nil {
		t.Fatalf("HandleJobCompleted() error = %v", err)
	}
	if _, err := os.Stat(workspaceDir); err != nil {
		t.Fatalf("workspace removed before runner exited: %v", err)
	}
	if _, ok := s.runners[runnerName]; !ok {
		t.Fatal("runner removed before process exited")
	}
}

func TestStartRunnerRemovesRegistrationWhenLaunchFails(t *testing.T) {
	t.Parallel()

	client := &fakeScaleSetClient{jitRunnerID: 42}
	workspaceRoot := t.TempDir()
	s := New(client, 1, 1, 0, "/srv/actions-runner", workspaceRoot, nil, "bwrap", "", "")
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

func TestHandleDesiredRunnerCountLogsOnlyWhenAssignedJobsIncrease(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	s := New(nil, 1, 0, 0, "/srv/actions-runner", t.TempDir(), nil, "bwrap", "", "")
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
