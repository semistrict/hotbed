package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/shell"
)

// What hotbed run does by default: a run is for quick work. One that has not
// ended after defaultRunWait is left running, detached, and read with hotbed
// output; past defaultMaxOutput, its output stops printing as it runs (all
// of it is kept), and its last lines print as it ends.
const (
	defaultRunWait   = 2 * time.Minute
	defaultMaxOutput = "32K"
	// endTailLines is how many of its last lines a run prints as it ends or
	// is detached, when its output did not all print as it ran.
	endTailLines = 40
	// exitDetached is how hotbed run exits when it left the command running,
	// detached, after --wait.
	exitDetached = 125
)

// runWait is how long an attached run waits on its command before
// detaching it: --wait, or defaultRunWait; 0 for until it ends, as with
// --soft-limit alone, which bounds the wait itself (the command pauses,
// and the run exits 124, as before).
func (c *runCmd) runWait() (time.Duration, error) {
	if c.Detach {
		if c.Wait != "" {
			return 0, api.Errorf(api.CodeInvalidArgs, "--wait is how long an attached run waits: a detached one does not wait").
				WithSuggestion("drop --wait, or --detach")
		}
		return 0, nil
	}
	if c.Wait == "" && c.SoftLimit != "" {
		return 0, nil
	}
	return parseLimit("--wait", c.Wait, defaultRunWait)
}

// ttyMode is the terminal --tty or --no-tty asks for.
func (c *runCmd) ttyMode() pb.TtyMode {
	switch {
	case c.TTY:
		return pb.TtyMode_TTY_MODE_ON
	case c.NoTTY:
		return pb.TtyMode_TTY_MODE_OFF
	}
	return pb.TtyMode_TTY_MODE_DEFAULT
}

// estimate is what the run is expected to need: as --mem and --expect
// give it, or, for a detached run that gives neither, what the newest
// earlier run of the same command line in the project held at its peak and
// took. A detached run with neither is refused: a long job says what it
// needs, so the node reserves it.
func (c *runCmd) estimate(ctx context.Context, d deps, project, repo string, args []string) (*pb.CommandEstimate, error) {
	var est pb.CommandEstimate
	if c.Mem != "" {
		mem, err := parseSize(c.Mem)
		if err != nil {
			return nil, err
		}
		est.MemoryBytes = mem
	}
	if c.Expect != "" {
		d, err := parseLimit("--expect", c.Expect, 0)
		if err != nil {
			return nil, err
		}
		est.Seconds = int64(d / time.Second)
	}
	if est.MemoryBytes > 0 || est.Seconds > 0 {
		est.Source = "given"
		return &est, nil
	}
	if !c.Detach {
		return nil, nil
	}
	argv := shell.Join(args)
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	limit := int32(50)
	res, err := cl.workspaces.ListCommands(ctx, &pb.ListCommandsRequest{Project: project, Ended: true, Match: argv, Limit: &limit})
	if err != nil {
		return nil, err
	}
	for i := len(res.Commands) - 1; i >= 0; i-- {
		cmd := res.Commands[i]
		if cmd.Argv != argv || cmd.Repo != repo || cmd.MemoryBytes <= 0 || cmd.EndedAt == nil || cmd.StartedAt == nil {
			continue
		}
		return &pb.CommandEstimate{
			MemoryBytes: cmd.MemoryBytes,
			Seconds:     (*cmd.EndedAt - *cmd.StartedAt) / 1000,
			Source:      "history",
		}, nil
	}
	return nil, api.Errorf(api.CodeInvalidArgs, "a detached run needs --mem: the memory it holds at its peak, which the node reserves for it; no earlier run of %s in project %s says", argv, project).
		WithSuggestion("pass --mem 4G (and --expect 20m), or run it attached: hotbed run waits on it, and detaches it after --wait")
}

// attachFor is attach, waiting at most wait from the command's start (0
// for until it ends): time queued is the node's, not the command's. A
// command that outlasts it is left running, detached, and detached says so.
func attachFor(ctx context.Context, wk workerClients, id, session string, wait time.Duration, w, stderr io.Writer) (code int, detached bool, err error) {
	if wait <= 0 {
		code, err = attach(ctx, wk, id, w, stderr)
		return code, false, err
	}
	fctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var waited atomic.Bool
	go func() {
		for {
			if c, err := wk.command(fctx, id); err == nil && c.State != pb.CommandState_COMMAND_STATE_QUEUED {
				break
			}
			select {
			case <-fctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
		select {
		case <-fctx.Done():
		case <-time.After(wait):
			waited.Store(true)
			cancel()
		}
	}()
	code, err = follow(fctx, wk, id, w, stderr)
	switch {
	case ctx.Err() != nil:
		// Interrupted: stop it, as attach does.
		code, err = attach(ctx, wk, id, w, stderr)
		return code, false, err
	case !waited.Load():
		return code, false, err
	}
	dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer dcancel()
	if _, derr := wk.calls.DetachCommand(dctx, &pb.DetachCommandRequest{Id: id, SessionId: session}); derr != nil {
		if api.CodeOf(derr) != api.CodeConflict {
			return 0, false, derr
		}
		// It ended, or paused, as the wait ran out: say how, without
		// following it again (which would print its output again, and
		// resume it, paused).
		c, err := wk.command(dctx, id)
		if err != nil {
			return 0, false, err
		}
		code, err = followStatus(id, c, stderr)
		return code, false, err
	}
	return 0, true, nil
}

// outputBudget passes the first limit bytes of a command's output through
// to w, cut at a line's end where one is near, and holds back the rest,
// noting once, inline, that it does.
type outputBudget struct {
	w       io.Writer
	limit   int64
	written int64
	held    int64
}

func (b *outputBudget) Write(p []byte) (int, error) {
	n := len(p)
	if b.limit <= 0 {
		return b.w.Write(p)
	}
	if room := b.limit - b.written; room > 0 && b.held == 0 {
		take := p[:min(int64(len(p)), room)]
		if int64(len(p)) > room {
			// Stop at the last line's end that fits, not mid-line.
			if i := bytes.LastIndexByte(take, '\n'); i >= 0 {
				take = take[:i+1]
			}
		}
		if _, err := b.w.Write(take); err != nil {
			return 0, err
		}
		b.written += int64(len(take))
		p = p[len(take):]
	}
	if len(p) > 0 {
		if b.held == 0 {
			if _, err := fmt.Fprintf(b.w, "\n[hotbed: output past %s stops printing here as it runs; all of it is kept, and its last lines print at its end]\n", formatBytes(b.limit)); err != nil {
				return 0, err
			}
		}
		b.held += int64(len(p))
	}
	return n, nil
}

// printTail prints the last lines of command id's output to w, as its
// terminal shows them, after a note of what did not print; the run's
// output having been held back (outputBudget).
func printTail(ctx context.Context, wk workerClients, id string, w io.Writer) error {
	q, err := wk.calls.QueryOutput(ctx, &pb.QueryOutputRequest{Id: id, Tail: endTailLines})
	if err != nil {
		return err
	}
	if len(q.Lines) == 0 {
		return nil
	}
	fmt.Fprintf(w, "[hotbed: its last %d of %d lines:]\n", len(q.Lines), q.TotalLines)
	for _, l := range q.Lines {
		fmt.Fprintln(w, l.Text)
	}
	fmt.Fprintf(w, "[hotbed: all %d lines are kept: hotbed output %s --grep PATTERN (or --from N --to M, --head N, --tail N)]\n", q.TotalLines, id)
	return nil
}

// detachedHelp is what a run says when it left its command running.
func detachedHelp(id string, wait time.Duration) string {
	return fmt.Sprintf("hotbed: %s still runs after %s, detached: its output is kept whole. "+
		"Read it: hotbed output %s (--grep PATTERN, --tail N); wait for a line: hotbed output %s --until PATTERN --timeout 600; "+
		"its end: hotbed output %s --wait --timeout 600, or hotbed poll. Stop it: hotbed stop %s",
		id, wait, id, id, id, id)
}

var (
	// queryOutputSlice is the longest a single waiting QueryOutput HTTP call
	// waits on the server before the client re-issues it for the remaining
	// timeout. Matching the controller's 30s QUERY_OUTPUT_SLICE_SEC keeps each
	// client HTTP slice 1:1 with a single worker RPC slice.
	queryOutputSlice = 30 * time.Second
	// queryOutputSliceGrace is headroom beyond a slice's server-side wait for
	// network RTT, worker reconnects, and log scanning before the client treats
	// the HTTP call as wedged, recycles its transport, and retries.
	queryOutputSliceGrace = 35 * time.Second
	queryOutputRetryDelay = time.Second
)

// queryOutputBounded runs QueryOutput, slicing waiting calls (--wait, --until)
// into bounded per-slice HTTP requests so a wedged stream, controller deploy,
// or worker reconnect cannot strand the caller past --timeout, and riding out
// transient retryable worker/controller disconnects on plain reads as well.
func queryOutputBounded(
	ctx context.Context,
	client hotbedv1connect.WorkspaceServiceClient,
	hc *http.Client,
	rideOut time.Duration,
	req *pb.QueryOutputRequest,
) (*pb.OutputQuery, error) {
	if (!req.Wait && req.Until == "") || req.TimeoutSeconds < 0 {
		var unreachableSince time.Time
		for {
			q, err := client.QueryOutput(ctx, req)
			if err == nil || ctx.Err() != nil {
				return q, err
			}
			ae := api.FromConnect(err)
			if rideOut > 0 && ae.Retryable && (ae.Code == api.CodeWorkerUnreachable || ae.Code == api.CodeControllerUnreachable) {
				if unreachableSince.IsZero() {
					unreachableSince = time.Now()
				}
				if time.Since(unreachableSince) < rideOut {
					api.ResetClientTransport(hc)
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(queryOutputRetryDelay):
					}
					continue
				}
			}
			return nil, err
		}
	}
	totalWait := 60 * time.Second
	if req.TimeoutSeconds > 0 {
		totalWait = time.Duration(req.TimeoutSeconds) * time.Second
	}
	deadline := time.Now().Add(totalWait)
	var unreachableSince time.Time

	for {
		remaining := time.Until(deadline)
		if remaining < time.Second {
			remaining = time.Second
		}
		slice := min(queryOutputSlice, remaining)
		sliceSec := max(int32(1), int32((slice+time.Second-1)/time.Second))
		sliceReq := proto.Clone(req).(*pb.QueryOutputRequest)
		sliceReq.TimeoutSeconds = sliceSec

		sliceCtx, cancel := context.WithTimeout(ctx, time.Duration(sliceSec)*time.Second+queryOutputSliceGrace)
		q, err := client.QueryOutput(sliceCtx, sliceReq)
		sliceTimedOut := errors.Is(sliceCtx.Err(), context.DeadlineExceeded)
		cancel()

		if err == nil {
			unreachableSince = time.Time{}
			if q.Waited != "timeout" || !time.Now().Before(deadline) || ctx.Err() != nil {
				return q, nil
			}
			continue
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if sliceTimedOut || connect.CodeOf(err) == connect.CodeDeadlineExceeded {
			api.ResetClientTransport(hc)
			if time.Now().Before(deadline) {
				continue
			}
			return nil, err
		}
		ae := api.FromConnect(err)
		if rideOut > 0 && ae.Retryable && (ae.Code == api.CodeWorkerUnreachable || ae.Code == api.CodeControllerUnreachable) {
			if unreachableSince.IsZero() {
				unreachableSince = time.Now()
			}
			if time.Since(unreachableSince) < rideOut && time.Now().Before(deadline) {
				api.ResetClientTransport(hc)
				delay := min(queryOutputRetryDelay, time.Until(deadline))
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(delay):
				}
				continue
			}
		}
		return nil, err
	}
}

// waitServing waits, at most timeout, for detached command cmd to be ready:
// a line of its output matching pattern, and/or its port taking
// connections; it fails if the command ends first.
func waitServing(ctx context.Context, wk workerClients, d deps, cmd *pb.Command, pattern string, port bool, timeout time.Duration, serving func(ctx context.Context, workspace string, port int) bool) error {
	deadline := time.Now().Add(timeout)
	if pattern != "" {
		left := time.Until(deadline)
		q, err := queryOutputBounded(ctx, wk.stream, d.stream, d.rideOut, &pb.QueryOutputRequest{Id: cmd.Id, Until: pattern, TimeoutSeconds: int32(max(1, left/time.Second)), Tail: endTailLines})
		if err != nil {
			return err
		}
		switch q.Waited {
		case "matched":
			fmt.Fprintf(d.stderr, "hotbed: %s is ready: its output says so\n", cmd.Id)
		case "timeout":
			return api.Errorf(api.CodeNotReady, "%s wrote no line matching %q in %s: it runs on", cmd.Id, pattern, timeout).
				WithSuggestion(fmt.Sprintf("read what it wrote: hotbed output %s", cmd.Id))
		default:
			return api.Errorf(api.CodeNotReady, "%s %s before writing a line matching %q", cmd.Id, q.Waited, pattern).
				WithSuggestion(fmt.Sprintf("read why: hotbed output %s", cmd.Id))
		}
	}
	if !port {
		return nil
	}
	u, err := url.Parse(cmd.Url)
	if err != nil || cmd.Url == "" {
		return api.Errorf(api.CodeInvalidArgs, "--ready-port needs --port: %s has no URL", cmd.Id)
	}
	portNum, err := strconv.Atoi(u.Port())
	if err != nil {
		return api.Errorf(api.CodeInternal, "%s's URL %s names no port", cmd.Id, cmd.Url)
	}
	for {
		if serving(ctx, cmd.WorkspaceId, portNum) {
			fmt.Fprintf(d.stderr, "hotbed: %s is ready: %s takes connections\n", cmd.Id, cmd.Url)
			return nil
		}
		if c, cerr := wk.command(ctx, cmd.Id); cerr == nil && !api.Live(c.State) {
			return api.Errorf(api.CodeNotReady, "%s ended before %s took connections: %s", cmd.Id, cmd.Url, c.Reason).
				WithSuggestion(fmt.Sprintf("read why: hotbed output %s", cmd.Id))
		}
		if time.Now().After(deadline) {
			return api.Errorf(api.CodeNotReady, "%s took no connections in %s: it runs on", cmd.Url, timeout).
				WithSuggestion(fmt.Sprintf("read what it wrote: hotbed output %s", cmd.Id))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
