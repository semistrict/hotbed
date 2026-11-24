package controller

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	controllerdb "github.com/semistrict/hotbed/internal/controller/db/gen"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/tailnet"
	"github.com/semistrict/hotbed/internal/tailnet/tailnettest"
	"github.com/semistrict/hotbed/internal/update"
	"github.com/semistrict/hotbed/internal/update/updatetest"
)

// countingDBTX wraps a controllerdb.DBTX and counts every SQL call executed.
type countingDBTX struct {
	base    controllerdb.DBTX
	mu      sync.Mutex
	queries []string
}

func (c *countingDBTX) record(q string) {
	c.mu.Lock()
	c.queries = append(c.queries, q)
	c.mu.Unlock()
}

func (c *countingDBTX) reset() {
	c.mu.Lock()
	c.queries = nil
	c.mu.Unlock()
}

func (c *countingDBTX) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queries)
}

func (c *countingDBTX) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.queries))
	copy(out, c.queries)
	return out
}

func (c *countingDBTX) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	c.record(query)
	return c.base.ExecContext(ctx, query, args...)
}

func (c *countingDBTX) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	c.record(query)
	return c.base.PrepareContext(ctx, query)
}

func (c *countingDBTX) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	c.record(query)
	return c.base.QueryContext(ctx, query, args...)
}

func (c *countingDBTX) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	c.record(query)
	return c.base.QueryRowContext(ctx, query, args...)
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(t.Context(), filepath.Join(t.TempDir(), "hotbed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCommandsIndexesExist(t *testing.T) {
	s := openTestStore(t)
	for _, idx := range []string{"commands_session", "commands_created"} {
		var name string
		err := s.db.QueryRowContext(t.Context(), `SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'commands' AND name = ?`, idx).Scan(&name)
		if err != nil {
			t.Errorf("expected index %s on commands table: %v", idx, err)
		}
	}
}

// Verify O(1) SQL queries in RecentSessions, Projects, allSchedules, beat, and
// scoped SessionProjects/views regardless of row counts (board 8011).
func TestQueryCountsAreConstant(t *testing.T) {
	ctx := t.Context()
	s := openTestStore(t)
	now := time.Unix(1795320000, 0)

	// Seed node + 25 projects, 25 live sessions, 100 historical ended sessions,
	// 25 schedules with 15 runs each, and 20 bases/workspaces/commands in node_state.
	node := tailnet.Peer{NodeID: "n1", Hostname: "worker-1", Login: "ramon@", Addr: netip.MustParseAddr("100.64.0.1")}
	if _, err := s.Join(ctx, node, now); err != nil {
		t.Fatal(err)
	}

	const N = 25
	for i := range N {
		pname := "proj-" + string(rune('a'+i))
		r1 := "github.com/acme/" + pname + "-1"
		r2 := "github.com/acme/" + pname + "-2"
		if _, err := s.AddProject(ctx, pname, []string{r2, r1}, &pb.Stamp{At: now.UnixMilli()}); err != nil {
			t.Fatal(err)
		}
		sid := "sess-" + pname
		if _, err := s.StartSession(ctx, sid, "laptop", pname, "", now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := s.AddWorkspace(ctx, "ws-"+pname, sid, node.NodeID, "laptop", now); err != nil {
			t.Fatal(err)
		}
		scName := "sched-" + pname
		if err := s.PutSchedule(ctx, &pb.ScheduledCommand{
			Name: scName, Project: pname, Repo: "r", Cron: "0 2 * * *",
			Command: &pb.RunCommandRequest{Args: []string{"true"}},
		}); err != nil {
			t.Fatal(err)
		}
		for r := range 15 {
			ended := now.Add(time.Duration(r) * time.Minute).UnixMilli()
			run := &pb.ScheduledRun{Schedule: scName, StartedAt: ended, EndedAt: &ended, Outcome: pb.RunOutcome_RUN_OUTCOME_SUCCEEDED}
			if r == 14 {
				run.SessionId = sid
				run.EndedAt = nil
				run.Outcome = pb.RunOutcome_RUN_OUTCOME_RUNNING
			}
			if err := s.AddRun(ctx, run); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Add 100 old ended sessions that must NOT be returned by scoped SessionProjects.
	oldTime := now.Add(-48 * time.Hour)
	for i := range 100 {
		sid := "old-sess-" + string(rune('a'+i%26)) + "-" + string(rune('0'+i/26))
		if _, err := s.StartSession(ctx, sid, "laptop", "proj-a", "", oldTime); err != nil {
			t.Fatal(err)
		}
		if err := s.EndSession(ctx, sid, "done", oldTime.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	// Populate node_state with 10 bases, all N workspaces, and commands in various states.
	cp := &pb.Checkpoint{Seq: 1}
	for i := range 10 {
		cp.Bases = append(cp.Bases, &pb.Base{Id: "base-" + string(rune('a'+i)), Project: "proj-a", State: pb.BaseState_BASE_STATE_WARM})
	}
	for i := range N {
		cp.Workspaces = append(cp.Workspaces, &pb.Workspace{Id: "ws-proj-" + string(rune('a'+i)), SessionId: "sess-proj-" + string(rune('a'+i)), NodeId: node.NodeID, State: pb.WorkspaceState_WORKSPACE_STATE_IDLE})
	}
	states := []pb.CommandState{
		pb.CommandState_COMMAND_STATE_RUNNING,
		pb.CommandState_COMMAND_STATE_RUNNING,
		pb.CommandState_COMMAND_STATE_RUNNING,
		pb.CommandState_COMMAND_STATE_QUEUED,
		pb.CommandState_COMMAND_STATE_QUEUED,
		pb.CommandState_COMMAND_STATE_PARKED,
		pb.CommandState_COMMAND_STATE_PAUSED,
		pb.CommandState_COMMAND_STATE_SUCCEEDED,
		pb.CommandState_COMMAND_STATE_FAILED,
	}
	for i, st := range states {
		cp.Commands = append(cp.Commands, &pb.Command{
			Id: "cmd-" + string(rune('a'+i)), SessionId: "sess-proj-a", NodeId: node.NodeID, State: st,
		})
	}
	if _, err := s.Push(ctx, node.NodeID, &pb.PushEventsRequest{Epoch: "e1", Checkpoint: cp}, now, nil); err != nil {
		t.Fatal(err)
	}

	// Wrap store queries with a counter.
	counter := &countingDBTX{base: s.db}
	s.q = controllerdb.New(counter)
	srv := NewServer(ServerConfig{Store: s, Now: func() time.Time { return now }})

	// 1. RecentSessions: O(1) queries (3: sessions, schedule_status, workspaces).
	counter.reset()
	recent, err := s.RecentSessions(ctx, now.Add(-24*time.Hour), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != N {
		t.Fatalf("RecentSessions returned %d, want %d", len(recent), N)
	}
	if got := counter.count(); got > 3 {
		t.Errorf("RecentSessions ran %d SQL queries for %d sessions, want <= 3", got, N)
	}
	for _, sess := range recent {
		if len(sess.Workspaces) != 1 || len(sess.WorkspaceIds) != 1 {
			t.Errorf("session %s workspaces = %v, want 1", sess.Id, sess.Workspaces)
		}
		if sess.Schedule == "" || !sess.ScheduleRunning {
			t.Errorf("session %s schedule = %q running=%v, want active schedule", sess.Id, sess.Schedule, sess.ScheduleRunning)
		}
	}

	// 2. Projects: O(1) queries (2: projects, project_repos).
	counter.reset()
	projs, err := s.Projects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projs) != N {
		t.Fatalf("Projects returned %d, want %d", len(projs), N)
	}
	if got := counter.count(); got > 2 {
		t.Errorf("Projects ran %d SQL queries for %d projects, want <= 2", got, N)
	}
	if len(projs[0].Repos) != 2 || projs[0].Repos[0] > projs[0].Repos[1] {
		t.Errorf("project %s repos = %v, want 2 sorted repos", projs[0].Name, projs[0].Repos)
	}

	// 3. allSchedules: O(1) queries (2: schedules, recent runs window query).
	counter.reset()
	scheds, err := srv.allSchedules(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(scheds) != N {
		t.Fatalf("allSchedules returned %d, want %d", len(scheds), N)
	}
	if got := counter.count(); got > 2 {
		t.Errorf("allSchedules ran %d SQL queries for %d schedules, want <= 2", got, N)
	}
	for _, sc := range scheds {
		if len(sc.Runs) != runsListed {
			t.Errorf("schedule %s has %d runs, want %d", sc.Name, len(sc.Runs), runsListed)
		}
		for i := 1; i < len(sc.Runs); i++ {
			if sc.Runs[i-1].Id <= sc.Runs[i].Id {
				t.Errorf("schedule %s runs not descending: %d <= %d", sc.Name, sc.Runs[i-1].Id, sc.Runs[i].Id)
			}
		}
	}

	// 4. beat(): counts live commands directly in SQL without loadView().
	counter.reset()
	hb := &pb.HeartbeatRequest{
		Resources: &pb.Resources{MemoryBytes: 16 << 30},
		Usage:     &pb.NodeUsage{CpuBusy: 0.5, MemoryUsedBytes: 8 << 30},
	}
	if err := beat(ctx, controllerdb.New(counter), node.NodeID, hb, now); err != nil {
		t.Fatal(err)
	}
	if got := counter.count(); got != 3 {
		t.Errorf("beat ran %d SQL queries, want 3 (heartbeat, count, sample)", got)
	}
	for _, q := range counter.snapshot() {
		if strings.Contains(q, "ListNodeStateForNode") {
			t.Errorf("beat must not call loadView/ListNodeStateForNode, saw:\n%s", q)
		}
	}
	samples, err := s.Samples(ctx, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].running != 3 || samples[0].queued != 2 || samples[0].parked != 2 {
		t.Errorf("beat sample = %+v, want running=3 queued=2 parked=2", samples)
	}

	// 5. SessionProjects excludes historical ended sessions not in node_state,
	// and views() only queries session IDs referenced by node_state.
	sp, err := s.SessionProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sp) != N {
		t.Errorf("SessionProjects returned %d entries, want only %d live/active (excluding 100 old ended)", len(sp), N)
	}
	counter.reset()
	nvs, err := srv.views(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nvs) != 1 || len(nvs[0].workspaces) != N || nvs[0].workspaces[0].Project != "proj-a" {
		t.Errorf("views() = %+v, want %d workspaces with project populated", nvs, N)
	}
	for _, q := range counter.snapshot() {
		if strings.Contains(q, "ListSessionProjects :") {
			t.Errorf("views() should use ListSessionProjectsByIDs, not full ListSessionProjects")
		}
	}
}

func TestGetSnapshotSamplesCache(t *testing.T) {
	ctx := t.Context()
	s := openTestStore(t)
	cur := time.Unix(1795320000, 0)
	node := tailnet.Peer{NodeID: "n1", Hostname: "worker-1", Login: "ramon@"}
	if _, err := s.Join(ctx, node, cur); err != nil {
		t.Fatal(err)
	}
	if err := s.Beat(ctx, node.NodeID, &pb.HeartbeatRequest{OomKills: 1}, cur); err != nil {
		t.Fatal(err)
	}

	counter := &countingDBTX{base: s.db}
	s.q = controllerdb.New(counter)
	srv := NewServer(ServerConfig{
		Store: s,
		Now:   func() time.Time { return cur },
	})

	countSamplesQueries := func() int {
		n := 0
		for _, q := range counter.snapshot() {
			if strings.Contains(q, "ListSamplesSince") {
				n++
			}
		}
		return n
	}

	// First GetSnapshot populates cache.
	counter.reset()
	snap1, err := srv.GetSnapshot(ctx, &pb.GetSnapshotRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := countSamplesQueries(); got != 1 {
		t.Fatalf("first GetSnapshot ran %d ListSamplesSince queries, want 1", got)
	}
	if len(snap1.Series[node.NodeID].GetSamples()) != 1 {
		t.Fatalf("snap1 samples = %v, want 1", snap1.Series)
	}

	// 1s-spaced cockpit polls with unchanged samplesGen hit the cache (0 ListSamplesSince queries).
	for range 5 {
		cur = cur.Add(time.Second)
		counter.reset()
		if _, err := srv.GetSnapshot(ctx, &pb.GetSnapshotRequest{}); err != nil {
			t.Fatal(err)
		}
		if got := countSamplesQueries(); got != 0 {
			t.Errorf("1s-spaced cached GetSnapshot ran %d ListSamplesSince queries, want 0", got)
		}
	}

	// New heartbeat invalidates cache immediately even within TTL.
	cur = cur.Add(time.Second)
	if err := s.Beat(ctx, node.NodeID, &pb.HeartbeatRequest{OomKills: 3}, cur); err != nil {
		t.Fatal(err)
	}
	counter.reset()
	snap2, err := srv.GetSnapshot(ctx, &pb.GetSnapshotRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := countSamplesQueries(); got != 1 {
		t.Errorf("post-beat GetSnapshot ran %d ListSamplesSince queries, want 1", got)
	}
	if snap2.Totals24H.OomKills != 2 {
		t.Errorf("snap2 OomKills = %d, want 2", snap2.Totals24H.OomKills)
	}

	// Advancing past samplesCacheTTL refreshes cache.
	cur = cur.Add(samplesCacheTTL + time.Second)
	counter.reset()
	if _, err := srv.GetSnapshot(ctx, &pb.GetSnapshotRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := countSamplesQueries(); got != 1 {
		t.Errorf("post-TTL GetSnapshot ran %d ListSamplesSince queries, want 1", got)
	}

	// Concurrent callers on a cold/expired cache coalesce into a single Samples query.
	const callers = 8
	var samplesCount atomic.Int32
	var eventsCount atomic.Int32
	samplesEntered := make(chan struct{})
	releaseSamples := make(chan struct{})
	hook := &hookDBTX{
		base: s.db,
		onQuery: func(q string) error {
			if strings.Contains(q, "ListRecentEvents") {
				eventsCount.Add(1)
			}
			if strings.Contains(q, "ListSamplesSince") {
				if samplesCount.Add(1) == 1 {
					close(samplesEntered)
				}
				<-releaseSamples
			}
			return nil
		},
	}
	s.q = controllerdb.New(hook)
	cur = cur.Add(samplesCacheTTL + time.Second)

	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap, err := srv.GetSnapshot(ctx, &pb.GetSnapshotRequest{})
			if err != nil {
				errs <- err
				return
			}
			if snap.Totals24H.OomKills != 2 {
				errs <- errors.New("unexpected OomKills in coalesced snapshot")
			}
		}()
	}
	<-samplesEntered
	// Wait until all callers have passed ListRecentEvents and reached cachedSamples.
	deadline := time.Now().Add(2 * time.Second)
	for eventsCount.Load() < callers && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(releaseSamples)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent GetSnapshot failed: %v", err)
	}
	if got := samplesCount.Load(); got != 1 {
		t.Errorf("%d concurrent GetSnapshot calls ran %d ListSamplesSince queries, want 1", callers, got)
	}
}

func TestDefaultWorkerClientTimeouts(t *testing.T) {
	srv := NewServer(ServerConfig{})
	hc := srv.cfg.Workers
	if hc == nil {
		t.Fatal("default Workers client is nil")
	}
	if hc.Timeout != 0 {
		t.Errorf("Workers.Timeout = %v, want 0 so streaming RPCs are not cut off", hc.Timeout)
	}
	tr, ok := hc.Transport.(*http.Transport)
	if !ok || tr == nil {
		t.Fatalf("Workers.Transport = %T, want *http.Transport", hc.Transport)
	}
	if tr.ResponseHeaderTimeout != 0 {
		t.Errorf("ResponseHeaderTimeout = %v, want 0 so quiet FollowLog streams and slow CreateWorkspace calls do not expire", tr.ResponseHeaderTimeout)
	}
	if tr.TLSHandshakeTimeout != 10*time.Second {
		t.Errorf("TLSHandshakeTimeout = %v, want 10s", tr.TLSHandshakeTimeout)
	}
	if tr.IdleConnTimeout != 90*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 90s", tr.IdleConnTimeout)
	}
	if tr.DialContext == nil {
		t.Errorf("DialContext is nil, want dialer with timeout")
	}
}

type blockingWorker struct {
	hotbedv1connect.UnimplementedWorkspaceServiceHandler
	mu          sync.Mutex
	onCreate    func(context.Context, *pb.CreateWorkspaceRequest) (*pb.Workspace, error)
	onGet       func(context.Context, *pb.GetWorkspaceRequest) (*pb.Workspace, error)
	onRun       func(context.Context, *pb.RunCommandRequest) (*pb.Command, error)
	onRelease   func(context.Context, *pb.ReleaseWorkspaceRequest) (*pb.ReleaseWorkspaceResponse, error)
	onStop      func(context.Context, *pb.StopCommandRequest) (*pb.Command, error)
	onTrace     func(context.Context, *pb.TraceCommandRequest, hotbedv1connect.WorkspaceServiceTraceCommandServerStream) error
	stoppedCmds []string
	stoppedReqs []*pb.StopCommandRequest
	workspaces  map[string]*pb.Workspace
}

func (w *blockingWorker) CreateWorkspace(ctx context.Context, req *pb.CreateWorkspaceRequest) (*pb.Workspace, error) {
	if w.onCreate != nil {
		return w.onCreate(ctx, req)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.workspaces == nil {
		w.workspaces = map[string]*pb.Workspace{}
	}
	ws := &pb.Workspace{Id: req.Id, SessionId: req.SessionId, State: pb.WorkspaceState_WORKSPACE_STATE_IDLE}
	w.workspaces[req.Id] = ws
	return proto.Clone(ws).(*pb.Workspace), nil
}

func (w *blockingWorker) GetWorkspace(ctx context.Context, req *pb.GetWorkspaceRequest) (*pb.Workspace, error) {
	if w.onGet != nil {
		return w.onGet(ctx, req)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	ws, ok := w.workspaces[req.Id]
	if !ok {
		return nil, api.ToConnect(api.Errorf(api.CodeNotFound, "no workspace %s", req.Id))
	}
	return proto.Clone(ws).(*pb.Workspace), nil
}

func (w *blockingWorker) TraceCommand(ctx context.Context, req *pb.TraceCommandRequest, stream hotbedv1connect.WorkspaceServiceTraceCommandServerStream) error {
	if w.onTrace != nil {
		return w.onTrace(ctx, req, stream)
	}
	return nil
}

func (w *blockingWorker) RunCommand(ctx context.Context, req *pb.RunCommandRequest) (*pb.Command, error) {
	if w.onRun != nil {
		return w.onRun(ctx, req)
	}
	return &pb.Command{Id: req.Id, WorkspaceId: req.WorkspaceId, State: pb.CommandState_COMMAND_STATE_RUNNING}, nil
}

func (w *blockingWorker) ReleaseWorkspace(ctx context.Context, req *pb.ReleaseWorkspaceRequest) (*pb.ReleaseWorkspaceResponse, error) {
	if w.onRelease != nil {
		return w.onRelease(ctx, req)
	}
	w.mu.Lock()
	delete(w.workspaces, req.Id)
	w.mu.Unlock()
	return &pb.ReleaseWorkspaceResponse{}, nil
}

func (w *blockingWorker) StopCommand(ctx context.Context, req *pb.StopCommandRequest) (*pb.Command, error) {
	w.mu.Lock()
	w.stoppedCmds = append(w.stoppedCmds, req.Id)
	w.stoppedReqs = append(w.stoppedReqs, proto.Clone(req).(*pb.StopCommandRequest))
	w.mu.Unlock()
	if w.onStop != nil {
		return w.onStop(ctx, req)
	}
	return &pb.Command{Id: req.Id, State: pb.CommandState_COMMAND_STATE_CANCELLED}, nil
}

func startStubWorkerServer(t *testing.T, h hotbedv1connect.WorkspaceServiceHandler) string {
	t.Helper()
	srv := connect.NewServer()
	hotbedv1connect.RegisterWorkspaceServiceHandler(srv, h)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, srv)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	return strings.TrimPrefix(hs.URL, "http://")
}

// Verify s.scheduling is released across slow worker RPCs in RunSchedule and
// TickSchedules, concurrent TickSchedules doesn't fail an in-flight start,
// conflict suggestion uses -s %s, and concurrent session end stops orphan cmds.
func TestSchedulingLockReleasedDuringWorkerRPC(t *testing.T) {
	ctx := t.Context()
	s := openTestStore(t)
	now := time.Unix(1795320000, 0)

	createStarted := make(chan struct{})
	releaseCreate := make(chan struct{})
	bw := &blockingWorker{
		onCreate: func(ctx context.Context, req *pb.CreateWorkspaceRequest) (*pb.Workspace, error) {
			select {
			case <-createStarted:
			default:
				close(createStarted)
			}
			select {
			case <-releaseCreate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &pb.Workspace{Id: req.Id, SessionId: req.SessionId, NodeId: "n1", State: pb.WorkspaceState_WORKSPACE_STATE_IDLE}, nil
		},
	}
	workerAddr := startStubWorkerServer(t, bw)

	node := tailnet.Peer{NodeID: "n1", Hostname: "w1", Login: "ramon@", Addr: netip.MustParseAddr("100.64.0.1")}
	if _, err := s.Join(ctx, node, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Beat(ctx, node.NodeID, &pb.HeartbeatRequest{Resources: &pb.Resources{Arch: "amd64", Cpus: 4, MemoryBytes: 16 << 30}}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRepo(ctx, &pb.Repo{Id: "app", Url: "https://github.com/acme/app.git"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddProject(ctx, "app", []string{"github.com/acme/app"}, &pb.Stamp{At: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSchedule(ctx, &pb.ScheduledCommand{
		Name: "nightly", Project: "app", Repo: "app", Cron: "0 2 * * *",
		Command: &pb.RunCommandRequest{Args: []string{"echo", "hi"}},
	}); err != nil {
		t.Fatal(err)
	}

	srv := NewServer(ServerConfig{
		Store:      s,
		Now:        func() time.Time { return now },
		WorkerAddr: func(string) string { return workerAddr },
	})
	scAPI := &schedules{srv}

	runDone := make(chan error, 1)
	var startedRun *pb.ScheduledRun
	go func() {
		var err error
		startedRun, err = scAPI.RunSchedule(ctx, &pb.RunScheduleRequest{Name: "nightly"})
		runDone <- err
	}()

	<-createStarted

	// 1. PauseSchedule must NOT block while RunSchedule is waiting on worker RPC.
	pauseDone := make(chan error, 1)
	go func() {
		_, err := scAPI.PauseSchedule(ctx, &pb.PauseScheduleRequest{Name: "nightly", Paused: true})
		pauseDone <- err
	}()
	select {
	case err := <-pauseDone:
		if err != nil {
			t.Fatalf("PauseSchedule during in-flight RunSchedule: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PauseSchedule blocked on s.scheduling while worker RPC was in flight")
	}

	// 2. Concurrent TickSchedules must NOT block and must NOT fail the in-flight start.
	if err := srv.TickSchedules(ctx); err != nil {
		t.Fatalf("TickSchedules during in-flight start: %v", err)
	}
	live, ok, err := s.LiveRun(ctx, "nightly")
	if err != nil || !ok || live.Outcome != pb.RunOutcome_RUN_OUTCOME_RUNNING {
		t.Fatalf("in-flight run was wrongly failed by TickSchedules: live=%v ok=%v err=%v", live, ok, err)
	}

	// 3. Second RunSchedule sees conflict with `-s <session>` flag (board 7898).
	_, conflictErr := scAPI.RunSchedule(ctx, &pb.RunScheduleRequest{Name: "nightly"})
	ae := api.FromConnect(conflictErr)
	if ae == nil || ae.Code != api.CodeConflict {
		t.Fatalf("second RunSchedule = %v, want CONFLICT", conflictErr)
	}
	if !strings.Contains(ae.Suggestion, "hotbed poll -s "+live.SessionId) || !strings.Contains(ae.Suggestion, "hotbed session end -s "+live.SessionId) {
		t.Errorf("conflict suggestion = %q, want -s %s for both poll and session end", ae.Suggestion, live.SessionId)
	}

	// Unblock CreateWorkspace and verify RunSchedule completes.
	close(releaseCreate)
	if err := <-runDone; err != nil {
		t.Fatalf("RunSchedule failed: %v", err)
	}
	if startedRun.CommandId == "" {
		t.Errorf("startedRun.CommandId is empty")
	}
}

func TestSchedulingRunUpdateConflictStopsOrphanCommand(t *testing.T) {
	ctx := t.Context()
	s := openTestStore(t)
	now := time.Unix(1795320000, 0)

	runEntered := make(chan struct{})
	releaseRun := make(chan struct{})
	bw := &blockingWorker{
		onRun: func(ctx context.Context, req *pb.RunCommandRequest) (*pb.Command, error) {
			close(runEntered)
			<-releaseRun
			return &pb.Command{Id: req.Id, WorkspaceId: req.WorkspaceId, State: pb.CommandState_COMMAND_STATE_RUNNING}, nil
		},
	}
	workerAddr := startStubWorkerServer(t, bw)

	node := tailnet.Peer{NodeID: "n1", Hostname: "w1", Login: "ramon@", Addr: netip.MustParseAddr("100.64.0.1")}
	if _, err := s.Join(ctx, node, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Beat(ctx, node.NodeID, &pb.HeartbeatRequest{Resources: &pb.Resources{Arch: "amd64", Cpus: 4, MemoryBytes: 16 << 30}}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRepo(ctx, &pb.Repo{Id: "app", Url: "https://github.com/acme/app.git"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddProject(ctx, "app", []string{"github.com/acme/app"}, &pb.Stamp{At: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSchedule(ctx, &pb.ScheduledCommand{
		Name: "nightly", Project: "app", Repo: "app", Cron: "0 2 * * *",
		Command: &pb.RunCommandRequest{Args: []string{"echo", "hi"}},
	}); err != nil {
		t.Fatal(err)
	}

	srv := NewServer(ServerConfig{
		Store:      s,
		Now:        func() time.Time { return now },
		WorkerAddr: func(string) string { return workerAddr },
	})
	scAPI := &schedules{srv}

	done := make(chan error, 1)
	var gotRun *pb.ScheduledRun
	go func() {
		var err error
		gotRun, err = scAPI.RunSchedule(ctx, &pb.RunScheduleRequest{Name: "nightly"})
		done <- err
	}()
	<-runEntered

	// End the session while RunCommand is in flight; UpdateRun will see conflict (!ok).
	live, ok, err := s.LiveRun(ctx, "nightly")
	if err != nil || !ok {
		t.Fatalf("LiveRun = %v, %v, %v", live, ok, err)
	}
	if _, err := srv.endSession(ctx, live.SessionId, "stopped by user"); err != nil {
		t.Fatal(err)
	}
	close(releaseRun)
	if err := <-done; err != nil {
		t.Fatalf("RunSchedule returned error: %v", err)
	}
	if gotRun == nil || gotRun.Outcome != pb.RunOutcome_RUN_OUTCOME_CANCELLED || gotRun.EndedAt == nil {
		t.Errorf("RunSchedule returned %+v, want terminal RUN_OUTCOME_CANCELLED", gotRun)
	}
	bw.mu.Lock()
	stopped := len(bw.stoppedCmds)
	stoppedReqs := append([]*pb.StopCommandRequest(nil), bw.stoppedReqs...)
	bw.mu.Unlock()
	if stopped != 1 {
		t.Errorf("expected StopCommand called once for concurrently ended run, got %d", stopped)
	}
	if len(stoppedReqs) == 1 && (!stoppedReqs[0].Internal || stoppedReqs[0].Reason == "") {
		t.Errorf("schedule cleanup StopCommand = %+v, want Internal=true and non-empty Reason", stoppedReqs[0])
	}
}

func TestStopCommandForwardsInternalTrue(t *testing.T) {
	ctx := t.Context()
	s := openTestStore(t)
	now := time.Unix(1795320000, 0)

	bw := &blockingWorker{}
	workerAddr := startStubWorkerServer(t, bw)
	if _, err := s.Join(ctx, tailnet.Peer{NodeID: "n1", Hostname: "w1", Login: "ramon@"}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Beat(ctx, "n1", &pb.HeartbeatRequest{Resources: &pb.Resources{MemoryBytes: 16 << 30}}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddProject(ctx, "app", nil, &pb.Stamp{At: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	sess, err := s.StartSession(ctx, "sess-int", "claude", "app", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddCommand(ctx, "cmd-int", sess.Id, "n1", "ws-1", now); err != nil {
		t.Fatal(err)
	}

	srv := NewServer(ServerConfig{
		Store:      s,
		Now:        func() time.Time { return now },
		WorkerAddr: func(string) string { return workerAddr },
	})
	ws := &workspaces{Server: srv}

	if _, err := ws.stopCommand(ctx, &pb.StopCommandRequest{Id: "cmd-int"}); err != nil {
		t.Fatalf("stopCommand default reason: %v", err)
	}
	if _, err := ws.stopCommand(ctx, &pb.StopCommandRequest{Id: "cmd-int", Reason: "schedule timed out", Host: "spoofed"}); err != nil {
		t.Fatalf("stopCommand custom reason: %v", err)
	}

	bw.mu.Lock()
	defer bw.mu.Unlock()
	if len(bw.stoppedReqs) != 2 {
		t.Fatalf("len(stoppedReqs) = %d, want 2", len(bw.stoppedReqs))
	}
	if !bw.stoppedReqs[0].Internal || bw.stoppedReqs[0].Reason != "stopped by controller" || bw.stoppedReqs[0].Host != "" {
		t.Errorf("stoppedReqs[0] = %+v, want Internal=true Reason='stopped by controller' Host=''", bw.stoppedReqs[0])
	}
	if !bw.stoppedReqs[1].Internal || bw.stoppedReqs[1].Reason != "schedule timed out" || bw.stoppedReqs[1].Host != "" {
		t.Errorf("stoppedReqs[1] = %+v, want Internal=true Reason='schedule timed out' Host=''", bw.stoppedReqs[1])
	}
}

// Verify offline failed workspace defers release instead of blocking healthy
// placement (board 7892), and s.placing is NOT held during slow worker RPCs
// (board 8015).
func TestPlacementOfflineFailedWorkspaceAndLockRelease(t *testing.T) {
	ctx := t.Context()
	s := openTestStore(t)
	now := time.Unix(1795320000, 0)

	healthyWorker := &blockingWorker{}
	healthyAddr := startStubWorkerServer(t, healthyWorker)

	releaseEntered := make(chan struct{})
	allowRelease := make(chan struct{})
	slowWorker := &blockingWorker{
		onRelease: func(ctx context.Context, req *pb.ReleaseWorkspaceRequest) (*pb.ReleaseWorkspaceResponse, error) {
			close(releaseEntered)
			<-allowRelease
			return &pb.ReleaseWorkspaceResponse{}, nil
		},
	}
	slowAddr := startStubWorkerServer(t, slowWorker)

	nOffline := tailnet.Peer{NodeID: "nOff", Hostname: "offline-node", Login: "ramon@", Addr: netip.MustParseAddr("100.64.0.10")}
	nHealthy := tailnet.Peer{NodeID: "nOk", Hostname: "healthy-node", Login: "ramon@", Addr: netip.MustParseAddr("100.64.0.11")}
	nSlow := tailnet.Peer{NodeID: "nSlow", Hostname: "slow-node", Login: "ramon@", Addr: netip.MustParseAddr("100.64.0.12")}
	for _, p := range []tailnet.Peer{nOffline, nHealthy, nSlow} {
		if _, err := s.Join(ctx, p, now); err != nil {
			t.Fatal(err)
		}
	}
	// Offline node's last heartbeat is older than OnlineWindow.
	if err := s.Beat(ctx, nOffline.NodeID, &pb.HeartbeatRequest{Resources: &pb.Resources{Arch: "amd64", Cpus: 4, MemoryBytes: 16 << 30}}, now.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{nHealthy.NodeID, nSlow.NodeID} {
		if err := s.Beat(ctx, id, &pb.HeartbeatRequest{Resources: &pb.Resources{Arch: "amd64", Cpus: 4, MemoryBytes: 16 << 30}}, now); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.PutRepo(ctx, &pb.Repo{Id: "app", Url: "https://github.com/acme/app.git"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddProject(ctx, "app", []string{"github.com/acme/app"}, &pb.Stamp{At: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartSession(ctx, "sess-offline-fail", "laptop", "app", "", now); err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspace(ctx, "ws-failed-off", "sess-offline-fail", nOffline.NodeID, "laptop", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Push(ctx, nOffline.NodeID, &pb.PushEventsRequest{
		Epoch: "e1",
		Checkpoint: &pb.Checkpoint{
			Seq: 1,
			Workspaces: []*pb.Workspace{{
				Id: "ws-failed-off", SessionId: "sess-offline-fail", NodeId: nOffline.NodeID, State: pb.WorkspaceState_WORKSPACE_STATE_FAILED,
			}},
		},
	}, now, nil); err != nil {
		t.Fatal(err)
	}

	addrMap := map[string]string{
		"100.64.0.10": "127.0.0.1:1",
		"100.64.0.11": healthyAddr,
		"100.64.0.12": slowAddr,
	}
	srv := NewServer(ServerConfig{
		Store:      s,
		Now:        func() time.Time { return now },
		WorkerAddr: func(h string) string { return addrMap[h] },
	})
	wsAPI := &workspaces{Server: srv}

	// 1. Offline failed workspace must NOT block placing on healthy-node.
	created, err := wsAPI.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{
		SessionId: "sess-offline-fail", Repo: "app", SyncDir: "/src/app",
		Placement: &pb.Placement{Node: "healthy-node"},
	})
	if err != nil {
		t.Fatalf("CreateWorkspace with failed workspace on offline node failed: %v", err)
	}
	if created.Node != "healthy-node" {
		t.Errorf("created workspace on %q, want healthy-node", created.Node)
	}
	// The failed workspace on the offline node must be marked released for deferred heartbeat cleanup.
	wRow, err := s.Workspace(ctx, "ws-failed-off")
	if err != nil || !wRow.released {
		t.Errorf("ws-failed-off released=%v err=%v, want released=true", wRow.released, err)
	}

	// 2. Slow ReleaseWorkspace RPC must NOT hold s.placing and stall another session.
	if _, err := s.StartSession(ctx, "sess-slow", "laptop", "app", "", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartSession(ctx, "sess-fast", "laptop", "app", "", now); err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspace(ctx, "ws-failed-slow", "sess-slow", nSlow.NodeID, "laptop", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Push(ctx, nSlow.NodeID, &pb.PushEventsRequest{
		Epoch: "e1",
		Checkpoint: &pb.Checkpoint{
			Seq: 1,
			Workspaces: []*pb.Workspace{{
				Id: "ws-failed-slow", SessionId: "sess-slow", NodeId: nSlow.NodeID, State: pb.WorkspaceState_WORKSPACE_STATE_FAILED,
			}},
		},
	}, now, nil); err != nil {
		t.Fatal(err)
	}

	slowDone := make(chan error, 1)
	go func() {
		_, err := wsAPI.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{
			SessionId: "sess-slow", Repo: "app", SyncDir: "/src/app",
			Placement: &pb.Placement{Node: "healthy-node"},
		})
		slowDone <- err
	}()
	<-releaseEntered

	fastDone := make(chan error, 1)
	go func() {
		_, err := wsAPI.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{
			SessionId: "sess-fast", Repo: "app", SyncDir: "/src/app",
			Placement: &pb.Placement{Node: "healthy-node"},
		})
		fastDone <- err
	}()
	select {
	case err := <-fastDone:
		if err != nil {
			t.Fatalf("concurrent CreateWorkspace failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent CreateWorkspace blocked on s.placing during slow ReleaseWorkspace RPC")
	}

	close(allowRelease)
	if err := <-slowDone; err != nil {
		t.Fatalf("slow session CreateWorkspace failed: %v", err)
	}
}

func TestEndSessionAndReapIdleRaces(t *testing.T) {
	ctx := t.Context()
	s := openTestStore(t)
	now := time.Unix(1795320000, 0)

	node := tailnet.Peer{NodeID: "n1", Hostname: "w1", Login: "ramon@"}
	if _, err := s.Join(ctx, node, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddProject(ctx, "app", nil, &pb.Stamp{At: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}

	// 1. ListWorkspacesToRelease includes workspaces with released_at IS NULL
	// whose session ended (e.g. created concurrently during endSession).
	if _, err := s.StartSession(ctx, "race-sess", "laptop", "app", "", now); err != nil {
		t.Fatal(err)
	}
	if err := s.EndSession(ctx, "race-sess", "ended", now); err != nil {
		t.Fatal(err)
	}
	// Simulate concurrent workspace creation after EndSession without released_at set.
	if err := s.AddWorkspace(ctx, "ws-orphan", "race-sess", node.NodeID, "laptop", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Push(ctx, node.NodeID, &pb.PushEventsRequest{
		Epoch: "e1",
		Checkpoint: &pb.Checkpoint{
			Seq: 1,
			Workspaces: []*pb.Workspace{{
				Id: "ws-orphan", SessionId: "race-sess", NodeId: node.NodeID, State: pb.WorkspaceState_WORKSPACE_STATE_IDLE,
			}},
		},
	}, now, nil); err != nil {
		t.Fatal(err)
	}
	toRelease, err := s.ToRelease(ctx, node.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(toRelease) != 1 || toRelease[0] != "ws-orphan" {
		t.Errorf("ToRelease = %v, want [ws-orphan] for ended session", toRelease)
	}

	// 2. EndSessionIfIdle refuses to kill a session touched concurrently.
	idleStart := now.Add(-2 * time.Hour)
	cutoff := now.Add(-sessionIdle)
	if _, err := s.StartSession(ctx, "touched-sess", "laptop", "app", "", idleStart); err != nil {
		t.Fatal(err)
	}
	if err := s.Touch(ctx, "touched-sess", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	ended, err := s.EndSessionIfIdle(ctx, "touched-sess", "abandoned", now, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if ended {
		t.Errorf("EndSessionIfIdle ended a recently touched session")
	}
	sess, err := s.Session(ctx, "touched-sess")
	if err != nil || sess.EndedAt != nil {
		t.Errorf("touched-sess should still be live, got %+v err=%v", sess, err)
	}

	// A session still idle before cutoff IS ended by ReapIdle.
	if _, err := s.StartSession(ctx, "truly-idle", "laptop", "app", "", idleStart); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(ServerConfig{Store: s, Now: func() time.Time { return now }})
	if err := srv.ReapIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if sess, err := s.Session(ctx, "truly-idle"); err != nil || sess.EndedAt == nil {
		t.Errorf("truly-idle should have been reaped, got %+v err=%v", sess, err)
	}
}

func TestListWorkspacesFiltersReleasedAndGetSessionSetsProject(t *testing.T) {
	ctx := t.Context()
	s := openTestStore(t)
	now := time.Unix(1795320000, 0)

	node := tailnet.Peer{NodeID: "n1", Hostname: "offline-w1", Login: "ramon@"}
	if _, err := s.Join(ctx, node, now); err != nil {
		t.Fatal(err)
	}
	// Offline worker: last heartbeat older than OnlineWindow.
	if err := s.Beat(ctx, node.NodeID, &pb.HeartbeatRequest{}, now.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddProject(ctx, "myproj", nil, &pb.Stamp{At: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartSession(ctx, "live-fox", "laptop", "myproj", "", now); err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspace(ctx, "ws-released", "live-fox", node.NodeID, "laptop", now); err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspace(ctx, "ws-live", "live-fox", node.NodeID, "laptop", now); err != nil {
		t.Fatal(err)
	}
	// Worker's view still reports both workspaces as IDLE (worker is offline and
	// hasn't processed the release yet), and intentionally omits Project on ws-live.
	if _, err := s.Push(ctx, node.NodeID, &pb.PushEventsRequest{
		Epoch: "e1",
		Checkpoint: &pb.Checkpoint{
			Seq: 1,
			Workspaces: []*pb.Workspace{
				{Id: "ws-released", SessionId: "live-fox", NodeId: node.NodeID, State: pb.WorkspaceState_WORKSPACE_STATE_IDLE},
				{Id: "ws-live", SessionId: "live-fox", NodeId: node.NodeID, State: pb.WorkspaceState_WORKSPACE_STATE_IDLE},
			},
		},
	}, now, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseWorkspace(ctx, "ws-released", now); err != nil {
		t.Fatal(err)
	}

	srv := NewServer(ServerConfig{
		Store:      s,
		Now:        func() time.Time { return now },
		WorkerAddr: func(string) string { return "127.0.0.1:1" },
	})
	wsAPI := &workspaces{Server: srv}
	sessAPI := &sessions{srv}

	// ListWorkspaces without SessionId must omit ws-released.
	allWS, err := wsAPI.ListWorkspaces(ctx, &pb.ListWorkspacesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(allWS.Workspaces) != 1 || allWS.Workspaces[0].Id != "ws-live" {
		t.Errorf("ListWorkspaces() = %v, want only ws-live", allWS.Workspaces)
	}

	// ListWorkspaces with SessionId must also omit ws-released.
	sessWS, err := wsAPI.ListWorkspaces(ctx, &pb.ListWorkspacesRequest{SessionId: "live-fox"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessWS.Workspaces) != 1 || sessWS.Workspaces[0].Id != "ws-live" {
		t.Errorf("ListWorkspaces(live-fox) = %v, want only ws-live", sessWS.Workspaces)
	}

	// GetSession fallback path sets ws.Project = sess.Project and omits ws-released.
	gs, err := sessAPI.GetSession(ctx, &pb.GetSessionRequest{Id: "live-fox"})
	if err != nil {
		t.Fatal(err)
	}
	if len(gs.Workspaces) != 1 || gs.Workspaces[0].Id != "ws-live" || gs.Workspaces[0].Project != "myproj" {
		t.Errorf("GetSession workspaces = %v, want ws-live with project myproj", gs.Workspaces)
	}
}

func TestFollowsOverlappingRetry(t *testing.T) {
	ctx := t.Context()
	s := openTestStore(t)
	now := time.Unix(1795320000, 0)
	node := tailnet.Peer{NodeID: "n1", Hostname: "w1", Login: "ramon@"}
	if _, err := s.Join(ctx, node, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Push(ctx, node.NodeID, &pb.PushEventsRequest{Epoch: "e1", Checkpoint: &pb.Checkpoint{Seq: 0}}, now, nil); err != nil {
		t.Fatal(err)
	}
	ev := func(seq, at int64) *pb.Event {
		return &pb.Event{Seq: seq, At: at, Change: &pb.Event_Workspace{Workspace: &pb.WorkspaceChange{What: "idle", Workspace: &pb.Workspace{Id: "ws1", NodeId: node.NodeID}}}}
	}
	res, err := s.Push(ctx, node.NodeID, &pb.PushEventsRequest{
		Epoch:  "e1",
		Events: []*pb.Event{ev(1, 1001), ev(2, 1002), ev(3, 1003)},
	}, now, nil)
	if err != nil || res.applied != 3 || res.checkpointNeeded {
		t.Fatalf("initial push = %+v, err=%v", res, err)
	}

	// Overlapping retry containing seq 2, 3, 4 with AfterAt=1001 (before seq 2)
	// must compare ev[seq==3].At (1003) against appliedAt (1003) and succeed
	// without forcing a full checkpoint!
	res, err = s.Push(ctx, node.NodeID, &pb.PushEventsRequest{
		Epoch:   "e1",
		AfterAt: 1001,
		Events:  []*pb.Event{ev(2, 1002), ev(3, 1003), ev(4, 1004)},
	}, now, nil)
	if err != nil || res.checkpointNeeded || res.applied != 4 {
		t.Fatalf("overlapping retry = %+v err=%v, want applied=4 checkpointNeeded=false", res, err)
	}

	// Overlapping retry where ev[seq==4].At disagrees with appliedAt (1004) must
	// still detect divergence and ask for a checkpoint.
	res, err = s.Push(ctx, node.NodeID, &pb.PushEventsRequest{
		Epoch:   "e1",
		AfterAt: 1002,
		Events:  []*pb.Event{ev(3, 1003), ev(4, 9999), ev(5, 10000)},
	}, now, nil)
	if err != nil || !res.checkpointNeeded || res.applied != 4 {
		t.Fatalf("diverged overlapping retry = %+v err=%v, want checkpointNeeded=true applied=4", res, err)
	}
}

func TestReviewDecideNilStampAndListResponsesCursor(t *testing.T) {
	ctx := t.Context()
	s := openTestStore(t)
	now := time.Unix(1795320000, 0)
	tn := tailnettest.New(tailnet.Self{
		Peer:  tailnet.Peer{NodeID: "nCtrl", Hostname: "ctrl", Login: "ramon@"},
		Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")},
	})
	srv := httptest.NewServer(NewServer(ServerConfig{
		Store: s, Identifier: tn, Self: []netip.Addr{netip.MustParseAddr("127.0.0.1")},
		Cluster: statedir.ControllerConfig{ClusterID: "c1", Owner: "ramon@", Port: 7120},
		Now:     func() time.Time { return now },
	}).Handler())
	t.Cleanup(srv.Close)

	hc := tn.Client(tailnet.Peer{NodeID: "nLaptop", Hostname: "laptop", Login: "ramon@"})
	conn := connect.NewClient(connecthttp.NewTransport(hc, srv.URL))
	clusterAPI := hotbedv1connect.NewClusterServiceClient(conn)
	sessAPI := hotbedv1connect.NewSessionServiceClient(conn)
	revAPI := hotbedv1connect.NewReviewServiceClient(conn)

	if _, err := clusterAPI.AddProject(ctx, &pb.AddProjectRequest{Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sessAPI.StartSession(ctx, &pb.StartSessionRequest{Id: "alpha-fox", Project: "demo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sessAPI.StartSession(ctx, &pb.StartSessionRequest{Id: "beta-owl", Project: "demo"}); err != nil {
		t.Fatal(err)
	}
	rev, err := revAPI.RequestReview(ctx, &pb.RequestReviewRequest{SessionId: "alpha-fox", Head: "abcdef1234567"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := revAPI.StartReview(ctx, &pb.StartReviewRequest{Id: rev.Id, SessionId: "beta-owl", Model: "test-model"}); err != nil {
		t.Fatal(err)
	}
	added, err := revAPI.AddFindings(ctx, &pb.AddFindingsRequest{
		ReviewId:  rev.Id,
		SessionId: "beta-owl",
		Findings:  []*pb.ReportedFinding{{Summary: "bug", Severity: "important"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	fid := added.Findings[0].Id

	// 1. Reopening an already-open finding (f.Decided == nil) must NOT format 1970-01-01.
	_, err = revAPI.DecideFindings(ctx, &pb.DecideFindingsRequest{
		ReviewId: rev.Id, FindingIds: []int32{fid}, Decision: pb.FindingDecision_FINDING_DECISION_OPEN, SessionId: "alpha-fox",
	})
	ae := api.FromConnect(err)
	if ae == nil || ae.Code != api.CodeConflict {
		t.Fatalf("duplicate reopen = %v, want CONFLICT", err)
	}
	if strings.Contains(ae.Message, "1970") {
		t.Errorf("duplicate reopen message leaked epoch date: %q", ae.Message)
	}

	// 2. Non-response event (accepted) must NOT trap ListResponses -> Respond --after in CONFLICT.
	e1, err := revAPI.Respond(ctx, &pb.RespondRequest{ReviewId: rev.Id, FindingId: fid, Text: "looking"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := revAPI.DecideFindings(ctx, &pb.DecideFindingsRequest{
		ReviewId: rev.Id, FindingIds: []int32{fid}, Decision: pb.FindingDecision_FINDING_DECISION_ACCEPTED, SessionId: "alpha-fox",
	}); err != nil {
		t.Fatal(err)
	}
	// Using stale cursor e1.Seq conflicts because of the accepted event.
	if _, err := revAPI.Respond(ctx, &pb.RespondRequest{ReviewId: rev.Id, Text: "stale", After: e1.Seq}); api.CodeOf(err) != api.CodeConflict {
		t.Fatalf("Respond with stale cursor before accepted event = %v, want CONFLICT", err)
	}
	// ListResponses advances Cursor past the accepted event so the caller can recover!
	lr, err := revAPI.ListResponses(ctx, &pb.ListResponsesRequest{ReviewId: rev.Id, After: e1.Seq})
	if err != nil {
		t.Fatal(err)
	}
	if len(lr.Responses) != 0 {
		t.Errorf("ListResponses returned %d responses, want 0", len(lr.Responses))
	}
	if lr.Cursor <= e1.Seq {
		t.Fatalf("ListResponses cursor = %d, want > %d (past accepted event)", lr.Cursor, e1.Seq)
	}
	if _, err := revAPI.Respond(ctx, &pb.RespondRequest{ReviewId: rev.Id, Text: "recovered", After: lr.Cursor}); err != nil {
		t.Fatalf("Respond with ListResponses cursor failed: %v", err)
	}
}

func TestWorkerClientCacheRefreshesOnAddressChange(t *testing.T) {
	ctx := t.Context()
	s := openTestStore(t)
	now := time.Unix(1795320000, 0)

	var hits1, hits2 atomic.Int32
	w1 := &blockingWorker{
		onRelease: func(context.Context, *pb.ReleaseWorkspaceRequest) (*pb.ReleaseWorkspaceResponse, error) {
			hits1.Add(1)
			return &pb.ReleaseWorkspaceResponse{}, nil
		},
	}
	w2 := &blockingWorker{
		onRelease: func(context.Context, *pb.ReleaseWorkspaceRequest) (*pb.ReleaseWorkspaceResponse, error) {
			hits2.Add(1)
			return &pb.ReleaseWorkspaceResponse{}, nil
		},
	}
	addr1 := startStubWorkerServer(t, w1)
	addr2 := startStubWorkerServer(t, w2)
	addrByIP := map[string]string{
		"100.64.0.1": addr1,
		"100.64.0.2": addr2,
	}

	srv := NewServer(ServerConfig{
		Store:      s,
		Now:        func() time.Time { return now },
		WorkerAddr: func(ip string) string { return addrByIP[ip] },
	})

	// Worker joins at IP1.
	if _, err := s.Join(ctx, tailnet.Peer{NodeID: "n1", Hostname: "ubu1", Login: "ramon@", Addr: netip.MustParseAddr("100.64.0.1")}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Beat(ctx, "n1", &pb.HeartbeatRequest{}, now); err != nil {
		t.Fatal(err)
	}
	c1, _, err := srv.worker(ctx, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c1.ReleaseWorkspace(ctx, &pb.ReleaseWorkspaceRequest{Id: "ws1"}); err != nil {
		t.Fatal(err)
	}
	if hits1.Load() != 1 || hits2.Load() != 0 {
		t.Fatalf("hits = (%d, %d), want (1, 0)", hits1.Load(), hits2.Load())
	}

	// Same hostname & node ID re-joins at new tailnet IP2.
	if _, err := s.Join(ctx, tailnet.Peer{NodeID: "n1", Hostname: "ubu1", Login: "ramon@", Addr: netip.MustParseAddr("100.64.0.2")}, now); err != nil {
		t.Fatal(err)
	}
	c2, _, err := srv.worker(ctx, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c2.ReleaseWorkspace(ctx, &pb.ReleaseWorkspaceRequest{Id: "ws2"}); err != nil {
		t.Fatal(err)
	}
	if hits1.Load() != 1 || hits2.Load() != 1 {
		t.Errorf("after IP change hits = (%d, %d), want (1, 1)", hits1.Load(), hits2.Load())
	}
}

func TestCockpitSeriesDeduplicatesPerNodeBucket(t *testing.T) {
	now := time.Unix(1795320000, 0)
	bucketStart := (now.UnixMilli() / (sampleSeconds * 1000)) * (sampleSeconds * 1000)
	samples := []sampleRow{
		{node: "n1", t: bucketStart + 1000, running: 2, queued: 1, parked: 1},
		{node: "n2", t: bucketStart + 2000, running: 1, queued: 2, parked: 0},
		// Second heartbeat from n1 in the same 5s bucket must replace, not add.
		{node: "n1", t: bucketStart + 3000, running: 3, queued: 0, parked: 2},
	}
	_, loads, _ := series(samples, now)
	if len(loads) != 1 {
		t.Fatalf("loads = %v, want 1 bucket", loads)
	}
	got := loads[0]
	if got.Running != 4 || got.Queued != 2 || got.Parked != 2 {
		t.Errorf("bucket load = (running=%d, queued=%d, parked=%d), want (4, 2, 2)", got.Running, got.Queued, got.Parked)
	}
}

// hookDBTX wraps a controllerdb.DBTX and invokes optional hooks before queries/execs.
type hookDBTX struct {
	base    controllerdb.DBTX
	onExec  func(query string) error
	onQuery func(query string) error
}

func (h *hookDBTX) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	if h.onExec != nil {
		if err := h.onExec(query); err != nil {
			return nil, err
		}
	}
	return h.base.ExecContext(ctx, query, args...)
}

func (h *hookDBTX) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return h.base.PrepareContext(ctx, query)
}

func (h *hookDBTX) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	if h.onQuery != nil {
		if err := h.onQuery(query); err != nil {
			return nil, err
		}
	}
	return h.base.QueryContext(ctx, query, args...)
}

func (h *hookDBTX) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	if h.onQuery != nil {
		if err := h.onQuery(query); err != nil {
			// Return a row that yields err on Scan by querying an invalid SQL statement?
			// Instead, hookDBTX callers that need QueryRow errors can use a cancelled ctx
			// or fail a multi-row QueryContext; for GetHistoryDoc we return a failing QueryRow.
			return h.base.QueryRowContext(ctx, "SELECT invalid_column_from_hook")
		}
	}
	return h.base.QueryRowContext(ctx, query, args...)
}

// Regression test for Finding #76 & #93 (board 8287):
// (a) EndSession racing ResumeSession + new workspace creation must never release
//
//	the resumed session's new workspace, and CreateWorkspace racing EndSession
//	must not leave an orphan workspace on the worker.
//
// (b) LiveSession racing EndSessionIfIdle must fail with CodeNotFound once ended.
func TestSessionEndResumeAndLiveSessionRaces(t *testing.T) {
	ctx := t.Context()
	now := time.Unix(1795320000, 0)

	t.Run("end racing resume and new workspace", func(t *testing.T) {
		s := openTestStore(t)
		releaseEntered := make(chan struct{})
		allowRelease := make(chan struct{})
		var mu sync.Mutex
		var releasedIDs []string
		bw := &blockingWorker{
			workspaces: map[string]*pb.Workspace{
				"ws-old": {Id: "ws-old", SessionId: "race-resume", NodeId: "n1", State: pb.WorkspaceState_WORKSPACE_STATE_IDLE},
			},
			onRelease: func(ctx context.Context, req *pb.ReleaseWorkspaceRequest) (*pb.ReleaseWorkspaceResponse, error) {
				mu.Lock()
				releasedIDs = append(releasedIDs, req.Id)
				mu.Unlock()
				if req.Id == "ws-old" {
					close(releaseEntered)
					<-allowRelease
				}
				return &pb.ReleaseWorkspaceResponse{}, nil
			},
		}
		workerAddr := startStubWorkerServer(t, bw)

		node := tailnet.Peer{NodeID: "n1", Hostname: "w1", Login: "ramon@", Addr: netip.MustParseAddr("100.64.0.1")}
		if _, err := s.Join(ctx, node, now); err != nil {
			t.Fatal(err)
		}
		if err := s.Beat(ctx, node.NodeID, &pb.HeartbeatRequest{Resources: &pb.Resources{Arch: "amd64", Cpus: 4, MemoryBytes: 16 << 30}}, now); err != nil {
			t.Fatal(err)
		}
		if err := s.PutRepo(ctx, &pb.Repo{Id: "app", Url: "https://github.com/acme/app.git"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddProject(ctx, "app", []string{"github.com/acme/app"}, &pb.Stamp{At: now.UnixMilli()}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.StartSession(ctx, "race-resume", "laptop", "app", "", now); err != nil {
			t.Fatal(err)
		}
		if err := s.AddWorkspace(ctx, "ws-old", "race-resume", node.NodeID, "laptop", now); err != nil {
			t.Fatal(err)
		}

		srv := NewServer(ServerConfig{
			Store:      s,
			Now:        func() time.Time { return now },
			WorkerAddr: func(string) string { return workerAddr },
		})
		sessAPI := &sessions{srv}
		wsAPI := &workspaces{Server: srv}

		endDone := make(chan error, 1)
		go func() {
			_, err := sessAPI.EndSession(ctx, &pb.EndSessionRequest{Id: "race-resume"})
			endDone <- err
		}()
		<-releaseEntered

		// Start ResumeSession concurrently while finishEndSession is still releasing ws-old.
		resumeDone := make(chan error, 1)
		go func() {
			_, err := sessAPI.StartSession(ctx, &pb.StartSessionRequest{Id: "race-resume", Resume: true})
			resumeDone <- err
		}()

		// ResumeSession must wait for the in-flight endSession to complete.
		select {
		case err := <-resumeDone:
			t.Fatalf("ResumeSession returned while EndSession was still releasing workspaces: err=%v", err)
		case <-time.After(50 * time.Millisecond):
		}

		close(allowRelease)
		if err := <-endDone; err != nil {
			t.Fatalf("EndSession failed: %v", err)
		}
		if err := <-resumeDone; err != nil {
			t.Fatalf("ResumeSession failed: %v", err)
		}

		// Create a new workspace on the resumed session.
		wsNew, err := wsAPI.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{
			SessionId: "race-resume", Repo: "app", SyncDir: "/src/app",
		})
		if err != nil {
			t.Fatalf("CreateWorkspace after resume failed: %v", err)
		}

		mu.Lock()
		gotReleased := append([]string(nil), releasedIDs...)
		mu.Unlock()
		if len(gotReleased) != 1 || gotReleased[0] != "ws-old" {
			t.Errorf("released workspaces = %v, want only [ws-old] (new workspace %s must not be released)", gotReleased, wsNew.Id)
		}
		wRow, err := s.Workspace(ctx, wsNew.Id)
		if err != nil || wRow.released {
			t.Errorf("resumed workspace %s row = %+v err=%v, want live (released=false)", wsNew.Id, wRow, err)
		}
		sess, err := s.Session(ctx, "race-resume")
		if err != nil || sess.EndedAt != nil {
			t.Errorf("resumed session = %+v err=%v, want live", sess, err)
		}
	})

	t.Run("AddLiveWorkspace rejects ended session", func(t *testing.T) {
		s := openTestStore(t)
		node := tailnet.Peer{NodeID: "n1", Hostname: "w1", Login: "ramon@"}
		if _, err := s.Join(ctx, node, now); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddProject(ctx, "app", nil, &pb.Stamp{At: now.UnixMilli()}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.StartSession(ctx, "ended-sess", "laptop", "app", "", now); err != nil {
			t.Fatal(err)
		}
		if err := s.EndSession(ctx, "ended-sess", "done", now); err != nil {
			t.Fatal(err)
		}
		ok, err := s.AddLiveWorkspace(ctx, "ws-nope", "ended-sess", node.NodeID, "laptop", now)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Errorf("AddLiveWorkspace succeeded for an ended session, want ok=false")
		}
	})

	t.Run("LiveSession racing EndSessionIfIdle", func(t *testing.T) {
		s := openTestStore(t)
		idleStart := now.Add(-2 * time.Hour)
		cutoff := now.Add(-sessionIdle)
		if _, err := s.AddProject(ctx, "app", nil, &pb.Stamp{At: idleStart.UnixMilli()}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.StartSession(ctx, "race-live", "laptop", "app", "", idleStart); err != nil {
			t.Fatal(err)
		}

		// Intercept TouchSession inside LiveSession and run EndSessionIfIdle right before it.
		var fired atomic.Bool
		directQ := controllerdb.New(s.db)
		s.q = controllerdb.New(&hookDBTX{
			base: s.db,
			onExec: func(q string) error {
				if strings.Contains(q, "TouchSession") && fired.CompareAndSwap(false, true) {
					n, err := directQ.EndSessionIfIdle(ctx, controllerdb.EndSessionIfIdleParams{
						EndedAt:      sql.NullInt64{Int64: now.UnixMilli(), Valid: true},
						EndReason:    "abandoned",
						ID:           "race-live",
						LastActiveAt: cutoff.UnixMilli(),
					})
					if err != nil {
						return err
					}
					if n != 1 {
						return errors.New("expected EndSessionIfIdle to end race-live")
					}
				}
				return nil
			},
		})

		sess, err := s.LiveSession(ctx, "race-live", now)
		if err == nil {
			t.Fatalf("LiveSession succeeded after concurrent EndSessionIfIdle: got %+v", sess)
		}
		if code := api.CodeOf(err); code != api.CodeNotFound {
			t.Errorf("LiveSession error code = %v (%v), want CodeNotFound", code, err)
		}
		if !strings.Contains(err.Error(), "abandoned") {
			t.Errorf("LiveSession error = %q, want ended reason 'abandoned'", err.Error())
		}
	})
}

// Regression test for Finding #77 (board 8289):
// Quiet relayed streams (such as TraceCommand) whose first chunk arrives after
// a delay must not expire on a transport ResponseHeaderTimeout, while short
// unary worker RPCs like ReleaseWorkspace and GetWorkspace still bound
// themselves with deadlines.
func TestRelayedStreamDelayedFirstMessageAndUnaryDeadlines(t *testing.T) {
	ctx := t.Context()
	s := openTestStore(t)
	now := time.Unix(1795320000, 0)

	var releaseHasDeadline, getHasDeadline atomic.Bool
	bw := &blockingWorker{
		workspaces: map[string]*pb.Workspace{
			"ws1": {Id: "ws1", SessionId: "quiet-fox", NodeId: "n1", State: pb.WorkspaceState_WORKSPACE_STATE_IDLE},
		},
		onGet: func(ctx context.Context, req *pb.GetWorkspaceRequest) (*pb.Workspace, error) {
			_, ok := ctx.Deadline()
			getHasDeadline.Store(ok)
			return &pb.Workspace{Id: req.Id, SessionId: "quiet-fox", NodeId: "n1", State: pb.WorkspaceState_WORKSPACE_STATE_IDLE}, nil
		},
		onRelease: func(ctx context.Context, req *pb.ReleaseWorkspaceRequest) (*pb.ReleaseWorkspaceResponse, error) {
			_, ok := ctx.Deadline()
			releaseHasDeadline.Store(ok)
			return &pb.ReleaseWorkspaceResponse{}, nil
		},
		onTrace: func(ctx context.Context, req *pb.TraceCommandRequest, stream hotbedv1connect.WorkspaceServiceTraceCommandServerStream) error {
			// Delay before sending the first message (and thus HTTP response headers).
			select {
			case <-time.After(150 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
			return stream.Send(&pb.TraceCommandChunk{Text: "quiet output\n", Done: true})
		},
	}
	workerAddr := startStubWorkerServer(t, bw)

	node := tailnet.Peer{NodeID: "n1", Hostname: "w1", Login: "ramon@", Addr: netip.MustParseAddr("100.64.0.1")}
	if _, err := s.Join(ctx, node, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Beat(ctx, node.NodeID, &pb.HeartbeatRequest{Resources: &pb.Resources{Arch: "amd64", Cpus: 4, MemoryBytes: 16 << 30}}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddProject(ctx, "app", nil, &pb.Stamp{At: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartSession(ctx, "quiet-fox", "laptop", "app", "", now); err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspace(ctx, "ws1", "quiet-fox", node.NodeID, "laptop", now); err != nil {
		t.Fatal(err)
	}
	if err := s.AddCommand(ctx, "cmd1", "quiet-fox", node.NodeID, "ws1", now); err != nil {
		t.Fatal(err)
	}

	tn := tailnettest.New(tailnet.Self{
		Peer:  tailnet.Peer{NodeID: "nCtrl", Hostname: "ctrl", Login: "ramon@"},
		Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")},
	})
	ctrlSrv := NewServer(ServerConfig{
		Store:      s,
		Identifier: tn,
		Self:       []netip.Addr{netip.MustParseAddr("127.0.0.1")},
		Cluster:    statedir.ControllerConfig{ClusterID: "c1", Owner: "ramon@", Port: 7120},
		Now:        func() time.Time { return now },
		WorkerAddr: func(string) string { return workerAddr },
	})
	hs := httptest.NewServer(ctrlSrv.Handler())
	t.Cleanup(hs.Close)

	// Verify delayed first chunk on TraceCommand succeeds over the controller's default worker client.
	hc := tn.Client(tailnet.Peer{NodeID: "nLaptop", Hostname: "laptop", Login: "ramon@"})
	conn := connect.NewClient(connecthttp.NewTransport(hc, hs.URL))
	wsClient := hotbedv1connect.NewWorkspaceServiceClient(conn)
	stream, err := wsClient.TraceCommand(ctx, &pb.TraceCommandRequest{Id: "cmd1"})
	if err != nil {
		t.Fatalf("TraceCommand open failed: %v", err)
	}
	defer stream.Close()
	var got strings.Builder
	for {
		chunk, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("TraceCommand receive failed: %v", err)
		}
		got.WriteString(chunk.Text)
	}
	if got.String() != "quiet output\n" {
		t.Errorf("TraceCommand output = %q, want %q", got.String(), "quiet output\n")
	}

	// Verify retired client data-plane forwarding RPCs return Unimplemented on the controller.
	for name, callErr := range map[string]error{
		"RunCommand": func() error { _, err := wsClient.RunCommand(ctx, &pb.RunCommandRequest{WorkspaceId: "ws1"}); return err }(),
		"Sync": func() error {
			st, err := wsClient.Sync(ctx)
			if err != nil {
				return err
			}
			_ = st.Send(&pb.SyncRequest{Msg: &pb.SyncRequest_Header{Header: &pb.SyncHeader{WorkspaceId: "ws1"}}})
			_, err = st.CloseAndReceive()
			return err
		}(),
		"FollowLog": func() error {
			st, err := wsClient.FollowLog(ctx, &pb.FollowLogRequest{Id: "cmd1"})
			if err != nil {
				return err
			}
			defer st.Close()
			_, err = st.Receive()
			return err
		}(),
		"WatchWorkspace": func() error {
			st, err := wsClient.WatchWorkspace(ctx, &pb.WatchWorkspaceRequest{Id: "ws1"})
			if err != nil {
				return err
			}
			defer st.Close()
			_, err = st.Receive()
			return err
		}(),
	} {
		if connect.CodeOf(callErr) != connect.CodeUnimplemented {
			t.Errorf("controller %s err = %v (code %v), want CodeUnimplemented", name, callErr, connect.CodeOf(callErr))
		}
	}

	// Verify short unary worker calls attach context deadlines even with context.Background().
	wsAPI := &workspaces{Server: ctrlSrv}
	if _, err := wsAPI.GetWorkspace(context.Background(), &pb.GetWorkspaceRequest{Id: "ws1"}); err != nil {
		t.Fatalf("GetWorkspace failed: %v", err)
	}
	if !getHasDeadline.Load() {
		t.Errorf("GetWorkspace did not set a context deadline on worker RPC")
	}
	if err := ctrlSrv.releaseWorkspace(context.Background(), "ws1"); err != nil {
		t.Fatalf("releaseWorkspace failed: %v", err)
	}
	if !releaseHasDeadline.Load() {
		t.Errorf("releaseWorkspace did not set a context deadline on worker RPC")
	}
}

// Regression test for Finding #94 (board 8289):
// Accumulated session cleanups for already-committed terminal runs must still
// run when a later settlement step or schedule-list step fails.
func TestTickSchedulesEndsAccumulatedSessionsOnLaterError(t *testing.T) {
	ctx := t.Context()
	s := openTestStore(t)
	now := time.Unix(1795320000, 0)

	node := tailnet.Peer{NodeID: "n1", Hostname: "w1", Login: "ramon@"}
	if _, err := s.Join(ctx, node, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddProject(ctx, "app", nil, &pb.Stamp{At: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSchedule(ctx, &pb.ScheduledCommand{
		Name: "job-a", Project: "app", Repo: "app", Cron: "0 2 * * *",
		Command: &pb.RunCommandRequest{Args: []string{"true"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSchedule(ctx, &pb.ScheduledCommand{
		Name: "job-b", Project: "app", Repo: "app", Cron: "0 3 * * *",
		Command: &pb.RunCommandRequest{Args: []string{"true"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartSession(ctx, "sched-sess-a", "schedule:job-a", "app", "", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartSession(ctx, "sched-sess-b", "schedule:job-b", "app", "", now); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRun(ctx, &pb.ScheduledRun{
		Schedule: "job-a", SessionId: "sched-sess-a", CommandId: "cmd-a",
		StartedAt: now.UnixMilli(), Outcome: pb.RunOutcome_RUN_OUTCOME_RUNNING,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRun(ctx, &pb.ScheduledRun{
		Schedule: "job-b", SessionId: "sched-sess-b", CommandId: "cmd-b",
		StartedAt: now.UnixMilli(), Outcome: pb.RunOutcome_RUN_OUTCOME_RUNNING,
	}); err != nil {
		t.Fatal(err)
	}
	// Record both commands as succeeded in history via PushEvents.
	if _, err := s.Push(ctx, node.NodeID, &pb.PushEventsRequest{
		Epoch:      "e1",
		Checkpoint: &pb.Checkpoint{Seq: 2},
		Events: []*pb.Event{
			{Seq: 1, At: now.UnixMilli(), Change: &pb.Event_Command{Command: &pb.CommandChange{
				What:     "exited",
				Command:  &pb.Command{Id: "cmd-a", SessionId: "sched-sess-a", NodeId: node.NodeID, State: pb.CommandState_COMMAND_STATE_SUCCEEDED},
				Finished: &pb.FinishedCommand{Id: "cmd-a", SessionId: "sched-sess-a", NodeId: node.NodeID, State: pb.CommandState_COMMAND_STATE_SUCCEEDED, EndedAt: now.UnixMilli()},
			}}},
			{Seq: 2, At: now.UnixMilli(), Change: &pb.Event_Command{Command: &pb.CommandChange{
				What:     "exited",
				Command:  &pb.Command{Id: "cmd-b", SessionId: "sched-sess-b", NodeId: node.NodeID, State: pb.CommandState_COMMAND_STATE_SUCCEEDED},
				Finished: &pb.FinishedCommand{Id: "cmd-b", SessionId: "sched-sess-b", NodeId: node.NodeID, State: pb.CommandState_COMMAND_STATE_SUCCEEDED, EndedAt: now.UnixMilli()},
			}}},
		},
	}, now, nil); err != nil {
		t.Fatal(err)
	}

	srv := NewServer(ServerConfig{Store: s, Now: func() time.Time { return now }})

	// 1. Fail on the 2nd GetHistoryDoc inside settleRunsLocked after job-a is already committed.
	var historyCalls atomic.Int32
	s.q = controllerdb.New(&hookDBTX{
		base: s.db,
		onQuery: func(q string) error {
			if strings.Contains(q, "GetHistoryDoc") && historyCalls.Add(1) == 2 {
				return errors.New("injected history read error")
			}
			return nil
		},
	})
	if err := srv.TickSchedules(ctx); err == nil {
		t.Fatalf("TickSchedules succeeded despite injected history error, want error")
	}
	s.q = controllerdb.New(s.db)
	sessA, err := s.Session(ctx, "sched-sess-a")
	if err != nil {
		t.Fatal(err)
	}
	if sessA.EndedAt == nil {
		t.Errorf("sched-sess-a was not ended after job-a settled and a later run errored")
	}
	// job-b was not settled yet, so sched-sess-b should still be live.
	sessB, err := s.Session(ctx, "sched-sess-b")
	if err != nil {
		t.Fatal(err)
	}
	if sessB.EndedAt != nil {
		t.Errorf("sched-sess-b ended unexpectedly: %+v", sessB)
	}

	// 2. Now settle job-b, then fail ListSchedules in TickSchedules: sched-sess-b must still end.
	s.q = controllerdb.New(&hookDBTX{
		base: s.db,
		onQuery: func(q string) error {
			if strings.Contains(q, "ListSchedules") {
				return errors.New("injected ListSchedules error")
			}
			return nil
		},
	})
	if err := srv.TickSchedules(ctx); err == nil {
		t.Fatalf("TickSchedules succeeded despite injected ListSchedules error, want error")
	}
	s.q = controllerdb.New(s.db)
	sessB, err = s.Session(ctx, "sched-sess-b")
	if err != nil {
		t.Fatal(err)
	}
	if sessB.EndedAt == nil {
		t.Errorf("sched-sess-b was not ended after settleRunsLocked succeeded and ListSchedules failed")
	}
}

// Regression test for board 7983:
// Starting a scheduled run onto a sleeping node must NOT hold s.scheduling
// while waiting for the node to wake; PutSchedule, DeleteSchedule, concurrent
// RunSchedule, and ticking another schedule on an awake node must all succeed
// immediately without waiting for the sleeping node to boot.
func TestScheduleSleepingNodeDoesNotBlockLockOrOtherSchedules(t *testing.T) {
	ctx := t.Context()
	s := openTestStore(t)
	var clockMu sync.Mutex
	cur := time.Date(2026, 11, 23, 2, 0, 10, 0, time.UTC)
	nowFn := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return cur
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		cur = cur.Add(d)
		clockMu.Unlock()
	}

	sleepyWorker := &blockingWorker{
		onCreate: func(_ context.Context, req *pb.CreateWorkspaceRequest) (*pb.Workspace, error) {
			return &pb.Workspace{Id: req.Id, SessionId: req.SessionId, NodeId: "nSleepy", State: pb.WorkspaceState_WORKSPACE_STATE_IDLE}, nil
		},
	}
	awakeWorker := &blockingWorker{
		onCreate: func(_ context.Context, req *pb.CreateWorkspaceRequest) (*pb.Workspace, error) {
			return &pb.Workspace{Id: req.Id, SessionId: req.SessionId, NodeId: "nAwake", State: pb.WorkspaceState_WORKSPACE_STATE_IDLE}, nil
		},
	}
	sleepyAddr := startStubWorkerServer(t, sleepyWorker)
	awakeAddr := startStubWorkerServer(t, awakeWorker)

	nSleepy := tailnet.Peer{NodeID: "nSleepy", Hostname: "sleepy", Login: "ramon@", Addr: netip.MustParseAddr("100.64.0.21")}
	nAwake := tailnet.Peer{NodeID: "nAwake", Hostname: "awake", Login: "ramon@", Addr: netip.MustParseAddr("100.64.0.22")}
	for _, p := range []tailnet.Peer{nSleepy, nAwake} {
		if _, err := s.Join(ctx, p, nowFn().Add(-10*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	res := &pb.Resources{Arch: "amd64", Cpus: 4, MemoryBytes: 16 << 30}
	if err := s.Beat(ctx, nSleepy.NodeID, &pb.HeartbeatRequest{Resources: res}, nowFn().Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.Beat(ctx, nAwake.NodeID, &pb.HeartbeatRequest{Resources: res}, nowFn()); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRepo(ctx, &pb.Repo{Id: "app", Url: "https://github.com/acme/app.git"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddProject(ctx, "app", []string{"github.com/acme/app"}, &pb.Stamp{At: nowFn().UnixMilli()}); err != nil {
		t.Fatal(err)
	}

	addrMap := map[string]string{
		"100.64.0.21": sleepyAddr,
		"100.64.0.22": awakeAddr,
	}
	srv := NewServer(ServerConfig{
		Store:      s,
		Now:        nowFn,
		WakeWait:   100 * time.Millisecond,
		WorkerAddr: func(h string) string { return addrMap[h] },
	})
	scAPI := &schedules{srv}

	// Put sleepy under Standby supervision in stopped (asleep) state.
	releaseSleepy := srv.power.hold("sleepy", false, 30*time.Minute)
	defer func() {
		if releaseSleepy != nil {
			releaseSleepy()
		}
	}()

	slot := time.Date(2026, 11, 23, 2, 0, 0, 0, time.UTC).UnixMilli()
	if err := s.PutSchedule(ctx, &pb.ScheduledCommand{
		Name: "sleepy-job", Project: "app", Repo: "app", Node: "sleepy",
		Cron: "CRON_TZ=UTC 0 2 * * *", NextAt: &slot,
		Command: &pb.RunCommandRequest{Args: []string{"echo", "sleepy"}},
	}); err != nil {
		t.Fatal(err)
	}

	// Start ticking sleepy-job; it will block in createAwake waiting for sleepy to boot.
	tickDone := make(chan error, 1)
	go func() {
		tickDone <- srv.TickSchedules(ctx)
	}()

	// Wait deterministically until sleepy-job has reserved its run and woken sleepy.
	deadline := time.Now().Add(3 * time.Second)
	for {
		live, ok, err := s.LiveRun(ctx, "sleepy-job")
		if err != nil {
			t.Fatal(err)
		}
		srv.power.mu.Lock()
		waking := srv.power.nodes["sleepy"] != nil && srv.power.nodes["sleepy"].want
		srv.power.mu.Unlock()
		if ok && live != nil && waking {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for sleepy-job to enter awaken (live=%v waking=%v)", ok, waking)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Confirm the tick is indeed still blocked waiting on the sleeping node.
	select {
	case err := <-tickDone:
		t.Fatalf("TickSchedules returned early while sleepy was still asleep: %v", err)
	default:
	}

	// 1. PutSchedule must succeed immediately without blocking on s.scheduling.
	putDone := make(chan error, 1)
	go func() {
		_, err := scAPI.PutSchedule(ctx, &pb.PutScheduleRequest{
			Schedule: &pb.ScheduledCommand{
				Name: "extra-job", Project: "app", Repo: "app", Node: "awake",
				Cron:    "CRON_TZ=UTC 0 3 * * *",
				Command: &pb.RunCommandRequest{Args: []string{"echo", "extra"}},
			},
		})
		putDone <- err
	}()
	select {
	case err := <-putDone:
		if err != nil {
			t.Fatalf("PutSchedule during sleeping-node wake failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PutSchedule blocked on s.scheduling while sleepy node was waking")
	}

	// 2. DeleteSchedule must succeed immediately without blocking on s.scheduling.
	delDone := make(chan error, 1)
	go func() {
		_, err := scAPI.DeleteSchedule(ctx, &pb.DeleteScheduleRequest{Name: "extra-job"})
		delDone <- err
	}()
	select {
	case err := <-delDone:
		if err != nil {
			t.Fatalf("DeleteSchedule during sleeping-node wake failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DeleteSchedule blocked on s.scheduling while sleepy node was waking")
	}

	// 3. Another schedule due on an awake node must start immediately on a tick,
	//    while sleepy-job's in-flight run is neither duplicated nor failed.
	if err := s.PutSchedule(ctx, &pb.ScheduledCommand{
		Name: "awake-job", Project: "app", Repo: "app", Node: "awake",
		Cron: "CRON_TZ=UTC 0 2 * * *", NextAt: &slot,
		Command: &pb.RunCommandRequest{Args: []string{"echo", "awake"}},
	}); err != nil {
		t.Fatal(err)
	}
	awakeTickDone := make(chan error, 1)
	go func() {
		awakeTickDone <- srv.TickSchedules(ctx)
	}()
	select {
	case err := <-awakeTickDone:
		if err != nil {
			t.Fatalf("concurrent TickSchedules for awake-job failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("TickSchedules for awake-job blocked while sleepy node was waking")
	}
	awakeRun, ok, err := s.LiveRun(ctx, "awake-job")
	if err != nil || !ok || awakeRun.CommandId == "" {
		t.Fatalf("awake-job run = %+v ok=%v err=%v, want started with CommandId", awakeRun, ok, err)
	}
	// Duplicate start of sleepy-job must be rejected immediately with CONFLICT.
	if _, err := scAPI.RunSchedule(ctx, &pb.RunScheduleRequest{Name: "sleepy-job"}); api.CodeOf(err) != api.CodeConflict {
		t.Fatalf("RunSchedule(sleepy-job) while waking = %v, want CONFLICT", err)
	}

	// 4. Wake sleepy; the original TickSchedules finishes cleanly and launches the command.
	releaseSleepy()
	releaseSleepy = srv.power.hold("sleepy", true, 30*time.Minute)
	advance(time.Second)
	if err := s.Beat(ctx, nSleepy.NodeID, &pb.HeartbeatRequest{Resources: res}, nowFn()); err != nil {
		t.Fatal(err)
	}
	srv.power.changes.wake("sleepy")

	select {
	case err := <-tickDone:
		if err != nil {
			t.Fatalf("sleepy-job TickSchedules failed after wake: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("sleepy-job TickSchedules did not finish after sleepy woke")
	}
	sleepyRun, ok, err := s.LiveRun(ctx, "sleepy-job")
	if err != nil || !ok || sleepyRun.CommandId == "" || sleepyRun.NodeId != nSleepy.NodeID {
		t.Fatalf("sleepy-job after wake = %+v ok=%v err=%v, want running on nSleepy with CommandId", sleepyRun, ok, err)
	}
}

// Regression test for board 7983:
// Deleting or pausing a schedule while its cron-triggered run is blocked waking
// a sleeping node must succeed immediately and abort the in-flight start cleanly
// without launching any command.
func TestScheduleDeleteAndPauseWhileWakingSleepingNode(t *testing.T) {
	runCase := func(t *testing.T, action string) {
		t.Helper()
		ctx := t.Context()
		s := openTestStore(t)
		now := time.Date(2026, 11, 23, 2, 0, 10, 0, time.UTC)

		var runCmdCalls atomic.Int32
		bw := &blockingWorker{
			onRun: func(ctx context.Context, req *pb.RunCommandRequest) (*pb.Command, error) {
				runCmdCalls.Add(1)
				return &pb.Command{Id: req.Id, WorkspaceId: req.WorkspaceId, State: pb.CommandState_COMMAND_STATE_RUNNING}, nil
			},
		}
		workerAddr := startStubWorkerServer(t, bw)

		nSleepy := tailnet.Peer{NodeID: "nSleepy", Hostname: "sleepy", Login: "ramon@", Addr: netip.MustParseAddr("100.64.0.21")}
		if _, err := s.Join(ctx, nSleepy, now.Add(-10*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := s.Beat(ctx, nSleepy.NodeID, &pb.HeartbeatRequest{
			Resources: &pb.Resources{Arch: "amd64", Cpus: 4, MemoryBytes: 16 << 30},
		}, now.Add(-5*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := s.PutRepo(ctx, &pb.Repo{Id: "app", Url: "https://github.com/acme/app.git"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddProject(ctx, "app", []string{"github.com/acme/app"}, &pb.Stamp{At: now.UnixMilli()}); err != nil {
			t.Fatal(err)
		}

		srv := NewServer(ServerConfig{
			Store:      s,
			Now:        func() time.Time { return now },
			WakeWait:   100 * time.Millisecond,
			WorkerAddr: func(string) string { return workerAddr },
		})
		scAPI := &schedules{srv}
		defer srv.power.hold("sleepy", false, 30*time.Minute)()

		slot := time.Date(2026, 11, 23, 2, 0, 0, 0, time.UTC).UnixMilli()
		if err := s.PutSchedule(ctx, &pb.ScheduledCommand{
			Name: "sleepy-job", Project: "app", Repo: "app", Node: "sleepy",
			Cron: "CRON_TZ=UTC 0 2 * * *", NextAt: &slot,
			Command: &pb.RunCommandRequest{Args: []string{"echo", "sleepy"}},
		}); err != nil {
			t.Fatal(err)
		}

		tickDone := make(chan error, 1)
		go func() {
			tickDone <- srv.TickSchedules(ctx)
		}()

		var reserved *pb.ScheduledRun
		deadline := time.Now().Add(3 * time.Second)
		for {
			live, ok, err := s.LiveRun(ctx, "sleepy-job")
			if err != nil {
				t.Fatal(err)
			}
			srv.power.mu.Lock()
			waking := srv.power.nodes["sleepy"] != nil && srv.power.nodes["sleepy"].want
			srv.power.mu.Unlock()
			if ok && live != nil && waking {
				reserved = live
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for sleepy-job to enter awaken")
			}
			time.Sleep(10 * time.Millisecond)
		}

		switch action {
		case "delete":
			if _, err := scAPI.DeleteSchedule(ctx, &pb.DeleteScheduleRequest{Name: "sleepy-job"}); err != nil {
				t.Fatalf("DeleteSchedule while waking: %v", err)
			}
		case "pause":
			if _, err := scAPI.PauseSchedule(ctx, &pb.PauseScheduleRequest{Name: "sleepy-job", Paused: true}); err != nil {
				t.Fatalf("PauseSchedule while waking: %v", err)
			}
		}

		select {
		case err := <-tickDone:
			if err != nil {
				t.Fatalf("TickSchedules after %s returned error: %v", action, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("TickSchedules did not abort after %s while waking sleeping node", action)
		}

		gotRun, err := s.Run(ctx, reserved.Id)
		if err != nil {
			t.Fatal(err)
		}
		if gotRun.Outcome != pb.RunOutcome_RUN_OUTCOME_NOT_STARTED || gotRun.EndedAt == nil {
			t.Errorf("run after %s = %+v, want RUN_OUTCOME_NOT_STARTED with EndedAt", action, gotRun)
		}
		if sess, err := s.Session(ctx, reserved.SessionId); err != nil || sess.EndedAt == nil {
			t.Errorf("session after %s = %+v err=%v, want ended", action, sess, err)
		}
		if runCmdCalls.Load() != 0 {
			t.Errorf("RunCommand was called %d times after %s, want 0", runCmdCalls.Load(), action)
		}
	}

	t.Run("delete while waking", func(t *testing.T) { runCase(t, "delete") })
	t.Run("pause while waking", func(t *testing.T) { runCase(t, "pause") })
}

// Regression test for board 7957 / 7959 / 7961 / 7963 / 7981 / 7913:
// (a) When CreateWorkspace is cancelled while the worker ignores cancellation
//
//	and still creates fwd.Id, make() releases fwd.Id on the worker using a
//	bounded context.WithoutCancel timeout, drops the controller DB row, and
//	a subsequent CreateWorkspace succeeds cleanly.
//
// (b) When ReleaseWorkspace returns CodeNotFound after cancellation, cleanup
//
//	still succeeds cleanly without leaving a phantom row.
//
// (c) Failed workspaces (both in node_state view and when the view lagged or
//
//	was absent while the live worker reports FAILED) are released and replaced
//	by a fresh workspace (board 7913).
func TestCreateWorkspaceCancelOrphanReleaseAndFailedReplacement(t *testing.T) {
	ctx := t.Context()
	now := time.Unix(1795320000, 0)

	t.Run("canceled create releases orphan workspace created on worker", func(t *testing.T) {
		s := openTestStore(t)
		createdOnWorker := make(chan string, 1)
		finishWorkerCreate := make(chan struct{})
		defer close(finishWorkerCreate)

		var releaseMu sync.Mutex
		var releasedIDs []string
		var releaseHadDeadline atomic.Bool

		bw := &blockingWorker{
			workspaces: map[string]*pb.Workspace{},
		}
		bw.onCreate = func(_ context.Context, req *pb.CreateWorkspaceRequest) (*pb.Workspace, error) {
			// Simulate a worker that ignores ctx cancellation and finishes creating fwd.Id.
			ws := &pb.Workspace{Id: req.Id, SessionId: req.SessionId, NodeId: "n1", State: pb.WorkspaceState_WORKSPACE_STATE_IDLE}
			bw.mu.Lock()
			bw.workspaces[req.Id] = ws
			bw.mu.Unlock()
			select {
			case createdOnWorker <- req.Id:
			default:
			}
			<-finishWorkerCreate
			return proto.Clone(ws).(*pb.Workspace), nil
		}
		bw.onRelease = func(rctx context.Context, req *pb.ReleaseWorkspaceRequest) (*pb.ReleaseWorkspaceResponse, error) {
			if _, ok := rctx.Deadline(); ok {
				releaseHadDeadline.Store(true)
			}
			releaseMu.Lock()
			releasedIDs = append(releasedIDs, req.Id)
			releaseMu.Unlock()
			bw.mu.Lock()
			delete(bw.workspaces, req.Id)
			bw.mu.Unlock()
			return &pb.ReleaseWorkspaceResponse{}, nil
		}
		workerAddr := startStubWorkerServer(t, bw)

		node := tailnet.Peer{NodeID: "n1", Hostname: "w1", Login: "ramon@", Addr: netip.MustParseAddr("100.64.0.1")}
		if _, err := s.Join(ctx, node, now); err != nil {
			t.Fatal(err)
		}
		if err := s.Beat(ctx, node.NodeID, &pb.HeartbeatRequest{Resources: &pb.Resources{Arch: "amd64", Cpus: 4, MemoryBytes: 16 << 30}}, now); err != nil {
			t.Fatal(err)
		}
		if err := s.PutRepo(ctx, &pb.Repo{Id: "app", Url: "https://github.com/acme/app.git"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddProject(ctx, "app", []string{"github.com/acme/app"}, &pb.Stamp{At: now.UnixMilli()}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.StartSession(ctx, "cancel-fox", "laptop", "app", "", now); err != nil {
			t.Fatal(err)
		}

		srv := NewServer(ServerConfig{
			Store:      s,
			Now:        func() time.Time { return now },
			WorkerAddr: func(string) string { return workerAddr },
		})
		wsAPI := &workspaces{Server: srv}

		callCtx, cancel := context.WithCancel(ctx)
		errCh := make(chan error, 1)
		go func() {
			_, err := wsAPI.CreateWorkspace(callCtx, &pb.CreateWorkspaceRequest{SessionId: "cancel-fox", Repo: "app"})
			errCh <- err
		}()

		orphanID := <-createdOnWorker
		cancel()
		if err := <-errCh; err == nil {
			t.Fatal("expected canceled CreateWorkspace to return an error")
		}

		// Worker must have received ReleaseWorkspace(orphanID) with a bounded deadline,
		// leaving zero orphan workspaces on the worker.
		releaseMu.Lock()
		gotReleased := append([]string(nil), releasedIDs...)
		releaseMu.Unlock()
		if len(gotReleased) != 1 || gotReleased[0] != orphanID {
			t.Errorf("worker ReleaseWorkspace calls = %v, want [%s]", gotReleased, orphanID)
		}
		if !releaseHadDeadline.Load() {
			t.Errorf("orphan ReleaseWorkspace RPC did not have a bounded context deadline")
		}
		bw.mu.Lock()
		remainingOnWorker := len(bw.workspaces)
		bw.mu.Unlock()
		if remainingOnWorker != 0 {
			t.Errorf("worker still holds %d workspaces after canceled create, want 0", remainingOnWorker)
		}

		// Controller DB must have no phantom row.
		sess, err := s.Session(ctx, "cancel-fox")
		if err != nil {
			t.Fatal(err)
		}
		if len(sess.Workspaces) != 0 {
			t.Errorf("controller DB still has workspaces %v after canceled create, want none", sess.Workspaces)
		}

		// Subsequent CreateWorkspace succeeds cleanly with a fresh workspace.
		bw.onCreate = nil
		fresh, err := wsAPI.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{SessionId: "cancel-fox", Repo: "app"})
		if err != nil {
			t.Fatalf("subsequent CreateWorkspace after cancel failed: %v", err)
		}
		if fresh.Id == "" || fresh.Id == orphanID {
			t.Errorf("fresh workspace ID = %q, want new ID distinct from %q", fresh.Id, orphanID)
		}
	})

	t.Run("canceled create ignores CodeNotFound from ReleaseWorkspace", func(t *testing.T) {
		s := openTestStore(t)
		entered := make(chan struct{})
		var releaseCalls atomic.Int32
		bw := &blockingWorker{
			workspaces: map[string]*pb.Workspace{},
			onCreate: func(cctx context.Context, _ *pb.CreateWorkspaceRequest) (*pb.Workspace, error) {
				close(entered)
				<-cctx.Done()
				return nil, cctx.Err()
			},
			onRelease: func(_ context.Context, req *pb.ReleaseWorkspaceRequest) (*pb.ReleaseWorkspaceResponse, error) {
				releaseCalls.Add(1)
				return nil, api.ToConnect(api.Errorf(api.CodeNotFound, "no workspace %s", req.Id))
			},
		}
		workerAddr := startStubWorkerServer(t, bw)

		node := tailnet.Peer{NodeID: "n1", Hostname: "w1", Login: "ramon@", Addr: netip.MustParseAddr("100.64.0.1")}
		if _, err := s.Join(ctx, node, now); err != nil {
			t.Fatal(err)
		}
		if err := s.Beat(ctx, node.NodeID, &pb.HeartbeatRequest{Resources: &pb.Resources{Arch: "amd64", Cpus: 4, MemoryBytes: 16 << 30}}, now); err != nil {
			t.Fatal(err)
		}
		if err := s.PutRepo(ctx, &pb.Repo{Id: "app", Url: "https://github.com/acme/app.git"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddProject(ctx, "app", []string{"github.com/acme/app"}, &pb.Stamp{At: now.UnixMilli()}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.StartSession(ctx, "cancel-notfound", "laptop", "app", "", now); err != nil {
			t.Fatal(err)
		}

		srv := NewServer(ServerConfig{
			Store:      s,
			Now:        func() time.Time { return now },
			WorkerAddr: func(string) string { return workerAddr },
		})
		wsAPI := &workspaces{Server: srv}

		callCtx, cancel := context.WithCancel(ctx)
		errCh := make(chan error, 1)
		go func() {
			_, err := wsAPI.CreateWorkspace(callCtx, &pb.CreateWorkspaceRequest{SessionId: "cancel-notfound", Repo: "app"})
			errCh <- err
		}()
		<-entered
		cancel()
		if err := <-errCh; err == nil {
			t.Fatal("expected canceled CreateWorkspace to return an error")
		}
		if releaseCalls.Load() != 1 {
			t.Errorf("ReleaseWorkspace calls = %d, want 1", releaseCalls.Load())
		}
		sess, err := s.Session(ctx, "cancel-notfound")
		if err != nil {
			t.Fatal(err)
		}
		if len(sess.Workspaces) != 0 {
			t.Errorf("controller DB workspaces = %v, want none", sess.Workspaces)
		}
		bw.onCreate = nil
		if _, err := wsAPI.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{SessionId: "cancel-notfound", Repo: "app"}); err != nil {
			t.Fatalf("next CreateWorkspace after canceled create failed: %v", err)
		}
	})

	t.Run("failed workspace replaced even when node view lagged or absent (board 7913)", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			viewState *pb.WorkspaceState // nil = workspace added after last checkpoint (needProbe)
		}{
			{name: "view reports failed", viewState: pb.WorkspaceState_WORKSPACE_STATE_FAILED.Enum()},
			{name: "view lagged as idle while worker reports failed", viewState: pb.WorkspaceState_WORKSPACE_STATE_IDLE.Enum()},
			{name: "absent from view while worker reports failed", viewState: nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				s := openTestStore(t)
				var releaseMu sync.Mutex
				var releasedIDs []string

				bw := &blockingWorker{
					workspaces: map[string]*pb.Workspace{
						"ws-broken": {Id: "ws-broken", SessionId: "fail-fox", NodeId: "n1", State: pb.WorkspaceState_WORKSPACE_STATE_FAILED},
					},
				}
				bw.onRelease = func(_ context.Context, req *pb.ReleaseWorkspaceRequest) (*pb.ReleaseWorkspaceResponse, error) {
					releaseMu.Lock()
					releasedIDs = append(releasedIDs, req.Id)
					releaseMu.Unlock()
					bw.mu.Lock()
					delete(bw.workspaces, req.Id)
					bw.mu.Unlock()
					return &pb.ReleaseWorkspaceResponse{}, nil
				}
				workerAddr := startStubWorkerServer(t, bw)

				node := tailnet.Peer{NodeID: "n1", Hostname: "w1", Login: "ramon@", Addr: netip.MustParseAddr("100.64.0.1")}
				if _, err := s.Join(ctx, node, now); err != nil {
					t.Fatal(err)
				}
				if err := s.Beat(ctx, node.NodeID, &pb.HeartbeatRequest{Resources: &pb.Resources{Arch: "amd64", Cpus: 4, MemoryBytes: 16 << 30}}, now); err != nil {
					t.Fatal(err)
				}
				if err := s.PutRepo(ctx, &pb.Repo{Id: "app", Url: "https://github.com/acme/app.git"}); err != nil {
					t.Fatal(err)
				}
				if _, err := s.AddProject(ctx, "app", []string{"github.com/acme/app"}, &pb.Stamp{At: now.UnixMilli()}); err != nil {
					t.Fatal(err)
				}
				if _, err := s.StartSession(ctx, "fail-fox", "laptop", "app", "", now); err != nil {
					t.Fatal(err)
				}
				cp := &pb.Checkpoint{Seq: 1}
				if tc.viewState != nil {
					if err := s.AddWorkspace(ctx, "ws-broken", "fail-fox", node.NodeID, "laptop", now); err != nil {
						t.Fatal(err)
					}
					cp.Workspaces = []*pb.Workspace{{
						Id: "ws-broken", SessionId: "fail-fox", NodeId: node.NodeID, State: *tc.viewState,
					}}
					if _, err := s.Push(ctx, node.NodeID, &pb.PushEventsRequest{Epoch: "e1", Checkpoint: cp}, now, nil); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := s.Push(ctx, node.NodeID, &pb.PushEventsRequest{Epoch: "e1", Checkpoint: cp}, now, nil); err != nil {
						t.Fatal(err)
					}
					// Added after the last checkpoint so node_state does not have it yet.
					if err := s.AddWorkspace(ctx, "ws-broken", "fail-fox", node.NodeID, "laptop", now); err != nil {
						t.Fatal(err)
					}
				}

				srv := NewServer(ServerConfig{
					Store:      s,
					Now:        func() time.Time { return now },
					WorkerAddr: func(string) string { return workerAddr },
				})
				wsAPI := &workspaces{Server: srv}

				fresh, err := wsAPI.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{SessionId: "fail-fox", Repo: "app"})
				if err != nil {
					t.Fatalf("CreateWorkspace with failed workspace failed: %v", err)
				}
				if fresh.Id == "ws-broken" || fresh.State != pb.WorkspaceState_WORKSPACE_STATE_IDLE {
					t.Errorf("got workspace %+v, want fresh IDLE workspace != ws-broken", fresh)
				}
				releaseMu.Lock()
				gotReleased := append([]string(nil), releasedIDs...)
				releaseMu.Unlock()
				if len(gotReleased) != 1 || gotReleased[0] != "ws-broken" {
					t.Errorf("released workspaces = %v, want [ws-broken]", gotReleased)
				}
				oldRow, err := s.Workspace(ctx, "ws-broken")
				if err != nil || !oldRow.released {
					t.Errorf("old workspace row = %+v err=%v, want released=true", oldRow, err)
				}
			})
		}
	})
}

// Regression test for manual RunSchedule shutdown lifecycle:
// Controller.Serve must cancel an in-flight manual RunSchedule (even though it
// detaches from its request ctx) and wait for its cleanup to finish before
// closing the SQLite store.
func TestScheduleShutdownWaitsForAndCancelsManualRunSchedule(t *testing.T) {
	now := time.Date(2026, 11, 23, 2, 0, 10, 0, time.UTC)

	setupController := func(t *testing.T, dbPath string, bw *blockingWorker, asleep bool) (*Controller, *Store) {
		t.Helper()
		ctx := t.Context()
		s, err := OpenStore(ctx, dbPath)
		if err != nil {
			t.Fatal(err)
		}
		workerAddr := startStubWorkerServer(t, bw)
		node := tailnet.Peer{NodeID: "n1", Hostname: "w1", Login: "ramon@", Addr: netip.MustParseAddr("100.64.0.31")}
		if _, err := s.Join(ctx, node, now.Add(-10*time.Minute)); err != nil {
			t.Fatal(err)
		}
		beatAt := now
		if asleep {
			beatAt = now.Add(-5 * time.Minute)
		}
		if err := s.Beat(ctx, node.NodeID, &pb.HeartbeatRequest{
			Resources: &pb.Resources{Arch: "amd64", Cpus: 4, MemoryBytes: 16 << 30},
		}, beatAt); err != nil {
			t.Fatal(err)
		}
		if err := s.PutRepo(ctx, &pb.Repo{Id: "app", Url: "https://github.com/acme/app.git"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddProject(ctx, "app", []string{"github.com/acme/app"}, &pb.Stamp{At: now.UnixMilli()}); err != nil {
			t.Fatal(err)
		}
		if err := s.PutSchedule(ctx, &pb.ScheduledCommand{
			Name: "manual-job", Project: "app", Repo: "app", Node: "w1",
			Cron: "CRON_TZ=UTC 0 2 * * *",
			Command: &pb.RunCommandRequest{Args: []string{"echo", "hi"}},
		}); err != nil {
			t.Fatal(err)
		}

		srv := NewServer(ServerConfig{
			Store:      s,
			Now:        func() time.Time { return now },
			WakeWait:   100 * time.Millisecond,
			WorkerAddr: func(string) string { return workerAddr },
		})
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		c := &Controller{
			store: s,
			api:   srv,
			srv: &http.Server{
				Handler:           srv.Handler(),
				ReadHeaderTimeout: 10 * time.Second,
			},
			listeners: []net.Listener{ln},
		}
		return c, s
	}

	t.Run("cancels manual RunSchedule waking sleeping node before DB close", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "hotbed.db")
		var runCmdCalls atomic.Int32
		bw := &blockingWorker{
			onRun: func(_ context.Context, req *pb.RunCommandRequest) (*pb.Command, error) {
				runCmdCalls.Add(1)
				return &pb.Command{Id: req.Id, WorkspaceId: req.WorkspaceId, State: pb.CommandState_COMMAND_STATE_RUNNING}, nil
			},
		}
		c, s := setupController(t, dbPath, bw, true)
		defer c.api.power.hold("w1", false, 30*time.Minute)()

		serveCtx, serveCancel := context.WithCancel(t.Context())
		serveDone := make(chan error, 1)
		go func() { serveDone <- c.Serve(serveCtx) }()

		scAPI := &schedules{c.api}
		type runResult struct {
			run *pb.ScheduledRun
			err error
		}
		runDone := make(chan runResult, 1)
		go func() {
			run, err := scAPI.RunSchedule(context.Background(), &pb.RunScheduleRequest{Name: "manual-job"})
			runDone <- runResult{run: run, err: err}
		}()

		// Wait until the manual start has reserved its run and is waking w1.
		deadline := time.Now().Add(3 * time.Second)
		for {
			live, ok, err := s.LiveRun(t.Context(), "manual-job")
			if err != nil {
				t.Fatal(err)
			}
			c.api.power.mu.Lock()
			waking := c.api.power.nodes["w1"] != nil && c.api.power.nodes["w1"].want
			c.api.power.mu.Unlock()
			if ok && live != nil && waking {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for manual-job to enter awaken (live=%v waking=%v)", ok, waking)
			}
			time.Sleep(10 * time.Millisecond)
		}

		// Shut down the controller: it must cancel the manual start and wait for
		// its cleanup before closing the DB.
		serveCancel()

		var res runResult
		select {
		case res = <-runDone:
		case <-time.After(3 * time.Second):
			t.Fatal("manual RunSchedule did not abort on controller shutdown")
		}
		select {
		case err := <-serveDone:
			if err != nil {
				t.Fatalf("Controller.Serve returned error on shutdown: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Controller.Serve did not finish after shutdown")
		}

		if res.err != nil {
			t.Fatalf("RunSchedule returned error (e.g. closed DB): %v", res.err)
		}
		if res.run == nil || res.run.Outcome != pb.RunOutcome_RUN_OUTCOME_NOT_STARTED || res.run.EndedAt == nil {
			t.Fatalf("RunSchedule returned %+v, want RUN_OUTCOME_NOT_STARTED with EndedAt", res.run)
		}
		if !strings.Contains(res.run.Reason, "the controller stopped while starting it") {
			t.Errorf("run reason = %q, want shutdown reason", res.run.Reason)
		}
		if runCmdCalls.Load() != 0 {
			t.Errorf("RunCommand was called %d times after shutdown, want 0", runCmdCalls.Load())
		}

		// Reopen the DB to confirm the terminal run and ended session were
		// persisted cleanly before store.Close().
		reopened, err := OpenStore(t.Context(), dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		gotRun, err := reopened.Run(t.Context(), res.run.Id)
		if err != nil {
			t.Fatal(err)
		}
		if gotRun.Outcome != pb.RunOutcome_RUN_OUTCOME_NOT_STARTED || gotRun.EndedAt == nil {
			t.Errorf("persisted run = %+v, want RUN_OUTCOME_NOT_STARTED with EndedAt", gotRun)
		}
		sess, err := reopened.Session(t.Context(), gotRun.SessionId)
		if err != nil {
			t.Fatal(err)
		}
		if sess.EndedAt == nil {
			t.Errorf("persisted session %s was not ended before DB close", sess.Id)
		}

		// A new manual RunSchedule after shutdown must fail fast without touching
		// the closed store.
		if _, err := scAPI.RunSchedule(context.Background(), &pb.RunScheduleRequest{Name: "manual-job"}); api.CodeOf(err) != api.CodeControllerUnreachable {
			t.Errorf("RunSchedule after shutdown = %v, want CONTROLLER_UNREACHABLE", err)
		}
	})

	t.Run("waits for manual RunSchedule in RunCommand before DB close", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "hotbed.db")
		runEntered := make(chan struct{})
		releaseRun := make(chan struct{})
		bw := &blockingWorker{
			onRun: func(_ context.Context, req *pb.RunCommandRequest) (*pb.Command, error) {
				close(runEntered)
				<-releaseRun
				return &pb.Command{Id: req.Id, WorkspaceId: req.WorkspaceId, State: pb.CommandState_COMMAND_STATE_RUNNING}, nil
			},
		}
		c, _ := setupController(t, dbPath, bw, false)

		serveCtx, serveCancel := context.WithCancel(t.Context())
		serveDone := make(chan error, 1)
		go func() { serveDone <- c.Serve(serveCtx) }()

		scAPI := &schedules{c.api}
		type runResult struct {
			run *pb.ScheduledRun
			err error
		}
		runDone := make(chan runResult, 1)
		go func() {
			run, err := scAPI.RunSchedule(context.Background(), &pb.RunScheduleRequest{Name: "manual-job"})
			runDone <- runResult{run: run, err: err}
		}()

		<-runEntered
		serveCancel()

		// Serve must NOT close the DB and return while RunSchedule is still in
		// RunCommand / finishing cleanup.
		select {
		case err := <-serveDone:
			t.Fatalf("Controller.Serve returned early while manual RunSchedule was still in flight: %v", err)
		case <-time.After(150 * time.Millisecond):
		}

		close(releaseRun)

		var res runResult
		select {
		case res = <-runDone:
		case <-time.After(3 * time.Second):
			t.Fatal("manual RunSchedule did not finish after RunCommand unblocked")
		}
		select {
		case err := <-serveDone:
			if err != nil {
				t.Fatalf("Controller.Serve returned error: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Controller.Serve did not finish after manual RunSchedule completed")
		}

		if res.err != nil {
			t.Fatalf("RunSchedule returned error: %v", res.err)
		}
		if res.run == nil || res.run.Outcome != pb.RunOutcome_RUN_OUTCOME_NOT_STARTED || res.run.EndedAt == nil {
			t.Fatalf("RunSchedule returned %+v, want RUN_OUTCOME_NOT_STARTED with EndedAt", res.run)
		}
		bw.mu.Lock()
		stopped := len(bw.stoppedCmds)
		bw.mu.Unlock()
		if stopped != 1 {
			t.Errorf("StopCommand called %d times during shutdown, want 1", stopped)
		}

		reopened, err := OpenStore(t.Context(), dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		gotRun, err := reopened.Run(t.Context(), res.run.Id)
		if err != nil {
			t.Fatal(err)
		}
		if gotRun.Outcome != pb.RunOutcome_RUN_OUTCOME_NOT_STARTED || gotRun.EndedAt == nil {
			t.Errorf("persisted run = %+v, want RUN_OUTCOME_NOT_STARTED with EndedAt", gotRun)
		}
		sess, err := reopened.Session(t.Context(), gotRun.SessionId)
		if err != nil {
			t.Fatal(err)
		}
		if sess.EndedAt == nil {
			t.Errorf("persisted session %s was not ended before DB close", sess.Id)
		}
	})
}

// Regression test: if DeleteSchedule or PauseSchedule arrives in the window
// after createAwake and before RunCommand returns, finishStart re-checks
// stopWhy, stops the just-launched command on the worker, marks the run
// RUN_OUTCOME_NOT_STARTED, and ends its session cleanly.
func TestScheduleStopDuringRunCommandStopsCommandAndEndsSession(t *testing.T) {
	runCase := func(t *testing.T, action string) {
		t.Helper()
		ctx := t.Context()
		s := openTestStore(t)
		now := time.Date(2026, 11, 23, 2, 0, 10, 0, time.UTC)

		runEntered := make(chan struct{})
		releaseRun := make(chan struct{})
		var releasedWS atomic.Int32
		bw := &blockingWorker{
			onRun: func(_ context.Context, req *pb.RunCommandRequest) (*pb.Command, error) {
				close(runEntered)
				<-releaseRun
				return &pb.Command{Id: req.Id, WorkspaceId: req.WorkspaceId, State: pb.CommandState_COMMAND_STATE_RUNNING}, nil
			},
			onRelease: func(_ context.Context, _ *pb.ReleaseWorkspaceRequest) (*pb.ReleaseWorkspaceResponse, error) {
				releasedWS.Add(1)
				return &pb.ReleaseWorkspaceResponse{}, nil
			},
		}
		workerAddr := startStubWorkerServer(t, bw)

		node := tailnet.Peer{NodeID: "n1", Hostname: "w1", Login: "ramon@", Addr: netip.MustParseAddr("100.64.0.1")}
		if _, err := s.Join(ctx, node, now); err != nil {
			t.Fatal(err)
		}
		if err := s.Beat(ctx, node.NodeID, &pb.HeartbeatRequest{Resources: &pb.Resources{Arch: "amd64", Cpus: 4, MemoryBytes: 16 << 30}}, now); err != nil {
			t.Fatal(err)
		}
		if err := s.PutRepo(ctx, &pb.Repo{Id: "app", Url: "https://github.com/acme/app.git"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddProject(ctx, "app", []string{"github.com/acme/app"}, &pb.Stamp{At: now.UnixMilli()}); err != nil {
			t.Fatal(err)
		}
		slot := time.Date(2026, 11, 23, 2, 0, 0, 0, time.UTC).UnixMilli()
		if err := s.PutSchedule(ctx, &pb.ScheduledCommand{
			Name: "nightly", Project: "app", Repo: "app",
			Cron: "CRON_TZ=UTC 0 2 * * *", NextAt: &slot,
			Command: &pb.RunCommandRequest{Args: []string{"echo", "hi"}},
		}); err != nil {
			t.Fatal(err)
		}

		srv := NewServer(ServerConfig{
			Store:      s,
			Now:        func() time.Time { return now },
			WorkerAddr: func(string) string { return workerAddr },
		})
		scAPI := &schedules{srv}

		done := make(chan error, 1)
		go func() {
			if action == "delete" {
				_, err := scAPI.RunSchedule(ctx, &pb.RunScheduleRequest{Name: "nightly"})
				done <- err
			} else {
				done <- srv.TickSchedules(ctx)
			}
		}()

		<-runEntered
		live, ok, err := s.LiveRun(ctx, "nightly")
		if err != nil || !ok {
			t.Fatalf("LiveRun = %v, %v, %v", live, ok, err)
		}

		switch action {
		case "delete":
			if _, err := scAPI.DeleteSchedule(ctx, &pb.DeleteScheduleRequest{Name: "nightly"}); err != nil {
				t.Fatalf("DeleteSchedule during RunCommand: %v", err)
			}
		case "pause":
			if _, err := scAPI.PauseSchedule(ctx, &pb.PauseScheduleRequest{Name: "nightly", Paused: true}); err != nil {
				t.Fatalf("PauseSchedule during RunCommand: %v", err)
			}
		}

		close(releaseRun)
		if err := <-done; err != nil {
			t.Fatalf("start returned error after %s: %v", action, err)
		}

		gotRun, err := s.Run(ctx, live.Id)
		if err != nil {
			t.Fatal(err)
		}
		if gotRun.Outcome != pb.RunOutcome_RUN_OUTCOME_NOT_STARTED || gotRun.EndedAt == nil {
			t.Errorf("run after %s during RunCommand = %+v, want RUN_OUTCOME_NOT_STARTED with EndedAt", action, gotRun)
		}
		if !strings.Contains(gotRun.Reason, action) {
			t.Errorf("run reason = %q, want mention of %q", gotRun.Reason, action)
		}
		sess, err := s.Session(ctx, live.SessionId)
		if err != nil || sess.EndedAt == nil {
			t.Errorf("session after %s = %+v err=%v, want ended", action, sess, err)
		}
		bw.mu.Lock()
		stopped := len(bw.stoppedCmds)
		bw.mu.Unlock()
		if stopped != 1 {
			t.Errorf("StopCommand called %d times after %s during RunCommand, want 1", stopped, action)
		}
		if releasedWS.Load() != 1 {
			t.Errorf("ReleaseWorkspace called %d times after %s during RunCommand, want 1", releasedWS.Load(), action)
		}
	}

	t.Run("delete during RunCommand", func(t *testing.T) { runCase(t, "delete") })
	t.Run("pause during RunCommand", func(t *testing.T) { runCase(t, "pause") })
}

// When all eligible nodes are still on build N-1 after a controller upgrade to
// build N, CreateWorkspace waits (without holding s.placing) until an eligible
// node heartbeats on build N, succeeds immediately on that heartbeat, and fails
// retryably if the wait budget expires (board 7989).
func TestCreateWorkspaceWaitsForUpgradingNode(t *testing.T) {
	ctx := t.Context()
	gh := updatetest.NewGitHub(t)
	gh.Publish(42)
	cache := update.Cache{Dir: t.TempDir()}
	rel, err := gh.Client().Latest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Put(ctx, gh.Client(), rel); err != nil {
		t.Fatal(err)
	}

	setup := func(t *testing.T, wait time.Duration) (*Server, *workspaces, *Store, *tailnettest.Fake, string, time.Time) {
		t.Helper()
		s := openTestStore(t)
		now := time.Unix(1795320000, 0)
		workerAddr := startStubWorkerServer(t, &blockingWorker{})
		ctrlPeer := tailnet.Self{
			Peer:  tailnet.Peer{NodeID: "nCtrl", Hostname: "ctrl", Login: "ramon@"},
			Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")},
		}
		tn := tailnettest.New(ctrlPeer)
		srv := NewServer(ServerConfig{
			Store:      s,
			Identifier: tn,
			Cluster:    statedir.ControllerConfig{ClusterID: "cl-1", Owner: "ramon@"},
			Self:       ctrlPeer.Addrs,
			Now:        func() time.Time { return now },
			WorkerAddr: func(string) string { return workerAddr },
			Builds:     &cache,
			Build:      42,
			WakeWait:   wait,
		})
		hs := httptest.NewServer(srv.Handler())
		t.Cleanup(hs.Close)
		if err := s.PutRepo(ctx, &pb.Repo{Id: "app", Url: "https://github.com/acme/app.git"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddProject(ctx, "app", []string{"github.com/acme/app"}, &pb.Stamp{At: now.UnixMilli()}); err != nil {
			t.Fatal(err)
		}
		return srv, &workspaces{Server: srv}, s, tn, strings.TrimPrefix(hs.URL, "http://"), now
	}

	t.Run("waits without holding placing lock and succeeds on upgraded heartbeat", func(t *testing.T) {
		_, wsAPI, s, tn, ctrlAddr, now := setup(t, 5*time.Second)
		nAmd := tailnet.Peer{NodeID: "n1", Hostname: "w-amd", Login: "ramon@", Addr: netip.MustParseAddr("100.64.0.1")}
		nArm := tailnet.Peer{NodeID: "n2", Hostname: "w-arm", Login: "ramon@", Addr: netip.MustParseAddr("100.64.0.2")}
		for _, p := range []tailnet.Peer{nAmd, nArm} {
			if _, err := s.Join(ctx, p, now); err != nil {
				t.Fatal(err)
			}
		}
		// w-amd is still on build 41; w-arm is already on build 42.
		if err := s.Beat(ctx, nAmd.NodeID, &pb.HeartbeatRequest{Resources: &pb.Resources{Arch: "amd64", Cpus: 4, MemoryBytes: 16 << 30, Version: "build 41 (aaaaaaaaaaaa)"}}, now); err != nil {
			t.Fatal(err)
		}
		if err := s.Beat(ctx, nArm.NodeID, &pb.HeartbeatRequest{Resources: &pb.Resources{Arch: "arm64", Cpus: 4, MemoryBytes: 16 << 30, Version: "build 42 (bbbbbbbbbbbb)"}}, now); err != nil {
			t.Fatal(err)
		}
		if _, err := s.StartSession(ctx, "sess-amd", "laptop", "app", "", now); err != nil {
			t.Fatal(err)
		}
		if _, err := s.StartSession(ctx, "sess-arm", "laptop", "app", "", now); err != nil {
			t.Fatal(err)
		}

		type res struct {
			ws  *pb.Workspace
			err error
		}
		amdDone := make(chan res, 1)
		go func() {
			ws, err := wsAPI.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{
				SessionId: "sess-amd", Repo: "app", Placement: &pb.Placement{Arch: "amd64"},
			})
			amdDone <- res{ws, err}
		}()

		// Give the amd64 CreateWorkspace a moment to enter waitUpgrade.
		time.Sleep(40 * time.Millisecond)
		select {
		case r := <-amdDone:
			t.Fatalf("amd64 CreateWorkspace returned early while w-amd was on build 41: ws=%+v err=%v", r.ws, r.err)
		default:
		}

		// A concurrent arm64 CreateWorkspace must NOT block on s.placing while
		// the amd64 caller is waiting for w-amd to upgrade.
		armDone := make(chan res, 1)
		go func() {
			ws, err := wsAPI.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{
				SessionId: "sess-arm", Repo: "app", Placement: &pb.Placement{Arch: "arm64"},
			})
			armDone <- res{ws, err}
		}()
		select {
		case r := <-armDone:
			if r.err != nil || r.ws.Node != "w-arm" {
				t.Fatalf("concurrent arm64 CreateWorkspace = (%+v, %v), want w-arm", r.ws, r.err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent arm64 CreateWorkspace blocked while amd64 caller waited for upgrade")
		}

		// Now w-amd finishes upgrading and sends a real Heartbeat on build 42.
		cl, _, err := api.Controller(ctrlAddr, tn.Client(nAmd))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cl.Heartbeat(ctx, &pb.HeartbeatRequest{
			ClusterId: "cl-1",
			Resources: &pb.Resources{Arch: "amd64", Cpus: 4, MemoryBytes: 16 << 30, Version: "build 42 (bbbbbbbbbbbb)"},
		}); err != nil {
			t.Fatal(err)
		}

		select {
		case r := <-amdDone:
			if r.err != nil || r.ws.Node != "w-amd" {
				t.Fatalf("amd64 CreateWorkspace after upgraded heartbeat = (%+v, %v), want w-amd", r.ws, r.err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("amd64 CreateWorkspace did not wake and succeed after w-amd upgraded to build 42")
		}
	})

	t.Run("returns retryable error when node stays on old build past wait timeout", func(t *testing.T) {
		_, wsAPI, s, _, _, now := setup(t, 120*time.Millisecond)
		n1 := tailnet.Peer{NodeID: "n1", Hostname: "w1", Login: "ramon@", Addr: netip.MustParseAddr("100.64.0.1")}
		if _, err := s.Join(ctx, n1, now); err != nil {
			t.Fatal(err)
		}
		if err := s.Beat(ctx, n1.NodeID, &pb.HeartbeatRequest{Resources: &pb.Resources{Arch: "amd64", Cpus: 4, MemoryBytes: 16 << 30, Version: "build 41 (aaaaaaaaaaaa)"}}, now); err != nil {
			t.Fatal(err)
		}
		if _, err := s.StartSession(ctx, "sess-timeout", "laptop", "app", "", now); err != nil {
			t.Fatal(err)
		}

		start := time.Now()
		_, err := wsAPI.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{SessionId: "sess-timeout", Repo: "app"})
		elapsed := time.Since(start)
		ae := api.FromConnect(err)
		if ae == nil || ae.Code != api.CodeWorkerUnreachable || !ae.Retryable {
			t.Fatalf("CreateWorkspace err = %v, want retryable WORKER_UNREACHABLE", err)
		}
		wantMsg := "no node can take the run: w1 is on build 41, not yet the controller's"
		if ae.Message != wantMsg {
			t.Errorf("message = %q, want %q", ae.Message, wantMsg)
		}
		if ae.Suggestion != "retry once the node finishes upgrading" {
			t.Errorf("suggestion = %q, want %q", ae.Suggestion, "retry once the node finishes upgrading")
		}
		if elapsed < 100*time.Millisecond || elapsed > 2*time.Second {
			t.Errorf("wait elapsed = %v, expected ~120ms", elapsed)
		}
	})

	t.Run("prefers already upgraded node without waiting", func(t *testing.T) {
		_, wsAPI, s, _, _, now := setup(t, 5*time.Second)
		nOld := tailnet.Peer{NodeID: "nOld", Hostname: "old", Login: "ramon@", Addr: netip.MustParseAddr("100.64.0.1")}
		nNew := tailnet.Peer{NodeID: "nNew", Hostname: "new", Login: "ramon@", Addr: netip.MustParseAddr("100.64.0.2")}
		for _, p := range []tailnet.Peer{nOld, nNew} {
			if _, err := s.Join(ctx, p, now); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Beat(ctx, nOld.NodeID, &pb.HeartbeatRequest{Resources: &pb.Resources{Arch: "amd64", Cpus: 4, MemoryBytes: 16 << 30, Version: "build 41 (aaaaaaaaaaaa)"}}, now); err != nil {
			t.Fatal(err)
		}
		if err := s.Beat(ctx, nNew.NodeID, &pb.HeartbeatRequest{Resources: &pb.Resources{Arch: "amd64", Cpus: 4, MemoryBytes: 16 << 30, Version: "build 42 (bbbbbbbbbbbb)"}}, now); err != nil {
			t.Fatal(err)
		}
		if _, err := s.StartSession(ctx, "sess-new", "laptop", "app", "", now); err != nil {
			t.Fatal(err)
		}

		start := time.Now()
		ws, err := wsAPI.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{SessionId: "sess-new", Repo: "app"})
		if err != nil {
			t.Fatalf("CreateWorkspace = %v", err)
		}
		if ws.Node != "new" {
			t.Errorf("placed on %q, want new", ws.Node)
		}
		if time.Since(start) > time.Second {
			t.Errorf("CreateWorkspace waited unexpectedly: %v", time.Since(start))
		}
	})
}
