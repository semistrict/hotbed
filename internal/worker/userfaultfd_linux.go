//go:build linux

package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

const (
	userfaultfdDevPath      = "/dev/userfaultfd"
	userfaultfdUdevRulePath = "/etc/udev/rules.d/90-hotbed-userfaultfd.rules"
	userfaultfdGroupName    = "kvm"
	userfaultfdUdevRule     = "# Managed by hotbed: grant the worker command user (via the kvm group)\n" +
		"# read-write access to /dev/userfaultfd for workspaces.\n" +
		"KERNEL==\"userfaultfd\", GROUP=\"kvm\", MODE=\"0660\"\n"
)

var errUnknownGroup = errors.New("unknown group")

type userfaultfdEnv struct {
	devPath     string
	rulePath    string
	groupName   string
	statDev     func(path string) (gid uint32, perm fs.FileMode, err error)
	lookupGroup func(name string) (gid uint32, err error)
	userInGroup func(username string, gid uint32) (bool, error)
	run         func(ctx context.Context, name string, args ...string) (string, error)
}

func defaultUserfaultfdEnv() userfaultfdEnv {
	return userfaultfdEnv{
		devPath:     userfaultfdDevPath,
		rulePath:    userfaultfdUdevRulePath,
		groupName:   userfaultfdGroupName,
		statDev:     statDeviceOwnerMode,
		lookupGroup: lookupOSGroupGID,
		userInGroup: osUserInGroup,
		run:         runHostCmd,
	}
}

// EnsureUserfaultfdAccess grants username read-write access to /dev/userfaultfd
// through the kvm group and a persistent udev rule (matching Linux KVM hosts),
// applying the rule live via udevadm. It is a no-op when the host kernel does
// not expose /dev/userfaultfd, and issues no subprocesses once configured.
func EnsureUserfaultfdAccess(ctx context.Context, username string) error {
	return ensureUserfaultfdAccess(ctx, username, defaultUserfaultfdEnv())
}

func ensureUserfaultfdAccess(ctx context.Context, username string, env userfaultfdEnv) error {
	if strings.TrimSpace(username) == "" {
		return errors.New("command user is required")
	}
	devGID, devPerm, err := env.statDev(env.devPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", env.devPath, err)
	}

	kvmGID, err := ensureGroupMember(ctx, env, username)
	if err != nil {
		return err
	}

	ruleChanged, err := writeUdevRuleIfChanged(env.rulePath, []byte(userfaultfdUdevRule))
	if err != nil {
		return fmt.Errorf("write %s: %w", env.rulePath, err)
	}

	needDeviceUpdate := devGID != kvmGID || devPerm&0o660 != 0o660
	if !ruleChanged && !needDeviceUpdate {
		return nil
	}

	if _, err := env.run(ctx, "udevadm", "control", "--reload-rules"); err != nil {
		return fmt.Errorf("reload udev rules: %w", err)
	}
	if _, err := env.run(ctx, "udevadm", "trigger", "--action=change", "--subsystem-match=misc", "--sysname-match=userfaultfd"); err != nil {
		return fmt.Errorf("trigger udev for %s: %w", env.devPath, err)
	}
	if _, err := env.run(ctx, "udevadm", "settle", "--timeout=5"); err != nil {
		return fmt.Errorf("settle udev for %s: %w", env.devPath, err)
	}

	devGID, devPerm, err = env.statDev(env.devPath)
	if err != nil {
		return fmt.Errorf("re-stat %s after udev trigger: %w", env.devPath, err)
	}
	if devGID != kvmGID || devPerm&0o660 != 0o660 {
		return fmt.Errorf("%s has gid=%d mode=%04o after applying %s (want gid=%d (%s) mode 0660); check for conflicting udev rules in %s",
			env.devPath, devGID, devPerm, env.rulePath, kvmGID, env.groupName, filepath.Dir(env.rulePath))
	}
	return nil
}

// ensureGroupMember makes username a member of env.groupName, creating the
// group when the host has none, and returns the group's gid.
func ensureGroupMember(ctx context.Context, env userfaultfdEnv, username string) (uint32, error) {
	gid, err := env.lookupGroup(env.groupName)
	if err != nil {
		if !errors.Is(err, errUnknownGroup) {
			return 0, fmt.Errorf("look up group %q: %w", env.groupName, err)
		}
		if _, err := env.run(ctx, "groupadd", "--system", env.groupName); err != nil {
			return 0, fmt.Errorf("create group %q: %w", env.groupName, err)
		}
		gid, err = env.lookupGroup(env.groupName)
		if err != nil {
			return 0, fmt.Errorf("look up group %q after groupadd: %w", env.groupName, err)
		}
	}
	inGroup, err := env.userInGroup(username, gid)
	if err != nil {
		return 0, fmt.Errorf("check %q membership in group %q: %w", username, env.groupName, err)
	}
	if inGroup {
		return gid, nil
	}
	if _, err := env.run(ctx, "usermod", "-aG", env.groupName, username); err != nil {
		return 0, fmt.Errorf("add %q to group %q: %w", username, env.groupName, err)
	}
	inGroup, err = env.userInGroup(username, gid)
	if err != nil {
		return 0, fmt.Errorf("verify %q membership in group %q: %w", username, env.groupName, err)
	}
	if !inGroup {
		return 0, fmt.Errorf("user %q is still not in group %q (gid %d) after usermod", username, env.groupName, gid)
	}
	return gid, nil
}

const kvmDevPath = "/dev/kvm"

// EnsureKVMAccess makes vmUser, the user Firecracker runs as, a member of the
// kvm group that owns /dev/kvm, so Firecracker can open it. It fails when
// /dev/kvm is missing or not group read-writable by kvm: the host cannot run
// the VM runtime then.
func EnsureKVMAccess(ctx context.Context, vmUser string) error {
	env := defaultUserfaultfdEnv()
	env.devPath = kvmDevPath
	return ensureKVMAccess(ctx, vmUser, env)
}

func ensureKVMAccess(ctx context.Context, vmUser string, env userfaultfdEnv) error {
	devGID, devPerm, err := env.statDev(env.devPath)
	if err != nil {
		return fmt.Errorf("the VM runtime needs %s: %w", env.devPath, err)
	}
	kvmGID, err := ensureGroupMember(ctx, env, vmUser)
	if err != nil {
		return err
	}
	if devPerm&0o006 == 0o006 {
		return nil // anyone may open it
	}
	if devGID != kvmGID || devPerm&0o060 != 0o060 {
		return fmt.Errorf("%s has gid=%d mode=%04o; Firecracker (as %s) needs it read-write for group %s (gid %d)",
			env.devPath, devGID, devPerm, vmUser, env.groupName, kvmGID)
	}
	return nil
}

// writeUdevRuleIfChanged writes want to rulePath atomically in rulePath's
// directory (so SELinux inherits udev_rules_t) when missing, non-regular, wrong
// mode, or different in content.
func writeUdevRuleIfChanged(rulePath string, want []byte) (bool, error) {
	if fi, err := os.Lstat(rulePath); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm() == 0o644 {
		if existing, err := os.ReadFile(rulePath); err == nil && bytes.Equal(existing, want) {
			return false, nil
		}
	}
	dir := filepath.Dir(rulePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(rulePath)+".*")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return false, err
	}
	if _, err := tmp.Write(want); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmpName, rulePath); err != nil {
		return false, err
	}
	return true, nil
}

func statDeviceOwnerMode(path string) (uint32, fs.FileMode, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("cannot read owner/group of %s", path)
	}
	return st.Gid, fi.Mode().Perm(), nil
}

func lookupOSGroupGID(name string) (uint32, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		var uge user.UnknownGroupError
		if errors.As(err, &uge) {
			return 0, fmt.Errorf("%w: %v", errUnknownGroup, err)
		}
		return 0, err
	}
	gid, err := strconv.ParseUint(g.Gid, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parse gid %q for group %q: %w", g.Gid, name, err)
	}
	return uint32(gid), nil
}

func osUserInGroup(username string, gid uint32) (bool, error) {
	u, err := lookupOSUser(username)
	if err != nil {
		return false, err
	}
	want := strconv.FormatUint(uint64(gid), 10)
	if u.Gid == want {
		return true, nil
	}
	gids, err := u.GroupIds()
	if err != nil {
		return false, err
	}
	return slices.Contains(gids, want), nil
}

func runHostCmd(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(out.String())
		if msg != "" {
			return out.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
		}
		return out.String(), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out.String(), nil
}
