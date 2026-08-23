// Package runner handles launching ephemeral GitHub Actions runners inside an
// isolated systemd-nspawn container.
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
	Name        string
	JITConfig   string
	MaskedPaths []string

	ImagePath  string // root filesystem directory (custom image)
	Entrypoint string // absolute path (in-container) of the boot command

	cmd    *exec.Cmd
	output *bytes.Buffer
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
	args := nspawnArgs(r)
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
	args = append(args, r.Entrypoint)
	return args
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
