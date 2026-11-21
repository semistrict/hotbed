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
	"strings"
	"sync"
	"syscall"

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
}

type proc struct {
	cmd    *exec.Cmd
	done   bool
	code   int
	signal int
	frozen bool
}

func (u *Units) Start(_ context.Context, s unit.Spec) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.units == nil {
		u.units = map[string]*proc{}
	}
	if _, dup := u.units[s.Name]; dup {
		return fmt.Errorf("unit %s exists", s.Name)
	}
	rewrite := func(x string) string { return x }
	for _, b := range s.Binds {
		src, dst := b.Source, b.Target
		prev := rewrite
		rewrite = func(x string) string { return strings.ReplaceAll(prev(x), dst, src) }
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
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = rewrite(s.Dir)
	cmd.Env = os.Environ()
	for _, e := range s.Env {
		cmd.Env = append(cmd.Env, rewrite(e))
	}
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		log.Close()
		return err
	}
	p := &proc{cmd: cmd}
	u.units[s.Name] = p
	go func() {
		err := cmd.Wait()
		log.Close()
		u.mu.Lock()
		defer u.mu.Unlock()
		p.done = true
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
	if !p.done {
		syscall.Kill(-p.cmd.Process.Pid, syscall.SIGCONT)
		syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		p.cmd.Process.Wait()
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
