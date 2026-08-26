package main

import (
	"bytes"
	"os"
	"sync"
	"testing"
)

func TestResolveReleaseTag(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"v0.0.4", "v0.0.4"},
		{"0.0.4", "0.0.4"},
		{"https://github.com/whywaita/actions-runner-processor/releases/tag/v0.0.4", "v0.0.4"},
		{"https://github.com/whywaita/actions-runner-processor/releases/tag/v0.0.5-rc2", "v0.0.5-rc2"},
	}
	for _, tc := range tests {
		if got := resolveReleaseTag(tc.in); got != tc.want {
			t.Errorf("resolveReleaseTag(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFullImageAssetPrefix(t *testing.T) {
	if got := fullImageAssetPrefix("amd64"); got != "actions-runner-image-full-amd64.tar.gz.part-" {
		t.Errorf("fullImageAssetPrefix(amd64) = %q", got)
	}
}

// TestOffsetWriterConcurrentOrder verifies that writing parts into a file at
// pre-allocated byte offsets from concurrent goroutines reconstructs the
// original byte order regardless of completion order.
func TestOffsetWriterConcurrentOrder(t *testing.T) {
	parts := [][]byte{
		bytes.Repeat([]byte("A"), 3000),
		bytes.Repeat([]byte("B"), 2000),
		bytes.Repeat([]byte("C"), 4000),
		bytes.Repeat([]byte("D"), 1000),
	}
	var want []byte
	for _, p := range parts {
		want = append(want, p...)
	}

	f, err := os.CreateTemp(t.TempDir(), "concat-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(int64(len(want))); err != nil {
		t.Fatal(err)
	}

	// Purposely submit in reverse order to prove order-independence.
	var wg sync.WaitGroup
	for i := len(parts) - 1; i >= 0; i-- {
		i := i
		var off int64
		for _, p := range parts[:i] {
			off += int64(len(p))
		}
		wg.Add(1)
		go func(idx int, o int64) {
			defer wg.Done()
			if _, err := (&offsetWriter{f: f, o: o}).Write(parts[idx]); err != nil {
				t.Errorf("write part %d: %v", idx, err)
			}
		}(i, off)
	}
	wg.Wait()

	got := make([]byte, len(want))
	if _, err := f.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("concurrent offset writes produced wrong order:\ngot  %x...\nwant %x...", got[:8], want[:8])
	}
}
