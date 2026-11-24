package cladefs

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"

	cladefsdv1 "github.com/semistrict/cladefs/go/cladefsd/v1"
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
	cells      map[string]uint64
	synced     map[string]bool
	peers      []string
	nextCell   uint64
	nextVer    uint64
	deleteBusy map[string]bool
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
