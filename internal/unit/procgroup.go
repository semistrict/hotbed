package unit

import (
	"errors"
	"log/slog"
	"os/exec"
	"sync"
	"syscall"
)

// ProcGroup is a started command that leads a process group of its own
// (Setpgid or Setsid). It signals the group only while its leader is not
// yet reaped: a reaped leader's ID, and so its group's, may name another
// process by the time a late signal is sent, which would hit it.
type ProcGroup struct {
	cmd    *exec.Cmd
	mu     sync.Mutex
	reaped bool // Wait began reaping the leader: no more signals
}

// NewProcGroup tracks cmd, which has started.
func NewProcGroup(cmd *exec.Cmd) *ProcGroup { return &ProcGroup{cmd: cmd} }

// Signal sends sig to the group, and nothing once its leader is reaped. A
// group already gone is no error, nor is one whose only member is the
// leader, exited but not reaped (macOS answers EPERM for it, and nothing
// else can for the caller's own child).
func (g *ProcGroup) Signal(sig syscall.Signal) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.reaped {
		return nil
	}
	return g.killLocked(sig)
}

func (g *ProcGroup) killLocked(sig syscall.Signal) error {
	err := syscall.Kill(-g.cmd.Process.Pid, sig)
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) {
		return nil
	}
	return err
}

// Wait waits for the leader to exit and reaps it, as cmd.Wait does. With
// killRest it first kills what is left of the group, while the unreaped
// leader still holds the group's ID. From then on Signal sends nothing.
func (g *ProcGroup) Wait(killRest bool) error {
	if err := waitExited(g.cmd.Process.Pid); err != nil {
		// Not knowing it exited, refusing signals now could leave it
		// running unstoppable: reap first, then refuse them.
		slog.Warn("wait for a process to exit without reaping it", "pid", g.cmd.Process.Pid, "err", err)
		werr := g.cmd.Wait()
		g.mu.Lock()
		g.reaped = true
		g.mu.Unlock()
		return werr
	}
	g.mu.Lock()
	if killRest {
		_ = g.killLocked(syscall.SIGKILL)
	}
	g.reaped = true
	g.mu.Unlock()
	return g.cmd.Wait()
}
