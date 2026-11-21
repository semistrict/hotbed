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
		return NewBtrfs(vols)
	case zfsSuperMagic:
		if dataset == "" {
			src, err := run(ctx, "findmnt", "-n", "-o", "SOURCE", "--target", dir)
			if err != nil {
				return nil, err
			}
			pool, _, _ := strings.Cut(strings.TrimSpace(src), "/")
			dataset = pool + "/hotbed"
		}
		return NewZFS(ctx, dataset, vols)
	default:
		return nil, fmt.Errorf("%s is on a filesystem hotbed cannot fork (type %#x); put the state directory on btrfs or zfs", dir, st.Type)
	}
}
