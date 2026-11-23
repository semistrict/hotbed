package controller

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/tailnet"
)

// fakeNow is a clock tests move by hand.
type fakeNow struct{ t time.Time }

func (c *fakeNow) now() time.Time          { return c.t }
func (c *fakeNow) advance(d time.Duration) { c.t = c.t.Add(d) }

// seen is the node as last heard from at the clock's time.
func (c *fakeNow) seen() *pb.Node {
	ms := c.t.UnixMilli()
	return &pb.Node{Hostname: "mac-worker", LastSeen: &ms}
}

func TestStandbySleepsAnIdleNodeAndWakesItForARun(t *testing.T) {
	clock := &fakeNow{t: time.UnixMilli(1_800_000_000_000)}
	s := &standby{now: clock.now}
	release := s.hold("mac-worker", true, 30*time.Minute)
	clock.advance(time.Second)
	node := clock.seen()
	settle := func(w work) (bool, string) { return s.settle("mac-worker", node.LastSeen, w) }

	if s.asleep(node) {
		t.Fatal("a running node heard from since its host said so is asleep")
	}
	if run, why := settle(work{}); !run {
		t.Fatalf("idle for a second, it sleeps (%s)", why)
	}

	// A session's command keeps it awake well past sleepAfter, and a
	// sleepAfter is counted from when it last had work.
	clock.advance(20 * time.Minute)
	settle(work{working: true, busy: true})
	clock.advance(29 * time.Minute)
	if run, why := settle(work{}); !run {
		t.Fatalf("29 minutes after its last work it sleeps (%s), want awake until 30", why)
	}
	// Busy with something not work (a base freshened on its own), it waits
	// to finish, but the clock runs on.
	clock.advance(2 * time.Minute)
	if run, why := settle(work{busy: true}); !run {
		t.Fatalf("it sleeps while busy (%s)", why)
	}
	if run, why := settle(work{}); run || why != "no work for 30m0s" {
		t.Fatalf("31 minutes without work, done with what it was busy with: run %v (%s), want false (no work for 30m0s)", run, why)
	}
	if !s.asleep(node) {
		t.Fatal("put to sleep, it is not asleep")
	}

	// Its host stops it and says so; a run wakes it.
	release()
	release = s.hold("mac-worker", false, 30*time.Minute)
	clock.advance(time.Minute)
	if run, _ := settle(work{}); run {
		t.Fatal("asleep, it wakes with no run placed on it")
	}
	s.wake("mac-worker")
	if run, _ := settle(work{}); !run {
		t.Fatal("woken, it is not to run")
	}
	// Until its host says it runs, and it is heard from since, it is still
	// asleep: booting.
	if !s.asleep(clock.seen()) {
		t.Fatal("woken but not yet started, it is awake")
	}
	release()
	release = s.hold("mac-worker", true, 30*time.Minute)
	defer release()
	if !s.asleep(node) {
		t.Fatal("started but not heard from since, it is awake")
	}
	clock.advance(time.Second)
	node = clock.seen()
	if s.asleep(node) {
		t.Fatal("started and heard from, it is asleep")
	}
	// Booting counts as work: woken, it stays a whole sleepAfter.
	clock.advance(29 * time.Minute)
	if run, why := settle(work{}); !run {
		t.Fatalf("29 minutes after it woke it sleeps (%s)", why)
	}
}

// A node running when its host first calls (a new controller, or a new
// supervisor) serves on: its heartbeats from before the call count.
func TestStandbyRunningNodeFirstKnownIsAwake(t *testing.T) {
	clock := &fakeNow{t: time.UnixMilli(1_800_000_000_000)}
	s := &standby{now: clock.now}
	node := clock.seen()
	clock.advance(time.Second)
	defer s.hold("mac-worker", true, 30*time.Minute)()
	if s.asleep(node) {
		t.Fatal("running when first known, heard from a second before, it is asleep")
	}
}

func TestStandbyNodeStartedByHandStaysUp(t *testing.T) {
	clock := &fakeNow{t: time.UnixMilli(1_800_000_000_000)}
	s := &standby{now: clock.now}
	release := s.hold("mac-worker", false, 30*time.Minute)
	if run, _ := s.settle("mac-worker", nil, work{}); run {
		t.Fatal("first known stopped, it is to run")
	}
	release()
	// hotbed worker start: its host says it runs, though not asked to.
	clock.advance(time.Hour)
	release = s.hold("mac-worker", true, 30*time.Minute)
	defer release()
	clock.advance(time.Second)
	node := clock.seen()
	clock.advance(10 * time.Minute)
	if run, why := s.settle("mac-worker", node.LastSeen, work{}); !run {
		t.Fatalf("started by hand 10 minutes ago, it sleeps (%s)", why)
	}
}

func TestStandbyNeverSleepsWithNoSleepAfter(t *testing.T) {
	clock := &fakeNow{t: time.UnixMilli(1_800_000_000_000)}
	s := &standby{now: clock.now}
	defer s.hold("mac-worker", false, 0)()
	if run, why := s.settle("mac-worker", nil, work{}); !run || why != "it never sleeps" {
		t.Fatalf("run %v (%s), want true (it never sleeps)", run, why)
	}
}

func TestStandbyUnsupervisedNodeIsNotAsleep(t *testing.T) {
	clock := &fakeNow{t: time.UnixMilli(1_800_000_000_000)}
	s := &standby{now: clock.now}
	if s.asleep(clock.seen()) {
		t.Fatal("a node no host supervises is asleep")
	}
	s.hold("mac-worker", false, time.Minute)()
	if !s.asleep(clock.seen()) {
		t.Fatal("stopped, with its host's call just ended, it is not asleep")
	}
	clock.advance(standbyGrace)
	if s.asleep(clock.seen()) {
		t.Fatal("its host gone for standbyGrace, it is still asleep: it is offline")
	}
}

// A run placed on a node while its VM powers off waits for it to stop and
// start again: still heard from, it is not awake.
func TestStandbyNodeWokenWhileStoppingIsNotAwakeUntilRestarted(t *testing.T) {
	clock := &fakeNow{t: time.UnixMilli(1_800_000_000_000)}
	s := &standby{now: clock.now}
	release := s.hold("mac-worker", true, time.Minute)
	clock.advance(2 * time.Minute)
	node := clock.seen()
	if run, _ := s.settle("mac-worker", node.LastSeen, work{}); run {
		t.Fatal("idle past sleepAfter, it is to run")
	}
	// Its host is powering it off; its worker still beats.
	s.wake("mac-worker")
	clock.advance(time.Second)
	node = clock.seen()
	if !s.asleep(node) {
		t.Fatal("woken while powering off, it is awake: a run would go to a dying VM")
	}
	if run, why := s.settle("mac-worker", node.LastSeen, work{}); !run || why != "busy" {
		t.Fatalf("woken while powering off: run %v (%s), want true (busy)", run, why)
	}
	// Stopped, its host calls again; then starts it and calls again.
	release()
	s.hold("mac-worker", false, time.Minute)()
	clock.advance(time.Second)
	release = s.hold("mac-worker", true, time.Minute)
	defer release()
	if !s.asleep(clock.seen()) {
		t.Fatal("restarted, not heard from since, it is awake")
	}
	clock.advance(time.Second)
	if s.asleep(clock.seen()) {
		t.Fatal("restarted and heard from, it is asleep")
	}
}

// A node woken that does not come up within bootTimeout goes back to sleep
// and takes no runs for wakeFailedHold.
func TestStandbyNodeThatDoesNotWakeTakesNoRuns(t *testing.T) {
	clock := &fakeNow{t: time.UnixMilli(1_800_000_000_000)}
	s := &standby{now: clock.now}
	defer s.hold("mac-worker", false, 30*time.Minute)()
	node := &pb.Node{Hostname: "mac-worker"}
	s.wake("mac-worker")
	clock.advance(bootTimeout - time.Second)
	if run, _ := s.settle("mac-worker", nil, work{}); !run || s.wakeFailed(node) {
		t.Fatal("still within bootTimeout, it gave up waking")
	}
	clock.advance(time.Second)
	if run, why := s.settle("mac-worker", nil, work{}); run || why != "it did not wake within 3m0s" {
		t.Fatalf("run %v (%s), want false (it did not wake within 3m0s)", run, why)
	}
	if !s.wakeFailed(node) {
		t.Fatal("it did not wake, and takes runs")
	}
	clock.advance(wakeFailedHold)
	if s.wakeFailed(node) {
		t.Fatal("wakeFailedHold on, it still takes no runs")
	}
}

// A node wanted all along whose VM crashed and was started again, but never
// comes up, times out like one woken for a run: bootTimeout holds however it
// came to be booting.
func TestStandbyNodeRestartedAfterACrashTimesOut(t *testing.T) {
	clock := &fakeNow{t: time.UnixMilli(1_800_000_000_000)}
	s := &standby{now: clock.now}
	s.hold("mac-worker", true, 0)()
	clock.advance(time.Second)
	up := clock.seen()
	if run, _ := s.settle("mac-worker", up.LastSeen, work{}); !run || s.asleep(up) {
		t.Fatal("running and heard from, it is not up")
	}
	// It crashed; its host says so, then starts it again; it never beats.
	s.hold("mac-worker", false, 0)()
	clock.advance(time.Second)
	defer s.hold("mac-worker", true, 0)()
	if run, _ := s.settle("mac-worker", up.LastSeen, work{}); !run {
		t.Fatal("restarted, it is not to run")
	}
	clock.advance(bootTimeout)
	s.settle("mac-worker", up.LastSeen, work{})
	if !s.wakeFailed(up) {
		t.Fatal("restarted bootTimeout ago and never heard from, it still takes runs")
	}
}

// A new controller told a node runs does not take a heartbeat from before
// it last slept as word that it is up.
func TestStandbyNodeFirstKnownBootingIsAsleep(t *testing.T) {
	clock := &fakeNow{t: time.UnixMilli(1_800_000_000_000)}
	s := &standby{now: clock.now}
	beforeSleep := clock.seen()
	clock.advance(time.Hour)
	defer s.hold("mac-worker", true, 30*time.Minute)()
	if !s.asleep(beforeSleep) {
		t.Fatal("first known running, heard from only an hour ago, it is awake")
	}
}

// A view snapshot taken while a command was still running must not stamp
// p.worked with a later clock reading after the work ended and time advanced
// (board 9357), nor move p.worked backwards past a newer wake.
func TestStandbyStaleSettleDoesNotResetWorked(t *testing.T) {
	clock := &fakeNow{t: time.UnixMilli(1_800_000_000_000)}
	s := &standby{now: clock.now}
	defer s.hold("mac-worker", true, 30*time.Minute)()
	clock.advance(time.Second)
	node := clock.seen()
	snapTime := clock.now()

	// Time advances past sleepAfter before the earlier view settles.
	clock.advance(31 * time.Minute)
	if run, _ := s.settleAt("mac-worker", node.LastSeen, work{working: true, busy: true}, snapTime); !run {
		t.Fatal("stale busy snapshot put the node to sleep")
	}
	if run, why := s.settle("mac-worker", node.LastSeen, work{}); run || why != "no work for 30m0s" {
		t.Fatalf("after stale busy snapshot settled at snapTime: run %v (%s), want false (no work for 30m0s)", run, why)
	}

	// A stale snapshot must not pull p.worked backwards past a newer wake either.
	s.wake("mac-worker")
	s.settleAt("mac-worker", node.LastSeen, work{working: true, busy: true}, snapTime)
	if got := s.nodes["mac-worker"].worked; !got.Equal(clock.now()) {
		t.Fatalf("worked = %v, want %v (must not move backwards)", got, clock.now())
	}
}

// Server.settlePower must read the clock before reading views: if a command
// finishes and sleepAfter elapses while settlePower is reading a busy view,
// stamping p.worked with a post-views clock reading would postpone sleep
// (board 9590).
func TestServerSettlePowerCapturesClockBeforeViews(t *testing.T) {
	ctx := t.Context()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "hotbed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	clock := &fakeNow{t: time.UnixMilli(1_800_000_000_000)}
	if _, err := store.Join(ctx, tailnet.Peer{NodeID: "n1", Hostname: "mac-worker", Login: "ramon@"}, clock.now()); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(ServerConfig{Store: store, Now: clock.now})
	defer srv.power.hold("mac-worker", true, 30*time.Minute)()

	clock.advance(time.Second)
	if err := store.Beat(ctx, "n1", &pb.HeartbeatRequest{}, clock.now()); err != nil {
		t.Fatal(err)
	}
	if err := putState(ctx, store.q, "n1", "command", "cmd-1", &pb.Command{
		Id:        "cmd-1",
		SessionId: "s1",
		State:     pb.CommandState_COMMAND_STATE_RUNNING,
	}); err != nil {
		t.Fatal(err)
	}

	advanced := false
	srv.power.views = func(ctx context.Context) ([]nodeView, error) {
		nvs, err := srv.views(ctx)
		if err != nil {
			return nil, err
		}
		if !advanced {
			advanced = true
			// The command finishes and sleepAfter elapses before settlePower
			// finishes with the busy snapshot it just read.
			if err := dropState(ctx, store.q, "n1", "command", "cmd-1"); err != nil {
				return nil, err
			}
			clock.advance(31 * time.Minute)
		}
		return nvs, nil
	}

	run, why, err := srv.settlePower(ctx, "mac-worker")
	if err != nil {
		t.Fatalf("first settlePower: %v", err)
	}
	if !advanced {
		t.Fatal("views hook did not run")
	}
	if !run {
		t.Fatalf("first settlePower with busy view: run = false (%s), want true", why)
	}

	// Next settlePower sees the idle view after sleepAfter has elapsed: it must
	// put the node to sleep rather than treating the stale busy view as work at
	// the post-advance clock time.
	run, why, err = srv.settlePower(ctx, "mac-worker")
	if err != nil {
		t.Fatalf("second settlePower: %v", err)
	}
	if run || why != "no work for 30m0s" {
		t.Fatalf("second settlePower after idle view: run = %v (%s), want false (no work for 30m0s)", run, why)
	}
}

func TestWorkOf(t *testing.T) {
	live := pb.CommandState_COMMAND_STATE_RUNNING
	for _, tc := range []struct {
		name string
		nv   nodeView
		want work
	}{
		{name: "nothing", want: work{}},
		{
			name: "a session's command",
			nv:   nodeView{view: &view{commands: []*pb.Command{{State: live, SessionId: "s"}}}},
			want: work{working: true, busy: true},
		},
		{
			name: "a base freshened on its own",
			nv:   nodeView{view: &view{commands: []*pb.Command{{State: live, Kind: "freshen"}}}},
			want: work{busy: true},
		},
		{
			name: "an ended command",
			nv:   nodeView{view: &view{commands: []*pb.Command{{State: pb.CommandState_COMMAND_STATE_SUCCEEDED, SessionId: "s"}}}},
			want: work{},
		},
		{
			name: "a base building",
			nv:   nodeView{view: &view{bases: []*pb.Base{{State: pb.BaseState_BASE_STATE_BUILDING}}}},
			want: work{busy: true},
		},
		{
			name: "a workspace being made",
			nv:   nodeView{view: &view{workspaces: []*pb.Workspace{{State: pb.WorkspaceState_WORKSPACE_STATE_PREPARING}}}},
			want: work{working: true, busy: true},
		},
		{
			name: "clients using it directly",
			nv:   nodeView{online: true, nodeRow: nodeRow{hb: &pb.HeartbeatRequest{ActiveSessions: []string{"s"}}}, view: &view{}},
			want: work{working: true, busy: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.nv.view == nil {
				tc.nv.view = &view{}
			}
			if got := workOf(tc.nv); got != tc.want {
				t.Errorf("workOf = %+v, want %+v", got, tc.want)
			}
		})
	}
}
