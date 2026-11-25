//go:build linux

package main

import (
	"bufio"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// runPID1Supervisor mounts Linux API filesystems, forks hotbed-guestd as a
// child process (PID 2) with --no-pid1, and stays as PID 1 solely to reap
// orphaned grandchildren. Separating PID 1 reaping from PID 2's exec.Cmd.Wait
// calls prevents wait4(-1) from stealing direct child exit statuses (ECHILD).
//
// Returns true if the supervisor ran to completion (caller should exit), or
// false if spawning the child failed and caller should fall back in-process.
func runPID1Supervisor(log *slog.Logger, nofile uint64) bool {
	if _, err := applyNofileLimit(log, nofile); err != nil {
		log.Warn("pid1 set RLIMIT_NOFILE failed", "limit", nofile, "err", err)
	}
	for _, m := range pid1Mounts {
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
