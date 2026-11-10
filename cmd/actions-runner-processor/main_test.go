package main

import (
	"testing"
)

func TestConfigPath(t *testing.T) {
	t.Setenv("CONFIG_PATH", "/tmp/custom-config.yaml")
	if got := configPath(); got != "/tmp/custom-config.yaml" {
		t.Fatalf("configPath() = %q, want custom path", got)
	}
}

// TestReportSandboxStack smoke-tests the preflight diagnostics on a bare host
// (no systemd-nspawn, no nspawn tooling): it must only log, never fail.
func TestReportSandboxStack(t *testing.T) {
	reportSandboxStack()
}
