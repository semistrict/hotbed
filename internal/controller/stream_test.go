package controller_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/controller"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/tailnet"
	"github.com/semistrict/hotbed/internal/tailnet/tailnettest"
)

const cluster = "c0ffee"

var (
	ubu1   = tailnet.Self{Peer: tailnet.Peer{NodeID: "nUbu1", Hostname: "ubu1", Login: "ramon@"}, Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
	asahi  = tailnet.Peer{NodeID: "nAsahi", Hostname: "asahimbp", Login: "ramon@"}
	laptop = tailnet.Peer{NodeID: "nLaptop", Hostname: "laptop", Login: "ramon@"}
)

// streamed is a controller with asahimbp joined, and clients as the worker
// and as an agent on a laptop.
type streamed struct {
	t          *testing.T
	worker     hotbedv1connect.ClusterServiceClient
	sessions   hotbedv1connect.SessionServiceClient
	workspaces hotbedv1connect.WorkspaceServiceClient
	cockpit    hotbedv1connect.ClusterServiceClient
	store      *controller.Store
	createDone <-chan struct{}
}

func newStreamed(t *testing.T) *streamed {
	t.Helper()
	store, err := controller.OpenStore(t.Context(), filepath.Join(t.TempDir(), "hotbed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	tn := tailnettest.New(ubu1)
	srv := httptest.NewServer(controller.NewServer(controller.ServerConfig{
		Store: store, Identifier: tn, Self: ubu1.Addrs,
		Cluster: statedir.ControllerConfig{ClusterID: cluster, Owner: "ramon@", Port: 7120},
		// No worker answers: calls to it fail as a worker out of reach would.
		WorkerAddr: func(h string) string { return "127.0.0.1:1" },
	}).Handler())
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	w, _, err := api.Controller(addr, tn.Client(asahi))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Join(t.Context(), &pb.JoinRequest{}); err != nil {
		t.Fatal(err)
	}
	conn, err := api.Dial(addr, api.DefaultPort, tn.Client(laptop))
	if err != nil {
		t.Fatal(err)
	}
	return &streamed{
		t: t, worker: w, store: store,
		sessions:   hotbedv1connect.NewSessionServiceClient(conn),
		workspaces: hotbedv1connect.NewWorkspaceServiceClient(conn),
		cockpit:    hotbedv1connect.NewClusterServiceClient(conn),
	}
}

func (s *streamed) push(req *pb.PushEventsRequest) *pb.PushEventsResponse {
	s.t.Helper()
	req.ClusterId = cluster
	res, err := s.worker.PushEvents(s.t.Context(), req)
	if err != nil {
		s.t.Fatal(err)
	}
	return res
}

func (s *streamed) beat() {
	s.t.Helper()
	if _, err := s.worker.Heartbeat(s.t.Context(), &pb.HeartbeatRequest{ClusterId: cluster, Resources: &pb.Resources{MemoryBytes: 16 << 30}}); err != nil {
		s.t.Fatal(err)
	}
}

// watch returns the session's events after a cursor, as wait would.
func (s *streamed) watch(session string, after int64) *pb.SessionEvents {
	s.t.Helper()
	in, err := s.sessions.WatchSession(s.t.Context(), &pb.WatchSessionRequest{SessionId: session, After: after, TimeoutSeconds: 1})
	if err != nil {
		s.t.Fatal(err)
	}
	defer in.Close()
	res, err := in.Receive()
	if err != nil {
		s.t.Fatal(err)
	}
	return res
}

func whats(events []*pb.SessionEvent) []string {
	var out []string
	for _, e := range events {
		what, id := api.EventWhat(e)
		out = append(out, what+" "+id)
	}
	return out
}

func wsEvent(seq int64, what string, ws *pb.Workspace) *pb.Event {
	return &pb.Event{Seq: seq, At: 1000 + seq, Change: &pb.Event_Workspace{Workspace: &pb.WorkspaceChange{What: what, Workspace: proto.Clone(ws).(*pb.Workspace)}}}
}

func cmdEvent(seq int64, what string, c *pb.Command, finished *pb.FinishedCommand) *pb.Event {
	return &pb.Event{Seq: seq, At: 1000 + seq, Change: &pb.Event_Command{Command: &pb.CommandChange{What: what, Command: proto.Clone(c).(*pb.Command), Finished: finished}}}
}

// A worker's events, pushed in order, become the controller's view of the
// node: the session's timeline, history, the cockpit's feed, and the
// commands ps lists, with where to reach the worker. Pushing them again
// changes nothing.
func TestEventsFoldIntoTheControllersView(t *testing.T) {
	s := newStreamed(t)
	if _, err := s.sessions.StartSession(t.Context(), &pb.StartSessionRequest{Id: "calm-owl", Remote: "github.com/o/app"}); err != nil {
		t.Fatal(err)
	}
	first := s.push(&pb.PushEventsRequest{Epoch: "e1"})
	if !first.CheckpointNeeded {
		t.Fatalf("first push of a new epoch = %v, want a checkpoint asked for", first)
	}
	if res := s.push(&pb.PushEventsRequest{Epoch: "e1", Checkpoint: &pb.Checkpoint{}}); res.Applied != 0 || res.CheckpointNeeded {
		t.Fatalf("after an empty checkpoint: %v", res)
	}

	ws := &pb.Workspace{Id: "ws1", Project: "app", NodeId: asahi.NodeID, SessionId: "calm-owl", State: pb.WorkspaceState_WORKSPACE_STATE_PREPARING}
	ready := proto.Clone(ws).(*pb.Workspace)
	ready.State = pb.WorkspaceState_WORKSPACE_STATE_IDLE
	cmd := &pb.Command{Id: "cmd1", WorkspaceId: "ws1", NodeId: asahi.NodeID, SessionId: "calm-owl", Argv: "cargo test", State: pb.CommandState_COMMAND_STATE_QUEUED}
	running := proto.Clone(cmd).(*pb.Command)
	running.State = pb.CommandState_COMMAND_STATE_RUNNING
	exited := proto.Clone(cmd).(*pb.Command)
	exited.State, exited.ExitCode = pb.CommandState_COMMAND_STATE_FAILED, proto.Int32(3)
	finished := &pb.FinishedCommand{Id: "cmd1", RepoId: "app", NodeId: asahi.NodeID, SessionId: "calm-owl", State: pb.CommandState_COMMAND_STATE_FAILED, EndedAt: time.Now().UnixMilli()}
	events := []*pb.Event{
		wsEvent(1, "created", ws),
		wsEvent(2, "ready", ready),
		cmdEvent(3, "queued", cmd, nil),
		cmdEvent(4, "admitted", running, nil),
		cmdEvent(5, "exited", exited, finished),
	}
	events[4].Feed = &pb.WorkerEvent{At: 1005, Kind: "fail", Text: "fail cargo test · app · exit 3", Ref: &pb.EventRef{Type: "command", Id: "cmd1"}}
	for range 2 {
		if res := s.push(&pb.PushEventsRequest{Epoch: "e1", Events: events}); res.Applied != 5 || res.CheckpointNeeded {
			t.Fatalf("push = %v, want applied 5", res)
		}
	}
	s.beat()

	got := s.watch("calm-owl", 0)
	if want := []string{"ready ws1", "admitted cmd1", "exited cmd1"}; !slices.Equal(whats(got.Events), want) {
		t.Errorf("session events = %q, want %q", whats(got.Events), want)
	}
	if last := got.Events[len(got.Events)-1].GetCommand(); last.GetExitCode() != 3 || last.State != pb.CommandState_COMMAND_STATE_FAILED || last.Argv != "cargo test" {
		t.Errorf("exited event = %v", last)
	}
	ps, err := s.workspaces.ListCommands(t.Context(), &pb.ListCommandsRequest{SessionId: "calm-owl"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ps.Commands) != 1 || ps.Commands[0].State != pb.CommandState_COMMAND_STATE_FAILED || ps.Commands[0].NodeAddr != "127.0.0.1:1" {
		t.Errorf("ps = %v, want cmd1 failed, at its worker's address", ps.Commands)
	}
	snap, err := s.cockpit.GetSnapshot(t.Context(), &pb.GetSnapshotRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.History) != 1 || snap.History[0].Id != "cmd1" {
		t.Errorf("history = %v, want cmd1 once", snap.History)
	}
	if len(snap.Events) != 1 || snap.Events[0].Text != "fail cargo test · app · exit 3" || snap.Events[0].NodeId != asahi.NodeID {
		t.Errorf("feed = %v", snap.Events)
	}
	if len(snap.Workspaces) != 1 || snap.Workspaces[0].State != pb.WorkspaceState_WORKSPACE_STATE_IDLE {
		t.Errorf("workspaces = %v", snap.Workspaces)
	}

	// A command the worker forgot leaves the view; history keeps it.
	s.push(&pb.PushEventsRequest{Epoch: "e1", Events: []*pb.Event{cmdEvent(6, "forgotten", exited, nil)}})
	if ps, err = s.workspaces.ListCommands(t.Context(), &pb.ListCommandsRequest{SessionId: "calm-owl"}); err != nil || len(ps.Commands) != 0 {
		t.Errorf("ps after the command was forgotten = %v, %v", ps, err)
	}
}

// A worker whose database is new (its epoch changed) starts the view again
// from its checkpoint. What the old view had and the checkpoint lacks is
// lost: the session hears that its live commands were cancelled and its
// workspaces failed, and they count as released.
func TestANewEpochStartsFromACheckpoint(t *testing.T) {
	s := newStreamed(t)
	if _, err := s.sessions.StartSession(t.Context(), &pb.StartSessionRequest{Id: "lost-elk", Remote: "github.com/o/app"}); err != nil {
		t.Fatal(err)
	}
	s.push(&pb.PushEventsRequest{Epoch: "old", Checkpoint: &pb.Checkpoint{Seq: 7}})
	ws := &pb.Workspace{Id: "ws1", Project: "app", NodeId: asahi.NodeID, SessionId: "lost-elk", State: pb.WorkspaceState_WORKSPACE_STATE_ACTIVE}
	cmd := &pb.Command{Id: "cmd1", WorkspaceId: "ws1", NodeId: asahi.NodeID, SessionId: "lost-elk", State: pb.CommandState_COMMAND_STATE_RUNNING}
	if res := s.push(&pb.PushEventsRequest{Epoch: "old", Events: []*pb.Event{wsEvent(8, "ready", ws), cmdEvent(9, "admitted", cmd, nil)}}); res.Applied != 9 {
		t.Fatalf("push = %v", res)
	}
	cursor := s.watch("lost-elk", 0).Cursor

	if res := s.push(&pb.PushEventsRequest{Epoch: "new", Events: []*pb.Event{wsEvent(1, "ready", ws)}}); !res.CheckpointNeeded || res.Applied != 0 {
		t.Fatalf("events of a new epoch = %v, want a checkpoint asked for", res)
	}
	if res := s.push(&pb.PushEventsRequest{Epoch: "new", Checkpoint: &pb.Checkpoint{Seq: 3}}); res.Applied != 3 || res.CheckpointNeeded {
		t.Fatalf("checkpoint = %v, want applied 3", res)
	}
	got := s.watch("lost-elk", cursor)
	if want := []string{"failed ws1", "cancelled cmd1"}; !slices.Equal(whats(got.Events), want) {
		t.Errorf("session events after the worker's state was lost = %q, want %q", whats(got.Events), want)
	}
	for _, e := range got.Events {
		if !strings.Contains(e.Detail, "lost") {
			t.Errorf("event %v does not say it was lost", e)
		}
	}
	sess, err := s.sessions.EndSession(t.Context(), &pb.EndSessionRequest{Id: "lost-elk"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.Workspaces) != 1 || sess.Workspaces[0].ReleasedAt == nil {
		t.Errorf("session's workspaces = %v, want ws1 released", sess.Workspaces)
	}
}

// A session that ends while its worker is out of reach has its workspaces
// released by the worker's next heartbeat.
func TestHeartbeatReleasesWorkspacesOfEndedSessions(t *testing.T) {
	s := newStreamed(t)
	if _, err := s.sessions.StartSession(t.Context(), &pb.StartSessionRequest{Id: "gone-yak", Remote: "github.com/o/app"}); err != nil {
		t.Fatal(err)
	}
	s.push(&pb.PushEventsRequest{Epoch: "e1", Checkpoint: &pb.Checkpoint{Workspaces: []*pb.Workspace{
		{Id: "ws1", Project: "app", NodeId: asahi.NodeID, SessionId: "gone-yak", State: pb.WorkspaceState_WORKSPACE_STATE_IDLE},
	}}})
	s.beat()
	if _, err := s.sessions.EndSession(t.Context(), &pb.EndSessionRequest{Id: "gone-yak"}); err != nil {
		t.Fatal(err)
	}
	res, err := s.worker.Heartbeat(t.Context(), &pb.HeartbeatRequest{ClusterId: cluster})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"ws1"}, res.Release); diff != "" {
		t.Errorf("release (-want +got):\n%s", diff)
	}
}

// What happened to a session's commands is its history even when a worker
// reports it after the session ended: the session's end stopped them while
// the worker was out of reach.
func TestCommandEventsAfterTheSessionEndedAreKept(t *testing.T) {
	s := newStreamed(t)
	if _, err := s.sessions.StartSession(t.Context(), &pb.StartSessionRequest{Id: "late-elk", Remote: "github.com/o/app"}); err != nil {
		t.Fatal(err)
	}
	s.push(&pb.PushEventsRequest{Epoch: "e1", Checkpoint: &pb.Checkpoint{}})
	cmd := &pb.Command{Id: "cmd1", NodeId: asahi.NodeID, SessionId: "late-elk", State: pb.CommandState_COMMAND_STATE_RUNNING}
	s.push(&pb.PushEventsRequest{Epoch: "e1", Events: []*pb.Event{cmdEvent(1, "admitted", cmd, nil)}})
	if _, err := s.sessions.EndSession(t.Context(), &pb.EndSessionRequest{Id: "late-elk"}); err != nil {
		t.Fatal(err)
	}
	stopped := proto.Clone(cmd).(*pb.Command)
	stopped.State = pb.CommandState_COMMAND_STATE_CANCELLED
	s.push(&pb.PushEventsRequest{Epoch: "e1", Events: []*pb.Event{cmdEvent(2, "cancelled", stopped, nil)}})
	evs, err := s.store.SessionEvents(t.Context(), "late-elk", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := whats(evs); !slices.Equal(got, []string{"admitted cmd1", "cancelled cmd1"}) {
		t.Errorf("session events = %q, want the command's end kept", got)
	}
}

// A checkpoint comes with the events it includes that the controller may
// lack, so what happened in them still reaches history and sessions: a
// command that ended and was forgotten is not lost.
func TestCheckpointCarriesTheEventsItIncludes(t *testing.T) {
	s := newStreamed(t)
	if _, err := s.sessions.StartSession(t.Context(), &pb.StartSessionRequest{Id: "late-bat", Remote: "github.com/o/app"}); err != nil {
		t.Fatal(err)
	}
	s.push(&pb.PushEventsRequest{Epoch: "e1", Checkpoint: &pb.Checkpoint{Seq: 1}})
	cmd := &pb.Command{Id: "cmd1", NodeId: asahi.NodeID, SessionId: "late-bat", State: pb.CommandState_COMMAND_STATE_RUNNING}
	s.push(&pb.PushEventsRequest{Epoch: "e1", Events: []*pb.Event{cmdEvent(2, "admitted", cmd, nil)}})
	cursor := s.watch("late-bat", 0).Cursor

	// The controller fell behind what the worker kept: the events it still
	// has (4 and 5; 3 is gone) come with the checkpoint, whose state no
	// longer has the command.
	done := proto.Clone(cmd).(*pb.Command)
	done.State, done.ExitCode = pb.CommandState_COMMAND_STATE_SUCCEEDED, proto.Int32(0)
	finished := &pb.FinishedCommand{Id: "cmd1", NodeId: asahi.NodeID, SessionId: "late-bat", State: pb.CommandState_COMMAND_STATE_SUCCEEDED, EndedAt: time.Now().UnixMilli()}
	res := s.push(&pb.PushEventsRequest{Epoch: "e1", Checkpoint: &pb.Checkpoint{Seq: 5}, Events: []*pb.Event{
		cmdEvent(4, "exited", done, finished),
		cmdEvent(5, "forgotten", done, nil),
	}})
	if res.Applied != 5 {
		t.Fatalf("checkpoint = %v, want applied 5", res)
	}
	if got := whats(s.watch("late-bat", cursor).Events); !slices.Equal(got, []string{"exited cmd1"}) {
		t.Errorf("session events = %q, want the command's exit, not its loss", got)
	}
	snap, err := s.cockpit.GetSnapshot(t.Context(), &pb.GetSnapshotRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.History) != 1 || snap.History[0].Id != "cmd1" {
		t.Errorf("history = %v, want cmd1", snap.History)
	}
}

// The controller continues a worker's events only from where it is: a gap
// after its applied event, or a different event at it (the worker's
// database is an older copy that has written on since), asks for a
// checkpoint rather than apply a history that does not follow.
func TestEventsThatDoNotFollowAskForACheckpoint(t *testing.T) {
	s := newStreamed(t)
	s.push(&pb.PushEventsRequest{Epoch: "e1", Checkpoint: &pb.Checkpoint{Seq: 0}})
	ws := &pb.Workspace{Id: "ws1", NodeId: asahi.NodeID}
	if res := s.push(&pb.PushEventsRequest{Epoch: "e1", Events: []*pb.Event{wsEvent(1, "ready", ws)}}); res.Applied != 1 {
		t.Fatalf("push = %v", res)
	}
	if res := s.push(&pb.PushEventsRequest{Epoch: "e1", AfterAt: 1001, Events: []*pb.Event{wsEvent(3, "idle", ws)}}); !res.CheckpointNeeded || res.Applied != 1 {
		t.Errorf("events after a gap = %v, want a checkpoint asked for", res)
	}
	if res := s.push(&pb.PushEventsRequest{Epoch: "e1", AfterAt: 999, Events: []*pb.Event{wsEvent(2, "idle", ws)}}); !res.CheckpointNeeded || res.Applied != 1 {
		t.Errorf("events after a different event 1 = %v, want a checkpoint asked for", res)
	}
	if res := s.push(&pb.PushEventsRequest{Epoch: "e1", AfterAt: 1001, Events: []*pb.Event{wsEvent(2, "idle", ws)}}); res.CheckpointNeeded || res.Applied != 2 {
		t.Errorf("events that follow = %v, want applied 2", res)
	}
}

// An event this controller does not understand (from a newer worker) is
// passed over, not refused: refusing it would stop the worker's stream.
func TestUnknownChangesArePassedOver(t *testing.T) {
	s := newStreamed(t)
	s.push(&pb.PushEventsRequest{Epoch: "e1", Checkpoint: &pb.Checkpoint{Seq: 0}})
	if res := s.push(&pb.PushEventsRequest{Epoch: "e1", Events: []*pb.Event{{Seq: 1, At: 1001}}}); res.Applied != 1 || res.CheckpointNeeded {
		t.Errorf("push of an unknown change = %v, want applied 1", res)
	}
}

type stubWorker struct {
	hotbedv1connect.UnimplementedWorkspaceServiceHandler
	mu         sync.Mutex
	workspaces map[string]*pb.Workspace
	commands   []*pb.Command
	onCreate   func(ctx context.Context, req *pb.CreateWorkspaceRequest) (*pb.Workspace, error)
	onRelease  func(id string)
}

func (w *stubWorker) CreateWorkspace(ctx context.Context, req *pb.CreateWorkspaceRequest) (*pb.Workspace, error) {
	if w.onCreate != nil {
		return w.onCreate(ctx, req)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	ws := &pb.Workspace{Id: req.Id, SessionId: req.SessionId, NodeId: asahi.NodeID, State: pb.WorkspaceState_WORKSPACE_STATE_IDLE, CreatedAt: time.Now().UnixMilli()}
	w.workspaces[ws.Id] = ws
	return proto.Clone(ws).(*pb.Workspace), nil
}

func (w *stubWorker) ReleaseWorkspace(_ context.Context, req *pb.ReleaseWorkspaceRequest) (*pb.ReleaseWorkspaceResponse, error) {
	w.mu.Lock()
	_, ok := w.workspaces[req.Id]
	delete(w.workspaces, req.Id)
	onRelease := w.onRelease
	w.mu.Unlock()
	if onRelease != nil {
		onRelease(req.Id)
	}
	if !ok {
		return nil, api.ToConnect(api.Errorf(api.CodeNotFound, "no workspace %s on this node", req.Id))
	}
	return &pb.ReleaseWorkspaceResponse{}, nil
}

func (w *stubWorker) GetWorkspace(_ context.Context, req *pb.GetWorkspaceRequest) (*pb.Workspace, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	ws, ok := w.workspaces[req.Id]
	if !ok {
		return nil, api.ToConnect(api.Errorf(api.CodeNotFound, "no workspace %s on this node", req.Id))
	}
	return proto.Clone(ws).(*pb.Workspace), nil
}

func (w *stubWorker) ListWorkspaces(context.Context, *pb.ListWorkspacesRequest) (*pb.ListWorkspacesResponse, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := &pb.ListWorkspacesResponse{}
	for _, ws := range w.workspaces {
		out.Workspaces = append(out.Workspaces, proto.Clone(ws).(*pb.Workspace))
	}
	return out, nil
}

func (w *stubWorker) ListCommands(_ context.Context, req *pb.ListCommandsRequest) (*pb.ListCommandsResponse, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := &pb.ListCommandsResponse{}
	for _, c := range w.commands {
		if req.SessionId == "" || c.SessionId == req.SessionId {
			out.Commands = append(out.Commands, proto.Clone(c).(*pb.Command))
		}
	}
	return out, nil
}

func newWithLiveWorker(t *testing.T, stub *stubWorker) (*streamed, *controller.Store) {
	t.Helper()
	store, err := controller.OpenStore(t.Context(), filepath.Join(t.TempDir(), "hotbed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	wsSrv := connect.NewServer()
	hotbedv1connect.RegisterWorkspaceServiceHandler(wsSrv, stub)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, wsSrv)
	workerHTTP := httptest.NewServer(mux)
	t.Cleanup(workerHTTP.Close)
	workerAddr := strings.TrimPrefix(workerHTTP.URL, "http://")

	tn := tailnettest.New(ubu1)
	createDone := make(chan struct{}, 8)
	ctrlHandler := controller.NewServer(controller.ServerConfig{
		Store: store, Identifier: tn, Self: ubu1.Addrs,
		Cluster:    statedir.ControllerConfig{ClusterID: cluster, Owner: "ramon@", Port: 7120},
		WorkerAddr: func(string) string { return workerAddr },
	}).Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if strings.HasSuffix(r.URL.Path, "/CreateWorkspace") {
				select {
				case createDone <- struct{}{}:
				default:
				}
			}
		}()
		ctrlHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")

	w, _, err := api.Controller(addr, tn.Client(asahi))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Join(t.Context(), &pb.JoinRequest{}); err != nil {
		t.Fatal(err)
	}
	conn, err := api.Dial(addr, api.DefaultPort, tn.Client(laptop))
	if err != nil {
		t.Fatal(err)
	}
	return &streamed{
		t: t, worker: w,
		sessions:   hotbedv1connect.NewSessionServiceClient(conn),
		workspaces: hotbedv1connect.NewWorkspaceServiceClient(conn),
		cockpit:    hotbedv1connect.NewClusterServiceClient(conn),
		createDone: createDone,
	}, store
}

// Even when the worker's PushEvents stream lags or is stalled, ps (ListCommands)
// and session status (GetSession) query reachable nodes directly so they agree
// with the node immediately after a stop or after the worker forgot a command
// (board 7232).
func TestPsAndSessionStatusAgreeWithNodeWhenEventsLag(t *testing.T) {
	stub := &stubWorker{workspaces: map[string]*pb.Workspace{}}
	s, _ := newWithLiveWorker(t, stub)
	if _, err := s.sessions.StartSession(t.Context(), &pb.StartSessionRequest{Id: "sweeping-swift", Remote: "github.com/o/app"}); err != nil {
		t.Fatal(err)
	}
	ws := &pb.Workspace{Id: "ws1", Project: "app", NodeId: asahi.NodeID, SessionId: "sweeping-swift", State: pb.WorkspaceState_WORKSPACE_STATE_ACTIVE}
	staleRunning := &pb.Command{Id: "zzrrosbgla", WorkspaceId: "ws1", NodeId: asahi.NodeID, SessionId: "sweeping-swift", Argv: "make check-tla", State: pb.CommandState_COMMAND_STATE_RUNNING, QueuedAt: 100}
	staleParked := &pb.Command{Id: "pdxtkgj2zh", WorkspaceId: "ws1", NodeId: asahi.NodeID, SessionId: "sweeping-swift", Argv: "cargo nextest", State: pb.CommandState_COMMAND_STATE_PARKED, QueuedAt: 200}

	// Controller's cached view still has zzrrosbgla running and pdxtkgj2zh parked.
	s.push(&pb.PushEventsRequest{Epoch: "e1", Checkpoint: &pb.Checkpoint{
		Seq:        10,
		Workspaces: []*pb.Workspace{ws},
		Commands:   []*pb.Command{staleRunning, staleParked},
	}})
	s.beat()

	// Meanwhile on the live node: zzrrosbgla was stopped (cancelled), pdxtkgj2zh
	// was already forgotten, and ws1 is now idle.
	wsIdle := proto.Clone(ws).(*pb.Workspace)
	wsIdle.State = pb.WorkspaceState_WORKSPACE_STATE_IDLE
	cancelled := proto.Clone(staleRunning).(*pb.Command)
	cancelled.State = pb.CommandState_COMMAND_STATE_CANCELLED
	cancelled.Reason = "stopped by laptop"
	stub.mu.Lock()
	stub.workspaces["ws1"] = wsIdle
	stub.commands = []*pb.Command{cancelled}
	stub.mu.Unlock()

	ps, err := s.workspaces.ListCommands(t.Context(), &pb.ListCommandsRequest{SessionId: "sweeping-swift"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ps.Commands) != 1 || ps.Commands[0].Id != "zzrrosbgla" || ps.Commands[0].State != pb.CommandState_COMMAND_STATE_CANCELLED {
		t.Errorf("ps = %v, want only zzrrosbgla cancelled (pdxtkgj2zh gone)", ps.Commands)
	}

	got, err := s.sessions.GetSession(t.Context(), &pb.GetSessionRequest{Id: "sweeping-swift"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Commands) != 1 || got.Commands[0].Id != "zzrrosbgla" || got.Commands[0].State != pb.CommandState_COMMAND_STATE_CANCELLED {
		t.Errorf("session status commands = %v, want only zzrrosbgla cancelled", got.Commands)
	}
	if len(got.Workspaces) != 1 || got.Workspaces[0].State != pb.WorkspaceState_WORKSPACE_STATE_IDLE {
		t.Errorf("session status workspaces = %v, want ws1 idle", got.Workspaces)
	}
}

// A canceled CreateWorkspace must still drop its reservation row (using
// context.WithoutCancel), and any pre-existing ghost workspace row that an
// online worker does not have is reconciled by GetSession, CreateWorkspace,
// and checkpoints so session status never shows state: unknown (board 7232).
func TestCanceledAndGhostWorkspacesAreCleanedUpAndReconciled(t *testing.T) {
	started := make(chan struct{})
	released := make(chan string, 1)
	stub := &stubWorker{
		workspaces: map[string]*pb.Workspace{},
		onCreate: func(ctx context.Context, req *pb.CreateWorkspaceRequest) (*pb.Workspace, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		},
		onRelease: func(id string) {
			select {
			case released <- id:
			default:
			}
		},
	}
	s, store := newWithLiveWorker(t, stub)
	if _, err := s.cockpit.PutRepo(t.Context(), &pb.PutRepoRequest{Repo: &pb.Repo{Id: "app", Url: "https://github.com/o/app.git"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.sessions.StartSession(t.Context(), &pb.StartSessionRequest{Id: "calm-fox", Project: "app"}); err != nil {
		t.Fatal(err)
	}
	s.push(&pb.PushEventsRequest{Epoch: "e1", Checkpoint: &pb.Checkpoint{Seq: 1}})
	s.beat()

	// 1. Cancel CreateWorkspace mid-RPC; DropWorkspace must still succeed.
	callCtx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() {
		_, err := s.workspaces.CreateWorkspace(callCtx, &pb.CreateWorkspaceRequest{SessionId: "calm-fox", Repo: "app"})
		errCh <- err
	}()
	<-started
	cancel()
	if err := <-errCh; err == nil {
		t.Fatal("expected canceled CreateWorkspace to return an error")
	}
	// The client returns immediately on context cancellation while the server's
	// handler still runs best-effort ReleaseWorkspace + DropWorkspace; wait
	// deterministically for both to finish before checking the store and API
	// (board 9328).
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for controller to call ReleaseWorkspace after canceled CreateWorkspace")
	}
	select {
	case <-s.createDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for canceled CreateWorkspace handler cleanup to finish")
	}
	// Check the store directly before GetSession so DropWorkspace itself is
	// verified rather than masked by GetSession's ghost reconciliation.
	sessBeforeGet, err := store.Session(t.Context(), "calm-fox")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessBeforeGet.Workspaces) != 0 {
		t.Fatalf("after canceled CreateWorkspace, stored session workspaces = %v, want none", sessBeforeGet.Workspaces)
	}
	got, err := s.sessions.GetSession(t.Context(), &pb.GetSessionRequest{Id: "calm-fox"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Session.Workspaces) != 0 {
		t.Fatalf("after canceled CreateWorkspace, session workspaces = %v, want none", got.Session.Workspaces)
	}

	// 2. Simulate an orphaned ghost row left by a controller crash mid-make
	// (fork_millis IS NULL, released_at IS NULL, worker has no such workspace).
	if err := store.AddWorkspace(t.Context(), "apeah4ub", "calm-fox", asahi.NodeID, "laptop", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err = s.sessions.GetSession(t.Context(), &pb.GetSessionRequest{Id: "calm-fox"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Session.Workspaces) != 0 {
		t.Errorf("GetSession should reconcile ghost workspace apeah4ub, got %v", got.Session.Workspaces)
	}

	// 3. Same ghost row present during CreateWorkspace placement must be
	// reconciled instead of failing with NOT_FOUND.
	stub.onCreate = nil
	if err := store.AddWorkspace(t.Context(), "ghostws2", "calm-fox", asahi.NodeID, "laptop", time.Now()); err != nil {
		t.Fatal(err)
	}
	ws, err := s.workspaces.CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{SessionId: "calm-fox", Repo: "app"})
	if err != nil {
		t.Fatalf("CreateWorkspace with stale ghost row failed: %v", err)
	}
	if ws.Id == "ghostws2" {
		t.Errorf("CreateWorkspace returned ghost workspace ID %q", ws.Id)
	}

	// 4. A checkpoint from the worker also cleans up ghost rows that never
	// reached node_state.
	if err := store.AddWorkspace(t.Context(), "ghostws3", "calm-fox", asahi.NodeID, "laptop", time.Now()); err != nil {
		t.Fatal(err)
	}
	s.push(&pb.PushEventsRequest{Epoch: "e1", Checkpoint: &pb.Checkpoint{Seq: 2, Workspaces: []*pb.Workspace{ws}}})
	sess, err := store.Session(t.Context(), "calm-fox")
	if err != nil {
		t.Fatal(err)
	}
	for _, sw := range sess.Workspaces {
		if sw.Id == "ghostws3" {
			t.Errorf("checkpoint left ghost workspace %q in session workspaces", sw.Id)
		}
	}
}
