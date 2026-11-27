package worker

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"regexp"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/cmdlog"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/outq"
)

// Bounds on how long a query of output waits for --until or --wait.
const (
	defaultOutputWait = 60 * time.Second
	maxOutputWait     = 10 * time.Minute
	// outputPoll is how often a waiting query reads what the output gained:
	// output growing is no change of the engine's.
	outputPoll = 200 * time.Millisecond
)

// Why a waiting query of output stopped waiting.
const (
	waitedMatched = "matched"
	waitedEnded   = "ended"
	waitedPaused  = "paused"
	waitedTimeout = "timeout"
)

// QueryOutput answers req of command req.Id's output (internal/outq),
// waiting first, with until or wait, for a matching line or its end.
func (e *Engine) QueryOutput(ctx context.Context, req *pb.QueryOutputRequest) (*pb.OutputQuery, error) {
	if err := validateID(req.Id); err != nil {
		return nil, err
	}
	if req.Timestamps {
		return nil, api.Errorf(api.CodeInvalidArgs, "timestamps is a client-only formatting option")
	}
	q, until, err := outputQuery(req)
	if err != nil {
		return nil, err
	}
	timeout := defaultOutputWait
	if req.TimeoutSeconds < 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "--timeout %ds may not be negative", req.TimeoutSeconds)
	}
	if req.TimeoutSeconds > 0 {
		timeout = min(time.Duration(req.TimeoutSeconds)*time.Second, maxOutputWait)
	}
	path, done, err := e.Follow(req.Id)
	if err != nil {
		return nil, err
	}
	defer done()
	after := req.After
	if req.SinceSync {
		q.SinceOffset = e.syncedAt(req.Id)
		if until != nil {
			// Waiting for a line since the sync: lines before it are past.
			line, total, err := outq.LineAt(fileSource(path), q.SinceOffset, req.Raw)
			if err != nil {
				return nil, err
			}
			if line == 0 {
				after = max(after, total)
			} else {
				after = max(after, line-1)
			}
		}
	}
	var waited string
	if until != nil || req.Wait {
		if waited, err = e.waitOutput(ctx, req.Id, path, until, after, req.Raw, timeout); err != nil {
			return nil, err
		}
	}
	if waited == waitedMatched && q.Grep == nil {
		// What it waited for, marked, with what came around it.
		q.Grep = until
	}
	ans, err := outq.RunTimed(timedFileSource(path), q)
	if err != nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "%v", err)
	}
	out := &pb.OutputQuery{
		CommandId: req.Id,
		Lines:     ans.Lines,
		Omitted:   ans.Omitted,
		Waited:    waited,
	}
	if q.Grep != nil {
		out.Matches = &ans.Matches
	}
	out.TotalLines = ans.Total
	// Read on after the lines that can no longer change: a row still being
	// drawn may yet end as the line waited for.
	out.NextLine = ans.Final
	if st, err := os.Stat(path); err == nil {
		out.TotalBytes = st.Size()
	}
	if d, err := cmdlog.ReadDropped(path); err == nil {
		out.DroppedBytes = d.Bytes()
	}
	if cmd, ok := e.Command(req.Id); ok {
		out.State, out.ExitCode = cmd.State, cmd.ExitCode
		if !api.Live(cmd.State) {
			out.NextLine = ans.Total
		}
	} else {
		out.NextLine = ans.Total // forgotten: long ended
	}
	return out, nil
}

// outputQuery is req as an outq.Query, and the pattern it waits until.
func outputQuery(req *pb.QueryOutputRequest) (outq.Query, *regexp.Regexp, error) {
	compile := func(flag, pat string) (*regexp.Regexp, error) {
		if pat == "" {
			return nil, nil
		}
		if req.IgnoreCase {
			pat = "(?i)" + pat
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, api.Errorf(api.CodeInvalidArgs, "--%s %q is not a regular expression: %v", flag, pat, err).
				WithSuggestion("it takes RE2 syntax (https://github.com/google/re2/wiki/Syntax)")
		}
		return re, nil
	}
	grep, err := compile("grep", req.Grep)
	if err != nil {
		return outq.Query{}, nil, err
	}
	until, err := compile("until", req.Until)
	if err != nil {
		return outq.Query{}, nil, err
	}
	q := outq.Query{
		Grep: grep, Context: int(req.Context),
		Head: int(req.Head), Tail: int(req.Tail),
		From: req.From, To: req.To, After: req.After,
		SinceOffset: -1,
		Raw:         req.Raw,
		Max:         int(req.MaxLines),
	}
	if err := q.Check(); err != nil {
		return outq.Query{}, nil, api.Errorf(api.CodeInvalidArgs, "%v", err)
	}
	return q, until, nil
}

// waitOutput waits, at most timeout, until a line of command id's output
// (at path) past after matches until (when given), or the command ends or
// pauses, and says which.
func (e *Engine) waitOutput(ctx context.Context, id, path string, until *regexp.Regexp, after int64, raw bool, timeout time.Duration) (string, error) {
	// A command not yet started has no log: it reads as empty until it does.
	var f *os.File
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	var w *outq.Watcher
	var buf []byte
	if until != nil {
		w = outq.NewWatcher(until, after, raw)
		buf = make([]byte, 256<<10)
	}
	var off int64
	// readAll feeds the watcher what the output gained, and says whether a
	// line matched.
	readAll := func() (bool, error) {
		if w == nil {
			return false, nil
		}
		if f == nil {
			var err error
			if f, err = os.Open(path); errors.Is(err, fs.ErrNotExist) {
				return false, nil
			} else if err != nil {
				return false, err
			}
		}
		for {
			data, next, err := cmdlog.ReadChunk(f, path, off, buf)
			if err != nil {
				return false, err
			}
			if len(data) == 0 {
				return w.Match() > 0, nil
			}
			w.Write(data)
			off = next
		}
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		// Whether it goes on is read before its output, so output written
		// just before it ended is read too.
		going, changed := e.Going(id)
		matched, err := readAll()
		if err != nil {
			return "", err
		}
		switch {
		case matched:
			return waitedMatched, nil
		case !going:
			if cmd, ok := e.Command(id); ok && cmd.State == paused {
				return waitedPaused, nil
			}
			return waitedEnded, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-e.streamsDone:
			return "", &api.Error{Code: api.CodeWorkerUnreachable, Message: "worker is shutting down", Retryable: true}
		case <-deadline.C:
			return waitedTimeout, nil
		case <-changed:
		case <-time.After(outputPoll):
		}
	}
}

// timedFileSource reads the log at path from its start with per-chunk received
// timestamps from <path>.ts; dropped-byte markers carry timestamp 0.
func timedFileSource(path string) outq.TimedSource {
	return func(fn func(off int64, chunk []byte, ts int64) error) error {
		f, err := os.Open(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil // not started: no output yet
		}
		if err != nil {
			return err
		}
		defer f.Close()
		tsIdx, _ := cmdlog.ReadTimestamps(path)
		buf := make([]byte, 256<<10)
		var off int64
		for {
			data, next, err := cmdlog.ReadChunk(f, path, off, buf)
			if err != nil {
				return err
			}
			if len(data) == 0 {
				return nil
			}
			d, _ := cmdlog.ReadDropped(path)
			if d.Bytes() > 0 && off >= d.From && off < d.To {
				if err := fn(off, data, 0); err != nil {
					return err
				}
			} else if err := tsIdx.SplitChunks(off, data, fn); err != nil {
				return err
			}
			off = next
		}
	}
}

// fileSource reads the log at path from its start, as cmdlog.ReadChunk shows
// it: dropped bytes as their marker.
func fileSource(path string) outq.Source {
	return func(fn func(off int64, chunk []byte) error) error {
		f, err := os.Open(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil // not started: no output yet
		}
		if err != nil {
			return err
		}
		defer f.Close()
		buf := make([]byte, 256<<10)
		var off int64
		for {
			data, next, err := cmdlog.ReadChunk(f, path, off, buf)
			if err != nil {
				return err
			}
			if len(data) == 0 {
				return nil
			}
			if err := fn(off, data); err != nil {
				return err
			}
			off = next
		}
	}
}

// syncedAt is the byte of command id's log written when its workspace last
// synced while it ran: what it wrote since an edit reached it. 0, the
// start, for one that has seen no sync since it started.
func (e *Engine) syncedAt(id string) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c, ok := e.commands[id]; ok {
		return c.rec.SyncedAtOffset
	}
	return 0
}

// markSynced notes, for each live command in workspace w, where its log
// stood as w synced. Call with e.mu held.
func (e *Engine) markSyncedLocked(wsID string) {
	for _, c := range e.commands {
		if c.cmd().WorkspaceId != wsID || !started(c.cmd().State) {
			continue
		}
		path, err := e.logPath(c.cmd().Id)
		if err != nil {
			continue
		}
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		c.rec.SyncedAtOffset = st.Size()
		e.saveCommand(c, change{what: "synced"})
	}
}

// Detach lets attached command id run on with no one following it, as a
// detached command does: left to its hard time limit, its soft one gone
// (a soft limit pauses a command that a waiting agent may have forgotten;
// a detached one no agent waits on). hotbed run detaches a command that
// outlasts its --wait.
func (e *Engine) Detach(id string) (*pb.Command, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.commands[id]
	if !ok {
		if err := validateID(id); err != nil {
			return nil, err
		}
		return nil, notFound("command", id)
	}
	cmd := c.cmd()
	switch {
	case !api.Live(cmd.State):
		return nil, api.Errorf(api.CodeConflict, "command %s has ended: there is nothing to detach", id)
	case cmd.State == paused:
		return nil, api.Errorf(api.CodeConflict, "command %s is paused: %s", id, cmd.Reason).
			WithSuggestion("resume it (hotbed resume), which leaves it running detached")
	case !cmd.Attached:
		return clone(cmd), nil
	}
	cmd.Attached = false
	if cmd.Limits != nil {
		cmd.Limits.SoftLimitSeconds = 0
	}
	const detail = "runs on with no one following it"
	cmd.Timeline = append(cmd.Timeline, &pb.CommandEvent{At: e.nowMs(), What: "detached", Detail: detail})
	e.saveCommand(c, change{what: "detached", detail: detail})
	return clone(cmd), nil
}

// SendInput writes data to the terminal of command id, run in one.
func (e *Engine) SendInput(ctx context.Context, id string, data []byte) error {
	e.mu.Lock()
	c, ok := e.commands[id]
	if !ok {
		e.mu.Unlock()
		if err := validateID(id); err != nil {
			return err
		}
		return notFound("command", id)
	}
	cmd, target := c.cmd(), c.runtimeTarget()
	state, input := cmd.State, cmd.TtyInput
	e.mu.Unlock()
	switch {
	case !input:
		return api.Errorf(api.CodeConflict, "command %s does not read its input from a terminal: it reads no input", id).
			WithSuggestion("run it with one: hotbed run --detach --tty")
	case state != running:
		return api.Errorf(api.CodeConflict, "command %s is %s, not running: nothing reads its input", id, stateName(state))
	case e.cfg.Runtime == nil:
		return api.Errorf(api.CodeConflict, "command %s runs on the host: only a VM's commands have a terminal", id)
	}
	return e.cfg.Runtime.SendInput(ctx, target, id, data)
}
