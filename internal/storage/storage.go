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
	// Path is where a volume's files are.
	Path(name string) string
	// Usage is what a volume holds: in all, and alone.
	Usage(ctx context.Context, name string) (Usage, error)
	// Space is the filesystem's size and free space.
	Space(ctx context.Context) (Space, error)
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
	// ErrNotFound is a volume or snapshot that does not exist.
	ErrNotFound = errors.New("not found")
	// ErrExists is a volume or snapshot that already exists.
	ErrExists = errors.New("already exists")
)

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
