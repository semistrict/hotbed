//go:build linux

package guestd

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
	// Enable memory and cpu controllers for child cgroups if available.
	_ = os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control", []byte("+cpu +memory +pids"), 0o644)
	_ = os.WriteFile(filepath.Join(guestCgroupRoot, "cgroup.subtree_control"), []byte("+cpu +memory +pids"), 0o644)
	return true
}

func createCmdCgroup(id string, req StartCommandRequest) *cmdCgroup {
	if !hasCgroupV2() {
		return nil
	}
	p := filepath.Join(guestCgroupRoot, id)
	if err := os.MkdirAll(p, 0o755); err != nil {
		return nil
	}
	if req.CPUWeight > 0 {
		w := min(max(req.CPUWeight, 1), 10000)
		_ = os.WriteFile(filepath.Join(p, "cpu.weight"), []byte(strconv.Itoa(w)), 0o644)
	}
	if req.MemoryLow > 0 {
		_ = os.WriteFile(filepath.Join(p, "memory.low"), []byte(strconv.FormatInt(req.MemoryLow, 10)), 0o644)
	}
	if req.MemoryHigh > 0 {
		_ = os.WriteFile(filepath.Join(p, "memory.high"), []byte(strconv.FormatInt(req.MemoryHigh, 10)), 0o644)
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

func (c *cmdCgroup) remove() {
	if c == nil {
		return
	}
	_ = os.Remove(c.path)
}

func (c *cmdCgroup) stats() (memAnon, memSwap, cpuUsec int64) {
	if c == nil {
		return 0, 0, 0
	}
	if f, err := os.Open(filepath.Join(c.path, "memory.stat")); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, ok := strings.Cut(sc.Text(), " ")
			if ok && k == "anon" {
				memAnon, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
				break
			}
		}
		_ = f.Close()
	}
	if b, err := os.ReadFile(filepath.Join(c.path, "memory.swap.current")); err == nil {
		memSwap, _ = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	}
	if f, err := os.Open(filepath.Join(c.path, "cpu.stat")); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, ok := strings.Cut(sc.Text(), " ")
			if ok && k == "usage_usec" {
				cpuUsec, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
				break
			}
		}
		_ = f.Close()
	}
	return memAnon, memSwap, cpuUsec
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
