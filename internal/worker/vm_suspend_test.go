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
	// stoppedErr is what StopVM says after stopping the VM all the same, as
	// a stop whose guest sync failed does.
	stoppedErr error
	// sizes is the size each VM was last asked for.
	sizes map[string]unit.VMSize
}

func newSuspendableTestResolver() *suspendableTestResolver {
	return &suspendableTestResolver{
		clients:      map[string]*guestd.Client{},
		suspended:    map[string]int64{},
		stopped:      map[string]bool{},
		suspendCount: map[string]int{},
		resumeCount:  map[string]int{},
		stopCount:    map[string]int{},
		sizes:        map[string]unit.VMSize{},
	}
}

func (r *suspendableTestResolver) EnsureVM(_ context.Context, wsID string, size unit.VMSize) (*guestd.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sizes[wsID] = size
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
	return r.stoppedErr
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

func newSuspendTestEngine(t *testing.T) (*Engine, *suspendableTestResolver, *fake.System, *time.Time) {
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
		Dir:        dir,
		NodeID:     "n1",
		Host:       "worker1",
		Store:      store,
		BaseImage:  fake.BaseImage(store),
		Units:      &fake.Units{},
		Runtime:    rt,
		RuntimeFor: func(string) bool { return true },
		System:     sys,
		Now:        func() time.Time { return now },
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

// Two VMs cannot share a workspace's disk and each command runs in a VM of
// its own: a second command in a workspace waits while the first runs, the
// first's VM stops as soon as it ends, and the second boots a new one.
func TestSecondCommandInWorkspaceWaitsAndGetsAVMOfItsOwn(t *testing.T) {
	eng, resolver, _, now := newSuspendTestEngine(t)
	seedTestVMWorkspace(t, eng, resolver, "ws-shared", *now)
	ctx := t.Context()

	c1, err := eng.Exec(ctx, &pb.RunCommandRequest{
		Id: "cmd-1", WorkspaceId: "ws-shared", Args: []string{"/bin/sh", "-c", "sleep 60"},
	})
	if err != nil {
		t.Fatalf("Exec c1: %v", err)
	}
	eng.step(ctx)
	*now = now.Add(time.Second)
	c2, err := eng.Exec(ctx, &pb.RunCommandRequest{
		Id: "cmd-2", WorkspaceId: "ws-shared", Args: []string{"/bin/sh", "-c", "echo SECOND_VM"},
	})
	if err != nil {
		t.Fatalf("Exec c2: %v", err)
	}
	eng.step(ctx)

	if cur, _ := eng.Command(c1.Id); cur.State != running {
		t.Fatalf("cmd-1 = %v (%q), want running", cur.State, cur.Reason)
	}
	if cur, _ := eng.Command(c2.Id); cur.State != queued || cur.Reason != "waiting for its workspace: command cmd-1 runs in its VM" {
		t.Fatalf("cmd-2 beside cmd-1 = %v (%q), want queued for its workspace", cur.State, cur.Reason)
	}

	if _, err := eng.Stop(ctx, c1.Id, "done"); err != nil {
		t.Fatalf("Stop c1: %v", err)
	}
	waitForCommandState(t, eng, c2.Id, succeeded)

	resolver.mu.Lock()
	stops, boots := resolver.stopCount["ws-shared"], resolver.resumeCount["ws-shared"]
	resolver.mu.Unlock()
	// cmd-1's VM stopped as it ended, cmd-2 booted a new one, and that
	// stopped as cmd-2 ended.
	if stops != 2 || boots != 1 {
		t.Fatalf("ws-shared stops=%d boots=%d, want 2 stops and 1 boot", stops, boots)
	}
	logPath, _ := eng.logPath(c2.Id)
	if got, _ := os.ReadFile(logPath); !strings.Contains(string(got), "SECOND_VM") {
		t.Fatalf("cmd-2 log = %q, want SECOND_VM", got)
	}
}

// waitForCommandState steps eng until command id is in state want.
func waitForCommandState(t *testing.T, eng *Engine, id string, want pb.CommandState) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		eng.step(t.Context())
		cur, ok := eng.Command(id)
		if ok && cur.State == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s to be %v: it is %v (%q)", id, want, cur.GetState(), cur.GetReason())
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestVMPauseSuspendsAndReleaseDiscardsWithoutResume(t *testing.T) {
	eng, resolver, _, now := newSuspendTestEngine(t)
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

// A VM booted ahead of any command (to prepare the workspace or sync a
// checkout) waits freshVMGrace for its command, then stops too. A command
// boots a new one on the workspace's disk, and that one stops as soon as the
// command ends: no VM idles between commands.
func TestFreshVMStopsAfterGraceAndEachCommandStopsItsVM(t *testing.T) {
	eng, resolver, _, now := newSuspendTestEngine(t)
	seedTestVMWorkspace(t, eng, resolver, "ws-idle", *now)
	ctx := t.Context()
	stops := func() int {
		resolver.mu.Lock()
		defer resolver.mu.Unlock()
		return resolver.stopCount["ws-idle"]
	}

	*now = now.Add(freshVMGrace - time.Second)
	eng.step(ctx)
	if n := stops(); n != 0 {
		t.Fatalf("fresh ws-idle stopped %d times within freshVMGrace, want 0", n)
	}
	*now = now.Add(2 * time.Second)
	eng.step(ctx)
	if n := stops(); n != 1 {
		t.Fatalf("fresh ws-idle stopped %d times past freshVMGrace, want 1", n)
	}
	// A stopped VM is not stopped again.
	*now = now.Add(time.Minute)
	eng.step(ctx)
	if n := stops(); n != 1 {
		t.Fatalf("stopped ws-idle stopped %d times, want still 1", n)
	}

	cmd, err := eng.Exec(ctx, &pb.RunCommandRequest{
		Id: "cmd-wake", WorkspaceId: "ws-idle", Args: []string{"/bin/sh", "-c", "echo BOOTED_FOR_ME"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	waitForCommandState(t, eng, cmd.Id, succeeded)
	resolver.mu.Lock()
	boots := resolver.resumeCount["ws-idle"]
	resolver.mu.Unlock()
	if boots != 1 {
		t.Fatalf("ws-idle boots = %d, want 1 for cmd-wake", boots)
	}
	if n := stops(); n != 2 {
		t.Fatalf("ws-idle stops after cmd-wake ended = %d, want 2: its VM stops with it", n)
	}
	logPath, _ := eng.logPath(cmd.Id)
	if got, _ := os.ReadFile(logPath); !strings.Contains(string(got), "BOOTED_FOR_ME") {
		t.Fatalf("log missing BOOTED_FOR_ME: %q", got)
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

// If stopping a VM whose command ended fails, its host RAM is reclaimed so it
// does not pin memory while it waits a minute to be stopped again.
func TestVMStopFailureReclaimsHostRAMAndRetries(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	sys := fake.NewSystem()
	vmCG := "/user.slice/user-1000.slice/user-1000-hotbed.slice/hotbed-vm-ws-stuck.service"
	inner := &hostVMReclaimUnits{
		Units: &fake.Units{},
		vmUnits: map[string]unit.State{
			"hotbed-vm-ws-stuck": {Loaded: true, Running: true, Cgroup: vmCG},
		},
		vmMem: map[string]unit.MemoryUse{
			vmCG: {Anon: 1024 << 20, Swap: 0},
		},
	}
	resolver := newSuspendableTestResolver()
	resolver.stopErr = fmt.Errorf("simulated stop failure")
	rt := runtime.NewGuestRuntime(resolver)
	eng, err := NewEngine(ctx, EngineConfig{
		Dir:        dir,
		NodeID:     "n1",
		Host:       "worker1",
		Store:      store,
		BaseImage:  fake.BaseImage(store),
		Units:      inner,
		Runtime:    rt,
		RuntimeFor: func(string) bool { return true },
		System:     sys,
		Now:        func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	seedTestVMWorkspace(t, eng, resolver, "ws-stuck", now)
	eng.mu.Lock()
	eng.workspaces["ws-stuck"].vmUsed = true // a command ran in it and ended
	eng.mu.Unlock()

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
		t.Fatalf("after a failed stop: host reclaim calls=%d bytes=%d, want 1 call of %d bytes", calls, reclaimed, wantReclaim)
	}

	// Within the minute it does not try again; past it, it stops.
	resolver.mu.Lock()
	resolver.stopErr = nil
	resolver.mu.Unlock()
	now = now.Add(30 * time.Second)
	eng.step(ctx)
	resolver.mu.Lock()
	stops := resolver.stopCount["ws-stuck"]
	resolver.mu.Unlock()
	if stops != 0 {
		t.Fatalf("ws-stuck stopped %d times within the retry minute, want 0", stops)
	}
	now = now.Add(31 * time.Second)
	eng.step(ctx)
	resolver.mu.Lock()
	stops = resolver.stopCount["ws-stuck"]
	resolver.mu.Unlock()
	if stops != 1 {
		t.Fatalf("ws-stuck stopped %d times past the retry minute, want 1", stops)
	}
	eng.mu.Lock()
	used := eng.workspaces["ws-stuck"].vmUsed
	eng.mu.Unlock()
	if used {
		t.Fatal("ws-stuck vmUsed after its VM stopped, want false: the next VM is new")
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

// --vm-cpus and --vm-mem size the VM a command boots, and only that: the
// node reserves for it only what it reserves for any command.
func TestCommandVMSizeSizesItsVMOnly(t *testing.T) {
	eng, resolver, _, now := newSuspendTestEngine(t)
	seedTestVMWorkspace(t, eng, resolver, "ws-sz", *now)
	ctx := t.Context()

	c, err := eng.Exec(ctx, &pb.RunCommandRequest{
		Id: "cmd-sz", WorkspaceId: "ws-sz", VmCpus: 2, VmMemoryBytes: 3<<30 + 1,
		Args: []string{"/bin/sh", "-c", "sleep 60"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	eng.step(ctx)
	if cur, _ := eng.Command(c.Id); cur.State != running {
		t.Fatalf("cmd-sz = %v (%q), want running", cur.State, cur.Reason)
	}
	resolver.mu.Lock()
	got := resolver.sizes["ws-sz"]
	resolver.mu.Unlock()
	if got != (unit.VMSize{VCPUs: 2, MemMiB: 3073}) {
		t.Fatalf("VM size asked = %+v, want 2 vCPUs, 3073 MiB (rounded up)", got)
	}
	if r := eng.Heartbeat("cl").ReservedMemoryBytes; r != defaultReservation {
		t.Fatalf("reserved = %d, want the default %d: a VM's size is not a reservation", r, defaultReservation)
	}
	_, _ = eng.Stop(ctx, c.Id, "done")
}

// A command asks of its VM only what its workspace and node can give.
func TestExecRefusesVMRequestsItCannotMeet(t *testing.T) {
	e, _, _, _ := parkEngine(t) // host runtime: no VMs
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{WorkspaceId: "a", VmCpus: 2, Args: []string{"true"}}); err == nil ||
		err.Error() != "INVALID_ARGS: workspace a runs commands on the host: --vm-cpus and --vm-mem need the vm runtime" {
		t.Fatalf("--vm-cpus on a host workspace: err = %v", err)
	}

	eng, resolver, _, now := newSuspendTestEngine(t)
	seedTestVMWorkspace(t, eng, resolver, "ws-big", *now)
	eng.mu.Lock()
	eng.resources.Cpus, eng.resources.MemoryBytes = 8, 16<<30
	eng.mu.Unlock()
	if _, err := eng.Exec(t.Context(), &pb.RunCommandRequest{WorkspaceId: "ws-big", VmCpus: 9, Args: []string{"true"}}); err == nil ||
		err.Error() != "INVALID_ARGS: --vm-cpus 9: worker1 has 8 CPUs" {
		t.Fatalf("--vm-cpus 9 on 8 CPUs: err = %v", err)
	}
	if _, err := eng.Exec(t.Context(), &pb.RunCommandRequest{WorkspaceId: "ws-big", VmMemoryBytes: 17 << 30, Args: []string{"true"}}); err == nil ||
		err.Error() != "INVALID_ARGS: --vm-mem 17.0 GiB: a VM on worker1 takes 0.5 GiB to 16.0 GiB" {
		t.Fatalf("--vm-mem 17G on 16 GiB: err = %v", err)
	}
}

// A VM a sync, file read or shell is using is not stopped under it, even one
// whose command ended: it stops once they let it go.
func TestVMInUseIsNotStopped(t *testing.T) {
	eng, resolver, _, now := newSuspendTestEngine(t)
	seedTestVMWorkspace(t, eng, resolver, "ws-use", *now)
	eng.mu.Lock()
	eng.workspaces["ws-use"].vmUsed = true // a command ran in it and ended
	eng.mu.Unlock()
	stops := func() int {
		resolver.mu.Lock()
		defer resolver.mu.Unlock()
		return resolver.stopCount["ws-use"]
	}

	release := eng.useVM("ws-use")
	eng.step(t.Context())
	if n := stops(); n != 0 {
		t.Fatalf("ws-use stopped %d times while in use, want 0", n)
	}
	release()
	eng.step(t.Context())
	if n := stops(); n != 1 {
		t.Fatalf("ws-use stopped %d times once let go, want 1", n)
	}
}

// A shell open in a fresh VM keeps it: a command asking no size runs in it,
// and one asking a size of its own waits rather than reboot it under the
// shell.
func TestSizedCommandWaitsForAShellsVM(t *testing.T) {
	eng, resolver, _, now := newSuspendTestEngine(t)
	seedTestVMWorkspace(t, eng, resolver, "ws-sh", *now)
	eng.mu.Lock()
	eng.shells["sh-1"] = &activeShell{id: "sh-1", workspaceID: "ws-sh", done: make(chan struct{})}
	eng.mu.Unlock()
	t.Cleanup(func() { // the shell closes before the engine does
		eng.mu.Lock()
		delete(eng.shells, "sh-1")
		eng.mu.Unlock()
	})
	ctx := t.Context()

	sized, err := eng.Exec(ctx, &pb.RunCommandRequest{Id: "cmd-big", WorkspaceId: "ws-sh", VmCpus: 2, Args: []string{"/bin/sh", "-c", "true"}})
	if err != nil {
		t.Fatalf("Exec sized: %v", err)
	}
	eng.step(ctx)
	if cur, _ := eng.Command(sized.Id); cur.State != queued || cur.Reason != "waiting for its workspace: a shell is open in its VM, and it asks a VM of its own size" {
		t.Fatalf("sized command beside a shell = %v (%q), want queued for the shell", cur.State, cur.Reason)
	}
	_, _ = eng.Stop(ctx, sized.Id, "done")

	plain, err := eng.Exec(ctx, &pb.RunCommandRequest{Id: "cmd-plain", WorkspaceId: "ws-sh", Args: []string{"/bin/sh", "-c", "echo IN_SHELLS_VM"}})
	if err != nil {
		t.Fatalf("Exec plain: %v", err)
	}
	waitForCommandState(t, eng, plain.Id, succeeded)
	if n := func() int { resolver.mu.Lock(); defer resolver.mu.Unlock(); return resolver.stopCount["ws-sh"] }(); n != 0 {
		t.Fatalf("ws-sh stopped %d times with a shell open in it, want 0", n)
	}
}

// A node profile reads the profiles of VMs that are up, and boots none: most
// workspaces' VMs are down between commands.
func TestNodeProfileBootsNoVM(t *testing.T) {
	eng, resolver, _, now := newSuspendTestEngine(t)
	seedTestVMWorkspace(t, eng, resolver, "ws-down", *now)
	if err := resolver.StopVM(t.Context(), "ws-down"); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.NodeProfile("worker1", &pb.GetProfileRequest{}); err != nil {
		t.Fatalf("NodeProfile: %v", err)
	}
	resolver.mu.Lock()
	_, asked := resolver.sizes["ws-down"]
	boots := resolver.resumeCount["ws-down"]
	resolver.mu.Unlock()
	if asked || boots != 0 {
		t.Fatalf("NodeProfile asked ws-down's VM up (asked=%v boots=%d), want it left down", asked, boots)
	}
}

// A stop that errs but leaves the VM gone (its guest's sync failed) counts
// as a stop: the workspace's next command does not wait a minute for it.
func TestVMGoneDespiteStopErrorCountsAsStopped(t *testing.T) {
	eng, resolver, _, now := newSuspendTestEngine(t)
	seedTestVMWorkspace(t, eng, resolver, "ws-err", *now)
	resolver.mu.Lock()
	resolver.stoppedErr = fmt.Errorf("guest shutdown/sync failed: connection reset")
	resolver.mu.Unlock()
	eng.mu.Lock()
	eng.workspaces["ws-err"].vmUsed = true
	eng.mu.Unlock()

	eng.step(t.Context())
	eng.mu.Lock()
	used, retryAt := eng.workspaces["ws-err"].vmUsed, eng.workspaces["ws-err"].vmRetryAt
	eng.mu.Unlock()
	if used || !retryAt.IsZero() {
		t.Fatalf("after a stop that erred but left no VM: vmUsed=%v vmRetryAt=%v, want false and zero", used, retryAt)
	}
}

// A sized command, which may boot its workspace's VM again at its size,
// waits while a sync, file read or shell uses the VM, and starts once it is
// let go.
func TestSizedCommandWaitsWhileItsVMIsInUse(t *testing.T) {
	eng, resolver, _, now := newSuspendTestEngine(t)
	seedTestVMWorkspace(t, eng, resolver, "ws-busy", *now)
	ctx := t.Context()
	release := eng.useVM("ws-busy")
	c, err := eng.Exec(ctx, &pb.RunCommandRequest{Id: "cmd-sz2", WorkspaceId: "ws-busy", VmMemoryBytes: 1 << 30, Args: []string{"/bin/sh", "-c", "sleep 60"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	eng.step(ctx)
	if cur, _ := eng.Command(c.Id); cur.State != queued || cur.Reason != "waiting for its workspace: a sync, file read or shell is using its VM" {
		t.Fatalf("sized command while its VM is in use = %v (%q), want queued", cur.State, cur.Reason)
	}
	release()
	eng.step(ctx)
	if cur, _ := eng.Command(c.Id); cur.State != running {
		t.Fatalf("sized command once its VM is let go = %v (%q), want running", cur.State, cur.Reason)
	}
	_, _ = eng.Stop(ctx, c.Id, "done")
}

// The VM of a failed workspace, in which nothing will run, stops.
func TestFailedWorkspaceVMStops(t *testing.T) {
	eng, resolver, _, now := newSuspendTestEngine(t)
	seedTestVMWorkspace(t, eng, resolver, "ws-failed", *now)
	eng.mu.Lock()
	eng.workspaces["ws-failed"].ws().State = failedWS
	eng.mu.Unlock()
	eng.step(t.Context())
	resolver.mu.Lock()
	stops := resolver.stopCount["ws-failed"]
	resolver.mu.Unlock()
	if stops != 1 {
		t.Fatalf("failed workspace's VM stopped %d times, want 1", stops)
	}
}
