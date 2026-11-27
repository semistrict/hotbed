// Package setup provisions a hotbed worker node on the current machine in a
// single command: directly on Linux (using native btrfs/zfs or a sparse
// file-backed ZFS pool in ~/.hotbed), and on macOS natively too, every
// command in a VM of machined's, which hotbed's daemon runs beside the
// worker (internal/macworker).
package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/semistrict/hotbed/internal/macworker"
)

const (
	// DefaultDiskSize is the default virtual size of the sparse ZFS disk image.
	DefaultDiskSize = 256 << 30 // 256 GiB
	// DefaultPoolName is the ZFS pool name created for file- and disk-image-backed workers.
	DefaultPoolName = "hotbed"
	// DaemonLaunchAgentLabel is the unified macOS LaunchAgent label for `hotbed daemon`.
	DaemonLaunchAgentLabel = "com.semistrict.hotbed.daemon"
)

// Options configure single-command worker setup on Linux or macOS.
type Options struct {
	Controller     string
	Token          string // optional bearer/join token for remote HTTPS controllers
	Dir            string // base directory; default ~/.hotbed
	User           string // unix user commands run as; default current/SUDO_USER
	DiskSize       int64  // virtual sparse ZFS disk size in bytes
	ZFSDataset     string // optional ZFS dataset override (Linux native ZFS)
	ZFSDevice      string // optional block device path under /dev/ for a ZFS pool (Linux)
	ZFSARCMax      int64  // Linux: cap on the ZFS ARC of a pool hotbed makes, in bytes; 0 for zfs's default
	UnitProperties []string
	Slice          string
	NoStart        bool
	// Hostname is the node name Setup reports for a worker already joined
	// whose state names none (Linux).
	Hostname string

	// Progress receives human-readable progress lines (typically stderr).
	Progress io.Writer
}

// Result is returned by Setup as the CLI envelope data.
type Result struct {
	Platform   string `json:"platform"` // "linux" or "darwin"
	Dir        string `json:"dir"`
	StateDir   string `json:"stateDir"`
	Controller string `json:"controller"`
	User       string `json:"user"`
	Node       string `json:"node,omitempty"`
	ClusterID  string `json:"clusterId,omitempty"`
	Storage    string `json:"storage"` // "btrfs", "zfs", "zfs-file", "zfs-device", "zfs-raw"
	Running    bool   `json:"running"`
	Service    string `json:"service,omitempty"`
	// Mac is how a Mac's worker and machined stand (hotbed's daemon runs
	// them).
	Mac   *macworker.State `json:"mac,omitempty"`
	Disks []DiskStat       `json:"disks,omitempty"`
	Next  []string         `json:"next,omitempty"`
}

// Status describes a locally configured worker.
type Status struct {
	Platform   string `json:"platform"`
	Dir        string `json:"dir"`
	StateDir   string `json:"stateDir"`
	Controller string `json:"controller,omitempty"`
	User       string `json:"user,omitempty"`
	Storage    string `json:"storage,omitempty"`
	Running    bool   `json:"running"`
	Service    string `json:"service,omitempty"`
	// Mac is how a Mac's worker and machined stand (hotbed's daemon runs
	// them).
	Mac   *macworker.State `json:"mac,omitempty"`
	Disks []DiskStat       `json:"disks,omitempty"`
}

// LocalWorkerSocket is a worker on this machine that serves its clients
// here on a Unix socket: those of its node's workspaces and commands call it
// there, the rest through the controller.
type LocalWorkerSocket struct {
	NodeID string
	Socket string
}

// DiskStat reports virtual vs actual allocated on-disk bytes for a sparse image.
type DiskStat struct {
	Name           string `json:"name"`
	Path           string `json:"path"`
	VirtualBytes   int64  `json:"virtualBytes,omitempty"`
	LogicalBytes   int64  `json:"logicalBytes"`
	AllocatedBytes int64  `json:"allocatedBytes"`
}

// TrimResult reports sparse space reclaimed by a trim operation.
type TrimResult struct {
	Dir            string     `json:"dir"`
	BeforeBytes    int64      `json:"beforeBytes"`
	AfterBytes     int64      `json:"afterBytes"`
	ReclaimedBytes int64      `json:"reclaimedBytes"`
	Disks          []DiskStat `json:"disks"`
}

// DestroyOptions control worker teardown.
type DestroyOptions struct {
	Dir      string
	Progress io.Writer
}

// DestroyResult summarizes what Destroy removed.
type DestroyResult struct {
	Dir     string   `json:"dir"`
	Removed []string `json:"removed"`
}

// DefaultDir returns ~/.hotbed for the invoking user (respecting SUDO_USER on
// Linux so `sudo hotbed worker init` targets the user's home, not /root).
func DefaultDir() (string, error) {
	home, _, err := invokingUserHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".hotbed"), nil
}

// DefaultUser returns the invoking non-root username when available.
func DefaultUser() (string, error) {
	_, name, err := invokingUserHome()
	return name, err
}

func invokingUserHome() (home, username string, err error) {
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" && sudoUser != "root" {
		if u, err := user.Lookup(sudoUser); err == nil && u.HomeDir != "" {
			return u.HomeDir, u.Username, nil
		}
	}
	u, err := user.Current()
	if err != nil {
		h, herr := os.UserHomeDir()
		if herr != nil {
			return "", "", fmt.Errorf("determine home directory: %w", err)
		}
		return h, os.Getenv("USER"), nil
	}
	return u.HomeDir, u.Username, nil
}

// ResolveDir expands an empty path to DefaultDir and a leading ~/ to the
// invoking user's home directory, returning an absolute path.
func ResolveDir(dir string) (string, error) {
	if dir == "" {
		return DefaultDir()
	}
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, _, err := invokingUserHome()
		if err != nil {
			return "", err
		}
		if dir == "~" {
			dir = home
		} else {
			dir = filepath.Join(home, dir[2:])
		}
	}
	return filepath.Abs(dir)
}

// JoinedController is the controller `hotbed worker init` joined this
// machine's worker to, as it recorded it in dir (default ~/.hotbed): what
// client commands call. It is "" when no worker was set up there.
func JoinedController(dir string) (string, error) {
	baseDir, err := ResolveDir(dir)
	if err != nil {
		return "", err
	}
	return joinedController(baseDir)
}

// controllerRecord is the controller a worker init's record at path names:
// "" if there is none.
func controllerRecord(path string) (string, error) {
	var rec struct {
		Controller string `json:"controller"`
	}
	if err := readJSON(path, &rec); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return rec.Controller, nil
}

// ParseBytes parses human-readable byte sizes such as "256G", "8GiB", "4096M",
// or "1T". Bare numbers are bytes.
func ParseBytes(s string) (int64, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return 0, errors.New("empty size")
	}
	u := strings.ToUpper(raw)
	type suffix struct {
		s string
		m int64
	}
	suffixes := []suffix{
		{"TIB", 1 << 40}, {"TB", 1 << 40}, {"T", 1 << 40},
		{"GIB", 1 << 30}, {"GB", 1 << 30}, {"G", 1 << 30},
		{"MIB", 1 << 20}, {"MB", 1 << 20}, {"M", 1 << 20},
		{"KIB", 1 << 10}, {"KB", 1 << 10}, {"K", 1 << 10},
		{"B", 1},
	}
	mult := int64(1)
	numStr := u
	for _, sf := range suffixes {
		if strings.HasSuffix(u, sf.s) {
			mult = sf.m
			numStr = strings.TrimSpace(strings.TrimSuffix(u, sf.s))
			break
		}
	}
	n, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid size %q (use e.g. 256G, 16G, 512M)", s)
	}
	if n > (1<<62)/mult {
		return 0, fmt.Errorf("size %q overflows int64", s)
	}
	return n * mult, nil
}

// FileDiskStat reads logical and actual allocated on-disk bytes for path.
// On APFS and ext4/xfs/btrfs/zfs, stat.Blocks*512 reflects actual sparse
// allocation after hole punching.
func FileDiskStat(name, path string, virtualBytes int64) (DiskStat, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return DiskStat{}, err
	}
	st := DiskStat{
		Name:           name,
		Path:           path,
		VirtualBytes:   virtualBytes,
		LogicalBytes:   fi.Size(),
		AllocatedBytes: fi.Size(),
	}
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		st.AllocatedBytes = sys.Blocks * 512
	}
	return st, nil
}

func progressf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, "hotbed: "+format+"\n", args...)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func copyFileAtomic(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("source %s is not a regular file", src)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if os.Geteuid() == 0 {
		if err := tmp.Chown(0, 0); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func validateZFSDevicePath(dev string) error {
	if dev == "" {
		return errors.New("empty ZFS device path")
	}
	if strings.ContainsAny(dev, "\x00\n\r\t ") {
		return fmt.Errorf("invalid ZFS device path %q: must not contain whitespace or control characters", dev)
	}
	if !filepath.IsAbs(dev) {
		return fmt.Errorf("invalid ZFS device path %q: must be an absolute path under /dev/", dev)
	}
	for _, seg := range strings.Split(dev, "/") {
		if seg == ".." {
			return fmt.Errorf("invalid ZFS device path %q: must not contain '..'", dev)
		}
	}
	clean := filepath.Clean(dev)
	if !strings.HasPrefix(clean, "/dev/") || clean == "/dev" {
		return fmt.Errorf("invalid ZFS device path %q: must be under /dev/", dev)
	}
	if clean != dev {
		return fmt.Errorf("invalid ZFS device path %q: must be a clean path", dev)
	}
	return nil
}

// FormatBytes formats a byte count concisely for human progress messages.
func FormatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
