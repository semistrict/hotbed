package worker_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/unit"
	"github.com/semistrict/hotbed/internal/worker"
)

func TestWorkerClonesColdBasesFromSharedBaseImage(t *testing.T) {
	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	units := &fake.Units{}
	var pulls, provisions int
	baseMgr := &baseimage.Manager{
		Store: store,
		Spec:  baseimage.Spec{Image: "test-debian:trixie", Arch: "amd64", User: "hotbed", UID: 1000, GID: 1000, Script: "echo provision"},
		Pull: func(_ context.Context, _, _, rootDir string) error {
			pulls++
			return os.MkdirAll(filepath.Join(rootDir, "usr", "bin"), 0o755)
		},
		Provision: baseimage.ProvisionFunc(func(_ context.Context, rootDir, _ string) error {
			provisions++
			return os.WriteFile(filepath.Join(rootDir, "etc", "hotbed-base-marker"), []byte("shared-base-v1\n"), 0o644)
		}),
	}

	scratch := t.TempDir()
	eng, err := worker.NewEngine(t.Context(), worker.EngineConfig{
		Dir: dir, NodeID: "nMac", Host: "mac-worker",
		Store: store, Units: units, System: fake.NewSystem(),
		BaseImage: baseMgr, ScratchDisk: scratch, AllowLocalRepos: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { eng.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
		eng.Close()
	})

	originDir := origin(t)
	proj := &pb.HostedProject{
		Name:  "proj1",
		Repos: []*pb.Repo{{Id: "app", Url: originDir}},
		Steps: []*pb.Step{{Name: "build", Script: "test -f app/lib.txt", EveryPull: true, Hash: "h1"}},
	}
	if _, err := eng.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{
		Id: "ws1", Repo: "app", SessionId: "s1", Agent: "a", Project: proj,
	}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "workspace cloned from shared base image to be ready", func() bool {
		ws, err := eng.Workspace("ws1")
		return err == nil && ws.State == pb.WorkspaceState_WORKSPACE_STATE_IDLE
	})
	if pulls != 1 || provisions != 1 {
		t.Fatalf("pulls=%d provisions=%d, want 1 each", pulls, provisions)
	}

	// The workspace volume must carry the shared base image marker and checkout under /work/<repo>.
	wsRoot := store.Path("ws/ws1")
	if b, err := os.ReadFile(filepath.Join(wsRoot, "etc", "hotbed-base-marker")); err != nil || string(b) != "shared-base-v1\n" {
		t.Fatalf("workspace missing base image marker: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(wsRoot, "work", "app", "lib.txt")); err != nil {
		t.Fatalf("workspace checkout not under <vol>/work/app: %v", err)
	}

	// Host-side Files() and File() must resolve the checkout inside <vol>/work/app.
	files, err := eng.Files(ctx, "ws1")
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if !slices.ContainsFunc(files, func(f *pb.FileEntry) bool { return f.Path == "app/lib.txt" }) {
		t.Fatalf("Files() did not list app/lib.txt: %+v", files)
	}
	fc, err := eng.File(ctx, "ws1", "app/lib.txt")
	if err != nil || fc.Text != "base\n" {
		t.Fatalf("File(app/lib.txt) = %+v, %v; want base", fc, err)
	}

	// A sync applies in the workspace's rootfs, as its user (board 8333):
	// the bundle where the rootfs sees it, the checkout at /work/app.
	commit, tree, bundle := syncBundle(t, originDir, "lib.txt", "synced\n")
	resp, err := eng.Sync(ctx, &pb.SyncHeader{WorkspaceId: "ws1", Repo: "app", Commit: commit, Tree: tree}, bytes.NewReader(bundle))
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if resp.Skipped || resp.FilesChanged != 1 {
		t.Errorf("Sync = %+v, want 1 file changed", resp)
	}
	if b, err := os.ReadFile(filepath.Join(wsRoot, "work", "app", "lib.txt")); err != nil || string(b) != "synced\n" {
		t.Fatalf("synced lib.txt = %q, %v; want synced", b, err)
	}

	// A command in the workspace must run in a sandbox of the workspace
	// rootfs (board 8145).
	cmd, err := eng.Exec(ctx, &pb.RunCommandRequest{WorkspaceId: "ws1", Repo: "app", Args: []string{"cat", "lib.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "command to finish", func() bool {
		c, ok := eng.Command(cmd.Id)
		return ok && c.State == pb.CommandState_COMMAND_STATE_SUCCEEDED
	})
	spec, ok := units.Spec("hotbed-" + cmd.Id)
	if !ok {
		t.Fatalf("unit spec for %s not recorded", cmd.Id)
	}
	if spec.RootDir != wsRoot {
		t.Errorf("spec.RootDir = %q, want %q", spec.RootDir, wsRoot)
	}
	// The scratch disk, where measurements keep their stores, at its own
	// path (board 7418).
	if !slices.Contains(spec.Binds, unit.Bind{Source: scratch, Target: scratch}) {
		t.Errorf("spec.Binds = %+v, want the scratch disk %s at its own path", spec.Binds, scratch)
	}
	if !spec.TraceFS || !slices.Equal(spec.Capabilities, []string{"CAP_PERFMON"}) {
		t.Errorf("spec tracefs=%v capabilities=%v; want true and [CAP_PERFMON]", spec.TraceFS, spec.Capabilities)
	}
	if want := filepath.Join(wsRoot, ".tmp", cmd.Id, "tmp"); spec.Tmp != want {
		t.Errorf("spec.Tmp = %q, want %q, its own on the volume", spec.Tmp, want)
	}
	if wantRun := filepath.Join(wsRoot, ".tmp", cmd.Id, "run"); !slices.Contains(spec.Binds, unit.Bind{Source: wantRun, Target: "/run/user/1000"}) {
		t.Errorf("spec.Binds = %+v, want XDG_RUNTIME_DIR bind %s -> /run/user/1000", spec.Binds, wantRun)
	}
	if !slices.Contains(spec.Env, "XDG_RUNTIME_DIR=/run/user/1000") {
		t.Errorf("spec.Env missing XDG_RUNTIME_DIR=/run/user/1000: %v", spec.Env)
	}
	if !slices.ContainsFunc(spec.ReadOnlyBinds, func(b unit.Bind) bool { return b.Target == "/etc/containers/containers.conf.d/50-hotbed.conf" }) {
		t.Errorf("spec.ReadOnlyBinds = %+v, want /etc/containers/containers.conf.d/50-hotbed.conf", spec.ReadOnlyBinds)
	}
	if !slices.Contains(spec.Env, "HOME=/work/.hotbed/home") {
		t.Errorf("spec.Env missing HOME=/work/.hotbed/home: %v", spec.Env)
	}

	// A second project's cold base reuses the cached shared base image without pulling again.
	proj2 := &pb.HostedProject{
		Name:  "proj2",
		Repos: []*pb.Repo{{Id: "app2", Url: origin(t)}},
		Steps: []*pb.Step{{Name: "build", Script: "true", EveryPull: true, Hash: "h2"}},
	}
	if _, err := eng.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{
		Id: "ws2", Repo: "app2", SessionId: "s2", Agent: "a", Project: proj2,
	}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "second project workspace to be ready", func() bool {
		ws, err := eng.Workspace("ws2")
		return err == nil && ws.State == pb.WorkspaceState_WORKSPACE_STATE_IDLE
	})
	if pulls != 1 || provisions != 1 {
		t.Fatalf("second project rebuilt shared base image: pulls=%d provisions=%d, want 1 each", pulls, provisions)
	}
}

// syncBundle commits content to file in a clone of origin and returns the
// commit, its tree and a bundle of it against origin's HEAD, as a client's
// sync sends it.
func syncBundle(t *testing.T, origin, file, content string) (commit, tree string, bundle []byte) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
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
	git("clone", "-q", origin, ".")
	base := git("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	git("commit", "-qam", "sync")
	commit, tree = git("rev-parse", "HEAD"), git("rev-parse", "HEAD^{tree}")
	git("update-ref", "refs/hotbed/sync", commit)
	path := filepath.Join(t.TempDir(), "sync.bundle")
	git("bundle", "create", "-q", path, base+"..refs/hotbed/sync")
	bundle, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return commit, tree, bundle
}
