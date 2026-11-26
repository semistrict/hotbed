package macworker

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/statedir"
)

// fakeMachined serves its --socket (an empty file) until SIGTERM.
const fakeMachined = `#!/bin/sh
while [ $# -gt 0 ]; do case $1 in --socket) sock=$2; shift;; esac; shift; done
trap 'rm -f "$sock"; exit 0' TERM
: > "$sock"
while :; do sleep 0.05; done
`

// fakeHotbed is hotbed serve: it runs until SIGTERM.
const fakeHotbed = `#!/bin/sh
trap 'exit 0' TERM
while :; do sleep 0.05; done
`

// states records each State a Supervisor reports, for a test to wait on.
type states chan State

func (s states) await(t *testing.T, want func(State) bool, what string) State {
	t.Helper()
	timeout := time.After(20 * time.Second)
	for {
		select {
		case st := <-s:
			if want(st) {
				return st
			}
		case <-timeout:
			t.Fatalf("no state %s within 20 s", what)
		}
	}
}

func running(st State) bool { return st.State == "running" && st.MachinedPID > 0 && st.WorkerPID > 0 }

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// The Supervisor runs machined and then the worker, restarts both when
// machined ends (its machines, the worker's VMs, end with it), keeps both
// stopped while the worker is stopped, and stops both when it returns.
func TestSupervisorRunsMachinedAndTheWorker(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "hbmw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	p := NewPaths(base)
	bin := filepath.Join(base, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	machined, hotbed := filepath.Join(bin, "machined"), filepath.Join(bin, "hotbed")
	for path, script := range map[string]string{machined: fakeMachined, hotbed: fakeHotbed} {
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := statedir.Prepare(p.Worker); err != nil {
		t.Fatal(err)
	}
	if err := statedir.Commit(p.Worker, statedir.Config{Worker: &statedir.WorkerConfig{ClusterID: "c", VM: p.VMConfig()}}); err != nil {
		t.Fatal(err)
	}

	seen := make(states, 64)
	sup := &Supervisor{
		Paths: p, Exe: hotbed, Machined: machined, Poll: 20 * time.Millisecond,
		MachinedStop: 5 * time.Second, WorkerStop: 5 * time.Second,
		OnState: func(st State) { seen <- st },
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()

	first := seen.await(t, running, "running")
	if got := p.ReadState(); got.State != "running" || got.WorkerPID != first.WorkerPID {
		t.Fatalf("ReadState = %+v, want the running state %+v", got, first)
	}

	// machined ends: both start again, the worker beside the new machined.
	if err := syscall.Kill(first.MachinedPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	second := seen.await(t, func(st State) bool { return running(st) && st.MachinedPID != first.MachinedPID }, "running on a new machined")
	if second.WorkerPID == first.WorkerPID || alive(first.WorkerPID) {
		t.Fatalf("worker %d still runs beside a new machined %d (was %d)", first.WorkerPID, second.MachinedPID, second.WorkerPID)
	}

	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	seen.await(t, func(st State) bool { return st.State == "stopped" && !st.Autostart }, "stopped")
	if alive(second.MachinedPID) || alive(second.WorkerPID) {
		t.Fatalf("machined %d or worker %d runs while stopped", second.MachinedPID, second.WorkerPID)
	}

	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	third := seen.await(t, running, "running after Start")

	cancel()
	<-done
	if alive(third.MachinedPID) || alive(third.WorkerPID) {
		t.Fatalf("machined %d or worker %d outlived the Supervisor", third.MachinedPID, third.WorkerPID)
	}
	if got := p.ReadState(); got.State != "stopped" {
		t.Fatalf("ReadState after Run = %+v, want stopped", got)
	}
}
