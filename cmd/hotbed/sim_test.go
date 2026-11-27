package main

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// TestSimulation drives a fleet of three workers through random steps
// (sessions, workspaces, commands, ends) while it takes the controller down
// and restarts it, restarts workers, cuts them off from the controller and
// wipes a worker's database. Then it heals everything and checks what must
// hold whatever happened:
//
//   - the controller's view of each worker is exactly the worker's state;
//   - every command a client started has exactly one end in its session's
//     events (none only when its worker lost its database before the
//     controller heard of it), and was admitted at most once;
//   - every command that exited is in history.
//
// HOTBED_SIM_SEED replays a run (the log names its seed);
// HOTBED_SIM_STEPS sets how many steps it takes.
func TestSimulation(t *testing.T) {
	seed := time.Now().UnixNano()
	if s := os.Getenv("HOTBED_SIM_SEED"); s != "" {
		var err error
		if seed, err = strconv.ParseInt(s, 10, 64); err != nil {
			t.Fatalf("HOTBED_SIM_SEED=%q: %v", s, err)
		}
	}
	steps := 40
	if s := os.Getenv("HOTBED_SIM_STEPS"); s != "" {
		var err error
		if steps, err = strconv.Atoi(s); err != nil {
			t.Fatalf("HOTBED_SIM_STEPS=%q: %v", s, err)
		}
		if steps <= 0 {
			t.Fatalf("HOTBED_SIM_STEPS=%d: must be positive", steps)
		}
	}
	t.Logf("seed %d (steps %d): reproduce with HOTBED_SIM_SEED=%d HOTBED_SIM_STEPS=%d go test -race -run '^TestSimulation$' ./cmd/hotbed", seed, steps, seed, steps)
	// Registered before newFleet so LIFO cleanup prints the repro command last on failure.
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("reproduce with HOTBED_SIM_SEED=%d HOTBED_SIM_STEPS=%d go test -race -run '^TestSimulation$' ./cmd/hotbed", seed, steps)
		}
	})
	c := newFleet(t, 3)
	c.addRepo(upstream(t))
	s := &sim{c: c, rng: rand.New(rand.NewPCG(uint64(seed), 0)), cmds: map[string]simCommand{}}
	for i := range steps {
		op := s.pick()
		t.Logf("step %d: %s", i, op)
	}
	s.heal()
	c.eventually("every command to end", func() bool {
		for _, nd := range c.nodes {
			for _, cmd := range nd.eng().Commands() {
				if api.Live(cmd.State) {
					return false
				}
			}
		}
		return true
	})
	c.converged()
	s.check()
}

type sim struct {
	c        *cluster
	rng      *rand.Rand
	n        int
	sessions []*simSession
	cmds     map[string]simCommand
	ctrlDown bool
	wiped    bool
}

type simSession struct {
	id    string
	node  int // its workspace's worker; -1 before add
	ended bool
}

type simCommand struct {
	session string
	node    int
	// wiped says its worker lost its database after it started: the
	// controller may never have heard of it.
	wiped bool
}

// pick takes one random step and says what it did.
func (s *sim) pick() string {
	type step struct {
		weight int
		do     func() string
	}
	steps := []step{
		{3, s.start}, {4, s.add}, {6, s.run}, {4, s.detach}, {2, s.stop}, {2, s.end},
		{2, s.toggleController}, {1, s.restartController}, {2, s.restartWorker}, {2, s.toggleCut}, {1, s.wipe},
	}
	total := 0
	for _, st := range steps {
		total += st.weight
	}
	n := s.rng.IntN(total)
	for _, st := range steps {
		if n < st.weight {
			return st.do()
		}
		n -= st.weight
	}
	panic("unreachable")
}

func (s *sim) live() []*simSession {
	return slices.DeleteFunc(slices.Clone(s.sessions), func(ss *simSession) bool { return ss.ended })
}

func (s *sim) withWorkspace() []*simSession {
	return slices.DeleteFunc(s.live(), func(ss *simSession) bool { return ss.node < 0 })
}

func (s *sim) start() string {
	s.n++
	id := fmt.Sprintf("sim-s%d", s.n)
	code, _, _ := s.c.hotbed("session", "start", id)
	if code != 0 {
		return "start " + id + ": failed"
	}
	s.sessions = append(s.sessions, &simSession{id: id, node: -1})
	return "start " + id
}

func (s *sim) add() string {
	free := slices.DeleteFunc(s.live(), func(ss *simSession) bool { return ss.node >= 0 })
	if len(free) == 0 {
		return s.start()
	}
	ss := free[s.rng.IntN(len(free))]
	ws, err := s.c.tryWorkspace(ss.id, nil)
	if err != nil {
		return fmt.Sprintf("add %s: failed", ss.id)
	}
	node := -1
	for i, n := range s.c.nodes {
		if n.peer.Hostname == ws.Node {
			node = i
			break
		}
	}
	ss.node = node
	return fmt.Sprintf("add %s on %s", ss.id, ws.Node)
}

func (s *sim) run() string {
	ws := s.withWorkspace()
	if len(ws) == 0 {
		return s.add()
	}
	ss := ws[s.rng.IntN(len(ws))]
	want := s.rng.IntN(4)
	code, _, stderr := s.c.hotbed("run", "-s", ss.id, "-r", "app", "--", "sh", "-c", fmt.Sprintf("exit %d", want))
	m := started.FindStringSubmatch(stderr)
	if m == nil {
		return fmt.Sprintf("run in %s: started nothing (exit %d)", ss.id, code)
	}
	s.cmds[m[1]] = simCommand{session: ss.id, node: ss.node}
	if code != want {
		s.c.t.Errorf("run %s of exit %d in %s returned %d", m[1], want, ss.id, code)
	}
	return fmt.Sprintf("run %s in %s: exit %d", m[1], ss.id, code)
}

func (s *sim) detach() string {
	ws := s.withWorkspace()
	if len(ws) == 0 {
		return s.add()
	}
	ss := ws[s.rng.IntN(len(ws))]
	cmd := &pb.Command{}
	code, stdout, _ := s.c.hotbed("--json", "run", "-s", ss.id, "-r", "app", "--detach", "--mem", "1G", "--", "sh", "-c", fmt.Sprintf("sleep 0.%d; exit %d", s.rng.IntN(9)+1, s.rng.IntN(3)))
	if code != 0 || decode(stdout, cmd) != nil {
		return fmt.Sprintf("detach in %s: failed", ss.id)
	}
	s.cmds[cmd.Id] = simCommand{session: ss.id, node: ss.node}
	return fmt.Sprintf("detach %s in %s", cmd.Id, ss.id)
}

func (s *sim) stop() string {
	var ids []string
	for id := range s.cmds {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return s.detach()
	}
	slices.Sort(ids)
	id := ids[s.rng.IntN(len(ids))]
	code, _, _ := s.c.hotbed("stop", "-s", s.cmds[id].session, id)
	return fmt.Sprintf("stop %s: exit %d", id, code)
}

func (s *sim) end() string {
	l := s.live()
	if len(l) == 0 {
		return s.start()
	}
	ss := l[s.rng.IntN(len(l))]
	if code, _, _ := s.c.hotbed("session", "end", "-s", ss.id); code != 0 {
		return "end " + ss.id + ": failed"
	}
	ss.ended = true
	return "end " + ss.id
}

func (s *sim) toggleController() string {
	s.ctrlDown = !s.ctrlDown
	s.c.ctrl.setDown(s.ctrlDown)
	return fmt.Sprintf("controller down: %v", s.ctrlDown)
}

func (s *sim) restartController() string {
	s.c.restart()
	return "restart the controller"
}

func (s *sim) restartWorker() string {
	nd := s.c.nodes[s.rng.IntN(len(s.c.nodes))]
	nd.restart()
	return "restart " + nd.peer.Hostname
}

func (s *sim) toggleCut() string {
	nd := s.c.nodes[s.rng.IntN(len(s.c.nodes))]
	cut := !nd.cut.Load()
	nd.cut.Store(cut)
	return fmt.Sprintf("%s cut off: %v", nd.peer.Hostname, cut)
}

// wipe loses one worker's database, once a run.
func (s *sim) wipe() string {
	if s.wiped {
		return s.restartWorker()
	}
	s.wiped = true
	i := s.rng.IntN(len(s.c.nodes))
	s.c.nodes[i].wipe()
	for id, cmd := range s.cmds {
		if cmd.node == i {
			cmd.wiped = true
			s.cmds[id] = cmd
		}
	}
	// Its sessions' workspaces are lost: they get new ones.
	for _, ss := range s.sessions {
		if ss.node == i {
			ss.node = -1
		}
	}
	return "wipe " + s.c.nodes[i].peer.Hostname + "'s database"
}

func (s *sim) heal() {
	s.ctrlDown = false
	s.c.ctrl.setDown(false)
	for _, nd := range s.c.nodes {
		nd.cut.Store(false)
	}
	s.c.heartbeat()
}

func (s *sim) check() {
	t := s.c.t
	history, err := s.c.store.History(t.Context(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	events := map[string][]*pb.SessionEvent{}
	for id, cmd := range s.cmds {
		evs, ok := events[cmd.session]
		if !ok {
			evs = s.c.events(cmd.session)
			events[cmd.session] = evs
		}
		got := ends(evs, id)
		switch {
		case len(got) > 1, len(got) == 0 && !cmd.wiped:
			t.Errorf("command %s (session %s) ended %d times: %q", id, cmd.session, len(got), got)
		}
		admitted := 0
		exited := false
		for _, e := range evs {
			if c := e.GetCommand(); c.GetCommandId() == id {
				admitted += map[bool]int{true: 1}[c.What == pb.CommandUpdate_WHAT_ADMITTED]
				exited = exited || c.What == pb.CommandUpdate_WHAT_EXITED
			}
		}
		if admitted > 1 {
			t.Errorf("command %s was admitted %d times", id, admitted)
		}
		if exited && !slices.ContainsFunc(history, func(f *pb.FinishedCommand) bool { return f.Id == id }) {
			t.Errorf("command %s exited but is not in history", id)
		}
	}
	t.Logf("checked %d commands in %d sessions", len(s.cmds), len(s.sessions))
}

// decode reads a successful envelope's data into m.
func decode(stdout string, m proto.Message) error {
	var out output
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		return err
	}
	if !out.OK {
		return fmt.Errorf("%+v", out.Error)
	}
	return protojson.Unmarshal(out.Data, m)
}
