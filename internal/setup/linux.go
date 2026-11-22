//go:build linux

package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/worker"
)

const (
	zfsSuperMagic         = 0x2fc12fc1
	linuxWorkerService    = "hotbed-worker.service"
	linuxTrimService      = "hotbed-worker-trim.service"
	linuxTrimTimer        = "hotbed-worker-trim.timer"
	systemdUnitDir        = "/etc/systemd/system"
	selinuxInstalledBin   = "/usr/local/bin/hotbed"
)

// Setup configures and starts a hotbed worker directly on this Linux host,
// storing its state under o.Dir (default ~/.hotbed).
func Setup(ctx context.Context, o Options, tn worker.Tailnet, hc *http.Client, check worker.Check) (Result, error) {
	if os.Geteuid() != 0 {
		return Result{}, api.Errorf(api.CodeInvalidArgs, "setting up a Linux worker requires root privileges (for storage, join, and systemd)").
			WithSuggestion("run with sudo: sudo hotbed worker setup " + o.Controller)
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
	if _, err := tn.Self(ctx); err != nil {
		return Result{}, err
	}

	progressf(o.Progress, "setting up Linux worker in %s (command user %s)", baseDir, username)
	if err := ensureTraversableBaseDir(baseDir, u); err != nil {
		return Result{}, err
	}

	stateDir := filepath.Join(baseDir, "worker")
	diskSize := o.DiskSize
	if diskSize <= 0 {
		diskSize = DefaultDiskSize
	}

	storageMode, poolName, imgPath, err := ensureLinuxStorage(ctx, baseDir, stateDir, diskSize, o.Progress)
	if err != nil {
		return Result{}, err
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
		}
	} else if !errors.Is(err, statedir.ErrNotInitialized) {
		return Result{}, err
	} else {
		progressf(o.Progress, "joining controller %s", controller)
		joinRes, err = worker.Join(ctx, worker.JoinOptions{
			Dir:            stateDir,
			Controller:     controller,
			User:           username,
			Dataset:        o.ZFSDataset,
			UnitProperties: o.UnitProperties,
			Slice:          o.Slice,
			TmpDir:         o.TmpDir,
		}, tn, hc, check)
		if err != nil {
			return Result{}, err
		}
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
		DiskSize:    diskSize,
		ServiceName: linuxWorkerService,
	}

	if storageMode == "zfs-file" {
		trimSvc, trimTmr := SystemdTrimUnits(poolName, false)
		if err := os.WriteFile(filepath.Join(systemdUnitDir, linuxTrimService), []byte(trimSvc), 0o644); err != nil {
			return Result{}, fmt.Errorf("write %s: %w", linuxTrimService, err)
		}
		if err := os.WriteFile(filepath.Join(systemdUnitDir, linuxTrimTimer), []byte(trimTmr), 0o644); err != nil {
			return Result{}, fmt.Errorf("write %s: %w", linuxTrimTimer, err)
		}
		meta.TrimTimer = linuxTrimTimer
	}

	if err := writeJSONAtomic(filepath.Join(baseDir, "setup.json"), meta, 0o644); err != nil {
		return Result{}, fmt.Errorf("save setup metadata: %w", err)
	}

	running := false
	if !o.NoStart {
		progressf(o.Progress, "enabling and starting %s", linuxWorkerService)
		if _, err := runCmd(ctx, "systemctl", "daemon-reload"); err != nil {
			return Result{}, err
		}
		if _, err := runCmd(ctx, "systemctl", "enable", linuxWorkerService); err != nil {
			return Result{}, err
		}
		if _, err := runCmd(ctx, "systemctl", "restart", linuxWorkerService); err != nil {
			return Result{}, err
		}
		if meta.TrimTimer != "" {
			if _, err := runCmd(ctx, "systemctl", "enable", "--now", meta.TrimTimer); err != nil {
				return Result{}, err
			}
		}
		running = isSystemdActive(ctx, linuxWorkerService)
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

func ensureTraversableBaseDir(baseDir string, u *user.User) error {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", baseDir, err)
	}
	if err := os.Chmod(baseDir, 0o755); err != nil {
		return fmt.Errorf("chmod %s: %w", baseDir, err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	_ = os.Chown(baseDir, uid, gid)
	return nil
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

	poolName = DefaultPoolName
	imgPath = filepath.Join(baseDir, "zfs.img")
	if prev, err := loadLinuxMeta(baseDir); err == nil && prev.PoolName != "" {
		poolName = prev.PoolName
	}

	if zpoolExists(ctx, poolName) {
		mp, err := runCmd(ctx, "zfs", "get", "-H", "-o", "value", "mountpoint", poolName)
		if err != nil {
			return "", "", "", err
		}
		if got := strings.TrimSpace(mp); got != stateDir {
			return "", "", "", api.Errorf(api.CodeConflict, "ZFS pool %q already exists and is mounted at %s, not %s", poolName, got, stateDir).
				WithSuggestion("pass a different --dir or destroy the existing pool before retrying")
		}
		_, _ = runCmd(ctx, "zpool", "set", "autotrim=on", poolName)
		progressf(progress, "using existing sparse ZFS pool %s mounted at %s", poolName, stateDir)
		return "zfs-file", poolName, imgPath, nil
	}

	if _, err := os.Stat(imgPath); err == nil {
		progressf(progress, "importing existing sparse ZFS pool %s from %s", poolName, imgPath)
		if _, err := runCmd(ctx, "zpool", "import", "-d", baseDir, poolName); err != nil {
			return "", "", "", fmt.Errorf("import existing ZFS pool from %s: %w", imgPath, err)
		}
		_, _ = runCmd(ctx, "zpool", "set", "autotrim=on", poolName)
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

	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return "", "", "", err
	}
	if _, err := runCmd(ctx, "zpool", "create", "-f",
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
	// Promptly punch out freed ZFS extents via autotrim instead of batching 32 TXGs.
	_ = os.WriteFile("/etc/modprobe.d/hotbed-zfs.conf", []byte("options zfs zfs_trim_txg_batch=1\n"), 0o644)
	_ = os.WriteFile("/sys/module/zfs/parameters/zfs_trim_txg_batch", []byte("1\n"), 0o644)
	return "zfs-file", poolName, imgPath, nil
}

// resolveLinuxServiceBinary returns the binary path for systemd ExecStart.
// On enforcing SELinux systems, systemd cannot execute binaries under /home,
// so a binary living under /home is installed to /usr/local/bin/hotbed.
func resolveLinuxServiceBinary(progress io.Writer) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find hotbed binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if selinuxEnforcing() && strings.HasPrefix(exe, "/home/") {
		progressf(progress, "SELinux is enforcing; installing %s to %s for systemd", exe, selinuxInstalledBin)
		if err := copyFileAtomic(exe, selinuxInstalledBin, 0o755); err != nil {
			return "", fmt.Errorf("install binary to %s: %w", selinuxInstalledBin, err)
		}
		return selinuxInstalledBin, nil
	}
	return exe, nil
}

func selinuxEnforcing() bool {
	b, err := os.ReadFile("/sys/fs/selinux/enforce")
	return err == nil && strings.TrimSpace(string(b)) == "1"
}

func copyFileAtomic(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
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

func loadLinuxMeta(baseDir string) (LinuxSetupMeta, error) {
	var m LinuxSetupMeta
	if err := readJSON(filepath.Join(baseDir, "setup.json"), &m); err != nil {
		return LinuxSetupMeta{}, err
	}
	return m, nil
}

// GetStatus reports the local Linux worker's status.
func GetStatus(ctx context.Context, dir string) (Status, error) {
	baseDir, err := ResolveDir(dir)
	if err != nil {
		return Status{}, err
	}
	meta, err := loadLinuxMeta(baseDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Status{}, api.Errorf(api.CodeNotInitialized, "no worker configured in %s", baseDir).
				WithSuggestion("run `sudo hotbed worker setup CONTROLLER`")
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
	meta, err := loadLinuxMeta(baseDir)
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
	meta, err := loadLinuxMeta(baseDir)
	if err != nil {
		return Status{}, err
	}
	if _, err := runCmd(ctx, "systemctl", "stop", meta.ServiceName); err != nil {
		return Status{}, err
	}
	return GetStatus(ctx, baseDir)
}

// Trim runs `zpool trim -w` on a file-backed ZFS worker pool to punch freed
// blocks out of the sparse backing image.
func Trim(ctx context.Context, dir string) (TrimResult, error) {
	if os.Geteuid() != 0 {
		return TrimResult{}, api.Errorf(api.CodeInvalidArgs, "trimming ZFS storage requires root").
			WithSuggestion("run with sudo: sudo hotbed worker trim")
	}
	baseDir, err := ResolveDir(dir)
	if err != nil {
		return TrimResult{}, err
	}
	meta, err := loadLinuxMeta(baseDir)
	if err != nil {
		return TrimResult{}, err
	}
	if meta.PoolName == "" {
		return TrimResult{}, api.Errorf(api.CodeInvalidArgs, "worker storage (%s) is not a managed sparse ZFS image", meta.StorageMode)
	}
	beforeStat, err := FileDiskStat("zfs", meta.ImagePath, meta.DiskSize)
	if err != nil {
		return TrimResult{}, err
	}
	// Wait for async frees and advance past ZFS's 2-TXG deferred-free window so
	// autotrim=on punches freed holes out of zfs.img without initializing
	// untouched metaslabs.
	_, _ = runCmd(ctx, "zpool", "wait", "-t", "free", meta.PoolName)
	for range 4 {
		if _, err := runCmd(ctx, "zpool", "sync", meta.PoolName); err != nil {
			return TrimResult{}, err
		}
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
	meta, err := loadLinuxMeta(baseDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return DestroyResult{}, api.Errorf(api.CodeNotInitialized, "no worker configured in %s", baseDir)
		}
		return DestroyResult{}, err
	}
	var removed []string
	if meta.ServiceName != "" {
		_, _ = runCmd(ctx, "systemctl", "disable", "--now", meta.ServiceName)
		p := filepath.Join(systemdUnitDir, meta.ServiceName)
		if os.Remove(p) == nil {
			removed = append(removed, p)
		}
	}
	if meta.TrimTimer != "" {
		_, _ = runCmd(ctx, "systemctl", "disable", "--now", meta.TrimTimer)
		for _, name := range []string{linuxTrimTimer, linuxTrimService} {
			p := filepath.Join(systemdUnitDir, name)
			if os.Remove(p) == nil {
				removed = append(removed, p)
			}
		}
	}
	_, _ = runCmd(ctx, "systemctl", "daemon-reload")

	if meta.StorageMode == "zfs-file" && meta.PoolName != "" {
		if zpoolExists(ctx, meta.PoolName) {
			if _, err := runCmd(ctx, "zpool", "destroy", "-f", meta.PoolName); err != nil {
				return DestroyResult{}, fmt.Errorf("destroy ZFS pool %s: %w", meta.PoolName, err)
			}
			removed = append(removed, "zpool:"+meta.PoolName)
		}
		if meta.ImagePath != "" && os.Remove(meta.ImagePath) == nil {
			removed = append(removed, meta.ImagePath)
		}
	}
	if meta.StateDir != "" {
		if err := os.RemoveAll(meta.StateDir); err == nil {
			removed = append(removed, meta.StateDir)
		}
	}
	metaPath := filepath.Join(baseDir, "setup.json")
	if os.Remove(metaPath) == nil {
		removed = append(removed, metaPath)
	}
	return DestroyResult{Dir: baseDir, Removed: removed}, nil
}

// SSH is only meaningful on macOS QEMU workers.
func SSH(context.Context, string, []string) (int, error) {
	return 1, api.Errorf(api.CodePlatformNotSupported, "worker ssh is for macOS QEMU VMs; this worker runs directly on Linux")
}

func zpoolExists(ctx context.Context, name string) bool {
	_, err := runCmd(ctx, "zpool", "list", "-H", "-o", "name", name)
	return err == nil
}

func isSystemdActive(ctx context.Context, unit string) bool {
	out, err := runCmd(ctx, "systemctl", "is-active", unit)
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
