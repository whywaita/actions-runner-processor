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
