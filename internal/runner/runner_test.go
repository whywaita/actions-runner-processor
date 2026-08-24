package runner

import (
	"bytes"
	"os"
	"path/filepath"
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
		Name:         "runner-eaa075e1",
		JITConfig:    "some-jit-config",
		ImagePath:    "/opt/runner/image",
		Entrypoint:   "/opt/actions-runner/run.sh",
		MaskedPaths:  []string{"/etc/actions-runner-processor/config.yaml", "/home/secret.pem"},
		WorkspaceDir: "/opt/runner/workspaces/runner-eaa075e1",
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
		"--bind",
		"/opt/runner/workspaces/runner-eaa075e1:/opt/actions-runner/_work",
		"--bind",
		"/opt/runner/workspaces/runner-eaa075e1-tool:/opt/actions-runner/_tool",
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

func TestNspawnArgsNoWorkspace(t *testing.T) {
	t.Parallel()

	r := &Runner{
		Name:       "runner-eaa075e1",
		JITConfig:  "some-jit-config",
		ImagePath:  "/opt/runner/image",
		Entrypoint: "/opt/actions-runner/run.sh",
	}
	args := nspawnArgs(r)

	for _, a := range args {
		if a == "--bind" {
			t.Fatalf("no workspace bind expected when WorkspaceDir is empty, got: %v", args)
		}
	}
	if args[len(args)-1] != "/opt/actions-runner/run.sh" {
		t.Fatalf("entrypoint must remain the last arg, got: %v", args)
	}
}

func TestPrepareWorkspace(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown to uid 1001 requires root")
	}

	base := t.TempDir()
	workspaceDir := filepath.Join(base, "ws")
	if err := prepareWorkspace(workspaceDir); err != nil {
		t.Fatalf("prepareWorkspace() error = %v", err)
	}

	for _, dir := range []string{workspaceDir, workspaceDir + "-tool"} {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if !fi.IsDir() {
			t.Errorf("%s is not a directory", dir)
		}
	}
}
