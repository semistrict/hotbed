package controller

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/httpx"
	"github.com/semistrict/hotbed/internal/tailnet"
	"github.com/semistrict/hotbed/internal/version"
)

const (
	// standbyGrace is how long a node stays supervised after its host's
	// Standby call ends, for the host to call again.
	standbyGrace = 30 * time.Second
	// defaultWakeWait is how long CreateWorkspace waits for a node it woke
	// before it tells the client to call again (NODE_WAKING): well under
	// the minute clients give a call.
	defaultWakeWait = 40 * time.Second
	// bootTimeout is how long a woken node has to come up. One that does
	// not takes no runs for wakeFailedHold, and goes back to sleep.
	bootTimeout    = 3 * time.Minute
	wakeFailedHold = 10 * time.Minute
)

// standby is what the controller knows of the power of nodes whose hosts
// run them in VMs (hotbed worker supervise), by host name, and what it
// wants of it: a node sleeps once it has had no work for its sleep_after,
// and wakes when a run is placed on it.
type standby struct {
	now func() time.Time
	// wakeWait is defaultWakeWait unless the config says otherwise.
	wakeWait time.Duration
	// views, when set by tests, wraps Server.views inside settlePower.
	views func(context.Context) ([]nodeView, error)
	mu    sync.Mutex
	nodes map[string]*power
	// changes wakes those waiting on a node's power: its host's Standby
	// call, to say what the controller now wants, and runs waiting for it
	// to wake.
	changes wakeups
}

// power is a node's, as its host last said and as the controller wants.
type power struct {
	// calls is how many of its host's Standby calls are open, and ended
	// when the last one ended.
	calls int
	ended time.Time
	// running is whether its VM ran, as its host last said, and since when
	// it has been so.
	running bool
	since   time.Time
	// stopping says the controller had it stop while it ran, and its host
	// has not yet said it stopped: it is not awake, wanted or not, until
	// it has started again.
	stopping bool
	// sleepAfter is how long it has to have had no work to sleep; 0 never.
	sleepAfter time.Duration
	// want is whether the controller wants it running, and worked when it
	// last had work, or was woken.
	want   bool
	worked time.Time
	// wokeAt is when it began coming up, wanted and not up (zero once up);
	// failedAt when it last did not come up within bootTimeout.
	wokeAt, failedAt time.Time
	// supervisor identifies the tailnet peer that holds (or last held)
	// Standby for this node, and supervisorHost its hostname for diagnostics.
	supervisor     string
	supervisorHost string
}

func supervisorKey(p tailnet.Peer) string {
	if p.NodeID != "" {
		return "id:" + p.NodeID
	}
	if p.Hostname != "" {
		return "host:" + p.Hostname
	}
	return ""
}

// hold notes a Standby call from the node's host, which says whether its VM
// runs; release when the call ends. Known for the first time (a new host,
// or a new controller), a node is wanted as it is: running, for one
// sleepAfter from now. So is one someone started: they want it.
func (s *standby) hold(host string, running bool, sleepAfter time.Duration) (release func()) {
	return s.holdBy(host, tailnet.Peer{}, running, sleepAfter)
}

// holdBy is hold that also binds the node's supervision to peer, so
// ReportHostUsage accepts host metrics only from that supervising peer.
func (s *standby) holdBy(host string, supervisor tailnet.Peer, running bool, sleepAfter time.Duration) (release func()) {
	now := s.now()
	s.mu.Lock()
	if s.nodes == nil {
		s.nodes = map[string]*power{}
	}
	p, known := s.nodes[host]
	if !known {
		// Since when it has been so is not known: a heartbeat it is online
		// by counts, but not one from before it last slept.
		p = &power{want: running, worked: now, running: running, since: now.Add(-api.OnlineWindow)}
		s.nodes[host] = p
	}
	if running && !p.running && !p.want {
		p.want, p.worked = true, now
	}
	if !running {
		p.stopping = false
	}
	if p.running != running {
		p.running, p.since = running, now
	}
	if key := supervisorKey(supervisor); key != "" {
		p.supervisor = key
		p.supervisorHost = cmp.Or(supervisor.Hostname, supervisor.NodeID)
	}
	p.calls++
	p.sleepAfter = sleepAfter
	s.mu.Unlock()
	s.changes.wake(host)
	return func() {
		s.mu.Lock()
		p.calls--
		p.ended = s.now()
		s.mu.Unlock()
	}
}

// authorizeHostReporter verifies that host is actively supervised by Standby
// (including within standbyGrace) and that peer matches its supervising peer.
func (s *standby) authorizeHostReporter(host string, peer tailnet.Peer, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.nodes[host]
	if p == nil || !p.supervised(now) {
		return api.Errorf(api.CodeConflict, "%s is not supervised by Standby", host).
			WithSuggestion("only a VM worker's active host supervisor (hotbed worker supervise) may report host usage")
	}
	caller := supervisorKey(peer)
	if p.supervisor == "" || caller == "" || p.supervisor != caller {
		who := cmp.Or(peer.Hostname, peer.NodeID, "unknown peer")
		sup := cmp.Or(p.supervisorHost, p.supervisor, "none")
		return api.Errorf(api.CodeNotAuthorized, "%s may not report host usage for %s (supervised by %s)", who, host, sup)
	}
	return nil
}

// supervised says the node's host powers it as the controller says.
func (p *power) supervised(now time.Time) bool {
	return p.calls > 0 || now.Sub(p.ended) < standbyGrace
}

// awake says the node runs, wanted, and has been heard from since it was
// started: its worker serves.
func (p *power) awake(lastSeen *int64) bool {
	return p.want && p.running && !p.stopping && lastSeen != nil && time.UnixMilli(*lastSeen).After(p.since)
}

// asleep says the node is supervised and not awake: powered off, or going
// to be, or still booting. A run placed on it waits for it to wake.
func (s *standby) asleep(n *pb.Node) bool {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.nodes[n.Hostname]
	return p != nil && p.supervised(now) && !p.awake(n.LastSeen)
}

// wakeFailed says the node, asleep, did not come up when last woken, a
// little while ago: it takes no runs for now.
func (s *standby) wakeFailed(n *pb.Node) bool {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.nodes[n.Hostname]
	return p != nil && !p.failedAt.IsZero() && now.Sub(p.failedAt) < wakeFailedHold && !p.awake(n.LastSeen)
}

// wake has the node run, and keeps it from sleeping for its sleepAfter.
func (s *standby) wake(host string) {
	now := s.now()
	s.mu.Lock()
	if p := s.nodes[host]; p != nil {
		p.want, p.worked = true, now
		// Up already, settle clears it.
		if p.wokeAt.IsZero() {
			p.wokeAt = now
		}
	}
	s.mu.Unlock()
	s.changes.wake(host)
}

// work is what the node is doing, as its view has it: what keeps it from
// sleeping.
type work struct {
	// working says it has work: a session's command, a workspace being
	// made, or clients using it directly. It sleeps only after a
	// sleepAfter without.
	working bool
	// busy says it is doing something that powering off would break, work
	// or not (a base freshened on its own): it sleeps only once it is done.
	busy bool
}

// workOf is what nv is doing.
func workOf(nv nodeView) work {
	var w work
	for _, c := range nv.commands {
		if api.Live(c.State) {
			w.busy = true
			w.working = w.working || c.SessionId != ""
		}
	}
	for _, ws := range nv.workspaces {
		if ws.State == pb.WorkspaceState_WORKSPACE_STATE_PREPARING {
			w.busy, w.working = true, true
		}
	}
	for _, b := range nv.bases {
		if b.State == pb.BaseState_BASE_STATE_BUILDING {
			w.busy = true
		}
	}
	if nv.online && len(nv.hb.GetActiveSessions()) > 0 {
		w.busy, w.working = true, true
	}
	return w
}

// settle decides whether the node should run, from what it is doing now,
// and says why.
func (s *standby) settle(host string, lastSeen *int64, w work) (bool, string) {
	return s.settleAt(host, lastSeen, w, s.now())
}

// settleAt is settle evaluated at the time now when w was observed, so a view
// read before a clock advance or slow query cannot stamp p.worked with a later
// time or move it backwards past a newer wake.
func (s *standby) settleAt(host string, lastSeen *int64, w work, now time.Time) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.nodes[host]
	if p == nil {
		return false, "unknown"
	}
	// Wanted and not up, it is busy coming up, however it came to be wanted
	// (woken, started by hand, restarted after a crash), for bootTimeout
	// at most.
	booting := p.want && !p.awake(lastSeen)
	switch {
	case !booting:
		p.wokeAt = time.Time{}
	case p.wokeAt.IsZero():
		p.wokeAt = now
	}
	if (w.working || booting) && now.After(p.worked) {
		p.worked = now
	}
	if booting && now.Sub(p.wokeAt) >= bootTimeout {
		p.failedAt, p.wokeAt = now, time.Time{}
		if p.sleepAfter > 0 {
			p.want, p.stopping = false, p.running
			return false, fmt.Sprintf("it did not wake within %s", bootTimeout)
		}
	}
	switch {
	case p.sleepAfter <= 0:
		p.want = true
		return true, "it never sleeps"
	case !p.want:
		return false, "asleep until a run is placed on it"
	case w.busy || booting:
		return true, "busy"
	case now.Sub(p.worked) >= p.sleepAfter:
		p.want, p.stopping = false, p.running
		return false, fmt.Sprintf("no work for %s", p.sleepAfter)
	}
	return true, fmt.Sprintf("last had work %s ago; sleeps after %s", now.Sub(p.worked).Round(time.Second), p.sleepAfter)
}

// Standby powers a node as the controller wants it, by its host: what it
// wants now, then each change, for up to maxWait.
func (s *Server) Standby(ctx context.Context, req *pb.StandbyRequest, stream hotbedv1connect.ClusterServiceStandbyServerStream) error {
	if req.SleepAfterSeconds < 0 {
		return api.Errorf(api.CodeInvalidArgs, "sleep_after_seconds %d is negative", req.SleepAfterSeconds)
	}
	if _, err := s.nodeNamed(ctx, req.Node); err != nil {
		return err
	}
	release := s.power.holdBy(req.Node, httpx.Peer(ctx), req.Running, time.Duration(req.SleepAfterSeconds)*time.Second)
	defer release()
	end := time.NewTimer(maxWait)
	defer end.Stop()
	tick := time.NewTicker(api.ReportInterval)
	defer tick.Stop()
	var sent *pb.StandbyResponse
	for {
		changed := s.power.changes.wait(req.Node)
		run, why, err := s.settlePower(ctx, req.Node)
		if err != nil {
			return err
		}
		if sent == nil || sent.Run != run {
			if sent != nil {
				slog.Info("node power", "node", req.Node, "run", run, "why", why)
			}
			sent = &pb.StandbyResponse{Run: run, Reason: why}
			if err := stream.Send(sent); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-end.C:
			return nil
		case <-changed:
		case <-tick.C:
		}
	}
}

// settlePower decides whether the node with host name host should run. It
// waits out a placement under way: a node placement just chose has work.
func (s *Server) settlePower(ctx context.Context, host string) (bool, string, error) {
	s.placing.Lock()
	defer s.placing.Unlock()
	now := s.now()
	readViews := s.views
	if s.power.views != nil {
		readViews = s.power.views
	}
	nvs, err := readViews(ctx)
	if err != nil {
		return false, "", err
	}
	for _, nv := range nvs {
		if nv.node.Hostname == host {
			run, why := s.power.settleAt(host, nv.node.LastSeen, workOf(nv), now)
			return run, why, nil
		}
	}
	return false, "", api.Errorf(api.CodeNotFound, "no node is called %s", host)
}

// nodeNamed is the node with host name host.
func (s *Server) nodeNamed(ctx context.Context, host string) (nodeRow, error) {
	rows, err := s.cfg.Store.Nodes(ctx)
	if err != nil {
		return nodeRow{}, err
	}
	for _, n := range rows {
		if n.node.Hostname == host {
			return n, nil
		}
	}
	return nodeRow{}, api.Errorf(api.CodeNotFound, "no node is called %s", host).
		WithSuggestion("hotbed nodes lists the nodes; a worker joins with hotbed worker init")
}

// awaken wakes the node with host name host for a run and waits, up to
// wakeWait, for it to serve on the controller's build; NODE_WAKING, which
// the client calls again on, when it is not up by then.
func (s *Server) awaken(ctx context.Context, host string) error {
	s.power.wake(host)
	slog.Info("waking node for a run", "node", host)
	deadline := time.NewTimer(s.power.wakeWait)
	defer deadline.Stop()
	for {
		changed := s.power.changes.wait(host)
		n, err := s.nodeNamed(ctx, host)
		if err != nil {
			return err
		}
		if s.serves(n) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return &api.Error{Code: api.CodeNodeWaking, Retryable: true,
				Message:    fmt.Sprintf("%s was asleep and is still waking up", host),
				Suggestion: "run again in a moment: hotbed run waits for it by itself"}
		case <-changed:
		}
	}
}

// serves says the node is awake, online, and on the build it follows.
func (s *Server) serves(n nodeRow) bool {
	node := s.withOnline(n.node)
	if !node.Online || node.Asleep {
		return false
	}
	res := n.hb.GetResources()
	want := s.follows(res.GetArch())
	return want == 0 || version.Of(res.GetVersion()) == want
}
