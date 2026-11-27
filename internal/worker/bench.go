package worker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
//   - Where the node's volumes are on a ZFS pool, or commands run in VMs
//     (whose disks cladefsd writes), which no cgroup's IO cap reaches, the
//     commands of volumes writing there past the measurement's bound are
//     held (parked) from just before it starts until it ends, and other
//     volumes' queued commands wait for it to end.
//   - A measurement in a VM boots its VM in the measurement's slice, its
//     vCPUs pinned to the measurement CPUs (Engine.vmBoot).
//   - Its isolation is checked while it runs and as it ends: a measurement
//     whose isolation was lost (its disks' quiet too) fails, whatever its
//     status.

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
	// Held says why the isolation no longer holds for the measurement in
	// volume own, or nil while it does.
	Held(own string) error
	// Sample reads what others have written to the disks a measurement
	// uses; the engine calls it every step, saying whether a measurement
	// waits or runs (measuring), without which it asks nothing costly.
	Sample(measuring bool)
	// Ready says whether the samples name who writes where a measurement
	// holds writers back, so one may start.
	Ready() bool
	// Quieted is where the engine holds writers back while a measurement
	// runs, which no cap reaches (a ZFS pool, the VMs' disks); "" for
	// nowhere.
	Quieted() string
	// Loud is the volumes other than own the engine must hold back for the
	// places Quieted names to stay quiet, heaviest first.
	Loud(own string) []bench.Writer
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

// benchPending says whether a measurement waits or runs. Call with e.mu
// held.
func (e *Engine) benchPending() bool {
	for _, c := range e.commands {
		if s := c.cmd().State; c.cmd().Bench && (s == queued || s == running || s == parked) {
			return true
		}
	}
	return false
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

// workCores is how many CPUs running work keeps busy: commands other than
// measurements and dev servers, which run until stopped and would keep a
// measurement, and everything queued behind it, waiting for good. Call with
// e.mu held.
func (e *Engine) workCores() float64 {
	var cores float64
	for _, c := range e.commands {
		if c.cmd().State == running && !c.cmd().Bench && c.cmd().Kind != "server" {
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
	if !e.cfg.Bench.Ready() {
		return fmt.Sprintf("waiting to see who writes to %s, to hold them back beside it", e.cfg.Bench.Quieted())
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

// postExitIsolator is optionally implemented by Isolators (such as
// *bench.Isolation) that can skip missing bench-slice cpuset files after exit
// while still verifying workqueue, resctrl, and disk-quiet isolation.
type postExitIsolator interface {
	HeldAfterExit(own string) error
}

// checkIsolation records, once, that the measurement c lost its isolation.
// When exited is true, ENOENT reading the bench slice's cpuset files is ignored
// because systemd tears down the empty bench slice once the command exits
// (board 7935), while workqueue, resctrl, and disk-quiet checks still run; any
// loss while running or any other post-exit failure is still recorded. Call
// with e.mu held.
func (e *Engine) checkIsolation(c *command, exited bool) {
	if !c.cmd().Bench || c.rec.IsolationLost != "" || e.cfg.Bench == nil {
		return
	}
	var err error
	if exited {
		if pe, ok := e.cfg.Bench.(postExitIsolator); ok {
			err = pe.HeldAfterExit(c.rec.Volume)
		} else {
			err = e.cfg.Bench.Held(c.rec.Volume)
			if err != nil && isBenchSliceGone(err, e.cfg.Bench.Slice()) {
				return
			}
		}
	} else {
		err = e.cfg.Bench.Held(c.rec.Volume)
	}
	if err == nil {
		return
	}
	cmd := c.cmd()
	c.rec.IsolationLost = err.Error()
	cmd.Timeline = append(cmd.Timeline, &pb.CommandEvent{At: e.nowMs(), What: "isolation lost", Detail: err.Error()})
	e.saveCommand(c, change{
		what: "isolation lost", detail: err.Error(),
		feed: feed(feedFail, fmt.Sprintf("measurement %s lost its isolation: %v", cmd.Argv, err), ref("command", cmd.Id)),
	})
}

// isBenchSliceGone reports whether err is ENOENT reading the bench slice's
// cpuset partition files after systemd tore down the empty slice when the
// command exited.
func isBenchSliceGone(err error, slice string) bool {
	if !errors.Is(err, os.ErrNotExist) {
		return false
	}
	var pe *os.PathError
	if !errors.As(err, &pe) {
		return false
	}
	base := filepath.Base(pe.Path)
	if base != "cpuset.cpus.partition" && base != "cpuset.cpus.effective" {
		return false
	}
	return slice == "" || filepath.Base(filepath.Dir(pe.Path)) == slice
}

// quietBlocker says why c, not a measurement, waits for the one running:
// it would write where no cap holds it (a ZFS pool, the VMs' disks), beside
// it. Call with e.mu held.
func (e *Engine) quietBlocker(c *command) string {
	if e.cfg.Bench == nil || e.cfg.Bench.Quieted() == "" {
		return ""
	}
	m := e.measuring()
	if m == nil || m.rec.Volume == c.rec.Volume {
		return ""
	}
	return fmt.Sprintf("waiting for measurement %s to finish: it keeps %s quiet, which no cap can do", m.cmd().Id, e.cfg.Bench.Quieted())
}

// holdLoud holds the commands of the volumes writing past the bound beside
// measurement m (about to start, or running), where no cap holds them,
// until no measurement runs: frozen, not pushed to swap, as they'll be back
// soon. One in a VM is frozen in its guest, its VM running on.
func (e *Engine) holdLoud(ctx context.Context, m *command) {
	iso := e.cfg.Bench
	if iso == nil || iso.Quieted() == "" {
		return
	}
	loud := iso.Loud(m.rec.Volume)
	if len(loud) == 0 {
		return
	}
	writers := map[string]bench.Writer{}
	for _, w := range loud {
		writers[w.Volume] = w
	}
	e.mu.Lock()
	var victims []*command
	for _, c := range e.commands {
		if _, ok := writers[c.rec.Volume]; ok && c.cmd().State == running && !c.cmd().Bench && !c.stopping && !c.deactivating {
			victims = append(victims, c)
		}
	}
	e.mu.Unlock()
	for _, c := range victims {
		e.holdOneLoud(ctx, c, m.cmd().Id, writers[c.rec.Volume])
	}
}

func (e *Engine) holdOneLoud(ctx context.Context, c *command, benchID string, w bench.Writer) {
	c.freezeMu.Lock()
	defer c.freezeMu.Unlock()
	e.mu.Lock()
	if c.cmd().State != running || c.cmd().Bench || c.stopping || c.deactivating || c.intervening {
		e.mu.Unlock()
		return
	}
	id := c.cmd().Id
	e.mu.Unlock()
	x, r := e.executorFor(c), e.ref(c)
	if err := x.Freeze(ctx, r); err != nil {
		if !e.reconcileInactiveUnit(ctx, c) {
			slog.Error("hold a command beside a measurement", "command", id, "err", err)
		}
		return
	}
	e.mu.Lock()
	cmd := c.cmd()
	if cmd.State != running || c.stopping {
		// Ended, stopped, or transitioned as it froze: only thaw if Stop is
		// still tearing down a running unit (never thaw a paused/parked unit).
		shouldThaw := c.stopping && cmd.State == running
		e.mu.Unlock()
		if shouldThaw {
			if err := x.Thaw(ctx, r); err != nil {
				slog.Warn("thaw a command no longer to hold", "command", id, "err", err)
			}
		}
		return
	}
	rate := w.Rate / 1e6
	cmd.State = parked
	cmd.CpuCores = 0
	cmd.Reason = fmt.Sprintf("held: its volume wrote %.1f MB/s to %s beside measurement %s, past its bound; thawed once it ends", rate, w.To, benchID)
	cmd.Timeline = append(cmd.Timeline, &pb.CommandEvent{At: e.nowMs(), What: "held", Detail: cmd.Reason})
	c.rec.HeldFor = benchID
	c.rec.ParkedAt = e.nowMs()
	e.saveCommand(c, change{
		what: "held", detail: cmd.Reason,
		feed: feed(feedPark, fmt.Sprintf("hold %s · %.1f MB/s to %s beside measurement %s", cmd.Argv, rate, w.To, benchID), ref("command", cmd.Id)),
	})
	e.mu.Unlock()
}

// releaseHeld thaws the commands held beside a measurement, once none
// runs. One paused meanwhile (its soft time limit) stays so. Holding beside
// a measurement is the only thing that freezes a command in parked, so any
// command found parked is thawed too.
func (e *Engine) releaseHeld(ctx context.Context) {
	e.mu.Lock()
	var held []*command
	for _, c := range e.commands {
		if c.rec.HeldFor != "" || c.cmd().State == parked {
			held = append(held, c)
		}
	}
	e.mu.Unlock()
	for _, c := range held {
		e.releaseOneHeld(ctx, c)
	}
}

func (e *Engine) releaseOneHeld(ctx context.Context, c *command) {
	c.freezeMu.Lock()
	defer c.freezeMu.Unlock()
	e.mu.Lock()
	cmd := c.cmd()
	if cmd.State != parked {
		if c.rec.HeldFor != "" {
			c.rec.HeldFor = ""
			e.saveCommand(c, change{what: "released"})
		}
		e.mu.Unlock()
		return
	}
	if c.deactivating || c.stopping || c.intervening {
		e.mu.Unlock()
		return
	}
	slept := (time.Duration(e.nowMs()-c.rec.ParkedAt) * time.Millisecond).Round(time.Second)
	detail := "after " + slept.String()
	if c.rec.HeldFor != "" {
		detail += fmt.Sprintf(", measurement %s over", c.rec.HeldFor)
	}
	if err := e.thawLocked(ctx, c, change{
		what: "thawed", detail: detail,
		feed: feed(feedThaw, fmt.Sprintf("thaw %s %s", cmd.Argv, detail), ref("command", cmd.Id)),
	}); err != nil {
		slog.Error("thaw a command held beside a measurement", "command", cmd.Id, "err", err)
	}
}

// quiet keeps the pool quiet beside the measurement running, if one is.
func (e *Engine) quiet(ctx context.Context) {
	e.mu.Lock()
	m := e.measuring()
	e.mu.Unlock()
	if m != nil {
		e.holdLoud(ctx, m)
	}
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
