// Package fake stands in for the parts of a worker that need Linux and
// root, so tests run anywhere: a store that copies directories, units that
// run commands as local processes, and a machine whose memory tests set.
//
// They keep the contracts the engine relies on, not the performance: a
// clone here is a full copy.
package fake

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/semistrict/hotbed/internal/bench"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/storage"
	"github.com/semistrict/hotbed/internal/sysinfo"
	"github.com/semistrict/hotbed/internal/unit"
)

// Store keeps volumes and snapshots as directories, copied in full.
type Store struct {
	Root string
}

func (s *Store) Kind() string                { return "fake" }
func (s *Store) Path(name string) string     { return filepath.Join(s.Root, "vol", name) }
func (s *Store) snapPath(name string) string { return filepath.Join(s.Root, "snap", name) }

func (s *Store) Create(_ context.Context, name string) error {
	if exists(s.Path(name)) {
		return fmt.Errorf("%s: %w", name, storage.ErrExists)
	}
	return os.MkdirAll(s.Path(name), 0o755)
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
	return copyTree(s.snapPath(snap), s.Path(name))
}

func (s *Store) Destroy(_ context.Context, name string) error {
	if !exists(s.Path(name)) {
		return fmt.Errorf("%s: %w", name, storage.ErrNotFound)
	}
	return os.RemoveAll(s.Path(name))
}

func (s *Store) DestroySnapshot(_ context.Context, snap string) error {
	if !exists(s.snapPath(snap)) {
		return fmt.Errorf("%s: %w", snap, storage.ErrNotFound)
	}
	return os.RemoveAll(s.snapPath(snap))
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
	out, err := exec.Command("cp", "-Rp", src, dst).CombinedOutput()
	if err != nil {
		return fmt.Errorf("cp -Rp %s %s: %w: %s", src, dst, err, out)
	}
	return nil
}

// Units runs each command as a local bash process. Commands see their
// workspace at /work through a bind mount on a real worker; here every
// "/work" in their arguments, directory and environment is rewritten to the
// volume's path instead.
type Units struct {
	mu    sync.Mutex
	units map[string]*proc
	specs map[string]unit.Spec
	cores float64
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
	cmd     *exec.Cmd
	started time.Time
	exited  chan struct{} // closed once done
	done    bool
	code    int
	signal  int
	frozen  bool
}

func (u *Units) Start(_ context.Context, s unit.Spec) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.units == nil {
		u.units, u.specs = map[string]*proc{}, map[string]unit.Spec{}
	}
	if _, dup := u.units[s.Name]; dup {
		return fmt.Errorf("unit %s exists", s.Name)
	}
	rewrite := func(x string) string { return x }
	for _, b := range s.Binds {
		// The bind's target as a whole path component: /work/app, not /worker.
		re := regexp.MustCompile(`(^|[^\w.-])` + regexp.QuoteMeta(b.Target) + `(/|$|[^\w.-])`)
		src, prev := b.Source, rewrite
		rewrite = func(x string) string {
			x = prev(x)
			for range 2 { // a match takes the character after it, which may begin the next
				x = re.ReplaceAllString(x, "${1}"+strings.ReplaceAll(src, "$", "$$")+"${2}")
			}
			return x
		}
	}
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
	// Like a real unit, put the spec's directories around PATH before
	// looking the command up.
	cmd := exec.Command("/bin/sh", append([]string{"-c", `PATH="${HOTBED_PATH:+$HOTBED_PATH:}$PATH${HOTBED_PATH_AFTER:+:$HOTBED_PATH_AFTER}"; exec "$@"`, "hotbed"}, args...)...)
	cmd.Dir = rewrite(s.Dir)
	cmd.Env = os.Environ()
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
	cmd.Env = append(cmd.Env, "HOTBED_PATH="+paths(s.PathPrefix), "HOTBED_PATH_AFTER="+paths(s.PathSuffix))
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		log.Close()
		return err
	}
	p := &proc{cmd: cmd, started: time.Now(), exited: make(chan struct{})}
	u.units[s.Name] = p
	u.specs[s.Name] = s
	go func() {
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

func (u *Units) Status(_ context.Context, name string) (unit.State, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	p, ok := u.units[name]
	if !ok {
		return unit.State{}, nil
	}
	return unit.State{
		Loaded: true, Running: !p.done, Exited: p.done, Frozen: p.frozen,
		ExitCode: p.code, Signal: p.signal, PID: p.cmd.Process.Pid,
		Cgroup: "/hotbed.slice/" + name + ".service", Memory: 64 << 20,
		CPUNanos: uint64(u.cores * float64(time.Since(p.started))),
	}, nil
}

func (u *Units) signal(name string, sig syscall.Signal) error {
	u.mu.Lock()
	p, ok := u.units[name]
	u.mu.Unlock()
	if !ok {
		return fmt.Errorf("no unit %s", name)
	}
	if p.done {
		return nil
	}
	return syscall.Kill(-p.cmd.Process.Pid, sig)
}

func (u *Units) Freeze(_ context.Context, name string) error {
	if err := u.signal(name, syscall.SIGSTOP); err != nil {
		return err
	}
	u.mu.Lock()
	u.units[name].frozen = true
	u.mu.Unlock()
	return nil
}

func (u *Units) Thaw(_ context.Context, name string) error {
	if err := u.signal(name, syscall.SIGCONT); err != nil {
		return err
	}
	u.mu.Lock()
	u.units[name].frozen = false
	u.mu.Unlock()
	return nil
}

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

func (u *Units) Reclaim(string, int64) error { return nil }
func (u *Units) Swap(string) (int64, error)  { return 0, nil }

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

// System is a machine whose memory and pressure tests set.
type System struct {
	mu   sync.Mutex
	Mem  sysinfo.Mem
	Psi  float64
	Busy float64
}

func NewSystem() *System {
	return &System{Mem: sysinfo.Mem{Total: 16 << 30, Available: 12 << 30, SwapTotal: 8 << 30, SwapFree: 8 << 30}}
}

func (s *System) Resources() (*pb.Resources, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &pb.Resources{Arch: "arm64", Cpus: 8, MemoryBytes: s.Mem.Total, SwapBytes: s.Mem.SwapTotal, Kernel: "fake", Machine: "Test Machine"}, nil
}

func (s *System) Meminfo() (sysinfo.Mem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Mem, nil
}

func (s *System) Pressure(resource string) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if resource == "memory" {
		return s.Psi, nil
	}
	return 0, nil
}

func (s *System) CPUBusy() (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Busy, nil
}

func (s *System) OOMKills() (int64, error) { return 0, nil }

// SetUsed sets how much memory is in use.
func (s *System) SetUsed(used int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Mem.Available = s.Mem.Total - used
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

func (f *Isolation) Held() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lost
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
