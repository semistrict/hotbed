// Package storage keeps hotbed's bases and workspaces on a copy-on-write
// filesystem: writable volumes, read-only snapshots of them, and writable
// clones of snapshots, each made in constant time and sharing blocks with
// what it came from.
//
// hotbed only ever creates, snapshots, clones and destroys volumes under its
// own root; it never changes how the filesystem itself is set up.
package storage

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Store is one filesystem's volumes. Volume names are slash-separated paths
// ("bases/ratel/g42", "ws/ws-3f2a"); snapshot names are flat and unique in
// the store ("ratel-g42").
type Store interface {
	// Kind is "btrfs" or "zfs".
	Kind() string
	// Create makes an empty writable volume.
	Create(ctx context.Context, name string) error
	// Snapshot makes a read-only snapshot of volume vol.
	Snapshot(ctx context.Context, vol, snap string) error
	// Clone makes a writable volume from a snapshot.
	Clone(ctx context.Context, snap, name string) error
	// Destroy removes a volume and its snapshots. A snapshot that still has
	// clones keeps living in one of them.
	Destroy(ctx context.Context, name string) error
	// DestroySnapshot removes a snapshot; it fails with ErrInUse while clones of it exist.
	DestroySnapshot(ctx context.Context, snap string) error
	// ListVolumes returns the relative names of all leaf volumes in the store.
	ListVolumes(ctx context.Context) ([]string, error)
	// ListSnapshots returns the names of all snapshots in the store.
	ListSnapshots(ctx context.Context) ([]string, error)
	// Path is where a volume's files are.
	Path(name string) string
	// Usage is what a volume holds: in all, and alone.
	Usage(ctx context.Context, name string) (Usage, error)
	// Space is the filesystem's size and free space.
	Space(ctx context.Context) (Space, error)
}

// Pooled is a store whose volumes are datasets of a ZFS pool, which ZFS
// writes from threads of its own: no cgroup's IO cap reaches their writers.
type Pooled interface {
	Store
	// Dataset is the dataset the volumes are under, such as rpool/hotbed.
	Dataset() string
}

// Usage of one volume.
type Usage struct {
	// Referenced is every byte the volume's files hold, shared or not.
	Referenced int64
	// Exclusive is what only this volume holds: what deleting it frees.
	Exclusive int64
}

// Space of the filesystem a store is on.
type Space struct {
	Size int64
	Free int64
}

var (
	// ErrInUse is a snapshot that clones still depend on.
	ErrInUse = errors.New("snapshot has clones")
	// ErrBusy is a volume the kernel still holds mounted somewhere.
	ErrBusy = errors.New("dataset is busy")
	// ErrNotFound is a volume or snapshot that does not exist.
	ErrNotFound = errors.New("not found")
	// ErrExists is a volume or snapshot that already exists.
	ErrExists = errors.New("already exists")
)

// busy marks err ErrBusy when it says the kernel holds the volume busy: a
// mount of it, often in another mount namespace, that has not gone yet.
func busy(err error) error {
	if err == nil || errors.Is(err, ErrBusy) {
		return err
	}
	msg := err.Error()
	if strings.Contains(msg, "dataset is busy") || strings.Contains(msg, "Device or resource busy") {
		return fmt.Errorf("%w: %w", ErrBusy, err)
	}
	return err
}

var (
	volumeName   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(/[a-z0-9][a-z0-9._-]*)*$`)
	snapshotName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
)

func checkVolume(name string) error {
	if !volumeName.MatchString(name) {
		return fmt.Errorf("invalid volume name %q", name)
	}
	return nil
}

func checkSnapshot(name string) error {
	if !snapshotName.MatchString(name) {
		return fmt.Errorf("invalid snapshot name %q", name)
	}
	return nil
}
