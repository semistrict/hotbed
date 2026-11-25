package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/cladefs"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/unit"
)

func TestManagedFreshenAdmissionHoldProgressAndCompletion(t *testing.T) {
	now := time.Unix(1795300000, 0)
	e := limited(t, &now)

	// Hold admission via host memory pressure (same condition as vm1 at 50%).
	e.SetHostUsage(&pb.HostUsage{HostMemoryPressure: 50, RemainingTtlMs: 30_000})
	e.Sample()

	fuseRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(fuseRoot, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	gateFile := filepath.Join(fuseRoot, "work", "go")
	proj := &pb.HostedProject{
		Name:  "cladefs",
		Repos: []*pb.Repo{{Id: "cladefs", Url: "https://example.invalid/cladefs.git", Branch: "main"}},
		Steps: []*pb.Step{
			{Name: "deps", Hash: "h1", Script: "echo deps"},
			{Name: "build", Hash: "h2", Script: "echo build", EveryPull: true},
		},
	}

	// Register a preparing cladefs workspace so we verify workspaceView + WorkspaceFreshenCommand.
	e.mu.Lock()
	e.workspaces["ws-fuse-1"] = &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{
			Id: "ws-fuse-1", Project: "cladefs", BaseId: "cladefs-base", NodeId: e.cfg.NodeID,
			State: preparing, Runtime: runtime.KindCladefs, Reason: "preparing cladefs R2 base",
		},
		Volume: "ws/ws-fuse-1", BaseSnapshot: "cladefs-base",
	}}
	e.mu.Unlock()

	baseID := "cladefs-fuse-12345678"
	e.setPreparingWorkspaceBase("ws-fuse-1", baseID)

	script := fmt.Sprintf(`set -euo pipefail
echo "::hotbed-head:: cladefs aaaa1111bbbb2222cccc3333dddd4444eeee5555"
echo "::hotbed:: step 1/2 deps"
echo "::hotbed:: step-done deps 500000"
echo "::hotbed:: step 2/2 build"
echo "   Compiling cladefs v0.1.0 (/work/cladefs)"
while [ ! -f %q ]; do sleep 0.02; done
echo "::hotbed:: step-done build 1500000"
echo "::hotbed:: steps done"
`, "/work/go")

	var beforeStartCalls atomic.Int32
	handle, err := e.startManagedFreshen(t.Context(), managedFreshenSpec{
		Project:    proj,
		BaseID:     baseID,
		Recipe:     "1234567890abcdef",
		Arch:       "arm64",
		Cold:       true,
		StepsToRun: proj.Steps,
		Heads:      map[string]string{"cladefs": "aaaa1111bbbb2222cccc3333dddd4444eeee5555"},
		Script:     script,
		RootDir:    fuseRoot,
		BeforeStart: func(context.Context) error {
			beforeStartCalls.Add(1)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("startManagedFreshen: %v", err)
	}
	cmdID := handle.CommandID()
	t.Cleanup(func() {
		_ = os.WriteFile(gateFile, []byte("go"), 0o644)
		_ = e.cfg.Units.Stop(context.WithoutCancel(t.Context()), "hotbed-"+cmdID)
	})
	if !strings.HasPrefix(cmdID, "f-cladefs-fuse-12345678-") {
		t.Fatalf("cmdID = %q, want f-cladefs-fuse-12345678-*", cmdID)
	}

	// Step while host memory pressure is holding: command MUST remain queued and
	// BeforeStart must NOT be invoked yet.
	e.admit(t.Context())
	if beforeStartCalls.Load() != 0 {
		t.Fatalf("BeforeStart called %d times while admission held, want 0", beforeStartCalls.Load())
	}
	if st := e.state(cmdID); st != queued {
		t.Fatalf("state while held = %v, want QUEUED", st)
	}
	if got := e.WorkspaceFreshenCommand("ws-fuse-1"); got != cmdID {
		t.Fatalf("WorkspaceFreshenCommand = %q, want %q", got, cmdID)
	}
	ws, err := e.Workspace("ws-fuse-1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if !strings.Contains(ws.Reason, "queued:") || !strings.Contains(ws.Reason, "host memory pressure") {
		t.Fatalf("preparing workspace reason while queued = %q, want queued + host memory pressure", ws.Reason)
	}

	// Clear host memory pressure: admission now starts the command.
	e.SetHostUsage(&pb.HostUsage{HostMemoryPressure: 0, RemainingTtlMs: 30_000})
	e.Sample()
	e.admit(t.Context())

	if beforeStartCalls.Load() != 1 {
		t.Fatalf("BeforeStart calls = %d, want 1", beforeStartCalls.Load())
	}
	if st := e.state(cmdID); st != running {
		t.Fatalf("state after admission cleared = %v, want RUNNING", st)
	}
	// Verify tmp dir was created inside fuseRoot, not Store.
	if _, err := os.Stat(filepath.Join(fuseRoot, tmpName, cmdID, "tmp")); err != nil {
		t.Fatalf("expected tmp dir inside fuseRoot: %v", err)
	}

	// Poll e.step until the running bash script reaches step 2/2 build.
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.step(t.Context())
		ws, _ = e.Workspace("ws-fuse-1")
		if strings.Contains(ws.Reason, "step 2/2 build") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for step 2/2 build; workspace reason = %q", ws.Reason)
		}
		time.Sleep(20 * time.Millisecond)
	}

	hb := e.Heartbeat("cluster")
	var foundBase *pb.BaseUsage
	for _, bu := range hb.Bases {
		if bu.Id == baseID {
			foundBase = bu
		}
	}
	if foundBase == nil || foundBase.Progress != 0.5 {
		t.Fatalf("heartbeat base progress = %+v, want 0.5", foundBase)
	}

	// Release the gate file so the freshen finishes cleanly.
	if err := os.WriteFile(gateFile, []byte("go"), 0o644); err != nil {
		t.Fatal(err)
	}

	waitDone := make(chan managedFreshenResult, 1)
	waitErrCh := make(chan error, 1)
	go func() {
		res, werr := handle.Wait(t.Context())
		waitErrCh <- werr
		waitDone <- res
	}()

	deadline = time.Now().Add(5 * time.Second)
	var res managedFreshenResult
	for {
		e.step(t.Context())
		select {
		case werr := <-waitErrCh:
			if werr != nil {
				t.Fatalf("handle.Wait: %v", werr)
			}
			res = <-waitDone
			goto waited
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for managed freshen completion; state=%v", e.state(cmdID))
		}
		time.Sleep(20 * time.Millisecond)
	}
waited:

	// While base is still BUILDING after steps done (syncing to R2), workspaceView
	// must report syncing to R2.
	ws, _ = e.Workspace("ws-fuse-1")
	if ws.Reason != "syncing base "+baseID+" to R2" {
		t.Fatalf("workspace reason during R2 sync = %q, want syncing base %s to R2", ws.Reason, baseID)
	}
	if res.State != succeeded {
		t.Fatalf("res.State = %v, want SUCCEEDED (reason=%q)", res.State, res.Reason)
	}
	if res.RepoHeads["cladefs"] != "aaaa1111bbbb2222cccc3333dddd4444eeee5555" {
		t.Fatalf("RepoHeads = %+v", res.RepoHeads)
	}
	if res.Units != 1 {
		t.Fatalf("Units = %d, want 1", res.Units)
	}
	// dropTmp must have completed before Wait returned so MountDetach never sees busy .tmp.
	if _, err := os.Stat(filepath.Join(fuseRoot, tmpName, cmdID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected .tmp/%s removed before Wait returned, err=%v", cmdID, err)
	}

	handle.Finish(true, res, cladefs.CellRef{Tenant: 1, Cell: 42, Version: 99}, "")

	e.mu.Lock()
	b := e.bases[baseID]
	e.mu.Unlock()
	if b == nil || b.State != warm || b.CladefsCell == nil || b.CladefsCell.Cell != 42 || b.CladefsCell.Version != 99 {
		t.Fatalf("final base = %+v, want WARM with cell 42/99", b)
	}
	if len(b.Baked) != 2 || b.Units != 1 {
		t.Fatalf("final base baked=%v units=%d, want 2 steps & 1 unit", b.Baked, b.Units)
	}

	// Chroot disk/tmp/base GC helpers must never choke on the R2 base record.
	e.measureDisk(t.Context())
	e.sweepTmp()
	e.collectBases(t.Context())
}

type failStartUnits struct {
	Units
	err error
}

func (f *failStartUnits) Start(_ context.Context, _ unit.Spec) error { return f.err }

func TestManagedFreshenCancelWhileQueuedSkipsBeforeStartAndRollsBackOnStartFail(t *testing.T) {
	now := time.Unix(1795300000, 0)
	e := limited(t, &now)

	proj := &pb.HostedProject{Name: "hotbed"}

	// Case 1: Stop while queued -> BeforeStart never runs, Wait unblocks with CANCELLED.
	e.SetHostUsage(&pb.HostUsage{HostMemoryPressure: 50, RemainingTtlMs: 30_000})
	e.Sample()
	var beforeCalls atomic.Int32
	h1, err := e.startManagedFreshen(t.Context(), managedFreshenSpec{
		Project: proj,
		BaseID:  "hotbed-vm-11111111",
		Recipe:  "11111111aaaaaaaa",
		Arch:    "amd64",
		Cold:    true,
		Script:  "echo hi",
		VMID:    "base-hotbed-vm-11111111",
		BeforeStart: func(context.Context) error {
			beforeCalls.Add(1)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.admit(t.Context())
	if _, err := e.Stop(t.Context(), h1.CommandID(), "cancelled by test"); err != nil {
		t.Fatal(err)
	}
	res1, err := h1.Wait(t.Context())
	if err != nil {
		t.Fatalf("Wait after stop: %v", err)
	}
	if res1.State != cancelled {
		t.Fatalf("res1.State = %v, want CANCELLED", res1.State)
	}
	if beforeCalls.Load() != 0 {
		t.Fatalf("BeforeStart ran %d times for queued-cancelled build, want 0", beforeCalls.Load())
	}
	h1.Finish(false, res1, cladefs.CellRef{}, "cancelled")

	// Case 2: Units.Start fails after BeforeStart succeeded -> OnStartFail cleans up VM.
	// Use a dedicated engine constructed with failStartUnits so e.cfg.Units is never mutated concurrently.
	store2 := &fake.Store{Root: t.TempDir()}
	baseImg2 := fake.BaseImage(store2)
	e2, err := NewEngine(t.Context(), EngineConfig{
		Dir:       t.TempDir(),
		NodeID:    "n-failstart",
		Host:      "n-failstart",
		BaseImage: baseImg2,
		Store:     store2,
		Units:     &failStartUnits{Units: &fake.Units{}, err: errors.New("guestd start failed")},
		System:    fake.NewSystem(),
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e2.Close() })
	e2.Sample()
	var startFailCalls atomic.Int32
	h2, err := e2.startManagedFreshen(t.Context(), managedFreshenSpec{
		Project: proj,
		BaseID:  "hotbed-vm-22222222",
		Recipe:  "22222222bbbbbbbb",
		Arch:    "amd64",
		Cold:    true,
		Script:  "echo hi",
		VMID:    "base-hotbed-vm-22222222",
		BeforeStart: func(context.Context) error {
			return nil
		},
		OnStartFail: func() {
			startFailCalls.Add(1)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	e2.admit(t.Context())
	res2, err := h2.Wait(t.Context())
	if err != nil {
		t.Fatalf("Wait after start failure: %v", err)
	}
	if res2.State != failed {
		t.Fatalf("res2.State = %v, want FAILED", res2.State)
	}
	if startFailCalls.Load() != 1 {
		t.Fatalf("OnStartFail calls = %d, want 1", startFailCalls.Load())
	}
	h2.Finish(false, res2, cladefs.CellRef{}, res2.Reason)
}

func TestReconcileLoadedEndsInterruptedCladefsBasesAndWorkspaces(t *testing.T) {
	now := time.Unix(1795300000, 0)
	e := limited(t, &now)

	e.mu.Lock()
	b := &pb.Base{
		Id:          "cladefs-fuse-deadbeef",
		Project:     "cladefs",
		NodeId:      e.cfg.NodeID,
		Generation:  1,
		State:       building,
		Command:     "f-cladefs-fuse-deadbeef-abc123",
		CladefsCell: &pb.CladefsCell{BaseId: "cladefs-fuse-deadbeef", Project: "cladefs", Arch: "arm64"},
	}
	c := e.newCommand(&pb.CommandRecord{
		Command: &pb.Command{
			Id: b.Command, NodeId: e.cfg.NodeID, Kind: "freshen", State: running,
			Argv: "freshen " + b.Id, Project: "cladefs", QueuedAt: now.UnixMilli(),
		},
		Project: "cladefs", BaseId: b.Id,
	})
	w := &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{
			Id: "ws-interrupted", Project: "cladefs", BaseId: b.Id, NodeId: e.cfg.NodeID,
			State: preparing, Runtime: runtime.KindCladefs, Reason: "preparing cladefs R2 base",
		},
		Volume: "ws/ws-interrupted",
	}}
	e.bases[b.Id] = b
	e.commands[c.cmd().Id] = c
	e.workspaces[w.ws().Id] = w
	e.mu.Unlock()

	e.reconcileLoaded()

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.bases[b.Id].State != broken {
		t.Fatalf("interrupted cladefs base state = %v, want FAILED", e.bases[b.Id].State)
	}
	if e.commands[b.Command].cmd().State != cancelled {
		t.Fatalf("interrupted cladefs freshen command state = %v, want CANCELLED", e.commands[b.Command].cmd().State)
	}
	if e.workspaces["ws-interrupted"].ws().State != failedWS {
		t.Fatalf("interrupted preparing cladefs workspace state = %v, want FAILED", e.workspaces["ws-interrupted"].ws().State)
	}
}

func TestRuntimeUnitsRoutesManagedVMFreshenViaRuntimeTarget(t *testing.T) {
	inner := &fake.Units{}
	var gotWSID string
	var gotEnv []string
	rt := &stubWorkspaceRuntime{
		onStart: func(wsID string, spec runtime.CommandSpec) error {
			gotWSID = wsID
			gotEnv = spec.Env
			return nil
		},
	}
	ru := newRuntimeUnits(inner, rt, func(wsID string) bool { return wsID == "base-cladefs-vm-12345678" }, 1000, 1000)
	err := ru.Start(t.Context(), unit.Spec{
		Name: "hotbed-f-cladefs-vm-12345678-abc123",
		Argv: []string{"/bin/bash", "-c", "echo ok"},
		Dir:  "/work",
		Env: []string{
			"HOTBED_WORKSPACE=",
			"HOTBED_COMMAND=f-cladefs-vm-12345678-abc123",
			"HOTBED_RUNTIME_TARGET=base-cladefs-vm-12345678",
			"FOO=bar",
		},
	})
	if err != nil {
		t.Fatalf("runtimeUnits.Start: %v", err)
	}
	if gotWSID != "base-cladefs-vm-12345678" {
		t.Fatalf("routed target = %q, want base-cladefs-vm-12345678", gotWSID)
	}
	for _, kv := range gotEnv {
		if strings.HasPrefix(kv, "HOTBED_RUNTIME_TARGET=") {
			t.Fatalf("internal HOTBED_RUNTIME_TARGET leaked into guest env: %v", gotEnv)
		}
	}
}

type stubWorkspaceRuntime struct {
	runtime.WorkspaceRuntime
	onStart func(wsID string, spec runtime.CommandSpec) error
}

func (s *stubWorkspaceRuntime) StartCommand(_ context.Context, wsID string, spec runtime.CommandSpec) error {
	if s.onStart != nil {
		return s.onStart(wsID, spec)
	}
	return nil
}

func (s *stubWorkspaceRuntime) StreamLogs(_ context.Context, _, _ string, _ int64, _ bool, _ io.Writer) (int64, error) {
	return 0, nil
}
