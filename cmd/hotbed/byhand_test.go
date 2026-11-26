package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/worker"
)

// ran is how a hotbed command that ran in the background exited.
type ran struct {
	code           int
	stdout, stderr string
}

// runInBackground runs an attached hotbed run in session's workspace of
// app, and returns its command's ID once it has printed start, and how run
// exits.
func (c *cluster) runInBackground(session string, args ...string) (string, <-chan ran) {
	c.t.Helper()
	done := make(chan ran, 1)
	go func() {
		code, stdout, stderr := c.hotbed(append([]string{"run", "-s", session, "-r", "app", "--"}, args...)...)
		done <- ran{code, stdout, stderr}
	}()
	var id string
	c.eventually("the run's command running", func() bool {
		var ls pb.ListCommandsResponse
		c.ok(&ls, "ps", "-s", session)
		for _, cmd := range ls.Commands {
			if cmd.State == pb.CommandState_COMMAND_STATE_RUNNING {
				id = cmd.Id
				return true
			}
		}
		return false
	})
	c.eventually("the command's start", func() bool {
		_, out, _ := c.hotbed("logs", "-s", session, id)
		return out == "start\n"
	})
	return id, done
}

func diagnosingCluster(t *testing.T) *cluster {
	t.Helper()
	c := newCluster(t, func(cfg *worker.EngineConfig) { cfg.Inspector = fake.Inspector{} })
	c.addRepo(upstream(t))
	return c
}

// Stopping a command via CLI with -m and --diagnose takes diagnostics first,
// ends the attached run with COMMAND_STOPPED (who stopped it, comment, and
// hotbed diagnostics hint), emits a session poll event with the note, and
// keeps diagnostics on the controller even after the worker is halted.
func TestByHandStopWithDiagnoseTellsWaitingAgentAndPersists(t *testing.T) {
	c := diagnosingCluster(t)
	c.ok(nil, "session", "start", "calm-yak")
	c.workspace("calm-yak", nil)
	id, done := c.runInBackground("calm-yak", "sh", "-c", "echo start; sleep 300")

	const comment = "taking too long"
	var stopped pb.Command
	c.ok(&stopped, "stop", "-s", "calm-yak", "-m", comment, "--diagnose", id)
	if stopped.State != pb.CommandState_COMMAND_STATE_CANCELLED || len(stopped.Interventions) != 1 {
		t.Fatalf("stopped: %v with interventions %v", stopped.State, stopped.Interventions)
	}
	iv := stopped.Interventions[0]
	if iv.Seq != 1 || iv.Kind != pb.InterventionKind_INTERVENTION_KIND_STOP || iv.Comment != comment ||
		!iv.Diagnosed || iv.DiagnosticsError != "" ||
		iv.By.GetHost() != "laptop" || iv.By.GetSession() != "calm-yak" {
		t.Errorf("its intervention: %v", iv)
	}
	note := "stopped by session calm-yak on laptop: \"" + comment + "\". Diagnostics taken as it stopped: hotbed diagnostics " + id + "."
	if stopped.Reason != note {
		t.Errorf("its reason = %q, want %q", stopped.Reason, note)
	}

	r := <-done
	if want := "hotbed: COMMAND_STOPPED: " + id + " was " + note + "\n"; r.code != exitError || r.stdout != "start\n" || r.stderr[strings.Index(r.stderr, "\n")+1:] != want {
		t.Errorf("the run: exit %d, stdout %q, stderr %q; want exit 1, start, and stderr ending %q", r.code, r.stdout, r.stderr, want)
	}

	// Verify followStatus returns CodeCommandStopped with diagnostics suggestion.
	var statusErrBuf bytes.Buffer
	_, statusErr := followStatus(id, &stopped, &statusErrBuf)
	apiErr := api.FromConnect(statusErr)
	if apiErr == nil || apiErr.Code != api.CodeCommandStopped {
		t.Fatalf("followStatus err = %v, want %s", statusErr, api.CodeCommandStopped)
	}
	if !strings.Contains(apiErr.Suggestion, "hotbed diagnostics "+id) {
		t.Errorf("followStatus suggestion = %q, want hotbed diagnostics %s", apiErr.Suggestion, id)
	}

	// Verify hotbed poll carries the cancelled event with the intervention note.
	c.eventually("its end in the session", func() bool { return len(ends(c.events("calm-yak"), id)) > 0 })
	var polled pb.SessionEvents
	c.ok(&polled, "poll", "-s", "calm-yak", "--after", "0", "--timeout", "1")
	foundCancel := false
	for _, ev := range polled.Events {
		if cu := ev.GetCommand(); cu != nil && cu.CommandId == id && cu.What == pb.CommandUpdate_WHAT_CANCELLED {
			foundCancel = true
			if ev.Detail != note {
				t.Errorf("poll cancelled detail = %q, want %q", ev.Detail, note)
			}
		}
	}
	if !foundCancel {
		t.Errorf("poll events missing WHAT_CANCELLED for %s: %v", id, polled.Events)
	}

	// The controller keeps the diagnostics even after the worker is gone.
	c.nodes[0].halt()
	var diags pb.GetCommandDiagnosticsResponse
	c.ok(&diags, "diagnostics", id)
	if len(diags.Diagnostics) != 1 {
		t.Fatalf("diagnostics of %s: %v", id, &diags)
	}
	d := diags.Diagnostics[0]
	if d.CommandId != id || d.Intervention.GetSeq() != 1 || d.Intervention.GetComment() != comment ||
		len(d.Sample.GetProcs()) == 0 || len(d.Inspect.GetProcs()) == 0 || len(d.Inspect.GetKstacks()) == 0 {
		t.Errorf("its diagnostics: command %s, intervention %v, sample procs %d, inspect procs %d kstacks %d",
			d.CommandId, d.Intervention, len(d.Sample.GetProcs()), len(d.Inspect.GetProcs()), len(d.Inspect.GetKstacks()))
	}

	code, text, stderr := c.hotbed("diagnostics", id)
	if code != exitOK {
		t.Fatalf("diagnostics as text: exit %d, stderr %q, stdout %q", code, stderr, text)
	}
	for _, want := range []string{
		"== 1: stop by session calm-yak on laptop",
		": " + comment,
		"Sampling command " + id,
		"=== Process ",
		"=== Inspection",
		"Processes (",
		"Kernel Stacks (",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("diagnostics text missing %q:\n%s", want, text)
		}
	}
}

// A person stops a command from the cockpit (via controller, no session): its
// diagnostics are taken first, the worker stamps the authenticated peer
// hostname ("controller" over the controller WebSocket tunnel) rather than
// trusting any caller-supplied Host field, and the agent's attached run ends
// with COMMAND_STOPPED naming that host, the comment, and the diagnostics
// command.
func TestByHandStopFromCockpitTellsWaitingAgent(t *testing.T) {
	c := diagnosingCluster(t)
	c.ok(nil, "session", "start", "keen-fox")
	c.workspace("keen-fox", nil)
	id, done := c.runInBackground("keen-fox", "sh", "-c", "echo start; sleep 300")

	const comment = "this has run for very long; what is going on?"
	stopped, err := c.workspaces().StopCommand(t.Context(), &pb.StopCommandRequest{Id: id, Comment: comment, Diagnose: true})
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != pb.CommandState_COMMAND_STATE_CANCELLED || len(stopped.Interventions) != 1 {
		t.Fatalf("stopped: %v with interventions %v", stopped.State, stopped.Interventions)
	}
	iv := stopped.Interventions[0]
	if iv.By.GetHost() != "controller" || iv.By.GetSession() != "" || !iv.Diagnosed {
		t.Errorf("cockpit stop intervention: %v", iv)
	}
	note := "stopped by controller: \"" + comment + "\". Diagnostics taken as it stopped: hotbed diagnostics " + id + "."
	if stopped.Reason != note {
		t.Errorf("reason = %q, want %q", stopped.Reason, note)
	}
	r := <-done
	if want := "hotbed: COMMAND_STOPPED: " + id + " was " + note + "\n"; r.code != exitError || r.stdout != "start\n" || r.stderr[strings.Index(r.stderr, "\n")+1:] != want {
		t.Errorf("the run: exit %d, stdout %q, stderr %q; want exit 1, start, and stderr ending %q", r.code, r.stdout, r.stderr, want)
	}
	c.eventually("its end in the session", func() bool { return len(ends(c.events("keen-fox"), id)) > 0 })
	if got := ends(c.events("keen-fox"), id); !slices.Equal(got, []string{"cancelled " + note}) {
		t.Errorf("ends of %s = %q", id, got)
	}
}

// Pausing a command by hand with -m and --diagnose makes attached run exit 124
// with the comment and diagnostics hint; running logs -f again does NOT undo
// the hand pause (exits 124 again, stays PAUSED); resume -m thaws it and lets
// it finish; poll carries the pause and resume intervention notes.
func TestByHandPauseHoldsUntilResumed(t *testing.T) {
	c := diagnosingCluster(t)
	c.ok(nil, "session", "start", "deft-emu")
	c.workspace("deft-emu", nil)
	id, done := c.runInBackground("deft-emu", "sh", "-c", "echo start; while [ ! -e go ]; do sleep 0.1; done; echo done; exit 7")

	var paused pb.Command
	c.ok(&paused, "pause", "-s", "deft-emu", "--diagnose", "-m", "check locks", id)
	note := "paused by session deft-emu on laptop: \"check locks\". Diagnostics taken as it paused: hotbed diagnostics " + id +
		". It stays paused until resumed: hotbed resume -s deft-emu " + id + ", or its page in the cockpit."
	if paused.State != pb.CommandState_COMMAND_STATE_PAUSED || paused.Reason != note {
		t.Fatalf("paused: %v, %q; want paused, %q", paused.State, paused.Reason, note)
	}
	held := "hotbed: " + id + " was " + note + "\n"
	r := <-done
	if r.code != exitPaused || r.stdout != "start\n" || !strings.HasSuffix(r.stderr, held) {
		t.Fatalf("the run: exit %d, stdout %q, stderr %q; want exit 124, start, and stderr ending %q", r.code, r.stdout, r.stderr, held)
	}

	// Waiting on it again leaves it paused: it does not see go.
	c.ok(nil, "run", "-s", "deft-emu", "-r", "app", "--detach", "--mem", "1G", "--", "touch", "go")
	if code, _, stderr := c.hotbed("logs", "-f", "-s", "deft-emu", id); code != exitPaused || stderr != held {
		t.Fatalf("logs -f of it: exit %d, stderr %q; want 124, %q", code, stderr, held)
	}
	if cmd := c.commandOf("deft-emu", id); cmd.State != pb.CommandState_COMMAND_STATE_PAUSED {
		t.Fatalf("after logs -f it is %v, want still paused", cmd.State)
	}
	if code, e := c.fails("pause", "-s", "deft-emu", id); code != exitError || e.Code != api.CodeConflict || e.Message != "command "+id+" is paused already: "+note {
		t.Errorf("pausing it again: exit %d, %v", code, e)
	}

	var resumed pb.Command
	c.ok(&resumed, "resume", "-s", "deft-emu", "-m", "go ahead", id)
	if resumed.State != pb.CommandState_COMMAND_STATE_RUNNING || len(resumed.Interventions) != 2 ||
		resumed.Interventions[1].Kind != pb.InterventionKind_INTERVENTION_KIND_RESUME || resumed.Interventions[1].Comment != "go ahead" {
		t.Fatalf("resumed: %v with interventions %v", resumed.State, resumed.Interventions)
	}
	code, stdout, stderr := c.hotbed("logs", "-f", "-s", "deft-emu", id)
	if code != 7 || stdout != "start\ndone\n" {
		t.Fatalf("logs -f once resumed: exit %d, stdout %q, stderr %q; want exit 7 with all its output", code, stdout, stderr)
	}
	if code, e := c.fails("resume", "-s", "deft-emu", id); code != exitError || e.Code != api.CodeConflict || e.Message != "command "+id+" has ended: there is nothing to resume" {
		t.Errorf("resuming it once ended: exit %d, %v", code, e)
	}

	// Verify hotbed poll returns paused, resumed, and exited events with notes.
	c.eventually("its end in the session", func() bool { return len(ends(c.events("deft-emu"), id)) > 0 })
	if got := eventWhats(c.events("deft-emu"), id); !slices.Equal(got, []string{"admitted", "paused", "resumed", "exited"}) {
		t.Errorf("events of %s = %q", id, got)
	}
	var polled pb.SessionEvents
	c.ok(&polled, "poll", "-s", "deft-emu", "--after", "0", "--timeout", "1")
	var pauseDetail, resumeDetail string
	for _, ev := range polled.Events {
		cu := ev.GetCommand()
		if cu == nil || cu.CommandId != id {
			continue
		}
		switch cu.What {
		case pb.CommandUpdate_WHAT_PAUSED:
			pauseDetail = ev.Detail
		case pb.CommandUpdate_WHAT_RESUMED:
			resumeDetail = ev.Detail
		}
	}
	if pauseDetail != note {
		t.Errorf("poll paused detail = %q, want %q", pauseDetail, note)
	}
	wantResume := `resumed by session deft-emu on laptop: "go ahead".`
	if resumeDetail != wantResume {
		t.Errorf("poll resumed detail = %q, want %q", resumeDetail, wantResume)
	}

	var diags pb.GetCommandDiagnosticsResponse
	c.ok(&diags, "diagnostics", id)
	if len(diags.Diagnostics) != 1 || diags.Diagnostics[0].Intervention.GetKind() != pb.InterventionKind_INTERVENTION_KIND_PAUSE {
		t.Errorf("diagnostics of %s: %v", id, &diags)
	}
}

// A stop without diagnostics asked for takes none, and a worker that takes
// none says so, while the stop or pause goes on; a command with none has
// none to read.
func TestByHandWithoutDiagnostics(t *testing.T) {
	c := newCluster(t) // no inspector: the worker takes no diagnostics
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "bare-gnu")
	c.workspace("bare-gnu", nil)
	cmd := &pb.Command{}
	c.ok(cmd, "run", "-s", "bare-gnu", "-r", "app", "--detach", "--mem", "1G", "--", "sleep", "300")
	c.eventually("it running", func() bool {
		return c.commandOf("bare-gnu", cmd.Id).State == pb.CommandState_COMMAND_STATE_RUNNING
	})

	// Oversized comments (> 2048 bytes) are rejected cleanly with INVALID_ARGS.
	tooLong := strings.Repeat("x", 2049)
	if code, e := c.fails("pause", "-s", "bare-gnu", "-m", tooLong, cmd.Id); code != exitError || e.Code != api.CodeInvalidArgs {
		t.Errorf("pause with 2049-byte comment: exit %d, %v; want INVALID_ARGS", code, e)
	}

	// Pause in plain text mode verifies both pause formatting and missing-inspector note.
	code, pauseOut, pauseErr := c.hotbed("pause", "-s", "bare-gnu", "--diagnose", cmd.Id)
	if code != exitOK {
		t.Fatalf("pause plain: exit %d, stderr %q", code, pauseErr)
	}
	wantPauseReason := "paused by session bare-gnu on laptop. No diagnostics: this worker takes no diagnostics: process inspection is not configured on it." +
		" It stays paused until resumed: hotbed resume -s bare-gnu " + cmd.Id + ", or its page in the cockpit."
	if !strings.Contains(pauseOut, cmd.Id) || !strings.Contains(pauseOut, "paused") || !strings.Contains(pauseOut, wantPauseReason) {
		t.Errorf("pause plain output = %q, want id, state, and %q", pauseOut, wantPauseReason)
	}

	var stopped pb.Command
	c.ok(&stopped, "stop", "-s", "bare-gnu", cmd.Id)
	if stopped.State != pb.CommandState_COMMAND_STATE_CANCELLED || stopped.Reason != "stopped by session bare-gnu on laptop." {
		t.Errorf("stopped: %v, %q", stopped.State, stopped.Reason)
	}

	var diags pb.GetCommandDiagnosticsResponse
	c.ok(&diags, "diagnostics", cmd.Id)
	if len(diags.Diagnostics) != 0 {
		t.Errorf("diagnostics of %s: %v, want none", cmd.Id, &diags)
	}
	if code, text, stderr := c.hotbed("diagnostics", cmd.Id); code != exitOK || text != "(no diagnostics)\n" {
		t.Errorf("empty diagnostics plain: exit %d, stderr %q, stdout %q", code, stderr, text)
	}
	if code, e := c.fails("diagnostics", "nosuchcmd"); code != exitError || e.Code != api.CodeNotFound {
		t.Errorf("diagnostics of no command: exit %d, %v", code, e)
	}
}

// hotbed diagnostics is served via apicmd (CLI + MCP) with --seq and --after-seq
// pagination, plain-text report formatting, and full JSON envelope output.
func TestByHandDiagnosticsCLIAndMCP(t *testing.T) {
	c := diagnosingCluster(t)
	c.ok(nil, "session", "start", "diag-owl")
	c.workspace("diag-owl", nil)
	id, done := c.runInBackground("diag-owl", "sh", "-c", "echo start; sleep 300")

	c.ok(nil, "pause", "-s", "diag-owl", "--diagnose", "-m", "first pause", id)
	r := <-done
	if r.code != exitPaused {
		t.Fatalf("after pause: exit %d, stderr %q; want 124", r.code, r.stderr)
	}
	c.ok(nil, "resume", "-s", "diag-owl", id)
	c.ok(nil, "stop", "-s", "diag-owl", "--diagnose", "-m", "final stop", id)

	// 1. CLI default (all diagnostics) + --seq and --after-seq pagination.
	var all pb.GetCommandDiagnosticsResponse
	c.ok(&all, "diagnostics", id)
	if len(all.Diagnostics) != 2 ||
		all.Diagnostics[0].GetIntervention().GetSeq() != 1 || all.Diagnostics[0].GetIntervention().GetComment() != "first pause" ||
		all.Diagnostics[1].GetIntervention().GetSeq() != 3 || all.Diagnostics[1].GetIntervention().GetComment() != "final stop" {
		t.Fatalf("diagnostics %s = %+v, want seq 1 (first pause) and seq 3 (final stop)", id, all.Diagnostics)
	}

	code, jsonOut, stderr := c.hotbed("--json", "diagnostics", id)
	if code != exitOK {
		t.Fatalf("diagnostics --json: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(jsonOut, `"hasMore": false`) || !strings.Contains(jsonOut, `"nextAfterSeq": 0`) {
		t.Errorf("diagnostics --json missing pagination fields:\n%s", jsonOut)
	}

	var seq1 pb.GetCommandDiagnosticsResponse
	c.ok(&seq1, "diagnostics", id, "--seq", "1")
	if len(seq1.Diagnostics) != 1 || seq1.Diagnostics[0].GetIntervention().GetSeq() != 1 || seq1.Diagnostics[0].GetIntervention().GetComment() != "first pause" {
		t.Fatalf("diagnostics --seq 1 = %+v", seq1.Diagnostics)
	}

	var after1 pb.GetCommandDiagnosticsResponse
	c.ok(&after1, "diagnostics", id, "--after-seq", "1")
	if len(after1.Diagnostics) != 1 || after1.Diagnostics[0].GetIntervention().GetSeq() != 3 || after1.Diagnostics[0].GetIntervention().GetComment() != "final stop" {
		t.Fatalf("diagnostics --after-seq 1 = %+v", after1.Diagnostics)
	}

	code, plainSeq1, stderr := c.hotbed("diagnostics", id, "--seq", "1")
	if code != exitOK {
		t.Fatalf("diagnostics --seq 1 plain: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(plainSeq1, "== 1: pause by session diag-owl on laptop") || !strings.Contains(plainSeq1, ": first pause") || strings.Contains(plainSeq1, "final stop") {
		t.Errorf("diagnostics --seq 1 plain unexpected:\n%s", plainSeq1)
	}

	// 2. MCP tool discovery and invocation with id, seq, and after_seq.
	t.Setenv("T3CODE_THREAD_ID", "thread-diagnostics-mcp")
	m := c.mcp()
	defer m.stop()

	tools, err := m.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var toolSchema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	foundTool := false
	for _, tool := range tools.Tools {
		if tool.Name == "diagnostics" {
			foundTool = true
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
				t.Errorf("MCP tool diagnostics ReadOnlyHint = %+v, want true", tool.Annotations)
			}
			b, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(b, &toolSchema); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !foundTool {
		t.Fatalf("MCP tool diagnostics not registered")
	}
	if !slices.Equal(toolSchema.Required, []string{"id"}) {
		t.Errorf("diagnostics MCP required = %v, want [id]", toolSchema.Required)
	}
	for _, prop := range []string{"id", "seq", "after_seq"} {
		if _, ok := toolSchema.Properties[prop]; !ok {
			t.Errorf("diagnostics MCP schema missing property %q: %v", prop, toolSchema.Properties)
		}
	}

	mcpAll := &pb.GetCommandDiagnosticsResponse{}
	m.call(mcpAll, "diagnostics", map[string]any{"id": id})
	if len(mcpAll.Diagnostics) != 2 {
		t.Fatalf("MCP diagnostics(id) = %d items, want 2", len(mcpAll.Diagnostics))
	}
	mcpSeq1 := &pb.GetCommandDiagnosticsResponse{}
	m.call(mcpSeq1, "diagnostics", map[string]any{"id": id, "seq": 1})
	if len(mcpSeq1.Diagnostics) != 1 || mcpSeq1.Diagnostics[0].GetIntervention().GetSeq() != 1 || mcpSeq1.Diagnostics[0].GetIntervention().GetComment() != "first pause" {
		t.Fatalf("MCP diagnostics(id, seq=1) = %+v", mcpSeq1.Diagnostics)
	}
	mcpAfter1 := &pb.GetCommandDiagnosticsResponse{}
	m.call(mcpAfter1, "diagnostics", map[string]any{"id": id, "after_seq": 1})
	if len(mcpAfter1.Diagnostics) != 1 || mcpAfter1.Diagnostics[0].GetIntervention().GetSeq() != 3 || mcpAfter1.Diagnostics[0].GetIntervention().GetComment() != "final stop" {
		t.Fatalf("MCP diagnostics(id, after_seq=1) = %+v", mcpAfter1.Diagnostics)
	}
}

// followLog honors LogChunk.State and LogChunk.EndedBy on the terminal chunk so
// a quick resume right after a hand pause (leaving GetCommand at RUNNING or
// SUCCEEDED) cannot hide the pause from the waiting agent, and a hand stop
// intervention is reliably surfaced as COMMAND_STOPPED.
func TestByHandFollowTerminalChunkPreservesPauseAndStop(t *testing.T) {
	newClient := func(t *testing.T, h hotbedv1connect.WorkspaceServiceHandler) hotbedv1connect.WorkspaceServiceClient {
		t.Helper()
		cs := connect.NewServer()
		hotbedv1connect.RegisterWorkspaceServiceHandler(cs, h)
		mux := http.NewServeMux()
		connecthttp.Mount(mux, cs)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		cl, err := api.Dial(strings.TrimPrefix(srv.URL, "http://"), api.WorkerPort, srv.Client())
		if err != nil {
			t.Fatal(err)
		}
		return hotbedv1connect.NewWorkspaceServiceClient(cl)
	}

	pauseIv := &pb.CommandIntervention{
		Seq:       1,
		Kind:      pb.InterventionKind_INTERVENTION_KIND_PAUSE,
		By:        &pb.Stamp{At: 1700000000000, Session: "deft-emu", Host: "laptop"},
		Comment:   "quick pause check",
		Diagnosed: true,
	}
	resumeIv := &pb.CommandIntervention{
		Seq:  2,
		Kind: pb.InterventionKind_INTERVENTION_KIND_RESUME,
		By:   &pb.Stamp{At: 1700000000500, Session: "deft-emu", Host: "laptop"},
	}

	zero := int32(0)
	for _, tc := range []struct {
		name      string
		cmdID     string
		getResult *pb.Command
	}{
		{
			name:  "quick resume leaves GetCommand RUNNING",
			cmdID: "cmd-quick-run",
			getResult: &pb.Command{
				Id:            "cmd-quick-run",
				SessionId:     "deft-emu",
				State:         pb.CommandState_COMMAND_STATE_RUNNING,
				Interventions: []*pb.CommandIntervention{pauseIv, resumeIv},
			},
		},
		{
			name:  "quick resume and finish leaves GetCommand SUCCEEDED",
			cmdID: "cmd-quick-done",
			getResult: &pb.Command{
				Id:            "cmd-quick-done",
				SessionId:     "deft-emu",
				State:         pb.CommandState_COMMAND_STATE_SUCCEEDED,
				ExitCode:      &zero,
				Interventions: []*pb.CommandIntervention{pauseIv, resumeIv},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var followCalls atomic.Int32
			cl := newClient(t, &fakeWorkspaceService{
				followLog: func(_ context.Context, _ *pb.FollowLogRequest, stream hotbedv1connect.WorkspaceServiceFollowLogServerStream) error {
					followCalls.Add(1)
					if err := stream.Send(&pb.LogChunk{Data: []byte("start\n"), NextOffset: 6}); err != nil {
						return err
					}
					return stream.Send(&pb.LogChunk{
						NextOffset: 6,
						State:      pb.CommandState_COMMAND_STATE_PAUSED,
						EndedBy:    pauseIv,
					})
				},
				getCommand: func(_ context.Context, _ *pb.GetCommandRequest) (*pb.Command, error) {
					return tc.getResult, nil
				},
			})
			wk := workerClients{calls: cl, stream: cl, controller: cl}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			var stdout, stderr bytes.Buffer
			code, err := follow(ctx, wk, tc.cmdID, &stdout, &stderr)
			if err != nil {
				t.Fatalf("follow err = %v, want nil", err)
			}
			if code != exitPaused {
				t.Errorf("follow exit = %d, want %d (exitPaused)", code, exitPaused)
			}
			if stdout.String() != "start\n" {
				t.Errorf("stdout = %q, want %q", stdout.String(), "start\n")
			}
			wantErr := "hotbed: " + tc.cmdID + ` was paused by session deft-emu on laptop: "quick pause check". Diagnostics taken as it paused: hotbed diagnostics ` + tc.cmdID + `. It stays paused until resumed: hotbed resume -s deft-emu ` + tc.cmdID + ", or its page in the cockpit.\n"
			if stderr.String() != wantErr {
				t.Errorf("stderr = %q, want %q", stderr.String(), wantErr)
			}
			if got := followCalls.Load(); got != 1 {
				t.Errorf("FollowLog called %d times, want 1 (must not re-follow after terminal PAUSED chunk)", got)
			}
		})
	}

	t.Run("terminal CANCELLED chunk preserves hand stop intervention", func(t *testing.T) {
		stopIv := &pb.CommandIntervention{
			Seq:       1,
			Kind:      pb.InterventionKind_INTERVENTION_KIND_STOP,
			By:        &pb.Stamp{At: 1700000001000, Host: "ubu1"},
			Comment:   "hung on lock",
			Diagnosed: true,
		}
		cl := newClient(t, &fakeWorkspaceService{
			followLog: func(_ context.Context, _ *pb.FollowLogRequest, stream hotbedv1connect.WorkspaceServiceFollowLogServerStream) error {
				return stream.Send(&pb.LogChunk{
					NextOffset: 0,
					State:      pb.CommandState_COMMAND_STATE_CANCELLED,
					EndedBy:    stopIv,
				})
			},
			getCommand: func(_ context.Context, _ *pb.GetCommandRequest) (*pb.Command, error) {
				return &pb.Command{Id: "cmd-stop", SessionId: "deft-emu", State: pb.CommandState_COMMAND_STATE_CANCELLED}, nil
			},
		})
		wk := workerClients{calls: cl, stream: cl, controller: cl}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		var stdout, stderr bytes.Buffer
		_, err := follow(ctx, wk, "cmd-stop", &stdout, &stderr)
		apiErr := api.FromConnect(err)
		if apiErr == nil || apiErr.Code != api.CodeCommandStopped {
			t.Fatalf("follow err = %v, want %s", err, api.CodeCommandStopped)
		}
		if !strings.Contains(apiErr.Message, "hung on lock") {
			t.Errorf("message = %q, want comment %q", apiErr.Message, "hung on lock")
		}
		if !strings.Contains(apiErr.Suggestion, "hotbed diagnostics cmd-stop") {
			t.Errorf("suggestion = %q, want hotbed diagnostics cmd-stop", apiErr.Suggestion)
		}
	})

	t.Run("e2e pause immediately followed by quick resume still exits 124 with pause comment", func(t *testing.T) {
		c := diagnosingCluster(t)
		c.ok(nil, "session", "start", "quick-fox")
		c.workspace("quick-fox", nil)
		id, done := c.runInBackground("quick-fox", "sh", "-c", "echo start; while [ ! -e go ]; do sleep 0.05; done; echo done")

		c.ok(nil, "pause", "-s", "quick-fox", "-m", "caught quick pause", id)
		c.ok(nil, "resume", "-s", "quick-fox", id)

		r := <-done
		if r.code != exitPaused {
			t.Fatalf("attached run after pause+quick resume: exit %d, stdout %q, stderr %q; want 124", r.code, r.stdout, r.stderr)
		}
		if !strings.Contains(r.stderr, `"caught quick pause"`) {
			t.Errorf("stderr = %q, want pause comment %q", r.stderr, "caught quick pause")
		}
		c.ok(nil, "stop", "-s", "quick-fox", id)
	})
}
