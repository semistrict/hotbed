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
	"github.com/semistrict/hotbed/internal/t3bridge"
)

type t3BridgeCmd struct {
	Run       t3BridgeRunCmd       `cmd:"" default:"withargs" help:"Run the outbound T3 Code question notification bridge in the foreground."`
	Install   t3BridgeInstallCmd   `cmd:"" help:"Install and start the t3-bridge background service (LaunchAgent on macOS, systemd user service on Linux)."`
	Uninstall t3BridgeUninstallCmd `cmd:"" help:"Stop and remove the t3-bridge background service."`
	Status    t3BridgeStatusCmd    `cmd:"" help:"Show the local t3-bridge service and state status."`
}

type t3BridgeCommonFlags struct {
	Controller   string        `env:"HOTBED_CONTROLLER" help:"Controller URL (default: HOTBED_CONTROLLER or configured controller)."`
	Debounce     time.Duration `name:"debounce" default:"5s" help:"Wait window after an answer arrives so rapid answers coalesce into one wake message."`
	PollInterval time.Duration `name:"poll-interval" default:"3s" help:"Re-check interval while a thread with pending answers is busy."`
	StateFile    string        `name:"state-file" type:"path" help:"Path to persist notified question revisions (default ~/.hotbed/t3-bridge-state.json)."`
	T3Bin        string        `name:"t3-bin" env:"T3_BIN" help:"Path to the local t3 CLI binary (default: t3 on PATH or ~/.t3/bin/t3)."`
	T3BaseDir    string        `name:"t3-base-dir" env:"T3CODE_HOME" help:"Optional T3 Code base directory passed as --base-dir to t3."`
}

type t3BridgeRunCmd struct {
	t3BridgeCommonFlags
	ExitOnUpgrade bool `name:"exit-on-upgrade" hidden:"" help:"Exit cleanly when the hotbed binary on disk is replaced so a service manager restarts into the new build."`
}

func (c *t3BridgeRunCmd) Exec(ctx context.Context, d deps) (any, error) {
	if c.Debounce <= 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "--debounce must be positive")
	}
	if c.PollInterval <= 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "--poll-interval must be positive")
	}

	ctrlAddr, err := resolveBridgeController(c.Controller, d)
	if err != nil {
		return nil, err
	}
	ctrlBase, err := api.ControllerBaseURL(ctrlAddr)
	if err != nil {
		return nil, err
	}

	t3Bin, err := t3bridge.ResolveT3Binary(c.T3Bin)
	if err != nil {
		return nil, api.Errorf(api.CodeNotFound, "%v", err)
	}

	statePath := strings.TrimSpace(c.StateFile)
	if statePath == "" {
		statePath, err = t3bridge.DefaultStateFile()
		if err != nil {
			return nil, err
		}
	}

	bridge, err := t3bridge.New(t3bridge.Config{
		ControllerURL: ctrlBase,
		TokenFunc: func(tctx context.Context) (string, error) {
			return resolveControllerAuthToken(tctx, ctrlBase, d.http)
		},
		HTTPClient:   d.stream,
		T3:           &t3bridge.CLIClient{Binary: t3Bin, BaseDir: strings.TrimSpace(c.T3BaseDir)},
		State:        &t3bridge.StateStore{Path: statePath},
		Debounce:     c.Debounce,
		PollInterval: c.PollInterval,
		Logger:       slog.Default(),
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
				go func() {
					if replacedAt(runCtx, exe, was, time.Minute) {
						slog.Info("t3-bridge: a new build replaced this binary; exiting for restart", "binary", exe)
						cancel()
					}
				}()
			}
		}
	}

	slog.Info("starting hotbed t3-bridge",
		"controller", ctrlBase,
		"t3Bin", t3Bin,
		"stateFile", statePath,
		"debounce", c.Debounce,
		"pollInterval", c.PollInterval,
	)
	if err := bridge.Run(runCtx); err != nil {
		return nil, err
	}
	return map[string]any{"stopped": true}, nil
}

type t3BridgeInstallCmd struct {
	t3BridgeCommonFlags
}

func (c *t3BridgeInstallCmd) Exec(ctx context.Context, d deps) (any, error) {
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

	t3Bin, err := t3bridge.ResolveT3Binary(c.T3Bin)
	if err != nil {
		return nil, api.Errorf(api.CodeNotFound, "%v", err)
	}
	ctrlAddr, err := resolveBridgeController(c.Controller, d)
	if err != nil {
		return nil, err
	}
	ctrlBase, err := api.ControllerBaseURL(ctrlAddr)
	if err != nil {
		return nil, err
	}

	args := []string{"t3-bridge", "run", "--exit-on-upgrade", "--t3-bin", t3Bin}
	if strings.TrimSpace(c.Controller) != "" {
		args = append(args, "--controller", ctrlBase)
	}
	if c.Debounce != t3bridge.DefaultDebounce {
		args = append(args, "--debounce", c.Debounce.String())
	}
	if c.PollInterval != t3bridge.DefaultPollInterval {
		args = append(args, "--poll-interval", c.PollInterval.String())
	}
	if sf := strings.TrimSpace(c.StateFile); sf != "" {
		args = append(args, "--state-file", sf)
	}
	if bd := strings.TrimSpace(c.T3BaseDir); bd != "" {
		args = append(args, "--t3-base-dir", bd)
	}

	status, err := t3bridge.InstallService(ctx, t3bridge.ServiceConfig{
		Binary: exe,
		Args:   args,
	})
	if err != nil {
		return nil, err
	}
	enrichBridgeStatus(&status, ctrlBase, t3Bin, c.StateFile)
	return status, nil
}

type t3BridgeUninstallCmd struct{}

func (c *t3BridgeUninstallCmd) Exec(ctx context.Context, _ deps) (any, error) {
	return t3bridge.UninstallService(ctx)
}

type t3BridgeStatusCmd struct {
	Controller string `env:"HOTBED_CONTROLLER" help:"Controller URL (default: configured controller)."`
	StateFile  string `name:"state-file" type:"path" help:"Path to state file (default ~/.hotbed/t3-bridge-state.json)."`
	T3Bin      string `name:"t3-bin" env:"T3_BIN" help:"Path to the local t3 CLI binary."`
}

func (c *t3BridgeStatusCmd) Exec(ctx context.Context, d deps) (any, error) {
	status, err := t3bridge.GetServiceStatus(ctx)
	if err != nil {
		return nil, err
	}
	ctrlAddr, _ := resolveBridgeController(c.Controller, d)
	if ctrlAddr != "" {
		if base, err := api.ControllerBaseURL(ctrlAddr); err == nil {
			ctrlAddr = base
		}
	}
	t3Bin, _ := t3bridge.ResolveT3Binary(c.T3Bin)
	enrichBridgeStatus(&status, ctrlAddr, t3Bin, c.StateFile)
	return status, nil
}

func resolveBridgeController(explicit string, d deps) (string, error) {
	if addr := strings.TrimSpace(explicit); addr != "" {
		return addr, nil
	}
	if d.controller != nil {
		return d.controller()
	}
	return localController()
}

func enrichBridgeStatus(status *t3bridge.ServiceStatus, controller, t3Bin, stateFile string) {
	status.Controller = controller
	status.T3Binary = t3Bin
	sf := strings.TrimSpace(stateFile)
	if sf == "" {
		sf, _ = t3bridge.DefaultStateFile()
	}
	status.StateFile = sf
	if sf != "" {
		if loaded, err := (&t3bridge.StateStore{Path: sf}).Load(); err == nil {
			status.TrackedIDs = len(loaded)
		}
	}
}
