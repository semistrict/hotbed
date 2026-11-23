package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/storage"
)

type snapInUseStore struct {
	*fake.Store
	mu    sync.Mutex
	inUse map[string]bool
}

func (s *snapInUseStore) setInUse(snap string, v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inUse == nil {
		s.inUse = map[string]bool{}
	}
	if v {
		s.inUse[snap] = true
	} else {
		delete(s.inUse, snap)
	}
}

func (s *snapInUseStore) DestroySnapshot(ctx context.Context, snap string) error {
	s.mu.Lock()
	busy := s.inUse[snap]
	s.mu.Unlock()
	if busy {
		return fmt.Errorf("snapshot %s: %w", snap, storage.ErrInUse)
	}
	return s.Store.DestroySnapshot(ctx, snap)
}

// Workers must garbage-collect superseded shared base images while keeping the
// current base image, any image referenced by a warm or building base, and any
// snapshot still pinned by storage clones (board 8021).
func TestCollectSupersededBaseImages(t *testing.T) {
	dir := t.TempDir()
	rawStore := &fake.Store{Root: filepath.Join(dir, "volumes")}
	store := &snapInUseStore{Store: rawStore}

	makeImg := func(script string) string {
		t.Helper()
		mgr := &baseimage.Manager{
			Store: store,
			Spec:  baseimage.Spec{Image: "test-debian:trixie", Arch: "amd64", User: "hotbed", UID: 1000, GID: 1000, Script: script},
			Pull: func(_ context.Context, _, _, rootDir string) error {
				return os.MkdirAll(filepath.Join(rootDir, "usr", "bin"), 0o755)
			},
			Provision: baseimage.ProvisionFunc(func(_ context.Context, _, _ string) error { return nil }),
		}
		snap, _, err := mgr.Ensure(t.Context())
		if err != nil {
			t.Fatalf("ensure baseimage %q: %v", script, err)
		}
		return snap
	}

	supersededKey := makeImg("echo v1-superseded")
	warmRefKey := makeImg("echo v2-warm")
	buildingRefKey := makeImg("echo v3-building")
	inUseKey := makeImg("echo v4-zfs-clone")

	currentSpec := baseimage.Spec{Image: "test-debian:trixie", Arch: "amd64", User: "hotbed", UID: 1000, GID: 1000, Script: "echo v5-current"}
	currentMgr := &baseimage.Manager{
		Store: store,
		Spec:  currentSpec,
		Pull: func(_ context.Context, _, _, rootDir string) error {
			return os.MkdirAll(filepath.Join(rootDir, "usr", "bin"), 0o755)
		},
		Provision: baseimage.ProvisionFunc(func(_ context.Context, _, _ string) error { return nil }),
	}
	currentKey, _, err := currentMgr.Ensure(t.Context())
	if err != nil {
		t.Fatalf("ensure current baseimage: %v", err)
	}

	eng, err := NewEngine(t.Context(), EngineConfig{
		Dir: dir, NodeID: "n1", Host: "worker1",
		Store: store, Units: &fake.Units{}, System: fake.NewSystem(),
		BaseImage: currentMgr,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })

	// Clone a warm base from warmRefKey and a building base from buildingRefKey.
	warmVol := baseVolume("proj-warm", 1)
	if err := store.Clone(t.Context(), warmRefKey, warmVol); err != nil {
		t.Fatal(err)
	}
	if err := store.Snapshot(t.Context(), warmVol, baseSnapshot("proj-warm", 1)); err != nil {
		t.Fatal(err)
	}
	buildVol := baseVolume("proj-build", 1)
	if err := store.Clone(t.Context(), buildingRefKey, buildVol); err != nil {
		t.Fatal(err)
	}
	eng.mu.Lock()
	eng.bases[baseSnapshot("proj-warm", 1)] = &pb.Base{
		Id: baseSnapshot("proj-warm", 1), Project: "proj-warm", Generation: 1, State: warm,
	}
	eng.bases[baseSnapshot("proj-build", 1)] = &pb.Base{
		Id: baseSnapshot("proj-build", 1), Project: "proj-build", Generation: 1, State: building,
	}
	eng.mu.Unlock()

	// Simulate ZFS reporting ErrInUse on inUseKey's snapshot.
	store.setInUse(inUseKey, true)

	volExists := func(key string) bool {
		_, err := os.Stat(store.Path("baseimages/" + key))
		return err == nil
	}
	snapExists := func(key string) bool {
		_, err := os.Stat(filepath.Join(rawStore.Root, "snap", key))
		return err == nil
	}

	eng.collectBaseImages(t.Context())

	if volExists(supersededKey) || snapExists(supersededKey) {
		t.Fatalf("superseded base image %s was not deleted (vol=%v snap=%v)", supersededKey, volExists(supersededKey), snapExists(supersededKey))
	}
	for label, key := range map[string]string{
		"current":  currentKey,
		"warm":     warmRefKey,
		"building": buildingRefKey,
		"in-use":   inUseKey,
	} {
		if !volExists(key) || !snapExists(key) {
			t.Errorf("%s base image %s was unexpectedly deleted (vol=%v snap=%v)", label, key, volExists(key), snapExists(key))
		}
	}

	// Once the ZFS clone dependency clears, the next collection removes inUseKey.
	store.setInUse(inUseKey, false)
	eng.collectBaseImages(t.Context())
	if volExists(inUseKey) || snapExists(inUseKey) {
		t.Fatalf("released in-use base image %s was not deleted (vol=%v snap=%v)", inUseKey, volExists(inUseKey), snapExists(inUseKey))
	}

	// Once the warm base is superseded and collected, warmRefKey is collected too.
	warmVol2 := baseVolume("proj-warm", 2)
	if err := store.Clone(t.Context(), currentKey, warmVol2); err != nil {
		t.Fatal(err)
	}
	if err := store.Snapshot(t.Context(), warmVol2, baseSnapshot("proj-warm", 2)); err != nil {
		t.Fatal(err)
	}
	eng.mu.Lock()
	eng.bases[baseSnapshot("proj-warm", 1)].State = stale
	eng.bases[baseSnapshot("proj-warm", 2)] = &pb.Base{
		Id: baseSnapshot("proj-warm", 2), Project: "proj-warm", Generation: 2, State: warm,
	}
	eng.mu.Unlock()
	eng.collectBases(t.Context())
	eng.collectBaseImages(t.Context())

	if volExists(warmRefKey) || snapExists(warmRefKey) {
		t.Fatalf("previously warm-referenced base image %s was not deleted after base collection (vol=%v snap=%v)", warmRefKey, volExists(warmRefKey), snapExists(warmRefKey))
	}
	if !volExists(currentKey) || !snapExists(currentKey) {
		t.Errorf("current base image %s should still exist", currentKey)
	}
	if !volExists(buildingRefKey) || !snapExists(buildingRefKey) {
		t.Errorf("building base image %s should still exist", buildingRefKey)
	}
}

// collectBaseImages must never block on a project-controlled FIFO or follow a
// symlink at .hotbed-baseimage-ready, and must fail safe by skipping deletion
// whenever any live base's parent image cannot be safely determined (board 8878).
func TestCollectBaseImagesHostileMarkers(t *testing.T) {
	setup := func(t *testing.T) (dir string, rawStore *fake.Store, eng *Engine, supersededKey, warmRefKey, currentKey, warmVol string) {
		t.Helper()
		dir = t.TempDir()
		rawStore = &fake.Store{Root: filepath.Join(dir, "volumes")}
		makeImg := func(script string) string {
			t.Helper()
			mgr := &baseimage.Manager{
				Store: rawStore,
				Spec:  baseimage.Spec{Image: "test-debian:trixie", Arch: "amd64", User: "hotbed", UID: 1000, GID: 1000, Script: script},
				Pull: func(_ context.Context, _, _, rootDir string) error {
					return os.MkdirAll(filepath.Join(rootDir, "usr", "bin"), 0o755)
				},
				Provision: baseimage.ProvisionFunc(func(_ context.Context, _, _ string) error { return nil }),
			}
			snap, _, err := mgr.Ensure(t.Context())
			if err != nil {
				t.Fatalf("ensure baseimage %q: %v", script, err)
			}
			return snap
		}

		supersededKey = makeImg("echo v1-superseded")
		warmRefKey = makeImg("echo v2-warm")
		currentMgr := &baseimage.Manager{
			Store: rawStore,
			Spec:  baseimage.Spec{Image: "test-debian:trixie", Arch: "amd64", User: "hotbed", UID: 1000, GID: 1000, Script: "echo v3-current"},
			Pull: func(_ context.Context, _, _, rootDir string) error {
				return os.MkdirAll(filepath.Join(rootDir, "usr", "bin"), 0o755)
			},
			Provision: baseimage.ProvisionFunc(func(_ context.Context, _, _ string) error { return nil }),
		}
		var err error
		currentKey, _, err = currentMgr.Ensure(t.Context())
		if err != nil {
			t.Fatalf("ensure current baseimage: %v", err)
		}

		eng, err = NewEngine(t.Context(), EngineConfig{
			Dir: dir, NodeID: "n1", Host: "worker1",
			Store: rawStore, Units: &fake.Units{}, System: fake.NewSystem(),
			BaseImage: currentMgr,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { eng.Close() })

		warmVol = baseVolume("proj-warm", 1)
		if err := rawStore.Clone(t.Context(), warmRefKey, warmVol); err != nil {
			t.Fatal(err)
		}
		eng.mu.Lock()
		eng.bases[baseSnapshot("proj-warm", 1)] = &pb.Base{
			Id: baseSnapshot("proj-warm", 1), Project: "proj-warm", Generation: 1, State: warm,
		}
		eng.mu.Unlock()
		return
	}

	assertExists := func(t *testing.T, rawStore *fake.Store, label, key string) {
		t.Helper()
		if _, err := os.Stat(rawStore.Path("baseimages/" + key)); err != nil {
			t.Errorf("%s base image volume %s was deleted: %v", label, key, err)
		}
		if _, err := os.Stat(filepath.Join(rawStore.Root, "snap", key)); err != nil {
			t.Errorf("%s base image snapshot %s was deleted: %v", label, key, err)
		}
	}

	runCollectNoBlock := func(t *testing.T, eng *Engine) {
		t.Helper()
		done := make(chan struct{})
		go func() {
			defer close(done)
			eng.collectBaseImages(t.Context())
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("collectBaseImages blocked on hostile base marker")
		}
	}

	t.Run("FIFO marker does not block and fails safe", func(t *testing.T) {
		_, rawStore, eng, supersededKey, warmRefKey, currentKey, warmVol := setup(t)
		marker := filepath.Join(rawStore.Path(warmVol), baseimage.ReadyMarker)
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(marker, 0o600); err != nil {
			t.Fatal(err)
		}

		runCollectNoBlock(t, eng)

		assertExists(t, rawStore, "superseded", supersededKey)
		assertExists(t, rawStore, "warm", warmRefKey)
		assertExists(t, rawStore, "current", currentKey)
	})

	t.Run("symlink marker pointing outside is not followed and fails safe", func(t *testing.T) {
		dir, rawStore, eng, supersededKey, warmRefKey, currentKey, warmVol := setup(t)
		outside := filepath.Join(dir, "outside-marker")
		// Point the symlink at a file naming currentKey: if followed, GC would
		// wrongly treat warmRefKey as unreferenced and delete both warmRefKey
		// and supersededKey.
		if err := os.WriteFile(outside, []byte(currentKey+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(rawStore.Path(warmVol), baseimage.ReadyMarker)
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, marker); err != nil {
			t.Fatal(err)
		}

		runCollectNoBlock(t, eng)

		assertExists(t, rawStore, "superseded", supersededKey)
		assertExists(t, rawStore, "warm", warmRefKey)
		assertExists(t, rawStore, "current", currentKey)
	})

	t.Run("missing marker fails safe", func(t *testing.T) {
		_, rawStore, eng, supersededKey, warmRefKey, currentKey, warmVol := setup(t)
		marker := filepath.Join(rawStore.Path(warmVol), baseimage.ReadyMarker)
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}

		runCollectNoBlock(t, eng)

		assertExists(t, rawStore, "superseded", supersededKey)
		assertExists(t, rawStore, "warm", warmRefKey)
		assertExists(t, rawStore, "current", currentKey)
	})

	t.Run("worker-owned base image mapping survives tampered marker", func(t *testing.T) {
		_, rawStore, eng, supersededKey, warmRefKey, currentKey, warmVol := setup(t)
		eng.setBaseImage(baseSnapshot("proj-warm", 1), warmRefKey)
		marker := filepath.Join(rawStore.Path(warmVol), baseimage.ReadyMarker)
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(marker, 0o600); err != nil {
			t.Fatal(err)
		}

		runCollectNoBlock(t, eng)

		if _, err := os.Stat(rawStore.Path("baseimages/" + supersededKey)); !os.IsNotExist(err) {
			t.Fatalf("superseded base image %s volume still exists: %v", supersededKey, err)
		}
		if _, err := os.Stat(filepath.Join(rawStore.Root, "snap", supersededKey)); !os.IsNotExist(err) {
			t.Fatalf("superseded base image %s snapshot still exists: %v", supersededKey, err)
		}
		assertExists(t, rawStore, "warm", warmRefKey)
		assertExists(t, rawStore, "current", currentKey)
	})
}
