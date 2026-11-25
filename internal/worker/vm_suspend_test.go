package worker

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/guestd"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/sysinfo"
	"github.com/semistrict/hotbed/internal/unit"
	"github.com/semistrict/hotbed/internal/vm"
)

type suspendableTestResolver struct {
	mu           sync.Mutex
	clients      map[string]*guestd.Client
	suspended    map[string]int64
	stopped      map[string]bool
	suspendCount map[string]int
	resumeCount  map[string]int
	stopCount    map[string]int
	suspendErr   error
	stopErr      error
}

func newSuspendableTestResolver() *suspendableTestResolver {
	return &suspendableTestResolver{
		clients:      map[string]*guestd.Client{},
		suspended:    map[string]int64{},
		stopped:      map[string]bool{},
		suspendCount: map[string]int{},
		resumeCount:  map[string]int{},
		stopCount:    map[string]int{},
	}
}

func (r *suspendableTestResolver) EnsureVM(_ context.Context, wsID string) (*guestd.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, susp := r.suspended[wsID]; susp {
		delete(r.suspended, wsID)
		r.resumeCount[wsID]++
	}
	if r.stopped[wsID] {
		delete(r.stopped, wsID)
		r.resumeCount[wsID]++
	}
	c, ok := r.clients[wsID]
	if !ok {
		return nil, fmt.Errorf("no guestd client for %s", wsID)
	}
	return c, nil
}

func (r *suspendableTestResolver) ClientFor(wsID string) (*guestd.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, susp := r.suspended[wsID]; susp {
		return nil, fmt.Errorf("vm %s is suspended", wsID)
	}
	if r.stopped[wsID] {
		return nil, fmt.Errorf("vm %s is stopped", wsID)
	}
	c, ok := r.clients[wsID]
	if !ok {
		return nil, fmt.Errorf("no guestd client for %s", wsID)
	}
	return c, nil
}

func (r *suspendableTestResolver) StopVM(_ context.Context, wsID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopErr != nil {
		return r.stopErr
	}
	delete(r.suspended, wsID)
	r.stopped[wsID] = true
	r.stopCount[wsID]++
	return nil
}

func (r *suspendableTestResolver) SuspendVM(_ context.Context, wsID string, workingSetBytes int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.suspendErr != nil {
		return r.suspendErr
	}
	r.suspended[wsID] = workingSetBytes
	r.suspendCount[wsID]++
	return nil
}

func (r *suspendableTestResolver) IsVMSuspended(wsID string) (bool, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ws, ok := r.suspended[wsID]
	return ok, ws
}

func (r *suspendableTestResolver) VMExitStatus(_ context.Context, wsID string) (bool, int, int, string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, susp := r.suspended[wsID]; susp {
		return false, 0, 0, "", false
	}
	return false, 0, 0, "", false
}

func startLocalTestGuestd(t *testing.T) *guestd.Client {
	t.Helper()
	// Use a 0755 base under /tmp (and chown workDir when running as root) so
	// guestd's unprivileged command user can chdir into WorkDir under sudo
	// test runs (test-linux-root), while keeping socket paths <104 bytes.
	guestBase, err := os.MkdirTemp("/tmp", "hb-susp-gd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(guestBase) })
	if err := os.Chmod(guestBase, 0o755); err != nil {
		t.Fatal(err)
	}
	guestWork := filepath.Join(guestBase, "work")
	guestState := filepath.Join(guestBase, "state")
	guestRun := filepath.Join(guestBase, "run")
	if err := os.MkdirAll(guestWork, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(guestState, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(guestRun, 0o755); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		sp := fake.BaseImage(nil).Spec.Normalized()
		_ = os.Chown(guestWork, sp.UID, sp.GID)
	}
	ctrlSock := filepath.Join(guestBase, "ctrl.sock")
	ln, err := net.Listen("unix", ctrlSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	srv, err := guestd.NewServer(guestd.ServerConfig{
		WorkDir:  guestWork,
		StateDir: guestState,
		RunDir:   guestRun,
		FreezeFS: func(string) error { return nil },
		ThawFS:   func(string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.ServeControl(ctx, ln) }()
	c := guestd.NewClient(guestd.UnixMapDialer(map[uint32]string{guestd.ControlPort: ctrlSock}))
	if _, err := c.WaitReady(ctx); err != nil {
		t.Fatalf("guestd WaitReady: %v", err)
	}
	return c
}

func newSuspendTestEngine(t *testing.T, idleAfter time.Duration) (*Engine, *suspendableTestResolver, *fake.System, *time.Time) {
	t.Helper()
	ctx := t.Context()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	sys := fake.NewSystem()
	sys.Mem = sysinfo.Mem{Total: 16 << 30, Available: 12 << 30}
	resolver := newSuspendableTestResolver()
	rt := runtime.NewGuestRuntime(resolver)
	eng, err := NewEngine(ctx, EngineConfig{
		Dir:                dir,
		NodeID:             "n1",
		Host:               "worker1",
		Store:              store,
		BaseImage:          fake.BaseImage(store),
		Units:              &fake.Units{},
		Runtime:            rt,
		RuntimeFor:         func(string) bool { return true },
		System:             sys,
		Now:                func() time.Time { return now },
		VMIdleSuspendAfter: idleAfter,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	return eng, resolver, sys, &now
}

func seedTestVMWorkspace(t *testing.T, eng *Engine, resolver *suspendableTestResolver, wsID string, now time.Time) {
	t.Helper()
	resolver.mu.Lock()
	resolver.clients[wsID] = startLocalTestGuestd(t)
	resolver.mu.Unlock()
	eng.mu.Lock()
	eng.vmWorkspaces.Store(wsID, struct{}{})
	eng.workspaces[wsID] = &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{
			Id:           wsID,
			Project:      "demo",
			State:        idle,
			Runtime:      runtime.KindVM,
			LastActiveAt: now.UnixMilli(),
		},
		Volume: "ws/" + wsID,
	}}
	eng.mu.Unlock()
}

func TestVMParkSuspendsToMemoryCellAndResumesOnThaw(t *testing.T) {
	eng, resolver, sys, now := newSuspendTestEngine(t, -1)
	seedTestVMWorkspace(t, eng, resolver, "ws-old", *now)
	seedTestVMWorkspace(t, eng, resolver, "ws-young", *now)
	ctx := t.Context()

	// Start two long-lived commands in separate workspace VMs.
	oldCmd, err := eng.Exec(ctx, &pb.RunCommandRequest{
		Id:          "cmd-old",
		WorkspaceId: "ws-old",
		Args:        []string{"/bin/sh", "-c", "sleep 60"},
	})
	if err != nil {
		t.Fatalf("Exec old: %v", err)
	}
	eng.step(ctx)
	*now = now.Add(time.Second)

	youngCmd, err := eng.Exec(ctx, &pb.RunCommandRequest{
		Id:          "cmd-young",
		WorkspaceId: "ws-young",
		Args:        []string{"/bin/sh", "-c", "echo BEFORE_PARK; sleep 60"},
	})
	if err != nil {
		t.Fatalf("Exec young: %v", err)
	}
	eng.step(ctx)

	// Give cmd-young a recorded memory footprint, then push host memory past parkAt (93%).
	eng.mu.Lock()
	yc := eng.commands[youngCmd.Id]
	yc.cmd().MemoryBytes = 4 << 30
	yc.residentBytes = 4 << 30
	yc.residentKnown = true
	eng.mu.Unlock()

	sys.Mem = sysinfo.Mem{Total: 16 << 30, Available: 512 << 20} // ~97% used -> parks cmd-young
	eng.step(ctx)

	cur, ok := eng.Command(youngCmd.Id)
	if !ok || cur.State != parked {
		t.Fatalf("cmd-young state = %v, want parked", cur.GetState())
	}
	susp, wsBytes := resolver.IsVMSuspended("ws-young")
	if !susp || wsBytes <= 0 {
		t.Fatalf("ws-young suspended = (%v, %d), want (true, >0)", susp, wsBytes)
	}
	if susp, _ := resolver.IsVMSuspended("ws-old"); susp {
		t.Fatal("ws-old must remain running while ws-young is parked")
	}

	// Higher-priority preemption reservation for the suspended VM must be 0 bytes!
	eng.mu.Lock()
	if got := yc.parkedReservationForHigherPriority(); got != 0 {
		eng.mu.Unlock()
		t.Fatalf("parkedReservationForHigherPriority = %d, want 0 for memory-cell suspended VM", got)
	}
	eng.mu.Unlock()

	// Subsequent ticks while suspended must keep cmd-young parked (never ExitCode 137),
	// report the pre-suspend working set as SwapBytes, and never wake the VM.
	*now = now.Add(2 * time.Second)
	eng.step(ctx)
	cur, _ = eng.Command(youngCmd.Id)
	if cur.State != parked {
		t.Fatalf("cmd-young after tick while suspended = %v (reason=%q), want parked", cur.State, cur.Reason)
	}
	if cur.SwapBytes != wsBytes {
		t.Fatalf("cmd-young SwapBytes while suspended = %d, want %d", cur.SwapBytes, wsBytes)
	}
	if susp, _ := resolver.IsVMSuspended("ws-young"); !susp {
		t.Fatal("ws-young should remain suspended across status ticks")
	}

	// Free memory below thawBelow -> park() thaws cmd-young, which resumes ws-young!
	sys.Mem = sysinfo.Mem{Total: 16 << 30, Available: 12 << 30}
	*now = now.Add(2 * time.Second)
	eng.step(ctx)

	cur, _ = eng.Command(youngCmd.Id)
	if cur.State != running {
		t.Fatalf("cmd-young after thaw = %v (reason=%q), want running", cur.State, cur.Reason)
	}
	if susp, _ := resolver.IsVMSuspended("ws-young"); susp {
		t.Fatal("ws-young should be resumed after thaw")
	}
	resolver.mu.Lock()
	resumes := resolver.resumeCount["ws-young"]
	resolver.mu.Unlock()
	if resumes != 1 {
		t.Fatalf("ws-young resumeCount = %d, want 1", resumes)
	}

	_, _ = eng.Stop(ctx, oldCmd.Id, "done")
	_, _ = eng.Stop(ctx, youngCmd.Id, "done")
}

func TestVMParkSkipsSuspendWhenSiblingCommandRunsInSameVM(t *testing.T) {
	eng, resolver, sys, now := newSuspendTestEngine(t, -1)
	seedTestVMWorkspace(t, eng, resolver, "ws-shared", *now)
	ctx := t.Context()

	c1, err := eng.Exec(ctx, &pb.RunCommandRequest{
		Id:          "cmd-1",
		WorkspaceId: "ws-shared",
		Args:        []string{"/bin/sh", "-c", "sleep 60"},
	})
	if err != nil {
		t.Fatalf("Exec c1: %v", err)
	}
	eng.step(ctx)
	*now = now.Add(time.Second)

	c2, err := eng.Exec(ctx, &pb.RunCommandRequest{
		Id:          "cmd-2",
		WorkspaceId: "ws-shared",
		Args:        []string{"/bin/sh", "-c", "sleep 60"},
	})
	if err != nil {
		t.Fatalf("Exec c2: %v", err)
	}
	eng.step(ctx)

	// Parking c2 while c1 is still running in the SAME workspace VM must NOT suspend ws-shared.
	sys.Mem = sysinfo.Mem{Total: 16 << 30, Available: 512 << 20}
	eng.step(ctx)

	cur2, _ := eng.Command(c2.Id)
	if cur2.State != parked {
		t.Fatalf("cmd-2 state = %v, want parked", cur2.State)
	}
	if susp, _ := resolver.IsVMSuspended("ws-shared"); susp {
		t.Fatal("ws-shared must NOT be suspended while sibling cmd-1 is still running")
	}

	_, _ = eng.Stop(ctx, c1.Id, "done")
	_, _ = eng.Stop(ctx, c2.Id, "done")
}

func TestVMPauseSuspendsAndReleaseDiscardsWithoutResume(t *testing.T) {
	eng, resolver, _, now := newSuspendTestEngine(t, -1)
	seedTestVMWorkspace(t, eng, resolver, "ws-pause", *now)
	ctx := t.Context()

	c, err := eng.Exec(ctx, &pb.RunCommandRequest{
		Id:          "cmd-pause",
		WorkspaceId: "ws-pause",
		Args:        []string{"/bin/sh", "-c", "sleep 60"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	eng.step(ctx)

	pausedCmd, err := eng.Pause(ctx, c.Id, Intervention{Comment: "take a break"})
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if pausedCmd.State != paused {
		t.Fatalf("state = %v, want paused", pausedCmd.State)
	}
	if susp, _ := resolver.IsVMSuspended("ws-pause"); !susp {
		t.Fatal("expected ws-pause to be suspended after Pause")
	}

	// Releasing the workspace while suspended must NOT resume the VM just to kill the paused command.
	if err := eng.ReleaseWorkspace(ctx, "ws-pause"); err != nil {
		t.Fatalf("ReleaseWorkspace: %v", err)
	}
	resolver.mu.Lock()
	resumes := resolver.resumeCount["ws-pause"]
	stops := resolver.stopCount["ws-pause"]
	resolver.mu.Unlock()
	if resumes != 0 {
		t.Fatalf("ReleaseWorkspace unexpectedly resumed suspended VM (%d times)", resumes)
	}
	if stops != 1 {
		t.Fatalf("ReleaseWorkspace StopVM count = %d, want 1", stops)
	}
	ended, _ := eng.Command(c.Id)
	if ended.State != cancelled {
		t.Fatalf("cmd-pause after release = %v, want cancelled", ended.State)
	}
}

func TestIdleVMWorkspaceHibernatesAndWakesOnExec(t *testing.T) {
	eng, resolver, _, now := newSuspendTestEngine(t, 2*time.Minute)
	seedTestVMWorkspace(t, eng, resolver, "ws-idle", *now)
	ctx := t.Context()

	// Before idle timeout, VM stays warm.
	*now = now.Add(time.Minute)
	eng.step(ctx)
	if susp, _ := resolver.IsVMSuspended("ws-idle"); susp {
		t.Fatal("ws-idle suspended before idle timeout")
	}

	// Past idle timeout, VM hibernates to memory cell.
	*now = now.Add(2 * time.Minute)
	eng.step(ctx)
	if susp, _ := resolver.IsVMSuspended("ws-idle"); !susp {
		t.Fatal("expected ws-idle to hibernate after idle timeout")
	}

	// Running a new command transparently wakes the VM and completes cleanly.
	cmd, err := eng.Exec(ctx, &pb.RunCommandRequest{
		Id:          "cmd-wake",
		WorkspaceId: "ws-idle",
		Args:        []string{"/bin/sh", "-c", "echo WOKE_FROM_CELL"},
	})
	if err != nil {
		t.Fatalf("Exec after hibernate: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		eng.step(ctx)
		cur, ok := eng.Command(cmd.Id)
		if ok && cur.State == succeeded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for cmd-wake after hibernate (state=%v)", cur.GetState())
		}
		time.Sleep(25 * time.Millisecond)
	}
	resolver.mu.Lock()
	resumes := resolver.resumeCount["ws-idle"]
	resolver.mu.Unlock()
	if resumes != 1 {
		t.Fatalf("ws-idle resumeCount = %d, want 1", resumes)
	}
	logPath, _ := eng.logPath(cmd.Id)
	got, _ := os.ReadFile(logPath)
	if !strings.Contains(string(got), "WOKE_FROM_CELL") {
		t.Fatalf("log missing WOKE_FROM_CELL: %q", string(got))
	}
}

func TestRuntimeUnitsMemoryReturnsZeroAnonWhileSuspended(t *testing.T) {
	resolver := newSuspendableTestResolver()
	resolver.suspended["ws-1"] = 768 << 20
	ru := newRuntimeUnits(&fake.Units{}, runtime.NewGuestRuntime(resolver), func(string) bool { return true }, 0, 0)
	mem, err := ru.Memory(makeVMCgroup("ws-1", "cmd-1"))
	if err != nil {
		t.Fatalf("Memory: %v", err)
	}
	if mem != (unit.MemoryUse{Anon: 0, Swap: 768 << 20}) {
		t.Fatalf("Memory = %+v, want {Anon:0 Swap:%d}", mem, int64(768<<20))
	}
}

// When memory-cell suspend returns ErrSuspendUnsupported on a completely idle
// VM (e.g. a pre-upgrade VM launched with --no-api or pre-upgrade Firecracker),
// hibernateIdleVMs cleanly stops the idle VM via Teardown so it holds 0 host
// RAM and 0 swap, keeps its CladeFS rootfs cell intact, does not loop on the
// already-stopped VM, and transparently boots it again on the next command.
// Under memory tightness, idle shutdown triggers after 15s instead of 2m.
func TestIdleVMStopsWhenSuspendUnsupportedAndRebootsOnNextCommand(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	sys := fake.NewSystem()
	sys.SetMem(sysinfo.Mem{Total: 16 << 30, Available: 1 << 30}) // 93.75% used -> tight memory
	resolver := newSuspendableTestResolver()
	resolver.suspendErr = fmt.Errorf("no api sock: %w", vm.ErrSuspendUnsupported)
	rt := runtime.NewGuestRuntime(resolver)
	eng, err := NewEngine(ctx, EngineConfig{
		Dir:                dir,
		NodeID:             "n1",
		Host:               "worker1",
		Store:              store,
		BaseImage:          fake.BaseImage(store),
		Units:              &fake.Units{},
		Runtime:            rt,
		RuntimeFor:         func(string) bool { return true },
		System:             sys,
		Now:                func() time.Time { return now },
		VMIdleSuspendAfter: 2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	seedTestVMWorkspace(t, eng, resolver, "ws-old-noapi", now)
	eng.Sample()

	// Tight memory tightens idle threshold to 15s: at +20s the idle VM stops.
	now = now.Add(20 * time.Second)
	eng.step(ctx)

	resolver.mu.Lock()
	stops := resolver.stopCount["ws-old-noapi"]
	stopped := resolver.stopped["ws-old-noapi"]
	resolver.mu.Unlock()
	if stops != 1 || !stopped {
		t.Fatalf("after 20s idle under tight memory: stopCount=%d stopped=%v, want 1/true", stops, stopped)
	}

	// Subsequent steps must skip the already-stopped VM (no repeated StopVM).
	now = now.Add(time.Minute)
	eng.step(ctx)
	resolver.mu.Lock()
	stopsAfter := resolver.stopCount["ws-old-noapi"]
	resolver.mu.Unlock()
	if stopsAfter != 1 {
		t.Fatalf("already-stopped VM should not call StopVM again: got %d", stopsAfter)
	}

	// Running a new command transparently wakes the VM via EnsureVM.
	sys.SetMem(sysinfo.Mem{Total: 16 << 30, Available: 12 << 30})
	eng.Sample()
	cmd, err := eng.Exec(ctx, &pb.RunCommandRequest{
		Id: "cmd-reboot", WorkspaceId: "ws-old-noapi", Args: []string{"sh", "-c", "echo REBOOTED_OK"},
	})
	if err != nil {
		t.Fatalf("Exec after idle stop: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		eng.step(ctx)
		cur, ok := eng.Command(cmd.Id)
		if ok && cur.State == succeeded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for cmd-reboot (state=%v)", cur.GetState())
		}
		time.Sleep(25 * time.Millisecond)
	}
	resolver.mu.Lock()
	resumes := resolver.resumeCount["ws-old-noapi"]
	resolver.mu.Unlock()
	if resumes != 1 {
		t.Fatalf("ws-old-noapi resumeCount = %d, want 1", resumes)
	}
}

// If both SuspendWorkspace (ErrSuspendUnsupported) and Teardown fail on an idle
// VM, hibernateIdleVMs falls back to host cgroup memory.reclaim so the VM still
// sheds cold host RAM.
func TestIdleVMFallbackReclaimsHostRAMWhenStopFails(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	sys := fake.NewSystem()
	vmCG := "/user.slice/user-1000.slice/user-1000-hotbed.slice/hotbed-vm-ws-old-noapi.service"
	inner := &hostVMReclaimUnits{
		Units: &fake.Units{},
		vmUnits: map[string]unit.State{
			"hotbed-vm-ws-old-noapi": {Loaded: true, Running: true, Cgroup: vmCG},
		},
		vmMem: map[string]unit.MemoryUse{
			vmCG: {Anon: 1024 << 20, Swap: 0},
		},
	}
	resolver := newSuspendableTestResolver()
	resolver.suspendErr = fmt.Errorf("no api sock: %w", vm.ErrSuspendUnsupported)
	resolver.stopErr = fmt.Errorf("simulated stop failure")
	rt := runtime.NewGuestRuntime(resolver)
	eng, err := NewEngine(ctx, EngineConfig{
		Dir:                dir,
		NodeID:             "n1",
		Host:               "worker1",
		Store:              store,
		BaseImage:          fake.BaseImage(store),
		Units:              inner,
		Runtime:            rt,
		RuntimeFor:         func(string) bool { return true },
		System:             sys,
		Now:                func() time.Time { return now },
		VMIdleSuspendAfter: 2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	seedTestVMWorkspace(t, eng, resolver, "ws-old-noapi", now)

	now = now.Add(3 * time.Minute)
	eng.step(ctx)

	inner.mu.Lock()
	calls := len(inner.reclaimCalls)
	reclaimed := int64(0)
	if calls > 0 {
		reclaimed = inner.reclaimCalls[0].n
	}
	inner.mu.Unlock()
	wantReclaim := int64(1024<<20) - vmHostMinResidentBytes
	if calls != 1 || reclaimed != wantReclaim {
		t.Fatalf("idle VM fallback host reclaim calls=%d bytes=%d, want 1 call of %d bytes", calls, reclaimed, wantReclaim)
	}
}

type lockCheckUnits struct {
	*fake.Units
	eng       *Engine
	mu        sync.Mutex
	memCalls  int
	lockedErr bool
}

func (u *lockCheckUnits) Memory(cgroup string) (unit.MemoryUse, error) {
	u.mu.Lock()
	u.memCalls++
	u.mu.Unlock()
	if !u.eng.mu.TryLock() {
		u.mu.Lock()
		u.lockedErr = true
		u.mu.Unlock()
	} else {
		u.eng.mu.Unlock()
	}
	return u.Units.Memory(cgroup)
}

// Engine.step must read Units.Memory outside e.mu so a slow cgroup or VM vsock
// memory read never blocks heartbeats, RPCs, or Stop calls across the worker.
func TestStepReadsMemoryOutsideEngineLock(t *testing.T) {
	baseUnits := &fake.Units{}
	units := &lockCheckUnits{Units: baseUnits}
	sys := fake.NewSystem()
	store := &fake.Store{Root: t.TempDir()}
	now := time.Unix(1795400000, 0)
	e, err := NewEngine(t.Context(), EngineConfig{
		Dir: t.TempDir(), NodeID: "n", Host: "ubu1",
		Store: store, BaseImage: fake.BaseImage(store), Units: units, System: sys, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	units.eng = e
	if err := os.MkdirAll(filepath.Join(store.Path("ws/a"), "work", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.workspaces["a"] = &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{Id: "a", Project: "app", Repos: []*pb.WorkspaceRepo{{Repo: "app"}}, State: idle},
		Volume:    "ws/a",
	}}
	_ = e.start(t, baseUnits, &now, "c-1", "a", "build")

	now = now.Add(time.Second)
	e.step(t.Context())

	units.mu.Lock()
	calls, wasLocked := units.memCalls, units.lockedErr
	units.mu.Unlock()
	if calls == 0 {
		t.Fatal("expected Units.Memory to be called during step")
	}
	if wasLocked {
		t.Fatal("Units.Memory was called while Engine.mu was held")
	}
}
