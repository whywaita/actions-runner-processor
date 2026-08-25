package runner

import (
	"bytes"
	"testing"
)

func TestSetBusy(t *testing.T) {
	t.Parallel()

	r := &Runner{}
	if r.IsBusy() {
		t.Fatal("IsBusy() = true initially, want false")
	}
	r.SetBusy(true)
	if !r.IsBusy() {
		t.Fatal("IsBusy() = false after SetBusy(true), want true")
	}
	r.SetBusy(false)
	if r.IsBusy() {
		t.Fatal("IsBusy() = true after SetBusy(false), want false")
	}
}

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

	r := &Runner{output: &syncBuffer{buf: *bytes.NewBufferString("runner diagnostic")}}
	if got := r.Output(); got != "runner diagnostic" {
		t.Fatalf("Output() = %q, want runner diagnostic", got)
	}
}

func TestRunningJob(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{"nil output", "", false},
		{"empty output", "", false},
		{"still booting listener", "Connected to GitHub\nListening for Jobs", false},
		{"mid job", "Running job: test\ngo test ./...\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r *Runner
			if tt.output == "" {
				r = &Runner{output: nil}
			} else {
				r = &Runner{output: &syncBuffer{buf: *bytes.NewBufferString(tt.output)}}
			}
			if got := r.RunningJob(); got != tt.want {
				t.Errorf("RunningJob() = %v, want %v", got, tt.want)
			}
		})
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

	// The nspawn argument list (excluding the trailing entrypoint) is
	// order-sensitive; verify it positionally.
	wantPrefix := []string{
		"--quiet",
		"--directory=/opt/runner/image",
		"--ephemeral",
		"--as-pid2",
		"--user=runner",
		"--capability=CAP_SYS_ADMIN,CAP_NET_ADMIN",
		"--setenv=ACTIONS_RUNNER_INPUT_JITCONFIG=some-jit-config",
		"--machine=runner-eaa075e1",
		"--bind-ro=/etc/resolv.conf",
		"--bind-ro=/etc/hosts",
		"--bind-ro=/dev/null:/etc/actions-runner-processor/config.yaml",
	}
	for i := range wantPrefix {
		if args[i] != wantPrefix[i] {
			t.Fatalf("nspawnArgs() prefix[%d] = %q, want %q\ngot:  %v", i, args[i], wantPrefix[i], args)
		}
	}

	// No writable --bind should ever be emitted now: with --ephemeral the whole
	// root (including the job workspace /opt/actions-runner/_work) is a
	// discarded real-disk CoW snapshot.
	for _, a := range args {
		if a == "--bind" {
			t.Fatalf("unexpected writable --bind in args: %v", args)
		}
	}

	// Entrypoint must remain the last arg.
	if args[len(args)-1] != "/opt/actions-runner/run.sh" {
		t.Fatalf("entrypoint must remain the last arg, got: %v", args)
	}

	// The /home/secret.pem masked path is outside /etc and must be skipped, and
	// the /etc masked path must be a single SRC:DEST token (not two separate
	// args, which would make nspawn exec the bare path as command).
	for _, a := range args {
		if a == "/etc/actions-runner-processor/config.yaml" {
			t.Errorf("masked /etc path leaked as a bare arg (must be a single --bind-ro=/dev/null:SRC:DEST token): %v", args)
		}
		if a == "/home/secret.pem" {
			t.Errorf("non-/etc path leaked into args: %v", args)
		}
	}
}
