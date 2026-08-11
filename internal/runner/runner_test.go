package runner

import (
	"bytes"
	"slices"
	"testing"
)

func TestBuildArgsUsesBubblewrapTemporaryOverlays(t *testing.T) {
	t.Parallel()

	r := &Runner{
		ActionsRunnerPath: "/srv/actions-runner",
		WorkDir:           "/srv/workspaces/runner-1234",
		MaskedPaths:       []string{"/etc/custom/key.pem", "/etc/custom/config.yaml", "/home/hidden-by-sandbox.pem"},
	}

	args := buildArgs(r)

	wantSequences := [][]string{
		{"--overlay-src", "/usr", "--tmp-overlay", "/usr"},
		{"--overlay-src", "/etc", "--tmp-overlay", "/etc"},
		{"--overlay-src", "/var", "--tmp-overlay", "/var"},
		{"--overlay-src", "/srv/actions-runner", "--tmp-overlay", "/actions-runner"},
		{"--tmpfs", "/run", "--dir", "/run/systemd", "--dir", "/run/systemd/resolve"},
		{"--ro-bind", "/etc/resolv.conf", "/etc/resolv.conf"},
		{"--ro-bind", "/dev/null", "/etc/custom/key.pem"},
		{"--ro-bind", "/dev/null", "/etc/custom/config.yaml"},
		{"--bind", "/srv/workspaces/runner-1234", "/actions-runner/_work"},
		{"--uid", "0", "--gid", "0"},
	}

	for _, want := range wantSequences {
		if !containsSequence(args, want) {
			t.Errorf("buildArgs() = %q, want sequence %q", args, want)
		}
	}

	if slices.Contains(args, "fuse-overlayfs") {
		t.Errorf("buildArgs() = %q, must not use fuse-overlayfs", args)
	}
	if slices.Contains(args, "/home/hidden-by-sandbox.pem") {
		t.Errorf("buildArgs() = %q, must not mount a mask outside /etc", args)
	}
	if indexOf(args, "--tmpfs", "/run") > indexOf(args, "--ro-bind", "/etc/resolv.conf", "/etc/resolv.conf") {
		t.Errorf("buildArgs() must create /run before binding /etc/resolv.conf: %q", args)
	}
}

func TestOutput(t *testing.T) {
	t.Parallel()

	r := &Runner{output: bytes.NewBufferString("runner diagnostic")}
	if got := r.Output(); got != "runner diagnostic" {
		t.Fatalf("Output() = %q, want runner diagnostic", got)
	}
}

func containsSequence(values, sequence []string) bool {
	for i := 0; i+len(sequence) <= len(values); i++ {
		if slices.Equal(values[i:i+len(sequence)], sequence) {
			return true
		}
	}
	return false
}

func indexOf(values []string, sequence ...string) int {
	for i := 0; i+len(sequence) <= len(values); i++ {
		if slices.Equal(values[i:i+len(sequence)], sequence) {
			return i
		}
	}
	return -1
}
