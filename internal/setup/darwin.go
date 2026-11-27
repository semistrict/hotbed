//go:build darwin

package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/macworker"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/worker"
)

// Setup makes this Mac a worker: it joins the controller as a worker in
// <dir>/worker whose every workspace is a VM of machined's. hotbed's daemon
// (hotbed daemon install) runs the worker and machined.
func Setup(ctx context.Context, o Options, hc *http.Client, check worker.Check) (Result, error) {
	if o.ZFSDevice != "" || o.ZFSDataset != "" || o.ZFSARCMax != 0 {
		return Result{}, api.Errorf(api.CodeInvalidArgs, "a macOS worker keeps its workspaces in machined's cladefs cells, not ZFS: --zfs-* are for Linux workers")
	}
	if o.Slice != "" || len(o.UnitProperties) > 0 {
		return Result{}, api.Errorf(api.CodeInvalidArgs, "--slice and --unit-property are systemd's: a macOS worker runs every command in a VM")
	}
	baseDir, err := ResolveDir(o.Dir)
	if err != nil {
		return Result{}, err
	}
	me, err := user.Current()
	if err != nil {
		return Result{}, err
	}
	if o.User != "" && o.User != me.Username {
		return Result{}, api.Errorf(api.CodeInvalidArgs, "a macOS worker runs as the user who sets it up (%s), its commands in VMs", me.Username).
			WithSuggestion("run hotbed worker init as " + o.User + ", or without --user")
	}
	p := macworker.NewPaths(baseDir)
	progressf(o.Progress, "Joining %s as a worker in %s\n", o.Controller, p.Worker)
	joined, err := worker.Join(ctx, worker.JoinOptions{
		Dir: p.Worker, Controller: o.Controller, User: me.Username, Token: o.Token,
	}, hc, check)
	if err != nil {
		return Result{}, err
	}
	cfg, err := statedir.Load(p.Worker)
	if err != nil {
		return Result{}, err
	}
	cfg.Worker.VM = p.VMConfig()
	cfg.Worker.Socket = p.WorkerSocket
	if err := statedir.Rewrite(p.Worker, cfg); err != nil {
		return Result{}, err
	}
	if o.NoStart {
		if err := p.Stop(); err != nil {
			return Result{}, err
		}
	} else if err := p.Start(); err != nil {
		return Result{}, err
	}
	st := p.ReadState()
	return Result{
		Platform:   "darwin",
		Dir:        baseDir,
		StateDir:   p.Worker,
		Controller: joined.Controller,
		User:       me.Username,
		Node:       joined.Node,
		ClusterID:  joined.ClusterID,
		Storage:    joined.Storage,
		Running:    st.State == "running",
		Service:    DaemonLaunchAgentLabel,
		Mac:        &st,
		Next:       []string{"hotbed daemon install"},
	}, nil
}

// GetStatus is how the Mac's worker and machined stand, as hotbed's daemon
// last said.
func GetStatus(_ context.Context, dir string) (Status, error) {
	baseDir, err := ResolveDir(dir)
	if err != nil {
		return Status{}, err
	}
	p := macworker.NewPaths(baseDir)
	cfg, err := statedir.Load(p.Worker)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && cfg.Worker == nil) {
		return Status{}, api.Errorf(api.CodeNotInitialized, "no worker in %s", p.Worker).
			WithSuggestion("hotbed worker init CONTROLLER")
	}
	if err != nil {
		return Status{}, err
	}
	st := p.ReadState()
	return Status{
		Platform:   "darwin",
		Dir:        baseDir,
		StateDir:   p.Worker,
		Controller: cfg.Worker.Controller,
		User:       cfg.Worker.User,
		Storage:    "cladefs",
		Running:    st.State == "running",
		Service:    DaemonLaunchAgentLabel,
		Mac:        &st,
	}, nil
}

// Start has hotbed's daemon run the worker and machined again, after Stop,
// and waits a minute at most for them to run.
func Start(ctx context.Context, dir string) (Status, error) {
	return setRunning(ctx, dir, true)
}

// Stop has hotbed's daemon stop the worker and machined, its VMs with it,
// and keep them stopped until Start; it waits for them to stop.
func Stop(ctx context.Context, dir string) (Status, error) {
	return setRunning(ctx, dir, false)
}

func setRunning(ctx context.Context, dir string, on bool) (Status, error) {
	if _, err := GetStatus(ctx, dir); err != nil {
		return Status{}, err
	}
	baseDir, err := ResolveDir(dir)
	if err != nil {
		return Status{}, err
	}
	p := macworker.NewPaths(baseDir)
	want := "stopped"
	if on {
		want = "running"
		err = p.Start()
	} else {
		err = p.Stop()
	}
	if err != nil {
		return Status{}, err
	}
	if !daemonLoaded(ctx) {
		return Status{}, api.Errorf(api.CodeConflict, "hotbed's daemon is not running: it is what runs the worker and machined on a Mac").
			WithSuggestion("hotbed daemon install")
	}
	// The daemon's macOS supervisor stops machined in up to 90 s.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		st, err := GetStatus(ctx, dir)
		if err != nil {
			return Status{}, err
		}
		if st.Mac.State == want {
			return st, nil
		}
		if on && st.Mac.State == "error" {
			return st, api.Errorf(api.CodeInternal, "the worker failed to start: %s", st.Mac.Detail)
		}
		if time.Now().After(deadline) {
			return st, api.Errorf(api.CodeNotReady, "the worker is %s after 2 minutes, not %s", st.Mac.State, want)
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Trim has nothing to trim on a Mac: it has no disk images, machined's
// cladefs daemon frees its own space.
func Trim(context.Context, string) (TrimResult, error) {
	return TrimResult{}, api.Errorf(api.CodePlatformNotSupported, "a macOS worker has no sparse disk images to trim: its cells' space is machined's cladefs daemon's to free")
}

// Destroy stops the Mac's worker and machined and removes them: the worker's
// state, machined's binaries, state and cells, and their logs.
func Destroy(ctx context.Context, o DestroyOptions) (DestroyResult, error) {
	baseDir, err := ResolveDir(o.Dir)
	if err != nil {
		return DestroyResult{}, err
	}
	p := macworker.NewPaths(baseDir)
	if !p.Configured() {
		if _, err := os.Stat(p.Machined); errors.Is(err, fs.ErrNotExist) {
			return DestroyResult{}, api.Errorf(api.CodeNotInitialized, "no worker in %s", p.Worker)
		}
	} else if daemonLoaded(ctx) {
		progressf(o.Progress, "Stopping the worker and machined\n")
		if _, err := Stop(ctx, o.Dir); err != nil {
			return DestroyResult{}, fmt.Errorf("stop the worker before removing it: %w", err)
		}
	}
	var removed []string
	for _, path := range []string{p.Worker, p.Machined, p.Stopped, p.Status} {
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		progressf(o.Progress, "Removing %s\n", path)
		if err := os.RemoveAll(path); err != nil {
			return DestroyResult{}, err
		}
		removed = append(removed, path)
	}
	return DestroyResult{Dir: baseDir, Removed: removed}, nil
}

// DisableLegacyWorkerLaunchAgent removes the LaunchAgent that supervised a
// worker VM before hotbed's daemon ran the Mac's worker natively (hotbed
// worker supervise), if it is there.
func DisableLegacyWorkerLaunchAgent(ctx context.Context, dir string) error {
	baseDir, err := ResolveDir(dir)
	if err != nil {
		return err
	}
	label := "com.semistrict.hotbed.worker"
	if def, err := DefaultDir(); err != nil || filepath.Clean(baseDir) != filepath.Clean(def) {
		label += "." + legacyLabelSuffix(baseDir)
	}
	_ = exec.CommandContext(ctx, "/bin/launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+label).Run()
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(home, "Library", "LaunchAgents", label+".plist")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// legacyLabelSuffix told apart the legacy LaunchAgents of workers in other
// base directories than ~/.hotbed.
func legacyLabelSuffix(baseDir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(baseDir)))
	return hex.EncodeToString(sum[:4])
}

// LocalWorker is the worker set up in dir (default ~/.hotbed) that serves
// clients on this Mac on a Unix socket: its node and socket, or zero.
func LocalWorker(dir string) (LocalWorkerSocket, error) {
	baseDir, err := ResolveDir(dir)
	if err != nil {
		return LocalWorkerSocket{}, err
	}
	cfg, err := statedir.Load(macworker.NewPaths(baseDir).Worker)
	if errors.Is(err, fs.ErrNotExist) {
		return LocalWorkerSocket{}, nil
	}
	if err != nil || cfg.Worker == nil {
		return LocalWorkerSocket{}, err
	}
	return LocalWorkerSocket{NodeID: cfg.Worker.NodeID, Socket: cfg.Worker.Socket}, nil
}

// joinedController is the controller the worker in baseDir joined.
func joinedController(baseDir string) (string, error) {
	cfg, err := statedir.Load(macworker.NewPaths(baseDir).Worker)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil || cfg.Worker == nil {
		return "", err
	}
	return cfg.Worker.Controller, nil
}

// daemonLoaded reports whether launchd runs hotbed's daemon.
func daemonLoaded(ctx context.Context) bool {
	return exec.CommandContext(ctx, "/bin/launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())+"/"+DaemonLaunchAgentLabel).Run() == nil
}
