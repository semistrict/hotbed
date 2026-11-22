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
		if got := normalizeGitURL(tc[0]); got != tc[1] {
			t.Errorf("normalizeGitURL(%q) = %q, want %q", tc[0], got, tc[1])
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
	t.Setenv("HOTBED_SESSION", "")
	t.Setenv("HOTBED_REPO", "")
	t.Setenv("T3CODE_THREAD_ID", "thread-alpha-123")

	c := newCluster(t)
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
		t.Fatalf("add with defaults = %v, want cladefs in thread-fox", ws)
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
	if cur.ID != "wt-owl" || cur.Source != "worktree" || cur.Key != canonicalPath(checkout) {
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
		t.Fatalf("add with HOTBED_SESSION+HOTBED_REPO = %v", ws)
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
