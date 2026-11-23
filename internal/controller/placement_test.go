package controller

import (
	"fmt"
	"slices"
	"testing"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

const gib = 1 << 30

// machine is a node for placement to weigh: online, idle and empty unless
// said otherwise.
type machine struct {
	host, arch string
	busy       float64
	queued     int
	bench      bool
	ram, used  int64
	offline    bool
	// asleep is a node powered off while idle (Standby): offline, its last
	// heartbeat from before it slept.
	asleep bool
	// wakeFailed: asleep, it did not come up when last woken.
	wakeFailed bool
	// bases are the node's bases of the project, oldest first.
	bases []pb.BaseState
	// build is the build the node runs; 0 for a dev build.
	build int64
	// pressure is its CPU pressure (%); holding the signal, if any, that
	// holds new work there.
	pressure  float64
	holding   string
	hostUsage *pb.HostUsage
}

func (m machine) view() nodeView {
	ram := m.ram
	if ram == 0 {
		ram = 24 * gib
	}
	arch := m.arch
	if arch == "" {
		arch = "amd64"
	}
	res := &pb.Resources{Arch: arch, Cpus: 8, MemoryBytes: ram, Version: "dev (0123456789ab)"}
	if m.build != 0 {
		res.Version = fmt.Sprintf("build %d (0123456789ab)", m.build)
	}
	if m.bench {
		res.BenchCpus = "6-7"
	}
	v := &view{}
	for i, st := range m.bases {
		v.bases = append(v.bases, &pb.Base{Project: "app", Generation: int32(i + 1), State: st})
	}
	for range m.queued {
		v.commands = append(v.commands, &pb.Command{State: pb.CommandState_COMMAND_STATE_QUEUED})
	}
	return nodeView{
		nodeRow: nodeRow{
			node: &pb.Node{Id: "n-" + m.host, Hostname: m.host, Resources: res, Asleep: m.asleep, HostUsage: m.hostUsage},
			hb:   &pb.HeartbeatRequest{Resources: res, Usage: &pb.NodeUsage{CpuBusy: m.busy, MemoryUsedBytes: m.used, CpuPressure: m.pressure}, Admission: m.admission()},
		},
		online:     !m.offline && !m.asleep,
		wakeFailed: m.wakeFailed,
		view:       v,
	}
}

// admission is what the machine's worker says of how it admits work.
func (m machine) admission() *pb.Admission {
	a := &pb.Admission{Holding: m.holding != ""}
	for _, name := range []string{"cpu-pressure", "memory-pressure", "memory-reserved", "disk-free"} {
		a.Signals = append(a.Signals, &pb.AdmissionSignal{Name: name, Holding: name == m.holding})
	}
	if a.Holding {
		a.Reason = m.holding + " holds new work"
	}
	return a
}

var warm = []pb.BaseState{pb.BaseState_BASE_STATE_WARM}

// noFollow says workers follow no build of the controller's: a dev build.
func noFollow(string) int64 { return 0 }

// pick places a run of app among the machines, where the session has
// workspaces on the hosts in mine.
func pick(t *testing.T, ms []machine, mine []string, p *pb.Placement, syncs bool) (string, *api.Error) {
	t.Helper()
	nvs := make([]nodeView, 0, len(ms))
	for _, m := range ms {
		nvs = append(nvs, m.view())
	}
	held := map[string]string{}
	for _, h := range mine {
		held["n-"+h] = "ws-" + h
	}
	c, _, err := choose(candidates(nvs, "app", held, noFollow), p, nil, syncs)
	if err != nil {
		e, ok := err.(*api.Error)
		if !ok {
			t.Fatalf("choose failed with %T %v, want an *api.Error", err, err)
		}
		return "", e
	}
	return c.host, nil
}

func TestPlacement(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ms    []machine
		mine  []string
		p     *pb.Placement
		syncs bool
		want  string
	}{
		{
			name: "an idle node with no base beats a saturated warm one",
			ms:   []machine{{host: "ubu1", busy: 0.99, bases: warm}, {host: "mac-worker", arch: "arm64"}},
			want: "mac-worker", syncs: true,
		},
		{
			name: "a lightly loaded warm node beats an idle cold one",
			ms:   []machine{{host: "ubu1", busy: 0.3, bases: warm}, {host: "mac-worker", arch: "arm64"}},
			want: "ubu1", syncs: true,
		},
		{
			name: "the session's node is kept though somewhat busier",
			ms:   []machine{{host: "a", busy: 0.5, bases: warm}, {host: "b", busy: 0.3, bases: warm}},
			mine: []string{"a"}, want: "a", syncs: true,
		},
		{
			name: "the session moves off a node much busier than another",
			ms:   []machine{{host: "a", busy: 0.9, bases: warm}, {host: "b", busy: 0.1, bases: warm}},
			mine: []string{"a"}, want: "b", syncs: true,
		},
		{
			name: "the session moves off a saturated node: CPU pressure tells it from one merely busy",
			ms:   []machine{{host: "a", busy: 1, pressure: 70, bases: warm}, {host: "b", busy: 0.6, bases: warm}},
			mine: []string{"a"}, want: "b", syncs: true,
		},
		{
			name: "light CPU pressure keeps the session where it is",
			ms:   []machine{{host: "a", busy: 0.5, pressure: 15, bases: warm}, {host: "b", busy: 0.4, bases: warm}},
			mine: []string{"a"}, want: "a", syncs: true,
		},
		{
			name: "a node that holds new work loses to one that does not",
			ms:   []machine{{host: "a", busy: 0.2, holding: "cpu-pressure", bases: warm}, {host: "b", busy: 0.6, bases: warm}},
			want: "b", syncs: true,
		},
		{
			name: "a measurement is not held by CPU pressure, on CPUs of its own",
			ms:   []machine{{host: "a", bench: true, holding: "cpu-pressure", bases: warm}, {host: "b", bench: true, busy: 0.6, bases: warm}},
			p:    &pb.Placement{Bench: true}, want: "a", syncs: true,
		},
		{
			name: "queued commands count as load",
			ms:   []machine{{host: "a", queued: 8, bases: warm}, {host: "b", busy: 0.5, bases: warm}},
			want: "b", syncs: true,
		},
		{
			name: "arch leaves out the other architecture",
			ms:   []machine{{host: "ubu1", bases: warm}, {host: "asahimbp", arch: "arm64", busy: 0.9}},
			p:    &pb.Placement{Arch: "arm64"}, want: "asahimbp", syncs: true,
		},
		{
			name: "node pins the run",
			ms:   []machine{{host: "ubu1", bases: warm}, {host: "asahimbp", arch: "arm64", busy: 0.9}},
			p:    &pb.Placement{Node: "asahimbp"}, want: "asahimbp", syncs: true,
		},
		{
			name: "a measurement goes only where there are measurement CPUs",
			ms:   []machine{{host: "a", bases: warm}, {host: "b", bench: true, busy: 0.9}},
			p:    &pb.Placement{Bench: true}, want: "b", syncs: true,
		},
		{
			name: "guaranteed memory prefers a node it fits on now",
			ms:   []machine{{host: "a", bases: warm, used: 20 * gib}, {host: "b", bases: warm, busy: 0.5}},
			p:    &pb.Placement{MemoryLowBytes: 8 * gib}, want: "b", syncs: true,
		},
		{
			name: "guaranteed memory leaves out a node without that much RAM",
			ms:   []machine{{host: "small", ram: 8 * gib, bases: warm}, {host: "big", ram: 64 * gib, busy: 0.9}},
			p:    &pb.Placement{MemoryLowBytes: 16 * gib}, want: "big", syncs: true,
		},
		{
			name: "a node whose newest base failed with none warm is kept off",
			ms:   []machine{{host: "a", bases: []pb.BaseState{pb.BaseState_BASE_STATE_FAILED}}, {host: "b", busy: 0.95, bases: warm}},
			want: "b", syncs: true,
		},
		{
			name: "a failed newer base does not matter beside a warm one",
			ms:   []machine{{host: "a", bases: []pb.BaseState{pb.BaseState_BASE_STATE_WARM, pb.BaseState_BASE_STATE_FAILED}}, {host: "b", busy: 0.5, bases: warm}},
			want: "a", syncs: true,
		},
		{
			name: "a node whose base failed is still where the run goes when nothing else fits",
			ms:   []machine{{host: "a", bases: []pb.BaseState{pb.BaseState_BASE_STATE_FAILED}}},
			want: "a", syncs: true,
		},
		{
			name: "a run that syncs nothing stays in the session's workspace however busy",
			ms:   []machine{{host: "a", busy: 1, queued: 20, bases: warm}, {host: "b", bases: warm}},
			mine: []string{"a"}, want: "a",
		},
		{
			name: "a run that syncs nothing starts afresh where its restrictions send it",
			ms:   []machine{{host: "a", bases: warm}, {host: "b", bench: true, busy: 0.9}},
			mine: []string{"a"}, p: &pb.Placement{Bench: true}, want: "b",
		},
		{
			name: "a run that syncs nothing picks the least busy of the session's workspaces",
			ms:   []machine{{host: "a", busy: 0.9, bases: warm}, {host: "b", busy: 0.2, bases: warm}, {host: "c", bases: warm}},
			mine: []string{"a", "b"}, want: "b",
		},
		{
			name: "a run that syncs nothing goes anywhere when the session has no workspace yet",
			ms:   []machine{{host: "a", busy: 1, bases: warm}, {host: "b", bases: warm}},
			want: "b",
		},
		{
			name: "an awake node, even a busy one, beats a node asleep",
			ms:   []machine{{host: "ubu1", busy: 0.8, bases: warm}, {host: "mac-worker", asleep: true, busy: 0.9, used: 20 * gib, bases: warm}},
			want: "ubu1", syncs: true,
		},
		{
			name: "a node asleep is woken when every awake one is saturated",
			ms:   []machine{{host: "ubu1", busy: 1, queued: 16, bases: warm}, {host: "mac-worker", asleep: true, bases: warm}},
			want: "mac-worker", syncs: true,
		},
		{
			name: "a node asleep takes the run when it alone fits",
			ms:   []machine{{host: "ubu1", bases: warm}, {host: "mac-worker", arch: "arm64", asleep: true}},
			p:    &pb.Placement{Arch: "arm64"}, want: "mac-worker", syncs: true,
		},
		{
			name: "a run that syncs nothing wakes the node of the session's workspace",
			ms:   []machine{{host: "mac-worker", asleep: true, bases: warm}, {host: "ubu1", bases: warm}},
			mine: []string{"mac-worker"}, want: "mac-worker",
		},
		{
			name: "a node that did not wake is passed over, though the session's workspace is there",
			ms:   []machine{{host: "mac-worker", asleep: true, wakeFailed: true, bases: warm}, {host: "ubu1", busy: 0.9}},
			mine: []string{"mac-worker"}, want: "ubu1", syncs: true,
		},
		{
			name: "ties go to the most free memory, then the host name",
			ms:   []machine{{host: "b", bases: warm}, {host: "a", bases: warm}, {host: "c", bases: warm, ram: 32 * gib}},
			want: "c", syncs: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pick(t, tc.ms, tc.mine, tc.p, tc.syncs)
			if err != nil {
				t.Fatalf("choose failed: %v", err)
			}
			if got != tc.want {
				t.Errorf("chose %s, want %s", got, tc.want)
			}
		})
	}
}

func TestPlacementRefusals(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ms        []machine
		mine      []string
		p         *pb.Placement
		syncs     bool
		code      api.Code
		retryable bool
		message   string
	}{
		{
			name: "no node of the architecture",
			ms:   []machine{{host: "ubu1"}, {host: "asahimbp", arch: "arm64"}},
			p:    &pb.Placement{Arch: "riscv64"}, syncs: true,
			code:    api.CodeInvalidArgs,
			message: "no node can take the run: ubu1 is amd64, not riscv64; asahimbp is arm64, not riscv64",
		},
		{
			name: "the one node that fits is offline",
			ms:   []machine{{host: "ubu1"}, {host: "asahimbp", arch: "arm64", offline: true}},
			p:    &pb.Placement{Arch: "arm64"}, syncs: true,
			code: api.CodeWorkerUnreachable, retryable: true,
			message: "no node can take the run: ubu1 is amd64, not arm64; asahimbp is offline",
		},
		{
			name: "the one node that fits did not wake when last woken",
			ms:   []machine{{host: "ubu1"}, {host: "mac-worker", arch: "arm64", asleep: true, wakeFailed: true}},
			p:    &pb.Placement{Arch: "arm64"}, syncs: true,
			code: api.CodeWorkerUnreachable, retryable: true,
			message: "no node can take the run: ubu1 is amd64, not arm64; mac-worker did not wake when last woken",
		},
		{
			name: "no node has measurement CPUs",
			ms:   []machine{{host: "a"}},
			p:    &pb.Placement{Bench: true}, syncs: true,
			code:    api.CodeInvalidArgs,
			message: "no node can take the run: a has no measurement CPUs",
		},
		{
			name: "a run that syncs nothing cannot leave its workspace's offline node",
			ms:   []machine{{host: "a", offline: true, bases: warm}, {host: "b", bases: warm}},
			mine: []string{"a"},
			code: api.CodeWorkerUnreachable, retryable: true,
			message: "the run syncs no checkout, so it runs only in the session's workspace of the repo, and no node with one can take it: a is offline",
		},
		{
			name: "no node of that name",
			ms:   []machine{{host: "ubu1"}},
			p:    &pb.Placement{Node: "nowhere"}, syncs: true,
			code:    api.CodeInvalidArgs,
			message: "no node is called nowhere",
		},
		{
			name: "the named node does not fit",
			ms:   []machine{{host: "ubu1"}, {host: "asahimbp", arch: "arm64"}},
			p:    &pb.Placement{Node: "ubu1", Arch: "arm64"}, syncs: true,
			code:    api.CodeInvalidArgs,
			message: "no node can take the run: ubu1 is amd64, not arm64",
		},
		{
			name: "an empty cluster",
			code: api.CodeWorkerUnreachable, retryable: true, syncs: true,
			message: "the cluster has no nodes",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pick(t, tc.ms, tc.mine, tc.p, tc.syncs)
			if err == nil {
				t.Fatalf("chose %s, want a refusal", got)
			}
			if err.Code != tc.code || err.Retryable != tc.retryable || err.Message != tc.message {
				t.Errorf("refused with %s (retryable %v) %q, want %s (retryable %v) %q", err.Code, err.Retryable, err.Message, tc.code, tc.retryable, tc.message)
			}
		})
	}
}

// A run never goes to a node on another build than the controller's (its
// workers follow it within minutes of an upgrade), so a new controller and an
// old worker never meet over a workspace; while every node that would fit is
// upgrading, the run fails retryably.
func TestPlacementWaitsForNodesOnAnotherBuild(t *testing.T) {
	follow42 := func(arch string) int64 { return 42 }
	place := func(ms []machine) (string, error) {
		var nvs []nodeView
		for _, m := range ms {
			nvs = append(nvs, m.view())
		}
		c, _, err := choose(candidates(nvs, "app", nil, follow42), &pb.Placement{}, nil, true)
		return c.host, err
	}
	host, err := place([]machine{{host: "old", build: 41, bases: warm}, {host: "new", build: 42, busy: 0.9}})
	if err != nil || host != "new" {
		t.Errorf("placed on %q (%v), want new: old is on build 41", host, err)
	}
	_, err = place([]machine{{host: "old", build: 41, bases: warm}})
	e := api.FromConnect(err)
	if e == nil || e.Code != api.CodeWorkerUnreachable || !e.Retryable || e.Message != "no node can take the run: old is on build 41, not yet the controller's" {
		t.Errorf("err = %v, want a retryable WORKER_UNREACHABLE saying old is on build 41", err)
	}
	if e != nil && e.Suggestion != "retry once the node finishes upgrading" {
		t.Errorf("suggestion = %q, want %q", e.Suggestion, "retry once the node finishes upgrading")
	}
	// upgradingFor identifies online nodes that would fit once upgraded, and
	// excludes offline or wrong-arch nodes.
	cs := candidates([]nodeView{
		(machine{host: "old-amd", arch: "amd64", build: 41}).view(),
		(machine{host: "off-amd", arch: "amd64", build: 41, offline: true}).view(),
		(machine{host: "old-arm", arch: "arm64", build: 41}).view(),
	}, "app", nil, follow42)
	if got := upgradingFor(cs, &pb.Placement{Arch: "amd64"}, nil, true); !slices.Equal(got, []string{"old-amd"}) {
		t.Errorf("upgradingFor(amd64) = %v, want [old-amd]", got)
	}
	if got := upgradingFor(cs, &pb.Placement{Arch: "riscv64"}, nil, true); len(got) != 0 {
		t.Errorf("upgradingFor(riscv64) = %v, want none", got)
	}
	// A run that syncs no checkout waits on its workspace's upgrading node
	// rather than abandoning it for another upgraded node.
	csHeld := candidates([]nodeView{
		(machine{host: "old", build: 41, bases: warm}).view(),
		(machine{host: "new", build: 42, bases: warm}).view(),
	}, "app", map[string]string{"n-old": "ws-old"}, follow42)
	if _, _, err := choose(csHeld, &pb.Placement{}, nil, false); err == nil {
		t.Errorf("no-sync run left session's upgrading workspace for another node")
	} else if got := upgradingFor(csHeld, &pb.Placement{}, nil, false); !slices.Equal(got, []string{"old"}) {
		t.Errorf("upgradingFor(no-sync held) = %v, want [old]", got)
	}
	// Asleep, it moves to the controller's build as it wakes, before the run
	// is placed again.
	host, err = place([]machine{{host: "old", build: 41, asleep: true, bases: warm}})
	if err != nil || host != "old" {
		t.Errorf("placed on %q (%v), want old: asleep, it upgrades as it wakes", host, err)
	}
}

// Every placement is recorded with each node's cost in parts, or why it
// could not take the run, and the node chosen.
func TestPlacementDecisionExplainsItself(t *testing.T) {
	ms := []machine{
		{host: "a", busy: 1, pressure: 70, holding: "cpu-pressure", bases: warm},
		{host: "b", busy: 0.25, bases: warm},
		{host: "c", arch: "arm64"},
	}
	nvs := make([]nodeView, 0, len(ms))
	for _, m := range ms {
		nvs = append(nvs, m.view())
	}
	p := &pb.Placement{Arch: "amd64"}
	cs := candidates(nvs, "app", map[string]string{"n-a": "ws-a"}, noFollow)
	c, why, err := choose(cs, p, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	d := placementDecision(cs, p, nil, c, why, err)
	if d.Hostname != "b" || d.Reason != "the least costly node" || len(d.Candidates) != 3 {
		t.Fatalf("decision = %v", d)
	}
	parts := func(pc *pb.PlacementCandidate) map[string]float64 {
		out := map[string]float64{}
		for _, p := range pc.Parts {
			out[p.Name] = p.Value
		}
		return out
	}
	a, b, cc := d.Candidates[0], d.Candidates[1], d.Candidates[2]
	if got, want := parts(a), map[string]float64{"cpu-busy": 1, "queued": 0, "cpu-pressure": 0.7, "holding": 1}; !a.Mine || a.Chosen || fmt.Sprint(got) != fmt.Sprint(want) || a.Cost != 2.7 {
		t.Errorf("a = %v (parts %v), want the session's node, not chosen, parts %v, cost 2.7", a, got, want)
	}
	if got, want := parts(b), map[string]float64{"cpu-busy": 0.25, "queued": 0, "cpu-pressure": 0, "move": 0.3}; !b.Chosen || fmt.Sprint(got) != fmt.Sprint(want) || b.Cost != 0.55 {
		t.Errorf("b = %v (parts %v), want chosen, parts %v, cost 0.55", b, got, want)
	}
	if cc.Unfit != "c is arm64, not amd64" || cc.Parts != nil || cc.Chosen {
		t.Errorf("c = %v, want ruled out as arm64", cc)
	}

	// A refusal records why no node could take it.
	p = &pb.Placement{Arch: "riscv64"}
	c, why, err = choose(cs, p, nil, true)
	d = placementDecision(cs, p, nil, c, why, err)
	if d.Hostname != "" || d.Reason != "no node can take the run: a is amd64, not riscv64; b is amd64, not riscv64; c is arm64, not riscv64" {
		t.Errorf("refused decision = %v", d)
	}
}

func TestPlacementPenalizesHostStarvedVM(t *testing.T) {
	starvedVM := machine{
		host:  "mac-worker",
		busy:  0.80, // guest thinks it is busy, PSI is 0, worker hasn't held yet
		bases: warm,
		hostUsage: &pb.HostUsage{
			VmCpuBusy:          0.18, // 62% CPU stolen on macOS host
			HostCpuBusy:        0.92,
			HostLoad_1M:         16.0,
			HostCpus:           12,
			HostMemoryPressure: 0,
		},
	}
	healthy := machine{
		host:  "ubu1",
		busy:  0.55,
		bases: warm,
	}

	nvs := []nodeView{starvedVM.view(), healthy.view()}
	p := &pb.Placement{}
	// Even when the session already has a workspace on mac-worker, host starvation
	// (holding + host-cpu-starvation cost parts) moves the run to healthy ubu1.
	cs := candidates(nvs, "app", map[string]string{"n-mac-worker": "ws-mac"}, noFollow)
	c, why, err := choose(cs, p, nil, true)
	if err != nil {
		t.Fatalf("choose: %v", err)
	}
	if c.host != "ubu1" {
		t.Fatalf("chose %s, want ubu1 over host-starved mac-worker", c.host)
	}

	d := placementDecision(cs, p, nil, c, why, err)
	macCand := d.Candidates[0]
	parts := map[string]float64{}
	for _, pt := range macCand.Parts {
		parts[pt.Name] = pt.Value
	}
	if parts["holding"] != holdingCost {
		t.Errorf("mac-worker holding part = %v, want %v (parts=%v)", parts["holding"], holdingCost, parts)
	}
	if parts["host-cpu-starvation"] <= 0 {
		t.Errorf("mac-worker host-cpu-starvation part = %v, want > 0 (parts=%v)", parts["host-cpu-starvation"], parts)
	}

	// Healthy VM node with normal HostUsage gets no holding or host-cpu-starvation penalty.
	healthyVM := machine{
		host:  "mac-healthy",
		busy:  0.20,
		bases: warm,
		hostUsage: &pb.HostUsage{
			VmCpuBusy:          0.19,
			HostCpuBusy:        0.30,
			HostLoad_1M:         3.0,
			HostCpus:           12,
			HostMemoryPressure: 0,
		},
	}
	cs2 := candidates([]nodeView{healthyVM.view(), healthy.view()}, "app", nil, noFollow)
	c2, _, err := choose(cs2, p, nil, true)
	if err != nil {
		t.Fatalf("choose healthy VM: %v", err)
	}
	if c2.host != "mac-healthy" {
		t.Fatalf("chose %s, want mac-healthy", c2.host)
	}
	for _, pt := range c2.parts(p) {
		if pt.Name == "holding" || pt.Name == "host-cpu-starvation" {
			t.Errorf("healthy VM unexpectedly got penalty part %+v", pt)
		}
	}
}
