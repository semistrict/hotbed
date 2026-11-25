//go:build linux

package setup

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/worker"
)

const (
	zfsSuperMagic       = 0x2fc12fc1
	linuxWorkerService  = "hotbed-worker.service"
	linuxTrimService    = "hotbed-worker-trim.service"
	linuxTrimTimer      = "hotbed-worker-trim.timer"
	legacyTrimService   = "hotbed-trim.service"
	legacyTrimTimer     = "hotbed-trim.timer"
	systemdUnitDir      = "/etc/systemd/system"
	installedServiceBin = "/usr/local/bin/hotbed"
)

var (
	// linuxMetaDir stores root-owned worker setup metadata outside the
	// user-owned baseDir so unprivileged users cannot tamper with fields read
	// by root commands (start, stop, trim, destroy). Overridable in tests.
	linuxMetaDir = "/etc/hotbed"

	// ZFS module configuration paths. Overridable in tests.
	zfsModprobeConfPath = "/etc/modprobe.d/hotbed-zfs.conf"
	zfsParamDir         = "/sys/module/zfs/parameters"
	zfsARCStatsPath     = "/proc/spl/kstat/zfs/arcstats"

	// Device inspection and ownership hooks. Overridable in tests so Linux
	// storage unit tests can run cleanly without root or real /dev block nodes.
	evalZFSDeviceSymlinks = filepath.EvalSymlinks
	statZFSDevice         = os.Stat
	chownPath             = func(path string, uid, gid int) error {
		if os.Geteuid() == 0 {
			return os.Chown(path, uid, gid)
		}
		return nil
	}

	linuxServiceNameRE = regexp.MustCompile(`^hotbed-[a-zA-Z0-9][a-zA-Z0-9._-]*(\.service)?$`)
	linuxTrimTimerRE   = regexp.MustCompile(`^hotbed-[a-zA-Z0-9][a-zA-Z0-9._-]*\.timer$`)
	linuxPoolNameRE    = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.-]{0,63}$`)
)

// Setup configures and starts a hotbed worker directly on this Linux host,
// storing its state under o.Dir (default ~/.hotbed).
func Setup(ctx context.Context, o Options, tn worker.Tailnet, hc *http.Client, check worker.Check) (Result, error) {
	if os.Geteuid() != 0 {
		return Result{}, api.Errorf(api.CodeInvalidArgs, "setting up a Linux worker requires root privileges (for storage, join, and systemd)").
			WithSuggestion("run with sudo: sudo hotbed worker init " + o.Controller)
	}
	controller, err := api.ControllerHostPort(o.Controller)
	if err != nil {
		return Result{}, err
	}
	baseDir, err := ResolveDir(o.Dir)
	if err != nil {
		return Result{}, err
	}
	username := o.User
	if username == "" {
		username, err = DefaultUser()
		if err != nil {
			return Result{}, err
		}
	}
	if username == "" || username == "root" {
		return Result{}, api.Errorf(api.CodeInvalidArgs, "specify a non-root user for commands with --user").
			WithSuggestion("pass --user <your-username> or run via sudo from your user account")
	}
	u, err := user.Lookup(username)
	if err != nil {
		return Result{}, api.Errorf(api.CodeInvalidArgs, "no user %q to run commands as", username)
	}
	if o.ZFSDevice != "" {
		if _, err := resolveZFSBlockDevice(o.ZFSDevice); err != nil {
			return Result{}, api.Errorf(api.CodeInvalidArgs, "--zfs-device: %v", err)
		}
		if _, err := exec.LookPath("zpool"); err != nil {
			return Result{}, api.Errorf(api.CodeInvalidArgs, "zpool is not installed").
				WithSuggestion("install ZFS (e.g. `sudo apt install zfsutils-linux`)")
		}
	}
	self, err := tn.Self(ctx)
	if err != nil {
		return Result{}, err
	}

	stateDir := resolveLinuxStateDir(baseDir, o.Dir, o.ZFSDevice)
	progressf(o.Progress, "setting up Linux worker in %s (command user %s)", baseDir, username)
	if err := prepareLinuxDirs(baseDir, stateDir, u, chownPath); err != nil {
		return Result{}, err
	}
	diskSize := o.DiskSize
	if diskSize <= 0 {
		diskSize = DefaultDiskSize
	}

	var storageMode, poolName, imgPath string
	if o.ZFSDevice != "" {
		storageMode, poolName, err = ensureLinuxZFSDeviceStorage(ctx, baseDir, stateDir, o.ZFSDevice, o.Progress, 0, runCmd)
	} else {
		storageMode, poolName, imgPath, err = ensureLinuxStorage(ctx, baseDir, stateDir, diskSize, o.Progress)
	}
	if err != nil {
		return Result{}, err
	}
	if err := enforceRootStateDir(stateDir, chownPath); err != nil {
		return Result{}, err
	}
	if storageMode == "zfs-file" || storageMode == "zfs-device" {
		if err := configureZFSModule(o.ZFSARCMax); err != nil {
			return Result{}, err
		}
	} else if o.ZFSARCMax > 0 {
		return Result{}, api.Errorf(api.CodeInvalidArgs, "--zfs-arc-max applies only to a ZFS pool hotbed makes (a sparse image or --zfs-device); this worker uses %s storage", storageMode).
			WithSuggestion("drop --zfs-arc-max and tune the ARC of your own pool yourself")
	}
	if err := worker.EnsureUserfaultfdAccess(ctx, username); err != nil {
		return Result{}, fmt.Errorf("configure /dev/userfaultfd access for %s: %w", username, err)
	}

	// Join cluster (idempotent when already joined to the same controller).
	var joinRes worker.JoinResult
	if existing, err := statedir.Load(stateDir); err == nil {
		if existing.Worker == nil {
			return Result{}, api.Errorf(api.CodeAlreadyInitialized, "%s is already initialized as a controller", stateDir)
		}
		if existing.Worker.Controller != controller {
			return Result{}, api.Errorf(api.CodeAlreadyInitialized, "%s is already joined to controller %s (want %s)", stateDir, existing.Worker.Controller, controller).
				WithSuggestion("destroy the existing worker with `sudo hotbed worker destroy` or pass a different --dir")
		}
		progressf(o.Progress, "state directory %s is already joined to %s", stateDir, controller)
		joinRes = worker.JoinResult{
			ClusterID:  existing.Worker.ClusterID,
			Dir:        stateDir,
			Controller: existing.Worker.Controller,
			Storage:    storageMode,
			User:       existing.Worker.User,
			Node:       cmp.Or(self.Hostname, o.Hostname),
		}
	} else if !errors.Is(err, statedir.ErrNotInitialized) {
		return Result{}, err
	} else {
		progressf(o.Progress, "joining controller %s", controller)
		joinRes, err = worker.Join(ctx, worker.JoinOptions{
			Dir:            stateDir,
			Controller:     controller,
			User:           username,
			Token:          o.Token,
			Dataset:        o.ZFSDataset,
			UnitProperties: o.UnitProperties,
			Slice:          o.Slice,
		}, tn, hc, check)
		if err != nil {
			return Result{}, err
		}
	}
	if err := enforceRootStateDir(stateDir, chownPath); err != nil {
		return Result{}, err
	}

	binPath, err := resolveLinuxServiceBinary(o.Progress)
	if err != nil {
		return Result{}, err
	}

	var zfsImportPre string
	if storageMode == "zfs-file" {
		zpoolBin, err := exec.LookPath("zpool")
		if err != nil {
			zpoolBin = "/sbin/zpool"
		}
		zfsImportPre = fmt.Sprintf("%s import -d %s %s", systemdQuote(zpoolBin), systemdQuote(baseDir), poolName)
	}

	servicePath := filepath.Join(systemdUnitDir, linuxWorkerService)
	unitText := SystemdWorkerUnit(binPath, stateDir, zfsImportPre)
	if err := os.WriteFile(servicePath, []byte(unitText), 0o644); err != nil {
		return Result{}, fmt.Errorf("write %s: %w", servicePath, err)
	}

	meta := LinuxSetupMeta{
		StateDir:    stateDir,
		User:        username,
		Controller:  controller,
		StorageMode: storageMode,
		PoolName:    poolName,
		ImagePath:   imgPath,
		ZFSDevice:   o.ZFSDevice,
		DiskSize:    diskSize,
		ServiceName: linuxWorkerService,
	}
	if storageMode == "zfs-device" {
		meta.DiskSize = 0
	}

	if storageMode == "zfs-file" || storageMode == "zfs-device" {
		cleanupLegacyTrimUnits(ctx, systemdUnitDir, runCmd)
		trimSvc, trimTmr := SystemdTrimUnits(poolName, o.TrimFS)
		if err := os.WriteFile(filepath.Join(systemdUnitDir, linuxTrimService), []byte(trimSvc), 0o644); err != nil {
			return Result{}, fmt.Errorf("write %s: %w", linuxTrimService, err)
		}
		if err := os.WriteFile(filepath.Join(systemdUnitDir, linuxTrimTimer), []byte(trimTmr), 0o644); err != nil {
			return Result{}, fmt.Errorf("write %s: %w", linuxTrimTimer, err)
		}
		meta.TrimTimer = linuxTrimTimer
	}

	if err := validateLinuxMeta(meta, baseDir); err != nil {
		return Result{}, fmt.Errorf("invalid setup metadata: %w", err)
	}
	if err := writeTrustedJSONAtomic(linuxMetaPath(baseDir), meta, 0o644); err != nil {
		return Result{}, fmt.Errorf("save setup metadata: %w", err)
	}
	removeLegacyMetaIfRegular(baseDir)

	running, err := applyLinuxSystemdState(ctx, o.NoStart, linuxWorkerService, meta.TrimTimer, o.Progress, runCmd)
	if err != nil {
		return Result{}, err
	}

	var disks []DiskStat
	if imgPath != "" {
		if ds, err := FileDiskStat("zfs", imgPath, diskSize); err == nil {
			disks = append(disks, ds)
		}
	}

	return Result{
		Platform:   "linux",
		Dir:        baseDir,
		StateDir:   stateDir,
		Controller: controller,
		User:       username,
		Node:       joinRes.Node,
		ClusterID:  joinRes.ClusterID,
		Storage:    storageMode,
		Running:    running,
		Service:    linuxWorkerService,
		Disks:      disks,
		Next: []string{
			"hotbed worker status",
			"hotbed nodes",
		},
	}, nil
}

func resolveLinuxStateDir(baseDir, rawDir, zfsDevice string) string {
	if zfsDevice != "" && rawDir != "" {
		if defDir, err := DefaultDir(); err != nil || filepath.Clean(baseDir) != filepath.Clean(defDir) {
			return filepath.Clean(baseDir)
		}
	}
	return filepath.Join(baseDir, "worker")
}

// prepareLinuxDirs ensures the directory hierarchy above stateDir is
// traversable without ever chowning stateDir itself to the unprivileged
// command user. When baseDir == stateDir (e.g. `--zfs-device --dir /var/lib/hotbed`),
// only parent directories strictly above stateDir are created and stateDir stays
// root-owned (0700).
func prepareLinuxDirs(baseDir, stateDir string, u *user.User, chown func(string, int, int) error) error {
	cleanBase := filepath.Clean(baseDir)
	cleanState := filepath.Clean(stateDir)
	if cleanBase == cleanState {
		if parent := filepath.Dir(cleanState); parent != cleanState {
			if err := os.MkdirAll(parent, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", parent, err)
			}
		}
		return nil
	}
	if err := os.MkdirAll(cleanBase, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", cleanBase, err)
	}
	if err := os.Chmod(cleanBase, 0o755); err != nil {
		return fmt.Errorf("chmod %s: %w", cleanBase, err)
	}
	if u != nil && chown != nil {
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		_ = chown(cleanBase, uid, gid)
	}
	return nil
}

// enforceRootStateDir guarantees stateDir exists with root:root ownership and
// 0700 permissions (matching statedir.Prepare) so worker state and hotbed.json
// are never exposed to the unprivileged command user.
func enforceRootStateDir(stateDir string, chown func(string, int, int) error) error {
	clean := filepath.Clean(stateDir)
	if parent := filepath.Dir(clean); parent != clean {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return fmt.Errorf("create parent of %s: %w", clean, err)
		}
	}
	if err := os.Mkdir(clean, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create %s: %w", clean, err)
	}
	if chown != nil {
		if err := chown(clean, 0, 0); err != nil {
			return fmt.Errorf("chown %s to root:root: %w", clean, err)
		}
	}
	if err := os.Chmod(clean, 0o700); err != nil {
		return fmt.Errorf("chmod %s to 0700: %w", clean, err)
	}
	return nil
}

func applyLinuxSystemdState(
	ctx context.Context,
	noStart bool,
	serviceName, trimTimer string,
	progress io.Writer,
	run func(context.Context, string, ...string) (string, error),
) (bool, error) {
	if _, err := run(ctx, "systemctl", "daemon-reload"); err != nil {
		return false, err
	}
	if noStart {
		return false, nil
	}
	progressf(progress, "enabling and starting %s", serviceName)
	if _, err := run(ctx, "systemctl", "enable", serviceName); err != nil {
		return false, err
	}
	if _, err := run(ctx, "systemctl", "restart", serviceName); err != nil {
		return false, err
	}
	if trimTimer != "" {
		if _, err := run(ctx, "systemctl", "enable", "--now", trimTimer); err != nil {
			return false, err
		}
	}
	return isSystemdActiveWith(ctx, run, serviceName), nil
}

// ensureLinuxStorage checks whether baseDir is already on btrfs or zfs; if
// not, it creates a sparse file-backed ZFS pool in <baseDir>/zfs.img mounted
// at stateDir with autotrim=on and compression=zstd so blocks are punched out
// when workspaces or bases are deleted.
func ensureLinuxStorage(ctx context.Context, baseDir, stateDir string, diskSize int64, progress io.Writer) (mode, poolName, imgPath string, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(baseDir, &st); err != nil {
		return "", "", "", fmt.Errorf("statfs %s: %w", baseDir, err)
	}
	switch st.Type {
	case unix.BTRFS_SUPER_MAGIC:
		progressf(progress, "%s is on btrfs; using native copy-on-write storage", baseDir)
		return "btrfs", "", "", nil
	case zfsSuperMagic:
		progressf(progress, "%s is on zfs; using native ZFS storage", baseDir)
		return "zfs", "", "", nil
	}

	if _, err := exec.LookPath("zpool"); err != nil {
		return "", "", "", api.Errorf(api.CodeInvalidArgs, "%s is not on btrfs or zfs, and zpool is not installed", baseDir).
			WithSuggestion("install ZFS (e.g. `sudo apt install zfsutils-linux`) or place --dir on a btrfs/zfs filesystem")
	}

	return ensureLinuxZFSFileStorage(ctx, baseDir, stateDir, diskSize, progress, 0, runCmd)
}

func ensureLinuxZFSFileStorage(
	ctx context.Context,
	baseDir, stateDir string,
	diskSize int64,
	progress io.Writer,
	expectedMetaUID uint32,
	run func(context.Context, string, ...string) (string, error),
) (mode, poolName, imgPath string, err error) {
	imgPath = filepath.Join(baseDir, "zfs.img")
	poolName, err = resolveLinuxPoolName(ctx, baseDir, stateDir, imgPath, expectedMetaUID, run)
	if err != nil {
		return "", "", "", err
	}

	if zpoolExistsWith(ctx, run, poolName) {
		mp, err := run(ctx, "zfs", "get", "-H", "-o", "value", "mountpoint", poolName)
		if err != nil {
			return "", "", "", err
		}
		if got := strings.TrimSpace(mp); got != stateDir {
			return "", "", "", api.Errorf(api.CodeConflict, "ZFS pool %q already exists and is mounted at %s, not %s", poolName, got, stateDir).
				WithSuggestion("pass a different --dir or destroy the existing pool before retrying")
		}
		_, _ = run(ctx, "zpool", "set", "autotrim=on", poolName)
		progressf(progress, "using existing sparse ZFS pool %s mounted at %s", poolName, stateDir)
		return "zfs-file", poolName, imgPath, nil
	}

	if _, err := os.Stat(imgPath); err == nil {
		progressf(progress, "importing existing sparse ZFS pool %s from %s", poolName, imgPath)
		if _, err := run(ctx, "zpool", "import", "-d", baseDir, poolName); err != nil {
			return "", "", "", fmt.Errorf("import existing ZFS pool from %s: %w", imgPath, err)
		}
		_, _ = run(ctx, "zpool", "set", "autotrim=on", poolName)
		return "zfs-file", poolName, imgPath, nil
	}

	progressf(progress, "creating %s sparse ZFS image at %s (autotrim=on, compression=zstd)", FormatBytes(diskSize), imgPath)
	f, err := os.OpenFile(imgPath, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o600)
	if err != nil {
		return "", "", "", fmt.Errorf("create sparse ZFS image %s: %w", imgPath, err)
	}
	if err := f.Truncate(diskSize); err != nil {
		f.Close()
		os.Remove(imgPath)
		return "", "", "", fmt.Errorf("size sparse ZFS image %s: %w", imgPath, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(imgPath)
		return "", "", "", err
	}

	if err := enforceRootStateDir(stateDir, chownPath); err != nil {
		return "", "", "", err
	}
	if _, err := run(ctx, "zpool", "create", "-f",
		"-o", "ashift=12",
		"-o", "autotrim=on",
		"-O", "compression=zstd",
		"-O", "atime=off",
		"-O", "xattr=sa",
		"-O", "dnodesize=auto",
		"-m", stateDir,
		poolName, imgPath,
	); err != nil {
		os.Remove(imgPath)
		return "", "", "", fmt.Errorf("create ZFS pool %s: %w", poolName, err)
	}
	if err := enforceRootStateDir(stateDir, chownPath); err != nil {
		return "", "", "", err
	}
	return "zfs-file", poolName, imgPath, nil
}

// configureZFSModule tunes the zfs module for a pool hotbed made, now and
// from the next boot on (modprobe.d):
//   - zfs_trim_txg_batch=1, so autotrim punches freed extents out of the
//     sparse image promptly instead of batching 32 TXGs;
//   - zfs_arc_max=arcMax, which caps the ARC; 0 is the module's default
//     (nearly all of RAM), which hotbed then sets back live too.
//
// zfs takes a zfs_arc_max it cannot use (not above zfs_arc_min, say) without
// an error and keeps the old cap, so the ARC's c_max is read back to check.
func configureZFSModule(arcMax int64) error {
	params := [][2]string{{"zfs_trim_txg_batch", "1"}, {"zfs_arc_max", strconv.FormatInt(arcMax, 10)}}
	conf := "options zfs zfs_trim_txg_batch=1"
	if arcMax > 0 {
		conf += " zfs_arc_max=" + strconv.FormatInt(arcMax, 10)
	}
	if err := os.WriteFile(zfsModprobeConfPath, []byte(conf+"\n"), 0o644); err != nil {
		return fmt.Errorf("configure zfs module: %w", err)
	}
	for _, p := range params {
		if err := os.WriteFile(filepath.Join(zfsParamDir, p[0]), []byte(p[1]+"\n"), 0o644); err != nil {
			return fmt.Errorf("set zfs module parameter %s=%s: %w", p[0], p[1], err)
		}
	}
	if arcMax == 0 {
		return nil
	}
	cMax, err := zfsARCStat("c_max")
	if err != nil {
		return err
	}
	if cMax != arcMax {
		return api.Errorf(api.CodeInvalidArgs, "zfs kept its ARC cap at %s and ignored zfs_arc_max=%s", FormatBytes(cMax), FormatBytes(arcMax)).
			WithSuggestion("pass a --zfs-arc-max above the ARC's minimum (c_min in " + zfsARCStatsPath + ") and below RAM")
	}
	return nil
}

// zfsARCStat is one counter of the ARC's kstats.
func zfsARCStat(name string) (int64, error) {
	b, err := os.ReadFile(zfsARCStatsPath)
	if err != nil {
		return 0, fmt.Errorf("read zfs ARC stats: %w", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && f[0] == name {
			v, err := strconv.ParseInt(f[2], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("zfs ARC stat %s: %w", name, err)
			}
			return v, nil
		}
	}
	return 0, fmt.Errorf("zfs ARC stats in %s have no %s", zfsARCStatsPath, name)
}

// resolveZFSBlockDevice verifies that dev is a valid path under /dev/, resolves
// symlinks to a canonical path still under /dev/, and verifies the target is a
// real block device (not a regular file, directory, or character device).
func resolveZFSBlockDevice(dev string) (string, error) {
	if err := validateZFSDevicePath(dev); err != nil {
		return "", err
	}
	resolved, err := evalZFSDeviceSymlinks(dev)
	if err != nil {
		return "", fmt.Errorf("resolve ZFS device %s: %w", dev, err)
	}
	resolved = filepath.Clean(resolved)
	if err := validateZFSDevicePath(resolved); err != nil {
		return "", fmt.Errorf("ZFS device %s resolves to %s: %w", dev, resolved, err)
	}
	fi, err := statZFSDevice(resolved)
	if err != nil {
		return "", fmt.Errorf("stat ZFS device %s: %w", resolved, err)
	}
	mode := fi.Mode()
	if mode&os.ModeDevice == 0 || mode&os.ModeCharDevice != 0 {
		return "", fmt.Errorf("ZFS device %s (resolved to %s) is not a block device (mode %s)", dev, resolved, mode)
	}
	return resolved, nil
}

func poolBackingVdevs(ctx context.Context, run func(context.Context, string, ...string) (string, error), poolName string) ([]string, error) {
	out, err := run(ctx, "zpool", "list", "-v", "-H", "-P", poolName)
	if err != nil {
		return nil, fmt.Errorf("inspect ZFS pool %s vdevs: %w", poolName, err)
	}
	var vdevs []string
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "\t") {
			continue
		}
		fields := strings.Split(line, "\t")
		for _, f := range fields {
			f = strings.TrimSpace(f)
			if f == "" {
				continue
			}
			if strings.HasPrefix(f, "/") {
				vdevs = append(vdevs, filepath.Clean(f))
			}
			break
		}
	}
	if len(vdevs) == 0 {
		return nil, fmt.Errorf("inspect ZFS pool %s vdevs: no backing device paths found", poolName)
	}
	return vdevs, nil
}

func vdevMatchesDevice(vdev, zfsDevice, canonicalDev string) bool {
	cleanVdev := filepath.Clean(vdev)
	cleanDev := filepath.Clean(zfsDevice)
	cleanCanon := filepath.Clean(canonicalDev)
	if cleanVdev == cleanDev || cleanVdev == cleanCanon || isPartitionOf(cleanVdev, cleanDev) || isPartitionOf(cleanVdev, cleanCanon) {
		return true
	}
	if resolved, err := evalZFSDeviceSymlinks(cleanVdev); err == nil {
		resolved = filepath.Clean(resolved)
		if resolved == cleanCanon || isPartitionOf(resolved, cleanCanon) {
			return true
		}
	}
	return false
}

func isPartitionOf(cand, base string) bool {
	if !strings.HasPrefix(cand, base) || len(cand) <= len(base) {
		return false
	}
	rest := cand[len(base):]
	if strings.HasPrefix(rest, "-part") {
		return isAllDigits(rest[len("-part"):])
	}
	last := base[len(base)-1]
	if last >= '0' && last <= '9' {
		return strings.HasPrefix(rest, "p") && isAllDigits(rest[1:])
	}
	if (last >= 'a' && last <= 'z') || (last >= 'A' && last <= 'Z') {
		return isAllDigits(rest)
	}
	return false
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func verifyPoolBackingDevice(
	ctx context.Context,
	run func(context.Context, string, ...string) (string, error),
	poolName, zfsDevice, canonicalDev string,
) error {
	vdevs, err := poolBackingVdevs(ctx, run, poolName)
	if err != nil {
		return err
	}
	for _, vdev := range vdevs {
		if !vdevMatchesDevice(vdev, zfsDevice, canonicalDev) {
			return api.Errorf(api.CodeConflict, "ZFS pool %q is backed by %s, not %s (%s)", poolName, strings.Join(vdevs, ", "), zfsDevice, canonicalDev).
				WithSuggestion("destroy or export the conflicting pool, or pass the matching --zfs-device / --dir")
		}
	}
	return nil
}

func isBlkidNoMatch(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() == 2
}

// probeBlankBlockDevice checks blkid TYPE and PTTYPE on zfsDevice before
// zpool create so an existing filesystem, foreign ZFS pool label, or partition
// table is never silently wiped.
func probeBlankBlockDevice(ctx context.Context, run func(context.Context, string, ...string) (string, error), zfsDevice string) error {
	out, err := run(ctx, "blkid", "-o", "value", "-s", "TYPE", zfsDevice)
	if err != nil && !isBlkidNoMatch(err) {
		return fmt.Errorf("probe %s with blkid: %w", zfsDevice, err)
	}
	if fsType := strings.TrimSpace(out); fsType != "" {
		return api.Errorf(api.CodeConflict, "refusing to create ZFS pool on %s: device already contains %q", zfsDevice, fsType).
			WithSuggestion("wipe the device manually or pass a blank --zfs-device")
	}
	ptOut, ptErr := run(ctx, "blkid", "-o", "value", "-s", "PTTYPE", zfsDevice)
	if ptErr != nil && !isBlkidNoMatch(ptErr) {
		return fmt.Errorf("probe partition table on %s with blkid: %w", zfsDevice, ptErr)
	}
	if ptType := strings.TrimSpace(ptOut); ptType != "" {
		return api.Errorf(api.CodeConflict, "refusing to create ZFS pool on %s: device already has a %q partition table", zfsDevice, ptType).
			WithSuggestion("wipe the device manually or import the existing pool if it belongs to hotbed")
	}
	return nil
}

func ensureLinuxZFSDeviceStorage(
	ctx context.Context,
	baseDir, stateDir, zfsDevice string,
	progress io.Writer,
	expectedMetaUID uint32,
	run func(context.Context, string, ...string) (string, error),
) (mode, poolName string, err error) {
	canonicalDev, err := resolveZFSBlockDevice(zfsDevice)
	if err != nil {
		return "", "", api.Errorf(api.CodeInvalidArgs, "--zfs-device: %v", err)
	}
	if _, err := run(ctx, "modprobe", "zfs"); err != nil {
		return "", "", fmt.Errorf("load zfs kernel module: %w", err)
	}

	poolName, err = resolveLinuxPoolName(ctx, baseDir, stateDir, "", expectedMetaUID, run)
	if err != nil {
		return "", "", err
	}

	if zpoolExistsWith(ctx, run, poolName) {
		if err := verifyPoolBackingDevice(ctx, run, poolName, zfsDevice, canonicalDev); err != nil {
			return "", "", err
		}
		mp, err := run(ctx, "zfs", "get", "-H", "-o", "value", "mountpoint", poolName)
		if err != nil {
			return "", "", err
		}
		if got := strings.TrimSpace(mp); got != stateDir {
			return "", "", api.Errorf(api.CodeConflict, "ZFS pool %q already exists and is mounted at %s, not %s", poolName, got, stateDir).
				WithSuggestion("pass a different --dir or destroy the existing pool before retrying")
		}
		if err := enforceRootStateDir(stateDir, chownPath); err != nil {
			return "", "", err
		}
		_, _ = run(ctx, "zpool", "set", "autotrim=on", poolName)
		_, _ = run(ctx, "systemctl", "enable", "zfs-import-cache.service", "zfs-mount.service", "zfs.target")
		progressf(progress, "using existing ZFS pool %s mounted at %s", poolName, stateDir)
		return "zfs-device", poolName, nil
	}

	if _, err := run(ctx, "zpool", "import", "-d", filepath.Dir(zfsDevice), poolName); err == nil {
		if verr := verifyPoolBackingDevice(ctx, run, poolName, zfsDevice, canonicalDev); verr != nil {
			_, _ = run(ctx, "zpool", "export", poolName)
			return "", "", verr
		}
		mp, merr := run(ctx, "zfs", "get", "-H", "-o", "value", "mountpoint", poolName)
		if merr != nil {
			_, _ = run(ctx, "zpool", "export", poolName)
			return "", "", merr
		}
		if got := strings.TrimSpace(mp); got != stateDir {
			_, _ = run(ctx, "zpool", "export", poolName)
			return "", "", api.Errorf(api.CodeConflict, "imported ZFS pool %q is mounted at %s, not %s", poolName, got, stateDir).
				WithSuggestion("pass a different --dir or destroy the existing pool before retrying")
		}
		if err := enforceRootStateDir(stateDir, chownPath); err != nil {
			return "", "", err
		}
		_, _ = run(ctx, "zpool", "set", "autotrim=on", poolName)
		_, _ = run(ctx, "systemctl", "enable", "zfs-import-cache.service", "zfs-mount.service", "zfs.target")
		progressf(progress, "imported existing ZFS pool %s mounted at %s", poolName, stateDir)
		return "zfs-device", poolName, nil
	}

	if err := probeBlankBlockDevice(ctx, run, zfsDevice); err != nil {
		return "", "", err
	}

	progressf(progress, "creating ZFS pool %s on %s mounted at %s (autotrim=on, compression=zstd)", poolName, zfsDevice, stateDir)
	if err := enforceRootStateDir(stateDir, chownPath); err != nil {
		return "", "", err
	}
	if _, err := run(ctx, "zpool", "create",
		"-o", "ashift=12",
		"-o", "autotrim=on",
		"-o", "cachefile=/etc/zfs/zpool.cache",
		"-O", "compression=zstd",
		"-O", "atime=off",
		"-O", "xattr=sa",
		"-O", "dnodesize=auto",
		"-m", stateDir,
		poolName, zfsDevice,
	); err != nil {
		return "", "", fmt.Errorf("create ZFS pool %s on %s: %w", poolName, zfsDevice, err)
	}
	if err := enforceRootStateDir(stateDir, chownPath); err != nil {
		return "", "", err
	}
	_, _ = run(ctx, "systemctl", "enable", "zfs-import-cache.service", "zfs-mount.service", "zfs.target")
	return "zfs-device", poolName, nil
}

// cleanupLegacyTrimUnits disables and removes the older hotbed-trim.{service,timer}
// units created by earlier macOS VM provisioning before unification onto
// hotbed-worker-trim.{service,timer}.
func cleanupLegacyTrimUnits(ctx context.Context, unitDir string, run func(context.Context, string, ...string) (string, error)) {
	timerPath := filepath.Join(unitDir, legacyTrimTimer)
	svcPath := filepath.Join(unitDir, legacyTrimService)
	_, timerErr := os.Lstat(timerPath)
	_, svcErr := os.Lstat(svcPath)
	if timerErr != nil && svcErr != nil {
		return
	}
	_, _ = run(ctx, "systemctl", "disable", "--now", legacyTrimTimer)
	_ = os.Remove(timerPath)
	_ = os.Remove(svcPath)
}

func isValidLinuxPoolName(name string) bool {
	return name != "" && linuxPoolNameRE.MatchString(name) && !strings.Contains(name, "..")
}

// resolveLinuxPoolName determines the ZFS pool name for baseDir without ever
// trusting legacy <baseDir>/setup.json (which lives inside the non-root-owned
// baseDir). When root-owned /etc/hotbed/worker-*.json does not yet exist (fresh
// setup or pre-upgrade migration), it derives any existing pool name from
// trusted live ZFS state (matching stateDir mountpoint) and falls back to
// DefaultPoolName.
func resolveLinuxPoolName(
	ctx context.Context,
	baseDir, stateDir, imgPath string,
	expectedMetaUID uint32,
	run func(context.Context, string, ...string) (string, error),
) (string, error) {
	metaPath := linuxMetaPath(baseDir)
	if _, err := os.Lstat(metaPath); err == nil {
		var prev LinuxSetupMeta
		if err := readTrustedJSON(metaPath, expectedMetaUID, &prev); err != nil {
			return "", api.Errorf(api.CodeInvalidArgs, "%v", err).
				WithSuggestion("re-run `sudo hotbed worker init CONTROLLER` to write root-owned setup metadata")
		}
		if err := validateLinuxMeta(prev, baseDir); err != nil {
			return "", api.Errorf(api.CodeInvalidArgs, "%s: %v", metaPath, err)
		}
		if prev.PoolName != "" {
			return prev.PoolName, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}

	if pool, ok := findLivePoolForMountpoint(ctx, stateDir, run); ok {
		return pool, nil
	}
	if imgPath != "" {
		if _, err := os.Stat(imgPath); err == nil {
			if pool, ok := findExportedPoolInDir(ctx, baseDir, run); ok {
				return pool, nil
			}
		}
	}
	return DefaultPoolName, nil
}

func findLivePoolForMountpoint(ctx context.Context, stateDir string, run func(context.Context, string, ...string) (string, error)) (string, bool) {
	out, err := run(ctx, "zpool", "list", "-H", "-o", "name")
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(out, "\n") {
		name := strings.TrimSpace(line)
		if !isValidLinuxPoolName(name) {
			continue
		}
		mp, err := run(ctx, "zfs", "get", "-H", "-o", "value", "mountpoint", name)
		if err == nil && strings.TrimSpace(mp) == stateDir {
			return name, true
		}
	}
	return "", false
}

func findExportedPoolInDir(ctx context.Context, baseDir string, run func(context.Context, string, ...string) (string, error)) (string, bool) {
	out, err := run(ctx, "zpool", "import", "-d", baseDir)
	if err != nil {
		return "", false
	}
	var found string
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "pool:") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(trimmed, "pool:"))
		if !isValidLinuxPoolName(name) {
			continue
		}
		if found != "" && found != name {
			return "", false
		}
		found = name
	}
	return found, found != ""
}

// resolveLinuxServiceBinary returns the binary path for systemd ExecStart.
// If the binary or any ancestor directory is not owned by root or is writable
// by non-root users (or SELinux is enforcing and the binary lives under /home),
// it copies the currently executing binary via /proc/self/exe into root-owned
// /usr/local/bin/hotbed (0755) after verifying every ancestor of the target
// directory is root-owned, non-symlink, and not group/world-writable.
func resolveLinuxServiceBinary(progress io.Writer) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find hotbed binary: %w", err)
	}
	return resolveLinuxServiceBinaryFrom(exe, "/proc/self/exe", selinuxEnforcing(), installedServiceBin, "/", 0, progress)
}

func resolveLinuxServiceBinaryFrom(
	exe, copySrc string,
	selinux bool,
	installDst, trustRoot string,
	expectedUID uint32,
	progress io.Writer,
) (string, error) {
	inspected := exe
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		inspected = resolved
	}
	reason := ""
	if selinux && strings.HasPrefix(inspected, "/home/") {
		reason = "SELinux is enforcing and binary is under /home"
	} else if unsafe, why := pathWritableByNonRoot(inspected); unsafe {
		reason = why
	}
	if reason != "" {
		if copySrc == "" {
			copySrc = "/proc/self/exe"
		}
		if err := ensureTrustedInstallDir(filepath.Dir(installDst), trustRoot, expectedUID); err != nil {
			return "", api.Errorf(api.CodeInvalidArgs, "refusing to install service binary to %s: %v", installDst, err).
				WithSuggestion(fmt.Sprintf("ensure %s and its ancestor directories are real directories owned by root and not group- or world-writable", filepath.Dir(installDst)))
		}
		progressf(progress, "%s; installing %s to %s for systemd", reason, inspected, installDst)
		if err := copyFileAtomic(copySrc, installDst, 0o755); err != nil {
			return "", fmt.Errorf("install binary to %s: %w", installDst, err)
		}
		return installDst, nil
	}
	return inspected, nil
}

// verifyTrustedDirChain verifies that every directory from dir up to trustRoot
// (inclusive) exists as a real directory (not a symlink), is owned by
// expectedUID, and is not group- or world-writable (mode & 0o022 == 0).
func verifyTrustedDirChain(dir, trustRoot string, expectedUID uint32) error {
	cur := filepath.Clean(dir)
	root := filepath.Clean(trustRoot)
	if !filepath.IsAbs(cur) || !filepath.IsAbs(root) {
		return fmt.Errorf("directory %q and root %q must be absolute paths", dir, trustRoot)
	}
	if cur != root && !(root == "/" && strings.HasPrefix(cur, "/")) && !isStrictSubpath(root, cur) {
		return fmt.Errorf("directory %s is outside trusted root %s", cur, root)
	}
	for {
		fi, err := os.Lstat(cur)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", cur, err)
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink", cur)
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", cur)
		}
		if perm := fi.Mode().Perm(); perm&0o022 != 0 {
			return fmt.Errorf("%s is group/world-writable (mode %04o)", cur, perm)
		}
		sys, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("cannot determine owner of %s", cur)
		}
		if sys.Uid != expectedUID {
			return fmt.Errorf("%s is owned by uid %d (want %d)", cur, sys.Uid, expectedUID)
		}
		if cur == root {
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return fmt.Errorf("directory %s did not reach %s", dir, root)
		}
		cur = parent
	}
	return nil
}

// ensureTrustedInstallDir verifies every existing ancestor from dir up to
// trustRoot is trusted, creates any missing leaf directories with 0755 (and
// root ownership when running as root), and re-verifies the entire chain.
func ensureTrustedInstallDir(dir, trustRoot string, expectedUID uint32) error {
	cleanDir := filepath.Clean(dir)
	cleanRoot := filepath.Clean(trustRoot)
	if !filepath.IsAbs(cleanDir) || !filepath.IsAbs(cleanRoot) {
		return fmt.Errorf("directory %q and root %q must be absolute paths", dir, trustRoot)
	}
	if cleanDir != cleanRoot && !(cleanRoot == "/" && strings.HasPrefix(cleanDir, "/")) && !isStrictSubpath(cleanRoot, cleanDir) {
		return fmt.Errorf("directory %s is outside trusted root %s", cleanDir, cleanRoot)
	}

	var missing []string
	cur := cleanDir
	for {
		_, err := os.Lstat(cur)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("inspect %s: %w", cur, err)
		}
		missing = append(missing, cur)
		if cur == cleanRoot {
			return fmt.Errorf("trusted root %s does not exist", cleanRoot)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return fmt.Errorf("directory %s has no existing ancestor under %s", cleanDir, cleanRoot)
		}
		cur = parent
	}
	if err := verifyTrustedDirChain(cur, cleanRoot, expectedUID); err != nil {
		return err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		p := missing[i]
		if err := os.Mkdir(p, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if err := os.Chmod(p, 0o755); err != nil {
			return err
		}
		if os.Geteuid() == 0 {
			if err := os.Chown(p, int(expectedUID), 0); err != nil {
				return err
			}
		}
	}
	return verifyTrustedDirChain(cleanDir, cleanRoot, expectedUID)
}

// pathWritableByNonRoot reports whether path or any of its ancestor
// directories is not owned by root (uid 0) or is group/world-writable.
func pathWritableByNonRoot(path string) (bool, string) {
	cur := filepath.Clean(path)
	if !filepath.IsAbs(cur) {
		return true, fmt.Sprintf("%s is not an absolute path", path)
	}
	for {
		fi, err := os.Lstat(cur)
		if err != nil {
			return true, fmt.Sprintf("cannot inspect %s: %v", cur, err)
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return true, fmt.Sprintf("%s is a symlink", cur)
		}
		if perm := fi.Mode().Perm(); perm&0o022 != 0 {
			return true, fmt.Sprintf("%s is writable by non-root (mode %04o)", cur, perm)
		}
		sys, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return true, fmt.Sprintf("cannot determine owner of %s", cur)
		}
		if sys.Uid != 0 {
			return true, fmt.Sprintf("%s is owned by uid %d (not root)", cur, sys.Uid)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return false, ""
}

func selinuxEnforcing() bool {
	b, err := os.ReadFile("/sys/fs/selinux/enforce")
	return err == nil && strings.TrimSpace(string(b)) == "1"
}

func linuxMetaPath(baseDir string) string {
	clean := filepath.Clean(baseDir)
	sum := sha256.Sum256([]byte(clean))
	return filepath.Join(linuxMetaDir, "worker-"+hex.EncodeToString(sum[:8])+".json")
}

func legacyLinuxMetaPath(baseDir string) string {
	return filepath.Join(baseDir, "setup.json")
}

func removeLegacyMetaIfRegular(baseDir string) {
	_, _ = removeAnchoredTarget(baseDir, legacyLinuxMetaPath(baseDir), true)
}

func writeTrustedJSONAtomic(path string, v any, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(dir, 0, 0); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
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

// verifyTrustedMetaFile verifies that path and its parent directory are not
// symlinks, are owned by expectedUID, and are not group- or world-writable.
func verifyTrustedMetaFile(path string, expectedUID uint32) (os.FileInfo, error) {
	dir := filepath.Dir(path)
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if dirInfo.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing to trust setup metadata directory %s: path is a symlink", dir)
	}
	if !dirInfo.IsDir() {
		return nil, fmt.Errorf("refusing to trust setup metadata directory %s: not a directory", dir)
	}
	if perm := dirInfo.Mode().Perm(); perm&0o022 != 0 {
		return nil, fmt.Errorf("refusing to trust setup metadata directory %s: mode %04o is group/world-writable", dir, perm)
	}
	dirSys, ok := dirInfo.Sys().(*syscall.Stat_t)
	if !ok || dirSys.Uid != expectedUID {
		gotUID := uint32(0)
		if ok {
			gotUID = dirSys.Uid
		}
		return nil, fmt.Errorf("refusing to trust setup metadata directory %s: owned by uid %d (want %d)", dir, gotUID, expectedUID)
	}

	fileInfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fileInfo.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing to trust setup metadata %s: path is a symlink", path)
	}
	if !fileInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("refusing to trust setup metadata %s: not a regular file", path)
	}
	if perm := fileInfo.Mode().Perm(); perm&0o022 != 0 {
		return nil, fmt.Errorf("refusing to trust setup metadata %s: mode %04o is group/world-writable", path, perm)
	}
	fileSys, ok := fileInfo.Sys().(*syscall.Stat_t)
	if !ok || fileSys.Uid != expectedUID {
		gotUID := uint32(0)
		if ok {
			gotUID = fileSys.Uid
		}
		return nil, fmt.Errorf("refusing to trust setup metadata %s: owned by uid %d (want %d)", path, gotUID, expectedUID)
	}
	return fileInfo, nil
}

func readTrustedJSON(path string, expectedUID uint32, v any) error {
	linfo, err := verifyTrustedMetaFile(path, expectedUID)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	finfo, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(linfo, finfo) || !finfo.Mode().IsRegular() {
		return fmt.Errorf("refusing to trust setup metadata %s: file changed while opening", path)
	}
	if perm := finfo.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("refusing to trust setup metadata %s: mode %04o is group/world-writable", path, perm)
	}
	fsys, ok := finfo.Sys().(*syscall.Stat_t)
	if !ok || fsys.Uid != expectedUID {
		return fmt.Errorf("refusing to trust setup metadata %s: owner mismatch", path)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func isStrictSubpath(parent, child string) bool {
	if !filepath.IsAbs(parent) || !filepath.IsAbs(child) {
		return false
	}
	cleanParent := filepath.Clean(parent)
	cleanChild := filepath.Clean(child)
	if cleanParent == "/" || cleanChild == "/" || cleanChild != child {
		return false
	}
	rel, err := filepath.Rel(cleanParent, cleanChild)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// validateLinuxMeta rejects unexpected or potentially dangerous fields read
// from Linux setup metadata before any privileged systemctl, zpool, or
// filesystem operation acts on them.
func validateLinuxMeta(m LinuxSetupMeta, baseDir string) error {
	cleanBase := filepath.Clean(baseDir)
	if !filepath.IsAbs(cleanBase) || cleanBase == "/" {
		return fmt.Errorf("invalid base directory %q", baseDir)
	}
	if !linuxServiceNameRE.MatchString(m.ServiceName) || strings.Contains(m.ServiceName, "..") {
		return fmt.Errorf("invalid serviceName %q in setup metadata", m.ServiceName)
	}
	if m.TrimTimer != "" && (!linuxTrimTimerRE.MatchString(m.TrimTimer) || strings.Contains(m.TrimTimer, "..")) {
		return fmt.Errorf("invalid trimTimer %q in setup metadata", m.TrimTimer)
	}
	switch m.StorageMode {
	case "btrfs", "zfs", "zfs-file", "zfs-device":
	default:
		return fmt.Errorf("invalid storageMode %q in setup metadata", m.StorageMode)
	}
	if (m.StorageMode == "zfs-file" || m.StorageMode == "zfs-device") && m.PoolName == "" {
		return fmt.Errorf("missing poolName for %s storage in setup metadata", m.StorageMode)
	}
	if m.PoolName != "" && (!linuxPoolNameRE.MatchString(m.PoolName) || strings.Contains(m.PoolName, "..")) {
		return fmt.Errorf("invalid poolName %q in setup metadata", m.PoolName)
	}
	if m.StorageMode == "zfs-device" {
		if m.StateDir != cleanBase && !isStrictSubpath(cleanBase, m.StateDir) {
			return fmt.Errorf("invalid stateDir %q in setup metadata (must be %s or a clean path under it)", m.StateDir, cleanBase)
		}
		if err := validateZFSDevicePath(m.ZFSDevice); err != nil {
			return fmt.Errorf("invalid zfsDevice in setup metadata: %w", err)
		}
	} else if !isStrictSubpath(cleanBase, m.StateDir) {
		return fmt.Errorf("invalid stateDir %q in setup metadata (must be a clean path under %s)", m.StateDir, cleanBase)
	}
	if m.StorageMode != "zfs-device" && m.ZFSDevice != "" {
		return fmt.Errorf("unexpected zfsDevice %q for %s storage in setup metadata", m.ZFSDevice, m.StorageMode)
	}
	if m.StorageMode == "zfs-file" && m.ImagePath == "" {
		return errors.New("missing imagePath for zfs-file storage in setup metadata")
	}
	if m.ImagePath != "" && !isStrictSubpath(cleanBase, m.ImagePath) {
		return fmt.Errorf("invalid imagePath %q in setup metadata (must be a clean path under %s)", m.ImagePath, cleanBase)
	}
	return nil
}

// workerRecord is the file worker init records the worker's setup in, its
// controller among it.
func workerRecord(baseDir string) string {
	p := linuxMetaPath(baseDir)
	if _, err := os.Lstat(p); err == nil {
		return p
	}
	return legacyLinuxMetaPath(baseDir)
}

func loadLinuxMeta(baseDir string, requireRoot bool) (LinuxSetupMeta, string, error) {
	metaPath := linuxMetaPath(baseDir)
	if _, err := os.Lstat(metaPath); errors.Is(err, fs.ErrNotExist) {
		legacy := legacyLinuxMetaPath(baseDir)
		if _, lerr := os.Lstat(legacy); lerr == nil {
			metaPath = legacy
		} else {
			return LinuxSetupMeta{}, "", err
		}
	} else if err != nil {
		return LinuxSetupMeta{}, "", err
	}

	var m LinuxSetupMeta
	if requireRoot && os.Geteuid() == 0 {
		if err := readTrustedJSON(metaPath, 0, &m); err != nil {
			return LinuxSetupMeta{}, "", api.Errorf(api.CodeInvalidArgs, "%v", err).
				WithSuggestion("re-run `sudo hotbed worker init CONTROLLER` to write root-owned setup metadata")
		}
	} else {
		if err := readJSON(metaPath, &m); err != nil {
			return LinuxSetupMeta{}, "", err
		}
	}
	if err := validateLinuxMeta(m, baseDir); err != nil {
		return LinuxSetupMeta{}, "", api.Errorf(api.CodeInvalidArgs, "%s: %v", metaPath, err)
	}
	return m, metaPath, nil
}

// GetStatus reports the local Linux worker's status.
func GetStatus(ctx context.Context, dir string) (Status, error) {
	baseDir, err := ResolveDir(dir)
	if err != nil {
		return Status{}, err
	}
	meta, _, err := loadLinuxMeta(baseDir, false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Status{}, api.Errorf(api.CodeNotInitialized, "no worker configured in %s", baseDir).
				WithSuggestion("run `sudo hotbed worker init CONTROLLER`")
		}
		return Status{}, err
	}
	st := Status{
		Platform:   "linux",
		Dir:        baseDir,
		StateDir:   meta.StateDir,
		Controller: meta.Controller,
		User:       meta.User,
		Storage:    meta.StorageMode,
		Running:    isSystemdActive(ctx, meta.ServiceName),
		Service:    meta.ServiceName,
	}
	if meta.ImagePath != "" {
		if ds, err := FileDiskStat("zfs", meta.ImagePath, meta.DiskSize); err == nil {
			st.Disks = append(st.Disks, ds)
		}
	}
	return st, nil
}

// Start starts the local Linux worker systemd service.
func Start(ctx context.Context, dir string) (Status, error) {
	if os.Geteuid() != 0 {
		return Status{}, api.Errorf(api.CodeInvalidArgs, "starting the Linux worker service requires root").
			WithSuggestion("run with sudo: sudo hotbed worker start")
	}
	baseDir, err := ResolveDir(dir)
	if err != nil {
		return Status{}, err
	}
	meta, _, err := loadLinuxMeta(baseDir, true)
	if err != nil {
		return Status{}, err
	}
	if _, err := runCmd(ctx, "systemctl", "start", meta.ServiceName); err != nil {
		return Status{}, err
	}
	return GetStatus(ctx, baseDir)
}

// Stop stops the local Linux worker systemd service.
func Stop(ctx context.Context, dir string) (Status, error) {
	if os.Geteuid() != 0 {
		return Status{}, api.Errorf(api.CodeInvalidArgs, "stopping the Linux worker service requires root").
			WithSuggestion("run with sudo: sudo hotbed worker stop")
	}
	baseDir, err := ResolveDir(dir)
	if err != nil {
		return Status{}, err
	}
	meta, _, err := loadLinuxMeta(baseDir, true)
	if err != nil {
		return Status{}, err
	}
	if _, err := runCmd(ctx, "systemctl", "stop", meta.ServiceName); err != nil {
		return Status{}, err
	}
	return GetStatus(ctx, baseDir)
}

// Trim waits for ZFS async frees and advances TXGs so autotrim=on punches
// freed blocks out of sparse ZFS backing storage without initializing
// untouched metaslabs.
func Trim(ctx context.Context, dir string) (TrimResult, error) {
	if os.Geteuid() != 0 {
		return TrimResult{}, api.Errorf(api.CodeInvalidArgs, "trimming ZFS storage requires root").
			WithSuggestion("run with sudo: sudo hotbed worker trim")
	}
	baseDir, err := ResolveDir(dir)
	if err != nil {
		return TrimResult{}, err
	}
	meta, _, err := loadLinuxMeta(baseDir, true)
	if err != nil {
		return TrimResult{}, err
	}
	if meta.PoolName == "" {
		return TrimResult{}, api.Errorf(api.CodeInvalidArgs, "worker storage (%s) is not a managed sparse ZFS pool", meta.StorageMode)
	}
	var beforeStat DiskStat
	if meta.ImagePath != "" {
		beforeStat, err = FileDiskStat("zfs", meta.ImagePath, meta.DiskSize)
		if err != nil {
			return TrimResult{}, err
		}
	}
	// Wait for async frees and advance past ZFS's 2-TXG deferred-free window so
	// autotrim=on punches freed holes out without initializing untouched metaslabs.
	_, _ = runCmd(ctx, "zpool", "wait", "-t", "free", meta.PoolName)
	for range 4 {
		if _, err := runCmd(ctx, "zpool", "sync", meta.PoolName); err != nil {
			return TrimResult{}, err
		}
	}
	if meta.ImagePath == "" {
		return TrimResult{Dir: baseDir}, nil
	}
	afterStat, err := FileDiskStat("zfs", meta.ImagePath, meta.DiskSize)
	if err != nil {
		return TrimResult{}, err
	}
	return TrimResult{
		Dir:            baseDir,
		BeforeBytes:    beforeStat.AllocatedBytes,
		AfterBytes:     afterStat.AllocatedBytes,
		ReclaimedBytes: max(0, beforeStat.AllocatedBytes-afterStat.AllocatedBytes),
		Disks:          []DiskStat{afterStat},
	}, nil
}

// Destroy stops the Linux worker service, removes its systemd units, and
// deletes its sparse ZFS pool/image and state directory.
func Destroy(ctx context.Context, o DestroyOptions) (DestroyResult, error) {
	if os.Geteuid() != 0 {
		return DestroyResult{}, api.Errorf(api.CodeInvalidArgs, "destroying a Linux worker requires root").
			WithSuggestion("run with sudo: sudo hotbed worker destroy")
	}
	baseDir, err := ResolveDir(o.Dir)
	if err != nil {
		return DestroyResult{}, err
	}
	meta, metaPath, err := loadLinuxMeta(baseDir, true)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return DestroyResult{}, api.Errorf(api.CodeNotInitialized, "no worker configured in %s", baseDir)
		}
		return DestroyResult{}, err
	}
	return destroyLinuxWorker(ctx, baseDir, meta, metaPath, systemdUnitDir, runCmd)
}

func destroyLinuxWorker(
	ctx context.Context,
	baseDir string,
	meta LinuxSetupMeta,
	metaPath, unitDir string,
	run func(context.Context, string, ...string) (string, error),
) (DestroyResult, error) {
	if err := validateLinuxMeta(meta, baseDir); err != nil {
		return DestroyResult{}, api.Errorf(api.CodeInvalidArgs, "invalid setup metadata: %v", err)
	}
	var removed []string
	if meta.ServiceName != "" {
		unitPath := filepath.Join(unitDir, meta.ServiceName)
		_, statErr := os.Lstat(unitPath)
		if statErr == nil || isSystemdActiveWith(ctx, run, meta.ServiceName) {
			if _, err := run(ctx, "systemctl", "disable", "--now", meta.ServiceName); err != nil {
				return DestroyResult{}, fmt.Errorf("disable service %s: %w", meta.ServiceName, err)
			}
		}
		if err := os.Remove(unitPath); err == nil {
			removed = append(removed, unitPath)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return DestroyResult{}, fmt.Errorf("remove unit %s: %w", unitPath, err)
		}
	}
	if meta.TrimTimer != "" {
		timerPath := filepath.Join(unitDir, meta.TrimTimer)
		_, statErr := os.Lstat(timerPath)
		if statErr == nil || isSystemdActiveWith(ctx, run, meta.TrimTimer) {
			if _, err := run(ctx, "systemctl", "disable", "--now", meta.TrimTimer); err != nil {
				return DestroyResult{}, fmt.Errorf("disable trim timer %s: %w", meta.TrimTimer, err)
			}
		}
		unitNames := []string{meta.TrimTimer}
		if linuxTrimService != meta.TrimTimer {
			unitNames = append(unitNames, linuxTrimService)
		}
		for _, name := range unitNames {
			p := filepath.Join(unitDir, name)
			if err := os.Remove(p); err == nil {
				removed = append(removed, p)
			} else if !errors.Is(err, fs.ErrNotExist) {
				return DestroyResult{}, fmt.Errorf("remove unit %s: %w", p, err)
			}
		}
	}
	if meta.ServiceName != "" || meta.TrimTimer != "" {
		if _, err := run(ctx, "systemctl", "daemon-reload"); err != nil {
			return DestroyResult{}, fmt.Errorf("reload systemd: %w", err)
		}
	}

	if (meta.StorageMode == "zfs-file" || meta.StorageMode == "zfs-device") && meta.PoolName != "" {
		if zpoolExistsWith(ctx, run, meta.PoolName) {
			if _, err := run(ctx, "zpool", "destroy", "-f", meta.PoolName); err != nil {
				return DestroyResult{}, fmt.Errorf("destroy ZFS pool %s: %w", meta.PoolName, err)
			}
			removed = append(removed, "zpool:"+meta.PoolName)
		}
		if meta.StorageMode == "zfs-file" && meta.ImagePath != "" {
			didRemove, err := removeAnchoredTarget(baseDir, meta.ImagePath, true)
			if err != nil {
				return DestroyResult{}, err
			}
			if didRemove {
				removed = append(removed, meta.ImagePath)
			}
		}
	}
	if meta.StateDir != "" && isStrictSubpath(baseDir, meta.StateDir) {
		didRemove, err := removeAnchoredTarget(baseDir, meta.StateDir, false)
		if err != nil {
			return DestroyResult{}, err
		}
		if didRemove {
			removed = append(removed, meta.StateDir)
		}
	}

	// Only remove trusted metadata after every cleanup step succeeded.
	if err := os.Remove(metaPath); err == nil {
		removed = append(removed, metaPath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return DestroyResult{}, fmt.Errorf("remove setup metadata %s: %w", metaPath, err)
	}
	removeLegacyMetaIfRegular(baseDir)
	return DestroyResult{Dir: baseDir, Removed: removed}, nil
}

// openAnchoredBaseRoot opens baseDir as an os.Root only after verifying that
// baseDir itself is a real directory (not a symlink, opened with O_NOFOLLOW)
// and that no non-root-owned ancestor of baseDir is a symlink.
func openAnchoredBaseRoot(baseDir string) (*os.Root, error) {
	cleanBase := filepath.Clean(baseDir)
	if !filepath.IsAbs(cleanBase) || cleanBase == "/" {
		return nil, fmt.Errorf("invalid base directory %q", baseDir)
	}
	// Check that no non-root-owned ancestor (and not baseDir itself) is a symlink.
	for cur := cleanBase; ; {
		fi, err := os.Lstat(cur)
		if err != nil {
			return nil, err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			if cur == cleanBase {
				return nil, fmt.Errorf("refusing to clean %s: base directory is a symlink", cleanBase)
			}
			sys, ok := fi.Sys().(*syscall.Stat_t)
			if !ok || sys.Uid != 0 {
				return nil, fmt.Errorf("refusing to clean %s: ancestor %s is a symlink", cleanBase, cur)
			}
		} else if !fi.IsDir() {
			return nil, fmt.Errorf("refusing to clean %s: %s is not a directory", cleanBase, cur)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}

	fd, err := syscall.Open(cleanBase, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("refusing to clean %s: base directory is a symlink", cleanBase)
		}
		return nil, err
	}
	baseFile := os.NewFile(uintptr(fd), cleanBase)
	defer baseFile.Close()
	fdInfo, err := baseFile.Stat()
	if err != nil {
		return nil, err
	}

	root, err := os.OpenRoot(cleanBase)
	if err != nil {
		return nil, err
	}
	rootInfo, err := root.Stat(".")
	if err != nil {
		root.Close()
		return nil, err
	}
	if !os.SameFile(fdInfo, rootInfo) {
		root.Close()
		return nil, fmt.Errorf("refusing to clean %s: directory changed while opening", cleanBase)
	}
	return root, nil
}

// removeAnchoredTarget removes target strictly inside baseDir via os.OpenRoot.
// Every path component between baseDir and target (inclusive) is verified via
// root.Lstat to be a non-symlink before removal; if expectRegular is true, the
// leaf must be a regular file and is removed with root.Remove, otherwise it is
// removed with root.RemoveAll.
func removeAnchoredTarget(baseDir, target string, expectRegular bool) (bool, error) {
	cleanBase := filepath.Clean(baseDir)
	cleanTarget := filepath.Clean(target)
	if !isStrictSubpath(cleanBase, target) || cleanTarget != target {
		return false, fmt.Errorf("refusing to remove %q: not a clean subpath of %s", target, cleanBase)
	}
	rel, err := filepath.Rel(cleanBase, cleanTarget)
	if err != nil {
		return false, err
	}

	root, err := openAnchoredBaseRoot(cleanBase)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer root.Close()

	parts := strings.Split(filepath.ToSlash(rel), "/")
	cur := ""
	var leafInfo fs.FileInfo
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false, fmt.Errorf("refusing to remove %s: invalid relative component %q", target, part)
		}
		if i == 0 {
			cur = part
		} else {
			cur = cur + "/" + part
		}
		fi, err := root.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("inspect %s under %s: %w", cur, cleanBase, err)
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return false, fmt.Errorf("refusing to remove %s: %s is a symlink", target, filepath.Join(cleanBase, filepath.FromSlash(cur)))
		}
		if i < len(parts)-1 && !fi.IsDir() {
			return false, fmt.Errorf("refusing to remove %s: %s is not a directory", target, filepath.Join(cleanBase, filepath.FromSlash(cur)))
		}
		leafInfo = fi
	}

	relSlash := filepath.ToSlash(rel)
	if expectRegular {
		if leafInfo == nil || !leafInfo.Mode().IsRegular() {
			return false, fmt.Errorf("refusing to remove %s: not a regular file", target)
		}
		if err := root.Remove(relSlash); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return false, nil
			}
			return false, fmt.Errorf("remove %s: %w", target, err)
		}
		return true, nil
	}
	if err := root.RemoveAll(relSlash); err != nil {
		return false, fmt.Errorf("remove %s: %w", target, err)
	}
	return true, nil
}

// SSH is only meaningful on macOS QEMU workers.
func SSH(context.Context, string, []string) (int, error) {
	return 1, api.Errorf(api.CodePlatformNotSupported, "worker ssh is for macOS QEMU VMs; this worker runs directly on Linux")
}

// Supervise is only meaningful on macOS QEMU workers: a Linux worker runs
// on its host, which the controller does not power.
func Supervise(context.Context, string, func(string) (hotbedv1connect.ClusterServiceClient, error), *slog.Logger) error {
	return api.Errorf(api.CodePlatformNotSupported, "worker supervise is for macOS QEMU VMs; this worker runs directly on Linux")
}

func DaemonVMStatus(context.Context, string) DaemonVMState {
	return DaemonVMState{Configured: false, State: "unconfigured"}
}

func SuperviseVMForDaemon(ctx context.Context, _ string, _ func(string) (hotbedv1connect.ClusterServiceClient, error), _ *slog.Logger, onState func(DaemonVMState)) {
	if onState != nil {
		onState(DaemonVMState{Configured: false, State: "unconfigured"})
	}
	<-ctx.Done()
}

func StartDaemonVM(context.Context, string) error {
	return api.Errorf(api.CodePlatformNotSupported, "local VM workers are only supported on macOS")
}

func StopDaemonVM(context.Context, string) error {
	return api.Errorf(api.CodePlatformNotSupported, "local VM workers are only supported on macOS")
}

func DisableLegacyWorkerLaunchAgent(context.Context, string) error { return nil }

func zpoolExists(ctx context.Context, name string) bool {
	return zpoolExistsWith(ctx, runCmd, name)
}

func zpoolExistsWith(ctx context.Context, run func(context.Context, string, ...string) (string, error), name string) bool {
	_, err := run(ctx, "zpool", "list", "-H", "-o", "name", name)
	return err == nil
}

func isSystemdActive(ctx context.Context, unit string) bool {
	return isSystemdActiveWith(ctx, runCmd, unit)
}

func isSystemdActiveWith(ctx context.Context, run func(context.Context, string, ...string) (string, error), unit string) bool {
	out, err := run(ctx, "systemctl", "is-active", unit)
	return err == nil && strings.TrimSpace(out) == "active"
}

func runCmd(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
