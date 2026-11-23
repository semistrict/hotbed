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
	"time"

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
