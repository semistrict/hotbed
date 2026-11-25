package worker

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	cladefsdv1 "github.com/semistrict/cladefs/go/cladefsd/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/cladefs"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

type fakeRetireCladefsClient struct {
	cladefsdv1.CladefsdClient
	createReqs []*cladefsdv1.CreateCellRequest
	deleteReqs []*cladefsdv1.DeleteCellRequest
	dropReqs   []*cladefsdv1.DropCellRequest
	createErr  func(*cladefsdv1.CreateCellRequest) error
	deleteErr  func(*cladefsdv1.DeleteCellRequest) error
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
	r.enqueue(0, 1001, "proj-old", t0)
	// Duplicate enqueue must not duplicate or push out RetireAfter.
	r.enqueue(0, 1001, "proj-old", t0.Add(2*time.Minute))

	// Before grace period elapses: nothing deleted.
	r.sweep(ctx, store, "retire", t0.Add(4*time.Minute))
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
	reloaded.sweep(ctx, store, "retire", t0.Add(5*time.Minute))
	if len(api.deleteReqs) != 1 {
		t.Fatalf("expected 1 delete attempt, got %d", len(api.deleteReqs))
	}
	reloaded.sweep(ctx, store, "retire", t0.Add(6*time.Minute))
	if len(api.deleteReqs) != 2 {
		t.Fatalf("expected retry delete attempt, got %d", len(api.deleteReqs))
	}
	// Already retired now: further sweeps do nothing.
	reloaded.sweep(ctx, store, "retire", t0.Add(7*time.Minute))
	if len(api.deleteReqs) != 2 {
		t.Fatalf("expected no further deletes after retirement, got %d", len(api.deleteReqs))
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
	if err := store.DeleteBucketCell(ctx, "retire/0-10", 0, 10); err != nil {
		t.Fatalf("expected nil on NotFound, got %v", err)
	}

	// Case 2: EROFS (deleted head) on TakeCell -> idempotent success.
	st := status.New(codes.FailedPrecondition, "cell deleted")
	_ = metadata.Pairs("cladefs-errno", "30")
	api.createErr = func(*cladefsdv1.CreateCellRequest) error {
		return st.Err()
	}
	// Without errno trailer, FailedPrecondition is NOT ignored (could be EBUSY/ESTALE).
	if err := store.DeleteBucketCell(ctx, "retire/0-11", 0, 11); err == nil {
		t.Fatal("expected error for plain FailedPrecondition without EROFS")
	}
}

func TestCladefsFUSEBackendRetiresSupersededBaseOnAdoptShared(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	api := &fakeRetireCladefsClient{}
	store, err := cladefs.NewStore(ctx, cladefs.Config{CellRoot: filepath.Join(tmp, "cells")}, api, nil, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	b := &CladefsFUSEBackend{
		workerDir:    tmp,
		store:        store,
		projectBases: map[string]*vmProjectBase{},
		sharedBases:  map[string]*pb.CladefsCell{},
		wsMounts:     map[string]fuseWSMount{},
	}
	b.retirer.load(b.cacheDir())
	b.retirer.setGrace(0)

	proj := &pb.HostedProject{
		Name: "cladefs",
		Repos: []*pb.Repo{
			{Id: "cladefs", Url: "https://example.invalid/cladefs", Branch: "main"},
		},
	}
	spec := baseimage.Spec{Image: "debian:bookworm", Arch: "arm64"}.Normalized()
	baseKey := b.fuseBaseKey(spec)
	heads := map[string]string{"cladefs": "deadbeef"}
	newRecipe := fuseProjectRecipe(baseKey, proj, heads)

	// Seed an older local cached base for the same project.
	b.projectBases["cladefs"] = &vmProjectBase{
		Ref:    cladefs.CellRef{Tenant: 0, Cell: 500, Version: 1},
		BaseID: "cladefs-fuse-old",
		Recipe: "oldrecipe",
	}

	shared := &pb.CladefsCell{
		Project: "cladefs",
		BaseId:  "cladefs-fuse-new",
		Recipe:  newRecipe,
		Tenant:  0,
		Cell:    501,
		Version: 2,
	}
	got, err := b.EnsureProjectBase(ctx, proj, heads, shared, spec, nil, nil, nil)
	if err != nil {
		t.Fatalf("EnsureProjectBase: %v", err)
	}
	if got.Ref.Cell != 501 {
		t.Fatalf("adopted cell = %d, want 501", got.Ref.Cell)
	}
	if len(api.createReqs) != 1 || api.createReqs[0].GetCell() != 500 {
		t.Fatalf("expected TakeCell for superseded cell 500, got %+v", api.createReqs)
	}
	if len(api.deleteReqs) != 1 {
		t.Fatalf("expected DeleteCell for superseded cell 500, got %+v", api.deleteReqs)
	}
}
