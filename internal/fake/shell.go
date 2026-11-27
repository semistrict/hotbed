package fake

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"github.com/semistrict/hotbed/internal/pty"
	"github.com/semistrict/hotbed/internal/unit"
)

// StartShell starts a local process (with a real PTY when sio.PTY is true, or
// piped stdio otherwise) rewriting /work binds just like Start.
func (u *Units) StartShell(ctx context.Context, s unit.Spec, sio unit.ShellIO) (unit.ShellHandle, error) {
	if s.RootDir == "" {
		return nil, errors.New("root directory is required")
	}
	rewrite := rewriter(s.RootDir, s.Binds, s.ReadOnlyBinds)

	argv := s.Argv
	if len(argv) == 0 {
		if _, err := exec.LookPath("/bin/bash"); err == nil {
			argv = []string{"/bin/bash"}
		} else {
			argv = []string{"/bin/sh"}
		}
	}
	args := make([]string, len(argv))
	for i, a := range argv {
		args[i] = rewrite(a)
	}

	cmd := exec.Command("/bin/sh", append([]string{"-c", `PATH="${HOTBED_PATH:+$HOTBED_PATH:}$PATH"; exec "$@"`, "hotbed"}, args...)...)
	cmd.Dir = rewrite(s.Dir)
	cmd.Env = os.Environ()
	if sio.Term != "" {
		cmd.Env = append(cmd.Env, "TERM="+sio.Term)
	} else if sio.PTY {
		cmd.Env = append(cmd.Env, "TERM=xterm-256color")
	}
	if s.Tmp != "" {
		cmd.Env = append(cmd.Env, "TMPDIR="+s.Tmp)
	}
	for _, e := range s.Env {
		cmd.Env = append(cmd.Env, rewrite(e))
	}
	paths := func(dirs []string) string {
		out := make([]string, len(dirs))
		for i, d := range dirs {
			out[i] = rewrite(d)
		}
		return strings.Join(out, ":")
	}
	cmd.Env = append(cmd.Env, "HOTBED_PATH="+paths(s.PathPrefix))

	var ptmx, pts *os.File
	var err error
	if sio.PTY {
		ptmx, pts, _, err = pty.Open()
		if err != nil {
			return nil, fmt.Errorf("open pty: %w", err)
		}
		if sio.Cols > 0 || sio.Rows > 0 {
			_ = pty.Resize(ptmx, sio.Cols, sio.Rows)
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = pts, pts, pts
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	} else {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = sio.Stdin, sio.Stdout, sio.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}

	if err := cmd.Start(); err != nil {
		if pts != nil {
			pts.Close()
		}
		if ptmx != nil {
			ptmx.Close()
		}
		return nil, err
	}
	if pts != nil {
		pts.Close()
	}

	u.mu.Lock()
	if u.specs == nil {
		u.specs = map[string]unit.Spec{}
	}
	u.specs[s.Name] = s
	u.mu.Unlock()

	h := &fakeShell{
		group: unit.NewProcGroup(cmd),
		ptmx:  ptmx,
		done:  make(chan struct{}),
	}
	go func() {
		// What the shell left in its group dies with it, while its
		// unreaped leader still holds the group's ID.
		err := h.group.Wait(true)
		code := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				code = 128 + int(ws.Signal())
			} else {
				code = exitErr.ExitCode()
			}
		} else if err != nil {
			code = 1
		}
		h.exitCode = code
		close(h.done)
	}()
	if ctx != nil && ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				_ = h.Close()
			case <-h.done:
			}
		}()
	}
	return h, nil
}

type fakeShell struct {
	group     *unit.ProcGroup
	ptmx      *os.File
	done      chan struct{}
	exitCode  int
	closeOnce sync.Once
}

func (h *fakeShell) PTY() *os.File { return h.ptmx }

func (h *fakeShell) Resize(cols, rows uint16) error {
	if h.ptmx == nil {
		return nil
	}
	return pty.Resize(h.ptmx, cols, rows)
}

func (h *fakeShell) Wait() int {
	<-h.done
	return h.exitCode
}

func (h *fakeShell) Close() error {
	h.closeOnce.Do(func() {
		if h.ptmx != nil {
			h.ptmx.Close()
		}
		_ = h.group.Signal(syscall.SIGKILL)
	})
	return nil
}
