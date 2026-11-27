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
	"time"

	guestdv1 "github.com/semistrict/hotbed/internal/gen/guestd/v1"
)

const guestCgroupRoot = "/sys/fs/cgroup/hotbed"

type cmdCgroup struct {
	// path is the command's delegated root cgroup (/sys/fs/cgroup/hotbed/<id>):
	// hierarchical stats, freeze, kill, reclaim, and limits live here.
	path string
	// leaf is the sub-cgroup (/sys/fs/cgroup/hotbed/<id>/main) the command's
	// initial process starts in so path has no internal processes and can
	// delegate +cpu +memory +pids +io to user-created sibling cgroups.
	leaf string
}

// cgroupRootDelegatedFiles are chowned on the command's root cgroup directory
// (matching systemd Delegate=yes, plus memory.reclaim) while leaving root's
// memory.low/memory.high/cpu.weight limits intact.
var cgroupRootDelegatedFiles = []string{
	"cgroup.procs",
	"cgroup.threads",
	"cgroup.subtree_control",
	"cgroup.type",
	"memory.reclaim",
}

// cgroupLeafDelegatedFiles are chowned on the command's leaf cgroup directory
// so the command user can govern and reclaim its own cgroup as well as any
// sibling/child cgroups it creates under path.
var cgroupLeafDelegatedFiles = []string{
	"cgroup.procs",
	"cgroup.threads",
	"cgroup.subtree_control",
	"cgroup.type",
	"memory.reclaim",
	"memory.high",
	"memory.max",
	"memory.low",
	"memory.min",
	"memory.swap.max",
	"memory.swap.high",
	"memory.oom.group",
	"cpu.weight",
	"cpu.max",
	"pids.max",
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
	// Enable controllers on p before placing any process in a child cgroup so
	// cgroup v2's no-internal-processes rule allows delegated child cgroups
	// (e.g. cladefsd's governed daemon + simulated VM cgroups) to use memory.*.
	_ = os.WriteFile(filepath.Join(p, "cgroup.subtree_control"), []byte("+cpu +memory +pids"), 0o644)
	_ = os.WriteFile(filepath.Join(p, "cgroup.subtree_control"), []byte("+io"), 0o644)

	leaf := filepath.Join(p, "main")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		leaf = p
	}
	if (req.GetUid() > 0 || req.GetGid() > 0) && os.Geteuid() == 0 {
		delegateCgroup(p, leaf, int(req.GetUid()), int(req.GetGid()), os.Chown)
	}
	return &cmdCgroup{path: p, leaf: leaf}
}

func delegateCgroup(rootDir, leafDir string, uid, gid int, chown func(string, int, int) error) {
	_ = chown(rootDir, uid, gid)
	for _, name := range cgroupRootDelegatedFiles {
		_ = chown(filepath.Join(rootDir, name), uid, gid)
	}
	if leafDir == "" || leafDir == rootDir {
		return
	}
	_ = chown(leafDir, uid, gid)
	for _, name := range cgroupLeafDelegatedFiles {
		_ = chown(filepath.Join(leafDir, name), uid, gid)
	}
}

func (c *cmdCgroup) attachDir() string {
	if c == nil {
		return ""
	}
	if c.leaf != "" {
		return c.leaf
	}
	return c.path
}

func (c *cmdCgroup) addPID(pid int) error {
	if c == nil {
		return nil
	}
	return os.WriteFile(filepath.Join(c.attachDir(), "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644)
}

// attachCgroupAttr configures attr to spawn the child directly inside c's leaf
// cgroup via CLONE_INTO_CGROUP (Linux 5.7+), eliminating the start-to-addPID race.
// The caller must invoke the returned cleanup function after cmd.Start returns.
func attachCgroupAttr(attr *syscall.SysProcAttr, c *cmdCgroup) (bool, func()) {
	if c == nil || attr == nil {
		return false, func() {}
	}
	fd, err := syscall.Open(c.attachDir(), syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
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

func guestDiskSwapTotalBytes() int64 {
	b, err := os.ReadFile("/proc/swaps")
	if err != nil {
		return 0
	}
	return parseSwapsDiskBytes(string(b))
}

func guestCleanFileBytes() int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	return parseMeminfoCleanFileBytes(string(b))
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
	payload, ok := planReclaim(c.path, n, guestDiskSwapTotalBytes())
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

func writeProcVM(path, val string) error {
	err := os.WriteFile(path, []byte(val), 0o644)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS) {
		return nil
	}
	return err
}

func dropPageCaches() error {
	return writeProcVM("/proc/sys/vm/drop_caches", "3\n")
}

func compactMemory() error {
	return writeProcVM("/proc/sys/vm/compact_memory", "1\n")
}

func (c *cmdCgroup) remove() {
	if c == nil {
		return
	}
	// Remove child cgroups (main + any delegated child cgroups created by the
	// command) bottom-up; retry briefly while cgroup.kill reaps exiting tasks.
	for i := range 10 {
		if err := removeCgroupTree(c.path); err == nil || errors.Is(err, os.ErrNotExist) {
			return
		}
		if i < 9 {
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// removeCgroupTree removes a cgroup v2 directory hierarchy bottom-up using
// os.Remove on directories only (kernfs control files cannot be unlinked).
func removeCgroupTree(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var firstErr error
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		if err := removeCgroupTree(filepath.Join(root, ent.Name())); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := os.Remove(root); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
		firstErr = err
	}
	return firstErr
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

func syncFilesystems() {
	syscall.Sync()
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
