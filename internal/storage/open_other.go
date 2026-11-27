//go:build !linux

package storage

import (
	"context"
	"fmt"
)

// Open is a store of no volumes in dir: off Linux (a macOS worker) every
// workspace is a VM's cladefs cell, never a btrfs or ZFS volume.
func Open(_ context.Context, dir, dataset string) (Store, error) {
	if dataset != "" {
		return nil, fmt.Errorf("storage: a ZFS dataset (%s) needs Linux", dataset)
	}
	return NewDir(dir), nil
}
