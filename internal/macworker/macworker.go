// Package macworker runs a Mac's hotbed worker: the worker itself (hotbed
// serve), natively, as the Mac's user, and machined, whose machines are its
// VMs, one per workspace, booted from cells of the cladefs daemon machined
// embeds. hotbed's daemon supervises both (Supervisor); hotbed worker init
// makes the worker's state (setup).
package macworker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/semistrict/hotbed/internal/statedir"
)

// Paths are where a Mac's worker keeps what it keeps, under hotbed's base
// directory (~/.hotbed).
type Paths struct {
	Base string
	// Worker is the worker's state directory (hotbed serve's).
	Worker string
	// Machined holds machined: its binaries (bin/<release>/), its API
	// socket, its state (its machines' vsock sockets and its cladefs
	// daemon's disk) and its log.
	Machined string
	// Socket is machined's API socket: Machined, and Cladefsd.
	Socket string
	// WorkerSocket is where the worker serves its API for clients on this
	// Mac (statedir.WorkerConfig.Socket).
	WorkerSocket string
	// State is machined's --state: short, as its vsock sockets go below it.
	State string
	// Images is machined's --images: the worker's artifacts directory, where
	// the controller's kernels download to, and where the worker makes the
	// initramfses hotbed-guestd boots from.
	Images string
	// Stopped, when there, says the worker is to stay stopped (hotbed
	// worker stop), across the daemon's restarts.
	Stopped string
	// Status is the Supervisor's account of the worker, for hotbed worker
	// status.
	Status string
	// Logs holds machined's and the worker's logs.
	Logs string
}

// NewPaths are the paths of the worker under base.
func NewPaths(base string) Paths {
	m := filepath.Join(base, "machined")
	w := filepath.Join(base, "worker")
	return Paths{
		Base:         base,
		Worker:       w,
		Machined:     m,
		Socket:       filepath.Join(m, "machined.sock"),
		WorkerSocket: filepath.Join(w, "worker.sock"),
		State:        filepath.Join(m, "state"),
		Images:       filepath.Join(w, "artifacts"),
		Stopped:      filepath.Join(base, "worker-stopped"),
		Status:       filepath.Join(base, "worker-status.json"),
		Logs:         filepath.Join(base, "logs"),
	}
}

// Configured reports whether p holds a worker's state: hotbed worker init
// made it.
func (p Paths) Configured() bool {
	cfg, err := statedir.Load(p.Worker)
	return err == nil && cfg.Worker != nil
}

// VMConfig is the worker's VM config: every workspace a VM of machined's.
func (p Paths) VMConfig() *statedir.VMConfig {
	return &statedir.VMConfig{Enabled: true, Default: true, MachinedSock: p.Socket, MachinedImages: p.Images}
}

// ensureConfig gives the worker's config what a Mac's worker runs with
// (VMConfig's machined, its Unix socket), as hotbed worker init writes it: a
// worker joined by an older build gets them as the daemon starts it.
func (p Paths) ensureConfig() error {
	cfg, err := statedir.Load(p.Worker)
	if err != nil {
		return err
	}
	w := cfg.Worker
	if w == nil {
		return fmt.Errorf("%s holds no worker", p.Worker)
	}
	want := p.VMConfig()
	changed := w.Socket != p.WorkerSocket
	w.Socket = p.WorkerSocket
	if w.VM == nil {
		w.VM, changed = want, true
	} else if w.VM.MachinedSock != want.MachinedSock || w.VM.MachinedImages != want.MachinedImages || !w.VM.Enabled {
		w.VM.Enabled, w.VM.MachinedSock, w.VM.MachinedImages, changed = true, want.MachinedSock, want.MachinedImages, true
	}
	if !changed {
		return nil
	}
	return statedir.Rewrite(p.Worker, cfg)
}

// State is how the worker stands, as the daemon's menubar and hotbed worker
// status show it.
type State struct {
	Configured bool `json:"configured"`
	// Autostart: the daemon starts it whenever it runs, unless it was
	// stopped (hotbed worker stop).
	Autostart bool `json:"autostart"`
	// State is "unconfigured", "stopped", "starting", "running", "stopping"
	// or "error".
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
	// MachinedPID and WorkerPID are their processes', while they run.
	MachinedPID int `json:"machinedPid,omitempty"`
	WorkerPID   int `json:"workerPid,omitempty"`
	// Release is the machined release it runs.
	Release   string    `json:"release,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Stop has the daemon stop the worker and machined, and keep them stopped
// until Start.
func (p Paths) Stop() error {
	if err := os.MkdirAll(filepath.Dir(p.Stopped), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p.Stopped, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}

// Start has the daemon start the worker again, after Stop.
func (p Paths) Start() error {
	if err := os.Remove(p.Stopped); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// stopped reports whether Stop holds the worker stopped.
func (p Paths) stopped() bool {
	_, err := os.Stat(p.Stopped)
	return err == nil
}

// ReadState is the Supervisor's last account of the worker, or an
// unconfigured or stopped one if there is none: no daemon has run it.
func (p Paths) ReadState() State {
	var st State
	b, err := os.ReadFile(p.Status)
	if err == nil && json.Unmarshal(b, &st) == nil {
		return st
	}
	if !p.Configured() {
		return State{State: "unconfigured"}
	}
	return State{Configured: true, Autostart: !p.stopped(), State: "stopped", Detail: "hotbed's daemon is not running it (hotbed daemon install)"}
}

func (p Paths) writeState(st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.Status + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p.Status); err != nil {
		return fmt.Errorf("write %s: %w", p.Status, err)
	}
	return nil
}
