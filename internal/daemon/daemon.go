package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/macworker"
	"github.com/semistrict/hotbed/internal/setup"
	"github.com/semistrict/hotbed/internal/t3bridge"
	"github.com/semistrict/hotbed/internal/version"
)

// Config configures the unified local Hotbed host daemon.
type Config struct {
	BaseDir         string
	ControllerFunc  func() (string, error)
	TokenFunc       func(ctx context.Context, controller string) (string, error)
	ClusterClient   func(controller string) (hotbedv1connect.ClusterServiceClient, error)
	HTTPClient      *http.Client
	LogPath         string
	StateFile       string
	T3Bin           string
	T3BaseDir       string
	Debounce        time.Duration
	PollInterval    time.Duration
	DisableT3Bridge bool
	// DisableWorker leaves a Mac's worker and machined to run elsewhere.
	DisableWorker  bool
	DisableMenubar bool
	Logger         *slog.Logger
	OpenURL        func(context.Context, string) error
	// Exe is the hotbed binary a Mac's worker runs from (hotbed serve).
	Exe string
	// Release is the machined a Mac's worker runs on; macworker.Pinned if
	// zero.
	Release macworker.Release
}

// Daemon runs all local host background services under one process:
//   - outbound T3 Code question notification bridge
//   - a Mac's worker and machined, which runs its VMs (macworker.Supervisor)
//   - native macOS menubar status app
type Daemon struct {
	cfg        Config
	baseDir    string
	statusPath string
	log        *slog.Logger

	mu      sync.Mutex
	snap    Snapshot
	subChs  map[chan Snapshot]struct{}
	restart context.CancelFunc
}

// New constructs a Daemon.
func New(cfg Config) (*Daemon, error) {
	baseDir, err := setup.ResolveDir(cfg.BaseDir)
	if err != nil {
		return nil, err
	}
	statusPath, err := StatusFilePath(baseDir)
	if err != nil {
		return nil, err
	}
	if cfg.LogPath == "" {
		cfg.LogPath, _ = DefaultLogPath(baseDir)
	}
	if cfg.StateFile == "" {
		cfg.StateFile = filepath.Join(baseDir, "t3-bridge-state.json")
	}
	if cfg.Debounce <= 0 {
		cfg.Debounce = t3bridge.DefaultDebounce
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = t3bridge.DefaultPollInterval
	}
	if cfg.Release == (macworker.Release{}) {
		cfg.Release = macworker.Pinned
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	t3State := "reconnecting"
	if cfg.DisableT3Bridge {
		t3State = "disabled"
	}

	snap := Snapshot{
		Version:   version.String(),
		Build:     version.Number(),
		PID:       os.Getpid(),
		UpdatedAt: time.Now().UTC(),
		LogPath:   cfg.LogPath,
		T3Bridge: T3BridgeSummary{
			Enabled: !cfg.DisableT3Bridge,
			State:   t3State,
		},
		Worker: macworker.NewPaths(baseDir).ReadState(),
	}

	return &Daemon{
		cfg:        cfg,
		baseDir:    baseDir,
		statusPath: statusPath,
		log:        log,
		snap:       snap,
		subChs:     map[chan Snapshot]struct{}{},
	}, nil
}

func (d *Daemon) subscribe() (chan Snapshot, Snapshot, func()) {
	ch := make(chan Snapshot, 8)
	d.mu.Lock()
	d.subChs[ch] = struct{}{}
	cur := d.snap
	d.mu.Unlock()
	return ch, cur, func() {
		d.mu.Lock()
		delete(d.subChs, ch)
		d.mu.Unlock()
	}
}

func (d *Daemon) update(fn func(*Snapshot)) {
	d.mu.Lock()
	fn(&d.snap)
	d.snap.UpdatedAt = time.Now().UTC()
	cur := d.snap
	subs := make([]chan Snapshot, 0, len(d.subChs))
	for ch := range d.subChs {
		subs = append(subs, ch)
	}
	d.mu.Unlock()

	if err := SaveSnapshot(d.statusPath, cur); err != nil {
		d.log.Debug("daemon: save status snapshot failed", "err", err)
	}
	for _, ch := range subs {
		select {
		case ch <- cur:
		default:
			// Drop stale frame and replace with latest snapshot.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- cur:
			default:
			}
		}
	}
}

// Snapshot returns a copy of the current daemon snapshot.
func (d *Daemon) Snapshot() Snapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.snap
}

// Run runs the unified daemon until ctx is cancelled (or Restart is requested
// from the menubar).
func (d *Daemon) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	d.mu.Lock()
	d.restart = cancel
	d.mu.Unlock()

	// Publish initial status immediately.
	if d.cfg.ControllerFunc != nil {
		if addr, err := d.cfg.ControllerFunc(); err == nil && strings.TrimSpace(addr) != "" {
			if base, berr := api.ControllerBaseURL(addr); berr == nil {
				d.update(func(s *Snapshot) {
					s.Controller = base
					s.CockpitURL = base
				})
			}
		}
	}

	var wg sync.WaitGroup

	// 1. T3 Code question notification bridge.
	if !d.cfg.DisableT3Bridge {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.runT3BridgeLoop(runCtx)
		}()
	}

	// 2. A Mac's worker and machined.
	if !d.cfg.DisableWorker && runtime.GOOS == "darwin" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sup := &macworker.Supervisor{
				Paths: macworker.NewPaths(d.baseDir), Release: d.cfg.Release, Exe: d.cfg.Exe,
				HTTP: d.cfg.HTTPClient, Log: d.log,
				OnState: func(st macworker.State) {
					d.update(func(s *Snapshot) { s.Worker = st })
				},
			}
			sup.Run(runCtx)
		}()
	}

	// 3. Native macOS menubar app (no-op on non-darwin or when disabled).
	if !d.cfg.DisableMenubar && runtime.GOOS == "darwin" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.runMenubar(runCtx)
		}()
	}

	<-runCtx.Done()
	wg.Wait()
	return nil
}

func (d *Daemon) runT3BridgeLoop(ctx context.Context) {
	for ctx.Err() == nil {
		ctrlAddr := ""
		if d.cfg.ControllerFunc != nil {
			if addr, err := d.cfg.ControllerFunc(); err == nil {
				ctrlAddr = strings.TrimSpace(addr)
			}
		}
		if ctrlAddr == "" {
			d.update(func(s *Snapshot) {
				s.T3Bridge.Available = false
				s.T3Bridge.State = "unavailable"
				s.T3Bridge.LastError = "no controller configured (run `hotbed login`)"
			})
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
				continue
			}
		}
		ctrlBase, err := api.ControllerBaseURL(ctrlAddr)
		if err != nil {
			d.update(func(s *Snapshot) {
				s.T3Bridge.Available = false
				s.T3Bridge.State = "unavailable"
				s.T3Bridge.LastError = err.Error()
			})
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
				continue
			}
		}
		d.update(func(s *Snapshot) {
			s.Controller = ctrlBase
			s.CockpitURL = ctrlBase
		})

		t3Bin, err := t3bridge.ResolveT3Binary(d.cfg.T3Bin)
		if err != nil {
			d.update(func(s *Snapshot) {
				s.T3Bridge.Available = false
				s.T3Bridge.State = "unavailable"
				s.T3Bridge.Binary = ""
				s.T3Bridge.LastError = "t3 CLI not installed"
			})
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
				continue
			}
		}

		d.update(func(s *Snapshot) {
			s.T3Bridge.Available = true
			s.T3Bridge.Binary = t3Bin
			s.T3Bridge.LastError = ""
		})

		bridge, err := t3bridge.New(t3bridge.Config{
			ControllerURL: ctrlBase,
			TokenFunc: func(tctx context.Context) (string, error) {
				if d.cfg.TokenFunc == nil {
					return "", nil
				}
				return d.cfg.TokenFunc(tctx, ctrlBase)
			},
			HTTPClient:   d.cfg.HTTPClient,
			T3:           &t3bridge.CLIClient{Binary: t3Bin, BaseDir: strings.TrimSpace(d.cfg.T3BaseDir)},
			State:        &t3bridge.StateStore{Path: d.cfg.StateFile},
			Debounce:     d.cfg.Debounce,
			PollInterval: d.cfg.PollInterval,
			Logger:       d.log,
			OnStatus: func(st t3bridge.Status) {
				d.update(func(s *Snapshot) {
					s.T3Bridge.Available = true
					s.T3Bridge.Binary = t3Bin
					if st.Connected {
						s.T3Bridge.State = "connected"
					} else {
						s.T3Bridge.State = "reconnecting"
					}
					s.T3Bridge.PendingQuestions = st.PendingQuestions
					s.T3Bridge.TrackedQuestions = st.TrackedQuestions
					s.T3Bridge.LastWakeAt = st.LastWakeAt
					s.T3Bridge.LastWakeThread = st.LastWakeThread
					s.T3Bridge.LastWakeIDs = st.LastWakeIDs
					s.T3Bridge.LastError = st.LastError
				})
			},
		})
		if err != nil {
			d.log.Warn("daemon: initialize t3-bridge failed; retrying", "err", err)
			d.update(func(s *Snapshot) {
				s.T3Bridge.State = "unavailable"
				s.T3Bridge.LastError = err.Error()
			})
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
				continue
			}
		}

		if err := bridge.Run(ctx); err != nil && ctx.Err() == nil {
			d.log.Warn("daemon: t3-bridge exited; restarting", "err", err)
			d.update(func(s *Snapshot) {
				s.T3Bridge.State = "reconnecting"
				s.T3Bridge.LastError = err.Error()
			})
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
}

func (d *Daemon) handleMenubarAction(ctx context.Context, action string) {
	switch action {
	case "open_cockpit":
		snap := d.Snapshot()
		if snap.CockpitURL != "" && d.cfg.OpenURL != nil {
			if err := d.cfg.OpenURL(ctx, snap.CockpitURL); err != nil {
				d.log.Warn("daemon: open cockpit failed", "err", err)
			}
		}
	case "open_logs":
		if d.cfg.LogPath != "" && d.cfg.OpenURL != nil {
			if err := d.cfg.OpenURL(ctx, d.cfg.LogPath); err != nil {
				d.log.Warn("daemon: open logs failed", "err", err)
			}
		}
	case "start_worker", "stop_worker":
		p := macworker.NewPaths(d.baseDir)
		set := p.Start
		if action == "stop_worker" {
			set = p.Stop
		}
		// The Supervisor sees it within its poll and says how it goes.
		if err := set(); err != nil {
			d.log.Warn("daemon: menubar "+action+" failed", "err", err)
			d.update(func(s *Snapshot) {
				s.Worker.State = "error"
				s.Worker.Detail = err.Error()
			})
		}
	case "restart":
		d.log.Info("daemon: restart requested from menubar")
		d.mu.Lock()
		cancel := d.restart
		d.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
}
