package worker

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/unit"
	"google.golang.org/protobuf/proto"
)

// pathPrefixUnits wraps fake.Units so Exec prepends a test-controlled directory
// to PATH inside sandboxed git/__sync-apply subprocesses, overriding sandboxEnv's
// fixed PATH without weakening production sandboxEnv.
type pathPrefixUnits struct {
	*fake.Units
	mu     sync.Mutex
	prefix string
}

func (u *pathPrefixUnits) setPathPrefix(dir string) {
	u.mu.Lock()
	u.prefix = dir
	u.mu.Unlock()
}

func (u *pathPrefixUnits) Exec(ctx context.Context, name, slice string, c unit.ExecSpec) (*exec.Cmd, error) {
	cmd, err := u.Units.Exec(ctx, name, slice, c)
	if err != nil {
		return nil, err
	}
	u.mu.Lock()
	prefix := u.prefix
	u.mu.Unlock()
	if prefix == "" {
		return cmd, nil
	}
	args := append([]string{"-c", `PATH="$1:$PATH"; shift; exec "$@"`, "wrap", prefix, cmd.Path}, cmd.Args[1:]...)
	wrapped := exec.CommandContext(ctx, "/bin/sh", args...)
	wrapped.Dir = cmd.Dir
	return wrapped, nil
}

func testOriginWithSubmodule(t *testing.T) (appOrigin, libOrigin string) {
	t.Helper()
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

	libOrigin = t.TempDir()
	runGit(libOrigin, "init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(libOrigin, "a.txt"), []byte("lib base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(libOrigin, "add", "-A")
	runGit(libOrigin, "commit", "--quiet", "-m", "lib base")

	appOrigin = t.TempDir()
	runGit(appOrigin, "init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(appOrigin, "main.txt"), []byte("app base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(appOrigin, "add", "-A")
	runGit(appOrigin, "commit", "--quiet", "-m", "app base")
	runGit(appOrigin, "submodule", "add", "--quiet", libOrigin, "lib")
	runGit(appOrigin, "commit", "--quiet", "-m", "add lib")
	return appOrigin, libOrigin
}

func packSyncStream(t *testing.T, wsID, repo string, snap gitops.Snap, base string) (*pb.SyncHeader, []byte) {
	t.Helper()
	var top bytes.Buffer
	subs, err := gitops.Bundle(t.Context(), snap, base, &top)
	if err != nil {
		t.Fatalf("gitops.Bundle: %v", err)
	}
	hdr := &pb.SyncHeader{
		WorkspaceId: wsID,
		Repo:        repo,
		Commit:      snap.Commit,
		Tree:        snap.Tree,
	}
	var stream bytes.Buffer
	for _, s := range subs {
		hdr.Submodules = append(hdr.Submodules, &pb.SyncSubmodule{
			Path:        s.Path,
			Commit:      s.Commit,
			BundleBytes: int64(len(s.Data)),
		})
		stream.Write(s.Data)
	}
	stream.Write(top.Bytes())
	return hdr, stream.Bytes()
}

// board 8019: when gitops.Apply fails after mutating the top-level checkout
// (e.g. a corrupt submodule bundle) and rollback restores the previous state,
// Engine.Sync must still invalidate the cached tree so a subsequent sync for
// the old tree is NOT skipped, while keeping disk cleanly back at the previous
// state so commands never see a half-updated checkout.
func TestSyncFailedApplyAfterCheckoutRollsBackAndForcesNextSync(t *testing.T) {
	appOrigin, _ := testOriginWithSubmodule(t)

	clientDir := filepath.Join(t.TempDir(), "client")
	cmd := exec.Command("git", "clone", "--quiet", "--recurse-submodules", appOrigin, clientDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone client: %v: %s", err, out)
	}
	base, err := (gitops.Git{Dir: clientDir}).Run(t.Context(), "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	// Snapshot A: modify both top-level and submodule lib.
	if err := os.WriteFile(filepath.Join(clientDir, "main.txt"), []byte("app A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clientDir, "lib/a.txt"), []byte("lib A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapA, err := gitops.Snapshot(t.Context(), clientDir)
	if err != nil {
		t.Fatal(err)
	}
	hdrA, streamA := packSyncStream(t, "ws1", "app", snapA, base)

	// Snapshot B: modify both top-level and submodule lib again.
	if err := os.WriteFile(filepath.Join(clientDir, "main.txt"), []byte("app B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clientDir, "lib/a.txt"), []byte("lib B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapB, err := gitops.Snapshot(t.Context(), clientDir)
	if err != nil {
		t.Fatal(err)
	}
	hdrB, streamB := packSyncStream(t, "ws1", "app", snapB, base)
	if len(hdrB.Submodules) != 1 || hdrB.Submodules[0].Path != "lib" {
		t.Fatalf("expected 1 submodule bundle for lib, got %+v", hdrB.Submodules)
	}

	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	eng := startTestEngine(t, EngineConfig{Dir: dir, Store: store})
	project := &pb.HostedProject{
		Name:  "app",
		Repos: []*pb.Repo{{Id: "app", Url: appOrigin}},
		Steps: []*pb.Step{{Name: "build", Script: "true", EveryPull: true, Hash: "h1"}},
	}
	if _, err := eng.CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{
		Id: "ws1", Repo: "app", SessionId: "s1", Agent: "a", Project: project,
	}); err != nil {
		t.Fatal(err)
	}
	eventuallyTrue(t, "ws1 ready", func() bool {
		ws, err := eng.Workspace("ws1")
		return err == nil && ws.State == idle
	})

	// 1. Sync A succeeds.
	respA, err := eng.Sync(t.Context(), hdrA, bytes.NewReader(streamA))
	if err != nil {
		t.Fatalf("sync A: %v", err)
	}
	if respA.Skipped {
		t.Fatal("initial sync A unexpectedly skipped")
	}

	// 2. Send B with a corrupted lib submodule bundle: top-level checkout to B
	// runs, then submodule lib apply fails and gitops.Apply rolls back to A.
	corruptStreamB := append([]byte(nil), streamB...)
	subLen := int(hdrB.Submodules[0].BundleBytes)
	for i := 0; i < subLen; i++ {
		corruptStreamB[i] = 'X'
	}
	_, err = eng.Sync(t.Context(), hdrB, bytes.NewReader(corruptStreamB))
	if api.CodeOf(err) != api.CodeSyncRefused {
		t.Fatalf("sync B with corrupt submodule = %v (code %v), want SYNC_REFUSED", err, api.CodeOf(err))
	}

	// Disk must be cleanly back at A (no half-updated mixed tree).
	wsRepoDir := filepath.Join(store.Path("ws/ws1"), "work", "app")
	if got, err := os.ReadFile(filepath.Join(wsRepoDir, "main.txt")); err != nil || string(got) != "app A\n" {
		t.Errorf("main.txt after failed sync B = %q (err=%v), want %q", got, err, "app A\n")
	}
	if got, err := os.ReadFile(filepath.Join(wsRepoDir, "lib/a.txt")); err != nil || string(got) != "lib A\n" {
		t.Errorf("lib/a.txt after failed sync B = %q (err=%v), want %q", got, err, "lib A\n")
	}

	// Workspace remains healthy (idle), but cached Tree was invalidated so a
	// subsequent sync for tree A is NOT skipped.
	ws, err := eng.Workspace("ws1")
	if err != nil {
		t.Fatal(err)
	}
	if ws.State != idle {
		t.Errorf("workspace state after rolled-back sync = %v, want IDLE", ws.State)
	}
	if got := repoOf(ws, "app").Tree; got != "" {
		t.Errorf("repo Tree after failed mutated sync = %q, want empty (invalidated)", got)
	}

	// 3. Re-syncing A must NOT be skipped and must restore clean metadata.
	respA2, err := eng.Sync(t.Context(), hdrA, bytes.NewReader(streamA))
	if err != nil {
		t.Fatalf("second sync A: %v", err)
	}
	if respA2.Skipped {
		t.Fatal("sync back to A was skipped after failed mutated apply, want full re-apply")
	}
	ws, err = eng.Workspace("ws1")
	if err != nil {
		t.Fatal(err)
	}
	if got := repoOf(ws, "app").Tree; got != snapA.Tree {
		t.Errorf("repo Tree after re-sync A = %q, want %q", got, snapA.Tree)
	}

	// 4. Valid sync B now succeeds cleanly.
	if _, err := eng.Sync(t.Context(), hdrB, bytes.NewReader(streamB)); err != nil {
		t.Fatalf("valid sync B: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(wsRepoDir, "main.txt")); err != nil || string(got) != "app B\n" {
		t.Errorf("main.txt after valid sync B = %q (err=%v), want %q", got, err, "app B\n")
	}
	if got, err := os.ReadFile(filepath.Join(wsRepoDir, "lib/a.txt")); err != nil || string(got) != "lib B\n" {
		t.Errorf("lib/a.txt after valid sync B = %q (err=%v), want %q", got, err, "lib B\n")
	}
}

// board 8019: when gitops.Apply fails after mutating the top-level checkout and
// rollback ALSO fails, the workspace is marked WORKSPACE_STATE_FAILED with
// invalidated repo metadata so both commands and future syncs know it is
// inconsistent and refuse to run against the mixed checkout.
func TestSyncFailedApplyWithFailedRollbackMarksWorkspaceFailed(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	appOrigin, _ := testOriginWithSubmodule(t)

	clientDir := filepath.Join(t.TempDir(), "client")
	cmd := exec.Command(realGit, "clone", "--quiet", "--recurse-submodules", appOrigin, clientDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone client: %v: %s", err, out)
	}
	base, err := (gitops.Git{Dir: clientDir}).Run(t.Context(), "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	// Snapshot A.
	if err := os.WriteFile(filepath.Join(clientDir, "main.txt"), []byte("app A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clientDir, "lib/a.txt"), []byte("lib A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapA, err := gitops.Snapshot(t.Context(), clientDir)
	if err != nil {
		t.Fatal(err)
	}
	hdrA, streamA := packSyncStream(t, "ws1", "app", snapA, base)

	// Snapshot B.
	if err := os.WriteFile(filepath.Join(clientDir, "main.txt"), []byte("app B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clientDir, "lib/a.txt"), []byte("lib B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapB, err := gitops.Snapshot(t.Context(), clientDir)
	if err != nil {
		t.Fatal(err)
	}
	hdrB, streamB := packSyncStream(t, "ws1", "app", snapB, base)

	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	units := &pathPrefixUnits{Units: &fake.Units{}}
	eng := startTestEngine(t, EngineConfig{Dir: dir, Store: store, Units: units})
	project := &pb.HostedProject{
		Name:  "app",
		Repos: []*pb.Repo{{Id: "app", Url: appOrigin}},
		Steps: []*pb.Step{{Name: "build", Script: "true", EveryPull: true, Hash: "h1"}},
	}
	if _, err := eng.CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{
		Id: "ws1", Repo: "app", SessionId: "s1", Agent: "a", Project: project,
	}); err != nil {
		t.Fatal(err)
	}
	eventuallyTrue(t, "ws1 ready", func() bool {
		ws, err := eng.Workspace("ws1")
		return err == nil && ws.State == idle
	})

	// 1. Sync A succeeds.
	if _, err := eng.Sync(t.Context(), hdrA, bytes.NewReader(streamA)); err != nil {
		t.Fatalf("sync A: %v", err)
	}

	// 2. Wrap git so rollback checkout to snapA.Commit fails, while corrupting
	// lib's bundle in sync B so Apply fails after top-level checkout moves to B.
	binDir := t.TempDir()
	wrapper := "#!/bin/sh\n" +
		"case \" $* \" in\n" +
		"  *\" checkout \"*\" " + snapA.Commit + " \"*)\n" +
		"    echo \"injected rollback failure\" >&2\n" +
		"    exit 1\n" +
		"    ;;\n" +
		"esac\n" +
		"exec \"" + realGit + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	units.setPathPrefix(binDir)

	corruptStreamB := append([]byte(nil), streamB...)
	subLen := int(hdrB.Submodules[0].BundleBytes)
	for i := 0; i < subLen; i++ {
		corruptStreamB[i] = 'X'
	}
	_, err = eng.Sync(t.Context(), hdrB, bytes.NewReader(corruptStreamB))
	if api.CodeOf(err) != api.CodeSyncRefused {
		t.Fatalf("sync B = %v (code %v), want SYNC_REFUSED", err, api.CodeOf(err))
	}
	if !strings.Contains(err.Error(), "inconsistent") {
		t.Errorf("sync B error = %q, want mention of inconsistent checkout", err.Error())
	}

	// Workspace must be marked FAILED with cleared Tree/Head.
	ws, err := eng.Workspace("ws1")
	if err != nil {
		t.Fatal(err)
	}
	if ws.State != failedWS {
		t.Fatalf("workspace state = %v, want FAILED", ws.State)
	}
	if !strings.Contains(ws.Reason, "inconsistent") {
		t.Errorf("workspace reason = %q, want mention of inconsistent checkout", ws.Reason)
	}
	r := repoOf(ws, "app")
	if r.Tree != "" || r.Head != "" {
		t.Errorf("repo after inconsistent sync = {Head:%q Tree:%q}, want both cleared", r.Head, r.Tree)
	}

	// Commands must be refused on the inconsistent workspace.
	if _, err := eng.Exec(t.Context(), &pb.RunCommandRequest{
		WorkspaceId: "ws1", Repo: "app", Args: []string{"true"},
	}); err == nil {
		t.Fatal("Exec on inconsistent workspace succeeded, want error")
	}

	// A subsequent sync for the old tree A must NOT be skipped; it must fail
	// with CodeWorkspaceFailed so the caller/controller replaces the workspace.
	if _, err := eng.Sync(t.Context(), hdrA, bytes.NewReader(streamA)); api.CodeOf(err) != api.CodeWorkspaceFailed {
		t.Fatalf("sync A on inconsistent workspace = %v (code %v), want WORKSPACE_FAILED", err, api.CodeOf(err))
	}
}

// board 8890: Engine.Sync persists the sync-in-progress intent and invalidates
// the cached tree BEFORE gitops.Apply can mutate disk. If the worker crashes
// or restarts mid-apply, NewEngine marks the workspace WORKSPACE_STATE_FAILED
// with cleared repo metadata so commands are refused and equal-tree syncs are
// never skipped. If Apply fails before mutating disk, the previous tree and
// reason are restored cleanly.
func TestSyncInterruptedMidApplyMarksWorkspaceFailedOnRestart(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	appOrigin, _ := testOriginWithSubmodule(t)

	clientDir := filepath.Join(t.TempDir(), "client")
	cmd := exec.Command(realGit, "clone", "--quiet", "--recurse-submodules", appOrigin, clientDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone client: %v: %s", err, out)
	}
	base, err := (gitops.Git{Dir: clientDir}).Run(t.Context(), "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	// Snapshot A.
	if err := os.WriteFile(filepath.Join(clientDir, "main.txt"), []byte("app A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapA, err := gitops.Snapshot(t.Context(), clientDir)
	if err != nil {
		t.Fatal(err)
	}
	hdrA, streamA := packSyncStream(t, "ws1", "app", snapA, base)

	// Snapshot B.
	if err := os.WriteFile(filepath.Join(clientDir, "main.txt"), []byte("app B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapB, err := gitops.Snapshot(t.Context(), clientDir)
	if err != nil {
		t.Fatal(err)
	}
	hdrB, _ := packSyncStream(t, "ws1", "app", snapB, base)

	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	units := &pathPrefixUnits{Units: &fake.Units{}}
	eng := startTestEngine(t, EngineConfig{Dir: dir, Store: store, Units: units})
	project := &pb.HostedProject{
		Name:  "app",
		Repos: []*pb.Repo{{Id: "app", Url: appOrigin}},
		Steps: []*pb.Step{{Name: "build", Script: "true", EveryPull: true, Hash: "h1"}},
	}
	if _, err := eng.CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{
		Id: "ws1", Repo: "app", SessionId: "s1", Agent: "a", Project: project,
	}); err != nil {
		t.Fatal(err)
	}
	eventuallyTrue(t, "ws1 ready", func() bool {
		ws, err := eng.Workspace("ws1")
		return err == nil && ws.State == idle
	})

	// 1. Sync A succeeds and sets Tree = snapA.Tree.
	if _, err := eng.Sync(t.Context(), hdrA, bytes.NewReader(streamA)); err != nil {
		t.Fatalf("sync A: %v", err)
	}

	// 2. Pause inside gitops.Apply (during fetch of a corrupt top-level bundle)
	// and verify that SQLite ALREADY holds the persisted sync-in-progress intent
	// and cleared Tree before gitops.Apply runs.
	gateDir := t.TempDir()
	inApply := filepath.Join(gateDir, "in-apply")
	releaseApply := filepath.Join(gateDir, "release-apply")
	binDir := t.TempDir()
	wrapper := "#!/bin/sh\n" +
		"case \" $* \" in\n" +
		"  *\" fetch \"*|*\" unbundle \"*)\n" +
		"    if [ ! -f \"" + inApply + "\" ]; then\n" +
		"      : > \"" + inApply + "\"\n" +
		"      while [ ! -f \"" + releaseApply + "\" ]; do sleep 0.005; done\n" +
		"    fi\n" +
		"    ;;\n" +
		"esac\n" +
		"exec \"" + realGit + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	units.setPathPrefix(binDir)

	syncErrCh := make(chan error, 1)
	go func() {
		_, err := eng.Sync(t.Context(), hdrB, bytes.NewReader([]byte("corrupt top-level bundle\n")))
		syncErrCh <- err
	}()
	eventuallyTrue(t, "sync B entered gitops.Apply", func() bool {
		_, err := os.Stat(inApply)
		return err == nil
	})

	// Read directly from SQLite to confirm the sync intent was persisted before Apply.
	var persisted *pb.WorkspaceRecord
	if err := eng.db.all("workspace", func() proto.Message { return &pb.WorkspaceRecord{} }, func(m proto.Message) {
		r := m.(*pb.WorkspaceRecord)
		if r.Workspace.Id == "ws1" {
			persisted = r
		}
	}); err != nil {
		t.Fatal(err)
	}
	if persisted == nil {
		t.Fatal("ws1 missing from SQLite")
	}
	if !isSyncInProgress(persisted.Workspace.Reason) {
		t.Errorf("persisted ws1 reason mid-Apply = %q, want sync-in-progress marker", persisted.Workspace.Reason)
	}
	if got := repoOf(persisted.Workspace, "app").Tree; got != "" {
		t.Errorf("persisted ws1 tree mid-Apply = %q, want empty (invalidated before Apply)", got)
	}

	// Unblock Apply: since the corrupt top-level bundle fails before mutating
	// disk, Tree and Reason must be cleanly restored.
	if err := os.WriteFile(releaseApply, []byte("go"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := <-syncErrCh; api.CodeOf(err) != api.CodeSyncRefused {
		t.Fatalf("sync B with corrupt top-level bundle = %v, want SYNC_REFUSED", err)
	}
	units.setPathPrefix("")
	ws, err := eng.Workspace("ws1")
	if err != nil {
		t.Fatal(err)
	}
	if ws.State != idle || ws.Reason != "" || repoOf(ws, "app").Tree != snapA.Tree {
		t.Fatalf("ws1 after unmutated refusal = {State:%v Reason:%q Tree:%q}, want IDLE / empty / %q", ws.State, ws.Reason, repoOf(ws, "app").Tree, snapA.Tree)
	}

	// 3. Simulate a crash mid-Apply: persist ws1 with the sync-in-progress
	// marker (as written right before gitops.Apply) and a queued command, then
	// close the engine and restart via NewEngine.
	eng.mu.Lock()
	w := eng.workspaces["ws1"]
	repoOf(w.ws(), "app").Tree = ""
	w.ws().Reason = syncInProgressReason("app")
	eng.saveWorkspace(w, change{what: "syncing", detail: "app"})
	queuedCmd := eng.newCommand(&pb.CommandRecord{
		Command: &pb.Command{
			Id: "cmd-mid-sync", WorkspaceId: "ws1", NodeId: eng.cfg.NodeID,
			Argv: "true", Args: []string{"true"}, Kind: "shell",
			State: queued, QueuedAt: eng.nowMs(), Limits: &pb.CommandLimits{CpuWeight: 100},
		},
		RepoId: "app", Project: "app", Agent: "a", Volume: w.rec.Volume, Dir: WorkPath + "/app",
	})
	eng.commands[queuedCmd.cmd().Id] = queuedCmd
	eng.saveCommand(queuedCmd, change{what: "queued", detail: "mid-sync"})
	eng.mu.Unlock()
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}

	// 4. Reopen with NewEngine: ws1 must immediately be marked FAILED with
	// cleared repo metadata, and queued commands on it must be failed and never
	// started.
	units2 := &fake.Units{}
	eng2, err := NewEngine(t.Context(), EngineConfig{
		Dir: dir, NodeID: "n1", Host: "w1",
		Store: store, BaseImage: fake.BaseImage(store), Units: units2, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatalf("NewEngine reload: %v", err)
	}
	t.Cleanup(func() { eng2.Close() })

	ws2, err := eng2.Workspace("ws1")
	if err != nil {
		t.Fatal(err)
	}
	if ws2.State != failedWS {
		t.Fatalf("reloaded ws1 state = %v, want WORKSPACE_STATE_FAILED", ws2.State)
	}
	if !strings.Contains(ws2.Reason, "interrupted during sync") {
		t.Errorf("reloaded ws1 reason = %q, want mention of interrupted sync", ws2.Reason)
	}
	if r := repoOf(ws2, "app"); r.Tree != "" || r.Head != "" {
		t.Errorf("reloaded ws1 repo = {Head:%q Tree:%q}, want both cleared", r.Head, r.Tree)
	}

	eng2.step(t.Context())
	if c, ok := eng2.Command("cmd-mid-sync"); !ok || c.State != failed {
		t.Fatalf("cmd-mid-sync after restart = %v (ok=%v), want FAILED", c, ok)
	}
	if st, err := units2.Status(t.Context(), "hotbed-cmd-mid-sync"); err != nil || st.Loaded {
		t.Fatalf("unit for cmd-mid-sync = %+v (err=%v), want never started", st, err)
	}

	// Exec must be refused on the failed workspace.
	if _, err := eng2.Exec(t.Context(), &pb.RunCommandRequest{
		WorkspaceId: "ws1", Repo: "app", Args: []string{"true"},
	}); err == nil {
		t.Fatal("Exec on interrupted-sync workspace succeeded, want error")
	}

	// Equal-tree Sync for A must NOT skip; it must fail with WORKSPACE_FAILED.
	if _, err := eng2.Sync(t.Context(), hdrA, bytes.NewReader(streamA)); api.CodeOf(err) != api.CodeWorkspaceFailed {
		t.Fatalf("sync A on interrupted-sync workspace = %v (code %v), want WORKSPACE_FAILED", err, api.CodeOf(err))
	}

	// Confirm the FAILED state was persisted across another restart.
	if err := eng2.Close(); err != nil {
		t.Fatal(err)
	}
	eng3, err := NewEngine(t.Context(), EngineConfig{
		Dir: dir, NodeID: "n1", Host: "w1",
		Store: store, BaseImage: fake.BaseImage(store), Units: &fake.Units{}, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatalf("third NewEngine reload: %v", err)
	}
	t.Cleanup(func() { eng3.Close() })
	if ws3, err := eng3.Workspace("ws1"); err != nil || ws3.State != failedWS {
		t.Fatalf("ws1 after second reload = %v (err=%v), want FAILED", ws3, err)
	}
}

// board 8892: queued commands belonging to a FAILED (or RELEASED) workspace
// must never be admitted after a restart or on Engine.step; they must
// transition to a terminal state without starting in Units.
func TestQueuedCommandOnFailedWorkspaceFailsOnRestartAndStepNeverStartsUnit(t *testing.T) {
	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	units1 := &fake.Units{}
	eng1, err := NewEngine(t.Context(), EngineConfig{
		Dir: dir, NodeID: "n1", Host: "w1",
		Store: store, BaseImage: fake.BaseImage(store), Units: units1, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Persist a FAILED workspace alongside a QUEUED command (simulating a crash
	// between saveWorkspace(FAILED) and ending queued commands).
	eng1.mu.Lock()
	w := &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{
			Id: "ws-failed", Project: "app", BaseId: "app-g1", NodeId: "n1", Agent: "a",
			State: failedWS, Reason: "sync of app failed and left checkout inconsistent",
			Repos: []*pb.WorkspaceRepo{{Repo: "app", BaseCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
		},
		Volume: "ws/ws-failed", BaseSnapshot: "app-g1",
	}}
	eng1.workspaces["ws-failed"] = w
	eng1.saveWorkspace(w, change{what: "failed", detail: w.ws().Reason})

	cRestart := eng1.newCommand(&pb.CommandRecord{
		Command: &pb.Command{
			Id: "q-restart", WorkspaceId: "ws-failed", NodeId: "n1",
			Argv: "echo should-not-run", Args: []string{"echo", "should-not-run"}, Kind: "shell",
			State: queued, QueuedAt: eng1.nowMs(), Limits: &pb.CommandLimits{CpuWeight: 100},
		},
		RepoId: "app", Project: "app", Agent: "a", Volume: "ws/ws-failed", Dir: WorkPath + "/app",
	})
	eng1.commands["q-restart"] = cRestart
	eng1.saveCommand(cRestart, change{what: "queued", detail: "before crash"})
	eng1.mu.Unlock()

	if err := eng1.Close(); err != nil {
		t.Fatal(err)
	}

	// Reload via NewEngine and step: q-restart must be FAILED and never started.
	units2 := &fake.Units{}
	eng2, err := NewEngine(t.Context(), EngineConfig{
		Dir: dir, NodeID: "n1", Host: "w1",
		Store: store, BaseImage: fake.BaseImage(store), Units: units2, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatalf("NewEngine reload: %v", err)
	}
	t.Cleanup(func() { eng2.Close() })

	eng2.step(t.Context())

	gotRestart, ok := eng2.Command("q-restart")
	if !ok {
		t.Fatal("q-restart missing after reload")
	}
	if gotRestart.State != failed {
		t.Fatalf("q-restart state = %v, want COMMAND_STATE_FAILED", gotRestart.State)
	}
	if !strings.Contains(gotRestart.Reason, "inconsistent") {
		t.Errorf("q-restart reason = %q, want workspace failure reason", gotRestart.Reason)
	}
	if st, err := units2.Status(t.Context(), "hotbed-q-restart"); err != nil || st.Loaded {
		t.Fatalf("hotbed-q-restart unit = %+v (err=%v), want never started", st, err)
	}

	// Also exercise admit()/step() directly without going through reconcileLoaded
	// for both FAILED and RELEASED workspaces.
	eng2.mu.Lock()
	cStepFailed := eng2.newCommand(&pb.CommandRecord{
		Command: &pb.Command{
			Id: "q-step-failed", WorkspaceId: "ws-failed", NodeId: "n1",
			Argv: "true", Args: []string{"true"}, Kind: "shell",
			State: queued, QueuedAt: eng2.nowMs(), Limits: &pb.CommandLimits{CpuWeight: 100},
		},
		RepoId: "app", Project: "app", Agent: "a", Volume: "ws/ws-failed", Dir: WorkPath + "/app",
	})
	eng2.commands["q-step-failed"] = cStepFailed

	wRel := &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{
			Id: "ws-released", Project: "app", BaseId: "app-g1", NodeId: "n1", Agent: "a",
			State: released,
		},
	}}
	eng2.workspaces["ws-released"] = wRel
	cStepRel := eng2.newCommand(&pb.CommandRecord{
		Command: &pb.Command{
			Id: "q-step-released", WorkspaceId: "ws-released", NodeId: "n1",
			Argv: "true", Args: []string{"true"}, Kind: "shell",
			State: queued, QueuedAt: eng2.nowMs(), Limits: &pb.CommandLimits{CpuWeight: 100},
		},
		RepoId: "app", Project: "app", Agent: "a", Volume: "ws/ws-released", Dir: WorkPath + "/app",
	})
	eng2.commands["q-step-released"] = cStepRel
	eng2.mu.Unlock()

	eng2.step(t.Context())

	if got, ok := eng2.Command("q-step-failed"); !ok || got.State != failed {
		t.Fatalf("q-step-failed = %v (ok=%v), want FAILED", got, ok)
	}
	if st, err := units2.Status(t.Context(), "hotbed-q-step-failed"); err != nil || st.Loaded {
		t.Fatalf("hotbed-q-step-failed unit = %+v (err=%v), want never started", st, err)
	}
	if got, ok := eng2.Command("q-step-released"); !ok || got.State != cancelled {
		t.Fatalf("q-step-released = %v (ok=%v), want CANCELLED", got, ok)
	}
	if st, err := units2.Status(t.Context(), "hotbed-q-step-released"); err != nil || st.Loaded {
		t.Fatalf("hotbed-q-step-released unit = %+v (err=%v), want never started", st, err)
	}
}
