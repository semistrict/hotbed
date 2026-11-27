package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/daemon"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/setup"
	"github.com/semistrict/hotbed/internal/t3bridge"
	"github.com/semistrict/hotbed/internal/update"
)

type daemonCmd struct {
	Run       daemonRunCmd       `cmd:"" default:"withargs" help:"Run the local Hotbed background daemon in the foreground (T3 question notifications, a Mac's worker and machined, and the macOS menubar app)."`
	Install   daemonInstallCmd   `cmd:"" help:"Install and start the background Hotbed daemon (LaunchAgent on macOS, systemd user service on Linux), migrating away from any legacy separate worker/t3-bridge services."`
	Uninstall daemonUninstallCmd `cmd:"" help:"Stop and remove the background Hotbed daemon service."`
	Status    daemonStatusCmd    `cmd:"" help:"Show the local Hotbed daemon service and live subsystem status."`
}

type daemonCommonFlags struct {
	Dir          string        `type:"path" help:"Hotbed state directory (default ~/.hotbed)."`
	Controller   string        `env:"HOTBED_CONTROLLER" help:"Controller URL (default: HOTBED_CONTROLLER or configured controller)."`
	Debounce     time.Duration `name:"debounce" default:"5s" help:"Wait window after an answer arrives so rapid answers coalesce into one T3 wake message."`
	PollInterval time.Duration `name:"poll-interval" default:"3s" help:"Re-check interval while a T3 thread with pending answers is busy."`
	T3Bin        string        `name:"t3-bin" env:"T3_BIN" help:"Path to the local t3 CLI binary (default: t3 on PATH or ~/.t3/bin/t3)."`
	T3BaseDir    string        `name:"t3-base-dir" env:"T3CODE_HOME" help:"Optional T3 Code base directory passed as --base-dir to t3."`
	NoT3Bridge   bool          `name:"no-t3-bridge" help:"Disable T3 Code question notifications."`
	NoWorker     bool          `name:"no-worker" help:"Do not run this Mac's worker and machined."`
	NoMenubar    bool          `name:"no-menubar" help:"Disable the macOS menubar status app."`
}

type daemonRunCmd struct {
	daemonCommonFlags
	ExitOnUpgrade bool `name:"exit-on-upgrade" hidden:"" help:"Exit cleanly when the hotbed binary on disk is replaced so the service manager restarts into the new build."`
}

func (c *daemonRunCmd) Exec(ctx context.Context, d deps) (any, error) {
	if c.Debounce <= 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "--debounce must be positive")
	}
	if c.PollInterval <= 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "--poll-interval must be positive")
	}

	opener := d.openURL
	if opener == nil {
		opener = openDefaultBrowser
	}
	// The binary a Mac's worker runs from: a new build replaces the file.
	exe, err := d.exe()
	if err != nil {
		return nil, fmt.Errorf("locate hotbed binary: %w", err)
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}

	dm, err := daemon.New(daemon.Config{
		BaseDir: c.Dir,
		ControllerFunc: func() (string, error) {
			if addr := strings.TrimSpace(c.Controller); addr != "" {
				return addr, nil
			}
			if d.controller != nil {
				return d.controller()
			}
			return localController(d.getenv)
		},
		TokenFunc: func(tctx context.Context, controller string) (string, error) {
			return resolveControllerAuthToken(tctx, d.getenv, controller, d.http)
		},
		ClusterClient: func(controller string) (hotbedv1connect.ClusterServiceClient, error) {
			ic, err := controllerAuthInterceptors(d.getenv, controller, d.http)
			if err != nil {
				return nil, err
			}
			cluster, _, err := api.Controller(controller, d.stream, ic...)
			return cluster, err
		},
		HTTPClient:      d.stream,
		T3Bin:           strings.TrimSpace(c.T3Bin),
		T3BaseDir:       strings.TrimSpace(c.T3BaseDir),
		Debounce:        c.Debounce,
		PollInterval:    c.PollInterval,
		DisableT3Bridge: c.NoT3Bridge,
		DisableWorker:   c.NoWorker,
		Exe:             exe,
		DisableMenubar:  c.NoMenubar,
		Logger:          slog.Default(),
		OpenURL:         opener,
	})
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if c.ExitOnUpgrade && d.exe != nil {
		if exe, err := d.exe(); err == nil {
			if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
				exe = resolved
			}
			if was, serr := os.Stat(exe); serr == nil {
				wasSum, _ := update.FileSHA256(exe)
				go func() {
					if replacedAt(runCtx, exe, was, wasSum, time.Minute) {
						slog.Info("daemon: a new build replaced this binary; exiting for restart", "binary", exe)
						dm.Restart()
					}
				}()
			}
		}
	}

	slog.Info("starting hotbed daemon", "dir", c.Dir)
	if err := dm.Run(runCtx); err != nil {
		return nil, err
	}
	return map[string]any{"stopped": true}, nil
}

type daemonInstallCmd struct {
	daemonCommonFlags
}

func (c *daemonInstallCmd) Exec(ctx context.Context, d deps) (any, error) {
	if c.Debounce <= 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "--debounce must be positive")
	}
	if c.PollInterval <= 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "--poll-interval must be positive")
	}

	exe, err := d.exe()
	if err != nil {
		return nil, fmt.Errorf("locate hotbed binary: %w", err)
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}

	baseDir, err := setup.ResolveDir(c.Dir)
	if err != nil {
		return nil, err
	}

	args := []string{"daemon", "run", "--exit-on-upgrade"}
	if strings.TrimSpace(c.Dir) != "" {
		args = append(args, "--dir", baseDir)
	}
	if ctrl := strings.TrimSpace(c.Controller); ctrl != "" {
		base, err := api.ControllerBaseURL(ctrl)
		if err != nil {
			return nil, err
		}
		args = append(args, "--controller", base)
	}
	if t3Bin := strings.TrimSpace(c.T3Bin); t3Bin != "" {
		args = append(args, "--t3-bin", t3Bin)
	} else if found, err := t3bridge.ResolveT3Binary(""); err == nil && found != "" {
		args = append(args, "--t3-bin", found)
	}
	if bd := strings.TrimSpace(c.T3BaseDir); bd != "" {
		args = append(args, "--t3-base-dir", bd)
	}
	if c.Debounce != t3bridge.DefaultDebounce {
		args = append(args, "--debounce", c.Debounce.String())
	}
	if c.PollInterval != t3bridge.DefaultPollInterval {
		args = append(args, "--poll-interval", c.PollInterval.String())
	}
	if c.NoT3Bridge {
		args = append(args, "--no-t3-bridge")
	}
	if c.NoWorker {
		args = append(args, "--no-worker")
	}
	if c.NoMenubar {
		args = append(args, "--no-menubar")
	}

	return daemon.InstallService(ctx, daemon.ServiceConfig{
		BaseDir: baseDir,
		Binary:  exe,
		Args:    args,
	})
}

type daemonUninstallCmd struct {
	Dir string `type:"path" help:"Hotbed state directory (default ~/.hotbed)."`
}

func (c *daemonUninstallCmd) Exec(ctx context.Context, _ deps) (any, error) {
	return daemon.UninstallService(ctx, c.Dir)
}

type daemonStatusCmd struct {
	Dir string `type:"path" help:"Hotbed state directory (default ~/.hotbed)."`
}

func (c *daemonStatusCmd) Exec(ctx context.Context, _ deps) (any, error) {
	return daemon.GetServiceStatus(ctx, c.Dir)
}

// replacedAt says, once the file at path has different content than wasSum
// (checking every interval, using stat as a fast path), that a new build
// replaced it; false when ctx ends first. Rewriting path with identical bytes
// (new inode or mtime, same SHA-256) advances was and does not trigger a
// restart.
func replacedAt(ctx context.Context, path string, was os.FileInfo, wasSum string, interval time.Duration) bool {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		}
		now, err := os.Stat(path)
		if err != nil || (os.SameFile(was, now) && now.ModTime().Equal(was.ModTime())) {
			continue
		}
		if wasSum != "" {
			nowSum, err := update.FileSHA256(path)
			if err != nil {
				continue
			}
			if nowSum == wasSum {
				was = now
				continue
			}
		}
		return true
	}
}
