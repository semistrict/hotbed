//go:build linux

package main

import (
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// runPID1Supervisor mounts Linux API filesystems, forks hotbed-guestd as a
// child process (PID 2) with --no-pid1, and stays as PID 1 solely to reap
// orphaned grandchildren. Separating PID 1 reaping from PID 2's exec.Cmd.Wait
// calls prevents wait4(-1) from stealing direct child exit statuses (ECHILD).
//
// Returns true if the supervisor ran to completion (caller should exit), or
// false if spawning the child failed and caller should fall back in-process.
func runPID1Supervisor(log *slog.Logger) bool {
	mounts := []struct {
		src, target, fstype string
		flags               uintptr
		data                string
	}{
		{"proc", "/proc", "proc", 0, ""},
		{"sysfs", "/sys", "sysfs", 0, ""},
		{"devtmpfs", "/dev", "devtmpfs", 0, ""},
		{"devpts", "/dev/pts", "devpts", 0, "gid=5,mode=620,ptmxmode=666"},
		{"tmpfs", "/dev/shm", "tmpfs", 0, "mode=1777"},
		{"tmpfs", "/run", "tmpfs", 0, "mode=755"},
		{"tmpfs", "/tmp", "tmpfs", 0, "mode=1777"},
		{"cgroup2", "/sys/fs/cgroup", "cgroup2", 0, ""},
	}
	for _, m := range mounts {
		_ = os.MkdirAll(m.target, 0o755)
		if err := syscall.Mount(m.src, m.target, m.fstype, m.flags, m.data); err != nil {
			if err != syscall.EBUSY {
				log.Warn("mount failed", "target", m.target, "fstype", m.fstype, "err", err)
			}
		}
	}
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
