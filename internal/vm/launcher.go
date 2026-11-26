package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Launcher starts, inspects, lists, and stops Firecracker VMM processes.
// Production Linux uses SystemdLauncher (transient units in hotbed.slice that
// survive hotbed-worker restarts). Tests and non-systemd hosts use DirectLauncher.
type Launcher interface {
	Start(ctx context.Context, spec ProcessSpec) error
	Stop(ctx context.Context, name string) error
	Status(ctx context.Context, name string) (ProcessStatus, error)
	List(ctx context.Context, slice string) ([]string, error)
}

// Killer is an optional Launcher extension that escalates a stubborn VMM
// process or systemd unit to SIGKILL when graceful Stop fails or leaves it alive.
type Killer interface {
	Kill(ctx context.Context, name string) error
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
	Loaded       bool
	Running      bool
	Deactivating bool
	Exited       bool
	ExitCode     int
	Signal       int
	Result       string
	OOMKilled    bool
}

// Alive reports whether the VMM process may still be executing (active,
// activating, or still tearing down in systemd's deactivating state).
func (s ProcessStatus) Alive() bool {
	return s.Running || s.Deactivating
}

// DiedAbnormally reports whether the VMM process exited due to a signal,
// OOM kill, core dump, or non-zero exit code.
func (s ProcessStatus) DiedAbnormally() bool {
	if !s.Exited {
		return false
	}
	return s.Signal != 0 || s.OOMKilled || s.ExitCode != 0 ||
		s.Result == "signal" || s.Result == "core-dump" || s.Result == "oom-kill" || s.Result == "exit-code"
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

func (l SystemdLauncher) Stop(ctx context.Context, name string) error {
	stopCtx := ctx
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		stopCtx, cancel = context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
	}
	out, err := exec.CommandContext(stopCtx, "systemctl", "stop", name+".service").CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "not loaded") {
			err = nil
		} else {
			err = fmt.Errorf("systemctl stop %s: %w (%s)", name, err, msg)
		}
	}

	// Verify the unit actually stopped; escalate to SIGKILL if still alive or
	// if systemctl stop timed out / failed.
	checkCtx, checkCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer checkCancel()
	st, stErr := l.Status(checkCtx, name)
	if stErr == nil && !st.Alive() {
		_ = exec.CommandContext(checkCtx, "systemctl", "reset-failed", name+".service").Run()
		return nil
	}

	killErr := l.Kill(checkCtx, name)
	deadline := time.Now().Add(5 * time.Second)
killWait:
	for time.Now().Before(deadline) {
		st, stErr = l.Status(checkCtx, name)
		if stErr == nil && !st.Alive() {
			_ = exec.CommandContext(checkCtx, "systemctl", "reset-failed", name+".service").Run()
			return nil
		}
		select {
		case <-checkCtx.Done():
			break killWait
		case <-time.After(50 * time.Millisecond):
		}
	}
	st, stErr = l.Status(checkCtx, name)
	if stErr == nil && !st.Alive() {
		_ = exec.CommandContext(checkCtx, "systemctl", "reset-failed", name+".service").Run()
		return nil
	}
	if stErr != nil {
		return fmt.Errorf("verify unit %s stopped: %w (stop=%v, kill=%v)", name, stErr, err, killErr)
	}
	return fmt.Errorf("unit %s still alive after SIGKILL (stop=%v, kill=%v)", name, err, killErr)
}

func (SystemdLauncher) Kill(ctx context.Context, name string) error {
	out, err := exec.CommandContext(ctx, "systemctl", "kill", "--kill-who=all", "--signal=SIGKILL", name+".service").CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "not loaded") || strings.Contains(msg, "not active") {
			return nil
		}
		return fmt.Errorf("systemctl kill %s: %w (%s)", name, err, msg)
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

// List returns the names (without ".service") of all loaded, non-inactive
// hotbed-vm-* service units belonging to slice (or all slices when slice == "").
func (SystemdLauncher) List(ctx context.Context, slice string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "list-units",
		"--type=service", "--all", "--no-legend", "--plain", "--full", "--", "hotbed-vm-*.service").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("systemctl list-units hotbed-vm-*.service: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	candidates := parseVMListUnits(string(out))
	if len(candidates) == 0 {
		return nil, nil
	}
	args := make([]string, 0, len(candidates)+3)
	args = append(args, "show", "--property=Id,Slice,LoadState,ActiveState", "--")
	args = append(args, candidates...)
	showOut, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("systemctl show hotbed-vm units: %w (%s)", err, strings.TrimSpace(string(showOut)))
	}
	return parseVMSliceUnits(string(showOut), slice), nil
}

func parseVMListUnits(out string) []string {
	var names []string
	seen := make(map[string]bool)
	for _, raw := range strings.Split(out, "\n") {
		for _, f := range strings.Fields(raw) {
			if !strings.HasSuffix(f, ".service") {
				continue
			}
			if strings.HasPrefix(f, "hotbed-vm-") && !seen[f] {
				seen[f] = true
				names = append(names, f)
			}
			break
		}
	}
	return names
}

func parseVMSliceUnits(out, slice string) []string {
	var res []string
	seen := make(map[string]bool)
	var block []string
	flush := func() {
		if len(block) == 0 {
			return
		}
		kv := make(map[string]string, len(block))
		for _, line := range block {
			if k, v, ok := strings.Cut(line, "="); ok {
				kv[k] = v
			}
		}
		block = block[:0]
		if kv["LoadState"] != "loaded" || kv["ActiveState"] == "inactive" {
			return
		}
		if slice != "" && kv["Slice"] != slice {
			return
		}
		id := kv["Id"]
		if !strings.HasPrefix(id, "hotbed-vm-") || !strings.HasSuffix(id, ".service") {
			return
		}
		name := strings.TrimSuffix(id, ".service")
		if !seen[name] {
			seen[name] = true
			res = append(res, name)
		}
	}
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		block = append(block, line)
	}
	flush()
	slices.Sort(res)
	return res
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
	st.Deactivating = activeState == "deactivating"
	st.Exited = !st.Running && !st.Deactivating && (activeState == "inactive" || activeState == "failed" || subState == "dead" || subState == "exited" || subState == "failed")
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
