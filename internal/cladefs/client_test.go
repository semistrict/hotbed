package cladefs

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	cladefsdv1 "github.com/semistrict/cladefs/go/cladefsd/v1"
	mountv1 "github.com/semistrict/cladefs/go/mount/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/semistrict/hotbed/internal/storage"
)

type fakeHealth struct {
	healthpb.HealthClient
	status healthpb.HealthCheckResponse_ServingStatus
	err    error
}

func (f *fakeHealth) Check(context.Context, *healthpb.HealthCheckRequest, ...grpc.CallOption) (*healthpb.HealthCheckResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &healthpb.HealthCheckResponse{Status: f.status}, nil
}

type fakeCladefs struct {
	cladefsdv1.CladefsdClient
	grantReq   *cladefsdv1.GrantRequest
	grantSock  string
	createReqs []*cladefsdv1.CreateCellRequest
	cells      map[string]uint64
	synced     map[string]bool
	peers      []string
	nextCell   uint64
	nextVer    uint64
	deleteBusy map[string]bool
}

type fakeMount struct {
	mountv1.MountClient
	attachReqs []*mountv1.AttachRequest
	nextCell   uint64
}

func (f *fakeMount) Attach(_ context.Context, req *mountv1.AttachRequest, _ ...grpc.CallOption) (*mountv1.AttachResponse, error) {
	f.attachReqs = append(f.attachReqs, req)
	id := req.GetCell()
	if id == 0 {
		f.nextCell++
		id = f.nextCell
	}
	return &mountv1.AttachResponse{Cell: id, Shown: true}, nil
}

func newFakeCladefs() *fakeCladefs {
	return &fakeCladefs{
		cells:      map[string]uint64{},
		synced:     map[string]bool{},
		deleteBusy: map[string]bool{},
		nextCell:   100,
		nextVer:    1,
	}
}

func (f *fakeCladefs) Grant(_ context.Context, req *cladefsdv1.GrantRequest, _ ...grpc.CallOption) (*cladefsdv1.GrantResponse, error) {
	f.grantReq = req
	return &cladefsdv1.GrantResponse{Socket: f.grantSock}, nil
}

func (f *fakeCladefs) ImportImage(_ context.Context, req *cladefsdv1.ImportImageRequest, _ ...grpc.CallOption) (*cladefsdv1.ImportImageResponse, error) {
	f.nextCell++
	f.cells[req.Path] = f.nextCell
	return &cladefsdv1.ImportImageResponse{Tenant: req.Tenant, Cell: f.nextCell}, nil
}

func (f *fakeCladefs) CreateCell(_ context.Context, req *cladefsdv1.CreateCellRequest, _ ...grpc.CallOption) (*cladefsdv1.CreateCellResponse, error) {
	f.createReqs = append(f.createReqs, req)
	id := req.Cell
	if id == 0 {
		f.nextCell++
		id = f.nextCell
	}
	f.cells[req.Path] = id
	return &cladefsdv1.CreateCellResponse{Tenant: req.Tenant, Cell: id}, nil
}

func (f *fakeCladefs) ForkCell(_ context.Context, req *cladefsdv1.ForkCellRequest, _ ...grpc.CallOption) (*cladefsdv1.ForkCellResponse, error) {
	f.nextCell++
	f.cells[req.To] = f.nextCell
	return &cladefsdv1.ForkCellResponse{Tenant: req.Tenant, Cell: f.nextCell}, nil
}

func (f *fakeCladefs) MakeVersion(_ context.Context, req *cladefsdv1.MakeVersionRequest, _ ...grpc.CallOption) (*cladefsdv1.MakeVersionResponse, error) {
	if _, ok := f.cells[req.Path]; !ok {
		return nil, status.Error(codes.NotFound, "missing")
	}
	v := f.nextVer
	f.nextVer++
	return &cladefsdv1.MakeVersionResponse{Version: v}, nil
}

func (f *fakeCladefs) SyncCell(_ context.Context, req *cladefsdv1.SyncCellRequest, _ ...grpc.CallOption) (*cladefsdv1.SyncCellResponse, error) {
	f.synced[req.Path] = true
	return &cladefsdv1.SyncCellResponse{}, nil
}

func (f *fakeCladefs) DeleteCell(_ context.Context, req *cladefsdv1.DeleteCellRequest, _ ...grpc.CallOption) (*cladefsdv1.DeleteCellResponse, error) {
	if f.deleteBusy[req.Path] {
		return nil, syscall.EBUSY
	}
	if _, ok := f.cells[req.Path]; !ok {
		return nil, status.Error(codes.NotFound, "gone")
	}
	delete(f.cells, req.Path)
	return &cladefsdv1.DeleteCellResponse{}, nil
}

func (f *fakeCladefs) SetPeers(_ context.Context, req *cladefsdv1.SetPeersRequest, _ ...grpc.CallOption) (*cladefsdv1.SetPeersResponse, error) {
	f.peers = append([]string(nil), req.Addresses...)
	return &cladefsdv1.SetPeersResponse{}, nil
}

func (f *fakeCladefs) ShrinkMemory(_ context.Context, req *cladefsdv1.ShrinkMemoryRequest, _ ...grpc.CallOption) (*cladefsdv1.ShrinkMemoryResponse, error) {
	return &cladefsdv1.ShrinkMemoryResponse{Bytes: req.TargetBytes}, nil
}

func TestStoreLifecycleAndPathConfinement(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	api := newFakeCladefs()
	api.grantSock = "/run/cladefsd/tenants/7.sock"

	st, err := NewStore(ctx, Config{
		CellRoot: root,
		TenantID: 7,
		VMUID:    999,
		VMGID:    999,
	}, api, &fakeHealth{status: healthpb.HealthCheckResponse_SERVING}, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if st.VMSock() != "/run/cladefsd/tenants/7.sock" {
		t.Fatalf("VMSock = %q, want granted tenant socket", st.VMSock())
	}
	if api.grantReq == nil || api.grantReq.Tenant != 7 || api.grantReq.GetUid() != 999 {
		t.Fatalf("unexpected GrantRequest: %+v", api.grantReq)
	}

	// Path traversal checks
	for _, bad := range []string{"", ".", "..", "../escape", "a/../../escape", "/abs/path"} {
		if _, err := st.CellPath(bad); err == nil {
			t.Fatalf("CellPath(%q) succeeded, expected error", bad)
		}
	}

	// Import -> MakeVersion -> SyncCell -> ForkVersion -> DeleteCell
	base, err := st.ImportImage(ctx, "baseimages/debian-v1", "/tmp/img.tar", 4<<30)
	if err != nil {
		t.Fatalf("ImportImage: %v", err)
	}
	if base.Cell == 0 || base.Version == 0 || !api.synced[base.HostPath] {
		t.Fatalf("unexpected imported CellRef: %+v", base)
	}

	ws, err := st.ForkVersion(ctx, base, "ws/ws-1234", true)
	if err != nil {
		t.Fatalf("ForkVersion: %v", err)
	}
	if ws.HostPath != filepath.Join(root, "ws/ws-1234") {
		t.Fatalf("ws.HostPath = %q", ws.HostPath)
	}

	// Busy delete maps to storage.ErrBusy
	api.deleteBusy[ws.HostPath] = true
	if err := st.DeleteCell(ctx, "ws/ws-1234"); !errors.Is(err, storage.ErrBusy) {
		t.Fatalf("busy DeleteCell err = %v, want storage.ErrBusy", err)
	}
	delete(api.deleteBusy, ws.HostPath)

	// Delete + idempotent second delete
	if err := st.DeleteCell(ctx, "ws/ws-1234"); err != nil {
		t.Fatalf("DeleteCell: %v", err)
	}
	if err := st.DeleteCell(ctx, "ws/ws-1234"); err != nil {
		t.Fatalf("idempotent DeleteCell: %v", err)
	}

	if err := st.SetPeers(ctx, []string{"100.73.156.10:9185"}); err != nil {
		t.Fatalf("SetPeers: %v", err)
	}
	if len(api.peers) != 1 || api.peers[0] != "100.73.156.10:9185" {
		t.Fatalf("peers = %v", api.peers)
	}
	got, err := st.ShrinkMemory(ctx, 1<<30)
	if err != nil || got != 1<<30 {
		t.Fatalf("ShrinkMemory = (%d, %v)", got, err)
	}
}

func TestStoreRefusesUnhealthyDaemon(t *testing.T) {
	_, err := NewStore(context.Background(), Config{CellRoot: t.TempDir()}, newFakeCladefs(),
		&fakeHealth{status: healthpb.HealthCheckResponse_NOT_SERVING}, nil)
	if err == nil {
		t.Fatal("expected error when cladefsd is NOT_SERVING")
	}
}

func TestStoreUploadIntervalMsPlumbing(t *testing.T) {
	ctx := context.Background()
	api := newFakeCladefs()
	mnt := &fakeMount{nextCell: 500}
	st, err := NewStore(ctx, Config{CellRoot: t.TempDir()}, api, nil, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	st.SetMount(mnt)

	// Zero duration leaves UploadIntervalMs nil (daemon default).
	if _, err := st.CreateCell(ctx, "normal/c1", 1<<30, nil, 0); err != nil {
		t.Fatalf("CreateCell(0): %v", err)
	}
	if _, err := st.TakeCell(ctx, "normal/t1", 0, 101, 1<<30, nil, 0); err != nil {
		t.Fatalf("TakeCell(0): %v", err)
	}
	if _, _, err := st.MountAttach(ctx, 0, 0, 1<<30, nil, 0); err != nil {
		t.Fatalf("MountAttach(0): %v", err)
	}

	// Explicit duration sets UploadIntervalMs in milliseconds.
	if _, err := st.CreateCell(ctx, "base/c2", 1<<30, nil, 15*time.Second); err != nil {
		t.Fatalf("CreateCell(15s): %v", err)
	}
	if _, err := st.TakeCell(ctx, "base/t2", 0, 102, 1<<30, nil, 15*time.Second); err != nil {
		t.Fatalf("TakeCell(15s): %v", err)
	}
	if _, _, err := st.MountAttach(ctx, 0, 102, 0, nil, 15*time.Second); err != nil {
		t.Fatalf("MountAttach(15s): %v", err)
	}

	// Sub-millisecond positive duration clamps up to 1 ms (cladefsd rejects 0).
	if _, err := st.CreateCell(ctx, "base/c3", 1<<30, nil, 500*time.Microsecond); err != nil {
		t.Fatalf("CreateCell(500us): %v", err)
	}

	if len(api.createReqs) != 5 {
		t.Fatalf("createReqs = %d, want 5", len(api.createReqs))
	}
	if api.createReqs[0].UploadIntervalMs != nil {
		t.Fatalf("CreateCell(0) UploadIntervalMs = %v, want nil", *api.createReqs[0].UploadIntervalMs)
	}
	if api.createReqs[1].UploadIntervalMs != nil {
		t.Fatalf("TakeCell(0) UploadIntervalMs = %v, want nil", *api.createReqs[1].UploadIntervalMs)
	}
	if api.createReqs[2].UploadIntervalMs == nil || api.createReqs[2].GetUploadIntervalMs() != 15000 {
		t.Fatalf("CreateCell(15s) UploadIntervalMs = %v, want 15000", api.createReqs[2].UploadIntervalMs)
	}
	if api.createReqs[3].UploadIntervalMs == nil || api.createReqs[3].GetUploadIntervalMs() != 15000 {
		t.Fatalf("TakeCell(15s) UploadIntervalMs = %v, want 15000", api.createReqs[3].UploadIntervalMs)
	}
	if api.createReqs[4].UploadIntervalMs == nil || api.createReqs[4].GetUploadIntervalMs() != 1 {
		t.Fatalf("CreateCell(500us) UploadIntervalMs = %v, want 1", api.createReqs[4].UploadIntervalMs)
	}

	if len(mnt.attachReqs) != 2 {
		t.Fatalf("attachReqs = %d, want 2", len(mnt.attachReqs))
	}
	if mnt.attachReqs[0].UploadIntervalMs != nil {
		t.Fatalf("MountAttach(0) UploadIntervalMs = %v, want nil", *mnt.attachReqs[0].UploadIntervalMs)
	}
	if mnt.attachReqs[1].UploadIntervalMs == nil || mnt.attachReqs[1].GetUploadIntervalMs() != 15000 {
		t.Fatalf("MountAttach(15s) UploadIntervalMs = %v, want 15000", mnt.attachReqs[1].UploadIntervalMs)
	}
}

func TestStoreJBODSpreadsAndResolvesAcrossMultipleRoots(t *testing.T) {
	ctx := context.Background()
	rootA := t.TempDir()
	rootB := t.TempDir()
	api := newFakeCladefs()

	st, err := NewStore(ctx, Config{
		CellRoot: rootA + string(filepath.ListSeparator) + rootB,
	}, api, &fakeHealth{status: healthpb.HealthCheckResponse_SERVING}, nil)
	if err != nil {
		t.Fatalf("NewStore JBOD: %v", err)
	}
	if len(st.CellRoots()) != 2 {
		t.Fatalf("CellRoots = %v, want 2", st.CellRoots())
	}

	c1, err := st.CreateCell(ctx, "ws/ws-1", 1<<30, nil, 0)
	if err != nil {
		t.Fatalf("CreateCell 1: %v", err)
	}
	c2, err := st.CreateCell(ctx, "ws/ws-2", 1<<30, nil, 0)
	if err != nil {
		t.Fatalf("CreateCell 2: %v", err)
	}
	if filepath.Dir(filepath.Dir(c1.HostPath)) == filepath.Dir(filepath.Dir(c2.HostPath)) {
		t.Fatalf("expected balanced roots to spread cells: c1=%s c2=%s", c1.HostPath, c2.HostPath)
	}
	got2, err := st.CellPath("ws/ws-2")
	if err != nil || got2 != c2.HostPath {
		t.Fatalf("CellPath(ws/ws-2) = (%q, %v), want %q", got2, err, c2.HostPath)
	}
	if err := st.DeleteCell(ctx, "ws/ws-2"); err != nil {
		t.Fatalf("DeleteCell ws/ws-2: %v", err)
	}
}
