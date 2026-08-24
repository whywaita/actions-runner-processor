// Package runner handles launching ephemeral GitHub Actions runners inside an
// isolated systemd-nspawn container.
package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// runnerUID is the in-container uid of the `runner` user (uid 1001, matching
// the GitHub-hosted/image layout) that runs job steps. Bind-mounted host
// workspace directories must be owned by this uid so the container runner
// (which runs as uid 1001) can write into them.
const runnerUID = 1001

// Runner represents a running ephemeral runner instance.
type Runner struct {
	Name        string
	JITConfig   string
	MaskedPaths []string

	ImagePath  string // root filesystem directory (custom image)
	Entrypoint string // absolute path (in-container) of the boot command

	// WorkspaceDir is the host directory bind-mounted into the container at
	// /opt/actions-runner/_work (and its _tool sibling). Placing the workspace
	// and tool cache on real disk is essential: --volatile=overlay puts all
	// writes inside the container on a RAM-backed tmpfs overlay / toolchain
	// (Go, Node, ...) extraction fills it up with ENOSPC even when the host
	// disk has plenty of free space. Empty string disables the bind (falls
	// back to the overlay).
	WorkspaceDir string

	cmd    *exec.Cmd
	output *syncBuffer // mutex-protected so RunningJob can read while exec copies
}

// syncBuffer is a concurrency-safe in-memory byte buffer used to capture a
// runner's combined stdout/stderr. os/exec writes into it from copy goroutines
// while Scaler.Shutdown may read it (RunningJob) to decide whether to drain.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (n int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Launch boots a runner in a systemd-nspawn container.
//
// The container boots from the custom image directory (ImagePath) with an
// ephemeral overlayed root (--volatile=overlay): the image is mounted
// read-only and all writes land on a private overlay layer that is discarded
// when the container exits. Networking is shared with the host (no
// --private-network) so the runner can reach GitHub over outbound HTTPS.
// The runner process boots in the container as the `runner` user (--user=runner,
// matching the GitHub-hosted image layout); the user has passwordless sudo, so
// `sudo` is available in job steps. CAP_SYS_ADMIN + CAP_NET_ADMIN are granted
// so dockerd (and thus `docker` in job steps) can run inside the container:
// CAP_SYS_ADMIN covers storage/mount namespaces, CAP_NET_ADMIN the netfilter
// (iptables) bridge network driver.
func Launch(ctx context.Context, r *Runner) error {
	// Create and prepare the host workspace dirs before booting the container.
	// The bind source must exist and be writable by the container's `runner`
	// user (uid 1001). The processor runs as root, so chown works here.
	if r.WorkspaceDir != "" {
		if err := prepareWorkspace(r.WorkspaceDir); err != nil {
			return fmt.Errorf("prepare workspace: %w", err)
		}
	}
	args := nspawnArgs(r)
	// Launch the container on a context that is unaffected by cancellation of
	// the caller's context (e.g. the processor's SIGTERM shutdown context).
	// If we bound the nspawn process to the signal-cancelled context,
	// exec.CommandContext would SIGKILL the container mid-job the moment the
	// process begins its graceful shutdown, losing in-flight jobs.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), "systemd-nspawn", args...)
	// Keep JIT config in the child environment too so any wrapper process the
	// entrypoint spawns inherits it.
	cmd.Env = append(cmd.Environ(),
		"ACTIONS_RUNNER_INPUT_JITCONFIG="+r.JITConfig,
		"HOME=/home/runner",
	)

	output := &syncBuffer{}
	cmd.Stdout = output
	cmd.Stderr = output
	r.output = output
	r.cmd = cmd

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("systemd-nspawn start: %w", err)
	}
	return nil
}

// nspawnArgs builds the systemd-nspawn argument list for a runner. Extracted
// from Launch so the arg assembly can be unit-tested.
func nspawnArgs(r *Runner) []string {
	args := []string{
		"--quiet",
		"--directory=" + r.ImagePath,
		"--volatile=overlay",
		"--as-pid2",
		"--user=runner",
		"--capability=CAP_SYS_ADMIN,CAP_NET_ADMIN",
		"--setenv=ACTIONS_RUNNER_INPUT_JITCONFIG=" + r.JITConfig,
		"--machine=" + r.Name,
		"--bind-ro=/etc/resolv.conf",
		"--bind-ro=/etc/hosts",
	}
	for _, path := range r.MaskedPaths {
		if !isWithin(path, "/etc") {
			continue
		}
		// systemd-nspawn's bind source and destination are a single token
		// (SRC:DEST). Passing them as separate args would make systemd-nspawn
		// treat the bare destination path as the in-container command to run.
		args = append(args, "--bind-ro=/dev/null:"+path)
	}
	// Bind the workspace (and tool cache) onto real host disk. Under
	// --volatile=overlay every other write lands on the RAM-backed tmpfs
	// overlay, which the Go/Node toolchain extraction exhausts (ENOSPC). Only
	// bind when a WorkspaceDir is configured.
	if r.WorkspaceDir != "" {
		args = append(args, "--bind", r.WorkspaceDir+":/opt/actions-runner/_work")
		args = append(args, "--bind", r.WorkspaceDir+"-tool:/opt/actions-runner/_tool")
	}
	args = append(args, r.Entrypoint)
	return args
}

func isWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// prepareWorkspace creates the host workspace and tool-cache directories that
// are bind-mounted into the container at /opt/actions-runner/_work and
// /opt/actions-runner/_tool, and chowns them to the container's `runner` user
// (uid 1001). The container runner writes job workspaces and toolchain caches
// into these dirs, which must therefore exist and be runner-owned before the
// container boots.
func prepareWorkspace(workspaceDir string) error {
	dirs := []string{workspaceDir, workspaceDir + "-tool"}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
		if err := os.Chown(dir, runnerUID, runnerUID); err != nil {
			return fmt.Errorf("chown %s: %w", dir, err)
		}
	}
	return nil
}

// Output returns the captured stdout and stderr from the runner process.
func (r *Runner) Output() string {
	if r.output == nil {
		return ""
	}
	return r.output.String()
}

// RunningJob reports whether the runner has started executing a job
// ("Running job: ..." appears in its captured output) and has not yet exited.
// Used during graceful shutdown to decide whether to drain (wait for an
// in-flight job to finish) or tear down immediately.
func (r *Runner) RunningJob() bool {
	if r.output == nil {
		return false
	}
	return strings.Contains(r.output.String(), "Running job:")
}

// Wait blocks until the runner exits.
func (r *Runner) Wait() error {
	if r.cmd == nil {
		return nil
	}
	return r.cmd.Wait()
}

// Kill terminates the runner process tree.
func (r *Runner) Kill() error {
	if r.cmd == nil || r.cmd.Process == nil {
		return nil
	}
	return r.cmd.Process.Kill()
}
