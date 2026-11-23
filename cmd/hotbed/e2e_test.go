package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/httpx"
	"github.com/semistrict/hotbed/internal/tailnet"
	"github.com/semistrict/hotbed/internal/tailnet/tailnettest"
	"github.com/semistrict/hotbed/internal/worker"
)

var asahi = tailnet.Peer{NodeID: "nAsahi", Hostname: "asahimbp", Login: "ramon@"}

// switchboard serves the controller, records the calls it gets, and, taken
// down, answers every call as unavailable, as a proxy in front of an
// unreachable machine would.
type switchboard struct {
	next  http.Handler
	mu    sync.Mutex
	down  bool
	calls []string
}

func (s *switchboard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	down := s.down
	if !down {
		s.calls = append(s.calls, r.URL.Path)
	}
	s.mu.Unlock()
	if down {
		http.Error(w, "controller down", http.StatusServiceUnavailable)
		return
	}
	s.next.ServeHTTP(w, r)
}

func (s *switchboard) setDown(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = down
}

// record clears the calls recorded so far and returns a func that lists
// the WorkspaceService calls since.
func (s *switchboard) record() func() []string {
	s.mu.Lock()
	s.calls = nil
	s.mu.Unlock()
	return func() []string {
		s.mu.Lock()
		defer s.mu.Unlock()
		var out []string
		for _, c := range s.calls {
			if m, ok := strings.CutPrefix(c, "/hotbed.v1.WorkspaceService/"); ok && !slices.Contains(out, m) {
				out = append(out, m)
			}
		}
		slices.Sort(out)
		return out
	}
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

// newCluster starts a cluster; each of with changes the worker's engine
// config first.

// heartbeat sends every running, reachable worker's heartbeat now, with the
// sessions clients used on it since the last one.
func (c *cluster) heartbeat() {
	c.t.Helper()
	for _, nd := range c.nodes {
		if nd.halted.Load() || nd.cut.Load() {
			continue
		}
		if err := worker.HeartbeatOnce(c.t.Context(), nd.eng(), nd.cluster, c.id, nil); err != nil {
			c.t.Fatal(err)
		}
	}
}

// online drives a heartbeat from every running, reachable worker and waits for
// the workers to be online by the controller's clock.
func (c *cluster) online() {
	c.t.Helper()
	c.heartbeat()
	c.eventually("the workers to be online", func() bool {
		res, err := c.asWorker.ListNodes(c.t.Context(), &pb.ListNodesRequest{})
		if err != nil || len(res.Nodes) != len(c.nodes) {
			return false
		}
		for _, n := range res.Nodes {
			if !n.Online {
				return false
			}
		}
		return true
	})
}

// eventually waits for cond; the engine and its connection run on their own.
func (c *cluster) eventually(what string, cond func() bool) {
	c.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.t.Fatalf("timed out waiting for %s (started waiting %s ago)", what, time.Since(deadline.Add(-30*time.Second)).Round(time.Millisecond))
}

// hotbed runs the CLI and returns its exit status, stdout and stderr.
func (c *cluster) hotbed(args ...string) (int, string, string) {
	c.t.Helper()
	return c.hotbedIn("", args...)
}

// hotbedIn is hotbed with stdin.
func (c *cluster) hotbedIn(stdin string, args ...string) (int, string, string) {
	c.t.Helper()
	var stdout, stderr bytes.Buffer
	d := c.cli
	d.stdin, d.stdout, d.stderr = strings.NewReader(stdin), &stdout, &stderr
	code := run(c.t.Context(), args, d)
	return code, stdout.String(), stderr.String()
}

// ok runs the CLI, expects a successful envelope, and decodes its data
// into m (a protobuf message, or anything JSON).
func (c *cluster) ok(m any, args ...string) {
	c.t.Helper()
	c.okIn("", m, args...)
}

// okIn is ok with stdin.
func (c *cluster) okIn(stdin string, m any, args ...string) {
	c.t.Helper()
	code, stdout, stderr := c.hotbedIn(stdin, append([]string{"--json"}, args...)...)
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
	return c.failsIn("", args...)
}

// failsIn is fails with stdin.
func (c *cluster) failsIn(stdin string, args ...string) (int, *api.Error) {
	c.t.Helper()
	code, stdout, _ := c.hotbedIn(stdin, append([]string{"--json"}, args...)...)
	var out output
	if err := json.Unmarshal([]byte(stdout), &out); err != nil || out.OK {
		c.t.Fatalf("hotbed %v printed %q, want an error envelope", args, stdout)
	}
	return code, out.Error
}

// addRepo registers a repo in project app, with a build; its first base is
// built when a workspace asks.
func (c *cluster) addRepo(origin string) {
	c.t.Helper()
	c.addProject(origin)
	c.ok(nil, "step", "set", "-p", "app", "build", "--every-pull", "--", freshen)
}

// addProject registers repo app at origin, in project app, which sessions
// then work on: only its repos are in their workspaces.
func (c *cluster) addProject(origin string) {
	c.t.Helper()
	c.ok(nil, "project", "add", "app", "--repo", origin)
	c.ok(nil, "repo", "add", "app", origin)
	c.t.Setenv("HOTBED_PROJECT", "app")
}

// workspace is the session's workspace of app for a run placed by p that
// syncs nothing, as hotbed run gets it before it runs, once it is ready:
// for tests that look at workspaces rather than run in them.
func (c *cluster) workspace(session string, p *pb.Placement) *pb.Workspace {
	c.t.Helper()
	ws, err := c.tryWorkspace(session, p)
	if err != nil {
		c.t.Fatalf("workspace of app for %s (%v): %v", session, p, err)
	}
	return ws
}

// tryWorkspace is workspace, failing with the API error it gets.
func (c *cluster) tryWorkspace(session string, p *pb.Placement) (*pb.Workspace, *api.Error) {
	c.t.Helper()
	addr, err := c.cli.controller()
	if err != nil {
		c.t.Fatal(err)
	}
	f := controllerClient{Controller: addr}
	d := c.cli
	d.stdout, d.stderr = io.Discard, io.Discard
	ws, w, err := f.workspaceFor(c.t.Context(), d, session, "app", p, "")
	if err == nil {
		ws, err = waitReady(c.t.Context(), w, ws, io.Discard)
	}
	if err != nil {
		return nil, toAPIError(err)
	}
	return ws, nil
}

// workspaces is a WorkspaceService client of the controller, as the
// cockpit calls it.
func (c *cluster) workspaces() hotbedv1connect.WorkspaceServiceClient {
	c.t.Helper()
	addr, err := c.cli.controller()
	if err != nil {
		c.t.Fatal(err)
	}
	conn, err := api.Dial(addr, api.DefaultPort, c.cli.http)
	if err != nil {
		c.t.Fatal(err)
	}
	return hotbedv1connect.NewWorkspaceServiceClient(conn)
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

// freshen is app's build, as a step of its project: steps run in /work,
// where each of the project's repos is checked out.
const freshen = `cd app && mkdir -p target "$XDG_CACHE_HOME" && cp -R src target/ && echo "   Compiling app v0.1.0 (/work/app)" && echo warm > "$XDG_CACHE_HOME/marker"`

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

	// A run in a checkout of the repo syncs it first: it sees the agent's
	// edits, the base's build and caches, in the fork.
	t.Chdir(agent)
	code, stdout, stderr := c.hotbed("run", "-s", S, "-r", "app", "--", "sh", "-c", `cat src/lib.txt src/new.txt target/src/keep.txt "$XDG_CACHE_HOME/marker"`)
	if code != 0 || stdout != "edited\nnew\nkeep\nwarm\n" || !strings.Contains(stderr, "synced 2 changed files") {
		t.Fatalf("run: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	ws := &pb.Workspace{}
	c.ok(ws, "workspace", "show", "-s", S)
	root := gitIn(t, agent, "rev-parse", "--show-toplevel")
	if ws.Project != "app" || ws.SessionId != S || len(ws.Repos) != 1 || ws.Repos[0].Repo != "app" || ws.Repos[0].SyncDir != root || ws.SyncHost != "laptop" || ws.Node != "asahimbp" || ws.Repos[0].FilesChanged != 2 {
		t.Fatalf("workspace show = %v", ws)
	}

	// An edit is synced by the next run.
	write(t, filepath.Join(agent, "src/lib.txt"), "edited again\n")
	code, stdout, stderr = c.hotbed("run", "-s", S, "-r", "app", "--", "cat", "src/lib.txt")
	if code != 0 || stdout != "edited again\n" || !strings.Contains(stderr, "synced 2 changed files") {
		t.Fatalf("run after an edit: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	// run passes the command's status through, and sends nothing for an
	// unchanged checkout.
	code, _, stderr = c.hotbed("run", "-s", S, "-r", "app", "--", "sh", "-c", "exit 3")
	if code != 3 {
		t.Errorf("run of exit 3 returned %d", code)
	}
	if strings.Contains(stderr, "synced") {
		t.Errorf("run synced an unchanged checkout: %q", stderr)
	}
	again := &pb.Workspace{}
	c.ok(again, "workspace", "show", "-s", S)
	if again.Id != ws.Id {
		t.Errorf("later runs went to workspace %s, want the session's %s", again.Id, ws.Id)
	}

	// wait reports what happened to the session's commands, each event once.
	var events []*pb.SessionEvent
	c.eventually("the session's events", func() bool {
		res := &pb.SessionEvents{}
		c.ok(res, "poll", "-s", S, "--timeout", "1")
		events = append(events, res.Events...)
		return countWhat(events, "exited") == 3
	})
	if n := countWhat(events, "admitted"); n != 3 {
		t.Errorf("%d admitted events, want 3: %v", n, events)
	}
	last := events[len(events)-1].GetCommand()
	if last.GetWhat() != pb.CommandUpdate_WHAT_EXITED || last.GetExitCode() != 3 || last.GetState() != pb.CommandState_COMMAND_STATE_FAILED || last.GetWorkspaceId() != ws.Id {
		t.Errorf("last event = %v", last)
	}
	quiet := &pb.SessionEvents{}
	c.ok(quiet, "poll", "-s", S, "--timeout", "1")
	if len(quiet.Events) != 0 || quiet.Live != 0 {
		t.Errorf("wait with nothing new = %v", quiet)
	}

	// A detached command runs until stopped.
	srv := &pb.Command{}
	c.ok(srv, "run", "-s", S, "-r", "app", "--detach", "--port", "5173", "--", "sh", "-c", "echo serving; sleep 300")
	if srv.Url != "http://asahimbp:5173" || srv.Attached || srv.SessionId != S {
		t.Errorf("detached run = %v", srv)
	}
	c.eventually("the server's output", func() bool {
		_, out, _ := c.hotbed("logs", "-s", S, srv.Id)
		return out == "serving\n"
	})
	ps := &pb.ListCommandsResponse{}
	c.ok(ps, "ps", "-s", S)
	if !containsLive(ps.Commands, srv.Id) {
		t.Errorf("ps = %v, want %s live", ps.Commands, srv.Id)
	}
	stopped := &pb.Command{}
	c.ok(stopped, "stop", "-s", S, srv.Id)
	if stopped.State != pb.CommandState_COMMAND_STATE_CANCELLED {
		t.Errorf("after stop: %v", stopped)
	}

	// Another session reads this one's commands, but cannot stop them.
	other := &pb.Session{}
	c.ok(other, "session", "start", "quiet-heron")
	if code, out, errOut := c.hotbed("logs", "-s", "quiet-heron", srv.Id); code != 0 || out != "serving\n" {
		t.Errorf("logs from another session: exit %d, %q (stderr %q), want serving", code, out, errOut)
	}
	if _, e := c.fails("stop", "-s", "quiet-heron", srv.Id); e.Code != api.CodeNotFound {
		t.Errorf("stop from another session: %+v", e)
	}
	if _, e := c.fails("session", "start", "quiet-heron"); e.Code != api.CodeConflict {
		t.Errorf("start of a taken name: %+v", e)
	}

	// The cockpit sees it all.
	snap := &pb.Snapshot{}
	c.ok(snap, "snapshot")
	if len(snap.Repos) != 1 || snap.Repos[0].Units != 1 || snap.Repos[0].ColdBuildSeconds <= 0 {
		t.Errorf("snapshot repos = %v", snap.Repos)
	}
	if len(snap.Sessions) != 2 || snap.Totals24H.Forks != 1 {
		t.Errorf("snapshot sessions %v, totals %v", snap.Sessions, snap.Totals24H)
	}

	ended := &pb.Session{}
	c.ok(ended, "session", "end", "-s", S)
	if ended.EndedAt == nil || ended.EndReason != "ended by its agent" || len(ended.WorkspaceIds) != 0 {
		t.Errorf("session end = %v", ended)
	}
	if _, e := c.fails("run", "-s", S, "-r", "app", "--", "true"); e.Code != api.CodeNotFound {
		t.Errorf("run in an ended session: %+v", e)
	}
}

func TestRemoteOnlyWorkspace(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	sess := &pb.Session{}
	c.ok(sess, "session", "start", "brave-otter")

	ws := c.workspace("brave-otter", nil)
	if len(ws.Repos) != 1 || ws.Repos[0].SyncDir != "" {
		t.Fatalf("a workspace no checkout synced to = %v, want app following no checkout", ws)
	}
	// The agent edits and commits in the workspace itself; it persists.
	for _, args := range [][]string{
		{"sh", "-c", "printf 'remote\\n' > src/lib.txt"},
		{"git", "-c", "user.name=a", "-c", "user.email=a@a", "commit", "-qam", "remote edit"},
	} {
		if code, out, errOut := c.hotbed(append([]string{"run", "-s", "brave-otter", "-r", "app", "--"}, args...)...); code != 0 {
			t.Fatalf("run %v: exit %d %q %q", args, code, out, errOut)
		}
	}
	code, stdout, _ := c.hotbed("run", "-s", "brave-otter", "-r", "app", "--", "git", "log", "-1", "--format=%s")
	if code != 0 || stdout != "remote edit\n" {
		t.Errorf("git log in the workspace: exit %d, %q", code, stdout)
	}
	if _, e := c.fails("sync", "-s", "brave-otter", "-r", "app"); e.Code != api.CodeInvalidArgs {
		t.Errorf("sync of a remote-only workspace: %+v", e)
	}
}

func TestAbandonedSessionEnds(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "idle-yak")
	c.workspace("idle-yak", nil)
	srv := &pb.Command{}
	c.ok(srv, "run", "-s", "idle-yak", "-r", "app", "--detach", "--", "sleep", "300")
	c.heartbeat() // the run counts as using the session now, not after the clock moves

	c.clock.advance(59 * time.Minute)
	if err := c.server.ReapIdle(t.Context()); err != nil {
		t.Fatal(err)
	}
	res := &pb.SessionEvents{}
	c.ok(res, "poll", "-s", "idle-yak", "--timeout", "1") // keeps it alive for another hour
	if res.Ended {
		t.Fatalf("session ended before an hour idle: %v", res)
	}

	c.clock.advance(61 * time.Minute)
	c.online() // a session on an unreachable worker waits for it
	if err := c.server.ReapIdle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, e := c.fails("poll", "-s", "idle-yak"); e.Code != api.CodeNotFound || !strings.Contains(e.Message, "abandoned") {
		t.Errorf("wait on an abandoned session: %+v", e)
	}
	c.eventually("the abandoned session's command to stop", func() bool {
		cmd, ok := c.engine().Command(srv.Id)
		return ok && cmd.State == pb.CommandState_COMMAND_STATE_CANCELLED
	})
}

// An ended session is taken up again by name with --resume: live again,
// in its own project, with no workspaces; a live one is as it is. Without
// --resume, its name is taken.
func TestSessionResumes(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "back-bear")
	c.workspace("back-bear", nil)
	c.ok(nil, "session", "end", "-s", "back-bear")

	if _, e := c.fails("session", "start", "back-bear"); e.Code != api.CodeConflict {
		t.Errorf("start an ended session's name = %v, want CONFLICT", e)
	}
	if _, e := c.fails("session", "start", "--resume"); e.Code != api.CodeInvalidArgs {
		t.Errorf("resume no session = %v, want INVALID_ARGS", e)
	}
	c.ok(nil, "project", "add", "other")
	if _, e := c.fails("session", "start", "back-bear", "--resume", "-p", "other"); e.Code != api.CodeConflict || !strings.Contains(e.Message, "works on project app, not other") {
		t.Errorf("resume in another project = %v, want CONFLICT", e)
	}

	resumed := &pb.Session{}
	c.ok(resumed, "session", "start", "back-bear", "--resume")
	if resumed.Id != "back-bear" || resumed.EndedAt != nil || resumed.Project != "app" {
		t.Fatalf("resumed = %v, want back-bear live in project app", resumed)
	}
	res := &pb.SessionEvents{}
	c.ok(res, "poll", "-s", "back-bear", "--timeout", "1")
	if res.Ended {
		t.Errorf("wait on a resumed session = %v, want it live", res)
	}
	ws := &pb.ListWorkspacesResponse{}
	c.ok(ws, "workspace", "ls", "-s", "back-bear")
	if len(ws.Workspaces) != 0 {
		t.Errorf("resumed session's workspaces = %v, want none: they went with its end", ws.Workspaces)
	}
	again := &pb.Session{}
	c.ok(again, "session", "start", "back-bear", "--resume")
	if again.StartedAt != resumed.StartedAt || again.EndedAt != nil {
		t.Errorf("resume a live session = %v, want it as it was", again)
	}
}

// An attached hotbed run keeps its session alive for as long as it runs,
// however long: an agent waiting on it needs no hotbed poll loop beside it.
func TestAttachedRunKeepsSessionAlive(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "busy-elk")
	c.workspace("busy-elk", nil)
	flag := filepath.Join(t.TempDir(), "go")
	out, w := io.Pipe()
	done := make(chan int, 1)
	go func() {
		d := c.cli
		d.stdin, d.stdout, d.stderr = strings.NewReader(""), w, io.Discard
		code := run(t.Context(), []string{"run", "-s", "busy-elk", "-r", "app", "--", "sh", "-c", "echo started; " + waitFor(flag, "echo finished")}, d)
		w.Close()
		done <- code
	}()
	lines := bufio.NewScanner(out)
	if !lines.Scan() || lines.Text() != "started" {
		t.Fatalf("attached run's first line = %q (%v)", lines.Text(), lines.Err())
	}

	// Its output streams, so it follows the command. Two idle hours pass,
	// each ending in a heartbeat: only the follow, not a one-off call,
	// can keep the session alive through both.
	for range 2 {
		c.clock.advance(61 * time.Minute)
		c.heartbeat()
		if err := c.server.ReapIdle(t.Context()); err != nil {
			t.Fatal(err)
		}
		if s := c.session("busy-elk"); s.EndedAt != nil {
			t.Fatalf("session ended while an attached run followed its command: %s", s.EndReason)
		}
	}

	if err := os.WriteFile(flag, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !lines.Scan() || lines.Text() != "finished" {
		t.Errorf("attached run's last line = %q (%v)", lines.Text(), lines.Err())
	}
	if code := <-done; code != 0 {
		t.Errorf("attached run exited %d", code)
	}
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
		ws := c.workspace(session, nil)
		if ws.State != pb.WorkspaceState_WORKSPACE_STATE_IDLE {
			t.Fatalf("add in %s = %v, want a ready, idle workspace", session, ws)
		}
		return ws
	}

	first := add("first-ant")
	g1 := c.base(first.BaseId)
	if g1.Generation != 1 || !g1.Cold || len(g1.Repos) != 1 || g1.Repos[0].Commit != gitIn(t, origin, "rev-parse", "HEAD") {
		t.Fatalf("first base = %v, want generation 1, cold, at the branch's head", g1)
	}

	if again := add("second-bee"); again.BaseId != first.BaseId {
		t.Errorf("add with the branch unmoved forked %s, want %s", again.BaseId, first.BaseId)
	}

	write(t, filepath.Join(origin, "src/lib.txt"), "moved\n")
	gitIn(t, origin, "commit", "--quiet", "-am", "Move the branch")
	moved := add("third-cat")
	g2 := c.base(moved.BaseId)
	if g2.Generation != 2 || g2.Cold || len(g2.Repos) != 1 || g2.Repos[0].Commit != gitIn(t, origin, "rev-parse", "HEAD") {
		t.Fatalf("base after the branch moved = %v, want generation 2, incremental, at the new head", g2)
	}
	code, out, _ := c.hotbed("run", "-s", "third-cat", "-r", "app", "--", "cat", "src/lib.txt")
	if code != 0 || out != "moved\n" {
		t.Errorf("the new base's checkout: exit %d, %q", code, out)
	}

	c.ok(nil, "project", "rebuild", "app")
	rebuilt := add("fourth-dog")
	g3 := c.base(rebuilt.BaseId)
	if g3.Generation != 3 || !g3.Cold {
		t.Fatalf("base after a rebuild = %v, want generation 3, from nothing", g3)
	}
	if after := add("fifth-eel"); after.BaseId != rebuilt.BaseId {
		t.Errorf("add after the rebuild's base forked %s, want %s", after.BaseId, rebuilt.BaseId)
	}
}

// A project holding a remote no repo has can have no workspace made, so its
// bases are not kept: they go once no workspace uses them.
func TestRemovedRepoBasesAreDeleted(t *testing.T) {
	c := newCluster(t)
	origin := upstream(t)
	c.addRepo(origin)
	c.ok(nil, "session", "start", "last-owl")
	ws := c.workspace("last-owl", nil)
	c.base(ws.BaseId)
	c.ok(nil, "session", "end", "-s", "last-owl")
	c.ok(nil, "repo", "rm", "app")
	c.ok(nil, "session", "start", "next-owl")
	if _, e := c.tryWorkspace("next-owl", nil); e == nil || e.Code != api.CodeNotFound || !strings.Contains(e.Message, "which no repo of the cluster has") {
		t.Errorf("a workspace of a project whose repo is gone: %v, want NOT_FOUND naming the remote", e)
	}
	// The heartbeat's reply tells the worker the repo is gone and its
	// workspace released. Sent now, rather than waiting on the worker's own
	// heartbeat, which a loaded machine's retries can push past the wait
	// below (board 7089).
	c.heartbeat()
	c.eventually("the controller to see the repo's bases gone", func() bool {
		snap, err := c.asWorker.GetSnapshot(c.t.Context(), &pb.GetSnapshotRequest{})
		if err != nil {
			c.t.Fatal(err)
		}
		return len(snap.Bases) == 0
	})
	if _, err := os.Stat(c.nodes[0].store.Path("bases/app/g1")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the base's volume is still there: %v", err)
	}
}

func TestRunOfAnUnreachableRepoIsRetryable(t *testing.T) {
	c := newCluster(t)
	c.addProject("/nonexistent/repo")
	c.ok(nil, "session", "start", "eager-gnu")
	code, e := c.fails("run", "-s", "eager-gnu", "-r", "app", "--", "true")
	if code != exitError || e.Code != api.CodeRepoUnreachable || !e.Retryable {
		t.Errorf("run of an unreachable repo: exit %d, %+v", code, e)
	}
}

func TestRunFailsWhenFreshenFails(t *testing.T) {
	c := newCluster(t)
	c.addProject(upstream(t))
	c.ok(nil, "step", "set", "-p", "app", "build", "--every-pull", "--", "echo broken build >&2; exit 3")
	c.ok(nil, "session", "start", "eager-gnu")
	code, e := c.fails("run", "-s", "eager-gnu", "-r", "app", "--", "true")
	if code != exitError || e.Code != api.CodeWorkspaceFailed || e.Retryable || !strings.Contains(e.Message, "failed at step build") {
		t.Errorf("run with a failing freshen: exit %d, %+v", code, e)
	}
}

func countWhat(events []*pb.SessionEvent, what string) int {
	n := 0
	for _, e := range events {
		if w, _ := api.EventWhat(e); w == what {
			n++
		}
	}
	return n
}

func containsLive(cmds []*pb.Command, id string) bool {
	for _, c := range cmds {
		if c.Id == id && api.Live(c.State) {
			return true
		}
	}
	return false
}

var sessionNameRE = regexpMust(`^[a-z]+-[a-z]+$`)

func regexpMust(s string) *regexp.Regexp { return regexp.MustCompile(s) }

// workerHandler serves an engine as a worker does: to the owner's
// machines, over the fake tailnet (loopback).
func workerHandler(tn *tailnettest.Fake, eng *worker.Engine) http.Handler {
	return worker.Handler(tn, httpx.Policy{Owner: "ramon@", Self: loopback}, eng)
}

var loopback = []netip.Addr{netip.MustParseAddr("127.0.0.1")}

// runs counts the lines a step appended to a file in the workspace's cache.
func (c *cluster) runs(session, file string) int {
	c.t.Helper()
	code, out, stderr := c.hotbed("run", "-s", session, "-r", "app", "--", "sh", "-c", `cat "$XDG_CACHE_HOME/`+file+`" 2>/dev/null || true`)
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
	c.addProject(origin)
	c.ok(nil, "step", "set", "-p", "app", "tool", "--", `mkdir -p "$XDG_CACHE_HOME" && echo run >> "$XDG_CACHE_HOME/tool"`)
	c.ok(nil, "step", "set", "-p", "app", "build", "--every-pull", "--", `mkdir -p "$XDG_CACHE_HOME" && echo run >> "$XDG_CACHE_HOME/build"`)
	add := func(session string) *pb.Workspace {
		t.Helper()
		c.ok(nil, "session", "start", session)
		ws := c.workspace(session, nil)
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

	c.ok(nil, "step", "set", "-p", "app", "tool", "--", `mkdir -p "$XDG_CACHE_HOME" && echo run v2 >> "$XDG_CACHE_HOME/tool"`)
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
		c.ok(ls, "step", "ls", "-p", "app")
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
	code, out, _ := c.hotbed("step", "show", "-p", "app", "tool")
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
	c.workspace("keen-elk", nil)

	project := &pb.Project{}
	c.ok(project, "step", "add", "-s", "keen-elk", "tool", "--", `mkdir -p "$HOTBED_BIN" && printf '#!/bin/sh\necho tool works\n' > "$HOTBED_BIN/tool" && chmod +x "$HOTBED_BIN/tool"`)
	if len(project.Steps) != 2 || project.Steps[1].Name != "tool" || len(project.Steps[1].Hash) != 12 || project.Steps[1].UpdatedBy != "laptop" {
		t.Fatalf("steps after add = %v", project.Steps)
	}
	if code, out, _ := c.hotbed("run", "-s", "keen-elk", "-r", "app", "--", "tool"); code != 0 || out != "tool works\n" {
		t.Errorf("the tool in the agent's workspace: exit %d, %q", code, out)
	}

	code, e := c.fails("step", "add", "-s", "keen-elk", "once", "--", `mkdir -p "$XDG_DATA_HOME" && mkdir "$XDG_DATA_HOME/once-only"`)
	if code != exitError || e.Code != api.CodeStepNotIdempotent {
		t.Errorf("a step that fails when repeated: exit %d, %+v", code, e)
	}
	code, e = c.fails("step", "add", "-s", "keen-elk", "broken", "--", "exit 4")
	if code != exitError || e.Code != api.CodeStepFailed || !strings.Contains(e.Message, "exited 4") {
		t.Errorf("a failing step: exit %d, %+v", code, e)
	}
	ls := &pb.ListStepsResponse{}
	c.ok(ls, "step", "ls", "-p", "app")
	var names []string
	for _, s := range ls.Steps {
		names = append(names, s.Step.Name)
	}
	if !slices.Equal(names, []string{"build", "tool"}) {
		t.Errorf("saved steps = %v, want only build and tool", names)
	}

	// A new workspace's base ran the step, so it has the tool too.
	c.ok(nil, "session", "start", "next-fox")
	ws := c.workspace("next-fox", nil)
	if code, out, _ := c.hotbed("run", "-s", "next-fox", "-r", "app", "--", "tool"); ws.BaseId != "app-g2" || code != 0 || out != "tool works\n" {
		t.Errorf("the tool in a new workspace on %s: exit %d, %q", ws.BaseId, code, out)
	}
}

// A step that breaks the freshen does not stop work: workspaces get the
// newest warm base with a note, and the broken recipe is not built again.
func TestFailingStepFallsBackToTheWarmBase(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "old-gnu")
	c.workspace("old-gnu", nil)

	c.ok(nil, "step", "set", "-p", "app", "broken", "--", "echo nope >&2; exit 5")
	for _, s := range []string{"new-hen", "late-jay"} {
		c.ok(nil, "session", "start", s)
		ws := c.workspace(s, nil)
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

// Each command has a tmp of its own on its workspace's volume, sticky, gone
// once it ends. The base's freshen littered its own, which was removed
// before the base was snapshotted, so none of it is in the fork. Its /tmp
// being that tmp is the real unit's (internal/unit's systemd tests): here it
// is only TMPDIR.
func TestCommandsHaveTheirOwnTmp(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "step", "set", "-p", "app", "litter", "--every-pull", "--", `mkdir "$TMPDIR/litter" && cd "$TMPDIR/litter" && seq 1 3000 | xargs touch`)
	c.ok(nil, "session", "start", "tidy-owl")
	ws := c.workspace("tidy-owl", nil)
	code, out, _ := c.hotbed("run", "-s", "tidy-owl", "-r", "app", "--", "sh", "-c", `echo "$HOTBED_COMMAND"; echo "$TMPDIR"; ls -ld "$TMPDIR" | cut -c1-10; ls "$(dirname "$TMPDIR")"; echo store > "$TMPDIR/store"`)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 4 {
		t.Fatalf("run: exit %d, %q", code, out)
	}
	id := lines[0]
	want := []string{id, filepath.Join(c.nodes[0].store.Path("ws/"+ws.Id), ".tmp", id), "drwxrwxrwt", id}
	if !slices.Equal(lines, want) {
		t.Fatalf("run printed %q, want %q: its own sticky tmp in its volume, alone there", lines, want)
	}
	c.eventually("the command's tmp to go", func() bool {
		_, err := os.Stat(lines[1])
		return errors.Is(err, os.ErrNotExist)
	})
}

type flushNotifyWriter struct {
	http.ResponseWriter
	onFlush func()
}

func (w *flushNotifyWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	if w.onFlush != nil {
		w.onFlush()
	}
}

func (w *flushNotifyWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// A deploy restarts the worker under a run waiting for its workspace: the
// watch drops, and run watches again rather than failing.
func TestRunWatchesAgainWhenTheStreamDrops(t *testing.T) {
	c := newCluster(t)
	c.addProject(upstream(t))
	flag := filepath.Join(t.TempDir(), "go")
	c.ok(nil, "step", "set", "-p", "app", "build", "--every-pull", "--", waitFor(flag, "true"))
	c.ok(nil, "session", "start", "calm-yak")

	var watches int
	var watchMu sync.Mutex
	var watchingOnce sync.Once
	watching := make(chan struct{})
	reconnected := make(chan struct{})
	origWorker := *c.nodes[0].handler.Load()
	var wrapped http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/WatchWorkspace") {
			watchMu.Lock()
			watches++
			n := watches
			if n == 2 {
				close(reconnected)
			}
			watchMu.Unlock()
			if n == 1 {
				w = &flushNotifyWriter{
					ResponseWriter: w,
					onFlush:        func() { watchingOnce.Do(func() { close(watching) }) },
				}
			}
		}
		origWorker.ServeHTTP(w, r)
	})
	c.nodes[0].handler.Store(&wrapped)

	type result struct {
		code           int
		stdout, stderr string
	}
	done := make(chan result, 1)
	go func() {
		code, out, errOut := c.hotbed("run", "-s", "calm-yak", "-r", "app", "--", "echo", "ran")
		done <- result{code, out, errOut}
	}()
	// Wait until the run has established its initial WatchWorkspace stream and
	// received the first flushed preparing update before dropping connections.
	// Waiting only for the controller snapshot to show PREPARING races ahead of
	// WatchWorkspace arriving (or drops CreateWorkspace mid-flight) (board 9276).
	select {
	case <-watching:
	case r := <-done:
		write(t, flag, "")
		t.Fatalf("run exited before establishing WatchWorkspace: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	case <-time.After(30 * time.Second):
		write(t, flag, "")
		t.Fatal("run never established initial WatchWorkspace")
	}
	c.nodes[0].srv.CloseClientConnections()
	select {
	case <-reconnected:
	case r := <-done:
		write(t, flag, "")
		t.Fatalf("run exited instead of re-watching after stream drop: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	case <-time.After(15 * time.Second):
		write(t, flag, "")
		t.Fatal("run never re-established WatchWorkspace after stream drop")
	}
	write(t, flag, "")
	r := <-done
	if r.code != 0 || r.stdout != "ran\n" || !strings.Contains(r.stderr, "lost the watch") {
		t.Errorf("run: exit %d, stdout %q, stderr %q; want it to run after watching again", r.code, r.stdout, r.stderr)
	}
}

// The cockpit shows sessions that ended, with the workspaces they had.
func TestSnapshotKeepsEndedSessions(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "spent-owl")
	ws := c.workspace("spent-owl", nil)
	c.ok(nil, "session", "end", "-s", "spent-owl")
	snap := &pb.Snapshot{}
	c.ok(snap, "snapshot")
	var got *pb.Session
	for _, s := range snap.Sessions {
		if s.Id == "spent-owl" {
			got = s
		}
	}
	if got == nil || got.EndedAt == nil || got.EndReason != "ended by its agent" || len(got.Workspaces) != 1 ||
		got.Workspaces[0].Id != ws.Id || got.Project != "app" || got.Workspaces[0].ReleasedAt == nil {
		t.Errorf("snapshot's session = %v", got)
	}
}

// commandStates is the session's commands' states, by ID.
func (c *cluster) commandStates(session string) map[string]pb.CommandState {
	c.t.Helper()
	ps := &pb.ListCommandsResponse{}
	c.ok(ps, "ps", "-s", session)
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
	c.workspace("full-yak", nil)
	first, second := &pb.Command{}, &pb.Command{}
	c.ok(first, "run", "-s", "full-yak", "-r", "app", "--detach", "--kind", "build", "--", "sleep", "300")
	c.ok(second, "run", "-s", "full-yak", "-r", "app", "--detach", "--kind", "build", "--", "sleep", "300")
	c.eventually("both to run", func() bool {
		st := c.commandStates("full-yak")
		return st[first.Id] == pb.CommandState_COMMAND_STATE_RUNNING && st[second.Id] == pb.CommandState_COMMAND_STATE_RUNNING
	})

	c.nodes[0].system.SetUsed(c.nodes[0].system.Mem.Total * 95 / 100)
	c.eventually("the newest to be parked", func() bool {
		return c.commandStates("full-yak")[second.Id] == pb.CommandState_COMMAND_STATE_PARKED
	})
	// The engine steps every second: three steps at 95% leave the other running.
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if st := c.commandStates("full-yak")[first.Id]; st != pb.CommandState_COMMAND_STATE_RUNNING {
			t.Fatalf("the last running command went %s", st)
		}
	}

	c.ok(nil, "stop", "-s", "full-yak", first.Id)
	c.eventually("the parked one to run again, memory still short", func() bool {
		return c.commandStates("full-yak")[second.Id] == pb.CommandState_COMMAND_STATE_RUNNING
	})
}

// A base build no workspace waits for any more, with a newer generation of
// its repo after it, is dropped: it would only hold memory and CPUs, and
// on a full machine be parked forever.
func TestSupersededBaseBuildIsDropped(t *testing.T) {
	c := newCluster(t)
	c.addProject(upstream(t))
	c.ok(nil, "step", "set", "-p", "app", "build", "--every-pull", "--", "sleep 30")
	c.ok(nil, "session", "start", "slow-ant")
	c.ok(nil, "run", "-s", "slow-ant", "-r", "app", "--detach", "--", "true")
	waiting := &pb.Workspace{}
	c.ok(waiting, "workspace", "show", "-s", "slow-ant")
	if waiting.BaseId != "app-g1" || waiting.State != pb.WorkspaceState_WORKSPACE_STATE_PREPARING {
		t.Fatalf("first workspace = %v, want one waiting for app-g1", waiting)
	}
	c.ok(nil, "step", "set", "-p", "app", "build", "--every-pull", "--", "true")
	c.ok(nil, "session", "start", "fast-bee")
	ws := c.workspace("fast-bee", nil)
	if ws.BaseId != "app-g2" {
		t.Fatalf("second workspace forked %s, want app-g2", ws.BaseId)
	}

	c.ok(nil, "session", "end", "-s", "slow-ant")
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

// The CLI syncs, runs and follows output on the workspace's worker itself:
// the controller only says where the workspace is.
func TestCLITalksToTheWorkerDirectly(t *testing.T) {
	c := newCluster(t)
	origin := upstream(t)
	c.addRepo(origin)
	c.ok(nil, "session", "start", "near-fox")
	agent := filepath.Join(t.TempDir(), "agent")
	gitIn(t, filepath.Dir(agent), "clone", "--quiet", origin, agent)
	t.Chdir(agent)
	write(t, filepath.Join(agent, "src/lib.txt"), "edited\n")
	calls := c.ctrl.record()
	code, stdout, stderr := c.hotbed("run", "-s", "near-fox", "--", "cat", "src/lib.txt")
	if code != 0 || stdout != "edited\n" || !strings.Contains(stderr, "synced 1 changed files") {
		t.Fatalf("run: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if got := calls(); !slices.Equal(got, []string{"CreateWorkspace", "ListWorkspaces"}) {
		t.Errorf("run called the controller's %v, want only CreateWorkspace, to place the run, and ListWorkspaces, to tell the repo", got)
	}

	srv := &pb.Command{}
	c.ok(srv, "run", "-s", "near-fox", "-r", "app", "--detach", "--", "sh", "-c", "echo up; sleep 300")
	calls = c.ctrl.record()
	c.eventually("the server's output", func() bool {
		_, out, _ := c.hotbed("logs", "-s", "near-fox", srv.Id)
		return out == "up\n"
	})
	c.ok(nil, "stop", "-s", "near-fox", srv.Id)
	if got := calls(); !slices.Equal(got, []string{"GetCommand"}) {
		t.Errorf("logs and stop called the controller's %v, want only GetCommand", got)
	}
}

// With the controller out of reach, an agent keeps running commands in its
// workspace; what happened reaches the session once the controller is
// back, and the session, used meanwhile, is not abandoned.
func TestWorkGoesOnWithoutTheController(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "lone-gnu")
	c.workspace("lone-gnu", nil)
	cursor := &pb.SessionEvents{}
	c.ok(cursor, "poll", "-s", "lone-gnu", "--timeout", "1")

	c.ctrl.setDown(true)
	code, stdout, stderr := c.hotbed("run", "-s", "lone-gnu", "-r", "app", "--", "sh", "-c", "echo alone; exit 4")
	if code != 4 || stdout != "alone\n" {
		t.Fatalf("run with the controller down: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if _, e := c.fails("ps", "-s", "lone-gnu"); e.Code != api.CodeControllerUnreachable {
		t.Errorf("ps with the controller down: %+v", e)
	}
	c.clock.advance(61 * time.Minute)
	c.ctrl.setDown(false)
	c.online()
	if err := c.server.ReapIdle(t.Context()); err != nil {
		t.Fatal(err)
	}
	var events []*pb.SessionEvent
	c.eventually("the command run meanwhile to reach the session", func() bool {
		res := &pb.SessionEvents{}
		c.ok(res, "poll", "-s", "lone-gnu", "--timeout", "1")
		if res.Ended {
			t.Fatalf("the session was abandoned while it was used: %v", res)
		}
		events = append(events, res.Events...)
		return countWhat(events, "exited") == 1
	})
	if last := events[len(events)-1].GetCommand(); last.GetExitCode() != 4 {
		t.Errorf("exited event = %v, want exit 4", last)
	}
}

// A base's freshen log, which a failed run names, can be read from the
// session.
func TestFreshenLogIsReadable(t *testing.T) {
	c := newCluster(t)
	c.addProject(upstream(t))
	c.ok(nil, "step", "set", "-p", "app", "build", "--every-pull", "--", "echo broken build >&2; exit 3")
	c.ok(nil, "session", "start", "keen-ox")
	_, e := c.fails("run", "-s", "keen-ox", "-r", "app", "--", "true")
	m := regexp.MustCompile(`its log is command (f-\S+)\)`).FindStringSubmatch(e.Message)
	if m == nil {
		t.Fatalf("run's error names no log: %+v", e)
	}
	code, out, stderr := c.hotbed("logs", "-s", "keen-ox", m[1])
	if code != 0 || !strings.Contains(out, "broken build") {
		t.Errorf("logs of the freshen: exit %d, %q, stderr %q", code, out, stderr)
	}
}
