// Subcommands for the actions-runner-processor binary.
//
// The full runner image is too large for a single GitHub Release asset (2GB/file
// cap), so build-image-full.yaml splits the tarball into <2GB parts and
// publishes them to the release as
//
//	actions-runner-image-full-<arch>.tar.gz.part-000, .part-001, ...
//
// This file implements `image install-full`, which downloads the split parts
// of the full image (default: the newest release; a specific release via
// --release), concatenates them back into the tar.gz, and expands it into the
// btrfs runner-image subvolume. Public release assets are read without
// authentication, so any user can install the full image.
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/whywaita/actions-runner-processor/internal/client"
	"github.com/whywaita/actions-runner-processor/internal/config"
)

// fullImageAssetPrefix is the release-asset name prefix for split full-image parts.
func fullImageAssetPrefix(arch string) string {
	return fmt.Sprintf("actions-runner-image-full-%s.tar.gz.part-", arch)
}

// runImageCmd dispatches the `image` subcommand tree.
func runImageCmd(args []string) int {
	if len(args) == 0 || (args[0] != "install-full") {
		usageImage()
		return 2
	}
	return cmdInstallFullImage(args[1:])
}

// usageImage prints the `image` subcommand usage.
func usageImage() {
	fmt.Fprintln(os.Stderr, `usage:
  actions-runner-processor image install-full [--release <tag|release-url>] [--image-path <path>] [--concurrency <n>]
  actions-runner-processor image install-full --url <tarball-url> [--image-path <path>]
  actions-runner-processor image install-full --from-actions [--owner <o> --repo <r>] [--artifact-prefix <p>] [--image-path <path>]

Download and expand the full runner image into the image subvolume (btrfs enforced).
Default pulls the split parts of the newest release in parallel (no auth needed);
--release selects a specific tag or release URL, --concurrency caps simultaneous
part downloads (0 = all in parallel). --url fetches a single operator-hosted
tarball; --from-actions pulls the latest build-image-full action artifact.`)
}

func cmdInstallFullImage(args []string) int {
	fs := flag.NewFlagSet("image install-full", flag.ExitOnError)
	release := fs.String("release", "", "GitHub tag or release page URL to install (default: latest release)")
	url := fs.String("url", "", "URL of a single full image tar.gz")
	fromActions := fs.Bool("from-actions", false, "download the latest build-image-full artifact via GitHub App auth")
	owner := fs.String("owner", "whywaita", "GitHub owner (default: whywaita)")
	repo := fs.String("repo", "actions-runner-processor", "GitHub repository (default: actions-runner-processor)")
	artifactPrefix := fs.String("artifact-prefix", "actions-runner-image-full", "artifact name prefix to match")
	imagePath := fs.String("image-path", "", "runner image subvolume (default: config image_path or /opt/runner-btrfs/image)")
	concurrency := fs.Int("concurrency", 0, "max simultaneous part downloads (0 = all parts in parallel)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	modes := 0
	if *release != "" {
		modes++
	}
	if *url != "" {
		modes++
	}
	if *fromActions {
		modes++
	}
	if modes > 1 {
		fmt.Fprintln(os.Stderr, "error: --release, --url and --from-actions are mutually exclusive")
		fs.Usage()
		return 2
	}
	if *imagePath == "" {
		if cfg, err := config.Load(); err == nil && cfg.Runner.ImagePath != "" {
			*imagePath = cfg.Runner.ImagePath
		}
	}
	if *imagePath == "" {
		*imagePath = "/opt/runner-btrfs/image"
	}

	if err := installFullImage(*release, *url, *fromActions, *owner, *repo, *artifactPrefix, *imagePath, *concurrency); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

// installFullImage downloads the full runner image and expands it into the
// image subvolume (replacing its contents). The source is, in precedence:
//   - --release: split parts of a specific or latest GitHub release (public, no auth)
//   - --url: a single operator-hosted tar.gz
//   - --from-actions: the latest build-image-full action artifact (GitHub App auth)
func installFullImage(release, url string, fromActions bool, owner, repo, artifactPrefix, imagePath string, concurrency int) error {
	if err := ensureImageSubvolume(imagePath); err != nil {
		return err
	}

	switch {
	case release != "":
		tag := resolveReleaseTag(release)
		fmt.Printf("fetching split full image from release %s (%s)\n", tag, owner+"/"+repo)
		if err := installFullFromRelease(owner, repo, tag, imagePath, concurrency); err != nil {
			return err
		}
	case url != "":
		if err := installFullFromURL(url, imagePath); err != nil {
			return err
		}
	case fromActions:
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		auth := client.GitHubAuth{
			ClientID:   cfg.GitHub.ClientID,
			PrivateKey: cfg.GitHub.PrivateKey,
			APIURL:     cfg.GitHub.APIURL,
		}
		if err := installFullFromArtifact(context.Background(), auth, owner, repo, artifactPrefix, imagePath); err != nil {
			return err
		}
	default:
		// No source given: default to the latest release (no auth needed).
		fmt.Printf("fetching split full image from the latest release (%s)\n", owner+"/"+repo)
		if err := installFullFromRelease(owner, repo, "", imagePath, concurrency); err != nil {
			return err
		}
	}

	// Sanity check: the tree must contain the runner.
	if ok, _ := dirHas(imagePath, "opt/actions-runner"); !ok {
		return fmt.Errorf("downloaded image does not look like a runner image (no opt/actions-runner)")
	}
	fmt.Println("full image installed at", imagePath)
	return nil
}

// resolveReleaseTag normalizes --release: a tag ("v0.0.4") is used as-is; a
// release page URL is reduced to its trailing tag.
func resolveReleaseTag(release string) string {
	if i := strings.LastIndex(release, "/releases/tag/"); i >= 0 {
		return release[i+len("/releases/tag/"):]
	}
	return release
}

// installFullFromRelease downloads the split parts of the given release
// (tagName=="" means the latest release) and expands the reconstructed tarball.
func installFullFromRelease(owner, repo, tagName, imagePath string, concurrency int) error {
	ctx := context.Background()
	assets, err := client.ListReleaseAssets(ctx, owner, repo, tagName)
	if err != nil {
		return err
	}
	arch := runtime.GOARCH
	prefix := fullImageAssetPrefix(arch)
	var parts []client.ReleaseAsset
	for _, a := range assets {
		if strings.HasPrefix(a.Name, prefix) {
			parts = append(parts, a)
		}
	}
	if len(parts) == 0 {
		return fmt.Errorf("release has no split full-image parts (prefix %q) for arch %s", prefix, arch)
	}
	// Order by the numeric part suffix (part-000, part-001, ...).
	sort.Slice(parts, func(i, j int) bool { return parts[i].Name < parts[j].Name })
	fmt.Printf("resolved %d parts for %s\n", len(parts), arch)

	// Download parts and concatenate them into a temp file on the same
	// filesystem as imagePath (NOT os.TempDir(): /tmp is often a small tmpfs,
	// and the reconstructed tarball is many GB). Remove the file on return.
	if err = os.MkdirAll(filepath.Dir(imagePath), 0o755); err != nil {
		return fmt.Errorf("create temp dir %s: %w", filepath.Dir(imagePath), err)
	}
	// Parts are independent; fetch them concurrently and write each to its
	// pre-allocated offset (bytes are sequential from the split, so WriteAt
	// preserves the ordering regardless of completion order). Concurrency
	// default is all parts at once; a smaller value bounds bandwidth/disk
	// on slow links or small hosts.
	if concurrency <= 0 {
		concurrency = len(parts)
	}
	tmp, err := os.CreateTemp(filepath.Dir(imagePath), "actions-runner-image-full-*.tar.gz")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err = tmp.Truncate(totalSize(parts)); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("preallocate %s: %w", tmpName, err)
	}
	_ = tmp.Close()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		sem      = make(chan struct{}, concurrency)
		offset   int64
	)
	for _, a := range parts {
		wg.Add(1)
		go func(part client.ReleaseAsset, off int64) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if werr := downloadReleasePart(tmpName, part, off); werr != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = werr
				}
				mu.Unlock()
			}
		}(a, offset)
		offset += a.Size
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}

	f, err := os.Open(tmpName)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("open gzip (concatenated parts may be corrupt): %w", err)
	}
	defer func() { _ = gr.Close() }()

	if err := clearDir(imagePath); err != nil {
		return err
	}
	fmt.Printf("expanding into %s\n", imagePath)
	if err := extractTar(gr, imagePath); err != nil {
		return err
	}
	return nil
}

// downloadReleasePart opens the concat temp file and downloads a single split
// part into it at the given offset. Kept as a helper so the goroutine body has
// its own error scope (avoids govet shadow warnings on the caller's err).
func downloadReleasePart(tmpName string, a client.ReleaseAsset, off int64) error {
	f, err := os.OpenFile(tmpName, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return appendReleaseAssetPart(f, a, off)
}

// appendReleaseAssetPart downloads a single split part asset and writes it into
// f at the given byte offset (preserving concatenation order across concurrent
// part downloads).
func appendReleaseAssetPart(f *os.File, a client.ReleaseAsset, off int64) error {
	fmt.Printf("downloading %s (%s) -> offset %d\n", a.Name, humanSize(a.Size), off)
	resp, err := http.Get(a.DownloadURL)
	if err != nil {
		return fmt.Errorf("download %s: %w", a.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %s", a.Name, resp.Status)
	}
	ow := &offsetWriter{f: f, o: off}
	if _, err := io.Copy(ow, resp.Body); err != nil {
		return fmt.Errorf("copy %s: %w", a.Name, err)
	}
	return nil
}

// offsetWriter wraps an *os.File so that io.Copy writes land at the given byte
// offset (WriteAt) instead of appending, letting concurrent parts be written in
// any order without corrupting the reconstruction. Pointer receiver so the
// running offset is reflected across multiple Write chunks.
type offsetWriter struct {
	f *os.File
	o int64
}

func (w *offsetWriter) Write(p []byte) (int, error) {
	n, err := w.f.WriteAt(p, w.o)
	w.o += int64(n)
	return n, err
}

// totalSize sums the byte sizes of all parts.
func totalSize(parts []client.ReleaseAsset) int64 {
	var n int64
	for _, a := range parts {
		n += a.Size
	}
	return n
}

// humanSize formats a byte count for progress output.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for nld := n / unit; nld >= unit; nld /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// installFullFromURL downloads a single operator-hosted tar.gz and expands it.
func installFullFromURL(url, imagePath string) error {
	fmt.Printf("downloading %s\n", url)
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %s", url, resp.Status)
	}
	gr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("open gzip stream: %w", err)
	}
	defer func() { _ = gr.Close() }()
	if err := clearDir(imagePath); err != nil {
		return err
	}
	fmt.Printf("expanding into %s\n", imagePath)
	if err := extractTar(gr, imagePath); err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	return nil
}

// installFullFromArtifact resolves the latest matching action artifact and
// expands it into dest. The artifact is a zip wrapping the image tar.gz.
func installFullFromArtifact(ctx context.Context, auth client.GitHubAuth, owner, repo, prefix, imagePath string) error {
	if owner == "" || repo == "" {
		return fmt.Errorf("--owner and --repo are required with --from-actions")
	}
	fmt.Printf("resolving latest artifact %q in %s/%s\n", prefix, owner, repo)
	artifactID, err := client.LatestArtifact(ctx, auth, owner, repo, prefix, nil)
	if err != nil {
		return err
	}
	fmt.Printf("downloading artifact %d\n", artifactID)

	r, w := io.Pipe()
	done := make(chan error, 1)
	go func() {
		defer func() { _ = w.Close() }()
		done <- client.DownloadArtifact(ctx, auth, owner, repo, artifactID, w)
	}()
	defer func() { _ = r.Close(); <-done }()

	if err := extractArtifactZip(r, imagePath); err != nil {
		return fmt.Errorf("extract artifact: %w", err)
	}
	return nil
}

// extractArtifactZip unwraps a Actions artifact (zip) containing the image
// tar.gz and expands it into dest.
func extractArtifactZip(r io.Reader, dest string) error {
	// Write the zip to the imagePath filesystem (NOT os.TempDir(): /tmp is
	// often a small tmpfs and the artifact is many GB).
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("create temp dir %s: %w", filepath.Dir(dest), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), "actions-runner-image-*.zip")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, cerr := io.Copy(tmp, r); cerr != nil {
		_ = tmp.Close()
		return cerr
	}
	if cerr := tmp.Close(); cerr != nil {
		return cerr
	}

	zr, err := zip.OpenReader(tmpName)
	if err != nil {
		return fmt.Errorf("open artifact zip: %w", err)
	}
	defer func() { _ = zr.Close() }()

	var imgEntry *zip.File
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, ".tar.gz") || strings.HasSuffix(f.Name, ".tgz") {
			imgEntry = f
			break
		}
	}
	if imgEntry == nil {
		return fmt.Errorf("artifact zip contains no .tar.gz entry")
	}

	f, err := imgEntry.Open()
	if err != nil {
		return fmt.Errorf("open %s: %w", imgEntry.Name, err)
	}
	defer func() { _ = f.Close() }()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("open gzip entry: %w", err)
	}
	defer func() { _ = gr.Close() }()

	if err := clearDir(dest); err != nil {
		return err
	}
	return extractTar(gr, dest)
}

// ensureImageSubvolume makes sure imagePath exists and is a btrfs subvolume,
// creating it under a btrfs parent if needed. Errors if the backing is not
// btrfs (btrfs is enforced).
func ensureImageSubvolume(path string) error {
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		if isBtrfsSubvolume(path) {
			return nil
		}
		return fmt.Errorf("%s exists but is not a btrfs subvolume (btrfs is enforced)", path)
	}
	parent := filepath.Dir(path)
	if !isBtrfs(parent) {
		return fmt.Errorf("parent %s is not on a btrfs filesystem (btrfs is enforced); provision a btrfs backing there via `actions-runner-processor setup`", parent)
	}
	if out, err := exec.Command("btrfs", "subvolume", "create", path).CombinedOutput(); err != nil {
		return fmt.Errorf("create btrfs subvolume %s: %s", path, strings.TrimSpace(string(out)))
	}
	return nil
}

func isBtrfs(path string) bool {
	if m, err := fsType(path); err == nil && m == btrfsSuperMagic {
		return true
	}
	return false
}

func isBtrfsSubvolume(path string) bool {
	if !isBtrfs(path) {
		return false
	}
	return exec.Command("btrfs", "subvolume", "show", path).Run() == nil
}

// extractTar reads a (possibly gzipped) tar stream from r and writes its
// entries under dest. Handles directories, regular files, and symlinks.
// Ownership (uid/gid) and mode are applied from the tar headers, matching the
// GitHub layout where the runner user owns /opt/actions-runner and /home/runner.
// Without this, install-full expanded every entry as the invoking (root) user
// and the runner (uid 1001) could not write its home/tool dirs at runtime.
func extractTar(r io.Reader, dest string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dest, hdr.Name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&0o777); err != nil {
				return err
			}
			if err := applyTarMeta(target, hdr); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			// Remove any placeholder from a prior entry of the same name so a
			// stale directory doesn't make os.Symlink fail with EEXIST.
			// (MkdirAll above only creates parents — target itself is fresh.)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
			if err := os.Lchown(target, hdr.Uid, hdr.Gid); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			if err := applyTarMeta(target, hdr); err != nil {
				return err
			}
		default:
			// skip devices, hardlinks handled by linkname via os.Link, etc.
		}
	}
}

// applyTarMeta applies a tar entry's ownership (uid/gid) and full mode to an
// already-extracted path. Chown must run BEFORE Chmod: chown clears the setuid
// and setgid bits on Linux, so the mode (including setuid e.g. /usr/bin/sudo
// 4755) is only guaranteed to survive if it is set last. hdr.Mode is stored in
// raw Unix mode bits (0o4000 = setuid, 0o2000 = setgid, 0o1000 = sticky), not
// Go's os.FileMode layout (ModeSetuid = 1<<22), so it cannot be passed straight
// to os.Chmod; tarModeToFileMode re-maps the special bits.
func applyTarMeta(target string, hdr *tar.Header) error {
	if err := os.Chown(target, hdr.Uid, hdr.Gid); err != nil {
		return err
	}
	return os.Chmod(target, tarModeToFileMode(hdr.Mode))
}

// tarModeToFileMode converts a raw Unix tar mode (archive/tar Header.Mode) into
// an os.FileMode, preserving the setuid/setgid/sticky special bits. archive/tar
// stores these in their standard Unix bit positions, which differ from
// os.FileMode's ModeSetuid/ModeSetgid/ModeSticky constants.
func tarModeToFileMode(m int64) os.FileMode {
	mode := os.FileMode(m & 0o777)
	if m&0o4000 != 0 {
		mode |= os.ModeSetuid
	}
	if m&0o2000 != 0 {
		mode |= os.ModeSetgid
	}
	if m&0o1000 != 0 {
		mode |= os.ModeSticky
	}
	return mode
}

func fsType(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Type), nil
}

// clearDir removes all entries inside dir (not dir itself).
func clearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// dirHas reports whether dir contains a path equal to sub.
func dirHas(dir, sub string) (bool, error) {
	if _, err := os.Stat(filepath.Join(dir, sub)); err != nil {
		return false, err
	}
	return true, nil
}
