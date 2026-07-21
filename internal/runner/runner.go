// Package runner handles launching ephemeral GitHub Actions runners
// inside bubblewrap sandboxes.
package runner

import (
	"context"
	"fmt"
	"os/exec"
)

// Runner represents a running ephemeral runner instance.
type Runner struct {
	Name      string
	JITConfig string
	WorkDir   string

	cmd       *exec.Cmd
	overlayPID int
	runnerOverlayPID int
}

// Launch starts a runner inside a bubblewrap sandbox.
// The caller must have already set up overlayfs directories.
func Launch(ctx context.Context, r *Runner) error {
	jobID := r.Name
	overlayDir := fmt.Sprintf("/opt/runner/overlays/%s", jobID)
	runnerOverlayDir := fmt.Sprintf("/opt/runner/overlays/%s-runner", jobID)
	workspaceDir := fmt.Sprintf("/opt/runner/workspaces/%s", jobID)

	// Build the bwrap command
	args := []string{
		"--bind", fmt.Sprintf("%s/merged/usr", overlayDir), "/usr",
		"--bind", fmt.Sprintf("%s/merged/lib", overlayDir), "/lib",
		"--bind", fmt.Sprintf("%s/merged/lib64", overlayDir), "/lib64",
		"--bind", fmt.Sprintf("%s/merged/bin", overlayDir), "/bin",
		"--bind", fmt.Sprintf("%s/merged/etc", overlayDir), "/etc",
		"--bind", fmt.Sprintf("%s/merged", runnerOverlayDir), "/actions-runner",
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

	r.cmd = cmd

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("bwrap start: %w", err)
	}

	return nil
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
