package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	cladefsdv1 "github.com/semistrict/cladefs/go/cladefsd/v1"
	mountv1 "github.com/semistrict/cladefs/go/mount/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/cladefs"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/statedir"
)

type detachControlMountClient struct {
	mountv1.MountClient
	mu         sync.Mutex
	attachReqs []*mountv1.AttachRequest
	detachReqs []*mountv1.DetachRequest
	nextCell   uint64
	detachHook func(ctx context.Context, req *mountv1.DetachRequest) error
}

func (f *detachControlMountClient) Attach(_ context.Context, req *mountv1.AttachRequest, _ ...grpc.CallOption) (*mountv1.AttachResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attachReqs = append(f.attachReqs, req)
	id := req.GetCell()
	if id == 0 {
		f.nextCell++
		id = f.nextCell
	}
	return &mountv1.AttachResponse{Cell: id, Shown: true}, nil
}

func (f *detachControlMountClient) Detach(ctx context.Context, req *mountv1.DetachRequest, _ ...grpc.CallOption) (*mountv1.DetachResponse, error) {
	f.mu.Lock()
	f.detachReqs = append(f.detachReqs, req)
	hook := f.detachHook
	f.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, req); err != nil {
			return nil, err
		}
	}
	return &mountv1.DetachResponse{}, nil
}

func (f *detachControlMountClient) detaches() []*mountv1.DetachRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*mountv1.DetachRequest(nil), f.detachReqs...)
}

type fakeDetachCladefsClient struct {
	cladefsdv1.CladefsdClient
	mu          sync.Mutex
	nextCell    uint64
	nextVersion uint64
	deleteReqs  []uint64
}

func (f *fakeDetachCladefsClient) ForkCell(_ context.Context, req *cladefsdv1.ForkCellRequest, _ ...grpc.CallOption) (*cladefsdv1.ForkCellResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextCell++
	return &cladefsdv1.ForkCellResponse{Tenant: req.GetTenant(), Cell: f.nextCell}, nil
}

func (f *fakeDetachCladefsClient) SyncCell(_ context.Context, _ *cladefsdv1.SyncCellRequest, _ ...grpc.CallOption) (*cladefsdv1.SyncCellResponse, error) {
	return &cladefsdv1.SyncCellResponse{}, nil
}

func (f *fakeDetachCladefsClient) MakeVersion(_ context.Context, _ *cladefsdv1.MakeVersionRequest, _ ...grpc.CallOption) (*cladefsdv1.MakeVersionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextVersion++
	return &cladefsdv1.MakeVersionResponse{Version: f.nextVersion}, nil
}

func (f *fakeDetachCladefsClient) DropCell(_ context.Context, _ *cladefsdv1.DropCellRequest, _ ...grpc.CallOption) (*cladefsdv1.DropCellResponse, error) {
	return &cladefsdv1.DropCellResponse{}, nil
}

func (f *fakeDetachCladefsClient) DeleteCell(_ context.Context, req *cladefsdv1.DeleteCellRequest, _ ...grpc.CallOption) (*cladefsdv1.DeleteCellResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteReqs = append(f.deleteReqs, req.GetCell())
	return &cladefsdv1.DeleteCellResponse{}, nil
}

func TestCladefsFUSEBackendDetachCleanupForceAndTimeout(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	mountRoot := filepath.Join(tmp, "mnt")
	if err := os.MkdirAll(mountRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	api := &fakeDetachCladefsClient{nextCell: 700, nextVersion: 10}
	mnt := &detachControlMountClient{nextCell: 800}
	store, err := cladefs.NewStore(ctx, cladefs.Config{CellRoot: filepath.Join(tmp, "cells")}, api, nil, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	store.SetMount(mnt)

	var events []string
	var eventsMu sync.Mutex
	record := func(ev string) {
		eventsMu.Lock()
		events = append(events, ev)
		eventsMu.Unlock()
	}

	b := newCladefsFUSEBackend(tmp, statedir.CladefsFUSEConfig{})
	b.mountRoot = mountRoot
	b.mountStoresRoot = filepath.Join(tmp, "mount-stores")
	b.detachTimeout = 80 * time.Millisecond
	b.cleanupMount = func(dir string, syncFS bool) {
		if syncFS {
			record("cleanup:sync:" + dir)
		} else {
			record("cleanup:nosync:" + dir)
		}
	}
	b.store.Store(store)
	b.retirer.load(b.cacheDir())
	b.retirer.setGrace(0)

	spec := baseimage.Spec{Image: "debian:trixie-slim", Arch: "arm64"}.Normalized()
	b.baseImages[b.baseKey(spec)] = cladefs.CellRef{Tenant: 0, Cell: 600, Version: 1}
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

	// 1. Simulate a wedged cladefsd Mount.Detach on the first project base build:
	// EnsureProjectBase must never call force=false (which wedges cladefsd in
	// Attaching::Detaching and makes subsequent force=true fail EBUSY), must
	// time out promptly instead of deadlocking projectLock, and must release
	// projectLock so the next build succeeds.
	var hangFirst sync.Once
	mnt.detachHook = func(dctx context.Context, req *mountv1.DetachRequest) error {
		record("detach:" + boolStr(req.GetForce()))
		if !req.GetForce() {
			return status.Error(codes.FailedPrecondition, "unexpected force=false detach")
		}
		hung := false
		hangFirst.Do(func() {
			hung = true
			<-dctx.Done()
		})
		if hung {
			return dctx.Err()
		}
		return nil
	}

	start := time.Now()
	_, err = b.EnsureProjectBase(ctx, proj, map[string]string{"app": "abc123"}, nil, spec, nil, nil, nil, runFreshen)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first EnsureProjectBase err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("first EnsureProjectBase took %s; detach timeout was not bounded", elapsed)
	}

	// 2. Second build on the same project must acquire projectLock cleanly and succeed,
	// running cleanupMount(mntDir, true) strictly BEFORE force=true MountDetach.
	eventsMu.Lock()
	events = nil
	eventsMu.Unlock()
	res, err := b.EnsureProjectBase(ctx, proj, map[string]string{"app": "abc123"}, nil, spec, nil, nil, nil, runFreshen)
	if err != nil {
		t.Fatalf("second EnsureProjectBase after detach timeout failed: %v", err)
	}
	wantDir := b.mountDir(0, res.Ref.Cell)
	eventsMu.Lock()
	gotEvents := append([]string(nil), events...)
	eventsMu.Unlock()
	if len(gotEvents) != 2 || gotEvents[0] != "cleanup:sync:"+wantDir || gotEvents[1] != "detach:true" {
		t.Fatalf("EnsureProjectBase detach sequence = %v, want [cleanup:sync:%s detach:true]", gotEvents, wantDir)
	}

	// 3. DeleteWorkspace must still cleanupMount(mntDir, false), detach, and delete
	// even when caller's ctx is already cancelled (without hanging forever).
	cell, err := b.PrepareWorkspace(ctx, "ws-cancel", res.Ref, 1)
	if err != nil {
		t.Fatalf("PrepareWorkspace: %v", err)
	}
	mntPath, _ := b.MountPath("ws/ws-cancel")
	eventsMu.Lock()
	events = nil
	eventsMu.Unlock()
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := b.DeleteWorkspace(canceledCtx, "ws-cancel"); err != nil {
		t.Fatalf("DeleteWorkspace with cancelled ctx: %v", err)
	}
	eventsMu.Lock()
	gotDelEvents := append([]string(nil), events...)
	eventsMu.Unlock()
	if len(gotDelEvents) != 2 || gotDelEvents[0] != "cleanup:nosync:"+mntPath || gotDelEvents[1] != "detach:true" {
		t.Fatalf("DeleteWorkspace sequence = %v, want [cleanup:nosync:%s detach:true]", gotDelEvents, mntPath)
	}
	for i, req := range mnt.detaches() {
		if !req.GetForce() {
			t.Errorf("detach request #%d (cell %d) used force=false; want force=true everywhere", i, req.GetCell())
		}
	}
	_ = cell

	// 4. EnsureProvisionedBaseImage error path cleans mount dir and uses bounded force detach.
	eventsMu.Lock()
	events = nil
	eventsMu.Unlock()
	specBad := baseimage.Spec{Image: ":::invalid-ref:::", Arch: "arm64"}.Normalized()
	if _, err := b.EnsureProvisionedBaseImage(ctx, specBad, nil); err == nil {
		t.Fatal("expected EnsureProvisionedBaseImage to fail on invalid image ref")
	}
	eventsMu.Lock()
	gotBaseErrEvents := append([]string(nil), events...)
	eventsMu.Unlock()
	if len(gotBaseErrEvents) != 2 || gotBaseErrEvents[1] != "detach:true" {
		t.Fatalf("EnsureProvisionedBaseImage cleanup sequence = %v, want [cleanup:nosync:... detach:true]", gotBaseErrEvents)
	}
}

func boolStr(v bool) string {
	if v {
		return "true"
	}
	return "false"
}
