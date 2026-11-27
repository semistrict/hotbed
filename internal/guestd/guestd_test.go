package guestd_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/fake"
	guestdv1 "github.com/semistrict/hotbed/internal/gen/guestd/v1"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/guestd"
	"github.com/semistrict/hotbed/internal/inspect"
	"github.com/semistrict/hotbed/internal/profiler"
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
	st1, err := client.StartCommand(ctx, &guestdv1.StartCommandRequest{
		Id:   "cmd-echo-1",
		Argv: []string{"/bin/sh", "-c", "echo hello-guestd; echo err-line >&2; exit 7"},
		Dir:  workDir,
	})
	if err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	st2, err := client.StartCommand(ctx, &guestdv1.StartCommandRequest{
		Id:   "cmd-echo-1",
		Argv: []string{"/bin/sh", "-c", "echo should-not-run"},
		Dir:  workDir,
	})
	if err != nil {
		t.Fatalf("idempotent StartCommand: %v", err)
	}
	if st2.MainPid != st1.MainPid {
		t.Fatalf("idempotent start spawned new pid: %d vs %d", st2.MainPid, st1.MainPid)
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
	if got.State != guestdv1.CommandState_COMMAND_STATE_EXITED || got.ExitCode != 7 {
		t.Fatalf("expected exited/7, got %+v", got)
	}

	// Unknown command returns Connect CodeNotFound.
	if _, err := client.StopCommand(ctx, "does-not-exist", time.Second); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("expected CodeNotFound for unknown command, got %v", err)
	}
}

func TestFreezeThawAndStopCommand(t *testing.T) {
	workDir := t.TempDir()
	client, _ := startTestGuestd(t, workDir)
	ctx := context.Background()

	_, err := client.StartCommand(ctx, &guestdv1.StartCommandRequest{
		Id:   "cmd-sleep-1",
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
	if st.State != guestdv1.CommandState_COMMAND_STATE_FROZEN {
		t.Fatalf("expected frozen, got %s", st.State)
	}

	st, err = client.ThawCommand(ctx, "cmd-sleep-1")
	if err != nil {
		t.Fatalf("ThawCommand: %v", err)
	}
	if st.State != guestdv1.CommandState_COMMAND_STATE_RUNNING {
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
	if st.State != guestdv1.CommandState_COMMAND_STATE_EXITED {
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

	files, err := client.ListFiles(ctx, []*guestdv1.RepoBase{{Repo: "demo", BaseCommit: baseCommit}})
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

	// Path traversal outside repo must be refused with InvalidArgument.
	if _, err := client.GetFile(ctx, "demo", baseCommit, "../etc/passwd"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("expected CodeInvalidArgument for traversal, got %v", err)
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
	if _, err := client.StartCommand(ctx, &guestdv1.StartCommandRequest{
		Id:   "cmd-reclaim-1",
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

type failingStreamInspector struct {
	inspect.Inspector
	traceErr error
}

func (f failingStreamInspector) Trace(_ context.Context, _ inspect.Target, _ *pb.TraceCommandRequest, emit inspect.Emit) error {
	if err := emit(&pb.TraceCommandChunk{Status: "starting"}); err != nil {
		return err
	}
	return f.traceErr
}

func startTestGuestdWithInspector(t *testing.T, workDir string, insp inspect.Inspector) *guestd.Client {
	t.Helper()
	sockDir, err := os.MkdirTemp("", "gdi-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })

	srv, err := guestd.NewServer(guestd.ServerConfig{
		WorkDir:   workDir,
		StateDir:  t.TempDir(),
		Inspector: insp,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ctrlSock := filepath.Join(sockDir, "ctrl.sock")
	ctrlLn, err := net.Listen("unix", ctrlSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctrlLn.Close() })
	go func() { _ = srv.ServeControl(ctx, ctrlLn) }()

	client := guestd.NewClient(guestd.UnixMapDialer(map[uint32]string{
		guestd.ControlPort: ctrlSock,
	}))
	if _, err := client.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	return client
}

func TestInspectTraceAndSampleEndpoints(t *testing.T) {
	ctx := context.Background()
	workDir := t.TempDir()
	client := startTestGuestdWithInspector(t, workDir, fake.Inspector{})

	// Unknown command returns structured NOT_FOUND api.Error (not older-guestd 404).
	_, err := client.InspectCommand(ctx, &pb.InspectCommandRequest{Id: "cmd-missing"})
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.CodeNotFound {
		t.Fatalf("InspectCommand(missing) err = %v, want api.Error NOT_FOUND", err)
	}

	// Start a live command and inspect, trace, and sample it through guestd.
	if _, err := client.StartCommand(ctx, &guestdv1.StartCommandRequest{
		Id:   "cmd-diag-1",
		Argv: []string{"/bin/sh", "-c", "sleep 30"},
		Dir:  workDir,
	}); err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	t.Cleanup(func() { _, _ = client.StopCommand(context.Background(), "cmd-diag-1", 500*time.Millisecond) })

	ins, err := client.InspectCommand(ctx, &pb.InspectCommandRequest{Id: "cmd-diag-1"})
	if err != nil {
		t.Fatalf("InspectCommand: %v", err)
	}
	if ins.CommandId != "cmd-diag-1" || ins.UnitName != "hotbed-cmd-diag-1" || len(ins.Procs) == 0 {
		t.Fatalf("unexpected InspectCommandResponse: id=%q unit=%q procs=%d", ins.CommandId, ins.UnitName, len(ins.Procs))
	}

	var traceChunks []*pb.TraceCommandChunk
	if err := client.TraceCommand(ctx, &pb.TraceCommandRequest{
		Id:   "cmd-diag-1",
		Kind: pb.TraceKind_TRACE_KIND_STACKS,
	}, func(ch *pb.TraceCommandChunk) error {
		traceChunks = append(traceChunks, ch)
		return nil
	}); err != nil {
		t.Fatalf("TraceCommand: %v", err)
	}
	if len(traceChunks) == 0 || !traceChunks[len(traceChunks)-1].Done || traceChunks[len(traceChunks)-1].Summary == nil {
		t.Fatalf("unexpected trace chunks: %+v", traceChunks)
	}

	var sample *pb.ProcessSample
	if err := client.SampleCommand(ctx, &pb.SampleCommandRequest{
		Id:              "cmd-diag-1",
		DurationSeconds: 1,
	}, func(ch *pb.SampleCommandChunk) error {
		if ch.Sample != nil {
			sample = ch.Sample
		}
		return nil
	}); err != nil {
		t.Fatalf("SampleCommand: %v", err)
	}
	if sample == nil || sample.CommandId != "cmd-diag-1" || len(sample.Procs) == 0 {
		t.Fatalf("unexpected ProcessSample: %+v", sample)
	}

	// Stop the command and verify ended commands fail with CONFLICT.
	if _, err := client.StopCommand(ctx, "cmd-diag-1", 500*time.Millisecond); err != nil {
		t.Fatalf("StopCommand: %v", err)
	}
	_, err = client.InspectCommand(ctx, &pb.InspectCommandRequest{Id: "cmd-diag-1"})
	ae = nil
	if !errors.As(err, &ae) || ae.Code != api.CodeConflict {
		t.Fatalf("InspectCommand(exited) err = %v, want api.Error CONFLICT", err)
	}

	// Mid-stream api.Error preserves code and suggestion across stream framing.
	wantErr := api.Errorf(api.CodePlatformNotSupported, "bpftrace not installed").WithSuggestion("rebuild image")
	errClient := startTestGuestdWithInspector(t, workDir, failingStreamInspector{
		Inspector: fake.Inspector{},
		traceErr:  wantErr,
	})
	if _, err := errClient.StartCommand(ctx, &guestdv1.StartCommandRequest{
		Id:   "cmd-err-1",
		Argv: []string{"/bin/sh", "-c", "sleep 30"},
		Dir:  workDir,
	}); err != nil {
		t.Fatalf("StartCommand(errClient): %v", err)
	}
	t.Cleanup(func() { _, _ = errClient.StopCommand(context.Background(), "cmd-err-1", 500*time.Millisecond) })

	var sawStatus bool
	err = errClient.TraceCommand(ctx, &pb.TraceCommandRequest{
		Id:   "cmd-err-1",
		Kind: pb.TraceKind_TRACE_KIND_BPF_EXECSNOOP,
	}, func(ch *pb.TraceCommandChunk) error {
		if ch.Status == "starting" {
			sawStatus = true
		}
		return nil
	})
	ae = nil
	if !sawStatus || !errors.As(err, &ae) || ae.Code != api.CodePlatformNotSupported || ae.Suggestion != "rebuild image" {
		t.Fatalf("mid-stream error: sawStatus=%v err=%v", sawStatus, err)
	}

	// Plain-text 404 (older guestd without inspect endpoints) maps to PLATFORM_NOT_SUPPORTED.
	sockDir, err := os.MkdirTemp("", "gd404-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	oldSock := filepath.Join(sockDir, "old.sock")
	oldLn, err := net.Listen("unix", oldSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = oldLn.Close() })
	oldSrv := &http.Server{Handler: http.NotFoundHandler()}
	go func() { _ = oldSrv.Serve(oldLn) }()
	t.Cleanup(func() { _ = oldSrv.Close() })

	oldClient := guestd.NewClient(guestd.UnixMapDialer(map[uint32]string{guestd.ControlPort: oldSock}))
	_, err = oldClient.InspectCommand(ctx, &pb.InspectCommandRequest{Id: "cmd-old"})
	ae = nil
	if !errors.As(err, &ae) || ae.Code != api.CodePlatformNotSupported {
		t.Fatalf("older guestd 404 err = %v, want PLATFORM_NOT_SUPPORTED", err)
	}
	_, err = oldClient.CommandProfile(ctx, &pb.GetProfileRequest{
		Target: &pb.GetProfileRequest_CommandId{CommandId: "cmd-old"},
	})
	ae = nil
	if !errors.As(err, &ae) || ae.Code != api.CodePlatformNotSupported {
		t.Fatalf("older guestd profile 404 err = %v, want PLATFORM_NOT_SUPPORTED", err)
	}
	if err := oldClient.ForgetCommandProfile(ctx, "cmd-old"); err != nil {
		t.Fatalf("older guestd forget 404 should be no-op, got %v", err)
	}
}

func TestGuestdCommandProfileEndpoints(t *testing.T) {
	ctx := context.Background()
	workDir := t.TempDir()
	prof := fake.NewProfiler()

	sockDir, err := os.MkdirTemp("", "gdp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })

	srv, err := guestd.NewServer(guestd.ServerConfig{
		WorkDir:   workDir,
		StateDir:  t.TempDir(),
		Inspector: fake.Inspector{},
		Profiler:  prof,
	})
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	ctrlSock := filepath.Join(sockDir, "ctrl.sock")
	ctrlLn, err := net.Listen("unix", ctrlSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctrlLn.Close() })
	go func() { _ = srv.ServeControl(sctx, ctrlLn) }()

	client := guestd.NewClient(guestd.UnixMapDialer(map[uint32]string{guestd.ControlPort: ctrlSock}))
	if _, err := client.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	if _, err := client.StartCommand(ctx, &guestdv1.StartCommandRequest{
		Id:   "cmd-prof-1",
		Argv: []string{"/bin/sh", "-c", "sleep 30"},
		Dir:  workDir,
	}); err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	t.Cleanup(func() { _, _ = client.StopCommand(context.Background(), "cmd-prof-1", 500*time.Millisecond) })

	prof.Store.Record(profiler.Sample{
		CommandID: "cmd-prof-1",
		Timestamp: time.Now(),
		Comm:      "rustc",
		Frames: []profiler.ResolvedFrame{
			{Name: "main", FrameType: "native"},
			{Name: "compile_crate", FrameType: "native"},
		},
	})

	got, err := client.CommandProfile(ctx, &pb.GetProfileRequest{
		Target: &pb.GetProfileRequest_CommandId{CommandId: "cmd-prof-1"},
	})
	if err != nil {
		t.Fatalf("CommandProfile: %v", err)
	}
	if !got.Active || !got.Live || got.TotalSamples != 1 {
		t.Fatalf("unexpected guest CommandProfile: active=%v live=%v samples=%d", got.Active, got.Live, got.TotalSamples)
	}

	gotNode, err := client.CommandProfile(ctx, &pb.GetProfileRequest{
		Target: &pb.GetProfileRequest_Node{Node: "vm:ws-prof"},
	})
	if err != nil {
		t.Fatalf("CommandProfile(node): %v", err)
	}
	if !gotNode.Active || gotNode.Node != "vm:ws-prof" || gotNode.TotalSamples != 1 {
		t.Fatalf("unexpected guest NodeProfile: active=%v node=%q samples=%d", gotNode.Active, gotNode.Node, gotNode.TotalSamples)
	}

	if err := client.ForgetCommandProfile(ctx, "cmd-prof-1"); err != nil {
		t.Fatalf("ForgetCommandProfile: %v", err)
	}
	afterForget, err := client.CommandProfile(ctx, &pb.GetProfileRequest{
		Target: &pb.GetProfileRequest_CommandId{CommandId: "cmd-prof-1"},
	})
	if err != nil {
		t.Fatalf("CommandProfile after forget: %v", err)
	}
	if afterForget.TotalSamples != 0 {
		t.Fatalf("expected 0 samples after forget, got %d", afterForget.TotalSamples)
	}
}

func TestConnectRetriesTransientVsockReset(t *testing.T) {
	workDir := t.TempDir()
	sockDir, err := os.MkdirTemp("", "gd-retry-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })

	srv, err := guestd.NewServer(guestd.ServerConfig{
		WorkDir:  workDir,
		StateDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ctrlSock := filepath.Join(sockDir, "ctrl.sock")
	ln, err := net.Listen("unix", ctrlSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = srv.ServeControl(ctx, ln) }()

	var attempts atomic.Int32
	client := guestd.NewClient(func(ctx context.Context, port uint32) (net.Conn, error) {
		if attempts.Add(1) <= 2 {
			return nil, syscall.ECONNRESET
		}
		var d net.Dialer
		return d.DialContext(ctx, "unix", ctrlSock)
	})

	h, err := client.Health(ctx)
	if err != nil {
		t.Fatalf("Health after transient resets: %v", err)
	}
	if h.WorkDir != workDir {
		t.Fatalf("WorkDir = %q, want %q", h.WorkDir, workDir)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

func TestSyncSubmoduleStreamingAndValidation(t *testing.T) {
	workDir := t.TempDir()
	client, _ := startTestGuestd(t, workDir)
	ctx := context.Background()

	// Create a submodule repo and a parent repo referencing it.
	subOrigin := filepath.Join(t.TempDir(), "sub")
	if err := os.MkdirAll(subOrigin, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, subOrigin, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(subOrigin, "s.txt"), []byte("sub-v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, subOrigin, "add", "s.txt")
	runGit(t, subOrigin, "commit", "-m", "sub init")

	srcRepo := filepath.Join(t.TempDir(), "parent")
	if err := os.MkdirAll(srcRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, srcRepo, "init", "-b", "main")
	runGit(t, srcRepo, "-c", "protocol.file.allow=always", "submodule", "add", subOrigin, "libs/sub")
	if err := os.WriteFile(filepath.Join(srcRepo, "main.txt"), []byte("main-v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, srcRepo, "add", "-A")
	runGit(t, srcRepo, "commit", "-m", "parent init")
	baseCommit := runGit(t, srcRepo, "rev-parse", "HEAD")

	wsRepo := filepath.Join(workDir, "parent")
	runGit(t, workDir, "-c", "protocol.file.allow=always", "clone", "--recurse-submodules", srcRepo, wsRepo)

	// Edit both the submodule and the parent repo, then snapshot + bundle.
	// Advance mtime by 2s so same-length edits within the same second never hit
	// git's racy-index stat cache on coarse filesystems.
	subFile := filepath.Join(srcRepo, "libs", "sub", "s.txt")
	mainFile := filepath.Join(srcRepo, "main.txt")
	if err := os.WriteFile(subFile, []byte("sub-v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mainFile, []byte("main-v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(subFile, future, future)
	_ = os.Chtimes(mainFile, future, future)
	snap, err := gitops.Snapshot(ctx, srcRepo)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	tmpBundles := t.TempDir()
	mainBundlePath := filepath.Join(tmpBundles, "main.bundle")
	bf, err := os.Create(mainBundlePath)
	if err != nil {
		t.Fatal(err)
	}
	subBundles, err := gitops.Bundle(ctx, snap, baseCommit, bf)
	_ = bf.Close()
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	if len(subBundles) != 1 || len(subBundles[0].Data) == 0 {
		t.Fatalf("expected 1 non-empty submodule bundle, got %+v", subBundles)
	}
	subBundlePath := filepath.Join(tmpBundles, "sub-0.bundle")
	if err := os.WriteFile(subBundlePath, subBundles[0].Data, 0o644); err != nil {
		t.Fatal(err)
	}

	subs := []gitops.SubCheckout{{
		Path:   subBundles[0].Path,
		Commit: subBundles[0].Commit,
		Bundle: subBundlePath,
	}}
	changed, err := client.ApplySync(ctx, "parent", mainBundlePath, snap.Commit, baseCommit, subs, nil, 0, 0)
	if err != nil {
		t.Fatalf("ApplySync with submodule: %v", err)
	}
	if changed == 0 {
		t.Fatalf("expected changed files > 0, got %d", changed)
	}
	gotSub, err := os.ReadFile(filepath.Join(wsRepo, "libs", "sub", "s.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotSub) != "sub-v2\n" {
		t.Fatalf("submodule file = %q, want %q", string(gotSub), "sub-v2\n")
	}
	gotMain, err := os.ReadFile(filepath.Join(wsRepo, "main.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotMain) != "main-v2\n" {
		t.Fatalf("main file = %q, want %q", string(gotMain), "main-v2\n")
	}

	// Missing header on raw Connect Sync stream returns CodeInvalidArgument.
	stream, err := client.RPC().Sync(ctx)
	if err != nil {
		t.Fatalf("open Sync stream: %v", err)
	}
	_ = stream.Send(&guestdv1.SyncRequest{Msg: &guestdv1.SyncRequest_Chunk{Chunk: []byte("no-header")}})
	if _, err := stream.CloseAndReceive(); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("expected CodeInvalidArgument without header, got %v", err)
	}
}

func TestShutdownPowerOffRequiresAllowPowerOff(t *testing.T) {
	// Default ServerConfig (AllowPowerOff=false) must never invoke PowerOff,
	// preventing in-process root host tests from calling syscall.Reboot.
	var deniedCalls atomic.Int32
	srvDenied, err := guestd.NewServer(guestd.ServerConfig{
		WorkDir:  t.TempDir(),
		StateDir: t.TempDir(),
		PowerOff: func() error {
			deniedCalls.Add(1)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srvDenied.Shutdown(t.Context(), &guestdv1.ShutdownRequest{PowerOff: true}); err != nil {
		t.Fatalf("Shutdown (denied): %v", err)
	}
	time.Sleep(120 * time.Millisecond)
	if deniedCalls.Load() != 0 {
		t.Fatalf("PowerOff invoked %d times with AllowPowerOff=false, want 0", deniedCalls.Load())
	}

	// AllowPowerOff=true (used by in-VM hotbed-guestd) invokes PowerOff.
	called := make(chan struct{}, 1)
	srvAllowed, err := guestd.NewServer(guestd.ServerConfig{
		WorkDir:       t.TempDir(),
		StateDir:      t.TempDir(),
		AllowPowerOff: true,
		PowerOff: func() error {
			select {
			case called <- struct{}{}:
			default:
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srvAllowed.Shutdown(t.Context(), &guestdv1.ShutdownRequest{PowerOff: true}); err != nil {
		t.Fatalf("Shutdown (allowed): %v", err)
	}
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("PowerOff was not invoked with AllowPowerOff=true")
	}
}

// Input typed into a command's terminal while it exits either reaches the
// terminal or is refused as sent to an exited command: the terminal's close
// never races a write to it (run with -race).
func TestSendInputRacesExitSafely(t *testing.T) {
	workDir := t.TempDir()
	client, _ := startTestGuestd(t, workDir)
	ctx := t.Context()
	if _, err := client.StartCommand(ctx, &guestdv1.StartCommandRequest{
		Id:       "typed",
		Argv:     []string{"/bin/sh", "-c", "read x; echo GOT $x"},
		Dir:      workDir,
		Tty:      true,
		TtyInput: true,
	}); err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	stop := make(chan struct{})
	errs := make(chan error, 4)
	for range 4 {
		go func() {
			for {
				select {
				case <-stop:
					errs <- nil
					return
				default:
				}
				err := client.SendInput(ctx, "typed", []byte("hi\n"))
				if err != nil && connect.CodeOf(err) != connect.CodeFailedPrecondition {
					errs <- err
					return
				}
			}
		}()
	}
	var buf bytes.Buffer
	if _, err := client.StreamLogs(ctx, "typed", 0, true, &buf); err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	close(stop)
	for range 4 {
		if err := <-errs; err != nil {
			t.Fatalf("SendInput while the command exits: %v, want success or FAILED_PRECONDITION", err)
		}
	}
	if !strings.Contains(buf.String(), "GOT hi") {
		t.Fatalf("log %q lacks the typed line", buf.String())
	}
	if err := client.SendInput(ctx, "typed", []byte("late\n")); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("input after exit: %v, want FAILED_PRECONDITION", err)
	}
}
