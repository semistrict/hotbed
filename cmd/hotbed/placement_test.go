package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/sysinfo"
	"github.com/semistrict/hotbed/internal/worker"
)

// load sets how busy each node's CPUs are, by host name, and has every
// running node report it.
func (c *cluster) load(busy map[string]float64) {
	c.t.Helper()
	for _, nd := range c.nodes {
		if nd.halted.Load() {
			continue
		}
		nd.system.SetCPU(sysinfo.CPUShare{Busy: busy[nd.peer.Hostname]})
		nd.eng().Sample()
		if err := worker.HeartbeatOnce(c.t.Context(), nd.eng(), nd.cluster, c.id, nil); err != nil {
			c.t.Fatal(err)
		}
	}
}

// ranOn runs args in session s from the current directory and says which
// node it ran on, failing unless it printed want.
func (c *cluster) ranOn(s, want string, args ...string) (node, stderr string) {
	c.t.Helper()
	code, out, errOut := c.hotbed(append([]string{"run", "-s", s, "-r", "app"}, args...)...)
	if code != 0 || out != want {
		c.t.Fatalf("run %v: exit %d, stdout %q (want %q), stderr %q", args, code, out, want, errOut)
	}
	_, rest, ok := strings.Cut(errOut, " on ")
	if !ok {
		c.t.Fatalf("run %v said nowhere where it ran: %q", args, errOut)
	}
	node, _, _ = strings.Cut(rest, " ")
	return node, errOut
}

// Each run goes where the controller places it: the session keeps its
// workspace while its node is not much busier than another, and gets one
// on another node, with the checkout synced there, when it is.
func TestRunsArePlacedByLoad(t *testing.T) {
	c := newFleetWith(t, 2, clusterOpts{engine: []func(*worker.EngineConfig){func(cfg *worker.EngineConfig) {
		if cfg.Host == "mini" {
			cfg.System.(*fake.System).Arch = "amd64"
		}
	}}})
	origin := upstream(t)
	c.addRepo(origin)
	const S = "keen-gnu"
	c.ok(nil, "session", "start", S)
	agent := filepath.Join(t.TempDir(), "agent")
	gitIn(t, filepath.Dir(agent), "clone", "--quiet", origin, agent)
	t.Chdir(agent)
	write(t, filepath.Join(agent, "src/lib.txt"), "edited\n")

	// Neither node has a base yet: the idle one takes the run.
	c.load(map[string]float64{"asahimbp": 0.95})
	if node, _ := c.ranOn(S, "edited\n", "--", "cat", "src/lib.txt"); node != "mini" {
		t.Fatalf("first run went to %s, want mini, the idle node", node)
	}

	// mini somewhat busier than the idle asahimbp, which has no base: the
	// session stays.
	c.load(map[string]float64{"mini": 0.4})
	if node, _ := c.ranOn(S, "edited\n", "--", "cat", "src/lib.txt"); node != "mini" {
		t.Fatalf("run went to %s, want mini, where the session's workspace is", node)
	}

	// mini saturated: the run moves, and the checkout's edits go with it.
	c.load(map[string]float64{"mini": 1})
	write(t, filepath.Join(agent, "src/lib.txt"), "moved\n")
	node, stderr := c.ranOn(S, "moved\n", "--", "cat", "src/lib.txt")
	if node != "asahimbp" || !strings.Contains(stderr, "synced 1 changed files") {
		t.Fatalf("run went to %s (stderr %q), want asahimbp with the checkout synced", node, stderr)
	}

	// The session now has a workspace on each, which workspace commands
	// tell apart by node.
	ls := &pb.ListWorkspacesResponse{}
	c.ok(ls, "workspace", "ls", "-s", S)
	var nodes []string
	for _, w := range ls.Workspaces {
		nodes = append(nodes, w.Node)
	}
	slices.Sort(nodes)
	if !slices.Equal(nodes, []string{"asahimbp", "mini"}) {
		t.Fatalf("workspaces on %v, want asahimbp and mini", nodes)
	}
	if _, e := c.fails("workspace", "show", "-s", S); e.Code != api.CodeConflict || e.Message != "session keen-gnu has workspaces on mini, asahimbp" {
		t.Errorf("workspace show with two: %+v", e)
	}
	shown := &pb.Workspace{}
	c.ok(shown, "workspace", "show", "-s", S, "--node", "mini")
	if shown.Node != "mini" {
		t.Errorf("workspace show --node mini = %v", shown)
	}

	// Restrictions narrow where the run may go, whatever the load.
	if node, _ := c.ranOn(S, "moved\n", "--arch", "amd64", "--", "cat", "src/lib.txt"); node != "mini" {
		t.Errorf("--arch amd64 ran on %s, want mini", node)
	}
	if node, _ := c.ranOn(S, "moved\n", "--node", "mini", "--", "cat", "src/lib.txt"); node != "mini" {
		t.Errorf("--node mini ran on %s", node)
	}
	if _, e := c.fails("run", "-s", S, "-r", "app", "--node", "nowhere", "--", "true"); e.Code != api.CodeInvalidArgs || e.Message != "no node is called nowhere" {
		t.Errorf("--node nowhere: %+v", e)
	}

	// The checkout goes to every workspace the session has.
	write(t, filepath.Join(agent, "src/lib.txt"), "everywhere\n")
	synced := syncResult{}
	c.ok(&synced, "sync", "-s", S)
	if len(synced.Synced) != 2 {
		t.Errorf("sync = %+v, want both workspaces", synced)
	}
}

// A run that syncs no checkout stays in the session's workspace however
// busy its node: what earlier runs changed there lives only there.
func TestRunWithoutACheckoutStaysInItsWorkspace(t *testing.T) {
	c := newFleet(t, 2)
	c.addRepo(upstream(t))
	const S = "calm-elk"
	c.ok(nil, "session", "start", S)
	c.load(map[string]float64{"mini": 0.9})
	if node, _ := c.ranOn(S, "", "--", "sh", "-c", "echo remote > src/lib.txt"); node != "asahimbp" {
		t.Fatalf("first run went to %s, want asahimbp, the idle node", node)
	}
	c.load(map[string]float64{"asahimbp": 1})
	if node, _ := c.ranOn(S, "remote\n", "--", "cat", "src/lib.txt"); node != "asahimbp" {
		t.Fatalf("run without a checkout went to %s, want asahimbp, where its edit is", node)
	}
	// With its workspace's node offline, it waits rather than lose the edit.
	c.nodes[0].halt()
	c.clock.advance(time.Minute)
	c.load(map[string]float64{})
	if _, e := c.fails("run", "-s", S, "-r", "app", "--", "true"); e.Code != api.CodeWorkerUnreachable || !e.Retryable || e.Message != "the run syncs no checkout, so it runs only in the session's workspace of the repo, and no node with one can take it: asahimbp is offline" {
		t.Errorf("run with its workspace's node offline: %+v", e)
	}
	// Narrowed to where it has no workspace, it gets a fresh one there.
	if node, _ := c.ranOn(S, "base\n", "--node", "mini", "--", "cat", "src/lib.txt"); node != "mini" {
		t.Errorf("--node mini without a checkout ran on %s", node)
	}
}

// A node that cannot make the workspace now is passed over: the run goes to
// the next node placement picks.
func TestRunPassesOverANodeThatCannotTakeIt(t *testing.T) {
	c := newFleet(t, 2)
	c.addRepo(upstream(t))
	const S = "wary-ibis"
	c.ok(nil, "session", "start", S)
	c.load(map[string]float64{"mini": 0.9})
	c.nodes[0].cut.Store(true) // asahimbp, the idle one, out of the controller's reach
	if node, _ := c.ranOn(S, "base\n", "--", "cat", "src/lib.txt"); node != "mini" {
		t.Fatalf("run went to %s, want mini, the one node the controller reaches", node)
	}
	// With nothing else left, the run fails as that node did.
	if _, e := c.fails("run", "-s", S, "-r", "app", "--node", "asahimbp", "--no-sync", "--", "true"); e.Code != api.CodeWorkerUnreachable || !e.Retryable {
		t.Errorf("run pinned to the cut-off node: %+v", e)
	}
}
