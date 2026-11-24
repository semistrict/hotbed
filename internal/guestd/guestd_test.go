package guestd_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/guestd"
	"github.com/semistrict/hotbed/internal/unit"
)

func startTestGuestd(t *testing.T, workDir string) (*guestd.Client, *guestd.Server) {
	t.Helper()
	// Keep socket paths short (<104 bytes on macOS).
	sockDir, err := os.MkdirTemp("", "gd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })

	stateDir := t.TempDir()
	srv, err := guestd.NewServer(guestd.ServerConfig{
		WorkDir:  workDir,
		StateDir: stateDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	paths := map[uint32]string{}
	listen := func(port uint32) net.Listener {
		p := filepath.Join(sockDir, fmt.Sprintf("p%d.sock", port))
		ln, err := net.Listen("unix", p)
		if err != nil {
			t.Fatal(err)
		}
		paths[port] = p
		t.Cleanup(func() { _ = ln.Close() })
		return ln
	}

	ctrlLn := listen(guestd.ControlPort)
	shellLn := listen(guestd.ShellPort)
	tcpLn := listen(guestd.TCPProxyPort)

	go func() { _ = srv.ServeControl(ctx, ctrlLn) }()
	go func() { _ = srv.ServeShell(ctx, shellLn) }()
	go func() { _ = srv.ServeTCPProxy(ctx, tcpLn) }()

	client := guestd.NewClient(guestd.UnixMapDialer(paths))
	if _, err := client.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	return client, srv
}

func TestHealthAndCommandLifecycle(t *testing.T) {
	workDir := t.TempDir()
	client, _ := startTestGuestd(t, workDir)
	ctx := context.Background()

	h, err := client.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.WorkDir != workDir || h.Arch == "" {
		t.Fatalf("unexpected health: %+v", h)
	}

	// Start a short command and verify idempotency on the same ID.
	st1, err := client.StartCommand(ctx, guestd.StartCommandRequest{
		ID:   "cmd-echo-1",
		Argv: []string{"/bin/sh", "-c", "echo hello-guestd; echo err-line >&2; exit 7"},
		Dir:  workDir,
	})
	if err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	st2, err := client.StartCommand(ctx, guestd.StartCommandRequest{
		ID:   "cmd-echo-1",
		Argv: []string{"/bin/sh", "-c", "echo should-not-run"},
		Dir:  workDir,
	})
	if err != nil {
		t.Fatalf("idempotent StartCommand: %v", err)
	}
	if st2.MainPID != st1.MainPID {
		t.Fatalf("idempotent start spawned new pid: %d vs %d", st2.MainPID, st1.MainPID)
	}

	var buf bytes.Buffer
	if _, err := client.StreamLogs(ctx, "cmd-echo-1", 0, true, &buf); err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	if !strings.Contains(buf.String(), "hello-guestd") || !strings.Contains(buf.String(), "err-line") {
		t.Fatalf("unexpected log output: %q", buf.String())
	}

	// StreamLogs from an offset returns only the tail.
	var tail bytes.Buffer
	if _, err := client.StreamLogs(ctx, "cmd-echo-1", 6, false, &tail); err != nil {
		t.Fatalf("StreamLogs offset: %v", err)
	}
	if tail.String() != buf.String()[6:] {
		t.Fatalf("offset log mismatch: got %q want %q", tail.String(), buf.String()[6:])
	}

	sts, err := client.StatusMany(ctx, []string{"cmd-echo-1"})
	if err != nil {
		t.Fatalf("StatusMany: %v", err)
	}
	got := sts["cmd-echo-1"]
	if got.State != guestd.StateExited || got.ExitCode != 7 {
		t.Fatalf("expected exited/7, got %+v", got)
	}
}

func TestFreezeThawAndStopCommand(t *testing.T) {
	workDir := t.TempDir()
	client, _ := startTestGuestd(t, workDir)
	ctx := context.Background()

	_, err := client.StartCommand(ctx, guestd.StartCommandRequest{
		ID:   "cmd-sleep-1",
		Argv: []string{"/bin/sh", "-c", "while true; do sleep 0.1; done"},
		Dir:  workDir,
	})
	if err != nil {
		t.Fatalf("StartCommand: %v", err)
	}

	st, err := client.FreezeCommand(ctx, "cmd-sleep-1")
	if err != nil {
		t.Fatalf("FreezeCommand: %v", err)
	}
	if st.State != guestd.StateFrozen {
		t.Fatalf("expected frozen, got %s", st.State)
	}

	st, err = client.ThawCommand(ctx, "cmd-sleep-1")
	if err != nil {
		t.Fatalf("ThawCommand: %v", err)
	}
	if st.State != guestd.StateRunning {
		t.Fatalf("expected running after thaw, got %s", st.State)
	}

	// Freeze again, then Stop must thaw + terminate cleanly.
	if _, err := client.FreezeCommand(ctx, "cmd-sleep-1"); err != nil {
		t.Fatalf("second FreezeCommand: %v", err)
	}
	st, err = client.StopCommand(ctx, "cmd-sleep-1", 500*time.Millisecond)
	if err != nil {
		t.Fatalf("StopCommand: %v", err)
	}
	if st.State != guestd.StateExited {
		t.Fatalf("expected exited after stop, got %+v", st)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
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
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestSyncAndFiles(t *testing.T) {
	workDir := t.TempDir()
	client, _ := startTestGuestd(t, workDir)
	ctx := context.Background()

	// Create a source repo and clone it into workDir/demo as the workspace base.
	srcRepo := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(srcRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, srcRepo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(srcRepo, "a.txt"), []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, srcRepo, "add", "a.txt")
	runGit(t, srcRepo, "commit", "-m", "init")
	baseCommit := runGit(t, srcRepo, "rev-parse", "HEAD")

	wsRepo := filepath.Join(workDir, "demo")
	runGit(t, workDir, "clone", srcRepo, wsRepo)

	// Make dirty edits in srcRepo, snapshot & bundle them via gitops.
	if err := os.WriteFile(filepath.Join(srcRepo, "a.txt"), []byte("line1\nline2-edited\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcRepo, "new.txt"), []byte("brand new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap, err := gitops.Snapshot(ctx, srcRepo)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	bundlePath := filepath.Join(t.TempDir(), "sync.bundle")
	bf, err := os.Create(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gitops.Bundle(ctx, snap, baseCommit, bf); err != nil {
		_ = bf.Close()
		t.Fatalf("Bundle: %v", err)
	}
	_ = bf.Close()

	changed, err := client.ApplySync(ctx, "demo", bundlePath, snap.Commit, baseCommit, nil, nil, 0, 0)
	if err != nil {
		t.Fatalf("ApplySync: %v", err)
	}
	if changed < 2 {
		t.Fatalf("expected >= 2 changed files, got %d", changed)
	}

	files, err := client.ListFiles(ctx, []guestd.RepoBase{{Repo: "demo", BaseCommit: baseCommit}})
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	byPath := map[string]string{}
	for _, f := range files {
		byPath[f.Path] = f.Change
	}
	if byPath["demo/a.txt"] != "modified" || byPath["demo/new.txt"] != "added" {
		t.Fatalf("unexpected file changes: %+v", byPath)
	}

	fc, err := client.GetFile(ctx, "demo", baseCommit, "a.txt")
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if fc.Text != "line1\nline2-edited\nline3\n" {
		t.Fatalf("unexpected file text: %q", fc.Text)
	}
	if len(fc.ChangedLines) != 2 || fc.ChangedLines[0] != 2 || fc.ChangedLines[1] != 3 {
		t.Fatalf("unexpected changed lines: %v", fc.ChangedLines)
	}

	// Path traversal outside repo must be refused.
	if _, err := client.GetFile(ctx, "demo", baseCommit, "../etc/passwd"); err == nil {
		t.Fatal("expected traversal error, got nil")
	}
}

func TestShellPipedAndPTY(t *testing.T) {
	workDir := t.TempDir()
	client, _ := startTestGuestd(t, workDir)
	ctx := context.Background()

	// Piped (non-PTY) shell verifies stdin, stdout, stderr, and exit code.
	var stdout, stderr bytes.Buffer
	h, err := client.StartShell(ctx, guestd.ShellOpenRequest{
		Argv: []string{"/bin/sh", "-c", "read line; echo out:$line; echo err:$line >&2; exit 3"},
		Dir:  workDir,
	}, unit.ShellIO{
		PTY:    false,
		Stdin:  strings.NewReader("ping\n"),
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("StartShell piped: %v", err)
	}
	code := h.Wait()
	_ = h.Close()
	if code != 3 {
		t.Fatalf("piped exit code: got %d want 3", code)
	}
	if strings.TrimSpace(stdout.String()) != "out:ping" || strings.TrimSpace(stderr.String()) != "err:ping" {
		t.Fatalf("piped io mismatch: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	// PTY shell verifies interactive PTY allocation, Resize, I/O, and exit.
	ph, err := client.StartShell(ctx, guestd.ShellOpenRequest{
		Argv: []string{"/bin/sh", "-c", "stty -echo; read x; echo pty:$x; exit 0"},
		Dir:  workDir,
	}, unit.ShellIO{
		PTY:  true,
		Cols: 100,
		Rows: 30,
		Term: "xterm-256color",
	})
	if err != nil {
		t.Fatalf("StartShell PTY: %v", err)
	}
	if ph.PTY() == nil {
		t.Fatal("expected non-nil PTY() file")
	}
	if err := ph.Resize(120, 40); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	var ptyOut bytes.Buffer
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		_, _ = io.Copy(&ptyOut, ph.PTY())
	}()
	if _, err := ph.PTY().Write([]byte("hello-pty\n")); err != nil {
		t.Fatalf("write pty: %v", err)
	}
	if code := ph.Wait(); code != 0 {
		t.Fatalf("pty exit code: got %d want 0 (out=%q)", code, ptyOut.String())
	}
	<-outDone
	_ = ph.Close()
	if !strings.Contains(ptyOut.String(), "pty:hello-pty") {
		t.Fatalf("unexpected PTY output: %q", ptyOut.String())
	}
}

func TestTCPProxyAndGitAuthBridge(t *testing.T) {
	workDir := t.TempDir()
	client, _ := startTestGuestd(t, workDir)
	ctx := context.Background()

	// Spin up a local HTTP server (simulating a guest dev server) and reach it via DialTCP.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("dev-server-ok"))
	}))
	defer ts.Close()
	port := uint16(ts.Listener.Addr().(*net.TCPAddr).Port)

	hc := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return client.DialTCP(ctx, port)
			},
		},
	}
	resp, err := hc.Get("http://guest/")
	if err != nil {
		t.Fatalf("DialTCP GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "dev-server-ok" {
		t.Fatalf("unexpected proxied body: %q", string(body))
	}

	// Verify StartGitAuthBridge forwards local Unix socket connections to host dialer.
	sockDir, err := os.MkdirTemp("", "ga-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	hostSock := filepath.Join(sockDir, "host.sock")
	hostLn, err := net.Listen("unix", hostSock)
	if err != nil {
		t.Fatal(err)
	}
	defer hostLn.Close()
	go func() {
		for {
			c, err := hostLn.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = conn.Write([]byte("git-auth-ok"))
			}(c)
		}
	}()

	guestSock := filepath.Join(sockDir, "guest-cred.sock")
	bctx, bcancel := context.WithCancel(ctx)
	defer bcancel()
	go func() {
		_ = guestd.StartGitAuthBridge(bctx, guestSock, func() (net.Conn, error) {
			return net.Dial("unix", hostSock)
		})
	}()
	// Wait for guestSock to appear.
	var gc net.Conn
	for range 40 {
		gc, err = net.Dial("unix", guestSock)
		if err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dial guest git-auth socket: %v", err)
	}
	defer gc.Close()
	got, _ := io.ReadAll(gc)
	if string(got) != "git-auth-ok" {
		t.Fatalf("git auth bridge mismatch: got %q", string(got))
	}
}

func TestFirecrackerVsockHandshake(t *testing.T) {
	sockDir, err := os.MkdirTemp("", "fc-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	uds := filepath.Join(sockDir, "v.sock")
	ln, err := net.Listen("unix", uds)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				br := bufio.NewReader(conn)
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if line == "CONNECT 5000\n" {
					_, _ = conn.Write([]byte("OK 10042\nhello-vsock"))
				} else {
					_, _ = conn.Write([]byte("ERR refused\n"))
				}
			}(c)
		}
	}()

	dial := guestd.FirecrackerVsockDialer(uds)
	conn, err := dial(context.Background(), 5000)
	if err != nil {
		t.Fatalf("FirecrackerVsockDialer(5000): %v", err)
	}
	data, _ := io.ReadAll(conn)
	_ = conn.Close()
	if string(data) != "hello-vsock" {
		t.Fatalf("unexpected buffered payload: %q", string(data))
	}

	if _, err := dial(context.Background(), 9999); err == nil {
		t.Fatal("expected refusal for port 9999")
	}
}

func TestReclaimEndpoint(t *testing.T) {
	workDir := t.TempDir()
	client, _ := startTestGuestd(t, workDir)
	ctx := context.Background()

	// Whole-VM reclaim (empty ID) succeeds (best-effort drop_caches).
	if err := client.Reclaim(ctx, "", 0); err != nil {
		t.Fatalf("Reclaim whole VM: %v", err)
	}

	// Reclaim against a live command ID succeeds.
	if _, err := client.StartCommand(ctx, guestd.StartCommandRequest{
		ID:   "cmd-reclaim-1",
		Argv: []string{"/bin/sh", "-c", "sleep 30"},
		Dir:  workDir,
	}); err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	t.Cleanup(func() { _, _ = client.StopCommand(context.Background(), "cmd-reclaim-1", 500*time.Millisecond) })

	if err := client.Reclaim(ctx, "cmd-reclaim-1", 64<<20); err != nil {
		t.Fatalf("Reclaim command: %v", err)
	}
}
