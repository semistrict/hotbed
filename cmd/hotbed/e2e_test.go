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
	"strings"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/controller"
	"github.com/semistrict/hotbed/internal/fake"
	"github.com/semistrict/hotbed/internal/httpx"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/tailnet"
	"github.com/semistrict/hotbed/internal/tailnet/tailnettest"
	"github.com/semistrict/hotbed/internal/worker"
)

var asahi = tailnet.Peer{NodeID: "nAsahi", Hostname: "asahimbp", Login: "ramon@"}

// cluster is a controller and one worker in this process: the real
// controller, engine and CLI, with fakes only where Linux and root would be
// needed (volumes are copied, commands are local processes).
type cluster struct {
	t        *testing.T
	tn       *tailnettest.Fake
	id       string
	ctrl     string // controller host:port
	engine   *worker.Engine
	sys      *fake.System
	cli      deps
	asWorker *api.Client
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

	sys := fake.NewSystem()
	eng, err := worker.NewEngine(t.Context(), worker.EngineConfig{
		Dir: filepath.Join(t.TempDir(), "worker"), NodeID: asahi.NodeID, Host: asahi.Hostname,
		Store: &fake.Store{Root: t.TempDir()}, Units: &fake.Units{}, System: sys,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	workerSrv := httptest.NewServer(httpx.Owner(tn, "ramon@", eng.Handler()))
	t.Cleanup(workerSrv.Close)

	ctrlSrv := httptest.NewServer(controller.NewServer(controller.ServerConfig{
		Store: store, Identifier: tn, Cluster: *cfg.Controller, Controller: "ubu1",
		Workers:    tn.Client(ubu1.Peer),
		WorkerAddr: func(string) string { return strings.TrimPrefix(workerSrv.URL, "http://") },
	}).Handler())
	t.Cleanup(ctrlSrv.Close)
	ctrl := strings.TrimPrefix(ctrlSrv.URL, "http://")

	asWorker, err := api.NewClient(ctrl, tn.Client(asahi))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := asWorker.Join(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { eng.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	c := &cluster{t: t, tn: tn, id: init.ClusterID, ctrl: ctrl, engine: eng, sys: sys, asWorker: asWorker}
	c.cli = deps{tailnet: tn, http: tn.Client(laptop), stream: tn.Client(laptop)}
	t.Setenv("HOTBED_CONTROLLER", ctrl)
	return c
}

// report sends the worker's report, as its report loop does.
func (c *cluster) report() {
	c.t.Helper()
	r, err := c.engine.Report(c.id)
	if err != nil {
		c.t.Fatal(err)
	}
	reply, err := c.asWorker.Report(c.t.Context(), r)
	if err != nil {
		c.t.Fatal(err)
	}
	c.engine.Acknowledged(reply.Ack)
	c.engine.SetRepos(reply.Repos)
}

// eventually reports until cond holds; the engine works on its own clock.
func (c *cluster) eventually(what string, cond func() bool) {
	c.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		c.report()
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.t.Fatalf("timed out waiting for %s", what)
}

// hotbed runs the CLI against the cluster and returns its exit status,
// stdout and stderr.
func (c *cluster) hotbed(args ...string) (int, string, string) {
	c.t.Helper()
	var stdout, stderr bytes.Buffer
	d := c.cli
	d.stdout, d.stderr = &stdout, &stderr
	code := run(c.t.Context(), args, d)
	return code, stdout.String(), stderr.String()
}

// envelope runs the CLI and decodes a successful envelope's data into v.
func (c *cluster) envelope(v any, args ...string) {
	c.t.Helper()
	code, stdout, stderr := c.hotbed(args...)
	var out output
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		c.t.Fatalf("hotbed %v printed %q (stderr %q)", args, stdout, stderr)
	}
	if code != 0 || !out.OK {
		c.t.Fatalf("hotbed %v: exit %d, error %+v", args, code, out.Error)
	}
	if v != nil {
		if err := json.Unmarshal(out.Data, v); err != nil {
			c.t.Fatal(err)
		}
	}
}

func git(t *testing.T, dir string, args ...string) string {
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
	git(t, dir, "init", "--quiet", "-b", "main")
	write(t, filepath.Join(dir, "src/lib.txt"), "base\n")
	write(t, filepath.Join(dir, "src/keep.txt"), "keep\n")
	write(t, filepath.Join(dir, ".gitignore"), "target/\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "--quiet", "-m", "First commit")
	return dir
}

const freshen = `mkdir -p target "$XDG_CACHE_HOME" && cp -R src target/ && echo "   Compiling app v0.1.0 (/work/app)" && echo warm > "$XDG_CACHE_HOME/marker"`

func TestEndToEnd(t *testing.T) {
	c := newCluster(t)
	origin := upstream(t)

	var repo api.Repo
	c.envelope(&repo, "repo", "add", "app", origin, "--freshen", freshen)
	if repo.ID != "app" || len(repo.Freshen) != 1 {
		t.Fatalf("repo add = %+v", repo)
	}

	// The worker learns of the repo from its next report and builds g1.
	var bases []api.Base
	c.eventually("a warm base", func() bool {
		r, err := c.engine.Report(c.id)
		if err != nil {
			t.Fatal(err)
		}
		bases = r.Bases
		return len(bases) == 1 && bases[0].State == api.BaseWarm
	})
	if b := bases[0]; b.Generation != 1 || !b.Cold || b.Message != "First commit" || b.Units != 1 {
		t.Errorf("base = %+v", b)
	}

	var ws api.Workspace
	c.envelope(&ws, "ws", "new", "app", "--agent", "claude/test")
	if ws.RepoID != "app" || ws.NodeID != asahi.NodeID || ws.Path != "/work/app" || ws.Agent != "claude/test" {
		t.Fatalf("ws new = %+v", ws)
	}

	// An agent's checkout with an uncommitted edit and a new file.
	agent := filepath.Join(t.TempDir(), "agent")
	git(t, filepath.Dir(agent), "clone", "--quiet", origin, agent)
	write(t, filepath.Join(agent, "src/lib.txt"), "edited\n")
	write(t, filepath.Join(agent, "src/new.txt"), "new\n")

	code, stdout, stderr := c.hotbed("exec", ws.ID, "--dir", agent, "--", "sh", "-c",
		`cat src/lib.txt src/new.txt target/src/keep.txt "$XDG_CACHE_HOME/marker"; pwd`)
	if code != 0 {
		t.Fatalf("exec: exit %d\nstdout %q\nstderr %q", code, stdout, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 5 || strings.Join(lines[:4], ",") != "edited,new,keep,warm" || !strings.HasSuffix(lines[4], "/ws/"+ws.ID+"/app") {
		t.Errorf("exec saw %q: want the agent's edit, its new file, the base's build and cache, in the workspace", stdout)
	}
	if !strings.Contains(stderr, "synced 2 changed files") {
		t.Errorf("exec stderr %q, want the sync reported", stderr)
	}

	// The same tree again: nothing to send.
	var again api.SyncResult
	c.envelope(&again, "sync", ws.ID, "--dir", agent)
	if !again.Skipped {
		t.Errorf("second sync = %+v, want skipped", again)
	}

	// exec passes the command's status through.
	if code, _, _ := c.hotbed("exec", ws.ID, "--no-sync", "--", "sh", "-c", "exit 3"); code != 3 {
		t.Errorf("exec exit 3 returned %d", code)
	}

	// A detached command shows up in ps, prints to logs, and stops.
	var srv api.Command
	c.envelope(&srv, "exec", ws.ID, "--no-sync", "--detach", "--port", "5173", "--", "sh", "-c", "echo serving; sleep 300")
	if srv.State != api.CommandQueued || srv.URL == nil || *srv.URL != "http://asahimbp:5173" || srv.Attached {
		t.Errorf("detached exec = %+v", srv)
	}
	c.eventually("the server running", func() bool {
		var ps struct{ Commands []api.Command }
		c.envelope(&ps, "ps")
		for _, cmd := range ps.Commands {
			if cmd.ID == srv.ID && cmd.State == api.CommandRunning {
				return true
			}
		}
		return false
	})
	c.eventually("the server's output", func() bool {
		_, out, _ := c.hotbed("logs", srv.ID)
		return out == "serving\n"
	})
	var stopped api.Command
	c.envelope(&stopped, "stop", srv.ID)
	if stopped.State != api.CommandCancelled {
		t.Errorf("after stop: %+v", stopped)
	}

	// The cockpit sees all of it.
	c.report()
	var snap api.Snapshot
	if err := c.asWorker.Do(t.Context(), http.MethodGet, "/v1/ui/snapshot", nil, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Nodes) != 1 || !snap.Nodes[0].Online || snap.Nodes[0].Machine != "Test Machine" {
		t.Errorf("snapshot nodes = %+v", snap.Nodes)
	}
	if len(snap.Repos) != 1 || snap.Repos[0].Units != 1 || snap.Repos[0].ColdBuildSeconds <= 0 {
		t.Errorf("snapshot repos = %+v", snap.Repos)
	}
	states := map[api.CommandState]int{}
	for _, h := range snap.History {
		states[h.State]++
	}
	if states[api.CommandSucceeded] != 1 || states[api.CommandFailed] != 1 || states[api.CommandCancelled] != 1 {
		t.Errorf("history states = %v, want one ok, one failed, one cancelled", states)
	}
	var ok api.FinishedCommand
	for _, h := range snap.History {
		if h.State == api.CommandSucceeded {
			ok = h
		}
	}
	if ok.ColdSeconds != nil {
		t.Errorf("a shell command got a cold time %v; only builds and tests do", *ok.ColdSeconds)
	}
	if snap.Totals24h.Forks != 1 || snap.Totals24h.OOMKills != 0 {
		t.Errorf("totals = %+v", snap.Totals24h)
	}
	kinds := map[string]bool{}
	for _, e := range snap.Events {
		kinds[e.Kind] = true
	}
	for _, k := range []string{"base", "fork", "admit", "finish", "fail"} {
		if !kinds[k] {
			t.Errorf("no %s event in %+v", k, snap.Events)
		}
	}

	// Files and their changes against the base.
	var files []api.FileEntry
	if err := c.asWorker.Do(t.Context(), http.MethodGet, "/v1/workspaces/"+ws.ID+"/files", nil, &files); err != nil {
		t.Fatal(err)
	}
	changes := map[string]string{}
	for _, f := range files {
		if f.Change != nil {
			changes[f.Path] = *f.Change
		}
	}
	if changes["src/lib.txt"] != "modified" || changes["src/new.txt"] != "added" || len(changes) != 2 {
		t.Errorf("file changes = %v", changes)
	}
	var file api.FileContent
	if err := c.asWorker.Do(t.Context(), http.MethodGet, "/v1/workspaces/"+ws.ID+"/files/src/lib.txt", nil, &file); err != nil {
		t.Fatal(err)
	}
	if file.Text != "edited\n" || len(file.ChangedLines) != 1 || file.ChangedLines[0] != 1 {
		t.Errorf("file = %+v", file)
	}

	var released map[string]string
	c.envelope(&released, "ws", "rm", ws.ID)
	c.report()
	var left struct{ Workspaces []api.Workspace }
	c.envelope(&left, "ws", "ls")
	if len(left.Workspaces) != 0 {
		t.Errorf("after ws rm: %+v", left.Workspaces)
	}
}

func TestExecBeforeAnyBaseIsRetryable(t *testing.T) {
	c := newCluster(t)
	c.envelope(nil, "repo", "add", "app", "/nonexistent/repo", "--freshen", "true")
	c.report()
	code, stdout, _ := c.hotbed("ws", "new", "app")
	var out output
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	if code != exitError || out.Error.Code != api.CodeNoWarmBase || !out.Error.Retryable {
		t.Errorf("ws new before a base: exit %d, %+v", code, out.Error)
	}
}
