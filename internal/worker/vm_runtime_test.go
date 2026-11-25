package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/guestd"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/unit"
	cladefsdv1 "github.com/semistrict/cladefs/go/cladefsd/v1"
)

type testGuestResolver struct {
	mu      sync.Mutex
	clients map[string]*guestd.Client
}

func (r *testGuestResolver) EnsureVM(_ context.Context, wsID string) (*guestd.Client, error) {
	return r.ClientFor(wsID)
}

func (r *testGuestResolver) ClientFor(wsID string) (*guestd.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.clients[wsID]
	if !ok {
		return nil, fmt.Errorf("no guestd client for %s", wsID)
	}
	return c, nil
}

func (r *testGuestResolver) StopVM(_ context.Context, _ string) error {
	return nil
}

type trackingRuntime struct {
	*runtime.GuestRuntime
	mu    sync.Mutex
	ready map[string]int
	torn  map[string]int
}

func (tr *trackingRuntime) EnsureReady(ctx context.Context, wsID string) error {
	tr.mu.Lock()
	tr.ready[wsID]++
	tr.mu.Unlock()
	return tr.GuestRuntime.EnsureReady(ctx, wsID)
}

func (tr *trackingRuntime) Teardown(ctx context.Context, wsID string) error {
	tr.mu.Lock()
	tr.torn[wsID]++
	tr.mu.Unlock()
	// Don't power off host in tests; just verify Teardown was invoked.
	return nil
}

func runGitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func TestEngineWithGuestRuntimeEndToEnd(t *testing.T) {
	ctx := t.Context()

	// Create a real source repo and a guestd WorkDir with a clone of it.
	originRepo := t.TempDir()
	runGitTest(t, originRepo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(originRepo, "hello.txt"), []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, originRepo, "add", ".")
	runGitTest(t, originRepo, "commit", "-m", "initial")
	baseCommit := runGitTest(t, originRepo, "rev-parse", "HEAD")

	// Start a real guestd.Server on Unix sockets (simulating one Firecracker VM's vsock).
	guestBase, err := os.MkdirTemp("/tmp", "hb-guest-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(guestBase) })
	_ = os.Chmod(guestBase, 0o755)
	guestWork := filepath.Join(guestBase, "work")
	guestState := filepath.Join(guestBase, "state")
	if err := os.MkdirAll(guestWork, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(guestState, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, guestWork, "clone", originRepo, "app")
	if os.Geteuid() == 0 {
		sp := fake.BaseImage(nil).Spec.Normalized()
		_ = filepath.Walk(guestWork, func(p string, _ os.FileInfo, err error) error {
			if err == nil {
				_ = os.Chown(p, sp.UID, sp.GID)
			}
			return nil
		})
	}

	sockDir, err := os.MkdirTemp("/tmp", "hb-rt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	ctrlSock := filepath.Join(sockDir, "ctrl.sock")
	shellSock := filepath.Join(sockDir, "shell.sock")
	tcpSock := filepath.Join(sockDir, "tcp.sock")

	ctrlLn, err := net.Listen("unix", ctrlSock)
	if err != nil {
		t.Fatal(err)
	}
	defer ctrlLn.Close()
	shellLn, err := net.Listen("unix", shellSock)
	if err != nil {
		t.Fatal(err)
	}
	defer shellLn.Close()
	tcpLn, err := net.Listen("unix", tcpSock)
	if err != nil {
		t.Fatal(err)
	}
	defer tcpLn.Close()

	srv, err := guestd.NewServer(guestd.ServerConfig{WorkDir: guestWork, StateDir: guestState})
	if err != nil {
		t.Fatalf("guestd.NewServer: %v", err)
	}
	go func() { _ = srv.ServeControl(ctx, ctrlLn) }()
	go func() { _ = srv.ServeShell(ctx, shellLn) }()
	go func() { _ = srv.ServeTCPProxy(ctx, tcpLn) }()

	guestClient := guestd.NewClient(guestd.UnixMapDialer(map[uint32]string{
		guestd.ControlPort:  ctrlSock,
		guestd.ShellPort:    shellSock,
		guestd.TCPProxyPort: tcpSock,
	}))

	wsID := "ws-vm-e2e"
	resolver := &testGuestResolver{clients: map[string]*guestd.Client{wsID: guestClient}}
	rt := &trackingRuntime{
		GuestRuntime: runtime.NewGuestRuntime(resolver),
		ready:        map[string]int{},
		torn:         map[string]int{},
	}

	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	units := &fake.Units{}
	eng, err := NewEngine(ctx, EngineConfig{
		Dir:             dir,
		NodeID:          "n1",
		Host:            "worker1",
		Store:           store,
		BaseImage:       fake.BaseImage(store),
		Units:           units,
		Runtime:         rt,
		System:          fake.NewSystem(),
		AllowLocalRepos: true,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	// Seed a warm base + snapshot so CreateWorkspace forks immediately.
	baseSnap := "demo-g1"
	if err := store.Create(ctx, "bases/demo/g1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Snapshot(ctx, "bases/demo/g1", baseSnap); err != nil {
		t.Fatal(err)
	}
	eng.mu.Lock()
	eng.bases[baseSnap] = &pb.Base{
		Id:         baseSnap,
		Project:    "demo",
		Generation: 1,
		State:      warm,
		Repos:      []*pb.BaseRepo{{Repo: "app", Commit: baseCommit}},
	}
	eng.setBaseImage(baseSnap, eng.baseImageKey())
	eng.mu.Unlock()

	ws, err := eng.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{
		Id:    wsID,
		Agent: "test-agent",
		Project: &pb.HostedProject{
			Name:  "demo",
			Repos: []*pb.Repo{{Id: "app", Url: originRepo, Branch: "main"}},
		},
	})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if ws.State != idle {
		t.Fatalf("workspace state = %v, want idle", ws.State)
	}
	if rt.ready[wsID] != 1 {
		t.Fatalf("EnsureReady count = %d, want 1", rt.ready[wsID])
	}

	// 1. Sync a real modified file via gitops.Snapshot -> git bundle -> Engine.Sync -> guestd /v1/sync!
	clientRepo := t.TempDir()
	runGitTest(t, clientRepo, "clone", originRepo, ".")
	if err := os.WriteFile(filepath.Join(clientRepo, "hello.txt"), []byte("line1\nline2-edited\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap, err := gitops.Snapshot(ctx, clientRepo)
	if err != nil {
		t.Fatalf("gitops.Snapshot: %v", err)
	}
	var bundleBuf bytes.Buffer
	if _, err := gitops.Bundle(ctx, snap, baseCommit, &bundleBuf); err != nil {
		t.Fatalf("gitops.Bundle: %v", err)
	}
	syncResp, err := eng.Sync(ctx, &pb.SyncHeader{
		WorkspaceId: wsID,
		Repo:        "app",
		Commit:      snap.Commit,
		Tree:        snap.Tree,
		Dir:         clientRepo,
	}, bytes.NewReader(bundleBuf.Bytes()))
	if err != nil {
		t.Fatalf("Engine.Sync via guestd: %v", err)
	}
	if syncResp.FilesChanged != 1 {
		t.Fatalf("FilesChanged = %d, want 1", syncResp.FilesChanged)
	}

	// 2. Files() and File() read directly from guestd over vsock.
	entries, err := eng.Files(ctx, wsID)
	if err != nil {
		t.Fatalf("Engine.Files: %v", err)
	}
	foundHello := false
	for _, ent := range entries {
		if ent.Path == "app/hello.txt" && ent.Change == "modified" {
			foundHello = true
		}
	}
	if !foundHello {
		t.Fatalf("expected modified app/hello.txt in %+v", entries)
	}
	fc, err := eng.File(ctx, wsID, "app/hello.txt")
	if err != nil {
		t.Fatalf("Engine.File: %v", err)
	}
	if !strings.Contains(fc.Text, "line2-edited") || len(fc.ChangedLines) == 0 {
		t.Fatalf("unexpected FileContent: %+v", fc)
	}

	// 3. Run a command via Engine.Exec -> admit -> guestd -> log pump -> host log file -> exit 0!
	cmd, err := eng.Exec(ctx, &pb.RunCommandRequest{
		WorkspaceId: wsID,
		Repo:        "app",
		Args:        []string{"/bin/sh", "-c", "cat hello.txt && echo VM_CMD_DONE"},
	})
	if err != nil {
		t.Fatalf("Engine.Exec: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		eng.step(ctx)
		cur, ok := eng.Command(cmd.Id)
		if ok && cur.State == succeeded {
			break
		}
		if ok && !api.Live(cur.State) && cur.State != succeeded {
			t.Fatalf("command ended in unexpected state %v (%s)", cur.State, cur.Reason)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for command %s to succeed", cmd.Id)
		}
		time.Sleep(50 * time.Millisecond)
	}
	logPath, _ := eng.logPath(cmd.Id)
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read host log %s: %v", logPath, err)
	}
	if !strings.Contains(string(logBytes), "line2-edited") || !strings.Contains(string(logBytes), "VM_CMD_DONE") {
		t.Fatalf("host log missing guest output: %q", string(logBytes))
	}

	// 4. Interactive piped shell routed through guestd!
	var shOut bytes.Buffer
	shHandle, err := eng.cfg.Units.StartShell(ctx, unit.Spec{
		Name: "hotbed-sh-test",
		Argv: []string{"/bin/sh", "-c", "echo VM_SHELL_OK"},
		Dir:  "/work/app",
		Env:  []string{"HOTBED_WORKSPACE=" + wsID},
	}, unit.ShellIO{
		Stdin:  strings.NewReader(""),
		Stdout: &shOut,
		Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("StartShell via guestd: %v", err)
	}
	if rc := shHandle.Wait(); rc != 0 {
		t.Fatalf("shell exit code = %d, want 0", rc)
	}
	if strings.TrimSpace(shOut.String()) != "VM_SHELL_OK" {
		t.Fatalf("shell output = %q", shOut.String())
	}

	// 5. TCP dev-server portProxy -> guestd TCPProxyPort -> loopback server!
	guestDevLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer guestDevLn.Close()
	go func() {
		c, err := guestDevLn.Accept()
		if err != nil {
			return
		}
		_, _ = c.Write([]byte("DEV_SERVER_VIA_VSOCK"))
		_ = c.Close()
	}()
	guestPort := uint16(guestDevLn.Addr().(*net.TCPAddr).Port)
	proxy, err := startPortProxy("127.0.0.1:0", wsID, guestPort, rt)
	if err != nil {
		t.Fatalf("startPortProxy: %v", err)
	}
	defer proxy.Close()
	pConn, err := net.Dial("tcp", proxy.Addr())
	if err != nil {
		t.Fatalf("dial port proxy: %v", err)
	}
	pBody, _ := io.ReadAll(pConn)
	_ = pConn.Close()
	if string(pBody) != "DEV_SERVER_VIA_VSOCK" {
		t.Fatalf("port proxy body = %q", string(pBody))
	}

	// 6. ReleaseWorkspace calls Runtime.Teardown cleanly.
	if err := eng.ReleaseWorkspace(ctx, wsID); err != nil {
		t.Fatalf("ReleaseWorkspace: %v", err)
	}
	if rt.torn[wsID] != 1 {
		t.Fatalf("Teardown count = %d, want 1", rt.torn[wsID])
	}
}

func TestIsVMWorkspaceNoDeadlockUnderEngineLock(t *testing.T) {
	eng := &Engine{}
	eng.cfg.Runtime = &trackingRuntime{}
	eng.cfg.RuntimeFor = eng.isVMWorkspace
	eng.vmWorkspaces.Store("ws-vm", struct{}{})

	// Hold e.mu just like Engine.endBy / Exec / step do, and verify usesRuntime
	// does not re-enter e.mu.
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if !eng.usesRuntime("ws-vm") {
		t.Fatal("expected ws-vm to use runtime while e.mu is held")
	}
	if eng.usesRuntime("ws-chroot") {
		t.Fatal("expected ws-chroot not to use runtime while e.mu is held")
	}
}

func TestObjectStoreStatsFromCounters(t *testing.T) {
	got := objectStoreStatsFromCounters(&cladefsdv1.GetCountersResponse{
		CommitWaitNs: 1_500_000_000,
		PageInBytes:  900,
		FetchedBytes: 100,
		ObjectStorage: &cladefsdv1.ObjectStorageStats{
			DownloadedBytes: 4096,
			UploadedBytes: map[string]uint64{
				"segments":    1000,
				"extents":     200,
				"checkpoints": 50,
				"other":       10,
			},
			Requests: map[string]uint64{
				"get":       3,
				"get-range": 7,
				"create":    5,
				"update":    2,
				"overwrite": 1,
				"head":      4,
				"delete":    2,
			},
			Failures: map[string]uint64{
				"create": 1,
				"get":    2,
			},
		},
	})
	if got.UploadedBytes != 1260 || got.UploadedSegmentsBytes != 1000 || got.UploadedExtentsBytes != 200 || got.UploadedCheckpointsBytes != 50 || got.UploadedOtherBytes != 10 {
		t.Fatalf("unexpected upload breakdown: %+v", got)
	}
	if got.DownloadedBytes != 4096 || got.GetRequests != 10 || got.PutRequests != 8 || got.OtherRequests != 6 || got.FailedRequests != 3 {
		t.Fatalf("unexpected requests/downloads: %+v", got)
	}
	if got.CommitWaitNs != 1_500_000_000 || got.PageInBytes != 900 || got.FetchedBytes != 100 {
		t.Fatalf("unexpected daemon counters: %+v", got)
	}

	applyCladefsStats(got, &cladefsdv1.GetStatsResponse{
		Gauges: map[string]uint64{
			"memory_bytes":                4_000_000_000,
			"memory_capacity_bytes":       6_000_000_000,
			"extent_cache_pinned_entries": 2451,
			"extent_cache_idle_entries":   10943,
			"window_mapped_bytes":         3_000_000_000,
		},
		Counters: map[string]uint64{
			"cache_trims": 7,
		},
		Vms: []*cladefsdv1.VmStats{
			{Guest: &cladefsdv1.GuestStats{Counters: map[string]uint64{
				"window_full":             2,
				"window_reclaims":         5,
				"window_regions_full":     1,
				"writes_held_by_seals_ns": 1_000_000_000,
				"device_read_bytes":       100_000,
				"device_reads_ns":         200_000,
			}}},
			{Guest: &cladefsdv1.GuestStats{Counters: map[string]uint64{
				"window_full":             3,
				"window_reclaims":         4,
				"writes_held_by_seals_ns": 500_000_000,
				"device_read_bytes":       50_000,
				"device_reads_ns":         100_000,
			}}},
		},
	})
	if got.ExtentCacheBytes != 4_000_000_000 || got.ExtentCacheCapacityBytes != 6_000_000_000 || got.ExtentCachePinnedEntries != 2451 || got.ExtentCacheIdleEntries != 10943 || got.CacheTrims != 7 {
		t.Fatalf("unexpected extent cache stats: %+v", got)
	}
	if got.ActiveVms != 2 || got.WindowMappedBytes != 3_000_000_000 || got.DaxWindowFull != 5 || got.DaxWindowReclaims != 9 || got.DaxRegionsFull != 1 || got.DaxWritesHeldBySealsNs != 1_500_000_000 || got.DaxDeviceReadBytes != 150_000 || got.DaxDeviceReadsNs != 300_000 {
		t.Fatalf("unexpected DAX stats: %+v", got)
	}

	used, capBytes, ok := parsePartCacheMetrics("# HELP cladefsd_part_cache_bytes x\ncladefsd_part_cache_bytes 37995837625\ncladefsd_part_cache_capacity_bytes 593477874669\n")
	if !ok || used != 37995837625 || capBytes != 593477874669 {
		t.Fatalf("parsePartCacheMetrics = (%d, %d, %v)", used, capBytes, ok)
	}
}

func TestToUnitStatePreservesFrozenAndOOM(t *testing.T) {
	frozen := toUnitState("ws-1", runtime.CommandStatus{
		ID:     "c-1",
		Frozen: true,
	})
	if !frozen.Running || !frozen.Frozen || frozen.Exited {
		t.Fatalf("frozen unit state = %+v, want Running=true Frozen=true Exited=false", frozen)
	}
	if frozen.Cgroup != "vm:ws-1:c-1" {
		t.Fatalf("cgroup = %q, want vm:ws-1:c-1", frozen.Cgroup)
	}

	oom := toUnitState("ws-1", runtime.CommandStatus{
		ID:        "c-2",
		Exited:    true,
		Signal:    9,
		Result:    "oom-kill",
		OOMKilled: true,
	})
	if !oom.Exited || !oom.OOMKilled || oom.Signal != 9 || oom.Result != "oom-kill" {
		t.Fatalf("oom unit state = %+v, want Exited=true OOMKilled=true Signal=9 Result=oom-kill", oom)
	}
}

func TestProjectBaseRecipesAndIncrementalStepSelection(t *testing.T) {
	proj := &pb.HostedProject{
		Name: "cladefs",
		Repos: []*pb.Repo{
			{Id: "cladefs", Url: "https://example.com/cladefs.git", Branch: "main"},
		},
		Steps: []*pb.Step{
			{Name: "deps", Hash: "h-deps", EveryPull: false},
			{Name: "build", Hash: "h-build", EveryPull: true},
		},
	}

	r1VM := vmProjectRecipe("base-a", proj, map[string]string{"cladefs": "aaa111"})
	r2VM := vmProjectRecipe("base-a", proj, map[string]string{"cladefs": "bbb222"})
	if r1VM == r2VM {
		t.Fatalf("vmProjectRecipe did not change when repo HEAD moved: %q", r1VM)
	}
	r1FUSE := fuseProjectRecipe("base-a", proj, map[string]string{"cladefs": "aaa111"})
	r2FUSE := fuseProjectRecipe("base-a", proj, map[string]string{"cladefs": "bbb222"})
	if r1FUSE == r2FUSE {
		t.Fatalf("fuseProjectRecipe did not change when repo HEAD moved: %q", r1FUSE)
	}

	// Cold build runs all steps.
	cold := selectProjectBaseSteps(proj, nil, map[string]string{"cladefs": "aaa111"})
	if len(cold) != 2 {
		t.Fatalf("cold build selected %d steps, want 2", len(cold))
	}

	// Incremental freshen when only repo HEAD moved runs only --every-pull steps.
	prev := &vmProjectBase{
		Repos:      map[string]string{"cladefs": "aaa111"},
		StepHashes: map[string]string{"deps": "h-deps", "build": "h-build"},
	}
	pullOnly := selectProjectBaseSteps(proj, prev, map[string]string{"cladefs": "bbb222"})
	if len(pullOnly) != 1 || pullOnly[0].GetName() != "build" {
		t.Fatalf("repo-moved freshen selected %v, want [build]", pullOnly)
	}

	// Incremental freshen when a non-every-pull step hash changed runs that step too.
	proj2 := &pb.HostedProject{
		Name:  proj.Name,
		Repos: proj.Repos,
		Steps: []*pb.Step{
			{Name: "deps", Hash: "h-deps-v2", EveryPull: false},
			{Name: "build", Hash: "h-build", EveryPull: true},
		},
	}
	hashChanged := selectProjectBaseSteps(proj2, prev, map[string]string{"cladefs": "aaa111"})
	if len(hashChanged) != 1 || hashChanged[0].GetName() != "deps" {
		t.Fatalf("step-hash-changed freshen selected %v, want [deps]", hashChanged)
	}
}

type reclaimSpyRuntime struct {
	runtime.WorkspaceRuntime
	mu    sync.Mutex
	calls []struct {
		wsID  string
		cmdID string
		bytes int64
	}
}

func (r *reclaimSpyRuntime) Reclaim(_ context.Context, wsID, cmdID string, bytes int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, struct {
		wsID  string
		cmdID string
		bytes int64
	}{wsID: wsID, cmdID: cmdID, bytes: bytes})
	return nil
}

func TestRuntimeUnitsReclaimRoutesVMCgroupToGuestRuntime(t *testing.T) {
	inner := &fake.Units{}
	spy := &reclaimSpyRuntime{}
	ru := newRuntimeUnits(inner, spy, func(string) bool { return true }, 0, 0)

	if err := ru.Reclaim("vm:ws-42:cmd-9", 128<<20); err != nil {
		t.Fatalf("Reclaim(vm cgroup): %v", err)
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.calls) != 1 || spy.calls[0].wsID != "ws-42" || spy.calls[0].cmdID != "cmd-9" || spy.calls[0].bytes != 128<<20 {
		t.Fatalf("reclaim calls = %+v, want [{ws-42 cmd-9 134217728}]", spy.calls)
	}
}

func TestVMCommandInspectTraceSampleDiagnoseAndProfile(t *testing.T) {
	ctx := t.Context()

	originRepo := t.TempDir()
	runGitTest(t, originRepo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(originRepo, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, originRepo, "add", ".")
	runGitTest(t, originRepo, "commit", "-m", "init")
	baseCommit := runGitTest(t, originRepo, "rev-parse", "HEAD")

	guestWork := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(guestWork, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, guestWork, "clone", originRepo, "app")

	sockDir, err := os.MkdirTemp("/tmp", "hb-vmdiag-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	ctrlSock := filepath.Join(sockDir, "ctrl.sock")
	ctrlLn, err := net.Listen("unix", ctrlSock)
	if err != nil {
		t.Fatal(err)
	}
	defer ctrlLn.Close()

	srv, err := guestd.NewServer(guestd.ServerConfig{
		WorkDir:   guestWork,
		StateDir:  t.TempDir(),
		Inspector: fake.Inspector{},
	})
	if err != nil {
		t.Fatalf("guestd.NewServer: %v", err)
	}
	go func() { _ = srv.ServeControl(ctx, ctrlLn) }()

	guestClient := guestd.NewClient(guestd.UnixMapDialer(map[uint32]string{
		guestd.ControlPort: ctrlSock,
	}))
	wsID := "ws-vm-diag"
	resolver := &testGuestResolver{clients: map[string]*guestd.Client{wsID: guestClient}}
	rt := runtime.NewGuestRuntime(resolver)

	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	// Deliberately leave host Inspector nil to prove VM diagnostics route through
	// guestd via WorkspaceRuntime rather than host inspect.Linux.
	eng, err := NewEngine(ctx, EngineConfig{
		Dir:             dir,
		NodeID:          "n1",
		Host:            "worker1",
		Store:           store,
		BaseImage:       fake.BaseImage(store),
		Units:           &fake.Units{},
		Runtime:         rt,
		Profiler:        fake.NewProfiler(),
		System:          fake.NewSystem(),
		AllowLocalRepos: true,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	baseSnap := "demo-g1"
	if err := store.Create(ctx, "bases/demo/g1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Snapshot(ctx, "bases/demo/g1", baseSnap); err != nil {
		t.Fatal(err)
	}
	eng.mu.Lock()
	eng.bases[baseSnap] = &pb.Base{
		Id:         baseSnap,
		Project:    "demo",
		Generation: 1,
		State:      warm,
		Repos:      []*pb.BaseRepo{{Repo: "app", Commit: baseCommit}},
	}
	eng.setBaseImage(baseSnap, eng.baseImageKey())
	eng.mu.Unlock()

	if _, err := eng.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{
		Id:    wsID,
		Agent: "test-agent",
		Project: &pb.HostedProject{
			Name:  "demo",
			Repos: []*pb.Repo{{Id: "app", Url: originRepo, Branch: "main"}},
		},
	}); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	cmd, err := eng.Exec(ctx, &pb.RunCommandRequest{
		WorkspaceId: wsID,
		Repo:        "app",
		Args:        []string{"/bin/sh", "-c", "sleep 30"},
	})
	if err != nil {
		t.Fatalf("Engine.Exec: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		eng.step(ctx)
		if cur, ok := eng.Command(cmd.Id); ok && cur.State == running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for command %s to start running", cmd.Id)
		}
		time.Sleep(25 * time.Millisecond)
	}

	// 1. Inspect routes through guestd and returns live guest processes.
	ins, err := eng.Inspect(ctx, &pb.InspectCommandRequest{Id: cmd.Id})
	if err != nil {
		t.Fatalf("Engine.Inspect(vm): %v", err)
	}
	if ins.CommandId != cmd.Id || len(ins.Procs) == 0 {
		t.Fatalf("unexpected VM inspect result: %+v", ins)
	}

	// 2. Trace routes through guestd and streams chunks.
	var gotTraceSummary *pb.TraceSummary
	if err := eng.Trace(ctx, &pb.TraceCommandRequest{
		Id:   cmd.Id,
		Kind: pb.TraceKind_TRACE_KIND_STACKS,
	}, func(ch *pb.TraceCommandChunk) error {
		if ch.Summary != nil {
			gotTraceSummary = ch.Summary
		}
		return nil
	}); err != nil {
		t.Fatalf("Engine.Trace(vm): %v", err)
	}
	if gotTraceSummary == nil || gotTraceSummary.Raw == "" {
		t.Fatalf("expected trace summary from guestd, got %+v", gotTraceSummary)
	}

	// 3. Sample routes through guestd and annotates Hostname with worker + VM workspace.
	var gotSample *pb.ProcessSample
	if err := eng.SampleCommand(ctx, &pb.SampleCommandRequest{
		Id:              cmd.Id,
		DurationSeconds: 1,
	}, func(ch *pb.SampleCommandChunk) error {
		if ch.Sample != nil {
			gotSample = ch.Sample
		}
		return nil
	}); err != nil {
		t.Fatalf("Engine.SampleCommand(vm): %v", err)
	}
	if gotSample == nil || len(gotSample.Procs) == 0 {
		t.Fatalf("expected ProcessSample from guestd, got %+v", gotSample)
	}
	if wantHost := "worker1 (vm:" + wsID + ")"; gotSample.Hostname != wantHost {
		t.Fatalf("sample Hostname = %q, want %q", gotSample.Hostname, wantHost)
	}

	// 4. CommandProfile clearly reports continuous eBPF profiling is unavailable inside VMs.
	prof, err := eng.CommandProfile(cmd.Id, &pb.GetProfileRequest{
		Target: &pb.GetProfileRequest_CommandId{CommandId: cmd.Id},
	})
	if err != nil {
		t.Fatalf("Engine.CommandProfile(vm): %v", err)
	}
	if prof.Active || !strings.Contains(prof.StatusNote, "Firecracker VMs") {
		t.Fatalf("unexpected VM CommandProfile: active=%v note=%q", prof.Active, prof.StatusNote)
	}

	// 5. StopWith --diagnose collects guest sample + inspection before stopping.
	stopped, err := eng.StopWith(ctx, cmd.Id, Intervention{
		Comment:  "checking VM diagnostics on stop",
		Diagnose: true,
		Host:     "laptop",
	})
	if err != nil {
		t.Fatalf("StopWith(diagnose): %v", err)
	}
	if len(stopped.Interventions) != 1 || !stopped.Interventions[0].Diagnosed || stopped.Interventions[0].DiagnosticsError != "" {
		t.Fatalf("unexpected VM intervention diagnostics: %+v", stopped.Interventions)
	}

	// 6. Ended VM command returns CONFLICT without a broken "use hotbed profile" hint.
	_, err = eng.Inspect(ctx, &pb.InspectCommandRequest{Id: cmd.Id})
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.CodeConflict {
		t.Fatalf("Inspect(ended vm) err = %v, want CONFLICT", err)
	}
	if strings.Contains(ae.Suggestion, "hotbed profile") {
		t.Fatalf("ended VM inspect should not suggest host eBPF profile, got suggestion=%q", ae.Suggestion)
	}
}
