package worker

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
)

// board 7919: Sync must reject a changed tree while a non-server, non-shell
// command is running in the workspace (retryable CodeConflict), while still
// allowing no-op skips (unchanged tree), syncs alongside server/shell commands,
// and syncs once the running command exits.
func TestSyncGuardWhileCommandRunning(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.file.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")

	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
		}
	}

	origin := t.TempDir()
	runGit(origin, "init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(origin, "main.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(origin, "add", "-A")
	runGit(origin, "commit", "--quiet", "-m", "base")

	clientDir := filepath.Join(t.TempDir(), "client")
	runGit(filepath.Dir(clientDir), "clone", "--quiet", origin, clientDir)
	base, err := (gitops.Git{Dir: clientDir}).Run(t.Context(), "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(clientDir, "main.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap1, err := gitops.Snapshot(t.Context(), clientDir)
	if err != nil {
		t.Fatal(err)
	}
	hdr1, stream1 := packSyncStream(t, "ws-guard", "app", snap1, base)

	if err := os.WriteFile(filepath.Join(clientDir, "main.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap2, err := gitops.Snapshot(t.Context(), clientDir)
	if err != nil {
		t.Fatal(err)
	}
	hdr2, stream2 := packSyncStream(t, "ws-guard", "app", snap2, base)

	units := &fake.Units{}
	store := &fake.Store{Root: t.TempDir()}
	e, err := NewEngine(t.Context(), EngineConfig{
		Dir:             t.TempDir(),
		NodeID:          "n1",
		Host:            "ubu1",
		Store:           store,
		BaseImage:       fake.BaseImage(store),
		Units:           units,
		System:          fake.NewSystem(),
		AllowLocalRepos: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })

	vol := "ws-guard-vol"
	wsRepoDir := filepath.Join(e.workRoot(vol), "app")
	if err := os.MkdirAll(filepath.Dir(wsRepoDir), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(filepath.Dir(wsRepoDir), "clone", "--quiet", origin, wsRepoDir)

	e.mu.Lock()
	e.workspaces["ws-guard"] = &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{
			Id:      "ws-guard",
			Project: "app",
			State:   idle,
			Repos:   []*pb.WorkspaceRepo{{Repo: "app", BaseCommit: base, Head: base}},
		},
		Volume: vol,
	}}
	e.mu.Unlock()

	// Initial sync to snap1 succeeds cleanly.
	res, err := e.Sync(t.Context(), hdr1, bytes.NewReader(stream1))
	if err != nil {
		t.Fatalf("initial Sync(snap1): %v", err)
	}
	if res.Skipped || res.Tree != snap1.Tree {
		t.Fatalf("initial Sync(snap1) = %+v, want tree %s not skipped", res, snap1.Tree)
	}

	// Start a build command in ws-guard.
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id:          "cmd-build",
		WorkspaceId: "ws-guard",
		Repo:        "app",
		Kind:        "build",
		Args:        []string{"sleep", "300"},
	}); err != nil {
		t.Fatal(err)
	}
	e.admit(t.Context())
	t.Cleanup(func() { _ = units.Stop(context.WithoutCancel(t.Context()), "hotbed-cmd-build") })
	if s := e.state("cmd-build"); s != running {
		t.Fatalf("cmd-build state = %v, want running", s)
	}

	// (a) Sync with changed tree is rejected with retryable CodeConflict naming cmd-build.
	_, err = e.Sync(t.Context(), hdr2, bytes.NewReader(stream2))
	ae := api.FromConnect(err)
	if ae == nil || ae.Code != api.CodeConflict || !ae.Retryable {
		t.Fatalf("Sync(snap2) while build running = %v (%+v), want retryable CodeConflict", err, ae)
	}
	if !strings.Contains(ae.Message, "cmd-build") {
		t.Errorf("error message %q missing running command ID cmd-build", ae.Message)
	}
	if !strings.Contains(ae.Suggestion, "--no-sync") {
		t.Errorf("error suggestion %q missing --no-sync", ae.Suggestion)
	}
	// Disk checkout must still contain v1, not v2.
	data, err := os.ReadFile(filepath.Join(wsRepoDir, "main.txt"))
	if err != nil || string(data) != "v1\n" {
		t.Fatalf("main.txt after rejected sync = %q (%v), want v1", data, err)
	}

	// (b) Sync with unchanged tree (h.Tree == r.Tree) succeeds (skipped) even while build runs.
	res, err = e.Sync(t.Context(), hdr1, bytes.NewReader(stream1))
	if err != nil {
		t.Fatalf("Sync(snap1 unchanged) while build running: %v", err)
	}
	if !res.Skipped {
		t.Fatalf("Sync(snap1 unchanged) Skipped = false, want true")
	}

	// (d) Stop the build command -> Sync with changed tree now succeeds!
	if _, err := e.Stop(t.Context(), "cmd-build", "done"); err != nil {
		t.Fatal(err)
	}
	res, err = e.Sync(t.Context(), hdr2, bytes.NewReader(stream2))
	if err != nil {
		t.Fatalf("Sync(snap2) after build exited: %v", err)
	}
	if res.Skipped || res.Tree != snap2.Tree {
		t.Fatalf("Sync(snap2) = %+v, want tree %s not skipped", res, snap2.Tree)
	}
	data, err = os.ReadFile(filepath.Join(wsRepoDir, "main.txt"))
	if err != nil || string(data) != "v2\n" {
		t.Fatalf("main.txt after sync = %q (%v), want v2", data, err)
	}

	// (c) Parked build also blocks changed-tree Sync until stopped (board 9782).
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id:          "cmd-parked",
		WorkspaceId: "ws-guard",
		Repo:        "app",
		Kind:        "build",
		Args:        []string{"sleep", "300"},
	}); err != nil {
		t.Fatal(err)
	}
	e.admit(t.Context())
	t.Cleanup(func() { _ = units.Stop(context.WithoutCancel(t.Context()), "hotbed-cmd-parked") })
	e.mu.Lock()
	parkedCmd := e.commands["cmd-parked"]
	e.mu.Unlock()
	if !e.parkOne(t.Context(), parkedCmd, "memory pressure", "park cmd-parked", "") {
		t.Fatal("parkOne(cmd-parked) = false, want true")
	}
	if s := e.state("cmd-parked"); s != parked {
		t.Fatalf("cmd-parked state = %v, want parked", s)
	}
	_, err = e.Sync(t.Context(), hdr1, bytes.NewReader(stream1))
	ae = api.FromConnect(err)
	if ae == nil || ae.Code != api.CodeConflict || !ae.Retryable || !strings.Contains(ae.Message, "cmd-parked") {
		t.Fatalf("Sync(snap1) while build parked = %v (%+v), want retryable CodeConflict naming cmd-parked", err, ae)
	}
	// Unchanged tree (snap2) is still skipped while parked.
	if res, err = e.Sync(t.Context(), hdr2, bytes.NewReader(stream2)); err != nil || !res.Skipped {
		t.Fatalf("Sync(snap2 unchanged) while build parked = (%+v, %v), want skipped", res, err)
	}
	if _, err := e.Stop(t.Context(), "cmd-parked", "done"); err != nil {
		t.Fatal(err)
	}

	// (d) Paused build also blocks changed-tree Sync until stopped (board 9782).
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id:          "cmd-paused",
		WorkspaceId: "ws-guard",
		Repo:        "app",
		Kind:        "test",
		Args:        []string{"sleep", "300"},
	}); err != nil {
		t.Fatal(err)
	}
	e.admit(t.Context())
	t.Cleanup(func() { _ = units.Stop(context.WithoutCancel(t.Context()), "hotbed-cmd-paused") })
	if _, err := e.Pause(t.Context(), "cmd-paused", Intervention{Comment: "hold", Host: "laptop"}); err != nil {
		t.Fatalf("Pause(cmd-paused): %v", err)
	}
	if s := e.state("cmd-paused"); s != paused {
		t.Fatalf("cmd-paused state = %v, want paused", s)
	}
	_, err = e.Sync(t.Context(), hdr1, bytes.NewReader(stream1))
	ae = api.FromConnect(err)
	if ae == nil || ae.Code != api.CodeConflict || !ae.Retryable || !strings.Contains(ae.Message, "cmd-paused") {
		t.Fatalf("Sync(snap1) while test paused = %v (%+v), want retryable CodeConflict naming cmd-paused", err, ae)
	}
	if _, err := e.Stop(t.Context(), "cmd-paused", "done"); err != nil {
		t.Fatal(err)
	}

	// (e) Start a server command and a shell command -> Sync with changed tree is allowed!
	for _, tc := range []struct{ id, kind string }{
		{id: "cmd-srv", kind: "server"},
		{id: "cmd-sh", kind: "shell"},
	} {
		if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
			Id:          tc.id,
			WorkspaceId: "ws-guard",
			Repo:        "app",
			Kind:        tc.kind,
			Args:        []string{"sleep", "300"},
		}); err != nil {
			t.Fatal(err)
		}
		e.admit(t.Context())
		t.Cleanup(func() { _ = units.Stop(context.WithoutCancel(t.Context()), "hotbed-"+tc.id) })
		if s := e.state(tc.id); s != running {
			t.Fatalf("%s state = %v, want running", tc.id, s)
		}
	}

	res, err = e.Sync(t.Context(), hdr1, bytes.NewReader(stream1))
	if err != nil {
		t.Fatalf("Sync(snap1) while only server+shell running: %v", err)
	}
	if res.Skipped || res.Tree != snap1.Tree {
		t.Fatalf("Sync(snap1) while server+shell running = %+v, want applied", res)
	}
}
