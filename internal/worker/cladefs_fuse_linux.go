//go:build linux

package worker

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// clearMountNosuid clears MOUNT_ATTR_NOSUID on dir when it is mounted nosuid
// (e.g. a cladefsd host FUSE mount created via fusermount3), so setuid binaries
// such as sudo work inside chrooted workspaces and setup steps.
func clearMountNosuid(dir string) error {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return fmt.Errorf("statfs %s: %w", dir, err)
	}
	if st.Flags&unix.ST_NOSUID == 0 {
		return nil
	}
	if err := unix.MountSetattr(unix.AT_FDCWD, dir, 0, &unix.MountAttr{Attr_clr: unix.MOUNT_ATTR_NOSUID}); err != nil {
		return fmt.Errorf("clear nosuid on %s: %w", dir, err)
	}
	return nil
}
