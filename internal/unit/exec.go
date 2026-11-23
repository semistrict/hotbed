package unit

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"strconv"
	"strings"
)

// ExecPrefix starts the name of every scope Exec runs in: what Exec runs
// ends with its caller, so one found when the worker starts is left from a
// worker that is gone (StopLeftExecs).
const ExecPrefix = "hotbed-x-"

// ExecSpec is a command the worker runs inside a workspace rootfs on its own
// behalf (such as git or sync-apply in a workspace checkout).
type ExecSpec struct {
	Root          string
	Binds         []Bind
	ReadOnlyBinds []Bind
	Dir           string
	User          string
	Argv          []string
}

// Validate checks c before any of it is acted on.
func (c ExecSpec) Validate() error {
	if !path.IsAbs(c.Root) || path.Clean(c.Root) == "/" {
		return fmt.Errorf("chroot root %q is not an absolute path below /", c.Root)
	}
	if len(c.Argv) == 0 {
		return errors.New("chroot exec has no command")
	}
	if !path.IsAbs(c.Dir) {
		return fmt.Errorf("chroot working directory %q is not absolute", c.Dir)
	}
	for _, b := range c.Binds {
		if err := validateBind(b); err != nil {
			return err
		}
	}
	for _, b := range c.ReadOnlyBinds {
		if err := validateBind(b); err != nil {
			return err
		}
	}
	return nil
}

func validateBind(b Bind) error {
	if !path.IsAbs(b.Source) {
		return fmt.Errorf("bind source %q is not absolute", b.Source)
	}
	if !path.IsAbs(b.Target) || path.Clean(b.Target) == "/" {
		return fmt.Errorf("bind target %q is not an absolute path below /", b.Target)
	}
	return nil
}

// StopLeftExecs stops every Exec scope there is: called as the worker
// starts, those of the worker it replaces, which its end did not end (the
// end of an Exec's context does; a worker's death does not).
func StopLeftExecs(ctx context.Context) error {
	_, err := run(ctx, "systemctl", "stop", "--", ExecPrefix+"*.scope")
	return err
}

// Exec returns a command that runs c chrooted in c.Root inside a transient
// systemd scope under slice. The caller sets stdio and environment (PATH is
// preserved into the chroot); canceling ctx terminates the command.
func Exec(ctx context.Context, name, slice string, c ExecSpec) (*exec.Cmd, error) {
	if !strings.HasPrefix(name, ExecPrefix) {
		return nil, fmt.Errorf("Exec scope %s is not named %s...", name, ExecPrefix)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	u, err := resolveUser(c.User)
	if err != nil {
		return nil, err
	}
	unshare, err := lookPath("unshare")
	if err != nil {
		return nil, fmt.Errorf("find unshare: %w", err)
	}
	// Scope units have no ExecContext, so systemd-run --scope rejects
	// execution properties such as OOMScoreAdjust= and SELinuxContext=
	// (board 7921).
	args := []string{
		"--quiet", "--scope", "--collect",
		"--unit=" + name,
		"--slice=" + cmp.Or(slice, DefaultSlice),
		"--description=" + description(c.Argv),
		"--property=KillMode=control-group",
		"--",
		unshare, "--mount", "--propagation", "slave", "--",
		"/bin/sh", "-eu", "-c", shellOOMScoreAdj + chrootScript, "hotbed-exec",
		c.Root, cmp.Or(c.Dir, "/"), u.userspec(), u.groupsSpec(),
		strconv.Itoa(len(c.Binds)), strconv.Itoa(len(c.ReadOnlyBinds)),
	}
	for _, b := range c.Binds {
		args = append(args, b.Source, path.Clean("/"+b.Target))
	}
	for _, b := range c.ReadOnlyBinds {
		args = append(args, b.Source, path.Clean("/"+b.Target))
	}
	args = append(args, c.Argv...)
	return exec.CommandContext(ctx, "systemd-run", args...), nil
}
