package main

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
)

// defaultGuestNofile is the default soft and hard RLIMIT_NOFILE applied inside
// Firecracker VMs. Linux's kernel default for PID 1 is 1024:4096, which is too
// low for tools like cladefsd (requires >= 35840 open files) and large parallel
// builds/tests.
const defaultGuestNofile uint64 = 65536

// resolveNofileLimit picks the effective RLIMIT_NOFILE target from explicit
// -nofile flag, kernel-propagated HOTBED_NOFILE environment variable, or
// defaultGuestNofile.
func resolveNofileLimit(flagVal uint64) uint64 {
	if flagVal > 0 {
		return flagVal
	}
	if raw := strings.TrimSpace(os.Getenv("HOTBED_NOFILE")); raw != "" {
		if v, err := strconv.ParseUint(raw, 10, 64); err == nil && v > 0 {
			return v
		}
	}
	return defaultGuestNofile
}

var _ = (*slog.Logger)(nil)
