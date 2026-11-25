//go:build linux

package guestd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	guestdv1 "github.com/semistrict/hotbed/internal/gen/guestd/v1"
)

const guestCgroupRoot = "/sys/fs/cgroup/hotbed"

type cmdCgroup struct {
	path string
}

// hasCgroupV2 reports whether cgroup v2 is mounted and writable for hotbed-guestd.
func hasCgroupV2() bool {
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return false
	}
	if err := os.MkdirAll(guestCgroupRoot, 0o755); err != nil {
		return false
	}
	// Enable memory, cpu, pids, and (best-effort) io controllers for child cgroups.
	_ = os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control", []byte("+cpu +memory +pids"), 0o644)
	_ = os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control", []byte("+io"), 0o644)
	_ = os.WriteFile(filepath.Join(guestCgroupRoot, "cgroup.subtree_control"), []byte("+cpu +memory +pids"), 0o644)
	_ = os.WriteFile(filepath.Join(guestCgroupRoot, "cgroup.subtree_control"), []byte("+io"), 0o644)
	return true
}

func createCmdCgroup(id string, req *guestdv1.StartCommandRequest) *cmdCgroup {
	if !hasCgroupV2() {
		return nil
	}
	p := filepath.Join(guestCgroupRoot, id)
	if err := os.MkdirAll(p, 0o755); err != nil {
		return nil
	}
	if w := int(req.GetCpuWeight()); w > 0 {
		w = min(max(w, 1), 10000)
		_ = os.WriteFile(filepath.Join(p, "cpu.weight"), []byte(strconv.Itoa(w)), 0o644)
	}
	if req.GetMemoryLow() > 0 {
		_ = os.WriteFile(filepath.Join(p, "memory.low"), []byte(strconv.FormatInt(req.GetMemoryLow(), 10)), 0o644)
	}
	if req.GetMemoryHigh() > 0 {
		_ = os.WriteFile(filepath.Join(p, "memory.high"), []byte(strconv.FormatInt(req.GetMemoryHigh(), 10)), 0o644)
	}
	return &cmdCgroup{path: p}
}

func (c *cmdCgroup) addPID(pid int) error {
	if c == nil {
		return nil
	}
	return os.WriteFile(filepath.Join(c.path, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644)
}

// attachCgroupAttr configures attr to spawn the child directly inside c via
// CLONE_INTO_CGROUP (Linux 5.7+), eliminating the start-to-addPID race.
// The caller must invoke the returned cleanup function after cmd.Start returns.
func attachCgroupAttr(attr *syscall.SysProcAttr, c *cmdCgroup) (bool, func()) {
	if c == nil || attr == nil {
		return false, func() {}
	}
	fd, err := syscall.Open(c.path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return false, func() {}
	}
	attr.UseCgroupFD = true
	attr.CgroupFD = fd
	return true, func() { _ = syscall.Close(fd) }
}

func (c *cmdCgroup) freeze() error {
	if c == nil {
		return errors.New("cgroup v2 unavailable")
	}
	return os.WriteFile(filepath.Join(c.path, "cgroup.freeze"), []byte("1"), 0o644)
}

func (c *cmdCgroup) thaw() error {
	if c == nil {
		return errors.New("cgroup v2 unavailable")
	}
	return os.WriteFile(filepath.Join(c.path, "cgroup.freeze"), []byte("0"), 0o644)
}

func (c *cmdCgroup) killAll() {
	if c == nil {
		return
	}
	_ = os.WriteFile(filepath.Join(c.path, "cgroup.kill"), []byte("1"), 0o644)
}

func guestSwapTotalBytes() int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	return parseMeminfoSwapTotal(string(b))
}

// writeCgroupReclaim writes payload to a cgroup v2 memory.reclaim file using raw
// syscalls. Never use os.WriteFile: kernfs cgroup files are pollable, so Go's
// runtime netpoller interprets memory.reclaim's EAGAIN (reclaimed less than
// asked) as "wait for EPOLLOUT and retry", spinning forever until client timeout.
func writeCgroupReclaim(path, payload string) error {
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) {
			return nil
		}
		return err
	}
	defer syscall.Close(fd)
	return writeCgroupReclaimFD(fd, payload)
}

func writeCgroupReclaimFD(fd int, payload string) error {
	buf := []byte(payload)
	var err error
	for {
		_, err = syscall.Write(fd, buf)
		if err != syscall.EINTR {
			break
		}
	}
	if err == nil || errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) {
		return nil
	}
	return err
}

func (c *cmdCgroup) reclaim(n int64) error {
	if c == nil || n <= 0 {
		return nil
	}
	payload, ok := planReclaim(c.path, n, guestSwapTotalBytes())
	if !ok {
		return nil
	}
	reclaimPath := filepath.Join(c.path, "memory.reclaim")
	err := writeCgroupReclaim(reclaimPath, payload)
	// Older kernels may reject "swappiness=0" on memory.reclaim with EINVAL;
	// fall back once to the plain byte count (still capped to reclaimable file
	// bytes and written via raw syscall so EAGAIN returns immediately).
	if errors.Is(err, syscall.EINVAL) {
		if fields := strings.Fields(payload); len(fields) > 1 {
			return writeCgroupReclaim(reclaimPath, fields[0])
		}
	}
	return err
}

func dropPageCaches() error {
	err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3\n"), 0o644)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS) {
		return nil
	}
	return err
}

func (c *cmdCgroup) remove() {
	if c == nil {
		return
	}
	_ = os.Remove(c.path)
}

func (c *cmdCgroup) stats() cmdCgroupStats {
	if c == nil {
		return cmdCgroupStats{}
	}
	return readCgroupStats(c.path)
}

// setCmdOOMScoreAdj marks a spawned command process as the preferred OOM victim
// inside the guest VM (matching host systemd OOMScoreAdjust=1000) so the guest
// OOM killer targets user commands before hotbed-guestd.
func setCmdOOMScoreAdj(pid, score int) {
	if pid <= 0 {
		return
	}
	_ = os.WriteFile(filepath.Join("/proc", strconv.Itoa(pid), "oom_score_adj"), []byte(strconv.Itoa(score)), 0o644)
}

func signalProcessGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	err := syscall.Kill(-pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func syncFilesystems() {
	syscall.Sync()
}

const (
	// Linux VFS freeze/thaw ioctls: _IOWR('X', 119, int) and _IOWR('X', 120, int).
	ioctlFIFREEZE = 0xc0045877
	ioctlFITHAW   = 0xc0045878
)

func freezeFilesystem(path string) error {
	if path == "" {
		path = "/"
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s for FIFREEZE: %w", path, err)
	}
	defer syscall.Close(fd)
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(ioctlFIFREEZE), 0)
	if errno != 0 && errno != syscall.EBUSY {
		return fmt.Errorf("FIFREEZE %s: %w", path, errno)
	}
	return nil
}

func thawFilesystem(path string) error {
	if path == "" {
		path = "/"
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s for FITHAW: %w", path, err)
	}
	defer syscall.Close(fd)
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(ioctlFITHAW), 0)
	if errno != 0 && errno != syscall.EINVAL {
		return fmt.Errorf("FITHAW %s: %w", path, errno)
	}
	return nil
}

func powerOffGuest() error {
	syscall.Sync()
	// Firecracker x86_64 exits cleanly with status 0 on keyboard-controller
	// reset (reboot=k -> LINUX_REBOOT_CMD_RESTART), whereas POWER_OFF halts
	// the vCPU without exiting the VMM.
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART); err != nil {
		if err2 := syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF); err2 != nil {
			return fmt.Errorf("reboot(RESTART/POWER_OFF): %w / %v", err, err2)
		}
	}
	return nil
}

func kernelRelease() string {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return ""
	}
	var b []byte
	for _, c := range u.Release {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}
