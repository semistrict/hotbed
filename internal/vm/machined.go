package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	machinedv1 "github.com/semistrict/cladefs/go/machined/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/semistrict/hotbed/internal/cladefs"
	"github.com/semistrict/hotbed/internal/guestd"
)

// MachinedGateway is the guest's DNS server and gateway on a machined
// machine's network (libmachine's: its guest is 10.0.2.2/30, configured by
// the kernel command line machined writes). Its guests' /etc/resolv.conf
// names it: machined answers their DNS, and checks what they connect to
// against the names they resolved through it.
const MachinedGateway = "10.0.2.1"

// machinedRootTag is the extent store device machined mounts a machine's root
// cell on (MachineSpec.root).
const machinedRootTag = "root"

// Machined runs hotbed's VMs as machined's machines: one per VM, each in a
// helper process of machined's own, booted from its workspace's cell of
// machined's embedded cladefs daemon (whose Cladefsd service, on the same
// socket, hotbed makes its cells with). Its machines outlive the worker (they
// are machined's), so a restarted worker takes them back as it would
// Firecracker's units.
type Machined struct {
	client machinedv1.MachinedClient
	// images is machined's images directory (its --images): kernels and
	// initramfses are named to it relative to it. The initramfses
	// hotbed-guestd boots from are made in it.
	images string

	mu       sync.Mutex
	consoles map[string]context.CancelFunc // unit -> its console's copy
}

// NewMachined runs VMs on the machined client talks to, whose images
// directory is images.
func NewMachined(client machinedv1.MachinedClient, images string) *Machined {
	return &Machined{client: client, images: images, consoles: map[string]context.CancelFunc{}}
}

// DialMachined connects to machined's API on its Unix socket: Machined, and
// the Cladefsd service of the cladefs daemon it embeds.
func DialMachined(socket string) (*grpc.ClientConn, error) {
	dial := func(ctx context.Context, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}
	conn, err := grpc.NewClient("passthrough:///machined",
		grpc.WithContextDialer(dial),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("a client of machined at %s: %w", socket, err)
	}
	return conn, nil
}

func (d *Machined) Boot(ctx context.Context, m Machine, spec LaunchSpec, cfg Config) (Booted, error) {
	tenant, cell, ok := cladefs.ParseCellID(spec.CellHostPath)
	if !ok {
		return Booted{}, fmt.Errorf("vm %s: %q is no cell ID (TENANT/CELL)", m.ID, spec.CellHostPath)
	}
	kernel, err := d.image(cfg.KernelPath)
	if err != nil {
		return Booted{}, fmt.Errorf("vm %s: kernel: %w", m.ID, err)
	}
	initramfsPath, err := EnsureInitramfs(filepath.Join(d.images, "hotbed"), cfg.GuestdBin)
	if err != nil {
		return Booted{}, fmt.Errorf("vm %s: %w", m.ID, err)
	}
	initramfs, err := d.image(initramfsPath)
	if err != nil {
		return Booted{}, fmt.Errorf("vm %s: initramfs: %w", m.ID, err)
	}
	vcpus, memMiB := cfg.size(spec)
	machine := &machinedv1.MachineSpec{
		Vcpus:       uint32(vcpus),
		MemoryBytes: uint64(memMiB) << 20,
		Boot: &machinedv1.Boot{
			Kernel:      kernel,
			Initramfs:   initramfs,
			CommandLine: strings.Join(machinedBootArgs(cfg, spec), " "),
		},
		Root:               &machinedv1.Cell{Tenant: tenant, Cell: cell},
		VsockListenPorts:   []uint32{guestd.GitAuthPort},
		Labels:             map[string]string{"hotbed-vm": m.ID},
		RootDaxWindowBytes: cfg.DAXWindowBytes,
		Balloon:            true,
	}
	if !cfg.DisableNetwork && !spec.DisableNet {
		// Anywhere on the internet; hotbed's guests reach the host's
		// services over vsock, not the network.
		machine.Network = &machinedv1.Network{Egress: []*machinedv1.EgressRule{{
			Action: machinedv1.EgressAction_EGRESS_ACTION_ALLOW, Host: "*",
		}}}
	}

	// A machine of this name machined still has stopped (a worker that died
	// before deleting it): its VM is gone, so it is deleted and made again.
	if err := d.Stop(ctx, m.Unit); err != nil {
		return Booted{}, fmt.Errorf("vm %s: remove its old machine: %w", m.ID, err)
	}
	if _, err := d.client.CreateMachine(ctx, &machinedv1.CreateMachineRequest{Name: m.Unit, Spec: machine}); err != nil {
		return Booted{}, fmt.Errorf("vm %s: create machine: %w", m.ID, err)
	}
	started, err := d.client.StartMachine(ctx, &machinedv1.StartMachineRequest{Name: m.Unit})
	if err != nil {
		if derr := d.Stop(context.WithoutCancel(ctx), m.Unit); derr != nil {
			slog.Error("delete a machine that failed to start", "vm", m.ID, "err", derr)
		}
		return Booted{}, fmt.Errorf("vm %s: start machine: %w", m.ID, err)
	}
	d.copyConsole(m)
	return Booted{VsockUDS: started.GetMachine().GetVsockSocket(), VCPUs: vcpus, MemMiB: memMiB}, nil
}

// machinedBootArgs is the kernel command line of a machine for spec, after
// machined's own (its root's, the network's) and libmachine's (the console).
func machinedBootArgs(cfg Config, spec LaunchSpec) []string {
	args := []string{
		"reboot=k",
		"panic=1",
		"psi=1",
		"rdinit=/hotbed-guestd",
		"HOTBED_ROOT=cladefs:" + machinedRootTag,
	}
	if cfg.Nofile > 0 {
		args = append(args, fmt.Sprintf("HOTBED_NOFILE=%d", cfg.Nofile))
	}
	if extra := strings.TrimSpace(spec.ExtraBootArgs); extra != "" {
		args = append(args, extra)
	}
	return args
}

// image is path's name in machined's images directory, which it must be in.
func (d *Machined) image(path string) (string, error) {
	rel, err := filepath.Rel(d.images, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%s is not in machined's images directory %s", path, d.images)
	}
	return rel, nil
}

// copyConsole appends m's console's output to its console log while its
// machine lasts: from where the log ends, so a worker that takes the machine
// back goes on where the last one left off.
func (d *Machined) copyConsole(m Machine) {
	ctx, cancel := context.WithCancel(context.Background())
	d.mu.Lock()
	if old := d.consoles[m.Unit]; old != nil {
		old()
	}
	d.consoles[m.Unit] = cancel
	d.mu.Unlock()
	go func() {
		defer cancel()
		log, err := os.OpenFile(m.ConsoleLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			slog.Error("open a machine's console log", "vm", m.ID, "err", err)
			return
		}
		defer log.Close()
		var offset uint64
		if info, err := log.Stat(); err == nil {
			offset = uint64(info.Size())
		}
		stream, err := d.client.StreamConsole(ctx, &machinedv1.StreamConsoleRequest{Name: m.Unit, Offset: offset, Follow: true})
		if err != nil {
			slog.Error("stream a machine's console", "vm", m.ID, "err", err)
			return
		}
		for {
			chunk, err := stream.Recv()
			if err != nil {
				if !errors.Is(err, io.EOF) && ctx.Err() == nil && status.Code(err) != codes.NotFound {
					slog.Warn("a machine's console stream ended", "vm", m.ID, "err", err)
				}
				return
			}
			if _, err := log.Write(chunk.GetData()); err != nil {
				slog.Error("write a machine's console log", "vm", m.ID, "err", err)
				return
			}
		}
	}()
}

func (d *Machined) stopConsole(unit string) {
	d.mu.Lock()
	cancel := d.consoles[unit]
	delete(d.consoles, unit)
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Stop deletes unit's machine, stopping it first if it runs: its cell is
// closed once it returns.
func (d *Machined) Stop(ctx context.Context, unit string) error {
	d.stopConsole(unit)
	_, err := d.client.DeleteMachine(ctx, &machinedv1.DeleteMachineRequest{Name: unit, Force: true})
	if status.Code(err) == codes.NotFound {
		return nil
	}
	return err
}

// Kill is Stop's: machined ends a machine's helper process when it stops it.
func (d *Machined) Kill(context.Context, string) error { return ErrNoKill }

func (d *Machined) Status(ctx context.Context, unit string) (ProcessStatus, error) {
	got, err := d.client.GetMachine(ctx, &machinedv1.GetMachineRequest{Name: unit})
	if status.Code(err) == codes.NotFound {
		return ProcessStatus{}, nil
	}
	if err != nil {
		return ProcessStatus{}, err
	}
	return machineStatus(got.GetMachine().GetStatus()), nil
}

// machineStatus is a machine's status as a VM's.
func machineStatus(s *machinedv1.MachineStatus) ProcessStatus {
	st := ProcessStatus{Loaded: true}
	switch s.GetState() {
	case machinedv1.MachineState_MACHINE_STATE_STARTING,
		machinedv1.MachineState_MACHINE_STATE_RUNNING,
		machinedv1.MachineState_MACHINE_STATE_SUSPENDED:
		st.Running = true
	case machinedv1.MachineState_MACHINE_STATE_STOPPING:
		st.Deactivating = true
	case machinedv1.MachineState_MACHINE_STATE_STOPPED:
		st.Exited = true
		exit := s.GetLastExit()
		st.Result = strings.ToLower(strings.TrimPrefix(exit.GetReason().String(), "EXIT_REASON_"))
		switch exit.GetReason() {
		case machinedv1.ExitReason_EXIT_REASON_POWEROFF, machinedv1.ExitReason_EXIT_REASON_STOPPED:
			st.Detail = "clean shutdown"
		default:
			st.ExitCode = 1
			st.Detail = "machine ended: " + st.Result
			if exit.GetDetail() != "" {
				st.Detail += ": " + exit.GetDetail()
			}
		}
	}
	return st
}

func (d *Machined) List(ctx context.Context, _ string) ([]string, error) {
	got, err := d.client.ListMachines(ctx, &machinedv1.ListMachinesRequest{})
	if err != nil {
		return nil, err
	}
	var units []string
	for _, m := range got.GetMachines() {
		if m.GetSpec().GetLabels()["hotbed-vm"] != "" {
			units = append(units, m.GetName())
		}
	}
	return units, nil
}

func (d *Machined) Adopt(m Machine, _ Booted) { d.copyConsole(m) }

// Free has nothing to do: a machine holds nothing past Stop's delete.
func (d *Machined) Free(context.Context, Machine, Booted) {}

func (d *Machined) Sweep(context.Context, map[string]bool) {}
