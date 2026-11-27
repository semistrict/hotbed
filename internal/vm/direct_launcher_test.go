package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// DirectLauncher runs Firecracker directly as a child process of the current
// process. Tests only: production launches VMs with SystemdLauncher.
type DirectLauncher struct {
	mu    sync.Mutex
	procs map[string]*directProc
}

type directProc struct {
	slice    string
	cmd      *exec.Cmd
	logFile  *os.File
	done     chan struct{}
	exited   bool
	exitCode int
	signal   int
	result   string
}

// NewDirectLauncher creates a DirectLauncher.
func NewDirectLauncher() *DirectLauncher {
	return &DirectLauncher{procs: map[string]*directProc{}}
}

func (l *DirectLauncher) Start(_ context.Context, s ProcessSpec) error {
	l.mu.Lock()
	if old, ok := l.procs[s.Name]; ok && !old.exited {
		l.mu.Unlock()
		return fmt.Errorf("vm process %s already running", s.Name)
	}
	l.mu.Unlock()

	cmd := exec.Command(s.Binary, s.Args...)
	var logFile *os.File
	if s.ConsoleLog != "" {
		f, err := os.OpenFile(s.ConsoleLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("open console log %s: %w", s.ConsoleLog, err)
		}
		logFile = f
		cmd.Stdout = f
		cmd.Stderr = f
	}
	if err := cmd.Start(); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return fmt.Errorf("start %s: %w", s.Binary, err)
	}

	p := &directProc{slice: s.Slice, cmd: cmd, logFile: logFile, done: make(chan struct{})}
	l.mu.Lock()
	l.procs[s.Name] = p
	l.mu.Unlock()

	go func() {
		err := cmd.Wait()
		code := 0
		sig := 0
		result := "success"
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
					sig = int(ws.Signal())
					result = "signal"
				} else {
					code = ee.ExitCode()
					if code != 0 {
						result = "exit-code"
					}
				}
			} else {
				code = 1
				result = "exit-code"
			}
		}
		l.mu.Lock()
		p.exited = true
		p.exitCode = code
		p.signal = sig
		p.result = result
		l.mu.Unlock()
		if logFile != nil {
			_ = logFile.Close()
		}
		close(p.done)
	}()
	return nil
}

func (l *DirectLauncher) Stop(ctx context.Context, name string) error {
	l.mu.Lock()
	p, ok := l.procs[name]
	l.mu.Unlock()
	if !ok {
		return nil
	}
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
	case <-timer.C:
	}
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGKILL)
	}
	killTimer := time.NewTimer(2 * time.Second)
	defer killTimer.Stop()
	select {
	case <-p.done:
		return nil
	case <-killTimer.C:
		return fmt.Errorf("vm process %s did not exit after SIGKILL", name)
	}
}

func (l *DirectLauncher) Kill(_ context.Context, name string) error {
	l.mu.Lock()
	p, ok := l.procs[name]
	l.mu.Unlock()
	if !ok || p.cmd.Process == nil {
		return nil
	}
	_ = p.cmd.Process.Signal(syscall.SIGKILL)
	return nil
}

func (l *DirectLauncher) Status(_ context.Context, name string) (ProcessStatus, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.procs[name]
	if !ok {
		return ProcessStatus{}, nil
	}
	return ProcessStatus{
		Loaded:   true,
		Running:  !p.exited,
		Exited:   p.exited,
		ExitCode: p.exitCode,
		Signal:   p.signal,
		Result:   p.result,
	}, nil
}

func (l *DirectLauncher) List(_ context.Context, slice string) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for name, p := range l.procs {
		if p.exited || !strings.HasPrefix(name, "hotbed-vm-") {
			continue
		}
		if slice != "" && p.slice != slice {
			continue
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out, nil
}

func TestDirectLauncherCapturesSignal(t *testing.T) {
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep binary not found")
	}
	dl := NewDirectLauncher()
	ctx := context.Background()
	if err := dl.Start(ctx, ProcessSpec{
		Name:   "hotbed-vm-sigtest",
		Binary: sleepBin,
		Args:   []string{"60"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := dl.Kill(ctx, "hotbed-vm-sigtest"); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st, err := dl.Status(ctx, "hotbed-vm-sigtest")
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if st.Exited {
			if st.Signal != int(syscall.SIGKILL) || st.Result != "signal" || !st.DiedAbnormally() {
				t.Fatalf("Status after SIGKILL = %+v, want signal=9 result=signal", st)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for killed process to exit")
}
