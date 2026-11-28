// Package storage defines the Store interface and NoVolumes implementation
// used by hotbed workers. Production workers keep all workspaces and bases in
// cladefs cells via VMs and store no local copy-on-write volumes (NewDir).
package storage

import (
	"context"
	"errors"
)

// Store is a volume store interface (NoVolumes in production; fake.Store in tests).
type Store interface {
	// Kind identifies the store implementation ("none" in production, "fake" in tests).
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
