package vm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/guestd"
)

type fakeFCAPI struct {
	mu          sync.Mutex
	ln          net.Listener
	srv         *http.Server
	patches     []string
	creates     []CreateSnapshotRequest
	loads       []LoadSnapshotRequest
	createResp  MemoryCellSnapshot
	createErr   string
	createCode  int
	loadErr     string
	loadCode    int
	nextVersion atomic.Uint64
}

func startFakeFCAPI(t *testing.T, sockPath string) *fakeFCAPI {
	t.Helper()
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen fake fc api %s: %v", sockPath, err)
	}
	f := &fakeFCAPI{
		ln:         ln,
		createResp: MemoryCellSnapshot{Tenant: 7, Cell: 42},
	}
	f.nextVersion.Store(1)
	mux := http.NewServeMux()
	mux.HandleFunc("/vm", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			State string `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.patches = append(f.patches, body.State)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/snapshot/create", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req CreateSnapshotRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.creates = append(f.creates, req)
		errMsg := f.createErr
		errCode := f.createCode
		resp := f.createResp
		f.mu.Unlock()
		if errMsg != "" {
			if errCode == 0 {
				errCode = http.StatusBadRequest
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(errCode)
			_ = json.NewEncoder(w).Encode(fcErrorBody{FaultMessage: errMsg})
			return
		}
		if req.SnapshotPath != "" {
			_ = os.WriteFile(req.SnapshotPath, []byte("fake-snapshot-state"), 0o644)
		}
		if (req.MemoryCell != nil && req.MemoryCell.Ephemeral) || resp.Ephemeral {
			resp.Ephemeral = true
			resp.Version = 0
		} else {
			resp.Version = f.nextVersion.Add(1) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/snapshot/load", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req LoadSnapshotRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.loads = append(f.loads, req)
		errMsg := f.loadErr
		errCode := f.loadCode
		f.mu.Unlock()
		if errMsg != "" {
			if errCode == 0 {
				errCode = http.StatusBadRequest
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(errCode)
			_ = json.NewEncoder(w).Encode(fcErrorBody{FaultMessage: errMsg})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	f.srv = &http.Server{Handler: mux}
	go func() { _ = f.srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = f.srv.Close()
		_ = ln.Close()
	})
	return f
}

func startSnapshotTestGuestd(t *testing.T, onFreeze, onThaw func(string) error) (guestd.DialFunc, func()) {
	t.Helper()
	sockDir, err := os.MkdirTemp("/tmp", "vms-")
	if err != nil {
		t.Fatal(err)
	}
	ctrlSock := filepath.Join(sockDir, "ctrl.sock")
	ln, err := net.Listen("unix", ctrlSock)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := guestd.NewServer(guestd.ServerConfig{
		WorkDir:  t.TempDir(),
		StateDir: t.TempDir(),
		FreezeFS: onFreeze,
		ThawFS:   onThaw,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.ServeControl(ctx, ln) }()
	cleanup := func() {
		cancel()
		_ = ln.Close()
		_ = os.RemoveAll(sockDir)
	}
	t.Cleanup(cleanup)
	return guestd.UnixMapDialer(map[uint32]string{guestd.ControlPort: ctrlSock}), cleanup
}

func TestSuspendAndResumeMemoryCell(t *testing.T) {
	runDir, err := os.MkdirTemp("/tmp", "vmr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })

	var events []string
	var evMu sync.Mutex
	record := func(ev string) {
		evMu.Lock()
		events = append(events, ev)
		evMu.Unlock()
	}

	dialFn, _ := startSnapshotTestGuestd(
		t,
		func(p string) error { record("freeze:" + p); return nil },
		func(p string) error { record("thaw:" + p); return nil },
	)

	fl := newFakeLauncher()
	var fcMu sync.Mutex
	var fcServers []*fakeFCAPI
	var launchArgs [][]string
	fl.onStart = func(s ProcessSpec) error {
		fcMu.Lock()
		launchArgs = append(launchArgs, slices.Clone(s.Args))
		fcMu.Unlock()
		// Extract --api-sock value and start fake Firecracker HTTP API on it.
		for i := 0; i+1 < len(s.Args); i++ {
			if s.Args[i] == "--api-sock" {
				api := startFakeFCAPI(t, s.Args[i+1])
				fcMu.Lock()
				fcServers = append(fcServers, api)
				fcMu.Unlock()
			}
		}
		return nil
	}

	mgr, err := NewManager(Config{
		RunDir:          runDir,
		KernelPath:      "/vmlinux",
		DisableNetwork:  true,
		BootTimeout:     5 * time.Second,
		ShutdownTimeout: time.Second,
	},
		WithLauncher(fl),
		WithDialerFactory(func(string) guestd.DialFunc { return dialFn }),
		fakeToolsDiskOpt(t),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx := t.Context()
	inst, err := mgr.Start(ctx, LaunchSpec{
		ID:            "ws-snap",
		CellHostPath:  "/mnt/cells/ws/ws-snap",
		CladefsVMSock: "/run/cladefsd/tenant-7.sock",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if inst.APISock == "" {
		t.Fatal("expected APISock to be populated on Start")
	}
	// Verify cold boot passed --api-sock and --config-file (not --no-api).
	if !slices.Contains(launchArgs[0], "--api-sock") || !slices.Contains(launchArgs[0], "--config-file") {
		t.Fatalf("cold boot args = %v, want --api-sock and --config-file", launchArgs[0])
	}

	// 1. First suspend (cold-booted VM): must pass path_on_host + cladefsd_socket + ephemeral: true.
	susp1, err := mgr.Suspend(ctx, "ws-snap", "/mnt/cells/mem/ws-snap/g1", 512<<20)
	if err != nil {
		t.Fatalf("Suspend #1: %v", err)
	}
	if !mgr.IsSuspended("ws-snap") {
		t.Fatal("expected IsSuspended=true after Suspend")
	}
	if _, err := mgr.ClientFor("ws-snap"); err == nil {
		t.Fatal("expected ClientFor to fail while suspended")
	}
	if susp1.WorkingSetBytes != 512<<20 || susp1.MemoryCell.Cell != 42 || !susp1.MemoryCell.Ephemeral || susp1.MemoryCell.Version != 0 {
		t.Fatalf("unexpected susp1: %+v", susp1)
	}
	fc1 := fcServers[0]
	fc1.mu.Lock()
	if !slices.Equal(fc1.patches, []string{"Paused"}) {
		t.Fatalf("fc1 patches = %v, want [Paused]", fc1.patches)
	}
	if len(fc1.creates) != 1 {
		t.Fatalf("fc1 creates len = %d, want 1", len(fc1.creates))
	}
	if got := fc1.creates[0].MemoryCell; got == nil || got.PathOnHost != "/mnt/cells/mem/ws-snap/g1" || got.CladefsdSocket != "/run/cladefsd/tenant-7.sock" || !got.Ephemeral {
		t.Fatalf("cold suspend memory_cell = %+v, want path_on_host + cladefsd_socket + ephemeral=true", got)
	}
	fc1.mu.Unlock()

	// Verify Start refuses to cold-boot over a suspended VM.
	if _, err := mgr.Start(ctx, LaunchSpec{
		ID:            "ws-snap",
		CellHostPath:  "/mnt/cells/ws/ws-snap",
		CladefsVMSock: "/run/cladefsd/tenant-7.sock",
	}); err == nil {
		t.Fatal("expected Start on suspended VM to fail")
	}

	// 2. Resume ephemeral memory cell in place: bare Firecracker (--api-sock, NO --config-file)
	// + PUT /snapshot/load reopening g1 in place with ephemeral: true + ThawFS.
	resumed, err := mgr.Resume(ctx, "ws-snap", "")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if mgr.IsSuspended("ws-snap") {
		t.Fatal("expected IsSuspended=false after Resume")
	}
	if !resumed.FromMemoryCell || resumed.MemoryCellPath != "/mnt/cells/mem/ws-snap/g1" {
		t.Fatalf("unexpected resumed instance: FromMemoryCell=%v MemoryCellPath=%q", resumed.FromMemoryCell, resumed.MemoryCellPath)
	}
	if slices.Contains(launchArgs[1], "--config-file") || !slices.Contains(launchArgs[1], "--api-sock") {
		t.Fatalf("resume launch args = %v, want --api-sock without --config-file", launchArgs[1])
	}
	fc2 := fcServers[1]
	fc2.mu.Lock()
	if len(fc2.loads) != 1 {
		t.Fatalf("fc2 loads len = %d, want 1", len(fc2.loads))
	}
	loadReq := fc2.loads[0]
	if loadReq.MemBackend.BackendType != "Cell" || loadReq.MemBackend.BackendPath != "/mnt/cells/mem/ws-snap/g1" {
		t.Fatalf("unexpected mem_backend: %+v", loadReq.MemBackend)
	}
	if loadReq.MemoryCell == nil || loadReq.MemoryCell.Tenant != 7 || loadReq.MemoryCell.Cell != 42 || loadReq.MemoryCell.Version != 0 || !loadReq.MemoryCell.Ephemeral {
		t.Fatalf("unexpected memory_cell on load: %+v", loadReq.MemoryCell)
	}
	if !loadReq.ResumeVM || len(loadReq.CladefsOverrides) != 1 || loadReq.CladefsOverrides[0].PathOnHost != "/mnt/cells/ws/ws-snap" {
		t.Fatalf("unexpected loadReq: %+v", loadReq)
	}
	fc2.mu.Unlock()

	evMu.Lock()
	if !slices.Equal(events, []string{"freeze:/", "thaw:/"}) {
		t.Fatalf("guest fs events = %v, want [freeze:/ thaw:/]", events)
	}
	evMu.Unlock()

	// 3. Second suspend (already restored from ephemeral memory cell): MUST pass empty memory_cell: {}!
	fc2.mu.Lock()
	fc2.createResp.Ephemeral = true
	fc2.mu.Unlock()
	susp2, err := mgr.Suspend(ctx, "ws-snap", "/mnt/cells/mem/ws-snap/should-be-ignored", 384<<20)
	if err != nil {
		t.Fatalf("Suspend #2: %v", err)
	}
	fc2.mu.Lock()
	if len(fc2.creates) != 1 {
		t.Fatalf("fc2 creates len = %d, want 1", len(fc2.creates))
	}
	if got := fc2.creates[0].MemoryCell; got == nil || got.PathOnHost != "" || got.CladefsdSocket != "" || got.Ephemeral {
		t.Fatalf("incremental re-suspend memory_cell = %+v, want empty {}", got)
	}
	fc2.mu.Unlock()
	if susp2.MemoryCellPath != "/mnt/cells/mem/ws-snap/g1" || !susp2.MemoryCell.Ephemeral {
		t.Fatalf("susp2 = %+v, want in-place ephemeral g1", susp2)
	}
	memPaths := mgr.MemoryCellPaths("ws-snap")
	if !slices.Equal(memPaths, []string{"/mnt/cells/mem/ws-snap/g1"}) {
		t.Fatalf("MemoryCellPaths = %v, want [g1]", memPaths)
	}
}

func TestSuspendRollbackOnUnsupportedSnapshotError(t *testing.T) {
	runDir, err := os.MkdirTemp("/tmp", "vmrb-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })

	var freezes, thaws atomic.Int32
	dialFn, _ := startSnapshotTestGuestd(
		t,
		func(string) error { freezes.Add(1); return nil },
		func(string) error { thaws.Add(1); return nil },
	)

	fl := newFakeLauncher()
	var api *fakeFCAPI
	fl.onStart = func(s ProcessSpec) error {
		for i := 0; i+1 < len(s.Args); i++ {
			if s.Args[i] == "--api-sock" {
				api = startFakeFCAPI(t, s.Args[i+1])
				api.createErr = "ephemeral cells do not support MakeVersion (ENOTSUP)"
			}
		}
		return nil
	}

	mgr, err := NewManager(Config{
		RunDir:          runDir,
		KernelPath:      "/vmlinux",
		DisableNetwork:  true,
		BootTimeout:     5 * time.Second,
		ShutdownTimeout: time.Second,
	},
		WithLauncher(fl),
		WithDialerFactory(func(string) guestd.DialFunc { return dialFn }),
		fakeToolsDiskOpt(t),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx := t.Context()
	if _, err := mgr.Start(ctx, LaunchSpec{
		ID:            "ws-rb",
		CellHostPath:  "/mnt/cells/ws/ws-rb",
		CladefsVMSock: "/run/cladefsd/vm.sock",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	_, err = mgr.Suspend(ctx, "ws-rb", "/mnt/cells/mem/ws-rb/g1", 100<<20)
	if !errors.Is(err, ErrSuspendUnsupported) {
		t.Fatalf("Suspend err = %v, want ErrSuspendUnsupported", err)
	}
	if mgr.IsSuspended("ws-rb") {
		t.Fatal("VM must not be marked suspended after failed CreateSnapshot")
	}
	if mgr.Get("ws-rb") == nil {
		t.Fatal("VM must remain live in instances after failed CreateSnapshot")
	}
	if freezes.Load() != 1 || thaws.Load() != 1 {
		t.Fatalf("expected freeze=1 thaw=1 rollback, got freeze=%d thaw=%d", freezes.Load(), thaws.Load())
	}
	api.mu.Lock()
	if !slices.Equal(api.patches, []string{"Paused", "Resumed"}) {
		t.Fatalf("expected [Paused Resumed] rollback patches, got %v", api.patches)
	}
	api.mu.Unlock()
}

func TestResumeRollbackAndWorkerRestartReconnect(t *testing.T) {
	runDir, err := os.MkdirTemp("/tmp", "vmrc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })

	dialFn, _ := startSnapshotTestGuestd(
		t,
		func(string) error { return nil },
		func(string) error { return nil },
	)

	fl := newFakeLauncher()
	failNextLoad := true
	fl.onStart = func(s ProcessSpec) error {
		for i := 0; i+1 < len(s.Args); i++ {
			if s.Args[i] == "--api-sock" {
				api := startFakeFCAPI(t, s.Args[i+1])
				if !slices.Contains(s.Args, "--config-file") && failNextLoad {
					api.loadErr = "transient backend error"
					api.loadCode = http.StatusInternalServerError
				}
			}
		}
		return nil
	}

	makeMgr := func() *Manager {
		m, err := NewManager(Config{
			RunDir:          runDir,
			KernelPath:      "/vmlinux",
			DisableNetwork:  true,
			BootTimeout:     5 * time.Second,
			ShutdownTimeout: time.Second,
		},
			WithLauncher(fl),
			WithDialerFactory(func(string) guestd.DialFunc { return dialFn }),
			fakeToolsDiskOpt(t),
		)
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		return m
	}

	ctx := t.Context()
	mgr1 := makeMgr()
	if _, err := mgr1.Start(ctx, LaunchSpec{
		ID:            "ws-rec",
		CellHostPath:  "/mnt/cells/ws/ws-rec",
		CladefsVMSock: "/run/cladefsd/vm.sock",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := mgr1.Suspend(ctx, "ws-rec", "/mnt/cells/mem/ws-rec/g1", 256<<20); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	// Simulate worker restart: fresh Manager reconnects the suspended VM.
	mgr2 := makeMgr()
	n, err := mgr2.Reconnect(ctx)
	if err != nil || n != 1 {
		t.Fatalf("Reconnect = (%d, %v), want (1, nil)", n, err)
	}
	if !mgr2.IsSuspended("ws-rec") {
		t.Fatal("expected ws-rec to be restored as suspended after Reconnect")
	}

	// ReconcileOrphans keeps owned suspended VMs intact.
	reaped, _ := mgr2.ReconcileOrphans(ctx, map[string]bool{"ws-rec": true})
	if len(reaped) != 0 || !mgr2.IsSuspended("ws-rec") {
		t.Fatalf("owned suspended VM was unexpectedly reaped: %v", reaped)
	}

	// Failed Resume leaves suspended state intact for retry.
	if _, err := mgr2.Resume(ctx, "ws-rec", "/mnt/cells/mem/ws-rec/g2"); err == nil {
		t.Fatal("expected first Resume to fail with simulated load error")
	}
	if !mgr2.IsSuspended("ws-rec") {
		t.Fatal("expected ws-rec to remain suspended after failed Resume")
	}

	// Retry Resume succeeds once backend error clears (ephemeral reopens g1 in place).
	failNextLoad = false
	if _, err := mgr2.Resume(ctx, "ws-rec", ""); err != nil {
		t.Fatalf("Resume retry: %v", err)
	}
	if mgr2.IsSuspended("ws-rec") || mgr2.Get("ws-rec") == nil {
		t.Fatal("expected ws-rec to be live after Resume retry")
	}

	// Suspend again and verify Stop discards without starting Firecracker.
	if _, err := mgr2.Suspend(ctx, "ws-rec", "", 128<<20); err != nil {
		t.Fatalf("Suspend #2: %v", err)
	}
	stopsBefore := len(fl.stopCalls)
	if err := mgr2.Stop(ctx, "ws-rec", true); err != nil {
		t.Fatalf("Stop suspended VM: %v", err)
	}
	if mgr2.IsSuspended("ws-rec") || len(fl.stopCalls) != stopsBefore {
		t.Fatalf("Stop on suspended VM should discard without touching launcher (isSusp=%v, stopCalls=%d->%d)",
			mgr2.IsSuspended("ws-rec"), stopsBefore, len(fl.stopCalls))
	}
}
