//go:build linux

package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// setuidOn ensures the root dataset allows setuid binaries (so sudo works
// inside chrooted workspaces) and remounts any existing child dataset that was
// mounted nosuid (e.g. from an earlier hotbed build).
func (z *ZFS) setuidOn(ctx context.Context) error {
	got, err := z.exec(ctx, "zfs", "get", "-H", "-o", "value", "setuid", z.root)
	if err != nil {
		return err
	}
	if strings.TrimSpace(got) != "on" {
		if _, err := z.exec(ctx, "zfs", "set", "setuid=on", z.root); err != nil {
			return err
		}
	}
	out, err := z.exec(ctx, "zfs", "list", "-H", "-r", "-t", "filesystem", "-o", "name,mounted,mountpoint", z.root)
	if err != nil {
		return err
	}
	for line := range strings.Lines(out) {
		f := strings.Split(strings.TrimRight(line, "\n"), "\t")
		if len(f) != 3 || f[1] != "yes" || !filepath.IsAbs(f[2]) {
			continue
		}
		var st unix.Statfs_t
		if err := unix.Statfs(f[2], &st); err != nil {
			return fmt.Errorf("statfs %s: %w", f[2], err)
		}
		if st.Flags&unix.ST_NOSUID == 0 {
			continue
		}
		if _, err := z.exec(ctx, "zfs", "mount", "-o", "remount,suid", f[0]); err != nil {
			return err
		}
	}
	return nil
}
