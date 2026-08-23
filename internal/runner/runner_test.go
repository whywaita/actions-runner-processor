package runner

import (
	"bytes"
	"testing"
)

func TestIsWithin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path string
		root string
		want bool
	}{
		{"/etc/custom/key.pem", "/etc", true},
		{"/etc/hosts", "/etc", true},
		{"/home/hidden.pem", "/etc", false},
		{"/etc/..", "/etc", false},
		{"/etc2/foo", "/etc", false},
		{"/opt/runner/foo", "/opt", true},
	}
	for _, tt := range tests {
		if got := isWithin(tt.path, tt.root); got != tt.want {
			t.Errorf("isWithin(%q, %q) = %v, want %v", tt.path, tt.root, got, tt.want)
		}
	}
}

func TestOutput(t *testing.T) {
	t.Parallel()

	r := &Runner{output: bytes.NewBufferString("runner diagnostic")}
	if got := r.Output(); got != "runner diagnostic" {
		t.Fatalf("Output() = %q, want runner diagnostic", got)
	}
}
