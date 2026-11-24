//go:build linux

package main

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func initPID1(log *slog.Logger) error {
	mounts := []struct {
		src, target, fstype string
		flags               uintptr
		data                string
	}{
		{"proc", "/proc", "proc", 0, ""},
		{"sysfs", "/sys", "sysfs", 0, ""},
		{"devtmpfs", "/dev", "devtmpfs", 0, ""},
		{"devpts", "/dev/pts", "devpts", 0, "gid=5,mode=620"},
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
	// Reap orphaned children when running as PID 1.
	ch := make(chan os.Signal, 16)
	signal.Notify(ch, syscall.SIGCHLD)
	go func() {
		for range ch {
			for {
				var ws syscall.WaitStatus
				pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
				if pid <= 0 || err != nil {
					break
				}
			}
		}
	}()
	return nil
}
