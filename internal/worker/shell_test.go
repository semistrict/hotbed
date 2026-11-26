package worker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/httpx"
	"github.com/semistrict/hotbed/internal/tailnet"
)

type staticID struct {
	peer tailnet.Peer
	uid  uint32
}

func (s staticID) WhoIs(context.Context, string) (tailnet.Peer, error) { return s.peer, nil }
func (s staticID) InTailnet(a netip.Addr) bool                         { return a.Unmap().IsLoopback() }
func (s staticID) LocalUID(netip.AddrPort, netip.AddrPort) (uint32, error) {
	return s.uid, nil
}

func genTestKey(t *testing.T) (ssh.Signer, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
}

func startShellTestEngine(t *testing.T, id httpx.Identifier, now *time.Time) (*Engine, *ShellServer, string) {
	t.Helper()
	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	units := &fake.Units{}
	eng, err := NewEngine(t.Context(), EngineConfig{
		Dir:       dir,
		NodeID:    "n1",
		Host:      "worker1",
		Store:     store,
		BaseImage: fake.BaseImage(store),
		Units:     units,
		System:    fake.NewSystem(),
		Now:       func() time.Time { return *now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })

	// Create a warm base and a ready workspace with repo "app".
	wsID := "ws-shell-1"
	vol := "ws/" + wsID
	if err := store.Create(t.Context(), vol); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(store.Path(vol), "work", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Path(vol), "work", "app", "hello.txt"), []byte("from-workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng.mu.Lock()
	eng.workspaces[wsID] = &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{
			Id:        wsID,
			Project:   "app",
			NodeId:    "n1",
			SessionId: "brave-otter",
			State:     idle,
			Repos:     []*pb.WorkspaceRepo{{Repo: "app"}},
		},
		Volume: vol,
	}}
	eng.mu.Unlock()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	eng.SetShellListen(addr.String(), addr.Port)

	policy := httpx.Policy{
		Owner:      "ramon@",
		Self:       []netip.Addr{netip.MustParseAddr("127.0.0.1")},
		LocalUsers: []uint32{1000},
	}
	srv := NewShellServer(id, policy, eng)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	return eng, srv, wsID
}

func dialShellSSH(t *testing.T, resp *pb.OpenShellResponse, signer ssh.Signer) (*ssh.Client, error) {
	t.Helper()
	return dialShellSSHOver(t, resp, signer, func(c net.Conn) net.Conn { return c })
}

// dialShellSSHOver is dialShellSSH with the client's TCP connection passed
// through wrap, so a test can interpose on what the client reads.
func dialShellSSHOver(t *testing.T, resp *pb.OpenShellResponse, signer ssh.Signer, wrap func(net.Conn) net.Conn) (*ssh.Client, error) {
	t.Helper()
	hostPub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(resp.HostPublicKey))
	if err != nil {
		t.Fatalf("parse host pubkey: %v", err)
	}
	cfg := &ssh.ClientConfig{
		User:            resp.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.FixedHostKey(hostPub),
		Timeout:         5 * time.Second,
	}
	raw, err := net.DialTimeout("tcp", resp.Addr, cfg.Timeout)
	if err != nil {
		return nil, err
	}
	conn := wrap(raw)
	c, chans, reqs, err := ssh.NewClientConn(conn, resp.Addr, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return ssh.NewClient(c, chans, reqs), nil
}

// stallingConn delays what the client reads while stalled: bytes the server
// sent sit in the client unprocessed, as with an ssh client starved of CPU.
// Writes pass through.
type stallingConn struct {
	net.Conn
	mu    sync.Mutex
	stall chan struct{} // non-nil while stalled; closed to resume
}

func (c *stallingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	stall := c.stall
	c.mu.Unlock()
	if stall != nil {
		<-stall
	}
	return n, err
}

func (c *stallingConn) hold() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stall = make(chan struct{})
}

func (c *stallingConn) resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	close(c.stall)
	c.stall = nil
}

func TestShellPTYResizeAndInteractive(t *testing.T) {
	now := time.Now()
	id := staticID{peer: tailnet.Peer{NodeID: "nLaptop", Hostname: "laptop", Login: "ramon@"}, uid: 1000}
	eng, _, wsID := startShellTestEngine(t, id, &now)

	signer, pub := genTestKey(t)
	resp, err := eng.OpenShell(t.Context(), &pb.OpenShellRequest{
		WorkspaceId: wsID,
		Repo:        "app",
		Env:         []string{"MY_VAR=hello-pty"},
		PublicKey:   pub,
	})
	if err != nil {
		t.Fatal(err)
	}

	client, err := dialShellSSH(t, resp, signer)
	if err != nil {
		t.Fatalf("ssh dial: %v", err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	if err := sess.RequestPty("xterm-256color", 33, 111, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	sess.Stdout = &out
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}

	// Verify active shell keeps the session alive in heartbeats.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		hb := eng.Heartbeat("c1")
		if slices.Contains(hb.ActiveSessions, "brave-otter") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := sess.WindowChange(44, 123); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(stdin, "cat hello.txt; echo VAR=$MY_VAR; echo TERM=$TERM; stty size; exit 0\n")
	if err := sess.Wait(); err != nil {
		t.Fatalf("shell wait: %v (out=%q)", err, out.String())
	}

	got := strings.ReplaceAll(out.String(), "\r\n", "\n")
	for _, want := range []string{"from-workspace", "VAR=hello-pty", "TERM=xterm-256color", "44 123"} {
		if !strings.Contains(got, want) {
			t.Errorf("shell output missing %q:\n%s", want, got)
		}
	}
}

func TestShellSecurityTicketsAndGate(t *testing.T) {
	now := time.Now()
	id := staticID{peer: tailnet.Peer{NodeID: "nLaptop", Hostname: "laptop", Login: "ramon@"}, uid: 1000}
	eng, _, wsID := startShellTestEngine(t, id, &now)

	signer1, pub1 := genTestKey(t)
	signer2, _ := genTestKey(t)

	// 1. Wrong public key is rejected.
	resp, err := eng.OpenShell(t.Context(), &pb.OpenShellRequest{
		WorkspaceId: wsID,
		Args:        []string{"echo", "ok"},
		PublicKey:   pub1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if c, err := dialShellSSH(t, resp, signer2); err == nil {
		c.Close()
		t.Fatal("ssh accepted wrong client key")
	}

	// 2. Valid key succeeds once, and ticket replay is rejected.
	c, err := dialShellSSH(t, resp, signer1)
	if err != nil {
		t.Fatalf("ssh with right key failed: %v", err)
	}
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	out, err := sess.CombinedOutput("")
	if err != nil {
		t.Fatalf("exec failed: %v (%s)", err, out)
	}
	if strings.TrimSpace(string(out)) != "ok" {
		t.Errorf("output = %q, want ok", out)
	}
	sess.Close()
	c.Close()

	if c2, err := dialShellSSH(t, resp, signer1); err == nil {
		c2.Close()
		t.Fatal("replayed ticket succeeded, want rejection")
	}

	// 3. Expired ticket is rejected.
	respExp, err := eng.OpenShell(t.Context(), &pb.OpenShellRequest{
		WorkspaceId: wsID,
		PublicKey:   pub1,
	})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(shellTicketTTL + time.Second)
	if c3, err := dialShellSSH(t, respExp, signer1); err == nil {
		c3.Close()
		t.Fatal("expired ticket succeeded, want rejection")
	}

	// 4. Unauthorized tailnet peer is rejected at the TCP gate before SSH auth.
	badID := staticID{peer: tailnet.Peer{NodeID: "nMallory", Hostname: "mallory", Login: "mallory@"}, uid: 1000}
	engBad, _, wsBad := startShellTestEngine(t, badID, &now)
	respBad, err := engBad.OpenShell(t.Context(), &pb.OpenShellRequest{
		WorkspaceId: wsBad,
		PublicKey:   pub1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if c4, err := dialShellSSH(t, respBad, signer1); err == nil {
		c4.Close()
		t.Fatal("unauthorized tailnet peer connected to SSH server")
	}
}

func TestShellValidationAndReleaseStopsShell(t *testing.T) {
	now := time.Now()
	id := staticID{peer: tailnet.Peer{NodeID: "nLaptop", Hostname: "laptop", Login: "ramon@"}, uid: 1000}
	eng, _, wsID := startShellTestEngine(t, id, &now)

	signer, pub := genTestKey(t)
	if _, err := eng.OpenShell(t.Context(), &pb.OpenShellRequest{WorkspaceId: wsID, PublicKey: "not-a-key"}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Errorf("bad key err = %v, want INVALID_ARGS", err)
	}
	if _, err := eng.OpenShell(t.Context(), &pb.OpenShellRequest{WorkspaceId: wsID, Repo: "missing", PublicKey: pub}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Errorf("missing repo err = %v, want INVALID_ARGS", err)
	}
	if _, err := eng.OpenShell(t.Context(), &pb.OpenShellRequest{WorkspaceId: wsID, Env: []string{"NOEQUALS"}, PublicKey: pub}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Errorf("bad env err = %v, want INVALID_ARGS", err)
	}

	// Open a long-running shell, then release the workspace; ReleaseWorkspace
	// must cancel the shell and succeed cleanly.
	resp, err := eng.OpenShell(t.Context(), &pb.OpenShellRequest{
		WorkspaceId: wsID,
		Args:        []string{"sleep", "60"},
		PublicKey:   pub,
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := dialShellSSH(t, resp, signer)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}

	// Wait until the engine records the active shell.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		eng.mu.Lock()
		n := len(eng.shells)
		eng.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := eng.ReleaseWorkspace(t.Context(), wsID); err != nil {
		t.Fatalf("ReleaseWorkspace with active shell: %v", err)
	}
	if err := sess.Wait(); err == nil {
		t.Error("expected killed shell to exit non-zero")
	}
}

// After sending exit-status and closing the session channel, ShellServer must
// wait for the client to disconnect cleanly instead of closing sconn/TCP
// immediately (which races OpenSSH's SSH_MSG_DISCONNECT and causes exit 255
// "Connection reset by peer" / "Broken pipe": board 8025, board 8316).
func TestShellTeardownWaitsForClientDisconnect(t *testing.T) {
	now := time.Now()
	id := staticID{peer: tailnet.Peer{NodeID: "nLaptop", Hostname: "laptop", Login: "ramon@"}, uid: 1000}

	t.Run("exec waits for client disconnect before closing transport", func(t *testing.T) {
		eng, srv, wsID := startShellTestEngine(t, id, &now)
		signer, pub := genTestKey(t)
		resp, err := eng.OpenShell(t.Context(), &pb.OpenShellRequest{
			WorkspaceId: wsID,
			Args:        []string{"echo", "clean-teardown"},
			PublicKey:   pub,
		})
		if err != nil {
			t.Fatal(err)
		}

		client, err := dialShellSSH(t, resp, signer)
		if err != nil {
			t.Fatalf("ssh dial: %v", err)
		}
		defer client.Close()

		sess, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		out, err := sess.CombinedOutput("")
		if err != nil {
			t.Fatalf("exec failed: %v (%s)", err, out)
		}
		if strings.TrimSpace(string(out)) != "clean-teardown" {
			t.Fatalf("output = %q, want clean-teardown", out)
		}
		_ = sess.Close()

		// Give an immediate-close server plenty of time to tear down sconn/TCP.
		// With the fix, the server keeps sconn alive until the client disconnects
		// (up to shellDisconnectGrace), so a global request still gets a clean
		// SSH reply instead of a transport error.
		time.Sleep(50 * time.Millisecond)
		if _, _, err := client.SendRequest("keepalive@hotbed", true, nil); err != nil {
			t.Fatalf("server closed SSH transport before client disconnected: %v", err)
		}

		// Once the client disconnects cleanly, the server releases the connection
		// promptly rather than waiting for the full disconnect grace timeout.
		if err := client.Close(); err != nil {
			t.Fatalf("client close: %v", err)
		}
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			srv.mu.Lock()
			n := len(srv.conns)
			srv.mu.Unlock()
			if n == 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("server did not release connection promptly after client disconnect")
	})

	t.Run("pty waits for client disconnect before closing transport", func(t *testing.T) {
		eng, _, wsID := startShellTestEngine(t, id, &now)
		signer, pub := genTestKey(t)
		resp, err := eng.OpenShell(t.Context(), &pb.OpenShellRequest{
			WorkspaceId: wsID,
			Args:        []string{"sh", "-c", "echo pty-clean"},
			PublicKey:   pub,
		})
		if err != nil {
			t.Fatal(err)
		}

		client, err := dialShellSSH(t, resp, signer)
		if err != nil {
			t.Fatalf("ssh dial: %v", err)
		}
		defer client.Close()

		sess, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{}); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		sess.Stdout = &out
		if err := sess.Shell(); err != nil {
			t.Fatal(err)
		}
		if err := sess.Wait(); err != nil {
			t.Fatalf("pty wait: %v (out=%q)", err, out.String())
		}
		_ = sess.Close()
		if !strings.Contains(out.String(), "pty-clean") {
			t.Fatalf("pty output = %q, want pty-clean", out.String())
		}

		time.Sleep(50 * time.Millisecond)
		if _, _, err := client.SendRequest("keepalive@hotbed", true, nil); err != nil {
			t.Fatalf("server closed PTY SSH transport before client disconnected: %v", err)
		}
	})

	// The client is slow to read what ends the session (its EOF, exit-status
	// and CHANNEL_CLOSE) for far longer than the disconnect grace. The server
	// must keep the transport until the client answers with its own
	// CHANNEL_CLOSE, so the client still gets the exit status and can say
	// goodbye on a live connection (board 9208).
	t.Run("slow client reader keeps the transport until it closes the channel", func(t *testing.T) {
		eng, srv, wsID := startShellTestEngine(t, id, &now)
		srv.disconnectGrace = time.Millisecond

		signer, pub := genTestKey(t)
		resp, err := eng.OpenShell(t.Context(), &pb.OpenShellRequest{
			WorkspaceId: wsID,
			// Wait for stdin so the session can only end after the client
			// has stopped reading.
			Args:      []string{"sh", "-c", `read l; echo "got $l"; exit 3`},
			PublicKey: pub,
		})
		if err != nil {
			t.Fatal(err)
		}
		var conn *stallingConn
		client, err := dialShellSSHOver(t, resp, signer, func(c net.Conn) net.Conn {
			conn = &stallingConn{Conn: c}
			return conn
		})
		if err != nil {
			t.Fatalf("ssh dial: %v", err)
		}
		defer client.Close()

		sess, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		sess.Stdout = &out
		stdin, err := sess.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.Start(""); err != nil {
			t.Fatal(err)
		}

		conn.hold()
		if _, err := io.WriteString(stdin, "line\n"); err != nil {
			t.Fatal(err)
		}
		if err := stdin.Close(); err != nil {
			t.Fatal(err)
		}

		// The shell has exited and the server has cleaned it up: everything
		// that ends the session is on its way to the stalled client.
		deadline := time.Now().Add(10 * time.Second)
		for {
			eng.mu.Lock()
			n := len(eng.shells)
			eng.mu.Unlock()
			if n == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("shell did not exit")
			}
			time.Sleep(10 * time.Millisecond)
		}
		// Stay stalled for many disconnect graces.
		time.Sleep(200 * time.Millisecond)
		srv.mu.Lock()
		open := len(srv.conns)
		srv.mu.Unlock()
		if open != 1 {
			t.Fatalf("server closed the connection while the client had not yet read the session's end (%d open)", open)
		}

		conn.resume()
		var exitErr *ssh.ExitError
		if err := sess.Wait(); !errors.As(err, &exitErr) || exitErr.ExitStatus() != 3 {
			t.Fatalf("Wait() = %v, want exit status 3", err)
		}
		if out.String() != "got line\n" {
			t.Fatalf("output = %q, want %q", out.String(), "got line\n")
		}
	})

	t.Run("grace timeout closes stalled client", func(t *testing.T) {
		eng, srv, wsID := startShellTestEngine(t, id, &now)
		srv.disconnectGrace = 100 * time.Millisecond

		signer, pub := genTestKey(t)
		resp, err := eng.OpenShell(t.Context(), &pb.OpenShellRequest{
			WorkspaceId: wsID,
			Args:        []string{"true"},
			PublicKey:   pub,
		})
		if err != nil {
			t.Fatal(err)
		}

		client, err := dialShellSSH(t, resp, signer)
		if err != nil {
			t.Fatalf("ssh dial: %v", err)
		}
		defer client.Close()

		sess, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.Run(""); err != nil {
			t.Fatalf("run: %v", err)
		}
		_ = sess.Close()

		// Deliberately keep client open; server must close after disconnectGrace.
		waitCh := make(chan error, 1)
		go func() { waitCh <- client.Wait() }()
		select {
		case <-waitCh:
		case <-time.After(1 * time.Second):
			t.Fatal("server did not close stalled client connection after disconnectGrace")
		}
	})
}

// A throwaway workspace holds its one command and nothing else: no shell
// opens in it.
func TestShellRefusesAThrowawayWorkspace(t *testing.T) {
	now := time.Now()
	id := staticID{peer: tailnet.Peer{NodeID: "nLaptop", Hostname: "laptop", Login: "ramon@"}, uid: 1000}
	eng, _, wsID := startShellTestEngine(t, id, &now)
	eng.mu.Lock()
	eng.workspaces[wsID].ws().Throwaway = true
	eng.mu.Unlock()

	_, pub := genTestKey(t)
	_, err := eng.OpenShell(t.Context(), &pb.OpenShellRequest{WorkspaceId: wsID, PublicKey: pub})
	want := "workspace " + wsID + " is throwaway: it holds one command, and goes when that ends"
	if e := api.FromConnect(err); api.CodeOf(err) != api.CodeConflict || e.Message != want {
		t.Errorf("shell in a throwaway workspace = %v; want CONFLICT %q", err, want)
	}
}
