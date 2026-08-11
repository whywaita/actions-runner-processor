package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHasCommandOption(t *testing.T) {
	t.Parallel()

	help := []byte("usage: bwrap [OPTIONS...]\n    --tmp-overlay DEST\n")
	if !hasCommandOption(help, "--tmp-overlay") {
		t.Fatal("hasCommandOption() = false, want true")
	}
	if hasCommandOption(help, "--overlay-src-missing") {
		t.Fatal("hasCommandOption() = true, want false")
	}
}

func TestConfigPath(t *testing.T) {
	t.Setenv("CONFIG_PATH", "/tmp/custom-config.yaml")
	if got := configPath(); got != "/tmp/custom-config.yaml" {
		t.Fatalf("configPath() = %q, want custom path", got)
	}
}

func TestCleanupRunnerWorkspaces(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	stale := filepath.Join(root, "runner-deadbeef")
	keep := filepath.Join(root, "keep-me")
	for _, path := range []string{stale, keep} {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := cleanupRunnerWorkspaces(root)
	if err != nil {
		t.Fatalf("cleanupRunnerWorkspaces() error = %v", err)
	}
	if removed != 1 {
		t.Fatalf("cleanupRunnerWorkspaces() removed = %d, want 1", removed)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale workspace still exists: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("unrelated directory was removed: %v", err)
	}
}
