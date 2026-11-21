package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2/connecthttp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/controller"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/httpx"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/tailnet"
	"github.com/semistrict/hotbed/internal/tailnet/tailnettest"
	"github.com/semistrict/hotbed/internal/worker"
)

var asahi = tailnet.Peer{NodeID: "nAsahi", Hostname: "asahimbp", Login: "ramon@"}

// cluster is a controller and one worker in this process: the real
// controller, engine and CLI over Connect, with fakes only where Linux and
// root would be needed (volumes are copied, commands are local processes).
type cluster struct {
	t        *testing.T
	id       string
	server   *controller.Server
	engine   *worker.Engine
	asWorker hotbedv1connect.ClusterServiceClient
	cli      deps
	clock    *clock
}

type clock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

func newCluster(t *testing.T) *cluster {
	t.Helper()
	tn := tailnettest.New(ubu1)
	dir := t.TempDir()
	init, err := controller.Init(t.Context(), dir, api.DefaultPort, tn)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := statedir.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := controller.OpenStore(t.Context(), filepath.Join(dir, "hotbed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	eng, err := worker.NewEngine(t.Context(), worker.EngineConfig{
		Dir: filepath.Join(t.TempDir(), "worker"), NodeID: asahi.NodeID, Host: asahi.Hostname,
		Store: &fake.Store{Root: t.TempDir()}, Units: &fake.Units{}, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	workerSrv := httptest.NewServer(workerHandler(tn, eng))
	t.Cleanup(workerSrv.Close)

	clk := &clock{}
	server := controller.NewServer(controller.ServerConfig{
		Store: store, Identifier: tn, Cluster: *cfg.Controller, Controller: "ubu1", Now: clk.Now,
		Workers:    tn.Client(ubu1.Peer),
		WorkerAddr: func(string) string { return strings.TrimPrefix(workerSrv.URL, "http://") },
	})
	ctrlSrv := httptest.NewServer(server.Handler())
	t.Cleanup(ctrlSrv.Close)
	ctrl := strings.TrimPrefix(ctrlSrv.URL, "http://")

	asWorker, _, err := api.Controller(ctrl, tn.Client(asahi))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := asWorker.Join(t.Context(), &pb.JoinRequest{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { eng.Run(ctx); close(done) }()
	go func() { worker.Reports(ctx, eng, asWorker, init.ClusterID) }()
	t.Cleanup(func() { cancel(); <-done })

	t.Setenv("HOTBED_CONTROLLER", ctrl)
	return &cluster{
		t: t, id: init.ClusterID, server: server, engine: eng, asWorker: asWorker, clock: clk,
		cli: deps{
			// The CLI runs on the laptop: its tailnet says so, and its calls
			// come from the laptop.
			tailnet: tailnettest.New(tailnet.Self{Peer: laptop}),
			http:    tn.Client(laptop), stream: tn.Client(laptop), cache: t.TempDir(),
		},
	}
}

// eventually waits for cond; the engine and its reports run on their own.
func (c *cluster) eventually(what string, cond func() bool) {
	c.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	c.t.Fatalf("timed out waiting for %s", what)
}

// hotbed runs the CLI and returns its exit status, stdout and stderr.
func (c *cluster) hotbed(args ...string) (int, string, string) {
	c.t.Helper()
	var stdout, stderr bytes.Buffer
	d := c.cli
	d.stdout, d.stderr = &stdout, &stderr
	code := run(c.t.Context(), args, d)
	return code, stdout.String(), stderr.String()
}

// ok runs the CLI, expects a successful envelope, and decodes its data
// into m (a protobuf message, or anything JSON).
func (c *cluster) ok(m any, args ...string) {
	c.t.Helper()
	code, stdout, stderr := c.hotbed(args...)
	var out output
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		c.t.Fatalf("hotbed %v printed %q (stderr %q)", args, stdout, stderr)
	}
	if code != 0 || !out.OK {
		c.t.Fatalf("hotbed %v: exit %d, error %+v", args, code, out.Error)
	}
	switch v := m.(type) {
	case nil:
	case proto.Message:
		if err := protojson.Unmarshal(out.Data, v); err != nil {
			c.t.Fatal(err)
		}
	default:
		if err := json.Unmarshal(out.Data, v); err != nil {
			c.t.Fatal(err)
		}
	}
}

// fails runs the CLI and returns the error of the envelope it printed.
func (c *cluster) fails(args ...string) (int, *api.Error) {
	c.t.Helper()
	code, stdout, _ := c.hotbed(args...)
	var out output
	if err := json.Unmarshal([]byte(stdout), &out); err != nil || out.OK {
		c.t.Fatalf("hotbed %v printed %q, want an error envelope", args, stdout)
	}
	return code, out.Error
}

// warmRepo registers a repo and waits for its first base.
func (c *cluster) warmRepo(origin string) {
	c.t.Helper()
	c.ok(nil, "repo", "add", "app", origin, "--freshen", freshen)
	c.eventually("the controller to see a warm base", func() bool {
		snap, err := c.asWorker.GetSnapshot(c.t.Context(), &pb.GetSnapshotRequest{})
		if err != nil {
			c.t.Fatal(err)
		}
		return len(snap.Bases) == 1 && snap.Bases[0].State == pb.BaseState_BASE_STATE_WARM
	})
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// upstream is the repo's origin: a project whose "build" copies its sources
// into target/ and keeps a cache in the workspace's cache directory.
func upstream(t *testing.T) string {
	dir := t.TempDir()
	gitIn(t, dir, "init", "--quiet", "-b", "main")
	write(t, filepath.Join(dir, "src/lib.txt"), "base\n")
	write(t, filepath.Join(dir, "src/keep.txt"), "keep\n")
	write(t, filepath.Join(dir, ".gitignore"), "target/\n")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "--quiet", "-m", "First commit")
	return dir
}

const freshen = `mkdir -p target "$XDG_CACHE_HOME" && cp -R src target/ && echo "   Compiling app v0.1.0 (/work/app)" && echo warm > "$XDG_CACHE_HOME/marker"`

func TestSessionWithSyncedCheckout(t *testing.T) {
	c := newCluster(t)
	origin := upstream(t)
	c.warmRepo(origin)

	sess := &pb.Session{}
	c.ok(sess, "session", "start")
	if !sessionNameRE.MatchString(sess.Id) {
		t.Fatalf("session name %q is not adjective-noun", sess.Id)
	}
	S := sess.Id

	agent := filepath.Join(t.TempDir(), "agent")
	gitIn(t, filepath.Dir(agent), "clone", "--quiet", origin, agent)
	write(t, filepath.Join(agent, "src/lib.txt"), "edited\n")
	write(t, filepath.Join(agent, "src/new.txt"), "new\n")

	ws := &pb.Workspace{}
	c.ok(ws, "add", S, "app", "--sync", agent)
	if ws.RepoId != "app" || ws.SessionId != S || ws.Path != "/work/app" || ws.SyncDir != agent || ws.SyncHost != "laptop" || ws.FilesChanged != 2 {
		t.Fatalf("add = %v", ws)
	}
	again := &pb.Workspace{}
	c.ok(again, "add", S, "app")
	if again.Id != ws.Id {
		t.Errorf("second add made workspace %s, want the session's %s", again.Id, ws.Id)
	}

	// run sees the agent's edits, the base's build and caches, in the fork.
	code, stdout, stderr := c.hotbed("run", S, "app", "--", "sh", "-c", `cat src/lib.txt src/new.txt target/src/keep.txt "$XDG_CACHE_HOME/marker"`)
	if code != 0 || stdout != "edited\nnew\nkeep\nwarm\n" {
		t.Fatalf("run: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if strings.Contains(stderr, "synced") {
		t.Errorf("run synced an unchanged checkout: %q", stderr)
	}

	// An edit after add is synced by the next run.
	write(t, filepath.Join(agent, "src/lib.txt"), "edited again\n")
	code, stdout, stderr = c.hotbed("run", S, "app", "--", "cat", "src/lib.txt")
	if code != 0 || stdout != "edited again\n" || !strings.Contains(stderr, "synced 2 changed files") {
		t.Fatalf("run after an edit: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	// run passes the command's status through.
	if code, _, _ := c.hotbed("run", S, "app", "--", "sh", "-c", "exit 3"); code != 3 {
		t.Errorf("run of exit 3 returned %d", code)
	}

	// wait reports what happened to the session's commands, each event once.
	var events []*pb.SessionEvent
	c.eventually("the session's events", func() bool {
		res := &pb.WaitSessionResponse{}
		c.ok(res, "wait", S, "--timeout", "1")
		events = append(events, res.Events...)
		return countWhat(events, "exited") == 3
	})
	if n := countWhat(events, "admitted"); n != 3 {
		t.Errorf("%d admitted events, want 3: %v", n, events)
	}
	last := events[len(events)-1]
	if last.What != "exited" || last.GetExitCode() != 3 || last.State != pb.CommandState_COMMAND_STATE_FAILED || last.WorkspaceId != ws.Id {
		t.Errorf("last event = %v", last)
	}
	quiet := &pb.WaitSessionResponse{}
	c.ok(quiet, "wait", S, "--timeout", "1")
	if len(quiet.Events) != 0 || quiet.Live != 0 {
		t.Errorf("wait with nothing new = %v", quiet)
	}

	// A detached command runs until stopped.
	srv := &pb.Command{}
	c.ok(srv, "run", S, "app", "--detach", "--port", "5173", "--", "sh", "-c", "echo serving; sleep 300")
	if srv.Url != "http://asahimbp:5173" || srv.Attached || srv.SessionId != S {
		t.Errorf("detached run = %v", srv)
	}
	c.eventually("the server's output", func() bool {
		_, out, _ := c.hotbed("logs", S, srv.Id)
		return out == "serving\n"
	})
	ps := &pb.ListCommandsResponse{}
	c.ok(ps, "ps", S)
	if !containsLive(ps.Commands, srv.Id) {
		t.Errorf("ps = %v, want %s live", ps.Commands, srv.Id)
	}
	stopped := &pb.Command{}
	c.ok(stopped, "stop", S, srv.Id)
	if stopped.State != pb.CommandState_COMMAND_STATE_CANCELLED {
		t.Errorf("after stop: %v", stopped)
	}

	// Another session cannot see this one's commands.
	other := &pb.Session{}
	c.ok(other, "session", "start", "quiet-heron")
	if _, e := c.fails("logs", "quiet-heron", srv.Id); e.Code != api.CodeNotFound {
		t.Errorf("logs from another session: %+v", e)
	}
	if _, e := c.fails("session", "start", "quiet-heron"); e.Code != api.CodeConflict {
		t.Errorf("start of a taken name: %+v", e)
	}

	// The cockpit sees it all.
	snap, err := c.asWorker.GetSnapshot(t.Context(), &pb.GetSnapshotRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Repos) != 1 || snap.Repos[0].Units != 1 || snap.Repos[0].ColdBuildSeconds <= 0 {
		t.Errorf("snapshot repos = %v", snap.Repos)
	}
	if len(snap.Sessions) != 2 || snap.Totals24H.Forks != 1 {
		t.Errorf("snapshot sessions %v, totals %v", snap.Sessions, snap.Totals24H)
	}

	ended := &pb.Session{}
	c.ok(ended, "session", "end", S)
	if ended.EndedAt == nil || ended.EndReason != "ended by its agent" || len(ended.WorkspaceIds) != 0 {
		t.Errorf("session end = %v", ended)
	}
	if _, e := c.fails("run", S, "app", "--", "true"); e.Code != api.CodeNotFound {
		t.Errorf("run in an ended session: %+v", e)
	}
}

func TestRemoteOnlyWorkspace(t *testing.T) {
	c := newCluster(t)
	c.warmRepo(upstream(t))
	sess := &pb.Session{}
	c.ok(sess, "session", "start", "brave-otter")

	ws := &pb.Workspace{}
	c.ok(ws, "add", "brave-otter", "app")
	if ws.SyncDir != "" {
		t.Fatalf("add without --sync follows %q", ws.SyncDir)
	}
	// The agent edits and commits in the workspace itself; it persists.
	for _, args := range [][]string{
		{"sh", "-c", "printf 'remote\\n' > src/lib.txt"},
		{"git", "-c", "user.name=a", "-c", "user.email=a@a", "commit", "-qam", "remote edit"},
	} {
		if code, out, errOut := c.hotbed(append([]string{"run", "brave-otter", "app", "--"}, args...)...); code != 0 {
			t.Fatalf("run %v: exit %d %q %q", args, code, out, errOut)
		}
	}
	code, stdout, _ := c.hotbed("run", "brave-otter", "app", "--", "git", "log", "-1", "--format=%s")
	if code != 0 || stdout != "remote edit\n" {
		t.Errorf("git log in the workspace: exit %d, %q", code, stdout)
	}
	if _, e := c.fails("sync", "brave-otter", "app"); e.Code != api.CodeInvalidArgs {
		t.Errorf("sync of a remote-only workspace: %+v", e)
	}
}

func TestAbandonedSessionEnds(t *testing.T) {
	c := newCluster(t)
	c.warmRepo(upstream(t))
	c.ok(nil, "session", "start", "idle-yak")
	c.ok(nil, "add", "idle-yak", "app")
	srv := &pb.Command{}
	c.ok(srv, "run", "idle-yak", "app", "--detach", "--", "sleep", "300")

	c.clock.advance(59 * time.Minute)
	if err := c.server.ReapIdle(t.Context()); err != nil {
		t.Fatal(err)
	}
	res := &pb.WaitSessionResponse{}
	c.ok(res, "wait", "idle-yak", "--timeout", "1") // keeps it alive for another hour
	if res.Ended {
		t.Fatalf("session ended before an hour idle: %v", res)
	}

	c.clock.advance(61 * time.Minute)
	if err := c.server.ReapIdle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, e := c.fails("wait", "idle-yak"); e.Code != api.CodeNotFound || !strings.Contains(e.Message, "abandoned") {
		t.Errorf("wait on an abandoned session: %+v", e)
	}
	c.eventually("the abandoned session's command to stop", func() bool {
		cmd, ok := c.engine.Command(srv.Id)
		return ok && cmd.State == pb.CommandState_COMMAND_STATE_CANCELLED
	})
}

func TestAddBeforeAnyBaseIsRetryable(t *testing.T) {
	c := newCluster(t)
	c.ok(nil, "repo", "add", "app", "/nonexistent/repo", "--freshen", "true")
	c.ok(nil, "session", "start", "eager-gnu")
	code, e := c.fails("add", "eager-gnu", "app")
	if code != exitError || e.Code != api.CodeNoWarmBase || !e.Retryable {
		t.Errorf("add before a base: exit %d, %+v", code, e)
	}
}

func countWhat(events []*pb.SessionEvent, what string) int {
	n := 0
	for _, e := range events {
		if e.What == what {
			n++
		}
	}
	return n
}

func containsLive(cmds []*pb.Command, id string) bool {
	for _, c := range cmds {
		if c.Id == id && live(c.State) {
			return true
		}
	}
	return false
}

var sessionNameRE = regexpMust(`^[a-z]+-[a-z]+$`)

func regexpMust(s string) *regexp.Regexp { return regexp.MustCompile(s) }

// workerHandler serves an engine as a worker does, to the cluster's owner.
func workerHandler(tn *tailnettest.Fake, eng *worker.Engine) http.Handler {
	server := httpx.NewServer("ramon@")
	hotbedv1connect.RegisterWorkspaceServiceHandler(server, &worker.Service{E: eng})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	return httpx.Identify(tn, mux)
}
