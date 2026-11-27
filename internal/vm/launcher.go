package vm

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/unit"
)

// Launcher starts, inspects, lists, and stops Firecracker VMM processes.
// Production Linux uses SystemdLauncher (transient units in hotbed.slice that
// survive hotbed-worker restarts). Tests use a fake Launcher.
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
	// Detail is how an exited VM ended, when its VMM says so in words
	// (machined's); else its ExitCode is Firecracker's.
	Detail string
}

// Why is how an exited VM ended, for people.
func (s ProcessStatus) Why() string {
	if s.Detail != "" {
		return s.Detail
	}
	return ExitReason(s.ExitCode)
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
	_ = unit.ResetFailed(ctx, s.Name)

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
	err := unit.Stop(stopCtx, name)
	if err != nil && strings.Contains(err.Error(), "not loaded") {
		err = nil
	}

	// Verify the unit actually stopped; escalate to SIGKILL if still alive or
	// if systemctl stop timed out / failed.
	checkCtx, checkCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer checkCancel()
	st, stErr := l.Status(checkCtx, name)
	if stErr == nil && !st.Alive() {
		_ = unit.ResetFailed(checkCtx, name)
		return nil
	}

	killErr := l.Kill(checkCtx, name)
	deadline := time.Now().Add(5 * time.Second)
killWait:
	for time.Now().Before(deadline) {
		st, stErr = l.Status(checkCtx, name)
		if stErr == nil && !st.Alive() {
			_ = unit.ResetFailed(checkCtx, name)
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
		_ = unit.ResetFailed(checkCtx, name)
		return nil
	}
	if stErr != nil {
		return fmt.Errorf("verify unit %s stopped: %w (stop=%v, kill=%v)", name, stErr, err, killErr)
	}
	return fmt.Errorf("unit %s still alive after SIGKILL (stop=%v, kill=%v)", name, err, killErr)
}

func (SystemdLauncher) Kill(ctx context.Context, name string) error {
	return unit.Kill(ctx, name)
}

func (SystemdLauncher) Status(ctx context.Context, name string) (ProcessStatus, error) {
	st, err := unit.Status(ctx, name)
	if err != nil {
		return ProcessStatus{}, err
	}
	return processStatus(st), nil
}

func processStatus(st unit.State) ProcessStatus {
	return ProcessStatus{
		Loaded:       st.Loaded,
		Running:      st.Running,
		Deactivating: st.Deactivating,
		Exited:       st.Exited,
		ExitCode:     st.ExitCode,
		Signal:       st.Signal,
		Result:       st.Result,
		OOMKilled:    st.OOMKilled,
	}
}

// List returns the names (without ".service") of all loaded, non-inactive
// hotbed-vm-* service units belonging to slice (or all slices when slice == "").
func (SystemdLauncher) List(ctx context.Context, slice string) ([]string, error) {
	return unit.ListSliceUnits(ctx, "hotbed-vm-", slice)
}
