package worker

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	cladefsdv1 "github.com/semistrict/cladefs/go/cladefsd/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/semistrict/hotbed/internal/cladefs"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/guestd"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/storage"
	"github.com/semistrict/hotbed/internal/vm"
)

type strictVMCladefsClient struct {
	cladefsdv1.CladefsdClient
	mu         sync.Mutex
	cells      map[string]uint64
	deleted    []string
	dropped    []string
	nextCellID uint64
}

func newStrictVMCladefsClient() *strictVMCladefsClient {
	return &strictVMCladefsClient{
		cells:      map[string]uint64{},
		nextCellID: 10,
	}
}

func (c *strictVMCladefsClient) ForkCell(_ context.Context, req *cladefsdv1.ForkCellRequest, _ ...grpc.CallOption) (*cladefsdv1.ForkCellResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.cells[req.GetTo()]; exists {
		return nil, status.Error(codes.AlreadyExists, "already exists (EEXIST)")
	}
	c.nextCellID++
	c.cells[req.GetTo()] = c.nextCellID
	_ = os.MkdirAll(req.GetTo(), 0o755)
	return &cladefsdv1.ForkCellResponse{Tenant: req.GetTenant(), Cell: c.nextCellID}, nil
}

func (c *strictVMCladefsClient) DeleteCell(_ context.Context, req *cladefsdv1.DeleteCellRequest, _ ...grpc.CallOption) (*cladefsdv1.DeleteCellResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleted = append(c.deleted, req.GetPath())
	delete(c.cells, req.GetPath())
	_ = os.RemoveAll(req.GetPath())
	return &cladefsdv1.DeleteCellResponse{}, nil
}

func (c *strictVMCladefsClient) DropCell(_ context.Context, req *cladefsdv1.DropCellRequest, _ ...grpc.CallOption) (*cladefsdv1.DropCellResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropped = append(c.dropped, req.GetPath())
	delete(c.cells, req.GetPath())
	_ = os.RemoveAll(req.GetPath())
	return &cladefsdv1.DropCellResponse{}, nil
}

type fakeVMLauncher struct {
	mu      sync.Mutex
	running map[string]bool
	stopped []string
}

func newFakeVMLauncher() *fakeVMLauncher {
	return &fakeVMLauncher{running: map[string]bool{}}
}

func (l *fakeVMLauncher) Start(_ context.Context, s vm.ProcessSpec) error {
	l.mu.Lock()
	l.running[s.Name] = true
	l.mu.Unlock()
	return nil
}

func (l *fakeVMLauncher) Stop(_ context.Context, name string) error {
	l.mu.Lock()
	delete(l.running, name)
	l.stopped = append(l.stopped, name)
	l.mu.Unlock()
	return nil
}

func (l *fakeVMLauncher) Status(_ context.Context, name string) (vm.ProcessStatus, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running[name] {
		return vm.ProcessStatus{Loaded: true, Running: true}, nil
	}
	return ProcessStatusNotFound(), nil
}

func ProcessStatusNotFound() vm.ProcessStatus {
	return vm.ProcessStatus{}
}

func (l *fakeVMLauncher) List(_ context.Context, _ string) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for name := range l.running {
		if strings.HasPrefix(name, "hotbed-vm-") {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out, nil
}

func startFakeGuestdDialer(t *testing.T) func(string) guestd.DialFunc {
	t.Helper()
	sockDir, err := os.MkdirTemp("/tmp", "hb-vmtest-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	ctrlSock := filepath.Join(sockDir, "ctrl.sock")
	ln, err := net.Listen("unix", ctrlSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	srv, err := guestd.NewServer(guestd.ServerConfig{WorkDir: t.TempDir(), StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.ServeControl(ctx, ln) }()
	return func(_ string) guestd.DialFunc {
		return guestd.UnixMapDialer(map[uint32]string{guestd.ControlPort: ctrlSock})
	}
}

func newTestVMBackend(t *testing.T, api *strictVMCladefsClient, fl *fakeVMLauncher) (*VMBackend, string) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	cellRoot := filepath.Join(tmp, "cells")
	if err := os.MkdirAll(cellRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := cladefs.NewStore(ctx, cladefs.Config{
		CellRoot: cellRoot,
		VMSock:   "/run/cladefsd/vm.sock",
	}, api, nil, nil)
	if err != nil {
		t.Fatalf("cladefs.NewStore: %v", err)
	}
	runDir, err := os.MkdirTemp("/tmp", "hb-vmrun-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })
	mgr, err := vm.NewManager(vm.Config{
		RunDir:          runDir,
		DisableNetwork:  true,
		BootTimeout:     5 * time.Second,
		ShutdownTimeout: 200 * time.Millisecond,
	}, vm.WithLauncher(fl), vm.WithDialerFactory(startFakeGuestdDialer(t)), vm.WithToolsDiskBuilder(func(cacheDir, _ string) (string, error) {
		return filepath.Join(cacheDir, "guestd-test.ext4"), nil
	}))
	if err != nil {
		t.Fatalf("vm.NewManager: %v", err)
	}
	b := &VMBackend{
		workerDir:      tmp,
		mgr:            mgr,
		baseImages:     map[string]cladefs.CellRef{},
		wsCells:        map[string]cladefs.CellRef{},
		projectBases:   map[string]*vmProjectBase{},
		sharedBases:    map[string]*pb.CladefsCell{},
		activeBuilds:   map[string]struct{}{},
		decommissioned: map[uint64]bool{},
	}
	b.store.Store(store)
	b.rt = runtime.NewGuestRuntime(b)
	return b, cellRoot
}

func TestVMBackendPrepareWorkspaceIdempotentAndGuardsLiveVM(t *testing.T) {
	ctx := context.Background()
	api := newStrictVMCladefsClient()
	fl := newFakeVMLauncher()
	b, cellRoot := newTestVMBackend(t, api, fl)

	baseRef := cladefs.CellRef{Tenant: 1, Cell: 100, Version: 5}
	stalePath := filepath.Join(cellRoot, "ws", "uhuvox5f")

	// Seed a stale ws/uhuvox5f cell with NO live VM: PrepareWorkspace must clear
	// it instead of failing with EEXIST.
	api.cells[stalePath] = 999
	if err := os.MkdirAll(stalePath, 0o755); err != nil {
		t.Fatal(err)
	}
	cell, err := b.PrepareWorkspace(ctx, "uhuvox5f", baseRef, 1)
	if err != nil {
		t.Fatalf("PrepareWorkspace with stale cell failed: %v", err)
	}
	if cell.GetCell() == 0 {
		t.Fatalf("expected non-zero forked cell, got %+v", cell)
	}

	// While uhuvox5f's VM is alive, PrepareWorkspace must refuse with ErrBusy
	// and MUST NOT delete the live workspace's cell.
	api.mu.Lock()
	deletedBefore := len(api.deleted)
	api.mu.Unlock()
	if _, err := b.PrepareWorkspace(ctx, "uhuvox5f", baseRef, 2); !errors.Is(err, storage.ErrBusy) {
		t.Fatalf("PrepareWorkspace while VM alive: err = %v, want storage.ErrBusy", err)
	}
	api.mu.Lock()
	deletedAfter := len(api.deleted)
	api.mu.Unlock()
	if deletedAfter != deletedBefore {
		t.Fatalf("PrepareWorkspace deleted cell while live VM held ws/uhuvox5f")
	}

	// Same guard must hold even for an UNTRACKED zombie VM unit in systemd.
	fl.mu.Lock()
	fl.running["hotbed-vm-zombie-ws"] = true
	fl.mu.Unlock()
	if _, err := b.PrepareWorkspace(ctx, "zombie-ws", baseRef, 3); !errors.Is(err, storage.ErrBusy) {
		t.Fatalf("PrepareWorkspace while untracked zombie VM alive: err = %v, want storage.ErrBusy", err)
	}
}

func TestEngineSweepOrphansAndPeriodicReconcileVMZombies(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	store := &fake.Store{Root: t.TempDir()}
	units := &fake.Units{}
	t.Cleanup(units.Close)

	// Seed a live VM workspace in worker.db on first boot, then close engine.
	e1, err := NewEngine(ctx, EngineConfig{
		Dir: dir, NodeID: "n1", Host: "ubu1",
		Store: store, BaseImage: fake.BaseImage(store), Units: units, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}
	e1.mu.Lock()
	liveWS := &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{Id: "ws-live", Project: "cladefs", State: idle, Runtime: runtime.KindVM},
		Volume:    "ws/ws-live",
	}}
	e1.workspaces["ws-live"] = liveWS
	e1.saveWorkspace(liveWS, change{what: "ready"})
	relWS := &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{Id: "ws-released", Project: "cladefs", State: released, Runtime: runtime.KindVM},
		Volume:    "ws/ws-released",
	}}
	e1.workspaces["ws-released"] = relWS
	e1.saveWorkspace(relWS, change{what: "released"})
	e1.mu.Unlock()
	if err := e1.Close(); err != nil {
		t.Fatal(err)
	}

	api := newStrictVMCladefsClient()
	fl := newFakeVMLauncher()
	vmb, cellRoot := newTestVMBackend(t, api, fl)

	// Live workspace VM + cell must survive startup sweep.
	if _, err := vmb.PrepareWorkspace(ctx, "ws-live", cladefs.CellRef{Tenant: 1, Cell: 1, Version: 1}, 1); err != nil {
		t.Fatalf("PrepareWorkspace ws-live: %v", err)
	}

	// Simulate surviving dir-less zombie units from prior worker incarnation:
	// two base-build VMs, a prov VM, a released workspace VM, and a stale ws cell.
	fl.mu.Lock()
	fl.running["hotbed-vm-base-cladefs-vm-b470a516"] = true
	fl.running["hotbed-vm-base-cladefs-vm-cba75ac3"] = true
	fl.running["hotbed-vm-prov-baseimg-deadbeef"] = true
	fl.running["hotbed-vm-ws-released"] = true
	fl.mu.Unlock()
	orphanCell := filepath.Join(cellRoot, "ws", "ws-orphan-disk")
	if err := os.MkdirAll(orphanCell, 0o755); err != nil {
		t.Fatal(err)
	}
	api.cells[orphanCell] = 500
	staleBaseCell := filepath.Join(cellRoot, "bases", "cladefs", "cladefs-vm-b470a516")
	if err := os.MkdirAll(staleBaseCell, 0o755); err != nil {
		t.Fatal(err)
	}
	api.cells[staleBaseCell] = 501

	// Restart engine with VMBackend: NewEngine runs sweepOrphans -> reconcileVMOrphans(startup=true).
	e2, err := NewEngine(ctx, EngineConfig{
		Dir: dir, NodeID: "n1", Host: "ubu1",
		Store: store, BaseImage: fake.BaseImage(store), Units: units, System: fake.NewSystem(),
		VM: vmb,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e2.Close() })

	// Every zombie unit must be stopped; live workspace VM must stay running.
	for _, dead := range []string{
		"hotbed-vm-base-cladefs-vm-b470a516",
		"hotbed-vm-base-cladefs-vm-cba75ac3",
		"hotbed-vm-prov-baseimg-deadbeef",
		"hotbed-vm-ws-released",
	} {
		if st, _ := fl.Status(ctx, dead); st.Alive() {
			t.Fatalf("zombie VM unit %s survived startup sweep", dead)
		}
	}
	if st, _ := fl.Status(ctx, "hotbed-vm-ws-live"); !st.Alive() {
		t.Fatal("live workspace VM ws-live was erroneously reaped")
	}
	if _, err := os.Stat(orphanCell); !os.IsNotExist(err) {
		t.Fatalf("expected orphan cell %s swept at startup", orphanCell)
	}
	if _, err := os.Stat(staleBaseCell); !os.IsNotExist(err) {
		t.Fatalf("expected stale local base cell %s dropped at startup", staleBaseCell)
	}

	// Periodic reconciliation (startup=false) must preserve an active in-flight
	// base build tracked in vmb.activeBuilds while reaping an untracked zombie.
	untrack := vmb.trackBuild("base-cladefs-vm-active01")
	defer untrack()
	fl.mu.Lock()
	fl.running["hotbed-vm-base-cladefs-vm-active01"] = true
	fl.running["hotbed-vm-base-cladefs-vm-escaped1"] = true
	fl.mu.Unlock()

	e2.reconcileVMOrphans(ctx, false)

	if st, _ := fl.Status(ctx, "hotbed-vm-base-cladefs-vm-active01"); !st.Alive() {
		t.Fatal("active in-flight base build VM was erroneously reaped during periodic reconcile")
	}
	if st, _ := fl.Status(ctx, "hotbed-vm-base-cladefs-vm-escaped1"); st.Alive() {
		t.Fatal("untracked zombie base VM escaped periodic reconcile")
	}
}
