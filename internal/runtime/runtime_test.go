package runtime_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
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
