package worker

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/cladefs"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func TestVMBackendTwoTierR2CaptureAndEphemeralSwap(t *testing.T) {
	ctx := t.Context()
	api := newStrictVMCladefsClient()
	fl := newFakeVMLauncher()
	b, _ := newTestVMBackend(t, api, fl)

	baseRef := cladefs.CellRef{Tenant: 1, Cell: 100, Version: 5}

	// 1. Routine workspace (captureBase=false): strictly ephemeral, zero R2 sync/take.
	ephemCell, err := b.PrepareWorkspace(ctx, "ws-ephem", baseRef, 1, false)
	if err != nil {
		t.Fatalf("PrepareWorkspace ephemeral: %v", err)
	}
	if ephemCell.GetCell() == 0 {
		t.Fatal("expected non-zero ephemeral cell")
	}
	api.mu.Lock()
	if len(api.forkReqs) != 1 || !api.forkReqs[0].GetEphemeral() {
		t.Fatalf("routine workspace fork must be ephemeral=true, got %+v", api.forkReqs)
	}
	if len(api.createReqs) != 0 {
		t.Fatalf("routine workspace must not call TakeCell, got %+v", api.createReqs)
	}
	api.mu.Unlock()

	// 2. Capture-enabled workspace (captureBase=true): bucket-backed with 24h
	// upload interval so zero bytes upload while commands run.
	capCell, err := b.PrepareWorkspace(ctx, "ws-cap", baseRef, 2, true)
	if err != nil {
		t.Fatalf("PrepareWorkspace capture: %v", err)
	}
	api.mu.Lock()
	if len(api.forkReqs) != 2 || api.forkReqs[1].GetEphemeral() {
		t.Fatalf("capture workspace fork must be ephemeral=false, got %+v", api.forkReqs[1])
	}
	if len(api.createReqs) != 1 || api.createReqs[0].UploadIntervalMs == nil {
		t.Fatalf("capture workspace must re-open with upload interval, got %+v", api.createReqs)
	}
	wantInterval := uint64(captureUploadInterval / time.Millisecond)
	if got := *api.createReqs[0].UploadIntervalMs; got != wantInterval {
		t.Fatalf("capture workspace upload interval = %d ms, want %d ms", got, wantInterval)
	}
	api.mu.Unlock()

	// Stop the VM before capturing (as retireVMs does after a command exits 0).
	if err := b.StopVM(ctx, "ws-cap"); err != nil {
		t.Fatalf("StopVM: %v", err)
	}

	// 3. CaptureWorkspaceBase versions & syncs the cell, updates heldBases,
	// drops the bucket cell, and swaps ws-cap onto a local-only ephemeral fork.
	repos := map[string]string{"hotbed": "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}
	captured, newWsCell, err := b.CaptureWorkspaceBase(ctx, "ws-cap", "hotbed", "base-key-1", 3, repos, 1700000000, 1700000500000)
	if err != nil {
		t.Fatalf("CaptureWorkspaceBase: %v", err)
	}
	if !captured.AutoCaptured || captured.BaseCommitTime != 1700000000 || captured.CapturedAt != 1700000500000 {
		t.Fatalf("captured metadata mismatch: %+v", captured)
	}
	if captured.Ref.Cell != capCell.GetCell() || captured.Ref.Version == 0 {
		t.Fatalf("captured ref = %+v, want cell %d with non-zero version", captured.Ref, capCell.GetCell())
	}
	if newWsCell == nil || newWsCell.GetCell() == capCell.GetCell() {
		t.Fatalf("expected ws-cap swapped to a fresh ephemeral cell, got %+v (old %d)", newWsCell, capCell.GetCell())
	}
	api.mu.Lock()
	if len(api.forkReqs) != 3 || !api.forkReqs[2].GetEphemeral() || api.forkReqs[2].GetCell() != captured.Ref.Cell || api.forkReqs[2].GetVersion() != captured.Ref.Version {
		t.Fatalf("post-capture swap must fork ephemeral from captured version, got %+v", api.forkReqs[2])
	}
	api.mu.Unlock()

	held, fresh := b.heldBases().get("hotbed")
	if held == nil || !fresh || held.Ref != captured.Ref {
		t.Fatalf("heldBases after capture = (%+v, fresh=%v), want %+v", held, fresh, captured.Ref)
	}

	// 4. Releasing an uncaptured capture-enabled workspace deletes its cell.
	uncapCell, err := b.PrepareWorkspace(ctx, "ws-uncap", baseRef, 3, true)
	if err != nil {
		t.Fatalf("PrepareWorkspace uncaptured: %v", err)
	}
	_ = b.StopVM(ctx, "ws-uncap")
	if err := b.DeleteWorkspace(ctx, "ws-uncap"); err != nil {
		t.Fatalf("DeleteWorkspace uncaptured: %v", err)
	}
	api.mu.Lock()
	foundDelete := false
	for _, id := range api.deleted {
		if id == uncapCell.GetCell() {
			foundDelete = true
			break
		}
	}
	api.mu.Unlock()
	if !foundDelete {
		t.Fatalf("expected DeleteCell for uncaptured workspace cell %d, got %+v", uncapCell.GetCell(), api.deleted)
	}
}

func TestPickAutoCaptureBase(t *testing.T) {
	p := &pb.HostedProject{Name: "hotbed", ScratchEpoch: 2}
	oldEpoch := &projectBase{Ref: cladefs.CellRef{Tenant: 1, Cell: 1, Version: 1}, BaseKey: "k1", ScratchEpoch: 1, CapturedAt: 500}
	wrongKey := &projectBase{Ref: cladefs.CellRef{Tenant: 1, Cell: 2, Version: 1}, BaseKey: "k2", ScratchEpoch: 2, CapturedAt: 600}
	held := &projectBase{Ref: cladefs.CellRef{Tenant: 1, Cell: 3, Version: 1}, BaseKey: "k1", ScratchEpoch: 2, CapturedAt: 1000}
	sharedNewer := &projectBase{Ref: cladefs.CellRef{Tenant: 1, Cell: 4, Version: 1}, BaseKey: "k1", ScratchEpoch: 2, CapturedAt: 2000}

	if got := pickAutoCaptureBase(held, sharedNewer, true, false, p, "k1"); got != nil {
		t.Fatalf("fresh=true must return nil, got %+v", got)
	}
	if got := pickAutoCaptureBase(oldEpoch, wrongKey, false, false, p, "k1"); got != nil {
		t.Fatalf("incompatible held & shared must return nil, got %+v", got)
	}
	if got := pickAutoCaptureBase(held, sharedNewer, false, false, p, "k1"); got == nil || got.Ref.Cell != 4 {
		t.Fatalf("expected newer shared cell 4, got %+v", got)
	}
	if got := pickAutoCaptureBase(held, sharedNewer, false, true, p, "k1"); got == nil || got.Ref.Cell != 3 {
		t.Fatalf("expected locally fresh held cell 3 to win, got %+v", got)
	}
}

func TestEngineAutoCapturesAfterSuccessAndSwapsToEphemeral(t *testing.T) {
	ctx := t.Context()
	api := newStrictVMCladefsClient()
	fl := newFakeVMLauncher()
	vmb, _ := newTestVMBackend(t, api, fl)

	dir := t.TempDir()
	eng, err := NewEngine(ctx, EngineConfig{
		Dir:     dir,
		NodeID:  "node-1",
		Host:    "node-1",
		Store:   &fake.Store{Root: filepath.Join(dir, "volumes")},
		Units:   &fake.Units{},
		System:  fake.NewSystem(),
		VM:      vmb,
		Runtime: vmb.Runtime(),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { eng.Close() })
	go eng.Run(ctx)

	// Seed a warm shared base matching this backend's tenant and OS base key
	// so CreateWorkspace forks it immediately without building a base image tar.
	shared := &pb.SharedBase{
		Cell: &pb.CladefsCell{
			Tenant:  vmb.tenantID(),
			Cell:    100,
			Version: 5,
			BaseId:  "hotbed-vm-seed",
			Project: "hotbed",
			Runtime: "vm",
		},
		BaseKey:      vmb.baseKeyFor(eng.cfg.BaseSpec),
		ScratchEpoch: 1,
		Repos:        map[string]string{"hotbed": "1111111111111111111111111111111111111111"},
		CapturedAt:   1000,
	}

	ws, err := eng.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{
		Id:             "capws001",
		SessionId:      "test-sess",
		Agent:          "test",
		Repo:           "hotbed",
		SyncDir:        "/local/hotbed",
		SyncHost:       "laptop",
		Runtime:        "vm",
		Project:        &pb.HostedProject{Name: "hotbed", ScratchEpoch: 1, Repos: []*pb.Repo{{Id: "hotbed"}}},
		SharedBase:     shared,
		CaptureBase:    true,
		BaseCommit:     "2222222222222222222222222222222222222222",
		BaseCommitTime: 1700000100,
	})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	eventuallyTrue(t, "workspace ready", func() bool {
		got, err := eng.Workspace(ws.Id)
		return err == nil && got.State == pb.WorkspaceState_WORKSPACE_STATE_IDLE
	})
	gotWS, err := eng.Workspace(ws.Id)
	if err != nil {
		t.Fatal(err)
	}
	if !gotWS.CaptureBase {
		t.Fatal("expected ready workspace to have CaptureBase=true")
	}
	initialCell := gotWS.GetCladefsCell().GetCell()

	// 1. A failing command must NOT trigger auto-capture.
	failCmd, err := eng.Exec(ctx, &pb.RunCommandRequest{
		WorkspaceId: ws.Id,
		Args:        []string{"sh", "-c", "exit 1"},
		Kind:        "test",
	})
	if err != nil {
		t.Fatalf("Exec failing: %v", err)
	}
	eventuallyTrue(t, "failing command ended", func() bool {
		c, ok := eng.Command(failCmd.Id)
		return ok && c.State == pb.CommandState_COMMAND_STATE_FAILED
	})
	eng.retireVMs(ctx)
	if held, fresh := vmb.heldBases().get("hotbed"); fresh || held.Ref.Cell != 100 {
		t.Fatalf("failing command should not capture base; held=(%+v, fresh=%v)", held, fresh)
	}

	// 2. A succeeding command with --no-capture must NOT trigger auto-capture.
	noCapCmd, err := eng.Exec(ctx, &pb.RunCommandRequest{
		WorkspaceId: ws.Id,
		Args:        []string{"sh", "-c", "true"},
		Kind:        "shell",
		NoCapture:   true,
	})
	if err != nil {
		t.Fatalf("Exec no-capture: %v", err)
	}
	eventuallyTrue(t, "no-capture command ended", func() bool {
		c, ok := eng.Command(noCapCmd.Id)
		return ok && c.State == pb.CommandState_COMMAND_STATE_SUCCEEDED
	})
	eng.retireVMs(ctx)
	if held, fresh := vmb.heldBases().get("hotbed"); fresh || held.Ref.Cell != 100 {
		t.Fatalf("--no-capture command should not capture base; held=(%+v, fresh=%v)", held, fresh)
	}

	// 3. A succeeding command triggers auto-capture once its VM stops, holds the
	// captured base, clears CaptureBase, and swaps the workspace to an ephemeral cell.
	okCmd, err := eng.Exec(ctx, &pb.RunCommandRequest{
		WorkspaceId: ws.Id,
		Args:        []string{"sh", "-c", "true"},
		Kind:        "test",
	})
	if err != nil {
		t.Fatalf("Exec succeeding: %v", err)
	}
	eventuallyTrue(t, "succeeding command ended", func() bool {
		c, ok := eng.Command(okCmd.Id)
		return ok && c.State == pb.CommandState_COMMAND_STATE_SUCCEEDED
	})
	eng.retireVMs(ctx)

	eventuallyTrue(t, "workspace base captured and swapped to ephemeral", func() bool {
		cur, err := eng.Workspace(ws.Id)
		return err == nil && !cur.CaptureBase && cur.GetCladefsCell().GetCell() != initialCell
	})
	held, fresh := vmb.heldBases().get("hotbed")
	if held == nil || !fresh || !held.AutoCaptured || held.Ref.Cell != initialCell || held.Ref.Version == 0 {
		t.Fatalf("expected captured base on cell %d, got (%+v, fresh=%v)", initialCell, held, fresh)
	}
}
