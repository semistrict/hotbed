//go:build darwin

package t3bridge

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

func launchAgentPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", LaunchAgentLabel+".plist"), nil
}

// InstallService writes and bootstraps the macOS LaunchAgent for hotbed t3-bridge.
func InstallService(ctx context.Context, cfg ServiceConfig) (ServiceStatus, error) {
	plist, err := launchAgentPath()
	if err != nil {
		return ServiceStatus{}, err
	}
	if cfg.LogPath == "" {
		cfg.LogPath, err = DefaultLogPath()
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

	// Boot out any existing instance before bootstrapping the updated plist.
	_ = bootoutIfLoaded(ctx)

	content := setup.LaunchAgentPlist(LaunchAgentLabel, cfg.Binary, cfg.Args, cfg.LogPath)
	if err := os.WriteFile(plist, []byte(content), 0o644); err != nil {
		return ServiceStatus{}, err
	}
	if out, err := runLaunchctl(ctx, "bootstrap", launchDomain(), plist); err != nil {
		return ServiceStatus{}, fmt.Errorf("launchctl bootstrap %s: %w: %s", plist, err, strings.TrimSpace(string(out)))
	}
	return GetServiceStatus(ctx)
}

// UninstallService stops and removes the macOS LaunchAgent for hotbed t3-bridge.
func UninstallService(ctx context.Context) (ServiceStatus, error) {
	plist, err := launchAgentPath()
	if err != nil {
		return ServiceStatus{}, err
	}
	if err := bootoutIfLoaded(ctx); err != nil {
		return ServiceStatus{}, err
	}
	if err := os.Remove(plist); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return ServiceStatus{}, err
	}
	return GetServiceStatus(ctx)
}

// GetServiceStatus inspects the macOS LaunchAgent state.
func GetServiceStatus(ctx context.Context) (ServiceStatus, error) {
	plist, err := launchAgentPath()
	if err != nil {
		return ServiceStatus{}, err
	}
	logPath, _ := DefaultLogPath()
	_, statErr := os.Stat(plist)
	installed := statErr == nil
	running, err := isLaunchAgentLoaded(ctx)
	if err != nil {
		return ServiceStatus{}, err
	}
	return ServiceStatus{
		Platform:  runtime.GOOS,
		Installed: installed,
		Running:   running,
		Label:     LaunchAgentLabel,
		UnitPath:  plist,
		LogPath:   logPath,
	}, nil
}

func isLaunchAgentLoaded(ctx context.Context) (bool, error) {
	target := launchDomain() + "/" + LaunchAgentLabel
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

func bootoutIfLoaded(ctx context.Context) error {
	loaded, err := isLaunchAgentLoaded(ctx)
	if err != nil || !loaded {
		return err
	}
	target := launchDomain() + "/" + LaunchAgentLabel
	if out, err := runLaunchctl(ctx, "bootout", target); err != nil && !isLaunchctlMissing(out, err) {
		stillLoaded, checkErr := isLaunchAgentLoaded(ctx)
		if checkErr == nil && !stillLoaded {
			return nil
		}
		return fmt.Errorf("launchctl bootout %s: %w: %s", target, err, strings.TrimSpace(string(out)))
	}
	for range 50 {
		stillLoaded, err := isLaunchAgentLoaded(ctx)
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
