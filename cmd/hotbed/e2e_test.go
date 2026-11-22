package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	store    *fake.Store // the worker's volumes
	ctrlSrv  *httptest.Server
	system   *fake.System // the worker's memory, which tests set
	// restart swaps in a new controller on the same database.
	serverCfg controller.ServerConfig
	handler   atomic.Pointer[http.Handler]
}

// restart replaces the controller with a new one on the same database, as a
// deploy does: whatever it knows must come from there.
func (c *cluster) restart() {
	c.server = controller.NewServer(c.serverCfg)
	h := c.server.Handler()
	c.handler.Store(&h)
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

// set makes the clock read t.
func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset = time.Until(t)
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
	init, err := controller.Init(t.Context(), dir, api.DefaultPort, "", tn)
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

	volumes := &fake.Store{Root: t.TempDir()}
	system := fake.NewSystem()
	eng, err := worker.NewEngine(t.Context(), worker.EngineConfig{
		Dir: filepath.Join(t.TempDir(), "worker"), NodeID: asahi.NodeID, Host: asahi.Hostname,
		Store: volumes, Units: &fake.Units{}, System: system,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	workerSrv := httptest.NewServer(workerHandler(tn, eng))
	t.Cleanup(workerSrv.Close)

	clk := &clock{}
	serverCfg := controller.ServerConfig{
		Store: store, Identifier: tn, Cluster: *cfg.Controller, Controller: "ubu1", Now: clk.Now, Self: loopback,
		Workers:    tn.Client(ubu1.Peer),
		WorkerAddr: func(string) string { return strings.TrimPrefix(workerSrv.URL, "http://") },
	}
	c := &cluster{t: t, id: init.ClusterID, engine: eng, clock: clk, store: volumes, system: system, serverCfg: serverCfg}
	c.restart()
	ctrlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { (*c.handler.Load()).ServeHTTP(w, r) }))
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
	c.asWorker, c.ctrlSrv = asWorker, ctrlSrv
	c.cli = deps{
		// The CLI runs on the laptop: its tailnet says so, and its calls
		// come from the laptop.
		tailnet: tailnettest.New(tailnet.Self{Peer: laptop}),
		http:    tn.Client(laptop), stream: tn.Client(laptop), cache: t.TempDir(),
	}
	return c
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

// addRepo registers a repo; its first base is built when a workspace asks.
func (c *cluster) addRepo(origin string) {
	c.t.Helper()
	c.ok(nil, "repo", "add", "app", origin)
	c.ok(nil, "step", "set", "app", "build", "--every-pull", "--", freshen)
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
	c.addRepo(origin)

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
		res := &pb.WatchSessionResponse{}
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
	quiet := &pb.WatchSessionResponse{}
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
	c.addRepo(upstream(t))
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
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "idle-yak")
	c.ok(nil, "add", "idle-yak", "app")
	srv := &pb.Command{}
	c.ok(srv, "run", "idle-yak", "app", "--detach", "--", "sleep", "300")

	c.clock.advance(59 * time.Minute)
	if err := c.server.ReapIdle(t.Context()); err != nil {
		t.Fatal(err)
	}
	res := &pb.WatchSessionResponse{}
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

// base waits for the controller to hear of base id and returns it.
func (c *cluster) base(id string) *pb.Base {
	c.t.Helper()
	var found *pb.Base
	c.eventually("the controller to see base "+id, func() bool {
		snap, err := c.asWorker.GetSnapshot(c.t.Context(), &pb.GetSnapshotRequest{})
		if err != nil {
			c.t.Fatal(err)
		}
		for _, b := range snap.Bases {
			if b.Id == id && b.State == pb.BaseState_BASE_STATE_WARM {
				found = b
				return true
			}
		}
		return false
	})
	return found
}

// Bases are freshened when a workspace asks for one and the branch has moved
// since the newest: incrementally, from the newest, unless a rebuild asked
// for one from nothing.
func TestBasesFreshenOnDemand(t *testing.T) {
	c := newCluster(t)
	origin := upstream(t)
	c.addRepo(origin)
	add := func(session string) *pb.Workspace {
		t.Helper()
		c.ok(nil, "session", "start", session)
		ws := &pb.Workspace{}
		c.ok(ws, "add", session, "app")
		if ws.State != pb.WorkspaceState_WORKSPACE_STATE_IDLE {
			t.Fatalf("add in %s = %v, want a ready, idle workspace", session, ws)
		}
		return ws
	}

	first := add("first-ant")
	g1 := c.base(first.BaseId)
	if g1.Generation != 1 || !g1.Cold || g1.Commit != gitIn(t, origin, "rev-parse", "HEAD") {
		t.Fatalf("first base = %v, want generation 1, cold, at the branch's head", g1)
	}

	if again := add("second-bee"); again.BaseId != first.BaseId {
		t.Errorf("add with the branch unmoved forked %s, want %s", again.BaseId, first.BaseId)
	}

	write(t, filepath.Join(origin, "src/lib.txt"), "moved\n")
	gitIn(t, origin, "commit", "--quiet", "-am", "Move the branch")
	moved := add("third-cat")
	g2 := c.base(moved.BaseId)
	if g2.Generation != 2 || g2.Cold || g2.Commit != gitIn(t, origin, "rev-parse", "HEAD") {
		t.Fatalf("base after the branch moved = %v, want generation 2, incremental, at the new head", g2)
	}
	code, out, _ := c.hotbed("run", "third-cat", "app", "--", "cat", "src/lib.txt")
	if code != 0 || out != "moved\n" {
		t.Errorf("the new base's checkout: exit %d, %q", code, out)
	}

	c.ok(nil, "repo", "rebuild", "app")
	rebuilt := add("fourth-dog")
	g3 := c.base(rebuilt.BaseId)
	if g3.Generation != 3 || !g3.Cold {
		t.Fatalf("base after a rebuild = %v, want generation 3, from nothing", g3)
	}
	if after := add("fifth-eel"); after.BaseId != rebuilt.BaseId {
		t.Errorf("add after the rebuild's base forked %s, want %s", after.BaseId, rebuilt.BaseId)
	}
}

func TestRemovedRepoBasesAreDeleted(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "last-owl")
	ws := &pb.Workspace{}
	c.ok(ws, "add", "last-owl", "app")
	c.base(ws.BaseId)
	c.ok(nil, "session", "end", "last-owl")
	c.ok(nil, "repo", "rm", "app")
	c.eventually("the controller to see the repo's bases gone", func() bool {
		snap, err := c.asWorker.GetSnapshot(c.t.Context(), &pb.GetSnapshotRequest{})
		if err != nil {
			c.t.Fatal(err)
		}
		return len(snap.Bases) == 0
	})
	if _, err := os.Stat(c.store.Path("bases/app/g1")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the base's volume is still there: %v", err)
	}
}

func TestAddUnreachableRepoIsRetryable(t *testing.T) {
	c := newCluster(t)
	c.ok(nil, "repo", "add", "app", "/nonexistent/repo")
	c.ok(nil, "session", "start", "eager-gnu")
	code, e := c.fails("add", "eager-gnu", "app")
	if code != exitError || e.Code != api.CodeRepoUnreachable || !e.Retryable {
		t.Errorf("add of an unreachable repo: exit %d, %+v", code, e)
	}
}

func TestAddFailsWhenFreshenFails(t *testing.T) {
	c := newCluster(t)
	c.ok(nil, "repo", "add", "app", upstream(t))
	c.ok(nil, "step", "set", "app", "build", "--every-pull", "--", "echo broken build >&2; exit 3")
	c.ok(nil, "session", "start", "eager-gnu")
	code, e := c.fails("add", "eager-gnu", "app")
	if code != exitError || e.Code != api.CodeWorkspaceFailed || e.Retryable || !strings.Contains(e.Message, "failed at step build") {
		t.Errorf("add with a failing freshen: exit %d, %+v", code, e)
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

// workerHandler serves an engine as a worker does: to the controller's node
// alone, over the fake tailnet (loopback).
func workerHandler(tn *tailnettest.Fake, eng *worker.Engine) http.Handler {
	return worker.Handler(tn, httpx.Policy{Owner: "ramon@", Nodes: []string{ubu1.NodeID}, Self: loopback}, eng)
}

var loopback = []netip.Addr{netip.MustParseAddr("127.0.0.1")}

// runs counts the lines a step appended to a file in the workspace's cache.
func (c *cluster) runs(session, file string) int {
	c.t.Helper()
	code, out, stderr := c.hotbed("run", session, "app", "--", "sh", "-c", `cat "$XDG_CACHE_HOME/`+file+`" 2>/dev/null || true`)
	if code != 0 {
		c.t.Fatalf("count %s: exit %d: %s", file, code, stderr)
	}
	return strings.Count(out, "\n")
}

// Steps are migrations: a base runs a step once and again only when its
// script changes, while an every-pull step also runs whenever the branch
// moved.
func TestStepsRunLikeMigrations(t *testing.T) {
	c := newCluster(t)
	origin := upstream(t)
	c.ok(nil, "repo", "add", "app", origin)
	c.ok(nil, "step", "set", "app", "tool", "--", `mkdir -p "$XDG_CACHE_HOME" && echo run >> "$XDG_CACHE_HOME/tool"`)
	c.ok(nil, "step", "set", "app", "build", "--every-pull", "--", `mkdir -p "$XDG_CACHE_HOME" && echo run >> "$XDG_CACHE_HOME/build"`)
	add := func(session string) *pb.Workspace {
		t.Helper()
		c.ok(nil, "session", "start", session)
		ws := &pb.Workspace{}
		c.ok(ws, "add", session, "app")
		return ws
	}
	counts := func(session string) [2]int { return [2]int{c.runs(session, "tool"), c.runs(session, "build")} }

	add("one-ant")
	if got := counts("one-ant"); got != [2]int{1, 1} {
		t.Errorf("first base ran tool, build %v times, want once each", got)
	}

	write(t, filepath.Join(origin, "src/lib.txt"), "moved\n")
	gitIn(t, origin, "commit", "--quiet", "-am", "Move the branch")
	if ws := add("two-bee"); ws.BaseId != "app-g2" {
		t.Fatalf("after a pull the workspace forked %s, want app-g2", ws.BaseId)
	}
	if got := counts("two-bee"); got != [2]int{1, 2} {
		t.Errorf("after a pull tool, build ran %v times, want only build again", got)
	}

	c.ok(nil, "step", "set", "app", "tool", "--", `mkdir -p "$XDG_CACHE_HOME" && echo run v2 >> "$XDG_CACHE_HOME/tool"`)
	if ws := add("three-cat"); ws.BaseId != "app-g3" {
		t.Fatalf("after a step changed the workspace forked %s, want app-g3", ws.BaseId)
	}
	if got := counts("three-cat"); got != [2]int{2, 2} {
		t.Errorf("after tool changed tool, build ran %v times, want only tool again", got)
	}
	if ws := add("four-dog"); ws.BaseId != "app-g3" {
		t.Errorf("with nothing changed the workspace forked %s, want app-g3", ws.BaseId)
	}

	// step ls is the cluster as workers last reported it.
	want := []string{"tool asahimbp app-g3 STEP_STATE_APPLIED", "build asahimbp app-g3 STEP_STATE_APPLIED"}
	var got []string
	c.eventually("step ls to show app-g3", func() bool {
		ls := &pb.ListStepsResponse{}
		c.ok(ls, "step", "ls", "app")
		got = got[:0]
		for _, s := range ls.Steps {
			for _, n := range s.Nodes {
				got = append(got, fmt.Sprintf("%s %s %s %s", s.Step.Name, n.Node, n.BaseId, n.State))
			}
		}
		return len(got) == 2 && strings.Contains(got[0], "app-g3")
	})
	if !slices.Equal(got, want) {
		t.Errorf("step ls = %q, want %q", got, want)
	}
	code, out, _ := c.hotbed("step", "show", "app", "tool")
	if code != 0 || out != `mkdir -p "$XDG_CACHE_HOME" && echo run v2 >> "$XDG_CACHE_HOME/tool"`+"\n" {
		t.Errorf("step show: exit %d, %q", code, out)
	}
}

// step add gives the agent's workspace what the step installs at once, and
// saves only a step that works twice.
func TestStepAddTriesItTwice(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "keen-elk")
	c.ok(nil, "add", "keen-elk", "app")

	repo := &pb.Repo{}
	c.ok(repo, "step", "add", "keen-elk", "app", "tool", "--", `mkdir -p "$HOTBED_BIN" && printf '#!/bin/sh\necho tool works\n' > "$HOTBED_BIN/tool" && chmod +x "$HOTBED_BIN/tool"`)
	if len(repo.Steps) != 2 || repo.Steps[1].Name != "tool" || len(repo.Steps[1].Hash) != 12 || repo.Steps[1].UpdatedBy != "laptop" {
		t.Fatalf("steps after add = %v", repo.Steps)
	}
	if code, out, _ := c.hotbed("run", "keen-elk", "app", "--", "tool"); code != 0 || out != "tool works\n" {
		t.Errorf("the tool in the agent's workspace: exit %d, %q", code, out)
	}

	code, e := c.fails("step", "add", "keen-elk", "app", "once", "--", `mkdir -p "$XDG_DATA_HOME" && mkdir "$XDG_DATA_HOME/once-only"`)
	if code != exitError || e.Code != api.CodeStepNotIdempotent {
		t.Errorf("a step that fails when repeated: exit %d, %+v", code, e)
	}
	code, e = c.fails("step", "add", "keen-elk", "app", "broken", "--", "exit 4")
	if code != exitError || e.Code != api.CodeStepFailed || !strings.Contains(e.Message, "exited 4") {
		t.Errorf("a failing step: exit %d, %+v", code, e)
	}
	ls := &pb.ListStepsResponse{}
	c.ok(ls, "step", "ls", "app")
	var names []string
	for _, s := range ls.Steps {
		names = append(names, s.Step.Name)
	}
	if !slices.Equal(names, []string{"build", "tool"}) {
		t.Errorf("saved steps = %v, want only build and tool", names)
	}

	// A new workspace's base ran the step, so it has the tool too.
	c.ok(nil, "session", "start", "next-fox")
	ws := &pb.Workspace{}
	c.ok(ws, "add", "next-fox", "app")
	if code, out, _ := c.hotbed("run", "next-fox", "app", "--", "tool"); ws.BaseId != "app-g2" || code != 0 || out != "tool works\n" {
		t.Errorf("the tool in a new workspace on %s: exit %d, %q", ws.BaseId, code, out)
	}
}

// A step that breaks the freshen does not stop work: workspaces get the
// newest warm base with a note, and the broken recipe is not built again.
func TestFailingStepFallsBackToTheWarmBase(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "old-gnu")
	c.ok(nil, "add", "old-gnu", "app")

	c.ok(nil, "step", "set", "app", "broken", "--", "echo nope >&2; exit 5")
	for _, s := range []string{"new-hen", "late-jay"} {
		c.ok(nil, "session", "start", s)
		ws := &pb.Workspace{}
		c.ok(ws, "add", s, "app")
		if ws.BaseId != "app-g1" || ws.State != pb.WorkspaceState_WORKSPACE_STATE_IDLE || !strings.HasPrefix(ws.Reason, "on app-g1, not up to date: base app-g2 failed at step broken") {
			t.Errorf("add in %s = %v, want app-g1 with a note", s, ws)
		}
	}
	// The worker reports its bases on its own; wait for the report, then
	// check that the broken recipe was built only once.
	want := []string{"app-g1 BASE_STATE_WARM", "app-g2 BASE_STATE_FAILED"}
	var bases []string
	c.eventually("the controller to see both bases", func() bool {
		snap, err := c.asWorker.GetSnapshot(t.Context(), &pb.GetSnapshotRequest{})
		if err != nil {
			t.Fatal(err)
		}
		bases = bases[:0]
		for _, b := range snap.Bases {
			bases = append(bases, b.Id+" "+b.State.String())
		}
		slices.Sort(bases)
		return len(bases) >= 2 && !strings.HasSuffix(bases[len(bases)-1], "BUILDING")
	})
	if !slices.Equal(bases, want) {
		t.Errorf("bases = %v, want %v", bases, want)
	}
}

// Each command has a TMPDIR of its own, on disk, gone once it ends.
func TestCommandsHaveTheirOwnTmpdir(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "tidy-owl")
	c.ok(nil, "add", "tidy-owl", "app")
	code, out, _ := c.hotbed("run", "tidy-owl", "app", "--", "sh", "-c", `echo "$TMPDIR"; ls -ld "$TMPDIR" | cut -c1-10; echo store > "$TMPDIR/store"`)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 2 || filepath.Base(filepath.Dir(lines[0])) != "tmp" || lines[1] != "drwxrwxrwt" {
		t.Fatalf("run: exit %d, %q, want its own sticky TMPDIR under the state directory's tmp", code, out)
	}
	c.eventually("the command's TMPDIR to go", func() bool {
		_, err := os.Stat(lines[0])
		return errors.Is(err, os.ErrNotExist)
	})
}

// A deploy restarts the controller under a waiting add: the watch drops, and
// add watches again rather than failing.
func TestAddWatchesAgainWhenTheStreamDrops(t *testing.T) {
	c := newCluster(t)
	c.ok(nil, "repo", "add", "app", upstream(t))
	c.ok(nil, "step", "set", "app", "build", "--every-pull", "--", "sleep 2")
	c.ok(nil, "session", "start", "calm-yak")
	type result struct {
		code           int
		stdout, stderr string
	}
	done := make(chan result, 1)
	go func() {
		code, out, errOut := c.hotbed("add", "calm-yak", "app")
		done <- result{code, out, errOut}
	}()
	c.eventually("the workspace to be preparing", func() bool {
		snap, err := c.asWorker.GetSnapshot(t.Context(), &pb.GetSnapshotRequest{})
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range snap.Workspaces {
			if w.State == pb.WorkspaceState_WORKSPACE_STATE_PREPARING {
				return true
			}
		}
		return false
	})
	c.ctrlSrv.CloseClientConnections()
	r := <-done
	ws := &pb.Workspace{}
	var out output
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil || !out.OK {
		t.Fatalf("add: exit %d, %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if err := protojson.Unmarshal(out.Data, ws); err != nil {
		t.Fatal(err)
	}
	if ws.State != pb.WorkspaceState_WORKSPACE_STATE_IDLE || !strings.Contains(r.stderr, "lost the watch") {
		t.Errorf("add = %v, stderr %q; want a ready workspace after watching again", ws, r.stderr)
	}
}

// The cockpit shows sessions that ended, with the workspaces they had.
func TestSnapshotKeepsEndedSessions(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "spent-owl")
	ws := &pb.Workspace{}
	c.ok(ws, "add", "spent-owl", "app")
	c.ok(nil, "session", "end", "spent-owl")
	snap, err := c.asWorker.GetSnapshot(t.Context(), &pb.GetSnapshotRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var got *pb.Session
	for _, s := range snap.Sessions {
		if s.Id == "spent-owl" {
			got = s
		}
	}
	if got == nil || got.EndedAt == nil || got.EndReason != "ended by its agent" || len(got.Workspaces) != 1 ||
		got.Workspaces[0].Id != ws.Id || got.Workspaces[0].RepoId != "app" || got.Workspaces[0].ReleasedAt == nil {
		t.Errorf("snapshot's session = %v", got)
	}
}

// commandStates is the session's commands' states, by ID.
func (c *cluster) commandStates(session string) map[string]pb.CommandState {
	c.t.Helper()
	ps := &pb.ListCommandsResponse{}
	c.ok(ps, "ps", session)
	out := map[string]pb.CommandState{}
	for _, cmd := range ps.Commands {
		out[cmd.Id] = cmd.State
	}
	return out
}

// Parking frees memory for other work; it never stops all of it. With
// memory short and staying short, one of two commands is frozen, never
// both, and the frozen one runs again once nothing else does.
func TestParkingNeverStopsAllWork(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "full-yak")
	c.ok(nil, "add", "full-yak", "app")
	first, second := &pb.Command{}, &pb.Command{}
	c.ok(first, "run", "full-yak", "app", "--detach", "--kind", "build", "--", "sleep", "300")
	c.ok(second, "run", "full-yak", "app", "--detach", "--kind", "build", "--", "sleep", "300")
	c.eventually("both to run", func() bool {
		st := c.commandStates("full-yak")
		return st[first.Id] == pb.CommandState_COMMAND_STATE_RUNNING && st[second.Id] == pb.CommandState_COMMAND_STATE_RUNNING
	})

	c.system.SetUsed(c.system.Mem.Total * 95 / 100)
	c.eventually("the newest to be parked", func() bool {
		return c.commandStates("full-yak")[second.Id] == pb.CommandState_COMMAND_STATE_PARKED
	})
	// The engine steps every second: three steps at 95% leave the other running.
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if st := c.commandStates("full-yak")[first.Id]; st != pb.CommandState_COMMAND_STATE_RUNNING {
			t.Fatalf("the last running command went %s", st)
		}
	}

	c.ok(nil, "stop", "full-yak", first.Id)
	c.eventually("the parked one to run again, memory still short", func() bool {
		return c.commandStates("full-yak")[second.Id] == pb.CommandState_COMMAND_STATE_RUNNING
	})
}

// A base build no workspace waits for any more, with a newer generation of
// its repo after it, is dropped: it would only hold memory and CPUs, and
// on a full machine be parked forever.
func TestSupersededBaseBuildIsDropped(t *testing.T) {
	c := newCluster(t)
	c.ok(nil, "repo", "add", "app", upstream(t))
	c.ok(nil, "step", "set", "app", "build", "--every-pull", "--", "sleep 30")
	c.ok(nil, "session", "start", "slow-ant")
	waiting := &pb.Workspace{}
	c.ok(waiting, "add", "slow-ant", "app", "--no-wait")
	if waiting.BaseId != "app-g1" || waiting.State != pb.WorkspaceState_WORKSPACE_STATE_PREPARING {
		t.Fatalf("first add = %v, want a workspace waiting for app-g1", waiting)
	}
	c.ok(nil, "step", "set", "app", "build", "--every-pull", "--", "true")
	c.ok(nil, "session", "start", "fast-bee")
	ws := &pb.Workspace{}
	c.ok(ws, "add", "fast-bee", "app")
	if ws.BaseId != "app-g2" {
		t.Fatalf("second add forked %s, want app-g2", ws.BaseId)
	}

	c.ok(nil, "session", "end", "slow-ant")
	c.eventually("app-g1 to be dropped, app-g2 kept", func() bool {
		snap, err := c.asWorker.GetSnapshot(t.Context(), &pb.GetSnapshotRequest{})
		if err != nil {
			t.Fatal(err)
		}
		var bases []string
		for _, b := range snap.Bases {
			bases = append(bases, b.Id+" "+b.State.String())
		}
		return slices.Equal(bases, []string{"app-g2 BASE_STATE_WARM"})
	})
}
