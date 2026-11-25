package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func sha256HexBytes(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func newTestUpgradeServer(t *testing.T, files map[string][]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestUpgradeSystemStagingFailureLeavesWorkUntouched(t *testing.T) {
	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	srv := newTestUpgradeServer(t, map[string][]byte{
		"/cladefsd": []byte("wrong-bytes"),
	})

	var installs, restarts atomic.Int32
	eng, err := NewEngine(t.Context(), EngineConfig{
		Dir: dir, NodeID: "n1", Host: "ubu1",
		Store: store, BaseImage: fake.BaseImage(store), Units: &fake.Units{}, System: fake.NewSystem(),
		Artifacts: &ArtifactStore{WorkerDir: dir, DownloadClient: srv.Client()},
		SystemUpgradeOps: &SystemUpgradeOps{
			InstallFile: func(src, dst, wantSHA string, mode os.FileMode) error {
				installs.Add(1)
				return nil
			},
			RestartService: func(ctx context.Context, service string) error {
				restarts.Add(1)
				return nil
			},
			Reconnect: func(ctx context.Context) (uint64, error) { return 1, nil },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })

	// Seed a live command and workspace to prove Phase 1 failure touches neither.
	eng.mu.Lock()
	eng.commands["cmd-live"] = &command{rec: &pb.CommandRecord{Command: &pb.Command{Id: "cmd-live", State: running, Limits: &pb.CommandLimits{}}}}
	eng.workspaces["ws-live"] = &workspace{rec: &pb.WorkspaceRecord{Workspace: &pb.Workspace{Id: "ws-live", State: idle, Runtime: "vm"}, Volume: "ws/ws-live"}}
	eng.mu.Unlock()

	_, err = eng.UpgradeSystem(t.Context(), &pb.WorkerUpgradeSystemRequest{
		Cladefsd: &pb.ManagedArtifact{
			Url:     srv.URL + "/cladefsd",
			Sha256:  strings.Repeat("a", 64),
			Version: "v-bad",
		},
		CladefsdBin: filepath.Join(dir, "bin", "cladefsd"),
	})
	if err == nil {
		t.Fatal("UpgradeSystem succeeded with bad sha256, want error")
	}
	if installs.Load() != 0 || restarts.Load() != 0 {
		t.Fatalf("Phase 1 failure ran side effects: installs=%d restarts=%d", installs.Load(), restarts.Load())
	}
	if eng.isSystemUpgrading() {
		t.Fatal("engine left in systemUpgrading state after Phase 1 failure")
	}
	eng.mu.Lock()
	cmdState := eng.commands["cmd-live"].rec.Command.State
	wsState := eng.workspaces["ws-live"].rec.Workspace.State
	eng.mu.Unlock()
	if cmdState != running || wsState != idle {
		t.Fatalf("live work disrupted on Phase 1 failure: cmd=%v ws=%v", cmdState, wsState)
	}
}

func TestUpgradeSystemUpToDateAndForce(t *testing.T) {
	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	payload := []byte("cladefsd-v1")
	sum := sha256HexBytes(payload)
	binPath := filepath.Join(dir, "cladefsd")
	if err := os.WriteFile(binPath, payload, 0o755); err != nil {
		t.Fatal(err)
	}
	srv := newTestUpgradeServer(t, map[string][]byte{"/cladefsd": payload})

	var restarts atomic.Int32
	var reconnects atomic.Int32
	eng, err := NewEngine(t.Context(), EngineConfig{
		Dir: dir, NodeID: "n1", Host: "ubu1",
		Store: store, BaseImage: fake.BaseImage(store), Units: &fake.Units{}, System: fake.NewSystem(),
		Artifacts: &ArtifactStore{WorkerDir: dir, DownloadClient: srv.Client()},
		SystemUpgradeOps: &SystemUpgradeOps{
			RestartService: func(ctx context.Context, service string) error {
				restarts.Add(1)
				return nil
			},
			Reconnect: func(ctx context.Context) (uint64, error) {
				reconnects.Add(1)
				return 7, nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })

	// Without --force: already matches SHA-256 -> up-to-date, zero restarts.
	res, err := eng.UpgradeSystem(t.Context(), &pb.WorkerUpgradeSystemRequest{
		Cladefsd:    &pb.ManagedArtifact{Url: srv.URL + "/cladefsd", Sha256: sum, Version: "v1"},
		CladefsdBin: binPath,
	})
	if err != nil {
		t.Fatalf("UpgradeSystem up-to-date: %v", err)
	}
	if res.Status != "up-to-date" || res.CladefsdUpdated || res.RestartedCladefsd {
		t.Fatalf("up-to-date result = %+v", res)
	}
	if restarts.Load() != 0 || reconnects.Load() != 0 {
		t.Fatalf("up-to-date triggered restart/reconnect: %d/%d", restarts.Load(), reconnects.Load())
	}

	// With --force: restarts and reconnects even though binary SHA matches.
	res, err = eng.UpgradeSystem(t.Context(), &pb.WorkerUpgradeSystemRequest{
		Cladefsd:    &pb.ManagedArtifact{Url: srv.URL + "/cladefsd", Sha256: sum, Version: "v1"},
		CladefsdBin: binPath,
		Force:       true,
	})
	if err != nil {
		t.Fatalf("UpgradeSystem force: %v", err)
	}
	if res.Status != "upgraded" || res.CladefsdUpdated || !res.RestartedCladefsd || res.CladefsIncarnation != 7 {
		t.Fatalf("force result = %+v", res)
	}
	if restarts.Load() != 1 || reconnects.Load() != 1 {
		t.Fatalf("force restarts/reconnects = %d/%d, want 1/1", restarts.Load(), reconnects.Load())
	}
}

func TestUpgradeSystemDryRunAndFullLifecycle(t *testing.T) {
	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	units := &fake.Units{}
	t.Cleanup(units.Close)

	binDir := filepath.Join(dir, "usr-local-bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cladefsdBin := filepath.Join(binDir, "cladefsd")
	fcBin := filepath.Join(binDir, "firecracker")
	if err := os.WriteFile(cladefsdBin, []byte("old-cladefsd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fcBin, []byte("old-firecracker"), 0o755); err != nil {
		t.Fatal(err)
	}

	newCladefsd := []byte("new-cladefsd-v2")
	newFC := []byte("new-firecracker-v2")
	srv := newTestUpgradeServer(t, map[string][]byte{
		"/cladefsd":    newCladefsd,
		"/firecracker": newFC,
	})

	restartStarted := make(chan string, 1)
	releaseRestart := make(chan struct{})
	eng, err := NewEngine(t.Context(), EngineConfig{
		Dir: dir, NodeID: "n1", Host: "ubu1",
		Store: store, BaseImage: fake.BaseImage(store), Units: units, System: fake.NewSystem(),
		Artifacts: &ArtifactStore{WorkerDir: dir, DownloadClient: srv.Client()},
		SystemUpgradeOps: &SystemUpgradeOps{
			RestartService: func(ctx context.Context, service string) error {
				restartStarted <- service
				select {
				case <-releaseRestart:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			},
			Reconnect: func(ctx context.Context) (uint64, error) { return 42, nil },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })

	// Register an active shell and a running command + cladefs workspace.
	shellDone := make(chan struct{})
	var shellOnce sync.Once
	var shellCancelled atomic.Bool
	eng.mu.Lock()
	eng.shells["sh-1"] = &activeShell{
		id: "sh-1",
		cancel: func() {
			shellCancelled.Store(true)
			shellOnce.Do(func() { close(shellDone) })
		},
		done: shellDone,
	}
	eng.workspaces["ws-1"] = &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{Id: "ws-1", Project: "app", State: idle, Runtime: "cladefs"},
		Volume:    "ws/ws-1",
	}}
	eng.commands["cmd-1"] = &command{
		rec: &pb.CommandRecord{
			Command: &pb.Command{Id: "cmd-1", WorkspaceId: "ws-1", State: running, Limits: &pb.CommandLimits{}},
		},
		out: newOutput(filepath.Join(dir, "cmd-1.log")),
	}
	eng.mu.Unlock()

	req := &pb.WorkerUpgradeSystemRequest{
		Cladefsd:        &pb.ManagedArtifact{Url: srv.URL + "/cladefsd", Sha256: sha256HexBytes(newCladefsd), Version: "v2"},
		Firecracker:     &pb.ManagedArtifact{Url: srv.URL + "/firecracker", Sha256: sha256HexBytes(newFC), Version: "fc-v2"},
		CladefsdBin:     cladefsdBin,
		CladefsdService: "cladefsd-custom.service",
		FirecrackerBin:  fcBin,
	}

	// 1. Dry-run reports all pending work without changing binaries or cancelling anything.
	dryReq := &pb.WorkerUpgradeSystemRequest{
		Cladefsd:        req.Cladefsd,
		Firecracker:     req.Firecracker,
		CladefsdBin:     req.CladefsdBin,
		CladefsdService: req.CladefsdService,
		FirecrackerBin:  req.FirecrackerBin,
		DryRun:          true,
	}
	dryRes, err := eng.UpgradeSystem(t.Context(), dryReq)
	if err != nil {
		t.Fatalf("UpgradeSystem dry-run: %v", err)
	}
	if dryRes.Status != "dry-run" ||
		!dryRes.CladefsdUpdated || !dryRes.FirecrackerUpdated || !dryRes.RestartedCladefsd ||
		dryRes.StoppedCommands != 1 || dryRes.ClosedShells != 1 || dryRes.ReleasedWorkspaces != 1 {
		t.Fatalf("dry-run result = %+v", dryRes)
	}
	if shellCancelled.Load() {
		t.Fatal("dry-run cancelled active shell")
	}
	if got, _ := os.ReadFile(cladefsdBin); string(got) != "old-cladefsd" {
		t.Fatalf("dry-run overwrote cladefsd: %q", got)
	}

	// 2. Real upgrade drains admission, tears down active work, installs binaries, restarts & reconnects.
	type upgradeOut struct {
		res *pb.NodeSystemUpgrade
		err error
	}
	done := make(chan upgradeOut, 1)
	go func() {
		res, err := eng.UpgradeSystem(t.Context(), req)
		done <- upgradeOut{res: res, err: err}
	}()

	var svc string
	select {
	case svc = <-restartStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for RestartService during UpgradeSystem")
	}
	if svc != "cladefsd-custom.service" {
		t.Fatalf("restarted service = %q, want cladefsd-custom.service", svc)
	}

	// While upgrade is in progress, admission must hold new work with retryable Conflict.
	if adm := eng.admission(); !adm.GetHolding() || !strings.Contains(adm.GetReason(), "upgrading cladefs") {
		t.Fatalf("admission while upgrading = %+v, want upgrading cladefs hold", adm)
	}
	if _, err := eng.CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{Id: "ws-during-upgrade", Project: &pb.HostedProject{Name: "app"}}); err == nil {
		t.Fatal("CreateWorkspace succeeded during system upgrade, want Conflict")
	} else {
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.CodeConflict || !ae.Retryable {
			t.Fatalf("CreateWorkspace err = %v, want retryable Conflict", err)
		}
	}

	close(releaseRestart)
	out := <-done
	if out.err != nil {
		t.Fatalf("UpgradeSystem failed: %v", out.err)
	}
	res := out.res
	if res.Status != "upgraded" ||
		!res.CladefsdUpdated || !res.FirecrackerUpdated || !res.RestartedCladefsd ||
		res.CladefsIncarnation != 42 ||
		res.ClosedShells != 1 || res.ReleasedWorkspaces != 1 {
		t.Fatalf("upgrade result = %+v", res)
	}
	if !shellCancelled.Load() {
		t.Fatal("active shell was not cancelled during upgrade")
	}
	if got, err := os.ReadFile(cladefsdBin); err != nil || string(got) != string(newCladefsd) {
		t.Fatalf("installed cladefsd = (%q, %v), want %q", got, err, newCladefsd)
	}
	if got, err := os.ReadFile(fcBin); err != nil || string(got) != string(newFC) {
		t.Fatalf("installed firecracker = (%q, %v), want %q", got, err, newFC)
	}
	if eng.isSystemUpgrading() {
		t.Fatal("systemUpgrading flag was not cleared after UpgradeSystem")
	}
	if adm := eng.admission(); adm.GetHolding() && strings.Contains(adm.GetReason(), "upgrading cladefs") {
		t.Fatalf("admission still holding for system upgrade after completion: %+v", adm)
	}
}
