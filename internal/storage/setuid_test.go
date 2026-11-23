//go:build linux && systemdtest

// Run as root: sudo go test -tags systemdtest ./internal/storage/
package storage

import (
	"testing"

	"golang.org/x/sys/unix"
)

// clearNosuid clears MOUNT_ATTR_NOSUID on a directory that was bind-mounted
// nosuid (e.g. by an earlier hotbed build), so setuid binaries like sudo work
// inside chrooted workspaces.
func TestClearNosuid(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Mount(dir, dir, "", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Unmount(dir, unix.MNT_DETACH) })
	if err := unix.MountSetattr(unix.AT_FDCWD, dir, 0, &unix.MountAttr{Attr_set: unix.MOUNT_ATTR_NOSUID}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := clearNosuid(dir); err != nil {
			t.Fatal(err)
		}
	}
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		t.Fatal(err)
	}
	if st.Flags&unix.ST_NOSUID != 0 {
		t.Errorf("%s is still nosuid: flags %#x", dir, st.Flags)
	}
}
