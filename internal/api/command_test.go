package api

import (
	"strings"
	"testing"

	"connectrpc.com/connect/v2"

	hotbedv1 "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func TestCommandInterventionHelpers(t *testing.T) {
	pauseIV := &hotbedv1.CommandIntervention{
		Seq:       1,
		Kind:      hotbedv1.InterventionKind_INTERVENTION_KIND_PAUSE,
		By:        &hotbedv1.Stamp{At: 1000, Host: "laptop"},
		Comment:   "investigating lock contention",
		Diagnosed: true,
	}
	resumeIV := &hotbedv1.CommandIntervention{
		Seq:  2,
		Kind: hotbedv1.InterventionKind_INTERVENTION_KIND_RESUME,
		By:   &hotbedv1.Stamp{At: 2000, Session: "brave-otter", Host: "macbook"},
	}
	stopIV := &hotbedv1.CommandIntervention{
		Seq:              3,
		Kind:             hotbedv1.InterventionKind_INTERVENTION_KIND_STOP,
		By:               &hotbedv1.Stamp{At: 3000, Host: "laptop"},
		Comment:          "stuck on network",
		DiagnosticsError: "process inspection is not configured",
	}

	pausedCmd := &hotbedv1.Command{
		Id:            "cmd-1",
		SessionId:     "sess-a",
		State:         hotbedv1.CommandState_COMMAND_STATE_PAUSED,
		Interventions: []*hotbedv1.CommandIntervention{pauseIV},
	}
	if !PausedByHand(pausedCmd) {
		t.Fatal("PausedByHand(pausedCmd) = false, want true")
	}
	if StoppedByHand(pausedCmd) {
		t.Fatal("StoppedByHand(pausedCmd) = true, want false")
	}
	if got := ByHand(pausedCmd); got != pauseIV {
		t.Fatalf("ByHand(pausedCmd) = %v, want %v", got, pauseIV)
	}
	if got := LastIntervention(pausedCmd, hotbedv1.InterventionKind_INTERVENTION_KIND_PAUSE); got != pauseIV {
		t.Fatalf("LastIntervention(PAUSE) = %v, want %v", got, pauseIV)
	}

	note := InterventionNote(pausedCmd, pauseIV)
	for _, want := range []string{
		`paused by laptop: "investigating lock contention".`,
		"hotbed diagnostics cmd-1.",
		"hotbed resume -s sess-a cmd-1",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("InterventionNote = %q, missing %q", note, want)
		}
	}

	// Resumed then paused by soft limit is NOT paused by hand.
	softPausedCmd := &hotbedv1.Command{
		Id:            "cmd-1",
		State:         hotbedv1.CommandState_COMMAND_STATE_PAUSED,
		Interventions: []*hotbedv1.CommandIntervention{pauseIV, resumeIV},
	}
	if PausedByHand(softPausedCmd) {
		t.Fatal("PausedByHand(softPausedCmd) = true, want false")
	}
	if ByHand(softPausedCmd) != nil {
		t.Fatalf("ByHand(softPausedCmd) = %v, want nil", ByHand(softPausedCmd))
	}
	if got := LastIntervention(softPausedCmd, hotbedv1.InterventionKind_INTERVENTION_KIND_UNSPECIFIED); got != resumeIV {
		t.Fatalf("LastIntervention(UNSPECIFIED) = %v, want resumeIV", got)
	}

	stoppedCmd := &hotbedv1.Command{
		Id:            "cmd-1",
		State:         hotbedv1.CommandState_COMMAND_STATE_CANCELLED,
		Interventions: []*hotbedv1.CommandIntervention{pauseIV, resumeIV, stopIV},
	}
	if !StoppedByHand(stoppedCmd) {
		t.Fatal("StoppedByHand(stoppedCmd) = false, want true")
	}
	if PausedByHand(stoppedCmd) {
		t.Fatal("PausedByHand(stoppedCmd) = true, want false")
	}
	stopNote := InterventionNote(stoppedCmd, stopIV)
	for _, want := range []string{
		`stopped by laptop: "stuck on network".`,
		"No diagnostics: process inspection is not configured.",
	} {
		if !strings.Contains(stopNote, want) {
			t.Errorf("stop InterventionNote = %q, missing %q", stopNote, want)
		}
	}

	if got := Who(resumeIV.By); got != "session brave-otter on macbook" {
		t.Errorf("Who(session+host) = %q", got)
	}
	if got := Who(&hotbedv1.Stamp{Session: "brave-otter"}); got != "session brave-otter" {
		t.Errorf("Who(session) = %q", got)
	}
	if got := Who(&hotbedv1.Stamp{Host: "ubu1", ActorKind: hotbedv1.ActorKind_ACTOR_KIND_NODE}); got != "the node ubu1" {
		t.Errorf("Who(node) = %q", got)
	}
	if got := Who(nil); got != "someone" {
		t.Errorf("Who(nil) = %q", got)
	}

	// CodeCommandStopped maps to Connect CodeAborted and round-trips through ToConnect/FromConnect.
	err := ToConnect(Errorf(CodeCommandStopped, "stopped"))
	if connect.CodeOf(err) != connect.CodeAborted {
		t.Errorf("connect.CodeOf(CodeCommandStopped) = %v, want Aborted", connect.CodeOf(err))
	}
	if CodeOf(err) != CodeCommandStopped {
		t.Errorf("CodeOf = %q, want %q", CodeOf(err), CodeCommandStopped)
	}
}
