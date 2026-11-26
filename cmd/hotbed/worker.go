package main

import (
	"cmp"
	"context"
	"fmt"
	"runtime"
	"strings"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/setup"
	"github.com/semistrict/hotbed/internal/t3bridge"
)

type workerCmd struct {
	Init    workerInitCmd    `cmd:"" help:"Initialize and start a worker node on this machine in ~/.hotbed: directly on Linux; on macOS natively too, every command in a VM of machined's, which hotbed's daemon runs beside the worker."`
	Status  workerStatusCmd  `cmd:"" help:"Show the local worker's state and sparse disk usage."`
	Start   workerStartCmd   `cmd:"" help:"Start the local worker (its systemd service on Linux; on macOS, the worker and machined, through hotbed's daemon)."`
	Stop    workerStopCmd    `cmd:"" help:"Gracefully stop the local worker (on macOS, machined and its VMs too, until worker start)."`
	Trim    workerTrimCmd    `cmd:"" help:"Punch freed blocks out of sparse worker storage (ZFS autotrim; Linux)."`
	Destroy workerDestroyCmd `cmd:"" help:"Stop and remove the local worker setup from ~/.hotbed."`
}

type workerInitCmd struct {
	Controller     string   `arg:"" optional:"" env:"HOTBED_CONTROLLER" help:"The controller's HTTPS URL or tailnet name/address, with :PORT unless 7120 (or set HOTBED_CONTROLLER)."`
	Token          string   `env:"HOTBED_TOKEN" help:"Bearer or org join token (hbj_...) for authenticating to a remote HTTPS controller."`
	Dir            string   `type:"path" help:"Base directory for worker state (default ~/.hotbed)."`
	User           string   `help:"Unix user commands run as (default: invoking user)."`
	DiskSize       string   `name:"disk-size" default:"256G" help:"Linux: virtual size of the sparse ZFS disk image when creating one (e.g. 256G, 512G)."`
	ZFSDataset     string   `name:"zfs-dataset" help:"Linux: ZFS dataset for volumes when using native ZFS."`
	ZFSDevice      string   `name:"zfs-device" help:"Linux: block device path under /dev/ for a dedicated ZFS pool (e.g. /dev/disk/by-id/virtio-hotbed-zfs)."`
	ZFSARCMax      string   `name:"zfs-arc-max" help:"Linux: cap the ZFS ARC of the pool hotbed makes, e.g. 4G (default: zfs's own, nearly all of RAM)."`
	UnitProperties []string `name:"unit-property" help:"Linux: systemd property every command gets (repeatable)."`
	Slice          string   `help:"Linux: systemd slice commands run in (default hotbed.slice)."`
	Hostname       string   `help:"Linux: the worker's node name when the tailnet reports none."`
	NoStart        bool     `name:"no-start" help:"Set up the worker without leaving it running."`
}

func (c *workerInitCmd) Exec(ctx context.Context, d deps) (any, error) {
	if strings.TrimSpace(c.Controller) == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "controller address is required").
			WithSuggestion("pass the controller as an argument (`hotbed worker init CONTROLLER`) or set HOTBED_CONTROLLER")
	}
	diskSize, err := setup.ParseBytes(c.DiskSize)
	if err != nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "--disk-size: %v", err)
	}
	var arcMax int64
	if c.ZFSARCMax != "" {
		arcMax, err = setup.ParseBytes(c.ZFSARCMax)
		if err != nil {
			return nil, api.Errorf(api.CodeInvalidArgs, "--zfs-arc-max: %v", err)
		}
	}
	token := strings.TrimSpace(c.Token)
	if token == "" {
		token = controllerAuthToken(c.Controller)
	}
	res, err := setup.Setup(ctx, setup.Options{
		Controller:     c.Controller,
		Token:          token,
		Dir:            c.Dir,
		User:           c.User,
		DiskSize:       diskSize,
		ZFSDataset:     c.ZFSDataset,
		ZFSDevice:      c.ZFSDevice,
		ZFSARCMax:      arcMax,
		UnitProperties: c.UnitProperties,
		Slice:          c.Slice,
		NoStart:        c.NoStart,
		Hostname:       c.Hostname,
		Progress:       d.stderr,
	}, d.tailnet, cmp.Or(d.stream, d.http), d.check)
	if err != nil || runtime.GOOS != "darwin" || c.NoStart {
		return res, err
	}
	// A Mac's worker and machined run under hotbed's daemon: install it,
	// or reinstall it, to run them.
	if _, err := (&daemonInstallCmd{daemonCommonFlags: daemonCommonFlags{
		Dir: c.Dir, Debounce: t3bridge.DefaultDebounce, PollInterval: t3bridge.DefaultPollInterval,
	}}).Exec(ctx, d); err != nil {
		return res, fmt.Errorf("install hotbed's daemon, which runs the worker: %w", err)
	}
	res.Next = nil
	return res, nil
}

type workerStatusCmd struct {
	Dir string `type:"path" help:"Base directory (default ~/.hotbed)."`
}

func (c *workerStatusCmd) Exec(ctx context.Context, _ deps) (any, error) {
	return setup.GetStatus(ctx, c.Dir)
}

type workerStartCmd struct {
	Dir string `type:"path" help:"Base directory (default ~/.hotbed)."`
}

func (c *workerStartCmd) Exec(ctx context.Context, _ deps) (any, error) {
	return setup.Start(ctx, c.Dir)
}

type workerStopCmd struct {
	Dir string `type:"path" help:"Base directory (default ~/.hotbed)."`
}

func (c *workerStopCmd) Exec(ctx context.Context, _ deps) (any, error) {
	return setup.Stop(ctx, c.Dir)
}

type workerTrimCmd struct {
	Dir string `type:"path" help:"Base directory (default ~/.hotbed)."`
}

func (c *workerTrimCmd) Exec(ctx context.Context, _ deps) (any, error) {
	return setup.Trim(ctx, c.Dir)
}

type workerDestroyCmd struct {
	Dir string `type:"path" help:"Base directory (default ~/.hotbed)."`
}

func (c *workerDestroyCmd) Exec(ctx context.Context, d deps) (any, error) {
	return setup.Destroy(ctx, setup.DestroyOptions{
		Dir:      c.Dir,
		Progress: d.stderr,
	})
}
