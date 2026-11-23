package storage

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeZFS simulates zfs CLI semantics in memory and records every invocation
// so tests can assert exact command counts and cache/fallback behavior.
type fakeZFS struct {
	mu       sync.Mutex
	datasets map[string]bool            // full dataset name -> exists
	snaps    map[string]map[string]bool // full dataset@snap -> clone datasets
	calls    []string
}

func newFakeZFS(root string) *fakeZFS {
	return &fakeZFS{
		datasets: map[string]bool{root: true},
		snaps:    map[string]map[string]bool{},
	}
}

func (f *fakeZFS) resetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

func (f *fakeZFS) getCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeZFS) run(_ context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if name != "zfs" || len(args) == 0 {
		return "", fmt.Errorf("unexpected command %s %v", name, args)
	}
	switch args[0] {
	case "create":
		// OpenZFS zfs-create(8): with -p, creating an already-existing dataset
		// succeeds silently; without -p, existing target fails with
		// "dataset already exists" and missing parent fails with
		// "parent does not exist".
		createParents := slices.Contains(args, "-p")
		ds := args[len(args)-1]
		if f.datasets[ds] {
			if createParents {
				return "", nil
			}
			return "", fmt.Errorf("zfs create: exit status 1: cannot create '%s': dataset already exists", ds)
		}
		if createParents {
			f.createParentsLocked(ds)
		} else if parent, ok := parentDataset(ds); ok && !f.datasets[parent] {
			return "", fmt.Errorf("zfs create: exit status 1: cannot create '%s': parent does not exist", ds)
		}
		f.datasets[ds] = true
		return "", nil
	case "snapshot":
		full := args[len(args)-1]
		ds, _, ok := strings.Cut(full, "@")
		if !ok || !f.datasets[ds] {
			return "", fmt.Errorf("zfs snapshot: exit status 1: cannot open '%s': dataset does not exist", ds)
		}
		if _, exists := f.snaps[full]; exists {
			return "", fmt.Errorf("zfs snapshot: exit status 1: cannot create snapshot '%s': dataset already exists", full)
		}
		f.snaps[full] = map[string]bool{}
		return "", nil
	case "clone":
		// OpenZFS zfs-clone(8): with -p, cloning onto an already-existing
		// target succeeds silently; without -p, existing target fails with
		// "dataset already exists" and missing parent fails with
		// "parent does not exist".
		createParents := slices.Contains(args, "-p")
		fullSnap := args[len(args)-2]
		target := args[len(args)-1]
		clones, ok := f.snaps[fullSnap]
		if !ok {
			return "", fmt.Errorf("zfs clone: exit status 1: cannot open '%s': dataset does not exist", fullSnap)
		}
		if f.datasets[target] {
			if createParents {
				return "", nil
			}
			return "", fmt.Errorf("zfs clone: exit status 1: cannot create '%s': dataset already exists", target)
		}
		if createParents {
			f.createParentsLocked(target)
		} else if parent, ok := parentDataset(target); ok && !f.datasets[parent] {
			return "", fmt.Errorf("zfs clone: exit status 1: cannot create '%s': parent does not exist", target)
		}
		f.datasets[target] = true
		clones[target] = true
		return "", nil
	case "promote":
		cloneDS := args[len(args)-1]
		if !f.datasets[cloneDS] {
			return "", fmt.Errorf("zfs promote: exit status 1: cannot open '%s': dataset does not exist", cloneDS)
		}
		// Move all snapshots whose clones include cloneDS onto cloneDS.
		for full, clones := range f.snaps {
			if !clones[cloneDS] {
				continue
			}
			oldDS, snapName, _ := strings.Cut(full, "@")
			newClones := map[string]bool{oldDS: true}
			for c := range clones {
				if c != cloneDS {
					newClones[c] = true
				}
			}
			delete(f.snaps, full)
			f.snaps[cloneDS+"@"+snapName] = newClones
		}
		return "", nil
	case "destroy":
		recursive := slices.Contains(args, "-r")
		target := args[len(args)-1]
		if strings.Contains(target, "@") {
			clones, ok := f.snaps[target]
			if !ok {
				return "", fmt.Errorf("zfs destroy: exit status 1: could not find any snapshots to destroy; check snapshot names.")
			}
			if len(clones) > 0 {
				return "", fmt.Errorf("zfs destroy: exit status 1: cannot destroy '%s': snapshot has dependent clones", target)
			}
			delete(f.snaps, target)
			return "", nil
		}
		if !f.datasets[target] {
			return "", fmt.Errorf("zfs destroy: exit status 1: cannot open '%s': dataset does not exist", target)
		}
		// Ensure no remaining snapshot under target has clones.
		for full, clones := range f.snaps {
			if (strings.HasPrefix(full, target+"@") || (recursive && strings.HasPrefix(full, target+"/"))) && len(clones) > 0 {
				return "", fmt.Errorf("zfs destroy: exit status 1: cannot destroy '%s': snapshot has dependent clones", full)
			}
		}
		for ds := range f.datasets {
			if ds == target || (recursive && strings.HasPrefix(ds, target+"/")) {
				delete(f.datasets, ds)
				for _, clones := range f.snaps {
					delete(clones, ds)
				}
			}
		}
		for full := range f.snaps {
			if strings.HasPrefix(full, target+"@") || (recursive && strings.HasPrefix(full, target+"/")) {
				delete(f.snaps, full)
			}
		}
		return "", nil
	case "list":
		target := args[len(args)-1]
		isSnap := slices.Contains(args, "snapshot")
		isFS := slices.Contains(args, "filesystem")
		recursive := slices.Contains(args, "-r")
		withClones := slices.Contains(args, "name,clones")
		if isSnap {
			var lines []string
			for full, clones := range f.snaps {
				ds, _, _ := strings.Cut(full, "@")
				match := ds == target || (recursive && strings.HasPrefix(ds, target+"/"))
				if !match {
					continue
				}
				if withClones {
					cList := make([]string, 0, len(clones))
					for c := range clones {
						cList = append(cList, c)
					}
					slices.Sort(cList)
					col := "-"
					if len(cList) > 0 {
						col = strings.Join(cList, ",")
					}
					lines = append(lines, full+"\t"+col)
				} else {
					lines = append(lines, full)
				}
			}
			slices.Sort(lines)
			return strings.Join(lines, "\n") + "\n", nil
		}
		if isFS || recursive {
			if !f.datasets[target] {
				return "", fmt.Errorf("zfs list: exit status 1: cannot open '%s': dataset does not exist", target)
			}
			var lines []string
			for ds := range f.datasets {
				if ds == target || (recursive && strings.HasPrefix(ds, target+"/")) {
					lines = append(lines, ds)
				}
			}
			slices.Sort(lines)
			return strings.Join(lines, "\n") + "\n", nil
		}
		// Single dataset or snapshot existence check.
		if strings.Contains(target, "@") {
			if _, ok := f.snaps[target]; ok {
				return target + "\n", nil
			}
		} else if f.datasets[target] {
			return target + "\n", nil
		}
		return "", fmt.Errorf("zfs list: exit status 1: cannot open '%s': dataset does not exist", target)
	default:
		return "", fmt.Errorf("unhandled zfs subcommand %q", args[0])
	}
}

func parentDataset(ds string) (string, bool) {
	if idx := strings.LastIndex(ds, "/"); idx > 0 {
		return ds[:idx], true
	}
	return "", false
}

func (f *fakeZFS) createParentsLocked(ds string) {
	parts := strings.Split(ds, "/")
	for i := 1; i < len(parts); i++ {
		f.datasets[strings.Join(parts[:i], "/")] = true
	}
}

func newTestZFS(root string) (*ZFS, *fakeZFS) {
	fz := newFakeZFS(root)
	z := &ZFS{
		root:     root,
		mount:    "/mnt/hotbed",
		cmdRun:   fz.run,
		flushVol: func(string) error { return nil },
	}
	return z, fz
}

// Create and Clone omit `-p` so duplicate targets return ErrExists (OpenZFS
// `-p` silently succeeds on existing targets). Missing parents are created
// on demand with `zfs create -p <parent>` (3 commands first time, 1 thereafter),
// and cloning a cached or standard snapshot runs with zero `zfs list` scans
// (board 7905).
func TestZFSCreateAndCloneParentCreationAndCache(t *testing.T) {
	ctx := t.Context()
	z, fz := newTestZFS("rpool/hotbed")

	// First Create under missing parent takes 3 commands: create (parent missing),
	// create -p <parent>, retry create without -p.
	fz.resetCalls()
	if err := z.Create(ctx, "bases/app/g1"); err != nil {
		t.Fatal(err)
	}
	wantCreateMissing := []string{
		"zfs create rpool/hotbed/bases/app/g1",
		"zfs create -p rpool/hotbed/bases/app",
		"zfs create rpool/hotbed/bases/app/g1",
	}
	if calls := fz.getCalls(); !slices.Equal(calls, wantCreateMissing) {
		t.Fatalf("Create missing-parent calls = %q, want %q", calls, wantCreateMissing)
	}

	// Second Create under existing parent takes 1 command.
	fz.resetCalls()
	if err := z.Create(ctx, "bases/app/g2"); err != nil {
		t.Fatal(err)
	}
	if calls := fz.getCalls(); len(calls) != 1 || calls[0] != "zfs create rpool/hotbed/bases/app/g2" {
		t.Fatalf("Create existing-parent calls = %q, want 1 create command", calls)
	}

	// Duplicate Create must fail with ErrExists in 1 command.
	fz.resetCalls()
	if err := z.Create(ctx, "bases/app/g1"); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate Create err = %v, want ErrExists", err)
	}
	if calls := fz.getCalls(); len(calls) != 1 {
		t.Fatalf("duplicate Create calls = %q, want 1", calls)
	}

	if err := z.Snapshot(ctx, "bases/app/g1", "app-g1"); err != nil {
		t.Fatal(err)
	}

	// Warm cache hit when target parent ("ws") does not exist yet: 3 commands
	// (clone -> parent missing, create -p ws, clone without -p).
	fz.resetCalls()
	if err := z.Clone(ctx, "app-g1", "ws/ws-1"); err != nil {
		t.Fatalf("Clone warm hit missing parent: %v", err)
	}
	wantCloneMissing := []string{
		"zfs clone rpool/hotbed/bases/app/g1@app-g1 rpool/hotbed/ws/ws-1",
		"zfs create -p rpool/hotbed/ws",
		"zfs clone rpool/hotbed/bases/app/g1@app-g1 rpool/hotbed/ws/ws-1",
	}
	if calls := fz.getCalls(); !slices.Equal(calls, wantCloneMissing) {
		t.Fatalf("warm hit Clone missing-parent calls = %q, want %q", calls, wantCloneMissing)
	}

	// Warm cache hit when target parent exists: exactly 1 command (no -p).
	fz.resetCalls()
	if err := z.Clone(ctx, "app-g1", "ws/ws-2"); err != nil {
		t.Fatalf("Clone warm hit existing parent: %v", err)
	}
	if calls := fz.getCalls(); len(calls) != 1 || calls[0] != "zfs clone rpool/hotbed/bases/app/g1@app-g1 rpool/hotbed/ws/ws-2" {
		t.Fatalf("warm hit Clone existing-parent calls = %q, want 1 clone command", calls)
	}

	// Duplicate Clone target must fail with ErrExists in 1 command.
	fz.resetCalls()
	if err := z.Clone(ctx, "app-g1", "ws/ws-1"); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate Clone err = %v, want ErrExists", err)
	}
	if calls := fz.getCalls(); len(calls) != 1 {
		t.Fatalf("duplicate Clone calls = %q, want 1", calls)
	}

	// Cold cache (fresh ZFS struct, as after worker restart) for standard
	// snapshot names ("<project>-g<N>" and "baseimg-<hash>") runs in 1 command
	// once parent exists, and 3 commands on first missing parent.
	cold := &ZFS{root: "rpool/hotbed", mount: "/mnt/hotbed", cmdRun: fz.run, flushVol: func(string) error { return nil }}
	fz.resetCalls()
	if err := cold.Clone(ctx, "app-g1", "ws/ws-3"); err != nil {
		t.Fatalf("cold Clone base snap: %v", err)
	}
	if calls := fz.getCalls(); len(calls) != 1 || calls[0] != "zfs clone rpool/hotbed/bases/app/g1@app-g1 rpool/hotbed/ws/ws-3" {
		t.Fatalf("cold Clone base snap calls = %q, want 1", calls)
	}

	// Base image snapshot on cold cache: missing parent ("bases/other") takes 3,
	// subsequent clone under same parent takes 1.
	if err := z.Create(ctx, "baseimages/baseimg-abc123def456"); err != nil {
		t.Fatal(err)
	}
	if err := z.Snapshot(ctx, "baseimages/baseimg-abc123def456", "baseimg-abc123def456"); err != nil {
		t.Fatal(err)
	}
	fz.resetCalls()
	if err := cold.Clone(ctx, "baseimg-abc123def456", "bases/other/g1"); err != nil {
		t.Fatalf("cold Clone baseimg snap (missing parent): %v", err)
	}
	if calls := fz.getCalls(); len(calls) != 3 {
		t.Fatalf("cold Clone baseimg missing-parent calls = %q, want 3", calls)
	}
	fz.resetCalls()
	if err := cold.Clone(ctx, "baseimg-abc123def456", "bases/other/g2"); err != nil {
		t.Fatalf("cold Clone baseimg snap (existing parent): %v", err)
	}
	if calls := fz.getCalls(); len(calls) != 1 {
		t.Fatalf("cold Clone baseimg existing-parent calls = %q, want 1", calls)
	}

	// Sanity-check error classification: "parent does not exist" must never be
	// misclassified as dataset/snapshot not found.
	parentErr := errors.New("zfs clone: exit status 1: cannot create 'rpool/hotbed/ws/x': parent does not exist")
	if !isParentNotFound(parentErr) {
		t.Error("isParentNotFound returned false for parent-missing error")
	}
	if isDatasetNotFound(parentErr) {
		t.Error("isDatasetNotFound misclassified parent-missing error as dataset not found")
	}
}

// When a cached or guessed snapshot path becomes stale (e.g. after a base
// dataset is destroyed and ZFS promotes a clone so the snapshot moves to the
// clone), Clone and DestroySnapshot fall back to findSnapshot, refresh the
// cache, and subsequent calls are back to 1 command.
func TestZFSStaleCacheAndPromotionFallback(t *testing.T) {
	ctx := t.Context()
	z, fz := newTestZFS("rpool/hotbed")

	if err := z.Create(ctx, "bases/app/g1"); err != nil {
		t.Fatal(err)
	}
	if err := z.Snapshot(ctx, "bases/app/g1", "app-g1"); err != nil {
		t.Fatal(err)
	}
	if err := z.Clone(ctx, "app-g1", "ws/ws-1"); err != nil {
		t.Fatal(err)
	}

	// Destroying bases/app/g1 promotes ws/ws-1, moving app-g1 to ws/ws-1@app-g1
	// and evicting the old cache entry.
	if err := z.Destroy(ctx, "bases/app/g1"); err != nil {
		t.Fatalf("Destroy promoted base: %v", err)
	}

	// Even with a deliberately stale cache entry pointing at the old dataset,
	// Clone must recover via findSnapshot and re-cache the promoted location.
	z.cacheSnap("app-g1", "rpool/hotbed/bases/app/g1@app-g1")
	fz.resetCalls()
	if err := z.Clone(ctx, "app-g1", "ws/ws-2"); err != nil {
		t.Fatalf("Clone after stale cache: %v", err)
	}
	if calls := fz.getCalls(); len(calls) != 3 {
		t.Fatalf("stale-cache recovery Clone calls = %q, want 3 (stale clone, list, real clone)", calls)
	}

	// Next clone uses the refreshed promoted cache entry in 1 command.
	fz.resetCalls()
	if err := z.Clone(ctx, "app-g1", "ws/ws-3"); err != nil {
		t.Fatalf("Clone after cache refresh: %v", err)
	}
	if calls := fz.getCalls(); len(calls) != 1 || calls[0] != "zfs clone rpool/hotbed/ws/ws-1@app-g1 rpool/hotbed/ws/ws-3" {
		t.Fatalf("post-refresh Clone calls = %q, want 1 clone from promoted dataset", calls)
	}

	// DestroySnapshot reports ErrInUse in 1 command while clones exist.
	fz.resetCalls()
	if err := z.DestroySnapshot(ctx, "app-g1"); !errors.Is(err, ErrInUse) {
		t.Fatalf("DestroySnapshot while cloned = %v, want ErrInUse", err)
	}
	if calls := fz.getCalls(); len(calls) != 1 {
		t.Fatalf("DestroySnapshot ErrInUse calls = %q, want 1", calls)
	}

	// After removing clones, DestroySnapshot succeeds in 1 command and evicts cache.
	if err := z.Destroy(ctx, "ws/ws-2"); err != nil {
		t.Fatal(err)
	}
	if err := z.Destroy(ctx, "ws/ws-3"); err != nil {
		t.Fatal(err)
	}
	fz.resetCalls()
	if err := z.DestroySnapshot(ctx, "app-g1"); err != nil {
		t.Fatalf("DestroySnapshot after clones gone: %v", err)
	}
	if calls := fz.getCalls(); len(calls) != 1 {
		t.Fatalf("DestroySnapshot calls = %q, want 1", calls)
	}
	if _, ok := z.cachedSnap("app-g1"); ok {
		t.Error("DestroySnapshot left snapshot in cache")
	}

	// Missing snapshot returns ErrNotFound.
	if err := z.Clone(ctx, "app-g1", "ws/ws-4"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Clone missing snap = %v, want ErrNotFound", err)
	}
	if err := z.DestroySnapshot(ctx, "app-g1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DestroySnapshot missing snap = %v, want ErrNotFound", err)
	}
}

func TestZFSListVolumesAndSnapshots(t *testing.T) {
	ctx := t.Context()
	z, _ := newTestZFS("rpool/hotbed")

	for _, vol := range []string{"bases/app/g1", "bases/app/g2", "ws/ws-1", "baseimages/baseimg-111"} {
		if err := z.Create(ctx, vol); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Snapshot(ctx, "bases/app/g1", "app-g1"); err != nil {
		t.Fatal(err)
	}
	if err := z.Snapshot(ctx, "baseimages/baseimg-111", "baseimg-111"); err != nil {
		t.Fatal(err)
	}

	vols, err := z.ListVolumes(ctx)
	if err != nil {
		t.Fatalf("ListVolumes: %v", err)
	}
	wantVols := []string{"baseimages/baseimg-111", "bases/app/g1", "bases/app/g2", "ws/ws-1"}
	if !slices.Equal(vols, wantVols) {
		t.Errorf("ListVolumes = %q, want %q (must exclude intermediate parent datasets)", vols, wantVols)
	}

	snaps, err := z.ListSnapshots(ctx)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	wantSnaps := []string{"app-g1", "baseimg-111"}
	if !slices.Equal(snaps, wantSnaps) {
		t.Errorf("ListSnapshots = %q, want %q", snaps, wantSnaps)
	}

	// Destroying every leaf volume leaves empty structural container datasets
	// (ws, bases, bases/app, baseimages) on ZFS; ListVolumes must NOT return
	// them, while still returning arbitrary non-container volumes like "v1" or "a/b".
	for _, vol := range []string{"bases/app/g1", "bases/app/g2", "ws/ws-1", "baseimages/baseimg-111"} {
		if err := z.Destroy(ctx, vol); err != nil {
			t.Fatalf("Destroy(%s): %v", vol, err)
		}
	}
	vols, err = z.ListVolumes(ctx)
	if err != nil {
		t.Fatalf("ListVolumes after destroying all leaves: %v", err)
	}
	if len(vols) != 0 {
		t.Errorf("ListVolumes with only empty container datasets = %q, want empty", vols)
	}

	for _, vol := range []string{"v1", "a/b"} {
		if err := z.Create(ctx, vol); err != nil {
			t.Fatalf("Create(%s): %v", vol, err)
		}
	}
	vols, err = z.ListVolumes(ctx)
	if err != nil {
		t.Fatalf("ListVolumes with arbitrary volumes: %v", err)
	}
	wantCustom := []string{"a/b", "v1"}
	if !slices.Equal(vols, wantCustom) {
		t.Errorf("ListVolumes with arbitrary volumes = %q, want %q", vols, wantCustom)
	}
}
