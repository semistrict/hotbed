package main

import (
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

// newMixedArchFleet starts a 2-node cluster where asahimbp is arm64 and mini is amd64.
func newMixedArchFleet(t *testing.T) *cluster {
	t.Helper()
	return newFleetWith(t, 2, clusterOpts{engine: []func(*worker.EngineConfig){func(cfg *worker.EngineConfig) {
		if cfg.Host == "mini" {
			cfg.System.(*fake.System).Arch = "amd64"
		}
	}}})
}

// Each run goes where the controller places it: the session keeps its
// workspace while its node is not much busier than another, and gets one
// on another node, with the checkout synced there, when it is.
func TestRunsArePlacedByLoad(t *testing.T) {
	t.Parallel()
	c := newMixedArchFleet(t)
	origin := upstream(t)
	c.addRepo(origin)
	const S = "keen-gnu"
	c.ok(nil, "session", "start", S)
	agent := filepath.Join(t.TempDir(), "agent")
	gitIn(t, filepath.Dir(agent), "clone", "--quiet", origin, agent)
	c.chdir(agent)
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
	if code, _, stderr := c.hotbed("run", "-s", S, "-r", "app", "--node", "mini", "--", "true"); code == 0 || !strings.Contains(stderr, "unknown flag --node") {
		t.Errorf("--node should be rejected on run: code=%d stderr=%q", code, stderr)
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
	t.Parallel()
	c := newMixedArchFleet(t)
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
	if node, _ := c.ranOn(S, "base\n", "--arch", "amd64", "--", "cat", "src/lib.txt"); node != "mini" {
		t.Errorf("--arch amd64 without a checkout ran on %s", node)
	}
}

// A node that cannot make the workspace now is passed over: the run goes to
// the next node placement picks.
func TestRunPassesOverANodeThatCannotTakeIt(t *testing.T) {
	t.Parallel()
	c := newMixedArchFleet(t)
	c.addRepo(upstream(t))
	const S = "wary-ibis"
	c.ok(nil, "session", "start", S)
	c.load(map[string]float64{"mini": 0.9})
	c.nodes[0].cut.Store(true) // asahimbp, the idle one, out of the controller's reach
	if node, _ := c.ranOn(S, "base\n", "--", "cat", "src/lib.txt"); node != "mini" {
		t.Fatalf("run went to %s, want mini, the one node the controller reaches", node)
	}
	// With nothing else left, the run fails as that node did.
	if _, e := c.fails("run", "-s", S, "-r", "app", "--arch", "arm64", "--no-sync", "--", "true"); e.Code != api.CodeWorkerUnreachable || !e.Retryable {
		t.Errorf("run constrained to the cut-off node's arch: %+v", e)
	}
}

// Each command carries the placement decision that put it on its node — both
// what the caller asked for (--node, --mem-low, --fork, --no-sync, or any node)
// and every candidate node weighed — through live state and ended history.
func TestCommandRecordsPlacementDecisionAndRequirements(t *testing.T) {
	t.Parallel()
	c := newMixedArchFleet(t)
	origin := upstream(t)
	c.addRepo(origin)
	const S = "clear-owl"
	c.ok(nil, "session", "start", S)
	agent := filepath.Join(t.TempDir(), "agent")
	gitIn(t, filepath.Dir(agent), "clone", "--quiet", origin, agent)
	c.chdir(agent)

	c.load(map[string]float64{"asahimbp": 0.9})
	// Unconstrained run: picks mini (idle), records empty Placement request.
	if node, _ := c.ranOn(S, "base\n", "--", "cat", "src/lib.txt"); node != "mini" {
		t.Fatalf("unconstrained run went to %s, want mini", node)
	}
	// Constrained + fork + mem-low run: goes to asahimbp despite load, records constraints.
	if node, _ := c.ranOn(S, "base\n", "--arch", "arm64", "--mem-low", "1G", "--fork", "--", "cat", "src/lib.txt"); node != "asahimbp" {
		t.Fatalf("constrained run went to %s, want asahimbp", node)
	}

	ps := &pb.ListCommandsResponse{}
	c.ok(ps, "ps", "-s", S, "-e")
	if len(ps.Commands) != 2 {
		t.Fatalf("ps -e returned %d commands, want 2", len(ps.Commands))
	}
	auto := ps.Commands[0].GetPlacement()
	if auto == nil || auto.GetHostname() != "mini" || auto.GetReason() != "the least costly node" {
		t.Fatalf("unconstrained command placement = %+v, want mini / least costly", auto)
	}
	if auto.GetRequest() == nil || auto.GetRequest().GetArch() != "" || auto.GetNoSync() || auto.GetThrowaway() {
		t.Errorf("unconstrained command request = %+v, noSync=%v throwaway=%v", auto.GetRequest(), auto.GetNoSync(), auto.GetThrowaway())
	}
	if len(auto.GetCandidates()) != 2 {
		t.Errorf("unconstrained candidates = %d, want 2", len(auto.GetCandidates()))
	}

	pinned := ps.Commands[1].GetPlacement()
	if pinned == nil || pinned.GetHostname() != "asahimbp" || pinned.GetReason() != "the least costly node" {
		t.Fatalf("constrained command placement = %+v, want asahimbp / least costly", pinned)
	}
	if pinned.GetRequest().GetArch() != "arm64" || pinned.GetRequest().GetMemoryLowBytes() != 1<<30 || !pinned.GetThrowaway() || pinned.GetNoSync() {
		t.Errorf("constrained command request = %+v, noSync=%v throwaway=%v", pinned.GetRequest(), pinned.GetNoSync(), pinned.GetThrowaway())
	}

	// Plain-text `hotbed placements` shows requested requirements on each line.
	code, out, errOut := c.hotbed("placements", "--session", S)
	if code != 0 {
		t.Fatalf("placements: exit %d, stderr %q", code, errOut)
	}
	if !strings.Contains(out, "[any node]") || !strings.Contains(out, "[--arch arm64 --mem-low 1.0 GiB --fork]") {
		t.Errorf("placements output missing request summary:\n%s", out)
	}
}

// When all eligible hosts are holding work, an unconstrained run queues at the
// controller instead of committing to one full host's local queue, and runs on
// whichever eligible host frees up first.
func TestRunWaitsAtControllerAndTakesFirstHostWithCapacity(t *testing.T) {
	t.Parallel()
	workerClock := &clock{}
	c := newFleetWith(t, 2, clusterOpts{engine: []func(*worker.EngineConfig){func(cfg *worker.EngineConfig) {
		cfg.Now = workerClock.Now
	}}})
	origin := upstream(t)
	c.addRepo(origin)
	const S = "late-fox"
	c.ok(nil, "session", "start", S)
	agent := filepath.Join(t.TempDir(), "agent")
	gitIn(t, filepath.Dir(agent), "clone", "--quiet", origin, agent)
	c.chdir(agent)

	// Both nodes hold new work; mini looks slightly busier than asahimbp, so
	// eager placement would have locked the run onto asahimbp.
	c.nodes[0].system.SetCPUPressure(75) // asahimbp
	c.nodes[1].system.SetCPUPressure(75) // mini
	c.load(map[string]float64{"asahimbp": 0.2, "mini": 0.8})

	type runRes struct {
		code   int
		out    string
		stderr string
	}
	done := make(chan runRes, 1)
	go func() {
		code, out, stderr := c.hotbed("run", "-s", S, "-r", "app", "--", "cat", "src/lib.txt")
		done <- runRes{code, out, stderr}
	}()

	// While both nodes hold work, the run waits at the controller: neither
	// worker gets a queued command.
	time.Sleep(250 * time.Millisecond)
	select {
	case r := <-done:
		t.Fatalf("run returned while both nodes were holding: exit %d out=%q stderr=%q", r.code, r.out, r.stderr)
	default:
	}
	ps := &pb.ListCommandsResponse{}
	c.ok(ps, "ps", "-s", S)
	if len(ps.Commands) != 0 {
		t.Fatalf("commands queued on workers while all nodes full = %+v, want none (late binding)", ps.Commands)
	}

	// Release ONLY mini's hold while keeping asahimbp holding: mini must win.
	c.nodes[1].system.SetCPUPressure(0)
	workerClock.advance(31 * time.Second)
	c.load(map[string]float64{"asahimbp": 0.2, "mini": 0.1})

	res := <-done
	if res.code != 0 || res.out != "base\n" {
		t.Fatalf("run after mini freed up: exit %d out=%q stderr=%q", res.code, res.out, res.stderr)
	}
	if !strings.Contains(res.stderr, " on mini ") {
		t.Fatalf("run went elsewhere (stderr=%q), want mini (the first host to free up)", res.stderr)
	}
	var hist pb.ListCommandsResponse
	c.ok(&hist, "ps", "-s", S, "-e")
	if len(hist.Commands) != 1 || hist.Commands[0].GetPlacement().GetHostname() != "mini" {
		t.Fatalf("history placement = %+v, want mini", hist.Commands)
	}
	if got := hist.Commands[0].GetPlacement().GetReason(); !strings.Contains(got, "for capacity") {
		t.Errorf("placement reason = %q, want wait-for-capacity note", got)
	}
}

// When two similarly-loaded eligible hosts need a new workspace, the controller
// races workspace creation across both, uses the first ready winner, and
// releases the losing workspace.
func TestRunRacesWorkspaceCreationAcrossTwoHosts(t *testing.T) {
	t.Parallel()
	c := newFleet(t, 2) // asahimbp and mini, both idle (equal cost)
	origin := upstream(t)
	c.addRepo(origin)
	const S = "race-jay"
	c.ok(nil, "session", "start", S)
	agent := filepath.Join(t.TempDir(), "agent")
	gitIn(t, filepath.Dir(agent), "clone", "--quiet", origin, agent)
	c.chdir(agent)

	// Stall asahimbp's CreateWorkspace so mini wins the 2-host race without
	// waiting on asahimbp.
	gate := make(chan struct{})
	c.nodes[0].createGate.Store(&gate)
	openGate := sync.OnceFunc(func() {
		close(gate)
		c.nodes[0].createGate.Store(nil)
	})
	defer openGate()

	node, stderr := c.ranOn(S, "base\n", "--", "cat", "src/lib.txt")
	if node != "mini" {
		t.Fatalf("raced run went to %s (stderr=%q), want mini to beat stalled asahimbp", node, stderr)
	}

	var hist pb.ListCommandsResponse
	c.ok(&hist, "ps", "-s", S, "-e")
	if len(hist.Commands) != 1 {
		t.Fatalf("ps -e = %d commands, want 1", len(hist.Commands))
	}
	pl := hist.Commands[0].GetPlacement()
	if pl.GetHostname() != "mini" || !strings.Contains(pl.GetReason(), "won workspace race against asahimbp") {
		t.Errorf("raced command placement = %+v, want mini winning race against asahimbp", pl)
	}

	// Unblock asahimbp and confirm the losing workspace is cleanly released.
	openGate()
	c.eventually("losing raced workspace on asahimbp to be released", func() bool {
		ls := &pb.ListWorkspacesResponse{}
		c.ok(ls, "workspace", "ls", "-s", S)
		return len(ls.Workspaces) == 1 && ls.Workspaces[0].Node == "mini"
	})
}
