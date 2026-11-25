// Package fake stands in for the parts of a worker that need Linux and
// root, so tests run anywhere: a store that copies directories, units that
// run commands as local processes, and a machine whose memory tests set.
//
// They keep the contracts the engine relies on, not the performance: a
// clone here is a full copy.
package fake

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/bench"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/storage"
	"github.com/semistrict/hotbed/internal/sysinfo"
	"github.com/semistrict/hotbed/internal/unit"
)

// BaseImage returns a fast in-process baseimage.Manager backed by store that
// creates a minimal rootfs skeleton (no OCI pull or systemd provisioning) for
// tests and dev-cockpit.
func BaseImage(store storage.Store) *baseimage.Manager {
	return &baseimage.Manager{
		Store: store,
		Spec: baseimage.Spec{
			Image:  "fake-base:test",
			Arch:   "amd64",
			User:   "hotbed",
			UID:    1000,
			GID:    1000,
			Script: "true",
		},
		Pull: func(_ context.Context, _, _, rootDir string) error {
			return os.MkdirAll(filepath.Join(rootDir, "work", ".hotbed", "home"), 0o755)
		},
		Provision: baseimage.ProvisionFunc(func(context.Context, string, string) error {
			return nil
		}),
	}
}

// Store keeps volumes and snapshots as directories, copied in full.
type Store struct {
	Root string

	mu sync.Mutex
	// held are volumes the kernel would still hold busy: Destroy fails with
	// storage.ErrBusy until they are let go.
	held map[string]bool
}

// Hold makes the volume busy, as a mount of it left in another mount
// namespace does.
func (s *Store) Hold(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held == nil {
		s.held = map[string]bool{}
	}
	s.held[name] = true
}

// Let lets go of a volume Hold made busy.
func (s *Store) Let(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.held, name)
}

func (s *Store) Kind() string                { return "fake" }
func (s *Store) Path(name string) string     { return filepath.Join(s.Root, "vol", name) }
func (s *Store) snapPath(name string) string { return filepath.Join(s.Root, "snap", name) }
func (s *Store) metaPath(name string) string {
	return filepath.Join(s.Root, "meta", url.PathEscape(name))
}

func (s *Store) recordVolume(name string) error {
	mp := s.metaPath(name)
	if err := os.MkdirAll(filepath.Dir(mp), 0o755); err != nil {
		return err
	}
	return os.WriteFile(mp, nil, 0o644)
}

func (s *Store) Create(_ context.Context, name string) error {
	if exists(s.Path(name)) {
		return fmt.Errorf("%s: %w", name, storage.ErrExists)
	}
	if err := os.MkdirAll(s.Path(name), 0o755); err != nil {
		return err
	}
	return s.recordVolume(name)
}

func (s *Store) Snapshot(_ context.Context, vol, snap string) error {
	if !exists(s.Path(vol)) {
		return fmt.Errorf("%s: %w", vol, storage.ErrNotFound)
	}
	if exists(s.snapPath(snap)) {
		return fmt.Errorf("%s: %w", snap, storage.ErrExists)
	}
	return copyTree(s.Path(vol), s.snapPath(snap))
}

func (s *Store) Clone(_ context.Context, snap, name string) error {
	if !exists(s.snapPath(snap)) {
		return fmt.Errorf("%s: %w", snap, storage.ErrNotFound)
	}
	if exists(s.Path(name)) {
		return fmt.Errorf("%s: %w", name, storage.ErrExists)
	}
	if err := copyTree(s.snapPath(snap), s.Path(name)); err != nil {
		return err
	}
	return s.recordVolume(name)
}

func (s *Store) Destroy(_ context.Context, name string) error {
	if !exists(s.Path(name)) {
		return fmt.Errorf("%s: %w", name, storage.ErrNotFound)
	}
	s.mu.Lock()
	held := s.held[name]
	s.mu.Unlock()
	if held {
		return fmt.Errorf("%s: %w", name, storage.ErrBusy)
	}
	if err := os.RemoveAll(s.Path(name)); err != nil {
		return err
	}
	if err := os.Remove(s.metaPath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Store) DestroySnapshot(_ context.Context, snap string) error {
	if !exists(s.snapPath(snap)) {
		return fmt.Errorf("%s: %w", snap, storage.ErrNotFound)
	}
	return os.RemoveAll(s.snapPath(snap))
}

// ListVolumes returns all existing volumes created via Create/Clone as well as
// any directories matching hotbed's standard volume layouts (ws/<id>,
// bases/<project>/<gen>, baseimages/<key>) created directly on disk.
func (s *Store) ListVolumes(ctx context.Context) ([]string, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	seen := make(map[string]bool)
	if entries, err := os.ReadDir(filepath.Join(s.Root, "meta")); err == nil {
		for _, e := range entries {
			name, err := url.PathUnescape(e.Name())
			if err != nil || name == "" {
				continue
			}
			if exists(s.Path(name)) {
				seen[name] = true
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// Also pick up standard hotbed volume directories created directly on disk.
	for _, top := range []string{"ws", "baseimages"} {
		if entries, err := os.ReadDir(filepath.Join(s.Root, "vol", top)); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					seen[top+"/"+e.Name()] = true
				}
			}
		}
	}
	if projects, err := os.ReadDir(filepath.Join(s.Root, "vol", "bases")); err == nil {
		for _, p := range projects {
			if !p.IsDir() {
				continue
			}
			if gens, err := os.ReadDir(filepath.Join(s.Root, "vol", "bases", p.Name())); err == nil {
				for _, g := range gens {
					if g.IsDir() {
						seen["bases/"+p.Name()+"/"+g.Name()] = true
					}
				}
			}
		}
	}
	vols := make([]string, 0, len(seen))
	for v := range seen {
		vols = append(vols, v)
	}
	slices.Sort(vols)
	return vols, nil
}

// ListSnapshots returns all snapshots under Root/snap.
func (s *Store) ListSnapshots(ctx context.Context) ([]string, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	entries, err := os.ReadDir(filepath.Join(s.Root, "snap"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	snaps := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			snaps = append(snaps, e.Name())
		}
	}
	slices.Sort(snaps)
	return snaps, nil
}

func (s *Store) Usage(_ context.Context, name string) (storage.Usage, error) {
	if !exists(s.Path(name)) {
		return storage.Usage{}, fmt.Errorf("%s: %w", name, storage.ErrNotFound)
	}
	var n int64
	filepath.Walk(s.Path(name), func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			n += info.Size()
		}
		return nil
	})
	return storage.Usage{Referenced: n, Exclusive: n}, nil
}

func (s *Store) Space(context.Context) (storage.Space, error) {
	return storage.Space{Size: 100 << 30, Free: 60 << 30}, nil
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// copyTree copies a directory keeping modes and mtimes, as a snapshot does.
func copyTree(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	// -a keeps extended attributes (SELinux labels among them) as real
	// snapshots and clones do: a copy relabeled by its new parent is a
	// rootfs systemd may not execute in.
	out, err := exec.Command("cp", "-a", src, dst).CombinedOutput()
	if err != nil {
		return fmt.Errorf("cp -a %s %s: %w: %s", src, dst, err, out)
	}
	return nil
}

// Units runs each command as a local bash process. Commands see their
// workspace at /work through a bind mount on a real worker; here every
// "/work" in their arguments, directory and environment is rewritten to the
// volume's path instead. A command's own tmp is only its TMPDIR here: its
// /tmp is the host's, which a rewrite would mistake for paths tests pass.
type Units struct {
	mu     sync.Mutex
	units  map[string]*proc
	specs  map[string]unit.Spec
	cores  float64
	swap   map[string]int64 // by unit name
	anon   map[string]int64 // by unit name
	execs  []Exec
	closed bool
	wg     sync.WaitGroup
}

// Exec is a chroot ExecSpec Units.Exec made, and the command it made for it, as
// its caller then set it up (stdio, environment).
type Exec struct {
	Config unit.ExecSpec
	Cmd    *exec.Cmd
}

// Execs are the exec commands Exec has made, in order.
func (u *Units) Execs() []Exec {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.execs)
}

// SetSwap has unit name report n bytes of its memory in swap.
func (u *Units) SetSwap(name string, n int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.swap == nil {
		u.swap = map[string]int64{}
	}
	u.swap[name] = n
}

// SetAnon has unit name report n bytes of anonymous memory in RAM.
func (u *Units) SetAnon(name string, n int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.anon == nil {
		u.anon = map[string]int64{}
	}
	u.anon[name] = n
}

// SetCores has every command report keeping this many CPUs busy.
func (u *Units) SetCores(cores float64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.cores = cores
}

// Spec is what the unit name was started with, after it ended too.
func (u *Units) Spec(name string) (unit.Spec, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	s, ok := u.specs[name]
	return s, ok
}

type proc struct {
	cmd       *exec.Cmd
	started   time.Time
	exited    chan struct{} // closed once done
	done      bool
	code      int
	signal    int
	result    string
	oomKilled bool
	frozen    bool
}

func (u *Units) Start(_ context.Context, s unit.Spec) error {
	if s.RootDir == "" {
		return errors.New("root directory is required")
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return fmt.Errorf("units closed")
	}
	if u.units == nil {
		u.units, u.specs = map[string]*proc{}, map[string]unit.Spec{}
	}
	if _, dup := u.units[s.Name]; dup {
		return fmt.Errorf("unit %s exists", s.Name)
	}
	rewrite := rewriter(s.RootDir, s.Binds, s.ReadOnlyBinds)
	args := make([]string, len(s.Argv))
	for i, a := range s.Argv {
		args[i] = rewrite(a)
	}
	if err := os.MkdirAll(filepath.Dir(s.Log), 0o755); err != nil {
		return err
	}
	log, err := os.OpenFile(s.Log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	// Like a real unit, put the spec's directories in front of PATH before
	// looking the command up.
	cmd := exec.Command("/bin/sh", append([]string{"-c", `PATH="${HOTBED_PATH:+$HOTBED_PATH:}$PATH"; exec "$@"`, "hotbed"}, args...)...)
	cmd.Dir = rewrite(s.Dir)
	cmd.Env = os.Environ()
	if s.Tmp != "" {
		cmd.Env = append(cmd.Env, "TMPDIR="+s.Tmp)
	}
	for _, e := range s.Env {
		cmd.Env = append(cmd.Env, rewrite(e))
	}
	paths := func(dirs []string) string {
		out := make([]string, len(dirs))
		for i, d := range dirs {
			out[i] = rewrite(d)
		}
		return strings.Join(out, ":")
	}
	cmd.Env = append(cmd.Env, "HOTBED_PATH="+paths(s.PathPrefix))
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		log.Close()
		return err
	}
	p := &proc{cmd: cmd, started: time.Now(), exited: make(chan struct{})}
	u.units[s.Name] = p
	u.specs[s.Name] = s
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		err := cmd.Wait()
		log.Close()
		u.mu.Lock()
		defer u.mu.Unlock()
		p.done = true
		close(p.exited)
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				p.signal = int(ws.Signal())
			} else {
				p.code = exit.ExitCode()
			}
		}
	}()
	return nil
}

// rewriter maps paths in a rootfs at rootDir (none when empty), with
// binds, to the host's: the fake runs on the host what a real unit runs in
// the rootfs, so it sees each bind at its source and the rootfs's /work at
// rootDir/work.
func rewriter(rootDir string, groups ...[]unit.Bind) func(string) string {
	var binds []unit.Bind
	for _, g := range groups {
		binds = append(binds, g...)
	}
	if rootDir != "" && !slices.ContainsFunc(binds, func(b unit.Bind) bool { return b.Target == "/work" }) {
		binds = append(binds, unit.Bind{Source: filepath.Join(rootDir, "work"), Target: "/work"})
	}
	// Longest target first; single-pass so a rewritten source path (e.g. under
	// /tmp on Linux test runners) is never re-matched by another bind.
	slices.SortFunc(binds, func(a, b unit.Bind) int { return len(b.Target) - len(a.Target) })
	return func(x string) string { return rewriteBinds(binds, x) }
}

// Exec runs c on the host, as the test's user, with its paths (argv,
// working directory) rewritten as rewriter does.
func (u *Units) Exec(ctx context.Context, _, _ string, c unit.ExecSpec) (*exec.Cmd, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	rewrite := rewriter(c.Root, c.Binds, c.ReadOnlyBinds)
	args := make([]string, len(c.Argv))
	for i, a := range c.Argv {
		args[i] = rewrite(a)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = rewrite(c.Dir)
	u.mu.Lock()
	u.execs = append(u.execs, Exec{Config: c, Cmd: cmd})
	u.mu.Unlock()
	return cmd, nil
}

// rewriteBinds replaces each bind's target as a whole path component in a
// single pass so replacement sources are never re-scanned by later binds.
func rewriteBinds(binds []unit.Bind, s string) string {
	if len(binds) == 0 || s == "" {
		return s
	}
	var out strings.Builder
	for i := 0; i < len(s); {
		if i == 0 || (!isPathWord(s[i-1]) && s[i-1] != '/') {
			matched := false
			for _, b := range binds {
				if b.Target == "" || !strings.HasPrefix(s[i:], b.Target) {
					continue
				}
				end := i + len(b.Target)
				if end == len(s) || s[end] == '/' || !isPathWord(s[end]) {
					out.WriteString(b.Source)
					i = end
					matched = true
					break
				}
			}
			if matched {
				continue
			}
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

func isPathWord(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '.' || c == '-'
}

func (u *Units) Status(_ context.Context, name string) (unit.State, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.stateLocked(name, time.Now()), nil
}

func (u *Units) StatusMany(ctx context.Context, names []string) (map[string]unit.State, error) {
	out, _, err := u.StatusManyAt(ctx, names)
	return out, err
}

// StatusManyAt returns every named unit's state together with the exact time
// at which CPUNanos was computed, eliminating scheduling skew between reading
// the counter and timestamping it.
func (u *Units) StatusManyAt(_ context.Context, names []string) (map[string]unit.State, time.Time, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	now := time.Now()
	out := make(map[string]unit.State, len(names))
	for _, name := range names {
		out[name] = u.stateLocked(name, now)
	}
	return out, now, nil
}

func (u *Units) stateLocked(name string, now time.Time) unit.State {
	p, ok := u.units[name]
	if !ok {
		return unit.State{}
	}
	elapsed := max(time.Duration(0), now.Sub(p.started))
	return unit.State{
		Loaded: true, Running: !p.done, Exited: p.done, Frozen: p.frozen,
		ExitCode: p.code, Signal: p.signal, Result: p.result, OOMKilled: p.oomKilled,
		PID:    p.cmd.Process.Pid,
		Cgroup: "/hotbed.slice/" + name + ".service", Memory: 64 << 20,
		CPUNanos: uint64(u.cores * float64(elapsed)),
	}
}

// OOMKill simulates systemd marking a unit Result=oom-kill and tearing down
// its main process with sig (e.g. SIGTERM under DefaultOOMPolicy=stop).
func (u *Units) OOMKill(name string, sig syscall.Signal) error {
	u.mu.Lock()
	p, ok := u.units[name]
	if !ok {
		u.mu.Unlock()
		return fmt.Errorf("no unit %s", name)
	}
	p.result = "oom-kill"
	p.oomKilled = true
	pid := p.cmd.Process.Pid
	u.mu.Unlock()
	if err := syscall.Kill(-pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	<-p.exited
	return nil
}

// freeze stops (frozen) or continues a unit's processes, as systemd's freeze
// and thaw do, and records it, all under u.mu: Start's goroutine marks the
// unit done under it, and Stop forgets it under it.
func (u *Units) freeze(name string, frozen bool) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	p, ok := u.units[name]
	if !ok {
		return fmt.Errorf("no unit %s", name)
	}
	if p.done {
		return nil
	}
	sig := syscall.SIGCONT
	if frozen {
		sig = syscall.SIGSTOP
	}
	// Its process may have exited while Start's goroutine waits for u.mu to
	// mark it done: nothing is left to signal, as for a unit that is done.
	// Gone, the group is ESRCH; still a zombie, macOS says EPERM, which
	// can mean nothing else for this process's own children.
	if err := syscall.Kill(-p.cmd.Process.Pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EPERM) {
		return err
	}
	p.frozen = frozen
	return nil
}

func (u *Units) Freeze(_ context.Context, name string) error { return u.freeze(name, true) }

func (u *Units) Thaw(_ context.Context, name string) error { return u.freeze(name, false) }

// Wait returns once the unit's process has exited.
func (u *Units) Wait(ctx context.Context, _, name string) error {
	u.mu.Lock()
	p, ok := u.units[name]
	u.mu.Unlock()
	if !ok {
		return nil
	}
	select {
	case <-p.exited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (u *Units) Reclaim(cgroup string, bytes int64) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if bytes <= 0 || u.anon == nil {
		return nil
	}
	name := strings.TrimSuffix(filepath.Base(cgroup), ".service")
	if move := min(u.anon[name], bytes); move > 0 {
		u.anon[name] -= move
		if u.swap == nil {
			u.swap = map[string]int64{}
		}
		u.swap[name] += move
	}
	return nil
}

// Memory is what SetAnon and SetSwap set for the unit of cgroup (Status's
// /hotbed.slice/NAME.service).
func (u *Units) Memory(cgroup string) (unit.MemoryUse, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	name := strings.TrimSuffix(filepath.Base(cgroup), ".service")
	return unit.MemoryUse{Anon: u.anon[name], Swap: u.swap[name]}, nil
}

// List returns all loaded hotbed-* unit names in slice (empty defaults to
// unit.DefaultSlice), matching systemd's RemainAfterExit behavior where units
// stay listed until Stop removes them.
func (u *Units) List(_ context.Context, slice string) ([]string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	wantSlice := cmp.Or(slice, unit.DefaultSlice)
	var names []string
	for name := range u.units {
		if !strings.HasPrefix(name, "hotbed-") {
			continue
		}
		if cmp.Or(u.specs[name].Slice, unit.DefaultSlice) != wantSlice {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names, nil
}

// Stop kills what is left of the unit and forgets it.
func (u *Units) Stop(_ context.Context, name string) error {
	u.mu.Lock()
	p, ok := u.units[name]
	u.mu.Unlock()
	if !ok {
		return nil
	}
	select {
	case <-p.exited:
	default:
		syscall.Kill(-p.cmd.Process.Pid, syscall.SIGCONT)
		syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		<-p.exited // Start's goroutine reaps it
	}
	u.mu.Lock()
	delete(u.units, name)
	u.mu.Unlock()
	return nil
}

// Close kills every unit still running (including its process group), prevents
// new units from starting, and waits until every unit's process and reaper
// goroutine have exited completely. Safe to call multiple times.
func (u *Units) Close() {
	u.mu.Lock()
	u.closed = true
	procs := make([]*proc, 0, len(u.units))
	for _, p := range u.units {
		procs = append(procs, p)
	}
	u.units = nil
	u.mu.Unlock()
	for _, p := range procs {
		select {
		case <-p.exited:
		default:
			pid := p.cmd.Process.Pid
			_ = syscall.Kill(-pid, syscall.SIGCONT)
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			<-p.exited
		}
	}
	u.wg.Wait()
}

// System is a machine whose architecture, memory, load and pressure tests
// set.
type System struct {
	mu   sync.Mutex
	Arch string
	Mem  sysinfo.Mem
	Psi  float64
	// CPUPsi is CPU pressure, PSI some avg10 in percent.
	CPUPsi float64
	Busy   float64
}

func NewSystem() *System {
	return &System{Arch: "arm64", Mem: sysinfo.Mem{Total: 16 << 30, Available: 12 << 30, SwapTotal: 8 << 30, SwapFree: 8 << 30}}
}

func (s *System) Resources() (*pb.Resources, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &pb.Resources{Arch: s.Arch, Cpus: 8, MemoryBytes: s.Mem.Total, SwapBytes: s.Mem.SwapTotal, Kernel: "fake", Machine: "Test Machine"}, nil
}

func (s *System) Meminfo() (sysinfo.Mem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Mem, nil
}

func (s *System) Pressure(resource string) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch resource {
	case "memory":
		return s.Psi, nil
	case "cpu":
		return s.CPUPsi, nil
	}
	return 0, nil
}

func (s *System) CPUBusy() (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Busy, nil
}

func (s *System) OOMKills() (int64, error) { return 0, nil }

// SetBusy sets the share of its CPUs kept busy, 0 to 1.
func (s *System) SetBusy(busy float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Busy = busy
}

// SetCPUPressure sets CPU pressure, PSI some avg10 in percent.
func (s *System) SetCPUPressure(psi float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.CPUPsi = psi
}

// SetUsed sets how much memory is in use.
func (s *System) SetUsed(used int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Mem.Available = s.Mem.Total - used
}

// SetMem replaces the fake system's memory and swap readings.
func (s *System) SetMem(m sysinfo.Mem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Mem = m
}

// Isolation stands in for a node's measurement isolation: it records what
// the engine asks of it, and loses its hold when a test says so.
type Isolation struct {
	cpus bench.CPUs
	work int

	mu        sync.Mutex
	active    bool
	ons, offs int
	lost      error
	offErr    error
	pool      string
	loud      []bench.Writer
	samples   int
	heldOwn   string
}

// NewIsolation isolates cpus (a cpulist), leaving work CPUs to the rest.
func NewIsolation(cpus string, work int) *Isolation {
	cs, err := bench.ParseCPUs(cpus)
	if err != nil {
		panic(err)
	}
	return &Isolation{cpus: cs, work: work}
}

func (f *Isolation) Slice() string            { return "hotbed_bench.slice" }
func (f *Isolation) CPUs() bench.CPUs         { return f.cpus }
func (f *Isolation) Count() (bench, work int) { return len(f.cpus), f.work }

func (f *Isolation) Active() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active
}

func (f *Isolation) On(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active = true
	f.ons++
	return nil
}

func (f *Isolation) Off(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offs++
	if f.offErr != nil {
		return f.offErr
	}
	f.active = false
	return nil
}

// SetOffErr has Off fail with err, leaving the node isolated.
func (f *Isolation) SetOffErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offErr = err
}

func (f *Isolation) Held(own string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heldOwn = own
	return f.lost
}

// HeldOwn is the volume the last Held was asked about, as the
// measurement's own.
func (f *Isolation) HeldOwn() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.heldOwn
}

func (f *Isolation) Pool() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pool
}

// SetPool has the isolation hold back writers to the ZFS pool dataset.
func (f *Isolation) SetPool(dataset string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pool = dataset
}

func (f *Isolation) Sample() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.samples++
}

// Samples counts the Samples taken.
func (f *Isolation) Samples() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.samples
}

func (f *Isolation) Loud(own string) []bench.Writer {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []bench.Writer
	for _, w := range f.loud {
		if w.Volume != own {
			out = append(out, w)
		}
	}
	return out
}

// SetLoud has Loud name writers from now on.
func (f *Isolation) SetLoud(writers ...bench.Writer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loud = writers
}

// SetLost has Held report err from now on.
func (f *Isolation) SetLost(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lost = err
}

// SetActive leaves the node isolated, as a worker that stopped would.
func (f *Isolation) SetActive(active bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active = active
}

// Ons and Offs count the times the node was isolated and given back.
func (f *Isolation) Ons() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ons
}

func (f *Isolation) Offs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.offs
}
