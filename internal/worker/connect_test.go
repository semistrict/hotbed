package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/update"
)

// sizedEvents are events whose feed text makes each encode to about kb KiB.
func sizedEvents(kb ...int) []*pb.Event {
	var out []*pb.Event
	for i, n := range kb {
		out = append(out, &pb.Event{Seq: int64(i + 1), Feed: &pb.WorkerEvent{Text: strings.Repeat("x", n<<10)}})
	}
	return out
}

func seqs(events []*pb.Event) []int64 {
	var out []int64
	for _, ev := range events {
		out = append(out, ev.Seq)
	}
	return out
}

// A push carries as many events as fit pushBytes, however few that is, and
// never none: several finished commands' profiles together outgrow what the
// controller reads in one message.
func TestPushesFitTheirEventsInPushBytes(t *testing.T) {
	events := sizedEvents(900, 900, 900, 10)
	if got := seqs(firstFitting(events, pushBytes)); !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("first fitting = %v, want [1 2]", got)
	}
	if got := seqs(firstFitting(sizedEvents(3000, 10), pushBytes)); !slices.Equal(got, []int64{1}) {
		t.Fatalf("first fitting of an event over budget = %v, want it alone", got)
	}
	if got := seqs(firstFitting(sizedEvents(1, 1, 1), pushBytes)); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Fatalf("first fitting of small events = %v, want all", got)
	}
	total := 0
	for _, ev := range firstFitting(events, pushBytes) {
		total += proto.Size(ev)
	}
	if total > pushBytes {
		t.Fatalf("push of %d bytes, over %d", total, pushBytes)
	}

	// A checkpoint brings along the newest of the events it includes that
	// fit beside it, which may be none.
	if got := seqs(lastFitting(events, pushBytes)); !slices.Equal(got, []int64{2, 3, 4}) {
		t.Fatalf("last fitting = %v, want [2 3 4]", got)
	}
	if got := seqs(lastFitting(events, 0)); len(got) != 0 {
		t.Fatalf("last fitting in no room = %v, want none", got)
	}
}

type sizeLimitedCluster struct {
	hotbedv1connect.ClusterServiceClient
	maxBytes    int
	applied     int64
	epoch       string
	pushed      []*pb.Event
	checkpoints int
}

func (c *sizeLimitedCluster) PushEvents(_ context.Context, req *pb.PushEventsRequest) (*pb.PushEventsResponse, error) {
	if proto.Size(req) > c.maxBytes {
		return nil, connect.NewError(connect.CodeResourceExhausted, "message is larger than configured max")
	}
	if req.Checkpoint == nil && req.Epoch != c.epoch {
		return &pb.PushEventsResponse{Applied: c.applied, CheckpointNeeded: true}, nil
	}
	if req.Checkpoint != nil {
		c.epoch = req.Epoch
		c.applied = req.Checkpoint.Seq
		c.checkpoints++
		return &pb.PushEventsResponse{Applied: c.applied}, nil
	}
	for _, ev := range req.Events {
		if ev.Seq > c.applied {
			c.pushed = append(c.pushed, ev)
			c.applied = ev.Seq
		}
	}
	return &pb.PushEventsResponse{Applied: c.applied}, nil
}

// A single oversized event must never wedge a worker's event stream: optional
// profiles are shed first, and if a single event is still refused as
// resource_exhausted, the pusher sends a checkpoint to skip past it (board 7232).
func TestPusherNeverWedgesOnOversizedEvent(t *testing.T) {
	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	eng, err := NewEngine(t.Context(), EngineConfig{
		Dir: dir, NodeID: "n1", Host: "asahimbp",
		Store: store, BaseImage: fake.BaseImage(store), Units: &fake.Units{}, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })

	// 1. An oversized event carrying a profile (> pushBytes) sheds its profile
	// and delivers the command state change without wedging.
	bigFrame := strings.Repeat("f", pushBytes+64<<10)
	cmd1 := &pb.Command{Id: "c1", State: pb.CommandState_COMMAND_STATE_FAILED}
	if err := eng.db.commit("command", "c1", &pb.CommandRecord{Command: cmd1}, &pb.Event{
		At: 1001,
		Change: &pb.Event_Command{Command: &pb.CommandChange{
			What:    "exited",
			Command: cmd1,
			Profile: &pb.CommandProfile{CommandId: "c1", Frames: []*pb.ProfileFrame{{Name: bigFrame}}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	// 2. An oversized event without a droppable profile (e.g. giant feed text)
	// that the controller rejects with ResourceExhausted triggers a checkpoint.
	cmd2 := &pb.Command{Id: "c2", State: pb.CommandState_COMMAND_STATE_CANCELLED}
	if err := eng.db.commit("command", "c2", &pb.CommandRecord{Command: cmd2}, &pb.Event{
		At:     1002,
		Feed:   &pb.WorkerEvent{At: 1002, Kind: "fail", Text: strings.Repeat("z", 64<<10)},
		Change: &pb.Event_Command{Command: &pb.CommandChange{What: "cancelled", Command: cmd2}},
	}); err != nil {
		t.Fatal(err)
	}
	// 3. A normal event after both must still be delivered.
	cmd3 := &pb.Command{Id: "c3", State: pb.CommandState_COMMAND_STATE_SUCCEEDED}
	if err := eng.db.commit("command", "c3", &pb.CommandRecord{Command: cmd3}, &pb.Event{
		At:     1003,
		Change: &pb.Event_Command{Command: &pb.CommandChange{What: "exited", Command: cmd3}},
	}); err != nil {
		t.Fatal(err)
	}

	cluster := &sizeLimitedCluster{maxBytes: 16 << 10}
	p := &pusher{eng: eng, cluster: cluster, clusterID: "cl"}
	// Establish epoch with empty checkpoint at seq 0 so the pusher walks events 1, 2, 3.
	cluster.epoch = eng.Epoch()
	p.known = true
	p.applied = 0

	for range 10 {
		more, err := p.once(t.Context())
		if err != nil {
			t.Fatalf("pusher.once failed: %v", err)
		}
		if !more {
			break
		}
	}
	if p.applied != 3 {
		t.Fatalf("applied = %d, want 3 (stream must advance past oversized events)", p.applied)
	}
	if cluster.checkpoints == 0 {
		t.Errorf("expected checkpoint fallback for unpushable event 2")
	}
	if len(cluster.pushed) == 0 || cluster.pushed[0].GetCommand().GetCommand().GetId() != "c1" || cluster.pushed[0].GetCommand().GetProfile() != nil {
		t.Errorf("event 1 should have been pushed with profile shed; pushed=%v", cluster.pushed)
	}
}

type flakyCheckpointCluster struct {
	hotbedv1connect.ClusterServiceClient
	failNext    int
	checkpoints []*pb.Checkpoint
	events      [][]*pb.Event
	onSuccess   func()
}

func (c *flakyCheckpointCluster) PushEvents(_ context.Context, req *pb.PushEventsRequest) (*pb.PushEventsResponse, error) {
	if req.Checkpoint != nil {
		if c.failNext > 0 {
			c.failNext--
			return nil, connect.NewError(connect.CodeUnavailable, "transient push failure")
		}
		c.checkpoints = append(c.checkpoints, proto.Clone(req.Checkpoint).(*pb.Checkpoint))
		evs := make([]*pb.Event, 0, len(req.Events))
		for _, ev := range req.Events {
			evs = append(evs, proto.Clone(ev).(*pb.Event))
		}
		c.events = append(c.events, evs)
		if c.onSuccess != nil {
			fn := c.onSuccess
			c.onSuccess = nil
			fn()
		}
		return &pb.PushEventsResponse{Applied: req.Checkpoint.Seq}, nil
	}
	return &pb.PushEventsResponse{}, nil
}

// When a DB save fails, the resulting unsaved in-memory checkpoint must stay
// dirty until PushEvents succeeds, and a concurrent save failure while a push
// is in flight must trigger a follow-up checkpoint (board 7888).
func TestUnsavedCheckpointRetriedAfterPushFailure(t *testing.T) {
	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	eng, err := NewEngine(t.Context(), EngineConfig{
		Dir: dir, NodeID: "n1", Host: "asahimbp",
		Store: store, BaseImage: fake.BaseImage(store), Units: &fake.Units{}, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })

	// Force SQLite writes to fail (simulating a full disk) while reads still work.
	if _, err := eng.db.sql.Exec(`PRAGMA query_only = ON`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = eng.db.sql.Exec(`PRAGMA query_only = OFF`) })

	w := &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{Id: "ws1", Project: "app", State: idle},
		Volume:    "ws/ws1",
	}}
	eng.mu.Lock()
	eng.workspaces["ws1"] = w
	eng.saveWorkspace(w, change{what: "created"})
	dirty := eng.isDirty()
	eng.mu.Unlock()
	if !dirty {
		t.Fatal("expected engine to be dirty after failed DB save")
	}

	cluster := &flakyCheckpointCluster{failNext: 1}
	p := &pusher{eng: eng, cluster: cluster, clusterID: "cl", known: true}

	// First push fails transiently: engine must stay dirty and retry.
	if _, err := p.once(t.Context()); err == nil {
		t.Fatal("first push succeeded, want transient failure")
	}
	eng.mu.Lock()
	stillDirty := eng.isDirty()
	eng.mu.Unlock()
	if !stillDirty {
		t.Fatal("dirty cleared before PushEvents succeeded")
	}

	// Simulate a second save failure arriving while the retry push is in flight.
	cluster.onSuccess = func() {
		w2 := &workspace{rec: &pb.WorkspaceRecord{
			Workspace: &pb.Workspace{Id: "ws2", Project: "app", State: idle},
			Volume:    "ws/ws2",
		}}
		eng.mu.Lock()
		eng.workspaces["ws2"] = w2
		eng.saveWorkspace(w2, change{what: "created"})
		eng.mu.Unlock()
	}

	// Second push delivers ws1; because ws2 failed to save concurrently, engine remains dirty.
	more, err := p.once(t.Context())
	if err != nil || !more {
		t.Fatalf("retry push = (%v, %v), want (true, nil)", more, err)
	}
	if len(cluster.checkpoints) != 1 || len(cluster.checkpoints[0].Workspaces) != 1 || cluster.checkpoints[0].Workspaces[0].Id != "ws1" {
		t.Fatalf("first delivered checkpoint = %+v, want [ws1]", cluster.checkpoints)
	}
	eng.mu.Lock()
	dirtyAfterConcurrent := eng.isDirty()
	eng.mu.Unlock()
	if !dirtyAfterConcurrent {
		t.Fatal("concurrent dirty lost when earlier generation finished pushing")
	}

	// Third push delivers both ws1 and ws2 and finally clears dirty.
	if _, err := p.once(t.Context()); err != nil {
		t.Fatalf("third push failed: %v", err)
	}
	if len(cluster.checkpoints) != 2 || len(cluster.checkpoints[1].Workspaces) != 2 {
		t.Fatalf("second delivered checkpoint = %+v, want 2 workspaces", cluster.checkpoints)
	}
	eng.mu.Lock()
	cleanAtEnd := !eng.isDirty()
	eng.mu.Unlock()
	if !cleanAtEnd {
		t.Fatal("engine still dirty after latest generation was pushed")
	}
}

// When a durable finished-command event (history + profile) commits after an
// earlier DB write failure and before the unsaved memory checkpoint retry,
// the memory checkpoint must still attach that pending durable event (Finding #8);
// and once markSaved clears isDirty, a subsequent controller-requested
// Checkpoint() must still read from live in-memory state so the unsaved
// workspace is never dropped by stale SQLite (Finding #9).
func TestUnsavedMemoryCheckpointPreservesDurableEventsAndFollowupCheckpoints(t *testing.T) {
	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	eng, err := NewEngine(t.Context(), EngineConfig{
		Dir: dir, NodeID: "n1", Host: "asahimbp",
		Store: store, BaseImage: fake.BaseImage(store), Units: &fake.Units{}, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })

	// 1. Force SQLite writes to fail so ws-unsaved exists only in memory.
	if _, err := eng.db.sql.Exec(`PRAGMA query_only = ON`); err != nil {
		t.Fatal(err)
	}
	ws := &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{Id: "ws-unsaved", Project: "app", State: idle},
		Volume:    "ws/ws-unsaved",
	}}
	eng.mu.Lock()
	eng.workspaces["ws-unsaved"] = ws
	eng.saveWorkspace(ws, change{what: "created"})
	eng.mu.Unlock()

	// 2. Disk recovers: a finished command commits durably before the unsaved
	// memory checkpoint is pushed.
	if _, err := eng.db.sql.Exec(`PRAGMA query_only = OFF`); err != nil {
		t.Fatal(err)
	}
	cmd := &command{rec: &pb.CommandRecord{
		Command: &pb.Command{Id: "c-fin", WorkspaceId: "ws-unsaved", State: succeeded},
	}}
	fin := &pb.FinishedCommand{Id: "c-fin", WorkspaceId: "ws-unsaved", State: succeeded}
	prof := &pb.CommandProfile{CommandId: "c-fin", TotalSamples: 42}
	eng.mu.Lock()
	eng.commands["c-fin"] = cmd
	eng.saveCommand(cmd, change{what: "exited", finished: fin, profile: prof})
	eng.mu.Unlock()

	cluster := &flakyCheckpointCluster{}
	p := &pusher{eng: eng, cluster: cluster, clusterID: "cl", known: true, applied: 0}

	// Push the unsaved memory checkpoint: it must carry ws-unsaved AND attach
	// the durable finished-command event (with Finished + Profile).
	if _, err := p.once(t.Context()); err != nil {
		t.Fatalf("unsaved checkpoint push failed: %v", err)
	}
	if len(cluster.checkpoints) != 1 {
		t.Fatalf("checkpoints pushed = %d, want 1", len(cluster.checkpoints))
	}
	if len(cluster.events) != 1 || len(cluster.events[0]) != 1 {
		t.Fatalf("events attached to memory checkpoint = %+v, want 1 durable event", cluster.events)
	}
	gotEv := cluster.events[0][0]
	if gotEv.GetCommand().GetFinished().GetId() != "c-fin" || gotEv.GetCommand().GetProfile().GetTotalSamples() != 42 {
		t.Fatalf("attached durable event = %+v, want finished c-fin with profile", gotEv)
	}

	// 3. Engine is now clean (!isDirty), but dirtyGen > 0 because SQLite still
	// lacks ws-unsaved. A subsequent controller-requested checkpoint must still
	// include ws-unsaved rather than reading stale SQLite.
	p.checkpoint = true
	if _, err := p.once(t.Context()); err != nil {
		t.Fatalf("controller-requested checkpoint push failed: %v", err)
	}
	if len(cluster.checkpoints) != 2 {
		t.Fatalf("checkpoints pushed = %d, want 2", len(cluster.checkpoints))
	}
	foundWS := false
	for _, w := range cluster.checkpoints[1].Workspaces {
		if w.Id == "ws-unsaved" {
			foundWS = true
		}
	}
	if !foundWS {
		t.Fatalf("follow-up checkpoint dropped live unsaved workspace: %+v", cluster.checkpoints[1].Workspaces)
	}
}

// Sample() updates in-memory telemetry sent via Heartbeat; when admission hold
// state does not change, it must not redundantly wake the event pusher (board 7888).
func TestSampleDoesNotRedundantlyKickEventPusher(t *testing.T) {
	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	eng, err := NewEngine(t.Context(), EngineConfig{
		Dir: dir, NodeID: "n1", Host: "asahimbp",
		Store: store, BaseImage: fake.BaseImage(store), Units: &fake.Units{}, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })

	// Drain any prior kick.
	select {
	case <-eng.Kicked():
	default:
	}

	eng.Sample()

	select {
	case <-eng.Kicked():
		t.Fatal("Sample() kicked event pusher even though no event was written")
	default:
	}
}

// startFreshen must wake the engine loop via stepSoon() so freshens start
// immediately rather than waiting up to 1s for the next tick (board 7888).
func TestStartFreshenWakesStepSoon(t *testing.T) {
	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	eng, err := NewEngine(t.Context(), EngineConfig{
		Dir: dir, NodeID: "n1", Host: "asahimbp",
		Store: store, BaseImage: fake.BaseImage(store), Units: &fake.Units{}, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })

	// Drain stepc.
	select {
	case <-eng.stepc:
	default:
	}

	p := &pb.HostedProject{Name: "app"}
	if _, err := eng.startFreshen(t.Context(), p, 1, nil, "no_previous_base", heads{}, "r1"); err != nil {
		t.Fatal(err)
	}

	select {
	case <-eng.stepc:
	default:
		t.Fatal("startFreshen did not signal stepc via stepSoon()")
	}
}

// While keep() is backing off after a push failure, local kicks must not wake
// it immediately — otherwise a burst of events while the controller is down
// drives failures straight to the 30s backoff cap (board 8280).
func TestKeepIgnoresKicksDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	kicked := make(chan struct{}, 1)
	var calls atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- keep(ctx, "test", kicked, nil, nil, func(context.Context) (bool, error) {
			calls.Add(1)
			return false, errors.New("controller down")
		})
	}()

	// Wait for the initial failing call, then send rapid kicks well within the 1s backoff.
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatalf("initial calls = %d, want 1", calls.Load())
	}
	for range 10 {
		select {
		case kicked <- struct{}{}:
		default:
		}
		time.Sleep(15 * time.Millisecond)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("calls during 1s backoff under rapid kicks = %d, want 1", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("keep returned %v, want nil on cancel", err)
	}
}

// When one loop in Connect succeeds after the controller returns, its peer
// sleeping in failure backoff wakes immediately rather than waiting out an
// independent backoff timer, while healthy loops never ping-pong (boards 7927, 7929).
func TestKeepPeerRecoveryWakesBackoffWithoutPingPong(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	wake := make(chan struct{}, 1)
	var fail atomic.Bool
	fail.Store(true)
	var calls atomic.Int32
	recovered := make(chan struct{}, 1)
	done := make(chan error, 1)

	go func() {
		done <- keep(ctx, "heartbeat", nil, wake, nil, func(context.Context) (bool, error) {
			calls.Add(1)
			if fail.Load() {
				return false, errors.New("controller down")
			}
			select {
			case recovered <- struct{}{}:
			default:
			}
			return false, nil
		})
	}()

	// Let it fail twice so it enters a 2s backoff sleep.
	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() < 2 {
		t.Fatalf("calls = %d, want >= 2", calls.Load())
	}

	// Controller comes back and sibling loop signals peer recovery: must wake
	// well before the 2s backoff expires.
	fail.Store(false)
	start := time.Now()
	wake <- struct{}{}
	select {
	case <-recovered:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("peer did not wake from backoff within 500ms")
	}
	if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
		t.Errorf("peer took %v to wake from backoff", elapsed)
	}

	// Once healthy (failures == 0), further peer wake signals must be ignored
	// (no ping-pong loop).
	afterRecover := calls.Load()
	for range 3 {
		select {
		case wake <- struct{}{}:
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := calls.Load(); got != afterRecover {
		t.Errorf("healthy loop ping-ponged on wake signals: calls %d -> %d", afterRecover, got)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("keep returned %v, want nil on cancel", err)
	}
}

type buildHeartbeatCluster struct {
	hotbedv1connect.ClusterServiceClient
	build *pb.Build
}

func (c *buildHeartbeatCluster) Heartbeat(_ context.Context, _ *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	return &pb.HeartbeatResponse{Build: c.build}, nil
}

// A busy worker defers installing a newer controller build and returning
// update.ErrRestart until active commands (running, queued, parked),
// interactive shells, and building bases finish, up to Follow.MaxDefer; paused
// commands and long-lived server commands (Kind == "server") do not block
// upgrades indefinitely (board 7989).
func TestHeartbeatDefersUpgradeDuringActiveWork(t *testing.T) {
	payloadFor := func(text string) ([]byte, string) {
		b := []byte(text)
		s := sha256.Sum256(b)
		return b, hex.EncodeToString(s[:])
	}
	payload9, sum9 := payloadFor("hotbed build 9 binary")
	payload10, sum10 := payloadFor("hotbed build 10 binary")
	buildPath9 := "/builds/9/hotbed-linux-amd64"
	buildPath10 := "/builds/10/hotbed-linux-amd64"

	var downloads atomic.Int32
	var hookMu sync.Mutex
	var onDownload func()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload []byte
		switch r.URL.Path {
		case buildPath9:
			payload = payload9
		case buildPath10:
			payload = payload10
		default:
			http.NotFound(w, r)
			return
		}
		downloads.Add(1)
		hookMu.Lock()
		fn := onDownload
		onDownload = nil
		hookMu.Unlock()
		if fn != nil {
			fn()
		}
		w.Write(payload)
	}))
	defer srv.Close()

	newEngine := func(t *testing.T) (*Engine, *Follow, string, *buildHeartbeatCluster) {
		t.Helper()
		dir := t.TempDir()
		store := &fake.Store{Root: filepath.Join(dir, "volumes")}
		eng, err := NewEngine(t.Context(), EngineConfig{
			Dir: dir, NodeID: "n1", Host: "asahimbp",
			Store: store, BaseImage: fake.BaseImage(store), Units: &fake.Units{}, System: fake.NewSystem(),
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { eng.Close() })
		exe := filepath.Join(t.TempDir(), "hotbed")
		if err := os.WriteFile(exe, []byte("old build 8"), 0o755); err != nil {
			t.Fatal(err)
		}
		follow := &Follow{URL: srv.URL, Client: srv.Client(), Exe: exe, Build: 8}
		cl := &buildHeartbeatCluster{build: &pb.Build{Number: 9, Sha256: sum9, Path: buildPath9}}
		return eng, follow, exe, cl
	}

	readExe := func(t *testing.T, exe string) string {
		t.Helper()
		b, err := os.ReadFile(exe)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	t.Run("running command defers upgrade until it finishes", func(t *testing.T) {
		eng, follow, exe, cl := newEngine(t)
		beforeDL := downloads.Load()
		cmd := &command{rec: &pb.CommandRecord{Command: &pb.Command{Id: "c-run", State: running, Limits: &pb.CommandLimits{}}}}
		eng.mu.Lock()
		eng.commands["c-run"] = cmd
		eng.mu.Unlock()

		if err := HeartbeatOnce(t.Context(), eng, cl, "cl", follow); err != nil {
			t.Fatalf("HeartbeatOnce with running command = %v, want nil", err)
		}
		if got := readExe(t, exe); got != "old build 8" {
			t.Fatalf("exe overwritten while command running: %q", got)
		}
		if downloads.Load() != beforeDL {
			t.Fatalf("binary downloaded while command running")
		}
		if got := follow.deferred(); got != 9 {
			t.Fatalf("follow.deferred() = %d, want 9", got)
		}

		// Once the command finishes, the next heartbeat installs and requests restart.
		eng.mu.Lock()
		cmd.rec.Command.State = succeeded
		eng.mu.Unlock()

		err := HeartbeatOnce(t.Context(), eng, cl, "cl", follow)
		if !errors.Is(err, update.ErrRestart) {
			t.Fatalf("HeartbeatOnce after command finished = %v, want update.ErrRestart", err)
		}
		if got := readExe(t, exe); got != string(payload9) {
			t.Fatalf("exe after upgrade = %q, want %q", got, string(payload9))
		}
		if got := follow.deferred(); got != 0 {
			t.Fatalf("follow.deferred() after upgrade = %d, want 0", got)
		}
	})

	t.Run("queued, parked, and building base defer upgrade", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			setup func(*Engine)
		}{
			{"queued command", func(e *Engine) {
				e.commands["c-q"] = &command{rec: &pb.CommandRecord{Command: &pb.Command{Id: "c-q", State: queued, Limits: &pb.CommandLimits{}}}}
			}},
			{"parked command", func(e *Engine) {
				e.commands["c-p"] = &command{rec: &pb.CommandRecord{Command: &pb.Command{Id: "c-p", State: parked, Limits: &pb.CommandLimits{}}}}
			}},
			{"building base", func(e *Engine) {
				e.bases["b-1"] = &pb.Base{Id: "b-1", Project: "app", State: building}
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				eng, follow, exe, cl := newEngine(t)
				eng.mu.Lock()
				tc.setup(eng)
				eng.mu.Unlock()
				if err := HeartbeatOnce(t.Context(), eng, cl, "cl", follow); err != nil {
					t.Fatalf("HeartbeatOnce with %s = %v, want nil", tc.name, err)
				}
				if got := readExe(t, exe); got != "old build 8" {
					t.Fatalf("exe overwritten during %s: %q", tc.name, got)
				}
			})
		}
	})

	t.Run("paused command does not block upgrade", func(t *testing.T) {
		eng, follow, exe, cl := newEngine(t)
		eng.mu.Lock()
		eng.commands["c-paused"] = &command{rec: &pb.CommandRecord{Command: &pb.Command{Id: "c-paused", State: paused, Limits: &pb.CommandLimits{}}}}
		eng.mu.Unlock()

		err := HeartbeatOnce(t.Context(), eng, cl, "cl", follow)
		if !errors.Is(err, update.ErrRestart) {
			t.Fatalf("HeartbeatOnce with only paused command = %v, want update.ErrRestart", err)
		}
		if got := readExe(t, exe); got != string(payload9) {
			t.Fatalf("exe after paused-only upgrade = %q, want %q", got, string(payload9))
		}
	})

	t.Run("server command does not block upgrade", func(t *testing.T) {
		eng, follow, exe, cl := newEngine(t)
		eng.mu.Lock()
		eng.commands["c-srv"] = &command{rec: &pb.CommandRecord{Command: &pb.Command{
			Id: "c-srv", State: running, Kind: "server", Limits: &pb.CommandLimits{},
		}}}
		eng.mu.Unlock()

		err := HeartbeatOnce(t.Context(), eng, cl, "cl", follow)
		if !errors.Is(err, update.ErrRestart) {
			t.Fatalf("HeartbeatOnce with only server command = %v, want update.ErrRestart", err)
		}
		if got := readExe(t, exe); got != string(payload9) {
			t.Fatalf("exe after server-only upgrade = %q, want %q", got, string(payload9))
		}
	})

	t.Run("interactive shell defers upgrade until it closes or MaxDefer elapses", func(t *testing.T) {
		newFakeShell := func(id string) *activeShell {
			done := make(chan struct{})
			var once sync.Once
			return &activeShell{
				id:     id,
				cancel: func() { once.Do(func() { close(done) }) },
				done:   done,
			}
		}

		// Case 1: closing the shell allows upgrade before MaxDefer.
		eng, follow, exe, cl := newEngine(t)
		now := time.Unix(1795320000, 0)
		follow.Now = func() time.Time { return now }
		follow.MaxDefer = 10 * time.Minute

		eng.mu.Lock()
		eng.shells["sh-1"] = newFakeShell("sh-1")
		eng.mu.Unlock()

		if err := HeartbeatOnce(t.Context(), eng, cl, "cl", follow); err != nil {
			t.Fatalf("HeartbeatOnce with active shell = %v, want nil", err)
		}
		if got := readExe(t, exe); got != "old build 8" {
			t.Fatalf("exe overwritten while interactive shell active: %q", got)
		}
		if got := follow.deferred(); got != 9 {
			t.Fatalf("follow.deferred() = %d, want 9", got)
		}

		// Still below MaxDefer: stays deferred.
		now = now.Add(9 * time.Minute)
		if err := HeartbeatOnce(t.Context(), eng, cl, "cl", follow); err != nil {
			t.Fatalf("HeartbeatOnce before MaxDefer with active shell = %v, want nil", err)
		}
		if got := readExe(t, exe); got != "old build 8" {
			t.Fatalf("exe overwritten before MaxDefer: %q", got)
		}

		// Closing the shell allows the next heartbeat to upgrade immediately.
		eng.mu.Lock()
		delete(eng.shells, "sh-1")
		eng.mu.Unlock()

		err := HeartbeatOnce(t.Context(), eng, cl, "cl", follow)
		if !errors.Is(err, update.ErrRestart) {
			t.Fatalf("HeartbeatOnce after shell closed = %v, want update.ErrRestart", err)
		}
		if got := readExe(t, exe); got != string(payload9) {
			t.Fatalf("exe after shell close = %q, want %q", got, string(payload9))
		}

		// Case 2: shell stays open past MaxDefer -> cap forces upgrade anyway.
		eng2, follow2, exe2, cl2 := newEngine(t)
		now2 := time.Unix(1795320000, 0)
		follow2.Now = func() time.Time { return now2 }
		follow2.MaxDefer = 10 * time.Minute
		eng2.mu.Lock()
		eng2.shells["sh-2"] = newFakeShell("sh-2")
		eng2.mu.Unlock()

		if err := HeartbeatOnce(t.Context(), eng2, cl2, "cl", follow2); err != nil {
			t.Fatalf("HeartbeatOnce with active shell at t=0 = %v, want nil", err)
		}
		now2 = now2.Add(10 * time.Minute)
		err = HeartbeatOnce(t.Context(), eng2, cl2, "cl", follow2)
		if !errors.Is(err, update.ErrRestart) {
			t.Fatalf("HeartbeatOnce with active shell at MaxDefer = %v, want update.ErrRestart", err)
		}
		if got := readExe(t, exe2); got != string(payload9) {
			t.Fatalf("exe after shell MaxDefer upgrade = %q, want %q", got, string(payload9))
		}
	})

	t.Run("default MaxDefer cap is 1 minute", func(t *testing.T) {
		eng, follow, exe, cl := newEngine(t)
		now := time.Unix(1795320000, 0)
		follow.Now = func() time.Time { return now }
		follow.MaxDefer = 0 // use defaultMaxUpgradeDefer

		if got := follow.maxDefer(); got != time.Minute {
			t.Fatalf("default follow.maxDefer() = %s, want 1m", got)
		}

		eng.mu.Lock()
		eng.commands["c-default-defer"] = &command{rec: &pb.CommandRecord{Command: &pb.Command{Id: "c-default-defer", State: running, Limits: &pb.CommandLimits{}}}}
		eng.mu.Unlock()

		// t=0: starts deferring.
		if err := HeartbeatOnce(t.Context(), eng, cl, "cl", follow); err != nil {
			t.Fatalf("HeartbeatOnce at t=0 = %v, want nil", err)
		}
		// t=59s: still deferred.
		now = now.Add(59 * time.Second)
		if err := HeartbeatOnce(t.Context(), eng, cl, "cl", follow); err != nil {
			t.Fatalf("HeartbeatOnce at t=59s = %v, want nil", err)
		}
		if got := readExe(t, exe); got != "old build 8" {
			t.Fatalf("exe overwritten before 1m default cap: %q", got)
		}
		// t=60s: default 1m cap reached -> installs and requests restart.
		now = now.Add(time.Second)
		err := HeartbeatOnce(t.Context(), eng, cl, "cl", follow)
		if !errors.Is(err, update.ErrRestart) {
			t.Fatalf("HeartbeatOnce at t=1m = %v, want update.ErrRestart", err)
		}
		if got := readExe(t, exe); got != string(payload9) {
			t.Fatalf("exe after 1m default cap = %q, want %q", got, string(payload9))
		}
	})

	t.Run("long-running command hits MaxDefer cap and upgrades cleanly", func(t *testing.T) {
		eng, follow, exe, cl := newEngine(t)
		now := time.Unix(1795320000, 0)
		follow.Now = func() time.Time { return now }
		follow.MaxDefer = 15 * time.Minute

		eng.mu.Lock()
		eng.commands["c-long"] = &command{rec: &pb.CommandRecord{Command: &pb.Command{Id: "c-long", State: running, Limits: &pb.CommandLimits{}}}}
		eng.mu.Unlock()

		// t=0: starts deferring.
		if err := HeartbeatOnce(t.Context(), eng, cl, "cl", follow); err != nil {
			t.Fatalf("HeartbeatOnce at t=0 = %v, want nil", err)
		}
		// t=14m59s: still deferred.
		now = now.Add(15*time.Minute - time.Second)
		if err := HeartbeatOnce(t.Context(), eng, cl, "cl", follow); err != nil {
			t.Fatalf("HeartbeatOnce at t=14m59s = %v, want nil", err)
		}
		if got := readExe(t, exe); got != "old build 8" {
			t.Fatalf("exe overwritten before MaxDefer cap: %q", got)
		}

		// t=15m00s: cap reached, installs and returns ErrRestart even while command runs.
		now = now.Add(time.Second)
		err := HeartbeatOnce(t.Context(), eng, cl, "cl", follow)
		if !errors.Is(err, update.ErrRestart) {
			t.Fatalf("HeartbeatOnce at MaxDefer = %v, want update.ErrRestart", err)
		}
		if got := readExe(t, exe); got != string(payload9) {
			t.Fatalf("exe after MaxDefer upgrade = %q, want %q", got, string(payload9))
		}
		if got := follow.deferred(); got != 0 {
			t.Fatalf("follow.deferred() after forced upgrade = %d, want 0", got)
		}
	})

	t.Run("rapid newer build while deferring does not reset deferredSince", func(t *testing.T) {
		eng, follow, exe, cl := newEngine(t)
		now := time.Unix(1795320000, 0)
		follow.Now = func() time.Time { return now }
		follow.MaxDefer = 15 * time.Minute

		eng.mu.Lock()
		eng.commands["c-long"] = &command{rec: &pb.CommandRecord{Command: &pb.Command{Id: "c-long", State: running, Limits: &pb.CommandLimits{}}}}
		eng.mu.Unlock()

		// t=0: begin deferring build 9.
		if err := HeartbeatOnce(t.Context(), eng, cl, "cl", follow); err != nil {
			t.Fatalf("HeartbeatOnce build 9 at t=0 = %v, want nil", err)
		}

		// t=10m: build 10 arrives while still deferring; deferredSince must stay anchored at t=0.
		now = now.Add(10 * time.Minute)
		cl.build = &pb.Build{Number: 10, Sha256: sum10, Path: buildPath10}
		if err := HeartbeatOnce(t.Context(), eng, cl, "cl", follow); err != nil {
			t.Fatalf("HeartbeatOnce build 10 at t=10m = %v, want nil", err)
		}
		if got := follow.deferred(); got != 10 {
			t.Fatalf("follow.deferred() = %d, want 10", got)
		}
		if got := readExe(t, exe); got != "old build 8" {
			t.Fatalf("exe overwritten at t=10m: %q", got)
		}

		// t=15m (only 5m after build 10 arrived, 15m after initial deferral): cap reached -> upgrades to build 10.
		now = now.Add(5 * time.Minute)
		err := HeartbeatOnce(t.Context(), eng, cl, "cl", follow)
		if !errors.Is(err, update.ErrRestart) {
			t.Fatalf("HeartbeatOnce build 10 at t=15m = %v, want update.ErrRestart", err)
		}
		if got := readExe(t, exe); got != string(payload10) {
			t.Fatalf("exe after build 10 upgrade = %q, want %q", got, string(payload10))
		}
	})

	t.Run("active work starting during download aborts swap and cleans staged file", func(t *testing.T) {
		eng, follow, exe, cl := newEngine(t)
		hookMu.Lock()
		onDownload = func() {
			eng.mu.Lock()
			eng.commands["c-race"] = &command{rec: &pb.CommandRecord{Command: &pb.Command{Id: "c-race", State: running, Limits: &pb.CommandLimits{}}}}
			eng.mu.Unlock()
		}
		hookMu.Unlock()

		if err := HeartbeatOnce(t.Context(), eng, cl, "cl", follow); err != nil {
			t.Fatalf("HeartbeatOnce with mid-download command = %v, want nil", err)
		}
		if got := readExe(t, exe); got != "old build 8" {
			t.Fatalf("exe overwritten despite mid-download command: %q", got)
		}
		if got := follow.deferred(); got != 9 {
			t.Fatalf("follow.deferred() = %d, want 9", got)
		}
		entries, err := os.ReadDir(filepath.Dir(exe))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != filepath.Base(exe) {
			names := make([]string, len(entries))
			for i, e := range entries {
				names[i] = e.Name()
			}
			t.Fatalf("leftover staged files in exe dir: %v", names)
		}
	})
}

type releaseHeartbeatCluster struct {
	hotbedv1connect.ClusterServiceClient
	release []string
}

func (c *releaseHeartbeatCluster) Heartbeat(_ context.Context, _ *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	return &pb.HeartbeatResponse{Release: c.release}, nil
}

func TestHeartbeatOnceDoesNotBlockOnPreparingWorkspaceRelease(t *testing.T) {
	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	eng, err := NewEngine(t.Context(), EngineConfig{
		Dir: dir, NodeID: "n1", Host: "asahimbp",
		Store: store, BaseImage: fake.BaseImage(store), Units: &fake.Units{}, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}
	forkDone := make(chan struct{})
	t.Cleanup(func() {
		close(forkDone)
		eng.Close()
	})

	eng.mu.Lock()
	eng.workspaces["ws-prep"] = &workspace{
		rec: &pb.WorkspaceRecord{
			Workspace: &pb.Workspace{Id: "ws-prep", State: preparing},
			Volume:    "ws/ws-prep",
		},
		forking:  true,
		forkDone: forkDone,
	}
	eng.mu.Unlock()

	cl := &releaseHeartbeatCluster{release: []string{"ws-prep"}}

	done := make(chan error, 1)
	go func() {
		done <- HeartbeatOnce(t.Context(), eng, cl, "cl", nil)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("HeartbeatOnce = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HeartbeatOnce blocked waiting for preparing workspace release")
	}
}

func TestKeepWithFailureHookResetsTransportOnTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var attempts atomic.Int32
	var resets atomic.Int32
	done := make(chan struct{})

	errCh := make(chan error, 1)
	go func() {
		errCh <- keepWithFailureHook(ctx, "heartbeat", nil, nil, func() {
			close(done)
			cancel()
		}, func(err error) {
			if errors.Is(err, context.DeadlineExceeded) {
				resets.Add(1)
			}
		}, func(context.Context) (bool, error) {
			if attempts.Add(1) == 1 {
				return false, context.DeadlineExceeded
			}
			return false, nil
		})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for recovery after timeout")
	}
	if err := <-errCh; err != nil {
		t.Fatalf("keepWithFailureHook returned %v", err)
	}
	if got := resets.Load(); got != 1 {
		t.Fatalf("transport resets = %d, want 1", got)
	}
}

func TestFollowDownloadsDirectlyFromGitHubAndNeverDowngrades(t *testing.T) {
	payload := []byte("hotbed build 172 direct from github")
	sumBytes := sha256.Sum256(payload)
	sum := hex.EncodeToString(sumBytes[:])

	var githubHits, controllerHits atomic.Int32
	githubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("GitHub download leaked Authorization header: %q", r.Header.Get("Authorization"))
			http.Error(w, "unexpected auth", http.StatusBadRequest)
			return
		}
		if r.Header.Get("X-Hotbed-Node-Id") != "" {
			t.Errorf("GitHub download leaked X-Hotbed-Node-Id header")
		}
		githubHits.Add(1)
		w.Write(payload)
	}))
	defer githubSrv.Close()

	ctrlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		controllerHits.Add(1)
		http.Error(w, "controller proxy should not be used when download_url is set", http.StatusInternalServerError)
	}))
	defer ctrlSrv.Close()

	// Wrap controller client with an auth header to prove DownloadClient never uses it.
	authClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			r = r.Clone(r.Context())
			r.Header.Set("Authorization", "Bearer hbw_secret_worker_token")
			r.Header.Set("X-Hotbed-Node-Id", "node-1")
			return http.DefaultTransport.RoundTrip(r)
		}),
	}

	t.Run("older worker downloads from download_url without controller auth and requests restart", func(t *testing.T) {
		exe := filepath.Join(t.TempDir(), "hotbed")
		if err := os.WriteFile(exe, []byte("old build 171"), 0o755); err != nil {
			t.Fatal(err)
		}
		follow := &Follow{
			URL:            ctrlSrv.URL,
			Client:         authClient,
			DownloadClient: githubSrv.Client(),
			Exe:            exe,
			Build:          171,
		}
		err := follow.To(t.Context(), &pb.Build{
			Number:      172,
			Sha256:      sum,
			Path:        "/builds/172/hotbed-linux-amd64",
			DownloadUrl: githubSrv.URL + "/semistrict/hotbed/releases/download/build-172/hotbed-linux-amd64",
		})
		if !errors.Is(err, update.ErrRestart) {
			t.Fatalf("Follow.To = %v, want update.ErrRestart", err)
		}
		got, err := os.ReadFile(exe)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(payload) {
			t.Fatalf("exe = %q, want %q", got, payload)
		}
		if githubHits.Load() != 1 || controllerHits.Load() != 0 {
			t.Fatalf("hits: github=%d controller=%d, want 1/0", githubHits.Load(), controllerHits.Load())
		}
	})

	t.Run("newer worker never downgrades to older controller build", func(t *testing.T) {
		exe := filepath.Join(t.TempDir(), "hotbed")
		if err := os.WriteFile(exe, []byte("newer build 173"), 0o755); err != nil {
			t.Fatal(err)
		}
		before := githubHits.Load()
		follow := &Follow{
			URL:            ctrlSrv.URL,
			Client:         authClient,
			DownloadClient: githubSrv.Client(),
			Exe:            exe,
			Build:          173,
		}
		if err := follow.To(t.Context(), &pb.Build{
			Number:      172,
			Sha256:      sum,
			Path:        "/builds/172/hotbed-linux-amd64",
			DownloadUrl: githubSrv.URL + "/semistrict/hotbed/releases/download/build-172/hotbed-linux-amd64",
		}); err != nil {
			t.Fatalf("Follow.To on newer worker = %v, want nil", err)
		}
		got, err := os.ReadFile(exe)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "newer build 173" {
			t.Fatalf("newer worker exe overwritten: %q", got)
		}
		if githubHits.Load() != before {
			t.Fatalf("newer worker downloaded older build")
		}
	})

	t.Run("rejects tampered binary from download_url without touching exe or falling back to controller", func(t *testing.T) {
		exe := filepath.Join(t.TempDir(), "hotbed")
		if err := os.WriteFile(exe, []byte("old build 171"), 0o755); err != nil {
			t.Fatal(err)
		}
		follow := &Follow{
			URL:            ctrlSrv.URL,
			Client:         authClient,
			DownloadClient: githubSrv.Client(),
			Exe:            exe,
			Build:          171,
		}
		err := follow.To(t.Context(), &pb.Build{
			Number:      172,
			Sha256:      strings.Repeat("0", 64),
			Path:        "/builds/172/hotbed-linux-amd64",
			DownloadUrl: githubSrv.URL + "/semistrict/hotbed/releases/download/build-172/hotbed-linux-amd64",
		})
		if err != nil {
			t.Fatalf("Follow.To on checksum mismatch = %v, want nil (logged and retried later)", err)
		}
		got, err := os.ReadFile(exe)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "old build 171" {
			t.Fatalf("exe overwritten despite bad checksum: %q", got)
		}
		if controllerHits.Load() != 0 {
			t.Fatalf("fell back to controller proxy on checksum mismatch")
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
