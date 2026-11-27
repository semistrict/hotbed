//go:build linux

package main

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

type pid1Mount struct {
	src, target, fstype string
	flags               uintptr
	data                string
}

// pid1Mounts defines the Linux API filesystems mounted by hotbed-guestd PID 1.
// Notably:
//   - /dev/shm and /run are mounted with MS_NOSUID|MS_NODEV (matching standard
//     Linux conventions; /run omits MS_NOEXEC so /run/hotbed/git-auth helpers
//     remain executable).
//   - /tmp is intentionally NOT mounted as tmpfs: leaving /tmp on the cladefs
//     rootfs gives commands O_DIRECT support and full workspace disk capacity
//     without pinning guest RAM (matching chroot workspaces' on-volume /tmp).
var pid1Mounts = []pid1Mount{
	{"proc", "/proc", "proc", 0, ""},
	{"sysfs", "/sys", "sysfs", 0, ""},
	{"devtmpfs", "/dev", "devtmpfs", 0, ""},
	{"devpts", "/dev/pts", "devpts", 0, "gid=5,mode=620,ptmxmode=666"},
	{"tmpfs", "/dev/shm", "tmpfs", syscall.MS_NOSUID | syscall.MS_NODEV, "mode=1777"},
	{"tmpfs", "/run", "tmpfs", syscall.MS_NOSUID | syscall.MS_NODEV, "mode=755"},
	{"cgroup2", "/sys/fs/cgroup", "cgroup2", 0, ""},
	{"tracefs", "/sys/kernel/tracing", "tracefs", 0, ""},
	{"bpf", "/sys/fs/bpf", "bpf", 0, ""},
}

// prepareGuestTmpDirs ensures /tmp and /var/tmp under root exist with mode
// 01777 and, when clearTmp is true, clears stale entries from /tmp on boot.
func prepareGuestTmpDirs(log *slog.Logger, root string, clearTmp bool) {
	tmpDir := filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmpDir, 0o777); err != nil && log != nil {
		log.Warn("mkdir /tmp failed", "path", tmpDir, "err", err)
	}
	if clearTmp {
		if entries, err := os.ReadDir(tmpDir); err == nil {
			for _, ent := range entries {
				if err := os.RemoveAll(filepath.Join(tmpDir, ent.Name())); err != nil && log != nil {
					log.Warn("clean /tmp entry failed", "name", ent.Name(), "err", err)
				}
			}
		}
	}
	if err := os.Chmod(tmpDir, os.ModePerm|os.ModeSticky); err != nil && log != nil {
		log.Warn("chmod /tmp failed", "path", tmpDir, "err", err)
	}

	varTmpDir := filepath.Join(root, "var", "tmp")
	if err := os.MkdirAll(varTmpDir, 0o777); err != nil && log != nil {
		log.Warn("mkdir /var/tmp failed", "path", varTmpDir, "err", err)
	}
	if err := os.Chmod(varTmpDir, os.ModePerm|os.ModeSticky); err != nil && log != nil {
		log.Warn("chmod /var/tmp failed", "path", varTmpDir, "err", err)
	}
}

// ensureBootRuntimeDirs pre-creates <runRoot>/user/<uid> (0700, owned by
// uid:gid when running as root) for regular non-root users in passwdPath
// (uid 1000..65533) so XDG_RUNTIME_DIR exists immediately on boot.
func ensureBootRuntimeDirs(log *slog.Logger, passwdPath, runRoot string) {
	userRoot := filepath.Join(runRoot, "user")
	if err := os.MkdirAll(userRoot, 0o755); err != nil {
		if log != nil {
			log.Warn("mkdir runtime user root failed", "path", userRoot, "err", err)
		}
		return
	}
	_ = os.Chmod(userRoot, 0o755)

	f, err := os.Open(passwdPath)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 4 {
			continue
		}
		uid, err1 := strconv.Atoi(fields[2])
		gid, err2 := strconv.Atoi(fields[3])
		if err1 != nil || err2 != nil || uid < 1000 || uid >= 65534 {
			continue
		}
		dir := filepath.Join(userRoot, strconv.Itoa(uid))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			if log != nil {
				log.Warn("mkdir runtime user dir failed", "path", dir, "err", err)
			}
			continue
		}
		if os.Geteuid() == 0 {
			if err := os.Chown(dir, uid, gid); err != nil && log != nil {
				log.Warn("chown runtime user dir failed", "path", dir, "uid", uid, "gid", gid, "err", err)
			}
		}
		if err := os.Chmod(dir, 0o700); err != nil && log != nil {
			log.Warn("chmod runtime user dir failed", "path", dir, "err", err)
		}
	}
}

type pivotRootOps struct {
	mount     func(source, target, fstype string, flags uintptr, data string) error
	mkdirAll  func(path string, perm os.FileMode) error
	pivotRoot func(newroot, putold string) error
	chdir     func(dir string) error
	chroot    func(dir string) error
	unsetenv  func(key string) error
	// rootIsInitramfs reports whether / is the kernel's initramfs (rootfs),
	// which pivot_root refuses to leave.
	rootIsInitramfs func() (bool, error)
}

var defaultPivotRootOps = pivotRootOps{
	mount:     syscall.Mount,
	mkdirAll:  os.MkdirAll,
	pivotRoot: syscall.PivotRoot,
	chdir:     os.Chdir,
	chroot:    syscall.Chroot,
	unsetenv:  os.Unsetenv,
	rootIsInitramfs: func() (bool, error) {
		var st syscall.Statfs_t
		if err := syscall.Statfs("/", &st); err != nil {
			return false, err
		}
		return st.Type == ramfsMagic || st.Type == tmpfsMagic, nil
	},
}

// The initramfs's filesystem types (statfs(2)).
const (
	ramfsMagic = 0x858458f6
	tmpfsMagic = 0x01021994
)

// pivotToWorkspaceRoot switches from what hotbed-guestd booted off (the
// read-only host tools disk under Firecracker, an initramfs under machined) to
// the cladefs workspace rootfs specified by HOTBED_ROOT (<fstype>:<source>,
// e.g. "cladefs:rootfs"), leaving the old root mounted at /run/hotbed-tools so
// hotbed-guestd executes directly off it without baking binaries into
// workspace base images.
func pivotToWorkspaceRoot(log *slog.Logger) (pivoted, devMoved bool, err error) {
	return pivotToWorkspaceRootWithOps(log, os.Getenv("HOTBED_ROOT"), defaultPivotRootOps)
}

func pivotToWorkspaceRootWithOps(log *slog.Logger, spec string, ops pivotRootOps) (pivoted, devMoved bool, err error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return false, false, nil
	}
	fstype, source, ok := strings.Cut(spec, ":")
	if !ok || fstype == "" || source == "" {
		return false, false, fmt.Errorf("invalid HOTBED_ROOT %q (expected fstype:source)", spec)
	}

	// Ensure mount propagation is private so pivot_root never fails with EINVAL.
	_ = ops.mount("", "/", "", syscall.MS_PRIVATE|syscall.MS_REC, "")

	const newRoot = "/newroot"
	if err := ops.mount(source, newRoot, fstype, 0, ""); err != nil {
		return false, false, fmt.Errorf("mount workspace root %s (%s) on %s: %w", source, fstype, newRoot, err)
	}

	// Move kernel-mounted devtmpfs into the workspace rootfs so /dev/console
	// stays attached seamlessly across pivot_root.
	_ = ops.mkdirAll(filepath.Join(newRoot, "dev"), 0o755)
	if err := ops.mount("/dev", filepath.Join(newRoot, "dev"), "", syscall.MS_MOVE, ""); err == nil {
		devMoved = true
	} else if log != nil {
		log.Warn("move /dev into workspace root failed; will mount fresh devtmpfs", "err", err)
	}

	// Mount /run tmpfs on the workspace rootfs and pivot the old read-only tools
	// disk onto /run/hotbed-tools so it never writes to or pollutes /.
	newRun := filepath.Join(newRoot, "run")
	_ = ops.mkdirAll(newRun, 0o755)
	if err := ops.mount("tmpfs", newRun, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "mode=755"); err != nil {
		return false, devMoved, fmt.Errorf("mount tmpfs on %s: %w", newRun, err)
	}
	putOld := filepath.Join(newRun, "hotbed-tools")
	if err := ops.mkdirAll(putOld, 0o555); err != nil {
		return false, devMoved, fmt.Errorf("mkdir %s: %w", putOld, err)
	}
	initramfs, err := ops.rootIsInitramfs()
	if err != nil {
		return false, devMoved, fmt.Errorf("statfs /: %w", err)
	}
	if initramfs {
		// pivot_root(2) refuses to leave rootfs: bind the initramfs where the
		// tools disk would go, move the workspace root over /, and chroot
		// into it (switch_root's way, with the old root kept reachable).
		if err := ops.mount("/", putOld, "", syscall.MS_BIND, ""); err != nil {
			return false, devMoved, fmt.Errorf("bind the initramfs on %s: %w", putOld, err)
		}
		if err := ops.chdir(newRoot); err != nil {
			return false, devMoved, fmt.Errorf("chdir %s: %w", newRoot, err)
		}
		if err := ops.mount(newRoot, "/", "", syscall.MS_MOVE, ""); err != nil {
			return false, devMoved, fmt.Errorf("move %s over /: %w", newRoot, err)
		}
		if err := ops.chroot("."); err != nil {
			return false, devMoved, fmt.Errorf("chroot into the workspace root: %w", err)
		}
	} else if err := ops.pivotRoot(newRoot, putOld); err != nil {
		return false, devMoved, fmt.Errorf("pivot_root(%s, %s): %w", newRoot, putOld, err)
	}
	if err := ops.chdir("/"); err != nil {
		return false, devMoved, fmt.Errorf("chdir / after switching root: %w", err)
	}
	_ = ops.unsetenv("HOTBED_ROOT")
	return true, devMoved, nil
}

// guestSysctls are the kernel settings hotbed-guestd PID 1 applies at boot,
// keyed by path under /proc/sys.
var guestSysctls = []struct {
	name  string
	value string
}{
	// Writeback: bound dirty page accumulation in large-RAM microVMs
	// (12-18.5 GiB). Linux defaults (dirty_background_ratio=10%,
	// dirty_ratio=20%, dirty_expire_centisecs=3000) let gigabytes of build
	// outputs sit dirty in guest RAM for 30s before flushing to cladefsd; dirty
	// folios cannot be reclaimed or sealed into host DAX until written back.
	{"vm/dirty_bytes", "536870912\n"},           // 512 MiB cap before throttling writers
	{"vm/dirty_background_bytes", "67108864\n"}, // 64 MiB starts async writeback promptly
	{"vm/dirty_expire_centisecs", "500\n"},      // 5s dirty age (down from 30s)
	{"vm/dirty_writeback_centisecs", "200\n"},   // 2s flusher sweep (down from 5s)

	// Profiling: let the workspace user run perf (kernel stacks, tracepoints,
	// system-wide) and bpftrace-style readers resolve kernel symbols without
	// sudo. The VM is single-tenant and its user already has passwordless sudo
	// to root, so the distro defaults (perf_event_paranoid=2, kptr_restrict=1)
	// guard nothing here; they only made a plain `perf record -g` exit 255.
	{"kernel/perf_event_paranoid", "-1\n"},
	{"kernel/kptr_restrict", "0\n"},
}

func configureGuestSysctls(log *slog.Logger, procRoot string) {
	sysDir := filepath.Join(procRoot, "sys")
	for _, s := range guestSysctls {
		p := filepath.Join(sysDir, s.name)
		if err := os.WriteFile(p, []byte(s.value), 0o644); err != nil && !os.IsNotExist(err) && log != nil {
			log.Warn("set guest sysctl failed", "path", p, "value", strings.TrimSpace(s.value), "err", err)
		}
	}
}

// configureGuestMGLRU enables Linux MGLRU and sets a 1s thrashing protection
// window when the guest kernel exposes /sys/kernel/mm/lru_gen (required by
// cladefsd's governed memory mode and beneficial for general guest reclaim).
func configureGuestMGLRU(log *slog.Logger, sysRoot string) {
	lruDir := filepath.Join(sysRoot, "kernel", "mm", "lru_gen")
	enabledPath := filepath.Join(lruDir, "enabled")
	if raw, err := os.ReadFile(enabledPath); err == nil {
		if v, perr := strconv.ParseUint(strings.TrimSpace(string(raw)), 0, 64); perr == nil && v == 0 {
			if werr := os.WriteFile(enabledPath, []byte("y\n"), 0o644); werr != nil && log != nil {
				log.Warn("enable guest MGLRU failed", "path", enabledPath, "err", werr)
			}
		}
	}
	ttlPath := filepath.Join(lruDir, "min_ttl_ms")
	if raw, err := os.ReadFile(ttlPath); err == nil && strings.TrimSpace(string(raw)) == "0" {
		if werr := os.WriteFile(ttlPath, []byte("1000\n"), 0o644); werr != nil && log != nil {
			log.Warn("set guest MGLRU min_ttl_ms failed", "path", ttlPath, "err", werr)
		}
	}
}

const (
	zramSwapSlabBytes = 16 << 20
	zramSwapMinBytes  = 256 << 20
	swapFlagPrefer    = 0x8000
	swapFlagPrioShift = 0
	zramSwapPriority  = 100
)

type zramSwapOps struct {
	readFile   func(path string) ([]byte, error)
	writeFile  func(path string, data []byte, perm os.FileMode) error
	stat       func(path string) (os.FileInfo, error)
	modprobe   func(module string) error
	formatSwap func(devPath string, diskSize int64, pageSize int) error
	swapon     func(devPath string, flags int) error
	pageSize   func() int
	waitForDev func(devPath string) error
}

var defaultZramSwapOps = zramSwapOps{
	readFile:  os.ReadFile,
	writeFile: os.WriteFile,
	stat:      os.Stat,
	modprobe: func(module string) error {
		bin, err := exec.LookPath("modprobe")
		if err != nil {
			return err
		}
		return exec.Command(bin, module).Run()
	},
	formatSwap: formatSwapDevice,
	swapon:     swaponSyscall,
	pageSize:   os.Getpagesize,
	waitForDev: func(devPath string) error {
		for range 100 {
			if _, err := os.Stat(devPath); err == nil {
				return nil
			}
			time.Sleep(10 * time.Millisecond)
		}
		return fmt.Errorf("%s did not appear", devPath)
	},
}

func swaponSyscall(devPath string, flags int) error {
	pathBytes, err := syscall.BytePtrFromString(devPath)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall(syscall.SYS_SWAPON, uintptr(unsafe.Pointer(pathBytes)), uintptr(flags), 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// buildLinuxSwapHeader builds a Linux swap_header v1 page for a device of
// diskSize bytes on a kernel with pageSize-byte pages (4 KiB or 16 KiB).
func buildLinuxSwapHeader(diskSize int64, pageSize int) ([]byte, error) {
	if pageSize < 4096 || pageSize&(pageSize-1) != 0 {
		return nil, fmt.Errorf("invalid page size %d", pageSize)
	}
	pages := diskSize / int64(pageSize)
	if pages < 10 {
		return nil, fmt.Errorf("swap device too small: %d bytes (%d pages)", diskSize, pages)
	}
	hdr := make([]byte, pageSize)
	// union swap_header.info starts at byte 1024 (after bootbits[1024]):
	// version (u32), last_page (u32), nr_badpages (u32), sws_uuid[16].
	binary.LittleEndian.PutUint32(hdr[1024:1028], 1)
	binary.LittleEndian.PutUint32(hdr[1028:1032], uint32(pages-1))
	binary.LittleEndian.PutUint32(hdr[1032:1036], 0)
	if _, err := rand.Read(hdr[1036:1052]); err != nil {
		return nil, err
	}
	hdr[1042] = (hdr[1042] & 0x0f) | 0x40 // UUID v4
	hdr[1044] = (hdr[1044] & 0x3f) | 0x80 // RFC 4122 variant
	copy(hdr[pageSize-10:pageSize], "SWAPSPACE2")
	return hdr, nil
}

func formatSwapDevice(devPath string, diskSize int64, pageSize int) error {
	hdr, err := buildLinuxSwapHeader(diskSize, pageSize)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(devPath, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(hdr); err != nil {
		return err
	}
	return f.Sync()
}

func parseMeminfoKB(raw, prefix string) int64 {
	for _, line := range strings.Split(raw, "\n") {
		rest, ok := strings.CutPrefix(line, prefix)
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0
		}
		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || n <= 0 {
			return 0
		}
		if len(fields) >= 2 && strings.EqualFold(fields[1], "kB") {
			return n * 1024
		}
		return n
	}
	return 0
}

// zramSwapSize returns the logical zram swap size for a guest with memTotal
// bytes of RAM: half of MemTotal rounded up to 16 MiB, at least 256 MiB
// (matching cladefsd --setup-host so SwapTotal comfortably exceeds its ~1/8
// governed swap floor even after swap-header overhead).
func zramSwapSize(memTotal int64) int64 {
	half := memTotal / 2
	aligned := ((half + zramSwapSlabBytes - 1) / zramSwapSlabBytes) * zramSwapSlabBytes
	if aligned < zramSwapMinBytes {
		return zramSwapMinBytes
	}
	return aligned
}

// configureGuestZramSwap provisions a zstd (fallback lz4) zram swap device
// when the guest boots with no swap. Normal workloads allocate zero zram until
// pressured, hotbed's own balloon-reclaim keeps swappiness=0 on zram-only
// guests to avoid double-compressing into host zram, and in-guest governed
// daemons (such as cladefsd) get SwapTotal >= floor without root setup.
func configureGuestZramSwap(log *slog.Logger) {
	if err := configureGuestZramSwapWithOps(log, defaultZramSwapOps); err != nil && log != nil {
		log.Warn("configure guest zram swap failed", "err", err)
	}
}

func configureGuestZramSwapWithOps(log *slog.Logger, ops zramSwapOps) error {
	meminfoRaw, err := ops.readFile("/proc/meminfo")
	if err != nil {
		return nil
	}
	meminfo := string(meminfoRaw)
	if parseMeminfoKB(meminfo, "SwapTotal:") > 0 {
		return nil
	}
	memTotal := parseMeminfoKB(meminfo, "MemTotal:")
	if memTotal <= 0 {
		return nil
	}

	const hotAddPath = "/sys/class/zram-control/hot_add"
	const hotRemovePath = "/sys/class/zram-control/hot_remove"
	if _, err := ops.stat(hotAddPath); err != nil {
		if err := ops.modprobe("zram"); err != nil {
			if log != nil {
				log.Debug("zram unavailable in guest kernel; skipping swap setup", "err", err)
			}
			return nil
		}
		if _, err := ops.stat(hotAddPath); err != nil {
			return nil
		}
	}

	devRaw, err := ops.readFile(hotAddPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", hotAddPath, err)
	}
	devNum, err := strconv.Atoi(strings.TrimSpace(string(devRaw)))
	if err != nil || devNum < 0 {
		return fmt.Errorf("invalid zram device number %q", strings.TrimSpace(string(devRaw)))
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = ops.writeFile(hotRemovePath, []byte(fmt.Sprintf("%d\n", devNum)), 0o644)
		}
	}()

	sysDev := fmt.Sprintf("/sys/block/zram%d", devNum)
	devPath := fmt.Sprintf("/dev/zram%d", devNum)
	compPath := filepath.Join(sysDev, "comp_algorithm")
	if err := ops.writeFile(compPath, []byte("zstd\n"), 0o644); err != nil {
		if errors.Is(err, syscall.EINVAL) {
			if err2 := ops.writeFile(compPath, []byte("lz4\n"), 0o644); err2 != nil {
				return fmt.Errorf("set %s to lz4: %w", compPath, err2)
			}
		} else {
			return fmt.Errorf("set %s to zstd: %w", compPath, err)
		}
	}

	diskSize := zramSwapSize(memTotal)
	sizePath := filepath.Join(sysDev, "disksize")
	if err := ops.writeFile(sizePath, []byte(fmt.Sprintf("%d\n", diskSize)), 0o644); err != nil {
		return fmt.Errorf("set %s to %d: %w", sizePath, diskSize, err)
	}
	if ops.waitForDev != nil {
		if err := ops.waitForDev(devPath); err != nil {
			return err
		}
	}
	if err := ops.formatSwap(devPath, diskSize, ops.pageSize()); err != nil {
		return fmt.Errorf("format swap %s: %w", devPath, err)
	}
	if err := ops.swapon(devPath, swapFlagPrefer|(zramSwapPriority<<swapFlagPrioShift)); err != nil {
		return fmt.Errorf("swapon %s: %w", devPath, err)
	}
	cleanup = false
	return nil
}

// runPID1Supervisor pivots onto the workspace rootfs (when booted from the
// separate read-only tools disk), mounts Linux API filesystems, forks
// hotbed-guestd as a child process (PID 2) with --no-pid1, and stays as PID 1
// solely to reap orphaned grandchildren. Separating PID 1 reaping from PID 2's
// exec.Cmd.Wait calls prevents wait4(-1) from stealing direct child exit
// statuses (ECHILD).
//
// Returns true if the supervisor ran to completion (caller should exit), or
// false if spawning the child failed and caller should fall back in-process.
func runPID1Supervisor(log *slog.Logger, nofile uint64) bool {
	pivoted, devMoved, err := pivotToWorkspaceRoot(log)
	if err != nil {
		log.Error("pid1 pivot to workspace root failed", "err", err)
		syscall.Sync()
		if os.Getpid() == 1 {
			_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
		}
		return true
	}
	if _, err := applyNofileLimit(log, nofile); err != nil {
		log.Warn("pid1 set RLIMIT_NOFILE failed", "limit", nofile, "err", err)
	}
	syncGuestClock(log)
	for _, m := range pid1Mounts {
		if m.target == "/dev" && devMoved {
			continue
		}
		if m.target == "/run" && pivoted {
			// /run tmpfs already hosts the pivoted /run/hotbed-tools mount;
			// overmounting it would hide the running hotbed-guestd binary.
			continue
		}
		_ = os.MkdirAll(m.target, 0o755)
		if err := syscall.Mount(m.src, m.target, m.fstype, m.flags, m.data); err != nil {
			if err != syscall.EBUSY {
				log.Warn("mount failed", "target", m.target, "fstype", m.fstype, "err", err)
			}
		}
	}
	prepareGuestTmpDirs(log, "/", true)
	ensureBootRuntimeDirs(log, "/etc/passwd", "/run")

	// Standard Linux /dev symlinks absent on bare devtmpfs (needed by bash
	// process substitution /dev/fd/N, /dev/stdin, /dev/stdout, /dev/stderr).
	devSymlinks := []struct{ target, link string }{
		{"/proc/self/fd", "/dev/fd"},
		{"/proc/self/fd/0", "/dev/stdin"},
		{"/proc/self/fd/1", "/dev/stdout"},
		{"/proc/self/fd/2", "/dev/stderr"},
	}
	for _, s := range devSymlinks {
		if _, err := os.Lstat(s.link); os.IsNotExist(err) {
			if err := os.Symlink(s.target, s.link); err != nil {
				log.Warn("symlink failed", "link", s.link, "target", s.target, "err", err)
			}
		}
	}
	// Bare devtmpfs creates /dev/fuse, /dev/userfaultfd, /dev/kvm, and
	// /dev/net/tun as 0600 root:root (no udev); open them to workspace users.
	fixDevPermissions(log, "/dev")
	configureGuestSysctls(log, "/proc")
	configureGuestMGLRU(log, "/sys")
	configureGuestZramSwap(log)
	// Ensure loopback is up (minimal Firecracker kernels leave lo DOWN).
	if ipBin, err := exec.LookPath("ip"); err == nil {
		_ = exec.Command(ipBin, "link", "set", "lo", "up").Run()
	}

	args := append([]string{"--no-pid1"}, os.Args[1:]...)
	cmd := exec.Command("/proc/self/exe", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Error("pid1 supervisor failed to spawn guestd child; falling back in-process", "err", err)
		return false
	}
	childPID := cmd.Process.Pid

	sigCh := make(chan os.Signal, 8)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for sig := range sigCh {
			_ = syscall.Kill(childPID, sig.(syscall.Signal))
		}
	}()

	// Reap all exiting processes (orphaned grandchildren AND our child PID 2).
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, 0, nil)
		if err == syscall.EINTR {
			continue
		}
		if pid == childPID || err == syscall.ECHILD {
			// Sync filesystems and cleanly restart/halt the Firecracker VM
			// instead of returning from PID 1 (which would kernel panic).
			syscall.Sync()
			if os.Getpid() == 1 {
				_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
			}
			return true
		}
	}
}

// syncGuestClock steps the guest's CLOCK_REALTIME to the host's HOTBED_NOW
// unix timestamp (passed via the kernel command line) when the guest clock is
// off by more than 5 seconds (e.g. VMMs without a hardware RTC, where guests
// boot at epoch 1970 or a stale time and TLS/apt signature checks fail).
func syncGuestClock(log *slog.Logger) {
	syncGuestClockWithOps(log, os.Getenv("HOTBED_NOW"), time.Now, func(sec int64) error {
		return syscall.Settimeofday(&syscall.Timeval{Sec: sec})
	}, os.Unsetenv)
}

func syncGuestClockWithOps(log *slog.Logger, raw string, now func() time.Time, setTime func(int64) error, unsetenv func(string) error) {
	_ = unsetenv("HOTBED_NOW")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	hostSec, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || hostSec <= 0 {
		if log != nil {
			log.Warn("ignore invalid HOTBED_NOW", "value", raw)
		}
		return
	}
	skew := now().Unix() - hostSec
	if skew >= -5 && skew <= 5 {
		return
	}
	if err := setTime(hostSec); err != nil && log != nil {
		log.Warn("sync guest clock from HOTBED_NOW failed", "host_sec", hostSec, "skew_s", skew, "err", err)
	}
}
