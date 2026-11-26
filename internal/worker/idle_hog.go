package worker

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/semistrict/hotbed/internal/unit"
)

// A command that reserves a large share of the node's memory while doing
// little with it keeps other work queued for nothing: admission keeps for it
// its guarantee, its estimate or the most it has held, whichever is most,
// until it ends (reservation). One that, for idleHogWindow, reserves over
// idleHogShare of the node's memory for work while using under a core for
// each idleHogGiBPerCore GiB of it is stopped, its diagnostics taken first to
// show what it was doing, unless its run asked to be kept (--keep-idle).
// What counts is what it reserves, not what it holds now: that is what makes
// other work queue (a VM command's charged its VM's footprint, which its VM
// keeps from the host after its guest is done with it).
const (
	// idleHogShare is the share of the node's memory for work (admitLimit)
	// a command must reserve for its CPU use to be watched.
	idleHogShare = 0.25
	// idleHogGiBPerCore is how many GiB of what it reserves a command must
	// keep a CPU core busy for, on average over idleHogWindow, not to be
	// stopped.
	idleHogGiBPerCore = 6
	idleHogWindow     = 5 * time.Minute
	// idleHogMark is how often the CPU time a command has used is marked,
	// so its average is taken over the last window to within that.
	idleHogMark = time.Minute
)

// cpuMark is the CPU time a command had used by a moment, and what it
// reserved then.
type cpuMark struct {
	at       time.Time
	nanos    uint64
	reserved int64
}

// idleWatch is what the engine has seen of a command since it began
// reserving over idleHogShare of the node's memory without a break: marks of
// the CPU time it had used, idleHogMark or more apart and oldest first, back
// to the newest at least idleHogWindow old; and its latest sample. Guarded by
// e.mu.
type idleWatch struct {
	marks []cpuMark
	last  cpuMark
}

// see records a sample of a command: whether it reserves enough to be
// watched (heavy), and m, what it reserves and the CPU time it has used by
// m.at. A sample that does not reserve enough, or whose CPU time went back (a
// new unit), starts the watch over.
func (w *idleWatch) see(heavy bool, m cpuMark) {
	if !heavy || m.nanos < w.last.nanos {
		*w = idleWatch{}
	}
	if !heavy {
		return
	}
	w.last = m
	if n := len(w.marks); n == 0 || m.at.Sub(w.marks[n-1].at) >= idleHogMark {
		w.marks = append(w.marks, m)
	}
	for len(w.marks) > 1 && m.at.Sub(w.marks[1].at) >= idleHogWindow {
		w.marks = slices.Delete(w.marks, 0, 1)
	}
}

// wasteful says whether the command has reserved enough for idleHogWindow
// while using under a core for each idleHogGiBPerCore GiB of the least it
// reserved meanwhile, and over how long, at what average, and on how much it
// did.
func (w *idleWatch) wasteful() (over time.Duration, cores float64, reserved int64, ok bool) {
	if len(w.marks) == 0 {
		return 0, 0, 0, false
	}
	base := w.marks[0]
	over = w.last.at.Sub(base.at)
	if over < idleHogWindow {
		return 0, 0, 0, false
	}
	reserved = w.last.reserved
	for _, m := range w.marks {
		reserved = min(reserved, m.reserved)
	}
	cores = float64(w.last.nanos-base.nanos) / 1e9 / over.Seconds()
	return over, cores, reserved, cores < float64(reserved)/(1<<30)/idleHogGiBPerCore
}

// idleHogWatched says whether c is one the stop applies to: running (one
// frozen, held or paused, uses no CPU by design), not being stopped, paused
// or resumed already, not asked to be kept, and not a dev server, an
// interactive command or a measurement, which may rightly sit on what they
// reserve while idle. Call with e.mu held.
func idleHogWatched(c *command) bool {
	cmd := c.cmd()
	return cmd.State == running && !c.stopping && !c.intervening && !c.deactivating &&
		!cmd.Limits.GetKeepIdle() && cmd.Kind != "server" && !cmd.TtyInput && !cmd.Bench
}

// watchIdle records in c's idle watch its sample st, taken at at, after
// observe has counted what it holds into its reservation. A unit seen frozen
// uses no CPU by design: it starts the watch over. Call with e.mu held.
func (e *Engine) watchIdle(c *command, st unit.State, at time.Time) {
	limit, reserved := e.admitLimit(), e.reservation(c)
	heavy := !st.Frozen && limit > 0 && float64(reserved) > idleHogShare*float64(limit) && idleHogWatched(c)
	c.idle.see(heavy, cpuMark{at: at, nanos: st.CPUNanos, reserved: reserved})
}

// stopIdleHogs stops each command that reserved over idleHogShare of the
// node's memory for work for idleHogWindow while using under a core for each
// idleHogGiBPerCore GiB of it. Each stop is the node's own intervention, so
// its diagnostics are taken first and kept with it (hotbed diagnostics), and
// runs in the background: the diagnostics take seconds, which step must not
// wait on. A stop someone else began first wins.
func (e *Engine) stopIdleHogs() {
	e.mu.Lock()
	defer e.mu.Unlock()
	limit := e.admitLimit()
	for _, c := range e.commands {
		if !idleHogWatched(c) {
			continue
		}
		over, cores, reserved, ok := c.idle.wasteful()
		if !ok {
			continue
		}
		id := c.cmd().Id
		why := fmt.Sprintf(
			"for %s it kept %s of the %s this node has for work, over %.0f%%, while using %.2f CPU cores on average, under one core for each %d GiB: stopped to free that memory for the work queued behind it (hotbed run --keep-idle keeps a command that rightly holds much memory for little CPU)",
			over.Round(time.Second), gib(reserved), gib(limit), idleHogShare*100, cores, idleHogGiBPerCore)
		// Watched afresh, should its stop not come to pass.
		c.idle = idleWatch{}
		e.idleStops.Add(1)
		if !e.startBgLocked(func(ctx context.Context) {
			defer e.idleStops.Done()
			// StopWith goes on whoever asked it gone, but the engine closing
			// should not wait out its diagnostics.
			defer context.AfterFunc(ctx, func() {
				e.mu.Lock()
				defer e.mu.Unlock()
				if c.cancelDiag != nil {
					c.cancelDiag()
				}
			})()
			slog.Warn("stop a command holding much memory for little CPU", "command", id, "why", why)
			if _, err := e.StopWith(ctx, id, Intervention{Comment: why, Diagnose: true, Host: e.cfg.Host, ByNode: true}); err != nil {
				slog.Warn("stop a command holding much memory for little CPU", "command", id, "err", err)
			}
		}) {
			e.idleStops.Done() // the engine is closing
		}
	}
}
