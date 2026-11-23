package controller_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/controller"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/tailnet/tailnettest"
)

// forgettingWorker answers GetCommand with NOT_FOUND ("no command <id> on this
// node"), reproducing a live worker that has forgotten an ended command or
// restarted without its database while keeping the command's log on disk.
type forgettingWorker struct {
	hotbedv1connect.UnimplementedWorkspaceServiceHandler
	calls *atomic.Int64
}

func (w forgettingWorker) GetCommand(_ context.Context, req *pb.GetCommandRequest) (*pb.Command, error) {
	if w.calls != nil {
		w.calls.Add(1)
	}
	return nil, api.ToConnect(api.Errorf(api.CodeNotFound, "no command %s on this node", req.Id))
}

type logLookupEnv struct {
	store        *controller.Store
	dbPath       string
	workerHTTP   *httptest.Server
	workerAddr   string
	workerCalls  *atomic.Int64
	workerCl     hotbedv1connect.ClusterServiceClient
	sessionsCl   hotbedv1connect.SessionServiceClient
	workspacesCl hotbedv1connect.WorkspaceServiceClient
}

func newLogLookupEnv(t *testing.T) *logLookupEnv {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "hotbed.db")
	store, err := controller.OpenStore(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	calls := &atomic.Int64{}
	wMux := http.NewServeMux()
	wSrv := connect.NewServer()
	hotbedv1connect.RegisterWorkspaceServiceHandler(wSrv, forgettingWorker{calls: calls})
	connecthttp.Mount(wMux, wSrv)
	workerHTTP := httptest.NewServer(wMux)
	t.Cleanup(workerHTTP.Close)
	workerAddr := strings.TrimPrefix(workerHTTP.URL, "http://")

	tn := tailnettest.New(ubu1)
	ctrlHTTP := httptest.NewServer(controller.NewServer(controller.ServerConfig{
		Store:      store,
		Identifier: tn,
		Self:       ubu1.Addrs,
		Cluster:    statedir.ControllerConfig{ClusterID: cluster, Owner: "ramon@", Port: 7120},
		WorkerAddr: func(string) string { return workerAddr },
	}).Handler())
	t.Cleanup(ctrlHTTP.Close)

	ctrlAddr := strings.TrimPrefix(ctrlHTTP.URL, "http://")
	workerCl, _, err := api.Controller(ctrlAddr, tn.Client(asahi))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workerCl.Join(t.Context(), &pb.JoinRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := workerCl.Heartbeat(t.Context(), &pb.HeartbeatRequest{ClusterId: cluster}); err != nil {
		t.Fatal(err)
	}
	conn, err := api.Dial(ctrlAddr, api.DefaultPort, tn.Client(laptop))
	if err != nil {
		t.Fatal(err)
	}
	return &logLookupEnv{
		store:        store,
		dbPath:       dbPath,
		workerHTTP:   workerHTTP,
		workerAddr:   workerAddr,
		workerCalls:  calls,
		workerCl:     workerCl,
		sessionsCl:   hotbedv1connect.NewSessionServiceClient(conn),
		workspacesCl: hotbedv1connect.NewWorkspaceServiceClient(conn),
	}
}

func execSQL(t *testing.T, dbPath, query string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func newControllerWithForgettingWorker(t *testing.T) (
	store *controller.Store,
	workerAddr string,
	workerCl hotbedv1connect.ClusterServiceClient,
	sessionsCl hotbedv1connect.SessionServiceClient,
	workspacesCl hotbedv1connect.WorkspaceServiceClient,
) {
	t.Helper()
	env := newLogLookupEnv(t)
	return env.store, env.workerAddr, env.workerCl, env.sessionsCl, env.workspacesCl
}

func baseEvent(seq int64, what string, b *pb.Base) *pb.Event {
	return &pb.Event{
		Seq: seq,
		At:  1000 + seq,
		Change: &pb.Event_Base{
			Base: &pb.BaseChange{
				What: what,
				Base: proto.Clone(b).(*pb.Base),
			},
		},
	}
}

// Regression test for board 7951 AC #2 and board 7917 AC #2: when a worker has
// forgotten a command (base freshens never enter history, or a worker lost its
// DB before emitting FinishedCommand), controller GetCommand must still return
// a command record carrying NodeAddr (from history, node_state bases, or the
// commands table) so `hotbed logs <id>` reaches the worker and streams its
// retained on-disk log file — while still returning NOT_FOUND for truly unknown
// command IDs.
func TestGetCommandFallbackAfterWorkerForgetsFreshenAndLostCommands(t *testing.T) {
	ctx := t.Context()
	store, workerAddr, workerCl, sessionsCl, workspacesCl := newControllerWithForgettingWorker(t)

	if _, err := sessionsCl.StartSession(ctx, &pb.StartSessionRequest{Id: "artistic-crab", Remote: "github.com/o/app"}); err != nil {
		t.Fatal(err)
	}

	push := func(req *pb.PushEventsRequest) {
		t.Helper()
		req.ClusterId = cluster
		if _, err := workerCl.PushEvents(ctx, req); err != nil {
			t.Fatal(err)
		}
	}

	builtAt := time.Now().UnixMilli()
	failedBase := &pb.Base{
		Id:         "cladefs-g49",
		Project:    "cladefs",
		NodeId:     asahi.NodeID,
		Generation: 49,
		State:      pb.BaseState_BASE_STATE_FAILED,
		BuiltAt:    &builtAt,
		Command:    "f-cladefs-g49-9fc5a2",
		Steps: []*pb.AppliedStep{
			{Name: "deps", Ran: true, Ok: true},
			{Name: "build", Ran: true, Ok: false},
		},
	}
	warmBase := &pb.Base{
		Id:         "cladefs-g48",
		Project:    "cladefs",
		NodeId:     asahi.NodeID,
		Generation: 48,
		State:      pb.BaseState_BASE_STATE_WARM,
		BuiltAt:    &builtAt,
		Command:    "f-cladefs-g48-111111",
	}
	collectedBase := &pb.Base{
		Id:         "cladefs-g10",
		Project:    "cladefs",
		NodeId:     asahi.NodeID,
		Generation: 10,
		State:      pb.BaseState_BASE_STATE_FAILED,
		BuiltAt:    &builtAt,
		Command:    "f-cladefs-g10-1eb106",
	}

	// Start epoch e1 and push:
	// - failed base g49 whose freshen command was later forgotten (not in history)
	// - superseded & collected base g10 whose freshen command was forgotten and base collected (only in commands table)
	// - sessionless command in history (SessionId == "", project_id = NULL)
	// - session command hlvz43qq that never emitted FinishedCommand before worker DB loss
	push(&pb.PushEventsRequest{Epoch: "e1", Checkpoint: &pb.Checkpoint{}})
	push(&pb.PushEventsRequest{
		Epoch: "e1",
		Events: []*pb.Event{
			baseEvent(1, "failed", failedBase),
			cmdEvent(2, "exited", &pb.Command{
				Id: "f-cladefs-g49-9fc5a2", NodeId: asahi.NodeID, Project: "cladefs",
				Kind: "freshen", Argv: "freshen cladefs g49", State: pb.CommandState_COMMAND_STATE_FAILED,
			}, nil),
			cmdEvent(3, "forgotten", &pb.Command{Id: "f-cladefs-g49-9fc5a2", NodeId: asahi.NodeID}, nil),
			baseEvent(4, "failed", collectedBase),
			cmdEvent(5, "exited", &pb.Command{
				Id: "f-cladefs-g10-1eb106", NodeId: asahi.NodeID, Project: "cladefs",
				Kind: "freshen", Argv: "freshen cladefs g10", State: pb.CommandState_COMMAND_STATE_FAILED,
			}, nil),
			cmdEvent(6, "forgotten", &pb.Command{Id: "f-cladefs-g10-1eb106", NodeId: asahi.NodeID}, nil),
			baseEvent(7, "collected", collectedBase),
			cmdEvent(8, "exited", &pb.Command{
				Id: "f-cladefs-hist-222222", NodeId: asahi.NodeID, Project: "cladefs",
				Kind: "freshen", Argv: "freshen cladefs hist", State: pb.CommandState_COMMAND_STATE_FAILED,
			}, &pb.FinishedCommand{
				Id: "f-cladefs-hist-222222", NodeId: asahi.NodeID, RepoId: "cladefs",
				Argv: "freshen cladefs hist", Kind: "freshen", State: pb.CommandState_COMMAND_STATE_FAILED,
				EndedAt: builtAt, Reason: "exit 2",
			}),
			cmdEvent(9, "forgotten", &pb.Command{Id: "f-cladefs-hist-222222", NodeId: asahi.NodeID}, nil),
			cmdEvent(10, "admitted", &pb.Command{
				Id: "hlvz43qq", WorkspaceId: "nrykg4th", NodeId: asahi.NodeID,
				SessionId: "artistic-crab", Argv: "cargo test", State: pb.CommandState_COMMAND_STATE_RUNNING,
			}, nil),
		},
	})

	// Push a new-epoch checkpoint that:
	// - keeps failedBase (g49) and adds warmBase (g48) & checkpointOnlyBase (g50, NEVER in commands table!)
	// - drops hlvz43qq (simulating worker DB reset before FinishedCommand was emitted)
	checkpointOnlyBase := &pb.Base{
		Id:         "cladefs-g50",
		Project:    "cladefs",
		NodeId:     asahi.NodeID,
		Generation: 50,
		State:      pb.BaseState_BASE_STATE_FAILED,
		BuiltAt:    &builtAt,
		Command:    "f-cladefs-g50-abcdef",
		Steps:      []*pb.AppliedStep{{Name: "test", Ran: true, Ok: false}},
	}
	push(&pb.PushEventsRequest{
		Epoch: "e2",
		Checkpoint: &pb.Checkpoint{
			Seq:   1,
			Bases: []*pb.Base{failedBase, warmBase, checkpointOnlyBase},
		},
	})

	t.Run("failed base freshen forgotten by worker", func(t *testing.T) {
		got, err := workspacesCl.GetCommand(ctx, &pb.GetCommandRequest{Id: "f-cladefs-g49-9fc5a2"})
		if err != nil {
			t.Fatalf("GetCommand failed: %v", err)
		}
		if got.Id != "f-cladefs-g49-9fc5a2" || got.NodeId != asahi.NodeID || got.NodeAddr != workerAddr {
			t.Errorf("got id=%q node=%q addr=%q, want f-cladefs-g49-9fc5a2 %s %s", got.Id, got.NodeId, got.NodeAddr, asahi.NodeID, workerAddr)
		}
		if got.Project != "cladefs" || got.Kind != "freshen" || got.State != pb.CommandState_COMMAND_STATE_FAILED {
			t.Errorf("got project=%q kind=%q state=%v", got.Project, got.Kind, got.State)
		}
		if !strings.Contains(got.Reason, "cladefs-g49") || !strings.Contains(got.Reason, "build") {
			t.Errorf("reason = %q, want base and failed step", got.Reason)
		}
	})

	t.Run("checkpoint-only base freshen absent from commands and history", func(t *testing.T) {
		// Verify Store.Command resolves via BaseByCommand too (used by GetLogLines).
		sess, node, err := store.Command(ctx, "f-cladefs-g50-abcdef")
		if err != nil || sess != "" || node != asahi.NodeID {
			t.Fatalf("Store.Command = (%q, %q, %v), want (\"\", %q, nil)", sess, node, err, asahi.NodeID)
		}
		got, err := workspacesCl.GetCommand(ctx, &pb.GetCommandRequest{Id: "f-cladefs-g50-abcdef"})
		if err != nil {
			t.Fatalf("GetCommand failed: %v", err)
		}
		if got.NodeAddr != workerAddr || got.Project != "cladefs" || got.Kind != "freshen" || got.State != pb.CommandState_COMMAND_STATE_FAILED {
			t.Errorf("got = %+v", got)
		}
		if !strings.Contains(got.Reason, "test") {
			t.Errorf("reason = %q, want failed step 'test'", got.Reason)
		}
	})

	t.Run("warm base freshen synthesizes succeeded", func(t *testing.T) {
		got, err := workspacesCl.GetCommand(ctx, &pb.GetCommandRequest{Id: "f-cladefs-g48-111111"})
		if err != nil {
			t.Fatalf("GetCommand failed: %v", err)
		}
		if got.NodeAddr != workerAddr || got.Kind != "freshen" || got.State != pb.CommandState_COMMAND_STATE_SUCCEEDED {
			t.Errorf("got = %+v, want succeeded freshen at %s", got, workerAddr)
		}
	})

	t.Run("superseded collected base freshen resolved via commands table", func(t *testing.T) {
		got, err := workspacesCl.GetCommand(ctx, &pb.GetCommandRequest{Id: "f-cladefs-g10-1eb106"})
		if err != nil {
			t.Fatalf("GetCommand failed: %v", err)
		}
		if got.NodeId != asahi.NodeID || got.NodeAddr != workerAddr || got.Kind != "freshen" || api.Live(got.State) {
			t.Errorf("got = %+v, want non-live freshen at %s", got, workerAddr)
		}
	})

	t.Run("sessionless command in history (project_id NULL)", func(t *testing.T) {
		got, err := workspacesCl.GetCommand(ctx, &pb.GetCommandRequest{Id: "f-cladefs-hist-222222"})
		if err != nil {
			t.Fatalf("GetCommand failed: %v", err)
		}
		if got.NodeAddr != workerAddr || got.Argv != "freshen cladefs hist" || got.Reason != "exit 2" {
			t.Errorf("got = %+v", got)
		}
	})

	t.Run("session command lost on worker DB reset resolved via commands table", func(t *testing.T) {
		got, err := workspacesCl.GetCommand(ctx, &pb.GetCommandRequest{Id: "hlvz43qq"})
		if err != nil {
			t.Fatalf("GetCommand failed: %v", err)
		}
		if got.NodeId != asahi.NodeID || got.NodeAddr != workerAddr {
			t.Errorf("got node=%q addr=%q, want %s %s", got.NodeId, got.NodeAddr, asahi.NodeID, workerAddr)
		}
		if got.SessionId != "artistic-crab" || got.Project != "app" || api.Live(got.State) {
			t.Errorf("got session=%q project=%q state=%v, want artistic-crab / app / non-live", got.SessionId, got.Project, got.State)
		}
	})

	t.Run("truly unknown command still returns NOT_FOUND", func(t *testing.T) {
		if _, err := workspacesCl.GetCommand(ctx, &pb.GetCommandRequest{Id: "does-not-exist"}); api.CodeOf(err) != api.CodeNotFound {
			t.Errorf("GetCommand(unknown) = %v, want NOT_FOUND", err)
		}
	})
}

// Regression test for board 9665 / Review #55 finding #218: when a command
// exists only in the commands routing table (or as a non-terminal building base
// with no cached command view) and its worker is unreachable (dial/connect
// error), GetCommand must preserve the retryable CodeWorkerUnreachable error
// instead of fabricating COMMAND_STATE_FAILED.
func TestGetCommandPreservesWorkerUnreachableWhenOnlyRoutingRowSurvives(t *testing.T) {
	ctx := t.Context()
	env := newLogLookupEnv(t)

	if _, err := env.sessionsCl.StartSession(ctx, &pb.StartSessionRequest{Id: "calm-otter", Remote: "github.com/o/app"}); err != nil {
		t.Fatal(err)
	}
	// Record a command ONLY in the commands routing table (simulating a worker
	// disconnect before its admitted event arrived).
	if err := env.store.AddCommand(ctx, "routing-only-cmd", "calm-otter", asahi.NodeID, "ws-1", time.Now()); err != nil {
		t.Fatal(err)
	}

	builtAt := time.Now().UnixMilli()
	buildingBase := &pb.Base{
		Id:         "app-g2",
		Project:    "app",
		NodeId:     asahi.NodeID,
		Generation: 2,
		State:      pb.BaseState_BASE_STATE_BUILDING,
		Command:    "f-app-g2-building",
	}
	warmBase := &pb.Base{
		Id:         "app-g1",
		Project:    "app",
		NodeId:     asahi.NodeID,
		Generation: 1,
		State:      pb.BaseState_BASE_STATE_WARM,
		BuiltAt:    &builtAt,
		Command:    "f-app-g1-warm",
	}
	if _, err := env.workerCl.PushEvents(ctx, &pb.PushEventsRequest{
		ClusterId: cluster,
		Epoch:     "e1",
		Checkpoint: &pb.Checkpoint{
			Seq:   1,
			Bases: []*pb.Base{buildingBase, warmBase},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Shut down the worker HTTP listener while its heartbeat is still fresh so
	// controller dials it and hits a real dial/connect error.
	env.workerHTTP.Close()

	t.Run("routing row only returns retryable WORKER_UNREACHABLE", func(t *testing.T) {
		got, err := env.workspacesCl.GetCommand(ctx, &pb.GetCommandRequest{Id: "routing-only-cmd"})
		if err == nil {
			t.Fatalf("GetCommand = %+v, want retryable WORKER_UNREACHABLE error", got)
		}
		ae := api.FromConnect(err)
		if ae.Code != api.CodeWorkerUnreachable || !ae.Retryable {
			t.Errorf("GetCommand error = %+v, want retryable %s", ae, api.CodeWorkerUnreachable)
		}
	})

	t.Run("non-terminal building base without command view returns retryable WORKER_UNREACHABLE", func(t *testing.T) {
		got, err := env.workspacesCl.GetCommand(ctx, &pb.GetCommandRequest{Id: "f-app-g2-building"})
		if err == nil {
			t.Fatalf("GetCommand = %+v, want retryable WORKER_UNREACHABLE error", got)
		}
		ae := api.FromConnect(err)
		if ae.Code != api.CodeWorkerUnreachable || !ae.Retryable {
			t.Errorf("GetCommand error = %+v, want retryable %s", ae, api.CodeWorkerUnreachable)
		}
	})

	t.Run("terminal warm base still answers while worker is unreachable", func(t *testing.T) {
		got, err := env.workspacesCl.GetCommand(ctx, &pb.GetCommandRequest{Id: "f-app-g1-warm"})
		if err != nil {
			t.Fatalf("GetCommand(warm base) failed: %v", err)
		}
		if got.State != pb.CommandState_COMMAND_STATE_SUCCEEDED || got.NodeId != asahi.NodeID {
			t.Errorf("got = %+v, want SUCCEEDED on %s", got, asahi.NodeID)
		}
	})
}

// Regression test for board 9665 / Review #55 finding #219: BASE_STATE_STALE
// must only map to COMMAND_STATE_SUCCEEDED when BuiltAt proves completion and
// no step failed; superseded-while-building stale bases map to CANCELLED, and
// stale bases with a failed step map to FAILED.
func TestGetCommandBaseStateStaleMapping(t *testing.T) {
	ctx := t.Context()
	env := newLogLookupEnv(t)

	builtAt := time.Now().UnixMilli()
	staleBuilt := &pb.Base{
		Id:         "app-g1",
		Project:    "app",
		NodeId:     asahi.NodeID,
		Generation: 1,
		State:      pb.BaseState_BASE_STATE_STALE,
		BuiltAt:    &builtAt,
		Command:    "f-app-g1-stale-built",
		Steps:      []*pb.AppliedStep{{Name: "deps", Ran: true, Ok: true}},
	}
	staleUnbuilt := &pb.Base{
		Id:         "app-g2",
		Project:    "app",
		NodeId:     asahi.NodeID,
		Generation: 2,
		State:      pb.BaseState_BASE_STATE_STALE,
		Command:    "f-app-g2-stale-unbuilt",
	}
	staleFailedStep := &pb.Base{
		Id:         "app-g3",
		Project:    "app",
		NodeId:     asahi.NodeID,
		Generation: 3,
		State:      pb.BaseState_BASE_STATE_STALE,
		BuiltAt:    &builtAt,
		Command:    "f-app-g3-stale-failed",
		Steps: []*pb.AppliedStep{
			{Name: "deps", Ran: true, Ok: true},
			{Name: "compile", Ran: true, Ok: false},
		},
	}
	buildingForgotten := &pb.Base{
		Id:         "app-g4",
		Project:    "app",
		NodeId:     asahi.NodeID,
		Generation: 4,
		State:      pb.BaseState_BASE_STATE_BUILDING,
		Command:    "f-app-g4-building",
	}

	if _, err := env.workerCl.PushEvents(ctx, &pb.PushEventsRequest{
		ClusterId: cluster,
		Epoch:     "e1",
		Checkpoint: &pb.Checkpoint{
			Seq:   1,
			Bases: []*pb.Base{staleBuilt, staleUnbuilt, staleFailedStep, buildingForgotten},
		},
	}); err != nil {
		t.Fatal(err)
	}

	t.Run("stale base with BuiltAt maps to SUCCEEDED", func(t *testing.T) {
		got, err := env.workspacesCl.GetCommand(ctx, &pb.GetCommandRequest{Id: "f-app-g1-stale-built"})
		if err != nil {
			t.Fatalf("GetCommand failed: %v", err)
		}
		if got.State != pb.CommandState_COMMAND_STATE_SUCCEEDED {
			t.Errorf("state = %v, want SUCCEEDED", got.State)
		}
	})

	t.Run("stale base without BuiltAt maps to CANCELLED", func(t *testing.T) {
		got, err := env.workspacesCl.GetCommand(ctx, &pb.GetCommandRequest{Id: "f-app-g2-stale-unbuilt"})
		if err != nil {
			t.Fatalf("GetCommand failed: %v", err)
		}
		if got.State != pb.CommandState_COMMAND_STATE_CANCELLED {
			t.Errorf("state = %v, want CANCELLED", got.State)
		}
		if !strings.Contains(got.Reason, "stale") {
			t.Errorf("reason = %q, want explanation that base became stale without completing", got.Reason)
		}
	})

	t.Run("stale base with failed step maps to FAILED even if BuiltAt is set", func(t *testing.T) {
		got, err := env.workspacesCl.GetCommand(ctx, &pb.GetCommandRequest{Id: "f-app-g3-stale-failed"})
		if err != nil {
			t.Fatalf("GetCommand failed: %v", err)
		}
		if got.State != pb.CommandState_COMMAND_STATE_FAILED {
			t.Errorf("state = %v, want FAILED", got.State)
		}
		if !strings.Contains(got.Reason, "compile") {
			t.Errorf("reason = %q, want failed step 'compile'", got.Reason)
		}
	})

	t.Run("building base forgotten by online worker maps to FAILED", func(t *testing.T) {
		before := env.workerCalls.Load()
		got, err := env.workspacesCl.GetCommand(ctx, &pb.GetCommandRequest{Id: "f-app-g4-building"})
		if err != nil {
			t.Fatalf("GetCommand failed: %v", err)
		}
		if env.workerCalls.Load() <= before {
			t.Fatalf("expected controller to query online worker before falling back")
		}
		if got.State != pb.CommandState_COMMAND_STATE_FAILED {
			t.Errorf("state = %v, want FAILED", got.State)
		}
	})
}

// Regression test for board 9665 / Review #55 finding #220: non-NOT_FOUND
// errors from viewCommand (such as corrupt JSON in node_state) must be
// propagated rather than swallowed and masked by baseCommand or knownCommand.
func TestGetCommandPropagatesViewCommandErrors(t *testing.T) {
	ctx := t.Context()
	env := newLogLookupEnv(t)

	builtAt := time.Now().UnixMilli()
	warmBase := &pb.Base{
		Id:         "app-g9",
		Project:    "app",
		NodeId:     asahi.NodeID,
		Generation: 9,
		State:      pb.BaseState_BASE_STATE_WARM,
		BuiltAt:    &builtAt,
		Command:    "f-app-g9-corrupt-view",
	}
	if _, err := env.workerCl.PushEvents(ctx, &pb.PushEventsRequest{
		ClusterId: cluster,
		Epoch:     "e1",
		Checkpoint: &pb.Checkpoint{
			Seq:   1,
			Bases: []*pb.Base{warmBase},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.AddCommand(ctx, "f-app-g9-corrupt-view", "", asahi.NodeID, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	// Inject corrupt JSON for this command's node_state row.
	execSQL(t, env.dbPath, `INSERT INTO node_state (node_id, kind, id, doc) VALUES (?, 'command', ?, '{corrupt-json')`, asahi.NodeID, "f-app-g9-corrupt-view")

	t.Run("online worker NOT_FOUND propagates viewCommand decode error", func(t *testing.T) {
		before := env.workerCalls.Load()
		got, err := env.workspacesCl.GetCommand(ctx, &pb.GetCommandRequest{Id: "f-app-g9-corrupt-view"})
		if err == nil {
			t.Fatalf("GetCommand = %+v, want decode error (must not mask with baseCommand/knownCommand)", got)
		}
		if env.workerCalls.Load() <= before {
			t.Fatalf("expected controller to query online worker before falling back")
		}
		if !strings.Contains(err.Error(), "read command f-app-g9-corrupt-view state") {
			t.Errorf("GetCommand error = %v, want viewCommand decode error", err)
		}
	})

	// Also verify unreachable worker path propagates the viewCommand decode error.
	env.workerHTTP.Close()
	t.Run("unreachable worker propagates viewCommand decode error", func(t *testing.T) {
		got, err := env.workspacesCl.GetCommand(ctx, &pb.GetCommandRequest{Id: "f-app-g9-corrupt-view"})
		if err == nil {
			t.Fatalf("GetCommand = %+v, want decode error", got)
		}
		if !strings.Contains(err.Error(), "read command f-app-g9-corrupt-view state") {
			t.Errorf("GetCommand error = %v, want viewCommand decode error", err)
		}
	})
}

// Regression test for board 9665 / Review #55 finding #221: ViewCommand and
// BaseByCommand use targeted SQL queries rather than decoding every node_state
// row across the cluster, so unrelated corrupt workspace/command docs do not
// interfere with command or base lookups.
func TestGetCommandTargetedNodeStateLookups(t *testing.T) {
	ctx := t.Context()
	env := newLogLookupEnv(t)

	builtAt := time.Now().UnixMilli()
	warmBase := &pb.Base{
		Id:         "app-g7",
		Project:    "app",
		NodeId:     asahi.NodeID,
		Generation: 7,
		State:      pb.BaseState_BASE_STATE_WARM,
		BuiltAt:    &builtAt,
		Command:    "f-app-g7-targeted",
	}
	if _, err := env.workerCl.PushEvents(ctx, &pb.PushEventsRequest{
		ClusterId: cluster,
		Epoch:     "e1",
		Checkpoint: &pb.Checkpoint{
			Seq:   1,
			Bases: []*pb.Base{warmBase},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Plant corrupt docs in unrelated workspace and command rows of node_state.
	// A full-cluster Views() scan would fail decoding them.
	execSQL(t, env.dbPath, `INSERT INTO node_state (node_id, kind, id, doc) VALUES (?, 'workspace', 'ws-bad', '{not-json')`, asahi.NodeID)
	execSQL(t, env.dbPath, `INSERT INTO node_state (node_id, kind, id, doc) VALUES (?, 'command', 'cmd-other-bad', '{not-json')`, asahi.NodeID)

	if _, _, ok, err := env.store.BaseByCommand(ctx, "does-not-exist"); err != nil || ok {
		t.Fatalf("BaseByCommand(unknown) = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
	if _, _, ok, err := env.store.ViewCommand(ctx, "does-not-exist"); err != nil || ok {
		t.Fatalf("ViewCommand(unknown) = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
	if _, err := env.workspacesCl.GetCommand(ctx, &pb.GetCommandRequest{Id: "does-not-exist"}); api.CodeOf(err) != api.CodeNotFound {
		t.Fatalf("GetCommand(unknown) = %v, want NOT_FOUND", err)
	}
	got, err := env.workspacesCl.GetCommand(ctx, &pb.GetCommandRequest{Id: "f-app-g7-targeted"})
	if err != nil {
		t.Fatalf("GetCommand(targeted base) failed: %v", err)
	}
	if got.State != pb.CommandState_COMMAND_STATE_SUCCEEDED || got.NodeId != asahi.NodeID {
		t.Errorf("got = %+v, want SUCCEEDED on %s", got, asahi.NodeID)
	}
}
