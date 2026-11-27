package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/update"
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

// onHostArch has a worker report this machine's arch, as one that installs a
// real build over itself must: update.Install takes only a binary it can run.
var onHostArch = []func(*worker.EngineConfig){func(cfg *worker.EngineConfig) {
	cfg.System.(*fake.System).Arch = runtime.GOARCH
}}

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
	t.Parallel()
	cache, _ := publishBuild(t, 7)
	workerExe := writeExe(t, "old worker")
	c := newClusterWith(t, clusterOpts{
		engine: onHostArch,
		server: func(s *testServerConfig) { s.Builds, s.Build = cache, 7 },
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
	if got := read(t, workerExe); got != hostBuild(7) {
		t.Errorf("the worker's binary is %q, want the controller's build for this machine, %q", got, hostBuild(7))
	}

	var ls pb.ListNodesResponse
	c.ok(&ls, "nodes")
	if version.Of(ls.ControllerVersion) != 7 || ls.Nodes[0].Resources.Version != version.String() {
		t.Errorf("nodes says the controller runs %q, the worker %q; want build 7 / %q",
			ls.ControllerVersion, ls.Nodes[0].Resources.Version, version.String())
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
	want := hostBuild(7)
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
	t.Parallel()
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
	if code != exitError || e.Code != "NOT_FOUND" || !strings.HasPrefix(e.Message, "the controller runs dev (") || !strings.HasSuffix(e.Message, "), a build it doesn't keep: nothing to upgrade to") {
		t.Errorf("upgrade from a dev controller: exit %d, %+v", code, e)
	}
}

// A rollout restarts workers under attached clients: hotbed run and logs -f
// ride out their worker's restart, printing all of the command's output
// once; run exits with its status.
func TestRunRidesOutAWorkerRestart(t *testing.T) {
	t.Parallel()
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
	c.eventually("the controller to lose the worker's tunnel", func() bool {
		ids, err := c.store.ConnectedWorkers(t.Context())
		return err == nil && !slices.Contains(ids, nd.peer.NodeID)
	})
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
	t.Parallel()
	cache, _ := publishBuild(t, 8)

	workerExe := writeExe(t, "old worker")
	c := newClusterWith(t, clusterOpts{
		engine: onHostArch,
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
	c.serverCfg.Builds, c.serverCfg.Build = cache, 8
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
	if got := read(t, workerExe); got != hostBuild(8) {
		t.Errorf("worker binary after drain = %q, want build 8 for this machine, %q", got, hostBuild(8))
	}
}

// When cf-controller knows its own build number, git commit, and release
// SHA256SUMS (as stamped by CI at deploy time), it reports that exact version
// immediately, sends workers a direct GitHub release download URL, and workers
// fetch directly from GitHub without controller auth and restart into it.
func TestPinnedControllerVersionUpdatesWorkerDirectlyFromGitHub(t *testing.T) {
	t.Parallel()
	_, files := publishBuild(t, 12)
	g := serveReleaseDownloads(t, 12, files)
	const wantCommit = "0123456789abcdef0123456789abcdef01234567"

	workerExe := writeExe(t, "old worker")
	c := newClusterWith(t, clusterOpts{
		engine: onHostArch,
		server: func(s *testServerConfig) {
			s.UpdatesRepo = "semistrict/hotbed"
			s.UpdatesDownloadBase = g.URL
			s.ControllerBuild = 12
			s.ControllerCommit = wantCommit
			s.ControllerSha256Sums = string(files[update.SumsName])
		},
		follow: func(ctrl string, hc *http.Client) *worker.Follow {
			return &worker.Follow{
				URL:            "http://" + ctrl,
				Client:         hc,
				DownloadClient: g.Client(),
				Exe:            workerExe,
				Build:          11,
			}
		},
	})

	select {
	case err := <-c.reported:
		if !errors.Is(err, update.ErrRestart) {
			t.Fatalf("the worker's reports ended with %v, want update.ErrRestart", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the worker never installed the controller's pinned build from GitHub")
	}
	if got := read(t, workerExe); got != hostBuild(12) {
		t.Errorf("worker binary = %q, want build 12 for this machine, %q", got, hostBuild(12))
	}
	if got := g.Hits.Load(); got == 0 {
		t.Errorf("Hits = %d, want >= 1 (worker should download directly from GitHub)", got)
	}

	var ls pb.ListNodesResponse
	c.ok(&ls, "nodes")
	if ls.ControllerVersion != "build 12 (0123456789ab)" {
		t.Errorf("ControllerVersion = %q, want %q", ls.ControllerVersion, "build 12 (0123456789ab)")
	}

	b, err := c.asWorker.GetBuild(t.Context(), &pb.GetBuildRequest{Os: runtime.GOOS, Arch: runtime.GOARCH})
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	wantURL := g.URL + "/semistrict/hotbed/releases/download/build-12/hotbed-" + runtime.GOOS + "-" + runtime.GOARCH
	if b.Number != 12 || b.DownloadUrl != wantURL {
		t.Errorf("GetBuild = {Number:%d DownloadUrl:%q}, want {12 %q}", b.Number, b.DownloadUrl, wantURL)
	}

	// A CLI already on a newer build (13 > 12) is not downgraded by `hotbed upgrade`.
	cliExe := writeExe(t, "newer client")
	c.cli.build = 13
	c.cli.exe = func() (string, error) { return cliExe, nil }
	var upRes upgradeResult
	c.ok(&upRes, "upgrade")
	if upRes != (upgradeResult{Build: 13}) || read(t, cliExe) != "newer client" {
		t.Errorf("upgrade on newer CLI = %+v, binary=%q; want no downgrade", upRes, read(t, cliExe))
	}
}
