package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// ZFS keeps volumes as datasets under a root dataset and snapshots as
// dataset@snap. A snapshot's name is unique in the store, so the store finds
// it by name alone.
type ZFS struct {
	root  string // dataset, such as rpool/hotbed
	mount string // where root is mounted; volumes inherit mountpoints below it

	// cmdRun and flushVol are test hooks; nil uses package run and flush.
	cmdRun   func(ctx context.Context, name string, args ...string) (string, error)
	flushVol func(path string) error

	mu    sync.Mutex
	snaps map[string]string // snap name -> full dataset@snap
}

// NewZFS returns a store under dataset root, creating it mounted at mount if
// it does not exist. It changes nothing else about the pool.
func NewZFS(ctx context.Context, root, mount string) (*ZFS, error) {
	return newZFS(ctx, &ZFS{root: root, mount: mount})
}

// ErrPoolNotImported is NewZFS's error when the root dataset's pool is not
// imported: its disk is missing, or nothing has imported it yet. It may
// pass; a worker waits for it.
var ErrPoolNotImported = errors.New("ZFS pool not imported")

func newZFS(ctx context.Context, z *ZFS) (*ZFS, error) {
	root, mount := z.root, z.mount
	if _, err := z.exec(ctx, "zfs", "list", "-H", "-o", "name", root); err != nil {
		// Without its pool, the dataset cannot be made: say so, not what
		// zfs create says of it.
		pool, _, _ := strings.Cut(root, "/")
		if _, perr := z.exec(ctx, "zpool", "list", "-H", "-o", "name", pool); perr != nil {
			return nil, fmt.Errorf("%w: pool %s, of dataset %s, is not imported (is its disk attached and its namespace not 0 B? zpool import %s): %v", ErrPoolNotImported, pool, root, pool, perr)
		}
		if _, err := z.exec(ctx, "zfs", "create", "-p", "-o", "mountpoint="+mount, root); err != nil {
			return nil, err
		}
	}
	got, err := z.exec(ctx, "zfs", "get", "-H", "-o", "value", "mountpoint", root)
	if err != nil {
		return nil, err
	}
	if m := strings.TrimSpace(got); m != mount {
		return nil, fmt.Errorf("dataset %s is mounted at %s, not %s", root, m, mount)
	}
	return z, nil
}

func (z *ZFS) exec(ctx context.Context, name string, args ...string) (string, error) {
	if z.cmdRun != nil {
		return z.cmdRun(ctx, name, args...)
	}
	return run(ctx, name, args...)
}

func (z *ZFS) flush(path string) error {
	if z.flushVol != nil {
		return z.flushVol(path)
	}
	return flush(path)
}

func (z *ZFS) cacheSnap(snap, full string) {
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.snaps == nil {
		z.snaps = make(map[string]string)
	}
	z.snaps[snap] = full
}

func (z *ZFS) cachedSnap(snap string) (string, bool) {
	z.mu.Lock()
	defer z.mu.Unlock()
	full, ok := z.snaps[snap]
	return full, ok
}

func (z *ZFS) evictSnap(snap string) {
	z.mu.Lock()
	defer z.mu.Unlock()
	delete(z.snaps, snap)
}

func (z *ZFS) evictDatasetSnaps(ds string) {
	z.mu.Lock()
	defer z.mu.Unlock()
	snapPrefix := ds + "@"
	childPrefix := ds + "/"
	for k, full := range z.snaps {
		if strings.HasPrefix(full, snapPrefix) || strings.HasPrefix(full, childPrefix) {
			delete(z.snaps, k)
		}
	}
}

// guessSnapshotDataset derives the expected dataset@snap path for hotbed's
// standard snapshot naming conventions ("baseimg-<hash>" and "<project>-g<N>"),
// allowing cold-cache Clone and DestroySnapshot calls to succeed in a single
// zfs command without a recursive snapshot scan. Callers fall back to
// findSnapshot if the snapshot lives on another dataset (e.g. after promote).
func (z *ZFS) guessSnapshotDataset(snap string) (string, bool) {
	if strings.HasPrefix(snap, "baseimg-") {
		return z.dataset("baseimages/"+snap) + "@" + snap, true
	}
	if idx := strings.LastIndex(snap, "-g"); idx > 0 && idx+2 < len(snap) {
		gen := snap[idx+2:]
		allDigits := true
		for i := range len(gen) {
			if gen[i] < '0' || gen[i] > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			project := snap[:idx]
			return z.dataset("bases/"+project+"/g"+gen) + "@" + snap, true
		}
	}
	return "", false
}

var _ Pooled = (*ZFS)(nil)

func (z *ZFS) Kind() string { return "zfs" }

// Dataset is the dataset the volumes are under, such as rpool/hotbed.
func (z *ZFS) Dataset() string { return z.root }

func (z *ZFS) Path(name string) string { return filepath.Join(z.mount, name) }

func (z *ZFS) dataset(name string) string { return z.root + "/" + name }

func (z *ZFS) Create(ctx context.Context, name string) error {
	if err := checkVolume(name); err != nil {
		return err
	}
	ds := z.dataset(name)
	_, err := z.exec(ctx, "zfs", "create", ds)
	if err != nil && isParentNotFound(err) {
		if perr := z.ensureParent(ctx, ds); perr != nil {
			return perr
		}
		_, err = z.exec(ctx, "zfs", "create", ds)
	}
	if err != nil && isDatasetExists(err) {
		return fmt.Errorf("%s: %w", name, ErrExists)
	}
	return err
}

func (z *ZFS) ensureParent(ctx context.Context, ds string) error {
	idx := strings.LastIndex(ds, "/")
	if idx <= 0 {
		return fmt.Errorf("dataset %s has no parent", ds)
	}
	parent := ds[:idx]
	if !strings.HasPrefix(parent, z.root+"/") {
		return fmt.Errorf("parent dataset %s is outside store root %s", parent, z.root)
	}
	_, err := z.exec(ctx, "zfs", "create", "-p", parent)
	return err
}

func (z *ZFS) cloneOnce(ctx context.Context, full, target string) error {
	_, err := z.exec(ctx, "zfs", "clone", full, target)
	if err != nil && isParentNotFound(err) {
		if perr := z.ensureParent(ctx, target); perr != nil {
			return perr
		}
		_, err = z.exec(ctx, "zfs", "clone", full, target)
	}
	return err
}

func (z *ZFS) Snapshot(ctx context.Context, vol, snap string) error {
	if err := checkVolume(vol); err != nil {
		return err
	}
	if err := checkSnapshot(snap); err != nil {
		return err
	}
	ds := z.dataset(vol)
	if !z.exists(ctx, ds) {
		return fmt.Errorf("%s: %w", vol, ErrNotFound)
	}
	if full, ok := z.cachedSnap(snap); ok {
		if z.exists(ctx, full) {
			return fmt.Errorf("snapshot %s: %w", snap, ErrExists)
		}
		z.evictSnap(snap)
	}
	if _, err := z.findSnapshot(ctx, snap); err == nil {
		return fmt.Errorf("snapshot %s: %w", snap, ErrExists)
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	if err := z.flush(z.Path(vol)); err != nil {
		return err
	}
	full := ds + "@" + snap
	if _, err := z.exec(ctx, "zfs", "snapshot", full); err != nil {
		if isDatasetExists(err) {
			z.cacheSnap(snap, full)
			return fmt.Errorf("snapshot %s: %w", snap, ErrExists)
		}
		return err
	}
	z.cacheSnap(snap, full)
	return nil
}

func (z *ZFS) Clone(ctx context.Context, snap, name string) error {
	if err := checkSnapshot(snap); err != nil {
		return err
	}
	if err := checkVolume(name); err != nil {
		return err
	}
	target := z.dataset(name)

	full, fromCache := z.cachedSnap(snap)
	fromGuess := false
	if !fromCache {
		full, fromGuess = z.guessSnapshotDataset(snap)
	}
	if fromCache || fromGuess {
		err := z.cloneOnce(ctx, full, target)
		if err == nil {
			if fromGuess {
				z.cacheSnap(snap, full)
			}
			return nil
		}
		if isDatasetExists(err) {
			return fmt.Errorf("%s: %w", name, ErrExists)
		}
		if !isDatasetNotFound(err) {
			return err
		}
		if fromCache {
			z.evictSnap(snap)
		}
	}

	full, err := z.findSnapshot(ctx, snap)
	if err != nil {
		return err
	}
	if err := z.cloneOnce(ctx, full, target); err != nil {
		if isDatasetExists(err) {
			return fmt.Errorf("%s: %w", name, ErrExists)
		}
		if isDatasetNotFound(err) {
			z.evictSnap(snap)
			return fmt.Errorf("snapshot %s: %w", snap, ErrNotFound)
		}
		return err
	}
	return nil
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
	_, err := z.exec(ctx, "zfs", "destroy", "-r", ds)
	if err == nil {
		z.evictDatasetSnaps(ds)
	}
	return busy(err)
}

// promoteOneClone promotes a clone of one of ds's snapshots, if any has one.
func (z *ZFS) promoteOneClone(ctx context.Context, ds string) (bool, error) {
	out, err := z.exec(ctx, "zfs", "list", "-H", "-p", "-t", "snapshot", "-o", "name,clones", "-s", "createtxg", "-d", "1", ds)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 2 || f[1] == "" || f[1] == "-" {
			continue
		}
		clones := strings.Split(f[1], ",")
		if _, err := z.exec(ctx, "zfs", "promote", clones[len(clones)-1]); err != nil {
			return false, err
		}
		// Promoting moves one or more of ds's snapshots to the promoted clone,
		// so invalidate any cached paths rooted at ds.
		z.evictDatasetSnaps(ds)
		return true, nil
	}
	return false, nil
}

func (z *ZFS) DestroySnapshot(ctx context.Context, snap string) error {
	if err := checkSnapshot(snap); err != nil {
		return err
	}
	full, fromCache := z.cachedSnap(snap)
	fromGuess := false
	if !fromCache {
		full, fromGuess = z.guessSnapshotDataset(snap)
	}
	if fromCache || fromGuess {
		_, err := z.exec(ctx, "zfs", "destroy", full)
		if err == nil {
			z.evictSnap(snap)
			return nil
		}
		if isSnapshotInUse(err) {
			if fromGuess {
				z.cacheSnap(snap, full)
			}
			return fmt.Errorf("snapshot %s: %w", snap, ErrInUse)
		}
		if !isDatasetNotFound(err) {
			return busy(err)
		}
		if fromCache {
			z.evictSnap(snap)
		}
	}

	full, err := z.findSnapshot(ctx, snap)
	if err != nil {
		return err
	}
	if _, err := z.exec(ctx, "zfs", "destroy", full); err != nil {
		if isSnapshotInUse(err) {
			return fmt.Errorf("snapshot %s: %w", snap, ErrInUse)
		}
		if isDatasetNotFound(err) {
			z.evictSnap(snap)
			return fmt.Errorf("snapshot %s: %w", snap, ErrNotFound)
		}
		return busy(err)
	}
	z.evictSnap(snap)
	return nil
}

// ListVolumes returns relative leaf dataset names under z.root (excluding
// z.root itself and intermediate parent datasets that have child datasets).
func (z *ZFS) ListVolumes(ctx context.Context) ([]string, error) {
	out, err := z.exec(ctx, "zfs", "list", "-H", "-o", "name", "-t", "filesystem", "-r", z.root)
	if err != nil {
		if isDatasetNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	prefix := z.root + "/"
	var all []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if rel, ok := strings.CutPrefix(line, prefix); ok && rel != "" {
			all = append(all, rel)
		}
	}
	slices.Sort(all)
	// Exclude structural container datasets (which remain empty on ZFS after
	// their last child volume is destroyed) and intermediate parent datasets.
	var leaves []string
	for i, name := range all {
		if isContainerDataset(name) {
			continue
		}
		if i+1 < len(all) && strings.HasPrefix(all[i+1], name+"/") {
			continue
		}
		leaves = append(leaves, name)
	}
	return leaves, nil
}

// isContainerDataset reports whether rel is a structural parent dataset under
// z.root that holds volumes rather than being a volume itself.
func isContainerDataset(rel string) bool {
	switch rel {
	case "ws", "bases", "baseimages":
		return true
	}
	if rest, ok := strings.CutPrefix(rel, "bases/"); ok && !strings.Contains(rest, "/") {
		return true
	}
	return false
}

// ListSnapshots returns all snapshot names under z.root and refreshes the
// in-memory snapshot cache.
func (z *ZFS) ListSnapshots(ctx context.Context) ([]string, error) {
	out, err := z.exec(ctx, "zfs", "list", "-H", "-o", "name", "-t", "snapshot", "-r", z.root)
	if err != nil {
		if isDatasetNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	seen := make(map[string]bool)
	var snaps []string
	z.mu.Lock()
	if z.snaps == nil {
		z.snaps = make(map[string]string)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if _, name, ok := strings.Cut(line, "@"); ok && name != "" {
			z.snaps[name] = line
			if !seen[name] {
				seen[name] = true
				snaps = append(snaps, name)
			}
		}
	}
	z.mu.Unlock()
	slices.Sort(snaps)
	return snaps, nil
}

// Usage reads ZFS's accounting: referenced is everything the dataset's files
// hold; used is what destroying the dataset (with its snapshots) frees.
func (z *ZFS) Usage(ctx context.Context, name string) (Usage, error) {
	if err := checkVolume(name); err != nil {
		return Usage{}, err
	}
	out, err := z.exec(ctx, "zfs", "get", "-H", "-p", "-o", "value", "referenced,used", z.dataset(name))
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
	out, err := z.exec(ctx, "zfs", "get", "-H", "-p", "-o", "value", "used,available", z.root)
	if err != nil {
		return Space{}, err
	}
	v, err := parseInts(out, 2)
	if err != nil {
		return Space{}, fmt.Errorf("zfs get used,available %s: %w", z.root, err)
	}
	// A dataset's size is what it may grow to: what it holds plus what is free.
	pool, _, _ := strings.Cut(z.root, "/")
	pout, err := z.exec(ctx, "zpool", "list", "-H", "-p", "-o", "size", pool)
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
	_, err := z.exec(ctx, "zfs", "list", "-H", "-o", "name", ds)
	return err == nil
}

// findSnapshot returns the full name (dataset@snap) of the store's snapshot
// called snap, warming the in-memory cache with every snapshot under z.root.
func (z *ZFS) findSnapshot(ctx context.Context, snap string) (string, error) {
	out, err := z.exec(ctx, "zfs", "list", "-H", "-o", "name", "-t", "snapshot", "-r", z.root)
	if err != nil {
		return "", err
	}
	found := ""
	z.mu.Lock()
	if z.snaps == nil {
		z.snaps = make(map[string]string)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if _, name, ok := strings.Cut(line, "@"); ok && name != "" {
			z.snaps[name] = line
			if name == snap {
				found = line
			}
		}
	}
	if found == "" {
		delete(z.snaps, snap)
	}
	z.mu.Unlock()
	if found != "" {
		return found, nil
	}
	return "", fmt.Errorf("snapshot %s: %w", snap, ErrNotFound)
}

func isDatasetExists(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "dataset already exists") || strings.Contains(msg, "already exists")
}

func isParentNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "parent does not exist")
}

func isDatasetNotFound(err error) bool {
	if err == nil || isParentNotFound(err) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "dataset does not exist") ||
		strings.Contains(msg, "snapshot does not exist") ||
		strings.Contains(msg, "could not find any snapshots")
}

func isSnapshotInUse(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "dependent clones") || strings.Contains(msg, "has clones")
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
