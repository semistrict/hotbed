//go:build darwin

package t3bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDarwinServiceLifecycle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	origLaunchctl := runLaunchctl
	t.Cleanup(func() { runLaunchctl = origLaunchctl })

	loaded := false
	var calls [][]string
	runLaunchctl = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if len(args) == 0 {
			return nil, errors.New("no args")
		}
		switch args[0] {
		case "print":
			if !loaded {
				return []byte("Could not find service"), errors.New("exit status 113")
			}
			return []byte("state = running"), nil
		case "bootstrap":
			loaded = true
			return nil, nil
		case "bootout":
			loaded = false
			return nil, nil
		default:
			return nil, nil
		}
	}

	ctx := context.Background()
	st, err := InstallService(ctx, ServiceConfig{
		Binary:  "/usr/local/bin/hotbed",
		Args:    []string{"t3-bridge", "run", "--exit-on-upgrade"},
		LogPath: filepath.Join(home, ".hotbed", "t3-bridge.log"),
	})
	if err != nil {
		t.Fatalf("InstallService: %v", err)
	}
	if !st.Installed || !st.Running {
		t.Fatalf("expected installed+running, got %+v", st)
	}
	plistBytes, err := os.ReadFile(st.UnitPath)
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	plist := string(plistBytes)
	for _, want := range []string{
		LaunchAgentLabel,
		"/usr/local/bin/hotbed",
		"<string>t3-bridge</string>",
		"<string>--exit-on-upgrade</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q:\n%s", want, plist)
		}
	}

	st2, err := UninstallService(ctx)
	if err != nil {
		t.Fatalf("UninstallService: %v", err)
	}
	if st2.Installed || st2.Running {
		t.Fatalf("expected uninstalled+stopped, got %+v", st2)
	}
	if _, err := os.Stat(st.UnitPath); !os.IsNotExist(err) {
		t.Fatalf("expected plist removed, stat err = %v", err)
	}
}
