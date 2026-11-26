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

	"github.com/google/go-cmp/cmp"
	cladefsdv1 "github.com/semistrict/cladefs/go/cladefsd/v1"
	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/guestd"
	"github.com/semistrict/hotbed/internal/profiler"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/unit"
	"google.golang.org/protobuf/testing/protocmp"
)

type testGuestResolver struct {
	mu      sync.Mutex
	clients map[string]*guestd.Client
}

func (r *testGuestResolver) EnsureVM(_ context.Context, wsID string, _ unit.VMSize) (*guestd.Client, error) {
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

	// 1. Sync a real modified file via gitops.Snapshot -> git bundle -> Engine.Sync -> guestd Sync RPC!
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
	// Its VM stopped as it ended: the next command boots a new one.
	if rt.torn[wsID] != 1 {
		t.Fatalf("Teardown count after the command ended = %d, want 1", rt.torn[wsID])
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
	if rt.torn[wsID] != 2 {
		t.Fatalf("Teardown count after release = %d, want 2", rt.torn[wsID])
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
	if eng.usesRuntime("ws-cladefs") {
		t.Fatal("expected ws-cladefs not to use runtime while e.mu is held")
	}
}

func TestResolveWorkspaceRuntimeOnlyAllowsVMAndCladefs(t *testing.T) {
	vmEng := &Engine{cfg: EngineConfig{VM: &VMBackend{}}}
	fuseEng := &Engine{cfg: EngineConfig{CladefsFUSE: &CladefsFUSEBackend{}}}

	if got, err := vmEng.resolveWorkspaceRuntime(&pb.CreateWorkspaceRequest{}); err != nil || got != runtime.KindVM {
		t.Fatalf("vm default = %q, %v; want %q, nil", got, err, runtime.KindVM)
	}
	if _, err := vmEng.resolveWorkspaceRuntime(&pb.CreateWorkspaceRequest{Runtime: "chroot"}); err == nil {
		t.Fatal("vm engine must reject chroot runtime")
	}
	if _, err := vmEng.resolveWorkspaceRuntime(&pb.CreateWorkspaceRequest{Runtime: runtime.KindCladefs}); err == nil {
		t.Fatal("vm-only engine must reject cladefs runtime")
	}
	if _, err := vmEng.resolveWorkspaceRuntime(&pb.CreateWorkspaceRequest{Placement: &pb.Placement{Bench: true}}); err == nil {
		t.Fatal("vm-only engine must reject --bench")
	}

	if got, err := fuseEng.resolveWorkspaceRuntime(&pb.CreateWorkspaceRequest{}); err != nil || got != runtime.KindCladefs {
		t.Fatalf("cladefs default = %q, %v; want %q, nil", got, err, runtime.KindCladefs)
	}
	if got, err := fuseEng.resolveWorkspaceRuntime(&pb.CreateWorkspaceRequest{Placement: &pb.Placement{Bench: true}}); err != nil || got != runtime.KindCladefs {
		t.Fatalf("cladefs --bench = %q, %v; want %q, nil", got, err, runtime.KindCladefs)
	}
	if _, err := fuseEng.resolveWorkspaceRuntime(&pb.CreateWorkspaceRequest{Runtime: "chroot"}); err == nil {
		t.Fatal("cladefs engine must reject chroot runtime")
	}
	if _, err := fuseEng.resolveWorkspaceRuntime(&pb.CreateWorkspaceRequest{Runtime: runtime.KindVM}); err == nil {
		t.Fatal("cladefs-only engine must reject vm runtime")
	}
	if _, err := fuseEng.resolveWorkspaceRuntime(&pb.CreateWorkspaceRequest{Placement: &pb.Placement{Bench: true, Runtime: runtime.KindVM}}); err == nil {
		t.Fatal("cladefs engine must reject --bench with vm runtime")
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
			"stores_open":                 4,
			"disk_bytes":                  12_000_000_000,
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
	if got.ExtentCacheBytes != 4_000_000_000 || got.ExtentCacheCapacityBytes != 6_000_000_000 || got.ExtentCachePinnedEntries != 2451 || got.ExtentCacheIdleEntries != 10943 || got.CacheTrims != 7 || got.OpenCells != 4 || got.HostDiskCellsBytes != 12_000_000_000 {
		t.Fatalf("unexpected extent cache / daemon stats: %+v", got)
	}
	if got.ActiveVms != 2 || got.WindowMappedBytes != 3_000_000_000 || got.DaxWindowFull != 5 || got.DaxWindowReclaims != 9 || got.DaxRegionsFull != 1 || got.DaxWritesHeldBySealsNs != 1_500_000_000 || got.DaxDeviceReadBytes != 150_000 || got.DaxDeviceReadsNs != 300_000 {
		t.Fatalf("unexpected DAX stats: %+v", got)
	}

	prom := strings.Join([]string{
		"# HELP cladefsd_part_cache_bytes x",
		"cladefsd_part_cache_bytes 37995837625",
		"cladefsd_part_cache_capacity_bytes 593477874669",
		`cladefsd_host_disk_size_bytes{device="259:2"} 1000000000000`,
		`cladefsd_host_disk_free_bytes{device="259:2"} 700000000000`,
		`cladefsd_host_disk_stores_bytes{device="259:2"} 20000000000`,
		`cladefsd_host_disk_cache_bytes{device="259:2"} 180000000000`,
		`cladefsd_host_disk_other_bytes{device="259:2"} 100000000000`,
		`cladefsd_host_disk_pressure{device="259:2"} 1`,
		`cladefsd_mount_cells{state="attached"} 3`,
		`cladefsd_mount_cells{state="detaching"} 1`,
		"cladefsd_page_cache_bytes 268435456",
		"cladefsd_page_cache_dirty_bytes 8388608",
		"cladefsd_page_cache_limit_bytes 536870912",
		`cladefsd_peer_parts_total{outcome="given"} 42`,
		`cladefsd_peer_parts_total{outcome="missing"} 5`,
		`cladefsd_peer_parts_total{outcome="failed"} 3`,
		`cladefsd_peer_served_parts_total{outcome="served"} 19`,
		`cladefsd_peer_served_parts_total{outcome="missing"} 2`,
		`cladefsd_part_cache_shard_state{device="230:16",shard="0"} 0`,
		`cladefsd_part_cache_shard_state{device="230:0",shard="1"} 2`,
		`cladefsd_upload_pending_bytes{cell="0/abc"} 1000`,
		`cladefsd_upload_pending_bytes{cell="0/def"} 2500`,
		"cladefsd_memory_pressure 1",
		"cladefsd_memory_full 1",
		"cladefsd_memory_limit_bytes 464519168",
		"cladefsd_memory_floor_bytes 464519168",
	}, "\n")
	if ok := applyCladefsPrometheusMetrics(got, prom); !ok {
		t.Fatal("applyCladefsPrometheusMetrics returned false")
	}
	if got.PartCacheBytes != 37995837625 || got.PartCacheCapacityBytes != 593477874669 {
		t.Fatalf("unexpected part cache: %+v", got)
	}
	if got.HostDiskSizeBytes != 1000000000000 || got.HostDiskFreeBytes != 700000000000 || got.HostDiskCellsBytes != 20000000000 || got.HostDiskCacheBytes != 180000000000 || got.HostDiskOtherBytes != 100000000000 || !got.HostDiskPressure {
		t.Fatalf("unexpected host disk stats: %+v", got)
	}
	if got.MountCellsAttached != 3 || got.PageCacheBytes != 268435456 || got.PageCacheDirtyBytes != 8388608 || got.PageCacheLimitBytes != 536870912 {
		t.Fatalf("unexpected mount/page cache stats: %+v", got)
	}
	if got.PeerPartsGiven != 42 || got.PeerPartsServed != 19 || got.UploadPendingBytes != 3500 || !got.MemoryPressure || !got.MemoryFull || got.ExtentCacheLimitBytes != 464519168 || got.ExtentCacheFloorBytes != 464519168 {
		t.Fatalf("unexpected peer/upload/memory governor stats: %+v", got)
	}
	if got.PeerPartsMissing != 5 || got.PeerPartsFailed != 3 || got.PeerServedPartsMissing != 2 {
		t.Fatalf("peer outcomes: missing=%d failed=%d served-missing=%d, want 5 3 2", got.PeerPartsMissing, got.PeerPartsFailed, got.PeerServedPartsMissing)
	}
	wantShards := []*pb.PartCacheShard{
		{Shard: 0, Device: "230:16", State: pb.PartCacheShardState_PART_CACHE_SHARD_STATE_ONLINE},
		{Shard: 1, Device: "230:0", State: pb.PartCacheShardState_PART_CACHE_SHARD_STATE_OFFLINE},
	}
	if diff := cmp.Diff(wantShards, got.PartCacheShards, protocmp.Transform()); diff != "" {
		t.Fatalf("part cache shards (-want +got):\n%s", diff)
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
	prev := &projectBase{
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
	reclaimErr error
	statuses   map[string]map[string]runtime.CommandStatus
}

func (r *reclaimSpyRuntime) Reclaim(_ context.Context, wsID, cmdID string, bytes int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, struct {
		wsID  string
		cmdID string
		bytes int64
	}{wsID: wsID, cmdID: cmdID, bytes: bytes})
	return r.reclaimErr
}

func (r *reclaimSpyRuntime) StatusMany(_ context.Context, wsID string, ids []string) (map[string]runtime.CommandStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]runtime.CommandStatus{}
	for _, id := range ids {
		if st, ok := r.statuses[wsID][id]; ok {
			out[id] = st
		}
	}
	return out, nil
}

type hostVMReclaimUnits struct {
	*fake.Units
	mu           sync.Mutex
	vmUnits      map[string]unit.State
	vmMem        map[string]unit.MemoryUse
	reclaimCalls []struct {
		cg string
		n  int64
	}
	freezeCalls []string
	reclaimErr  error
}

func (h *hostVMReclaimUnits) Status(ctx context.Context, name string) (unit.State, error) {
	h.mu.Lock()
	st, ok := h.vmUnits[name]
	h.mu.Unlock()
	if ok {
		return st, nil
	}
	return h.Units.Status(ctx, name)
}

func (h *hostVMReclaimUnits) Freeze(ctx context.Context, name string) error {
	h.mu.Lock()
	h.freezeCalls = append(h.freezeCalls, name)
	h.mu.Unlock()
	return h.Units.Freeze(ctx, name)
}

func (h *hostVMReclaimUnits) Memory(cg string) (unit.MemoryUse, error) {
	h.mu.Lock()
	m, ok := h.vmMem[cg]
	h.mu.Unlock()
	if ok {
		return m, nil
	}
	return h.Units.Memory(cg)
}

func (h *hostVMReclaimUnits) Reclaim(cg string, n int64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reclaimCalls = append(h.reclaimCalls, struct {
		cg string
		n  int64
	}{cg: cg, n: n})
	if h.reclaimErr != nil {
		return h.reclaimErr
	}
	if m, ok := h.vmMem[cg]; ok {
		moved := min(n, m.Anon)
		m.Anon -= moved
		m.Swap += moved
		h.vmMem[cg] = m
	}
	return nil
}

func TestRuntimeUnitsReclaimRoutesVMCgroupToGuestAndHostFirecrackerUnit(t *testing.T) {
	vmCG := "/user.slice/user-1000.slice/user-1000-hotbed.slice/hotbed-vm-ws-42.service"
	inner := &hostVMReclaimUnits{
		Units: &fake.Units{},
		vmUnits: map[string]unit.State{
			"hotbed-vm-ws-42": {Loaded: true, Running: true, Cgroup: vmCG},
		},
		vmMem: map[string]unit.MemoryUse{
			// 1024 MiB anon on host Firecracker unit (960 MiB workload + 64 MiB VMM/guestd).
			vmCG: {Anon: 1024 << 20, Swap: 0},
		},
	}
	spy := &reclaimSpyRuntime{
		statuses: map[string]map[string]runtime.CommandStatus{
			"ws-42": {
				"cmd-9": {ID: "cmd-9", Frozen: true, MemoryAnon: 960 << 20, MemorySwap: 0},
			},
		},
	}
	ru := newRuntimeUnits(inner, spy, func(string) bool { return true }, 0, 0)

	// Reclaim pushes 960 MiB out of the host Firecracker unit while preserving the
	// 64 MiB hot floor (never freezing the host Firecracker unit!).
	if err := ru.Reclaim("vm:ws-42:cmd-9", 1024<<20); err != nil {
		t.Fatalf("Reclaim(vm cgroup): %v", err)
	}
	spy.mu.Lock()
	if len(spy.calls) != 1 || spy.calls[0].wsID != "ws-42" || spy.calls[0].cmdID != "cmd-9" || spy.calls[0].bytes != 1024<<20 {
		spy.mu.Unlock()
		t.Fatalf("guest reclaim calls = %+v", spy.calls)
	}
	spy.mu.Unlock()

	inner.mu.Lock()
	if len(inner.freezeCalls) != 0 {
		inner.mu.Unlock()
		t.Fatalf("host Firecracker unit must NEVER be frozen, got freezeCalls=%v", inner.freezeCalls)
	}
	if len(inner.reclaimCalls) != 1 || inner.reclaimCalls[0].cg != vmCG || inner.reclaimCalls[0].n != 960<<20 {
		inner.mu.Unlock()
		t.Fatalf("host reclaim calls = %+v, want 960 MiB (clamped by 64 MiB floor)", inner.reclaimCalls)
	}
	inner.mu.Unlock()

	// Memory() now reflects host-swapped anonymous RAM while preserving Held().
	mem, err := ru.Memory("vm:ws-42:cmd-9")
	if err != nil {
		t.Fatalf("Memory after reclaim: %v", err)
	}
	if mem.Anon != 64<<20 || mem.Swap != 896<<20 || mem.Held() != 960<<20 {
		t.Fatalf("Memory after host reclaim = %+v (held=%d), want Anon=64MiB Swap=896MiB Held=960MiB", mem, mem.Held())
	}

	// Tiny VM below the 64 MiB floor does not needlessly reclaim host VMM RAM.
	if err := ru.Reclaim("vm:ws-42:cmd-9", 64<<20); err != nil {
		t.Fatalf("Reclaim below floor: %v", err)
	}
	inner.mu.Lock()
	if len(inner.reclaimCalls) != 1 {
		inner.mu.Unlock()
		t.Fatalf("expected no host reclaim when host anon <= 64 MiB floor, got %+v", inner.reclaimCalls)
	}
	inner.mu.Unlock()

	// Even when in-guest reclaim fails (older guestd timeout), host reclaim still runs.
	inner.mu.Lock()
	inner.vmMem[vmCG] = unit.MemoryUse{Anon: 576 << 20, Swap: 0}
	inner.mu.Unlock()
	spy.mu.Lock()
	spy.reclaimErr = context.DeadlineExceeded
	spy.mu.Unlock()
	if err := ru.Reclaim("vm:ws-42:cmd-9", 512<<20); err != nil {
		t.Fatalf("Reclaim when guestd fails but host succeeds should succeed, got: %v", err)
	}
	inner.mu.Lock()
	if len(inner.reclaimCalls) != 2 || inner.reclaimCalls[1].n != 512<<20 {
		inner.mu.Unlock()
		t.Fatalf("expected host reclaim fallback call of 512 MiB, got %+v", inner.reclaimCalls)
	}
	// Host reclaim error MUST propagate so scheduler does not mark reclaimed=true.
	inner.vmMem[vmCG] = unit.MemoryUse{Anon: 576 << 20, Swap: 0}
	inner.reclaimErr = errors.New("memory.reclaim: EPERM")
	inner.mu.Unlock()
	if err := ru.Reclaim("vm:ws-42:cmd-9", 512<<20); err == nil {
		t.Fatal("expected error when host memory.reclaim fails")
	}
}

// When guest zram, tmpfs, or guest kernel caches hold gigabytes inside the host
// Firecracker process while the guest command cgroup reports tiny anon memory
// (0-1 MiB), runtimeUnits.Memory must attribute the real host Firecracker RAM
// and runtimeUnits.Reclaim on a quiescent VM must reclaim all host RAM above
// vmHostMinResidentBytes (not just the tiny guest cgroup anon figure).
func TestRuntimeUnitsMemoryAndReclaimAccountForHostVMRam(t *testing.T) {
	vmCG := "/user.slice/user-1000.slice/user-1000-hotbed.slice/hotbed-vm-ws-zram.service"
	inner := &hostVMReclaimUnits{
		Units: &fake.Units{},
		vmUnits: map[string]unit.State{
			"hotbed-vm-ws-zram": {Loaded: true, Running: true, Cgroup: vmCG},
		},
		vmMem: map[string]unit.MemoryUse{
			// 3000 MiB host anon + 400 MiB host swap on the Firecracker unit.
			vmCG: {Anon: 3000 << 20, Swap: 400 << 20},
		},
	}
	spy := &reclaimSpyRuntime{
		statuses: map[string]map[string]runtime.CommandStatus{
			"ws-zram": {
				// Guest command pushed its pages to guest zram: guest anon is 1 MiB.
				"cmd-1": {ID: "cmd-1", Frozen: true, MemoryAnon: 1 << 20, MemorySwap: 256 << 20},
			},
		},
	}
	ru := newRuntimeUnits(inner, spy, func(string) bool { return true }, 0, 0)
	ru.isVMQuiescent = func(string) bool { return true }

	mem, err := ru.Memory("vm:ws-zram:cmd-1")
	if err != nil {
		t.Fatalf("Memory: %v", err)
	}
	wantAnon := int64(3000<<20) - vmHostMinResidentBytes
	wantHeld := int64(3400<<20) - vmHostMinResidentBytes
	if mem.Anon != wantAnon || mem.Held() != wantHeld {
		t.Fatalf("Memory = %+v (held=%d), want Anon=%d Held=%d (real host VM RAM)",
			mem, mem.Held(), wantAnon, wantHeld)
	}

	// Even when the caller passes n=0 (or tiny n=1 MiB), quiescent VM fallback
	// reclaim must push ALL reclaimable host VM RAM (3000 - 64 = 2936 MiB).
	if err := ru.Reclaim("vm:ws-zram:cmd-1", 0); err != nil {
		t.Fatalf("Reclaim(0): %v", err)
	}
	inner.mu.Lock()
	if len(inner.reclaimCalls) != 1 || inner.reclaimCalls[0].n != wantAnon {
		inner.mu.Unlock()
		t.Fatalf("host reclaim calls = %+v, want single reclaim of %d bytes", inner.reclaimCalls, wantAnon)
	}
	inner.mu.Unlock()
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

	guestBase, err := os.MkdirTemp("/tmp", "hb-guest-diag-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(guestBase) })
	if err := os.Chmod(guestBase, 0o755); err != nil {
		t.Fatal(err)
	}
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

	guestProf := fake.NewProfiler()
	srv, err := guestd.NewServer(guestd.ServerConfig{
		WorkDir:   guestWork,
		StateDir:  guestState,
		Inspector: fake.Inspector{},
		Profiler:  guestProf,
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
	hostProf := fake.NewProfiler()
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
		Profiler:        hostProf,
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
		if cur, ok := eng.Command(cmd.Id); ok {
			if cur.State == running {
				break
			}
			if !api.Live(cur.State) {
				t.Fatalf("command %s ended before running: state=%v reason=%q", cmd.Id, cur.State, cur.Reason)
			}
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

	// 3. Sample routes through guestd and annotates Hostname with worker + VM
	// workspace, and the sample is kept so labeled.
	var gotSample *pb.ProcessSample
	var gotSeq int32
	if err := eng.SampleCommand(ctx, &pb.SampleCommandRequest{
		Id:              cmd.Id,
		DurationSeconds: 1,
	}, "laptop", func(ch *pb.SampleCommandChunk) error {
		if ch.Sample != nil {
			gotSample = ch.Sample
		}
		if ch.Done {
			gotSeq = ch.Kept.GetSeq()
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
	if gotSeq != 1 {
		t.Fatalf("final sample chunk seq = %d, want 1", gotSeq)
	}
	kept, err := eng.PendingSamples()
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0].Seq != 1 || kept[0].Sample.GetHostname() != gotSample.Hostname || kept[0].By.GetHost() != "laptop" {
		t.Fatalf("kept samples = %+v, want sample 1 from laptop labeled %q", kept, gotSample.Hostname)
	}

	// 4. CommandProfile queries the guest's always-on eBPF profiler live over vsock.
	guestProf.Store.Record(profiler.Sample{
		CommandID: cmd.Id,
		Timestamp: time.Now(),
		Comm:      "python3",
		Frames: []profiler.ResolvedFrame{
			{Name: "_PyEval_EvalFrameDefault", FrameType: "native"},
			{Name: "hot_loop", FrameType: "python"},
		},
	})
	prof, err := eng.CommandProfile(cmd.Id, &pb.GetProfileRequest{
		Target: &pb.GetProfileRequest_CommandId{CommandId: cmd.Id},
	})
	if err != nil {
		t.Fatalf("Engine.CommandProfile(vm): %v", err)
	}
	if !prof.Active || !prof.Live || prof.TotalSamples != 1 {
		t.Fatalf("unexpected live VM CommandProfile: active=%v live=%v samples=%d note=%q", prof.Active, prof.Live, prof.TotalSamples, prof.StatusNote)
	}

	// 4b. NodeProfile merges host samples with live in-guest VM profiles.
	hostProf.Store.Record(profiler.Sample{
		Timestamp: time.Now(),
		Comm:      "firecracker",
		Frames: []profiler.ResolvedFrame{
			{Name: "vcpu_run", FrameType: "native"},
		},
	})
	nodeProf, err := eng.NodeProfile("worker1", &pb.GetProfileRequest{
		Target: &pb.GetProfileRequest_Node{Node: "worker1"},
	})
	if err != nil {
		t.Fatalf("Engine.NodeProfile: %v", err)
	}
	if !nodeProf.Active || nodeProf.TotalSamples != 2 || nodeProf.MatchedSamples != 2 {
		t.Fatalf("unexpected merged NodeProfile: active=%v total=%d matched=%d", nodeProf.Active, nodeProf.TotalSamples, nodeProf.MatchedSamples)
	}
	vmFiltered, err := eng.NodeProfile("worker1", &pb.GetProfileRequest{
		Target: &pb.GetProfileRequest_Node{Node: "worker1"},
		Query:  "hot_loop",
	})
	if err != nil {
		t.Fatalf("Engine.NodeProfile(query): %v", err)
	}
	if vmFiltered.MatchedSamples != 1 {
		t.Fatalf("NodeProfile(query=hot_loop) matched=%d, want 1", vmFiltered.MatchedSamples)
	}

	// 5. StopWith --diagnose collects guest sample + inspection before stopping,
	//    and synchronously snapshots + persists the guest eBPF profile BEFORE
	//    returning so immediate VM teardown cannot race persistence.
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

	// Immediately tear down guestd (simulating instant `hotbed session end`):
	// the profile must ALREADY be persisted on disk with no polling delay.
	_ = ctrlLn.Close()
	profPath, _ := eng.profilePath(cmd.Id)
	if _, statErr := os.Stat(profPath); statErr != nil {
		t.Fatalf("guest command profile not persisted synchronously on stop at %s: %v", profPath, statErr)
	}
	persisted, err := eng.CommandProfile(cmd.Id, &pb.GetProfileRequest{
		Target: &pb.GetProfileRequest_CommandId{CommandId: cmd.Id},
	})
	if err != nil {
		t.Fatalf("Engine.CommandProfile(persisted vm): %v", err)
	}
	if persisted.Live || persisted.TotalSamples != 1 {
		t.Fatalf("unexpected persisted VM CommandProfile: live=%v samples=%d", persisted.Live, persisted.TotalSamples)
	}

	// 6. Ended VM command returns CONFLICT suggesting `hotbed profile`.
	_, err = eng.Inspect(ctx, &pb.InspectCommandRequest{Id: cmd.Id})
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.CodeConflict {
		t.Fatalf("Inspect(ended vm) err = %v, want CONFLICT", err)
	}
	if !strings.Contains(ae.Suggestion, "hotbed profile") {
		t.Fatalf("ended VM inspect should suggest hotbed profile, got suggestion=%q", ae.Suggestion)
	}
}

func TestVMCommandGuestOOMAndPeakMemoryTelemetry(t *testing.T) {
	ctx := t.Context()
	e, _, _, now := parkEngine(t)

	for _, tc := range []struct {
		name         string
		id           string
		status       runtime.CommandStatus
		wantReason   string
		wantFailure  string
		wantExitCode *int32
	}{
		{
			name: "main process OOM killed in guest",
			id:   "guest-oom-main",
			status: runtime.CommandStatus{
				ID:         "guest-oom-main",
				Exited:     true,
				ExitCode:   137,
				Signal:     9,
				OOMKilled:  true,
				Result:     "oom-kill",
				MemoryAnon: 0,
				MemoryPeak: 7 << 30,
				CPUUsec:    120000,
			},
			wantReason:   "killed by the OOM killer (signal 9; peak 7.0 GiB)",
			wantFailure:  "oom",
			wantExitCode: nil,
		},
		{
			name: "child process OOM killed under shell",
			id:   "guest-oom-child",
			status: runtime.CommandStatus{
				ID:         "guest-oom-child",
				Exited:     true,
				ExitCode:   137,
				Signal:     0,
				OOMKilled:  true,
				Result:     "oom-kill",
				MemoryAnon: 0,
				MemoryPeak: 6 << 30,
			},
			wantReason:   "killed by the OOM killer (exit 137; peak 6.0 GiB)",
			wantFailure:  "oom",
			wantExitCode: func() *int32 { v := int32(137); return &v }(),
		},
		// The VM's end is never passed off as the command's own: cladefsd
		// letting a stalled VM go SIGKILLs Firecracker, and the command
		// was reported "killed by signal 9".
		{
			name:        "VM killed by a signal under it",
			id:          "vm-signal",
			status:      runtime.CommandStatus{ID: "vm-signal", Exited: true, VMExit: &unit.VMExit{Signal: 9, Result: "signal"}},
			wantReason:  "its VM was killed by signal 9",
			wantFailure: "vm",
		},
		{
			name:        "VM killed by the host's OOM killer",
			id:          "vm-oom",
			status:      runtime.CommandStatus{ID: "vm-oom", Exited: true, VMExit: &unit.VMExit{Signal: 9, Result: "oom-kill", OOMKilled: true}},
			wantReason:  "its VM was killed by the host's OOM killer",
			wantFailure: "vm",
		},
		{
			name:        "VM lost cladefsd",
			id:          "vm-158",
			status:      runtime.CommandStatus{ID: "vm-158", Exited: true, VMExit: &unit.VMExit{ExitCode: 158, Result: "exit-code"}},
			wantReason:  "its VM ended: firecracker exited 158: lost connection to cladefsd (or VM process was frozen past quarantine deadline)",
			wantFailure: "vm",
		},
		{
			name:        "VM gone",
			id:          "vm-gone",
			status:      runtime.CommandStatus{ID: "vm-gone", Exited: true, VMExit: &unit.VMExit{Gone: true}},
			wantReason:  "its VM stopped while it ran",
			wantFailure: "vm",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := e.newCommand(&pb.CommandRecord{
				Command: &pb.Command{
					Id:          tc.id,
					WorkspaceId: "a",
					State:       running,
					Kind:        "run",
				},
			})
			e.mu.Lock()
			e.commands[tc.id] = c
			e.mu.Unlock()

			st := toUnitState("a", tc.status)
			e.observe(ctx, c, st, *now)

			got := c.cmd()
			if got.State != failed {
				t.Fatalf("state = %v, want failed", got.State)
			}
			if got.Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if got.MemoryBytes != tc.status.MemoryPeak {
				t.Fatalf("MemoryBytes = %d, want %d", got.MemoryBytes, tc.status.MemoryPeak)
			}
			if c.rec.PeakMemoryBytes != tc.status.MemoryPeak {
				t.Fatalf("PeakMemoryBytes = %d, want %d", c.rec.PeakMemoryBytes, tc.status.MemoryPeak)
			}
			if tc.wantExitCode == nil && got.ExitCode != nil {
				t.Fatalf("ExitCode = %d, want nil", *got.ExitCode)
			}
			if tc.wantExitCode != nil && (got.ExitCode == nil || *got.ExitCode != *tc.wantExitCode) {
				t.Fatalf("ExitCode = %v, want %d", got.ExitCode, *tc.wantExitCode)
			}
			events, err := e.Events(0, 10000)
			if err != nil {
				t.Fatal(err)
			}
			var finished *pb.FinishedCommand
			for _, ev := range events {
				if ch := ev.GetCommand(); ch != nil && ch.Command.Id == tc.id && ch.Finished != nil {
					finished = ch.Finished
				}
			}
			if finished == nil || finished.Failure != tc.wantFailure {
				t.Fatalf("finished = %+v, want failure %q", finished, tc.wantFailure)
			}
		})
	}
}
