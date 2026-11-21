//go:build linux

package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Btrfs keeps volumes as subvolumes under root/vol and snapshots as
// read-only subvolumes under root/snap.
type Btrfs struct {
	root string
}

// NewBtrfs returns a store rooted at dir, which must be on btrfs.
func NewBtrfs(dir string) (*Btrfs, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return nil, fmt.Errorf("statfs %s: %w", dir, err)
	}
	if st.Type != unix.BTRFS_SUPER_MAGIC {
		return nil, fmt.Errorf("%s is not on btrfs", dir)
	}
	for _, d := range []string{"vol", "snap"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return nil, err
		}
	}
	return &Btrfs{root: dir}, nil
}

func (b *Btrfs) Kind() string { return "btrfs" }

func (b *Btrfs) Path(name string) string { return filepath.Join(b.root, "vol", name) }

func (b *Btrfs) snapPath(snap string) string { return filepath.Join(b.root, "snap", snap) }

func (b *Btrfs) Create(ctx context.Context, name string) error {
	if err := checkVolume(name); err != nil {
		return err
	}
	if err := b.absent(b.Path(name)); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(b.Path(name)), 0o755); err != nil {
		return err
	}
	_, err := run(ctx, "btrfs", "subvolume", "create", b.Path(name))
	return err
}

func (b *Btrfs) Snapshot(ctx context.Context, vol, snap string) error {
	if err := checkVolume(vol); err != nil {
		return err
	}
	if err := checkSnapshot(snap); err != nil {
		return err
	}
	if err := b.present(b.Path(vol)); err != nil {
		return err
	}
	if err := b.absent(b.snapPath(snap)); err != nil {
		return err
	}
	if err := flush(b.Path(vol)); err != nil {
		return err
	}
	_, err := run(ctx, "btrfs", "subvolume", "snapshot", "-r", b.Path(vol), b.snapPath(snap))
	return err
}

func (b *Btrfs) Clone(ctx context.Context, snap, name string) error {
	if err := checkSnapshot(snap); err != nil {
		return err
	}
	if err := checkVolume(name); err != nil {
		return err
	}
	if err := b.present(b.snapPath(snap)); err != nil {
		return err
	}
	if err := b.absent(b.Path(name)); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(b.Path(name)), 0o755); err != nil {
		return err
	}
	_, err := run(ctx, "btrfs", "subvolume", "snapshot", b.snapPath(snap), b.Path(name))
	return err
}

// Destroy deletes the volume's subvolume. On btrfs, snapshots and clones
// are independent subvolumes, so nothing else depends on it.
func (b *Btrfs) Destroy(ctx context.Context, name string) error {
	if err := checkVolume(name); err != nil {
		return err
	}
	if err := b.present(b.Path(name)); err != nil {
		return err
	}
	_, err := run(ctx, "btrfs", "subvolume", "delete", b.Path(name))
	return err
}

// DestroySnapshot deletes the snapshot. Clones on btrfs do not depend on the
// snapshot they came from, so it is never in use.
func (b *Btrfs) DestroySnapshot(ctx context.Context, snap string) error {
	if err := checkSnapshot(snap); err != nil {
		return err
	}
	if err := b.present(b.snapPath(snap)); err != nil {
		return err
	}
	_, err := run(ctx, "btrfs", "subvolume", "delete", b.snapPath(snap))
	return err
}

// Usage asks btrfs which of the volume's extents are shared. It walks the
// volume, so it takes time on a big tree; callers sample it, not poll it.
func (b *Btrfs) Usage(ctx context.Context, name string) (Usage, error) {
	if err := b.present(b.Path(name)); err != nil {
		return Usage{}, err
	}
	out, err := run(ctx, "btrfs", "filesystem", "du", "-s", "--raw", b.Path(name))
	if err != nil {
		return Usage{}, err
	}
	return parseBtrfsDu(out)
}

// parseBtrfsDu reads `btrfs filesystem du -s --raw`:
//
//	     Total   Exclusive  Set shared  Filename
//	1073741824     4096     1073737728  /path
func parseBtrfsDu(out string) (Usage, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return Usage{}, fmt.Errorf("btrfs filesystem du printed %q", out)
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 3 {
		return Usage{}, fmt.Errorf("btrfs filesystem du printed %q", out)
	}
	total, err1 := strconv.ParseInt(f[0], 10, 64)
	excl, err2 := strconv.ParseInt(f[1], 10, 64)
	if err := errors.Join(err1, err2); err != nil {
		return Usage{}, fmt.Errorf("parse btrfs filesystem du %q: %w", lines[len(lines)-1], err)
	}
	return Usage{Referenced: total, Exclusive: excl}, nil
}

func (b *Btrfs) Space(context.Context) (Space, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(b.root, &st); err != nil {
		return Space{}, fmt.Errorf("statfs %s: %w", b.root, err)
	}
	return Space{Size: int64(st.Blocks) * st.Bsize, Free: int64(st.Bavail) * st.Bsize}, nil
}

func (b *Btrfs) present(path string) error {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: %w", path, ErrNotFound)
	} else if err != nil {
		return err
	}
	return nil
}

func (b *Btrfs) absent(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%s: %w", path, ErrExists)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
