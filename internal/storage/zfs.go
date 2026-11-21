//go:build linux

package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// ZFS keeps volumes as datasets under a root dataset and snapshots as
// dataset@snap. A snapshot's name is unique in the store, so the store finds
// it by name alone.
type ZFS struct {
	root  string // dataset, such as rpool/hotbed
	mount string // where root is mounted; volumes inherit mountpoints below it
}

// NewZFS returns a store under dataset root, creating it mounted at mount if
// it does not exist. It changes nothing else about the pool.
func NewZFS(ctx context.Context, root, mount string) (*ZFS, error) {
	if _, err := run(ctx, "zfs", "list", "-H", "-o", "name", root); err != nil {
		if _, err := run(ctx, "zfs", "create", "-p", "-o", "mountpoint="+mount, root); err != nil {
			return nil, err
		}
	}
	got, err := run(ctx, "zfs", "get", "-H", "-o", "value", "mountpoint", root)
	if err != nil {
		return nil, err
	}
	if m := strings.TrimSpace(got); m != mount {
		return nil, fmt.Errorf("dataset %s is mounted at %s, not %s", root, m, mount)
	}
	return &ZFS{root: root, mount: mount}, nil
}

func (z *ZFS) Kind() string { return "zfs" }

func (z *ZFS) Path(name string) string { return filepath.Join(z.mount, name) }

func (z *ZFS) dataset(name string) string { return z.root + "/" + name }

func (z *ZFS) Create(ctx context.Context, name string) error {
	if err := checkVolume(name); err != nil {
		return err
	}
	if z.exists(ctx, z.dataset(name)) {
		return fmt.Errorf("%s: %w", name, ErrExists)
	}
	_, err := run(ctx, "zfs", "create", "-p", z.dataset(name))
	return err
}

func (z *ZFS) Snapshot(ctx context.Context, vol, snap string) error {
	if err := checkVolume(vol); err != nil {
		return err
	}
	if err := checkSnapshot(snap); err != nil {
		return err
	}
	if !z.exists(ctx, z.dataset(vol)) {
		return fmt.Errorf("%s: %w", vol, ErrNotFound)
	}
	if _, err := z.findSnapshot(ctx, snap); err == nil {
		return fmt.Errorf("snapshot %s: %w", snap, ErrExists)
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	_, err := run(ctx, "zfs", "snapshot", z.dataset(vol)+"@"+snap)
	return err
}

func (z *ZFS) Clone(ctx context.Context, snap, name string) error {
	if err := checkSnapshot(snap); err != nil {
		return err
	}
	if err := checkVolume(name); err != nil {
		return err
	}
	full, err := z.findSnapshot(ctx, snap)
	if err != nil {
		return err
	}
	if z.exists(ctx, z.dataset(name)) {
		return fmt.Errorf("%s: %w", name, ErrExists)
	}
	_, err = run(ctx, "zfs", "clone", "-p", full, z.dataset(name))
	return err
}

// Destroy removes a dataset and its snapshots. A snapshot that clones still
// use cannot go, so the newest clone is promoted first: the snapshot moves to
// it, and the dataset being destroyed no longer owns anything others need.
func (z *ZFS) Destroy(ctx context.Context, name string) error {
	if err := checkVolume(name); err != nil {
		return err
	}
	ds := z.dataset(name)
	if !z.exists(ctx, ds) {
		return fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	for {
		promoted, err := z.promoteOneClone(ctx, ds)
		if err != nil {
			return err
		}
		if !promoted {
			break
		}
	}
	_, err := run(ctx, "zfs", "destroy", "-r", ds)
	return err
}

// promoteOneClone promotes a clone of one of ds's snapshots, if any has one.
func (z *ZFS) promoteOneClone(ctx context.Context, ds string) (bool, error) {
	out, err := run(ctx, "zfs", "list", "-H", "-p", "-t", "snapshot", "-o", "name,clones", "-s", "createtxg", "-d", "1", ds)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 2 || f[1] == "" || f[1] == "-" {
			continue
		}
		clones := strings.Split(f[1], ",")
		if _, err := run(ctx, "zfs", "promote", clones[len(clones)-1]); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func (z *ZFS) DestroySnapshot(ctx context.Context, snap string) error {
	if err := checkSnapshot(snap); err != nil {
		return err
	}
	full, err := z.findSnapshot(ctx, snap)
	if err != nil {
		return err
	}
	clones, err := run(ctx, "zfs", "get", "-H", "-o", "value", "clones", full)
	if err != nil {
		return err
	}
	if c := strings.TrimSpace(clones); c != "" && c != "-" {
		return fmt.Errorf("snapshot %s (clones %s): %w", snap, c, ErrInUse)
	}
	_, err = run(ctx, "zfs", "destroy", full)
	return err
}

// Usage reads ZFS's accounting: referenced is everything the dataset's files
// hold; used is what destroying the dataset (with its snapshots) frees.
func (z *ZFS) Usage(ctx context.Context, name string) (Usage, error) {
	if err := checkVolume(name); err != nil {
		return Usage{}, err
	}
	out, err := run(ctx, "zfs", "get", "-H", "-p", "-o", "value", "referenced,used", z.dataset(name))
	if err != nil {
		if !z.exists(ctx, z.dataset(name)) {
			return Usage{}, fmt.Errorf("%s: %w", name, ErrNotFound)
		}
		return Usage{}, err
	}
	v, err := parseInts(out, 2)
	if err != nil {
		return Usage{}, fmt.Errorf("zfs get referenced,used %s: %w", name, err)
	}
	return Usage{Referenced: v[0], Exclusive: v[1]}, nil
}

func (z *ZFS) Space(ctx context.Context) (Space, error) {
	out, err := run(ctx, "zfs", "get", "-H", "-p", "-o", "value", "used,available", z.root)
	if err != nil {
		return Space{}, err
	}
	v, err := parseInts(out, 2)
	if err != nil {
		return Space{}, fmt.Errorf("zfs get used,available %s: %w", z.root, err)
	}
	// A dataset's size is what it may grow to: what it holds plus what is free.
	pool, _, _ := strings.Cut(z.root, "/")
	pout, err := run(ctx, "zpool", "list", "-H", "-p", "-o", "size", pool)
	if err != nil {
		return Space{}, err
	}
	size, err := parseInts(pout, 1)
	if err != nil {
		return Space{}, fmt.Errorf("zpool list %s: %w", pool, err)
	}
	return Space{Size: size[0], Free: v[1]}, nil
}

func (z *ZFS) exists(ctx context.Context, ds string) bool {
	_, err := run(ctx, "zfs", "list", "-H", "-o", "name", ds)
	return err == nil
}

// findSnapshot returns the full name (dataset@snap) of the store's snapshot
// called snap.
func (z *ZFS) findSnapshot(ctx context.Context, snap string) (string, error) {
	out, err := run(ctx, "zfs", "list", "-H", "-o", "name", "-t", "snapshot", "-r", z.root)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasSuffix(line, "@"+snap) {
			return line, nil
		}
	}
	return "", fmt.Errorf("snapshot %s: %w", snap, ErrNotFound)
}

func parseInts(out string, n int) ([]int64, error) {
	f := strings.Fields(out)
	if len(f) != n {
		return nil, fmt.Errorf("want %d values, got %q", n, out)
	}
	v := make([]int64, n)
	for i, s := range f {
		x, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, err
		}
		v[i] = x
	}
	return v, nil
}
