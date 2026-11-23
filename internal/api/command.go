package api

import (
	"fmt"
	"strings"

	hotbedv1 "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// Live says whether a command in state s has yet to end: queued, running,
// or frozen (parked for memory, or paused).
func Live(s hotbedv1.CommandState) bool {
	switch s {
	case hotbedv1.CommandState_COMMAND_STATE_QUEUED, hotbedv1.CommandState_COMMAND_STATE_RUNNING,
		hotbedv1.CommandState_COMMAND_STATE_PARKED, hotbedv1.CommandState_COMMAND_STATE_PAUSED:
		return true
	}
	return false
}

// LastIntervention returns the most recent intervention on c matching kind,
// or the most recent intervention of any kind when kind is unspecified.
func LastIntervention(c *hotbedv1.Command, kind hotbedv1.InterventionKind) *hotbedv1.CommandIntervention {
	ivs := c.GetInterventions()
	for i := len(ivs) - 1; i >= 0; i-- {
		if kind == hotbedv1.InterventionKind_INTERVENTION_KIND_UNSPECIFIED || ivs[i].GetKind() == kind {
			return ivs[i]
		}
	}
	return nil
}

// ByHand is the stop or pause someone made of c that left it as it is:
// stopped (cancelled) or paused by them. It is nil when c is as it is for
// any other reason, such as its soft time limit or its end.
func ByHand(c *hotbedv1.Command) *hotbedv1.CommandIntervention {
	ivs := c.GetInterventions()
	if len(ivs) == 0 {
		return nil
	}
	last := ivs[len(ivs)-1]
	switch {
	case c.State == hotbedv1.CommandState_COMMAND_STATE_CANCELLED && last.GetKind() == hotbedv1.InterventionKind_INTERVENTION_KIND_STOP,
		c.State == hotbedv1.CommandState_COMMAND_STATE_PAUSED && last.GetKind() == hotbedv1.InterventionKind_INTERVENTION_KIND_PAUSE:
		return last
	}
	return nil
}

// PausedByHand reports whether c is currently paused because of a manual pause
// intervention (not its soft time limit).
func PausedByHand(c *hotbedv1.Command) bool {
	iv := ByHand(c)
	return iv != nil && iv.GetKind() == hotbedv1.InterventionKind_INTERVENTION_KIND_PAUSE
}

// StoppedByHand reports whether c ended as cancelled because of a manual stop
// intervention.
func StoppedByHand(c *hotbedv1.Command) bool {
	iv := ByHand(c)
	return iv != nil && iv.GetKind() == hotbedv1.InterventionKind_INTERVENTION_KIND_STOP
}

// InterventionNote says what someone did to c (iv, one of its interventions),
// for its reason and for whoever waits on it: who, what they wrote, how to
// read its diagnostics, and how to resume it when paused.
func InterventionNote(c *hotbedv1.Command, iv *hotbedv1.CommandIntervention) string {
	var b strings.Builder
	b.WriteString(InterventionSummary(c.GetId(), iv))
	if iv.GetKind() == hotbedv1.InterventionKind_INTERVENTION_KIND_PAUSE {
		fmt.Fprintf(&b, " It stays paused until resumed: hotbed resume%s %s, or its page in the cockpit.", sessionFlag(c), c.GetId())
	}
	return b.String()
}

// InterventionSummary formats a factual summary of iv: who acted, their
// comment, and how to fetch diagnostics (or why diagnostics were not taken).
func InterventionSummary(commandID string, iv *hotbedv1.CommandIntervention) string {
	if iv == nil {
		return ""
	}
	var b strings.Builder
	verb := map[hotbedv1.InterventionKind]string{
		hotbedv1.InterventionKind_INTERVENTION_KIND_STOP:   "stopped",
		hotbedv1.InterventionKind_INTERVENTION_KIND_PAUSE:  "paused",
		hotbedv1.InterventionKind_INTERVENTION_KIND_RESUME: "resumed",
	}[iv.Kind]
	if verb == "" {
		verb = "intervened on"
	}
	fmt.Fprintf(&b, "%s by %s", verb, Who(iv.By))
	if iv.Comment != "" {
		fmt.Fprintf(&b, ": %q", iv.Comment)
	}
	b.WriteString(".")
	if iv.Diagnosed {
		fmt.Fprintf(&b, " Diagnostics taken as it %s: hotbed diagnostics %s", verb, commandID)
		if iv.DiagnosticsError != "" {
			fmt.Fprintf(&b, " (not all: %s)", iv.DiagnosticsError)
		}
		b.WriteString(".")
	} else if iv.DiagnosticsError != "" {
		fmt.Fprintf(&b, " No diagnostics: %s.", iv.DiagnosticsError)
	}
	return b.String()
}

// Who names whoever a stamp is of: an agent by its session, a person by the
// host they came from.
func Who(by *hotbedv1.Stamp) string {
	switch {
	case by.GetSession() != "" && by.GetHost() != "":
		return fmt.Sprintf("session %s on %s", by.Session, by.Host)
	case by.GetSession() != "":
		return "session " + by.Session
	case by.GetHost() != "":
		return by.Host
	}
	return "someone"
}

func sessionFlag(c *hotbedv1.Command) string {
	if c.GetSessionId() == "" {
		return ""
	}
	return " -s " + c.SessionId
}
