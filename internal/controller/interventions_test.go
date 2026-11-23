package controller_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2/connecthttp"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/controller"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/httpx"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/tailnet/tailnettest"
)

func TestPushEventsPersistsDiagnosticsAndOutlivesWorkerForget(t *testing.T) {
	s := newStreamed(t)
	ctx := t.Context()

	if _, err := s.cockpit.AddProject(ctx, &pb.AddProjectRequest{Name: "app"}); err != nil {
		t.Fatal(err)
	}
	sess, err := s.sessions.StartSession(ctx, &pb.StartSessionRequest{Project: "app", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}

	pauseIV := &pb.CommandIntervention{
		Seq:       1,
		Kind:      pb.InterventionKind_INTERVENTION_KIND_PAUSE,
		By:        &pb.Stamp{At: 2000, Host: "laptop"},
		Comment:   "checking lock contention",
		Diagnosed: true,
	}
	resumeIV := &pb.CommandIntervention{
		Seq:     2,
		Kind:    pb.InterventionKind_INTERVENTION_KIND_RESUME,
		By:      &pb.Stamp{At: 3000, Host: "laptop"},
		Comment: "all clear",
	}
	stopIV := &pb.CommandIntervention{
		Seq:       3,
		Kind:      pb.InterventionKind_INTERVENTION_KIND_STOP,
		By:        &pb.Stamp{At: 4000, Host: "laptop"},
		Comment:   "deadlocked",
		Diagnosed: true,
	}

	pauseDiag := &pb.CommandDiagnostics{
		CommandId:    "cmd-diag",
		Intervention: pauseIV,
		Sample:       &pb.ProcessSample{CommandId: "cmd-diag", DurationMs: 2000},
		Inspect:      &pb.InspectCommandResponse{CommandId: "cmd-diag", UnitName: "hotbed-cmd-diag"},
	}
	stopDiag := &pb.CommandDiagnostics{
		CommandId:    "cmd-diag",
		Intervention: stopIV,
		Sample:       &pb.ProcessSample{CommandId: "cmd-diag", DurationMs: 2000},
		Inspect:      &pb.InspectCommandResponse{CommandId: "cmd-diag", UnitName: "hotbed-cmd-diag"},
	}

	cmd := func(st pb.CommandState, ivs ...*pb.CommandIntervention) *pb.Command {
		return &pb.Command{
			Id:            "cmd-diag",
			WorkspaceId:   "ws-1",
			SessionId:     sess.Id,
			NodeId:        asahi.NodeID,
			Argv:          "cargo test",
			State:         st,
			Interventions: ivs,
		}
	}

	s.push(&pb.PushEventsRequest{
		Epoch: "ep-1",
		Checkpoint: &pb.Checkpoint{Seq: 0},
	})
	endedAt := int64(4000)
	cancelledCmd := cmd(pb.CommandState_COMMAND_STATE_CANCELLED, pauseIV, resumeIV, stopIV)
	cancelledCmd.EndedAt = &endedAt

	s.push(&pb.PushEventsRequest{
		Epoch: "ep-1",
		Events: []*pb.Event{
			{
				Seq: 1, At: 1000,
				Change: &pb.Event_Command{Command: &pb.CommandChange{
					What: "queued", Command: cmd(pb.CommandState_COMMAND_STATE_QUEUED),
				}},
			},
			{
				Seq: 2, At: 2000,
				Change: &pb.Event_Command{Command: &pb.CommandChange{
					What:        "paused",
					Command:     cmd(pb.CommandState_COMMAND_STATE_PAUSED, pauseIV),
					Diagnostics: pauseDiag,
				}},
			},
			{
				Seq: 3, At: 3000,
				Change: &pb.Event_Command{Command: &pb.CommandChange{
					What:    "resumed",
					Command: cmd(pb.CommandState_COMMAND_STATE_RUNNING, pauseIV, resumeIV),
				}},
			},
			{
				Seq: 4, At: 4000,
				Change: &pb.Event_Command{Command: &pb.CommandChange{
					What:        "cancelled",
					Command:     cancelledCmd,
					Diagnostics: stopDiag,
					Finished: &pb.FinishedCommand{
						Id: "cmd-diag", NodeId: asahi.NodeID, WorkspaceId: "ws-1",
						SessionId: sess.Id, Argv: "cargo test", State: pb.CommandState_COMMAND_STATE_CANCELLED,
						EndedAt: endedAt,
					},
				}},
			},
			{
				Seq: 5, At: 5000,
				Change: &pb.Event_Command{Command: &pb.CommandChange{
					What: "forgotten", Command: cancelledCmd,
				}},
			},
		},
	})

	// Worker has forgotten cmd-diag (removed from node_state), yet controller still serves its diagnostics oldest-first.
	if _, _, ok, err := s.store.ViewCommand(ctx, "cmd-diag"); err != nil || ok {
		t.Fatalf("ViewCommand after forgotten = ok:%v err:%v, want not in node_state", ok, err)
	}
	res, err := s.cockpit.GetCommandDiagnostics(ctx, &pb.GetCommandDiagnosticsRequest{Id: "cmd-diag"})
	if err != nil {
		t.Fatalf("GetCommandDiagnostics: %v", err)
	}
	if len(res.Diagnostics) != 2 {
		t.Fatalf("len(Diagnostics) = %d, want 2", len(res.Diagnostics))
	}
	if res.Diagnostics[0].GetIntervention().GetSeq() != 1 || res.Diagnostics[0].GetIntervention().GetComment() != "checking lock contention" {
		t.Errorf("Diagnostics[0] = %+v", res.Diagnostics[0])
	}
	if res.Diagnostics[1].GetIntervention().GetSeq() != 3 || res.Diagnostics[1].GetIntervention().GetComment() != "deadlocked" {
		t.Errorf("Diagnostics[1] = %+v", res.Diagnostics[1])
	}

	// Session events carry intervention comment, who, and diagnostics hint cleanly (even when worker Detail was empty).
	evs := s.watch(sess.Id, 0)
	if len(evs.Events) != 3 {
		t.Fatalf("len(session events) = %d, want 3 (paused, resumed, cancelled)", len(evs.Events))
	}
	if !strings.Contains(evs.Events[0].Detail, "paused by laptop") || !strings.Contains(evs.Events[0].Detail, "checking lock contention") || !strings.Contains(evs.Events[0].Detail, "hotbed diagnostics cmd-diag") {
		t.Errorf("paused session event detail = %q", evs.Events[0].Detail)
	}
	if !strings.Contains(evs.Events[1].Detail, "resumed by laptop") || !strings.Contains(evs.Events[1].Detail, "all clear") {
		t.Errorf("resumed session event detail = %q", evs.Events[1].Detail)
	}
	if !strings.Contains(evs.Events[2].Detail, "stopped by laptop") || !strings.Contains(evs.Events[2].Detail, "deadlocked") || !strings.Contains(evs.Events[2].Detail, "hotbed diagnostics cmd-diag") {
		t.Errorf("cancelled session event detail = %q", evs.Events[2].Detail)
	}

	// Known command with no diagnostics returns empty list; unknown command returns NotFound.
	s.push(&pb.PushEventsRequest{
		Epoch: "ep-1",
		Events: []*pb.Event{{
			Seq: 6, At: 6000,
			Change: &pb.Event_Command{Command: &pb.CommandChange{
				What: "queued", Command: &pb.Command{Id: "cmd-plain", SessionId: sess.Id, NodeId: asahi.NodeID, State: pb.CommandState_COMMAND_STATE_QUEUED},
			}},
		}},
	})
	empty, err := s.cockpit.GetCommandDiagnostics(ctx, &pb.GetCommandDiagnosticsRequest{Id: "cmd-plain"})
	if err != nil {
		t.Fatalf("GetCommandDiagnostics(cmd-plain): %v", err)
	}
	if len(empty.Diagnostics) != 0 {
		t.Errorf("len(cmd-plain diagnostics) = %d, want 0", len(empty.Diagnostics))
	}
	if _, err := s.cockpit.GetCommandDiagnostics(ctx, &pb.GetCommandDiagnosticsRequest{Id: "no-such-cmd"}); api.CodeOf(err) != api.CodeNotFound {
		t.Errorf("GetCommandDiagnostics(unknown) err = %v, want CodeNotFound", err)
	}

	// Prune removes command_diagnostics older than keepHistory (30 days).
	if err := s.store.Prune(ctx, time.UnixMilli(4000).Add(31*24*time.Hour)); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	afterPrune, err := s.store.CommandDiagnostics(ctx, "cmd-diag")
	if err != nil {
		t.Fatal(err)
	}
	if len(afterPrune) != 0 {
		t.Errorf("after Prune: len(CommandDiagnostics) = %d, want 0", len(afterPrune))
	}
}

type mockInterventionWorker struct {
	hotbedv1connect.UnimplementedWorkspaceServiceHandler
	mu     sync.Mutex
	stops  []*pb.StopCommandRequest
	pauses []*pb.PauseCommandRequest
	resumes []*pb.ResumeCommandRequest
}

func (m *mockInterventionWorker) StopCommand(_ context.Context, req *pb.StopCommandRequest) (*pb.Command, error) {
	m.mu.Lock()
	m.stops = append(m.stops, req)
	m.mu.Unlock()
	return &pb.Command{Id: req.Id, State: pb.CommandState_COMMAND_STATE_CANCELLED}, nil
}

func (m *mockInterventionWorker) PauseCommand(_ context.Context, req *pb.PauseCommandRequest) (*pb.Command, error) {
	m.mu.Lock()
	m.pauses = append(m.pauses, req)
	m.mu.Unlock()
	return &pb.Command{Id: req.Id, State: pb.CommandState_COMMAND_STATE_PAUSED}, nil
}

func (m *mockInterventionWorker) ResumeCommand(_ context.Context, req *pb.ResumeCommandRequest) (*pb.Command, error) {
	m.mu.Lock()
	m.resumes = append(m.resumes, req)
	m.mu.Unlock()
	return &pb.Command{Id: req.Id, State: pb.CommandState_COMMAND_STATE_RUNNING}, nil
}

func TestControllerForwardsStopPauseResumeSessionCheckAndClearsHost(t *testing.T) {
	ctx := t.Context()
	store, err := controller.OpenStore(ctx, filepath.Join(t.TempDir(), "hotbed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	tn := tailnettest.New(ubu1)
	mockW := &mockInterventionWorker{}
	wsSrv := httpx.NewServer()
	hotbedv1connect.RegisterWorkspaceServiceHandler(wsSrv, mockW)
	wMux := http.NewServeMux()
	connecthttp.Mount(wMux, wsSrv)
	wHTTP := httptest.NewServer(httpx.Gate(tn, httpx.Policy{Owner: "ramon@", Self: ubu1.Addrs}, wMux))
	t.Cleanup(wHTTP.Close)
	wAddr := strings.TrimPrefix(wHTTP.URL, "http://")

	ctrl := controller.NewServer(controller.ServerConfig{
		Store:      store,
		Identifier: tn,
		Self:       ubu1.Addrs,
		Cluster:    statedir.ControllerConfig{ClusterID: cluster, Owner: "ramon@", Port: 7120},
		WorkerAddr: func(string) string { return wAddr },
		Workers:    tn.Client(ubu1.Peer),
	})
	hs := httptest.NewServer(ctrl.Handler())
	t.Cleanup(hs.Close)
	ctrlAddr := strings.TrimPrefix(hs.URL, "http://")

	// Join worker and heartbeat so it is online.
	wClient, _, err := api.Controller(ctrlAddr, tn.Client(asahi))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wClient.Join(ctx, &pb.JoinRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := wClient.Heartbeat(ctx, &pb.HeartbeatRequest{ClusterId: cluster, Resources: &pb.Resources{MemoryBytes: 16 << 30}}); err != nil {
		t.Fatal(err)
	}

	conn, err := api.Dial(ctrlAddr, api.DefaultPort, tn.Client(laptop))
	if err != nil {
		t.Fatal(err)
	}
	cl := hotbedv1connect.NewClusterServiceClient(conn)
	ss := hotbedv1connect.NewSessionServiceClient(conn)
	ws := hotbedv1connect.NewWorkspaceServiceClient(conn)

	if _, err := cl.AddProject(ctx, &pb.AddProjectRequest{Name: "myproj"}); err != nil {
		t.Fatal(err)
	}
	sess, err := ss.StartSession(ctx, &pb.StartSessionRequest{Project: "myproj", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddCommand(ctx, "cmd-fwd", sess.Id, asahi.NodeID, "ws-1", time.Now()); err != nil {
		t.Fatal(err)
	}

	// PauseCommand forwarded with Host cleared and Project enriched.
	paused, err := ws.PauseCommand(ctx, &pb.PauseCommandRequest{
		Id: "cmd-fwd", Comment: "inspecting", Diagnose: true, SessionId: sess.Id, Host: "spoofed",
	})
	if err != nil {
		t.Fatalf("PauseCommand: %v", err)
	}
	if paused.State != pb.CommandState_COMMAND_STATE_PAUSED || paused.Project != "myproj" {
		t.Fatalf("PauseCommand returned %+v", paused)
	}

	// ResumeCommand forwarded with Host cleared.
	resumed, err := ws.ResumeCommand(ctx, &pb.ResumeCommandRequest{
		Id: "cmd-fwd", Comment: "go", SessionId: sess.Id, Host: "spoofed",
	})
	if err != nil {
		t.Fatalf("ResumeCommand: %v", err)
	}
	if resumed.State != pb.CommandState_COMMAND_STATE_RUNNING || resumed.Project != "myproj" {
		t.Fatalf("ResumeCommand returned %+v", resumed)
	}

	// StopCommand forwarded with Host, Internal, and Reason cleared.
	stopped, err := ws.StopCommand(ctx, &pb.StopCommandRequest{
		Id: "cmd-fwd", Comment: "done", Diagnose: true, SessionId: sess.Id, Host: "spoofed",
		Internal: true, Reason: "spoofed-internal",
	})
	if err != nil {
		t.Fatalf("StopCommand: %v", err)
	}
	if stopped.State != pb.CommandState_COMMAND_STATE_CANCELLED || stopped.Project != "myproj" {
		t.Fatalf("StopCommand returned %+v", stopped)
	}

	// Sessionless (cockpit) calls with empty SessionId also succeed with Host cleared.
	if _, err := ws.PauseCommand(ctx, &pb.PauseCommandRequest{Id: "cmd-fwd", Comment: "cockpit pause", Host: "spoofed"}); err != nil {
		t.Fatalf("sessionless PauseCommand: %v", err)
	}
	if _, err := ws.ResumeCommand(ctx, &pb.ResumeCommandRequest{Id: "cmd-fwd", Comment: "cockpit resume", Host: "spoofed"}); err != nil {
		t.Fatalf("sessionless ResumeCommand: %v", err)
	}
	if _, err := ws.StopCommand(ctx, &pb.StopCommandRequest{Id: "cmd-fwd", Comment: "cockpit stop", Host: "spoofed"}); err != nil {
		t.Fatalf("sessionless StopCommand: %v", err)
	}

	// Mismatched SessionId is rejected at the controller boundary before forwarding to the worker.
	otherSess, err := ss.StartSession(ctx, &pb.StartSessionRequest{Project: "myproj", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.PauseCommand(ctx, &pb.PauseCommandRequest{Id: "cmd-fwd", SessionId: otherSess.Id}); api.CodeOf(err) != api.CodeNotFound {
		t.Errorf("mismatched PauseCommand err = %v, want CodeNotFound", err)
	}
	if _, err := ws.ResumeCommand(ctx, &pb.ResumeCommandRequest{Id: "cmd-fwd", SessionId: otherSess.Id}); api.CodeOf(err) != api.CodeNotFound {
		t.Errorf("mismatched ResumeCommand err = %v, want CodeNotFound", err)
	}
	if _, err := ws.StopCommand(ctx, &pb.StopCommandRequest{Id: "cmd-fwd", SessionId: otherSess.Id}); api.CodeOf(err) != api.CodeNotFound {
		t.Errorf("mismatched StopCommand err = %v, want CodeNotFound", err)
	}

	mockW.mu.Lock()
	defer mockW.mu.Unlock()
	if len(mockW.pauses) != 2 || mockW.pauses[0].Host != "" || mockW.pauses[0].Comment != "inspecting" || !mockW.pauses[0].Diagnose || mockW.pauses[1].Host != "" {
		t.Errorf("forwarded PauseCommand = %+v", mockW.pauses)
	}
	if len(mockW.resumes) != 2 || mockW.resumes[0].Host != "" || mockW.resumes[0].Comment != "go" || mockW.resumes[1].Host != "" {
		t.Errorf("forwarded ResumeCommand = %+v", mockW.resumes)
	}
	if len(mockW.stops) != 2 || mockW.stops[0].Host != "" || mockW.stops[0].Comment != "done" || !mockW.stops[0].Diagnose || mockW.stops[0].Internal || mockW.stops[0].Reason != "" || mockW.stops[1].Host != "" {
		t.Errorf("forwarded StopCommand = %+v", mockW.stops)
	}
}

func TestSaveCommandDiagnostics(t *testing.T) {
	s := newStreamed(t)
	ctx := t.Context()

	diag := &pb.CommandDiagnostics{
		CommandId: "cmd-save",
		Intervention: &pb.CommandIntervention{
			Seq:       1,
			Kind:      pb.InterventionKind_INTERVENTION_KIND_PAUSE,
			By:        &pb.Stamp{At: 2000, Host: "laptop"},
			Comment:   "first save",
			Diagnosed: true,
		},
		Sample: &pb.ProcessSample{CommandId: "cmd-save", DurationMs: 500},
	}

	// Joined worker persists diagnostics.
	if _, err := s.worker.SaveCommandDiagnostics(ctx, &pb.SaveCommandDiagnosticsRequest{Diagnostics: diag}); err != nil {
		t.Fatalf("SaveCommandDiagnostics: %v", err)
	}

	// Retrying with the same (command_id, seq) is idempotent.
	updated := proto.Clone(diag).(*pb.CommandDiagnostics)
	updated.Intervention.Comment = "retried save"
	if _, err := s.worker.SaveCommandDiagnostics(ctx, &pb.SaveCommandDiagnosticsRequest{Diagnostics: updated}); err != nil {
		t.Fatalf("SaveCommandDiagnostics retry: %v", err)
	}

	got, err := s.cockpit.GetCommandDiagnostics(ctx, &pb.GetCommandDiagnosticsRequest{Id: "cmd-save"})
	if err != nil {
		t.Fatalf("GetCommandDiagnostics: %v", err)
	}
	if len(got.Diagnostics) != 1 || got.Diagnostics[0].GetIntervention().GetComment() != "retried save" {
		t.Fatalf("GetCommandDiagnostics = %+v, want 1 updated report", got.Diagnostics)
	}

	// Unjoined peer (laptop) is rejected with CodeNotJoined.
	if _, err := s.cockpit.SaveCommandDiagnostics(ctx, &pb.SaveCommandDiagnosticsRequest{Diagnostics: diag}); api.CodeOf(err) != api.CodeNotJoined {
		t.Errorf("unjoined SaveCommandDiagnostics err = %v, want CodeNotJoined", err)
	}

	// Invalid requests from joined worker are rejected with CodeInvalidArgs.
	badReqs := []*pb.SaveCommandDiagnosticsRequest{
		{},
		{Diagnostics: &pb.CommandDiagnostics{Intervention: &pb.CommandIntervention{Seq: 1}}},
		{Diagnostics: &pb.CommandDiagnostics{CommandId: "   ", Intervention: &pb.CommandIntervention{Seq: 1}}},
		{Diagnostics: &pb.CommandDiagnostics{CommandId: "cmd-save"}},
		{Diagnostics: &pb.CommandDiagnostics{CommandId: "cmd-save", Intervention: &pb.CommandIntervention{Seq: 0}}},
		{Diagnostics: &pb.CommandDiagnostics{CommandId: "cmd-save", Intervention: &pb.CommandIntervention{Seq: -1}}},
	}
	for i, req := range badReqs {
		if _, err := s.worker.SaveCommandDiagnostics(ctx, req); api.CodeOf(err) != api.CodeInvalidArgs {
			t.Errorf("badReqs[%d] err = %v, want CodeInvalidArgs", i, err)
		}
	}
}

func TestGetCommandDiagnosticsPaginationAndSeqLookup(t *testing.T) {
	s := newStreamed(t)
	ctx := t.Context()

	// Persist 3 large diagnostics (~1.2 MiB each) so each page fits 1 report
	// under the 2 MiB budget without approaching Connect's 4 MiB limit.
	blob := strings.Repeat("x", 1200*1024)
	for seq := int32(1); seq <= 3; seq++ {
		d := &pb.CommandDiagnostics{
			CommandId: "cmd-big",
			Intervention: &pb.CommandIntervention{
				Seq:       seq,
				Kind:      pb.InterventionKind_INTERVENTION_KIND_PAUSE,
				By:        &pb.Stamp{At: int64(seq) * 1000, Host: "laptop"},
				Comment:   "report",
				Diagnosed: true,
			},
			Inspect: &pb.InspectCommandResponse{
				CommandId: "cmd-big",
				Cgroup:    blob,
			},
		}
		if _, err := s.worker.SaveCommandDiagnostics(ctx, &pb.SaveCommandDiagnosticsRequest{Diagnostics: d}); err != nil {
			t.Fatalf("SaveCommandDiagnostics seq %d: %v", seq, err)
		}
	}

	// Paginate across all pages using AfterSeq/NextAfterSeq.
	var gotSeqs []int32
	var afterSeq int32
	pages := 0
	for {
		res, err := s.cockpit.GetCommandDiagnostics(ctx, &pb.GetCommandDiagnosticsRequest{
			Id:       "cmd-big",
			AfterSeq: afterSeq,
		})
		if err != nil {
			t.Fatalf("GetCommandDiagnostics(afterSeq=%d): %v", afterSeq, err)
		}
		pages++
		if proto.Size(res) > 2<<20 {
			t.Fatalf("page %d encoded size = %d, exceeds 2 MiB cap", pages, proto.Size(res))
		}
		for _, d := range res.Diagnostics {
			gotSeqs = append(gotSeqs, d.GetIntervention().GetSeq())
		}
		if !res.HasMore {
			if res.NextAfterSeq != 0 {
				t.Errorf("final page NextAfterSeq = %d, want 0", res.NextAfterSeq)
			}
			break
		}
		if res.NextAfterSeq <= afterSeq {
			t.Fatalf("NextAfterSeq = %d did not advance past %d", res.NextAfterSeq, afterSeq)
		}
		afterSeq = res.NextAfterSeq
	}
	if pages != 3 {
		t.Errorf("pages = %d, want 3", pages)
	}
	if len(gotSeqs) != 3 || gotSeqs[0] != 1 || gotSeqs[1] != 2 || gotSeqs[2] != 3 {
		t.Errorf("paginated seqs = %v, want [1 2 3]", gotSeqs)
	}

	// Single-item Seq lookup returns only seq 2.
	only2, err := s.cockpit.GetCommandDiagnostics(ctx, &pb.GetCommandDiagnosticsRequest{Id: "cmd-big", Seq: 2})
	if err != nil {
		t.Fatalf("GetCommandDiagnostics(seq=2): %v", err)
	}
	if len(only2.Diagnostics) != 1 || only2.Diagnostics[0].GetIntervention().GetSeq() != 2 || only2.HasMore || only2.NextAfterSeq != 0 {
		t.Fatalf("seq=2 response = len:%d hasMore:%v next:%d", len(only2.Diagnostics), only2.HasMore, only2.NextAfterSeq)
	}

	// Missing seq for a command that has diagnostics returns empty list, not NotFound.
	missingSeq, err := s.cockpit.GetCommandDiagnostics(ctx, &pb.GetCommandDiagnosticsRequest{Id: "cmd-big", Seq: 99})
	if err != nil {
		t.Fatalf("GetCommandDiagnostics(seq=99): %v", err)
	}
	if len(missingSeq.Diagnostics) != 0 || missingSeq.HasMore {
		t.Errorf("seq=99 response = %+v, want empty", missingSeq)
	}

	// Unknown command with Seq still returns CodeNotFound.
	if _, err := s.cockpit.GetCommandDiagnostics(ctx, &pb.GetCommandDiagnosticsRequest{Id: "no-such-cmd", Seq: 1}); api.CodeOf(err) != api.CodeNotFound {
		t.Errorf("unknown seq=1 err = %v, want CodeNotFound", err)
	}
	// Negative Seq / AfterSeq return CodeInvalidArgs.
	if _, err := s.cockpit.GetCommandDiagnostics(ctx, &pb.GetCommandDiagnosticsRequest{Id: "cmd-big", Seq: -1}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Errorf("negative seq err = %v, want CodeInvalidArgs", err)
	}
	if _, err := s.cockpit.GetCommandDiagnostics(ctx, &pb.GetCommandDiagnosticsRequest{Id: "cmd-big", AfterSeq: -1}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Errorf("negative after_seq err = %v, want CodeInvalidArgs", err)
	}
}

func TestGetCommandFromHistoryPreservesInterventions(t *testing.T) {
	s := newStreamed(t)
	ctx := t.Context()

	if _, err := s.cockpit.AddProject(ctx, &pb.AddProjectRequest{Name: "app"}); err != nil {
		t.Fatal(err)
	}
	sess, err := s.sessions.StartSession(ctx, &pb.StartSessionRequest{Project: "app", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}

	pauseIV := &pb.CommandIntervention{
		Seq:     1,
		Kind:    pb.InterventionKind_INTERVENTION_KIND_PAUSE,
		By:      &pb.Stamp{At: 2000, Host: "laptop"},
		Comment: "pausing",
	}
	stopIV := &pb.CommandIntervention{
		Seq:     2,
		Kind:    pb.InterventionKind_INTERVENTION_KIND_STOP,
		By:      &pb.Stamp{At: 4000, Host: "laptop"},
		Comment: "stopped by hand",
	}
	endedAt := int64(4000)
	cancelledCmd := &pb.Command{
		Id:            "cmd-hist-iv",
		WorkspaceId:   "ws-1",
		SessionId:     sess.Id,
		NodeId:        asahi.NodeID,
		Argv:          "cargo test",
		State:         pb.CommandState_COMMAND_STATE_CANCELLED,
		EndedAt:       &endedAt,
		Interventions: []*pb.CommandIntervention{pauseIV, stopIV},
	}

	s.push(&pb.PushEventsRequest{Epoch: "ep-1", Checkpoint: &pb.Checkpoint{Seq: 0}})
	s.push(&pb.PushEventsRequest{
		Epoch: "ep-1",
		Events: []*pb.Event{
			{
				Seq: 1, At: 4000,
				Change: &pb.Event_Command{Command: &pb.CommandChange{
					What:    "cancelled",
					Command: cancelledCmd,
					Finished: &pb.FinishedCommand{
						Id:            "cmd-hist-iv",
						NodeId:        asahi.NodeID,
						WorkspaceId:   "ws-1",
						SessionId:     sess.Id,
						Argv:          "cargo test",
						State:         pb.CommandState_COMMAND_STATE_CANCELLED,
						EndedAt:       endedAt,
						Interventions: []*pb.CommandIntervention{pauseIV, stopIV},
					},
				}},
			},
			{
				Seq: 2, At: 5000,
				Change: &pb.Event_Command{Command: &pb.CommandChange{
					What:    "forgotten",
					Command: cancelledCmd,
				}},
			},
		},
	})

	// Confirm the command is gone from node_state so GetCommand must serve from history.
	if _, _, ok, err := s.store.ViewCommand(ctx, "cmd-hist-iv"); err != nil || ok {
		t.Fatalf("ViewCommand after forgotten = ok:%v err:%v, want not in node_state", ok, err)
	}

	got, err := s.workspaces.GetCommand(ctx, &pb.GetCommandRequest{Id: "cmd-hist-iv"})
	if err != nil {
		t.Fatalf("GetCommand from history: %v", err)
	}
	if len(got.Interventions) != 2 {
		t.Fatalf("len(got.Interventions) = %d, want 2", len(got.Interventions))
	}
	if got.Interventions[0].GetSeq() != 1 || got.Interventions[1].GetSeq() != 2 || got.Interventions[1].GetComment() != "stopped by hand" {
		t.Errorf("got.Interventions = %+v", got.Interventions)
	}
	if !api.StoppedByHand(got) {
		t.Errorf("api.StoppedByHand(got) = false, want true (state=%v interventions=%+v)", got.State, got.Interventions)
	}
}
