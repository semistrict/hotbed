package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Launcher starts, inspects, and stops Firecracker VMM processes.
// Production Linux uses SystemdLauncher (transient units in hotbed.slice that
// survive hotbed-worker restarts). Tests and non-systemd hosts use DirectLauncher.
type Launcher interface {
	Start(ctx context.Context, spec ProcessSpec) error
	Stop(ctx context.Context, name string) error
	Status(ctx context.Context, name string) (ProcessStatus, error)
}

// ProcessSpec describes one Firecracker VMM process to launch.
type ProcessSpec struct {
	Name       string // unit/process identifier (e.g. hotbed-vm-ws-3f2a01)
	Slice      string // systemd slice (e.g. hotbed.slice)
	User       string // non-root host user to run Firecracker as
	Binary     string // path to firecracker binary
	Args       []string
	ConsoleLog string // file receiving Firecracker stdout+stderr (guest ttyS0)
}

// ProcessStatus reports whether a Firecracker VMM is currently running or exited.
type ProcessStatus struct {
	Loaded    bool
	Running   bool
	Exited    bool
	ExitCode  int
	Signal    int
	Result    string
	OOMKilled bool
}

// SystemdLauncher runs each Firecracker VM as a transient systemd service unit
// so VMs outlive hotbed-worker restarts and are accounted under hotbed.slice.
//
// CRITICAL: hotbed NEVER freezes Firecracker host units (which would starve
// cladefsd DAX quarantine unmaps and trigger ExitCodeCladefsdLost=158).
// Parking freezes cgroups inside the guest via hotbed-guestd instead.
type SystemdLauncher struct{}

func (SystemdLauncher) Start(ctx context.Context, s ProcessSpec) error {
	if s.Name == "" || s.Binary == "" {
		return errors.New("vm launcher: Name and Binary are required")
	}
	// Reset any previous failed/exited transient unit with the same name.
	_ = exec.CommandContext(ctx, "systemctl", "reset-failed", s.Name+".service").Run()

	args := []string{
		"--unit=" + s.Name,
		"--service-type=simple",
		"--property=RemainAfterExit=no",
		"--property=KillMode=control-group",
		"--property=TimeoutStopSec=5s",
		"--property=LimitNOFILE=65536",
		"--property=LimitMEMLOCK=infinity",
	}
	if s.Slice != "" {
		args = append(args, "--slice="+s.Slice)
	}
	if s.User != "" {
		args = append(args, "--uid="+s.User)
	}
	if s.ConsoleLog != "" {
		args = append(args,
			"--property=StandardOutput=append:"+s.ConsoleLog,
			"--property=StandardError=append:"+s.ConsoleLog,
		)
	}
	args = append(args, "--", s.Binary)
	args = append(args, s.Args...)

	out, err := exec.CommandContext(ctx, "systemd-run", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemd-run %s: %w (%s)", s.Name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (SystemdLauncher) Stop(ctx context.Context, name string) error {
	out, err := exec.CommandContext(ctx, "systemctl", "stop", name+".service").CombinedOutput()
	_ = exec.CommandContext(ctx, "systemctl", "reset-failed", name+".service").Run()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "not loaded") {
			return nil
		}
		return fmt.Errorf("systemctl stop %s: %w (%s)", name, err, msg)
	}
	return nil
}

func (SystemdLauncher) Status(ctx context.Context, name string) (ProcessStatus, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "show", name+".service",
		"--property=LoadState,ActiveState,SubState,Result,ExecMainCode,ExecMainStatus").CombinedOutput()
	if err != nil {
		return ProcessStatus{}, fmt.Errorf("systemctl show %s: %w (%s)", name, err, strings.TrimSpace(string(out)))
	}
	return parseSystemdProcessStatus(string(out)), nil
}

func parseSystemdProcessStatus(out string) ProcessStatus {
	var st ProcessStatus
	var loadState, activeState, subState, execCode string
	var execStatus int
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "LoadState":
			loadState = v
		case "ActiveState":
			activeState = v
		case "SubState":
			subState = v
		case "Result":
			st.Result = v
		case "ExecMainCode":
			execCode = v
		case "ExecMainStatus":
			execStatus, _ = strconv.Atoi(v)
		}
	}
	st.Loaded = loadState != "" && loadState != "not-found"
	st.Running = activeState == "active" || activeState == "activating"
	st.Exited = !st.Running && (activeState == "inactive" || activeState == "failed" || subState == "dead" || subState == "exited" || subState == "failed")
	if st.Result == "oom-kill" {
		st.OOMKilled = true
	}
	// ExecMainCode is a CLD_* value: 1 exited, 2 killed, 3 dumped core.
	if execCode == "2" || execCode == "3" {
		st.Signal = execStatus
	} else {
		st.ExitCode = execStatus
	}
	return st
}

// DirectLauncher runs Firecracker directly as a child process of the current
// process (used in unit tests or hosts without systemd).
type DirectLauncher struct {
	mu    sync.Mutex
	procs map[string]*directProc
}

type directProc struct {
	cmd      *exec.Cmd
	logFile  *os.File
	done     chan struct{}
	exited   bool
	exitCode int
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

	p := &directProc{cmd: cmd, logFile: logFile, done: make(chan struct{})}
	l.mu.Lock()
	l.procs[s.Name] = p
	l.mu.Unlock()

	go func() {
		err := cmd.Wait()
		code := 0
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else {
				code = 1
			}
		}
		l.mu.Lock()
		p.exited = true
		p.exitCode = code
		l.mu.Unlock()
		if logFile != nil {
			_ = logFile.Close()
		}
		close(p.done)
	}()
	return nil
}

func (l *DirectLauncher) Stop(_ context.Context, name string) error {
	l.mu.Lock()
	p, ok := l.procs[name]
	l.mu.Unlock()
	if !ok {
		return nil
	}
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
	}
	<-p.done
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
	}, nil
}
