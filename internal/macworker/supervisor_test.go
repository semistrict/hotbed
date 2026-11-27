package macworker

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
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

// A daemon upgrade hands off machined and the worker so active VMs survive,
// the next daemon adopts both PIDs, a newly pinned machined release waits
// until machined has no active VMs before restarting, and a full shutdown
// stops the adopted processes cleanly.
func TestSupervisorHandoffPreservesMachinedAndWorker(t *testing.T) {
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

	// 1. Daemon #1 exits for an upgrade handoff: machined and worker stay alive.
	seen1 := make(states, 64)
	sup1 := &Supervisor{
		Paths: p, Release: Release{Tag: "cladefs-v1"}, Exe: hotbed, Machined: machined,
		Poll: 20 * time.Millisecond, MachinedStop: 5 * time.Second, WorkerStop: 5 * time.Second,
		PreserveOnExit: func() bool { return true },
		OnState:        func(st State) { seen1 <- st },
	}
	ctx1, cancel1 := context.WithCancel(t.Context())
	done1 := make(chan struct{})
	go func() { sup1.Run(ctx1); close(done1) }()

	first := seen1.await(t, running, "running on daemon 1")
	t.Cleanup(func() {
		_ = syscall.Kill(first.WorkerPID, syscall.SIGKILL)
		_ = syscall.Kill(first.MachinedPID, syscall.SIGKILL)
	})
	cancel1()
	<-done1
	if !alive(first.MachinedPID) || !alive(first.WorkerPID) {
		t.Fatalf("handoff killed machined (%d alive=%v) or worker (%d alive=%v)",
			first.MachinedPID, alive(first.MachinedPID), first.WorkerPID, alive(first.WorkerPID))
	}
	if got := p.ReadState(); got.State != "running" {
		t.Fatalf("ReadState after handoff = %+v, want running", got)
	}

	// 2. Daemon #2 starts on the same release and adopts both PIDs untouched.
	seen2 := make(states, 64)
	sup2 := &Supervisor{
		Paths: p, Release: Release{Tag: "cladefs-v1"}, Exe: hotbed, Machined: machined,
		Poll: 20 * time.Millisecond, MachinedStop: 5 * time.Second, WorkerStop: 5 * time.Second,
		PreserveOnExit: func() bool { return true },
		OnState:        func(st State) { seen2 <- st },
	}
	ctx2, cancel2 := context.WithCancel(t.Context())
	done2 := make(chan struct{})
	go func() { sup2.Run(ctx2); close(done2) }()

	second := seen2.await(t, running, "adopted on daemon 2")
	if second.MachinedPID != first.MachinedPID || second.WorkerPID != first.WorkerPID {
		t.Fatalf("daemon 2 did not adopt running processes: got (%d, %d), want (%d, %d)",
			second.MachinedPID, second.WorkerPID, first.MachinedPID, first.WorkerPID)
	}
	cancel2()
	<-done2

	// 3. Daemon #3 pins a newer machined release: defers while busy, upgrades once idle.
	var idle atomic.Bool
	seen3 := make(states, 64)
	sup3 := &Supervisor{
		Paths: p, Release: Release{Tag: "cladefs-v2"}, Exe: hotbed, Machined: machined,
		Poll: 20 * time.Millisecond, MachinedStop: 5 * time.Second, WorkerStop: 5 * time.Second,
		MachinedIdle: func(context.Context) (bool, error) { return idle.Load(), nil },
		OnState:      func(st State) { seen3 <- st },
	}
	ctx3, cancel3 := context.WithCancel(t.Context())
	done3 := make(chan struct{})
	go func() { sup3.Run(ctx3); close(done3) }()

	adopted := seen3.await(t, running, "adopted on daemon 3 before machined upgrade")
	if adopted.MachinedPID != first.MachinedPID || adopted.Release != "cladefs-v1" {
		t.Fatalf("daemon 3 initial state = %+v, want adopted pid %d release cladefs-v1", adopted, first.MachinedPID)
	}
	time.Sleep(100 * time.Millisecond)
	if !alive(first.MachinedPID) {
		t.Fatalf("daemon 3 killed machined %d while VMs were active", first.MachinedPID)
	}

	idle.Store(true)
	upgraded := seen3.await(t, func(st State) bool {
		return running(st) && st.MachinedPID != first.MachinedPID && st.Release == "cladefs-v2"
	}, "upgraded to cladefs-v2 once idle")
	t.Cleanup(func() {
		_ = syscall.Kill(upgraded.WorkerPID, syscall.SIGKILL)
		_ = syscall.Kill(upgraded.MachinedPID, syscall.SIGKILL)
	})
	if alive(first.MachinedPID) || alive(first.WorkerPID) {
		t.Fatalf("old machined (%d) or worker (%d) survived machined upgrade", first.MachinedPID, first.WorkerPID)
	}

	// 4. Full shutdown (PreserveOnExit nil) stops both processes cleanly.
	cancel3()
	<-done3
	if alive(upgraded.MachinedPID) || alive(upgraded.WorkerPID) {
		t.Fatalf("full shutdown left machined (%d) or worker (%d) running", upgraded.MachinedPID, upgraded.WorkerPID)
	}
}
