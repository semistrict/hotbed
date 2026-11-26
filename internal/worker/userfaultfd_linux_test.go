//go:build linux

package worker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestEnsureUserfaultfdAccess(t *testing.T) {
	t.Run("skips when /dev/userfaultfd is absent", func(t *testing.T) {
		rulePath := filepath.Join(t.TempDir(), "etc/udev/rules.d/90-hotbed-userfaultfd.rules")
		var cmds []string
		env := userfaultfdEnv{
			devPath:   "/dev/userfaultfd",
			rulePath:  rulePath,
			groupName: "kvm",
			statDev:   func(string) (uint32, fs.FileMode, error) { return 0, 0, fs.ErrNotExist },
			lookupGroup: func(string) (uint32, error) {
				t.Fatal("lookupGroup should not be called when device is absent")
				return 0, nil
			},
			userInGroup: func(string, uint32) (bool, error) {
				t.Fatal("userInGroup should not be called when device is absent")
				return false, nil
			},
			run: func(_ context.Context, name string, args ...string) (string, error) {
				cmds = append(cmds, name+" "+strings.Join(args, " "))
				return "", nil
			},
		}
		if err := ensureUserfaultfdAccess(t.Context(), "ramon", env); err != nil {
			t.Fatalf("ensureUserfaultfdAccess: %v", err)
		}
		if len(cmds) != 0 {
			t.Fatalf("unexpected commands: %v", cmds)
		}
		if _, err := os.Stat(rulePath); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("rule file should not be created when device is absent, stat err = %v", err)
		}
	})

	t.Run("provisions fresh vm1-like host (0600 root:root, user not in kvm)", func(t *testing.T) {
		rulePath := filepath.Join(t.TempDir(), "etc/udev/rules.d/90-hotbed-userfaultfd.rules")
		devGID := uint32(0)
		devPerm := fs.FileMode(0o600)
		inKVM := false
		var cmds []string
		env := userfaultfdEnv{
			devPath:     "/dev/userfaultfd",
			rulePath:    rulePath,
			groupName:   "kvm",
			statDev:     func(string) (uint32, fs.FileMode, error) { return devGID, devPerm, nil },
			lookupGroup: func(string) (uint32, error) { return 991, nil },
			userInGroup: func(u string, gid uint32) (bool, error) {
				if u != "ramon" || gid != 991 {
					return false, fmt.Errorf("unexpected userInGroup(%q, %d)", u, gid)
				}
				return inKVM, nil
			},
			run: func(_ context.Context, name string, args ...string) (string, error) {
				cmd := name + " " + strings.Join(args, " ")
				cmds = append(cmds, cmd)
				switch cmd {
				case "usermod -aG kvm ramon":
					inKVM = true
				case "udevadm trigger --action=change --subsystem-match=misc --sysname-match=userfaultfd":
					devGID = 991
					devPerm = 0o660
				}
				return "", nil
			},
		}

		if err := ensureUserfaultfdAccess(t.Context(), "ramon", env); err != nil {
			t.Fatalf("ensureUserfaultfdAccess: %v", err)
		}
		wantCmds := []string{
			"usermod -aG kvm ramon",
			"udevadm control --reload-rules",
			"udevadm trigger --action=change --subsystem-match=misc --sysname-match=userfaultfd",
			"udevadm settle --timeout=5",
		}
		if !slices.Equal(cmds, wantCmds) {
			t.Fatalf("commands = %v, want %v", cmds, wantCmds)
		}
		gotRule, err := os.ReadFile(rulePath)
		if err != nil {
			t.Fatalf("read rule: %v", err)
		}
		if string(gotRule) != userfaultfdUdevRule {
			t.Fatalf("rule content = %q, want %q", gotRule, userfaultfdUdevRule)
		}
		fi, err := os.Stat(rulePath)
		if err != nil {
			t.Fatalf("stat rule: %v", err)
		}
		if fi.Mode().Perm() != 0o644 {
			t.Fatalf("rule mode = %04o, want 0644", fi.Mode().Perm())
		}

		// Second call on already-configured host runs zero subprocesses.
		cmds = nil
		if err := ensureUserfaultfdAccess(t.Context(), "ramon", env); err != nil {
			t.Fatalf("second ensureUserfaultfdAccess: %v", err)
		}
		if len(cmds) != 0 {
			t.Fatalf("steady-state call ran unexpected commands: %v", cmds)
		}
	})

	t.Run("creates kvm group when absent", func(t *testing.T) {
		rulePath := filepath.Join(t.TempDir(), "etc/udev/rules.d/90-hotbed-userfaultfd.rules")
		hasKVM := false
		inKVM := false
		devGID := uint32(0)
		devPerm := fs.FileMode(0o600)
		var cmds []string
		env := userfaultfdEnv{
			devPath:   "/dev/userfaultfd",
			rulePath:  rulePath,
			groupName: "kvm",
			statDev:   func(string) (uint32, fs.FileMode, error) { return devGID, devPerm, nil },
			lookupGroup: func(string) (uint32, error) {
				if !hasKVM {
					return 0, errUnknownGroup
				}
				return 995, nil
			},
			userInGroup: func(string, uint32) (bool, error) { return inKVM, nil },
			run: func(_ context.Context, name string, args ...string) (string, error) {
				cmd := name + " " + strings.Join(args, " ")
				cmds = append(cmds, cmd)
				switch cmd {
				case "groupadd --system kvm":
					hasKVM = true
				case "usermod -aG kvm alice":
					inKVM = true
				case "udevadm trigger --action=change --subsystem-match=misc --sysname-match=userfaultfd":
					devGID = 995
					devPerm = 0o660
				}
				return "", nil
			},
		}
		if err := ensureUserfaultfdAccess(t.Context(), "alice", env); err != nil {
			t.Fatalf("ensureUserfaultfdAccess: %v", err)
		}
		if len(cmds) == 0 || cmds[0] != "groupadd --system kvm" {
			t.Fatalf("expected groupadd first, got %v", cmds)
		}
	})

	t.Run("retriggers udev when device permissions drift even if rule matches", func(t *testing.T) {
		rulePath := filepath.Join(t.TempDir(), "etc/udev/rules.d/90-hotbed-userfaultfd.rules")
		if err := os.MkdirAll(filepath.Dir(rulePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(rulePath, []byte(userfaultfdUdevRule), 0o644); err != nil {
			t.Fatal(err)
		}
		devGID := uint32(0)
		devPerm := fs.FileMode(0o600)
		var cmds []string
		env := userfaultfdEnv{
			devPath:     "/dev/userfaultfd",
			rulePath:    rulePath,
			groupName:   "kvm",
			statDev:     func(string) (uint32, fs.FileMode, error) { return devGID, devPerm, nil },
			lookupGroup: func(string) (uint32, error) { return 36, nil },
			userInGroup: func(string, uint32) (bool, error) { return true, nil },
			run: func(_ context.Context, name string, args ...string) (string, error) {
				cmd := name + " " + strings.Join(args, " ")
				cmds = append(cmds, cmd)
				if strings.HasPrefix(cmd, "udevadm trigger") {
					devGID = 36
					devPerm = 0o660
				}
				return "", nil
			},
		}
		if err := ensureUserfaultfdAccess(t.Context(), "ramon", env); err != nil {
			t.Fatalf("ensureUserfaultfdAccess: %v", err)
		}
		wantCmds := []string{
			"udevadm control --reload-rules",
			"udevadm trigger --action=change --subsystem-match=misc --sysname-match=userfaultfd",
			"udevadm settle --timeout=5",
		}
		if !slices.Equal(cmds, wantCmds) {
			t.Fatalf("commands = %v, want %v", cmds, wantCmds)
		}
	})

	t.Run("fails hard when device still lacks kvm rw access after udevadm", func(t *testing.T) {
		rulePath := filepath.Join(t.TempDir(), "etc/udev/rules.d/90-hotbed-userfaultfd.rules")
		env := userfaultfdEnv{
			devPath:     "/dev/userfaultfd",
			rulePath:    rulePath,
			groupName:   "kvm",
			statDev:     func(string) (uint32, fs.FileMode, error) { return 0, 0o600, nil },
			lookupGroup: func(string) (uint32, error) { return 991, nil },
			userInGroup: func(string, uint32) (bool, error) { return true, nil },
			run:         func(context.Context, string, ...string) (string, error) { return "", nil },
		}
		err := ensureUserfaultfdAccess(t.Context(), "ramon", env)
		if err == nil || !strings.Contains(err.Error(), "mode=0600") {
			t.Fatalf("err = %v, want diagnostic error naming mode=0600", err)
		}
	})

	t.Run("fails hard when usermod does not grant group membership", func(t *testing.T) {
		rulePath := filepath.Join(t.TempDir(), "etc/udev/rules.d/90-hotbed-userfaultfd.rules")
		env := userfaultfdEnv{
			devPath:     "/dev/userfaultfd",
			rulePath:    rulePath,
			groupName:   "kvm",
			statDev:     func(string) (uint32, fs.FileMode, error) { return 991, 0o660, nil },
			lookupGroup: func(string) (uint32, error) { return 991, nil },
			userInGroup: func(string, uint32) (bool, error) { return false, nil },
			run:         func(context.Context, string, ...string) (string, error) { return "", nil },
		}
		err := ensureUserfaultfdAccess(t.Context(), "ramon", env)
		if err == nil || !strings.Contains(err.Error(), "still not in group") {
			t.Fatalf("err = %v, want membership verification error", err)
		}
	})

	t.Run("rejects empty username", func(t *testing.T) {
		if err := ensureUserfaultfdAccess(t.Context(), "", userfaultfdEnv{}); err == nil {
			t.Fatal("expected error for empty username")
		}
	})
}

// Firecracker runs as the VM user and opens /dev/kvm: on a host whose VM
// user is not in the kvm group (vm1, set up without it), the worker adds it.
func TestEnsureKVMAccess(t *testing.T) {
	kvmEnv := func(devGID uint32, devPerm fs.FileMode, inKVM *bool, cmds *[]string) userfaultfdEnv {
		return userfaultfdEnv{
			devPath:     "/dev/kvm",
			groupName:   "kvm",
			statDev:     func(string) (uint32, fs.FileMode, error) { return devGID, devPerm, nil },
			lookupGroup: func(string) (uint32, error) { return 991, nil },
			userInGroup: func(u string, gid uint32) (bool, error) {
				if u != "cladefsd" || gid != 991 {
					return false, fmt.Errorf("unexpected userInGroup(%q, %d)", u, gid)
				}
				return *inKVM, nil
			},
			run: func(_ context.Context, name string, args ...string) (string, error) {
				cmd := name + " " + strings.Join(args, " ")
				*cmds = append(*cmds, cmd)
				if cmd == "usermod -aG kvm cladefsd" {
					*inKVM = true
				}
				return "", nil
			},
		}
	}

	t.Run("adds the VM user to kvm", func(t *testing.T) {
		inKVM := false
		var cmds []string
		if err := ensureKVMAccess(t.Context(), "cladefsd", kvmEnv(991, 0o660, &inKVM, &cmds)); err != nil {
			t.Fatalf("ensureKVMAccess: %v", err)
		}
		if !slices.Equal(cmds, []string{"usermod -aG kvm cladefsd"}) {
			t.Fatalf("commands = %v, want [usermod -aG kvm cladefsd]", cmds)
		}
	})

	t.Run("runs nothing once the VM user is in kvm", func(t *testing.T) {
		inKVM := true
		var cmds []string
		if err := ensureKVMAccess(t.Context(), "cladefsd", kvmEnv(991, 0o660, &inKVM, &cmds)); err != nil || len(cmds) != 0 {
			t.Fatalf("ensureKVMAccess = %v, commands %v; want nil and none", err, cmds)
		}
	})

	t.Run("fails when kvm cannot open /dev/kvm", func(t *testing.T) {
		inKVM := true
		var cmds []string
		err := ensureKVMAccess(t.Context(), "cladefsd", kvmEnv(0, 0o600, &inKVM, &cmds))
		if err == nil || err.Error() != "/dev/kvm has gid=0 mode=0600; Firecracker (as cladefsd) needs it read-write for group kvm (gid 991)" {
			t.Fatalf("ensureKVMAccess(0600 root) = %v", err)
		}
	})

	t.Run("fails without /dev/kvm", func(t *testing.T) {
		env := userfaultfdEnv{devPath: "/dev/kvm", statDev: func(string) (uint32, fs.FileMode, error) { return 0, 0, fs.ErrNotExist }}
		if err := ensureKVMAccess(t.Context(), "cladefsd", env); err == nil || !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("ensureKVMAccess(no /dev/kvm) = %v, want not exist", err)
		}
	})
}
