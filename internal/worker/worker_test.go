package worker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/httpx"
	"github.com/semistrict/hotbed/internal/statedir"
)

// A worker must reject --user root (or any account resolving to uid 0 or gid 0)
// during Join, Run, and NewEngine rather than normalizing uid 0 to 1000 inside
// baseimage provisioning (board 9272).
func TestRejectRootCommandUser(t *testing.T) {
	origLookup := lookupOSUser
	t.Cleanup(func() { lookupOSUser = origLookup })

	lookupOSUser = func(name string) (*user.User, error) {
		switch name {
		case "root":
			return &user.User{Username: "root", Uid: "0", Gid: "0", HomeDir: "/root"}, nil
		case "toor":
			return &user.User{Username: "toor", Uid: "0", Gid: "1000", HomeDir: "/root"}, nil
		case "wheelonly":
			return &user.User{Username: "wheelonly", Uid: "1000", Gid: "0", HomeDir: "/home/wheelonly"}, nil
		case "builder":
			return &user.User{Username: "builder", Uid: "1000", Gid: "1000", HomeDir: "/home/builder"}, nil
		default:
			return nil, user.UnknownUserError(name)
		}
	}

	assertRejected := func(t *testing.T, err error, wantSubstr string) {
		t.Helper()
		if err == nil {
			t.Fatalf("got nil error, want %q", wantSubstr)
		}
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.CodeInvalidArgs {
			t.Fatalf("err = %v (%T), want api.CodeInvalidArgs", err, err)
		}
		if !strings.Contains(err.Error(), wantSubstr) {
			t.Fatalf("err = %q, want substring %q", err.Error(), wantSubstr)
		}
	}

	t.Run("resolveCommandUser", func(t *testing.T) {
		_, _, err := resolveCommandUser("root")
		assertRejected(t, err, "uid 0 (root)")

		_, _, err = resolveCommandUser("toor")
		assertRejected(t, err, "uid 0 (root)")

		_, _, err = resolveCommandUser("wheelonly")
		assertRejected(t, err, "gid 0 (root)")

		_, _, err = resolveCommandUser("")
		assertRejected(t, err, "command user is required")

		_, _, err = resolveCommandUser("missing")
		assertRejected(t, err, `no user "missing"`)

		uid, gid, err := resolveCommandUser("builder")
		if err != nil || uid != 1000 || gid != 1000 {
			t.Fatalf("resolveCommandUser(builder) = (%d, %d, %v), want (1000, 1000, nil)", uid, gid, err)
		}
	})

	for _, bad := range []struct {
		user string
		want string
	}{
		{"root", "uid 0 (root)"},
		{"toor", "uid 0 (root)"},
		{"wheelonly", "gid 0 (root)"},
	} {
		t.Run("Join/"+bad.user, func(t *testing.T) {
			checkCalled := false
			_, err := Join(t.Context(), JoinOptions{
				Dir:        t.TempDir(),
				Controller: "https://ctrl.example.com",
				User:       bad.user,
			}, nil, func(context.Context, JoinOptions) (string, error) {
				checkCalled = true
				return "btrfs", nil
			})
			assertRejected(t, err, bad.want)
			if checkCalled {
				t.Fatal("Join invoked check callback before rejecting root user")
			}
		})

		t.Run("Run/"+bad.user, func(t *testing.T) {
			err := Run(t.Context(), t.TempDir(), statedir.WorkerConfig{
				Controller: "https://ctrl.example.com",
				User:       bad.user,
			}, nil)
			assertRejected(t, err, bad.want)
		})

		t.Run("NewEngine/"+bad.user, func(t *testing.T) {
			store := &fake.Store{Root: t.TempDir()}
			_, err := NewEngine(t.Context(), EngineConfig{
				Dir:       t.TempDir(),
				NodeID:    "n1",
				Host:      "worker1",
				User:      bad.user,
				Store:     store,
				BaseImage: fake.BaseImage(store),
				Units:     &fake.Units{},
				System:    fake.NewSystem(),
			})
			assertRejected(t, err, bad.want)
		})
	}
}

// Verify the real OS user lookup (without stubbing) rejects "root".
func TestRejectRealOSRootUser(t *testing.T) {
	if _, err := user.Lookup("root"); err != nil {
		t.Skipf("no root user on this host: %v", err)
	}
	if _, _, err := resolveCommandUser("root"); err == nil || !strings.Contains(err.Error(), "uid 0 (root)") {
		t.Fatalf("resolveCommandUser(root) = %v, want uid 0 error", err)
	}
}

// Worker HTTP server shutdown must terminate open FollowLog and WatchWorkspace
// streams immediately via Engine.DrainStreams (not wait 10s for srv.Shutdown to
// time out before eng.Close cancels bgCtx), while leaving bgCtx live during
// the graceful drain window for in-flight unary RPCs.
func TestServerShutdownDrainsStreamsImmediately(t *testing.T) {
	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	eng, err := NewEngine(t.Context(), EngineConfig{
		Dir: dir, NodeID: "n1", Host: "worker1",
		Store: store, BaseImage: fake.BaseImage(store), Units: &fake.Units{}, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })

	// Seed a running command with an existing log file (so FollowLog flushes
	// response headers and blocks waiting for further output) and an idle
	// workspace (so WatchWorkspace sends the initial state and blocks).
	logPath, err := eng.logPath("cmd-stream")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("initial line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng.mu.Lock()
	eng.commands["cmd-stream"] = eng.newCommand(&pb.CommandRecord{
		Command: &pb.Command{Id: "cmd-stream", WorkspaceId: "ws-stream", State: running},
	})
	eng.workspaces["ws-stream"] = &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{Id: "ws-stream", Project: "app", State: idle},
		Volume:    "ws/ws-stream",
	}}
	eng.mu.Unlock()

	peer := httpx.Peer{NodeID: "nClient", Hostname: "laptop", Login: "ramon@"}
	srv := &http.Server{
		Handler:           Handler(peer, eng),
		ReadHeaderTimeout: 10 * time.Second,
		Protocols:         httpx.Protocols(),
	}
	srv.RegisterOnShutdown(eng.DrainStreams)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		<-serveErr
	})

	conn, err := api.Dial(ln.Addr().String(), api.WorkerPort, &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	client := hotbedv1connect.NewWorkspaceServiceClient(conn)

	// Open both streams and read their initial messages while keeping the
	// client contexts alive (holding the HTTP connections open).
	streamCtx, cancelStreams := context.WithCancel(t.Context())
	defer cancelStreams()

	followStream, err := client.FollowLog(streamCtx, &pb.FollowLogRequest{Id: "cmd-stream", Follow: true})
	if err != nil {
		t.Fatalf("FollowLog: %v", err)
	}
	defer followStream.Close()
	firstChunk, err := followStream.Receive()
	if err != nil {
		t.Fatalf("FollowLog initial receive: %v", err)
	}
	if string(firstChunk.Data) != "initial line\n" {
		t.Fatalf("FollowLog initial chunk = %q, want %q", firstChunk.Data, "initial line\n")
	}

	watchStream, err := client.WatchWorkspace(streamCtx, &pb.WatchWorkspaceRequest{Id: "ws-stream"})
	if err != nil {
		t.Fatalf("WatchWorkspace: %v", err)
	}
	defer watchStream.Close()
	firstWS, err := watchStream.Receive()
	if err != nil {
		t.Fatalf("WatchWorkspace initial receive: %v", err)
	}
	if firstWS.Id != "ws-stream" {
		t.Fatalf("WatchWorkspace initial id = %q, want ws-stream", firstWS.Id)
	}

	followErrCh := make(chan error, 1)
	go func() {
		_, err := followStream.Receive()
		followErrCh <- err
	}()
	watchErrCh := make(chan error, 1)
	go func() {
		_, err := watchStream.Receive()
		watchErrCh <- err
	}()

	// Shut down the HTTP server without calling eng.Close() first: RegisterOnShutdown
	// calls eng.DrainStreams(), so srv.Shutdown must finish well under 1s (not 10s).
	start := time.Now()
	shutdownCtx, shutdownCancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("srv.Shutdown: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("srv.Shutdown took %v, want < 1s", elapsed)
	}

	// bgCtx must remain live during HTTP shutdown (only cancelled on eng.Close).
	if eng.bgCtx.Err() != nil {
		t.Fatalf("eng.bgCtx was cancelled during srv.Shutdown: %v", eng.bgCtx.Err())
	}

	for _, tc := range []struct {
		name string
		ch   <-chan error
	}{
		{"FollowLog", followErrCh},
		{"WatchWorkspace", watchErrCh},
	} {
		select {
		case err := <-tc.ch:
			if connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), "worker shutting down") {
				t.Errorf("%s error = %v (code %v), want CodeUnavailable worker shutting down", tc.name, err, connect.CodeOf(err))
			}
		case <-time.After(time.Second):
			t.Errorf("%s did not terminate promptly on shutdown", tc.name)
		}
	}
}

// clientAs is a WorkspaceService client of eng whose every call comes from
// peer, as the worker's tunnel or Unix socket names its caller.
func clientAs(t *testing.T, peer httpx.Peer, eng *Engine) hotbedv1connect.WorkspaceServiceClient {
	t.Helper()
	ts := httptest.NewUnstartedServer(Handler(peer, eng))
	ts.Config.Protocols = httpx.Protocols()
	ts.Start()
	t.Cleanup(ts.Close)
	conn, err := api.Dial(strings.TrimPrefix(ts.URL, "http://"), api.WorkerPort, ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	return hotbedv1connect.NewWorkspaceServiceClient(conn)
}

// A worker whose hotbed.json records no node of its own (one that joined
// when nodes were the tailnet's) refuses to run and says to join again,
// rather than ask a tailnet for one.
func TestRunRefusesAWorkerWithNoNode(t *testing.T) {
	origLookup := lookupOSUser
	t.Cleanup(func() { lookupOSUser = origLookup })
	lookupOSUser = func(name string) (*user.User, error) {
		return &user.User{Username: name, Uid: "1000", Gid: "1000", HomeDir: "/home/" + name}, nil
	}
	err := Run(t.Context(), t.TempDir(), statedir.WorkerConfig{
		ClusterID: "c1", Controller: "https://ctrl.example.com", User: "builder", Token: "tok",
	}, nil)
	e := api.FromConnect(err)
	if e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "records no node ID, host or controller node") ||
		e.Suggestion != "join again: hotbed worker init https://ctrl.example.com" {
		t.Fatalf("Run with no node = %+v, want INVALID_ARGS saying to join again", e)
	}
}
