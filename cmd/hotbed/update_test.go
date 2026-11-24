package main

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/update"
	"github.com/semistrict/hotbed/internal/update/updatetest"
	"github.com/semistrict/hotbed/internal/version"
	"github.com/semistrict/hotbed/internal/worker"
)

func writeExe(t *testing.T, content string) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "hotbed")
	if err := os.WriteFile(exe, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The controller runs build 7, which it keeps; its worker and a client on
// a CI build install it over themselves, from the controller, never from
// GitHub.
func TestNodesFollowTheControllersBuild(t *testing.T) {
	g := updatetest.NewGitHub(t)
	g.Publish(7)
	cache := update.Cache{Dir: t.TempDir()}
	rel, err := g.Client().Latest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Put(t.Context(), g.Client(), rel); err != nil {
		t.Fatal(err)
	}
	workerExe := writeExe(t, "old worker")
	c := newClusterWith(t, clusterOpts{
		server: func(s *testServerConfig) { s.Builds, s.Build = &cache, 7 },
		follow: func(ctrl string, hc *http.Client) *worker.Follow {
			return &worker.Follow{URL: "http://" + ctrl, Client: hc, Exe: workerExe}
		},
	})

	select {
	case err := <-c.reported:
		if !errors.Is(err, update.ErrRestart) {
			t.Fatalf("the worker's reports ended with %v, want a restart", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the worker never installed the controller's build")
	}
	if got := read(t, workerExe); got != "hotbed build 7 for linux/arm64" {
		t.Errorf("the worker's binary is %q, want the controller's build for linux/arm64", got)
	}

	var ls pb.ListNodesResponse
	c.ok(&ls, "nodes")
	if ls.ControllerVersion != "build 7 (cloudflare-do)" || ls.Nodes[0].Resources.Version != version.String() {
		t.Errorf("nodes says the controller runs %q, the worker %q; want %q / %q",
			ls.ControllerVersion, ls.Nodes[0].Resources.Version, "build 7 (cloudflare-do)", version.String())
	}

	// A client on a dev build is left alone.
	cliExe := writeExe(t, "old client")
	var reexec [][]string
	c.cli.exe = func() (string, error) { return cliExe, nil }
	c.cli.reexec = func(args ...string) error { reexec = append(reexec, args); return nil }
	if code, _, stderr := c.hotbed("nodes"); code != 0 || read(t, cliExe) != "old client" {
		t.Fatalf("a dev client: exit %d, stderr %q, binary %q", code, stderr, read(t, cliExe))
	}

	// One on a CI build follows the controller, after the command it ran.
	c.cli.build = 3
	code, _, stderr := c.hotbed("nodes")
	want := "hotbed build 7 for " + runtime.GOOS + "/" + runtime.GOARCH
	if code != 0 || read(t, cliExe) != want || !strings.Contains(stderr, "hotbed: upgraded to build 7, the controller's") {
		t.Fatalf("a client on build 3: exit %d, stderr %q, binary %q, want %q", code, stderr, read(t, cliExe), want)
	}
	real, err := filepath.EvalSymlinks(cliExe)
	if err != nil {
		t.Fatal(err)
	}
	if len(reexec) != 1 || !slices.Equal(reexec[0], []string{real, "skills", "install"}) {
		t.Errorf("after the upgrade it ran %q, want the new build's skills install", reexec)
	}

	// On the controller's build, upgrade has nothing to do.
	c.cli.build = 7
	var res upgradeResult
	c.ok(&res, "upgrade")
	if res != (upgradeResult{Build: 7}) {
		t.Errorf("upgrade on the controller's build = %+v", res)
	}
}

// A controller on a build it doesn't keep (a dev build) has nothing to hand
// out: workers stay as they are, and upgrade says why.
func TestADevControllerHandsOutNoBuild(t *testing.T) {
	workerExe := writeExe(t, "old worker")
	c := newClusterWith(t, clusterOpts{
		follow: func(ctrl string, hc *http.Client) *worker.Follow {
			return &worker.Follow{URL: "http://" + ctrl, Client: hc, Exe: workerExe}
		},
	})
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "tidy-owl")
	c.workspace("tidy-owl", nil) // the worker reported, and was answered
	if got := read(t, workerExe); got != "old worker" {
		t.Errorf("the worker's binary is %q", got)
	}
	code, e := c.fails("upgrade")
	if code != exitError || e.Code != "NOT_FOUND" || e.Message != "the controller runs dev (cloudflare-do), a build it doesn't keep: nothing to upgrade to" {
		t.Errorf("upgrade from a dev controller: exit %d, %+v", code, e)
	}
	// Without updates configured on the controller, admin upgrade says why.
	code, e = c.fails("admin", "upgrade")
	if code != exitError || e.Code != "NOT_FOUND" || !strings.Contains(e.Message, "does not follow CI builds") {
		t.Errorf("admin upgrade without updates configured: exit %d, %+v", code, e)
	}
}

// Admin upgrade refreshes the controller's cached GitHub release metadata in
// place (no process restart) and immediately serves the new build's binaries.
func TestAdminUpgradeRefreshesControllerRelease(t *testing.T) {
	g := updatetest.NewGitHub(t)
	g.Publish(7)
	ghClient := g.Client()

	c := newClusterWith(t, clusterOpts{
		server: func(s *testServerConfig) {
			s.UpdatesRepo = ghClient.Repo
			s.UpdatesAPIBase = ghClient.API
			s.UpdatesToken = ghClient.Token
		},
	})

	// First admin upgrade fetches build 7 from GitHub.
	var res adminUpgradeResult
	c.ok(&res, "admin", "upgrade")
	if res != (adminUpgradeResult{Build: 7, Upgraded: true, From: 0, Restarting: false}) {
		t.Fatalf("initial admin upgrade = %+v", res)
	}

	// Already on build 7: admin upgrade reports no upgrade.
	c.ok(&res, "admin", "upgrade")
	if res != (adminUpgradeResult{Build: 7, Upgraded: false, From: 0, Restarting: false}) {
		t.Fatalf("admin upgrade on latest = %+v", res)
	}

	// Publish build 8: admin upgrade immediately advances to build 8 and serves
	// build 8's assets via /builds/8/<asset>.
	g.Publish(8)
	c.ok(&res, "admin", "upgrade")
	if res != (adminUpgradeResult{Build: 8, Upgraded: true, From: 7, Restarting: false}) {
		t.Fatalf("admin upgrade to 8 = %+v", res)
	}
	b, err := c.asWorker.GetBuild(t.Context(), &pb.GetBuildRequest{Os: runtime.GOOS, Arch: runtime.GOARCH})
	if err != nil || b.Number != 8 {
		t.Fatalf("the controller's build after admin upgrade = %v, %v; want 8", b, err)
	}
	resp, err := c.tn.Client(laptop).Get(c.ctrlSrv.URL + b.Path)
	if err != nil {
		t.Fatalf("GET %s: %v", b.Path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	want := "hotbed build 8 for " + runtime.GOOS + "/" + runtime.GOARCH
	if resp.StatusCode != http.StatusOK || string(body) != want {
		t.Errorf("GET %s = %d %q, want 200 %q", b.Path, resp.StatusCode, string(body), want)
	}
}

// A rollout restarts workers under attached clients: hotbed run and logs -f
// ride out their worker's restart, printing all of the command's output
// once; run exits with its status.
func TestRunRidesOutAWorkerRestart(t *testing.T) {
	for _, logs := range []bool{false, true} {
		t.Run(map[bool]string{false: "run", true: "logs -f"}[logs], func(t *testing.T) { ridesOut(t, logs) })
	}
}

func ridesOut(t *testing.T, logs bool) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "tidy-owl")
	c.workspace("tidy-owl", nil)
	g := newGate(t)
	type result struct {
		code     int
		out, err string
	}
	done := make(chan result, 1)
	nd := c.nodes[0]
	// add followed the base's freshen: the client's own stream comes after.
	opened := nd.follows.Load()
	script := []string{"sh", "-c", `echo before; while [ ! -e "$1" ]; do sleep 0.05; done; echo after`, "sh", filepath.Join(string(g), "go")}
	if logs {
		cmd := c.detach(nil, script...)
		go func() {
			code, out, stderr := c.hotbed("logs", "-s", "tidy-owl", cmd.Id, "-f")
			done <- result{code, out, stderr}
		}()
	} else {
		go func() {
			code, out, stderr := c.hotbed(append([]string{"run", "-s", "tidy-owl", "-r", "app", "--"}, script...)...)
			done <- result{code, out, stderr}
		}()
	}
	c.eventually("the client to follow the command's output", func() bool { return nd.follows.Load() > opened })
	nd.halt()
	c.eventually("the client to find its worker stopped", func() bool { return nd.refused.Load() > 0 })
	g.open(t, "go")
	nd.start()
	select {
	case r := <-done:
		if r.code != 0 || r.out != "before\nafter\n" {
			t.Errorf("across its worker's restart: exit %d, %q, stderr %q", r.code, r.out, r.err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("run never ended")
	}
}

// When the controller advances to a new build while a command is running on a
// worker, the worker keeps running the command without replacing its binary or
// restarting, then immediately installs the new build and returns
// update.ErrRestart once the command finishes (board 7989).
func TestWorkerDefersUpgradeUntilRunningCommandFinishes(t *testing.T) {
	gh := updatetest.NewGitHub(t)
	gh.Publish(8)
	cache := update.Cache{Dir: t.TempDir()}
	rel, err := gh.Client().Latest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Put(t.Context(), gh.Client(), rel); err != nil {
		t.Fatal(err)
	}

	workerExe := writeExe(t, "old worker")
	c := newClusterWith(t, clusterOpts{
		follow: func(ctrl string, hc *http.Client) *worker.Follow {
			return &worker.Follow{URL: "http://" + ctrl, Client: hc, Exe: workerExe}
		},
	})
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "drain-fox")
	c.workspace("drain-fox", nil)

	g := newGate(t)
	type result struct {
		code     int
		out, err string
	}
	runDone := make(chan result, 1)
	script := []string{"sh", "-c", `echo started; while [ ! -e "$1" ]; do sleep 0.05; done; echo finished`, "sh", filepath.Join(string(g), "go")}
	go func() {
		code, out, stderr := c.hotbed(append([]string{"run", "-s", "drain-fox", "-r", "app", "--"}, script...)...)
		runDone <- result{code, out, stderr}
	}()

	c.eventually("command to be running on the worker", func() bool {
		for _, cmd := range c.engine().Commands() {
			if cmd.SessionId == "drain-fox" && cmd.State == pb.CommandState_COMMAND_STATE_RUNNING {
				return true
			}
		}
		return false
	})

	// Advance the controller to build 8 while the command is still running.
	c.serverCfg.Builds, c.serverCfg.Build = &cache, 8
	c.restart()

	// Exercise the worker's real follow path on a heartbeat while busy: it must
	// defer instead of overwriting workerExe or restarting.
	if err := worker.HeartbeatOnce(t.Context(), c.engine(), c.asWorker, c.id, c.nodes[0].follow); err != nil {
		t.Fatalf("HeartbeatOnce while command running = %v, want nil (deferred)", err)
	}
	select {
	case err := <-c.reported:
		t.Fatalf("worker connection ended while command was running: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if got := read(t, workerExe); got != "old worker" {
		t.Fatalf("workerExe overwritten while command was running: %q", got)
	}

	// Let the running command finish cleanly.
	g.open(t, "go")
	select {
	case r := <-runDone:
		if r.code != 0 || r.out != "started\nfinished\n" {
			t.Fatalf("run across controller upgrade: exit %d, stdout %q, stderr %q", r.code, r.out, r.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("running command never finished")
	}

	// Once the command exits and its event pushes, Connect immediately triggers
	// a heartbeat that installs build 8 and exits with update.ErrRestart.
	select {
	case err := <-c.reported:
		if !errors.Is(err, update.ErrRestart) {
			t.Fatalf("worker connection ended with %v, want update.ErrRestart", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("worker never installed build 8 and restarted after command finished")
	}
	if got := read(t, workerExe); got != "hotbed build 8 for linux/arm64" {
		t.Errorf("worker binary after drain = %q, want build 8 for linux/arm64", got)
	}
}
