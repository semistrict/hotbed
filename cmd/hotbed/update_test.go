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

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/controller"
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
		server: func(s *controller.ServerConfig) { s.Builds, s.Build = &cache, 7 },
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
	if ls.ControllerVersion != version.String() || ls.Nodes[0].Resources.Version != version.String() {
		t.Errorf("nodes says the controller runs %q, the worker %q; want %q", ls.ControllerVersion, ls.Nodes[0].Resources.Version, version.String())
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
	c.ok(nil, "add", "-s", "tidy-owl", "-r", "app") // the worker reported, and was answered
	if got := read(t, workerExe); got != "old worker" {
		t.Errorf("the worker's binary is %q", got)
	}
	code, e := c.fails("upgrade")
	if code != exitError || e.Code != "NOT_FOUND" || e.Message != "the controller runs "+version.String()+", a build it doesn't keep: nothing to upgrade to" {
		t.Errorf("upgrade from a dev controller: exit %d, %+v", code, e)
	}
	// Without updates configured on the controller, admin upgrade says why.
	code, e = c.fails("admin", "upgrade")
	if code != exitError || e.Code != "NOT_FOUND" || !strings.Contains(e.Message, "does not follow CI builds") {
		t.Errorf("admin upgrade without updates configured: exit %d, %+v", code, e)
	}
}

func TestAdminUpgradeTriggersControllerRestart(t *testing.T) {
	g := updatetest.NewGitHub(t)
	g.Publish(7)
	cache := update.Cache{Dir: t.TempDir()}
	ctrlExe := writeExe(t, "controller build 7")
	f := &controller.Follower{GitHub: g.Client(), Cache: cache, Exe: ctrlExe, Build: 7}

	c := newClusterWith(t, clusterOpts{
		server: func(s *controller.ServerConfig) {
			s.Builds, s.Build, s.Follower = &cache, 7, f
		},
	})

	runDone := make(chan error, 1)
	go func() { runDone <- f.Run(t.Context()) }()

	// Already on build 7: admin upgrade reports no restart.
	var res adminUpgradeResult
	c.ok(&res, "admin", "upgrade")
	if res != (adminUpgradeResult{Build: 7, Upgraded: false, Restarting: false}) {
		t.Fatalf("admin upgrade on latest = %+v", res)
	}

	// Publish build 8: admin upgrade installs it immediately and wakes Run,
	// which ends for the controller to start the new build in its place, as
	// serve does; admin upgrade returns once the controller answers on it.
	restarted := make(chan error, 1)
	go func() {
		err := <-runDone
		if errors.Is(err, update.ErrRestart) {
			c.serverCfg.Build = 8
			c.serverCfg.Follower = &controller.Follower{GitHub: g.Client(), Cache: cache, Exe: ctrlExe, Build: 8}
			c.restart()
		}
		restarted <- err
	}()
	g.Publish(8)
	c.ok(&res, "admin", "upgrade")
	if res != (adminUpgradeResult{Build: 8, Upgraded: true, From: 7, Restarting: true}) {
		t.Fatalf("admin upgrade to 8 = %+v", res)
	}
	select {
	case err := <-restarted:
		if !errors.Is(err, update.ErrRestart) {
			t.Fatalf("Follower.Run ended with %v, want update.ErrRestart", err)
		}
	default:
		t.Fatal("admin upgrade returned before the controller restarted")
	}
	if b, err := c.asWorker.GetBuild(t.Context(), &pb.GetBuildRequest{Os: runtime.GOOS, Arch: runtime.GOARCH}); err != nil || b.Number != 8 {
		t.Fatalf("the controller's build after admin upgrade = %v, %v; want 8", b, err)
	}
	want := "hotbed build 8 for " + runtime.GOOS + "/" + runtime.GOARCH
	if got := read(t, ctrlExe); got != want {
		t.Errorf("controller binary is %q, want %q", got, want)
	}
	if !cache.Has(8) {
		t.Error("controller build cache does not have build 8")
	}

	// A controller that does not come back on the new build is said to.
	g.Publish(9)
	code, e := c.fails("admin", "upgrade", "--wait", "1s")
	if code != exitError || e.Code != api.CodeControllerUnreachable || e.Message != "the controller did not answer on build 9 within 1s (it still answers on build 8)" {
		t.Errorf("admin upgrade to a build the controller never restarts into: exit %d, %+v", code, e)
	}
	// --wait 0 returns as it restarts.
	c.ok(&res, "admin", "upgrade", "--wait", "0")
	if res != (adminUpgradeResult{Build: 9, Upgraded: true, From: 8, Restarting: true}) {
		t.Errorf("admin upgrade --wait 0 = %+v", res)
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
	c.ok(nil, "add", "-s", "tidy-owl", "-r", "app")
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
