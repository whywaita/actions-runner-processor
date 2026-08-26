// Subcommands for the actions-runner-processor binary.
//
// The full runner image is too large to ship on a GitHub Release (2GB/file
// cap), so it is built on demand (build-image-full.yaml) and hosted at a URL
// the operator controls. This file implements:
//
//	actions-runner-processor image install-full --url <tarball-url> [--image-path <path>]
//
// which downloads the tarball and expands it into the runner image subvolume,
// enforcing the btrfs requirement.
package main

import (
	"archive/tar"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/whywaita/actions-runner-processor/internal/config"
)

// runImageCmd dispatches the `image` subcommand tree.
func runImageCmd(args []string) int {
	if len(args) == 0 || (args[0] != "install-full") {
		fmt.Fprintln(os.Stderr, "usage: actions-runner-processor image install-full --url <tarball-url> [--image-path <path>]")
		return 2
	}
	return cmdInstallFullImage(args[1:])
}

func cmdInstallFullImage(args []string) int {
	fs := flag.NewFlagSet("image install-full", flag.ExitOnError)
	url := fs.String("url", "", "URL of the full image tar.gz (required)")
	imagePath := fs.String("image-path", "", "runner image subvolume (default: config image_path or /opt/runner-btrfs/image)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *url == "" {
		fmt.Fprintln(os.Stderr, "error: --url is required")
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

	if err := installFullImage(*url, *imagePath); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

// installFullImage downloads the gzipped full-image tarball from url and
// expands it into the runner image subvolume (replacing its contents). btrfs
// is enforced for the image path.
func installFullImage(url, imagePath string) error {
	if err := ensureImageSubvolume(imagePath); err != nil {
		return err
	}

	fmt.Printf("downloading %s\n", url)
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %s", url, resp.Status)
	}

	gr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("open gzip stream: %w", err)
	}
	defer gr.Close()

	// Replace the image subvolume contents with the freshly-downloaded tree.
	// (A re-run overwrites; the processor boots from the same subvolume.)
	if err := clearDir(imagePath); err != nil {
		return err
	}
	fmt.Printf("extracting into %s\n", imagePath)
	if err := extractTar(gr, imagePath); err != nil {
		return fmt.Errorf("extract: %w", err)
	}

	// Sanity check: the archive must contain the runner.
	if ok, _ := dirHas(imagePath, "opt/actions-runner"); !ok {
		return fmt.Errorf("downloaded archive does not look like a runner image (no opt/actions-runner)")
	}
	fmt.Println("full image installed at", imagePath)
	return nil
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
