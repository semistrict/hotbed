package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/sysinfo"
)

func TestShellCommandEndToEnd(t *testing.T) {
	c := newCluster(t)
	origin := upstream(t)
	c.addRepo(origin)
	var sess pb.Session
	c.ok(&sess, "session", "start")

	// 1. Run an interactive shell script over stdin (piped) in repo app.
	// Since app is the project's only repo, hotbed shell starts in /work/app
	// even without -r, and streams the Hotbed-Progress URL while freshening.
	code, out, stderr := c.hotbedIn("echo hi-from-shell\npwd\nexit 0\n", "shell", "-s", sess.Id)
	if code != 0 {
		t.Fatalf("hotbed shell exited %d (out=%q stderr=%q)", code, out, stderr)
	}
	if !strings.Contains(out, "hi-from-shell") {
		t.Errorf("shell stdout = %q, want hi-from-shell", out)
	}
	if !strings.Contains(stderr, "hotbed: watch progress at http://") || !strings.Contains(stderr, "/commands/f-app-g1-") {
		t.Errorf("shell stderr = %q, want Hotbed-Progress URL for base freshen command", stderr)
	}
	if !strings.Contains(stderr, "hotbed: shell in ") || !strings.Contains(stderr, "(/work/app)") {
		t.Errorf("shell stderr = %q, want banner in /work/app", stderr)
	}

	// 2. Run a command after -- with --env and verify exit code propagation.
	code, out, stderr = c.hotbed("shell", "-s", sess.Id, "-r", "app", "--env", "GREETING=ahoy", "--", "sh", "-c", "echo $GREETING; exit 7")
	if code != 7 {
		t.Fatalf("hotbed shell -- exit 7 returned %d (out=%q stderr=%q)", code, out, stderr)
	}
	if strings.TrimSpace(out) != "ahoy" {
		t.Errorf("stdout = %q, want ahoy", out)
	}

	// 3. Default does NOT overwrite remote workspace from local checkout; --sync does.
	work := filepath.Join(t.TempDir(), "agent")
	gitIn(t, filepath.Dir(work), "clone", "--quiet", origin, work)
	if err := os.WriteFile(filepath.Join(work, "local-edit.txt"), []byte("synced-for-shell\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	code, out, stderr = c.hotbed("shell", "-s", sess.Id, "--", "sh", "-c", "test ! -e local-edit.txt && echo untouched")
	if code != 0 || strings.TrimSpace(out) != "untouched" {
		t.Fatalf("hotbed shell without --sync: code=%d out=%q stderr=%q", code, out, stderr)
	}
	code, out, stderr = c.hotbed("shell", "-s", sess.Id, "--sync", "--", "cat", "local-edit.txt")
	if code != 0 || strings.TrimSpace(out) != "synced-for-shell" {
		t.Fatalf("hotbed shell --sync: code=%d out=%q stderr=%q", code, out, stderr)
	}

	// 4. Shelling into a session from an unrelated git checkout does not fail.
	other := filepath.Join(t.TempDir(), "unrelated")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, other, "init", "--quiet")
	gitIn(t, other, "remote", "add", "origin", "https://github.com/example/unrelated-repo.git")
	t.Chdir(other)
	code, out, stderr = c.hotbed("shell", "-s", sess.Id, "--", "cat", "local-edit.txt")
	if code != 0 || strings.TrimSpace(out) != "synced-for-shell" {
		t.Fatalf("hotbed shell from unrelated repo: code=%d out=%q stderr=%q", code, out, stderr)
	}

	// 5. Invalid --env fails before reaching the server.
	code, apiErr := c.fails("shell", "-s", sess.Id, "--env", "BROKEN")
	if code != exitError || apiErr.Code != api.CodeInvalidArgs {
		t.Errorf("bad --env: code=%d err=%+v, want INVALID_ARGS", code, apiErr)
	}
}

func TestShellPrefersExistingWorkspaceOverBusyPlacement(t *testing.T) {
	c := newFleet(t, 2) // asahimbp and mini
	origin := upstream(t)
	c.addRepo(origin)
	var sess pb.Session
	c.ok(&sess, "session", "start")

	// Start a workspace on asahimbp.
	code, _, stderr := c.hotbed("run", "-s", sess.Id, "-r", "app", "--node", "asahimbp", "--", "sh", "-c", "echo on-asahi > /work/app/marker.txt")
	if code != 0 {
		t.Fatalf("seed run on asahimbp failed: %d %q", code, stderr)
	}

	// Make asahimbp look heavily loaded so normal load-based placement would
	// migrate a new workspace to idle mini.
	c.nodes[0].system.SetCPU(sysinfo.CPUShare{Busy: 0.99})
	c.nodes[0].system.SetCPUPressure(80)
	c.nodes[0].eng().Sample()
	c.heartbeat()

	// hotbed shell without --node must stay on asahimbp's existing workspace
	// instead of creating a cold workspace on mini.
	code, out, stderr := c.hotbed("shell", "-s", sess.Id, "--", "cat", "marker.txt")
	if code != 0 || strings.TrimSpace(out) != "on-asahi" {
		t.Fatalf("hotbed shell migrated away from existing workspace: code=%d out=%q stderr=%q", code, out, stderr)
	}
	if !strings.Contains(stderr, "on asahimbp") {
		t.Errorf("stderr = %q, want on asahimbp", stderr)
	}

	var list pb.ListWorkspacesResponse
	c.ok(&list, "workspace", "ls", "-s", sess.Id)
	if len(list.Workspaces) != 1 || list.Workspaces[0].Node != "asahimbp" {
		t.Errorf("workspaces = %v, want only existing workspace on asahimbp", list.Workspaces)
	}
}

func TestWaitReadyPrintsProgressHeaderAndStepsOnRun(t *testing.T) {
	c := newCluster(t)
	origin := upstream(t)
	c.addProject(origin)
	// A slow two-step freshen so WatchWorkspace observes step transitions.
	c.ok(nil, "step", "set", "-p", "app", "step-one", "--every-pull", "--", "sleep 1.2")
	c.ok(nil, "step", "set", "-p", "app", "step-two", "--every-pull", "--", "sleep 1.2")
	var sess pb.Session
	c.ok(&sess, "session", "start")

	code, out, stderr := c.hotbed("run", "-s", sess.Id, "-r", "app", "--", "echo", "done")
	if code != 0 || strings.TrimSpace(out) != "done" {
		t.Fatalf("run failed: code=%d out=%q stderr=%q", code, out, stderr)
	}
	if !strings.Contains(stderr, "hotbed: watch progress at http://") || !strings.Contains(stderr, "/commands/f-app-g1-") {
		t.Errorf("run stderr missing progress URL:\n%s", stderr)
	}
	if !strings.Contains(stderr, "on asahimbp") {
		t.Errorf("run stderr missing node name:\n%s", stderr)
	}
	if !strings.Contains(stderr, "step 1/2 step-one") && !strings.Contains(stderr, "step 2/2 step-two") {
		t.Errorf("run stderr missing live freshen step progress:\n%s", stderr)
	}
}
