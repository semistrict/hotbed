package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/cladefs"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/sysinfo"
	"github.com/semistrict/hotbed/internal/vm"
)

func intPtr(v int) *int           { return &v }
func floatPtr(v float64) *float64 { return &v }
func boolPtr(v bool) *bool        { return &v }

func TestFreshenBudgetDutyCycleAndMinInterval(t *testing.T) {
	now := time.Unix(1795600000, 0)
	b := newFreshenBudget(nil, nil)

	// 1. Short 40s build: duty wait = 40s * 19 = 760s (~12.7m) on node,
	// while minInterval (30m) governs the same project.
	b.recordSuccess(now, "hotbed", runtime.KindVM, 40*time.Second)

	// At +10m: both hotbed and another project (cladefs) are blocked by node debt (12m40s).
	dHotbed10 := b.decide(now.Add(10*time.Minute), "hotbed", runtime.KindVM, "r2", "s1", 0, "b1", true, false, false, false)
	if dHotbed10.action != freshenActionSkip || dHotbed10.note != "" {
		t.Fatalf("hotbed at +10m = %+v, want clean skip", dHotbed10)
	}
	dCladefs10 := b.decide(now.Add(10*time.Minute), "cladefs", runtime.KindVM, "c2", "s1", 0, "cb1", true, false, false, false)
	if dCladefs10.action != freshenActionSkip {
		t.Fatalf("cladefs at +10m = %+v, want skip due to node duty-cycle debt", dCladefs10)
	}

	// At +13m: node debt (12m40s) cleared so cladefs can optionally freshen,
	// while hotbed is still inside its 30m per-project minInterval.
	dCladefs13 := b.decide(now.Add(13*time.Minute), "cladefs", runtime.KindVM, "c2", "s1", 0, "cb1", true, false, false, false)
	if dCladefs13.action != freshenActionRunForeground {
		t.Fatalf("cladefs at +13m = %+v, want allowed", dCladefs13)
	}
	dHotbed13 := b.decide(now.Add(13*time.Minute), "hotbed", runtime.KindVM, "r2", "s1", 0, "b1", true, false, false, false)
	if dHotbed13.action != freshenActionSkip {
		t.Fatalf("hotbed at +13m = %+v, want still throttled by 30m minInterval", dHotbed13)
	}
	dHotbed31 := b.decide(now.Add(31*time.Minute), "hotbed", runtime.KindVM, "r2", "s1", 0, "b1", true, false, true, false)
	if dHotbed31.action != freshenActionDeferBackground {
		t.Fatalf("hotbed at +31m with allowBackground = %+v, want deferBackground", dHotbed31)
	}

	// 2. Heavy 12m build: duty wait = 12m * 19 = 228m (3h48m).
	t2 := now.Add(31 * time.Minute)
	b.recordSuccess(t2, "cladefs", runtime.KindVM, 12*time.Minute)
	if d := b.decide(t2.Add(3*time.Hour+40*time.Minute), "cladefs", runtime.KindVM, "c3", "s1", 0, "cb2", true, false, false, false); d.action != freshenActionSkip {
		t.Fatalf("cladefs at +3h40m after 12m build = %+v, want skip", d)
	}
	if d := b.decide(t2.Add(3*time.Hour+50*time.Minute), "cladefs", runtime.KindVM, "c3", "s1", 0, "cb2", true, false, false, false); d.action != freshenActionRunForeground {
		t.Fatalf("cladefs at +3h50m after 12m build = %+v, want allowed", d)
	}

	// 3. Huge 25m cold build: 25m * 19 = 475m -> clamped to maxCooldown (6h).
	t3 := t2.Add(4 * time.Hour)
	b.recordSuccess(t3, "cladefs", runtime.KindVM, 25*time.Minute)
	if d := b.decide(t3.Add(5*time.Hour+59*time.Minute), "cladefs", runtime.KindVM, "c4", "s1", 0, "cb3", true, false, false, false); d.action != freshenActionSkip {
		t.Fatalf("cladefs at +5h59m after 25m build = %+v, want skip", d)
	}
	if d := b.decide(t3.Add(6*time.Hour+time.Second), "cladefs", runtime.KindVM, "c4", "s1", 0, "cb3", true, false, false, false); d.action != freshenActionRunForeground {
		t.Fatalf("cladefs at +6h01s after 25m build = %+v, want allowed (clamped at 6h)", d)
	}

	// 4. Required structural builds ALWAYS bypass duty-cycle / minInterval throttling.
	if d := b.decide(t3.Add(time.Minute), "cladefs", runtime.KindVM, "c-struct", "s2", 0, "cb3", true, true, true, false); d.action != freshenActionRunForeground || !d.required {
		t.Fatalf("required build during cooldown = %+v, want runForeground", d)
	}

	// 5. DisableOptional suppresses commit-only freshens even after cooldown expires,
	// while still permitting required builds.
	b.updateConfig(&statedir.FreshenConfig{DisableOptional: boolPtr(true)})
	if d := b.decide(t3.Add(24*time.Hour), "cladefs", runtime.KindVM, "c5", "s1", 0, "cb3", true, false, false, false); d.action != freshenActionSkip {
		t.Fatalf("optional build with DisableOptional=true = %+v, want skip", d)
	}
	if d := b.decide(t3.Add(24*time.Hour), "cladefs", runtime.KindVM, "c5", "s2", 0, "cb3", true, true, false, false); d.action != freshenActionRunForeground {
		t.Fatalf("required build with DisableOptional=true = %+v, want runForeground", d)
	}

	// 6. Explicit zero overrides disable minInterval and hard limits cleanly.
	b.updateConfig(&statedir.FreshenConfig{
		MaxDutyCycle:               floatPtr(1.0),
		MinIntervalSeconds:         intPtr(0),
		MaxOptionalDurationSeconds: intPtr(0),
		MaxRequiredDurationSeconds: intPtr(0),
	})
	if b.hardLimitSeconds(true) != 0 || b.hardLimitSeconds(false) != 0 {
		t.Fatalf("explicit 0 hard limits = (%d, %d), want (0, 0)", b.hardLimitSeconds(true), b.hardLimitSeconds(false))
	}
}

func TestFreshenBudgetFailureBackoffAndSetupChangeClear(t *testing.T) {
	now := time.Unix(1795600000, 0)
	b := newFreshenBudget(nil, nil)

	// Failure 1 on warm project: suppresses exact recipe AND enforces 15m backoff even on new commits.
	b.recordFailure(now, "cladefs", runtime.KindVM, "bad-r1", "setup-1", 0, "cladefs-vm-bad1", "f-1", "base cladefs-vm-bad1 failed: oom", false, true, false, 2*time.Minute)

	// Same recipe after 1h -> STILL suppressed (exact failed recipe never loops).
	dSame := b.decide(now.Add(time.Hour), "cladefs", runtime.KindVM, "bad-r1", "setup-1", 0, "cladefs-vm-good", true, false, false, false)
	if dSame.action != freshenActionSkip || !strings.Contains(dSame.note, "on cladefs-vm-good, not up to date: base cladefs-vm-bad1 failed: oom") {
		t.Fatalf("same failed recipe at +1h = %+v, want skip with fallback note", dSame)
	}

	// New commit (different recipe) at +10m -> STILL blocked by 15m failure backoff!
	dNew10 := b.decide(now.Add(10*time.Minute), "cladefs", runtime.KindVM, "new-r2", "setup-1", 0, "cladefs-vm-good", true, true, false, false)
	if dNew10.action != freshenActionSkip || !strings.Contains(dNew10.note, "on cladefs-vm-good, not up to date:") {
		t.Fatalf("new commit during 15m failure backoff = %+v, want skip with fallback note", dNew10)
	}

	// Setup step fixed (setupHash changed) at +1m -> IMMEDIATELY clears failure suppression & backoff!
	dFixed := b.decide(now.Add(time.Minute), "cladefs", runtime.KindVM, "new-r2", "setup-2", 0, "cladefs-vm-good", true, true, false, false)
	if dFixed.action != freshenActionRunForeground {
		t.Fatalf("after setupHash changed = %+v, want runForeground immediately", dFixed)
	}

	// Exponential backoff on cold project (no warm base): starts at 2m -> 4m -> 8m.
	b.recordFailure(now, "coldproj", runtime.KindCladefs, "cold-r1", "cs1", 0, "coldproj-fuse-1", "f-c1", "base coldproj-fuse-1 failed: oom", false, false, false, 3*time.Minute)
	dCold1 := b.decide(now.Add(time.Minute), "coldproj", runtime.KindCladefs, "cold-r1", "cs1", 0, "", false, true, false, false)
	if dCold1.action != freshenActionSkip || dCold1.err == nil || !api.FromConnect(dCold1.err).Retryable {
		t.Fatalf("cold non-step failure at +1m = %+v, want retryable error during 2m backoff", dCold1)
	}
	// Explicit project rebuild (scratchEpoch bump) clears cold failure backoff immediately.
	dColdRebuild := b.decide(now.Add(time.Minute), "coldproj", runtime.KindCladefs, "cold-r2", "cs1", 1, "", false, true, false, false)
	if dColdRebuild.action != freshenActionRunForeground {
		t.Fatalf("cold project after scratchEpoch bump = %+v, want runForeground immediately", dColdRebuild)
	}

	// Cold deterministic step failure fails fast without waiting on retry window.
	b.recordFailure(now, "stepfail", runtime.KindCladefs, "sf-r1", "sfs1", 0, "stepfail-fuse-1", "f-s1", "base stepfail-fuse-1 failed at step build", true, false, false, time.Minute)
	dStep := b.decide(now.Add(10*time.Hour), "stepfail", runtime.KindCladefs, "sf-r1", "sfs1", 0, "", false, true, false, false)
	if dStep.action != freshenActionSkip || dStep.err == nil || api.CodeOf(dStep.err) != api.CodeWorkspaceFailed {
		t.Fatalf("cold step failure = %+v, want WORKSPACE_FAILED fast-fail", dStep)
	}

	// Shutdown cancellation charges active time (duty cycle) WITHOUT incrementing failure streak.
	tShut := now.Add(20 * time.Hour)
	b.recordFailure(tShut, "shutproj", runtime.KindVM, "shut-r1", "sh1", 0, "shut-1", "f-sh1", "context canceled", false, true, true, 5*time.Minute)
	// Required build on shut-r1 is NOT suppressed after shutdown.
	dShutReq := b.decide(tShut.Add(time.Second), "shutproj", runtime.KindVM, "shut-r1", "sh1", 0, "shut-prev", true, true, false, false)
	if dShutReq.action != freshenActionRunForeground {
		t.Fatalf("required build after shutdown cancel = %+v, want allowed", dShutReq)
	}
	// Optional build IS throttled by the 5m * 19 = 95m duty-cycle charge!
	dShutOpt := b.decide(tShut.Add(60*time.Minute), "shutproj", runtime.KindVM, "shut-r1", "sh1", 0, "shut-prev", true, false, false, false)
	if dShutOpt.action != freshenActionSkip || dShutOpt.throttledUntilMs == 0 {
		t.Fatalf("optional build at +60m after 5m shutdown-cancelled build = %+v, want throttled", dShutOpt)
	}
}

func TestFreshenBudgetPersistsAcrossWorkerRestart(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1795600000, 0)
	store := &fake.Store{Root: t.TempDir()}
	baseImg := fake.BaseImage(store)

	open := func() *Engine {
		e, err := NewEngine(t.Context(), EngineConfig{
			Dir:       dir,
			NodeID:    "n-persist",
			Host:      "n-persist",
			BaseImage: baseImg,
			Store:     store,
			Units:     &fake.Units{},
			System:    fake.NewSystem(),
			Now:       func() time.Time { return now },
		})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}

	e1 := open()
	e1.freshenBudget.recordSuccess(now, "cladefs", runtime.KindVM, 10*time.Minute)
	e1.freshenBudget.recordFailure(now, "hotbed", runtime.KindVM, "bad-rec", "sh1", 0, "hotbed-vm-bad", "f-bad", "base hotbed-vm-bad failed: boom", false, true, false, time.Minute)
	if err := e1.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen engine on the same worker.db: cooldown debt and failure suppression MUST persist.
	e2 := open()
	defer e2.Close()

	dCladefs := e2.freshenBudget.decide(now.Add(60*time.Minute), "cladefs", runtime.KindVM, "r-next", "s1", 0, "cb1", true, false, false, false)
	if dCladefs.action != freshenActionSkip || dCladefs.throttledUntilMs == 0 {
		t.Fatalf("after restart, cladefs duty-cycle debt lost: %+v", dCladefs)
	}
	dHotbed := e2.freshenBudget.decide(now.Add(2*time.Hour), "hotbed", runtime.KindVM, "bad-rec", "sh1", 0, "hb1", true, false, false, false)
	if dHotbed.action != freshenActionSkip || !strings.Contains(dHotbed.note, "boom") {
		t.Fatalf("after restart, hotbed failed recipe suppression lost: %+v", dHotbed)
	}
}

func TestVMBackendFreshenCapFallbackAndDedupe(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	apiClient := &fakeRetireCladefsClient{nextCell: 1000, nextVersion: 10}
	cstore, err := cladefs.NewStore(ctx, cladefs.Config{CellRoot: filepath.Join(tmp, "cells")}, apiClient, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := vm.NewManager(vm.Config{
		RunDir:         filepath.Join(tmp, "vms"),
		DisableNetwork: true,
	}, vm.WithLauncher(failingVMLauncher{}), vm.WithToolsDiskBuilder(func(string, string) (string, error) {
		return filepath.Join(tmp, "tools.ext4"), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	b := &VMBackend{
		workerDir:    tmp,
		mgr:          mgr,
		baseImages:   map[string]cladefs.CellRef{},
		wsCells:      map[string]cladefs.CellRef{},
		activeBuilds: map[string]struct{}{},
	}
	b.store.Store(cstore)
	spec := baseimage.Spec{Image: "debian:trixie-slim", Arch: "amd64"}.Normalized()
	baseKey := b.vmBaseKey(spec)
	b.baseImages[baseKey] = cladefs.CellRef{Tenant: 0, Cell: 900, Version: 1}

	now := time.Unix(1795600000, 0)
	budget := newFreshenBudget(nil, nil)
	proj := &pb.HostedProject{
		Name:  "demo",
		Repos: []*pb.Repo{{Id: "app", Url: "https://example.invalid/app.git", Branch: "main"}},
		Steps: []*pb.Step{{Name: "build", Hash: "h1", Script: "echo ok", EveryPull: true}},
	}

	var runs atomic.Int32
	var failNext atomic.Bool
	var lastOptional atomic.Bool
	runFreshen := func(_ context.Context, s managedFreshenSpec) (*managedFreshenHandle, error) {
		runs.Add(1)
		lastOptional.Store(s.Optional)
		if failNext.Load() {
			return &managedFreshenHandle{
				baseID: s.BaseID,
				cmdID:  "f-fail",
				stubResult: &managedFreshenResult{
					BaseID:           s.BaseID,
					CommandID:        "f-fail",
					State:            failed,
					Reason:           "exit 1",
					ActiveRunSeconds: 60,
					Started:          true,
					StepFailed:       true,
					Steps:            []*pb.AppliedStep{{Name: "build", Hash: "h2", Ran: true, Ok: false}},
				},
			}, nil
		}
		return &managedFreshenHandle{
			baseID: s.BaseID,
			cmdID:  "f-ok",
			stubResult: &managedFreshenResult{
				BaseID:           s.BaseID,
				CommandID:        "f-ok",
				RepoHeads:        s.Heads,
				State:            succeeded,
				ActiveRunSeconds: 30,
				Started:          true,
			},
		}, nil
	}

	opts := func(allowBG bool) ensureBaseOpts {
		return ensureBaseOpts{
			Budget:          budget,
			Now:             func() time.Time { return now },
			AllowBackground: allowBG,
		}
	}

	// 1. Cold build succeeds; SetupHash and Runtime="vm" are recorded.
	res1, err := b.ensureProjectBase(ctx, proj, map[string]string{"app": "c1"}, nil, spec, nil, nil, runFreshen, opts(true))
	if err != nil {
		t.Fatalf("cold ensureProjectBase: %v", err)
	}
	if runs.Load() != 1 || lastOptional.Load() {
		t.Fatalf("cold build runs=%d optional=%v, want 1 false", runs.Load(), lastOptional.Load())
	}
	if res1.Base.SetupHash == "" {
		t.Fatalf("expected SetupHash populated on warm base")
	}
	if _, _, held, _ := b.HeartbeatMeta(); len(held) != 1 || held[0].GetCell().GetRuntime() != runtime.KindVM {
		t.Fatalf("held bases = %+v, want runtime=vm", held)
	}

	// 2. Commit-only drift within minInterval (30m): skipped, returns warm base with empty note.
	res2, err := b.ensureProjectBase(ctx, proj, map[string]string{"app": "c2"}, nil, spec, nil, nil, runFreshen, opts(true))
	if err != nil || res2.Base.BaseID != res1.Base.BaseID || res2.Note != "" || res2.DeferBackground || runs.Load() != 1 {
		t.Fatalf("throttled commit-only = (%+v, %v, runs=%d), want warm reuse without build", res2, err, runs.Load())
	}

	// 3. After 31m with AllowBackground=true: returns warm base IMMEDIATELY with DeferBackground=true
	// EVEN WHILE projectLock is held by an in-flight background freshen!
	now = now.Add(31 * time.Minute)
	lock := b.held.lock(proj.Name)
	lock.Lock()
	done := make(chan ensureBaseResult, 1)
	go func() {
		r, gerr := b.ensureProjectBase(ctx, proj, map[string]string{"app": "c2"}, nil, spec, nil, nil, runFreshen, opts(true))
		if gerr != nil {
			t.Errorf("lock-free defer err: %v", gerr)
		}
		done <- r
	}()
	select {
	case res3 := <-done:
		if res3.Base.BaseID != res1.Base.BaseID || !res3.DeferBackground || runs.Load() != 1 {
			t.Fatalf("deferred result = %+v (runs=%d), want warm base + DeferBackground=true", res3, runs.Load())
		}
	case <-time.After(2 * time.Second):
		lock.Unlock()
		t.Fatal("AllowBackground ensureProjectBase blocked on projectLock instead of returning warm base immediately")
	}
	lock.Unlock()

	// 4. Structural step change triggers required build -> fails -> falls back to warm base with note!
	projBroken := &pb.HostedProject{
		Name:  "demo",
		Repos: proj.Repos,
		Steps: []*pb.Step{{Name: "build", Hash: "h2-broken", Script: "exit 1", EveryPull: true}},
	}
	failNext.Store(true)
	res4, err := b.ensureProjectBase(ctx, projBroken, map[string]string{"app": "c2"}, nil, spec, nil, nil, runFreshen, opts(true))
	if err != nil {
		t.Fatalf("failed freshen with warm base must fall back, got err: %v", err)
	}
	if res4.Base.BaseID != res1.Base.BaseID || !strings.Contains(res4.Note, "on "+res1.Base.BaseID+", not up to date:") || !strings.Contains(res4.Note, "failed at step build") {
		t.Fatalf("fallback result = %+v, want warm base with failed-step note", res4)
	}
	if runs.Load() != 2 {
		t.Fatalf("runs after first failure = %d, want 2", runs.Load())
	}

	// 5. Second caller with same broken recipe is deduped immediately (runs stays 2!).
	res5, err := b.ensureProjectBase(ctx, projBroken, map[string]string{"app": "c2"}, nil, spec, nil, nil, runFreshen, opts(true))
	if err != nil || res5.Base.BaseID != res1.Base.BaseID || !strings.Contains(res5.Note, "failed at step build") || runs.Load() != 2 {
		t.Fatalf("deduped retry = (%+v, %v, runs=%d), want immediate fallback without rebuilding", res5, err, runs.Load())
	}

	// 6. Fixing the step (new hash -> new setupHash) clears suppression immediately and builds!
	projFixed := &pb.HostedProject{
		Name:  "demo",
		Repos: proj.Repos,
		Steps: []*pb.Step{{Name: "build", Hash: "h3-fixed", Script: "echo ok", EveryPull: true}},
	}
	failNext.Store(false)
	res6, err := b.ensureProjectBase(ctx, projFixed, map[string]string{"app": "c2"}, nil, spec, nil, nil, runFreshen, opts(true))
	if err != nil || res6.Base.BaseID == res1.Base.BaseID || res6.Note != "" || runs.Load() != 3 {
		t.Fatalf("after fix = (%+v, %v, runs=%d), want new warm base", res6, err, runs.Load())
	}
}

func TestCladefsFUSEBackendFreshenCapFallbackAndDedupe(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	apiClient := &fakeRetireCladefsClient{nextCell: 2000, nextVersion: 20}
	mnt := &fakeRetireMountClient{}
	cstore, err := cladefs.NewStore(ctx, cladefs.Config{CellRoot: filepath.Join(tmp, "cells")}, apiClient, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cstore.SetMount(mnt)
	b := &CladefsFUSEBackend{
		workerDir:       tmp,
		mountRoot:       filepath.Join(tmp, "mnt"),
		mountStoresRoot: filepath.Join(tmp, "mount-stores"),
		baseImages:      map[string]cladefs.CellRef{},
		wsMounts:        map[string]fuseWSMount{},
		clearNosuidFn:   func(string) error { return nil },
		cleanupMount:    func(string, bool) {},
	}
	b.store.Store(cstore)
	spec := baseimage.Spec{Image: "debian:trixie-slim", Arch: "arm64"}.Normalized()
	baseKey := b.fuseBaseKey(spec)
	b.baseImages[baseKey] = cladefs.CellRef{Tenant: 0, Cell: 1900, Version: 1}

	now := time.Unix(1795600000, 0)
	budget := newFreshenBudget(nil, nil)
	proj := &pb.HostedProject{
		Name:  "demo",
		Repos: []*pb.Repo{{Id: "app", Url: "https://example.invalid/app.git", Branch: "main"}},
		Steps: []*pb.Step{{Name: "build", Hash: "h1", Script: "echo ok", EveryPull: true}},
	}

	var runs atomic.Int32
	var failNext atomic.Bool
	runFreshen := func(_ context.Context, s managedFreshenSpec) (*managedFreshenHandle, error) {
		runs.Add(1)
		if failNext.Load() {
			return &managedFreshenHandle{
				baseID: s.BaseID,
				cmdID:  "f-fuse-fail",
				stubResult: &managedFreshenResult{
					BaseID:           s.BaseID,
					CommandID:        "f-fuse-fail",
					State:            failed,
					Reason:           "killed by the OOM killer",
					ActiveRunSeconds: 120,
					Started:          true,
				},
			}, nil
		}
		return &managedFreshenHandle{
			baseID: s.BaseID,
			cmdID:  "f-fuse-ok",
			stubResult: &managedFreshenResult{
				BaseID:           s.BaseID,
				CommandID:        "f-fuse-ok",
				RepoHeads:        s.Heads,
				State:            succeeded,
				ActiveRunSeconds: 45,
				Started:          true,
			},
		}, nil
	}

	opts := func(allowBG bool) ensureBaseOpts {
		return ensureBaseOpts{
			Budget:          budget,
			Now:             func() time.Time { return now },
			AllowBackground: allowBG,
		}
	}

	// 1. Cold build succeeds with Runtime="cladefs".
	res1, err := b.ensureProjectBase(ctx, proj, map[string]string{"app": "c1"}, nil, spec, nil, runFreshen, nil, nil, opts(true))
	if err != nil || runs.Load() != 1 {
		t.Fatalf("cold cladefs ensureProjectBase = (%+v, %v, runs=%d)", res1, err, runs.Load())
	}
	if _, _, held, _ := b.HeartbeatMeta(); len(held) != 1 || held[0].GetCell().GetRuntime() != runtime.KindCladefs {
		t.Fatalf("held bases = %+v, want runtime=cladefs", held)
	}

	// 2. OOM failure on structural step change falls back to warm base and suppresses retry!
	proj2 := &pb.HostedProject{
		Name:  "demo",
		Repos: proj.Repos,
		Steps: []*pb.Step{{Name: "build", Hash: "h2", Script: "oom", EveryPull: true}},
	}
	failNext.Store(true)
	res2, err := b.ensureProjectBase(ctx, proj2, map[string]string{"app": "c2"}, nil, spec, nil, runFreshen, nil, nil, opts(true))
	if err != nil || res2.Base.BaseID != res1.Base.BaseID || !strings.Contains(res2.Note, "killed by the OOM killer") {
		t.Fatalf("cladefs OOM fallback = (%+v, %v), want warm fallback with OOM note", res2, err)
	}
	// Immediate concurrent/follow-up caller gets warm fallback with zero retry!
	res3, err := b.ensureProjectBase(ctx, proj2, map[string]string{"app": "c2"}, nil, spec, nil, runFreshen, nil, nil, opts(true))
	if err != nil || res3.Base.BaseID != res1.Base.BaseID || runs.Load() != 2 {
		t.Fatalf("cladefs dedupe = (%+v, %v, runs=%d), want runs=2", res3, err, runs.Load())
	}

	// 3. Even a brand-new commit during 15m failure backoff does NOT retry!
	res4, err := b.ensureProjectBase(ctx, proj2, map[string]string{"app": "c3"}, nil, spec, nil, runFreshen, nil, nil, opts(true))
	if err != nil || res4.Base.BaseID != res1.Base.BaseID || runs.Load() != 2 {
		t.Fatalf("cladefs new-commit during backoff = (%+v, %v, runs=%d), want runs=2", res4, err, runs.Load())
	}

	// 4. R2 shared base adoption (Fast Path 2) clears local failure state at 0 build cost!
	recipeC3 := fuseProjectRecipe(baseKey, proj2, map[string]string{"app": "c3"})
	adopted, err := b.ensureProjectBase(ctx, proj2, map[string]string{"app": "c3"}, &pb.SharedBase{Cell: &pb.CladefsCell{
		Project: "demo",
		Arch:    "arm64",
		BaseId:  "demo-fuse-adopted",
		Recipe:  recipeC3,
		Runtime: runtime.KindCladefs,
		Tenant:  0,
		Cell:    7777,
		Version: 99,
	}}, spec, nil, runFreshen, nil, nil, opts(true))
	if err != nil || adopted.Base.BaseID != "demo-fuse-adopted" || runs.Load() != 2 {
		t.Fatalf("R2 adoption = (%+v, %v, runs=%d), want demo-fuse-adopted with runs=2", adopted, err, runs.Load())
	}
}

func TestBackgroundOptionalFreshenIdleGated(t *testing.T) {
	var nowMu sync.Mutex
	curNow := time.Unix(1795600000, 0)
	nowFn := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return curNow
	}
	advance := func(d time.Duration) {
		nowMu.Lock()
		curNow = curNow.Add(d)
		nowMu.Unlock()
	}

	store := &fake.Store{Root: t.TempDir()}
	units := &fake.Units{}
	t.Cleanup(units.Close)
	sys := fake.NewSystem()
	sys.Mem = sysinfo.Mem{Total: 10 << 30, Available: 9 << 30, SwapTotal: 8 << 30, SwapFree: 8 << 30}

	e, err := NewEngine(t.Context(), EngineConfig{
		Dir:             t.TempDir(),
		NodeID:          "n-bg",
		Host:            "n-bg",
		BaseImage:       fake.BaseImage(store),
		Store:           store,
		Units:           units,
		System:          sys,
		Now:             nowFn,
		AllowLocalRepos: true,
		Freshen: &statedir.FreshenConfig{
			MinIntervalSeconds: intPtr(60),
			IdleDelaySeconds:   intPtr(15),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	e.Sample()

	repoDir := initTestOriginRepo(t)
	gateFile := filepath.Join(t.TempDir(), "freshen-go")
	proj := &pb.HostedProject{
		Name:  "app",
		Repos: []*pb.Repo{{Id: "app", Url: repoDir}},
		Steps: []*pb.Step{{
			Name:      "build",
			Hash:      "h1",
			EveryPull: true,
			Script:    `if [ -f ` + gateFile + `.hold ]; then while [ ! -f ` + gateFile + ` ]; do sleep 0.02; done; fi; echo built`,
		}},
	}

	// 1. Cold build for ws1.
	ws1, err := e.CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{
		Id: "ws1", Project: proj, Agent: "a", SyncDir: "/local/app",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil := func(desc string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			e.step(t.Context())
			if cond() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", desc)
	}
	waitUntil("ws1 idle on app-g1", func() bool {
		w, _ := e.Workspace(ws1.Id)
		return w != nil && w.State == idle && w.BaseId == "app-g1"
	})

	// 2. Push a new commit to repoDir and arm the gate so the upcoming optional freshen stays running.
	if err := os.WriteFile(filepath.Join(repoDir, "lib.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInDir(t, repoDir, "commit", "--quiet", "-am", "v2")
	if err := os.WriteFile(gateFile+".hold", []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Advance past minInterval (60s), then create a SYNCING workspace (SyncDir != "").
	// It MUST fork app-g1 IMMEDIATELY (not block on app-g2) and defer the optional freshen!
	advance(2 * time.Minute)
	ws2, err := e.CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{
		Id: "ws2", Project: proj, Agent: "a", SyncDir: "/local/app",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ws2.State != idle || ws2.BaseId != "app-g1" {
		t.Fatalf("syncing ws2 = (%v, %s), want immediate IDLE on app-g1", ws2.State, ws2.BaseId)
	}

	// 3. Before IdleDelay (15s) elapses, background optional freshen MUST NOT start.
	advance(10 * time.Second)
	e.step(t.Context())
	e.mu.Lock()
	_, hasG2Early := e.bases["app-g2"]
	e.mu.Unlock()
	if hasG2Early {
		t.Fatal("background optional freshen started before IdleDelay elapsed")
	}

	// 4. Advance past IdleDelay (15s): background optional freshen starts at priority -10, weight 20, hardLimit 1800s!
	advance(10 * time.Second)
	var optCmdID string
	waitUntil("optional background freshen app-g2 running", func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		b := e.bases["app-g2"]
		if b == nil || b.Command == "" {
			return false
		}
		c := e.commands[b.Command]
		if c != nil && c.cmd().State == running {
			optCmdID = c.cmd().Id
			return true
		}
		return false
	})
	optCmd, ok := e.Command(optCmdID)
	if !ok {
		t.Fatalf("optional freshen command %s missing", optCmdID)
	}
	if optCmd.Limits.Priority != optionalFreshenPriority || optCmd.Limits.CpuWeight != optionalFreshenCPUWeight || optCmd.Limits.HardLimitSeconds != 1800 {
		t.Fatalf("optional freshen limits = %+v, want priority=%d weight=%d hardLimit=1800",
			optCmd.Limits, optionalFreshenPriority, optionalFreshenCPUWeight)
	}

	// 5. Release gate -> app-g2 warms!
	if err := os.WriteFile(gateFile, []byte("go"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitUntil("app-g2 warm", func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		b := e.bases["app-g2"]
		return b != nil && b.State == warm
	})
}

func TestOptionalFreshenHardDurationLimit(t *testing.T) {
	var nowMu sync.Mutex
	curNow := time.Unix(1795600000, 0)
	nowFn := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return curNow
	}
	advance := func(d time.Duration) {
		nowMu.Lock()
		curNow = curNow.Add(d)
		nowMu.Unlock()
	}

	store := &fake.Store{Root: t.TempDir()}
	units := &fake.Units{}
	t.Cleanup(units.Close)
	e, err := NewEngine(t.Context(), EngineConfig{
		Dir:             t.TempDir(),
		NodeID:          "n-lim",
		Host:            "n-lim",
		BaseImage:       fake.BaseImage(store),
		Store:           store,
		Units:           units,
		System:          fake.NewSystem(),
		Now:             nowFn,
		AllowLocalRepos: true,
		Freshen: &statedir.FreshenConfig{
			MinIntervalSeconds:         intPtr(0),
			MaxOptionalDurationSeconds: intPtr(60),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	e.Sample()

	repoDir := initTestOriginRepo(t)
	proj := &pb.HostedProject{
		Name:  "app",
		Repos: []*pb.Repo{{Id: "app", Url: repoDir}},
		Steps: []*pb.Step{{Name: "build", Hash: "h1", EveryPull: true, Script: "if [ -f app/hang ]; then sleep 30; fi; echo ok"}},
	}
	// Warm g1.
	ws1, err := e.CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{Id: "ws1", Project: proj, Agent: "a"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor := func(desc string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			e.step(t.Context())
			if cond() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", desc)
	}
	waitFor("ws1 idle", func() bool {
		w, _ := e.Workspace(ws1.Id)
		return w != nil && w.State == idle
	})

	// Commit hang file and trigger optional freshen (non-syncing, MinInterval=0).
	if err := os.WriteFile(filepath.Join(repoDir, "hang"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInDir(t, repoDir, "add", "hang")
	gitInDir(t, repoDir, "commit", "--quiet", "-m", "hang")

	ws2, err := e.CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{Id: "ws2", Project: proj, Agent: "a"})
	if err != nil {
		t.Fatal(err)
	}
	var cmdID string
	waitFor("g2 running", func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		b := e.bases["app-g2"]
		if b == nil || b.Command == "" {
			return false
		}
		if c := e.commands[b.Command]; c != nil && c.cmd().State == running {
			cmdID = c.cmd().Id
			return true
		}
		return false
	})

	// Advance clock past the 60s optional hard limit -> engine stops freshen, falls ws2 back to app-g1!
	advance(65 * time.Second)
	waitFor("g2 stopped by hard limit and ws2 fallen back to app-g1", func() bool {
		w, _ := e.Workspace(ws2.Id)
		return w != nil && w.State == idle && w.BaseId == "app-g1" && e.state(cmdID) == cancelled
	})
}

func TestCommandActiveSecondsExcludesParkedAndPaused(t *testing.T) {
	t0 := int64(1_000_000)
	startedAt := t0 + 5_000 // queued for 5s
	cmd := &pb.Command{
		QueuedAt:  t0,
		StartedAt: &startedAt,
		Timeline: []*pb.CommandEvent{
			{At: t0, What: "queued"},
			{At: startedAt, What: "admitted"},
			{At: startedAt + 10_000, What: "parked"},   // ran 10s
			{At: startedAt + 610_000, What: "thawed"},  // parked 600s
			{At: startedAt + 615_000, What: "paused"},  // ran 5s
			{At: startedAt + 915_000, What: "resumed"}, // paused 300s
		},
	}
	endedAt := startedAt + 920_000 // ran 5s more -> total active = 20s out of 920s elapsed
	if got := commandActiveSeconds(cmd, endedAt); got != 20 {
		t.Fatalf("commandActiveSeconds = %v, want 20", got)
	}
	// Command killed while still parked counts only pre-park runtime (10s).
	cmdKilledParked := &pb.Command{
		QueuedAt:  t0,
		StartedAt: &startedAt,
		Timeline: []*pb.CommandEvent{
			{At: startedAt, What: "admitted"},
			{At: startedAt + 10_000, What: "parked"},
		},
	}
	if got := commandActiveSeconds(cmdKilledParked, startedAt+120_000); got != 10 {
		t.Fatalf("commandActiveSeconds(killed while parked) = %v, want 10", got)
	}
	// Never-started command has 0 active seconds.
	if got := commandActiveSeconds(&pb.Command{QueuedAt: t0}, t0+30_000); got != 0 {
		t.Fatalf("commandActiveSeconds(never started) = %v, want 0", got)
	}
}

func initTestOriginRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitInDir(t, dir, "init", "--quiet", "-b", "main")
	gitInDir(t, dir, "config", "user.email", "test@example.com")
	gitInDir(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "lib.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInDir(t, dir, "add", "lib.txt")
	gitInDir(t, dir, "commit", "--quiet", "-m", "v1")
	return dir
}

func gitInDir(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
