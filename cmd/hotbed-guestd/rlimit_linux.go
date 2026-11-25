//go:build linux

package main

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func readNrOpen() uint64 {
	b, err := os.ReadFile("/proc/sys/fs/nr_open")
	if err != nil {
		return 0
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// applyNofileLimit raises RLIMIT_NOFILE soft and hard limits to limit (clamped
// to /proc/sys/fs/nr_open, and to the current hard limit when non-root).
// Calling syscall.Setrlimit explicitly also clears Go 1.19+'s saved
// origRlimitNofile so child processes spawned via os/exec inherit the raised
// limits instead of reverting to the kernel PID 1 default (1024:4096).
func applyNofileLimit(log *slog.Logger, limit uint64) (uint64, error) {
	if limit == 0 {
		limit = defaultGuestNofile
	}
	if nrOpen := readNrOpen(); nrOpen > 0 && limit > nrOpen {
		limit = nrOpen
	}
	var cur syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &cur); err == nil {
		if os.Geteuid() != 0 && limit > cur.Max {
			limit = cur.Max
		}
	}
	rlim := syscall.Rlimit{Cur: limit, Max: limit}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rlim); err != nil {
		return 0, err
	}
	if log != nil {
		log.Info("configured RLIMIT_NOFILE", "soft", limit, "hard", limit)
	}
	return limit, nil
}
