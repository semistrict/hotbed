package macworker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	machinedv1 "github.com/semistrict/cladefs/go/machined/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Supervisor runs a Mac's worker for hotbed's daemon: machined, then the
// worker (hotbed serve) once machined serves, each started again when it
// ends (a machined that ends takes the worker's VMs with it, so the worker
// is restarted beside the new one), and both stopped while the worker is to
// stay stopped (Paths.Stop) or unconfigured, and when Run returns — unless
// PreserveOnExit reports a daemon restart handoff, in which case machined
// and the worker stay running for the next daemon to adopt so active VMs
// survive hotbed upgrades.
type Supervisor struct {
	Paths   Paths
	Release Release
	// Exe is the hotbed binary the worker runs from (hotbed serve): one
	// that installs a newer build over itself execs it in place.
	Exe  string
	HTTP *http.Client
	Log  *slog.Logger
	// OnState hears each change in how the worker stands.
	OnState func(State)
	// Machined is the machined binary to run instead of Release's (tests).
	Machined string
	// Poll is how often it checks whether the worker is to run; 2 s if 0.
	Poll time.Duration
	// MachinedStop and WorkerStop are how long each may take to stop on
	// SIGTERM before it is killed: 90 s and 30 s if 0. machined drains
	// its cladefs daemon, its machines stopped, in up to a minute.
	MachinedStop, WorkerStop time.Duration
	// PreserveOnExit, when it returns true as Run exits, leaves machined
	// and the worker running for the restarted daemon to adopt rather than
	// tearing down active VMs.
	PreserveOnExit func() bool
	// MachinedIdle reports whether machined has no active VMs so it may be
	// restarted to move to a newly pinned Release. Defaults to querying
	// machined's ListMachines RPC on Paths.Socket.
	MachinedIdle func(context.Context) (bool, error)
}

// child is a process the Supervisor started or adopted from a prior daemon.
type child struct {
	name    string
	pidNum  int
	cmd     *exec.Cmd // nil when adopted
	pidFile string    // its pid file
	done    chan struct{}
	err     error // how it ended, once done is closed
	log     string
}

// maxLogBytes is how large a log grows before a start begins it anew.
const maxLogBytes = 64 << 20

// Run supervises the worker until ctx ends, then stops it.
func (s *Supervisor) Run(ctx context.Context) {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	poll := s.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	tick := time.NewTicker(poll)
	defer tick.Stop()

	var (
		mach, work      *child
		machAdopted     bool
		deferredUpgrade bool
		backoff         time.Duration
		retryAt         time.Time
		last            State
		detail          string // why the last start failed or a process ended
		release         string
		since           time.Time // when the worker last started
	)
	// watchCtx bounds poll goroutines monitoring adopted processes; cancelled
	// AFTER the shutdown/handoff defer runs (LIFO) so stopAll can still wait on
	// adopted children's done channels during full shutdown.
	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()

	report := func(state string) {
		st := State{
			Configured: s.Paths.Configured(), Autostart: !s.Paths.stopped(), State: state,
			Detail: detail, Release: release,
		}
		if mach != nil {
			st.MachinedPID = mach.pidNum
		}
		if work != nil {
			st.WorkerPID = work.pidNum
		}
		if state == "running" {
			st.Detail = ""
		}
		cmp := st
		cmp.UpdatedAt = last.UpdatedAt
		if cmp == last {
			return
		}
		st.UpdatedAt = time.Now().UTC()
		last = st
		if err := s.Paths.writeState(st); err != nil {
			s.Log.Warn("macworker: record the worker's state", "err", err)
		}
		if s.OnState != nil {
			s.OnState(st)
		}
	}
	failed := func(what string, err error) {
		backoff = min(max(2*backoff, 2*time.Second), time.Minute)
		retryAt = time.Now().Add(backoff)
		detail = fmt.Sprintf("%s: %v", what, err)
		s.Log.Error("macworker: "+what, "err", err, "retryIn", backoff)
	}
	stopAll := func() {
		if work != nil {
			s.stop(work, cmpOr(s.WorkerStop, 30*time.Second))
			work = nil
		}
		if mach != nil {
			s.stop(mach, cmpOr(s.MachinedStop, 90*time.Second))
			mach = nil
			machAdopted = false
			deferredUpgrade = false
		}
	}
	defer func() {
		if s.PreserveOnExit != nil && s.PreserveOnExit() && s.Paths.Configured() && !s.Paths.stopped() && (mach != nil || work != nil) {
			var mpid, wpid int
			if mach != nil {
				mpid = mach.pidNum
			}
			if work != nil {
				wpid = work.pidNum
			}
			s.Log.Info("macworker: handing off running processes for daemon restart", "machinedPid", mpid, "workerPid", wpid)
			return
		}
		if mach != nil || work != nil {
			report("stopping")
		}
		stopAll()
		report("stopped")
	}()

	for {
		switch {
		case !s.Paths.Configured():
			stopAll()
			detail = ""
			report("unconfigured")
		case s.Paths.stopped():
			if mach != nil || work != nil {
				report("stopping")
			}
			stopAll()
			detail, backoff = "", 0
			report("stopped")
		case time.Now().Before(retryAt):
			report("error")
		case mach == nil:
			if m, rel := s.adoptMachined(watchCtx, poll); m != nil {
				mach, release, machAdopted = m, rel, true
				s.Log.Info("macworker: adopted running machined", "pid", m.pidNum, "release", rel)
			} else {
				report("starting")
				m, rel, err := s.startMachined(ctx)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					failed("start machined", err)
					report("error")
					break
				}
				mach, release, machAdopted = m, rel, false
			}
			fallthrough
		case work == nil:
			if machAdopted {
				machAdopted = false
				if w := s.adoptWorker(watchCtx, poll); w != nil {
					work, since = w, time.Now()
					s.Log.Info("macworker: adopted running worker", "pid", w.pidNum)
					report("running")
					break
				}
			}
			if err := s.Paths.ensureConfig(); err != nil {
				failed("configure the worker", err)
				report("error")
				break
			}
			w, err := s.start(ctx, "worker", filepath.Join(s.Paths.Logs, "worker.log"),
				filepath.Join(s.Paths.Base, "worker.pid"), s.Exe, "serve", s.Paths.Worker)
			if err != nil {
				failed("start the worker", err)
				report("error")
				break
			}
			work, since = w, time.Now()
			report("running")
		default:
			// Up a while: the next failure backs off from the start.
			if time.Since(since) > time.Minute {
				backoff = 0
			}
			report("running")
			if want := s.wantRelease(); want != "" && release != want {
				idle, err := s.isMachinedIdle(ctx)
				if err != nil {
					if !deferredUpgrade {
						s.Log.Warn("macworker: check machined idle before upgrade", "was", release, "want", want, "err", err)
						deferredUpgrade = true
					}
				} else if !idle {
					if !deferredUpgrade {
						s.Log.Info("macworker: deferring machined upgrade while VMs run", "was", release, "want", want)
						deferredUpgrade = true
					}
				} else {
					if s.Machined == "" {
						if _, err := s.Release.Ensure(ctx, s.Paths, s.HTTP); err != nil {
							s.Log.Warn("macworker: prefetch new machined release before upgrade", "want", want, "err", err)
							break
						}
					}
					s.Log.Info("macworker: upgrading machined while no VMs run", "was", release, "want", want)
					stopAll()
					continue
				}
			}
		}

		var machDone, workDone chan struct{}
		if mach != nil {
			machDone = mach.done
		}
		if work != nil {
			workDone = work.done
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-machDone:
			// Its machines, the worker's VMs, ended with it: the worker
			// starts again beside the next machined, taking none back.
			failed("machined ended", endedHow(mach))
			mach = nil
			if work != nil {
				s.stop(work, cmpOr(s.WorkerStop, 30*time.Second))
				work = nil
			}
		case <-workDone:
			failed("the worker ended", endedHow(work))
			work = nil
		}
	}
}

// startMachined starts machined, its release's binaries fetched if they are
// not there yet, and returns once it serves its socket.
func (s *Supervisor) startMachined(ctx context.Context) (*child, string, error) {
	bin, release := s.Machined, s.wantRelease()
	if bin == "" {
		var err error
		if bin, err = s.Release.Ensure(ctx, s.Paths, s.HTTP); err != nil {
			return nil, "", err
		}
		release = s.Release.Tag
	}
	for _, d := range []string{s.Paths.Machined, s.Paths.Images} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, "", err
		}
	}
	// machined refuses a socket that is there: one a machined left that is
	// not running (stopStale stopped any that was).
	if err := os.Remove(s.Paths.Socket); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, "", err
	}
	c, err := s.start(ctx, "machined", filepath.Join(s.Paths.Logs, "machined.log"),
		filepath.Join(s.Paths.Machined, "machined.pid"), bin,
		"--socket", s.Paths.Socket, "--images", s.Paths.Images, "--state", s.Paths.State)
	if err != nil {
		return nil, "", err
	}
	// No deadline: machined serves only once its cladefs daemon has opened
	// its part cache, which reads every cached part on a filesystem whose
	// holes it can't find (APFS), minutes for a Mac's hundreds of GiB. A
	// deadline killed it mid-boot, forever (issue 33's rollout); a machined
	// that fails ends, and the worker's state says it is starting meanwhile.
	wait := time.NewTicker(20 * time.Millisecond)
	defer wait.Stop()
	for {
		if _, err := os.Stat(s.Paths.Socket); err == nil {
			_ = os.WriteFile(s.machinedReleasePath(), []byte(release+"\n"), 0o600)
			return c, release, nil
		}
		select {
		case <-c.done:
			return nil, "", fmt.Errorf("machined ended before serving: %v", endedHow(c))
		case <-ctx.Done():
			s.stop(c, cmpOr(s.MachinedStop, 90*time.Second))
			return nil, "", ctx.Err()
		case <-wait.C:
		}
	}
}

func (s *Supervisor) machinedReleasePath() string {
	return filepath.Join(s.Paths.Machined, "machined.release")
}

func (s *Supervisor) wantRelease() string {
	if s.Machined != "" {
		if s.Release.Tag != "" {
			return s.Release.Tag
		}
		return "local"
	}
	return s.Release.Tag
}

func (s *Supervisor) isMachinedIdle(ctx context.Context) (bool, error) {
	if s.MachinedIdle != nil {
		return s.MachinedIdle(ctx)
	}
	return defaultMachinedIdle(ctx, s.Paths.Socket)
}

func defaultMachinedIdle(ctx context.Context, socket string) (bool, error) {
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	dial := func(ctx context.Context, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}
	conn, err := grpc.NewClient("passthrough:///machined",
		grpc.WithContextDialer(dial),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return false, err
	}
	defer conn.Close()
	got, err := machinedv1.NewMachinedClient(conn).ListMachines(dctx, &machinedv1.ListMachinesRequest{})
	if err != nil {
		return false, err
	}
	for _, m := range got.GetMachines() {
		switch m.GetStatus().GetState() {
		case machinedv1.MachineState_MACHINE_STATE_STARTING,
			machinedv1.MachineState_MACHINE_STATE_RUNNING,
			machinedv1.MachineState_MACHINE_STATE_SUSPENDED,
			machinedv1.MachineState_MACHINE_STATE_STOPPING:
			return false, nil
		}
	}
	return true, nil
}

// adoptMachined reconnects to a live machined handed off by a prior daemon so
// active VMs survive daemon upgrades and restarts.
func (s *Supervisor) adoptMachined(watchCtx context.Context, poll time.Duration) (*child, string) {
	pidPath := filepath.Join(s.Paths.Machined, "machined.pid")
	pid, args, ok := liveProcess(pidPath, s.Paths.Socket)
	if !ok {
		return nil, ""
	}
	if _, err := os.Stat(s.Paths.Socket); err != nil {
		return nil, ""
	}
	rel := s.adoptedMachinedRelease(args)
	return watchAdopted(watchCtx, "machined", pid, pidPath, filepath.Join(s.Paths.Logs, "machined.log"), poll), rel
}

func (s *Supervisor) adoptedMachinedRelease(args string) string {
	if b, err := os.ReadFile(s.machinedReleasePath()); err == nil {
		if rel := strings.TrimSpace(string(b)); rel != "" {
			return rel
		}
	}
	// Backwards compatibility with daemons that ran before machined.release was
	// written: the binary path is <Machined>/bin/<release>/machined.
	prefix := filepath.Join(s.Paths.Machined, "bin") + string(filepath.Separator)
	if _, after, ok := strings.Cut(args, prefix); ok {
		if rel, _, ok := strings.Cut(after, string(filepath.Separator)); ok && rel != "" {
			return rel
		}
	}
	return s.wantRelease()
}

// adoptWorker reconnects to a live worker handed off beside an adopted
// machined so active commands, shells, and base freshens suffer no disconnect.
func (s *Supervisor) adoptWorker(watchCtx context.Context, poll time.Duration) *child {
	pidPath := filepath.Join(s.Paths.Base, "worker.pid")
	pid, _, ok := liveProcess(pidPath, s.Paths.Worker)
	if !ok {
		return nil
	}
	return watchAdopted(watchCtx, "worker", pid, pidPath, filepath.Join(s.Paths.Logs, "worker.log"), poll)
}

func liveProcess(pidPath, wantArg string) (int, string, bool) {
	b, err := os.ReadFile(pidPath)
	if err != nil {
		return 0, "", false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 || syscall.Kill(pid, 0) != nil {
		return 0, "", false
	}
	out, err := exec.Command("/bin/ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, "", false
	}
	args := strings.TrimSpace(string(out))
	if args == "" || !strings.Contains(args, wantArg) {
		return 0, "", false
	}
	return pid, args, true
}

func watchAdopted(watchCtx context.Context, name string, pid int, pidPath, logPath string, poll time.Duration) *child {
	c := &child{name: name, pidNum: pid, pidFile: pidPath, done: make(chan struct{}), log: logPath}
	interval := min(poll, 250*time.Millisecond)
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-t.C:
				if syscall.Kill(pid, 0) != nil {
					c.err = errors.New("exited")
					close(c.done)
					return
				}
			}
		}
	}()
	return c
}

// start starts name's process, its output appended to logPath and its pid
// recorded in pidPath, after stopping any process of its a daemon that died
// left running.
func (s *Supervisor) start(_ context.Context, name, logPath, pidPath, bin string, args ...string) (*child, error) {
	stopStale(pidPath, filepath.Base(bin), s.Log)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Stat(logPath); err == nil && info.Size() > maxLogBytes {
		_ = os.Rename(logPath, logPath+".1")
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(log, "--- %s: starting %s %s\n", time.Now().UTC().Format(time.RFC3339), bin, strings.Join(args, " "))
	// Not the daemon's context: the Supervisor stops it, with SIGTERM
	// first, as the daemon stops.
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = log, log
	// Its own process group: a signal to the daemon's (a terminal's ^C)
	// leaves it to the Supervisor to stop.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		log.Close()
		return nil, err
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600); err != nil {
		s.Log.Warn("macworker: record a pid", "process", name, "err", err)
	}
	c := &child{name: name, pidNum: cmd.Process.Pid, cmd: cmd, pidFile: pidPath, done: make(chan struct{}), log: logPath}
	go func() {
		c.err = cmd.Wait()
		log.Close()
		close(c.done)
	}()
	s.Log.Info("macworker: started", "process", name, "pid", cmd.Process.Pid)
	return c, nil
}

// stop stops c: SIGTERM, then SIGKILL if it takes longer than grace.
func (s *Supervisor) stop(c *child, grace time.Duration) {
	_ = syscall.Kill(c.pidNum, syscall.SIGTERM)
	select {
	case <-c.done:
	case <-time.After(grace):
		s.Log.Warn("macworker: killing a process that did not stop", "process", c.name, "grace", grace)
		_ = syscall.Kill(c.pidNum, syscall.SIGKILL)
		<-c.done
	}
	_ = os.Remove(c.pidFile)
	if c.name == "machined" {
		_ = os.Remove(s.machinedReleasePath())
	}
	s.Log.Info("macworker: stopped", "process", c.name, "how", c.err)
}

// stopStale kills the process pidPath names if it still runs comm: one a
// daemon that died left behind, which would hold the worker's ports or
// machined's state.
func stopStale(pidPath, comm string, log *slog.Logger) {
	b, err := os.ReadFile(pidPath)
	if err != nil {
		return
	}
	defer os.Remove(pidPath)
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return
	}
	out, err := exec.Command("/bin/ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil || filepath.Base(strings.TrimSpace(string(out))) != comm {
		return
	}
	log.Warn("macworker: stopping a process a daemon before left running", "pid", pid, "comm", comm)
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for range 300 {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// endedHow is how c ended, with its log's last lines.
func endedHow(c *child) error {
	err := c.err
	if err == nil {
		err = errors.New("exited 0")
	}
	if tail := logTail(c.log, 1024); tail != "" {
		return fmt.Errorf("%w; its log (%s) ends:\n%s", err, c.log, tail)
	}
	return err
}

func logTail(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > n {
		_, _ = f.Seek(-n, io.SeekEnd)
	}
	b, _ := io.ReadAll(f)
	return string(bytes.TrimSpace(b))
}

func cmpOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}
