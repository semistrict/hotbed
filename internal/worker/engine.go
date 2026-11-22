package worker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/storage"
	"github.com/semistrict/hotbed/internal/sysinfo"
	"github.com/semistrict/hotbed/internal/unit"
)

// WorkPath is where every command sees its workspace volume; the checkout
// is at WorkPath/<repo>, the same path the base was built at.
const WorkPath = "/work"

// The thresholds the node runs work by.
const (
	admitMemory   = 0.86 // admit while guarantees fit in this share of RAM
	admitPressure = 22.0 // and memory pressure (PSI some avg10, %) is below this
	parkAt        = 0.93 // park the newest command when memory in use passes this
	thawBelow     = 0.78 // thaw the oldest parked one when it fits under this
	minFreeDisk   = 0.05 // keep this share of the disk free before admitting
	orphanAfter   = 30 * time.Second
	keepEnded     = 2 * time.Minute
	keepReleased  = 10 * time.Minute
)

// Units starts and controls commands; Systemd is the real one.
type Units interface {
	Start(ctx context.Context, s unit.Spec) error
	Status(ctx context.Context, name string) (unit.State, error)
	Freeze(ctx context.Context, name string) error
	Thaw(ctx context.Context, name string) error
	Reclaim(cgroup string, n int64) error
	Swap(cgroup string) (int64, error)
	Stop(ctx context.Context, name string) error
	// Wait returns once the unit's processes have all exited (or ctx is
	// done), so the engine notices an exit without waiting for its tick.
	Wait(ctx context.Context, name string) error
}

// System measures the node; Linux is the real one.
type System interface {
	Resources() (*pb.Resources, error)
	Meminfo() (sysinfo.Mem, error)
	Pressure(resource string) (float64, error)
	CPUBusy() (float64, error)
	OOMKills() (int64, error)
}

// EngineConfig is what an Engine runs on.
type EngineConfig struct {
	Dir    string // state directory: logs, bundles, worker.db
	NodeID string
	Host   string // tailnet host name, for dev server URLs
	User   string // commands run as this user
	// Slice is the systemd slice commands run in; empty for hotbed.slice.
	// One under a capped slice (user-hotbed.slice, in user.slice) shares that
	// slice's memory, and the engine admits and parks against it.
	Slice string
	// TmpDir holds each command's TMPDIR, a directory of its own removed
	// when it ends; empty for the state directory's tmp.
	TmpDir string
	// UnitProperties are systemd properties every command gets, to grant
	// what builds need beyond a plain user: AmbientCapabilities=,
	// DeviceAllow=, LimitMEMLOCK= and so on.
	UnitProperties []string
	Store          storage.Store
	Units          Units
	System         System
	Now            func() time.Time
}

// Engine is a worker's bases, workspaces and commands. The messages it
// holds are its own; it hands out clones.
type Engine struct {
	cfg  EngineConfig
	db   *db
	cred *syscall.Credential
	home string

	mu         sync.Mutex
	repos      map[string]*pb.Repo
	bases      map[string]*pb.Base
	workspaces map[string]*workspace
	commands   map[string]*command
	usage      *pb.NodeUsage
	disk       *pb.DiskUsage
	diskBytes  int64
	oomKills   int64
	resources  *pb.Resources
	// active is the sessions clients used here since the last heartbeat.
	active map[string]bool

	kick    chan struct{} // an event was written: push soon
	collect chan struct{} // a base may have become garbage: collect soon
	stepc   chan struct{} // a command may start or have ended: step soon
	// updated is closed, and replaced, whenever a base, workspace or
	// command changes, to wake whoever watches them.
	updated chan struct{}

	// One workspace request at a time decides how to bring a repo's base up
	// to date, so two requests share one freshen.
	repoLocks sync.Map // repo ID -> *sync.Mutex
}

// workspace is a workspace and what only the worker needs about it.
type workspace struct {
	rec  *pb.WorkspaceRecord
	sync sync.Mutex // one sync at a time
}

func (w *workspace) ws() *pb.Workspace { return w.rec.Workspace }

// command is a command and what only the worker needs about it.
type command struct {
	rec *pb.CommandRecord

	cpuNanos   uint64
	cpuAt      time.Time
	followers  int
	lastFollow time.Time
	// stopping is set while Stop ends the command, which then owns its
	// ending: its unit exiting meanwhile is the stop, not a failure.
	stopping bool
}

func (c *command) cmd() *pb.Command { return c.rec.Command }
func (c *command) unitName() string { return "hotbed-" + c.cmd().Id }

func live(s pb.CommandState) bool {
	return s == pb.CommandState_COMMAND_STATE_QUEUED || s == pb.CommandState_COMMAND_STATE_RUNNING || s == pb.CommandState_COMMAND_STATE_PARKED
}

// NewEngine loads the worker's state from dir.
func NewEngine(ctx context.Context, cfg EngineConfig) (*Engine, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cred, home, err := lookupUser(cfg.User)
	if err != nil {
		return nil, err
	}
	// The command user runs git in volumes below the state directory and
	// reads sync bundles from it, so it may pass through (not list) them.
	for _, d := range []string{"", "logs", "bundles"} {
		dir := filepath.Join(cfg.Dir, d)
		if err := os.MkdirAll(dir, 0o711); err != nil {
			return nil, err
		}
		if err := os.Chmod(dir, 0o711); err != nil {
			return nil, err
		}
	}
	d, err := openDB(ctx, filepath.Join(cfg.Dir, "worker.db"))
	if err != nil {
		return nil, err
	}
	e := &Engine{
		cfg: cfg, db: d, cred: cred, home: home,
		bases:      map[string]*pb.Base{},
		workspaces: map[string]*workspace{}, commands: map[string]*command{},
		usage: &pb.NodeUsage{}, disk: &pb.DiskUsage{}, active: map[string]bool{},
		kick: make(chan struct{}, 1), collect: make(chan struct{}, 1), stepc: make(chan struct{}, 1),
		updated: make(chan struct{}),
	}
	if e.resources, err = cfg.System.Resources(); err != nil {
		d.close()
		return nil, err
	}
	if err := e.load(); err != nil {
		d.close()
		return nil, fmt.Errorf("load worker state: %w", err)
	}
	return e, nil
}

func (e *Engine) load() error {
	if err := e.db.all("base", func() proto.Message { return &pb.Base{} }, func(m proto.Message) {
		b := m.(*pb.Base)
		e.bases[b.Id] = b
	}); err != nil {
		return err
	}
	if err := e.db.all("workspace", func() proto.Message { return &pb.WorkspaceRecord{} }, func(m proto.Message) {
		r := m.(*pb.WorkspaceRecord)
		e.workspaces[r.Workspace.Id] = &workspace{rec: r}
	}); err != nil {
		return err
	}
	return e.db.all("command", func() proto.Message { return &pb.CommandRecord{} }, func(m proto.Message) {
		r := m.(*pb.CommandRecord)
		// A command that was running when the worker stopped is still a
		// systemd unit; the next tick reads how it is doing.
		e.commands[r.Command.Id] = &command{rec: r, lastFollow: e.now()}
	})
}

// Close releases the database. Commands keep running.
func (e *Engine) Close() error { return e.db.close() }

// Run drives the node until ctx ends: commands every second, samples every
// report interval, disk usage and old bases every two minutes. Bases are
// built only when a workspace asks for one, never here.
func (e *Engine) Run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	sample := time.NewTicker(api.ReportInterval)
	defer sample.Stop()
	usage := time.NewTicker(2 * time.Minute)
	defer usage.Stop()
	e.sample()
	e.measureDisk(ctx)
	e.sweepTmp()
	e.mu.Lock()
	for _, c := range e.commands {
		if s := c.cmd().State; s == running || s == parked {
			go e.watchExit(ctx, c.unitName())
		}
	}
	e.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			e.step(ctx)
		case <-e.stepc:
			e.step(ctx)
		case <-sample.C:
			e.sample()
		case <-e.collect:
			e.collectBases(ctx)
		case <-usage.C:
			e.collectBases(ctx)
			e.measureDisk(ctx)
		}
	}
}

// Kicked is signalled whenever an event was written.
func (e *Engine) Kicked() <-chan struct{} { return e.kick }

// stepSoon asks Run to step now rather than at its next tick: a command was
// queued, or one may have ended.
func (e *Engine) stepSoon() {
	select {
	case e.stepc <- struct{}{}:
	default:
	}
}

// watchExit steps as soon as a command's unit has exited. The tick would
// notice too, a second later.
func (e *Engine) watchExit(ctx context.Context, name string) {
	if err := e.cfg.Units.Wait(ctx, name); err != nil {
		if ctx.Err() == nil {
			slog.Warn("watch command for its exit", "unit", name, "err", err)
		}
		return
	}
	e.stepSoon()
}

// notify wakes everyone waiting on updated. Call with e.mu held.
func (e *Engine) notify() {
	close(e.updated)
	e.updated = make(chan struct{})
}

// collectSoon asks Run to delete the bases no one needs any more.
func (e *Engine) collectSoon() {
	select {
	case e.collect <- struct{}{}:
	default:
	}
}

func (e *Engine) changed() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

func (e *Engine) now() time.Time { return e.cfg.Now() }

func (e *Engine) nowMs() int64 { return e.now().UnixMilli() }

// SetRepos is what the controller says this node hosts.
// SetRepos records the cluster's repos, as the controller reports them. The
// bases of a repo that was removed go stale, so they are deleted once no
// workspace uses them.
func (e *Engine) SetRepos(repos []*pb.Repo) {
	e.mu.Lock()
	defer e.mu.Unlock()
	next := map[string]*pb.Repo{}
	for _, r := range repos {
		next[r.Id] = r
	}
	e.repos = next
	removed := false
	for _, b := range e.bases {
		if next[b.RepoId] == nil && (b.State == warm || b.State == broken) {
			b.State = stale
			e.saveBase(b, change{what: "stale", detail: "its repo was removed"})
			removed = true
		}
	}
	if removed {
		e.collectSoon()
	}
}

func (e *Engine) repoLock(id string) *sync.Mutex {
	m, _ := e.repoLocks.LoadOrStore(id, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func (e *Engine) sample() {
	busy, err := e.cfg.System.CPUBusy()
	if err != nil {
		slog.Warn("sample cpu", "err", err)
	}
	mem, err := e.cfg.System.Meminfo()
	if err != nil {
		slog.Warn("sample memory", "err", err)
	}
	memP, err := e.cfg.System.Pressure("memory")
	if err != nil {
		slog.Warn("sample memory pressure", "err", err)
	}
	cpuP, err := e.cfg.System.Pressure("cpu")
	if err != nil {
		slog.Warn("sample cpu pressure", "err", err)
	}
	oom, err := e.cfg.System.OOMKills()
	if err != nil {
		slog.Warn("sample oom kills", "err", err)
	}
	e.mu.Lock()
	e.oomKills = oom
	e.usage = &pb.NodeUsage{
		CpuBusy:         busy,
		MemoryUsedBytes: mem.Used(),
		SwapUsedBytes:   mem.SwapTotal - mem.SwapFree,
		CpuPressure:     cpuP,
		MemoryPressure:  memP,
	}
	e.mu.Unlock()
	e.changed()
}

// measureDisk reads the filesystem's space and what each base and
// workspace holds. Usage walks volumes on btrfs, so it runs rarely.
func (e *Engine) measureDisk(ctx context.Context) {
	sp, err := e.cfg.Store.Space(ctx)
	if err != nil {
		slog.Warn("measure disk", "err", err)
		return
	}
	e.mu.Lock()
	vols := map[string]string{}
	for _, b := range e.bases {
		vols["b:"+b.Id] = baseVolume(b.RepoId, b.Generation)
	}
	for _, w := range e.workspaces {
		if w.ws().State != pb.WorkspaceState_WORKSPACE_STATE_RELEASED {
			vols["w:"+w.ws().Id] = w.rec.Volume
		}
	}
	e.mu.Unlock()
	use := map[string]storage.Usage{}
	for k, v := range vols {
		u, err := e.cfg.Store.Usage(ctx, v)
		if err != nil {
			slog.Warn("measure volume", "volume", v, "err", err)
			continue
		}
		use[k] = u
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var bases, forks int64
	for _, b := range e.bases {
		if u, ok := use["b:"+b.Id]; ok {
			b.SizeBytes = u.Referenced
		}
		if b.State != pb.BaseState_BASE_STATE_STALE {
			bases += b.SizeBytes
		}
	}
	for _, w := range e.workspaces {
		ws := w.ws()
		if u, ok := use["w:"+ws.Id]; ok {
			ws.ExclusiveBytes = u.Exclusive
			ws.SharedBytes = max(0, u.Referenced-u.Exclusive)
		}
		if ws.State != pb.WorkspaceState_WORKSPACE_STATE_RELEASED {
			forks += ws.ExclusiveBytes
		}
	}
	e.diskBytes = sp.Size
	e.disk = &pb.DiskUsage{BasesBytes: bases, ForksExclusiveBytes: forks, OtherBytes: max(0, sp.Size-sp.Free-bases-forks), FreeBytes: sp.Free}
}

func clone[T proto.Message](m T) T { return proto.Clone(m).(T) }
