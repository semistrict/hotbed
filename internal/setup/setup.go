// Package setup provisions a hotbed worker node on the current machine in a
// single command: directly on Linux (using native btrfs/zfs or a sparse
// file-backed ZFS pool in ~/.hotbed) and via a QEMU Ubuntu VM with sparse
// discard-enabled qcow2 + ZFS disks in ~/.hotbed on macOS.
package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	// DefaultUbuntuVersion is the Ubuntu LTS release used for macOS QEMU VMs.
	DefaultUbuntuVersion = "26.04"
	// DefaultDiskSize is the default virtual size of the sparse ZFS disk image.
	DefaultDiskSize = 256 << 30 // 256 GiB
	// DefaultRootSize is the default virtual size of the VM OS root disk.
	DefaultRootSize = 32 << 30 // 32 GiB
	// DefaultPoolName is the ZFS pool name created for file/qcow2-backed workers.
	DefaultPoolName = "hotbed"
	// DefaultVMStateDir is the worker state directory inside a macOS QEMU VM.
	DefaultVMStateDir = "/var/lib/hotbed"
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
	TrimFS         bool   // Linux: also run fstrim -a in the periodic trim timer (for VM guests)
	ZFSARCMax      int64  // Linux: cap on the ZFS ARC of a pool hotbed makes, in bytes; 0 for zfs's default
	UnitProperties []string
	Slice          string
	NoStart        bool

	// macOS QEMU VM options (ignored on Linux).
	Hostname         string
	CPUs             int
	MemoryBytes      int64
	RootSize         int64
	SwapSize         int64
	UbuntuVersion    string
	ImageURL         string
	BinaryPath       string // path to Linux hotbed binary
	TailscaleAuthKey string
	SSHPort          int
	NoAutostart      bool
	SleepAfter       string // macOS VM: how long idle before it powers off; "" default, "0" never

	// Progress receives human-readable progress lines (typically stderr).
	Progress io.Writer
}

// Result is returned by Setup as the CLI envelope data.
type Result struct {
	Platform   string     `json:"platform"` // "linux" or "darwin-qemu"
	Dir        string     `json:"dir"`
	StateDir   string     `json:"stateDir"`
	Controller string     `json:"controller"`
	User       string     `json:"user"`
	Node       string     `json:"node,omitempty"`
	ClusterID  string     `json:"clusterId,omitempty"`
	Storage    string     `json:"storage"` // "btrfs", "zfs", "zfs-file", "zfs-device", "zfs-qcow2"
	Running    bool       `json:"running"`
	Service    string     `json:"service,omitempty"`
	VM         *VMStatus  `json:"vm,omitempty"`
	Disks      []DiskStat `json:"disks,omitempty"`
	Next       []string   `json:"next,omitempty"`
}

// Status describes a locally configured worker.
type Status struct {
	Platform   string     `json:"platform"`
	Dir        string     `json:"dir"`
	StateDir   string     `json:"stateDir"`
	Controller string     `json:"controller,omitempty"`
	User       string     `json:"user,omitempty"`
	Storage    string     `json:"storage,omitempty"`
	Running    bool       `json:"running"`
	Service    string     `json:"service,omitempty"`
	VM         *VMStatus  `json:"vm,omitempty"`
	Disks      []DiskStat `json:"disks,omitempty"`
}

// VMStatus describes the macOS QEMU VM backing a worker.
type VMStatus struct {
	Hostname   string   `json:"hostname"`
	Arch       string   `json:"arch"`
	Ubuntu     string   `json:"ubuntu"`
	CPUs       int      `json:"cpus"`
	MemoryMB   int64    `json:"memoryMB"`
	SSHPort    int      `json:"sshPort"`
	PID        int      `json:"pid,omitempty"`
	Running    bool     `json:"running"`
	Paused     bool     `json:"paused,omitempty"`
	QMPStatus  string   `json:"qmpStatus,omitempty"`
	IOErrors   []string `json:"ioErrors,omitempty"`
	Tailscale  string   `json:"tailscale,omitempty"`
	Autostart  bool     `json:"autostart"`
	Supervised bool     `json:"supervised"` // its supervisor (LaunchAgent) runs
	ConsoleLog string   `json:"consoleLog,omitempty"`
}

var watchPausedVM func(ctx context.Context, dir string, log *slog.Logger)

// WatchPausedVM periodically checks a local worker VM for pause states such as
// QEMU io-error (host ENOSPC) and resumes it once host disk space recovers.
// It is a no-op on platforms without a local VM.
func WatchPausedVM(ctx context.Context, dir string, log *slog.Logger) {
	if watchPausedVM != nil {
		watchPausedVM(ctx, dir, log)
	}
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
	Dir        string
	PurgeCache bool
	Progress   io.Writer
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
	path := workerRecord(baseDir)
	if path == "" {
		return "", nil
	}
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

// SanitizeHostname returns a DNS/Tailscale-safe lowercase hostname label.
func SanitizeHostname(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if host, _, ok := strings.Cut(s, "."); ok {
		s = host
	}
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		case !lastDash && b.Len() > 0:
			b.WriteByte('-')
			lastDash = true
		}
		if b.Len() >= 55 {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "hotbed-worker"
	}
	return out
}

// DefaultVMHostname derives a clean Tailscale hostname for a local VM worker.
func DefaultVMHostname() string {
	h, err := os.Hostname()
	if err != nil || strings.TrimSpace(h) == "" {
		return "hotbed-worker"
	}
	base := SanitizeHostname(h)
	if strings.HasSuffix(base, "-worker") {
		return base
	}
	return base + "-worker"
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

func writeJSONAtomic(path string, v any, perm fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
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
	return os.Rename(tmp.Name(), path)
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
