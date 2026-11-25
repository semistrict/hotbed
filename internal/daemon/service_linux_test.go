//go:build linux

package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxDaemonServiceLifecycleAndLegacyMigration(t *testing.T) {
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	baseDir := t.TempDir()

	legacyUnit := filepath.Join(cfgHome, "systemd", "user", LegacyT3BridgeUnitName)
	if err := os.MkdirAll(filepath.Dir(legacyUnit), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyUnit, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origSystemctl := runSystemctl
	t.Cleanup(func() { runSystemctl = origSystemctl })

	active := false
	runSystemctl = func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "--user" {
			switch args[1] {
			case "enable":
				active = true
				return nil, nil
			case "disable":
				if len(args) >= 4 && args[3] == SystemdUnitName {
					active = false
				}
				return nil, nil
			case "is-active":
				if active {
					return []byte("active\n"), nil
				}
				return []byte("inactive\n"), errors.New("exit status 3")
			}
		}
		return nil, nil
	}

	ctx := context.Background()
	st, err := InstallService(ctx, ServiceConfig{
		BaseDir: baseDir,
		Binary:  "/usr/local/bin/hotbed",
		Args:    []string{"daemon", "run", "--exit-on-upgrade"},
	})
	if err != nil {
		t.Fatalf("InstallService: %v", err)
	}
	if !st.Installed || !st.Running || st.Label != SystemdUnitName {
		t.Fatalf("expected installed+running, got %+v", st)
	}
	if _, err := os.Stat(legacyUnit); !os.IsNotExist(err) {
		t.Fatalf("expected legacy unit removed, stat err = %v", err)
	}
	unitBytes, err := os.ReadFile(st.UnitPath)
	if err != nil {
		t.Fatalf("read systemd unit: %v", err)
	}
	if !strings.Contains(string(unitBytes), `ExecStart="/usr/local/bin/hotbed" "daemon" "run" "--exit-on-upgrade"`) {
		t.Errorf("unexpected systemd unit:\n%s", string(unitBytes))
	}

	st2, err := UninstallService(ctx, baseDir)
	if err != nil {
		t.Fatalf("UninstallService: %v", err)
	}
	if st2.Installed || st2.Running {
		t.Fatalf("expected uninstalled+stopped, got %+v", st2)
	}
}
