package main

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/setup"
)

type workerCmd struct {
	Init      workerInitCmd      `cmd:"" help:"Initialize and start a worker node on this machine in ~/.hotbed (direct on Linux, an Ubuntu+ZFS VM with nested virtualization on macOS)."`
	Status    workerStatusCmd    `cmd:"" help:"Show the local worker's state and sparse disk usage."`
	Start     workerStartCmd     `cmd:"" help:"Start the local worker (systemd service on Linux, the worker VM on macOS)."`
	Stop      workerStopCmd      `cmd:"" help:"Gracefully stop the local worker."`
	Trim      workerTrimCmd      `cmd:"" help:"Punch freed blocks out of sparse worker storage (ZFS autotrim / fstrim)."`
	SSH       workerSSHCmd       `cmd:"" help:"Open an SSH shell or run a command inside the macOS worker VM."`
	Supervise workerSuperviseCmd `cmd:"" help:"Power the macOS worker VM as the controller says, until stopped: off once idle, on when a run needs it. Its LaunchAgent runs it."`
	Destroy   workerDestroyCmd   `cmd:"" help:"Stop and remove the local worker setup from ~/.hotbed."`
}

type workerInitCmd struct {
	Controller       string   `arg:"" optional:"" env:"HOTBED_CONTROLLER" help:"The controller's HTTPS URL or tailnet name/address, with :PORT unless 7120 (or set HOTBED_CONTROLLER)."`
	Token            string   `env:"HOTBED_TOKEN" help:"Bearer or org join token (hbj_...) for authenticating to a remote HTTPS controller."`
	Dir              string   `type:"path" help:"Base directory for worker state and VM files (default ~/.hotbed)."`
	User             string   `help:"Unix user commands run as (default: invoking user)."`
	DiskSize         string   `name:"disk-size" default:"256G" help:"Virtual size of the sparse ZFS disk image when creating one (e.g. 256G, 512G)."`
	ZFSDataset       string   `name:"zfs-dataset" help:"ZFS dataset for volumes when using native ZFS on Linux."`
	ZFSDevice        string   `name:"zfs-device" help:"Linux: block device path under /dev/ for a dedicated ZFS pool (e.g. /dev/disk/by-id/virtio-hotbed-zfs)."`
	TrimFS           bool     `name:"fstrim" hidden:"" help:"Linux: also run fstrim -a in the periodic trim timer (used for VM guests)."`
	ZFSARCMax        string   `name:"zfs-arc-max" help:"Linux: cap the ZFS ARC of the pool hotbed makes, e.g. 4G (default: zfs's own, nearly all of RAM; the macOS VM caps it to a quarter of its RAM)."`
	UnitProperties   []string `name:"unit-property" help:"systemd property every command gets (repeatable)."`
	Slice            string   `help:"systemd slice commands run in (default hotbed.slice)."`
	NoStart          bool     `name:"no-start" help:"Set up the worker without leaving it running."`
	Hostname         string   `help:"macOS VM: Tailscale and guest hostname (default <mac-hostname>-worker)."`
	CPUs             int      `name:"cpus" help:"macOS VM: vCPU count (default: half of host CPUs, min 2)."`
	Memory           string   `help:"macOS VM: RAM size, e.g. 8G, 16G (default: a quarter of host RAM, clamped 4G..16G). The Mac keeps all of it for as long as the VM runs."`
	RootSize         string   `name:"root-size" default:"32G" help:"macOS VM: virtual size of the sparse Ubuntu OS disk."`
	SwapSize         string   `name:"swap-size" help:"macOS VM: virtual size of the sparse discard swap disk (default: matches VM memory)."`
	Ubuntu           string   `default:"26.04" help:"macOS VM: Ubuntu release version for the cloud image."`
	ImageURL         string   `name:"image-url" help:"macOS VM: override Ubuntu cloud image URL."`
	Binary           string   `type:"path" help:"macOS VM: path to a Linux hotbed binary (default: fetched from the controller)."`
	TailscaleAuthKey string   `name:"tailscale-auth-key" env:"TS_AUTHKEY" help:"macOS VM: optional Tailscale auth key for non-interactive login."`
	NoAutostart      bool     `name:"no-autostart" help:"macOS VM: do not install the LaunchAgent that powers the VM off once idle and on when a run needs it."`
	SleepAfter       string   `name:"sleep-after" help:"macOS VM: how long the VM has no work before it powers off, e.g. 30m, 2h; 0 never (default 30m, or as before)."`
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
	rootSize, err := setup.ParseBytes(c.RootSize)
	if err != nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "--root-size: %v", err)
	}
	var memBytes int64
	if c.Memory != "" {
		memBytes, err = setup.ParseBytes(c.Memory)
		if err != nil {
			return nil, api.Errorf(api.CodeInvalidArgs, "--memory: %v", err)
		}
	}
	var arcMax int64
	if c.ZFSARCMax != "" {
		arcMax, err = setup.ParseBytes(c.ZFSARCMax)
		if err != nil {
			return nil, api.Errorf(api.CodeInvalidArgs, "--zfs-arc-max: %v", err)
		}
	}
	var swapSize int64
	if c.SwapSize != "" {
		swapSize, err = setup.ParseBytes(c.SwapSize)
		if err != nil {
			return nil, api.Errorf(api.CodeInvalidArgs, "--swap-size: %v", err)
		}
	}
	token := strings.TrimSpace(c.Token)
	if token == "" {
		token = controllerAuthToken(c.Controller)
	}
	return setup.Setup(ctx, setup.Options{
		Controller:       c.Controller,
		Token:            token,
		Dir:              c.Dir,
		User:             c.User,
		DiskSize:         diskSize,
		ZFSDataset:       c.ZFSDataset,
		ZFSDevice:        c.ZFSDevice,
		TrimFS:           c.TrimFS,
		ZFSARCMax:        arcMax,
		UnitProperties:   c.UnitProperties,
		Slice:            c.Slice,
		NoStart:          c.NoStart,
		Hostname:         c.Hostname,
		CPUs:             c.CPUs,
		MemoryBytes:      memBytes,
		RootSize:         rootSize,
		SwapSize:         swapSize,
		UbuntuVersion:    c.Ubuntu,
		ImageURL:         c.ImageURL,
		BinaryPath:       c.Binary,
		TailscaleAuthKey: c.TailscaleAuthKey,
		NoAutostart:      c.NoAutostart,
		SleepAfter:       c.SleepAfter,
		Progress:         d.stderr,
	}, d.tailnet, cmp.Or(d.stream, d.http), d.check)
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

type workerSSHCmd struct {
	Dir  string   `type:"path" help:"Base directory (default ~/.hotbed)."`
	Args []string `arg:"" optional:"" passthrough:"" help:"Command to run inside the VM (omit for an interactive shell)."`
}

func (c *workerSSHCmd) Exec(context.Context, deps) (any, error) { return nil, nil }

func (c *workerSSHCmd) Raw(ctx context.Context, _ deps) (int, bool, error) {
	code, err := setup.SSH(ctx, c.Dir, c.Args)
	if err != nil {
		return exitError, false, err
	}
	return code, true, nil
}

type workerSuperviseCmd struct {
	Dir string `type:"path" help:"Base directory (default ~/.hotbed)."`
}

func (c *workerSuperviseCmd) Exec(ctx context.Context, d deps) (any, error) {
	exe, err := d.exe()
	if err != nil {
		return nil, err
	}
	// A new build installed over this one takes over: the LaunchAgent
	// starts it again once this one exits.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	was, err := os.Stat(exe)
	if err != nil {
		return nil, fmt.Errorf("stat its own binary: %w", err)
	}
	go func() {
		if replacedAt(ctx, exe, was, time.Minute) {
			slog.Info("supervise: a new build replaced this one; exiting for it", "binary", exe)
			cancel()
		}
	}()
	err = setup.Supervise(ctx, c.Dir, func(controller string) (hotbedv1connect.ClusterServiceClient, error) {
		ic, err := controllerAuthInterceptors(controller, d.http)
		if err != nil {
			return nil, err
		}
		cluster, _, err := api.Controller(controller, d.stream, ic...)
		return cluster, err
	}, slog.Default())
	cancel()
	return nil, err
}

// replacedAt says, once the file at path is another than was (checking
// every interval), that it was replaced; false when ctx ends first.
func replacedAt(ctx context.Context, path string, was os.FileInfo, interval time.Duration) bool {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		}
		now, err := os.Stat(path)
		if err == nil && (!os.SameFile(was, now) || !now.ModTime().Equal(was.ModTime())) {
			return true
		}
	}
}

type workerDestroyCmd struct {
	Dir        string `type:"path" help:"Base directory (default ~/.hotbed)."`
	PurgeCache bool   `name:"purge-cache" help:"Also delete cached cloud images and Linux binaries in ~/.hotbed/cache."`
}

func (c *workerDestroyCmd) Exec(ctx context.Context, d deps) (any, error) {
	return setup.Destroy(ctx, setup.DestroyOptions{
		Dir:        c.Dir,
		PurgeCache: c.PurgeCache,
		Progress:   d.stderr,
	})
}
