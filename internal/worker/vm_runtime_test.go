package worker

import (
	"bytes"
	"context"
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
