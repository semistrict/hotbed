package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/update"
	"github.com/semistrict/hotbed/internal/version"
)

// Placement weighs each node a run may go to by its load, in units of a
// whole node's CPUs kept busy, plus what going there would cost on top.
const (
	// moveCost is what leaving the session's workspace for a node it has
	// none on weighs: the run there forks afresh and rebuilds what the
	// session's builds had compiled. A node with a workspace is kept unless
	// another is this much less loaded.
	moveCost = 0.3
	// coldCost is what a node with no warm base of the project weighs on
	// top: its first workspace builds the project from nothing.
	coldCost = 0.5
	// fullCost is what a node the run's guaranteed memory does not fit on
	// now weighs: the run would queue there until it does.
	fullCost = 1
	// brokenCost keeps off a node whose newest base of the project failed with
	// no warm one to fall back to, unless no other node fits, when its
	// worker says why.
	brokenCost = 10
	// pressureCost is what a node's CPU pressure (PSI some, 0..1) weighs
	// per unit: CPU busy tops out at a whole node, but pressure goes on
	// rising as more work queues for the CPUs, so it tells a saturated node
	// from one merely busy.
	pressureCost = 1
	// holdingCost is what a node that holds new work weighs: the run would
	// queue there until it starts work again.
	holdingCost = 1
	// wakeCost is what a node asleep weighs on top: the run waits for it to
	// boot, about a minute.
	wakeCost = 1
)

// candidate is a node as placement weighs it for one run.
type candidate struct {
	nodeView
	host string
	// mine is the session's workspace on the node, or "".
	mine string
	// warm says the node has a warm base of the project; broken that its
	// newest base of it failed and it has no warm one.
	warm, broken bool
	// asleep says the node is powered off while idle (Standby), or booting:
	// it takes runs, once woken.
	asleep bool
	// upgrading names the build the node is on, when it is not yet on the
	// controller's: a run waits for it rather than mix builds.
	upgrading int64
	cpus      int32
	cpuBusy   float64
	queued    int   // commands waiting to be admitted
	ram, free int64 // free: not used or guaranteed to a command
	// pressure is CPU pressure (PSI some avg10, %); admission is how the
	// node decides whether to start new work.
	pressure  float64
	admission *pb.Admission
	hostUsage *pb.HostUsage
}

// cost is how much running there weighs; the least wins.
func (c candidate) cost(p *pb.Placement) float64 {
	var sum float64
	for _, part := range c.parts(p) {
		sum += part.Value
	}
	return sum
}

// parts is what makes up the cost of running there, each named and with
// how it was worked out.
func (c candidate) parts(p *pb.Placement) []*pb.CostPart {
	out := []*pb.CostPart{
		{Name: "cpu-busy", Value: c.cpuBusy, Detail: fmt.Sprintf("%.0f%% of %d CPUs busy", c.cpuBusy*100, c.cpus)},
		{Name: "queued", Value: float64(c.queued) / float64(max(c.cpus, 1)), Detail: fmt.Sprintf("%d queued on %d CPUs", c.queued, c.cpus)},
		{Name: "cpu-pressure", Value: c.pressure / 100 * pressureCost, Detail: fmt.Sprintf("CPU pressure %.0f%%", c.pressure)},
	}
	add := func(name string, v float64, detail string) {
		out = append(out, &pb.CostPart{Name: name, Value: v, Detail: detail})
	}
	if c.holds(p) {
		add("holding", holdingCost, "it holds new work: "+c.admission.Reason)
	}
	if v, detail := hostStarvationCost(c.cpuBusy, c.hostUsage); v > 0 {
		add("host-cpu-starvation", v, detail)
	}
	if p.GetMemoryLowBytes() > c.free {
		add("memory", fullCost, fmt.Sprintf("the %d bytes guaranteed do not fit in the %d free now", p.GetMemoryLowBytes(), max(0, c.free)))
	}
	if c.asleep {
		add("asleep", wakeCost, "it is asleep: the run waits for it to boot")
	}
	if c.mine == "" {
		add("move", moveCost, "the session has no workspace here")
		if !c.warm {
			add("cold", coldCost, "no warm base of the project here")
		}
		if c.broken {
			add("broken", brokenCost, "its newest base of the project failed")
		}
	}
	return out
}

// holds says the node holds the new work a run would start: a measurement,
// on CPUs of its own, is held only by what holds measurements too.
func (c candidate) holds(p *pb.Placement) bool {
	a := c.admission
	if !a.GetHolding() {
		return false
	}
	for _, s := range a.Signals {
		if s.Holding && (s.Name != "cpu-pressure" || !p.GetBench()) {
			return true
		}
	}
	return false
}

// unfit says why a run cannot go to the node at all, or "". Placement.node
// narrows the nodes weighed before.
func (c candidate) unfit(p *pb.Placement, hosts []string) string {
	switch {
	case !c.online && !c.asleep || c.hb == nil:
		if why := c.node.GetStorageError(); why != "" {
			// It heartbeats, but serves nothing until its storage opens.
			return c.host + " cannot open its storage: " + why
		}
		return c.host + " is offline"
	case c.asleep && c.wakeFailed:
		return c.host + " did not wake when last woken"
	case c.upgrading != 0:
		return fmt.Sprintf("%s is on build %d, not yet the controller's", c.host, c.upgrading)
	case len(hosts) > 0 && !slices.Contains(hosts, c.host):
		return c.host + " keeps no bases of the project"
	case p.GetArch() != "" && c.hb.GetResources().GetArch() != p.GetArch():
		return fmt.Sprintf("%s is %s, not %s", c.host, c.hb.GetResources().GetArch(), p.GetArch())
	case p.GetBench() && c.hb.GetResources().GetBenchCpus() == "":
		return c.host + " has no measurement CPUs"
	case p.GetMemoryLowBytes() > c.ram:
		return fmt.Sprintf("%s has %d bytes of RAM, less than the %d guaranteed", c.host, c.ram, p.GetMemoryLowBytes())
	}
	return ""
}

// candidates weighs every node for a run in project by the session, whose
// live workspaces are mine (by node ID). follows is the build nodes of an
// architecture are to run, the controller's, or 0 when they are not to
// follow it.
func candidates(nvs []nodeView, project string, mine map[string]string, follows func(arch string) int64) []candidate {
	out := make([]candidate, 0, len(nvs))
	for _, nv := range nvs {
		c := candidate{nodeView: nv, host: nv.node.Hostname, mine: mine[nv.node.Id], asleep: nv.node.Asleep}
		res := nv.hb.GetResources()
		// A node asleep moves to the controller's build as it wakes.
		if want, on := follows(res.GetArch()), version.Of(res.GetVersion()); want != 0 && on != want && !c.asleep {
			c.upgrading = cmp.Or(on, -1) // a dev build is 0; say -1 rather than nothing
		}
		var newest *pb.Base
		for _, b := range nv.bases {
			if b.Project != project {
				continue
			}
			c.warm = c.warm || b.State == pb.BaseState_BASE_STATE_WARM
			if newest == nil || b.Generation > newest.Generation {
				newest = b
			}
		}
		c.broken = !c.warm && newest != nil && newest.State == pb.BaseState_BASE_STATE_FAILED
		if r := nv.hb; r != nil {
			c.cpus, c.cpuBusy = r.GetResources().GetCpus(), r.GetUsage().GetCpuBusy()
			c.pressure, c.admission = r.GetUsage().GetCpuPressure(), r.Admission
			c.ram = r.GetResources().GetMemoryBytes()
			c.free = c.ram - max(r.GetUsage().GetMemoryUsedBytes(), r.ReservedMemoryBytes)
			if c.asleep {
				// Its last heartbeat is from before it slept: it wakes idle.
				c.cpuBusy, c.pressure, c.admission, c.free = 0, 0, nil, c.ram
			} else if hu := nv.node.GetHostUsage(); hu != nil {
				c.hostUsage = hu
				c.admission = mergeHostAdmission(c.admission, r.GetUsage(), hu, time.Time{})
			}
		}
		for _, cmd := range nv.commands {
			if cmd.State == pb.CommandState_COMMAND_STATE_QUEUED {
				c.queued++
			}
		}
		out = append(out, c)
	}
	return out
}

// choose picks the node for a run among cs. A run that syncs no checkout
// changes only what the session's workspace already holds, which lives on
// its node alone, so it stays in a workspace the session has where the run
// may go, and waits for one whose node is offline rather than leave it;
// only restrictions that rule out every node with one send it elsewhere,
// to start afresh. Otherwise the least costly node wins; ties go to the
// most free memory, then the host name.
func choose(cs []candidate, p *pb.Placement, hosts []string, syncs bool) (candidate, string, error) {
	scope := cs
	if n := p.GetNode(); n != "" {
		if scope = only(cs, func(c candidate) bool { return c.host == n }); len(scope) == 0 {
			return candidate{}, "", api.Errorf(api.CodeInvalidArgs, "no node is called %s", n).WithSuggestion("hotbed nodes lists the nodes")
		}
	}
	fits := func(c candidate) bool { return c.unfit(p, hosts) == "" }
	if held := only(scope, func(c candidate) bool { return c.mine != "" }); !syncs && len(held) > 0 {
		if fit := only(held, fits); len(fit) > 0 {
			return cheapest(fit, p), "the run syncs no checkout, so it stays in the session's workspace", nil
		}
		if away := only(held, func(c candidate) bool { return (!c.online || c.upgrading != 0) && fitsOnline(c, p, hosts) }); len(away) > 0 {
			return candidate{}, "", refusal(away, p, hosts, true)
		}
	}
	fit := only(scope, fits)
	if len(fit) == 0 {
		return candidate{}, "", refusal(scope, p, hosts, false)
	}
	why := "the least costly node"
	if p.GetNode() != "" {
		why = "the node asked for"
	}
	return cheapest(fit, p), why, nil
}

// placementDecision is the record of choosing among cs for a session: every
// candidate with its cost in parts or why it could not take the run.
func placementDecision(cs []candidate, p *pb.Placement, hosts []string, chosen candidate, why string, err error) *pb.PlacementDecision {
	d := &pb.PlacementDecision{NodeId: chosen.node.GetId(), Hostname: chosen.host, Reason: why}
	if err != nil {
		d.NodeId, d.Hostname, d.Reason = "", "", err.Error()
		if ae := (*api.Error)(nil); errors.As(err, &ae) {
			d.Reason = ae.Message
		}
	}
	for _, c := range cs {
		pc := &pb.PlacementCandidate{NodeId: c.node.Id, Hostname: c.host, Mine: c.mine != "", Chosen: err == nil && c.node.Id == d.NodeId}
		if n := p.GetNode(); n != "" && c.host != n {
			pc.Unfit = "the run asked for " + n
		} else {
			pc.Unfit = c.unfit(p, hosts)
		}
		if pc.Unfit == "" {
			pc.Parts = c.parts(p)
			pc.Cost = c.cost(p)
		}
		d.Candidates = append(d.Candidates, pc)
	}
	return d
}

// cheapest is the least costly of cs, which are not empty.
func cheapest(cs []candidate, p *pb.Placement) candidate {
	return slices.MinFunc(cs, func(a, b candidate) int {
		return cmp.Or(
			cmp.Compare(a.cost(p), b.cost(p)),
			cmp.Compare(b.free, a.free),
			cmp.Compare(a.host, b.host),
		)
	})
}

// only is the candidates keep says to.
func only(cs []candidate, keep func(candidate) bool) []candidate {
	var out []candidate
	for _, c := range cs {
		if keep(c) {
			out = append(out, c)
		}
	}
	return out
}

// refusal says why none of the nodes in scope can take the run. stuck says
// scope is the offline nodes with the session's workspaces, which a run that
// syncs nothing waits for. Only waiting helps when a node that would fit is
// offline, or there are no nodes yet.
func refusal(scope []candidate, p *pb.Placement, hosts []string, stuck bool) error {
	var why []string
	for _, c := range scope {
		why = append(why, c.unfit(p, hosts))
	}
	e := (&api.Error{Code: api.CodeInvalidArgs, Message: "no node can take the run: " + strings.Join(why, "; ")}).
		WithSuggestion("hotbed nodes lists the nodes, their architectures and whether they are online; loosen --node or --arch")
	switch {
	case len(scope) == 0:
		e.Message = "the cluster has no nodes"
	case stuck:
		e.Message = "the run syncs no checkout, so it runs only in the session's workspace of the repo, and no node with one can take it: " + strings.Join(why, "; ")
		e.Suggestion = "run it from a checkout of the repo, which it then syncs wherever it goes, or hotbed workspace release the workspace to start over elsewhere"
	}
	if len(scope) == 0 || slices.ContainsFunc(scope, func(c candidate) bool { return (!c.online || c.upgrading != 0) && fitsOnline(c, p, hosts) }) {
		e.Code, e.Retryable = api.CodeWorkerUnreachable, true
	}
	if !stuck && len(upgradingHosts(scope, p, hosts)) > 0 {
		e.Suggestion = "retry once the node finishes upgrading"
	}
	return e
}

// upgradingHosts returns the host names of online nodes in scope that would
// take the run once they finish upgrading to the controller's build.
func upgradingHosts(scope []candidate, p *pb.Placement, hosts []string) []string {
	var out []string
	for _, c := range scope {
		if c.online && c.upgrading != 0 && fitsOnline(c, p, hosts) {
			out = append(out, c.host)
		}
	}
	return out
}

// upgradingFor returns the host names of online nodes in cs that would take
// the run once they finish upgrading to the controller's build, matching the
// scope choose refused.
func upgradingFor(cs []candidate, p *pb.Placement, hosts []string, syncs bool) []string {
	scope := cs
	if n := p.GetNode(); n != "" {
		scope = only(cs, func(c candidate) bool { return c.host == n })
	}
	if held := only(scope, func(c candidate) bool { return c.mine != "" }); !syncs && len(held) > 0 {
		if away := only(held, func(c candidate) bool { return (!c.online || c.upgrading != 0) && fitsOnline(c, p, hosts) }); len(away) > 0 {
			scope = away
		}
	}
	return upgradingHosts(scope, p, hosts)
}

// fitsOnline says the node would take the run were it online, and on the
// controller's build.
func fitsOnline(c candidate, p *pb.Placement, hosts []string) bool {
	c.online, c.upgrading, c.wakeFailed = true, 0, false
	if c.hb == nil {
		c.hb = &pb.HeartbeatRequest{Resources: c.node.Resources}
	}
	return c.unfit(p, hosts) == ""
}

type placeResult struct {
	node        candidate
	project     *pb.HostedProject
	needRelease []workspaceRow
	needProbe   []workspaceRow
	upgrading   []string
}

// place picks the node for a session's run among the nodes not in skip,
// and says which of the session's workspaces is there, if one is, and the
// session's project as the node needs it. It does no worker RPCs while
// s.placing is held: any failed workspaces to release or missing workspaces to
// probe are returned so the caller can act on them after unlocking s.placing.
func (s *workspaces) place(ctx context.Context, sess *pb.Session, req *pb.CreateWorkspaceRequest, skip, probed map[string]bool) (placeResult, error) {
	p, err := s.cfg.Store.Project(ctx, sess.Project)
	if err != nil {
		return placeResult{}, err
	}
	hosted, err := s.hosted(ctx, p)
	if err != nil {
		return placeResult{}, err
	}
	if req.Repo != "" && !slices.ContainsFunc(hosted.Repos, func(r *pb.Repo) bool { return r.Id == req.Repo }) {
		return placeResult{}, api.Errorf(api.CodeInvalidArgs, "session %s works on project %s, which holds no repo %s", sess.Id, p.Name, req.Repo).
			WithSuggestion(fmt.Sprintf("hotbed project add %s --repo REMOTE adds the repo's remote to it; or start a session in a project that holds it (hotbed session start -p PROJECT)", p.Name))
	}
	held, err := s.cfg.Store.SessionWorkspaces(ctx, sess.Id)
	if err != nil {
		return placeResult{}, err
	}
	nvs, err := s.views(ctx)
	if err != nil {
		return placeResult{}, err
	}
	var needRelease, needProbe []workspaceRow
	mine := map[string]string{}
	for _, w := range held {
		if failed(nvs, w.id) {
			needRelease = append(needRelease, w)
			continue
		}
		if !inView(nvs, w.id) && !s.isCreatingWS(w.id) && !probed[w.id] {
			needProbe = append(needProbe, w)
			continue
		}
		mine[w.node] = w.id
	}
	if len(needRelease) > 0 || len(needProbe) > 0 {
		return placeResult{needRelease: needRelease, needProbe: needProbe}, nil
	}
	nvs = slices.DeleteFunc(nvs, func(nv nodeView) bool { return skip[nv.node.Id] })
	cs := candidates(nvs, p.Name, mine, s.follows)
	c, why, err := choose(cs, req.Placement, p.Hosts, req.SyncDir != "")
	d := placementDecision(cs, req.Placement, p.Hosts, c, why, err)
	d.SessionId, d.Project, d.Repo = sess.Id, p.Name, req.Repo
	if rerr := s.cfg.Store.AddPlacement(ctx, d, s.now()); rerr != nil {
		slog.Error("record placement", "session", sess.Id, "err", rerr)
	}
	var upgrading []string
	if err != nil {
		upgrading = upgradingFor(cs, req.Placement, p.Hosts, req.SyncDir != "")
	}
	return placeResult{node: c, project: hosted, upgrading: upgrading}, err
}

// follows is the build workers of arch are to run: the controller's, when
// it hands it out; 0 otherwise.
func (s *Server) follows(arch string) int64 {
	if s.build(update.Platform{OS: "linux", Arch: arch}) == nil {
		return 0
	}
	return s.cfg.Build
}

// failed says the workspace failed, as its node's view has it.
func failed(nvs []nodeView, id string) bool {
	for _, nv := range nvs {
		for _, w := range nv.workspaces {
			if w.Id == id {
				return w.State == pb.WorkspaceState_WORKSPACE_STATE_FAILED
			}
		}
	}
	return false
}

// inView says whether any node's view currently contains workspace id.
func inView(nvs []nodeView, id string) bool {
	for _, nv := range nvs {
		for _, w := range nv.workspaces {
			if w.Id == id {
				return true
			}
		}
	}
	return false
}
