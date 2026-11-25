//go:build darwin

package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDarwinDaemonServiceLifecycleAndLegacyMigration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	baseDir := filepath.Join(home, ".hotbed")

	// Seed a legacy t3-bridge plist to verify InstallService removes it.
	legacyPlist := filepath.Join(home, "Library", "LaunchAgents", LegacyT3BridgeLabel+".plist")
	if err := os.MkdirAll(filepath.Dir(legacyPlist), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPlist, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}

	origLaunchctl := runLaunchctl
	t.Cleanup(func() { runLaunchctl = origLaunchctl })

	loaded := map[string]bool{
		LegacyT3BridgeLabel: true,
	}
	runLaunchctl = func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) == 0 {
			return nil, errors.New("no args")
		}
		switch args[0] {
		case "print":
			label := args[1][strings.LastIndex(args[1], "/")+1:]
			if !loaded[label] {
				return []byte("Could not find service"), errors.New("exit status 113")
			}
			return []byte("state = running"), nil
		case "bootstrap":
			if strings.Contains(args[2], LaunchAgentLabel) {
				loaded[LaunchAgentLabel] = true
			}
			return nil, nil
		case "bootout":
			label := args[1][strings.LastIndex(args[1], "/")+1:]
			delete(loaded, label)
			return nil, nil
		default:
			return nil, nil
		}
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
	if !st.Installed || !st.Running || st.Label != LaunchAgentLabel {
		t.Fatalf("expected installed+running unified daemon, got %+v", st)
	}
	if _, err := os.Stat(legacyPlist); !os.IsNotExist(err) {
		t.Fatalf("expected legacy t3-bridge plist removed, stat err = %v", err)
	}
	plistBytes, err := os.ReadFile(st.UnitPath)
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	plist := string(plistBytes)
	for _, want := range []string{
		LaunchAgentLabel,
		"/usr/local/bin/hotbed",
		"<string>daemon</string>",
		"<string>--exit-on-upgrade</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q:\n%s", want, plist)
		}
	}

	st2, err := UninstallService(ctx, baseDir)
	if err != nil {
		t.Fatalf("UninstallService: %v", err)
	}
	if st2.Installed || st2.Running {
		t.Fatalf("expected uninstalled+stopped, got %+v", st2)
	}
}

func TestDarwinMenubarSwiftCompiles(t *testing.T) {
	if _, err := os.Stat("/usr/bin/swiftc"); err != nil {
		t.Skip("swiftc not installed")
	}
	dir := t.TempDir()
	bin, err := ensureMenubarBinary(context.Background(), dir)
	if err != nil {
		t.Fatalf("ensureMenubarBinary: %v", err)
	}
	if st, err := os.Stat(bin); err != nil || st.Mode()&0o111 == 0 {
		t.Fatalf("compiled menubar binary %s invalid: stat=%v err=%v", bin, st, err)
	}
	// Second call hits cache cleanly.
	bin2, err := ensureMenubarBinary(context.Background(), dir)
	if err != nil || bin2 != bin {
		t.Fatalf("second ensureMenubarBinary = (%q, %v), want (%q, nil)", bin2, err, bin)
	}
}
