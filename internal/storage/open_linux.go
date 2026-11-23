//go:build linux

package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const zfsSuperMagic = 0x2fc12fc1

// Open returns the store for state directory dir, by the filesystem dir is
// on: btrfs keeps volumes in dir/volumes; zfs keeps them in a dataset
// <pool>/hotbed (or dataset, when given) mounted at dir/volumes.
//
// Volumes must allow setuid binaries (such as sudo inside chrooted rootfs
// containers), so Open clears any nosuid flag left on volumes from earlier
// runs.
func Open(ctx context.Context, dir, dataset string) (Store, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return nil, fmt.Errorf("statfs %s: %w", dir, err)
	}
	vols := filepath.Join(dir, "volumes")
	switch st.Type {
	case unix.BTRFS_SUPER_MAGIC:
		if err := ensureDir(vols); err != nil {
			return nil, err
		}
		b, err := NewBtrfs(vols)
		if err != nil {
			return nil, err
		}
		return b, clearNosuid(vols)
	case zfsSuperMagic:
		if dataset == "" {
			src, err := run(ctx, "findmnt", "-n", "-o", "SOURCE", "--target", dir)
			if err != nil {
				return nil, err
			}
			pool, _, _ := strings.Cut(strings.TrimSpace(src), "/")
			dataset = pool + "/hotbed"
		}
		z, err := NewZFS(ctx, dataset, vols)
		if err != nil {
			return nil, err
		}
		return z, z.setuidOn(ctx)
	default:
		return nil, fmt.Errorf("%s is on a filesystem hotbed cannot fork (type %#x); put the state directory on btrfs or zfs", dir, st.Type)
	}
}

// clearNosuid clears MOUNT_ATTR_NOSUID on dir if it is mounted nosuid (for
// example, a self-bind left by an earlier hotbed build before reboot).
func clearNosuid(dir string) error {
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
