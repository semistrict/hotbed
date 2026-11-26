package worker

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"
)

// A command that holds a large share of the node's memory while barely using
// a CPU is presumed stuck (a hung test, a deadlock, a wait on something that
// will not come) and keeps that memory from all other work: admission
// reserves what commands hold (board 9128), and pushed to swap it still fills
// the node's swap. One that does so for idleHogWindow is stopped.
const (
	// idleHogShare is the share of the node's memory (admitLimit) a command
	// must hold, in RAM or swap, for its CPU use to be watched.
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
// frozen, held or paused, uses no CPU by design), and not a dev server, an
// interactive command or a measurement, which may rightly sit on what they
// hold while idle. Call with e.mu held.
func idleHogWatched(c *command) bool {
	cmd := c.cmd()
	return cmd.State == running && !c.stopping && !c.intervening && !c.deactivating &&
		cmd.Kind != "server" && !cmd.TtyInput && !cmd.Bench
}

// watchIdle records c's sample in its idle watch: it holds held, in RAM or
// swap (known says whether that could be read), and has used nanos of CPU
// time by at. Call with e.mu held.
func (e *Engine) watchIdle(c *command, held int64, known bool, at time.Time, nanos uint64) {
	limit := e.admitLimit()
	heavy := known && limit > 0 && float64(held) > idleHogShare*float64(limit) && idleHogWatched(c)
	c.idle.see(heavy, held, at, nanos)
}

// stopIdleHogs stops each command that held over idleHogShare of the node's
// memory for idleHogWindow while using under idleHogCores.
func (e *Engine) stopIdleHogs(ctx context.Context) {
	type hog struct{ id, why string }
	var hogs []hog
	e.mu.Lock()
	limit := e.admitLimit()
	for _, c := range e.commands {
		if !idleHogWatched(c) {
			continue
		}
		over, cores, ok := c.idle.stuck()
		if !ok {
			continue
		}
		hogs = append(hogs, hog{c.cmd().Id, fmt.Sprintf(
			"it held %s, over %.0f%% of the node's %s, for %s while using %.2f CPU cores on average: stopped as stuck, to free that memory",
			gib(c.idle.held), idleHogShare*100, gib(limit), over.Round(time.Second), cores)})
	}
	e.mu.Unlock()
	for _, h := range hogs {
		slog.Warn("stop a command holding much memory idle", "command", h.id, "why", h.why)
		if _, err := e.Stop(ctx, h.id, h.why); err != nil {
			slog.Error("stop a command holding much memory idle", "command", h.id, "err", err)
		}
	}
}
