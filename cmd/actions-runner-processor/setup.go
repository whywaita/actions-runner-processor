// `setup` subcommand — one-shot host bootstrap for actions-runner-processor.
//
// Historically the btrfs backing for the runner image was provisioned inside
// deploy/postinstall.sh (a deb maintainer script) and the image +
// NAT + service wiring lived in deploy/setup.sh. This command absorbs all of
// that into a single, root-run binary subcommand so a host can be brought up
// with one invocation:
//
//	sudo actions-runner-processor setup --image lightweight
//	sudo actions-runner-processor setup --image full
//
// The required --image flag selects the image kind, which drives the btrfs
// backing size:
//
//   - lightweight: fixed 20G default (operator-confirmed / --size overridable)
//   - full:        derived from the actual compressed tarball size x an
//     expansion factor, max'ed against 80% of the free disk, then
//     operator-confirmed (or forced via --size / --yes)
//
// The backing file is sparse (truncate -s), so the logical size only bounds the
// btrfs usable capacity; physical disk is consumed on demand as data is written.
package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/whywaita/actions-runner-processor/internal/client"
)

const (
	// Default paths mirror the deploy layout / config.example.yaml.
	defaultBtrfsImg   = "/var/lib/actions-runner-processor/runner-btrfs.img"
	defaultBtrfsMount = "/opt/runner-btrfs"
	defaultImagePath  = "/opt/runner-btrfs/image"

	// lightweightSize is the default btrfs backing size for the lightweight
	// image (kept from the old postinstall.sh BTRFS_SIZE default of 20G).
	lightweightSize = int64(20) * 1024 * 1024 * 1024

	// fullSizeFactor is how much larger than the compressed tarball the full
	// backing must be, to cover the expanded rootfs plus CoW snapshot headroom.
	fullSizeFactor = 3

	// freeSpaceRatio caps the full-size suggestion at 80% of the free disk so
	// the loopback backing never tries to fill the host filesystem.
	freeSpaceRatio = 0.8
)

var sizeRe = regexp.MustCompile(`(?i)^\s*([0-9]+(?:\.[0-9]+)?)\s*([kmgtp]?)(ib?)?\s*$`)

// setupOptions carries the parsed flags for the `setup` command.
type setupOptions struct {
	kind        string // "lightweight" or "full"
	sizeStr     string // --size override ("" = not set)
	yes         bool   // --yes: accept computed default without prompting
	img         string // loopback btrfs backing file
	mount       string // btrfs mount point
	imagePath   string // image subvolume
	release     string // image release tag ("" = latest)
	owner       string
	repo        string
	concurrency int
}

// runSetupCmd dispatches the `setup` subcommand.
func runSetupCmd(args []string) int {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	image := fs.String("image", "", "runner image kind: lightweight or full (required)")
	sizeStr := fs.String("size", "", "btrfs backing size override (e.g. 50G); skips the confirmation prompt")
	yes := fs.Bool("yes", false, "accept the computed default btrfs size without prompting")
	img := fs.String("btrfs-img", defaultBtrfsImg, "loopback btrfs backing file")
	mount := fs.String("mount", defaultBtrfsMount, "btrfs mount point")
	imagePath := fs.String("image-path", defaultImagePath, "runner image subvolume")
	release := fs.String("release", "", "image release tag (default: latest release)")
	owner := fs.String("owner", "whywaita", "GitHub owner")
	repo := fs.String("repo", "actions-runner-processor", "GitHub repository")
	concurrency := fs.Int("concurrency", 0, "full image: max simultaneous part downloads (0 = all in parallel)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	kind := strings.ToLower(strings.TrimSpace(*image))
	if kind != "lightweight" && kind != "full" {
		fmt.Fprintln(os.Stderr, "error: --image is required and must be 'lightweight' or 'full'")
		usageSetup()
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "error: setup must run as root (creates btrfs, mounts, and systemd units)")
		return 1
	}

	opts := &setupOptions{
		kind:        kind,
		sizeStr:     strings.TrimSpace(*sizeStr),
		yes:         *yes,
		img:         *img,
		mount:       *mount,
		imagePath:   *imagePath,
		release:     *release,
		owner:       *owner,
		repo:        *repo,
		concurrency: *concurrency,
	}
	if err := runSetup(opts); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

// usageSetup prints the `setup` subcommand usage.
func usageSetup() {
	fmt.Fprintln(os.Stderr, `usage:
  actions-runner-processor setup --image <lightweight|full> [options]

One-shot host bootstrap: provisions the loopback btrfs backing (sized for the
selected image kind), mounts it, creates the image subvolume, downloads and
expands the image, sets up NAT for the runner zone, and enables the service.

Required:
  --image <lightweight|full>   image kind; drives the default btrfs size

Size:
  --size <n>                   force the btrfs backing size (e.g. 50G); no prompt
  --yes                        accept the computed default size without prompting
                               (lightweight=20G; full=max(tarball-size*3, free*0.8))

Image source:
  --release <tag|url>          release to fetch (default: latest)
  --owner <o> --repo <r>       release owner/repo (default: whywaita/actions-runner-processor)
  --concurrency <n>            full: parallel part downloads (0 = all at once)

Paths:
  --btrfs-img <path>           loopback image file (default /var/lib/actions-runner-processor/runner-btrfs.img)
  --mount <path>               btrfs mount point (default /opt/runner-btrfs)
  --image-path <path>          image subvolume (default /opt/runner-btrfs/image)

Must run as root.`)
}

// runSetup performs the one-shot bootstrap in order.
func runSetup(opts *setupOptions) error {
	// 1. Determine the btrfs backing size.
	size, err := resolveBackingSize(opts)
	if err != nil {
		return err
	}
	fmt.Printf(">>> btrfs backing size: %s\n", humanSize(size))

	// 2. Provision the loopback btrfs backing + mount.
	if err := ensureBtrfsBacking(opts.img, opts.mount, size); err != nil {
		return err
	}

	// 3. Image subvolume.
	if err := ensureImageSubvolume(opts.imagePath); err != nil {
		return err
	}

	// 4. Fetch and expand the image.
	if opts.kind == "full" {
		if err := installFullImage(opts.release, "", false, opts.owner, opts.repo, "", opts.imagePath, opts.concurrency); err != nil {
			return err
		}
	} else {
		if err := installLightweightImage(opts.owner, opts.repo, opts.release, opts.imagePath); err != nil {
			return err
		}
	}

	// 5. Host networking for the runner zone.
	if err := setupNAT(); err != nil {
		return err
	}

	// 6. Enable the service (config still needs to be filled before start).
	if err := enableService(); err != nil {
		return err
	}
	fmt.Println(">>> setup complete. Fill in /etc/actions-runner-processor/config.yaml and run: systemctl start actions-runner-processor")
	return nil
}

// resolveBackingSize returns the size to use for the btrfs backing: --size
// wins outright; otherwise the per-kind default, overridable via the
// confirmation prompt unless --yes.
func resolveBackingSize(opts *setupOptions) (int64, error) {
	if opts.sizeStr != "" {
		s, err := parseSize(opts.sizeStr)
		if err != nil {
			return 0, err
		}
		return s, nil
	}
	def, err := defaultBackingSize(opts)
	if err != nil {
		return 0, err
	}
	if !opts.yes {
		return promptSize(def)
	}
	return def, nil
}

// defaultBackingSize computes the default btrfs backing size for the image kind.
func defaultBackingSize(opts *setupOptions) (int64, error) {
	if opts.kind == "lightweight" {
		return lightweightSize, nil
	}
	// full: the logical cap must hold the expanded rootfs plus snapshot
	// headroom, so size it from the real tarball (factor) and the free disk.
	compressed, err := fullImageCompressedSize(opts.owner, opts.repo, opts.release)
	if err != nil {
		return 0, err
	}
	free, err := freeBytes(filepath.Dir(opts.img))
	if err != nil {
		return 0, err
	}
	cand := computeFullSize(compressed, free)
	fmt.Printf(">>> full image compressed size %s; free disk %s\n", humanSize(compressed), humanSize(free))
	return cand, nil
}

// computeFullSize is the pure full-image backing-size formula:
// the larger of the expanded-rootfs estimate (compressed x factor) and 80% of
// the free disk, floored at the lightweight default.
func computeFullSize(compressed, free int64) int64 {
	cand := compressed * fullSizeFactor
	freeFrac := int64(float64(free) * freeSpaceRatio)
	if freeFrac > cand {
		cand = freeFrac
	}
	if cand < lightweightSize {
		cand = lightweightSize
	}
	return cand
}

// fullImageCompressedSize returns the total byte size of the split full-image
// parts for the given release ("" = latest), summed from the GitHub API
// (available before any download).
func fullImageCompressedSize(owner, repo, tag string) (int64, error) {
	assets, err := client.ListReleaseAssets(context.Background(), owner, repo, resolveReleaseTag(tag))
	if err != nil {
		return 0, err
	}
	prefix := fullImageAssetPrefix(runtime.GOARCH)
	var n int64
	for _, a := range assets {
		if strings.HasPrefix(a.Name, prefix) {
			n += a.Size
		}
	}
	if n == 0 {
		return 0, fmt.Errorf("release has no split full-image parts (prefix %q) for arch %s", prefix, runtime.GOARCH)
	}
	return n, nil
}

// freeBytes returns the free byte capacity of the filesystem containing path.
func freeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// parseSize parses a human size like "50G", "20GiB", "500M", or plain bytes.
func parseSize(s string) (int64, error) {
	m := sizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("invalid size %q (e.g. 50G, 20GiB, 10737418240)", s)
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	mult := 1.0
	switch strings.ToLower(m[2]) {
	case "k":
		mult = 1024
	case "m":
		mult = 1024 * 1024
	case "g":
		mult = 1024 * 1024 * 1024
	case "t":
		mult = 1024 * 1024 * 1024 * 1024
	case "p":
		mult = 1024 * 1024 * 1024 * 1024 * 1024
	}
	return int64(v * mult), nil
}

// promptSize asks the operator to confirm or adjust the default size.
// Enter accepts the default, a size input overrides it, "n" aborts.
func promptSize(def int64) (int64, error) {
	fmt.Printf("btrfs backing size: %s\n", humanSize(def))
	fmt.Printf("Press Enter to use %s, enter a size (e.g. 50G) to override, or n to abort: ", humanSize(def))
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	if strings.EqualFold(line, "n") {
		return 0, errors.New("aborted by user")
	}
	return parseSize(line)
}

// ensureBtrfsBacking creates (if missing) and mounts a loopback btrfs backing
// of the given logical size at mount, registered through a systemd .mount unit
// so it survives reboot.
func ensureBtrfsBacking(img, mount string, size int64) error {
	if err := os.MkdirAll(filepath.Dir(img), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(img), err)
	}
	if err := os.MkdirAll(mount, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", mount, err)
	}

	if btrfsMounted(mount) {
		fmt.Println(">>>", mount, "is already a btrfs mount")
	} else {
		if _, err := os.Stat(img); errors.Is(err, os.ErrNotExist) {
			if err := truncateFile(img, size); err != nil {
				return err
			}
			if err := mkfsBtrfs(img); err != nil {
				return err
			}
		}
		if err := writeMountUnit(img, mount); err != nil {
			return err
		}
		runQuiet("systemctl", "daemon-reload")
		runQuiet("systemctl", "enable", "actions-runner-btrfs.mount")
		if err := runQuietErr("systemctl", "start", "actions-runner-btrfs.mount"); err != nil {
			if merr := runQuietErr("mount", "-o", "loop", img, mount); merr != nil {
				return fmt.Errorf("could not mount %s: %v (start: %v)", mount, merr, err)
			}
		}
		fmt.Println(">>> btrfs backing mounted at", mount)
	}
	return nil
}

// btrfsMounted reports whether path is currently mounted as btrfs.
func btrfsMounted(path string) bool {
	out, err := exec.Command("findmnt", "-rn", "-o", "FSTYPE", path).Output()
	return err == nil && strings.Contains(string(out), "btrfs")
}

// truncateFile creates (or resizes) a sparse file of the given logical size.
func truncateFile(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(size); err != nil {
		return fmt.Errorf("truncate %s to %d: %w", path, size, err)
	}
	return nil
}

// mkfsBtrfs formats img as a btrfs filesystem.
func mkfsBtrfs(img string) error {
	if _, err := exec.LookPath("mkfs.btrfs"); err != nil {
		return fmt.Errorf("mkfs.btrfs not found: install btrfs-progs before running setup")
	}
	fmt.Println(">>> formatting", img, "as btrfs")
	if out, err := exec.Command("mkfs.btrfs", "-f", img).CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.btrfs failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// writeMountUnit writes the systemd .mount unit that mounts img at mount on boot.
func writeMountUnit(img, mount string) error {
	unit := fmt.Sprintf(`[Unit]
Description=actions-runner-processor runner image btrfs backing
Before=actions-runner-processor.service

[Mount]
What=%s
Where=%s
Type=btrfs
Options=loop,noatime
`, img, mount)
	path := "/etc/systemd/system/actions-runner-btrfs.mount"
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Println(">>> wrote", path)
	return nil
}

// installLightweightImage downloads the lightweight image tarball from the
// given release ("" = latest) and expands it into imagePath.
func installLightweightImage(owner, repo, tag, imagePath string) error {
	assets, err := client.ListReleaseAssets(context.Background(), owner, repo, resolveReleaseTag(tag))
	if err != nil {
		return err
	}
	arch := runtime.GOARCH
	name := fmt.Sprintf("actions-runner-image-%s.tar.gz", arch)
	var url string
	for _, a := range assets {
		if a.Name == name {
			url = a.DownloadURL
			break
		}
	}
	if url == "" {
		return fmt.Errorf("release has no lightweight image asset %q for arch %s", name, arch)
	}
	fmt.Printf(">>> downloading %s\n", url)
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
	fmt.Printf(">>> expanding into %s\n", imagePath)
	if err := extractTar(gr, imagePath); err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	fmt.Println("lightweight image installed at", imagePath)
	return nil
}

// setupNAT configures ip_forward and an outbound MASQUERADE rule for the
// private runner nspawn zone, mirroring deploy/setup.sh.
func setupNAT() error {
	uplink := defaultIface()
	if uplink == "" {
		fmt.Println(">>> WARNING: no default-route interface detected; skipped NAT setup")
		return nil
	}
	fmt.Println(">>> configuring host NAT for runner zone (egress", uplink+")")
	if err := os.WriteFile("/etc/sysctl.d/99-actions-runner-forward.conf", []byte("net.ipv4.ip_forward=1\n"), 0o644); err != nil {
		return fmt.Errorf("write sysctl conf: %w", err)
	}
	runQuiet("sysctl", "-w", "net.ipv4.ip_forward=1")

	unit := fmt.Sprintf(`[Unit]
Description=actions-runner-processor: NAT for the nspawn runner zone
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh -c 'iptables -t nat -C POSTROUTING -o "%s" -j MASQUERADE 2>/dev/null || iptables -t nat -A POSTROUTING -o "%s" -j MASQUERADE'

[Install]
WantedBy=multi-user.target
`, uplink, uplink)
	if err := os.WriteFile("/etc/systemd/system/actions-runner-nat.service", []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write NAT unit: %w", err)
	}
	runQuiet("systemctl", "daemon-reload")
	if err := runQuietErr("systemctl", "enable", "--now", "actions-runner-nat.service"); err != nil {
		return fmt.Errorf("enable NAT unit: %v", err)
	}
	return nil
}

// defaultIface returns the name of the default-route interface, or "".
func defaultIface() string {
	out, err := exec.Command("ip", "route").CombinedOutput()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[0] == "default" {
			for i := 0; i < len(f)-1; i++ {
				if f[i] == "dev" {
					return f[i+1]
				}
			}
		}
	}
	return ""
}

// enableService enables the processor service (does not start it — config must
// be filled in first).
func enableService() error {
	runQuiet("systemctl", "daemon-reload")
	if err := runQuietErr("systemctl", "enable", "actions-runner-processor.service"); err != nil {
		return fmt.Errorf("enable service: %v", err)
	}
	return nil
}

// runQuiet runs a command, discarding output (best-effort, errors ignored).
func runQuiet(name string, args ...string) {
	_ = exec.Command(name, args...).Run()
}

// runQuietErr runs a command, discarding output but returning an error.
func runQuietErr(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}
