package unit

import (
	"bufio"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startGroupLeftBehind starts a group leader that leaves a sleeping child
// in its group and exits; it returns the child's PID.
func startGroupLeftBehind(t *testing.T) (*ProcGroup, int) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "sleep 30 & echo $!; exec 1>&-; exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	return NewProcGroup(cmd), child
}

// outlives reports whether pid still runs after waiting up to d for it to
// go (a killed orphan is reaped by init, not by the test).
func outlives(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		if time.Now().After(deadline) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// Once its leader is reaped a group's ID may name someone else: Signal
// sends nothing then, even to members the group still has.
func TestProcGroupSignalsNothingOnceReaped(t *testing.T) {
	g, child := startGroupLeftBehind(t)
	if err := g.Wait(false); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if err := g.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("Signal after reaping: %v", err)
	}
	if !outlives(child, 500*time.Millisecond) {
		t.Fatal("Signal after the leader was reaped killed a member of its old group")
	}
}

// Wait(true) kills what the leader left in its group while its unreaped
// leader still holds the group's ID.
func TestProcGroupWaitKillsTheRest(t *testing.T) {
	g, child := startGroupLeftBehind(t)
	if err := g.Wait(true); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if outlives(child, 5*time.Second) {
		t.Fatal("the leader's group member outlived Wait(true)")
	}
}

// Before its leader exits, Signal reaches the whole group.
func TestProcGroupSignalReachesTheGroupWhileItRuns(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	g := NewProcGroup(cmd)
	if err := g.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	err := g.Wait(false)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
		t.Fatalf("Wait = %v, want killed by SIGTERM", err)
	}
}
