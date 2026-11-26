//go:build !linux

package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Open is a store of no volumes in dir: off Linux (a macOS worker) every
// workspace is a VM's cladefs cell, never a btrfs or ZFS volume.
func Open(_ context.Context, dir, dataset string) (Store, error) {
	if dataset != "" {
		return nil, fmt.Errorf("storage: a ZFS dataset (%s) needs Linux", dataset)
	}
	return NoVolumes{Root: filepath.Join(dir, "volumes")}, nil
}

// ErrNoVolumes is a NoVolumes store's answer to making a volume.
var ErrNoVolumes = errors.New("storage: this worker keeps no volumes (its workspaces are VMs' cells)")

// NoVolumes is the store of a worker whose workspaces are all VMs' cells: it
// holds no volumes and makes none, and names paths under Root only as the
// engine works out where a volume would be.
type NoVolumes struct{ Root string }

func (NoVolumes) Kind() string                                    { return "none" }
func (NoVolumes) Create(context.Context, string) error            { return ErrNoVolumes }
func (NoVolumes) Snapshot(context.Context, string, string) error  { return ErrNoVolumes }
func (NoVolumes) Clone(context.Context, string, string) error     { return ErrNoVolumes }
func (NoVolumes) Destroy(context.Context, string) error           { return nil }
func (NoVolumes) DestroySnapshot(context.Context, string) error   { return nil }
func (NoVolumes) ListVolumes(context.Context) ([]string, error)   { return nil, nil }
func (NoVolumes) ListSnapshots(context.Context) ([]string, error) { return nil, nil }
func (s NoVolumes) Path(name string) string                       { return filepath.Join(s.Root, name) }
func (NoVolumes) Usage(context.Context, string) (Usage, error)    { return Usage{}, nil }

// Space is the space of the filesystem Root's parent, the worker's
// directory, is on.
func (s NoVolumes) Space(context.Context) (Space, error) {
	var st unix.Statfs_t
	dir := filepath.Dir(s.Root)
	if err := unix.Statfs(dir, &st); err != nil {
		return Space{}, fmt.Errorf("statfs %s: %w", dir, err)
	}
	return Space{Size: int64(st.Blocks) * int64(st.Bsize), Free: int64(st.Bavail) * int64(st.Bsize)}, nil
}
