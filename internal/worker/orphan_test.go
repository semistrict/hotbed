package worker

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/storage"
	"github.com/semistrict/hotbed/internal/unit"
)

// zfsStrictStore wraps fake.Store to enforce ZFS's clone-dependency contract:
// DestroySnapshot fails with storage.ErrInUse while any clone of that snapshot
// still exists, and records destruction order so tests verify clones go first.
type zfsStrictStore struct {
	*fake.Store
	mu     sync.Mutex
	clones map[string]map[string]bool // snap -> set of live clone volumes
	origin map[string]string          // clone volume -> origin snap
	order  []string
}

func newZFSStrictStore(root string) *zfsStrictStore {
	return &zfsStrictStore{
		Store:  &fake.Store{Root: root},
		clones: map[string]map[string]bool{},
		origin: map[string]string{},
	}
}

func (s *zfsStrictStore) Clone(ctx context.Context, snap, name string) error {
	if err := s.Store.Clone(ctx, snap, name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clones[snap] == nil {
		s.clones[snap] = map[string]bool{}
	}
	s.clones[snap][name] = true
	s.origin[name] = snap
	return nil
}

func (s *zfsStrictStore) Destroy(ctx context.Context, name string) error {
	if err := s.Store.Destroy(ctx, name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if snap, ok := s.origin[name]; ok {
		delete(s.clones[snap], name)
		delete(s.origin, name)
	}
	s.order = append(s.order, "vol:"+name)
	return nil
}

func (s *zfsStrictStore) DestroySnapshot(ctx context.Context, snap string) error {
	s.mu.Lock()
	if len(s.clones[snap]) > 0 {
		s.mu.Unlock()
		return fmt.Errorf("snapshot %s: %w", snap, storage.ErrInUse)
	}
	s.mu.Unlock()
	if err := s.Store.DestroySnapshot(ctx, snap); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clones, snap)
	s.order = append(s.order, "snap:"+snap)
	return nil
}

// Startup orphan sweeping stops unknown units, destroys orphaned workspace
// clones BEFORE orphaned base snapshots (required by ZFS), destroys orphaned
// base volumes newest generation first, preserves all DB-tracked live records,
// and never touches shared baseimage volumes/snapshots/units (board 7923).
func TestSweepOrphansOrderAndPreservation(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	store := newZFSStrictStore(t.TempDir())
	units := &fake.Units{}
	t.Cleanup(units.Close)
	iso := fake.NewIsolation("0-1", 2)

	// Start an initial engine to seed live DB records, then close it so we can
	// add orphaned volumes/snapshots/units on disk before restarting.
	e1, err := NewEngine(ctx, EngineConfig{
		Dir: dir, NodeID: "n1", Host: "worker1", Bench: iso,
		Store: store, BaseImage: fake.BaseImage(store), Units: units, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Shared base image: must NEVER be swept by Engine.
	if err := store.Create(ctx, "baseimages/baseimg-keep123"); err != nil {
		t.Fatal(err)
	}
	if err := store.Snapshot(ctx, "baseimages/baseimg-keep123", "baseimg-keep123"); err != nil {
		t.Fatal(err)
	}

	// Live base + workspace + running command (recorded in SQLite).
	if err := store.Clone(ctx, "baseimg-keep123", "bases/liveproj/g1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Snapshot(ctx, "bases/liveproj/g1", "liveproj-g1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Clone(ctx, "liveproj-g1", "ws/ws-live"); err != nil {
		t.Fatal(err)
	}
	logDir := t.TempDir()
	if err := units.Start(ctx, unit.Spec{Name: "hotbed-cmd-live", RootDir: store.Path("ws/ws-live"), Argv: []string{"sleep", "60"}, Log: filepath.Join(logDir, "live.log")}); err != nil {
		t.Fatal(err)
	}
	if err := units.Start(ctx, unit.Spec{Name: "hotbed-baseimg-keep123", RootDir: store.Path("baseimages/baseimg-keep123"), Argv: []string{"sleep", "60"}, Log: filepath.Join(logDir, "baseimg.log")}); err != nil {
		t.Fatal(err)
	}
	e1.mu.Lock()
	liveBase := &pb.Base{Id: "liveproj-g1", Project: "liveproj", Generation: 1, State: warm, Command: "f-liveproj-g1-aaa"}
	e1.bases[liveBase.Id] = liveBase
	e1.saveBase(liveBase, change{what: "warm"})
	liveWS := &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{Id: "ws-live", Project: "liveproj", BaseId: "liveproj-g1", State: idle},
		Volume:    "ws/ws-live", BaseSnapshot: "liveproj-g1",
	}}
	e1.workspaces["ws-live"] = liveWS
	e1.saveWorkspace(liveWS, change{what: "ready"})
	liveCmd := e1.newCommand(&pb.CommandRecord{
		Command: &pb.Command{Id: "cmd-live", State: running, WorkspaceId: "ws-live"},
	})
	e1.commands["cmd-live"] = liveCmd
	e1.saveCommand(liveCmd, change{what: "running"})
	e1.mu.Unlock()
	if err := e1.Close(); err != nil {
		t.Fatal(err)
	}

	// Orphaned base chain (g1 -> g2 -> ws-orphan) and orphaned units in both
	// default and bench slices, NOT in SQLite.
	if err := store.Create(ctx, "bases/deadproj/g1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Snapshot(ctx, "bases/deadproj/g1", "deadproj-g1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Clone(ctx, "deadproj-g1", "bases/deadproj/g2"); err != nil {
		t.Fatal(err)
	}
	if err := store.Snapshot(ctx, "bases/deadproj/g2", "deadproj-g2"); err != nil {
		t.Fatal(err)
	}
	if err := store.Clone(ctx, "deadproj-g2", "ws/ws-orphan"); err != nil {
		t.Fatal(err)
	}
	// Hold ws/ws-orphan busy until its orphan unit is stopped? Better: verify
	// orphan units in default and bench slices are stopped.
	if err := units.Start(ctx, unit.Spec{Name: "hotbed-cmd-orphan", RootDir: store.Path("ws/ws-orphan"), Argv: []string{"sleep", "60"}, Log: filepath.Join(logDir, "orphan.log")}); err != nil {
		t.Fatal(err)
	}
	if err := units.Start(ctx, unit.Spec{Name: "hotbed-bench-orphan", RootDir: store.Path("ws/ws-orphan"), Slice: iso.Slice(), Argv: []string{"sleep", "60"}, Log: filepath.Join(logDir, "bench.log")}); err != nil {
		t.Fatal(err)
	}

	// Restart engine on the same DB/store/units: NewEngine runs sweepOrphans.
	e2, err := NewEngine(ctx, EngineConfig{
		Dir: dir, NodeID: "n1", Host: "worker1", Bench: iso,
		Store: store, BaseImage: fake.BaseImage(store), Units: units, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e2.Close() })

	// 1. Orphaned units in both slices must be gone; live and baseimg units preserved.
	defUnits, err := units.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	wantDef := []string{"hotbed-baseimg-keep123", "hotbed-cmd-live"}
	if !slices.Equal(defUnits, wantDef) {
		t.Errorf("default slice units after sweep = %q, want %q", defUnits, wantDef)
	}
	benchUnits, err := units.List(ctx, iso.Slice())
	if err != nil {
		t.Fatal(err)
	}
	if len(benchUnits) != 0 {
		t.Errorf("bench slice units after sweep = %q, want empty", benchUnits)
	}

	// 2. Orphaned volumes & snapshots must be gone; live & baseimage preserved.
	vols, err := store.ListVolumes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantVols := []string{"baseimages/baseimg-keep123", "bases/liveproj/g1", "ws/ws-live"}
	if !slices.Equal(vols, wantVols) {
		t.Errorf("volumes after sweep = %q, want %q", vols, wantVols)
	}
	snaps, err := store.ListSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantSnaps := []string{"baseimg-keep123", "liveproj-g1"}
	if !slices.Equal(snaps, wantSnaps) {
		t.Errorf("snapshots after sweep = %q, want %q", snaps, wantSnaps)
	}

	// 3. Verify strict dependency order: ws/ws-orphan before bases/deadproj/g2
	// before bases/deadproj/g1 before snapshots.
	store.mu.Lock()
	order := slices.Clone(store.order)
	store.mu.Unlock()
	idx := func(s string) int { return slices.Index(order, s) }
	if idx("vol:ws/ws-orphan") < 0 || idx("vol:bases/deadproj/g2") < 0 || idx("vol:bases/deadproj/g1") < 0 ||
		idx("snap:deadproj-g2") < 0 || idx("snap:deadproj-g1") < 0 {
		t.Fatalf("unexpected sweep order %q", order)
	}
	if !(idx("vol:ws/ws-orphan") < idx("vol:bases/deadproj/g2") &&
		idx("vol:bases/deadproj/g2") < idx("vol:bases/deadproj/g1") &&
		idx("vol:bases/deadproj/g1") < idx("snap:deadproj-g1") &&
		idx("vol:ws/ws-orphan") < idx("snap:deadproj-g2")) {
		t.Errorf("sweep did not destroy clones before snapshots / newest base first: %q", order)
	}
}

// One busy orphaned volume must not block sweeping the remaining orphans.
func TestSweepOrphansBestEffortOnBusyVolume(t *testing.T) {
	ctx := t.Context()
	store := &fake.Store{Root: t.TempDir()}
	units := &fake.Units{}
	t.Cleanup(units.Close)

	if err := store.Create(ctx, "ws/ws-busy"); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, "ws/ws-clean"); err != nil {
		t.Fatal(err)
	}
	store.Hold("ws/ws-busy")

	e, err := NewEngine(ctx, EngineConfig{
		Dir: t.TempDir(), NodeID: "n1", Host: "worker1",
		Store: store, BaseImage: fake.BaseImage(store), Units: units, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })

	vols, err := store.ListVolumes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(vols, []string{"ws/ws-busy"}) {
		t.Errorf("volumes after partial sweep = %q, want only [ws/ws-busy]", vols)
	}
	// Sanity: ErrBusy remained on the held volume while ws-clean was swept.
	if err := store.Destroy(ctx, "ws/ws-busy"); !errors.Is(err, storage.ErrBusy) {
		t.Errorf("ws-busy err = %v, want ErrBusy", err)
	}
}

// A non-released workspace (such as a preparing workspace waiting to fork) must
// preserve its BaseSnapshot even if its base record is no longer in e.bases,
// whereas a released workspace must not keep an otherwise-orphaned snapshot alive.
func TestSweepOrphansPreservesLiveWorkspaceBaseSnapshot(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	store := &fake.Store{Root: t.TempDir()}
	units := &fake.Units{}
	t.Cleanup(units.Close)

	e1, err := NewEngine(ctx, EngineConfig{
		Dir: dir, NodeID: "n1", Host: "worker1",
		Store: store, BaseImage: fake.BaseImage(store), Units: units, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create two base snapshots with NO matching records in e.bases:
	// - "retiredproj-g1" is referenced by a non-released (preparing) workspace.
	// - "releasedproj-g1" is referenced only by a released workspace.
	if err := store.Create(ctx, "bases/retiredproj/g1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Snapshot(ctx, "bases/retiredproj/g1", "retiredproj-g1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Destroy(ctx, "bases/retiredproj/g1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, "bases/releasedproj/g1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Snapshot(ctx, "bases/releasedproj/g1", "releasedproj-g1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Destroy(ctx, "bases/releasedproj/g1"); err != nil {
		t.Fatal(err)
	}

	e1.mu.Lock()
	prepWS := &workspace{rec: &pb.WorkspaceRecord{
		Workspace:    &pb.Workspace{Id: "ws-prep", Project: "retiredproj", BaseId: "retiredproj-g1", State: preparing},
		Volume:       "ws/ws-prep",
		BaseSnapshot: "retiredproj-g1",
	}}
	e1.workspaces["ws-prep"] = prepWS
	e1.saveWorkspace(prepWS, change{what: "preparing"})

	relWS := &workspace{rec: &pb.WorkspaceRecord{
		Workspace:    &pb.Workspace{Id: "ws-rel", Project: "releasedproj", BaseId: "releasedproj-g1", State: released},
		Volume:       "ws/ws-rel",
		BaseSnapshot: "releasedproj-g1",
	}}
	e1.workspaces["ws-rel"] = relWS
	e1.saveWorkspace(relWS, change{what: "released"})
	e1.mu.Unlock()

	if err := e1.Close(); err != nil {
		t.Fatal(err)
	}

	e2, err := NewEngine(ctx, EngineConfig{
		Dir: dir, NodeID: "n1", Host: "worker1",
		Store: store, BaseImage: fake.BaseImage(store), Units: units, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e2.Close() })

	snaps, err := store.ListSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"retiredproj-g1"}
	if !slices.Equal(snaps, want) {
		t.Errorf("snapshots after sweep = %q, want %q", snaps, want)
	}
}
