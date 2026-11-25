package worker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/cmdlog"
	"github.com/semistrict/hotbed/internal/daemonlog"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/inspect"
	"github.com/semistrict/hotbed/internal/profiler"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/storage"
	"github.com/semistrict/hotbed/internal/sysinfo"
	"github.com/semistrict/hotbed/internal/unit"
	"github.com/semistrict/hotbed/internal/version"
)

// WorkPath is where every command sees its workspace volume; each of the
// project's checkouts is at WorkPath/<repo>, the same path the base was
// built at.
const WorkPath = "/work"

// The thresholds the node runs work by.
const (
	admitMemory   = 0.86 // admit while reservations fit in this share of RAM
	admitPressure = 22.0 // and memory pressure (PSI some avg10, %) is below this
	parkAt        = 0.93 // park the newest command when memory in use passes this
	parkPressure  = 40.0 // or memory pressure (PSI some avg10, %) reaches this
	// pressureSettle is how long a park for pressure waits on the one
	// before to show in pressure, an average over 10 s.
	pressureSettle = 20 * time.Second
	thawBelow      = 0.78 // thaw the oldest parked one when it fits under this
	minFreeDisk    = 0.05 // keep this share of the disk free before admitting
	orphanAfter    = 30 * time.Second
	keepEnded      = 2 * time.Minute
	keepReleased   = 10 * time.Minute
	// keepProfiles is how long a command's profile file stays when the
	// controller may lack it: as long as the controller keeps history.
	keepProfiles = 30 * 24 * time.Hour
)

// Units starts and controls commands; Systemd is the real one.
type Units interface {
	Start(ctx context.Context, s unit.Spec) error
	Status(ctx context.Context, name string) (unit.State, error)
	StatusMany(ctx context.Context, names []string) (map[string]unit.State, error)
	Freeze(ctx context.Context, name string) error
	Thaw(ctx context.Context, name string) error
	Reclaim(cgroup string, n int64) error
	// Memory is the own memory (anonymous and swapped) of the unit at
	// cgroup, without the file cache its cgroup holds too.
	Memory(cgroup string) (unit.MemoryUse, error)
	Stop(ctx context.Context, name string) error
	// List returns the loaded hotbed-* unit names in slice (empty defaults to
	// unit.DefaultSlice).
	List(ctx context.Context, slice string) ([]string, error)
	// Wait returns once the processes of the unit name, in slice, have all
	// exited (or ctx is done), so the engine notices an exit without
	// waiting for its tick.
	Wait(ctx context.Context, slice, name string) error
	// StartShell runs an interactive or piped shell process in a workspace's
	// mount namespace / cgroup.
	StartShell(ctx context.Context, s unit.Spec, sio unit.ShellIO) (unit.ShellHandle, error)
	// Exec makes the command that runs c chrooted in a scope of its own,
	// name, under slice: the worker's own work in a workspace's rootfs, its
	// git in the workspace's checkouts and sync apply.
	Exec(ctx context.Context, name, slice string, c unit.ExecSpec) (*exec.Cmd, error)
}

func (Systemd) StatusMany(ctx context.Context, names []string) (map[string]unit.State, error) {
	return unit.StatusMany(ctx, names)
}

func (Systemd) List(ctx context.Context, slice string) ([]string, error) {
	return unit.List(ctx, cmp.Or(slice, unit.DefaultSlice))
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
	// UnitProperties are systemd properties every command gets, to grant
	// what builds need beyond a plain user: AmbientCapabilities=,
	// DeviceAllow=, LimitMEMLOCK= and so on.
	UnitProperties []string
	// Bench isolates the node for measurements; nil on a node that takes
	// none.
	Bench Isolator
	// ScratchDisk is where measurements keep their stores (the bench
	// config's disk): chrooted commands see it at the same path.
	ScratchDisk string
	// BaseImage manages the shared worker rootfs snapshot that cold bases
	// clone from. Required: every workspace and command runs in a rootfs clone.
	BaseImage *baseimage.Manager
	// GitAuth delegates centralized GitHub authentication from the controller to
	// host git and all workspace commands (HTTPS credential helper, Git SSH
	// protocol proxy, and worker `gh` REST/GraphQL shim).
	GitAuth   GitAuthClient
	Inspector inspect.Inspector
	Profiler  profiler.Profiler
	Store     storage.Store
	Units     Units
	// Runtime, when non-nil, executes workspace commands, syncs, interactive
	// shells, file queries, and --port forwarding through a WorkspaceRuntime
	// (e.g. Firecracker microVM via hotbed-guestd) instead of host chroot.
	Runtime runtime.WorkspaceRuntime
	// RuntimeFor optionally selects which workspace IDs use Runtime (enabling
	// mixed vm + chroot on one node). Nil when Runtime != nil means all
	// workspaces use Runtime.
	RuntimeFor func(wsID string) bool
	// VM, when non-nil, enables the cladefsd + Firecracker microVM backend
	// alongside chroot for workspaces requesting runtime="vm" (or by default
	// when VMConfig.Default is true).
	VM *VMBackend
	// CladefsFUSE, when non-nil, enables the cladefsd host-FUSE backend
	// alongside chroot for workspaces requesting runtime="cladefs" (or by
	// default when CladefsFUSEConfig.Default is true).
	CladefsFUSE *CladefsFUSEBackend
	System      System
	Now    func() time.Time
	// LogLimit is what a command's log keeps once past it; zero for
	// DefaultLogLimit.
	LogLimit cmdlog.Limit
	// DaemonLogs holds this worker's rotating WARN/ERROR slog store; nil disables persistence.
	DaemonLogs *daemonlog.Store
	// ControllerNodeID, when set, restricts Service.CreateWorkspace to requests
	// authenticated as this tailnet node ID (the cluster controller).
	ControllerNodeID string
	// RequireController fails Service.CreateWorkspace closed even when
	// ControllerNodeID is empty (production always sets this).
	RequireController bool
	// AllowLocalRepos permits file:// and absolute local filesystem paths as
	// repo URLs (for tests); production leaves this false.
	AllowLocalRepos bool
	// MaxSyncBundleBytes caps the total bundle bytes accepted by a single
	// Engine.Sync call (repo + submodules); zero or negative uses
	// DefaultMaxSyncBundleBytes.
	MaxSyncBundleBytes int64
	// Artifacts caches controller-managed binaries and kernels for explicit
	// system upgrades (optional; defaults to WorkerDir-backed ArtifactStore).
	Artifacts *ArtifactStore
	// SystemUpgradeOps overrides host OS actions in Engine.UpgradeSystem (for tests).
	SystemUpgradeOps *SystemUpgradeOps
}

// DefaultMaxSyncBundleBytes caps the total bundle bytes accepted by a single
// Engine.Sync call so an oversized upload cannot exhaust worker disk space.
const DefaultMaxSyncBundleBytes int64 = 2 << 30

// DefaultLogLimit keeps a command's log to about 256 MiB however much it
// writes: its first 16 MiB, where a build's first error is, and its last
// 240 MiB, where it is now.
var DefaultLogLimit = cmdlog.Limit{Head: 16 << 20, Tail: 240 << 20}

// Engine is a worker's bases, workspaces and commands. The messages it
// holds are its own; it hands out clones.
type Engine struct {
	cfg            EngineConfig
	db             *db
	cred           *syscall.Credential
	home           string
	gitAuth        *gitAuthServer
	containersConf string
	rtUnits        *runtimeUnits
	portProxies    map[string]*portProxy

	mu              sync.Mutex
	upgradeMu       sync.Mutex
	systemUpgrading bool
	// projects is what the controller's last heartbeat said this node keeps
	// bases of; nil until one has.
	projects map[string]*pb.HostedProject
	// vouched is when a workspace request last carried a project: the
	// controller's word that the node keeps it, newer than a heartbeat sent
	// before.
	vouched map[string]time.Time
	bases   map[string]*pb.Base
	// finalizingBases tracks base IDs whose snapshot and git metadata are
	// being finalized outside e.mu so collectBases does not delete them
	// mid-snapshot.
	finalizingBases map[string]bool
	workspaces      map[string]*workspace
	commands        map[string]*command
	usage           *pb.NodeUsage
	// lastMem is the most recent sysinfo.Mem reading from Sample, kept so
	// fallback memory checks pair total and used from the same domain (host
	// or enclosing cgroup slice) instead of mixing slice limit with host use.
	lastMem sysinfo.Mem
	// cpu holds new work while the CPUs are saturated; held is what holds
	// new work on the node now, if anything, and since when.
	cpu  cpuGate
	held hold
	// hostUsage is the latest host-side view of this VM worker from the
	// controller's heartbeat reply, and hostUsageUntil is the engine clock
	// deadline after which it is treated as stale and ignored. hostCPUHolding
	// tracks hysteresis for host CPU starvation.
	hostUsage      *pb.HostUsage
	hostUsageUntil time.Time
	hostCPUHolding bool
	// pressureParkedAt is when park last froze a command for memory
	// pressure.
	pressureParkedAt time.Time
	disk             *pb.DiskUsage
	diskBytes        int64
	oomKills         int64
	objectStore      *pb.ObjectStoreStats
	resources        *pb.Resources
	offErr           string    // the last failure to give back a measurement's isolation, logged once
	offRetry         time.Time // when to try to give it back again, after a failure
	// active is the sessions clients used here since the last heartbeat.
	active map[string]bool
	// SSH shell state: ephemeral host key, one-time tickets, and active shells.
	shellHostKey ssh.Signer
	shellHostPub string
	shellPort    int
	shellAddr    string
	shellTickets map[string]*shellTicket
	shells       map[string]*activeShell
	// dirtyGen increments whenever a DB save fails; savedGen tracks the last
	// dirtyGen delivered to the controller as an in-memory checkpoint.
	dirtyGen uint64
	savedGen uint64
	// applied is the last event the controller said it applied.
	applied atomic.Int64

	kick    chan struct{} // an event was written: push soon
	collect chan struct{} // a base may have become garbage: collect soon
	stepc   chan struct{} // a command may start or have ended: step soon
	// updated is closed, and replaced, whenever a base, workspace or
	// command changes, to wake whoever watches them.
	updated chan struct{}

	// One workspace request at a time decides how to bring a project's base
	// up to date, so two requests share one freshen.
	projectLocks sync.Map // project name -> *sync.Mutex

	// vmWorkspaces records workspace IDs created with runtime="vm". A workspace's
	// runtime is immutable for its lifetime, so reading vmWorkspaces never needs
	// e.mu (preventing re-entrant deadlocks when callers such as endBy already
	// hold e.mu and query usesRuntime).
	vmWorkspaces sync.Map // workspace ID -> struct{}

	// bgCtx is cancelled and closing set under e.mu when Close begins; bg
	// tracks every Engine-owned background goroutine so Close waits for them
	// before closing e.db.
	bgCtx    context.Context
	bgCancel context.CancelFunc
	bg       sync.WaitGroup
	closing  bool

	// streamsDone is closed by DrainStreams (and Close) to end long-lived
	// streaming RPCs (FollowLog, WatchWorkspace) during HTTP server shutdown
	// while in-flight unary RPCs still drain gracefully.
	streamsOnce sync.Once
	streamsDone chan struct{}
}

// volPath returns the host rootfs path for volume vol, checking CladefsFUSE
// mounts first before falling back to the host ZFS/btrfs Store.
func (e *Engine) volPath(vol string) string {
	if e.cfg.CladefsFUSE != nil {
		if p, ok := e.cfg.CladefsFUSE.MountPath(vol); ok {
			return p
		}
	}
	return e.cfg.Store.Path(vol)
}

// commandVolPath returns the host rootfs path for command c, honoring a
// managed cladefs-FUSE base build's mounted root directory when set.
func (e *Engine) commandVolPath(c *command) string {
	if c != nil && c.managedFreshen != nil && c.managedFreshen.rootDir != "" {
		return c.managedFreshen.rootDir
	}
	return e.volPath(c.rec.Volume)
}

// workRoot returns the host path corresponding to /work inside volume vol.
func (e *Engine) workRoot(vol string) string {
	return filepath.Join(e.volPath(vol), "work")
}

// baseImageKey returns the configured shared base image key.
func (e *Engine) baseImageKey() string {
	return e.cfg.BaseImage.Spec.Key()
}

type forkOutcome struct {
	snap   string
	note   string
	forkMs int64
	err    error
}

// workspace is a workspace and what only the worker needs about it.
type workspace struct {
	rec         *pb.WorkspaceRecord
	sync        sync.Mutex    // serializes Sync and ReleaseWorkspace volume access
	forking     bool          // guarded by e.mu: Store.Clone is in flight
	forkDone    chan struct{} // guarded by e.mu: closed when in-flight fork finishes
	pendingFork *forkOutcome  // guarded by e.mu: fork outcome deferred while releasing
	releasing     bool          // guarded by e.mu: ReleaseWorkspace is in progress
	releaseQueued bool          // guarded by e.mu: background release goroutine is queued
	releaseDone   chan struct{} // guarded by e.mu: closed when in-flight ReleaseWorkspace finishes
	releaseErr    error         // guarded by e.mu: error from the last ReleaseWorkspace attempt
}

func (w *workspace) ws() *pb.Workspace { return w.rec.Workspace }

// command is a command and what only the worker needs about it.
type command struct {
	rec *pb.CommandRecord

	cpuNanos   uint64
	cpuAt      time.Time
	followers  int
	lastFollow time.Time
	// profileSeq is the event that carries the command's profile to the
	// controller, once it ended with one (0 if not known).
	profileSeq int64
	// freezeMu serializes freeze/thaw transitions on this command so a losing
	// soft-limit freeze or park cannot thaw a unit that another transition
	// paused or parked. Always acquire freezeMu before e.mu.
	freezeMu sync.Mutex
	// stopping is set while Stop ends the command, which then owns its
	// ending: its unit exiting meanwhile is the stop, not a failure.
	stopping bool
	stopWhy  string
	stopBy   *byHand
	// intervening is set while a hand stop, pause or resume is in flight
	// (including while diagnostics are taken outside e.mu), so another
	// intervention or background scheduler transition does not race it.
	// cancelDiag cancels any in-flight diagnostic capture when hard-limit
	// expiry or an engine stop takes precedence.
	intervening bool
	cancelDiag  context.CancelFunc
	// pendingThaw holds a thaw transition while Units.Thaw is in flight so
	// "resumed"/"thawed" is published only after Units.Thaw succeeds (or if
	// observe sees the unit exit immediately after thawing).
	pendingThaw *pendingThaw
	// admitting is set while admit() is creating tmpDir and starting the unit
	// outside e.mu; admitDone closes once admit finishes (including cleanup if
	// Stop cancelled it mid-admit). admitStopErr records a Units.Stop failure
	// during the stop-vs-admit race so the waiting Stop returns it.
	admitting    bool
	admitDone    chan struct{}
	admitStopErr error
	// deactivating is set when systemd reports the unit loaded and not yet
	// exited, but no longer active (ActiveState=deactivating, e.g. OOMPolicy=stop):
	// freeze and thaw fail on it while systemd finishes tearing it down.
	deactivating bool
	// waiting is the kind of wait a queued command last said it had.
	waiting string
	// preemptedBy is the ID of the strictly higher-priority command that parked
	// this command to make room; while that command is queued or running, this
	// command stays parked (board 7907). Persisted in worker.db across restarts.
	preemptedBy string
	// residentBytes is the most recent anonymous RAM (memory.stat anon) read
	// from the unit's cgroup, and residentKnown reports whether it has been
	// measured. reclaimed reports whether Reclaim succeeded and residentBytes
	// was re-measured for the current parked state, so parked lower-priority
	// commands keep charging their full reservation when reclaim failed or was
	// skipped, and keep charging remaining resident pages on partial reclaim.
	residentBytes int64
	residentKnown bool
	reclaimed     bool
	// tmpGone closes once its temporary directory is removed; nil until it
	// ends in this worker's life.
	tmpGone chan struct{}
	// lastFreshenStep and lastFreshenDone track the last step progress
	// broadcast to WatchWorkspace watchers for a freshen command.
	lastFreshenStep int
	lastFreshenDone bool
	// managedFreshen is non-nil for VM / cladefs-FUSE base builds scheduled
	// through Engine admission control and finalized by EnsureProjectBase.
	managedFreshen *managedFreshen
	out            *output
}

// newCommand makes the engine's command for rec.
func (e *Engine) newCommand(rec *pb.CommandRecord) *command {
	logPath, err := e.logPath(rec.Command.Id)
	if err != nil {
		slog.Error("invalid command ID for log path", "command", rec.Command.Id, "err", err)
	}
	return &command{rec: rec, lastFollow: e.now(), out: newOutput(logPath)}
}

func (c *command) cmd() *pb.Command { return c.rec.Command }
func (c *command) unitName() string { return "hotbed-" + c.cmd().Id }

// runtimeTarget returns the WorkspaceRuntime target ID for c: its temporary
// base VM ID for VM base freshens, or its workspace ID for workspace commands.
func (c *command) runtimeTarget() string {
	if c != nil && c.managedFreshen != nil && c.managedFreshen.vmID != "" {
		return c.managedFreshen.vmID
	}
	return c.cmd().WorkspaceId
}

// NewEngine loads the worker's state from dir.
func NewEngine(ctx context.Context, cfg EngineConfig) (*Engine, error) {
	if cfg.BaseImage == nil {
		return nil, errors.New("worker engine requires BaseImage")
	}
	if cfg.LogLimit == (cmdlog.Limit{}) {
		cfg.LogLimit = DefaultLogLimit
	}
	if err := cfg.LogLimit.Check(); err != nil {
		return nil, err
	}
	cred, home, err := lookupUser(cfg.User)
	if err != nil {
		return nil, err
	}
	// The command user runs git in volumes below the state directory and
	// reads sync bundles from it, so it may pass through (not list) them.
	for _, d := range []string{"", "logs", "bundles", "profiles"} {
		dir := filepath.Join(cfg.Dir, d)
		if err := os.MkdirAll(dir, 0o711); err != nil {
			return nil, err
		}
		if err := os.Chmod(dir, 0o711); err != nil {
			return nil, err
		}
	}
	if err := cmdlog.CheckPunch(filepath.Join(cfg.Dir, "logs")); err != nil {
		return nil, err
	}
	containersConf, err := writeContainersConf(cfg.Dir)
	if err != nil {
		return nil, err
	}
	d, err := openDB(ctx, filepath.Join(cfg.Dir, "worker.db"))
	if err != nil {
		return nil, err
	}
	hostKey, hostPub, err := newShellHostKey()
	if err != nil {
		d.close()
		return nil, err
	}
	bgCtx, bgCancel := context.WithCancel(context.Background())
	e := &Engine{
		cfg: cfg, db: d, cred: cred, home: home,
		containersConf:  containersConf,
		bases:           map[string]*pb.Base{},
		finalizingBases: map[string]bool{},
		workspaces:      map[string]*workspace{},
		commands:        map[string]*command{},
		usage:           &pb.NodeUsage{},
		disk:            &pb.DiskUsage{},
		active:          map[string]bool{},
		vouched:         map[string]time.Time{},
		shellHostKey:    hostKey,
		shellHostPub:    hostPub,
		shellTickets:    map[string]*shellTicket{},
		shells:          map[string]*activeShell{},
		portProxies:     map[string]*portProxy{},
		kick:            make(chan struct{}, 1),
		collect:         make(chan struct{}, 1),
		stepc:           make(chan struct{}, 1),
		updated:         make(chan struct{}),
		bgCtx:           bgCtx,
		bgCancel:        bgCancel,
		streamsDone:     make(chan struct{}),
	}
	if e.resources, err = cfg.System.Resources(); err != nil {
		bgCancel()
		d.close()
		return nil, err
	}
	e.resources.Version = version.String()
	if cfg.VM != nil {
		e.resources.Runtimes = cfg.VM.Runtimes()
		if cfg.Runtime == nil {
			e.cfg.Runtime = cfg.VM.Runtime()
		}
		if cfg.RuntimeFor == nil {
			e.cfg.RuntimeFor = e.isVMWorkspace
		}
	} else if cfg.CladefsFUSE != nil {
		e.resources.Runtimes = cfg.CladefsFUSE.Runtimes()
	} else if cfg.Runtime != nil {
		e.resources.Runtimes = []string{runtime.KindVM, runtime.KindChroot}
	} else {
		e.resources.Runtimes = []string{runtime.KindChroot}
	}
	if cfg.Bench != nil {
		e.resources.BenchCpus = cfg.Bench.CPUs().String()
	}
	if e.cfg.Runtime != nil {
		var uid, gid int
		if cred != nil {
			uid, gid = int(cred.Uid), int(cred.Gid)
		} else {
			sp := cfg.BaseImage.Spec.Normalized()
			uid, gid = sp.UID, sp.GID
		}
		e.rtUnits = newRuntimeUnits(cfg.Units, e.cfg.Runtime, e.usesRuntime, uid, gid)
		e.cfg.Units = e.rtUnits
	}
	if err := e.load(); err != nil {
		bgCancel()
		d.close()
		return nil, fmt.Errorf("load worker state: %w", err)
	}
	if e.rtUnits != nil {
		for _, c := range e.commands {
			if started(c.cmd().State) && e.usesRuntime(c.cmd().WorkspaceId) {
				if lp, lerr := e.logPath(c.cmd().Id); lerr == nil {
					e.rtUnits.RegisterExisting(e.bgCtx, c.unitName(), c.cmd().WorkspaceId, c.cmd().Id, lp)
				}
			}
		}
	}
	e.sweepOrphans(ctx)
	if cfg.GitAuth != nil {
		ga, err := startGitAuthServer(cfg.Dir, cred, cfg.GitAuth)
		if err != nil {
			bgCancel()
			d.close()
			return nil, fmt.Errorf("start git-auth helper: %w", err)
		}
		e.gitAuth = ga
		if e.cfg.VM != nil {
			e.cfg.VM.SetGitAuthHostSock(ga.sockPath)
		}
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
		if r.Workspace.Runtime == runtime.KindVM {
			e.vmWorkspaces.Store(r.Workspace.Id, struct{}{})
		}
	}); err != nil {
		return err
	}
	if err := e.db.all("command", func() proto.Message { return &pb.CommandRecord{} }, func(m proto.Message) {
		r := m.(*pb.CommandRecord)
		// A command that was running when the worker stopped is still a
		// systemd unit; the next tick reads how it is doing.
		e.commands[r.Command.Id] = e.newCommand(r)
	}); err != nil {
		return err
	}
	preemptions, err := e.db.loadPreemptions()
	if err != nil {
		return err
	}
	for id, by := range preemptions {
		if c, ok := e.commands[id]; ok {
			c.preemptedBy = by
		} else if err := e.db.deletePreemption(id); err != nil {
			slog.Warn("clean up orphan preemption record", "command", id, "err", err)
		}
	}
	e.reconcileLoaded()
	return nil
}

// startBgLocked spawns fn as a tracked engine background goroutine unless Close
// has started. Holding e.mu across the closing check and bg.Add(1) ensures
// bg.Add never races with Close's bg.Wait. Call with e.mu held.
func (e *Engine) startBgLocked(fn func(ctx context.Context)) bool {
	if e.closing {
		return false
	}
	e.bg.Add(1)
	ctx := e.bgCtx
	go func() {
		defer e.bg.Done()
		fn(ctx)
	}()
	return true
}

// watchExitBgLocked spawns watchExit as a tracked background goroutine whose
// context cancels when either ctx or the engine's lifecycle context ends.
// Call with e.mu held.
func (e *Engine) watchExitBgLocked(ctx context.Context, slice, name string) {
	e.startBgLocked(func(bgCtx context.Context) {
		watchCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		stop := context.AfterFunc(bgCtx, cancel)
		defer stop()
		e.watchExit(watchCtx, slice, name)
	})
}

// DrainStreams ends long-lived streaming RPCs (FollowLog, WatchWorkspace) so
// HTTP server shutdown does not block waiting on its own streams while
// in-flight unary RPCs drain gracefully. It is safe to call more than once.
func (e *Engine) DrainStreams() {
	e.streamsOnce.Do(func() {
		if e.streamsDone != nil {
			close(e.streamsDone)
		}
	})
}

// Close releases the database and stops active shells, tracked background work,
// and the local git-auth helper socket. Detached commands keep running.
func (e *Engine) Close() error {
	e.DrainStreams()
	e.mu.Lock()
	e.closing = true
	e.bgCancel()
	var active []*activeShell
	for _, sh := range e.shells {
		active = append(active, sh)
	}
	var proxies []*portProxy
	for k, p := range e.portProxies {
		proxies = append(proxies, p)
		delete(e.portProxies, k)
	}
	e.mu.Unlock()
	for _, p := range proxies {
		_ = p.Close()
	}
	for _, sh := range active {
		sh.cancel()
	}
	for _, sh := range active {
		<-sh.done
	}
	e.bg.Wait()
	if e.gitAuth != nil {
		_ = e.gitAuth.Close()
	}
	return e.db.close()
}

// Run drives the node until ctx ends: commands every second, samples every
// report interval, disk usage, old bases and volumes released workspaces
// still hold every two minutes. Bases are
// built only when a workspace asks for one, never here.
func (e *Engine) Run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	sample := time.NewTicker(api.ReportInterval)
	defer sample.Stop()
	usage := time.NewTicker(2 * time.Minute)
	defer usage.Stop()
	e.Sample()
	e.measureDisk(ctx)
	e.sweepTmp()
	e.pruneLogs()
	e.mu.Lock()
	for _, c := range e.commands {
		if started(c.cmd().State) {
			e.watchExitBgLocked(ctx, e.sliceOf(c), c.unitName())
		}
	}
	e.mu.Unlock()
	e.thawStrays(ctx)
	e.settleBench(ctx)
	// Volumes left busy before a restart go now, not two minutes on.
	e.collectSoon()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			e.step(ctx)
		case <-e.stepc:
			e.step(ctx)
		case <-sample.C:
			e.Sample()
		case <-e.collect:
			e.collectWorkspaces(ctx)
			e.collectBases(ctx)
			e.collectBaseImages(ctx)
		case <-usage.C:
			e.collectWorkspaces(ctx)
			e.collectBases(ctx)
			e.collectBaseImages(ctx)
			e.reconcileVMOrphans(ctx, false)
			e.measureDisk(ctx)
			e.pruneEvents()
			e.pruneLogs()
			e.pruneProfiles()
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
func (e *Engine) watchExit(ctx context.Context, slice, name string) {
	if err := e.cfg.Units.Wait(ctx, slice, name); err != nil {
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

// collectSoon asks Run to delete the bases no one needs any more and the
// volumes released workspaces still name.
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

func (e *Engine) now() time.Time {
	if e.cfg.Now != nil {
		return e.cfg.Now()
	}
	return time.Now()
}

func (e *Engine) nowMs() int64 { return e.now().UnixMilli() }

// setProjects records the projects this node keeps bases of, as the reply
// to a heartbeat sent at asOf has them. The bases of a project it no longer
// keeps go stale, so they are deleted once no workspace uses them.
func (e *Engine) setProjects(projects []*pb.HostedProject, asOf time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	next := map[string]*pb.HostedProject{}
	for _, p := range projects {
		next[p.Name] = p
	}
	e.projects = next
	for name, at := range e.vouched {
		if next[name] != nil || at.Before(asOf) {
			delete(e.vouched, name) // the reply is the newer word
		}
	}
	removed := false
	for _, b := range e.bases {
		if e.projectRemoved(b.Project) && (b.State == warm || b.State == broken) {
			b.State = stale
			e.saveBase(b, change{what: "stale", detail: "the node no longer keeps its project"})
			removed = true
		}
	}
	if removed {
		e.collectSoon()
	}
}

// projectRemoved says whether the controller has said this node no longer
// keeps a project's bases: a heartbeat's reply that does not list it, newer
// than any workspace request that carried it. Call with e.mu held.
func (e *Engine) projectRemoved(name string) bool {
	if e.projects == nil || e.projects[name] != nil {
		return false
	}
	_, vouched := e.vouched[name]
	return !vouched
}

func (e *Engine) projectLock(name string) *sync.Mutex {
	m, _ := e.projectLocks.LoadOrStore(name, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// Sample measures the machine's load and memory now, for the next report;
// the engine does it every api.ReportInterval.
func (e *Engine) Sample() {
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
	var objStore *pb.ObjectStoreStats
	if e.cfg.VM != nil {
		objStore = e.cfg.VM.SampleObjectStore(context.Background())
	} else if e.cfg.CladefsFUSE != nil {
		objStore = e.cfg.CladefsFUSE.SampleObjectStore(context.Background())
	}
	e.mu.Lock()
	e.oomKills = oom
	if objStore != nil {
		e.objectStore = objStore
	}
	if mem.Total > 0 {
		e.lastMem = mem
	}
	e.usage = &pb.NodeUsage{
		CpuBusy:         busy,
		MemoryUsedBytes: mem.Used(),
		SwapUsedBytes:   mem.SwapTotal - mem.SwapFree,
		CpuPressure:     cpuP,
		MemoryPressure:  memP,
	}
	now := e.now()
	e.cpu.weigh(now, cpuP)
	wasHeld := e.held.kind != ""
	e.weighHold(now)
	cleared := wasHeld && e.held.kind == ""
	e.mu.Unlock()
	if cleared {
		e.stepSoon()
	}
}

// measureDisk reads the filesystem's space and what each base and
// workspace holds. Usage walks volumes on btrfs, so it runs rarely.
func (e *Engine) measureDisk(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	sp, err := e.cfg.Store.Space(ctx)
	if err != nil {
		if ctx.Err() == nil && !errors.Is(err, context.Canceled) {
			slog.Warn("measure disk", "err", err)
		}
		return
	}
	e.mu.Lock()
	vols := map[string]string{}
	for _, b := range e.bases {
		if isCladefsBase(b) {
			continue
		}
		if b.State == pb.BaseState_BASE_STATE_FAILED {
			b.SizeBytes = 0
			continue
		}
		vols["b:"+b.Id] = baseVolume(b.Project, b.Generation)
	}
	for _, w := range e.workspaces {
		if wsHasVolume(w.ws().State) && w.ws().Runtime != runtime.KindVM && w.ws().Runtime != runtime.KindCladefs {
			vols["w:"+w.ws().Id] = w.rec.Volume
		}
	}
	e.mu.Unlock()
	use := map[string]storage.Usage{}
	for k, v := range vols {
		if ctx.Err() != nil {
			return
		}
		u, err := e.cfg.Store.Usage(ctx, v)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return
			}
			if errors.Is(err, storage.ErrNotFound) && !e.volumeExpected(k) {
				continue
			}
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
		if b.State != pb.BaseState_BASE_STATE_STALE && b.State != pb.BaseState_BASE_STATE_FAILED {
			bases += b.SizeBytes
		}
	}
	for _, w := range e.workspaces {
		ws := w.ws()
		if u, ok := use["w:"+ws.Id]; ok {
			ws.ExclusiveBytes = u.Exclusive
			ws.SharedBytes = max(0, u.Referenced-u.Exclusive)
		}
		if wsHasVolume(ws.State) {
			forks += ws.ExclusiveBytes
		}
	}
	e.diskBytes = sp.Size
	e.disk = &pb.DiskUsage{BasesBytes: bases, ForksExclusiveBytes: forks, OtherBytes: max(0, sp.Size-sp.Free-bases-forks), FreeBytes: sp.Free}
}

func wsHasVolume(st pb.WorkspaceState) bool {
	return st == pb.WorkspaceState_WORKSPACE_STATE_ACTIVE || st == pb.WorkspaceState_WORKSPACE_STATE_IDLE
}

// volumeExpected says whether the base or workspace named by measureDisk key k
// is still in a state whose volume must exist on disk (warm base or live
// active/idle workspace). Volumes of building/stale/failed bases or
// preparing/failed/released workspaces may legitimately be absent.
func (e *Engine) volumeExpected(k string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if id, ok := k[2:], len(k) > 2 && k[:2] == "b:"; ok {
		b, exists := e.bases[id]
		return exists && b.State == pb.BaseState_BASE_STATE_WARM
	}
	if id, ok := k[2:], len(k) > 2 && k[:2] == "w:"; ok {
		w, exists := e.workspaces[id]
		return exists && wsHasVolume(w.ws().State)
	}
	return false
}

func clone[T proto.Message](m T) T { return proto.Clone(m).(T) }
