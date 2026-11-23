package gitops

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/semistrict/hotbed/internal/api"
)

// fileProtocol lets submodules clone from local paths, which git refuses by
// default.
var fileProtocol = []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=protocol.file.allow", "GIT_CONFIG_VALUE_0=always"}

func subGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := Git{Dir: dir, Env: append(slices.Clone(identity), fileProtocol...)}.Run(t.Context(), args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// origin makes a repo with files, committed on main.
func origin(t *testing.T, files map[string]string) string {
	dir := t.TempDir()
	subGit(t, dir, "init", "--quiet", "-b", "main")
	for name, data := range files {
		write(t, filepath.Join(dir, name), data)
	}
	subGit(t, dir, "add", "-A")
	subGit(t, dir, "commit", "--quiet", "-m", "first")
	return dir
}

func addSubmodule(t *testing.T, dir, url, path string) {
	subGit(t, dir, "submodule", "add", "--quiet", url, path)
	subGit(t, dir, "commit", "--quiet", "-m", "add "+path)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// project is an app whose submodule lib has a submodule deep, and which
// has a second submodule same, and extra, a repo an agent can add as a new
// submodule; origins lists every repo's origin.
type project struct {
	app, lib, extra string
	origins         []string
}

func submoduleProject(t *testing.T) project {
	deep := origin(t, map[string]string{"d.txt": "deep\n"})
	lib := origin(t, map[string]string{"a.txt": "lib\n", ".gitignore": "target/\n"})
	addSubmodule(t, lib, deep, "deep")
	same := origin(t, map[string]string{"s.txt": "same\n"})
	app := origin(t, map[string]string{"main.txt": "app\n"})
	addSubmodule(t, app, lib, "lib")
	addSubmodule(t, app, same, "same")
	extra := origin(t, map[string]string{"x.txt": "extra\n"})
	return project{app: app, lib: lib, extra: extra, origins: []string{deep, lib, same, app, extra}}
}

// cutOff makes the origins unreachable, as a workspace that must get
// everything from its sync bundles would find them.
func (p project) cutOff(t *testing.T) {
	for _, o := range p.origins {
		if err := os.Rename(o, o+".gone"); err != nil {
			t.Fatal(err)
		}
	}
}

// fileBundles writes each submodule bundle to a file, as the worker does.
func fileBundles(t *testing.T, subs []SubBundle) (paths string, checkouts []SubCheckout) {
	var names []string
	for _, s := range subs {
		names = append(names, s.Path)
		f := filepath.Join(t.TempDir(), "sub.bundle")
		if err := os.WriteFile(f, s.Data, 0o644); err != nil {
			t.Fatal(err)
		}
		checkouts = append(checkouts, SubCheckout{Path: s.Path, Commit: s.Commit, Bundle: f})
	}
	return strings.Join(names, " "), checkouts
}

func writeBundle(t *testing.T, b []byte) string {
	f := filepath.Join(t.TempDir(), "top.bundle")
	if err := os.WriteFile(f, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func clone(t *testing.T, url string) string {
	dir := filepath.Join(t.TempDir(), "checkout")
	subGit(t, filepath.Dir(dir), "clone", "--quiet", "--recurse-submodules", url, dir)
	return dir
}

func TestSyncRestoresDirtySubmodulesRecursively(t *testing.T) {
	p := submoduleProject(t)
	agent := clone(t, p.app)
	ws := clone(t, p.app) // the workspace, at the base
	base := subGit(t, agent, "rev-parse", "HEAD")

	// lib: an edited file, an untracked one and an ignored one.
	write(t, filepath.Join(agent, "lib/a.txt"), "lib edited\n")
	write(t, filepath.Join(agent, "lib/new.txt"), "lib new\n")
	write(t, filepath.Join(agent, "lib/target/big"), "artifact\n")
	// lib/deep: a commit no remote has, and an edit on top of it.
	write(t, filepath.Join(agent, "lib/deep/d.txt"), "deep committed\n")
	subGit(t, filepath.Join(agent, "lib/deep"), "commit", "--quiet", "-am", "local only")
	write(t, filepath.Join(agent, "lib/deep/d.txt"), "deep dirty\n")
	// extra: a submodule new since the base, edited.
	subGit(t, agent, "submodule", "add", "--quiet", p.extra, "extra")
	write(t, filepath.Join(agent, "extra/x.txt"), "extra dirty\n")
	libHead := subGit(t, filepath.Join(agent, "lib"), "rev-parse", "HEAD")
	libStatus := subGit(t, filepath.Join(agent, "lib"), "status", "--porcelain")

	snap, err := Snapshot(t.Context(), agent)
	if err != nil {
		t.Fatal(err)
	}
	if got := subGit(t, filepath.Join(agent, "lib"), "rev-parse", "HEAD"); got != libHead {
		t.Errorf("lib's HEAD moved to %s", got)
	}
	if got := subGit(t, filepath.Join(agent, "lib"), "status", "--porcelain"); got != libStatus {
		t.Errorf("lib's status after snapshot:\n%s\nwant\n%s", got, libStatus)
	}
	again, err := Snapshot(t.Context(), agent)
	if err != nil {
		t.Fatal(err)
	}
	if again.Tree != snap.Tree || again.Commit != snap.Commit {
		t.Errorf("snapshots of the same state differ: %s/%s then %s/%s", snap.Commit, snap.Tree, again.Commit, again.Tree)
	}

	var top bytes.Buffer
	subs, err := Bundle(t.Context(), snap, base, &top)
	if err != nil {
		t.Fatal(err)
	}
	paths, checkouts := fileBundles(t, subs)
	if got := paths; got != "extra lib lib/deep" {
		t.Errorf("bundled submodules %q, want the changed ones, parents first: extra lib lib/deep", got)
	}
	for _, dir := range []string{agent, filepath.Join(agent, "lib"), filepath.Join(agent, "lib/deep"), filepath.Join(agent, "extra")} {
		if out := subGit(t, dir, "for-each-ref", "refs/hotbed"); out != "" {
			t.Errorf("Bundle left a ref behind in %s: %s", dir, out)
		}
	}
	p.cutOff(t)
	n, err := Apply(t.Context(), Git{Dir: ws, Env: identity}, writeBundle(t, top.Bytes()), snap.Commit, base, checkouts)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("Apply changed %d files, want 3 (.gitmodules, extra, lib)", n)
	}
	for path, want := range map[string]string{
		"lib/a.txt":      "lib edited\n",
		"lib/new.txt":    "lib new\n",
		"lib/deep/d.txt": "deep dirty\n",
		"extra/x.txt":    "extra dirty\n",
		"same/s.txt":     "same\n",
	} {
		if got := readFile(t, filepath.Join(ws, path)); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(ws, "lib/target")); !os.IsNotExist(err) {
		t.Errorf("ignored lib/target was synced: %v", err)
	}
	// The new submodule is one as git submodule update makes: its repo in
	// the parent's, its origin the one .gitmodules names.
	if got := subGit(t, filepath.Join(ws, "extra"), "rev-parse", "--absolute-git-dir"); got != subGit(t, ws, "rev-parse", "--absolute-git-dir")+"/modules/extra" {
		t.Errorf("extra's git dir is %s", got)
	}
	if got := subGit(t, filepath.Join(ws, "extra"), "remote", "get-url", "origin"); got != p.extra {
		t.Errorf("extra's origin is %s, want %s", got, p.extra)
	}
	status, err := Git{Dir: ws}.Lines(t.Context(), "submodule", "status", "--recursive")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range status {
		if !strings.HasPrefix(line, " ") {
			t.Errorf("workspace submodule out of step: %q", line)
		}
	}

	// The workspace's state is the agent's, exactly: snapshotting it gives
	// the same tree.
	mirror, err := Snapshot(t.Context(), ws)
	if err != nil {
		t.Fatal(err)
	}
	if mirror.Tree != snap.Tree {
		t.Errorf("workspace tree %s, want the agent's %s", mirror.Tree, snap.Tree)
	}
}

func TestBundleSkipsUnchangedSubmodules(t *testing.T) {
	p := submoduleProject(t)
	agent := clone(t, p.app)
	base := subGit(t, agent, "rev-parse", "HEAD")
	write(t, filepath.Join(agent, "main.txt"), "edited\n")
	snap, err := Snapshot(t.Context(), agent)
	if err != nil {
		t.Fatal(err)
	}
	if want := subGit(t, agent, "rev-parse", "HEAD:lib"); snap.Subs[0].Path != "lib" || snap.Subs[0].Commit != want {
		t.Errorf("clean lib snapshot = %+v, want its HEAD %s", snap.Subs[0], want)
	}
	subs, err := Bundle(t.Context(), snap, base, &bytes.Buffer{})
	if err != nil || len(subs) != 0 {
		t.Errorf("Bundle = %v, %v; want no submodule bundles", subs, err)
	}
}

// When this checkout lacks the submodule commit the base records, a
// submodule commit its origin has is left for the workspace to fetch, and
// one only this checkout has goes whole.
func TestBundleWithoutTheBasesSubmoduleCommit(t *testing.T) {
	p := submoduleProject(t)
	agent := clone(t, p.app)
	// The base moved lib on to a commit this checkout never fetched.
	write(t, filepath.Join(p.lib, "a.txt"), "lib later\n")
	subGit(t, p.lib, "commit", "--quiet", "-am", "later")
	subGit(t, filepath.Join(p.app, "lib"), "pull", "--quiet", "--no-recurse-submodules")
	subGit(t, p.app, "commit", "--quiet", "-am", "move lib")
	ws := clone(t, p.app)
	subGit(t, agent, "fetch", "--quiet", "--no-recurse-submodules")
	base := subGit(t, agent, "rev-parse", "origin/main")
	write(t, filepath.Join(agent, "main.txt"), "edited\n")

	snap, err := Snapshot(t.Context(), agent)
	if err != nil {
		t.Fatal(err)
	}
	var top bytes.Buffer
	subs, err := Bundle(t.Context(), snap, base, &top)
	if err != nil || len(subs) != 0 {
		t.Fatalf("Bundle = %v, %v; want lib's published commit left to its origin", subs, err)
	}
	if _, err := Apply(t.Context(), Git{Dir: ws, Env: append(slices.Clone(identity), fileProtocol...)}, writeBundle(t, top.Bytes()), snap.Commit, base, nil); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(ws, "lib/a.txt")); got != "lib\n" {
		t.Errorf("lib/a.txt = %q, want the agent's lib", got)
	}

	write(t, filepath.Join(agent, "lib/a.txt"), "lib local\n")
	snap, err = Snapshot(t.Context(), agent)
	if err != nil {
		t.Fatal(err)
	}
	subs, err = Bundle(t.Context(), snap, base, &bytes.Buffer{})
	if err != nil || len(subs) != 1 || subs[0].Path != "lib" {
		t.Fatalf("Bundle = %v, %v; want lib whole", subs, err)
	}
	headerEnd := bytes.Index(subs[0].Data, []byte("\n\n"))
	if headerEnd < 0 {
		t.Fatalf("lib's bundle missing header terminator")
	}
	if header := subs[0].Data[:headerEnd]; bytes.Contains(header, []byte("\n-")) {
		t.Errorf("lib's bundle has prerequisites the workspace may lack:\n%s", header)
	}
}

func TestPinSubsKeepsOnlySnapshots(t *testing.T) {
	p := submoduleProject(t)
	agent := clone(t, p.app)
	write(t, filepath.Join(agent, "lib/a.txt"), "lib edited\n")
	snap, err := Snapshot(t.Context(), agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := snap.PinSubs(t.Context(), "refs/hotbed/reviews/"); err != nil {
		t.Fatal(err)
	}
	lib := snap.Subs[0]
	if got := subGit(t, filepath.Join(agent, "lib"), "for-each-ref", "--format=%(refname)", "refs/hotbed"); got != "refs/hotbed/reviews/"+lib.Commit {
		t.Errorf("lib's pins = %q, want its snapshot %s", got, lib.Commit)
	}
	for _, clean := range []string{"lib/deep", "same"} {
		if got := subGit(t, filepath.Join(agent, clean), "for-each-ref", "refs/hotbed"); got != "" {
			t.Errorf("clean %s pinned %q", clean, got)
		}
	}
}

// A submodule moved since the base keeps its name, and so its repo in the
// workspace and the base's commit to bundle against; its new path holds a
// space and glob characters that must stay literal.
func TestSyncCarriesMovedSubmodule(t *testing.T) {
	p := submoduleProject(t)
	agent := clone(t, p.app)
	ws := clone(t, p.app)
	base := subGit(t, agent, "rev-parse", "HEAD")
	const moved = "vendor/my lib[1]"
	if err := os.Mkdir(filepath.Join(agent, "vendor"), 0o755); err != nil {
		t.Fatal(err)
	}
	subGit(t, agent, "mv", "lib", moved)
	write(t, filepath.Join(agent, moved, "a.txt"), "moved and edited\n")

	snap, err := Snapshot(t.Context(), agent)
	if err != nil {
		t.Fatal(err)
	}
	var top bytes.Buffer
	subs, err := Bundle(t.Context(), snap, base, &top)
	if err != nil {
		t.Fatal(err)
	}
	paths, checkouts := fileBundles(t, subs)
	if paths != moved {
		t.Fatalf("bundled %q, want %q", paths, moved)
	}
	if !bytes.Contains(subs[0].Data, []byte("\n-")) {
		t.Errorf("the moved submodule's bundle has no prerequisite: it went whole, not against the base's lib")
	}
	p.cutOff(t)
	if _, err := Apply(t.Context(), Git{Dir: ws, Env: identity}, writeBundle(t, top.Bytes()), snap.Commit, base, checkouts); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(ws, moved, "a.txt")); got != "moved and edited\n" {
		t.Errorf("moved a.txt = %q", got)
	}
	if got := readFile(t, filepath.Join(ws, moved, "deep/d.txt")); got != "deep\n" {
		t.Errorf("moved lib's deep/d.txt = %q", got)
	}
	mirror, err := Snapshot(t.Context(), ws)
	if err != nil {
		t.Fatal(err)
	}
	if mirror.Tree != snap.Tree {
		t.Errorf("workspace tree %s, want the agent's %s", mirror.Tree, snap.Tree)
	}
}

func TestSnapshotRefusesRepoNoSubmoduleNames(t *testing.T) {
	dir, _ := agentRepo(t)
	nested := filepath.Join(dir, "nested")
	subGit(t, dir, "init", "--quiet", nested)
	write(t, filepath.Join(nested, "f"), "f\n")
	subGit(t, nested, "add", "f")
	subGit(t, nested, "commit", "--quiet", "-m", "f")
	subGit(t, dir, "add", "nested")
	_, err := Snapshot(t.Context(), dir)
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeSyncRefused || !strings.Contains(e.Message, "no .gitmodules entry") {
		t.Errorf("Snapshot = %v, want SYNC_REFUSED naming the unlisted repo", err)
	}
}

// Apply checks a submodule bundle out only where the snapshot records it.
func TestApplyRefusesSubmoduleTheSnapshotDoesNotRecord(t *testing.T) {
	p := submoduleProject(t)
	agent := clone(t, p.app)
	ws := clone(t, p.app)
	base := subGit(t, agent, "rev-parse", "HEAD")
	write(t, filepath.Join(agent, "lib/a.txt"), "edited\n")
	snap, err := Snapshot(t.Context(), agent)
	if err != nil {
		t.Fatal(err)
	}
	var top bytes.Buffer
	subs, err := Bundle(t.Context(), snap, base, &top)
	if err != nil {
		t.Fatal(err)
	}
	_, checkouts := fileBundles(t, subs)
	checkouts[0].Path = "main.txt/x"
	_, err = Apply(t.Context(), Git{Dir: ws, Env: identity}, writeBundle(t, top.Bytes()), snap.Commit, base, checkouts)
	if err == nil || !strings.Contains(err.Error(), "not the snapshot") {
		t.Errorf("Apply = %v, want it refused", err)
	}
	var applyErr *ApplyError
	if !errors.As(err, &applyErr) || !applyErr.Mutated || !applyErr.RolledBack || applyErr.Inconsistent() {
		t.Errorf("Apply error = %#v, want Mutated=true RolledBack=true", applyErr)
	}
	if got := subGit(t, ws, "rev-parse", "HEAD"); got != base {
		t.Errorf("ws HEAD after refused apply = %s, want rolled back to %s", got, base)
	}
}

// board 8019: a failure after the top-level checkout (here, a corrupt nested
// submodule bundle after top-level, new submodule extra, and parent submodule
// lib already moved) rolls the workspace and all its submodules cleanly back
// to the previous checkout, even with origins unreachable.
func TestApplyRollsBackOnPostCheckoutSubmoduleFailure(t *testing.T) {
	p := submoduleProject(t)
	agent := clone(t, p.app)
	ws := clone(t, p.app)
	base := subGit(t, agent, "rev-parse", "HEAD")

	// First apply snapshot A so ws is on a synced snapshot (detached HEAD).
	write(t, filepath.Join(agent, "main.txt"), "app A\n")
	write(t, filepath.Join(agent, "lib/a.txt"), "lib A\n")
	write(t, filepath.Join(agent, "lib/deep/d.txt"), "deep A\n")
	snapA, err := Snapshot(t.Context(), agent)
	if err != nil {
		t.Fatal(err)
	}
	var topA bytes.Buffer
	subsA, err := Bundle(t.Context(), snapA, base, &topA)
	if err != nil {
		t.Fatal(err)
	}
	_, checkoutsA := fileBundles(t, subsA)
	if _, err := Apply(t.Context(), Git{Dir: ws, Env: identity}, writeBundle(t, topA.Bytes()), snapA.Commit, base, checkoutsA); err != nil {
		t.Fatalf("Apply A: %v", err)
	}

	// Prepare snapshot B modifying top-level, adding a new submodule, and
	// modifying both lib and lib/deep.
	write(t, filepath.Join(agent, "main.txt"), "app B\n")
	write(t, filepath.Join(agent, "new-in-b.txt"), "only in B\n")
	subGit(t, agent, "submodule", "add", "--quiet", p.extra, "extra")
	write(t, filepath.Join(agent, "extra/x.txt"), "extra B\n")
	write(t, filepath.Join(agent, "lib/a.txt"), "lib B\n")
	write(t, filepath.Join(agent, "lib/deep/d.txt"), "deep B\n")
	snapB, err := Snapshot(t.Context(), agent)
	if err != nil {
		t.Fatal(err)
	}
	var topB bytes.Buffer
	subsB, err := Bundle(t.Context(), snapB, base, &topB)
	if err != nil {
		t.Fatal(err)
	}
	_, checkoutsB := fileBundles(t, subsB)

	// Corrupt the final nested submodule bundle (lib/deep) so top-level checkout,
	// extra, and lib all mutate disk before failing.
	corrupted := false
	for i := range checkoutsB {
		if checkoutsB[i].Path == "lib/deep" {
			if err := os.WriteFile(checkoutsB[i].Bundle, []byte("not a valid git bundle\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			corrupted = true
		}
	}
	if !corrupted {
		t.Fatalf("expected lib/deep in checkoutsB: %+v", checkoutsB)
	}

	p.cutOff(t)
	_, err = Apply(t.Context(), Git{Dir: ws, Env: identity}, writeBundle(t, topB.Bytes()), snapB.Commit, base, checkoutsB)
	if err == nil {
		t.Fatal("Apply with corrupt lib/deep bundle succeeded, want error")
	}
	var applyErr *ApplyError
	if !errors.As(err, &applyErr) {
		t.Fatalf("Apply error type = %T (%v), want *ApplyError", err, err)
	}
	if !applyErr.Mutated || !applyErr.RolledBack || applyErr.Inconsistent() || applyErr.RollbackErr != nil {
		t.Fatalf("ApplyError = %+v, want Mutated=true RolledBack=true Inconsistent=false", applyErr)
	}

	if got := subGit(t, ws, "rev-parse", "HEAD"); got != snapA.Commit {
		t.Errorf("HEAD after rollback = %s, want %s", got, snapA.Commit)
	}
	for path, want := range map[string]string{
		"main.txt":       "app A\n",
		"lib/a.txt":      "lib A\n",
		"lib/deep/d.txt": "deep A\n",
		"same/s.txt":     "same\n",
	} {
		if got := readFile(t, filepath.Join(ws, path)); got != want {
			t.Errorf("%s after rollback = %q, want %q", path, got, want)
		}
	}
	for _, absent := range []string{"new-in-b.txt", "extra"} {
		if _, err := os.Stat(filepath.Join(ws, absent)); !os.IsNotExist(err) {
			t.Errorf("%s still exists after rollback: %v", absent, err)
		}
	}
	status, err := Git{Dir: ws}.Lines(t.Context(), "submodule", "status", "--recursive")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range status {
		if !strings.HasPrefix(line, " ") {
			t.Errorf("submodule out of step after rollback: %q", line)
		}
	}
	mirror, err := Snapshot(t.Context(), ws)
	if err != nil {
		t.Fatal(err)
	}
	if mirror.Tree != snapA.Tree {
		t.Errorf("workspace tree after rollback = %s, want snapA %s", mirror.Tree, snapA.Tree)
	}
}

// board 8019: if a brand-new submodule fails during checkout before
// absorbgitdirs, checkoutSub removes the unabsorbed directory so rollback
// cleanly restores the previous checkout.
func TestApplyRollsBackOnFailedNewSubmoduleCheckout(t *testing.T) {
	p := submoduleProject(t)
	agent := clone(t, p.app)
	ws := clone(t, p.app)
	base := subGit(t, agent, "rev-parse", "HEAD")

	write(t, filepath.Join(agent, "main.txt"), "app with broken extra\n")
	subGit(t, agent, "submodule", "add", "--quiet", p.extra, "extra")
	write(t, filepath.Join(agent, "extra/x.txt"), "extra dirty\n")
	snap, err := Snapshot(t.Context(), agent)
	if err != nil {
		t.Fatal(err)
	}
	var top bytes.Buffer
	subs, err := Bundle(t.Context(), snap, base, &top)
	if err != nil {
		t.Fatal(err)
	}
	_, checkouts := fileBundles(t, subs)
	if len(checkouts) != 1 || checkouts[0].Path != "extra" {
		t.Fatalf("checkouts = %+v, want single extra", checkouts)
	}
	if err := os.WriteFile(checkouts[0].Bundle, []byte("corrupt extra bundle\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p.cutOff(t)
	_, err = Apply(t.Context(), Git{Dir: ws, Env: identity}, writeBundle(t, top.Bytes()), snap.Commit, base, checkouts)
	if err == nil {
		t.Fatal("Apply with corrupt extra bundle succeeded")
	}
	var applyErr *ApplyError
	if !errors.As(err, &applyErr) || !applyErr.Mutated || !applyErr.RolledBack || applyErr.Inconsistent() {
		t.Fatalf("ApplyError = %+v, want Mutated=true RolledBack=true", applyErr)
	}
	if got := subGit(t, ws, "rev-parse", "HEAD"); got != base {
		t.Errorf("HEAD = %s, want %s", got, base)
	}
	if got := readFile(t, filepath.Join(ws, "main.txt")); got != "app\n" {
		t.Errorf("main.txt = %q, want %q", got, "app\n")
	}
	if _, err := os.Stat(filepath.Join(ws, "extra")); !os.IsNotExist(err) {
		t.Errorf("extra still exists after rollback: %v", err)
	}
}

// board 8019: when a failure occurs after top-level checkout and rollback
// itself fails, Apply returns an *ApplyError reporting Inconsistent() == true
// and the rollback error.
func TestApplyReportsInconsistentWhenRollbackFails(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	p := submoduleProject(t)
	agent := clone(t, p.app)
	ws := clone(t, p.app)
	base := subGit(t, agent, "rev-parse", "HEAD")

	write(t, filepath.Join(agent, "main.txt"), "app B\n")
	write(t, filepath.Join(agent, "lib/a.txt"), "lib B\n")
	snapB, err := Snapshot(t.Context(), agent)
	if err != nil {
		t.Fatal(err)
	}
	var topB bytes.Buffer
	subsB, err := Bundle(t.Context(), snapB, base, &topB)
	if err != nil {
		t.Fatal(err)
	}
	_, checkoutsB := fileBundles(t, subsB)
	if len(checkoutsB) == 0 {
		t.Fatal("expected lib checkout")
	}
	// Corrupt lib's bundle so Apply fails after top-level checkout moves HEAD to snapB.
	if err := os.WriteFile(checkoutsB[0].Bundle, []byte("corrupt\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Wrap git so checkout of base (the rollback target) fails while checkout of
	// snapB succeeds.
	binDir := t.TempDir()
	wrapper := "#!/bin/sh\n" +
		"case \" $* \" in\n" +
		"  *\" checkout \"*\" " + base + " \"*)\n" +
		"    echo \"injected rollback failure\" >&2\n" +
		"    exit 1\n" +
		"    ;;\n" +
		"esac\n" +
		"exec \"" + realGit + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err = Apply(t.Context(), Git{Dir: ws, Env: identity}, writeBundle(t, topB.Bytes()), snapB.Commit, base, checkoutsB)
	if err == nil {
		t.Fatal("Apply succeeded, want error")
	}
	var applyErr *ApplyError
	if !errors.As(err, &applyErr) {
		t.Fatalf("Apply error type = %T (%v), want *ApplyError", err, err)
	}
	if !applyErr.Mutated || applyErr.RolledBack || !applyErr.Inconsistent() || applyErr.RollbackErr == nil {
		t.Fatalf("ApplyError = %+v, want Mutated=true RolledBack=false Inconsistent=true RollbackErr!=nil", applyErr)
	}
	if !strings.Contains(err.Error(), "inconsistent") || !strings.Contains(applyErr.RollbackErr.Error(), "injected rollback failure") {
		t.Errorf("error message = %q, rollbackErr = %v", err.Error(), applyErr.RollbackErr)
	}
}

// A failure before checkout mutates disk (corrupt top-level bundle) must not
// report Mutated or Inconsistent and leaves the checkout untouched.
func TestApplyFailureBeforeCheckoutDoesNotMutate(t *testing.T) {
	p := submoduleProject(t)
	ws := clone(t, p.app)
	base := subGit(t, ws, "rev-parse", "HEAD")

	badBundle := writeBundle(t, []byte("not a bundle\n"))
	_, err := Apply(t.Context(), Git{Dir: ws, Env: identity}, badBundle, base, base, nil)
	if err == nil {
		t.Fatal("Apply with corrupt top-level bundle succeeded")
	}
	var applyErr *ApplyError
	if errors.As(err, &applyErr) && (applyErr.Mutated || applyErr.Inconsistent()) {
		t.Fatalf("ApplyError = %+v, want non-mutated failure", applyErr)
	}
	if got := subGit(t, ws, "rev-parse", "HEAD"); got != base {
		t.Errorf("HEAD = %s, want untouched %s", got, base)
	}
}

// board 8888: if a brand-new submodule's destination already exists with
// untracked workspace files (e.g. build outputs), checkoutSub must refuse
// before git init and preserve the pre-existing files untouched on rollback.
func TestApplyRefusesNonEmptyNewSubmoduleDirAndPreservesUntrackedFiles(t *testing.T) {
	p := submoduleProject(t)
	agent := clone(t, p.app)
	ws := clone(t, p.app)
	base := subGit(t, agent, "rev-parse", "HEAD")

	// Pre-existing untracked build file at the new submodule's path.
	keepPath := filepath.Join(ws, "extra", "keep.txt")
	write(t, keepPath, "precious build output\n")

	write(t, filepath.Join(agent, "main.txt"), "app adding extra\n")
	subGit(t, agent, "submodule", "add", "--quiet", p.extra, "extra")
	write(t, filepath.Join(agent, "extra/x.txt"), "extra dirty\n")
	snap, err := Snapshot(t.Context(), agent)
	if err != nil {
		t.Fatal(err)
	}
	var top bytes.Buffer
	subs, err := Bundle(t.Context(), snap, base, &top)
	if err != nil {
		t.Fatal(err)
	}
	_, checkouts := fileBundles(t, subs)
	if len(checkouts) != 1 || checkouts[0].Path != "extra" {
		t.Fatalf("checkouts = %+v, want single extra", checkouts)
	}
	// Inject a failing bundle as well: refusal must happen before git init or
	// any cleanup, leaving ws/extra/keep.txt completely untouched.
	if err := os.WriteFile(checkouts[0].Bundle, []byte("corrupt extra bundle\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p.cutOff(t)
	_, err = Apply(t.Context(), Git{Dir: ws, Env: identity}, writeBundle(t, top.Bytes()), snap.Commit, base, checkouts)
	if err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("Apply = %v, want refusal mentioning non-empty destination", err)
	}
	var applyErr *ApplyError
	if !errors.As(err, &applyErr) || !applyErr.Mutated || !applyErr.RolledBack || applyErr.Inconsistent() {
		t.Fatalf("ApplyError = %+v, want Mutated=true RolledBack=true Inconsistent=false", applyErr)
	}
	if got := readFile(t, keepPath); got != "precious build output\n" {
		t.Errorf("pre-existing untracked file = %q, want %q", got, "precious build output\n")
	}
	if _, err := os.Lstat(filepath.Join(ws, "extra", ".git")); !os.IsNotExist(err) {
		t.Errorf("extra/.git exists after refused submodule init: %v", err)
	}
	if got := subGit(t, ws, "rev-parse", "HEAD"); got != base {
		t.Errorf("HEAD after rollback = %s, want %s", got, base)
	}
	if got := readFile(t, filepath.Join(ws, "main.txt")); got != "app\n" {
		t.Errorf("main.txt after rollback = %q, want %q", got, "app\n")
	}
}

// board 8894: Apply preflight must verify that the bundle's fetched SyncRef
// actually resolves to the requested commit, not merely that the commit exists
// somewhere in the local git object database from an earlier sync.
func TestApplyRejectsBundleCommitMismatchEvenWhenCommitExistsLocally(t *testing.T) {
	p := submoduleProject(t)
	agent := clone(t, p.app)
	ws := clone(t, p.app)
	base := subGit(t, agent, "rev-parse", "HEAD")

	// Snapshot A.
	write(t, filepath.Join(agent, "main.txt"), "app A\n")
	write(t, filepath.Join(agent, "lib/a.txt"), "lib A\n")
	snapA, err := Snapshot(t.Context(), agent)
	if err != nil {
		t.Fatal(err)
	}
	var topA bytes.Buffer
	subsA, err := Bundle(t.Context(), snapA, base, &topA)
	if err != nil {
		t.Fatal(err)
	}
	_, checkoutsA := fileBundles(t, subsA)
	topAPath := writeBundle(t, topA.Bytes())

	// Snapshot B.
	write(t, filepath.Join(agent, "main.txt"), "app B\n")
	write(t, filepath.Join(agent, "lib/a.txt"), "lib B\n")
	snapB, err := Snapshot(t.Context(), agent)
	if err != nil {
		t.Fatal(err)
	}
	var topB bytes.Buffer
	subsB, err := Bundle(t.Context(), snapB, base, &topB)
	if err != nil {
		t.Fatal(err)
	}
	_, checkoutsB := fileBundles(t, subsB)
	topBPath := writeBundle(t, topB.Bytes())

	p.cutOff(t)

	// Apply A then B so both commit A and commit B (and lib's A and B) are
	// present in ws's local git object databases, with checkout currently at B.
	if _, err := Apply(t.Context(), Git{Dir: ws, Env: identity}, topAPath, snapA.Commit, base, checkoutsA); err != nil {
		t.Fatalf("Apply A: %v", err)
	}
	if _, err := Apply(t.Context(), Git{Dir: ws, Env: identity}, topBPath, snapB.Commit, base, checkoutsB); err != nil {
		t.Fatalf("Apply B: %v", err)
	}

	// 1. Top-level mismatch: request commit A with bundle B. Must fail before
	// mutating the checkout (Mutated == false).
	_, err = Apply(t.Context(), Git{Dir: ws, Env: identity}, topBPath, snapA.Commit, base, checkoutsA)
	if err == nil || !strings.Contains(err.Error(), snapB.Commit) || !strings.Contains(err.Error(), snapA.Commit) {
		t.Fatalf("Apply(commit A, bundle B) = %v, want mismatch naming both commits", err)
	}
	var applyErr *ApplyError
	if errors.As(err, &applyErr) && (applyErr.Mutated || applyErr.Inconsistent()) {
		t.Fatalf("ApplyError = %+v, want non-mutated refusal", applyErr)
	}
	if got := subGit(t, ws, "rev-parse", "HEAD"); got != snapB.Commit {
		t.Errorf("HEAD after refused mismatch = %s, want untouched %s", got, snapB.Commit)
	}
	if got := readFile(t, filepath.Join(ws, "main.txt")); got != "app B\n" {
		t.Errorf("main.txt after refused mismatch = %q, want %q", got, "app B\n")
	}

	// 2. Submodule mismatch: valid top-level A, but lib's SubCheckout requests
	// libA while supplying libB's bundle. Must reject and cleanly roll back to B.
	if len(checkoutsA) != 1 || len(checkoutsB) != 1 {
		t.Fatalf("expected 1 submodule checkout each, got %d and %d", len(checkoutsA), len(checkoutsB))
	}
	mismatchedSubs := []SubCheckout{{
		Path:   checkoutsA[0].Path,
		Commit: checkoutsA[0].Commit,
		Bundle: checkoutsB[0].Bundle,
	}}
	_, err = Apply(t.Context(), Git{Dir: ws, Env: identity}, topAPath, snapA.Commit, base, mismatchedSubs)
	if err == nil || !strings.Contains(err.Error(), checkoutsB[0].Commit) {
		t.Fatalf("Apply with mismatched submodule bundle = %v, want mismatch error", err)
	}
	if !errors.As(err, &applyErr) || !applyErr.Mutated || !applyErr.RolledBack || applyErr.Inconsistent() {
		t.Fatalf("ApplyError = %+v, want Mutated=true RolledBack=true Inconsistent=false", applyErr)
	}
	if got := subGit(t, ws, "rev-parse", "HEAD"); got != snapB.Commit {
		t.Errorf("HEAD after submodule mismatch rollback = %s, want %s", got, snapB.Commit)
	}
	if got := readFile(t, filepath.Join(ws, "lib/a.txt")); got != "lib B\n" {
		t.Errorf("lib/a.txt after submodule mismatch rollback = %q, want %q", got, "lib B\n")
	}
}
