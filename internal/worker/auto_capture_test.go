package worker

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	cladefsdv1 "github.com/semistrict/cladefs/go/cladefsd/v1"
	"google.golang.org/grpc"

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
	steps := map[string]string{"go-toolchain": "h-go", "node": "h-node"}
	captured, newWsCell, err := b.CaptureWorkspaceBase(ctx, "ws-cap", "hotbed", "base-key-1", "setup-h1", steps, 3, repos, 1700000000, 1700000500000)
	if err != nil {
		t.Fatalf("CaptureWorkspaceBase: %v", err)
	}
	if captured.SetupHash != "setup-h1" || captured.StepHashes["go-toolchain"] != "h-go" || captured.StepHashes["node"] != "h-node" {
		t.Fatalf("CaptureWorkspaceBase dropped step metadata: setup=%q steps=%v", captured.SetupHash, captured.StepHashes)
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

	// Issue #23: a project with setup steps must reject any captured base whose
	// StepHashes are empty (bare OS image) or stale, and accept one that ran all steps.
	pStepped := &pb.HostedProject{
		Name:         "hotbed",
		ScratchEpoch: 2,
		Steps: []*pb.Step{
			{Name: "go-toolchain", Hash: "h-go"},
			{Name: "node", Hash: "h-node"},
		},
	}
	poisoned := &projectBase{Ref: cladefs.CellRef{Tenant: 1, Cell: 5, Version: 1}, BaseKey: "k1", ScratchEpoch: 2, CapturedAt: 5000, AutoCaptured: true}
	staleStep := &projectBase{Ref: cladefs.CellRef{Tenant: 1, Cell: 6, Version: 1}, BaseKey: "k1", ScratchEpoch: 2, CapturedAt: 4000, StepHashes: map[string]string{"go-toolchain": "h-go", "node": "old"}}
	validStepped := &projectBase{Ref: cladefs.CellRef{Tenant: 1, Cell: 7, Version: 1}, BaseKey: "k1", ScratchEpoch: 2, CapturedAt: 3000, StepHashes: map[string]string{"go-toolchain": "h-go", "node": "h-node"}}
	if got := pickAutoCaptureBase(poisoned, staleStep, false, false, pStepped, "k1"); got != nil {
		t.Fatalf("unstepped/stale bases must be rejected when project has steps, got %+v", got)
	}
	if got := pickAutoCaptureBase(poisoned, validStepped, false, true, pStepped, "k1"); got == nil || got.Ref.Cell != 7 {
		t.Fatalf("expected valid stepped base cell 7 over poisoned held base, got %+v", got)
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

func TestVMBackendOnMachinedCaptureForksChildAndPreservesBaseOnDelete(t *testing.T) {
	ctx := t.Context()
	api := newStrictVMCladefsClient()
	fl := newFakeVMLauncher()
	b, _ := newTestVMBackend(t, api, fl)
	// Simulate local-only machined (macOS): no R2 bucket, so the local cell IS
	// the base and must NOT be dropped on capture or deleted when ws is released.
	b.machined = &grpc.ClientConn{}

	baseRef := cladefs.CellRef{Tenant: 0, Cell: 100, Version: 5}
	capCell, err := b.PrepareWorkspace(ctx, "ws-mac-cap", baseRef, 1, true)
	if err != nil {
		t.Fatalf("PrepareWorkspace machined capture: %v", err)
	}
	if err := b.StopVM(ctx, "ws-mac-cap"); err != nil {
		t.Fatalf("StopVM: %v", err)
	}

	captured, newWsCell, err := b.CaptureWorkspaceBase(ctx, "ws-mac-cap", "hotbed", "base-key-mac", "setup-mac", map[string]string{"go": "h1"}, 1, map[string]string{"hotbed": "aaaa"}, 1700000000, 1700000500000)
	if err != nil {
		t.Fatalf("CaptureWorkspaceBase machined: %v", err)
	}
	if captured.Ref.Cell != capCell.GetCell() {
		t.Fatalf("captured base cell = %d, want %d", captured.Ref.Cell, capCell.GetCell())
	}
	if newWsCell == nil || newWsCell.GetCell() == capCell.GetCell() {
		t.Fatalf("machined workspace must fork onto a distinct child cell after capture, got %+v (base %d)", newWsCell, capCell.GetCell())
	}
	api.mu.Lock()
	if len(api.dropped) != 0 {
		t.Fatalf("machined capture must not DropCell local base, dropped=%+v", api.dropped)
	}
	lastFork := api.forkReqs[len(api.forkReqs)-1]
	if lastFork.GetEphemeral() || lastFork.GetCell() != captured.Ref.Cell || lastFork.GetVersion() != captured.Ref.Version {
		t.Fatalf("machined post-capture fork must be non-ephemeral from captured version, got %+v", lastFork)
	}
	api.mu.Unlock()

	// Releasing the original workspace must delete only its child cell and
	// preserve the locally held base cell.
	if err := b.DeleteWorkspace(ctx, "ws-mac-cap"); err != nil {
		t.Fatalf("DeleteWorkspace: %v", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if !api.cells[captured.Ref.Cell] {
		t.Fatalf("releasing captured workspace deleted machined local base cell %d (deleted=%+v)", captured.Ref.Cell, api.deleted)
	}
	if api.cells[newWsCell.GetCell()] {
		t.Fatalf("expected child cell %d to be deleted on release", newWsCell.GetCell())
	}
}

// Issue #23: a project with setup steps preserves StepHashes/SetupHash across
// auto-capture, and a --fresh workspace forked from the bare OS image must
// never auto-capture and poison the project's warm base.
func TestSteppedProjectPreservesStepsOnCaptureAndRefusesBareOSCapture(t *testing.T) {
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

	baseKey := vmb.baseKeyFor(eng.cfg.BaseSpec)
	proj := &pb.HostedProject{
		Name:         "hotbed",
		ScratchEpoch: 1,
		Repos:        []*pb.Repo{{Id: "hotbed"}},
		Steps: []*pb.Step{
			{Name: "go-toolchain", Hash: "h-go"},
			{Name: "node", Hash: "h-node"},
		},
	}
	wantSteps := map[string]string{"go-toolchain": "h-go", "node": "h-node"}
	shared := &pb.SharedBase{
		Cell: &pb.CladefsCell{
			Tenant:  vmb.tenantID(),
			Cell:    200,
			Version: 5,
			BaseId:  "hotbed-vm-stepped",
			Project: "hotbed",
			Runtime: "vm",
		},
		BaseKey:      baseKey,
		SetupHash:    "setup-hash-1",
		ScratchEpoch: 1,
		Repos:        map[string]string{"hotbed": "1111111111111111111111111111111111111111"},
		StepHashes:   wantSteps,
		CapturedAt:   1000,
	}

	// 1. Fork from a valid stepped base and auto-capture: the captured base
	// must keep SetupHash and StepHashes so pickAutoCaptureBase accepts it.
	ws, err := eng.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{
		Id:             "stepcap1",
		SessionId:      "test-sess",
		Agent:          "test",
		Repo:           "hotbed",
		SyncDir:        "/local/hotbed",
		SyncHost:       "laptop",
		Runtime:        "vm",
		Project:        proj,
		SharedBase:     shared,
		CaptureBase:    true,
		BaseCommit:     "2222222222222222222222222222222222222222",
		BaseCommitTime: 1700000100,
	})
	if err != nil {
		t.Fatalf("CreateWorkspace stepped: %v", err)
	}
	eventuallyTrue(t, "stepped workspace ready", func() bool {
		got, err := eng.Workspace(ws.Id)
		return err == nil && got.State == pb.WorkspaceState_WORKSPACE_STATE_IDLE
	})
	cmd, err := eng.Exec(ctx, &pb.RunCommandRequest{
		WorkspaceId: ws.Id,
		Args:        []string{"sh", "-c", "true"},
		Kind:        "test",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	eventuallyTrue(t, "command succeeded", func() bool {
		c, ok := eng.Command(cmd.Id)
		return ok && c.State == pb.CommandState_COMMAND_STATE_SUCCEEDED
	})
	eng.retireVMs(ctx)
	eventuallyTrue(t, "stepped workspace captured", func() bool {
		held, fresh := vmb.heldBases().get("hotbed")
		return fresh && held != nil && held.AutoCaptured
	})
	held, _ := vmb.heldBases().get("hotbed")
	if held.SetupHash != "setup-hash-1" || held.StepHashes["go-toolchain"] != "h-go" || held.StepHashes["node"] != "h-node" {
		t.Fatalf("captured stepped base lost step metadata: %+v", held)
	}
	if picked := pickAutoCaptureBase(held, nil, false, true, proj, baseKey); picked == nil || picked.BaseID != held.BaseID {
		t.Fatalf("pickAutoCaptureBase rejected newly captured stepped base: %+v", picked)
	}

	// 2. A --fresh workspace on a stepped project forks from the bare OS image
	// and must NEVER set CaptureBase=true even when requested.
	vmb.imagesMu.Lock()
	vmb.baseImages[baseKey] = cladefs.CellRef{Tenant: vmb.tenantID(), Cell: 99, Version: 1}
	vmb.imagesMu.Unlock()
	freshWS, err := eng.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{
		Id:          "freshst1",
		SessionId:   "test-sess",
		Agent:       "test",
		Repo:        "hotbed",
		SyncDir:     "/local/hotbed",
		SyncHost:    "laptop",
		Runtime:     "vm",
		Project:     proj,
		Fresh:       true,
		CaptureBase: true,
	})
	if err != nil {
		t.Fatalf("CreateWorkspace --fresh: %v", err)
	}
	eventuallyTrue(t, "fresh workspace ready", func() bool {
		got, err := eng.Workspace(freshWS.Id)
		return err == nil && got.State == pb.WorkspaceState_WORKSPACE_STATE_IDLE
	})
	gotFresh, err := eng.Workspace(freshWS.Id)
	if err != nil {
		t.Fatal(err)
	}
	if gotFresh.CaptureBase {
		t.Fatalf("--fresh workspace on stepped project must not be capture-eligible (would poison warm base)")
	}
}

// Local-only machined (macOS) workers keep every cell strictly on local disk
// with no R2 bucket: they must never publish held bases in HeartbeatMeta and
// must never adopt or fork a controller-supplied SharedBase from another node.
func TestVMBackendOnMachinedNeverReportsOrAdoptsSharedBases(t *testing.T) {
	ctx := t.Context()
	api := newStrictVMCladefsClient()
	fl := newFakeVMLauncher()
	vmb, baseKey := newTestVMBackend(t, api, fl)
	vmb.machined = &grpc.ClientConn{}

	// Seed a local OS base image so a cold synced workspace can fork locally
	// without contacting R2 or provisioning from OCI.
	vmb.imagesMu.Lock()
	vmb.baseImages[baseKey] = cladefs.CellRef{Tenant: vmb.tenantID(), Cell: 99, Version: 1}
	vmb.imagesMu.Unlock()

	// Record a locally captured base and confirm HeartbeatMeta hides it.
	vmb.heldBases().hold("localproj", &projectBase{
		Ref:          cladefs.CellRef{Tenant: 0, Cell: 100, Version: 5},
		BaseID:       "localproj-vm-cap-1",
		BaseKey:      baseKey,
		AutoCaptured: true,
	}, true, true)
	if peer, inc, held, retired := vmb.HeartbeatMeta(); peer != "" || inc != 0 || len(held) != 0 || len(retired) != 0 {
		t.Fatalf("machined HeartbeatMeta = (%q, %d, %d held, %d retired), want all empty", peer, inc, len(held), len(retired))
	}

	dir := t.TempDir()
	eng, err := NewEngine(ctx, EngineConfig{
		Dir:     dir,
		NodeID:  "mac-node",
		Host:    "mac",
		Store:   &fake.Store{Root: filepath.Join(dir, "volumes")},
		Units:   &fake.Units{},
		System:  fake.NewSystem(),
		VM:      vmb,
		Runtime: vmb.Runtime(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	baseKey = vmb.baseKeyFor(eng.cfg.BaseSpec)
	vmb.imagesMu.Lock()
	vmb.baseImages[baseKey] = cladefs.CellRef{Tenant: vmb.tenantID(), Cell: 99, Version: 1}
	vmb.imagesMu.Unlock()

	// Controller hands down another node's SharedBase (cell 777, not on this Mac).
	remoteShared := &pb.SharedBase{
		Cell: &pb.CladefsCell{
			Tenant:  0,
			Cell:    777,
			Version: 12,
			BaseId:  "other-mac-cap",
			Project: "cladefs",
			Arch:    "arm64",
			Runtime: "vm",
		},
		BaseKey:      baseKey,
		AutoCaptured: true,
	}
	proj := &pb.HostedProject{
		Name:  "cladefs",
		Repos: []*pb.Repo{{Id: "cladefs", Url: "https://example.invalid/cladefs.git", Branch: "main"}},
	}
	ws, err := eng.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{
		Id:         "macws1",
		SessionId:  "sess-mac",
		Agent:      "agent",
		Repo:       "cladefs",
		SyncDir:    "/local/cladefs",
		SyncHost:   "laptop",
		Runtime:    "vm",
		Project:    proj,
		SharedBase: remoteShared,
	})
	if err != nil {
		t.Fatalf("CreateWorkspace on machined: %v", err)
	}
	if ws.State != pb.WorkspaceState_WORKSPACE_STATE_IDLE {
		t.Fatalf("fast local fork returned state %v, want IDLE", ws.State)
	}
	api.mu.Lock()
	for _, fr := range api.forkReqs {
		if fr.GetCell() == 777 {
			api.mu.Unlock()
			t.Fatalf("machined forked controller-supplied remote SharedBase cell 777: %+v", fr)
		}
	}
	api.mu.Unlock()
	if held, _ := vmb.heldBases().get("cladefs"); held != nil && held.Ref.Cell == 777 {
		t.Fatalf("machined poisoned heldBases with remote SharedBase cell 777: %+v", held)
	}
}

// Releasing a PREPARING cladefs workspace (e.g. a losing 2-host race) must
// cancel its in-flight prepare context immediately instead of hanging until
// ReleaseWorkspace times out.
func TestReleaseCancelsSlowCladefsPrepareAndDoesNotPoisonHeldBase(t *testing.T) {
	ctx := t.Context()
	api := newStrictVMCladefsClient()
	forkStarted := make(chan struct{}, 1)
	api.forkHook = func(fctx context.Context, _ *cladefsdv1.ForkCellRequest) error {
		select {
		case forkStarted <- struct{}{}:
		default:
		}
		<-fctx.Done()
		return fctx.Err()
	}
	fl := newFakeVMLauncher()
	vmb, _ := newTestVMBackend(t, api, fl)

	dir := t.TempDir()
	eng, err := NewEngine(ctx, EngineConfig{
		Dir:     dir,
		NodeID:  "node-slow",
		Host:    "slow",
		Store:   &fake.Store{Root: filepath.Join(dir, "volumes")},
		Units:   &fake.Units{},
		System:  fake.NewSystem(),
		VM:      vmb,
		Runtime: vmb.Runtime(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	baseKey := vmb.baseKeyFor(eng.cfg.BaseSpec)

	shared := &pb.SharedBase{
		Cell: &pb.CladefsCell{
			Tenant:  0,
			Cell:    555,
			Version: 3,
			BaseId:  "demo-vm-shared",
			Project: "demo",
			Arch:    "arm64",
			Runtime: "vm",
		},
		BaseKey:      baseKey,
		AutoCaptured: true,
	}
	proj := &pb.HostedProject{
		Name:  "demo",
		Repos: []*pb.Repo{{Id: "demo", Url: "https://example.invalid/demo.git", Branch: "main"}},
	}

	// Start CreateWorkspace with a short caller context; prepare stays running
	// in the background until ReleaseWorkspace cancels forkCtx.
	createCtx, cancelCreate := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancelCreate()
	_, _ = eng.CreateWorkspace(createCtx, &pb.CreateWorkspaceRequest{
		Id:         "slowws1",
		SessionId:  "sess-race",
		Agent:      "agent",
		Repo:       "demo",
		SyncDir:    "/local/demo",
		SyncHost:   "laptop",
		Runtime:    "vm",
		Project:    proj,
		SharedBase: shared,
	})
	select {
	case <-forkStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("ForkVersion never started")
	}

	relCtx, cancelRel := context.WithTimeout(ctx, 2*time.Second)
	defer cancelRel()
	if err := eng.ReleaseWorkspace(relCtx, "slowws1"); err != nil {
		t.Fatalf("ReleaseWorkspace blocked or failed on hung prepare: %v", err)
	}
	if held, _ := vmb.heldBases().get("demo"); held != nil {
		t.Fatalf("failed/canceled prepare poisoned heldBases with %+v", held)
	}
}
