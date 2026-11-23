package worker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/httpx"
	"github.com/semistrict/hotbed/internal/inspect"
	"github.com/semistrict/hotbed/internal/tailnet"
	"github.com/semistrict/hotbed/internal/tailnet/tailnettest"
)

type fakeInspector struct {
	sample     *pb.ProcessSample
	sampleErr  error
	inspectRes *pb.InspectCommandResponse
	inspectErr error
	sampleReq  *pb.SampleCommandRequest
	inspectReq *pb.InspectCommandRequest
}

func (f *fakeInspector) Inspect(_ context.Context, _ inspect.Target, req *pb.InspectCommandRequest) (*pb.InspectCommandResponse, error) {
	f.inspectReq = req
	if f.inspectErr != nil {
		return nil, f.inspectErr
	}
	return f.inspectRes, nil
}

func (f *fakeInspector) Trace(context.Context, inspect.Target, *pb.TraceCommandRequest, inspect.Emit) error {
	return nil
}

func (f *fakeInspector) Sample(_ context.Context, _ inspect.Target, req *pb.SampleCommandRequest, emit inspect.SampleEmit) error {
	f.sampleReq = req
	if f.sampleErr != nil {
		return f.sampleErr
	}
	if f.sample != nil {
		return emit(&pb.SampleCommandChunk{Sample: f.sample})
	}
	return nil
}

func (f *fakeInspector) Capabilities() []*pb.ToolCapability { return nil }

func newInterventionEngine(t *testing.T, dir string, store *fake.Store, u Units, insp inspect.Inspector, now *time.Time, mutateCfg ...func(*EngineConfig)) *Engine {
	t.Helper()
	cfg := EngineConfig{
		Dir:       dir,
		NodeID:    "n1",
		Host:      "asahimbp",
		Store:     store,
		BaseImage: fake.BaseImage(store),
		Units:     u,
		System:    fake.NewSystem(),
		Inspector: insp,
		Now:       func() time.Time { return *now },
	}
	for _, fn := range mutateCfg {
		fn(&cfg)
	}
	e, err := NewEngine(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func startTestCommand(t *testing.T, e *Engine, store *fake.Store, units *fake.Units, id string, reqMut ...func(*pb.RunCommandRequest)) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(store.Path("v"), "work", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	if _, ok := e.workspaces["w"]; !ok {
		e.workspaces["w"] = &workspace{rec: &pb.WorkspaceRecord{
			Workspace: &pb.Workspace{Id: "w", Project: "app", SessionId: "sess-1", Repos: []*pb.WorkspaceRepo{{Repo: "app"}}, State: idle},
			Volume:    "v",
		}}
	}
	e.mu.Unlock()
	req := &pb.RunCommandRequest{
		Id:          id,
		WorkspaceId: "w",
		Args:        []string{"sleep", "300"},
		Attached:    true,
		Limits:      &pb.CommandLimits{SoftLimitSeconds: 600, HardLimitSeconds: 7200},
	}
	for _, fn := range reqMut {
		fn(req)
	}
	if _, err := e.Exec(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	e.admit(t.Context())
	t.Cleanup(func() { _ = units.Stop(context.WithoutCancel(t.Context()), "hotbed-"+id) })
}

func TestStopWithCommentAndDiagnose(t *testing.T) {
	now := time.Unix(1795300000, 0)
	units, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}
	insp := &fakeInspector{
		sample:     &pb.ProcessSample{CommandId: "c-stop", DurationMs: 2000},
		inspectRes: &pb.InspectCommandResponse{CommandId: "c-stop", UnitName: "hotbed-c-stop"},
	}
	e := newInterventionEngine(t, t.TempDir(), store, units, insp, &now)
	startTestCommand(t, e, store, units, "c-stop")

	got, err := e.StopWith(t.Context(), "c-stop", Intervention{
		Comment:  "hung waiting on mutex",
		Diagnose: true,
		Host:     "laptop",
	})
	if err != nil {
		t.Fatalf("StopWith: %v", err)
	}
	if got.State != cancelled {
		t.Fatalf("state = %v, want cancelled", got.State)
	}
	if !api.StoppedByHand(got) {
		t.Fatalf("StoppedByHand = false, interventions = %+v", got.Interventions)
	}
	if len(got.Interventions) != 1 {
		t.Fatalf("len(Interventions) = %d, want 1", len(got.Interventions))
	}
	iv := got.Interventions[0]
	if iv.Seq != 1 || iv.Kind != pb.InterventionKind_INTERVENTION_KIND_STOP || iv.Comment != "hung waiting on mutex" || iv.By.GetHost() != "laptop" || !iv.Diagnosed || iv.DiagnosticsError != "" {
		t.Fatalf("unexpected intervention: %+v", iv)
	}
	for _, want := range []string{"stopped by laptop", "hung waiting on mutex", "hotbed diagnostics c-stop"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("Reason = %q, missing %q", got.Reason, want)
		}
	}
	if insp.sampleReq == nil || insp.sampleReq.DurationSeconds != diagnoseSampleSeconds {
		t.Errorf("sampleReq = %+v, want DurationSeconds=%d", insp.sampleReq, diagnoseSampleSeconds)
	}

	// Verify diagnostics are persisted in worker.db (pending_diag), NOT inline
	// on CommandChange, and FinishedCommand carries cloned Interventions.
	evs, err := e.Events(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var fin *pb.FinishedCommand
	for _, ev := range evs {
		if ch := ev.GetCommand(); ch != nil {
			if ch.Diagnostics != nil {
				t.Fatalf("unexpected inline CommandChange.Diagnostics on event %d; diagnostics must deliver via SaveCommandDiagnostics", ev.Seq)
			}
			if ch.Finished != nil {
				fin = ch.Finished
			}
		}
	}
	if fin == nil || len(fin.Interventions) != 1 || fin.Interventions[0].Seq != 1 || fin.Interventions[0].Comment != "hung waiting on mutex" {
		t.Fatalf("FinishedCommand.Interventions = %+v, want 1 cloned intervention", fin)
	}
	pending, err := e.PendingDiagnostics()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("len(PendingDiagnostics) = %d, want 1", len(pending))
	}
	diag := pending[0]
	if diag.CommandId != "c-stop" || diag.GetIntervention().GetSeq() != 1 || diag.Sample == nil || diag.Inspect == nil {
		t.Fatalf("unexpected pending diagnostics payload: %+v", diag)
	}

	// Calling StopWith on an already-ended command returns it as it ended without adding a second intervention.
	again, err := e.StopWith(t.Context(), "c-stop", Intervention{Comment: "second", Host: "laptop"})
	if err != nil {
		t.Fatalf("second StopWith: %v", err)
	}
	if len(again.Interventions) != 1 {
		t.Errorf("second StopWith changed interventions: %+v", again.Interventions)
	}
}

func TestStopWithDiagnosticsErrorsDoNotBlockStop(t *testing.T) {
	t.Run("inspector fails completely", func(t *testing.T) {
		now := time.Unix(1795300000, 0)
		units, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}
		insp := &fakeInspector{
			sampleErr:  errors.New("perf unavailable"),
			inspectErr: errors.New("procfs denied"),
		}
		e := newInterventionEngine(t, t.TempDir(), store, units, insp, &now)
		startTestCommand(t, e, store, units, "c-err")

		got, err := e.StopWith(t.Context(), "c-err", Intervention{Comment: "kill it", Diagnose: true, Host: "laptop"})
		if err != nil {
			t.Fatalf("StopWith: %v", err)
		}
		if got.State != cancelled {
			t.Fatalf("state = %v, want cancelled", got.State)
		}
		iv := got.Interventions[0]
		if iv.Diagnosed {
			t.Errorf("Diagnosed = true, want false")
		}
		if !strings.Contains(iv.DiagnosticsError, "perf unavailable") || !strings.Contains(iv.DiagnosticsError, "procfs denied") {
			t.Errorf("DiagnosticsError = %q, want both errors", iv.DiagnosticsError)
		}
	})

	t.Run("partial diagnostics keeps what succeeded", func(t *testing.T) {
		now := time.Unix(1795300000, 0)
		units, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}
		insp := &fakeInspector{
			sampleErr:  errors.New("sample timed out"),
			inspectRes: &pb.InspectCommandResponse{CommandId: "c-part"},
		}
		e := newInterventionEngine(t, t.TempDir(), store, units, insp, &now)
		startTestCommand(t, e, store, units, "c-part")

		got, err := e.StopWith(t.Context(), "c-part", Intervention{Diagnose: true, Host: "laptop"})
		if err != nil {
			t.Fatalf("StopWith: %v", err)
		}
		iv := got.Interventions[0]
		if !iv.Diagnosed {
			t.Errorf("Diagnosed = false, want true when inspect succeeded")
		}
		if !strings.Contains(iv.DiagnosticsError, "sample timed out") {
			t.Errorf("DiagnosticsError = %q, want sample error", iv.DiagnosticsError)
		}
	})
}

func TestPauseWithCommentDiagnoseAndWaitOnDoesNotResume(t *testing.T) {
	now := time.Unix(1795300000, 0)
	units, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}
	insp := &fakeInspector{
		sample:     &pb.ProcessSample{CommandId: "c-pause", DurationMs: 2000},
		inspectRes: &pb.InspectCommandResponse{CommandId: "c-pause", UnitName: "hotbed-c-pause"},
	}
	e := newInterventionEngine(t, t.TempDir(), store, units, insp, &now)
	startTestCommand(t, e, store, units, "c-pause")

	got, err := e.Pause(t.Context(), "c-pause", Intervention{
		Comment:  "checking disk usage",
		Diagnose: true,
		Host:     "laptop",
	})
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if got.State != paused || !e.frozen(t, "hotbed-c-pause") {
		t.Fatalf("state=%v frozen=%v, want paused and frozen", got.State, e.frozen(t, "hotbed-c-pause"))
	}
	if !api.PausedByHand(got) {
		t.Fatalf("PausedByHand = false")
	}
	if len(got.Interventions) != 1 || got.Interventions[0].Seq != 1 || !got.Interventions[0].Diagnosed {
		t.Fatalf("interventions = %+v", got.Interventions)
	}

	// CRITICAL AC: WaitOn (from FollowLog wait=true) must NOT resume a hand-paused command.
	if err := e.WaitOn(t.Context(), "c-pause"); err != nil {
		t.Fatalf("WaitOn: %v", err)
	}
	if s := e.state("c-pause"); s != paused || !e.frozen(t, "hotbed-c-pause") {
		t.Fatalf("WaitOn undid hand pause: state=%v frozen=%v", s, e.frozen(t, "hotbed-c-pause"))
	}
	if going, _ := e.Going("c-pause"); going {
		t.Fatal("Going(c-pause) = true while paused, want false")
	}

	// Pausing again while already hand-paused is rejected with Conflict.
	if _, err := e.Pause(t.Context(), "c-pause", Intervention{Host: "laptop"}); api.CodeOf(err) != api.CodeConflict {
		t.Errorf("second Pause err = %v, want CodeConflict", err)
	}

	// Verify diagnostics are persisted in worker.db (pending_diag) and not inline on events.
	evs, err := e.Events(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if ch := ev.GetCommand(); ch != nil && ch.Diagnostics != nil {
			t.Fatalf("unexpected inline CommandChange.Diagnostics on event %d", ev.Seq)
		}
	}
	pending, err := e.PendingDiagnostics()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].CommandId != "c-pause" || pending[0].GetIntervention().GetSeq() != 1 {
		t.Fatalf("PendingDiagnostics = %+v, want 1 entry for c-pause seq 1", pending)
	}
}

func TestPauseRejectsQueuedAndEnded(t *testing.T) {
	now := time.Unix(1795300000, 0)
	units, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}
	e := newInterventionEngine(t, t.TempDir(), store, units, nil, &now)
	if err := os.MkdirAll(filepath.Join(store.Path("v"), "work", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.workspaces["w"] = &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{Id: "w", Project: "app", Repos: []*pb.WorkspaceRepo{{Repo: "app"}}, State: idle},
		Volume:    "v",
	}}
	// Queued (not admitted yet).
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{Id: "c-q", WorkspaceId: "w", Args: []string{"sleep", "10"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Pause(t.Context(), "c-q", Intervention{Host: "laptop"}); api.CodeOf(err) != api.CodeConflict {
		t.Errorf("Pause(queued) err = %v, want CodeConflict", err)
	}
	if _, err := e.Resume(t.Context(), "c-q", Intervention{Host: "laptop"}); api.CodeOf(err) != api.CodeConflict {
		t.Errorf("Resume(queued) err = %v, want CodeConflict", err)
	}
	// Stop it, then verify Pause and Resume both return Conflict on ended command.
	if _, err := e.Stop(t.Context(), "c-q", "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Pause(t.Context(), "c-q", Intervention{Host: "laptop"}); api.CodeOf(err) != api.CodeConflict {
		t.Errorf("Pause(ended) err = %v, want CodeConflict", err)
	}
	if _, err := e.Resume(t.Context(), "c-q", Intervention{Host: "laptop"}); api.CodeOf(err) != api.CodeConflict {
		t.Errorf("Resume(ended) err = %v, want CodeConflict", err)
	}
}

func TestResumeThawsHandPausedAndSoftPausedAndResetsSoftLimit(t *testing.T) {
	t.Run("hand-paused resumed and soft limit reset", func(t *testing.T) {
		now := time.Unix(1795300000, 0)
		units, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}
		e := newInterventionEngine(t, t.TempDir(), store, units, nil, &now)
		startTestCommand(t, e, store, units, "c-res")

		if _, err := e.Pause(t.Context(), "c-res", Intervention{Comment: "hold", Host: "laptop"}); err != nil {
			t.Fatal(err)
		}
		// Advance past the 10m soft limit while paused.
		now = now.Add(15 * time.Minute)
		got, err := e.Resume(t.Context(), "c-res", Intervention{Comment: "continue", Session: "s1", Host: "mac"})
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if got.State != running || e.frozen(t, "hotbed-c-res") || got.Attached {
			t.Fatalf("after Resume: state=%v frozen=%v attached=%v", got.State, e.frozen(t, "hotbed-c-res"), got.Attached)
		}
		if api.PausedByHand(got) {
			t.Fatal("PausedByHand = true after Resume")
		}
		if len(got.Interventions) != 2 || got.Interventions[1].Seq != 2 || got.Interventions[1].Kind != pb.InterventionKind_INTERVENTION_KIND_RESUME {
			t.Fatalf("Interventions = %+v", got.Interventions)
		}

		// 9 minutes after Resume: still running (soft limit reset from Resume time).
		now = now.Add(9 * time.Minute)
		e.limit(t.Context())
		if s := e.state("c-res"); s != running {
			t.Fatalf("9m after Resume: state = %v, want running", s)
		}
		// 1 more minute -> soft limit pauses it, and WaitOn DOES resume a soft-paused command.
		now = now.Add(time.Minute)
		e.limit(t.Context())
		cmd, _ := e.Command("c-res")
		if cmd.State != paused || api.PausedByHand(cmd) {
			t.Fatalf("10m after Resume: state=%v pausedByHand=%v, want soft-paused", cmd.State, api.PausedByHand(cmd))
		}
		if err := e.WaitOn(t.Context(), "c-res"); err != nil {
			t.Fatalf("WaitOn after soft pause: %v", err)
		}
		if s := e.state("c-res"); s != running {
			t.Fatalf("after WaitOn on soft-paused command: state = %v, want running", s)
		}
	})

	t.Run("soft-paused resumed by Resume", func(t *testing.T) {
		now := time.Unix(1795300000, 0)
		units, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}
		e := newInterventionEngine(t, t.TempDir(), store, units, nil, &now)
		startTestCommand(t, e, store, units, "c-soft")

		now = now.Add(11 * time.Minute)
		e.limit(t.Context())
		if s := e.state("c-soft"); s != paused {
			t.Fatalf("state = %v, want paused", s)
		}
		got, err := e.Resume(t.Context(), "c-soft", Intervention{Host: "laptop"})
		if err != nil {
			t.Fatalf("Resume(soft-paused): %v", err)
		}
		if got.State != running || e.frozen(t, "hotbed-c-soft") {
			t.Fatalf("after Resume: state=%v frozen=%v", got.State, e.frozen(t, "hotbed-c-soft"))
		}
		if len(got.Interventions) != 1 || got.Interventions[0].Kind != pb.InterventionKind_INTERVENTION_KIND_RESUME {
			t.Fatalf("Interventions = %+v", got.Interventions)
		}
	})

	t.Run("thaw failure emits no resumed event or timeline entry and keeps hand-pause", func(t *testing.T) {
		now := time.Unix(1795300000, 0)
		units, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}
		e := newInterventionEngine(t, t.TempDir(), store, thawFails{units}, nil, &now)
		startTestCommand(t, e, store, units, "c-failthaw")

		if _, err := e.Pause(t.Context(), "c-failthaw", Intervention{Comment: "hold", Host: "laptop"}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Resume(t.Context(), "c-failthaw", Intervention{Host: "laptop"}); err == nil {
			t.Fatal("expected Resume to fail when Thaw fails")
		}
		cmd, _ := e.Command("c-failthaw")
		if cmd.State != paused || !api.PausedByHand(cmd) || len(cmd.Interventions) != 1 {
			t.Fatalf("after failed Resume thaw: state=%v pausedByHand=%v interventions=%+v", cmd.State, api.PausedByHand(cmd), cmd.Interventions)
		}
		for _, te := range cmd.Timeline {
			if te.What == "resumed" {
				t.Fatalf("unexpected resumed timeline entry after failed Thaw: %+v", cmd.Timeline)
			}
		}
		evs, err := e.Events(0, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range evs {
			if ch := ev.GetCommand(); ch != nil && ch.What == "resumed" {
				t.Fatalf("unexpected resumed event emitted after failed Thaw: %+v", ev)
			}
		}
	})
}

func TestHandPauseSurvivesWorkerRestart(t *testing.T) {
	now := time.Unix(1795300000, 0)
	dir := t.TempDir()
	units, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}

	e1 := newInterventionEngine(t, dir, store, units, nil, &now)
	startTestCommand(t, e1, store, units, "c-restart")
	if _, err := e1.Pause(t.Context(), "c-restart", Intervention{Comment: "stay paused across restart", Host: "laptop"}); err != nil {
		t.Fatal(err)
	}
	if err := e1.Close(); err != nil {
		t.Fatal(err)
	}

	// Re-open engine on the same worker.db directory.
	e2 := newInterventionEngine(t, dir, store, units, nil, &now)
	cmd, ok := e2.Command("c-restart")
	if !ok {
		t.Fatal("command missing after restart")
	}
	if cmd.State != paused || !api.PausedByHand(cmd) {
		t.Fatalf("after restart: state=%v pausedByHand=%v interventions=%+v", cmd.State, api.PausedByHand(cmd), cmd.Interventions)
	}
	if err := e2.WaitOn(t.Context(), "c-restart"); err != nil {
		t.Fatalf("WaitOn after restart: %v", err)
	}
	if s := e2.state("c-restart"); s != paused || !e2.frozen(t, "hotbed-c-restart") {
		t.Fatalf("WaitOn after restart undid hand pause: state=%v frozen=%v", s, e2.frozen(t, "hotbed-c-restart"))
	}
	resumed, err := e2.Resume(t.Context(), "c-restart", Intervention{Host: "laptop"})
	if err != nil {
		t.Fatalf("Resume after restart: %v", err)
	}
	if resumed.State != running || e2.frozen(t, "hotbed-c-restart") {
		t.Fatalf("after Resume on restarted engine: state=%v frozen=%v", resumed.State, e2.frozen(t, "hotbed-c-restart"))
	}
}

func TestServiceInterventionHostIdentityAndValidation(t *testing.T) {
	now := time.Unix(1795300000, 0)
	units, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}
	e := newInterventionEngine(t, t.TempDir(), store, units, nil, &now, func(cfg *EngineConfig) {
		cfg.RequireController = true
		cfg.ControllerNodeID = "nCtrl"
	})
	startTestCommand(t, e, store, units, "c-auth")
	ctrl := tailnet.Peer{NodeID: "nCtrl", Hostname: "ubu1", Login: "ramon@"}
	agent := tailnet.Peer{NodeID: "nAgent", Hostname: "agentbox", Login: "ramon@"}
	self := tailnet.Self{Peer: tailnet.Peer{NodeID: "n1", Hostname: "asahimbp", Login: "ramon@"}, Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
	tn := tailnettest.New(self)
	srv := httptest.NewServer(Handler(tn, httpx.Policy{Owner: "ramon@", Self: self.Addrs}, e))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")

	ctrlConn, err := api.Dial(addr, api.WorkerPort, tn.Client(ctrl))
	if err != nil {
		t.Fatal(err)
	}
	ctrlWS := hotbedv1connect.NewWorkspaceServiceClient(ctrlConn)

	// Even the controller's forwarded req.Host is ignored; authoritative host
	// is always httpx.Peer(ctx).Hostname (board 9810). Sessionless calls
	// (cockpit/human) are allowed for any command (board 9796).
	pausedCmd, err := ctrlWS.PauseCommand(t.Context(), &pb.PauseCommandRequest{Id: "c-auth", Comment: "from cockpit", Host: "laptop"})
	if err != nil {
		t.Fatalf("PauseCommand as controller: %v", err)
	}
	if got := pausedCmd.Interventions[0].By.GetHost(); got != "ubu1" {
		t.Errorf("controller peer host = %q, want ubu1 (req.Host must be ignored)", got)
	}

	// Non-controller caller cannot spoof req.Host: uses httpx.Peer hostname.
	agentConn, err := api.Dial(addr, api.WorkerPort, tn.Client(agent))
	if err != nil {
		t.Fatal(err)
	}
	agentWS := hotbedv1connect.NewWorkspaceServiceClient(agentConn)

	// Mismatched SessionId is rejected on Resume/Pause/Stop (board 9796).
	if _, err := agentWS.ResumeCommand(t.Context(), &pb.ResumeCommandRequest{Id: "c-auth", SessionId: "other-sess"}); api.CodeOf(err) != api.CodeNotFound {
		t.Errorf("ResumeCommand(wrong session) err = %v, want CodeNotFound", err)
	}
	if _, err := agentWS.PauseCommand(t.Context(), &pb.PauseCommandRequest{Id: "c-auth", SessionId: "other-sess"}); api.CodeOf(err) != api.CodeNotFound {
		t.Errorf("PauseCommand(wrong session) err = %v, want CodeNotFound", err)
	}
	if _, err := agentWS.StopCommand(t.Context(), &pb.StopCommandRequest{Id: "c-auth", SessionId: "other-sess"}); api.CodeOf(err) != api.CodeNotFound {
		t.Errorf("StopCommand(wrong session) err = %v, want CodeNotFound", err)
	}

	// Matching SessionId succeeds, and req.Host is ignored in favor of peer hostname.
	resumedCmd, err := agentWS.ResumeCommand(t.Context(), &pb.ResumeCommandRequest{Id: "c-auth", SessionId: "sess-1", Host: "spoofed-host"})
	if err != nil {
		t.Fatalf("ResumeCommand direct with owning session: %v", err)
	}
	if got := resumedCmd.Interventions[1].By.GetHost(); got != "agentbox" {
		t.Errorf("direct caller host = %q, want agentbox", got)
	}
	if got := resumedCmd.Interventions[1].By.GetSession(); got != "sess-1" {
		t.Errorf("direct caller session = %q, want sess-1", got)
	}

	// Internal stop from non-controller peer is rejected (board 9808).
	if _, err := agentWS.StopCommand(t.Context(), &pb.StopCommandRequest{Id: "c-auth", Internal: true, Reason: "spoofed internal"}); api.CodeOf(err) != api.CodeNotAuthorized {
		t.Errorf("StopCommand(internal from non-controller) err = %v, want CodeNotAuthorized", err)
	}

	// Internal stop from controller stops command with Reason without recording a CommandIntervention (board 9808).
	startTestCommand(t, e, store, units, "c-int")
	stoppedInt, err := ctrlWS.StopCommand(t.Context(), &pb.StopCommandRequest{Id: "c-int", Internal: true, Reason: "scheduled cleanup"})
	if err != nil {
		t.Fatalf("StopCommand(internal from controller): %v", err)
	}
	if stoppedInt.State != cancelled || stoppedInt.Reason != "scheduled cleanup" {
		t.Fatalf("internal stop = state %v reason %q, want cancelled 'scheduled cleanup'", stoppedInt.State, stoppedInt.Reason)
	}
	if len(stoppedInt.Interventions) != 0 || api.StoppedByHand(stoppedInt) {
		t.Fatalf("internal stop recorded human intervention: %+v", stoppedInt.Interventions)
	}

	// Invalid IDs are rejected with CodeInvalidArgs.
	if _, err := agentWS.PauseCommand(t.Context(), &pb.PauseCommandRequest{Id: "../bad"}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Errorf("PauseCommand(bad id) err = %v, want CodeInvalidArgs", err)
	}
	if _, err := agentWS.ResumeCommand(t.Context(), &pb.ResumeCommandRequest{Id: "../bad"}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Errorf("ResumeCommand(bad id) err = %v, want CodeInvalidArgs", err)
	}
}

// blockingFreezeUnits blocks the first Freeze call until release is closed.
type blockingFreezeUnits struct {
	Units
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (u *blockingFreezeUnits) Freeze(ctx context.Context, name string) error {
	var blocked bool
	u.once.Do(func() {
		blocked = true
		close(u.entered)
	})
	if blocked {
		<-u.release
	}
	return u.Units.Freeze(ctx, name)
}

// board 9780: Concurrent soft-limit pause and manual Pause must serialize
// freeze/thaw per command so a losing soft-limit freeze can never thaw a unit
// that manual Pause paused.
func TestConcurrentSoftLimitPauseAndManualPauseKeepUnitFrozen(t *testing.T) {
	t.Run("soft-limit enters Freeze first, manual Pause wins", func(t *testing.T) {
		now := time.Unix(1795300000, 0)
		baseUnits, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}
		bu := &blockingFreezeUnits{
			Units:   baseUnits,
			entered: make(chan struct{}),
			release: make(chan struct{}),
		}
		e := newInterventionEngine(t, t.TempDir(), store, bu, nil, &now)
		startTestCommand(t, e, store, baseUnits, "c-race1")

		now = now.Add(11 * time.Minute)
		softDone := make(chan struct{})
		go func() {
			e.limit(t.Context())
			close(softDone)
		}()
		<-bu.entered

		manualDone := make(chan error, 1)
		go func() {
			_, err := e.Pause(t.Context(), "c-race1", Intervention{Comment: "hand pause", Host: "laptop"})
			manualDone <- err
		}()

		// Wait until manual Pause has set intervening=true (so soft-limit loses).
		eventuallyTrue(t, "manual Pause sets intervening", func() bool {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.commands["c-race1"].intervening
		})
		close(bu.release)

		<-softDone
		if err := <-manualDone; err != nil {
			t.Fatalf("manual Pause: %v", err)
		}
		cmd, _ := e.Command("c-race1")
		if cmd.State != paused || !api.PausedByHand(cmd) {
			t.Fatalf("after concurrent pause: state=%v pausedByHand=%v", cmd.State, api.PausedByHand(cmd))
		}
		if !e.frozen(t, "hotbed-c-race1") {
			t.Fatal("unit was thawed by losing soft-limit pause; want unit FROZEN")
		}
	})

	t.Run("manual Pause enters Freeze first, soft-limit sees paused and does not thaw", func(t *testing.T) {
		now := time.Unix(1795300000, 0)
		baseUnits, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}
		bu := &blockingFreezeUnits{
			Units:   baseUnits,
			entered: make(chan struct{}),
			release: make(chan struct{}),
		}
		e := newInterventionEngine(t, t.TempDir(), store, bu, nil, &now)
		startTestCommand(t, e, store, baseUnits, "c-race2")

		e.mu.Lock()
		c := e.commands["c-race2"]
		e.mu.Unlock()

		now = now.Add(11 * time.Minute)
		manualDone := make(chan error, 1)
		go func() {
			_, err := e.Pause(t.Context(), "c-race2", Intervention{Comment: "hand pause", Host: "laptop"})
			manualDone <- err
		}()
		<-bu.entered

		// Simulate e.pause already dispatched before c.intervening was checked.
		softDone := make(chan struct{})
		go func() {
			e.pause(t.Context(), c)
			close(softDone)
		}()
		time.Sleep(20 * time.Millisecond)
		close(bu.release)

		if err := <-manualDone; err != nil {
			t.Fatalf("manual Pause: %v", err)
		}
		<-softDone
		cmd, _ := e.Command("c-race2")
		if cmd.State != paused || !api.PausedByHand(cmd) {
			t.Fatalf("after concurrent pause: state=%v pausedByHand=%v", cmd.State, api.PausedByHand(cmd))
		}
		if !e.frozen(t, "hotbed-c-race2") {
			t.Fatal("unit was thawed by losing soft-limit pause; want unit FROZEN")
		}
	})
}

type blockingInspector struct {
	entered       chan struct{}
	sampleCtxErr  atomic.Value
	inspectCalled atomic.Bool
}

func (b *blockingInspector) Sample(ctx context.Context, _ inspect.Target, _ *pb.SampleCommandRequest, _ inspect.SampleEmit) error {
	close(b.entered)
	<-ctx.Done()
	b.sampleCtxErr.Store(ctx.Err())
	return ctx.Err()
}

func (b *blockingInspector) Inspect(_ context.Context, _ inspect.Target, _ *pb.InspectCommandRequest) (*pb.InspectCommandResponse, error) {
	b.inspectCalled.Store(true)
	return &pb.InspectCommandResponse{}, nil
}

func (b *blockingInspector) Trace(context.Context, inspect.Target, *pb.TraceCommandRequest, inspect.Emit) error {
	return nil
}

func (b *blockingInspector) Capabilities() []*pb.ToolCapability { return nil }

// board 9784: Hard-limit expiry must take precedence over c.intervening,
// cancel an in-flight diagnostic capture promptly, and stop the command.
func TestHardLimitExpiryCancelsInFlightDiagnosticsAndStops(t *testing.T) {
	now := time.Unix(1795300000, 0)
	units, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}
	insp := &blockingInspector{entered: make(chan struct{})}
	e := newInterventionEngine(t, t.TempDir(), store, units, insp, &now)
	startTestCommand(t, e, store, units, "c-hard", func(req *pb.RunCommandRequest) {
		req.Limits = &pb.CommandLimits{SoftLimitSeconds: 600, HardLimitSeconds: 3600}
	})

	pauseErrCh := make(chan error, 1)
	go func() {
		_, err := e.Pause(t.Context(), "c-hard", Intervention{Comment: "inspecting", Diagnose: true, Host: "laptop"})
		pauseErrCh <- err
	}()
	<-insp.entered

	// Advance clock past the 1h hard limit and run limit().
	now = now.Add(3601 * time.Second)
	e.limit(t.Context())

	select {
	case err := <-pauseErrCh:
		if api.CodeOf(err) != api.CodeConflict {
			t.Fatalf("Pause err after hard-limit stop = %v, want CodeConflict", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Pause did not abort promptly when hard-limit cancelled diagnostics")
	}

	if gotErr, _ := insp.sampleCtxErr.Load().(error); !errors.Is(gotErr, context.Canceled) {
		t.Errorf("Sample ctx.Err() = %v, want context.Canceled", gotErr)
	}
	if insp.inspectCalled.Load() {
		t.Error("Inspect should not be called after Sample context was cancelled")
	}
	cmd, _ := e.Command("c-hard")
	if cmd.State != cancelled || !strings.Contains(cmd.Reason, "hard time limit") {
		t.Fatalf("cmd = state %v reason %q, want cancelled by hard time limit", cmd.State, cmd.Reason)
	}
}

// board 9786: Manual Resume must respect active benchmarks and admission/host
// holds, transitioning PAUSED -> PARKED (unit stays frozen) and letting the
// scheduler thaw automatically once the hold clears.
func TestManualResumeHonorsBenchmarkAndSchedulerHolds(t *testing.T) {
	t.Run("active bench on ZFS pool keeps resumed command parked until bench ends", func(t *testing.T) {
		iso := fake.NewIsolation("4-7", 4)
		e, units := quietEngine(t, iso)

		work := e.exec(t, units, "c-work", "b", false)
		e.step(t.Context())
		if work.cmd().State != running {
			t.Fatalf("c-work state = %v, want running", work.cmd().State)
		}
		if _, err := e.Pause(t.Context(), "c-work", Intervention{Comment: "hold", Host: "laptop"}); err != nil {
			t.Fatalf("Pause: %v", err)
		}

		// Start a --bench measurement in workspace m.
		benchCmd := e.exec(t, units, "m-1", "m", true)
		e.step(t.Context())
		if benchCmd.cmd().State != running {
			t.Fatalf("m-1 state = %v (%s), want running", benchCmd.cmd().State, benchCmd.cmd().Reason)
		}

		// Manual Resume while measurement runs must leave c-work PARKED and frozen.
		resumed, err := e.Resume(t.Context(), "c-work", Intervention{Comment: "go when ready", Host: "laptop"})
		if err != nil {
			t.Fatalf("Resume during bench: %v", err)
		}
		if resumed.State != parked {
			t.Fatalf("Resume during bench state = %v, want PARKED", resumed.State)
		}
		if api.PausedByHand(resumed) {
			t.Fatal("PausedByHand = true after Resume")
		}
		if len(resumed.Interventions) != 2 || resumed.Interventions[1].Kind != pb.InterventionKind_INTERVENTION_KIND_RESUME {
			t.Fatalf("interventions = %+v, want PAUSE then RESUME", resumed.Interventions)
		}
		if !e.frozen(t, "hotbed-c-work") {
			t.Fatal("unit thawed during active bench; want FROZEN")
		}

		// Stepping while bench still runs must keep c-work PARKED and frozen.
		e.step(t.Context())
		if s := e.state("c-work"); s != parked || !e.frozen(t, "hotbed-c-work") {
			t.Fatalf("during bench step: state=%v frozen=%v, want parked and frozen", s, e.frozen(t, "hotbed-c-work"))
		}

		// Once the measurement ends, stepping thaws c-work automatically.
		if _, err := e.Stop(t.Context(), "m-1", "done"); err != nil {
			t.Fatal(err)
		}
		e.step(t.Context())
		if s := e.state("c-work"); s != running || e.frozen(t, "hotbed-c-work") {
			t.Fatalf("after bench ended: state=%v frozen=%v, want running and thawed", s, e.frozen(t, "hotbed-c-work"))
		}
	})

	t.Run("memory reservation limit keeps resumed command parked until room frees", func(t *testing.T) {
		now := time.Unix(1795300000, 0)
		units, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}
		e := newInterventionEngine(t, t.TempDir(), store, units, nil, &now)
		// Reserve 10 GiB on a 16 GiB node (admitLimit = 13.76 GiB).
		startTestCommand(t, e, store, units, "c-big1", func(req *pb.RunCommandRequest) {
			req.Limits = &pb.CommandLimits{MemoryLowBytes: 10 << 30}
		})
		if _, err := e.Pause(t.Context(), "c-big1", Intervention{Comment: "hold", Host: "laptop"}); err != nil {
			t.Fatal(err)
		}
		// While c-big1 is paused (reserves 0), admit another 10 GiB command.
		startTestCommand(t, e, store, units, "c-big2", func(req *pb.RunCommandRequest) {
			req.Limits = &pb.CommandLimits{MemoryLowBytes: 10 << 30}
		})
		if s := e.state("c-big2"); s != running {
			t.Fatalf("c-big2 state = %v, want running", s)
		}

		// Resuming c-big1 cannot fit beside c-big2 -> stays PARKED & frozen.
		resumed, err := e.Resume(t.Context(), "c-big1", Intervention{Host: "laptop"})
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if resumed.State != parked || !e.frozen(t, "hotbed-c-big1") {
			t.Fatalf("Resume over admitLimit: state=%v frozen=%v, want parked and frozen", resumed.State, e.frozen(t, "hotbed-c-big1"))
		}

		// Stop c-big2 and step -> c-big1 thaws into running.
		if _, err := e.Stop(t.Context(), "c-big2", "done"); err != nil {
			t.Fatal(err)
		}
		e.step(t.Context())
		if s := e.state("c-big1"); s != running || e.frozen(t, "hotbed-c-big1") {
			t.Fatalf("after c-big2 ended: state=%v frozen=%v, want running and thawed", s, e.frozen(t, "hotbed-c-big1"))
		}
	})
}

type diagDeliveryCluster struct {
	hotbedv1connect.ClusterServiceClient
	sizeLimitedCluster
	failDiagNext int
	savedDiags   []*pb.CommandDiagnostics
}

func (c *diagDeliveryCluster) PushEvents(ctx context.Context, req *pb.PushEventsRequest) (*pb.PushEventsResponse, error) {
	return c.sizeLimitedCluster.PushEvents(ctx, req)
}

func (c *diagDeliveryCluster) SaveCommandDiagnostics(_ context.Context, req *pb.SaveCommandDiagnosticsRequest) (*pb.SaveCommandDiagnosticsResponse, error) {
	if c.failDiagNext > 0 {
		c.failDiagNext--
		return nil, connect.NewError(connect.CodeUnavailable, "controller busy")
	}
	c.savedDiags = append(c.savedDiags, proto.Clone(req.Diagnostics).(*pb.CommandDiagnostics))
	return &pb.SaveCommandDiagnosticsResponse{}, nil
}

// board 9788: Oversized diagnostics are explicitly bounded to <= 1 MiB,
// persisted as pending_diag in worker.db, and delivered independently via
// SaveCommandDiagnostics with ack/retry even when checkpoints skip events.
func TestDiagnosticsBoundedAndDeliveredIndependentlyOfCheckpoints(t *testing.T) {
	t.Run("oversized diagnostics truncated to <= 1 MiB", func(t *testing.T) {
		now := time.Unix(1795300000, 0)
		units, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}
		bigStr := strings.Repeat("m", 200<<10)
		maps := make([]*pb.ProcessMemoryMap, 8)
		for i := range maps {
			maps[i] = &pb.ProcessMemoryMap{Pid: int32(i + 1), Comm: bigStr}
		}
		frames := make([]*pb.ProfileFrame, 8)
		for i := range frames {
			frames[i] = &pb.ProfileFrame{Name: bigStr}
		}
		insp := &fakeInspector{
			sample:     &pb.ProcessSample{CommandId: "c-bigdiag", DurationMs: 2000, Frames: frames},
			inspectRes: &pb.InspectCommandResponse{CommandId: "c-bigdiag", UnitName: "hotbed-c-bigdiag", Maps: maps},
		}
		e := newInterventionEngine(t, t.TempDir(), store, units, insp, &now)
		startTestCommand(t, e, store, units, "c-bigdiag")

		got, err := e.StopWith(t.Context(), "c-bigdiag", Intervention{Comment: "oom?", Diagnose: true, Host: "laptop"})
		if err != nil {
			t.Fatalf("StopWith: %v", err)
		}
		iv := got.Interventions[0]
		if !iv.Diagnosed {
			t.Fatalf("Diagnosed = false, want true after trimming verbose sections; err=%q", iv.DiagnosticsError)
		}
		if !strings.Contains(iv.DiagnosticsError, "truncated") {
			t.Errorf("DiagnosticsError = %q, want truncation note", iv.DiagnosticsError)
		}
		pending, err := e.PendingDiagnostics()
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) != 1 {
			t.Fatalf("len(PendingDiagnostics) = %d, want 1", len(pending))
		}
		if sz := proto.Size(pending[0]); sz > maxDiagnosticsBytes {
			t.Fatalf("bounded diagnostics size = %d bytes, want <= %d", sz, maxDiagnosticsBytes)
		}
	})

	t.Run("unshrinkable oversized diagnostics records Diagnosed=false", func(t *testing.T) {
		now := time.Unix(1795300000, 0)
		units, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}
		huge := strings.Repeat("x", maxDiagnosticsBytes+64<<10)
		insp := &fakeInspector{
			inspectRes: &pb.InspectCommandResponse{CommandId: "c-unshrink", Cgroup: huge},
		}
		e := newInterventionEngine(t, t.TempDir(), store, units, insp, &now)
		startTestCommand(t, e, store, units, "c-unshrink")

		got, err := e.StopWith(t.Context(), "c-unshrink", Intervention{Diagnose: true, Host: "laptop"})
		if err != nil {
			t.Fatalf("StopWith: %v", err)
		}
		iv := got.Interventions[0]
		if iv.Diagnosed {
			t.Fatalf("Diagnosed = true for unshrinkable > 1 MiB diagnostics, want false")
		}
		if !strings.Contains(iv.DiagnosticsError, "exceeded") {
			t.Errorf("DiagnosticsError = %q, want size-exceeded note", iv.DiagnosticsError)
		}
		pending, err := e.PendingDiagnostics()
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) != 0 {
			t.Fatalf("len(PendingDiagnostics) = %d, want 0", len(pending))
		}
	})

	t.Run("pending_diag persists across checkpoints, retries on error, deletes only on ack", func(t *testing.T) {
		now := time.Unix(1795300000, 0)
		dir := t.TempDir()
		units, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}
		insp := &fakeInspector{
			sample:     &pb.ProcessSample{CommandId: "c-deliv", DurationMs: 2000},
			inspectRes: &pb.InspectCommandResponse{CommandId: "c-deliv", UnitName: "hotbed-c-deliv"},
		}
		e := newInterventionEngine(t, dir, store, units, insp, &now)
		startTestCommand(t, e, store, units, "c-deliv")
		if _, err := e.StopWith(t.Context(), "c-deliv", Intervention{Comment: "stuck", Diagnose: true, Host: "laptop"}); err != nil {
			t.Fatal(err)
		}
		// Add an unpushable event that forces the pusher to checkpoint past it.
		if err := e.db.commitEvent(&pb.Event{
			At:   now.UnixMilli(),
			Feed: &pb.WorkerEvent{At: now.UnixMilli(), Kind: "fail", Text: strings.Repeat("z", 64<<10)},
		}); err != nil {
			t.Fatal(err)
		}

		cluster := &diagDeliveryCluster{
			sizeLimitedCluster: sizeLimitedCluster{maxBytes: 16 << 10, epoch: e.Epoch()},
			failDiagNext:       1,
		}
		p := &pusher{eng: e, cluster: cluster, clusterID: "cl", known: true}

		// Push until events + checkpoint advance and first SaveCommandDiagnostics attempt fails.
		var diagErr error
		for range 10 {
			more, err := p.once(t.Context())
			if err != nil {
				diagErr = err
				break
			}
			if !more {
				break
			}
		}
		if diagErr == nil {
			t.Fatal("expected first SaveCommandDiagnostics attempt to fail transiently")
		}
		if cluster.checkpoints == 0 {
			t.Error("expected checkpoint fallback for oversized event")
		}
		// Unacknowledged pending_diag must remain in worker.db.
		pending, err := e.PendingDiagnostics()
		if err != nil || len(pending) != 1 {
			t.Fatalf("after failed SaveCommandDiagnostics: pending = (%d, %v), want 1", len(pending), err)
		}

		// Retry succeeds and deletes pending_diag from worker.db.
		if _, err := p.once(t.Context()); err != nil {
			t.Fatalf("retry pusher.once: %v", err)
		}
		if len(cluster.savedDiags) != 1 || cluster.savedDiags[0].CommandId != "c-deliv" || cluster.savedDiags[0].GetIntervention().GetSeq() != 1 {
			t.Fatalf("savedDiags = %+v, want c-deliv seq 1", cluster.savedDiags)
		}
		pending, err = e.PendingDiagnostics()
		if err != nil || len(pending) != 0 {
			t.Fatalf("after ack: pending = (%d, %v), want 0", len(pending), err)
		}
	})
}

// board 9794: FollowLog emits a terminal LogChunk carrying State and EndedBy
// when following ends, even if a hand pause is immediately followed by Resume.
func TestFollowLogEmitsTerminalChunkOnQuickPauseResume(t *testing.T) {
	now := time.Unix(1795300000, 0)
	units, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}
	e := newInterventionEngine(t, t.TempDir(), store, units, nil, &now)
	startTestCommand(t, e, store, units, "c-follow")

	logPath, err := e.logPath("c-follow")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	peer := tailnet.Peer{NodeID: "nClient", Hostname: "laptop", Login: "ramon@"}
	tn := tailnettest.New(tailnet.Self{Peer: peer})
	ts := httptest.NewUnstartedServer(Handler(tn, httpx.Policy{
		Owner: "ramon@",
		Self:  []netip.Addr{netip.MustParseAddr("127.0.0.1")},
	}, e))
	ts.Config.Protocols = httpx.Protocols()
	ts.Start()
	defer ts.Close()

	conn, err := api.Dial(strings.TrimPrefix(ts.URL, "http://"), api.WorkerPort, tn.Client(peer))
	if err != nil {
		t.Fatal(err)
	}
	client := hotbedv1connect.NewWorkspaceServiceClient(conn)

	var chunks []*pb.LogChunk
	var received bytes.Buffer
	recvDone := make(chan error, 1)
	go func() {
		stream, err := client.FollowLog(t.Context(), &pb.FollowLogRequest{Id: "c-follow", Follow: true})
		if err != nil {
			recvDone <- err
			return
		}
		defer stream.Close()
		for {
			chunk, err := stream.Receive()
			if errors.Is(err, io.EOF) {
				recvDone <- nil
				return
			}
			if err != nil {
				recvDone <- err
				return
			}
			chunks = append(chunks, proto.Clone(chunk).(*pb.LogChunk))
			received.Write(chunk.Data)
		}
	}()

	eventuallyTrue(t, "follower registered", func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.commands["c-follow"].followers == 1
	})

	// Pause and immediately resume without waiting for FollowLog to exit first.
	if _, err := e.Pause(t.Context(), "c-follow", Intervention{Comment: "checking deadlock", Host: "laptop"}); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if _, err := e.Resume(t.Context(), "c-follow", Intervention{Host: "laptop"}); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	select {
	case err := <-recvDone:
		if err != nil {
			t.Fatalf("FollowLog error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for FollowLog to emit terminal chunk on pause")
	}

	if received.String() != "hello\n" {
		t.Errorf("received data = %q, want %q", received.String(), "hello\n")
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least one LogChunk")
	}
	last := chunks[len(chunks)-1]
	if last.State != paused {
		t.Fatalf("terminal chunk State = %v, want PAUSED", last.State)
	}
	if last.EndedBy == nil || last.EndedBy.Kind != pb.InterventionKind_INTERVENTION_KIND_PAUSE || last.EndedBy.Comment != "checking deadlock" {
		t.Fatalf("terminal chunk EndedBy = %+v, want PAUSE intervention 'checking deadlock'", last.EndedBy)
	}
}

// board 9812: Comments over maxInterventionCommentBytes (2 KiB) are rejected
// with CodeInvalidArgs, timeline detail stays concise, and retained
// Command.Interventions is capped with monotonic Seq.
func TestInterventionCommentBoundAndTimelineConcise(t *testing.T) {
	now := time.Unix(1795300000, 0)
	units, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}
	e := newInterventionEngine(t, t.TempDir(), store, units, nil, &now)
	startTestCommand(t, e, store, units, "c-bound")

	tooLong := strings.Repeat("a", maxInterventionCommentBytes+1)
	if _, err := e.Pause(t.Context(), "c-bound", Intervention{Comment: tooLong, Host: "laptop"}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Fatalf("Pause(>2KiB comment) err = %v, want CodeInvalidArgs", err)
	}
	if _, err := e.StopWith(t.Context(), "c-bound", Intervention{Comment: tooLong, Host: "laptop"}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Fatalf("StopWith(>2KiB comment) err = %v, want CodeInvalidArgs", err)
	}
	if cmd, _ := e.Command("c-bound"); cmd.State != running || len(cmd.Interventions) != 0 {
		t.Fatalf("oversized comment mutated command: state=%v interventions=%+v", cmd.State, cmd.Interventions)
	}

	// Valid comment (1500 bytes <= 2048) is stored in full on CommandIntervention
	// while timeline detail carries a concise truncated preview.
	longValid := strings.Repeat("b", 1500)
	got, err := e.Pause(t.Context(), "c-bound", Intervention{Comment: longValid, Host: "laptop"})
	if err != nil {
		t.Fatalf("Pause(1500B comment): %v", err)
	}
	if got.Interventions[0].Comment != longValid {
		t.Errorf("stored comment len = %d, want 1500", len(got.Interventions[0].Comment))
	}
	lastTL := got.Timeline[len(got.Timeline)-1]
	if len(lastTL.Detail) >= len(longValid) {
		t.Errorf("timeline detail len = %d, want concise preview (< %d)", len(lastTL.Detail), len(longValid))
	}

	// Resume rejects >2KiB comment too.
	if _, err := e.Resume(t.Context(), "c-bound", Intervention{Comment: tooLong, Host: "laptop"}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Fatalf("Resume(>2KiB comment) err = %v, want CodeInvalidArgs", err)
	}
	if _, err := e.Resume(t.Context(), "c-bound", Intervention{Host: "laptop"}); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	// Exceeding maxCommandInterventions keeps the most recent 32 with monotonic Seq.
	for range 20 {
		if _, err := e.Pause(t.Context(), "c-bound", Intervention{Host: "laptop"}); err != nil {
			t.Fatalf("Pause loop: %v", err)
		}
		if _, err := e.Resume(t.Context(), "c-bound", Intervention{Host: "laptop"}); err != nil {
			t.Fatalf("Resume loop: %v", err)
		}
	}
	cmd, _ := e.Command("c-bound")
	if len(cmd.Interventions) != maxCommandInterventions {
		t.Fatalf("len(Interventions) = %d, want capped at %d", len(cmd.Interventions), maxCommandInterventions)
	}
	lastSeq := cmd.Interventions[len(cmd.Interventions)-1].Seq
	if lastSeq != 42 {
		t.Fatalf("last Seq = %d, want 42 (monotonic across cap)", lastSeq)
	}
}
