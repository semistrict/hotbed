//go:build darwin

package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/setup"
)

var runLaunchctl = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "/bin/launchctl", args...).CombinedOutput()
}

func launchDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func launchAgentPathFor(label string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

// InstallService writes and bootstraps the unified macOS LaunchAgent
// (`com.semistrict.hotbed.daemon`), migrating away from any legacy separate
// `t3-bridge` or `worker` LaunchAgents so only one Hotbed service runs.
func InstallService(ctx context.Context, cfg ServiceConfig) (ServiceStatus, error) {
	plist, err := launchAgentPathFor(LaunchAgentLabel)
	if err != nil {
		return ServiceStatus{}, err
	}
	if cfg.LogPath == "" {
		cfg.LogPath, err = DefaultLogPath(cfg.BaseDir)
		if err != nil {
			return ServiceStatus{}, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(cfg.LogPath), 0o700); err != nil {
		return ServiceStatus{}, err
	}
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return ServiceStatus{}, err
	}

	// Cleanly migrate away from legacy standalone LaunchAgents.
	_ = removeLaunchAgent(ctx, LegacyT3BridgeLabel)
	_ = setup.DisableLegacyWorkerLaunchAgent(ctx, cfg.BaseDir)

	// Boot out any existing instance before bootstrapping the updated plist.
	_ = bootoutIfLoaded(ctx, LaunchAgentLabel)

	content := setup.LaunchAgentPlist(LaunchAgentLabel, cfg.Binary, cfg.Args, cfg.LogPath)
	if err := os.WriteFile(plist, []byte(content), 0o644); err != nil {
		return ServiceStatus{}, err
	}
	if out, err := runLaunchctl(ctx, "bootstrap", launchDomain(), plist); err != nil {
		return ServiceStatus{}, fmt.Errorf("launchctl bootstrap %s: %w: %s", plist, err, strings.TrimSpace(string(out)))
	}
	return GetServiceStatus(ctx, cfg.BaseDir)
}

// UninstallService stops and removes the unified macOS LaunchAgent (and any
// legacy t3-bridge LaunchAgent).
func UninstallService(ctx context.Context, baseDir string) (ServiceStatus, error) {
	_ = removeLaunchAgent(ctx, LegacyT3BridgeLabel)
	if err := removeLaunchAgent(ctx, LaunchAgentLabel); err != nil {
		return ServiceStatus{}, err
	}
	return GetServiceStatus(ctx, baseDir)
}

// GetServiceStatus inspects the unified macOS LaunchAgent state and loads the
// live daemon snapshot when running.
func GetServiceStatus(ctx context.Context, baseDir string) (ServiceStatus, error) {
	plist, err := launchAgentPathFor(LaunchAgentLabel)
	if err != nil {
		return ServiceStatus{}, err
	}
	logPath, _ := DefaultLogPath(baseDir)
	statusPath, _ := StatusFilePath(baseDir)
	_, statErr := os.Stat(plist)
	installed := statErr == nil
	running, err := isLaunchAgentLoaded(ctx, LaunchAgentLabel)
	if err != nil {
		return ServiceStatus{}, err
	}
	return ServiceStatus{
		Platform:   runtime.GOOS,
		Installed:  installed,
		Running:    running,
		Label:      LaunchAgentLabel,
		UnitPath:   plist,
		LogPath:    logPath,
		StatusFile: statusPath,
		Live:       LoadLiveSnapshot(statusPath),
	}, nil
}

func removeLaunchAgent(ctx context.Context, label string) error {
	if err := bootoutIfLoaded(ctx, label); err != nil {
		return err
	}
	plist, err := launchAgentPathFor(label)
	if err != nil {
		return err
	}
	if err := os.Remove(plist); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func isLaunchAgentLoaded(ctx context.Context, label string) (bool, error) {
	target := launchDomain() + "/" + label
	out, err := runLaunchctl(ctx, "print", target)
	if err == nil {
		return true, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if isLaunchctlMissing(out, err) {
		return false, nil
	}
	return false, fmt.Errorf("launchctl print %s: %w: %s", target, err, strings.TrimSpace(string(out)))
}

func bootoutIfLoaded(ctx context.Context, label string) error {
	loaded, err := isLaunchAgentLoaded(ctx, label)
	if err != nil || !loaded {
		return err
	}
	target := launchDomain() + "/" + label
	if out, err := runLaunchctl(ctx, "bootout", target); err != nil && !isLaunchctlMissing(out, err) {
		stillLoaded, checkErr := isLaunchAgentLoaded(ctx, label)
		if checkErr == nil && !stillLoaded {
			return nil
		}
		return fmt.Errorf("launchctl bootout %s: %w: %s", target, err, strings.TrimSpace(string(out)))
	}
	for range 50 {
		stillLoaded, err := isLaunchAgentLoaded(ctx, label)
		if err != nil || !stillLoaded {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil
}

func isLaunchctlMissing(out []byte, err error) bool {
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 113 {
		return true
	}
	msg := string(out)
	for _, needle := range []string{"Could not find service", "No such process", "Could not find domain"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}
