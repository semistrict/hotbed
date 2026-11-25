//go:build darwin

package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
)

// The worker VM's supervisor is hotbed worker supervise, which a
// LaunchAgent runs from login on: it holds the controller's Standby call
// and starts or stops the VM as the controller says (Standby).

// qemuVM is the worker VM, as Standby runs it.
type qemuVM struct {
	cfg     VMConfig
	p       VMPaths
	sampler *hostUsageSampler
}

func (vm qemuVM) Running(ctx context.Context) bool {
	_, state, _ := vmProcessState(ctx, vm.p)
	return state == vmProcRunning
}

func (vm qemuVM) Start(ctx context.Context) error {
	// Once begun, a start or stop finishes, so a VM is never left half
	// stopped (the supervisor exiting for a new build, or unloaded).
	return ensureVMRunning(context.WithoutCancel(ctx), vm.cfg, vm.p, os.Stderr)
}

func (vm qemuVM) Stop(ctx context.Context) error {
	return stopVM(context.WithoutCancel(ctx), vm.cfg, vm.p, os.Stderr)
}

// Supervise runs the worker VM in dir as the controller wants it, until ctx
// ends: what the LaunchAgent runs. cluster is the controller's client.
func Supervise(ctx context.Context, dir string, cluster func(controller string) (hotbedv1connect.ClusterServiceClient, error), log *slog.Logger) error {
	_, cfg, p, err := loadVM(dir)
	if err != nil {
		return err
	}
	sleepAfter, err := cfg.SleepAfterDuration()
	if err != nil {
		return fmt.Errorf("%s: %w", p.Config, err)
	}
	c, err := cluster(cfg.Controller)
	if err != nil {
		return err
	}
	vm := qemuVM{cfg: cfg, p: p, sampler: newDarwinHostUsageSampler()}
	cleanStaleVMFiles(ctx, p)
	log.Info("supervising the worker VM", "node", cfg.Hostname, "controller", cfg.Controller, "sleepAfter", sleepAfter)
	Standby(ctx, c, cfg.Hostname, sleepAfter, vm, log)
	return nil
}

// cleanStaleVMFiles removes leftover qemu.pid and qmp.sock files (for example
// after a crash or host reboot) when no QEMU process for this VM is running.
func cleanStaleVMFiles(ctx context.Context, p VMPaths) {
	if _, state, _ := vmProcessState(ctx, p); state != vmProcMissing {
		return
	}
	_ = os.Remove(p.PIDFile)
	_ = os.Remove(qmpSocketPath(p))
}

func launchAgentLabel(baseDir string) string {
	def, err := DefaultDir()
	if err == nil && filepath.Clean(baseDir) == filepath.Clean(def) {
		return "com.semistrict.hotbed.worker"
	}
	sum := sha256.Sum256([]byte(filepath.Clean(baseDir)))
	return "com.semistrict.hotbed.worker." + hex.EncodeToString(sum[:4])
}

func launchAgentPath(baseDir string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel(baseDir)+".plist")
}

// launchDomain is the LaunchAgent's launchd domain: this user's GUI session.
func launchDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

// startSupervisor writes the LaunchAgent that runs this binary's hotbed
// worker supervise on the VM in p and loads it, so it runs now and from
// every login on. The supervisor must be stopped (stopSupervisor).
func startSupervisor(ctx context.Context, p VMPaths) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find the hotbed binary for the LaunchAgent: %w", err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return fmt.Errorf("find the hotbed binary for the LaunchAgent: %w", err)
	}
	plist := launchAgentPath(p.BaseDir)
	if plist == "" {
		return errors.New("could not determine the LaunchAgents directory")
	}
	content := LaunchAgentPlist(launchAgentLabel(p.BaseDir), exe, []string{"worker", "supervise", "--dir", p.BaseDir}, p.StandbyLog)
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(plist, []byte(content), 0o644); err != nil {
		return err
	}
	if err := launchctl(ctx, "bootstrap", launchDomain(), plist); err != nil {
		return api.Errorf(api.CodeInternal, "load the worker VM's supervisor: %v", err).
			WithSuggestion("launchd runs it in your login session (" + launchDomain() + "): log in to this Mac's desktop and run `hotbed worker start`, or set the worker up with --no-autostart to keep the VM always on")
	}
	return nil
}

// supervisorPollInterval is how often stopSupervisor checks launchd after
// bootout. Overridable in tests.
var supervisorPollInterval = 100 * time.Millisecond

// runLaunchctl executes launchctl and returns its combined output. Overridable in tests.
var runLaunchctl = func(ctx context.Context, args ...string) ([]byte, error) {
	bin := findInPaths("launchctl", "/bin", "/usr/bin")
	if bin == "" {
		bin = "/bin/launchctl"
	}
	return exec.CommandContext(ctx, bin, args...).CombinedOutput()
}

// stopSupervisor unloads the LaunchAgent, if loaded, and returns once it is
// gone, so nothing starts or stops the VM until it is loaded again; the VM
// runs on. A LaunchAgent from before the supervisor ran QEMU itself, which
// unloading it kills: the VM is shut down cleanly first.
func stopSupervisor(ctx context.Context, cfg VMConfig, p VMPaths) error {
	target := launchDomain() + "/" + launchAgentLabel(p.BaseDir)
	out, loaded, err := supervisorPrint(ctx, p)
	if err != nil {
		return err
	}
	if !loaded {
		return nil
	}
	if strings.Contains(out, "qemu-system") {
		if err := stopVM(ctx, cfg, p, os.Stderr); err != nil {
			return err
		}
	}
	if err := launchctl(ctx, "bootout", target); err != nil {
		stillLoaded, stateErr := supervisorState(ctx, p)
		if stateErr != nil {
			return fmt.Errorf("%w (verify launchd state: %w)", err, stateErr)
		}
		if !stillLoaded {
			return nil
		}
		return err
	}
	// bootout returns before the job is gone; a bootstrap then fails.
	for range 10 * supervisorExitTimeout {
		stillLoaded, err := supervisorState(ctx, p)
		if err != nil {
			return err
		}
		if !stillLoaded {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(supervisorPollInterval):
		}
	}
	return fmt.Errorf("launchd still runs %s %ds after launchctl bootout", target, supervisorExitTimeout)
}

// disableSupervisor stops the supervisor and removes its LaunchAgent, so it
// does not come back at the next login.
func disableSupervisor(ctx context.Context, cfg VMConfig, p VMPaths) error {
	if err := stopSupervisor(ctx, cfg, p); err != nil {
		return err
	}
	if plist := launchAgentPath(p.BaseDir); plist != "" {
		if err := os.Remove(plist); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// supervisorPrint queries launchd for this VM's LaunchAgent, returning its
// print output and whether it is loaded. It returns ("", false, nil) only when
// launchctl explicitly reports that the service or domain does not exist.
func supervisorPrint(ctx context.Context, p VMPaths) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	target := launchDomain() + "/" + launchAgentLabel(p.BaseDir)
	out, err := runLaunchctl(ctx, "print", target)
	if err == nil {
		return string(out), true, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", false, ctxErr
	}
	if isLaunchctlMissingError(out, err) {
		return "", false, nil
	}
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return "", false, fmt.Errorf("launchctl print %s: %w: %s", target, err, msg)
	}
	return "", false, fmt.Errorf("launchctl print %s: %w", target, err)
}

// supervisorState reports whether this VM's LaunchAgent is loaded in launchd.
// Unexpected launchctl errors and context cancellations are returned rather
// than treated as absent.
func supervisorState(ctx context.Context, p VMPaths) (loaded bool, err error) {
	_, loaded, err = supervisorPrint(ctx, p)
	return loaded, err
}

func isLaunchctlMissingError(out []byte, err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 113 {
		return true
	}
	msg := string(out)
	for _, needle := range []string{
		"Could not find service",
		"No such process",
		"Could not find domain",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// supervised says the LaunchAgent is loaded.
func supervised(ctx context.Context, p VMPaths) bool {
	loaded, err := supervisorState(ctx, p)
	return err == nil && loaded
}

func launchctl(ctx context.Context, args ...string) error {
	out, err := runLaunchctl(ctx, args...)
	if err != nil {
		return fmt.Errorf("launchctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func isDaemonLoaded(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	target := launchDomain() + "/" + DaemonLaunchAgentLabel
	_, err := runLaunchctl(ctx, "print", target)
	return err == nil
}

// DisableLegacyWorkerLaunchAgent stops and removes the standalone
// com.semistrict.hotbed.worker LaunchAgent (without shutting down any running
// VM) when migrating to the unified `hotbed daemon` LaunchAgent.
func DisableLegacyWorkerLaunchAgent(ctx context.Context, dir string) error {
	baseDir, err := ResolveDir(dir)
	if err != nil {
		return err
	}
	p := NewVMPaths(baseDir)
	var cfg VMConfig
	_ = readJSON(p.Config, &cfg)
	return disableSupervisor(ctx, cfg, p)
}

func setVMMarker(path string, on bool) error {
	if path == "" {
		return nil
	}
	if !on {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}

func isVMStoppedByUser(p VMPaths) bool {
	if p.StoppedMarker == "" {
		return false
	}
	_, err := os.Stat(p.StoppedMarker)
	return err == nil
}

// isVMAsleepSinceBoot reports whether Standby intentionally put the VM to sleep
// during the current macOS boot session. Stale asleep markers from before a
// host reboot return false so the daemon boots the VM fresh after reboot.
func isVMAsleepSinceBoot(p VMPaths) bool {
	if p.AsleepMarker == "" {
		return false
	}
	fi, err := os.Stat(p.AsleepMarker)
	if err != nil {
		return false
	}
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return true
	}
	bootTime := time.Unix(tv.Sec, int64(tv.Usec)*1000)
	return !fi.ModTime().Before(bootTime)
}

func vmStateBase(cfg VMConfig) DaemonVMState {
	return DaemonVMState{
		Configured: true,
		Hostname:   cfg.Hostname,
		Arch:       cfg.Arch,
		CPUs:       cfg.CPUs,
		MemoryMB:   cfg.MemoryBytes / (1 << 20),
		Autostart:  cfg.Autostart,
	}
}

// DaemonVMStatus returns a fast, non-SSH summary of the local macOS worker VM.
func DaemonVMStatus(ctx context.Context, dir string) DaemonVMState {
	_, cfg, p, err := loadVM(dir)
	if err != nil {
		return DaemonVMState{Configured: false, State: "unconfigured"}
	}
	st := vmStateBase(cfg)
	_, procState, qmp, err := vmDetailedState(ctx, p)
	if err != nil {
		st.State = "error"
		st.Detail = err.Error()
		return st
	}
	switch procState {
	case vmProcRunning:
		st.State = "running"
	case vmProcPaused:
		st.State = "paused"
		if len(qmp.IOErrors) > 0 {
			st.Detail = strings.Join(qmp.IOErrors, "; ")
		} else if qmp.QMPStatus != "" {
			st.Detail = qmp.QMPStatus
		}
	default:
		switch {
		case isVMStoppedByUser(p):
			st.State = "stopped"
			st.Detail = "stopped by user"
		case isVMAsleepSinceBoot(p):
			st.State = "asleep"
			st.Detail = "sleeping while idle (wakes on demand)"
		default:
			st.State = "stopped"
		}
	}
	return st
}

// StartDaemonVM clears any user-stop marker and boots the configured macOS
// worker VM (used by the menubar app).
func StartDaemonVM(ctx context.Context, dir string) error {
	_, cfg, p, err := loadVM(dir)
	if err != nil {
		return err
	}
	_ = setVMMarker(p.StoppedMarker, false)
	_ = setVMMarker(p.AsleepMarker, false)
	if err := ensureVMRunning(context.WithoutCancel(ctx), cfg, p, os.Stderr); err != nil {
		return err
	}
	if signer, err := loadSSHSigner(p.KeyPriv); err == nil {
		if cl, err := waitForSSH(ctx, cfg, p, signer, os.Stderr); err == nil {
			defer cl.Close()
			_, _ = sshRun(ctx, cl, "sudo systemctl start hotbed-worker.service")
		}
	}
	return nil
}

// StopDaemonVM marks the VM stopped by the user and shuts it down cleanly
// without stopping the unified daemon itself (used by the menubar app).
func StopDaemonVM(ctx context.Context, dir string) error {
	_, cfg, p, err := loadVM(dir)
	if err != nil {
		return err
	}
	_ = setVMMarker(p.StoppedMarker, true)
	_ = setVMMarker(p.AsleepMarker, false)
	return stopVM(context.WithoutCancel(ctx), cfg, p, os.Stderr)
}

type daemonSupervisedVM struct {
	qemuVM
	onState func(DaemonVMState)
}

func (vm daemonSupervisedVM) emit(state, detail string) {
	if vm.onState == nil {
		return
	}
	st := vmStateBase(vm.cfg)
	st.State = state
	st.Detail = detail
	vm.onState(st)
}

func (vm daemonSupervisedVM) Start(ctx context.Context) error {
	_ = setVMMarker(vm.p.AsleepMarker, false)
	vm.emit("starting", "booting worker VM")
	if err := vm.qemuVM.Start(ctx); err != nil {
		vm.emit("error", err.Error())
		return err
	}
	vm.emit("running", "")
	return nil
}

func (vm daemonSupervisedVM) Stop(ctx context.Context) error {
	vm.emit("stopping", "powering off idle VM")
	if err := vm.qemuVM.Stop(ctx); err != nil {
		vm.emit("error", err.Error())
		return err
	}
	_ = setVMMarker(vm.p.AsleepMarker, true)
	vm.emit("asleep", "sleeping while idle (wakes on demand)")
	return nil
}

// SuperviseVMForDaemon watches the local macOS worker VM configuration under
// dir, auto-starts the VM on boot/daemon startup (unless explicitly stopped or
// sleeping since this boot), runs Standby + host-usage reporting + paused-VM
// watching, and reports state transitions to onState until ctx is cancelled.
func SuperviseVMForDaemon(
	ctx context.Context,
	dir string,
	cluster func(controller string) (hotbedv1connect.ClusterServiceClient, error),
	log *slog.Logger,
	onState func(DaemonVMState),
) {
	if log == nil {
		log = slog.Default()
	}
	emit := func(st DaemonVMState) {
		if onState != nil {
			onState(st)
		}
	}

	var (
		subCancel context.CancelFunc
		subWG     sync.WaitGroup
		activeKey string
	)
	stopSub := func() {
		if subCancel != nil {
			subCancel()
			subWG.Wait()
			subCancel = nil
			activeKey = ""
		}
	}
	defer stopSub()

	reconcile := func() {
		_, cfg, p, err := loadVM(dir)
		if err != nil {
			stopSub()
			emit(DaemonVMState{Configured: false, State: "unconfigured"})
			return
		}
		if !cfg.Autostart || isVMStoppedByUser(p) {
			stopSub()
			emit(DaemonVMStatus(ctx, dir))
			return
		}

		key := fmt.Sprintf("%s|%s|%s", cfg.Hostname, cfg.Controller, cfg.SleepAfter)
		if subCancel != nil && activeKey == key {
			emit(DaemonVMStatus(ctx, dir))
			return
		}
		stopSub()

		sleepAfter, err := cfg.SleepAfterDuration()
		if err != nil {
			st := vmStateBase(cfg)
			st.State = "error"
			st.Detail = err.Error()
			emit(st)
			return
		}
		cl, err := cluster(cfg.Controller)
		if err != nil {
			st := vmStateBase(cfg)
			st.State = "error"
			st.Detail = err.Error()
			emit(st)
			return
		}

		subCtx, cancel := context.WithCancel(ctx)
		subCancel = cancel
		activeKey = key

		vm := daemonSupervisedVM{
			qemuVM:  qemuVM{cfg: cfg, p: p, sampler: newDarwinHostUsageSampler()},
			onState: emit,
		}
		cleanStaleVMFiles(subCtx, p)

		subWG.Add(2)
		go func() {
			defer subWG.Done()
			WatchPausedVM(subCtx, dir, log)
		}()
		go func() {
			defer subWG.Done()
			// Auto-start the VM on Mac boot / daemon startup unless Standby
			// intentionally put it to sleep during this boot session.
			if !vm.Running(subCtx) && !isVMAsleepSinceBoot(p) {
				log.Info("daemon: starting configured macOS worker VM", "node", cfg.Hostname)
				if err := vm.Start(subCtx); err != nil {
					log.Warn("daemon: auto-start worker VM failed; continuing in standby", "node", cfg.Hostname, "err", err)
				} else if signer, err := loadSSHSigner(p.KeyPriv); err == nil {
					if sshCl, err := waitForSSH(subCtx, cfg, p, signer, os.Stderr); err == nil {
						_, _ = sshRun(subCtx, sshCl, "sudo systemctl start hotbed-worker.service")
						_ = sshCl.Close()
					}
				}
			}
			emit(DaemonVMStatus(subCtx, dir))
			log.Info("daemon: supervising macOS worker VM", "node", cfg.Hostname, "controller", cfg.Controller, "sleepAfter", sleepAfter)
			Standby(subCtx, cl, cfg.Hostname, sleepAfter, vm, log)
		}()
	}

	reconcile()
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			reconcile()
		}
	}
}
