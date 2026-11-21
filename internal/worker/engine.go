package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/semistrict/hotbed/internal/api"
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

// Units starts and controls commands; *systemd is the real one.
type Units interface {
	Start(ctx context.Context, s unit.Spec) error
	Status(ctx context.Context, name string) (unit.State, error)
	Freeze(ctx context.Context, name string) error
	Thaw(ctx context.Context, name string) error
	Reclaim(cgroup string, n int64) error
	Swap(cgroup string) (int64, error)
	Stop(ctx context.Context, name string) error
}

// System measures the node; *linuxSystem is the real one.
type System interface {
	Resources() (api.Resources, error)
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
	// UnitProperties are systemd properties every command gets, to grant
	// what builds need beyond a plain user: AmbientCapabilities=,
	// DeviceAllow=, LimitMEMLOCK= and so on.
	UnitProperties []string
	Store          storage.Store
	Units          Units
	System         System
	Now            func() time.Time
}

// Engine is a worker's bases, workspaces and commands.
type Engine struct {
	cfg  EngineConfig
	db   *db
	cred *syscall.Credential
	home string

	mu         sync.Mutex
	repos      map[string]api.Repo
	bases      map[string]*api.Base
	workspaces map[string]*workspace
	commands   map[string]*command
	usage      api.NodeUsage
	disk       api.DiskUsage
	oomKills   int64
	diskBytes  int64
	resources  api.Resources
	heads      map[string]remoteHead

	kick  chan struct{} // something changed: report soon
	repoc chan struct{} // the hosted repos changed: look at bases now
}

type remoteHead struct {
	commit string
	at     time.Time
}

// workspace is a workspace and what only the worker needs about it.
type workspace struct {
	api.Workspace
	Volume   string `json:"volume"`
	BaseSnap string `json:"baseSnap"`
	sync     sync.Mutex
}

// command is a command and what only the worker needs about it.
type command struct {
	api.Command
	RepoID   string    `json:"repoID"`
	Agent    string    `json:"agent"`
	Volume   string    `json:"volume"`
	Dir      string    `json:"dir"`
	Env      []string  `json:"env"`
	BaseID   string    `json:"baseID,omitempty"` // set for a freshen
	Parks    int       `json:"parks"`
	ParkedAt time.Time `json:"parkedAt"`
	Peak     int64     `json:"peak"`
	Cgroup   string    `json:"cgroup"`

	cpuNanos   uint64
	cpuAt      time.Time
	followers  int
	lastFollow time.Time
}

func (c *command) unitName() string { return "hotbed-" + c.ID }

// NewEngine loads the worker's state from dir.
func NewEngine(ctx context.Context, cfg EngineConfig) (*Engine, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cred, home, err := lookupUser(cfg.User)
	if err != nil {
		return nil, err
	}
	for _, d := range []string{"logs", "bundles"} {
		if err := os.MkdirAll(filepath.Join(cfg.Dir, d), 0o755); err != nil {
			return nil, err
		}
	}
	d, err := openDB(ctx, filepath.Join(cfg.Dir, "worker.db"))
	if err != nil {
		return nil, err
	}
	e := &Engine{
		cfg: cfg, db: d, cred: cred, home: home,
		repos: map[string]api.Repo{}, bases: map[string]*api.Base{},
		workspaces: map[string]*workspace{}, commands: map[string]*command{},
		heads: map[string]remoteHead{}, kick: make(chan struct{}, 1), repoc: make(chan struct{}, 1),
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
	if err := e.db.all("base", func(doc []byte) error {
		var b api.Base
		if err := json.Unmarshal(doc, &b); err != nil {
			return err
		}
		e.bases[b.ID] = &b
		return nil
	}); err != nil {
		return err
	}
	if err := e.db.all("workspace", func(doc []byte) error {
		w := &workspace{}
		if err := json.Unmarshal(doc, w); err != nil {
			return err
		}
		e.workspaces[w.ID] = w
		return nil
	}); err != nil {
		return err
	}
	return e.db.all("command", func(doc []byte) error {
		c := &command{}
		if err := json.Unmarshal(doc, c); err != nil {
			return err
		}
		// A command that was running when the worker stopped is still a
		// systemd unit; the next tick reads how it is doing.
		c.lastFollow = e.cfg.Now()
		e.commands[c.ID] = c
		return nil
	})
}

// Close releases the database. Commands keep running.
func (e *Engine) Close() error { return e.db.close() }

// Run drives the node until ctx ends: commands every second, samples every
// report interval, bases and disk usage less often.
func (e *Engine) Run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	sample := time.NewTicker(api.ReportInterval)
	defer sample.Stop()
	bases := time.NewTicker(30 * time.Second)
	defer bases.Stop()
	usage := time.NewTicker(2 * time.Minute)
	defer usage.Stop()
	e.sample()
	e.measureDisk(ctx)
	e.maintainBases(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			e.step(ctx)
		case <-sample.C:
			e.sample()
		case <-bases.C:
			e.maintainBases(ctx)
		case <-e.repoc:
			e.maintainBases(ctx)
		case <-usage.C:
			e.measureDisk(ctx)
		}
	}
}

// Kicked is signalled whenever something worth reporting changed.
func (e *Engine) Kicked() <-chan struct{} { return e.kick }

func (e *Engine) changed() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

func (e *Engine) now() time.Time { return e.cfg.Now() }

func (e *Engine) stamp() string { return e.now().UTC().Format(time.RFC3339Nano) }

// event queues an event for the controller. Call with e.mu held.
func (e *Engine) event(kind, text string, ref *api.EventRef) {
	ev := api.WorkerEvent{At: e.now(), Kind: kind, Text: text, Ref: ref}
	if _, err := e.db.enqueue("event", ev); err != nil {
		slog.Error("queue event", "err", err)
	}
	e.changed()
}

func (e *Engine) saveBase(b *api.Base) {
	if err := e.db.put("base", b.ID, b); err != nil {
		slog.Error("save base", "base", b.ID, "err", err)
	}
}

func (e *Engine) saveWorkspace(w *workspace) {
	if err := e.db.put("workspace", w.ID, w); err != nil {
		slog.Error("save workspace", "workspace", w.ID, "err", err)
	}
}

func (e *Engine) saveCommand(c *command) {
	if err := e.db.put("command", c.ID, c); err != nil {
		slog.Error("save command", "command", c.ID, "err", err)
	}
}

// SetRepos is what the controller says this node hosts.
func (e *Engine) SetRepos(repos []api.Repo) {
	e.mu.Lock()
	defer e.mu.Unlock()
	next := map[string]api.Repo{}
	changed := len(repos) != len(e.repos)
	for _, r := range repos {
		next[r.ID] = r
		if old, ok := e.repos[r.ID]; !ok || old.URL != r.URL || old.Branch != r.Branch || !slices.Equal(old.Freshen, r.Freshen) {
			changed = true
		}
	}
	e.repos = next
	if changed {
		select {
		case e.repoc <- struct{}{}:
		default:
		}
	}
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
	e.usage = api.NodeUsage{
		CPUBusy:         busy,
		MemoryUsedBytes: mem.Used(),
		SwapUsedBytes:   mem.SwapTotal - mem.SwapFree,
		CPUPressure:     cpuP,
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
		vols["b:"+b.ID] = baseVolume(b.RepoID, b.Generation)
	}
	for _, w := range e.workspaces {
		if w.State != api.WorkspaceReleased {
			vols["w:"+w.ID] = w.Volume
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
		if u, ok := use["b:"+b.ID]; ok {
			b.SizeBytes = u.Referenced
		}
		if b.State != api.BaseStale {
			bases += b.SizeBytes
		}
	}
	for _, w := range e.workspaces {
		if u, ok := use["w:"+w.ID]; ok {
			w.ExclusiveBytes = u.Exclusive
			w.SharedBytes = max(0, u.Referenced-u.Exclusive)
		}
		if w.State != api.WorkspaceReleased {
			forks += w.ExclusiveBytes
		}
	}
	other := max(0, sp.Size-sp.Free-bases-forks)
	e.diskBytes = sp.Size
	e.disk = api.DiskUsage{BasesBytes: bases, ForksExclusiveBytes: forks, OtherBytes: other, FreeBytes: sp.Free}
}
