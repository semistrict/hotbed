package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cladefsdv1 "github.com/semistrict/cladefs/go/cladefsd/v1"
	mountv1 "github.com/semistrict/cladefs/go/mount/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/cladefs"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/vm"
)

type fakeRetireCladefsClient struct {
	cladefsdv1.CladefsdClient
	createReqs      []*cladefsdv1.CreateCellRequest
	forkReqs        []*cladefsdv1.ForkCellRequest
	syncReqs        []*cladefsdv1.SyncCellRequest
	makeVersionReqs []*cladefsdv1.MakeVersionRequest
	deleteReqs      []*cladefsdv1.DeleteCellRequest
	dropReqs        []*cladefsdv1.DropCellRequest
	nextCell        uint64
	nextVersion     uint64
	createErr       func(*cladefsdv1.CreateCellRequest) error
	forkErr         func(*cladefsdv1.ForkCellRequest) error
	deleteErr       func(*cladefsdv1.DeleteCellRequest) error
	makeVersionErr  func(*cladefsdv1.MakeVersionRequest) error
}

func (f *fakeRetireCladefsClient) CreateCell(_ context.Context, req *cladefsdv1.CreateCellRequest, _ ...grpc.CallOption) (*cladefsdv1.CreateCellResponse, error) {
	f.createReqs = append(f.createReqs, req)
	if f.createErr != nil {
		if err := f.createErr(req); err != nil {
			return nil, err
		}
	}
	return &cladefsdv1.CreateCellResponse{Tenant: req.GetTenant(), Cell: req.GetCell()}, nil
}

func (f *fakeRetireCladefsClient) ForkCell(_ context.Context, req *cladefsdv1.ForkCellRequest, _ ...grpc.CallOption) (*cladefsdv1.ForkCellResponse, error) {
	f.forkReqs = append(f.forkReqs, req)
	if f.forkErr != nil {
		if err := f.forkErr(req); err != nil {
			return nil, err
		}
	}
	f.nextCell++
	return &cladefsdv1.ForkCellResponse{Tenant: req.GetTenant(), Cell: f.nextCell}, nil
}

func (f *fakeRetireCladefsClient) SyncCell(_ context.Context, req *cladefsdv1.SyncCellRequest, _ ...grpc.CallOption) (*cladefsdv1.SyncCellResponse, error) {
	f.syncReqs = append(f.syncReqs, req)
	return &cladefsdv1.SyncCellResponse{}, nil
}

func (f *fakeRetireCladefsClient) MakeVersion(_ context.Context, req *cladefsdv1.MakeVersionRequest, _ ...grpc.CallOption) (*cladefsdv1.MakeVersionResponse, error) {
	f.makeVersionReqs = append(f.makeVersionReqs, req)
	if f.makeVersionErr != nil {
		if err := f.makeVersionErr(req); err != nil {
			return nil, err
		}
	}
	f.nextVersion++
	return &cladefsdv1.MakeVersionResponse{Version: f.nextVersion}, nil
}

func (f *fakeRetireCladefsClient) DeleteCell(_ context.Context, req *cladefsdv1.DeleteCellRequest, _ ...grpc.CallOption) (*cladefsdv1.DeleteCellResponse, error) {
	f.deleteReqs = append(f.deleteReqs, req)
	if f.deleteErr != nil {
		if err := f.deleteErr(req); err != nil {
			return nil, err
		}
	}
	return &cladefsdv1.DeleteCellResponse{}, nil
}

func (f *fakeRetireCladefsClient) DropCell(_ context.Context, req *cladefsdv1.DropCellRequest, _ ...grpc.CallOption) (*cladefsdv1.DropCellResponse, error) {
	f.dropReqs = append(f.dropReqs, req)
	return &cladefsdv1.DropCellResponse{}, nil
}

// GetStats answers the incarnation each heartbeat follows (cladefsdLink.sync).
func (f *fakeRetireCladefsClient) GetStats(context.Context, *cladefsdv1.GetStatsRequest, ...grpc.CallOption) (*cladefsdv1.GetStatsResponse, error) {
	return &cladefsdv1.GetStatsResponse{}, nil
}

func TestBaseRetirerLifecycleAndPersistence(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cacheDir := filepath.Join(tmp, "bases")
	cellRoot := filepath.Join(tmp, "cells")

	api := &fakeRetireCladefsClient{}
	store, err := cladefs.NewStore(ctx, cladefs.Config{CellRoot: cellRoot}, api, nil, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	t0 := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	var r baseRetirer
	r.load(cacheDir)
	r.setGrace(5 * time.Minute)
	r.enqueue(0, 1001, "baseimage-old", t0)
	// Duplicate enqueue must not duplicate or push out RetireAfter.
	r.enqueue(0, 1001, "baseimage-old", t0.Add(2*time.Minute))

	// Before grace period elapses: nothing deleted.
	r.sweep(ctx, store, t0.Add(4*time.Minute))
	if len(api.createReqs) != 0 || len(api.deleteReqs) != 0 {
		t.Fatalf("deleted prematurely: create=%d delete=%d", len(api.createReqs), len(api.deleteReqs))
	}

	// Reload from disk into a fresh retirer (simulating worker restart).
	var reloaded baseRetirer
	reloaded.load(cacheDir)

	// Transient error keeps the entry queued for retry.
	failOnce := true
	api.deleteErr = func(*cladefsdv1.DeleteCellRequest) error {
		if failOnce {
			failOnce = false
			return status.Error(codes.Unavailable, "transient R2 outage")
		}
		return nil
	}
	reloaded.sweep(ctx, store, t0.Add(5*time.Minute))
	if len(api.deleteReqs) != 1 {
		t.Fatalf("expected 1 delete attempt, got %d", len(api.deleteReqs))
	}
	reloaded.sweep(ctx, store, t0.Add(6*time.Minute))
	if len(api.deleteReqs) != 2 {
		t.Fatalf("expected retry delete attempt, got %d", len(api.deleteReqs))
	}
	// Already retired now: further sweeps do nothing.
	reloaded.sweep(ctx, store, t0.Add(7*time.Minute))
	if len(api.deleteReqs) != 2 {
		t.Fatalf("expected no further deletes after retirement, got %d", len(api.deleteReqs))
	}
}

// Project bases are the controller's to retire: a node's retire queue from
// before keeps only its base images, since another node may now hold a
// project base this node replaced.
func TestBaseRetirerLeavesQueuedProjectBasesToTheController(t *testing.T) {
	cacheDir := t.TempDir()
	saved := []retiredBase{
		{Cell: 1, BaseID: "baseimage-k1"},
		{Cell: 2, BaseID: "demo-vm-12345678"},
		{Cell: 3, BaseID: "demo-fuse-12345678"},
	}
	raw, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, retiredBasesFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	var r baseRetirer
	r.load(cacheDir)
	if len(r.pending) != 1 || r.pending[0].Cell != 1 {
		t.Fatalf("pending = %+v, want only the base image (cell 1)", r.pending)
	}
}

func TestDeleteBucketCellTreatsAlreadyDeletedAsSuccess(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	api := &fakeRetireCladefsClient{}
	store, err := cladefs.NewStore(ctx, cladefs.Config{CellRoot: filepath.Join(tmp, "cells")}, api, nil, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	// Case 1: NotFound on TakeCell -> idempotent success.
	api.createErr = func(*cladefsdv1.CreateCellRequest) error {
		return status.Error(codes.NotFound, "no cell")
	}
	if err := store.DeleteBucketCell(ctx, 0, 10); err != nil {
		t.Fatalf("expected nil on NotFound, got %v", err)
	}

	// Case 2: EROFS (deleted head) on TakeCell -> idempotent success.
	st := status.New(codes.FailedPrecondition, "cell deleted")
	_ = metadata.Pairs("cladefs-errno", "30")
	api.createErr = func(*cladefsdv1.CreateCellRequest) error {
		return st.Err()
	}
	// Without errno trailer, FailedPrecondition is NOT ignored (could be EBUSY/ESTALE).
	if err := store.DeleteBucketCell(ctx, 0, 11); err == nil {
		t.Fatal("expected error for plain FailedPrecondition without EROFS")
	}
}

// Adopting the cluster's up-to-date base deletes nothing: the base this node
// held may be another node's, or forked from still, so only the controller,
// which sees every node's held bases, says when a base cell goes.
func TestAdoptingSharedBaseLeavesRetiringToTheController(t *testing.T) {
	ctx := t.Context()
	tmp := t.TempDir()
	api := &fakeRetireCladefsClient{}
	store, err := cladefs.NewStore(ctx, cladefs.Config{CellRoot: filepath.Join(tmp, "cells")}, api, nil, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	b := newCladefsFUSEBackend(tmp, statedir.CladefsFUSEConfig{})
	b.store.Store(store)
	b.loadState()
	b.retirer.setGrace(0)

	proj := &pb.HostedProject{
		Name: "cladefs",
		Repos: []*pb.Repo{
			{Id: "cladefs", Url: "https://example.invalid/cladefs", Branch: "main"},
		},
	}
	spec := baseimage.Spec{Image: "debian:bookworm", Arch: "arm64"}.Normalized()
	baseKey := b.baseKey(spec)
	heads := map[string]string{"cladefs": "deadbeef"}
	newRecipe := projectRecipe(fuseFlavor.recipeTag, baseKey, proj, heads)

	b.held.hold("cladefs", &projectBase{
		Ref:    cladefs.CellRef{Tenant: 0, Cell: 500, Version: 1},
		BaseID: "cladefs-fuse-old",
		Recipe: "oldrecipe",
	}, false, false)

	shared := &pb.SharedBase{Cell: &pb.CladefsCell{
		Project: "cladefs",
		BaseId:  "cladefs-fuse-new",
		Recipe:  newRecipe,
		Tenant:  0,
		Cell:    501,
		Version: 2,
	}}
	got, err := b.EnsureProjectBase(ctx, proj, heads, shared, spec, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("EnsureProjectBase: %v", err)
	}
	if got.Ref.Cell != 501 {
		t.Fatalf("adopted cell = %d, want 501", got.Ref.Cell)
	}
	b.ApplyHeartbeat(ctx, nil, nil, nil)
	if len(api.createReqs) != 0 || len(api.deleteReqs) != 0 {
		t.Fatalf("adopting deleted cells: takes=%+v deletes=%+v, want none", api.createReqs, api.deleteReqs)
	}
	_, _, held, _ := b.HeartbeatMeta()
	if len(held) != 1 || held[0].GetCell().GetCell() != 501 {
		t.Fatalf("held = %+v, want only cell 501", held)
	}
}

func TestRetireSupersededBaseImagesRemovesStaleJSONAndRetiresCells(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cacheDir := filepath.Join(tmp, "bases")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldJSON := filepath.Join(cacheDir, "baseimage-old-key.json")
	newJSON := filepath.Join(cacheDir, "baseimage-new-key.json")
	if err := os.WriteFile(oldJSON, []byte(`{"cell":201,"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newJSON, []byte(`{"cell":202,"version":2}`), 0o644); err != nil {
		t.Fatal(err)
	}

	api := &fakeRetireCladefsClient{}
	store, err := cladefs.NewStore(ctx, cladefs.Config{CellRoot: filepath.Join(tmp, "cells")}, api, nil, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	baseImages := map[string]cladefs.CellRef{
		"old-key": {Tenant: 0, Cell: 201, Version: 1},
		"new-key": {Tenant: 0, Cell: 202, Version: 2},
	}
	var r baseRetirer
	r.load(cacheDir)
	r.setGrace(0)

	superseded := collectSupersededBaseImages(baseImages, "new-key", 202)
	if len(superseded) != 1 || superseded[0].key != "old-key" || superseded[0].ref.Cell != 201 {
		t.Fatalf("superseded = %+v, want [old-key/201]", superseded)
	}
	if _, stillPresent := baseImages["old-key"]; stillPresent {
		t.Fatal("old-key was not removed from baseImages map")
	}
	if _, kept := baseImages["new-key"]; !kept {
		t.Fatal("new-key was unexpectedly removed from baseImages map")
	}

	now := time.Now()
	retireSupersededBaseImages(cacheDir, superseded, &r, now)
	if _, err := os.Stat(oldJSON); !os.IsNotExist(err) {
		t.Fatalf("stale %s still exists (err=%v)", oldJSON, err)
	}
	if _, err := os.Stat(newJSON); err != nil {
		t.Fatalf("active %s was removed: %v", newJSON, err)
	}

	r.sweep(ctx, store, now)
	if len(api.createReqs) != 1 || api.createReqs[0].GetCell() != 201 {
		t.Fatalf("expected TakeCell for superseded baseimage 201, got %+v", api.createReqs)
	}
	if len(api.deleteReqs) != 1 {
		t.Fatalf("expected DeleteCell for superseded baseimage 201, got %+v", api.deleteReqs)
	}
}

type fakeRetireMountClient struct {
	mountv1.MountClient
	attachReqs []*mountv1.AttachRequest
	detachReqs []*mountv1.DetachRequest
	nextCell   uint64
}

func (f *fakeRetireMountClient) Attach(_ context.Context, req *mountv1.AttachRequest, _ ...grpc.CallOption) (*mountv1.AttachResponse, error) {
	f.attachReqs = append(f.attachReqs, req)
	id := req.GetCell()
	if id == 0 {
		f.nextCell++
		id = f.nextCell
	}
	return &mountv1.AttachResponse{Cell: id, Shown: true}, nil
}

func (f *fakeRetireMountClient) Detach(_ context.Context, req *mountv1.DetachRequest, _ ...grpc.CallOption) (*mountv1.DetachResponse, error) {
	f.detachReqs = append(f.detachReqs, req)
	return &mountv1.DetachResponse{}, nil
}

func TestCladefsFUSEBackendSetsUploadIntervalOnBaseBuilds(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	mountRoot := filepath.Join(tmp, "mnt")
	if err := os.MkdirAll(mountRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	api := &fakeRetireCladefsClient{nextCell: 700, nextVersion: 10}
	mnt := &fakeRetireMountClient{nextCell: 800}
	store, err := cladefs.NewStore(ctx, cladefs.Config{CellRoot: filepath.Join(tmp, "cells")}, api, nil, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	store.SetMount(mnt)

	b := newCladefsFUSEBackend(tmp, statedir.CladefsFUSEConfig{})
	b.mountRoot = mountRoot
	b.mountStoresRoot = filepath.Join(tmp, "mount-stores")
	b.store.Store(store)
	b.retirer.load(b.cacheDir())
	b.retirer.setGrace(0)

	// 1. EnsureProvisionedBaseImage sets UploadIntervalMs on MountAttach (and cleans up on PullOCI failure).
	specBad := baseimage.Spec{Image: ":::invalid-ref:::", Arch: "arm64"}.Normalized()
	if _, err := b.EnsureProvisionedBaseImage(ctx, specBad, nil); err == nil {
		t.Fatal("expected EnsureProvisionedBaseImage to fail on invalid image ref")
	}
	if len(mnt.attachReqs) != 1 {
		t.Fatalf("MountAttach count = %d, want 1", len(mnt.attachReqs))
	}
	wantMs := uint64(baseBuildUploadInterval.Milliseconds())
	if mnt.attachReqs[0].UploadIntervalMs == nil || mnt.attachReqs[0].GetUploadIntervalMs() != wantMs {
		t.Fatalf("EnsureProvisionedBaseImage UploadIntervalMs = %v, want %d", mnt.attachReqs[0].UploadIntervalMs, wantMs)
	}
	if len(api.deleteReqs) != 1 {
		t.Fatalf("expected cleanup DeleteCell after PullOCI failure, got %d", len(api.deleteReqs))
	}

	// 2. EnsureProjectBase sets UploadIntervalMs on MountAttach for base builds.
	spec := baseimage.Spec{Image: "debian:trixie-slim", Arch: "arm64"}.Normalized()
	baseKey := b.baseKey(spec)
	b.baseImages[baseKey] = cladefs.CellRef{Tenant: 0, Cell: 600, Version: 1}
	proj := &pb.HostedProject{
		Name:  "demo",
		Repos: []*pb.Repo{{Id: "app", Url: "https://example.invalid/app.git", Branch: "main"}},
	}
	runFreshen := func(_ context.Context, s managedFreshenSpec) (*managedFreshenHandle, error) {
		return &managedFreshenHandle{
			baseID: s.BaseID,
			cmdID:  "f-" + s.BaseID + "-test",
			stubResult: &managedFreshenResult{
				BaseID:    s.BaseID,
				CommandID: "f-" + s.BaseID + "-test",
				RepoHeads: map[string]string{"app": "abc123"},
				State:     succeeded,
			},
		}, nil
	}
	res, err := b.EnsureProjectBase(ctx, proj, map[string]string{"app": "abc123"}, nil, spec, nil, nil, nil, runFreshen)
	if err != nil {
		t.Fatalf("EnsureProjectBase: %v", err)
	}
	if res.Ref.Cell != 701 {
		t.Fatalf("built project base cell = %d, want 701", res.Ref.Cell)
	}
	if len(mnt.attachReqs) != 2 {
		t.Fatalf("MountAttach count = %d, want 2", len(mnt.attachReqs))
	}
	if mnt.attachReqs[1].UploadIntervalMs == nil || mnt.attachReqs[1].GetUploadIntervalMs() != wantMs {
		t.Fatalf("EnsureProjectBase UploadIntervalMs = %v, want %d", mnt.attachReqs[1].UploadIntervalMs, wantMs)
	}

	// 3. Normal workspace PrepareWorkspace leaves UploadIntervalMs nil.
	if _, err := b.PrepareWorkspace(ctx, "ws-normal", res.Ref, 1); err != nil {
		t.Fatalf("PrepareWorkspace: %v", err)
	}
	if len(mnt.attachReqs) != 3 {
		t.Fatalf("MountAttach count = %d, want 3", len(mnt.attachReqs))
	}
	if mnt.attachReqs[2].UploadIntervalMs != nil {
		t.Fatalf("PrepareWorkspace UploadIntervalMs = %v, want nil", *mnt.attachReqs[2].UploadIntervalMs)
	}
}

type failingVMLauncher struct{}

func (failingVMLauncher) Start(context.Context, vm.ProcessSpec) error {
	return errors.New("intentional test launch failure")
}
func (failingVMLauncher) Stop(context.Context, string) error { return nil }
func (failingVMLauncher) Status(context.Context, string) (vm.ProcessStatus, error) {
	return vm.ProcessStatus{}, nil
}
func (failingVMLauncher) List(context.Context, string) ([]string, error) {
	return nil, nil
}

func TestVMBackendSetsUploadIntervalOnProjectBaseBuild(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	api := &fakeRetireCladefsClient{nextCell: 900, nextVersion: 20}
	store, err := cladefs.NewStore(ctx, cladefs.Config{CellRoot: filepath.Join(tmp, "cells")}, api, nil, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	mgr, err := vm.NewManager(vm.Config{
		RunDir:         filepath.Join(tmp, "vms"),
		DisableNetwork: true,
	}, vm.WithLauncher(failingVMLauncher{}), vm.WithToolsDiskBuilder(func(string, string) (string, error) {
		return filepath.Join(tmp, "tools.ext4"), nil
	}))
	if err != nil {
		t.Fatalf("vm.NewManager: %v", err)
	}

	b := newVMBackend(tmp, statedir.VMConfig{})
	b.mgr = mgr
	b.store.Store(store)
	spec := baseimage.Spec{Image: "debian:trixie-slim", Arch: "amd64"}.Normalized()
	baseKey := b.baseKey(spec)
	b.baseImages[baseKey] = cladefs.CellRef{Tenant: 0, Cell: 850, Version: 3}

	proj := &pb.HostedProject{
		Name:  "demo",
		Repos: []*pb.Repo{{Id: "app", Url: "https://example.invalid/app.git", Branch: "main"}},
	}
	// The freshen runs as admission starts it: BeforeStart boots its VM, and
	// a start that fails ends with OnStartFail.
	bootingFreshen := func(ctx context.Context, s managedFreshenSpec) (*managedFreshenHandle, error) {
		if err := s.BeforeStart(ctx); err != nil {
			s.OnStartFail()
			return nil, err
		}
		return nil, errors.New("failingVMLauncher booted a VM")
	}
	_, err = b.EnsureProjectBase(ctx, proj, map[string]string{"app": "def456"}, nil, spec, nil, nil, nil, bootingFreshen)
	if err == nil || !strings.Contains(err.Error(), "intentional test launch failure") {
		t.Fatalf("EnsureProjectBase = %v, want it to fail at failingVMLauncher", err)
	}

	// Verify ForkVersion -> SyncCell -> DropCell -> TakeCell(UploadIntervalMs=15000) -> cleanup DeleteCell.
	if len(api.forkReqs) != 1 || api.forkReqs[0].GetEphemeral() {
		t.Fatalf("forkReqs = %+v, want 1 non-ephemeral fork", api.forkReqs)
	}
	if len(api.createReqs) != 1 {
		t.Fatalf("TakeCell/CreateCell count = %d, want 1", len(api.createReqs))
	}
	wantMs := uint64(baseBuildUploadInterval.Milliseconds())
	if api.createReqs[0].GetCell() != 901 {
		t.Fatalf("TakeCell cell = %d, want 901", api.createReqs[0].GetCell())
	}
	if api.createReqs[0].UploadIntervalMs == nil || api.createReqs[0].GetUploadIntervalMs() != wantMs {
		t.Fatalf("VM EnsureProjectBase TakeCell UploadIntervalMs = %v, want %d", api.createReqs[0].UploadIntervalMs, wantMs)
	}
	// DeleteCell is called on VM launch failure cleanup.
	if len(api.deleteReqs) < 1 || api.deleteReqs[len(api.deleteReqs)-1].GetCell() != 901 {
		t.Fatalf("expected error cleanup DeleteCell for 901 after VM start failure, got %+v", api.deleteReqs)
	}

	// Verify TakeCell failure preserves forkRef.Cell and cleans it up in R2 via DeleteBucketCell.
	api2 := &fakeRetireCladefsClient{nextCell: 950, nextVersion: 30}
	failFirstTake := true
	api2.createErr = func(req *cladefsdv1.CreateCellRequest) error {
		if failFirstTake && req.UploadIntervalMs != nil {
			failFirstTake = false
			return status.Error(codes.Unavailable, "take failed")
		}
		return nil
	}
	store2, err := cladefs.NewStore(ctx, cladefs.Config{CellRoot: filepath.Join(tmp, "cells2")}, api2, nil, nil)
	if err != nil {
		t.Fatalf("NewStore2: %v", err)
	}
	b2 := newVMBackend(tmp, statedir.VMConfig{})
	b2.mgr = mgr
	b2.baseImages = map[string]cladefs.CellRef{baseKey: {Tenant: 0, Cell: 850, Version: 3}}
	b2.store.Store(store2)
	if _, err := b2.EnsureProjectBase(ctx, proj, map[string]string{"app": "def456"}, nil, spec, nil, nil, nil, bootingFreshen); err == nil {
		t.Fatal("expected EnsureProjectBase to fail when TakeCell fails")
	}
	// createReqs[0]: failed TakeCell(951, 15000ms); createReqs[1]: cleanup DeleteBucketCell -> TakeCell(951, nil).
	if len(api2.createReqs) != 2 || api2.createReqs[1].GetCell() != 951 {
		t.Fatalf("expected cleanup DeleteBucketCell for cell 951 after TakeCell error, got createReqs=%+v", api2.createReqs)
	}
	if len(api2.deleteReqs) < 1 || api2.deleteReqs[len(api2.deleteReqs)-1].GetCell() != 951 {
		t.Fatalf("expected DeleteCell for 951 in DeleteBucketCell cleanup, got %+v", api2.deleteReqs)
	}
}
