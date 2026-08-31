package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"sync"
	"syscall"
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

// TestExtractTarPreservesOwnership verifies that install-full expansion applies
// the tar headers' uid/gid to extracted entries — the fix for the runner being
// unable to write /opt/actions-runner and /home/runner after a split-part install.
func TestExtractTarPreservesOwnership(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to chown extracted entries")
	}
	// UID/GID for the container runner user.
	const uid, gid = 1001, 1001

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// /opt/actions-runner owned by the runner user.
	if err := tw.WriteHeader(&tar.Header{
		Name:     "opt/actions-runner/",
		Typeflag: tar.TypeDir,
		Mode:     0o755,
		Uid:      uid,
		Gid:      gid,
	}); err != nil {
		t.Fatal(err)
	}
	// A regular file inside it, also runner-owned, with a distinctive mode.
	const content = "hello"
	if err := tw.WriteHeader(&tar.Header{
		Name:     "opt/actions-runner/run.sh",
		Typeflag: tar.TypeReg,
		Mode:     0o750,
		Size:     int64(len(content)),
		Uid:      uid,
		Gid:      gid,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	// A root-owned setuid binary (sudo: 4755). Regression guard for issue #22:
	// extractTar must preserve the setuid/setgid bits, which a naive
	// mode&0o777 and/or a chmod-before-chown ordering would drop.
	const sucontent = "sudo"
	if err := tw.WriteHeader(&tar.Header{
		Name:     "usr/bin/sudo",
		Typeflag: tar.TypeReg,
		Mode:     0o4755,
		Size:     int64(len(sucontent)),
		Uid:      0,
		Gid:      0,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(sucontent)); err != nil {
		t.Fatal(err)
	}
	// A symlink, also runner-owned (must use Lchown, not Chown).
	if err := tw.WriteHeader(&tar.Header{
		Name:     "opt/actions-runner/self",
		Typeflag: tar.TypeSymlink,
		Linkname: "run.sh",
		Uid:      uid,
		Gid:      gid,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	if err := extractTar(bytes.NewReader(buf.Bytes()), dest); err != nil {
		t.Fatal(err)
	}

	check := func(p string, wantMode os.FileMode) {
		t.Helper()
		st, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		stat, ok := st.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatalf("%s: Stat_t unavailable (%T)", p, st.Sys())
		}
		if stat.Uid != uint32(uid) || stat.Gid != uint32(gid) {
			t.Errorf("%s: uid/gid = %d/%d, want %d/%d", p, stat.Uid, stat.Gid, uid, gid)
		}
		if got := st.Mode().Perm(); got != wantMode {
			t.Errorf("%s: mode = %o, want %o", p, got, wantMode)
		}
	}
	check(filepath.Join(dest, "opt/actions-runner"), 0o755)
	check(filepath.Join(dest, "opt/actions-runner/run.sh"), 0o750)
	check(filepath.Join(dest, "opt/actions-runner/self"), 0o777) // symlink perms are stubbed, only ownership matters

	// sudo must retain its setuid bit (0o4000 in unix mode == ModeSetuid);
	// st.Mode() surfaces ModeSetuid, unlike .Perm().
	suPath := filepath.Join(dest, "usr/bin/sudo")
	sust, err := os.Lstat(suPath)
	if err != nil {
		t.Fatal(err)
	}
	if sust.Mode()&os.ModeSetuid == 0 {
		t.Errorf("usr/bin/sudo lost setuid bit: mode=%v (want ModeSetuid)", sust.Mode())
	}
	if perms := sust.Mode().Perm(); perms != 0o755 {
		t.Errorf("usr/bin/sudo perms = %o, want 755", perms)
	}

	// Content must survive the copy too.
	got, err := os.ReadFile(filepath.Join(dest, "opt/actions-runner/run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("run.sh content = %q, want %q", got, content)
	}
}

// TestTarModeToFileMode verifies that raw Unix tar mode bits (setuid 0o4000,
// setgid 0o2000, sticky 0o1000) are re-mapped into os.FileMode's special-bit
// layout. This is the core of the sudo setuid fix and runs without root.
func TestTarModeToFileMode(t *testing.T) {
	tests := []struct {
		raw  int64
		want os.FileMode
	}{
		{0o755, 0o755},
		{0o4755, 0o755 | os.ModeSetuid},
		{0o2755, 0o755 | os.ModeSetgid},
		{0o1755, 0o755 | os.ModeSticky},
		{0o4755 | 0o2000, 0o755 | os.ModeSetuid | os.ModeSetgid},
		{0o4750, 0o750 | os.ModeSetuid},
	}
	for _, tc := range tests {
		if got := tarModeToFileMode(tc.raw); got != tc.want {
			t.Errorf("tarModeToFileMode(%#o) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
