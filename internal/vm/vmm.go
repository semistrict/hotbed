package vm

import (
	"context"
	"errors"
)

// A VMM runs hotbed's VMs, one each: Firecracker processes under systemd on
// Linux (NewManager's default), machined's machines on macOS (Machined). The
// Manager does the rest the same over either: which VMs run, their guestd's
// health, how they stop, their git-auth bridges, and taking them back after a
// worker restart. A VM is named by its unit (hotbed-vm-<id>) to its VMM.
type VMM interface {
	// Boot starts m's VM as spec and cfg say, returning once it runs (its
	// guest booting) with what it took. On error nothing of it runs.
	Boot(ctx context.Context, m Machine, spec LaunchSpec, cfg Config) (Booted, error)
	// Stop stops unit's VM; it is no error if there is none.
	Stop(ctx context.Context, unit string) error
	// Kill stops unit's VM at once, if Stop left it running; ErrNoKill if the
	// VMM has no stronger way than Stop.
	Kill(ctx context.Context, unit string) error
	// Status is unit's VM's, zero (not Loaded) if there is none.
	Status(ctx context.Context, unit string) (ProcessStatus, error)
	// List names the unit of every VM the VMM runs in slice (Firecracker's
	// systemd slice; machined has none).
	List(ctx context.Context, slice string) ([]string, error)
	// Adopt takes back what m's VM, which outlived the last worker, holds.
	Adopt(m Machine, b Booted)
	// Free releases what Boot took beyond the VM itself (a TAP) once m's VM
	// is gone: b is zero if its boot never recorded it.
	Free(ctx context.Context, m Machine, b Booted)
	// Sweep releases what VMs no longer running hold, all but live's.
	Sweep(ctx context.Context, live map[string]bool)
}

// ErrNoKill is a VMM's Kill when it has no stronger way to stop a VM than
// Stop.
var ErrNoKill = errors.New("vmm: no kill beyond stop")

// Machine is one VM as the Manager names it to its VMM.
type Machine struct {
	ID   string
	Unit string // hotbed-vm-<ID>
	// Dir is its own directory, the Manager's (meta.json) and the VMM's.
	Dir string
	// ConsoleLog is the file its console's output goes to.
	ConsoleLog string
	// CID is its guest's vsock context ID.
	CID uint32
}

// Booted is what a VM's boot took, kept in its meta.json for the worker that
// takes it back.
type Booted struct {
	// VsockUDS is the Unix socket its guest's vsock is reached through, in
	// Firecracker's protocol (guestd.FirecrackerVsockDialer): guest ports
	// it connects to are "<VsockUDS>_<port>".
	VsockUDS string
	// Net is its TAP (Firecracker): zero when the VMM gives it none.
	Net NetAlloc
	// ToolsDiskPath is the read-only disk it boots hotbed-guestd from
	// (Firecracker).
	ToolsDiskPath string
	// FirecrackerLog is Firecracker's own log, kept with its console's
	// when it crashes.
	FirecrackerLog string
	// VCPUs and MemMiB are the size it booted at.
	VCPUs  int
	MemMiB int
}
