package runner

import (
	"bytes"
	"testing"
)

func TestIsWithin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path string
		root string
		want bool
	}{
		{"/etc/custom/key.pem", "/etc", true},
		{"/etc/hosts", "/etc", true},
		{"/home/hidden.pem", "/etc", false},
		{"/etc/..", "/etc", false},
		{"/etc2/foo", "/etc", false},
		{"/opt/runner/foo", "/opt", true},
	}
	for _, tt := range tests {
		if got := isWithin(tt.path, tt.root); got != tt.want {
			t.Errorf("isWithin(%q, %q) = %v, want %v", tt.path, tt.root, got, tt.want)
		}
	}
}

func TestOutput(t *testing.T) {
	t.Parallel()

	r := &Runner{output: bytes.NewBufferString("runner diagnostic")}
	if got := r.Output(); got != "runner diagnostic" {
		t.Fatalf("Output() = %q, want runner diagnostic", got)
	}
}

func TestNspawnArgs(t *testing.T) {
	t.Parallel()

	r := &Runner{
		Name:        "runner-eaa075e1",
		JITConfig:   "some-jit-config",
		ImagePath:   "/opt/runner/image",
		Entrypoint:  "/opt/actions-runner/run.sh",
		MaskedPaths: []string{"/etc/actions-runner-processor/config.yaml", "/home/secret.pem"},
	}
	args := nspawnArgs(r)

	want := []string{
		"--quiet",
		"--directory=/opt/runner/image",
		"--volatile=overlay",
		"--as-pid2",
		"--user=runner",
		"--capability=CAP_SYS_ADMIN,CAP_NET_ADMIN",
		"--setenv=ACTIONS_RUNNER_INPUT_JITCONFIG=some-jit-config",
		"--machine=runner-eaa075e1",
		"--bind-ro=/etc/resolv.conf",
		"--bind-ro=/etc/hosts",
		"--bind-ro=/dev/null:/etc/actions-runner-processor/config.yaml",
		"/opt/actions-runner/run.sh",
	}
	if len(args) != len(want) {
		t.Fatalf("nspawnArgs() len = %d, want %d\ngot:  %v\nwant: %v", len(args), len(want), args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("nspawnArgs()[%d] = %q, want %q\ngot:  %v\nwant: %v", i, args[i], want[i], args, want)
		}
	}

	// The /home/secret.pem masked path is outside /etc and must be skipped,
	// and the /etc masked path must be a single SRC:DEST token (not two
	// separate args, which would make nspawn exec the bare path as command).
	for _, a := range args {
		if a == "/etc/actions-runner-processor/config.yaml" {
			t.Errorf("masked /etc path leaked as a bare arg (must be a single --bind-ro=/dev/null:SRC:DEST token): %v", args)
		}
		if a == "/home/secret.pem" {
			t.Errorf("non-/etc path leaked into args: %v", args)
		}
	}
}
