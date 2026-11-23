package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// A run syncs the agent's submodules as they are: edited, untracked and
// unpushed work, which the workspace cannot fetch from any origin.
func TestRunSyncsDirtySubmodules(t *testing.T) {
	// Submodules here clone from local paths, which git refuses by default.
	// (The worker's git sets GIT_CONFIG_COUNT for itself.)
	global := filepath.Join(t.TempDir(), "gitconfig")
	write(t, global, "[protocol \"file\"]\n\tallow = always\n")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	c := newCluster(t)
	lib := t.TempDir()
	gitIn(t, lib, "init", "--quiet", "-b", "main")
	write(t, filepath.Join(lib, "a.txt"), "lib\n")
	write(t, filepath.Join(lib, "c.txt"), "lib\n")
	gitIn(t, lib, "add", "-A")
	gitIn(t, lib, "commit", "--quiet", "-m", "lib")
	origin := upstream(t)
	gitIn(t, origin, "submodule", "add", "--quiet", lib, "lib")
	gitIn(t, origin, "commit", "--quiet", "-m", "add lib")
	c.addRepo(origin)
	sess := &struct{ Id string }{}
	c.ok(sess, "session", "start")

	agent := filepath.Join(t.TempDir(), "agent")
	gitIn(t, filepath.Dir(agent), "clone", "--quiet", "--recurse-submodules", origin, agent)
	sub := filepath.Join(agent, "lib")
	write(t, filepath.Join(sub, "c.txt"), "committed here only\n")
	gitIn(t, sub, "commit", "--quiet", "-am", "local")
	write(t, filepath.Join(sub, "a.txt"), "edited\n")
	write(t, filepath.Join(sub, "new.txt"), "untracked\n")

	t.Chdir(agent)
	code, stdout, stderr := c.hotbed("run", "-s", sess.Id, "-r", "app", "--", "cat", "lib/a.txt", "lib/c.txt", "lib/new.txt")
	if code != 0 || stdout != "edited\ncommitted here only\nuntracked\n" || !strings.Contains(stderr, "synced") {
		t.Fatalf("run: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	// Nothing changed, so the next run sends nothing.
	code, _, stderr = c.hotbed("run", "-s", sess.Id, "-r", "app", "--", "true")
	if code != 0 || strings.Contains(stderr, "synced") {
		t.Errorf("run of an unchanged checkout: exit %d, stderr %q", code, stderr)
	}
	// An edit in the submodule alone is synced.
	write(t, filepath.Join(sub, "a.txt"), "edited again\n")
	code, stdout, stderr = c.hotbed("run", "-s", sess.Id, "-r", "app", "--", "cat", "lib/a.txt")
	if code != 0 || stdout != "edited again\n" {
		t.Errorf("run after a submodule edit: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}
