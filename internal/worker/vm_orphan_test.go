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

	"github.com/semistrict/hotbed/internal/cladefs"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/guestd"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/storage"
	"github.com/semistrict/hotbed/internal/unit"
	"github.com/semistrict/hotbed/internal/vm"
)

type strictVMCladefsClient struct {
	cladefsdv1.CladefsdClient
	mu          sync.Mutex
	cells       map[uint64]bool
	forkReqs    []*cladefsdv1.ForkCellRequest
	createReqs  []*cladefsdv1.CreateCellRequest
	deleted     []uint64
	dropped     []uint64
	nextCellID  uint64
	nextVersion uint64
	forkHook    func(context.Context, *cladefsdv1.ForkCellRequest) error
}

func newStrictVMCladefsClient() *strictVMCladefsClient {
	return &strictVMCladefsClient{
		cells:      map[uint64]bool{},
		nextCellID: 10,
	}
}

func (c *strictVMCladefsClient) ForkCell(ctx context.Context, req *cladefsdv1.ForkCellRequest, _ ...grpc.CallOption) (*cladefsdv1.ForkCellResponse, error) {
	c.mu.Lock()
	hook := c.forkHook
	c.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, req); err != nil {
			return nil, err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.forkReqs = append(c.forkReqs, req)
	c.nextCellID++
	c.cells[c.nextCellID] = true
	return &cladefsdv1.ForkCellResponse{Tenant: req.GetTenant(), Cell: c.nextCellID}, nil
}

func (c *strictVMCladefsClient) CreateCell(_ context.Context, req *cladefsdv1.CreateCellRequest, _ ...grpc.CallOption) (*cladefsdv1.CreateCellResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.createReqs = append(c.createReqs, req)
	cell := req.GetCell()
	if cell == 0 {
		c.nextCellID++
		cell = c.nextCellID
	}
	c.cells[cell] = true
	return &cladefsdv1.CreateCellResponse{Tenant: req.GetTenant(), Cell: cell}, nil
}

func (c *strictVMCladefsClient) SyncCell(_ context.Context, _ *cladefsdv1.SyncCellRequest, _ ...grpc.CallOption) (*cladefsdv1.SyncCellResponse, error) {
	return &cladefsdv1.SyncCellResponse{}, nil
}

func (c *strictVMCladefsClient) MakeVersion(_ context.Context, _ *cladefsdv1.MakeVersionRequest, _ ...grpc.CallOption) (*cladefsdv1.MakeVersionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextVersion++
	return &cladefsdv1.MakeVersionResponse{Version: c.nextVersion}, nil
}

func (c *strictVMCladefsClient) DeleteCell(_ context.Context, req *cladefsdv1.DeleteCellRequest, _ ...grpc.CallOption) (*cladefsdv1.DeleteCellResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleted = append(c.deleted, req.GetCell())
	delete(c.cells, req.GetCell())
	return &cladefsdv1.DeleteCellResponse{}, nil
}

func (c *strictVMCladefsClient) DropCell(_ context.Context, req *cladefsdv1.DropCellRequest, _ ...grpc.CallOption) (*cladefsdv1.DropCellResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropped = append(c.dropped, req.GetCell())
	delete(c.cells, req.GetCell())
	return &cladefsdv1.DropCellResponse{}, nil
}

func (c *strictVMCladefsClient) GetCounters(_ context.Context, _ *cladefsdv1.GetCountersRequest, _ ...grpc.CallOption) (*cladefsdv1.GetCountersResponse, error) {
	return &cladefsdv1.GetCountersResponse{}, nil
}

func (c *strictVMCladefsClient) GetStats(_ context.Context, _ *cladefsdv1.GetStatsRequest, _ ...grpc.CallOption) (*cladefsdv1.GetStatsResponse, error) {
	return &cladefsdv1.GetStatsResponse{}, nil
}

type fakeVMLauncher struct {
	mu      sync.Mutex
	running map[string]bool
	stopped []string
	// starting, when set, is told of each Start, which then waits for gate.
	starting chan string
	gate     chan struct{}
}

func newFakeVMLauncher() *fakeVMLauncher {
	return &fakeVMLauncher{running: map[string]bool{}}
}

func (l *fakeVMLauncher) Start(_ context.Context, s vm.ProcessSpec) error {
	if l.starting != nil {
		l.starting <- s.Name
		<-l.gate
	}
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
	// Use a 0755 base under /tmp (and chown workDir when running as root) so
	// guestd's unprivileged command user can chdir into WorkDir under sudo
	// test runs (test-linux-root), while keeping socket paths <104 bytes.
	sockDir, err := os.MkdirTemp("/tmp", "hb-vmtest-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	if err := os.Chmod(sockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	guestWork := filepath.Join(sockDir, "work")
	guestState := filepath.Join(sockDir, "state")
	guestRun := filepath.Join(sockDir, "run")
	for _, d := range []string{guestWork, guestState, guestRun} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if os.Geteuid() == 0 {
		sp := fake.BaseImage(nil).Spec.Normalized()
		_ = os.Chown(guestWork, sp.UID, sp.GID)
	}
	ctrlSock := filepath.Join(sockDir, "ctrl.sock")
	ln, err := net.Listen("unix", ctrlSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	srv, err := guestd.NewServer(guestd.ServerConfig{
		WorkDir:  guestWork,
		StateDir: guestState,
		RunDir:   guestRun,
	})
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
	b := newVMBackend(tmp, statedir.VMConfig{})
	b.mgr = mgr
	b.activeBuilds = map[string]struct{}{}
	b.store.Store(store)
	b.rt = runtime.NewGuestRuntime(b)
	return b, cellRoot
}

func TestVMBackendPrepareWorkspaceIdempotentAndGuardsLiveVM(t *testing.T) {
	ctx := context.Background()
	api := newStrictVMCladefsClient()
	fl := newFakeVMLauncher()
	b, _ := newTestVMBackend(t, api, fl)

	baseRef := cladefs.CellRef{Tenant: 1, Cell: 100, Version: 5}

	// Seed a stale wsCells entry with NO live VM: PrepareWorkspace must delete it
	// before forking a fresh cell.
	api.cells[999] = true
	b.wsCells["uhuvox5f"] = cladefs.CellRef{Tenant: 1, Cell: 999, HostPath: cladefs.FormatCellID(1, 999)}
	cell, err := b.PrepareWorkspace(ctx, "uhuvox5f", baseRef, 1, false)
	if err != nil {
		t.Fatalf("PrepareWorkspace with stale cell failed: %v", err)
	}
	if cell.GetCell() == 0 {
		t.Fatalf("expected non-zero forked cell, got %+v", cell)
	}
	if api.cells[999] {
		t.Fatal("expected stale cell 999 to be deleted")
	}

	// While uhuvox5f's VM is alive, PrepareWorkspace must refuse with ErrBusy
	// and MUST NOT delete the live workspace's cell.
	api.mu.Lock()
	deletedBefore := len(api.deleted)
	api.mu.Unlock()
	if _, err := b.PrepareWorkspace(ctx, "uhuvox5f", baseRef, 2, false); !errors.Is(err, storage.ErrBusy) {
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
	if _, err := b.PrepareWorkspace(ctx, "zombie-ws", baseRef, 3, false); !errors.Is(err, storage.ErrBusy) {
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
	vmb, _ := newTestVMBackend(t, api, fl)

	// Live workspace VM + cell must survive startup sweep.
	if _, err := vmb.PrepareWorkspace(ctx, "ws-live", cladefs.CellRef{Tenant: 1, Cell: 1, Version: 1}, 1, false); err != nil {
		t.Fatalf("PrepareWorkspace ws-live: %v", err)
	}

	// Simulate surviving dir-less zombie units from prior worker incarnation:
	// two base-build VMs, a prov VM, and a released workspace VM with a tracked cell.
	fl.mu.Lock()
	fl.running["hotbed-vm-base-cladefs-vm-b470a516"] = true
	fl.running["hotbed-vm-base-cladefs-vm-cba75ac3"] = true
	fl.running["hotbed-vm-prov-baseimg-deadbeef"] = true
	fl.running["hotbed-vm-ws-released"] = true
	fl.mu.Unlock()
	api.cells[500] = true
	vmb.wsCells["ws-released"] = cladefs.CellRef{Tenant: 1, Cell: 500, HostPath: cladefs.FormatCellID(1, 500)}

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
	if api.cells[500] {
		t.Fatalf("expected orphan cell 500 swept at startup")
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

// A VM up at another size than a command asks (booted for a sync, say) boots
// again at that size; one asking no size takes the VM as it is.
func TestVMBackendEnsureVMBootsAgainAtTheSizeAsked(t *testing.T) {
	ctx := context.Background()
	api := newStrictVMCladefsClient()
	fl := newFakeVMLauncher()
	b, _ := newTestVMBackend(t, api, fl)
	if _, err := b.PrepareWorkspace(ctx, "sizews", cladefs.CellRef{Tenant: 1, Cell: 100, Version: 5}, 1, false); err != nil {
		t.Fatalf("PrepareWorkspace: %v", err)
	}
	t.Cleanup(func() { _ = b.mgr.Stop(context.Background(), "sizews", true) })
	stops := func() int {
		fl.mu.Lock()
		defer fl.mu.Unlock()
		return len(fl.stopped)
	}

	if _, err := b.EnsureVM(ctx, "sizews", unit.VMBoot{}); err != nil {
		t.Fatalf("EnsureVM at no size: %v", err)
	}
	if n := stops(); n != 0 {
		t.Fatalf("EnsureVM at no size stopped %d VMs, want 0", n)
	}

	if _, err := b.EnsureVM(ctx, "sizews", unit.VMBoot{VCPUs: 3, MemMiB: 1536}); err != nil {
		t.Fatalf("EnsureVM at 3 vCPUs, 1536 MiB: %v", err)
	}
	if inst := b.mgr.Get("sizews"); inst == nil || inst.VCPUs != 3 || inst.MemMiB != 1536 {
		t.Fatalf("VM after EnsureVM at 3 vCPUs, 1536 MiB = %+v", inst)
	}
	if n := stops(); n != 1 {
		t.Fatalf("EnsureVM at another size stopped %d VMs, want 1", n)
	}

	// At the size it has, it stays.
	if _, err := b.EnsureVM(ctx, "sizews", unit.VMBoot{VCPUs: 3}); err != nil {
		t.Fatalf("EnsureVM at 3 vCPUs again: %v", err)
	}
	if n := stops(); n != 1 {
		t.Fatalf("EnsureVM at its own size stopped %d VMs, want still 1", n)
	}
}

// Callers that find a VM down at once (a file list and a file read, say)
// wait for one boot rather than fail on the boot in flight.
func TestVMBackendEnsureVMBootsOnceForConcurrentCallers(t *testing.T) {
	ctx := context.Background()
	api := newStrictVMCladefsClient()
	fl := newFakeVMLauncher()
	b, _ := newTestVMBackend(t, api, fl)
	if _, err := b.PrepareWorkspace(ctx, "twows", cladefs.CellRef{Tenant: 1, Cell: 100, Version: 5}, 1, false); err != nil {
		t.Fatalf("PrepareWorkspace: %v", err)
	}
	t.Cleanup(func() { _ = b.mgr.Stop(context.Background(), "twows", true) })
	if err := b.mgr.Stop(ctx, "twows", true); err != nil {
		t.Fatalf("stop: %v", err)
	}

	fl.mu.Lock()
	fl.starting, fl.gate = make(chan string, 2), make(chan struct{})
	fl.mu.Unlock()
	errs := make(chan error, 2)
	go func() { _, err := b.EnsureVM(ctx, "twows", unit.VMBoot{}); errs <- err }()
	<-fl.starting // the first boot is in flight
	go func() { _, err := b.EnsureVM(ctx, "twows", unit.VMBoot{}); errs <- err }()
	close(fl.gate)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("EnsureVM beside a boot in flight: %v", err)
		}
	}
	select {
	case name := <-fl.starting:
		t.Fatalf("second boot %s, want one", name)
	default:
	}
}

// A worker that restarts boots an idle VM workspace again on the cell its
// saved record names: the in-memory cell map starts empty, and the record
// is what lasts.
func TestEngineRestartBootsSavedVMWorkspaceOnItsCell(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	store := &fake.Store{Root: t.TempDir()}
	units := &fake.Units{}
	t.Cleanup(units.Close)

	e1, err := NewEngine(ctx, EngineConfig{
		Dir: dir, NodeID: "n1", Host: "vm1",
		Store: store, BaseImage: fake.BaseImage(store), Units: units, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}
	e1.mu.Lock()
	for _, w := range []*workspace{
		{rec: &pb.WorkspaceRecord{
			Workspace: &pb.Workspace{Id: "ws-idle", Project: "hotbed", State: idle, Runtime: runtime.KindVM,
				CladefsCell: &pb.CladefsCell{Tenant: 1, Cell: 42, Version: 7}},
			Volume: "ws/ws-idle",
		}},
		{rec: &pb.WorkspaceRecord{
			Workspace: &pb.Workspace{Id: "ws-released", Project: "hotbed", State: released, Runtime: runtime.KindVM,
				CladefsCell: &pb.CladefsCell{Tenant: 1, Cell: 43, Version: 7}},
			Volume: "ws/ws-released",
		}},
	} {
		e1.workspaces[w.ws().Id] = w
		e1.saveWorkspace(w, change{what: "ready"})
	}
	e1.mu.Unlock()
	if err := e1.Close(); err != nil {
		t.Fatal(err)
	}

	api := newStrictVMCladefsClient()
	api.cells[42] = true
	fl := newFakeVMLauncher()
	vmb, _ := newTestVMBackend(t, api, fl)
	e2, err := NewEngine(ctx, EngineConfig{
		Dir: dir, NodeID: "n1", Host: "vm1",
		Store: store, BaseImage: fake.BaseImage(store), Units: units, System: fake.NewSystem(),
		VM: vmb,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e2.Close() })
	t.Cleanup(func() { _ = vmb.mgr.Stop(context.Background(), "ws-idle", true) })

	if _, err := vmb.EnsureVM(ctx, "ws-idle", unit.VMBoot{}); err != nil {
		t.Fatalf("EnsureVM after restart: %v", err)
	}
	if got, want := vmb.mgr.Get("ws-idle").CellPath, cladefs.FormatCellID(1, 42); got != want {
		t.Fatalf("VM booted on cell %q, want the saved %q", got, want)
	}
	if !api.cells[42] {
		t.Fatal("restart deleted the idle workspace's cell")
	}
	if _, err := vmb.EnsureVM(ctx, "ws-released", unit.VMBoot{}); err == nil ||
		err.Error() != "vm ws-released: no cladefs cell recorded for workspace" {
		t.Fatalf("EnsureVM on a released workspace = %v, want no cell", err)
	}
}
