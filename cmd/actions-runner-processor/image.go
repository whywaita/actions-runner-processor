// Subcommands for the actions-runner-processor binary.
//
// The full runner image is too large to ship on a GitHub Release (2GB/file
// cap), so it is built on demand (build-image-full.yaml) and made available
// either at a URL the operator controls or directly from the repository's
// Actions build artifacts. This file implements:
//
//	actions-runner-processor image install-full --url <tarball-url> [--image-path <path>]
//	actions-runner-processor image install-full \
//	  --from-actions --owner <owner> --repo <repo> \
//	  [--artifact-prefix <prefix>] [--image-path <path>]
//
// which downloads the full-image artifact and expands it into the runner image
// subvolume, enforcing the btrfs requirement. The --from-actions mode
// authenticates as the configured GitHub App and pulls the most recent
// unexpired build artifact, so no externally-hosted URL is required.
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
	"strings"
	"syscall"

	"github.com/whywaita/actions-runner-processor/internal/client"
	"github.com/whywaita/actions-runner-processor/internal/config"
)

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
  actions-runner-processor image install-full --url <tarball-url> [--image-path <path>]
  actions-runner-processor image install-full --from-actions --owner <owner> --repo <repo> [--artifact-prefix <prefix>] [--image-path <path>]

Download and expand the full runner image into the image subvolume (btrfs enforced).
Use --url for an operator-hosted tarball, or --from-actions to pull the latest
build-image-full artifact from the repository's Actions runs.`)
}

func cmdInstallFullImage(args []string) int {
	fs := flag.NewFlagSet("image install-full", flag.ExitOnError)
	url := fs.String("url", "", "URL of the full image tar.gz")
	fromActions := fs.Bool("from-actions", false, "download the latest build-image-full artifact via GitHub App auth")
	owner := fs.String("owner", "", "GitHub owner (required with --from-actions)")
	repo := fs.String("repo", "", "GitHub repository (required with --from-actions)")
	artifactPrefix := fs.String("artifact-prefix", "actions-runner-image-full", "artifact name prefix to match")
	imagePath := fs.String("image-path", "", "runner image subvolume (default: config image_path or /opt/runner-btrfs/image)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *url == "" && !*fromActions {
		fmt.Fprintln(os.Stderr, "error: specify --url or --from-actions")
		fs.Usage()
		return 2
	}
	if *url != "" && *fromActions {
		fmt.Fprintln(os.Stderr, "error: --url and --from-actions are mutually exclusive")
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

	if err := installFullImage(*url, *fromActions, *owner, *repo, *artifactPrefix, *imagePath); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

// installFullImage downloads the full runner image and expands it into the
// image subvolume (replacing its contents). The source is either an
// operator-hosted tarball URL or, when fromActions is set, the latest matching
// build-image-full artifact pulled with GitHub App credentials from config.
func installFullImage(url string, fromActions bool, owner, repo, artifactPrefix, imagePath string) error {
	if err := ensureImageSubvolume(imagePath); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	auth := client.GitHubAuth{
		ClientID:   cfg.GitHub.ClientID,
		PrivateKey: cfg.GitHub.PrivateKey,
		APIURL:     cfg.GitHub.APIURL,
	}
	ctx := context.Background()

	var stream io.Reader
	var closeFn func() error
	if fromActions {
		stream, closeFn, err = openArtifactStream(ctx, auth, owner, repo, artifactPrefix)
	} else {
		stream, closeFn, err = openHTTPStream(url)
	}
	if err != nil {
		return err
	}
	defer func() { _ = closeFn() }()

	// The artifact source is a zip that wraps the tar.gz; a bare URL is the
	// tar.gz itself.
	if fromActions {
		fmt.Printf("expanding into %s\n", imagePath)
		if err := extractArtifactZip(stream, imagePath); err != nil {
			return fmt.Errorf("extract artifact: %w", err)
		}
	} else {
		gr, err := gzip.NewReader(stream)
		if err != nil {
			return fmt.Errorf("open gzip stream: %w", err)
		}
		defer gr.Close()
		if err := clearDir(imagePath); err != nil {
			return err
		}
		fmt.Printf("expanding into %s\n", imagePath)
		if err := extractTar(gr, imagePath); err != nil {
			return fmt.Errorf("extract: %w", err)
		}
	}

	// Sanity check: the tree must contain the runner.
	if ok, _ := dirHas(imagePath, "opt/actions-runner"); !ok {
		return fmt.Errorf("downloaded image does not look like a runner image (no opt/actions-runner)")
	}
	fmt.Println("full image installed at", imagePath)
	return nil
}

// openHTTPStream opens a plain (optionally gzipped) tarball from url.
func openHTTPStream(url string) (io.Reader, func() error, error) {
	fmt.Printf("downloading %s\n", url)
	resp, err := http.Get(url)
	if err != nil {
		return nil, nil, fmt.Errorf("download %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, nil, fmt.Errorf("download %s: HTTP %s", url, resp.Status)
	}
	return resp.Body, resp.Body.Close, nil
}

// openArtifactStream resolves the latest matching artifact and returns a
// reader over its zip payload plus a close function.
func openArtifactStream(ctx context.Context, auth client.GitHubAuth, owner, repo, prefix string) (io.Reader, func() error, error) {
	if owner == "" || repo == "" {
		return nil, nil, fmt.Errorf("--owner and --repo are required with --from-actions")
	}
	fmt.Printf("resolving latest artifact %q in %s/%s\n", prefix, owner, repo)
	artifactID, err := client.LatestArtifact(ctx, auth, owner, repo, prefix, nil)
	if err != nil {
		return nil, nil, err
	}
	fmt.Printf("downloading artifact %d\n", artifactID)

	r, w := io.Pipe()
	done := make(chan error, 1)
	go func() {
		defer func() { _ = w.Close() }()
		done <- client.DownloadArtifact(ctx, auth, owner, repo, artifactID, w)
	}()
	close := func() error {
		_ = r.Close()
		return <-done
	}
	return r, close, nil
}

// extractArtifactZip unwraps a Actions artifact (zip) containing the image
// tar.gz and expands it into dest.
func extractArtifactZip(r io.Reader, dest string) error {
	tmp, err := os.CreateTemp("", "actions-runner-image-*.zip")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
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
	defer gr.Close()

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
		return fmt.Errorf("parent %s is not on a btrfs filesystem (btrfs is enforced); mount a btrfs backing there (see deploy/setup.sh)", parent)
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
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		default:
			// skip devices, hardlinks handled by linkname via os.Link, etc.
		}
	}
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