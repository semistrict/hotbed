package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/unit"
)

// limited is an engine whose clock the test sets, running command c-1
// (sleep 300) attached with a soft limit of 10m and a hard limit of 2h; wrap,
// if given, wraps its units.
func limited(t *testing.T, now *time.Time, wrap ...func(Units) Units) *Engine {
	t.Helper()
	units, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}
	var u Units = units
	for _, w := range wrap {
		u = w(u)
	}
	e, err := NewEngine(t.Context(), EngineConfig{
		Dir: t.TempDir(), NodeID: "n", Host: "asahimbp",
		Store: store, BaseImage: fake.BaseImage(store), Units: u, System: fake.NewSystem(), Now: func() time.Time { return *now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	if err := os.MkdirAll(filepath.Join(store.Path("v"), "work", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.workspaces["w"] = &workspace{rec: &pb.WorkspaceRecord{Workspace: &pb.Workspace{Id: "w", Project: "app", Repos: []*pb.WorkspaceRepo{{Repo: "app"}}, State: idle}, Volume: "v"}}
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "c-1", WorkspaceId: "w", Args: []string{"sleep", "300"}, Attached: true,
		Limits: &pb.CommandLimits{SoftLimitSeconds: 600, HardLimitSeconds: 7200},
	}); err != nil {
		t.Fatal(err)
	}
	e.admit(t.Context())
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-c-1") })
	if c := e.commands["c-1"].cmd(); c.State != running {
		t.Fatalf("c-1 is %v: %s", c.State, c.Reason)
	}
	return e
}

// frozen says whether the unit name is frozen.
func (e *Engine) frozen(t *testing.T, name string) bool {
	t.Helper()
	st, err := e.cfg.Units.Status(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	return st.Frozen
}

func (e *Engine) state(id string) pb.CommandState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.commands[id].cmd().State
}

// The soft limit counts from the command's start, and again from each time
// an agent starts waiting on it.
func TestSoftLimitCountsFromTheLastWait(t *testing.T) {
	now := time.Unix(1795300000, 0)
	e := limited(t, &now)
	now = now.Add(8 * time.Minute)
	if err := e.WaitOn(t.Context(), "c-1"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(9 * time.Minute) // 17 minutes in, 9 since the wait
	e.limit(t.Context())
	if s := e.state("c-1"); s != running {
		t.Fatalf("9 minutes after a wait, c-1 is %v, want running", s)
	}
	now = now.Add(time.Minute)
	e.limit(t.Context())
	if s := e.state("c-1"); s != paused {
		t.Fatalf("10 minutes after a wait, c-1 is %v, want paused", s)
	}
	if !e.frozen(t, "hotbed-c-1") {
		t.Error("the paused command's unit is not frozen")
	}
	if err := e.WaitOn(t.Context(), "c-1"); err != nil {
		t.Fatal(err)
	}
	if s := e.state("c-1"); s != running || e.frozen(t, "hotbed-c-1") {
		t.Errorf("waited on again, c-1 is %v, frozen %v; want running, thawed", s, e.frozen(t, "hotbed-c-1"))
	}
}

// A paused attached command, which no one follows any more, is not stopped
// as an orphan: its agent may come back for it until its hard limit, which
// then stops it.
func TestAPausedCommandWaitsForItsHardLimit(t *testing.T) {
	now := time.Unix(1795300000, 0)
	e := limited(t, &now)
	now = now.Add(11 * time.Minute)
	e.limit(t.Context())
	now = now.Add(time.Hour)
	e.reap(t.Context())
	if s := e.state("c-1"); s != paused {
		t.Fatalf("an hour after it paused, with no one following it, c-1 is %v, want paused", s)
	}
	now = now.Add(time.Hour)
	e.limit(t.Context())
	if c := e.commands["c-1"].cmd(); c.State != cancelled || c.Reason != "its hard time limit of 2h0m0s passed" {
		t.Errorf("past its hard limit, c-1 is %v: %q", c.State, c.Reason)
	}
}

// A paused command holds no memory reservation: its memory went to swap,
// and the agent it waits for may never come.
func TestAPausedCommandReservesNoMemory(t *testing.T) {
	now := time.Unix(1795300000, 0)
	e := limited(t, &now)
	e.mu.Lock()
	e.commands["c-1"].cmd().Limits.MemoryLowBytes = 4 << 30
	e.mu.Unlock()
	if got := e.Heartbeat("cl").ReservedMemoryBytes; got != 4<<30 {
		t.Fatalf("running, it reserves %d, want 4 GiB", got)
	}
	now = now.Add(11 * time.Minute)
	e.limit(t.Context())
	if got := e.Heartbeat("cl").ReservedMemoryBytes; e.state("c-1") != paused || got != 0 {
		t.Errorf("%v, it reserves %d, want paused and none", e.state("c-1"), got)
	}
}

// A command resumed by an agent waiting on it is no longer its first
// client's: it is left to its limits, and not stopped when the agent now
// waiting on it goes.
func TestAResumedCommandIsNotReaped(t *testing.T) {
	now := time.Unix(1795300000, 0)
	e := limited(t, &now)
	now = now.Add(11 * time.Minute)
	e.limit(t.Context())
	if err := e.WaitOn(t.Context(), "c-1"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Minute)
	e.reap(t.Context())
	if c := e.commands["c-1"].cmd(); c.State != running || c.Attached {
		t.Errorf("resumed and then left, c-1 is %v, attached %v; want running, detached", c.State, c.Attached)
	}
}

// A worker that stopped between freezing a running command and recording
// that thaws its unit when it starts again.
func TestAStrayFrozenUnitIsThawed(t *testing.T) {
	now := time.Unix(1795300000, 0)
	e := limited(t, &now)
	if err := e.cfg.Units.Freeze(t.Context(), "hotbed-c-1"); err != nil {
		t.Fatal(err)
	}
	e.thawStrays(t.Context())
	if e.frozen(t, "hotbed-c-1") {
		t.Error("the running command's unit is still frozen")
	}
}

// freezeHook runs a hook as a unit freezes, before it is frozen.
type freezeHook struct {
	Units
	hook func()
}

func (u freezeHook) Freeze(ctx context.Context, name string) error {
	u.hook()
	return u.Units.Freeze(ctx, name)
}

// An agent that begins waiting on a command as its soft limit pauses it
// gets its wait: the command is not paused after all.
func TestAWaitAsItPausesKeepsItRunning(t *testing.T) {
	now := time.Unix(1795300000, 0)
	var e *Engine // set before anything freezes
	e = limited(t, &now, func(u Units) Units {
		return freezeHook{Units: u, hook: func() {
			if err := e.WaitOn(t.Context(), "c-1"); err != nil {
				t.Error(err)
			}
		}}
	})
	now = now.Add(11 * time.Minute)
	e.limit(t.Context())
	if s := e.state("c-1"); s != running || e.frozen(t, "hotbed-c-1") {
		t.Errorf("waited on as it paused, c-1 is %v, frozen %v; want running, thawed", s, e.frozen(t, "hotbed-c-1"))
	}
}

// If Stop completes before pause() runs (e.g. between limit() releasing e.mu
// and calling pause()) or while Freeze is in flight, pause() must not
// overwrite cancelled with paused or append a paused event.
func TestPauseDoesNotOverwriteCompletedStop(t *testing.T) {
	t.Run("stop completes before pause runs", func(t *testing.T) {
		now := time.Unix(1795300000, 0)
		e := limited(t, &now)
		now = now.Add(11 * time.Minute)
		e.mu.Lock()
		c := e.commands["c-1"]
		e.mu.Unlock()

		if _, err := e.Stop(t.Context(), "c-1", "cancelled before pause"); err != nil {
			t.Fatal(err)
		}
		e.pause(t.Context(), c)

		got, ok := e.Command("c-1")
		if !ok {
			t.Fatal("c-1 missing")
		}
		if got.State != cancelled {
			t.Fatalf("state after pause() on stopped command = %v, want cancelled", got.State)
		}
		for _, ev := range got.Timeline {
			if ev.What == "paused" {
				t.Errorf("unexpected paused timeline event on stopped command: %+v", got.Timeline)
			}
		}
	})

	t.Run("stop completes during Freeze inside pause", func(t *testing.T) {
		now := time.Unix(1795300000, 0)
		var e *Engine
		e = limited(t, &now, func(u Units) Units {
			return freezeHook{Units: u, hook: func() {
				if _, err := e.Stop(t.Context(), "c-1", "cancelled during pause freeze"); err != nil {
					t.Errorf("Stop during pause Freeze: %v", err)
				}
			}}
		})
		now = now.Add(11 * time.Minute)
		e.limit(t.Context())

		got, ok := e.Command("c-1")
		if !ok {
			t.Fatal("c-1 missing")
		}
		if got.State != cancelled {
			t.Fatalf("state after Stop during pause Freeze = %v, want cancelled", got.State)
		}
		for _, ev := range got.Timeline {
			if ev.What == "paused" {
				t.Errorf("unexpected paused timeline event: %+v", got.Timeline)
			}
		}
	})
}

// thawHook runs a hook once a unit has thawed.
type thawHook struct {
	Units
	hook func()
}

func (u thawHook) Thaw(ctx context.Context, name string) error {
	if err := u.Units.Thaw(ctx, name); err != nil {
		return err
	}
	u.hook()
	return nil
}

// A paused command that ends as soon as waiting on it thaws it is still
// resumed before it ends: its session hears it resumed, then exited, and
// never of an end without its resumption.
func TestAResumedCommandThatEndsAtOnceWasResumed(t *testing.T) {
	now := time.Unix(1795300000, 0)
	var e *Engine // set before anything thaws
	e = limited(t, &now, func(u Units) Units {
		return thawHook{Units: u, hook: func() {
			// It ends the moment it thaws, and the engine sees that before
			// WaitOn goes on.
			e.mu.Lock()
			c := e.commands["c-1"]
			e.mu.Unlock()
			if err := e.cfg.Units.Stop(t.Context(), c.unitName()); err != nil {
				t.Error(err)
			}
			e.observe(t.Context(), c, unit.State{}, e.now())
		}}
	})
	now = now.Add(11 * time.Minute)
	e.limit(t.Context())
	if s := e.state("c-1"); s != paused {
		t.Fatalf("past its soft limit, c-1 is %v, want paused", s)
	}
	if err := e.WaitOn(t.Context(), "c-1"); err != nil {
		t.Fatal(err)
	}
	got, _ := e.Command("c-1")
	var whats []string
	for _, ev := range got.Timeline {
		whats = append(whats, ev.What)
	}
	if i := slices.Index(whats, "resumed"); i < 0 || i != slices.Index(whats, "paused")+1 || api.Live(got.State) {
		t.Errorf("c-1 is %v with timeline %q; want it resumed after its pause, then ended", got.State, whats)
	}
}

// thawFails is units whose thaw fails, leaving the unit frozen.
type thawFails struct{ Units }

func (thawFails) Thaw(context.Context, string) error { return errors.New("cgroup.freeze: busy") }

// A paused command whose unit does not thaw stays paused, as it was, and
// the wait on it fails.
func TestAResumeThatCannotThawLeavesItPaused(t *testing.T) {
	now := time.Unix(1795300000, 0)
	e := limited(t, &now, func(u Units) Units { return thawFails{u} })
	now = now.Add(11 * time.Minute)
	e.limit(t.Context())
	got, _ := e.Command("c-1")
	reason := got.Reason
	if err := e.WaitOn(t.Context(), "c-1"); err == nil || !strings.Contains(err.Error(), "cgroup.freeze: busy") {
		t.Fatalf("WaitOn = %v, want the thaw's failure", err)
	}
	got, _ = e.Command("c-1")
	if got.State != paused || got.Reason != reason || !e.frozen(t, "hotbed-c-1") {
		t.Errorf("c-1 is %v (%q), frozen %v; want paused as before (%q), frozen", got.State, got.Reason, e.frozen(t, "hotbed-c-1"), reason)
	}
	if last := got.Timeline[len(got.Timeline)-1]; last.What != "paused" || !strings.Contains(last.Detail, "could not be thawed") {
		t.Errorf("c-1's last timeline entry = %v, want it paused again", last)
	}
}

// Soft-limit pause reasons and sync-conflict suggestions include -s <session>
// whenever the command/workspace belongs to a session so the hinted CLI command
// works verbatim from another session or thread context, while omitting -s when
// SessionId is empty.
func TestSessionScopedHintsIncludeSessionFlag(t *testing.T) {
	t.Run("paused with session", func(t *testing.T) {
		now := time.Unix(1795300000, 0)
		e := limited(t, &now)
		e.mu.Lock()
		e.commands["c-1"].cmd().SessionId = "swift-fox"
		e.mu.Unlock()
		now = now.Add(11 * time.Minute)
		e.limit(t.Context())
		got, _ := e.Command("c-1")
		want := "paused: it ran 10m0s, its soft time limit, without ending; waiting on it again (hotbed logs -f -s swift-fox c-1) resumes it"
		if got.State != paused || got.Reason != want {
			t.Fatalf("paused reason = %q, want %q", got.Reason, want)
		}
	})

	t.Run("paused without session", func(t *testing.T) {
		now := time.Unix(1795300000, 0)
		e := limited(t, &now)
		now = now.Add(11 * time.Minute)
		e.limit(t.Context())
		got, _ := e.Command("c-1")
		want := "paused: it ran 10m0s, its soft time limit, without ending; waiting on it again (hotbed logs -f c-1) resumes it"
		if got.State != paused || got.Reason != want {
			t.Fatalf("paused reason without session = %q, want %q", got.Reason, want)
		}
	})

	t.Run("syncConflictError with and without session", func(t *testing.T) {
		withSess := syncConflictError("ws-1", "swift-fox", []string{"c-9"})
		if !strings.Contains(withSess.Suggestion, "`hotbed stop -s swift-fox c-9`") {
			t.Fatalf("syncConflictError with session suggestion = %q, want -s swift-fox", withSess.Suggestion)
		}
		withoutSess := syncConflictError("ws-1", "", []string{"c-9"})
		if !strings.Contains(withoutSess.Suggestion, "`hotbed stop c-9`") {
			t.Fatalf("syncConflictError without session suggestion = %q, want no -s flag", withoutSess.Suggestion)
		}
	})
}
