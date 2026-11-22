package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
)

func TestNormalizeGitURL(t *testing.T) {
	cases := [][2]string{
		{"https://github.com/semistrict/hotbed.git", "github.com/semistrict/hotbed"},
		{"https://github.com/semistrict/hotbed", "github.com/semistrict/hotbed"},
		{"https://GITHUB.COM/semistrict/hotbed/", "github.com/semistrict/hotbed"},
		{"git@github.com:semistrict/hotbed.git", "github.com/semistrict/hotbed"},
		{"git@github.com:semistrict/hotbed", "github.com/semistrict/hotbed"},
		{"ssh://git@github.com/semistrict/hotbed.git", "github.com/semistrict/hotbed"},
		{"ssh://git@github.com:22/semistrict/hotbed.git", "github.com/semistrict/hotbed"},
	}
	for _, tc := range cases {
		if got := gitops.CanonicalRemote(tc[0]); got != tc[1] {
			t.Errorf("gitops.CanonicalRemote(%q) = %q, want %q", tc[0], got, tc[1])
		}
	}
}

func TestRunParseWithFlagsEnvAndRejectsPositionals(t *testing.T) {
	t.Setenv("HOTBED_SESSION", "")
	t.Setenv("HOTBED_REPO", "")
	cases := []struct {
		name        string
		args        []string
		wantSession string
		wantRepo    string
		wantArgs    []string
		wantDet     bool
	}{
		{
			name:     "defaulted session and repo before --",
			args:     []string{"run", "--", "go", "test", "-race", "./..."},
			wantArgs: []string{"go", "test", "-race", "./..."},
		},
		{
			name:        "flags before -- and nested -- after",
			args:        []string{"run", "-s", "s1", "-r", "hotbed", "--detach", "--", "go", "test", "--", "-run", "Foo"},
			wantSession: "s1",
			wantRepo:    "hotbed",
			wantArgs:    []string{"go", "test", "--", "-run", "Foo"},
			wantDet:     true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, _, exitCode, err := parse(tc.args, io.Discard)
			if err != nil || exitCode != nil {
				t.Fatalf("parse(%v): err=%v exitCode=%v", tc.args, err, exitCode)
			}
			rc, ok := cmd.(*runCmd)
			if !ok {
				t.Fatalf("got %T, want *runCmd", cmd)
			}
			if rc.Session != tc.wantSession || rc.Repo != tc.wantRepo || rc.Detach != tc.wantDet || !slices.Equal(rc.Args, tc.wantArgs) {
				t.Errorf("got session=%q repo=%q detach=%v args=%v; want session=%q repo=%q detach=%v args=%v",
					rc.Session, rc.Repo, rc.Detach, rc.Args, tc.wantSession, tc.wantRepo, tc.wantDet, tc.wantArgs)
			}
		})
	}

	// Positional session or repo arguments must be rejected on add/sync/ps/wait/session end.
	for _, bad := range [][]string{
		{"add", "s1", "app"},
		{"sync", "s1"},
		{"ps", "s1"},
		{"wait", "s1"},
		{"session", "end", "s1"},
		{"step", "ls", "app"},
	} {
		if _, _, _, err := parse(bad, io.Discard); err == nil {
			t.Errorf("parse(%v) succeeded, want error rejecting positional session/repo", bad)
		}
	}
}

// TestSessionAndRepoDefaultFromThreadWorktreeAndEnv verifies zero-flag usage
// inside a checkout using T3CODE_THREAD_ID, git worktree fallback, HOTBED_SESSION,
// and HOTBED_REPO.
func TestSessionAndRepoDefaultFromThreadWorktreeAndEnv(t *testing.T) {
	c := newCluster(t)
	t.Setenv("T3CODE_THREAD_ID", "thread-alpha-123")
	remote := upstream(t)
	// Register the repo under a different name than the local checkout folder
	// (like ~/src/fxdax -> cladefs).
	c.ok(&pb.Repo{}, "repo", "add", "cladefs", remote)

	checkout := filepath.Join(t.TempDir(), "fxdax")
	if _, err := (gitops.Git{}).Run(t.Context(), "clone", "--quiet", remote, checkout); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(checkout, "src")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	// 1. Start session with T3CODE_THREAD_ID set -> bound to thread in sessions.json.
	var sess pb.Session
	c.ok(&sess, "session", "start", "thread-fox")

	var cur sessionBinding
	c.ok(&cur, "session", "current")
	if cur.ID != "thread-fox" || cur.Source != "thread" || cur.Key != "thread-alpha-123" {
		t.Fatalf("session current = %+v, want thread-fox from thread", cur)
	}

	// Zero-flag add, run, sync, ps, wait, logs, stop, step commands!
	var ws pb.Workspace
	c.ok(&ws, "add", "--sync", checkout)
	if ws.RepoId != "cladefs" || ws.SessionId != "thread-fox" {
		t.Fatalf("add with defaults = %v, want cladefs in thread-fox", &ws)
	}

	code, stdout, stderr := c.hotbed("run", "--", "cat", "src/lib.txt")
	if code != 0 || stdout != "base\n" {
		t.Fatalf("run with defaults: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	var syncRes pb.SyncResponse
	c.ok(&syncRes, "sync")
	if syncRes.WorkspaceId != ws.Id {
		t.Fatalf("sync with defaults hit %q, want %q", syncRes.WorkspaceId, ws.Id)
	}

	var ps pb.ListCommandsResponse
	c.ok(&ps, "ps")
	if len(ps.Commands) == 0 {
		t.Fatalf("ps with defaults returned no commands")
	}

	c.ok(&pb.Repo{}, "step", "set", "check", "--", "true")
	var steps pb.ListStepsResponse
	c.ok(&steps, "step", "ls")
	if len(steps.Steps) != 1 || steps.Steps[0].Step.Name != "check" {
		t.Fatalf("step ls with defaults = %+v", steps.Steps)
	}
	code, showOut, showErr := c.hotbed("step", "show", "check")
	if code != 0 || showOut != "true\n" {
		t.Fatalf("step show with defaults: exit %d, out %q, err %q", code, showOut, showErr)
	}
	c.ok(&pb.Repo{}, "step", "rm", "check")

	// Ending session clears the thread binding.
	c.ok(&pb.Session{}, "session", "end")
	if _, e := c.fails("session", "current"); e.Code != api.CodeInvalidArgs {
		t.Errorf("session current after end = %+v, want INVALID_ARGS", e)
	}

	// 2. Without T3CODE_THREAD_ID, session binds to the git worktree root.
	t.Setenv("T3CODE_THREAD_ID", "")
	c.ok(&pb.Session{}, "session", "start", "wt-owl")
	c.ok(&cur, "session", "current")
	if cur.ID != "wt-owl" || cur.Source != "worktree" || cur.Key != gitops.CanonicalPath(checkout) {
		t.Fatalf("worktree session current = %+v, want wt-owl from worktree", cur)
	}

	// 3. HOTBED_SESSION + HOTBED_REPO override both thread and worktree, even outside a checkout.
	t.Chdir(t.TempDir())
	t.Setenv("HOTBED_SESSION", "wt-owl")
	t.Setenv("HOTBED_REPO", "cladefs")
	c.ok(&cur, "session", "current")
	if cur.ID != "wt-owl" || cur.Source != "env" {
		t.Fatalf("env session current = %+v, want wt-owl from env", cur)
	}
	c.ok(&ws, "add")
	if ws.RepoId != "cladefs" || ws.SessionId != "wt-owl" {
		t.Fatalf("add with HOTBED_SESSION+HOTBED_REPO = %v", &ws)
	}

	// Outside a checkout with HOTBED_REPO unset, step ls fails cleanly.
	t.Setenv("HOTBED_REPO", "")
	if _, e := c.fails("step", "ls"); e.Code != api.CodeInvalidArgs {
		t.Errorf("step ls outside checkout = %+v, want INVALID_ARGS", e)
	}
}

func TestUICommand(t *testing.T) {
	var opened []string
	d := fakeDeps()
	d.openURL = func(_ context.Context, u string) error {
		opened = append(opened, u)
		return nil
	}

	d.controller = at("ubu1")
	code, out := hotbed(t, d, "ui")
	if code != exitOK || !out.OK || out.Command != "ui" {
		t.Fatalf("ui: exit %d, envelope %+v", code, out)
	}
	var res uiResult
	if err := json.Unmarshal(out.Data, &res); err != nil {
		t.Fatal(err)
	}
	if res.URL != "http://ubu1:7120/" || !res.Opened || !slices.Equal(opened, []string{"http://ubu1:7120/"}) {
		t.Fatalf("ui res=%+v opened=%v", res, opened)
	}

	// Custom port + --no-open should not invoke the browser opener.
	opened = nil
	var buf bytes.Buffer
	d.stdout, d.stderr = &buf, io.Discard
	d.controller = at("ubu1:9000")
	if code := run(t.Context(), []string{"ui", "--no-open"}, d); code != exitOK {
		t.Fatalf("ui --no-open exit %d", code)
	}
	var env output
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(env.Data, &res); err != nil {
		t.Fatal(err)
	}
	if res.URL != "http://ubu1:9000/" || res.Opened || len(opened) != 0 {
		t.Fatalf("ui --no-open res=%+v opened=%v", res, opened)
	}
}

func TestSessionStatus(t *testing.T) {
	c := newCluster(t)
	up := upstream(t)
	c.addRepo(up)

	checkout := filepath.Join(t.TempDir(), "local-checkout")
	if _, err := (gitops.Git{}).Run(t.Context(), "clone", up, checkout); err != nil {
		t.Fatal(err)
	}
	t.Chdir(checkout)
	t.Setenv("T3CODE_THREAD_ID", "thread-status-test")

	// Reject positional session argument.
	if code, err := c.fails("session", "status", "tidy-owl"); code != exitInvalidArgs || err.Code != api.CodeInvalidArgs {
		t.Fatalf("positional session status: exit %d, %+v, want exit 2 INVALID_ARGS", code, err)
	}

	// No session bound locally: returns active=false, state="none" with exit 0.
	var st sessionStatusResult
	c.ok(&st, "session", "status")
	if st.Active || st.State != "none" || st.ID != "" {
		t.Fatalf("unbound session status = %+v, want active=false state=none", st)
	}

	// Start a session (bound to T3CODE_THREAD_ID), add a workspace, and run a command.
	var started pb.Session
	c.ok(&started, "session", "start")
	c.ok(nil, "add", "--sync", ".")
	if code, _, stderr := c.hotbed("run", "--", "sh", "-c", "echo hi"); code != 0 {
		t.Fatalf("run: exit %d, stderr %q", code, stderr)
	}
	c.eventually("command appears in controller view", func() bool {
		var cur sessionStatusResult
		c.ok(&cur, "session", "status")
		return len(cur.Commands) == 1 && cur.Commands[0].State == "succeeded"
	})

	// Advance clock by 5 minutes: session status must report idleSeconds >= 300
	// without resetting lastActiveAt.
	c.clock.advance(5 * time.Minute)
	var active1, active2 sessionStatusResult
	c.ok(&active1, "session", "status")
	c.clock.advance(1 * time.Minute)
	c.ok(&active2, "session", "status")

	if !active1.Active || active1.State != "active" || active1.ID != started.Id {
		t.Fatalf("active1 = %+v, want active %s", active1, started.Id)
	}
	if active1.Source != "thread" || active1.Key != "thread-status-test" {
		t.Fatalf("active1 source/key = %q/%q, want thread/thread-status-test", active1.Source, active1.Key)
	}
	if active1.IdleTimeoutSeconds != 3600 || active1.IdleSeconds == nil || *active1.IdleSeconds < 300 {
		t.Fatalf("active1 idle = %+v (timeout=%d), want >=300s", active1.IdleSeconds, active1.IdleTimeoutSeconds)
	}
	if active1.ExpiresInSeconds == nil || *active1.ExpiresInSeconds > 3300 {
		t.Fatalf("active1 expiresIn = %v, want <=3300s", active1.ExpiresInSeconds)
	}
	if active2.LastActiveAt != active1.LastActiveAt {
		t.Fatalf("session status reset lastActiveAt: %d -> %d", active1.LastActiveAt, active2.LastActiveAt)
	}
	if active2.IdleSeconds == nil || *active2.IdleSeconds < *active1.IdleSeconds+60 {
		t.Fatalf("idleSeconds did not advance across read-only status calls: %v -> %v", active1.IdleSeconds, active2.IdleSeconds)
	}
	if len(active1.Workspaces) != 1 || active1.Workspaces[0].Repo != "app" || active1.Workspaces[0].Node == "" {
		t.Fatalf("workspaces = %+v, want 1 workspace of app with node hostname", active1.Workspaces)
	}
	if len(active1.Commands) != 1 || active1.Commands[0].State != "succeeded" || active1.LiveCommands != 0 {
		t.Fatalf("commands = %+v (live=%d), want 1 succeeded", active1.Commands, active1.LiveCommands)
	}

	// Let the worker heartbeat under the advanced clock so it is online when
	// session end releases the workspace.
	c.eventually("worker online", func() bool {
		var nodes pb.ListNodesResponse
		c.ok(&nodes, "nodes")
		return len(nodes.Nodes) == 1 && nodes.Nodes[0].Online
	})

	// End session: unbound locally -> "none"; querying by -s shows "ended" with released workspace.
	c.ok(nil, "session", "end")
	var afterEnd sessionStatusResult
	c.ok(&afterEnd, "session", "status")
	if afterEnd.Active || afterEnd.State != "none" {
		t.Fatalf("after end default status = %+v, want state=none", afterEnd)
	}

	var ended sessionStatusResult
	c.ok(&ended, "session", "status", "-s", started.Id)
	if ended.Active || ended.State != "ended" || ended.EndedAt == nil || ended.EndReason != "ended by its agent" {
		t.Fatalf("ended status = %+v", ended)
	}
	if ended.Source != "flag" || ended.IdleSeconds != nil || ended.ExpiresInSeconds != nil {
		t.Fatalf("ended status source/idle = %+v", ended)
	}
	if len(ended.Workspaces) != 1 || ended.Workspaces[0].State != "released" || ended.Workspaces[0].ReleasedAt == nil {
		t.Fatalf("ended workspaces = %+v, want 1 released workspace", ended.Workspaces)
	}

	// Explicit nonexistent session returns NOT_FOUND.
	if code, err := c.fails("session", "status", "-s", "ghost-otter"); code != exitError || err.Code != api.CodeNotFound {
		t.Fatalf("explicit ghost-otter: exit %d, %+v, want exit 1 NOT_FOUND", code, err)
	}

	// Stale local binding in sessions.json returns active=false, state="not_found" cleanly.
	place, ok := startPlace(t.Context())
	if !ok {
		t.Fatal("the test runs in no T3 thread or git worktree")
	}
	if err := updateSessionMap(sessionsFilePath(c.cli), func(m sessionMap) { m.bind(place, "ghost-otter") }); err != nil {
		t.Fatal(err)
	}
	var stale sessionStatusResult
	c.ok(&stale, "session", "status")
	if stale.Active || stale.State != "not_found" || stale.ID != "ghost-otter" || stale.Source != "thread" {
		t.Fatalf("stale binding status = %+v, want state=not_found id=ghost-otter", stale)
	}
}

// session start in a T3 thread or worktree bound to a live session refuses
// to start another unless told to replace it, which ends the old one; a
// bound session that ended or that the controller never had is no obstacle.
func TestSessionStartReplacesOnlyWhenTold(t *testing.T) {
	c := newCluster(t)
	t.Setenv("T3CODE_THREAD_ID", "thread-replace")
	bindHere := func(id string) {
		t.Helper()
		place, ok := startPlace(t.Context())
		if !ok {
			t.Fatal("no T3 thread to bind to")
		}
		if err := updateSessionMap(sessionsFilePath(c.cli), func(m sessionMap) { m.bind(place, id) }); err != nil {
			t.Fatal(err)
		}
	}
	current := func(want string) {
		t.Helper()
		var cur sessionBinding
		c.ok(&cur, "session", "current")
		if cur.ID != want || cur.Source != "thread" {
			t.Fatalf("session current = %+v, want %s from the thread", cur, want)
		}
	}

	c.ok(nil, "session", "start", "one-fox")
	_, e := c.fails("session", "start", "two-fox")
	if e.Code != api.CodeConflict || e.Message != "T3 thread thread-replace is bound to session one-fox, which is live" ||
		e.Suggestion != "keep using one-fox (hotbed session status), end it first (hotbed session end), or pass --replace to end it and start a new one" {
		t.Fatalf("second start = %+v, want CONFLICT naming one-fox", e)
	}
	if _, e := c.fails("session", "status", "-s", "two-fox"); e.Code != api.CodeNotFound {
		t.Fatalf("a refused start started two-fox: %+v", e)
	}
	current("one-fox")

	c.ok(nil, "session", "start", "two-fox", "--replace")
	current("two-fox")
	if s := c.session("one-fox"); s.EndedAt == nil {
		t.Fatalf("--replace left one-fox live: %v", s)
	}

	// Bound to a session that ended, or one the controller never had.
	c.ok(nil, "session", "end")
	bindHere("two-fox")
	c.ok(nil, "session", "start", "three-fox")
	current("three-fox")
	bindHere("ghost-fox")
	c.ok(nil, "session", "start", "four-fox")
	current("four-fox")
	if s := c.session("three-fox"); s.EndedAt != nil {
		t.Fatalf("starting over a ghost ended three-fox, bound before it: %v", s)
	}

	// A sessions file it cannot read stops it before it starts anything.
	if err := os.WriteFile(sessionsFilePath(c.cli), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, e := c.fails("session", "start", "five-fox"); code != exitError || e.Code != api.CodeInternal {
		t.Fatalf("start with an unreadable sessions file: exit %d, %+v, want exit 1 INTERNAL", code, e)
	}
	if _, e := c.fails("session", "status", "-s", "five-fox"); e.Code != api.CodeNotFound {
		t.Fatalf("start with an unreadable sessions file started five-fox: %+v", e)
	}
}
