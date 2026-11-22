package worker

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/bench"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/unit"
)

// Measurements (hotbed run --bench) run one at a time on a node, on the
// node's measurement CPUs, which no other command uses while one runs:
//
//   - A measurement waits for another to end, and for running work to fit
//     in the CPUs it leaves them. While it waits for room, work queued after
//     it waits behind it, so that the work ahead drains; while it waits for
//     another measurement, work goes on past it.
//   - The node is isolated (bench.Isolation) just before it starts, and
//     given back once no measurement runs: after it ends, and after a worker
//     restart that finds none running.
//   - It never swaps, is never parked, and runs at the default nice.
//   - Its isolation is checked while it runs and as it ends: a measurement
//     whose isolation was lost fails, whatever its status.

// offRetryAfter is how long a worker waits to try again to give back an
// isolation it failed to.
const offRetryAfter = 30 * time.Second

// Isolator isolates a node for its measurements; *bench.Isolation is the
// real one.
type Isolator interface {
	// Slice is the slice measurements run in.
	Slice() string
	// CPUs is the measurement's CPUs.
	CPUs() bench.CPUs
	// Count is how many CPUs a measurement takes, and how many it leaves.
	Count() (bench, work int)
	// Active says whether the node is isolated, or partly.
	Active() bool
	// On isolates the node, or fails having undone what it did.
	On(ctx context.Context) error
	// Off gives the node back.
	Off(ctx context.Context) error
	// Held says why the isolation no longer holds, or nil while it does.
	Held() error
}

// BenchSlice is where a worker whose commands run in slice runs its
// measurements: beside it, in the same parent (user-1000-hotbed.slice's in
// user-1000.slice), so they share its memory but not its IO caps.
func BenchSlice(slice string) string {
	return strings.TrimSuffix(cmp.Or(slice, unit.DefaultSlice), ".slice") + "_bench.slice"
}

// sliceOf is the slice c's unit runs in.
func (e *Engine) sliceOf(c *command) string {
	if c.cmd().Bench && e.cfg.Bench != nil {
		return e.cfg.Bench.Slice()
	}
	return e.cfg.Slice
}

// measuring is the measurement running now, if one is. Call with e.mu held.
func (e *Engine) measuring() *command {
	for _, c := range e.commands {
		if s := c.cmd().State; c.cmd().Bench && (s == running || s == parked) {
			return c
		}
	}
	return nil
}

// workCores is how many CPUs running commands other than measurements keep
// busy. Call with e.mu held.
func (e *Engine) workCores() float64 {
	var cores float64
	for _, c := range e.commands {
		if c.cmd().State == running && !c.cmd().Bench {
			cores += c.cmd().CpuCores
		}
	}
	return cores
}

// benchBlocker says why a measurement cannot start now, beyond what keeps
// any command waiting, or "": ahead is an older one still waiting. Call
// with e.mu held.
func (e *Engine) benchBlocker(ahead *command) string {
	if m := e.measuring(); m != nil {
		return fmt.Sprintf("waiting for measurement %s to finish", m.cmd().Id)
	}
	if ahead != nil {
		return fmt.Sprintf("waiting for measurement %s, queued before it", ahead.cmd().Id)
	}
	return ""
}

// benchRoom says why running work leaves the measurement no room, or "".
// Call with e.mu held.
func (e *Engine) benchRoom() string {
	_, work := e.cfg.Bench.Count()
	if cores := e.workCores(); cores > float64(work) {
		return fmt.Sprintf("waiting for CPUs: running commands keep %.1f busy, more than the %d a measurement leaves them", cores, work)
	}
	return ""
}

// checkIsolation records, once, that the measurement c lost its isolation.
// Call with e.mu held.
func (e *Engine) checkIsolation(c *command) {
	if !c.cmd().Bench || c.rec.IsolationLost != "" || e.cfg.Bench == nil {
		return
	}
	err := e.cfg.Bench.Held()
	if err == nil {
		return
	}
	cmd := c.cmd()
	c.rec.IsolationLost = err.Error()
	cmd.Timeline = append(cmd.Timeline, &pb.CommandEvent{At: e.nowMs(), What: "isolation lost", Detail: err.Error()})
	e.saveCommand(c)
	e.sessionEvent(c, "isolation lost", err.Error())
	e.event("fail", fmt.Sprintf("measurement %s lost its isolation: %v", cmd.Argv, err), ref("command", cmd.Id))
}

// settleBench gives the node back once no measurement runs: after one
// ended, or after a worker that stopped mid-measurement started again.
func (e *Engine) settleBench(ctx context.Context) {
	iso := e.cfg.Bench
	if iso == nil {
		return
	}
	e.mu.Lock()
	m := e.measuring()
	e.mu.Unlock()
	if m != nil || !iso.Active() || e.now().Before(e.offRetry) {
		return
	}
	if err := iso.Off(ctx); err != nil {
		// Tried again a while later, not every step: each try asks systemd
		// too. A measurement meanwhile isolates from what was saved first.
		e.offRetry = e.now().Add(offRetryAfter)
		if msg := err.Error(); msg != e.offErr {
			slog.Error("give back the measurement's isolation", "err", err)
			e.offErr = msg
		}
		return
	}
	e.offErr, e.offRetry = "", time.Time{}
	slog.Info("gave back the measurement's isolation")
}
