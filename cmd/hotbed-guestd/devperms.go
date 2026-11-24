package main

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
)

// guestDevNodes lists device nodes under /dev that bare devtmpfs creates as
// 0600 root:root (without udev), which unprivileged workspace commands need
// read/write access to for FUSE, userfaultfd, nested KVM, and TAP/TUN networking.
var guestDevNodes = []string{
	"fuse",
	"userfaultfd",
	"kvm",
	"net/tun",
}

// fixDevPermissions chmods guestDevNodes under devRoot to 0666 when present,
// silently skipping nodes that do not exist in the guest kernel.
func fixDevPermissions(log *slog.Logger, devRoot string) {
	for _, rel := range guestDevNodes {
		p := filepath.Join(devRoot, rel)
		if err := os.Chmod(p, 0o666); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if log != nil {
				log.Warn("chmod device node failed", "path", p, "err", err)
			}
		}
	}
}
