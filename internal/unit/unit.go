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
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// DefaultSlice holds every hotbed command unless the worker names another,
// so a node's commands share one subtree.
const DefaultSlice = "hotbed.slice"

// OOMScoreAdjust is every command's oom_score_adj: the most, so the kernel's
// OOM killer, in a cgroup's OOM or the host's, takes hotbed's commands before
// anything else in the same domain.
const OOMScoreAdjust = 1000

// unconfinedSELinuxContext is the SELinux security context for transient
// command service units on SELinux-enabled hosts (board 7921). Without it,
// systemd runs transient services under system_u:system_r:unconfined_service_t:s0
// (or initrc_t), which can transition programs like rsync into confined domains
// that are denied access to workspace files and command logs.
const unconfinedSELinuxContext = "unconfined_u:unconfined_r:unconfined_t:s0"

// hasSELinux reports whether SELinux is active on the host. A package variable
// for tests.
var hasSELinux = defaultHasSELinux

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

// TmpPaths are where a command with a Tmp of its own sees it, TMPDIR first.
var TmpPaths = [...]string{"/tmp", "/var/tmp"}

// Spec is a command to start.
type Spec struct {
	Name          string // unit name without .service, such as hotbed-c-3f2a01
	Slice         string // the slice it runs in; empty for DefaultSlice
	Argv          []string
	RootDir       string // required: rootfs to chroot into (systemd RootDirectory= + MountAPIVFS=yes)
	Dir           string // working directory, as the command sees it
	Binds         []Bind
	ReadOnlyBinds []Bind // mounted via BindReadOnlyPaths= (for ephemeral read-only host files in RootDir)
	Tmp           string // directory it sees as /tmp and /var/tmp, its own, TMPDIR=/tmp; empty for the rootfs's
	User          string
	Env           []string // KEY=VALUE extra env in RootDir
	PathPrefix    []string // directories put first on PATH
	CPUWeight     int      // 1..10000; 100 is the default share
	MemoryLow     int64    // guaranteed bytes; 0 for none
	MemoryHigh    int64    // bytes past which the kernel throttles and reclaims; 0 for none
	Log           string   // file that gets stdout and stderr
	// NoSwap keeps all its memory in RAM (MemorySwapMax=0), as a
	// measurement's: one swapped out measures the disk.
	NoSwap bool
	// Capabilities are Linux capabilities granted via AmbientCapabilities
	// (e.g. "CAP_PERFMON" so non-root workloads in RootDir can invoke perf).
	Capabilities []string
	// TraceFS exposes /sys/kernel/tracing and /sys/fs/cgroup inside RootDir
	// when present on the host, so workloads and tracers can use perf/bpftrace
	// and sudo can set the unit's own cgroup limits.
	TraceFS bool
	// Properties are more systemd properties (KEY=VALUE), such as
	// AmbientCapabilities=CAP_SYS_ADMIN, for what a build needs.
	Properties []string
}

// tmpBinds are the binds that show s.Tmp at TmpPaths.
func (s Spec) tmpBinds() []Bind {
	if s.Tmp == "" {
		return nil
	}
	binds := make([]Bind, len(TmpPaths))
	for i, t := range TmpPaths {
		binds[i] = Bind{Source: s.Tmp, Target: t}
	}
	return binds
}

// Start starts the command. It returns once systemd has the unit, not when
// the command ends. The command runs chrooted inside RootDir with a clean
// deterministic PATH.
func Start(ctx context.Context, s Spec) error {
	if len(s.Argv) == 0 {
		return errors.New("no command")
	}
	if s.RootDir == "" {
		return errors.New("root directory is required")
	}
	if err := os.MkdirAll(filepath.Dir(s.Log), 0o755); err != nil {
		return err
	}
	args, err := startArgs(s)
	if err != nil {
		return err
	}
	if err := s.prepareChroot(); err != nil {
		return err
	}
	if _, err := run(ctx, "systemd-run", args...); err != nil {
		return fmt.Errorf("start unit %s: %w", s.Name, err)
	}
	return nil
}

// prepareChroot makes each bind mountpoint inside s.RootDir, resolved inside it
// so symlinks in the rootfs never lead outside it.
func (s Spec) prepareChroot() error {
	var binds []Bind
	for _, b := range append(append(slices.Clone(s.Binds), s.tmpBinds()...), s.ReadOnlyBinds...) {
		binds = append(binds, Bind{Source: b.Source, Target: b.Target})
	}
	if _, err := os.Stat("/etc/resolv.conf"); err == nil {
		binds = append(binds, Bind{Source: "/etc/resolv.conf", Target: "/etc/resolv.conf"})
	}
	return prepareChrootTargets(s.RootDir, binds)
}

// lookPath finds a host program. A package variable for tests.
var lookPath = exec.LookPath

// rootfsPath is the deterministic PATH of a command in a rootfs: no host
// login shell.
func (s Spec) rootfsPath() string {
	dirs := append(slices.Clone(s.PathPrefix), "/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin")
	return strings.Join(dirs, ":")
}

// startArgs is systemd-run's arguments for s.
func startArgs(s Spec) ([]string, error) {
	if s.RootDir == "" {
		return nil, errors.New("root directory is required")
	}
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
		// When memory does run out, the kernel kills a command's processes
		// first. A slice in user.slice shares its limit with the user's
		// desktop, whose processes (oom_score_adj 100 to 300) the kernel
		// would otherwise pick over a build's (0): a cargo build's linkers
		// once took the whole session down before one ld was killed.
		"--property=OOMScoreAdjust=" + strconv.Itoa(OOMScoreAdjust),
	}
	if hasSELinux() {
		args = append(args, "--property=SELinuxContext="+unconfinedSELinuxContext)
	}
	args = append(args,
		"--property=RootDirectory="+s.RootDir,
		"--property=MountAPIVFS=yes",
		"--property=BindReadOnlyPaths=-/etc/resolv.conf",
	)
	if s.TraceFS {
		args = append(args,
			"--property=BindPaths=-/sys/kernel/tracing:/sys/kernel/tracing",
			"--property=BindPaths=-/sys/fs/cgroup:/sys/fs/cgroup",
		)
	}
	if s.User != "" {
		// No PAMName: pam_systemd would move the command into a login
		// session's scope under user.slice, out of this unit's cgroup and
		// every limit and freeze set on it. systemd sets HOME, USER and
		// SHELL for User= itself.
		args = append(args, "--uid="+s.User)
	}
	if len(s.Capabilities) > 0 {
		args = append(args, "--property=AmbientCapabilities="+strings.Join(s.Capabilities, " "))
	}
	if s.Dir != "" {
		args = append(args, "--working-directory="+s.Dir)
	}
	for _, b := range append(slices.Clone(s.Binds), s.tmpBinds()...) {
		args = append(args, "--property=BindPaths="+b.Source+":"+b.Target)
	}
	if s.Tmp != "" {
		args = append(args, "--setenv=TMPDIR="+TmpPaths[0])
	}
	for _, b := range s.ReadOnlyBinds {
		args = append(args, "--property=BindReadOnlyPaths="+b.Source+":"+b.Target)
	}
	args = appendLimits(args, s)
	for _, e := range s.Env {
		args = append(args, "--setenv="+e)
	}
	// Inside RootDirectory, use a deterministic PATH with no host login shell.
	// /bin/sh resolves s.Argv[0] via PATH before exec'ing it directly.
	args = append(args, "--setenv=PATH="+s.rootfsPath())
	args = append(args, "--", "/bin/sh", "-c", `exec "$@"`, "hotbed")
	return append(args, s.Argv...), nil
}

// appendLimits appends s's CPU and memory limits and extra properties.
func appendLimits(args []string, s Spec) []string {
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
	return args
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
	// Result is systemd's unit Result property (e.g. "success", "exit-code",
	// "signal", "oom-kill", "timeout"). OOMKilled is true when systemd
	// reported Result=oom-kill or the unit's cgroup recorded an OOM kill.
	Result    string
	OOMKilled bool
	PID       int
	Cgroup     string // path under /sys/fs/cgroup
	CPUNanos   uint64
	Memory     int64
	MemoryPeak int64
	// MemoryUse, when non-nil, carries the unit's own anonymous + swap memory
	// already sampled alongside Status/StatusMany (avoiding a second RPC for
	// guest VM commands).
	MemoryUse *MemoryUse
}

const statusProperties = "Id,LoadState,ActiveState,SubState,Result,ExecMainStatus,ExecMainCode,MainPID,ControlGroup,CPUUsageNSec,MemoryCurrent,FreezerState"

// Status reads the unit's state.
func Status(ctx context.Context, name string) (State, error) {
	out, err := run(ctx, "systemctl", "show", name+".service", "--property="+statusProperties)
	if err != nil {
		return State{}, err
	}
	s, err := parseShow(out)
	if err != nil {
		return State{}, err
	}
	if s.Exited && !s.OOMKilled && s.Cgroup != "" && cgroupOOMKilled(s.Cgroup) {
		s.OOMKilled = true
	}
	return s, nil
}

// StatusMany reads several units' states in one `systemctl show` invocation.
// Keys in the returned map match the unit names passed in (without ".service").
// Unknown or unloaded units appear with Loaded=false without failing the batch.
func StatusMany(ctx context.Context, names []string) (map[string]State, error) {
	states, _, err := StatusManyAt(ctx, names)
	return states, err
}

// StatusManyAt is StatusMany together with the timestamp captured immediately
// when `systemctl show` returned (before parsing and cgroup memory.events reads),
// so CPU rate deltas use the sample time rather than a later wall clock read.
func StatusManyAt(ctx context.Context, names []string) (map[string]State, time.Time, error) {
	if len(names) == 0 {
		return map[string]State{}, time.Now(), nil
	}
	unique := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		unique = append(unique, n)
	}
	args := make([]string, 0, len(unique)+2)
	args = append(args, "show", "--property="+statusProperties)
	for _, n := range unique {
		args = append(args, n+".service")
	}
	out, err := run(ctx, "systemctl", args...)
	sampledAt := time.Now()
	if err != nil {
		return nil, time.Time{}, err
	}
	states, err := parseShowMany(out, unique)
	if err != nil {
		return nil, time.Time{}, err
	}
	for name, s := range states {
		if s.Exited && !s.OOMKilled && s.Cgroup != "" && cgroupOOMKilled(s.Cgroup) {
			s.OOMKilled = true
			states[name] = s
		}
	}
	return states, sampledAt, nil
}

// cgroupOOMKilled reports whether the unit's cgroup memory.events recorded
// any kernel OOM kill. Missing or unreadable files return false.
func cgroupOOMKilled(cgroup string) bool {
	b, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", cgroup, "memory.events"))
	if err != nil {
		return false
	}
	return parseMemoryEventsOOM(string(b))
}

func parseMemoryEventsOOM(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if fields[0] == "oom_kill" || fields[0] == "oom_group_kill" {
			if n, err := strconv.ParseInt(fields[1], 10, 64); err == nil && n > 0 {
				return true
			}
		}
	}
	return false
}

// parseShow reads `systemctl show` key=value lines.
func parseShow(out string) (State, error) {
	kv := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if k, v, ok := strings.Cut(line, "="); ok {
			kv[k] = v
		}
	}
	return stateFromKV(kv), nil
}

// parseShowMany reads blank-line-separated unit blocks from `systemctl show`
// into a map keyed by unit name (without ".service"). Each block is keyed by
// its Id property when present, falling back to positional matching against
// names when Id is missing or unrecognized.
func parseShowMany(out string, names []string) (map[string]State, error) {
	requested := make(map[string]bool, len(names))
	for _, n := range names {
		requested[n] = true
	}
	res := make(map[string]State, len(names))
	var block []string
	blockIdx := 0
	flush := func() {
		if len(block) == 0 {
			return
		}
		kv := make(map[string]string, len(block))
		for _, line := range block {
			if k, v, ok := strings.Cut(line, "="); ok {
				kv[k] = v
			}
		}
		key := ""
		if id := kv["Id"]; id != "" {
			trimmed := strings.TrimSuffix(id, ".service")
			switch {
			case len(requested) == 0 || requested[trimmed]:
				key = trimmed
			case requested[id]:
				key = id
			case blockIdx < len(names):
				if _, exists := res[names[blockIdx]]; !exists {
					key = names[blockIdx]
				} else {
					key = trimmed
				}
			default:
				key = trimmed
			}
		} else if blockIdx < len(names) {
			key = names[blockIdx]
		}
		if key != "" {
			res[key] = stateFromKV(kv)
		}
		block = block[:0]
		blockIdx++
	}
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		block = append(block, line)
	}
	flush()
	return res, nil
}

func stateFromKV(kv map[string]string) State {
	var s State
	if kv["LoadState"] != "loaded" {
		return s
	}
	s.Loaded = true
	sub := kv["SubState"]
	s.Running = kv["ActiveState"] == "active" && sub == "running" || kv["ActiveState"] == "activating"
	s.Exited = sub == "exited" || sub == "failed" || sub == "dead" || kv["ActiveState"] == "failed" || kv["ActiveState"] == "inactive"
	s.Frozen = kv["FreezerState"] == "frozen" || kv["FreezerState"] == "freezing"
	s.Result = kv["Result"]
	s.PID, _ = strconv.Atoi(kv["MainPID"])
	s.Cgroup = kv["ControlGroup"]
	s.CPUNanos, _ = strconv.ParseUint(kv["CPUUsageNSec"], 10, 64)
	s.Memory, _ = strconv.ParseInt(kv["MemoryCurrent"], 10, 64)
	if s.Exited {
		if s.Result == "oom-kill" {
			s.OOMKilled = true
		}
		status, _ := strconv.Atoi(kv["ExecMainStatus"])
		// ExecMainCode is a CLD_* value: 1 exited, 2 killed, 3 dumped core.
		if kv["ExecMainCode"] == "1" {
			s.ExitCode = status
		} else {
			s.Signal = status
		}
	}
	return s
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

// MemoryUse is a unit's own memory: what it needs in RAM to run, unlike
// the file cache its cgroup's MemoryCurrent counts too, which the kernel
// drops for others.
type MemoryUse struct {
	Anon int64 // anonymous memory in RAM (memory.stat anon)
	Swap int64 // in swap (memory.swap.current)
}

// Held is all of it, in RAM or swap.
func (m MemoryUse) Held() int64 { return m.Anon + m.Swap }

// Memory reads the own memory of the unit at cgroup, under /sys/fs/cgroup.
func Memory(cgroup string) (MemoryUse, error) {
	return memoryIn(filepath.Join("/sys/fs/cgroup", cgroup))
}

// memoryIn reads the own memory of the cgroup at dir.
func memoryIn(dir string) (MemoryUse, error) {
	b, err := os.ReadFile(filepath.Join(dir, "memory.swap.current"))
	if err != nil {
		return MemoryUse{}, err
	}
	var m MemoryUse
	if m.Swap, err = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err != nil {
		return MemoryUse{}, fmt.Errorf("%s/memory.swap.current: %w", dir, err)
	}
	stat, err := os.ReadFile(filepath.Join(dir, "memory.stat"))
	if err != nil {
		return MemoryUse{}, err
	}
	if m.Anon, err = statField(string(stat), "anon"); err != nil {
		return MemoryUse{}, fmt.Errorf("%s/memory.stat: %w", dir, err)
	}
	return m, nil
}

// statField is the value of key in a cgroup's memory.stat.
func statField(stat, key string) (int64, error) {
	for _, line := range strings.Split(stat, "\n") {
		if v, ok := strings.CutPrefix(line, key+" "); ok {
			return strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		}
	}
	return 0, fmt.Errorf("no %s", key)
}

// List returns the names (without ".service") of all loaded active or failed
// hotbed-* service units running in slice (empty defaults to DefaultSlice).
// Units in other slices — notably hotbed-worker.service in system.slice — are
// excluded.
func List(ctx context.Context, slice string) ([]string, error) {
	slice = cmp.Or(slice, DefaultSlice)
	out, err := run(ctx, "systemctl", "list-units", "--type=service", "--no-legend", "--plain", "--full", "--", "hotbed-*.service")
	if err != nil {
		return nil, err
	}
	candidates := parseListUnits(out)
	if len(candidates) == 0 {
		return nil, nil
	}
	args := make([]string, 0, len(candidates)+3)
	args = append(args, "show", "--property=Id,Slice,LoadState,ActiveState", "--")
	args = append(args, candidates...)
	showOut, err := run(ctx, "systemctl", args...)
	if err != nil {
		return nil, err
	}
	return parseSliceUnits(showOut, slice), nil
}

// parseListUnits extracts hotbed-*.service unit names from `systemctl list-units --no-legend --plain --full`.
func parseListUnits(out string) []string {
	var names []string
	seen := make(map[string]bool)
	for _, raw := range strings.Split(out, "\n") {
		for _, f := range strings.Fields(raw) {
			if !strings.HasSuffix(f, ".service") {
				continue
			}
			if strings.HasPrefix(f, "hotbed-") && !seen[f] {
				seen[f] = true
				names = append(names, f)
			}
			break
		}
	}
	return names
}

// parseSliceUnits filters `systemctl show --property=Id,Slice,LoadState,ActiveState`
// blocks to loaded, non-inactive hotbed-* services belonging to slice, returning
// sorted unit names without the ".service" suffix.
func parseSliceUnits(out, slice string) []string {
	var res []string
	seen := make(map[string]bool)
	var block []string
	flush := func() {
		if len(block) == 0 {
			return
		}
		kv := make(map[string]string, len(block))
		for _, line := range block {
			if k, v, ok := strings.Cut(line, "="); ok {
				kv[k] = v
			}
		}
		block = block[:0]
		if kv["LoadState"] != "loaded" || kv["Slice"] != slice || kv["ActiveState"] == "inactive" {
			return
		}
		id := kv["Id"]
		if !strings.HasPrefix(id, "hotbed-") || !strings.HasSuffix(id, ".service") {
			return
		}
		name := strings.TrimSuffix(id, ".service")
		if !seen[name] {
			seen[name] = true
			res = append(res, name)
		}
	}
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		block = append(block, line)
	}
	flush()
	slices.Sort(res)
	return res
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
// stopped. A slice in a user's (user-1000-hotbed.slice) would not:
// systemd's user-.slice.d gives every user-*.slice StopWhenUnneeded=yes,
// so it stops as its last unit ends, and its cgroup goes, with whatever was
// written there (a measurement's partition). A runtime drop-in of hotbed's
// keeps it; systemd reloads its units only when that drop-in is new.
func StartSlice(ctx context.Context, slice string) error {
	if err := keepSlice(ctx, slice); err != nil {
		return err
	}
	_, err := run(ctx, "systemctl", "start", slice)
	return err
}

// keptSlice is the drop-in that keeps a slice started while nothing runs in
// it; systemctl revert removes it with the slice's other runtime settings.
const keptSlice = `# hotbed: started until stopped, while nothing runs in it too
[Unit]
StopWhenUnneeded=no
`

// sliceDropIns is where keepSlice writes: systemd's runtime unit files,
// gone at boot with the slices they keep.
var sliceDropIns = "/run/systemd/system"

func keepSlice(ctx context.Context, slice string) error {
	f := filepath.Join(sliceDropIns, slice+".d", "50-hotbed-keep.conf")
	if b, err := os.ReadFile(f); err == nil && string(b) == keptSlice {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		return fmt.Errorf("keep %s started: %w", slice, err)
	}
	if err := os.WriteFile(f, []byte(keptSlice), 0o644); err != nil {
		return fmt.Errorf("keep %s started: %w", slice, err)
	}
	_, err := run(ctx, "systemctl", "daemon-reload")
	return err
}

// Swap describes the host's swap backing for computing a slice backstop.
type Swap struct {
	Zram int64 // bytes backed by in-RAM compressed swap (/dev/zram*)
	Disk int64 // bytes backed by disk partitions or files
}

// SwapMaxUnbounded is the Backstop.SwapMax value indicating no slice-level
// swap cap. Backstop.Properties emits it as MemorySwapMax=infinity (never
// MemorySwapMax=0, which would disable swap entirely) so applying an unbounded
// backstop clears any stale finite runtime cap from a prior worker run.
const SwapMaxUnbounded int64 = 0

// Backstop is the memory a work slice may take of a host with total bytes
// of RAM, all its commands together: past High the kernel throttles them
// and reclaims their memory; past Max it kills one of them, in the slice,
// rather than stall the whole host. Either leaves the host room
// (Headroom, an eighth of RAM, at least 1 GiB) for its desktop, its
// system, the worker and the kernel: their memory is what the host needs
// to stay up (board 9128). The worker's admission and parking keep
// commands well under it; it is there for when they fall short.
//
// On hosts with zram swap, pages the slice swaps out are compressed into
// zram's own kernel memory pool, which is not charged to the slice's memcg
// (board 9361). SwapMax caps the slice's MemorySwapMax when zram could
// otherwise exceed the swap budget, and High and Max are lowered by the full
// worst-case (1:1 incompressible) zram footprint so the host still keeps its
// full Headroom at High and Headroom/2 hard reserve at Max.
type Backstop struct {
	Headroom int64
	High     int64
	Max      int64
	// SwapMax caps MemorySwapMax on the work slice when > 0; SwapMaxUnbounded
	// (0) emits MemorySwapMax=infinity so swap is unbounded and any stale
	// finite runtime cap on the slice is cleared.
	SwapMax int64
}

// BackstopFor is the backstop of a host with total bytes of RAM and swap
// backing swap.
func BackstopFor(total int64, swap Swap) Backstop {
	if total <= 0 {
		return Backstop{}
	}
	room := max(total/8, 1<<30)
	if room > total/2 {
		room = total / 2
	}
	high := total - room
	maxMem := total - room/2

	zram := max(swap.Zram, 0)
	disk := max(swap.Disk, 0)
	if zram == 0 {
		return Backstop{Headroom: room, High: high, Max: maxMem, SwapMax: SwapMaxUnbounded}
	}

	// Bound zram usage to one headroom slice (and at most a quarter of
	// total-room on small hosts so High remains well above zero). Deduct
	// the full zramBound (1:1 incompressible worst case) from resident High
	// and Max so even incompressible pages swapped into zram leave the host
	// its full Headroom at High and Headroom/2 hard reserve at Max.
	swapBudget := min(room, high/4)
	zramBound := min(zram, swapBudget)

	// Cap MemorySwapMax whenever zram exceeds the swap budget (including
	// mixed zram+disk hosts like ubu1, where higher-priority zram fills
	// before disk) or when zram is the only swap. When a host pairs a small
	// zram device (<= swapBudget) with disk swap, zram is already bounded
	// by its device size, so leaving MemorySwapMax unbounded (infinity)
	// lets parked commands spill to disk freely while High/Max still reserve
	// the full zram device size at 1:1.
	swapMax := SwapMaxUnbounded
	if zram > swapBudget || disk == 0 {
		swapMax = zramBound
	}

	return Backstop{
		Headroom: room,
		High:     high - zramBound,
		Max:      maxMem - zramBound,
		SwapMax:  swapMax,
	}
}

// MemoryLimits are memory caps configured statically on a slice unit
// (0 means unset or infinity).
type MemoryLimits struct {
	High    int64
	Max     int64
	SwapMax int64
}

// Clamp tightens b so it never weakens stricter static limits configured by
// the host (for example a slice drop-in that reserves extra room for ZFS ARC
// or a desktop session).
func (b Backstop) Clamp(limits MemoryLimits) Backstop {
	if limits.High > 0 && (b.High == 0 || limits.High < b.High) {
		b.High = limits.High
		if limits.Max == 0 && b.Max > b.High+b.Headroom/2 {
			b.Max = b.High + b.Headroom/2
		}
	}
	if limits.Max > 0 && (b.Max == 0 || limits.Max < b.Max) {
		b.Max = limits.Max
		wantHigh := b.Max
		// When a static drop-in sets MemoryMax without MemoryHigh (e.g. a
		// desktop-sharing slice cap), preserve a reclaim/throttle cushion below
		// Max so workloads hit memory.high before slamming into hard OOM.
		if limits.High == 0 {
			cushion := b.Max / 8
			if b.Headroom > 0 && b.Headroom/2 < cushion {
				cushion = b.Headroom / 2
			}
			if b.Max > cushion {
				wantHigh = b.Max - cushion
			}
		}
		if b.High == 0 || b.High > wantHigh {
			b.High = wantHigh
		}
	}
	if limits.SwapMax > 0 && (b.SwapMax == SwapMaxUnbounded || limits.SwapMax < b.SwapMax) {
		b.SwapMax = limits.SwapMax
	}
	return b
}

// StaticMemoryLimits reads MemoryHigh, MemoryMax, and MemorySwapMax from the
// unit's non-runtime configuration files (ignoring /run/systemd/system.control/
// drop-ins created by SetProperties).
func StaticMemoryLimits(ctx context.Context, unit string, total int64) (MemoryLimits, error) {
	props, err := Show(ctx, unit, "FragmentPath", "DropInPaths")
	if err != nil {
		return MemoryLimits{}, err
	}
	var paths []string
	for _, p := range props["FragmentPath"] {
		if p != "" && !isRuntimeControlPath(p) {
			paths = append(paths, p)
		}
	}
	for _, line := range props["DropInPaths"] {
		for _, p := range strings.Fields(line) {
			if !isRuntimeControlPath(p) {
				paths = append(paths, p)
			}
		}
	}
	return parseMemoryLimitFiles(paths, total), nil
}

func isRuntimeControlPath(path string) bool {
	return strings.Contains(path, "/run/systemd/system.control/")
}

func parseMemoryLimitFiles(paths []string, total int64) MemoryLimits {
	var limits MemoryLimits
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		parseMemoryLimitINI(string(data), total, &limits)
	}
	return limits
}

func parseMemoryLimitINI(content string, total int64, limits *MemoryLimits) {
	inSlice := false
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inSlice = line == "[Slice]"
			continue
		}
		if !inSlice {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "MemoryHigh":
			if n, ok := parseSystemdBytes(v, total); ok {
				limits.High = n
			}
		case "MemoryMax":
			if n, ok := parseSystemdBytes(v, total); ok {
				limits.Max = n
			}
		case "MemorySwapMax":
			if n, ok := parseSystemdBytes(v, total); ok {
				limits.SwapMax = n
			}
		}
	}
}

// parseSystemdBytes parses a systemd memory value (bytes, K/M/G/T binary
// suffix, percentage of total, or ""/"infinity" -> 0).
func parseSystemdBytes(s string, total int64) (int64, bool) {
	if s == "" || strings.EqualFold(s, "infinity") {
		return 0, true
	}
	if strings.HasSuffix(s, "%") {
		if total <= 0 {
			return 0, false
		}
		pct, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
		if err != nil || pct < 0 {
			return 0, false
		}
		return int64(float64(total) * pct / 100.0), true
	}
	mult := int64(1)
	num := s
	if len(s) > 1 {
		switch s[len(s)-1] {
		case 'K', 'k':
			mult = 1 << 10
			num = s[:len(s)-1]
		case 'M', 'm':
			mult = 1 << 20
			num = s[:len(s)-1]
		case 'G', 'g':
			mult = 1 << 30
			num = s[:len(s)-1]
		case 'T', 't':
			mult = 1 << 40
			num = s[:len(s)-1]
		}
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n * mult, true
}

// Properties are the slice's properties that set it. It always emits
// MemorySwapMax (either the finite byte cap or "infinity" when SwapMax is
// unbounded) so re-applying a backstop clears any stale finite MemorySwapMax
// persisted on the slice across worker restarts.
func (b Backstop) Properties() []string {
	swapMax := "infinity"
	if b.SwapMax > 0 {
		swapMax = strconv.FormatInt(b.SwapMax, 10)
	}
	return []string{
		"MemoryHigh=" + strconv.FormatInt(b.High, 10),
		"MemoryMax=" + strconv.FormatInt(b.Max, 10),
		"MemorySwapMax=" + swapMax,
	}
}

// SetProperties sets properties (KEY=VALUE; KEY= resets one) of a unit until
// the next boot, as systemctl set-property --runtime does.
func SetProperties(ctx context.Context, unit string, props ...string) error {
	_, err := run(ctx, "systemctl", append([]string{"set-property", "--runtime", unit}, props...)...)
	return err
}

// Show is a unit's properties, each property's values: a list property,
// such as IOReadBandwidthMax, has one per entry, an empty one none.
func Show(ctx context.Context, unit string, props ...string) (map[string][]string, error) {
	args := []string{"show", unit}
	for _, p := range props {
		args = append(args, "--property="+p)
	}
	out, err := run(ctx, "systemctl", args...)
	if err != nil {
		return nil, err
	}
	return parseProperties(out), nil
}

func parseProperties(out string) map[string][]string {
	m := map[string][]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && v != "" {
			m[k] = append(m[k], v)
		}
	}
	return m
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
