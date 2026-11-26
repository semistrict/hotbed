package worker

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/semistrict/hotbed/internal/unit"
)

// A command that holds a large share of the node's memory while barely using
// a CPU is presumed stuck (a hung test, a deadlock, a wait on something that
// will not come) and keeps that memory from all other work: in RAM it leaves
// the rest of the node less, pushed to swap it fills the node's swap, and
// admission reserves at least what it held until it ends (board 9128). One
// that does so for idleHogWindow is stopped, its diagnostics taken first to
// show where it was stuck, unless its run asked to be kept (--keep-idle).
const (
	// idleHogShare is the share of the node's memory for work (admitLimit) a
	// command must hold itself, in RAM or swap, for its CPU use to be
	// watched.
	idleHogShare = 0.25
	// idleHogCores is the average CPU use, over idleHogWindow, below which a
	// command holding that much is stopped.
	idleHogCores  = 0.1
	idleHogWindow = 5 * time.Minute
	// idleHogMark is how often the CPU time a command has used is marked,
	// so its average is taken over the last window to within that.
	idleHogMark = time.Minute
)

// cpuMark is the CPU time a command had used by a moment.
type cpuMark struct {
	at    time.Time
	nanos uint64
}

// idleWatch is what the engine has seen of a command since it began holding
// over idleHogShare of the node's memory without a break: marks of the CPU
// time it had used, idleHogMark or more apart and oldest first, back to the
// newest at least idleHogWindow old; and its latest sample. Guarded by e.mu.
type idleWatch struct {
	marks []cpuMark
	last  cpuMark
	held  int64
}

// see records a sample of a command at at: whether it holds enough to be
// watched (heavy), what it holds, and the CPU time it has used. A sample
// that does not hold enough, or whose CPU time went back (a new unit), starts
// the watch over.
func (w *idleWatch) see(heavy bool, held int64, at time.Time, nanos uint64) {
	if !heavy || nanos < w.last.nanos {
		*w = idleWatch{}
	}
	if !heavy {
		return
	}
	m := cpuMark{at: at, nanos: nanos}
	w.last, w.held = m, held
	if n := len(w.marks); n == 0 || at.Sub(w.marks[n-1].at) >= idleHogMark {
		w.marks = append(w.marks, m)
	}
	for len(w.marks) > 1 && at.Sub(w.marks[1].at) >= idleHogWindow {
		w.marks = slices.Delete(w.marks, 0, 1)
	}
}

// stuck says whether the command has held enough for idleHogWindow while
// using under idleHogCores, and over how long and at what average it did.
func (w *idleWatch) stuck() (over time.Duration, cores float64, ok bool) {
	if len(w.marks) == 0 {
		return 0, 0, false
	}
	base := w.marks[0]
	over = w.last.at.Sub(base.at)
	if over < idleHogWindow {
		return 0, 0, false
	}
	cores = float64(w.last.nanos-base.nanos) / 1e9 / over.Seconds()
	return over, cores, cores < idleHogCores
}

// idleHogWatched says whether c is one the stop applies to: running (one
// frozen, held or paused, uses no CPU by design), not being stopped, paused
// or resumed already, not asked to be kept, and not a dev server, an
// interactive command or a measurement, which may rightly sit on what they
// hold while idle. Call with e.mu held.
func idleHogWatched(c *command) bool {
	cmd := c.cmd()
	return cmd.State == running && !c.stopping && !c.intervening && !c.deactivating &&
		!cmd.Limits.GetKeepIdle() && cmd.Kind != "server" && !cmd.TtyInput && !cmd.Bench
}

// watchIdle records in c's idle watch its sample st, taken at at, and mem,
// the memory it holds as observe read it (known says whether it could). What
// counts is what its processes hold themselves (st.OwnMemoryUse, if set): a
// VM command is not charged its share of its VM's footprint here, as
// stopping it would free no more than its own. Call with e.mu held.
func (e *Engine) watchIdle(c *command, st unit.State, mem unit.MemoryUse, known bool, at time.Time) {
	own := mem
	if st.OwnMemoryUse != nil {
		own, known = *st.OwnMemoryUse, true
	}
	limit := e.admitLimit()
	heavy := known && !st.Frozen && limit > 0 && float64(own.Held()) > idleHogShare*float64(limit) && idleHogWatched(c)
	c.idle.see(heavy, own.Held(), at, st.CPUNanos)
}

// stopIdleHogs stops each command that held over idleHogShare of the node's
// memory for work for idleHogWindow while using under idleHogCores. Each
// stop is the node's own intervention, so its diagnostics are taken first
// and kept with it (hotbed diagnostics), and runs in the background: the
// diagnostics take seconds, which step must not wait on. A stop someone
// else began first wins.
func (e *Engine) stopIdleHogs() {
	e.mu.Lock()
	defer e.mu.Unlock()
	limit := e.admitLimit()
	for _, c := range e.commands {
		if !idleHogWatched(c) {
			continue
		}
		over, cores, ok := c.idle.stuck()
		if !ok {
			continue
		}
		id := c.cmd().Id
		why := fmt.Sprintf(
			"it held %s, over %.0f%% of the %s this node has for work, for %s while using %.2f CPU cores on average: stopped as stuck, to free that memory (hotbed run --keep-idle keeps a command that rightly waits on much memory)",
			gib(c.idle.held), idleHogShare*100, gib(limit), over.Round(time.Second), cores)
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
			slog.Warn("stop a command holding much memory idle", "command", id, "why", why)
			if _, err := e.StopWith(ctx, id, Intervention{Comment: why, Diagnose: true, Host: e.cfg.Host, ByNode: true}); err != nil {
				slog.Warn("stop a command holding much memory idle", "command", id, "err", err)
			}
		}) {
			e.idleStops.Done() // the engine is closing
		}
	}
}
