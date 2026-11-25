package worker

import (
	"bytes"
	"log/slog"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/semistrict/hotbed/internal/cmdlog"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// output is what the engine learns from a command's output: the units cargo
// compiled, signs of a failure's kind, and a freshen's step markers. It
// reads the log as it grows, from where it left off, so no log is ever read
// whole, and none under the engine's lock: the engine's tick and Stop catch
// it up, and the engine, under its lock, only asks what it found, which
// waits on no reading.
type output struct {
	read  sync.Mutex // held while reading the log
	lines *cmdlog.Lines
	mu    sync.Mutex // guards found, held only briefly
	found found
}

// found is what output has read from a command's log so far.
type found struct {
	units                int
	compileErr, testFail bool
	// The freshen step started last: step of steps, and its name.
	step, steps int
	stepName    string
	stepStartMs int64              // unix ms when stepName was observed starting
	allDone     bool               // the freshen said its steps are done
	started     map[string]bool    // freshen steps started
	done        map[string]float64 // seconds each finished freshen step took
	phaseDone   map[string]float64 // seconds each finished freshen phase took (e.g. "git")
	repoHeads   map[string]string  // repo -> commit SHA from ::hotbed-head:: lines
}

func newOutput(path string) *output { return &output{lines: cmdlog.NewLines(path)} }

var (
	compiling   = regexp.MustCompile(`^\s+Compiling \S+ v`)
	rustError   = regexp.MustCompile(`^error(\[E\d+\])?: `)
	testFailure = regexp.MustCompile(`(^test result: FAILED|^\s+FAIL \[|^--- FAIL:|^FAIL\s|Tests:\s+\d+ failed)`)
	stepStart   = regexp.MustCompile(`^::hotbed:: step (\d+)/(\d+) (\S+)$`)
	stepDone    = regexp.MustCompile(`^::hotbed:: step-done (\S+) (\d+)$`)
	phaseDone   = regexp.MustCompile(`^::hotbed:: phase-done (\S+) (\d+)$`)
	stepsDone   = []byte("::hotbed:: steps done")
)

// catchUpBytes is the most of a command's output catchUp reads at once, so
// one that writes faster than it can be read is compacted meanwhile.
const catchUpBytes = 64 << 20

// catchUp reads what the command wrote since the last call, up to
// catchUpBytes. Call it without the engine's lock: the command may have
// written a lot.
func (o *output) catchUp() {
	o.read.Lock()
	defer o.read.Unlock()
	o.readLines(catchUpBytes)
}

// finish reads the rest of a command's log, its last line too if that
// lacks a newline. Call it once the command's processes are gone, so the
// log is complete, and without the engine's lock.
func (o *output) finish() {
	o.read.Lock()
	defer o.read.Unlock()
	o.readLines(0)
	o.lines.Flush(o.add)
}

// compact keeps the log to l, and returns what the log has dropped. What
// output has yet to read of what it drops, it never reads: a command that
// writes faster than output reads keeps to l too.
func (o *output) compact(l cmdlog.Limit) (cmdlog.Dropped, error) {
	o.read.Lock()
	defer o.read.Unlock()
	if o.lines.Path() == "" {
		return cmdlog.Dropped{}, nil
	}
	return cmdlog.Compact(o.lines.Path(), l)
}

func (o *output) readLines(max int64) {
	if o.lines.Path() == "" {
		return
	}
	if err := o.lines.Read(o.add, max); err != nil {
		slog.Error("read command output", "err", err)
	}
}

func (o *output) add(line []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.found.line(line)
}

// result is the units cargo compiled and, if the command (of kind, ended in
// state with code) failed, the kind of failure.
func (o *output) result(state pb.CommandState, code *int32, kind string) (units int, failure string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.found.units, o.found.failure(state, code, kind)
}

// progress is how far a freshen got: step k of n starting means k-1 are
// done.
func (o *output) progress() float64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.found.steps == 0 {
		return 0
	}
	return float64(o.found.step-1) / float64(o.found.steps)
}

// freshenStep reports the current freshen step (1-based), total steps, step
// name, and whether all steps have finished.
func (o *output) freshenStep() (step, steps int, name string, allDone bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.found.step, o.found.steps, o.found.stepName, o.found.allDone
}

// freshenLive reports live step progress, step start timestamp, and git phase
// duration parsed from the freshen log so far.
func (o *output) freshenLive() (step, steps int, name string, startMs int64, allDone bool, gitSec float64, gitDone bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	gitSec, gitDone = o.found.phaseDone["git"]
	return o.found.step, o.found.steps, o.found.stepName, o.found.stepStartMs, o.found.allDone, gitSec, gitDone
}

// gitSeconds returns the elapsed seconds reported by ::hotbed:: phase-done git.
func (o *output) gitSeconds() float64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.found.phaseDone["git"]
}

// snapshotLive returns a copy of steps enriched with completed step timings
// while a freshen is still running, keeping not-yet-reached steps intact.
func (o *output) snapshotLive(steps []*pb.AppliedStep) []*pb.AppliedStep {
	o.mu.Lock()
	defer o.mu.Unlock()
	f := o.found
	out := make([]*pb.AppliedStep, len(steps))
	k := 0
	for i, s := range steps {
		cp := &pb.AppliedStep{
			Name:    s.Name,
			Hash:    s.Hash,
			Ran:     s.Ran,
			Ok:      s.Ok,
			Seconds: s.Seconds,
		}
		if cp.Ran {
			k++
			if secs, ok := f.done[cp.Name]; ok {
				cp.Ok = true
				cp.Seconds = secs
			} else if f.allDone || k < f.step {
				cp.Ok = true
			}
		}
		out[i] = cp
	}
	return out
}

// repoHeads returns a copy of repo -> HEAD SHA markers emitted during a freshen.
func (o *output) repoHeads() map[string]string {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make(map[string]string, len(o.found.repoHeads))
	for k, v := range o.found.repoHeads {
		out[k] = v
	}
	return out
}

// settle marks the steps a freshen ran as done, with their times; the one
// it started and did not finish failed. Steps after it never ran, so the
// base does not have them and they are left out. The freshen runs its
// steps in order and stops at the first that fails, so a step before the
// last one started is done though its markers were in output a long log
// dropped (its time then unknown, 0).
func (o *output) settle(steps []*pb.AppliedStep) []*pb.AppliedStep {
	o.mu.Lock()
	defer o.mu.Unlock()
	f := o.found
	var kept []*pb.AppliedStep
	k := 0 // the step's place among those the freshen ran, from 1
	for _, s := range steps {
		if s.Ran {
			k++
			secs, ok := f.done[s.Name]
			ok = ok || f.allDone || k < f.step
			s.Ok, s.Seconds = ok, secs
			if !ok && !f.started[s.Name] && k != f.step {
				continue // never reached
			}
		}
		kept = append(kept, s)
	}
	return kept
}

// line notes what one line of output says. Most lines say nothing it looks
// for, so a cheap check of the bytes each pattern needs comes before it.
func (f *found) line(l []byte) {
	switch {
	case bytes.Contains(l, []byte("Compiling ")) && compiling.Match(l):
		f.units++
	case bytes.HasPrefix(l, []byte("error")) && rustError.Match(l):
		f.compileErr = true
	case (bytes.Contains(l, []byte("FAIL")) || bytes.Contains(l, []byte("failed"))) && testFailure.Match(l):
		f.testFail = true
	}
	if bytes.HasPrefix(l, []byte("::hotbed-head:: ")) {
		fields := bytes.Fields(l[len("::hotbed-head:: "):])
		if len(fields) == 2 {
			if f.repoHeads == nil {
				f.repoHeads = map[string]string{}
			}
			f.repoHeads[string(fields[0])] = string(fields[1])
		}
		return
	}
	if bytes.HasPrefix(l, []byte("::hotbed:: phase-done ")) {
		if m := phaseDone.FindSubmatch(l); m != nil {
			us, _ := strconv.ParseInt(string(m[2]), 10, 64)
			if f.phaseDone == nil {
				f.phaseDone = map[string]float64{}
			}
			f.phaseDone[string(m[1])] = float64(us) / 1e6
		}
		return
	}
	if !bytes.HasPrefix(l, []byte("::hotbed:: step")) {
		return
	}
	if bytes.Equal(l, stepsDone) {
		f.allDone = true
		return
	}
	if m := stepStart.FindSubmatch(l); m != nil {
		f.step, _ = strconv.Atoi(string(m[1]))
		f.steps, _ = strconv.Atoi(string(m[2]))
		f.stepName = string(m[3])
		f.stepStartMs = time.Now().UnixMilli()
		if f.started == nil {
			f.started = map[string]bool{}
		}
		f.started[f.stepName] = true
	} else if m := stepDone.FindSubmatch(l); m != nil {
		us, _ := strconv.ParseInt(string(m[2]), 10, 64)
		if f.done == nil {
			f.done = map[string]float64{}
		}
		f.done[string(m[1])] = float64(us) / 1e6
	}
}

// failure sorts how a command that ended in state failed into a kind people
// recognise; empty when it did not fail.
func (f found) failure(state pb.CommandState, code *int32, kind string) string {
	switch {
	case state != failed:
		return ""
	case code == nil:
		return "signal"
	case f.testFail:
		return "test"
	case f.compileErr && kind == "check":
		return "lint"
	case f.compileErr:
		return "compile"
	default:
		return "exit"
	}
}
