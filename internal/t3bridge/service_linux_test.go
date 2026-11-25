//go:build linux

package t3bridge

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestLinuxServiceLifecycle(t *testing.T) {
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)

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
				active = false
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
		Binary: "/usr/local/bin/hotbed",
		Args:   []string{"t3-bridge", "run", "--exit-on-upgrade"},
	})
	if err != nil {
		t.Fatalf("InstallService: %v", err)
	}
	if !st.Installed || !st.Running {
		t.Fatalf("expected installed+running, got %+v", st)
	}
	unitBytes, err := os.ReadFile(st.UnitPath)
	if err != nil {
		t.Fatalf("read systemd unit: %v", err)
	}
	unit := string(unitBytes)
	if !strings.Contains(unit, `ExecStart="/usr/local/bin/hotbed" "t3-bridge" "run" "--exit-on-upgrade"`) {
		t.Errorf("unexpected systemd unit:\n%s", unit)
	}

	st2, err := UninstallService(ctx)
	if err != nil {
		t.Fatalf("UninstallService: %v", err)
	}
	if st2.Installed || st2.Running {
		t.Fatalf("expected uninstalled+stopped, got %+v", st2)
	}
	if _, err := os.Stat(st.UnitPath); !os.IsNotExist(err) {
		t.Fatalf("expected unit removed, stat err = %v", err)
	}
}
