package main

import (
	"context"
	"strings"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/setup"
)

type workerCmd struct {
	Init    workerInitCmd    `cmd:"" help:"Initialize and start a worker node on this machine in ~/.hotbed (direct on Linux, QEMU Ubuntu+ZFS sparse qcow2 VM on macOS)."`
	Status  workerStatusCmd  `cmd:"" help:"Show the local worker's state and sparse disk usage."`
	Start   workerStartCmd   `cmd:"" help:"Start the local worker (systemd service on Linux, QEMU VM on macOS)."`
	Stop    workerStopCmd    `cmd:"" help:"Gracefully stop the local worker."`
	Trim    workerTrimCmd    `cmd:"" help:"Punch freed blocks out of sparse worker storage (ZFS autotrim / fstrim)."`
	SSH     workerSSHCmd     `cmd:"" help:"Open an SSH shell or run a command inside the macOS QEMU worker VM."`
	Destroy workerDestroyCmd `cmd:"" help:"Stop and remove the local worker setup from ~/.hotbed."`
}

type workerInitCmd struct {
	Controller       string   `arg:"" optional:"" env:"HOTBED_CONTROLLER" help:"The controller's tailnet name or address, with :PORT unless 7120 (or set HOTBED_CONTROLLER)."`
	Dir              string   `type:"path" help:"Base directory for worker state and VM files (default ~/.hotbed)."`
	User             string   `help:"Unix user commands run as (default: invoking user)."`
	DiskSize         string   `name:"disk-size" default:"256G" help:"Virtual size of the sparse ZFS disk image when creating one (e.g. 256G, 512G)."`
	ZFSDataset       string   `name:"zfs-dataset" help:"ZFS dataset for volumes when using native ZFS on Linux."`
	UnitProperties   []string `name:"unit-property" help:"systemd property every command gets (repeatable)."`
	Slice            string   `help:"systemd slice commands run in (default hotbed.slice)."`
	TmpDir           string   `name:"tmp-dir" help:"Directory each command's TMPDIR is created in."`
	NoStart          bool     `name:"no-start" help:"Set up the worker without leaving it running."`
	Hostname         string   `help:"macOS VM: Tailscale and guest hostname (default <mac-hostname>-worker)."`
	CPUs             int      `name:"cpus" help:"macOS VM: vCPU count (default: half of host CPUs, min 2)."`
	Memory           string   `help:"macOS VM: RAM size, e.g. 8G, 16G (default: half of host RAM, clamped 4G..32G)."`
	RootSize         string   `name:"root-size" default:"32G" help:"macOS VM: virtual size of the sparse Ubuntu OS qcow2 disk."`
	SwapSize         string   `name:"swap-size" help:"macOS VM: virtual size of the sparse discard swap qcow2 disk (default: matches VM memory)."`
	Ubuntu           string   `default:"26.04" help:"macOS VM: Ubuntu release version for the cloud image."`
	ImageURL         string   `name:"image-url" help:"macOS VM: override Ubuntu cloud image URL."`
	Binary           string   `type:"path" help:"macOS VM: path to a Linux hotbed binary (auto-cross-compiled if in the hotbed source tree)."`
	TailscaleAuthKey string   `name:"tailscale-auth-key" env:"TS_AUTHKEY" help:"macOS VM: optional Tailscale auth key for non-interactive login."`
	SSHPort          int      `name:"ssh-port" help:"macOS VM: localhost TCP port forwarded to guest SSH (default: 22222 or a free port)."`
	NoAutostart      bool     `name:"no-autostart" help:"macOS VM: do not install a LaunchAgent to auto-start the VM at login."`
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
	var swapSize int64
	if c.SwapSize != "" {
		swapSize, err = setup.ParseBytes(c.SwapSize)
		if err != nil {
			return nil, api.Errorf(api.CodeInvalidArgs, "--swap-size: %v", err)
		}
	}
	return setup.Setup(ctx, setup.Options{
		Controller:       c.Controller,
		Dir:              c.Dir,
		User:             c.User,
		DiskSize:         diskSize,
		ZFSDataset:       c.ZFSDataset,
		UnitProperties:   c.UnitProperties,
		Slice:            c.Slice,
		TmpDir:           c.TmpDir,
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
		SSHPort:          c.SSHPort,
		NoAutostart:      c.NoAutostart,
		Progress:         d.stderr,
	}, d.tailnet, d.http, d.check)
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

type workerDestroyCmd struct {
	Dir        string `type:"path" help:"Base directory (default ~/.hotbed)."`
	PurgeCache bool   `name:"purge-cache" help:"Also delete cached cloud images and cross-compiled binaries in ~/.hotbed/cache."`
}

func (c *workerDestroyCmd) Exec(ctx context.Context, d deps) (any, error) {
	return setup.Destroy(ctx, setup.DestroyOptions{
		Dir:        c.Dir,
		PurgeCache: c.PurgeCache,
		Progress:   d.stderr,
	})
}
