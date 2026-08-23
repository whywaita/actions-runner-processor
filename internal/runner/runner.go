// Package runner handles launching ephemeral GitHub Actions runners
// inside an isolated sandbox (systemd-nspawn by default, bubblewrap as a
// deprecated fallback).
package runner

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// Runner represents a running ephemeral runner instance.
type Runner struct {
	Name              string
	JITConfig         string
	ActionsRunnerPath string
	WorkDir           string
	MaskedPaths       []string

	// Mode is the sandbox backend: "nspawn" (default) or "bwrap".
	Mode       string
	ImagePath  string // nspawn: root filesystem directory (custom image)
	Entrypoint string // nspawn: absolute path (in-container) of the boot command

	cmd    *exec.Cmd
	output *bytes.Buffer
}

// Launch starts a runner in an isolated sandbox.
func Launch(ctx context.Context, r *Runner) error {
	if r.Mode == "bwrap" {
		return launchBwrap(ctx, r)
	}
	return launchNspawn(ctx, r)
}

// launchNspawn boots the runner in a systemd-nspawn container.
//
// The container boots from the custom image directory (ImagePath) with an
// ephemeral overlayed root (--volatile=overlay): the image is mounted
// read-only and all writes land on a private overlay layer that is discarded
// when the container exits. Networking is shared with the host (no
// --private-network) so the runner can reach GitHub over outbound HTTPS.
// The runner process boots in the container as the `runner` user (--user=runner,
// matching the GitHub-hosted image layout); the user has passwordless sudo, so
// `sudo` is available in job steps.
func launchNspawn(ctx context.Context, r *Runner) error {
	args := []string{
		"--quiet",
		"--directory=" + r.ImagePath,
		"--volatile=overlay",
		"--as-pid2",
		"--user=runner",
		"--setenv=ACTIONS_RUNNER_INPUT_JITCONFIG=" + r.JITConfig,
		"--machine=" + r.Name,
		"--bind-ro=/etc/resolv.conf",
		"--bind-ro=/etc/hosts",
	}
	for _, path := range r.MaskedPaths {
		if !isWithin(path, "/etc") {
			continue
		}
		args = append(args, "--bind-ro=/dev/null", path)
	}
	args = append(args, r.Entrypoint)

	cmd := exec.CommandContext(ctx, "systemd-nspawn", args...)
	// Keep JIT config in the child environment too so any wrapper process the
	// entrypoint spawns inherits it.
	cmd.Env = append(cmd.Environ(),
		"ACTIONS_RUNNER_INPUT_JITCONFIG="+r.JITConfig,
		"HOME=/home/runner",
	)

	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	r.output = &output
	r.cmd = cmd

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("systemd-nspawn start: %w", err)
	}
	return nil
}

// launchBwrap starts the runner in a bubblewrap sandbox (deprecated).
func launchBwrap(ctx context.Context, r *Runner) error {
	cmd := exec.CommandContext(ctx, "bwrap", buildArgs(r)...)
	cmd.Env = append(cmd.Environ(),
		"ACTIONS_RUNNER_INPUT_JITCONFIG="+r.JITConfig,
		"HOME=/home/runner",
		"RUNNER_ALLOW_RUNASROOT=1",
	)

	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	r.output = &output
	r.cmd = cmd

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("bwrap start: %w", err)
	}
	return nil
}

func buildArgs(r *Runner) []string {
	args := []string{
		"--overlay-src", "/usr", "--tmp-overlay", "/usr",
		"--overlay-src", "/lib", "--tmp-overlay", "/lib",
		"--overlay-src", "/lib64", "--tmp-overlay", "/lib64",
		"--overlay-src", "/bin", "--tmp-overlay", "/bin",
		"--overlay-src", "/sbin", "--tmp-overlay", "/sbin",
		"--overlay-src", "/etc", "--tmp-overlay", "/etc",
		"--overlay-src", "/var", "--tmp-overlay", "/var",
		"--overlay-src", r.ActionsRunnerPath, "--tmp-overlay", "/actions-runner",
		"--tmpfs", "/run",
		"--dir", "/run/systemd",
		"--dir", "/run/systemd/resolve",
		"--ro-bind", "/etc/resolv.conf", "/etc/resolv.conf",
		"--ro-bind", "/etc/hosts", "/etc/hosts",
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/home/runner",
		"--tmpfs", "/tmp",
		"--bind", r.WorkDir, "/actions-runner/_work",
		"--unshare-all",
		"--share-net",
		"--uid", "0",
		"--gid", "0",
		"--die-with-parent",
		"--new-session",
		"--chdir", "/actions-runner",
	}
	for _, path := range r.MaskedPaths {
		if !isWithin(path, "/etc") {
			continue
		}
		args = append(args, "--ro-bind", "/dev/null", path)
	}
	return append(args, "/actions-runner/run.sh")
}

func isWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// Output returns the captured stdout and stderr from the runner process.
func (r *Runner) Output() string {
	if r.output == nil {
		return ""
	}
	return r.output.String()
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
