package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// reclaimingUnits wraps fake.Units to simulate Linux cgroup memory.reclaim
// pushing anonymous RAM to swap, with optional error injection, partial-reclaim
// floor (leaveAnon), and physical memory accounting on sys.
type reclaimingUnits struct {
	*fake.Units
	mu         sync.Mutex
	reclaimErr error
	leaveAnon  int64
	sys        *fake.System
}

func (u *reclaimingUnits) Reclaim(cgroup string, n int64) error {
	u.mu.Lock()
	err := u.reclaimErr
	leave := u.leaveAnon
	sys := u.sys
	u.mu.Unlock()
	if err != nil {
		return err
	}
	m, merr := u.Units.Memory(cgroup)
	if merr != nil {
		return merr
	}
	reclaimable := max(int64(0), m.Anon-leave)
	moved := min(n, reclaimable)
	if moved <= 0 {
		return nil
	}
	name := strings.TrimSuffix(filepath.Base(cgroup), ".service")
	u.Units.SetAnon(name, m.Anon-moved)
	u.Units.SetSwap(name, m.Swap+moved)
	if sys != nil {
		cur, _ := sys.Meminfo()
		sys.SetUsed(max(int64(0), cur.Used()-moved))
	}
	return nil
}

func parkReclaimEngine(t *testing.T) (*Engine, *reclaimingUnits, *fake.System, *time.Time) {
	t.Helper()
	baseUnits := &fake.Units{}
	sys := fake.NewSystem()
	units := &reclaimingUnits{Units: baseUnits}
	store := &fake.Store{Root: t.TempDir()}
	now := time.Unix(1795400000, 0)
	e, err := NewEngine(t.Context(), EngineConfig{
		Dir: t.TempDir(), NodeID: "n", Host: "asahimbp",
		Store: store, BaseImage: fake.BaseImage(store), Units: units, System: sys, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	for _, w := range []string{"a", "b", "c"} {
		if err := os.MkdirAll(filepath.Join(store.Path("ws/"+w), "work", "app"), 0o755); err != nil {
			t.Fatal(err)
		}
		e.workspaces[w] = &workspace{rec: &pb.WorkspaceRecord{Workspace: &pb.Workspace{Id: w, Project: "app", Repos: []*pb.WorkspaceRepo{{Repo: "app"}}, State: idle}, Volume: "ws/" + w}}
	}
	return e, units, sys, &now
}

func startWithLimits(t *testing.T, e *Engine, units *fake.Units, now *time.Time, id, ws, kind string, limits *pb.CommandLimits) *command {
	t.Helper()
	*now = now.Add(time.Second)
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: id, WorkspaceId: ws, Args: []string{"sleep", "60"}, Kind: kind, Limits: limits,
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-"+id) })
	e.step(t.Context())
	if s := e.state(id); s != running {
		t.Fatalf("%s is %v (%q), want running", id, s, e.commands[id].cmd().Reason)
	}
	return e.commands[id]
}

// TestSamePriorityDoesNotPreempt verifies that commands of equal priority do
// not preempt each other and follow normal FIFO / memory rules.
func TestSamePriorityDoesNotPreempt(t *testing.T) {
	e, units, _, now := parkEngine(t)
	_ = startWithLimits(t, e, units, now, "norm-1", "a", "build", &pb.CommandLimits{Priority: 10})
	units.SetAnon("hotbed-norm-1", 14<<30)
	*now = now.Add(time.Second)
	e.step(t.Context())

	*now = now.Add(time.Second)
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "norm-2", WorkspaceId: "b", Args: []string{"sleep", "60"}, Kind: "build",
		Limits: &pb.CommandLimits{Priority: 10},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-norm-2") })
	e.step(t.Context())

	if s1, s2 := e.state("norm-1"), e.state("norm-2"); s1 != running || s2 != queued {
		t.Fatalf("same priority: norm-1=%v norm-2=%v, want running/queued", s1, s2)
	}
}

// TestQueueAdmissionOrdersByPriorityThenFIFO verifies that higher-priority
// queued commands are admitted ahead of earlier-queued lower-priority commands.
func TestQueueAdmissionOrdersByPriorityThenFIFO(t *testing.T) {
	e, units, _, now := parkEngine(t)
	// Queue low first (t+1), then high (t+2), each wanting 8 GiB on a 16 GiB
	// node (admitLimit 13.76 GiB, so only one can start at once).
	*now = now.Add(time.Second)
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "q-low", WorkspaceId: "a", Args: []string{"sleep", "60"},
		Limits: &pb.CommandLimits{Priority: -10, MemoryLowBytes: 8 << 30},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-q-low") })

	*now = now.Add(time.Second)
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "q-high", WorkspaceId: "b", Args: []string{"sleep", "60"},
		Limits: &pb.CommandLimits{Priority: 10, MemoryLowBytes: 8 << 30},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-q-high") })

	e.admit(t.Context())
	if sHigh, sLow := e.state("q-high"), e.state("q-low"); sHigh != running || sLow != queued {
		t.Fatalf("queue admission order: q-high=%v q-low=%v, want running/queued", sHigh, sLow)
	}
}

// TestPriorityDoesNotBypassHostStarvationAndRejectsOutOfRange verifies that
// urgent commands cannot preempt past host starvation and that Exec rejects
// out-of-range priorities.
func TestPriorityDoesNotBypassHostStarvationAndRejectsOutOfRange(t *testing.T) {
	e, units, _, now := parkEngine(t)

	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "bad-hi", WorkspaceId: "a", Args: []string{"sleep", "1"},
		Limits: &pb.CommandLimits{Priority: 101},
	}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Fatalf("priority 101 err = %v, want INVALID_ARGS", err)
	}
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "bad-lo", WorkspaceId: "a", Args: []string{"sleep", "1"},
		Limits: &pb.CommandLimits{Priority: -101},
	}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Fatalf("priority -101 err = %v, want INVALID_ARGS", err)
	}

	_ = startWithLimits(t, e, units, now, "low-1", "a", "build", &pb.CommandLimits{Priority: -10})
	e.SetHostUsage(&pb.HostUsage{
		HostCpuBusy: 0.95,
		HostCpus:    8,
		SampledAt:   now.UnixMilli(),
	})

	*now = now.Add(time.Second)
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "urgent-1", WorkspaceId: "b", Args: []string{"sleep", "60"},
		Limits: &pb.CommandLimits{Priority: 20},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-urgent-1") })
	e.step(t.Context())

	if sUrgent, sLow := e.state("urgent-1"), e.state("low-1"); sUrgent != queued || sLow != running {
		t.Fatalf("under host starvation: urgent-1=%v low-1=%v, want queued/running", sUrgent, sLow)
	}
}
