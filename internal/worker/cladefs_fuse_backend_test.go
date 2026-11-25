package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/cladefs"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/storage"
	"github.com/semistrict/hotbed/internal/unit"
)

// strictUnits mimics systemd's RemainAfterExit=yes behavior: once a unit is
// started, it stays loaded until Stop() is called, and starting a unit with an
// already-loaded name fails with systemd's exact collision error.
type strictUnits struct {
	mu      sync.Mutex
	loaded  map[string]bool
	started []string
	stopped []string
	// failUnit, when matching unit name, simulates a non-zero exit code.
	failSubstr string
}

func newStrictUnits() *strictUnits {
	return &strictUnits{loaded: map[string]bool{}}
}

func (u *strictUnits) Start(_ context.Context, s unit.Spec) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.loaded[s.Name] {
		return fmt.Errorf("start unit %s: systemd-run --unit=%s: exit status 1: Failed to start transient service unit: Unit %s.service was already loaded or has a fragment file.", s.Name, s.Name, s.Name)
	}
	u.loaded[s.Name] = true
	u.started = append(u.started, s.Name)
	if s.Log != "" {
		_ = os.MkdirAll(filepath.Dir(s.Log), 0o755)
		_ = os.WriteFile(s.Log, []byte("ok\n"), 0o644)
	}
	return nil
}

func (u *strictUnits) Wait(context.Context, string, string) error { return nil }

func (u *strictUnits) Status(_ context.Context, name string) (unit.State, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	exitCode := 0
	if u.failSubstr != "" && strings.Contains(name, u.failSubstr) {
		exitCode = 1
	}
	return unit.State{Loaded: u.loaded[name], Exited: true, ExitCode: exitCode}, nil
}

func (u *strictUnits) StatusMany(context.Context, []string) (map[string]unit.State, error) {
	return map[string]unit.State{}, nil
}
func (u *strictUnits) Freeze(context.Context, string) error        { return nil }
func (u *strictUnits) Thaw(context.Context, string) error          { return nil }
func (u *strictUnits) Reclaim(string, int64) error                 { return nil }
func (u *strictUnits) Memory(string) (unit.MemoryUse, error)       { return unit.MemoryUse{}, nil }
func (u *strictUnits) List(context.Context, string) ([]string, error) { return nil, nil }
func (u *strictUnits) StartShell(context.Context, unit.Spec, unit.ShellIO) (unit.ShellHandle, error) {
	return nil, errors.New("not implemented")
}
func (u *strictUnits) Exec(context.Context, string, string, unit.ExecSpec) (*exec.Cmd, error) {
	return nil, errors.New("not implemented")
}

func (u *strictUnits) Stop(_ context.Context, name string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.loaded, name)
	u.stopped = append(u.stopped, name)
	return nil
}

func TestCladefsFUSERunInChrootStopsUnitsOnSuccessAndFailure(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "rootfs")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	su := newStrictUnits()
	e := &Engine{
		cfg: EngineConfig{
			Dir:   dir,
			User:  "ramon",
			Units: su,
		},
	}

	runInChroot := func(stepCtx context.Context, rootDir, unitName string, argv, env []string) (string, error) {
		if err := makeTmpDirs(rootDir, unitName, e.cred); err != nil {
			return "", err
		}
		tmpBase := filepath.Join(rootDir, tmpName, unitName)
		defer os.RemoveAll(tmpBase)
		tmpDir := filepath.Join(tmpBase, "tmp")
		runDir := filepath.Join(tmpBase, "run")
		runtimeTarget := e.runtimeChrootDir()
		logPath := filepath.Join(e.cfg.Dir, "logs", unitName+".log")
		_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
		stepEnv := append(append([]string(nil), env...), "XDG_RUNTIME_DIR="+runtimeTarget)
		spec := unit.Spec{
			Name:       unitName,
			Slice:      unit.DefaultSlice,
			Argv:       argv,
			Dir:        WorkPath,
			Binds:      []unit.Bind{{Source: runDir, Target: runtimeTarget}},
			Tmp:        tmpDir,
			User:       e.cfg.User,
			Env:        stepEnv,
			PathPrefix: cachePath,
			Log:        logPath,
		}
		e.inRootfs(&spec, rootDir)
		if err := e.cfg.Units.Start(stepCtx, spec); err != nil {
			return "", err
		}
		defer func() {
			_ = e.cfg.Units.Stop(context.Background(), spec.Name)
		}()
		if err := e.cfg.Units.Wait(stepCtx, spec.Slice, spec.Name); err != nil {
			return "", err
		}
		st, err := e.cfg.Units.Status(stepCtx, spec.Name)
		outBytes, _ := os.ReadFile(logPath)
		out := string(outBytes)
		if err != nil {
			return out, err
		}
		if st.ExitCode != 0 {
			return out, fmt.Errorf("chroot step %s exited %d", unitName, st.ExitCode)
		}
		return out, nil
	}

	// 1. Successful step must be stopped so it does not stay loaded in systemd.
	if _, err := runInChroot(context.Background(), rootDir, "hotbed-fuse-repos-test", []string{"/bin/true"}, nil); err != nil {
		t.Fatalf("first runInChroot failed: %v", err)
	}
	if len(su.loaded) != 0 {
		t.Fatalf("expected all units unloaded after success, still loaded: %v", su.loaded)
	}

	// 2. Failed step must also be stopped so retries never collide.
	su.failSubstr = "step-1"
	if _, err := runInChroot(context.Background(), rootDir, "hotbed-fuse-step-1-test", []string{"/bin/false"}, nil); err == nil {
		t.Fatal("expected failure on step-1")
	}
	if len(su.loaded) != 0 {
		t.Fatalf("expected all units unloaded after failure, still loaded: %v", su.loaded)
	}

	// 3. Re-running with the same unit name succeeds because Stop() unloaded it.
	su.failSubstr = ""
	if _, err := runInChroot(context.Background(), rootDir, "hotbed-fuse-step-1-test", []string{"/bin/true"}, nil); err != nil {
		t.Fatalf("retry runInChroot failed: %v", err)
	}
}

func TestRandomUnitSuffixProducesDistinctNames(t *testing.T) {
	seen := map[string]bool{}
	for range 32 {
		s := randomUnitSuffix()
		if len(s) != 8 {
			t.Fatalf("suffix len = %d (%q), want 8", len(s), s)
		}
		if seen[s] {
			t.Fatalf("duplicate randomUnitSuffix %q", s)
		}
		seen[s] = true
	}
}

func TestCladefsFUSELoadStateIndexesMultipleBaseImagesByKey(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cladefs-fuse-bases")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := baseimage.Spec{User: "ramon", UID: 1000, GID: 1000}.Normalized()
	b := &CladefsFUSEBackend{
		workerDir:      dir,
		decommissioned: map[uint64]bool{},
		baseImages:     map[string]cladefs.CellRef{},
		projectBases:   map[string]*vmProjectBase{},
		sharedBases:    map[string]*pb.CladefsCell{},
		wsMounts:       map[string]fuseWSMount{},
	}
	wantKey := b.fuseBaseKey(spec)
	// Write both the wanted key and a lexicographically later stale key.
	for key, cell := range map[string]uint64{
		wantKey:        101,
		"zzzz-old-key": 999,
	} {
		raw, _ := json.Marshal(cladefs.CellRef{RelPath: "baseimages/fuse-" + key, Tenant: 0, Cell: cell, Version: 2})
		if err := os.WriteFile(filepath.Join(cacheDir, "baseimage-"+key+".json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b.loadState()
	got, ok := b.baseImages[wantKey]
	if !ok || got.Cell != 101 {
		t.Fatalf("baseImages[%q] = %+v (ok=%v), want cell 101", wantKey, got, ok)
	}
}

func TestTenantCellRootAndCacheIsolation(t *testing.T) {
	// Tenant 0 preserves legacy paths verbatim.
	if got := tenantCellRoot("/mnt/cells/hotbed", 0); got != "/mnt/cells/hotbed" {
		t.Fatalf("tenantCellRoot(0) = %q, want legacy path unchanged", got)
	}
	// Non-zero tenants get isolated subdirectories under every configured JBOD root.
	wantJBOD := strings.Join([]string{
		filepath.Join("/mnt/a/hotbed", "tenant-3"),
		filepath.Join("/mnt/b/hotbed", "tenant-3"),
	}, string(filepath.ListSeparator))
	if got := tenantCellRoot("/mnt/a/hotbed:/mnt/b/hotbed", 3); got != wantJBOD {
		t.Fatalf("tenantCellRoot(3) = %q, want %q", got, wantJBOD)
	}

	dir := t.TempDir()
	vm0 := &VMBackend{
		workerDir:    dir,
		baseImages:   map[string]cladefs.CellRef{},
		projectBases: map[string]*vmProjectBase{},
		sharedBases:  map[string]*pb.CladefsCell{},
	}
	vm2 := &VMBackend{
		cfg:          statedir.VMConfig{TenantID: 2},
		workerDir:    dir,
		baseImages:   map[string]cladefs.CellRef{},
		projectBases: map[string]*vmProjectBase{},
		sharedBases:  map[string]*pb.CladefsCell{},
	}
	if vm0.cacheDir() != filepath.Join(dir, "vm-bases") {
		t.Fatalf("vm0.cacheDir() = %q, want legacy vm-bases", vm0.cacheDir())
	}
	if vm2.cacheDir() != filepath.Join(dir, "vm-bases-t2") {
		t.Fatalf("vm2.cacheDir() = %q, want vm-bases-t2", vm2.cacheDir())
	}

	// Mismatched-tenant cache entries on disk are ignored rather than causing cross-tenant EACCES.
	if err := os.MkdirAll(vm2.cacheDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	wrongRef, _ := json.Marshal(cladefs.CellRef{RelPath: "baseimages/vm-k", Tenant: 0, Cell: 10, Version: 1})
	rightRef, _ := json.Marshal(cladefs.CellRef{RelPath: "baseimages/vm-k2", Tenant: 2, Cell: 20, Version: 2})
	if err := os.WriteFile(filepath.Join(vm2.cacheDir(), "baseimage-wrong.json"), wrongRef, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vm2.cacheDir(), "baseimage-right.json"), rightRef, 0o644); err != nil {
		t.Fatal(err)
	}
	vm2.loadCachedBases()
	if _, ok := vm2.baseImages["wrong"]; ok {
		t.Fatalf("vm2 loaded mismatched tenant-0 base image from cache")
	}
	if got, ok := vm2.baseImages["right"]; !ok || got.Cell != 20 || got.Tenant != 2 {
		t.Fatalf("vm2.baseImages[right] = %+v (ok=%v), want tenant 2 cell 20", got, ok)
	}

	// Same isolation for CladefsFUSEBackend.
	fuse0 := &CladefsFUSEBackend{workerDir: dir}
	fuse4 := &CladefsFUSEBackend{
		cfg:            statedir.CladefsFUSEConfig{TenantID: 4},
		workerDir:      dir,
		baseImages:     map[string]cladefs.CellRef{},
		projectBases:   map[string]*vmProjectBase{},
		sharedBases:    map[string]*pb.CladefsCell{},
		wsMounts:       map[string]fuseWSMount{},
		decommissioned: map[uint64]bool{},
	}
	if fuse0.cacheDir() != filepath.Join(dir, "cladefs-fuse-bases") || fuse0.wsStatePath() != filepath.Join(dir, "cladefs-fuse-ws.json") {
		t.Fatalf("fuse0 paths changed unexpectedly: cache=%q ws=%q", fuse0.cacheDir(), fuse0.wsStatePath())
	}
	if fuse4.cacheDir() != filepath.Join(dir, "cladefs-fuse-bases-t4") || fuse4.wsStatePath() != filepath.Join(dir, "cladefs-fuse-ws-t4.json") {
		t.Fatalf("fuse4 paths = cache=%q ws=%q", fuse4.cacheDir(), fuse4.wsStatePath())
	}
	if err := os.MkdirAll(fuse4.cacheDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	fuseWrong, _ := json.Marshal(cladefs.CellRef{RelPath: "baseimages/fuse-w", Tenant: 0, Cell: 30, Version: 1})
	fuseRight, _ := json.Marshal(cladefs.CellRef{RelPath: "baseimages/fuse-r", Tenant: 4, Cell: 40, Version: 2})
	if err := os.WriteFile(filepath.Join(fuse4.cacheDir(), "baseimage-wrong.json"), fuseWrong, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fuse4.cacheDir(), "baseimage-right.json"), fuseRight, 0o644); err != nil {
		t.Fatal(err)
	}
	fuse4.loadState()
	if _, ok := fuse4.baseImages["wrong"]; ok {
		t.Fatalf("fuse4 loaded mismatched tenant-0 base image from cache")
	}
	if got, ok := fuse4.baseImages["right"]; !ok || got.Cell != 40 || got.Tenant != 4 {
		t.Fatalf("fuse4.baseImages[right] = %+v (ok=%v), want tenant 4 cell 40", got, ok)
	}
}

func TestCladefsFUSEHeartbeatWithdrawsUnhealthyRuntimeAndRecoversENOTCONN(t *testing.T) {
	errNotConn := errors.New("transport endpoint is not connected")
	var healthy atomic.Bool
	var detached atomic.Int32
	reconnected := make(chan struct{}, 4)

	b := &CladefsFUSEBackend{
		mountRoot: "/var/lib/cladefs/mnt",
		mountHealthFn: func(string) error {
			if healthy.Load() {
				return nil
			}
			return errNotConn
		},
		isNotConnFn: func(err error) bool { return errors.Is(err, errNotConn) },
		detachDeadFn: func(string) error {
			detached.Add(1)
			return nil
		},
		reconnectFn: func(context.Context) (uint64, error) {
			healthy.Store(true)
			reconnected <- struct{}{}
			return 42, nil
		},
	}

	// While mountRoot returns ENOTCONN, heartbeats must withdraw "cladefs"
	// so the controller stops placing cladefs workspaces here and only advertises "chroot".
	got := b.RuntimesForHeartbeat()
	if !slices.Equal(got, []string{runtime.KindChroot}) {
		t.Fatalf("unhealthy RuntimesForHeartbeat = %v, want [%s]", got, runtime.KindChroot)
	}

	select {
	case <-reconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for background ENOTCONN recovery")
	}
	if detached.Load() != 1 {
		t.Fatalf("detachDead calls = %d, want 1", detached.Load())
	}

	// Once recovered, heartbeats re-advertise both "cladefs" and "chroot".
	got = b.RuntimesForHeartbeat()
	if !slices.Equal(got, []string{runtime.KindCladefs, runtime.KindChroot}) {
		t.Fatalf("recovered RuntimesForHeartbeat = %v, want [%s %s]", got, runtime.KindCladefs, runtime.KindChroot)
	}
}

func TestCladefsFUSEEnsureMountReadyRecoversFromENOTCONN(t *testing.T) {
	errNotConn := errors.New("transport endpoint is not connected")
	var attempts atomic.Int32
	var detached, reconnected atomic.Int32

	b := &CladefsFUSEBackend{
		mountRoot: "/var/lib/cladefs/mnt",
		clearNosuidFn: func(string) error {
			if attempts.Add(1) == 1 {
				return errNotConn
			}
			return nil
		},
		isNotConnFn: func(err error) bool { return errors.Is(err, errNotConn) },
		detachDeadFn: func(string) error {
			detached.Add(1)
			return nil
		},
		reconnectFn: func(context.Context) (uint64, error) {
			reconnected.Add(1)
			return 7, nil
		},
	}

	if err := b.ensureMountReady(context.Background()); err != nil {
		t.Fatalf("ensureMountReady failed after ENOTCONN recovery: %v", err)
	}
	if attempts.Load() != 2 || detached.Load() != 1 || reconnected.Load() != 1 {
		t.Fatalf("attempts=%d detached=%d reconnected=%d, want 2/1/1", attempts.Load(), detached.Load(), reconnected.Load())
	}
}

func TestMakeVersionAndSyncAfterStopRetriesTransientBusy(t *testing.T) {
	var attempts atomic.Int32
	ver, err := makeVersionAndSyncAfterStop(context.Background(), func(context.Context) (uint64, error) {
		if attempts.Add(1) < 3 {
			return 0, fmt.Errorf("cladefs make version: %w", storage.ErrBusy)
		}
		return 99, nil
	}, 2*time.Second, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("makeVersionAndSyncAfterStop failed on transient ErrBusy: %v", err)
	}
	if ver != 99 || attempts.Load() != 3 {
		t.Fatalf("ver=%d attempts=%d, want 99/3", ver, attempts.Load())
	}

	// Non-busy errors must fail immediately without retrying.
	attempts.Store(0)
	boom := errors.New("boom")
	if _, err := makeVersionAndSyncAfterStop(context.Background(), func(context.Context) (uint64, error) {
		attempts.Add(1)
		return 0, boom
	}, 2*time.Second, 5*time.Millisecond); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("non-busy attempts = %d, want 1", attempts.Load())
	}
}
