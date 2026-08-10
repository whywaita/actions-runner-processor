// Package runner handles launching ephemeral GitHub Actions runners
// inside bubblewrap sandboxes.
package runner

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// Runner represents a running ephemeral runner instance.
type Runner struct {
	Name      string
	JITConfig string
	WorkDir   string

	cmd    *exec.Cmd
	stderr *bytes.Buffer
}

// Launch starts a runner inside a bubblewrap sandbox.
func Launch(ctx context.Context, r *Runner) error {
	jobID := r.Name
	overlayDir := fmt.Sprintf("/opt/runner/overlays/%s", jobID)
	runnerOverlayDir := fmt.Sprintf("/opt/runner/overlays/%s-runner", jobID)
	workspaceDir := fmt.Sprintf("/opt/runner/workspaces/%s", jobID)

	args := []string{
		"--bind", overlayDir + "/merged/usr", "/usr",
		"--bind", overlayDir + "/merged/lib", "/lib",
		"--bind", overlayDir + "/merged/lib64", "/lib64",
		"--bind", overlayDir + "/merged/bin", "/bin",
		"--bind", overlayDir + "/merged/etc", "/etc",
		"--bind", runnerOverlayDir + "/merged", "/actions-runner",
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/home/runner",
		"--tmpfs", "/tmp",
		"--tmpfs", "/var",
		"--bind", workspaceDir, "/actions-runner/_work",
		"--unshare-all",
		"--share-net",
		"--die-with-parent",
		"--new-session",
		"/actions-runner/run.sh",
	}

	cmd := exec.CommandContext(ctx, "bwrap", args...)
	cmd.Env = append(cmd.Environ(), "ACTIONS_RUNNER_INPUT_JITCONFIG="+r.JITConfig)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	r.stderr = &stderr

	r.cmd = cmd

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("bwrap start: %w", err)
	}

	return nil
}

// StderrOutput returns the captured stderr from the runner process.
func (r *Runner) StderrOutput() string {
	if r.stderr == nil {
		return ""
	}
	return r.stderr.String()
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
