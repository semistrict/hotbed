//go:build linux

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
	"strings"
)

var runSystemctl = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
}

func systemdUnitPathFor(name string) (string, error) {
	cfgDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfgDir, "systemd", "user", name), nil
}

func renderSystemdUnit(binary string, args []string) string {
	quoted := make([]string, 0, len(args)+1)
	quoted = append(quoted, quoteSystemdArg(binary))
	for _, a := range args {
		quoted = append(quoted, quoteSystemdArg(a))
	}
	return fmt.Sprintf(`[Unit]
Description=Hotbed host daemon
After=network-online.target

[Service]
Type=simple
ExecStart=%s
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
`, strings.Join(quoted, " "))
}

func quoteSystemdArg(s string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
	return `"` + escaped + `"`
}

// InstallService writes and enables the unified systemd user unit
// (`hotbed-daemon.service`), migrating away from any legacy `hotbed-t3-bridge.service`.
func InstallService(ctx context.Context, cfg ServiceConfig) (ServiceStatus, error) {
	unitPath, err := systemdUnitPathFor(SystemdUnitName)
	if err != nil {
		return ServiceStatus{}, err
	}
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return ServiceStatus{}, err
	}

	// Clean up legacy unit if present.
	_, _ = runSystemctl(ctx, "--user", "disable", "--now", LegacyT3BridgeUnitName)
	if oldPath, err := systemdUnitPathFor(LegacyT3BridgeUnitName); err == nil {
		_ = os.Remove(oldPath)
	}

	if err := os.WriteFile(unitPath, []byte(renderSystemdUnit(cfg.Binary, cfg.Args)), 0o644); err != nil {
		return ServiceStatus{}, err
	}
	if out, err := runSystemctl(ctx, "--user", "daemon-reload"); err != nil {
		return ServiceStatus{}, fmt.Errorf("systemctl --user daemon-reload: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := runSystemctl(ctx, "--user", "enable", "--now", SystemdUnitName); err != nil {
		return ServiceStatus{}, fmt.Errorf("systemctl --user enable --now %s: %w: %s", SystemdUnitName, err, strings.TrimSpace(string(out)))
	}
	return GetServiceStatus(ctx, cfg.BaseDir)
}

// UninstallService disables, stops, and removes the unified systemd user unit.
func UninstallService(ctx context.Context, baseDir string) (ServiceStatus, error) {
	_, _ = runSystemctl(ctx, "--user", "disable", "--now", LegacyT3BridgeUnitName)
	if oldPath, err := systemdUnitPathFor(LegacyT3BridgeUnitName); err == nil {
		_ = os.Remove(oldPath)
	}
	unitPath, err := systemdUnitPathFor(SystemdUnitName)
	if err != nil {
		return ServiceStatus{}, err
	}
	_, _ = runSystemctl(ctx, "--user", "disable", "--now", SystemdUnitName)
	if err := os.Remove(unitPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return ServiceStatus{}, err
	}
	_, _ = runSystemctl(ctx, "--user", "daemon-reload")
	return GetServiceStatus(ctx, baseDir)
}

// GetServiceStatus inspects the unified systemd user unit state.
func GetServiceStatus(ctx context.Context, baseDir string) (ServiceStatus, error) {
	unitPath, err := systemdUnitPathFor(SystemdUnitName)
	if err != nil {
		return ServiceStatus{}, err
	}
	statusPath, _ := StatusFilePath(baseDir)
	_, statErr := os.Stat(unitPath)
	installed := statErr == nil
	running := false
	if out, err := runSystemctl(ctx, "--user", "is-active", SystemdUnitName); err == nil && strings.TrimSpace(string(out)) == "active" {
		running = true
	}
	return ServiceStatus{
		Platform:   runtime.GOOS,
		Installed:  installed,
		Running:    running,
		Label:      SystemdUnitName,
		UnitPath:   unitPath,
		StatusFile: statusPath,
		Live:       LoadLiveSnapshot(statusPath),
	}, nil
}
