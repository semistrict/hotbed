package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	cladefsdv1 "github.com/semistrict/cladefs/go/cladefsd/v1"
	mountv1 "github.com/semistrict/cladefs/go/mount/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/semistrict/hotbed/internal/cladefs"
	"github.com/semistrict/hotbed/internal/storage"
)

type fakeFuseCladefsClient struct {
	cladefsdv1.CladefsdClient
	forkReqs   []*cladefsdv1.ForkCellRequest
	syncReqs   []*cladefsdv1.SyncCellRequest
	dropReqs   []*cladefsdv1.DropCellRequest
	deleteReqs []*cladefsdv1.DeleteCellRequest
	cells      map[uint64]bool
	nextCell   uint64
}

func (f *fakeFuseCladefsClient) ForkCell(_ context.Context, req *cladefsdv1.ForkCellRequest, _ ...grpc.CallOption) (*cladefsdv1.ForkCellResponse, error) {
	f.forkReqs = append(f.forkReqs, req)
	if f.cells == nil {
		f.cells = map[uint64]bool{}
	}
	f.nextCell++
	f.cells[f.nextCell] = true
	return &cladefsdv1.ForkCellResponse{Tenant: req.GetTenant(), Cell: f.nextCell}, nil
}

func (f *fakeFuseCladefsClient) SyncCell(_ context.Context, req *cladefsdv1.SyncCellRequest, _ ...grpc.CallOption) (*cladefsdv1.SyncCellResponse, error) {
	f.syncReqs = append(f.syncReqs, req)
	return &cladefsdv1.SyncCellResponse{}, nil
}

func (f *fakeFuseCladefsClient) DropCell(_ context.Context, req *cladefsdv1.DropCellRequest, _ ...grpc.CallOption) (*cladefsdv1.DropCellResponse, error) {
	f.dropReqs = append(f.dropReqs, req)
	delete(f.cells, req.GetCell())
	return &cladefsdv1.DropCellResponse{}, nil
}

func (f *fakeFuseCladefsClient) DeleteCell(_ context.Context, req *cladefsdv1.DeleteCellRequest, _ ...grpc.CallOption) (*cladefsdv1.DeleteCellResponse, error) {
	f.deleteReqs = append(f.deleteReqs, req)
	delete(f.cells, req.GetCell())
	return &cladefsdv1.DeleteCellResponse{}, nil
}

type fakeFuseMountClient struct {
	mountv1.MountClient
	attachReqs []*mountv1.AttachRequest
	detachReqs []*mountv1.DetachRequest
	attachErr  error
}

func (f *fakeFuseMountClient) Attach(_ context.Context, req *mountv1.AttachRequest, _ ...grpc.CallOption) (*mountv1.AttachResponse, error) {
	f.attachReqs = append(f.attachReqs, req)
	if f.attachErr != nil {
		return nil, f.attachErr
	}
	return &mountv1.AttachResponse{Cell: req.GetCell(), Shown: true}, nil
}

func (f *fakeFuseMountClient) Detach(_ context.Context, req *mountv1.DetachRequest, _ ...grpc.CallOption) (*mountv1.DetachResponse, error) {
	f.detachReqs = append(f.detachReqs, req)
	return &mountv1.DetachResponse{}, nil
}

func TestCladefsFUSEBackendPrepareAndDeleteWorkspaceEphemeral(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cellRoot := filepath.Join(tmp, "cells")
	mountRoot := filepath.Join(tmp, "mnt")
	mountStoresRoot := filepath.Join(tmp, "mount-stores")
	if err := os.MkdirAll(mountRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	api := &fakeFuseCladefsClient{nextCell: 100}
	mnt := &fakeFuseMountClient{}
	store, err := cladefs.NewStore(ctx, cladefs.Config{CellRoot: cellRoot}, api, nil, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	store.SetMount(mnt)

	b := &CladefsFUSEBackend{
		workerDir:       tmp,
		mountRoot:       mountRoot,
		mountStoresRoot: mountStoresRoot,
		wsMounts:        map[string]fuseWSMount{},
	}
	b.store.Store(store)

	baseRef := cladefs.CellRef{Tenant: 0, Cell: 42, Version: 7}

	cell, mntPath, err := b.PrepareWorkspace(ctx, "ws-1", baseRef, 99)
	if err != nil {
		t.Fatalf("PrepareWorkspace: %v", err)
	}
	if len(api.forkReqs) != 1 || !api.forkReqs[0].GetEphemeral() {
		t.Fatalf("ForkCell requests = %+v, want 1 ephemeral=true fork", api.forkReqs)
	}
	if len(api.syncReqs) != 0 {
		t.Fatalf("SyncCell called %d times for ephemeral workspace, want 0", len(api.syncReqs))
	}
	if len(mnt.attachReqs) != 1 || mnt.attachReqs[0].GetCell() != 101 {
		t.Fatalf("MountAttach requests = %+v, want cell 101", mnt.attachReqs)
	}
	if mnt.attachReqs[0].UploadIntervalMs != nil {
		t.Fatalf("ephemeral workspace MountAttach UploadIntervalMs = %v, want nil", *mnt.attachReqs[0].UploadIntervalMs)
	}
	if cell.GetCell() != 101 {
		t.Fatalf("returned cell = %d, want 101", cell.GetCell())
	}
	wantMount := filepath.Join(mountRoot, cladefs.Base32ID(0), cladefs.Base32ID(101))
	if mntPath != wantMount {
		t.Fatalf("mountPath = %q, want %q", mntPath, wantMount)
	}

	// Calling PrepareWorkspace while ws-1 still has an active FUSE mount must fail
	// ErrBusy without deleting the live mount's cell.
	deletesBeforeBusy := len(api.deleteReqs)
	if _, _, err := b.PrepareWorkspace(ctx, "ws-1", baseRef, 100); !errors.Is(err, storage.ErrBusy) {
		t.Fatalf("PrepareWorkspace while active mount exists: err = %v, want storage.ErrBusy", err)
	}
	if len(api.deleteReqs) != deletesBeforeBusy {
		t.Fatalf("PrepareWorkspace deleted cell while active mount existed: %+v", api.deleteReqs)
	}

	if err := b.DeleteWorkspace(ctx, "ws-1"); err != nil {
		t.Fatalf("DeleteWorkspace: %v", err)
	}
	if len(mnt.detachReqs) != 1 || mnt.detachReqs[0].GetCell() != 101 || !mnt.detachReqs[0].GetForce() {
		t.Fatalf("MountDetach requests = %+v, want force detach cell 101", mnt.detachReqs)
	}
	if len(api.deleteReqs) != 1 || api.deleteReqs[0].GetCell() != 101 {
		t.Fatalf("DeleteCell requests = %+v, want cell 101", api.deleteReqs)
	}

	// Verify attach failure cleans up the forked ephemeral cell.
	mnt.attachErr = status.Error(codes.Internal, "boom")
	if _, _, err := b.PrepareWorkspace(ctx, "ws-2", baseRef, 100); err == nil {
		t.Fatal("expected PrepareWorkspace error when MountAttach fails")
	}
	if len(api.deleteReqs) != 2 || api.deleteReqs[1].GetCell() != 102 {
		t.Fatalf("cleanup DeleteCell = %+v, want 2nd cell 102", api.deleteReqs)
	}
}

func TestCladefsFUSEMountStoreDirJBOD(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	b := &CladefsFUSEBackend{
		mountStoresRoot: string(filepath.ListSeparator) + rootA + string(filepath.ListSeparator) + rootB,
	}
	rel := filepath.Join(cladefs.Base32ID(0), cladefs.Base32ID(42))
	// Leading empty entry must be skipped, falling back to rootA (not raw ":A:B").
	got, err := b.mountStoreDir(0, 42)
	if err != nil || got != filepath.Join(rootA, rel) {
		t.Fatalf("mountStoreDir fallback = (%q, %v), want %q", got, err, filepath.Join(rootA, rel))
	}
	// Existing directory on rootB is found.
	wantB := filepath.Join(rootB, rel)
	if err := os.MkdirAll(wantB, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err = b.mountStoreDir(0, 42)
	if err != nil || got != wantB {
		t.Fatalf("mountStoreDir on rootB = (%q, %v), want %q", got, err, wantB)
	}
	// Unreadable secondary root must error instead of falling back to rootA.
	if os.Geteuid() != 0 {
		if err := os.Chmod(filepath.Join(rootB, cladefs.Base32ID(0)), 0o000); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(filepath.Join(rootB, cladefs.Base32ID(0)), 0o755)
		if _, err := b.mountStoreDir(0, 42); err == nil {
			t.Fatalf("expected error when secondary mount store is unreadable, got nil")
		}
	}
}
