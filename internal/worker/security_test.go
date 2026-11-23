package worker

import (
	"bytes"
	"context"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/httpx"
	"github.com/semistrict/hotbed/internal/profiler"
	"github.com/semistrict/hotbed/internal/tailnet"
	"github.com/semistrict/hotbed/internal/tailnet/tailnettest"
)

func TestValidateID(t *testing.T) {
	valid := []string{
		"a",
		"ws1",
		"ws-shell-1",
		"c-1",
		"f-app-g14-a1b2c3",
		"ABC_123-xyz",
		strings.Repeat("a", 128),
	}
	for _, id := range valid {
		if err := validateID(id); err != nil {
			t.Errorf("validateID(%q) = %v, want nil", id, err)
		}
	}

	invalid := []string{
		"",
		"../../etc",
		"../foo",
		"a/b",
		`a\b`,
		".",
		"..",
		".hidden",
		"foo..bar",
		"a.b",
		"-leading-dash",
		"_leading_underscore",
		"with space",
		"with\nnewline",
		"with\x00nul",
		"cmd$(id)",
		"cmd;rm",
		strings.Repeat("a", 129),
	}
	for _, id := range invalid {
		err := validateID(id)
		if err == nil {
			t.Errorf("validateID(%q) = nil, want error", id)
			continue
		}
		if api.CodeOf(err) != api.CodeInvalidArgs {
			t.Errorf("validateID(%q) code = %v, want INVALID_ARGS", id, api.CodeOf(err))
		}
	}
}

// Path-traversal and malformed IDs must be rejected at every worker RPC entry
// point as well as inside tmpDir, logPath, and profilePath helpers (board 8001).
func TestWorkerRPCsAndPathHelpersRejectTraversalIDs(t *testing.T) {
	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	eng, err := NewEngine(t.Context(), EngineConfig{
		Dir: dir, NodeID: "n1", Host: "worker1",
		Store: store, BaseImage: fake.BaseImage(store), Units: &fake.Units{}, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })

	// Create a valid workspace so RunCommand tests req.Id validation separately.
	wsVol := "ws/ws1"
	if err := store.Create(t.Context(), wsVol); err != nil {
		t.Fatal(err)
	}
	eng.mu.Lock()
	eng.workspaces["ws1"] = &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{Id: "ws1", Project: "app", State: idle, Repos: []*pb.WorkspaceRepo{{Repo: "app"}}},
		Volume:    wsVol,
	}}
	eng.mu.Unlock()

	// Plant files outside logs/ and profiles/ that a traversal ID like "../secret"
	// would otherwise resolve to.
	if err := os.WriteFile(filepath.Join(dir, "secret.log"), []byte("top-secret-log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := profiler.SaveProfile(filepath.Join(dir, "secret.pb.gz"), &pb.CommandProfile{CommandId: "secret", TotalSamples: 1}); err != nil {
		t.Fatal(err)
	}

	svc := &Service{E: eng}
	ctx := t.Context()
	proj := &pb.HostedProject{Name: "app"}

	badIDs := []string{"../../etc", "../foo", "../secret", "a/b", ".hidden", "bad id"}
	for _, bad := range badIDs {
		assertInvalid := func(name string, err error) {
			t.Helper()
			if api.CodeOf(err) != api.CodeInvalidArgs {
				t.Errorf("%s(%q): err = %v (code %v), want INVALID_ARGS", name, bad, err, api.CodeOf(err))
			}
		}

		_, err := svc.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{Id: bad, Project: proj})
		assertInvalid("CreateWorkspace", err)

		_, err = svc.GetWorkspace(ctx, &pb.GetWorkspaceRequest{Id: bad})
		assertInvalid("GetWorkspace", err)

		_, err = svc.ReleaseWorkspace(ctx, &pb.ReleaseWorkspaceRequest{Id: bad})
		assertInvalid("ReleaseWorkspace", err)

		_, err = eng.Sync(ctx, &pb.SyncHeader{WorkspaceId: bad, Repo: "app"}, bytes.NewReader(nil))
		assertInvalid("Sync", err)

		_, err = svc.ListFiles(ctx, &pb.ListFilesRequest{WorkspaceId: bad})
		assertInvalid("ListFiles", err)

		_, err = svc.GetFile(ctx, &pb.GetFileRequest{WorkspaceId: bad, Path: "app/lib.txt"})
		assertInvalid("GetFile", err)

		_, err = svc.RunCommand(ctx, &pb.RunCommandRequest{WorkspaceId: bad, Args: []string{"true"}})
		assertInvalid("RunCommand(WorkspaceId)", err)

		_, err = svc.RunCommand(ctx, &pb.RunCommandRequest{WorkspaceId: "ws1", Id: bad, Args: []string{"true"}})
		assertInvalid("RunCommand(Id)", err)

		_, err = svc.OpenShell(ctx, &pb.OpenShellRequest{WorkspaceId: bad, PublicKey: "ssh-ed25519 AAAA"})
		assertInvalid("OpenShell", err)

		_, err = svc.GetCommand(ctx, &pb.GetCommandRequest{Id: bad})
		assertInvalid("GetCommand", err)

		_, err = svc.StopCommand(ctx, &pb.StopCommandRequest{Id: bad})
		assertInvalid("StopCommand", err)

		_, err = svc.GetLogLines(ctx, &pb.GetLogLinesRequest{Id: bad})
		assertInvalid("GetLogLines", err)

		err = svc.FollowLog(ctx, &pb.FollowLogRequest{Id: bad}, hotbedv1connect.WorkspaceServiceFollowLogServerStream{})
		assertInvalid("FollowLog", err)

		err = svc.WatchWorkspace(ctx, &pb.WatchWorkspaceRequest{Id: bad}, hotbedv1connect.WorkspaceServiceWatchWorkspaceServerStream{})
		assertInvalid("WatchWorkspace", err)

		_, err = svc.InspectCommand(ctx, &pb.InspectCommandRequest{Id: bad})
		assertInvalid("InspectCommand", err)

		err = svc.TraceCommand(ctx, &pb.TraceCommandRequest{Id: bad}, hotbedv1connect.WorkspaceServiceTraceCommandServerStream{})
		assertInvalid("TraceCommand", err)

		err = svc.SampleCommand(ctx, &pb.SampleCommandRequest{Id: bad}, hotbedv1connect.WorkspaceServiceSampleCommandServerStream{})
		assertInvalid("SampleCommand", err)

		_, err = svc.GetProfile(ctx, &pb.GetProfileRequest{Target: &pb.GetProfileRequest_CommandId{CommandId: bad}})
		assertInvalid("GetProfile(command)", err)

		// Defensive path helpers must also reject invalid IDs directly.
		_, err = eng.logPath(bad)
		assertInvalid("logPath", err)

		_, err = eng.profilePath(bad)
		assertInvalid("profilePath", err)

		fakeCmd := &command{rec: &pb.CommandRecord{Command: &pb.Command{Id: bad}, Volume: wsVol}}
		_, err = eng.tmpDir(fakeCmd)
		assertInvalid("tmpDir", err)

		err = eng.makeTmp(fakeCmd)
		assertInvalid("makeTmp", err)
	}

	t.Run("GetProfile rejects missing or blank target", func(t *testing.T) {
		if _, err := svc.GetProfile(ctx, &pb.GetProfileRequest{}); api.CodeOf(err) != api.CodeInvalidArgs {
			t.Errorf("GetProfile(empty): err = %v (code %v), want INVALID_ARGS", err, api.CodeOf(err))
		}
		if _, err := svc.GetProfile(ctx, &pb.GetProfileRequest{Target: &pb.GetProfileRequest_Node{Node: "   "}}); api.CodeOf(err) != api.CodeInvalidArgs {
			t.Errorf("GetProfile(blank node): err = %v (code %v), want INVALID_ARGS", err, api.CodeOf(err))
		}
	})
}

// Engine.File, Engine.Files, makeTmpEntry, and toolchain must use os.OpenRoot
// so symlinks pointing outside the workspace/repo root are never followed as
// root (board 8003).
func TestWorkspaceFileAndHelpersRefuseSymlinkTraversal(t *testing.T) {
	dir := t.TempDir()
	outsideDir := t.TempDir()
	outsideSecret := filepath.Join(outsideDir, "host-secret.txt")
	if err := os.WriteFile(outsideSecret, []byte("ROOT-SECRET-CONTENT\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	eng, err := NewEngine(t.Context(), EngineConfig{
		Dir: dir, NodeID: "n1", Host: "worker1",
		Store: store, BaseImage: fake.BaseImage(store), Units: &fake.Units{}, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })

	wsVol := "ws/ws1"
	if err := store.Create(t.Context(), wsVol); err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(store.Path(wsVol), "work", "app")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "lib.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Initialize a git repo so git diff / ls-files work in File() and Files().
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "lib.txt"}, {"commit", "-qm", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	headOut, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	baseCommit := strings.TrimSpace(string(headOut))

	// 1. Absolute symlink pointing to host secret outside workspace.
	if err := os.Symlink(outsideSecret, filepath.Join(repoDir, "abs-leak")); err != nil {
		t.Fatal(err)
	}
	// 2. Relative symlink escaping the repo root.
	relToSecret, err := filepath.Rel(repoDir, outsideSecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relToSecret, filepath.Join(repoDir, "rel-leak")); err != nil {
		t.Fatal(err)
	}
	// 3. Directory symlink pointing outside the repo.
	if err := os.Symlink(outsideDir, filepath.Join(repoDir, "dir-leak")); err != nil {
		t.Fatal(err)
	}
	// 4. Intra-repo symlink pointing to a legitimate file inside the repo.
	if err := os.Symlink("lib.txt", filepath.Join(repoDir, "ok-link")); err != nil {
		t.Fatal(err)
	}
	// 5. Repo directory itself replaced with a symlink to outsideDir.
	if err := os.Symlink(outsideDir, filepath.Join(store.Path(wsVol), "work", "evilrepo")); err != nil {
		t.Fatal(err)
	}

	eng.mu.Lock()
	eng.workspaces["ws1"] = &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{
			Id: "ws1", Project: "app", State: idle,
			Repos: []*pb.WorkspaceRepo{
				{Repo: "app", Head: baseCommit, BaseCommit: baseCommit},
				{Repo: "evilrepo", Head: baseCommit, BaseCommit: baseCommit},
			},
		},
		Volume: wsVol,
	}}
	eng.mu.Unlock()

	ctx := t.Context()
	for _, escapePath := range []string{
		"app/abs-leak",
		"app/rel-leak",
		"app/dir-leak/host-secret.txt",
		"evilrepo/host-secret.txt",
	} {
		fc, err := eng.File(ctx, "ws1", escapePath)
		if err == nil {
			t.Fatalf("Engine.File(%q) succeeded with %q, want symlink traversal error", escapePath, fc.Text)
		}
		if strings.Contains(err.Error(), "ROOT-SECRET-CONTENT") {
			t.Fatalf("error leaked secret content: %v", err)
		}
	}

	// Legitimate regular file and intra-repo symlink still work.
	for _, okPath := range []string{"app/lib.txt", "app/ok-link"} {
		fc, err := eng.File(ctx, "ws1", okPath)
		if err != nil || fc.Text != "hello\n" {
			t.Errorf("Engine.File(%q) = (%+v, %v), want hello\\n", okPath, fc, err)
		}
	}

	// Engine.Files must refuse to open a repo that is a symlink outside workspace root.
	if _, err := eng.Files(ctx, "ws1"); err == nil {
		t.Fatal("Engine.Files succeeded with symlinked repo pointing outside workspace root")
	}

	// toolchain() must not follow a symlinked pin file outside the checkout.
	if err := os.Symlink(outsideSecret, filepath.Join(repoDir, "rust-toolchain")); err != nil {
		t.Fatal(err)
	}
	if got := toolchain(store.Path(wsVol), "app"); strings.Contains(got, "ROOT-SECRET") {
		t.Fatalf("toolchain() followed symlink outside repo: %q", got)
	}
	// toolchain() must also refuse a symlinked repo dir pointing outside volume.
	if err := os.WriteFile(filepath.Join(outsideDir, "rust-toolchain"), []byte("1.99.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := toolchain(store.Path(wsVol), "evilrepo"); got != "" {
		t.Fatalf("toolchain() followed symlinked repo outside volume: %q", got)
	}

	// makeTmpEntry() must refuse symlinked .tmp/<id> (both outside and in-root)
	// and not chmod the target.
	tmpTarget := filepath.Join(outsideDir, "untouched.txt")
	if err := os.WriteFile(tmpTarget, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(store.Path(wsVol), tmpName), 0o711); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(tmpTarget, filepath.Join(store.Path(wsVol), tmpName, "cmd-symlink")); err != nil {
		t.Fatal(err)
	}
	if err := makeTmpEntry(store.Path(wsVol), "cmd-symlink"); err == nil {
		t.Fatal("makeTmpEntry succeeded on symlinked tmp entry")
	}
	fi, err := os.Stat(tmpTarget)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("outside target mode changed to %o, want 0600", fi.Mode().Perm())
	}

	// In-root symlink: .tmp/cmd-inroot -> .tmp/other (Finding #3).
	inRootOther := filepath.Join(store.Path(wsVol), tmpName, "other")
	if err := os.Mkdir(inRootOther, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("other", filepath.Join(store.Path(wsVol), tmpName, "cmd-inroot")); err != nil {
		t.Fatal(err)
	}
	if err := makeTmpEntry(store.Path(wsVol), "cmd-inroot"); err == nil {
		t.Fatal("makeTmpEntry succeeded on in-root symlink .tmp/cmd-inroot -> .tmp/other")
	}
	otherFi, err := os.Stat(inRootOther)
	if err != nil {
		t.Fatal(err)
	}
	if otherFi.Mode().Perm() != 0o700 || otherFi.Mode()&os.ModeSticky != 0 {
		t.Fatalf("in-root target mode changed to %v, want 0700 without sticky bit", otherFi.Mode())
	}

	// FIFO inside workspace must return immediately with an error rather than blocking (Finding #4).
	fifoPath := filepath.Join(repoDir, "pipe.fifo")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.File(ctx, "ws1", "app/pipe.fifo"); err == nil || api.CodeOf(err) != api.CodeInvalidArgs {
		t.Fatalf("Engine.File on FIFO = %v (code %v), want INVALID_ARGS", err, api.CodeOf(err))
	}
	// FIFO in .tmp/<id> must also be rejected immediately without blocking.
	tmpFifo := filepath.Join(store.Path(wsVol), tmpName, "cmd-fifo")
	if err := syscall.Mkfifo(tmpFifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := makeTmpEntry(store.Path(wsVol), "cmd-fifo"); err == nil {
		t.Fatal("makeTmpEntry succeeded on FIFO")
	}
	// FIFO as rust-toolchain pin file must not block toolchain().
	_ = os.Remove(filepath.Join(repoDir, "rust-toolchain"))
	if err := syscall.Mkfifo(filepath.Join(repoDir, "rust-toolchain"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = toolchain(store.Path(wsVol), "app")

	// Rootfs volume with symlinked work/ pointing outside volume (Finding #2).
	rootfsVol := "ws/ws-rootfs"
	if err := store.Create(ctx, rootfsVol); err != nil {
		t.Fatal(err)
	}
	rootfsPath := store.Path(rootfsVol)
	if err := os.WriteFile(filepath.Join(rootfsPath, baseimage.ReadyMarker), []byte("k1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outsideWork := t.TempDir()
	outsideApp := filepath.Join(outsideWork, "app")
	if err := os.MkdirAll(outsideApp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideApp, "secret.txt"), []byte("ROOT-SECRET-CONTENT\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideApp, "rust-toolchain"), []byte("1.88.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideWork, filepath.Join(rootfsPath, "work")); err != nil {
		t.Fatal(err)
	}
	eng.mu.Lock()
	eng.workspaces["ws-rootfs"] = &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{
			Id: "ws-rootfs", Project: "app", State: idle,
			Repos: []*pb.WorkspaceRepo{{Repo: "app", Head: baseCommit, BaseCommit: baseCommit}},
		},
		Volume: rootfsVol,
	}}
	eng.mu.Unlock()
	if _, err := eng.File(ctx, "ws-rootfs", "app/secret.txt"); err == nil {
		t.Fatal("Engine.File succeeded through symlinked rootfs work/ dir")
	}
	if _, err := eng.Files(ctx, "ws-rootfs"); err == nil {
		t.Fatal("Engine.Files succeeded through symlinked rootfs work/ dir")
	}
	if r, _, err := openWorkRoot(rootfsPath); err == nil {
		r.Close()
		t.Fatal("openWorkRoot succeeded through symlinked rootfs work/ dir")
	}
	if got := toolchain(rootfsPath, "app"); got != "" {
		t.Fatalf("toolchain() followed symlinked rootfs work/ dir: %q", got)
	}

	// verifyCheckoutPath must detect if dir is swapped to a symlink or another dir (Finding #15).
	openedRoot, openedDir, err := openRepoRoot(store.Path(wsVol), "app")
	if err != nil {
		t.Fatalf("openRepoRoot: %v", err)
	}
	defer openedRoot.Close()
	if err := verifyCheckoutPath(store.Path(wsVol), openedRoot, openedDir); err != nil {
		t.Fatalf("verifyCheckoutPath on genuine checkout: %v", err)
	}
	if err := verifyCheckoutPath(store.Path(wsVol), openedRoot, outsideDir); err == nil {
		t.Fatal("verifyCheckoutPath succeeded when dir pointed to different directory")
	}
	if err := verifyCheckoutPath(store.Path(wsVol), openedRoot, filepath.Join(store.Path(wsVol), "work", "evilrepo")); err == nil {
		t.Fatal("verifyCheckoutPath succeeded when dir was a symlink")
	}
}

// Finding #1: removeTmp and sweepTmp must anchor at volPath via os.OpenRoot so
// a symlinked .tmp directory never deletes anything outside the volume.
func TestTmpCleanupAndSweepRefuseSymlinkedTmpDir(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	outsideSub := filepath.Join(outside, "dead-cmd")
	outsideFile := filepath.Join(outsideSub, "precious.txt")
	if err := os.MkdirAll(outsideSub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsideFile, []byte("do-not-delete"), 0o600); err != nil {
		t.Fatal(err)
	}

	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	eng, err := NewEngine(t.Context(), EngineConfig{
		Dir: dir, NodeID: "n1", Host: "worker1",
		Store: store, BaseImage: fake.BaseImage(store), Units: &fake.Units{}, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })

	wsVol := "ws/ws-symtmp"
	if err := store.Create(t.Context(), wsVol); err != nil {
		t.Fatal(err)
	}
	volPath := store.Path(wsVol)
	if err := os.Symlink(outside, filepath.Join(volPath, tmpName)); err != nil {
		t.Fatal(err)
	}
	eng.mu.Lock()
	eng.workspaces["ws-symtmp"] = &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{Id: "ws-symtmp", Project: "app", State: idle},
		Volume:    wsVol,
	}}
	eng.mu.Unlock()

	removeTmp(volPath, "dead-cmd")
	eng.sweepTmp()

	b, err := os.ReadFile(outsideFile)
	if err != nil || string(b) != "do-not-delete" {
		t.Fatalf("outside file deleted or modified through symlinked .tmp: %q, %v", b, err)
	}
}

// board 8005: remoteHead rejects malicious repo URLs and leading-dash branches
// before spawning host git, and Service.CreateWorkspace admits only the
// controller's tailnet peer when ControllerNodeID is configured.
func TestWorkerRejectsMaliciousRepoURLAndBranchBeforeGit(t *testing.T) {
	// Put a trap git binary first in PATH: if validation misses an input and
	// spawns git, the trap writes a marker file and fails the test.
	trapDir := t.TempDir()
	marker := filepath.Join(trapDir, "git-was-invoked")
	trap := filepath.Join(trapDir, "git")
	if err := os.WriteFile(trap, []byte("#!/bin/sh\necho invoked > "+marker+"\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", trapDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	store := &fake.Store{Root: t.TempDir()}
	eng, err := NewEngine(t.Context(), EngineConfig{
		Dir: t.TempDir(), NodeID: "n1", Host: "w1",
		Store: store, BaseImage: fake.BaseImage(store), Units: &fake.Units{}, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })

	for _, tc := range []struct {
		name   string
		url    string
		branch string
	}{
		{"leading-dash url", "--upload-pack=touch /tmp/pwn", "main"},
		{"ext helper", "ext::sh -c id", "main"},
		{"file url without opt-in", "file:///tmp/repo", "main"},
		{"local path without opt-in", "/tmp/repo", "main"},
		{"leading-dash branch", "https://github.com/semistrict/hotbed.git", "--upload-pack=sh"},
		{"dotdot branch", "https://github.com/semistrict/hotbed.git", "main..evil"},
		{"glob star branch", "https://github.com/semistrict/hotbed.git", "main*"},
		{"glob question branch", "https://github.com/semistrict/hotbed.git", "feat?"},
		{"glob bracket branch", "https://github.com/semistrict/hotbed.git", "a[b]"},
		{"tilde branch", "https://github.com/semistrict/hotbed.git", "main~1"},
		{"caret branch", "https://github.com/semistrict/hotbed.git", "main^"},
		{"colon branch", "https://github.com/semistrict/hotbed.git", "a:b"},
		{"backslash branch", "https://github.com/semistrict/hotbed.git", `a\b`},
		{"reflog branch", "https://github.com/semistrict/hotbed.git", "main@{0}"},
		{"trailing slash branch", "https://github.com/semistrict/hotbed.git", "main/"},
		{"trailing dot branch", "https://github.com/semistrict/hotbed.git", "main."},
		{"trailing lock branch", "https://github.com/semistrict/hotbed.git", "main.lock"},
	} {
		_, err := eng.remoteHead(t.Context(), &pb.Repo{Id: "app", Url: tc.url, Branch: tc.branch})
		if err == nil {
			t.Errorf("%s: remoteHead succeeded, want INVALID_ARGS", tc.name)
			continue
		}
		if code := api.CodeOf(err); code != api.CodeInvalidArgs {
			t.Errorf("%s: remoteHead error code = %q (%v), want %q", tc.name, code, err, api.CodeInvalidArgs)
		}
		if _, statErr := os.Stat(marker); statErr == nil {
			t.Fatalf("%s: trap git was spawned despite invalid input", tc.name)
		}
	}
}

func TestServiceCreateWorkspaceRequiresControllerPeer(t *testing.T) {
	ctrl := tailnet.Peer{NodeID: "nCtrl", Hostname: "ubu1", Login: "ramon@"}
	client := tailnet.Peer{NodeID: "nLaptop", Hostname: "laptop", Login: "ramon@"}
	tn := tailnettest.New(tailnet.Self{Peer: ctrl})

	store := &fake.Store{Root: t.TempDir()}
	eng, err := NewEngine(t.Context(), EngineConfig{
		Dir: t.TempDir(), NodeID: "nW1", Host: "w1",
		Store: store, BaseImage: fake.BaseImage(store), Units: &fake.Units{}, System: fake.NewSystem(),
		ControllerNodeID: ctrl.NodeID,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })

	srv := httptest.NewServer(Handler(tn, httpx.Policy{
		Owner: "ramon@",
		Self:  []netip.Addr{netip.MustParseAddr("127.0.0.1")},
	}, eng))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")

	// Non-controller peer (same owner's laptop) must be refused CreateWorkspace.
	laptopConn, err := api.Dial(addr, api.WorkerPort, tn.Client(client))
	if err != nil {
		t.Fatal(err)
	}
	laptopWS := hotbedv1connect.NewWorkspaceServiceClient(laptopConn)
	_, err = laptopWS.CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{
		Id: "ws-evil", SessionId: "s1",
		Project: &pb.HostedProject{
			Name:  "evil",
			Repos: []*pb.Repo{{Id: "r", Url: "https://github.com/semistrict/hotbed.git"}},
		},
	})
	if code := api.CodeOf(err); code != api.CodeNotAuthorized {
		t.Fatalf("non-controller CreateWorkspace code = %q (%v), want %q", code, err, api.CodeNotAuthorized)
	}

	// Controller peer passes the authorization check (reaches ID/project validation).
	ctrlConn, err := api.Dial(addr, api.WorkerPort, tn.Client(ctrl))
	if err != nil {
		t.Fatal(err)
	}
	ctrlWS := hotbedv1connect.NewWorkspaceServiceClient(ctrlConn)
	_, err = ctrlWS.CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{
		Id: "ws-ok", SessionId: "s1",
		Project: &pb.HostedProject{
			Name:  "app",
			Repos: []*pb.Repo{{Id: "r", Url: "--upload-pack=evil"}},
		},
	})
	if code := api.CodeOf(err); code != api.CodeInvalidArgs {
		t.Fatalf("controller CreateWorkspace with bad URL code = %q (%v), want %q", code, err, api.CodeInvalidArgs)
	}

	// Finding #6: colocated local non-root caller (CallerUID: 1000) sharing the
	// controller's NodeID must be refused CreateWorkspace even when admitted by
	// Gate's LocalUsers policy, while local root (CallerUID: 0) is admitted.
	colocatedTN := tailnettest.New(tailnet.Self{Peer: ctrl})
	colocatedTN.CallerUID = 1000
	colocatedSrv := httptest.NewServer(Handler(colocatedTN, httpx.Policy{
		Owner:      "ramon@",
		Self:       []netip.Addr{netip.MustParseAddr("127.0.0.1")},
		LocalUsers: []uint32{1000},
	}, eng))
	t.Cleanup(colocatedSrv.Close)
	colocatedAddr := strings.TrimPrefix(colocatedSrv.URL, "http://")

	nonRootConn, err := api.Dial(colocatedAddr, api.WorkerPort, colocatedTN.Client(ctrl))
	if err != nil {
		t.Fatal(err)
	}
	nonRootWS := hotbedv1connect.NewWorkspaceServiceClient(nonRootConn)
	_, err = nonRootWS.CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{
		Id: "ws-nonroot", SessionId: "s1",
		Project: &pb.HostedProject{
			Name:  "app",
			Repos: []*pb.Repo{{Id: "r", Url: "--upload-pack=evil"}},
		},
	})
	if code := api.CodeOf(err); code != api.CodeNotAuthorized {
		t.Fatalf("colocated non-root CreateWorkspace code = %q (%v), want %q", code, err, api.CodeNotAuthorized)
	}

	colocatedTN.CallerUID = 0
	rootConn, err := api.Dial(colocatedAddr, api.WorkerPort, colocatedTN.Client(ctrl))
	if err != nil {
		t.Fatal(err)
	}
	rootWS := hotbedv1connect.NewWorkspaceServiceClient(rootConn)
	_, err = rootWS.CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{
		Id: "ws-root", SessionId: "s1",
		Project: &pb.HostedProject{
			Name:  "app",
			Repos: []*pb.Repo{{Id: "r", Url: "--upload-pack=evil"}},
		},
	})
	if code := api.CodeOf(err); code != api.CodeInvalidArgs {
		t.Fatalf("colocated root CreateWorkspace with bad URL code = %q (%v), want %q", code, err, api.CodeInvalidArgs)
	}

	// Finding #5: RequireController: true with empty ControllerNodeID must fail closed.
	fcStore := &fake.Store{Root: t.TempDir()}
	failClosedEng, err := NewEngine(t.Context(), EngineConfig{
		Dir: t.TempDir(), NodeID: "nW2", Host: "w2",
		Store: fcStore, BaseImage: fake.BaseImage(fcStore), Units: &fake.Units{}, System: fake.NewSystem(),
		RequireController: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { failClosedEng.Close() })
	failClosedSrv := httptest.NewServer(Handler(tn, httpx.Policy{
		Owner: "ramon@",
		Self:  []netip.Addr{netip.MustParseAddr("127.0.0.1")},
	}, failClosedEng))
	t.Cleanup(failClosedSrv.Close)
	fcConn, err := api.Dial(strings.TrimPrefix(failClosedSrv.URL, "http://"), api.WorkerPort, tn.Client(ctrl))
	if err != nil {
		t.Fatal(err)
	}
	fcWS := hotbedv1connect.NewWorkspaceServiceClient(fcConn)
	_, err = fcWS.CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{
		Id: "ws-fc", SessionId: "s1",
		Project: &pb.HostedProject{Name: "app"},
	})
	if code := api.CodeOf(err); code != api.CodeNotAuthorized {
		t.Fatalf("RequireController with empty ControllerNodeID code = %q (%v), want %q", code, err, api.CodeNotAuthorized)
	}
}

// Finding #14: parseLSRemote requires an exact ref match and rejects ambiguous
// multi-line matches.
func TestParseLSRemoteExactAndUnambiguous(t *testing.T) {
	sha1 := "1111111111111111111111111111111111111111"
	sha2 := "2222222222222222222222222222222222222222"

	got, err := parseLSRemote(sha1+"\trefs/heads/main\n", "https://example/repo.git", "refs/heads/main")
	if err != nil || got != sha1 {
		t.Fatalf("exact match = (%q, %v), want %q", got, err, sha1)
	}
	// Extra unrelated ref (like refs/remotes/origin/HEAD) is ignored for exact HEAD.
	got, err = parseLSRemote(sha2+"\trefs/remotes/origin/HEAD\n"+sha1+"\tHEAD\n", "https://example/repo.git", "HEAD")
	if err != nil || got != sha1 {
		t.Fatalf("exact HEAD with trailing ref = (%q, %v), want %q", got, err, sha1)
	}
	// Non-matching ref must fail.
	if _, err := parseLSRemote(sha1+"\trefs/heads/main-other\n", "https://example/repo.git", "refs/heads/main"); err == nil {
		t.Fatal("parseLSRemote accepted non-matching ref")
	}
	// Duplicate exact match must fail as ambiguous.
	if _, err := parseLSRemote(sha1+"\trefs/heads/main\n"+sha2+"\trefs/heads/main\n", "https://example/repo.git", "refs/heads/main"); err == nil {
		t.Fatal("parseLSRemote accepted ambiguous duplicate matches")
	}
}

// Finding #13: pre-existing loaded commands with legacy IDs (e.g. containing a
// dot) restored from worker.db must still be stoppable via Engine.Stop while
// path helpers keep enforcing validateID.
func TestStopAllowsLegacyPersistedCommandID(t *testing.T) {
	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	units := &fake.Units{}
	eng, err := NewEngine(t.Context(), EngineConfig{
		Dir: dir, NodeID: "n1", Host: "w1",
		Store: store, BaseImage: fake.BaseImage(store), Units: units, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })

	legacyID := "c.legacy-1"
	rec := &pb.CommandRecord{
		Command: &pb.Command{Id: legacyID, WorkspaceId: "ws1", State: queued},
		Volume:  "ws/ws1",
	}
	eng.mu.Lock()
	eng.commands[legacyID] = eng.newCommand(rec)
	eng.mu.Unlock()

	stopped, err := eng.Stop(context.Background(), legacyID, "workspace released")
	if err != nil {
		t.Fatalf("Engine.Stop(%q) failed: %v", legacyID, err)
	}
	if stopped.State != cancelled {
		t.Fatalf("legacy command state = %v, want cancelled", stopped.State)
	}
	// Path helpers stay protected against legacy/dot IDs.
	if _, err := eng.logPath(legacyID); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Errorf("logPath(%q) = %v, want INVALID_ARGS", legacyID, err)
	}
	if _, err := eng.profilePath(legacyID); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Errorf("profilePath(%q) = %v, want INVALID_ARGS", legacyID, err)
	}
}
