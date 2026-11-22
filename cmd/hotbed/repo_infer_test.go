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

func TestRunParseWithOmittedRepoAndDashDash(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantRepo string
		wantArgs []string
		wantDet  bool
	}{
		{
			name:     "omitted repo before --",
			args:     []string{"run", "--controller", "ubu1", "s1", "--", "go", "test", "-race", "./..."},
			wantRepo: "",
			wantArgs: []string{"go", "test", "-race", "./..."},
		},
		{
			name:     "omitted repo with flags before -- and nested -- after",
			args:     []string{"run", "--controller", "ubu1", "s1", "--detach", "--", "go", "test", "--", "-run", "Foo"},
			wantRepo: "",
			wantArgs: []string{"go", "test", "--", "-run", "Foo"},
			wantDet:  true,
		},
		{
			name:     "explicit repo with flags before --",
			args:     []string{"run", "--controller", "ubu1", "s1", "hotbed", "--detach", "--", "echo", "hi"},
			wantRepo: "hotbed",
			wantArgs: []string{"echo", "hi"},
			wantDet:  true,
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
			if rc.Repo != tc.wantRepo || rc.Detach != tc.wantDet || !slices.Equal(rc.Args, tc.wantArgs) {
				t.Errorf("got repo=%q detach=%v args=%v; want repo=%q detach=%v args=%v",
					rc.Repo, rc.Detach, rc.Args, tc.wantRepo, tc.wantDet, tc.wantArgs)
			}
		})
	}
}

func TestInferRepoFromCheckout(t *testing.T) {
	c := newCluster(t)
	remote := upstream(t)
	// Register the repo under a different name than the local checkout folder
	// (like ~/src/fxdax -> cladefs).
	c.ok(&pb.Repo{}, "repo", "add", "cladefs", remote)

	checkout := filepath.Join(t.TempDir(), "fxdax")
	if _, err := (gitops.Git{}).Run(t.Context(), "clone", "--quiet", remote, checkout); err != nil {
		t.Fatal(err)
	}
	// Also test from inside a nested subdirectory of the checkout.
	sub := filepath.Join(checkout, "src")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	// Start a session, add workspace without naming the repo, run without naming
	// the repo, sync without naming the repo, and manage steps without naming the repo.
	var sess pb.Session
	c.ok(&sess, "session", "start", "inferred-fox")

	var ws pb.Workspace
	c.ok(&ws, "add", sess.Id, "--sync", checkout)
	if ws.RepoId != "cladefs" {
		t.Fatalf("inferred workspace repoId = %q, want cladefs", ws.RepoId)
	}

	code, stdout, stderr := c.hotbed("run", sess.Id, "--", "cat", "src/lib.txt")
	if code != 0 || stdout != "base\n" {
		t.Fatalf("run without repo: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	var syncRes pb.SyncResponse
	c.ok(&syncRes, "sync", sess.Id)
	if syncRes.WorkspaceId != ws.Id {
		t.Fatalf("sync without repo hit workspace %q, want %q", syncRes.WorkspaceId, ws.Id)
	}

	// Step commands without explicit repo name.
	c.ok(&pb.Repo{}, "step", "set", "check", "--", "true")
	var steps pb.ListStepsResponse
	c.ok(&steps, "step", "ls")
	if len(steps.Steps) != 1 || steps.Steps[0].Step.Name != "check" {
		t.Fatalf("step ls without repo = %+v", steps.Steps)
	}
	code, showOut, showErr := c.hotbed("step", "show", "check")
	if code != 0 || showOut != "true\n" {
		t.Fatalf("step show without repo: exit %d, out %q, err %q", code, showOut, showErr)
	}
	c.ok(&pb.Repo{}, "step", "rm", "check")

	// Running outside a git checkout without REPO must fail with INVALID_ARGS.
	t.Chdir(t.TempDir())
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

	code, out := hotbed(t, d, "ui", "--controller", "ubu1")
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
	if code := run(t.Context(), []string{"ui", "--controller", "ubu1:9000", "--no-open"}, d); code != exitOK {
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
