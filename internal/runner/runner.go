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
	"sync/atomic"
	"time"
)

// runnerUID is the in-container uid of the `runner` user (uid 1001, matching
// the GitHub-hosted/image layout). The protected JIT-config file is chowned to
// this uid so the container's entrypoint (which runs as `runner`) can read it.
const runnerUID = 1001

// jitDir is a root-only tmpfs directory holding one per-runner JIT config file
// (mode 0600). The file is bind-mounted read-only into the container instead of
// being passed on the systemd-nspawn argv, so the short-lived runner
// registration credential is never visible in /proc/<pid>/cmdline.
const jitDir = "/run/actions-runner-processor"

func jitFilePath(name string) string {
	return filepath.Join(jitDir, name+".jitconfig")
}

// Runner represents a running ephemeral runner instance.
type Runner struct {
	Name        string
	JITConfig   string
	MaskedPaths []string

	ImagePath  string // root filesystem directory (custom image)
	Entrypoint string // absolute path (in-container) of the boot command

	cmd    *exec.Cmd
	output *syncBuffer // mutex-protected so RunningJob can read while exec copies

	// busy tracks whether a GitHub job has been assigned to this runner (set by
	// the scaler via SetBusy when it sees a JobStarted for this runner). It is
	// authoritative and does not depend on parsing captured output, so graceful
	// shutdown can reliably tell an in-flight runner apart from an idle one even
	// before the "Running job:" line is flushed.
	busy atomic.Bool

	// startedAt records when the container launched. Used by graceful shutdown
	// to give a runner a short "assignment grace" window: right after launch a
	// runner may have accepted a job whose JobStarted has not been processed
	// yet, in which case both busy and output are unset and it must not be
	// killed as idle.
	startedAt time.Time
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
// The container boots from the custom image directory (ImagePath) with
// --ephemeral: the image (a btrfs subvolume) is CoW-snapshotted onto real
// disk, the container runs against the writable snapshot, and the snapshot is
// discarded when the container exits. Networking is shared with the host (no
// --private-network) so the runner can reach GitHub over outbound HTTPS.
// The runner process boots in the container as the `runner` user (--user=runner,
// matching the GitHub-hosted image layout); the user has passwordless sudo, so
// `sudo` is available in job steps. CAP_SYS_ADMIN + CAP_NET_ADMIN are granted
// so dockerd (and thus `docker` in job steps) can run inside the container:
// CAP_SYS_ADMIN covers storage/mount namespaces, CAP_NET_ADMIN the netfilter
// (iptables) bridge network driver.
func Launch(ctx context.Context, r *Runner) error {
	if err := writeJITFile(r); err != nil {
		return err
	}
	args := nspawnArgs(r)
	// Launch the container on a context that is unaffected by cancellation of
	// the caller's context (e.g. the processor's SIGTERM shutdown context).
	// If we bound the nspawn process to the signal-cancelled context,
	// exec.CommandContext would SIGKILL the container mid-job the moment the
	// process begins its graceful shutdown, losing in-flight jobs.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), "systemd-nspawn", args...)

	output := &syncBuffer{}
	cmd.Stdout = output
	cmd.Stderr = output
	r.output = output
	r.cmd = cmd

	if err := cmd.Start(); err != nil {
		// cleanupJIT normally runs from Wait (on a successful launch). If
		// systemd-nspawn cannot start, Wait is never reached, so remove the
		// credential file here to avoid leaking runner credentials under
		// /run/actions-runner-processor across repeated launch failures.
		r.cleanupJIT()
		return fmt.Errorf("systemd-nspawn start: %w", err)
	}
	r.startedAt = time.Now()
	return nil
}

// AssignmentGraceElapsed reports whether a runner's job-assignment state can be
// trusted. Within the grace window after launch, a runner may have accepted a
// job whose JobStarted has not been processed yet (both busy and captured
// output are still unset), so it must not be classified as idle. Graceful
// shutdown uses this before killing an idle-looking runner.
func (r *Runner) AssignmentGraceElapsed(grace time.Duration) bool {
	return !r.startedAt.IsZero() && time.Since(r.startedAt) >= grace
}

// writeJITFile persists the encoded JIT config to a root-only file (mode 0600,
// chown'd to the container's `runner` uid so the in-image entrypoint can read
// it). Launching with this file bound read-only into the container keeps the
// credential off the process argv.
func writeJITFile(r *Runner) error {
	if r.JITConfig == "" {
		return nil
	}
	if err := os.MkdirAll(jitDir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", jitDir, err)
	}
	p := jitFilePath(r.Name)
	if err := os.WriteFile(p, []byte(r.JITConfig), 0o600); err != nil {
		return fmt.Errorf("write jit config: %w", err)
	}
	if err := os.Chown(p, runnerUID, runnerUID); err != nil {
		return fmt.Errorf("chown jit config: %w", err)
	}
	return nil
}

func (r *Runner) cleanupJIT() {
	if r.JITConfig == "" {
		return
	}
	_ = os.Remove(jitFilePath(r.Name))
}

// nspawnArgs builds the systemd-nspawn argument list for a runner. Extracted
// from Launch so the arg assembly can be unit-tested.
//
// The container is booted with systemd as PID 1 (--boot): this makes
// `systemctl` usable inside job steps (so the full image's dockerd can be
// started with `systemctl start docker`) and gives the private network
// namespace a working DHCP client (systemd-networkd). --network-zone puts the
// runner in its own network namespace on a private nspawn-managed bridge, so
// the granted CAP_NET_ADMIN/CAP_SYS_ADMIN (needed by dockerd) can only affect
// the container's own netns, never the host. Outbound internet is provided by
// host-side NAT. The JIT credential travels via a bind-mounted protected file,
// never through argv.
//
// The zone name is derived from the runner name (not a shared static "runner"):
// a fixed shared zone would attach every concurrent runner to the SAME bridge
// (vz-runner), letting untrusted jobs reach each other over L2 even though they
// have separate netns. A per-runner zone isolates each job on its own bridge.
func nspawnArgs(r *Runner) []string {
	args := []string{
		"--quiet",
		"--directory=" + r.ImagePath,
		"--ephemeral",
		"--boot",
		"--network-zone=" + zoneFor(r.Name),
		"--capability=CAP_SYS_ADMIN,CAP_NET_ADMIN",
		"--machine=" + r.Name,
	}
	if r.JITConfig != "" {
		args = append(args, "--bind-ro="+jitFilePath(r.Name)+":/opt/actions-runner/.jitconfig")
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
	return args
}

// zoneFor returns a short, unique network-zone name for a runner. systemd-nspawn
// names the zone's bridge "vz-" + zone; Linux interface names cap at 15 chars, so
// a zone derived from the full runner name ("runner-<8 hex>") would exceed that
// and break bridge creation. The runner name's trailing 8-hex id is unique per
// job, so reuse it with a short "rn-" prefix (bridge "vz-rn-<id>", 14 chars).
func zoneFor(name string) string {
	id := name
	if i := strings.LastIndexByte(name, '-'); i >= 0 && len(name)-i-1 == 8 {
		id = name[i+1:]
	}
	return "rn-" + id
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

// IsBusy reports whether a GitHub job has been assigned to this runner, as
// tracked by the scaler via SetBusy on JobStarted/JobCompleted. Unlike
// RunningJob it does not rely on output capture timing.
func (r *Runner) IsBusy() bool {
	return r.busy.Load()
}

// SetBusy marks whether this runner currently has an assigned job. The scaler
// calls it from HandleJobStarted (true) and HandleJobCompleted (false).
func (r *Runner) SetBusy(busy bool) {
	r.busy.Store(busy)
}

// Wait blocks until the runner exits.
func (r *Runner) Wait() error {
	if r.cmd == nil {
		return nil
	}
	err := r.cmd.Wait()
	r.cleanupJIT()
	return err
}

// Kill terminates the runner process tree.
func (r *Runner) Kill() error {
	if r.cmd == nil || r.cmd.Process == nil {
		return nil
	}
	return r.cmd.Process.Kill()
}
