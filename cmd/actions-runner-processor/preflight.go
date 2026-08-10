package main

import (
	"fmt"
	"os"
	"os/exec"
)

// preflight checks required dependencies are installed and configured.
func preflight() error {
	checks := []struct {
		name string
		fn   func() error
	}{
		{"bwrap", checkBinary("bwrap")},
		{"fuse-overlayfs", checkBinary("fuse-overlayfs")},
		{"/dev/fuse", checkDevFuse},
	}

	for _, c := range checks {
		if err := c.fn(); err != nil {
			return fmt.Errorf("%s: %w", c.name, err)
		}
	}
	return nil
}

func checkBinary(name string) func() error {
	return func() error {
		_, err := exec.LookPath(name)
		return err
	}
}

func checkDevFuse() error {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		return fmt.Errorf("/dev/fuse not found — install the 'fuse' package (apt install fuse)")
	}
	return nil
}
