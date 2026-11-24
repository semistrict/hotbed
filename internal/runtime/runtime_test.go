package runtime_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/guestd"
	"github.com/semistrict/hotbed/internal/runtime"
)

type fakeResolver struct {
	mu      sync.Mutex
	clients map[string]*guestd.Client
	stopped map[string]bool
	makeFn  func(wsID string) (*guestd.Client, error)
}

func (f *fakeResolver) EnsureVM(ctx context.Context, wsID string) (*guestd.Client, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.clients[wsID]; ok {
		return c, nil
	}
	c, err := f.makeFn(wsID)
	if err != nil {
		return nil, err
	}
	f.clients[wsID] = c
	return c, nil
}

func (f *fakeResolver) ClientFor(wsID string) (*guestd.Client, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.clients[wsID]
	if !ok {
		return nil, errors.New("vm not running")
	}
	return c, nil
}

func (f *fakeResolver) StopVM(ctx context.Context, wsID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.clients, wsID)
	f.stopped[wsID] = true
	return nil
}

func TestGuestRuntimeEndToEnd(t *testing.T) {
	workDir := t.TempDir()
	sockDir, err := os.MkdirTemp("", "rt-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)

	srv, err := guestd.NewServer(guestd.ServerConfig{
		WorkDir:  workDir,
		StateDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	paths := map[uint32]string{}
	for _, port := range []uint32{guestd.ControlPort, guestd.ShellPort, guestd.TCPProxyPort} {
		p := filepath.Join(sockDir, fmt.Sprintf("p%d.sock", port))
		ln, err := net.Listen("unix", p)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		paths[port] = p
		switch port {
		case guestd.ControlPort:
			go func() { _ = srv.ServeControl(ctx, ln) }()
		case guestd.ShellPort:
			go func() { _ = srv.ServeShell(ctx, ln) }()
		case guestd.TCPProxyPort:
			go func() { _ = srv.ServeTCPProxy(ctx, ln) }()
		}
	}

	res := &fakeResolver{
		clients: map[string]*guestd.Client{},
		stopped: map[string]bool{},
		makeFn: func(wsID string) (*guestd.Client, error) {
			return guestd.NewClient(guestd.UnixMapDialer(paths)), nil
		},
	}

	var rt runtime.WorkspaceRuntime = runtime.NewGuestRuntime(res)
	if rt.Kind() != runtime.KindVM {
		t.Fatalf("Kind: got %q want %q", rt.Kind(), runtime.KindVM)
	}
	if err := rt.EnsureReady(ctx, "ws-1"); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	if err := rt.StartCommand(ctx, "ws-1", runtime.CommandSpec{
		ID:   "c1",
		Argv: []string{"/bin/sh", "-c", "echo runtime-ok"},
		Dir:  workDir,
	}); err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	var out bytes.Buffer
	if _, err := rt.StreamLogs(ctx, "ws-1", "c1", 0, true, &out); err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	if strings.TrimSpace(out.String()) != "runtime-ok" {
		t.Fatalf("unexpected log: %q", out.String())
	}
	sts, err := rt.StatusMany(ctx, "ws-1", []string{"c1"})
	if err != nil {
		t.Fatalf("StatusMany: %v", err)
	}
	if !sts["c1"].Exited || sts["c1"].ExitCode != 0 {
		t.Fatalf("unexpected status: %+v", sts["c1"])
	}
	if err := rt.Teardown(ctx, "ws-1"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if !res.stopped["ws-1"] {
		t.Fatal("expected StopVM to be called")
	}
	if _, err := rt.StatusMany(ctx, "ws-1", nil); err == nil {
		t.Fatal("expected error after Teardown")
	}
	_ = time.Second
}

type deadVMResolver struct {
	*fakeResolver
	exited map[string]int
}

func (d *deadVMResolver) VMExited(_ context.Context, wsID string) (bool, int) {
	code, ok := d.exited[wsID]
	return ok, code
}

func TestGuestRuntimeDeadVMStatusAndStop(t *testing.T) {
	// Point client at a closed Unix socket so every vsock RPC fails with ECONNREFUSED.
	closedSock := filepath.Join(t.TempDir(), "dead.sock")
	deadClient := guestd.NewClient(guestd.UnixMapDialer(map[uint32]string{
		guestd.ControlPort: closedSock,
	}))
	res := &deadVMResolver{
		fakeResolver: &fakeResolver{
			clients: map[string]*guestd.Client{"ws-dead": deadClient},
			stopped: map[string]bool{},
		},
		exited: map[string]int{"ws-dead": 158},
	}
	rt := runtime.NewGuestRuntime(res)
	ctx := context.Background()

	sts, err := rt.StatusMany(ctx, "ws-dead", []string{"c1", "c2"})
	if err != nil {
		t.Fatalf("StatusMany on dead VM returned error: %v", err)
	}
	for _, id := range []string{"c1", "c2"} {
		st := sts[id]
		if !st.Exited || st.ExitCode != 158 {
			t.Fatalf("status[%s] = %+v, want Exited=true ExitCode=158", id, st)
		}
	}
	if err := rt.StopCommand(ctx, "ws-dead", "c1", time.Second); err != nil {
		t.Fatalf("StopCommand on dead VM returned error: %v", err)
	}
}

type oomVMResolver struct {
	*fakeResolver
	ensureCalls int
	reclaimHits int
}

func (o *oomVMResolver) EnsureVM(ctx context.Context, wsID string) (*guestd.Client, error) {
	o.mu.Lock()
	o.ensureCalls++
	o.mu.Unlock()
	return o.fakeResolver.EnsureVM(ctx, wsID)
}

func (o *oomVMResolver) VMExitStatus(_ context.Context, wsID string) (bool, int, int, string, bool) {
	if wsID == "ws-oom" {
		return true, 0, 9, "oom-kill", true
	}
	return false, 0, 0, "", false
}

func TestGuestRuntimeOOMStatusEnsureVMAndReclaim(t *testing.T) {
	ctx := context.Background()
	closedSock := filepath.Join(t.TempDir(), "dead.sock")
	deadClient := guestd.NewClient(guestd.UnixMapDialer(map[uint32]string{
		guestd.ControlPort: closedSock,
	}))

	// Start a minimal HTTP server on a unix socket to test StartCommand -> EnsureVM
	// recovery and Reclaim (including 404 compatibility with older guestd).
	sockDir, err := os.MkdirTemp("", "rt-rec-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	ctrlSock := filepath.Join(sockDir, "ctrl.sock")
	ln, err := net.Listen("unix", ctrlSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	var reclaimStatus int = http.StatusOK
	var reclaimSeen int
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/commands/start", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c-new","state":"running","main_pid":42}`))
	})
	mux.HandleFunc("/v1/reclaim", func(w http.ResponseWriter, r *http.Request) {
		reclaimSeen++
		if reclaimStatus == http.StatusNotFound {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	liveClient := guestd.NewClient(guestd.UnixMapDialer(map[uint32]string{
		guestd.ControlPort: ctrlSock,
	}))

	res := &oomVMResolver{
		fakeResolver: &fakeResolver{
			clients: map[string]*guestd.Client{
				"ws-oom":  deadClient,
				"ws-live": liveClient,
			},
			stopped: map[string]bool{},
		},
	}
	rt := runtime.NewGuestRuntime(res)

	// 1. StatusMany propagates OOMKilled / Signal / Result from VMExitStatus.
	sts, err := rt.StatusMany(ctx, "ws-oom", []string{"c1"})
	if err != nil {
		t.Fatalf("StatusMany on OOM VM: %v", err)
	}
	st := sts["c1"]
	if !st.Exited || !st.OOMKilled || st.Signal != 9 || st.Result != "oom-kill" {
		t.Fatalf("OOM status = %+v, want Exited=true OOMKilled=true Signal=9 Result=oom-kill", st)
	}

	// 2. StartCommand always goes through EnsureVM (not bypassing it via ClientFor),
	// so a dead VM gets detected/rebooted instead of dialing a stale vsock client.
	if err := rt.StartCommand(ctx, "ws-live", runtime.CommandSpec{
		ID:   "c-new",
		Argv: []string{"true"},
		Dir:  "/work",
	}); err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	if res.ensureCalls != 1 {
		t.Fatalf("StartCommand called EnsureVM %d times, want 1", res.ensureCalls)
	}

	// 3. Reclaim calls /v1/reclaim and ignores 404 from older guestd binaries.
	if err := rt.Reclaim(ctx, "ws-live", "c-new", 1<<20); err != nil {
		t.Fatalf("Reclaim (200): %v", err)
	}
	reclaimStatus = http.StatusNotFound
	if err := rt.Reclaim(ctx, "ws-live", "", 0); err != nil {
		t.Fatalf("Reclaim (404 compat): %v", err)
	}
	if reclaimSeen != 2 {
		t.Fatalf("reclaimSeen = %d, want 2", reclaimSeen)
	}
}
