// Package unit runs a command as a transient systemd service: its own
// cgroup with a CPU weight, a memory guarantee and a soft ceiling, its own
// mount namespace that shows its workspace at a fixed path, and output to a
// log file. Units outlive the worker that started them, so a worker restart
// costs no running build or dev server.
package unit

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode"
)

// DefaultSlice holds every hotbed command unless the worker names another,
// so a node's commands share one subtree.
const DefaultSlice = "hotbed.slice"

// SliceDir is where systemd puts a slice, under /sys/fs/cgroup: a dash in
// its name nests it, so user-hotbed.slice is /user.slice/user-hotbed.slice.
func SliceDir(slice string) string {
	name := strings.TrimSuffix(slice, ".slice")
	dir := ""
	for i := range name {
		if name[i] == '-' {
			dir += "/" + name[:i] + ".slice"
		}
	}
	return dir + "/" + slice
}

// description is a unit's Description: its command on one short line.
// systemd writes a transient unit to a file and reads it back on every
// daemon-reload; a newline in the description (a script's) would be read
// as lines of their own.
func description(argv []string) string {
	d := strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.Join(argv, " "))), " ")
	if r := []rune(d); len(r) > 200 {
		d = string(r[:199]) + "…"
	}
	return "hotbed " + d
}

// CheckSlice says whether s can name a slice.
func CheckSlice(s string) error {
	name, ok := strings.CutSuffix(s, ".slice")
	if !ok || name == "" || strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") || strings.Contains(name, "--") ||
		strings.ContainsFunc(name, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.')
		}) {
		return fmt.Errorf("%q is not a systemd slice name, such as hotbed.slice or user-hotbed.slice", s)
	}
	return nil
}

// Bind shows Source at Target inside the command's mount namespace.
type Bind struct {
	Source, Target string
}

// Spec is a command to start.
type Spec struct {
	Name       string // unit name without .service, such as hotbed-c-3f2a01
	Slice      string // the slice it runs in; empty for DefaultSlice
	Argv       []string
	Dir        string // working directory, as the command sees it
	Binds      []Bind
	User       string
	Env        []string // KEY=VALUE, beyond what the user's login shell sets
	PathPrefix []string // directories put first on PATH, after the login shell set it
	PathSuffix []string // directories put last on PATH, for tools a non-interactive login shell misses
	CPUWeight  int      // 1..10000; 100 is the default share
	MemoryLow  int64    // guaranteed bytes; 0 for none
	MemoryHigh int64    // bytes past which the kernel throttles and reclaims; 0 for none
	Log        string   // file that gets stdout and stderr
	// NoSwap keeps all its memory in RAM (MemorySwapMax=0), as a
	// measurement's: one swapped out measures the disk.
	NoSwap bool
	// Properties are more systemd properties (KEY=VALUE), such as
	// AmbientCapabilities=CAP_SYS_ADMIN, for what a build needs.
	Properties []string
}

// Start starts the command. It returns once systemd has the unit, not when
// the command ends. The command runs in the user's login shell, so PATH and
// the rest of their environment (cargo, go, pnpm) are what they would be in a
// terminal.
func Start(ctx context.Context, s Spec) error {
	if len(s.Argv) == 0 {
		return errors.New("no command")
	}
	if err := os.MkdirAll(filepath.Dir(s.Log), 0o755); err != nil {
		return err
	}
	if _, err := run(ctx, "systemd-run", startArgs(s)...); err != nil {
		return fmt.Errorf("start unit %s: %w", s.Name, err)
	}
	return nil
}

// startArgs is systemd-run's arguments for s.
func startArgs(s Spec) []string {
	swap := "infinity"
	if s.NoSwap {
		swap = "0"
	}
	args := []string{
		"--unit=" + s.Name,
		"--slice=" + cmp.Or(s.Slice, DefaultSlice),
		// systemd would otherwise substitute $VAR and ${VAR} in the command
		// line itself, before the shell sees them.
		"--expand-environment=no",
		"--description=" + description(s.Argv),
		"--property=Type=exec",
		// Keep the unit, and its exit status, after the command ends; the
		// worker reads the status and then removes the unit.
		"--property=RemainAfterExit=yes",
		"--property=KillMode=control-group",
		"--property=TimeoutStopSec=15",
		"--property=StandardInput=null",
		"--property=StandardOutput=append:" + s.Log,
		"--property=StandardError=append:" + s.Log,
		// Memory past MemoryHigh is reclaimed, never killed; the worker parks
		// commands before memory runs out.
		"--property=MemoryMax=infinity",
		"--property=MemorySwapMax=" + swap,
	}
	if s.User != "" {
		// No PAMName: pam_systemd would move the command into a login
		// session's scope under user.slice, out of this unit's cgroup and
		// every limit and freeze set on it. systemd sets HOME, USER and
		// SHELL for User= itself, and the login shell reads the profile.
		args = append(args, "--uid="+s.User)
	}
	if s.Dir != "" {
		args = append(args, "--working-directory="+s.Dir)
	}
	for _, b := range s.Binds {
		args = append(args, "--property=BindPaths="+b.Source+":"+b.Target)
	}
	if s.CPUWeight > 0 {
		args = append(args, "--property=CPUWeight="+strconv.Itoa(s.CPUWeight))
	}
	if s.MemoryLow > 0 {
		args = append(args, "--property=MemoryLow="+strconv.FormatInt(s.MemoryLow, 10))
	}
	if s.MemoryHigh > 0 {
		args = append(args, "--property=MemoryHigh="+strconv.FormatInt(s.MemoryHigh, 10))
	}
	for _, p := range s.Properties {
		args = append(args, "--property="+p)
	}
	for _, e := range s.Env {
		args = append(args, "--setenv="+e)
	}
	if len(s.PathPrefix) > 0 {
		args = append(args, "--setenv=HOTBED_PATH="+strings.Join(s.PathPrefix, ":"))
	}
	if len(s.PathSuffix) > 0 {
		args = append(args, "--setenv=HOTBED_PATH_AFTER="+strings.Join(s.PathSuffix, ":"))
	}
	// The login shell sets PATH from the user's profile; HOTBED_PATH goes in
	// front of it afterwards, so tools installed in the workspace win, and
	// HOTBED_PATH_AFTER behind it, for tools (rustup, go) a profile adds only
	// to interactive shells.
	args = append(args, "--", "/bin/bash", "-lc",
		`PATH="${HOTBED_PATH:+$HOTBED_PATH:}$PATH${HOTBED_PATH_AFTER:+:$HOTBED_PATH_AFTER}"; exec "$@"`, "hotbed")
	return append(args, s.Argv...)
}

// State is a unit as systemd sees it.
type State struct {
	Loaded  bool
	Running bool // the command has not exited
	Frozen  bool
	Exited  bool
	// ExitCode is the command's status when it exited normally; Signal is
	// the signal that ended it, if one did.
	ExitCode int
	Signal   int
	PID      int
	Cgroup   string // path under /sys/fs/cgroup
	CPUNanos uint64
	Memory   int64
}

// Status reads the unit's state.
func Status(ctx context.Context, name string) (State, error) {
	out, err := run(ctx, "systemctl", "show", name+".service",
		"--property=LoadState,ActiveState,SubState,ExecMainStatus,ExecMainCode,MainPID,ControlGroup,CPUUsageNSec,MemoryCurrent,FreezerState")
	if err != nil {
		return State{}, err
	}
	return parseShow(out)
}

// parseShow reads `systemctl show` key=value lines.
func parseShow(out string) (State, error) {
	kv := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			kv[k] = v
		}
	}
	var s State
	if kv["LoadState"] != "loaded" {
		return s, nil
	}
	s.Loaded = true
	sub := kv["SubState"]
	s.Running = kv["ActiveState"] == "active" && sub == "running" || kv["ActiveState"] == "activating"
	s.Exited = sub == "exited" || sub == "failed" || kv["ActiveState"] == "failed"
	s.Frozen = kv["FreezerState"] == "frozen" || kv["FreezerState"] == "freezing"
	s.PID, _ = strconv.Atoi(kv["MainPID"])
	s.Cgroup = kv["ControlGroup"]
	s.CPUNanos, _ = strconv.ParseUint(kv["CPUUsageNSec"], 10, 64)
	s.Memory, _ = strconv.ParseInt(kv["MemoryCurrent"], 10, 64)
	if s.Exited {
		status, _ := strconv.Atoi(kv["ExecMainStatus"])
		// ExecMainCode is a CLD_* value: 1 exited, 2 killed, 3 dumped core.
		if kv["ExecMainCode"] == "1" {
			s.ExitCode = status
		} else {
			s.Signal = status
		}
	}
	return s, nil
}

// Freeze stops every process of the unit where it is, without ending it.
func Freeze(ctx context.Context, name string) error {
	_, err := run(ctx, "systemctl", "freeze", name+".service")
	return err
}

// Thaw lets a frozen unit run again.
func Thaw(ctx context.Context, name string) error {
	_, err := run(ctx, "systemctl", "thaw", name+".service")
	return err
}

// Reclaim asks the kernel to push up to n bytes of the unit's memory out
// (to swap, for anonymous memory), as memory.reclaim does.
func Reclaim(cgroup string, n int64) error {
	f := filepath.Join("/sys/fs/cgroup", cgroup, "memory.reclaim")
	// Not os.WriteFile: cgroup files are pollable, so Go's runtime would take
	// the kernel's EAGAIN for "try again when writable" and wait forever.
	fd, err := syscall.Open(f, syscall.O_WRONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("reclaim %s: %w", f, err)
	}
	defer syscall.Close(fd)
	for {
		_, err = syscall.Write(fd, []byte(strconv.FormatInt(n, 10)))
		if err != syscall.EINTR {
			break
		}
	}
	// The kernel answers EAGAIN when it reclaimed less than asked, which is
	// not a failure for a best-effort push to swap.
	if err != nil && err != syscall.EAGAIN {
		return fmt.Errorf("reclaim %s: %w", f, err)
	}
	return nil
}

// Swap is how much of the unit's memory is in swap.
func Swap(cgroup string) (int64, error) {
	b, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", cgroup, "memory.swap.current"))
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

// Stop ends the unit's processes, if any are left, and removes the unit.
func Stop(ctx context.Context, name string) error {
	if _, err := run(ctx, "systemctl", "stop", name+".service"); err != nil {
		return err
	}
	// A unit that failed stays loaded until its failure is reset.
	if _, err := run(ctx, "systemctl", "reset-failed", name+".service"); err != nil && !strings.Contains(err.Error(), "not loaded") {
		return err
	}
	return nil
}

// StartSlice starts a slice, which stays when its last unit ends, until
// stopped.
func StartSlice(ctx context.Context, slice string) error {
	_, err := run(ctx, "systemctl", "start", slice)
	return err
}

// SetProperties sets properties (KEY=VALUE; KEY= resets one) of a unit until
// the next boot, as systemctl set-property --runtime does.
func SetProperties(ctx context.Context, unit string, props ...string) error {
	_, err := run(ctx, "systemctl", append([]string{"set-property", "--runtime", unit}, props...)...)
	return err
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// The arguments carry whole scripts; the first one says enough.
		return "", fmt.Errorf("%s %s: %w: %s", name, args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
