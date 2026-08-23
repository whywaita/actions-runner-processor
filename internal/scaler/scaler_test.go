package scaler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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

func TestStartRunnerRemovesRegistrationWhenLaunchFails(t *testing.T) {
	t.Parallel()

	client := &fakeScaleSetClient{jitRunnerID: 42}
	s := New(client, 1, 1, 0, nil, "/srv/image", "/opt/actions-runner/run.sh")
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
	s := New(client, 1, 1, 0, nil, "/srv/image", "/opt/actions-runner/run.sh")

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
	if got.Entrypoint != "/opt/actions-runner/run.sh" {
		t.Errorf("Entrypoint = %q, want /opt/actions-runner/run.sh", got.Entrypoint)
	}
	if got.JITConfig != "jit-config" || got.Name == "" {
		t.Errorf("runner fields not populated: %+v", got)
	}
}

func TestHandleDesiredRunnerCountLogsOnlyWhenAssignedJobsIncrease(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	s := New(nil, 1, 0, 0, nil, "/srv/image", "/opt/actions-runner/run.sh")
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
