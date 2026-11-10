// Sandbox stack detection.
//
// The processor launches raw `systemd-nspawn` machines and does not depend on
// the nspawn.org container tooling (github.com/nspawn/nspawn), but operators
// may run both on the same host. These probes report the installed stack so
// the startup log shows the environment the processor runs on.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"time"
)

// nspawnAPISocket is the host socket of the Docker Engine API served by the
// nspawn tooling since 3.0 (opened by nspawn-api.socket, shipped disabled).
// The processor never mounts it into runner machines; its presence only says
// the host runs the nspawn 3.x API service.
var nspawnAPISocket = "/run/nspawn/api.sock"

// systemdVersionRe matches the version banner of `systemd-nspawn --version`
// ("systemd 255", "systemd-nspawn 252" on older releases).
var systemdVersionRe = regexp.MustCompile(`(?m)^systemd(?:-nspawn)?\s+v?(\d+)\b`)

// SystemdNspawnVersion returns the systemd version of the `systemd-nspawn`
// binary on PATH (e.g. "255").
func SystemdNspawnVersion() (string, error) {
	out, err := exec.Command("systemd-nspawn", "--version").Output()
	if err != nil {
		return "", fmt.Errorf("systemd-nspawn --version: %w", err)
	}
	return parseSystemdVersion(string(out))
}

// parseSystemdVersion extracts "255" from a `systemd-nspawn --version` banner.
func parseSystemdVersion(out string) (string, error) {
	m := systemdVersionRe.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("no systemd version banner in %q", out)
	}
	return m[1], nil
}

// NspawnTooling describes the nspawn.org container tooling found on the host
// (nspawn 2.x machine records / 3.x Docker Engine API).
type NspawnTooling struct {
	// Installed reports whether the `nspawn` CLI is on PATH.
	Installed bool
	// Version is the service version reported by `nspawn info` (e.g. "3.2.0").
	Version string
	// ServiceUp reports whether the org.nspawn bus service answered. A CLI
	// without the bus service installed (`nspawn daemon --install`) does not
	// answer.
	ServiceUp bool
	// SystemdVersion is the systemd the service reports (Info's
	// systemd_version; empty when it could not be read).
	SystemdVersion string
	// APISocket reports whether the Docker Engine API socket exists on the
	// host (only meaningful on nspawn 3.x with nspawn-api.socket enabled).
	APISocket bool
}

// DetectNspawnTooling probes for the nspawn.org tooling. It never fails:
// absent tooling reads as the zero value. The probe is bounded so a wedged
// on-demand bus activation cannot stall the preflight.
func DetectNspawnTooling() NspawnTooling {
	var t NspawnTooling
	if _, err := exec.LookPath("nspawn"); err != nil {
		return t
	}
	t.Installed = true
	t.APISocket = apiSocketPresent(nspawnAPISocket)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nspawn", "info", "-f", "json").Output()
	if err != nil {
		// `nspawn info` exits 1 when the service does not answer.
		return t
	}
	t.ServiceUp = true
	t.Version, t.SystemdVersion = parseNspawnInfoJSON(out)
	return t
}

// parseNspawnInfoJSON extracts the version fields from `nspawn info -f json`
// output (Info's bus answer: {"version": "...", "systemd_version": "...", ...}).
// Unknown keys and shapes are tolerated: unparsable output yields empty strings.
func parseNspawnInfoJSON(out []byte) (version, systemdVersion string) {
	var info struct {
		Version        string `json:"version"`
		SystemdVersion string `json:"systemd_version"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &info); err != nil {
		return "", ""
	}
	return info.Version, info.SystemdVersion
}

// apiSocketPresent reports whether the nspawn Docker Engine API socket exists.
func apiSocketPresent(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
