package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// Intervention is someone's stop, pause or resume of a command by hand, as
// their call asks it, or the node's own (ByNode).
type Intervention struct {
	Comment string
	// Diagnose takes the command's diagnostics first (a stop or pause).
	Diagnose bool
	// Session is the agent's session that asks it; empty for a person.
	Session string
	// Host is where the call came from.
	Host string
	// ByNode is the node's own, by a rule of its own (Host names it), not
	// a person's or an agent's.
	ByNode bool
}

// How long the activity sample taken of a command stopped or paused by hand
// watches it, how long its diagnostics may take in all, and size bounds on
// intervention comments, history, and diagnostics payloads.
const (
	diagnoseSampleSeconds       = 2
	diagnoseTimeout             = time.Minute
	maxInterventionCommentBytes = 2048
	maxTimelineCommentBytes     = 200
	maxCommandInterventions     = 32
	maxDiagnosticsBytes         = 1 << 20
)

// byHand is a stop, pause or resume someone makes of a command, and the
// diagnostics taken of it first, if any. A nil one is none: the engine's own
// doing (a time limit, a client gone).
type byHand struct {
	iv   *pb.CommandIntervention
	diag *pb.CommandDiagnostics
}

// record adds the intervention to cmd's, numbered monotonically after those
// before it, caps retained interventions so replicated commands stay bounded,
// and syncs the assigned Seq onto any captured diagnostics.
func (h *byHand) record(cmd *pb.Command) {
	if h == nil {
		return
	}
	var nextSeq int32 = 1
	if n := len(cmd.Interventions); n > 0 {
		nextSeq = cmd.Interventions[n-1].GetSeq() + 1
	}
	h.iv.Seq = nextSeq
	cmd.Interventions = append(cmd.Interventions, h.iv)
	if len(cmd.Interventions) > maxCommandInterventions {
		cmd.Interventions = append([]*pb.CommandIntervention(nil), cmd.Interventions[len(cmd.Interventions)-maxCommandInterventions:]...)
	}
	if h.diag != nil {
		h.diag.Intervention = clone(h.iv)
	}
}

func (h *byHand) diagnostics() *pb.CommandDiagnostics {
	if h == nil {
		return nil
	}
	return h.diag
}

// truncateBytesUTF8 shortens s to at most maxBytes bytes without splitting a
// multi-byte UTF-8 rune, appending "..." when truncated.
func truncateBytesUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	if maxBytes <= 3 {
		return s[:maxBytes]
	}
	cut := maxBytes - 3
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// conciseInterventionNote formats an intervention note with the comment
// preview capped to maxTimelineCommentBytes so timeline entries stay concise
// while CommandIntervention.Comment preserves up to maxInterventionCommentBytes.
func conciseInterventionNote(cmd *pb.Command, iv *pb.CommandIntervention) string {
	if iv == nil {
		return ""
	}
	if len(iv.Comment) <= maxTimelineCommentBytes {
		return api.InterventionNote(cmd, iv)
	}
	short := clone(iv)
	short.Comment = truncateBytesUTF8(iv.Comment, maxTimelineCommentBytes)
	return api.InterventionNote(cmd, short)
}

// StopWith is someone's stop of command id: its diagnostics are taken first if
// asked, and whoever waits on it is told who stopped it, what they wrote,
// and how to read them. A command that has ended is returned as it ended.
func (e *Engine) StopWith(ctx context.Context, id string, in Intervention) (*pb.Command, error) {
	// The stop goes on once asked, whoever asked it gone or not.
	ctx = context.WithoutCancel(ctx)
	h, done, err := e.intervene(ctx, id, pb.InterventionKind_INTERVENTION_KIND_STOP, in)
	if err != nil {
		return nil, err
	}
	if h == nil {
		return e.current(id)
	}
	defer done()
	return e.stop(ctx, id, api.InterventionNote(h.cmd, h.iv), &h.byHand)
}

// Pause is someone's pause of command id: frozen, its memory to swap, as
// its soft time limit would, after its diagnostics if asked; whoever waits on
// it is told so. It stays paused until someone resumes it (Resume), or its
// hard time limit stops it.
func (e *Engine) Pause(ctx context.Context, id string, in Intervention) (*pb.Command, error) {
	ctx = context.WithoutCancel(ctx)
	h, done, err := e.intervene(ctx, id, pb.InterventionKind_INTERVENTION_KIND_PAUSE, in)
	if err != nil {
		return nil, err
	}
	if h == nil {
		return nil, api.Errorf(api.CodeConflict, "command %s has ended: there is nothing to pause", id)
	}
	defer done()
	c, note := h.c, api.InterventionNote(h.cmd, h.iv)
	tlNote := conciseInterventionNote(h.cmd, h.iv)
	pausedOK, err := e.pauseUnit(ctx, c, func(was pb.CommandState) bool {
		return !c.stopping && c.cmd().State == was && (was == running || was == parked || was == paused)
	}, func() {
		cmd := c.cmd()
		h.byHand.record(cmd)
		cmd.State = paused
		cmd.CpuCores = 0
		cmd.Reason = note
		c.rec.HeldFor = ""
		cmd.Timeline = append(cmd.Timeline, &pb.CommandEvent{At: e.nowMs(), What: "paused", Detail: tlNote})
		e.saveCommand(c, change{
			what: "paused", detail: tlNote, out: outbox{diagnostics: h.diag},
			feed: feed(feedPause, fmt.Sprintf("pause %s · by %s", cmd.Argv, api.Who(h.iv.By)), ref("command", cmd.Id)),
		})
	})
	if err != nil {
		return nil, fmt.Errorf("pause %s: %w", id, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !pausedOK {
		return nil, api.Errorf(api.CodeConflict, "command %s became %s as it was paused", id, stateName(c.cmd().State)).
			WithSuggestion("look at it again (hotbed ps), and pause it again if it still runs")
	}
	return clone(c.cmd()), nil
}

// Resume is someone's resume of command id, paused by hand or by its soft
// time limit. It resets its soft limit timer from now and leaves it to its
// limits, as a detached command is. It runs again at once, swapping back in
// what it must, unless a measurement keeps its volume quiet: then it stays
// frozen, held beside the measurement, and is thawed once that ends.
func (e *Engine) Resume(ctx context.Context, id string, in Intervention) (*pb.Command, error) {
	ctx = context.WithoutCancel(ctx)
	in.Diagnose = false
	h, done, err := e.intervene(ctx, id, pb.InterventionKind_INTERVENTION_KIND_RESUME, in)
	if err != nil {
		return nil, err
	}
	if h == nil {
		return nil, api.Errorf(api.CodeConflict, "command %s has ended: there is nothing to resume", id)
	}
	defer done()
	tlNote := conciseInterventionNote(h.cmd, h.iv)
	h.c.freezeMu.Lock()
	defer h.c.freezeMu.Unlock()
	e.mu.Lock()
	cmd := h.c.cmd()
	if h.c.stopping || cmd.State != paused {
		st := cmd.State
		e.mu.Unlock()
		return nil, api.Errorf(api.CodeConflict, "command %s became %s as it was resumed", id, stateName(st))
	}
	if blockReason, heldFor := e.resumeHold(h.c); heldFor != "" {
		now := e.nowMs()
		h.byHand.record(cmd)
		cmd.Attached = false
		h.c.rec.WaitedAt = now
		cmd.State = parked
		cmd.CpuCores = 0
		h.c.rec.ParkedAt = now
		h.c.rec.HeldFor = heldFor
		cmd.Reason = fmt.Sprintf("resumed; held: %s", blockReason)
		detail := cmd.Reason
		if tlNote != "" {
			detail = tlNote + " (held: " + blockReason + ")"
		}
		cmd.Timeline = append(cmd.Timeline, &pb.CommandEvent{At: now, What: "held", Detail: detail})
		e.saveCommand(h.c, change{
			what: "held", detail: detail,
			feed: feed(feedResume, fmt.Sprintf("resume %s (held) · by %s", cmd.Argv, api.Who(h.iv.By)), ref("command", cmd.Id)),
		})
		e.stepSoon()
		out := clone(cmd)
		e.mu.Unlock()
		return out, nil
	}
	if err := e.thawLockedBy(ctx, h.c, change{
		what: "resumed", detail: tlNote,
		feed: feed(feedResume, fmt.Sprintf("resume %s · by %s", cmd.Argv, api.Who(h.iv.By)), ref("command", cmd.Id)),
	}, &h.byHand, false); err != nil {
		return nil, fmt.Errorf("resume %s: %w", id, err)
	}
	return e.current(id)
}

// resumeHold says why paused command c must stay frozen as it is resumed,
// and the measurement it is held beside: one whose volume a running
// measurement keeps quiet. It returns "" for both when c may run now. Call
// with e.mu held.
func (e *Engine) resumeHold(c *command) (reason, heldFor string) {
	if c.cmd().Bench {
		return "", ""
	}
	if r := e.quietBlocker(c); r != "" {
		return r, e.measuring().cmd().Id
	}
	return "", ""
}

// intervention is one under way: the command, a copy of it as it was when
// it began, and what is done to it.
type intervention struct {
	byHand
	c   *command
	cmd *pb.Command
}

// intervene begins someone's stop, pause or resume (kind) of command id:
// it checks that the command can take it, marks it as theirs until done is
// called, and takes its diagnostics when asked. It returns a nil
// intervention for a command that has ended.
func (e *Engine) intervene(ctx context.Context, id string, kind pb.InterventionKind, in Intervention) (*intervention, func(), error) {
	if len(in.Comment) > maxInterventionCommentBytes {
		return nil, nil, api.Errorf(api.CodeInvalidArgs, "comment is too long (%d bytes; maximum is %d bytes)", len(in.Comment), maxInterventionCommentBytes)
	}
	var diagCtx context.Context
	var cancelDiag context.CancelFunc
	if in.Diagnose {
		diagCtx, cancelDiag = context.WithTimeout(ctx, diagnoseTimeout)
	}
	e.mu.Lock()
	c, ok := e.commands[id]
	if !ok {
		e.mu.Unlock()
		if cancelDiag != nil {
			cancelDiag()
		}
		if err := validateID(id); err != nil {
			return nil, nil, err
		}
		return nil, nil, notFound("command", id)
	}
	cmd := c.cmd()
	if !api.Live(cmd.State) {
		e.mu.Unlock()
		if cancelDiag != nil {
			cancelDiag()
		}
		return nil, nil, nil
	}
	if err := canTake(cmd, kind); err != nil {
		e.mu.Unlock()
		if cancelDiag != nil {
			cancelDiag()
		}
		return nil, nil, err
	}
	if c.intervening || c.stopping {
		e.mu.Unlock()
		if cancelDiag != nil {
			cancelDiag()
		}
		return nil, nil, api.Errorf(api.CodeConflict, "command %s is being stopped, paused or resumed already", id).
			WithSuggestion("wait for that to finish, then look at it again")
	}
	c.intervening = true
	c.cancelDiag = cancelDiag
	h := &intervention{c: c, cmd: clone(cmd), byHand: byHand{iv: &pb.CommandIntervention{
		Kind: kind, Comment: in.Comment,
		By: &pb.Stamp{At: e.nowMs(), Session: in.Session, Host: in.Host},
	}}}
	if in.ByNode {
		h.iv.By.ActorKind = pb.ActorKind_ACTOR_KIND_NODE
	}
	e.mu.Unlock()
	done := func() {
		e.mu.Lock()
		c.intervening = false
		c.cancelDiag = nil
		e.mu.Unlock()
		if cancelDiag != nil {
			cancelDiag()
		}
	}
	if in.Diagnose {
		h.diag = e.diagnose(diagCtx, h.cmd, h.iv)
		e.mu.Lock()
		c.cancelDiag = nil
		e.mu.Unlock()
		cancelDiag()
	}
	return h, done, nil
}

// canTake says why a live command cannot be stopped, paused or resumed
// (kind) by hand, if it cannot.
func canTake(cmd *pb.Command, kind pb.InterventionKind) error {
	switch kind {
	case pb.InterventionKind_INTERVENTION_KIND_PAUSE:
		switch {
		case cmd.State == queued:
			return api.Errorf(api.CodeConflict, "command %s has not started: there is nothing to pause", cmd.Id).
				WithSuggestion("stop it instead (hotbed stop)")
		case cmd.Bench:
			return api.Errorf(api.CodeConflict, "command %s is a measurement: pausing it would spoil its numbers", cmd.Id).
				WithSuggestion("stop it instead (hotbed stop)")
		case api.PausedByHand(cmd):
			return api.Errorf(api.CodeConflict, "command %s is paused already: %s", cmd.Id, cmd.Reason)
		}
	case pb.InterventionKind_INTERVENTION_KIND_RESUME:
		if cmd.State != paused {
			return api.Errorf(api.CodeConflict, "command %s is %s, not paused: there is nothing to resume", cmd.Id, stateName(cmd.State))
		}
	}
	return nil
}

// diagnose takes what command cmd is doing now, for iv, someone's stop or
// pause of it: its activity sample and its full inspection. iv says what
// could not be taken and why; the stop or pause goes on regardless. It
// returns nil when nothing could be taken or the bounded payload still exceeds
// maxDiagnosticsBytes.
func (e *Engine) diagnose(ctx context.Context, cmd *pb.Command, iv *pb.CommandIntervention) *pb.CommandDiagnostics {
	if cmd.State == queued {
		iv.DiagnosticsError = "it had not started: it had no processes to diagnose"
		return nil
	}
	route, err := e.resolveLiveCommand(cmd.Id)
	if err != nil {
		iv.DiagnosticsError = message(err)
		return nil
	}
	if !route.isVM && e.cfg.Inspector == nil {
		iv.DiagnosticsError = "this worker takes no diagnostics: process inspection is not configured on it"
		return nil
	}
	d := &pb.CommandDiagnostics{CommandId: cmd.Id, Intervention: iv}
	var failed []string
	sampleReq := &pb.SampleCommandRequest{Id: cmd.Id, DurationSeconds: diagnoseSampleSeconds}
	inspectReq := &pb.InspectCommandRequest{Id: cmd.Id}
	onSample := func(ch *pb.SampleCommandChunk) error {
		if ch.Sample != nil {
			d.Sample = ch.Sample
		}
		return nil
	}
	if route.isVM {
		if err := e.cfg.Runtime.SampleCommand(ctx, route.wsID, sampleReq, e.wrapVMSampleEmit(route.wsID, onSample)); err != nil {
			failed = append(failed, "its activity sample: "+message(err))
		}
		if ctx.Err() != nil {
			failed = append(failed, "its inspection: "+message(ctx.Err()))
		} else if d.Inspect, err = e.cfg.Runtime.InspectCommand(ctx, route.wsID, inspectReq); err != nil {
			failed = append(failed, "its inspection: "+message(err))
		}
	} else {
		target := e.hostInspectTarget(ctx, cmd.Id, route)
		if err := e.cfg.Inspector.Sample(ctx, target, sampleReq, onSample); err != nil {
			failed = append(failed, "its activity sample: "+message(err))
		}
		if ctx.Err() != nil {
			failed = append(failed, "its inspection: "+message(ctx.Err()))
		} else if d.Inspect, err = e.cfg.Inspector.Inspect(ctx, target, inspectReq); err != nil {
			failed = append(failed, "its inspection: "+message(err))
		}
	}
	iv.DiagnosticsError = strings.Join(failed, "; ")
	if d.Sample == nil && d.Inspect == nil {
		if iv.DiagnosticsError == "" {
			iv.DiagnosticsError = "its sample and inspection returned nothing"
		}
		return nil
	}
	bounded, ok := boundDiagnostics(d, iv)
	if !ok {
		iv.Diagnosed = false
		return nil
	}
	iv.Diagnosed = true
	return bounded
}

// boundDiagnostics ensures d encodes to at most maxDiagnosticsBytes (1 MiB) by
// progressively trimming verbose sections (trimToFit, then the inspection or
// the sample whole) and recording a truncation note in iv.DiagnosticsError.
// If the payload still exceeds maxDiagnosticsBytes after trimming, it
// records an error in iv.DiagnosticsError and returns (nil, false).
func boundDiagnostics(d *pb.CommandDiagnostics, iv *pb.CommandIntervention) (*pb.CommandDiagnostics, bool) {
	if d == nil {
		return nil, false
	}
	// Reserve headroom for seq varint and any truncation note added to iv.
	const headroom = 512
	budget := maxDiagnosticsBytes - headroom
	if proto.Size(d) <= budget {
		return d, true
	}
	d = proto.Clone(d).(*pb.CommandDiagnostics)
	d.Intervention = iv
	appendErr := func(note string) {
		if iv.DiagnosticsError == "" {
			iv.DiagnosticsError = note
		} else {
			iv.DiagnosticsError += "; " + note
		}
	}
	fits := func() bool { return proto.Size(d) <= budget }
	trimmed := trimToFit(fits, d.Sample, d.Inspect)
	if !fits() && d.Inspect != nil && d.Sample != nil {
		origInspect := d.Inspect
		d.Inspect = nil
		if fits() {
			trimmed = append(trimmed, "inspection")
		} else {
			d.Inspect = origInspect
			d.Sample = nil
			if fits() {
				trimmed = append(trimmed, "activity sample")
			} else {
				d.Inspect = nil
				trimmed = append(trimmed, "inspection", "activity sample")
			}
		}
	}
	if len(trimmed) > 0 {
		appendErr("diagnostics truncated to fit 1 MiB limit (dropped " + strings.Join(trimmed, ", ") + ")")
	}
	if (d.Sample == nil && d.Inspect == nil) || proto.Size(d) > maxDiagnosticsBytes {
		appendErr(fmt.Sprintf("diagnostics exceeded %d byte limit even after truncation (%d bytes)", maxDiagnosticsBytes, proto.Size(d)))
		return nil, false
	}
	return d, true
}

// trimToFit drops the bulkiest parts of sample and inspect (either may be
// nil), a tier at a time: memory maps, files, kernel stacks, user stacks,
// then threads and processes, until fits says what holds them is small
// enough. It returns the tiers it dropped.
func trimToFit(fits func() bool, sample *pb.ProcessSample, inspect *pb.InspectCommandResponse) []string {
	var trimmed []string
	tier := func(name string, drop func() bool) {
		if !fits() && drop() {
			trimmed = append(trimmed, name)
		}
	}
	tier("maps", func() bool {
		dropped := false
		if inspect != nil && len(inspect.Maps) > 0 {
			inspect.Maps = nil
			dropped = true
		}
		if sample != nil {
			for _, p := range sample.Procs {
				if p.MemoryMap != nil {
					p.MemoryMap = nil
					dropped = true
				}
			}
		}
		return dropped
	})
	tier("files", func() bool {
		dropped := false
		if inspect != nil && len(inspect.Files) > 0 {
			inspect.Files = nil
			dropped = true
		}
		if sample != nil {
			if len(sample.Files) > 0 {
				sample.Files = nil
				dropped = true
			}
			if len(sample.Locks) > 0 {
				sample.Locks = nil
				dropped = true
			}
		}
		return dropped
	})
	tier("kstacks", func() bool {
		dropped := false
		if inspect != nil && len(inspect.Kstacks) > 0 {
			inspect.Kstacks = nil
			dropped = true
		}
		if sample != nil {
			for _, p := range sample.Procs {
				for _, th := range p.Threads {
					if len(th.KernelFrames) > 0 {
						th.KernelFrames = nil
						dropped = true
					}
				}
			}
		}
		return dropped
	})
	tier("stacks", func() bool {
		if sample == nil {
			return false
		}
		dropped := len(sample.SampledStacks) > 0 || len(sample.OnCpuStacks) > 0 || len(sample.OnCpuTopFunctions) > 0 || len(sample.Frames) > 0
		sample.SampledStacks, sample.OnCpuStacks, sample.OnCpuTopFunctions, sample.Frames = nil, nil, nil, nil
		for _, p := range sample.Procs {
			for _, th := range p.Threads {
				if len(th.UserFrames) > 0 {
					th.UserFrames = nil
					dropped = true
				}
			}
		}
		return dropped
	})
	tier("processes", func() bool {
		dropped := false
		if sample != nil && len(sample.Procs) > 0 {
			for _, p := range sample.Procs {
				if len(p.Threads) > 0 {
					p.Threads = nil
					dropped = true
				}
			}
			if !fits() {
				sample.Procs = nil
				dropped = true
			}
		}
		if !fits() && inspect != nil {
			if len(inspect.Limits) > 0 {
				inspect.Limits = nil
				dropped = true
			}
			if len(inspect.AvailableTools) > 0 {
				inspect.AvailableTools = nil
				dropped = true
			}
			if !fits() && len(inspect.Procs) > 0 {
				inspect.Procs = nil
				dropped = true
			}
		}
		return dropped
	})
	return trimmed
}

// current returns command id as it is.
func (e *Engine) current(id string) (*pb.Command, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.commands[id]
	if !ok {
		return nil, notFound("command", id)
	}
	return clone(c.cmd()), nil
}

// message is what err says, without the code an *api.Error has.
func message(err error) string {
	var e *api.Error
	if errors.As(err, &e) {
		return e.Message
	}
	return err.Error()
}

// stateName is a command state as people say it: running, paused.
func stateName(s pb.CommandState) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "COMMAND_STATE_"))
}
