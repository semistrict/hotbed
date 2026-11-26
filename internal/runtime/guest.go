package runtime

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/semistrict/hotbed/internal/api"
	guestdv1 "github.com/semistrict/hotbed/internal/gen/guestd/v1"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/guestd"
	"github.com/semistrict/hotbed/internal/inspect"
	"github.com/semistrict/hotbed/internal/unit"
)

// GuestResolver maps a workspace ID to a live hotbed-guestd client and handles
// VM boot/teardown lifecycle (implemented by the Firecracker + cladefsd VM
// manager in Step 3, or a test harness).
type GuestResolver interface {
	// EnsureVM boots wsID's VM unless it is up and returns its client. A VM up
	// at another size than a non-zero field of size asks is stopped and booted
	// again at that size: callers ask a size only of a VM nothing runs in yet.
	EnsureVM(ctx context.Context, wsID string, size unit.VMSize) (*guestd.Client, error)
	ClientFor(wsID string) (*guestd.Client, error)
	StopVM(ctx context.Context, wsID string) error
}

// GuestRuntime implements WorkspaceRuntime by delegating every workspace
// operation to hotbed-guestd inside the workspace's Firecracker microVM.
type GuestRuntime struct {
	vms GuestResolver
}

// NewGuestRuntime creates a VM-backed WorkspaceRuntime using vms.
func NewGuestRuntime(vms GuestResolver) *GuestRuntime {
	return &GuestRuntime{vms: vms}
}

func (g *GuestRuntime) Kind() string { return KindVM }

func (g *GuestRuntime) EnsureReady(ctx context.Context, wsID string) error {
	c, err := g.vms.EnsureVM(ctx, wsID, unit.VMSize{})
	if err != nil {
		return err
	}
	_, err = c.WaitReady(ctx)
	return err
}

// Teardown stops wsID's VM, its guest's filesystem synced and its commands
// stopped first: StopVM does both, in one Shutdown (a second would find the
// guest powering off and fail).
func (g *GuestRuntime) Teardown(ctx context.Context, wsID string) error {
	return g.vms.StopVM(ctx, wsID)
}

// IsVMRunning reports whether wsID currently has a live Firecracker VM process
// attached.
func (g *GuestRuntime) IsVMRunning(wsID string) bool {
	_, err := g.vms.ClientFor(wsID)
	return err == nil
}

func (g *GuestRuntime) client(ctx context.Context, wsID string) (*guestd.Client, error) {
	// Always go through EnsureVM so a tracked Firecracker VMM that exited
	// unexpectedly (e.g. systemd-oomd kill or cladefsd exit 158) is detected,
	// cleaned up, and rebooted cleanly instead of returning a dead vsock client.
	return g.vms.EnsureVM(ctx, wsID, unit.VMSize{})
}

func (g *GuestRuntime) StartCommand(ctx context.Context, wsID string, spec CommandSpec) error {
	c, err := g.vms.EnsureVM(ctx, wsID, spec.VM)
	if err != nil {
		return err
	}
	_, err = c.StartCommand(ctx, &guestdv1.StartCommandRequest{
		Id:         spec.ID,
		Argv:       spec.Argv,
		Dir:        spec.Dir,
		Env:        spec.Env,
		User:       spec.User,
		Uid:        int32(spec.UID),
		Gid:        int32(spec.GID),
		CpuWeight:  int32(spec.CPUWeight),
		MemoryLow:  spec.MemoryLow,
		MemoryHigh: spec.MemoryHigh,
		Tty:        spec.TTY,
	})
	return err
}

func (g *GuestRuntime) SendInput(ctx context.Context, wsID, cmdID string, data []byte) error {
	c, err := g.vms.ClientFor(wsID)
	if err != nil {
		return err
	}
	return c.SendInput(ctx, cmdID, data)
}

// VMExitReporter is optionally implemented by a GuestResolver that can tell
// how a workspace's Firecracker process ended (e.g. exit 158 on losing
// cladefsd, signal 9 from systemd-oomd or cladefsd letting it go).
type VMExitReporter interface {
	// VMExit reports how wsID's VM ended, or false while it runs.
	VMExit(ctx context.Context, wsID string) (unit.VMExit, bool)
}

func (g *GuestRuntime) vmExit(ctx context.Context, wsID string) (unit.VMExit, bool) {
	if r, ok := g.vms.(VMExitReporter); ok {
		return r.VMExit(ctx, wsID)
	}
	return unit.VMExit{}, false
}

func (g *GuestRuntime) StatusMany(ctx context.Context, wsID string, ids []string) (map[string]CommandStatus, error) {
	c, err := g.vms.ClientFor(wsID)
	if err == nil {
		raw, rerr := c.StatusMany(ctx, ids)
		if rerr == nil {
			out := make(map[string]CommandStatus, len(raw))
			for id, st := range raw {
				out[id] = CommandStatus{
					ID:             st.Id,
					Running:        st.State == guestdv1.CommandState_COMMAND_STATE_RUNNING,
					Frozen:         st.State == guestdv1.CommandState_COMMAND_STATE_FROZEN,
					Exited:         st.State == guestdv1.CommandState_COMMAND_STATE_EXITED,
					ExitCode:       int(st.ExitCode),
					Signal:         int(st.Signal),
					Result:         st.Result,
					OOMKilled:      st.OomKilled,
					MainPID:        int(st.MainPid),
					Cgroup:         st.Cgroup,
					MemoryAnon:     st.MemoryAnon,
					MemorySwap:     st.MemorySwap,
					MemoryPeak:     st.MemoryPeak,
					MemorySwapPeak: st.MemorySwapPeak,
					CPUUsec:        st.CpuUsec,
				}
			}
			return out, nil
		}
		err = rerr
	}
	if len(ids) > 0 {
		// The guest cannot be asked, and its VM has ended: every command in
		// it ended with it, each with no exit of its own to report.
		if ex, ended := g.vmExit(ctx, wsID); ended {
			out := make(map[string]CommandStatus, len(ids))
			for _, id := range ids {
				out[id] = CommandStatus{ID: id, Exited: true, VMExit: &ex}
			}
			return out, nil
		}
	}
	return nil, err
}

func (g *GuestRuntime) StopCommand(ctx context.Context, wsID, cmdID string, grace time.Duration) error {
	c, err := g.vms.ClientFor(wsID)
	if err == nil {
		_, err = c.StopCommand(ctx, cmdID, grace)
		if err == nil {
			return nil
		}
	}
	if _, ended := g.vmExit(ctx, wsID); ended {
		return nil
	}
	return err
}

func (g *GuestRuntime) FreezeCommand(ctx context.Context, wsID, cmdID string) error {
	c, err := g.vms.ClientFor(wsID)
	if err != nil {
		return err
	}
	_, err = c.FreezeCommand(ctx, cmdID)
	return err
}

func (g *GuestRuntime) ThawCommand(ctx context.Context, wsID, cmdID string) error {
	c, err := g.vms.ClientFor(wsID)
	if err != nil {
		return err
	}
	_, err = c.ThawCommand(ctx, cmdID)
	return err
}

func (g *GuestRuntime) Reclaim(ctx context.Context, wsID, cmdID string, bytes int64) error {
	c, err := g.vms.ClientFor(wsID)
	if err != nil {
		return nil
	}
	if err := c.Reclaim(ctx, cmdID, bytes); err != nil {
		// Older guestd builds without Reclaim return Unimplemented; treat as a
		// best-effort no-op so rolling upgrades never log spurious errors.
		if connect.CodeOf(err) == connect.CodeUnimplemented {
			return nil
		}
		return err
	}
	return nil
}

func (g *GuestRuntime) StreamLogs(ctx context.Context, wsID, cmdID string, offset int64, follow bool, dst io.Writer) (int64, error) {
	c, err := g.vms.ClientFor(wsID)
	if err != nil {
		return 0, err
	}
	return c.StreamLogs(ctx, cmdID, offset, follow, dst)
}

func (g *GuestRuntime) InspectCommand(ctx context.Context, wsID string, req *pb.InspectCommandRequest) (*pb.InspectCommandResponse, error) {
	c, err := g.vms.ClientFor(wsID)
	if err != nil {
		return nil, api.Errorf(api.CodeConflict, "workspace %s VM is not running: %v", wsID, err)
	}
	return c.InspectCommand(ctx, req)
}

func (g *GuestRuntime) TraceCommand(ctx context.Context, wsID string, req *pb.TraceCommandRequest, emit inspect.Emit) error {
	c, err := g.vms.ClientFor(wsID)
	if err != nil {
		return api.Errorf(api.CodeConflict, "workspace %s VM is not running: %v", wsID, err)
	}
	return c.TraceCommand(ctx, req, emit)
}

func (g *GuestRuntime) SampleCommand(ctx context.Context, wsID string, req *pb.SampleCommandRequest, emit inspect.SampleEmit) error {
	c, err := g.vms.ClientFor(wsID)
	if err != nil {
		return api.Errorf(api.CodeConflict, "workspace %s VM is not running: %v", wsID, err)
	}
	return c.SampleCommand(ctx, req, emit)
}

func (g *GuestRuntime) CommandProfile(ctx context.Context, wsID string, req *pb.GetProfileRequest) (*pb.CommandProfile, error) {
	c, err := g.vms.ClientFor(wsID)
	if err != nil {
		return nil, api.Errorf(api.CodeConflict, "workspace %s VM is not running: %v", wsID, err)
	}
	return c.CommandProfile(ctx, req)
}

func (g *GuestRuntime) ForgetCommandProfile(ctx context.Context, wsID, cmdID string) error {
	c, err := g.vms.ClientFor(wsID)
	if err != nil {
		return nil
	}
	return c.ForgetCommandProfile(ctx, cmdID)
}

func (g *GuestRuntime) ApplySync(ctx context.Context, wsID string, spec SyncSpec) (int, error) {
	c, err := g.client(ctx, wsID)
	if err != nil {
		return 0, err
	}
	return c.ApplySync(ctx, spec.Repo, spec.BundlePath, spec.Commit, spec.BaseCommit, spec.Subs, spec.Env, spec.UID, spec.GID)
}

func (g *GuestRuntime) StartShell(ctx context.Context, wsID string, spec ShellSpec, sio unit.ShellIO) (unit.ShellHandle, error) {
	c, err := g.client(ctx, wsID)
	if err != nil {
		return nil, err
	}
	return c.StartShell(ctx, guestd.ShellOpenRequest{
		Argv: spec.Argv,
		Dir:  spec.Dir,
		Env:  spec.Env,
		User: spec.User,
		UID:  spec.UID,
		GID:  spec.GID,
	}, sio)
}

func (g *GuestRuntime) ListFiles(ctx context.Context, wsID string, repos []RepoRef) ([]*pb.FileEntry, error) {
	c, err := g.client(ctx, wsID)
	if err != nil {
		return nil, err
	}
	req := make([]*guestdv1.RepoBase, len(repos))
	for i, r := range repos {
		req[i] = &guestdv1.RepoBase{Repo: r.Repo, BaseCommit: r.BaseCommit}
	}
	return c.ListFiles(ctx, req)
}

func (g *GuestRuntime) GetFile(ctx context.Context, wsID, repo, baseCommit, relPath string) (*pb.FileContent, error) {
	c, err := g.client(ctx, wsID)
	if err != nil {
		return nil, err
	}
	return c.GetFile(ctx, repo, baseCommit, relPath)
}

func (g *GuestRuntime) DialPort(ctx context.Context, wsID string, port uint16) (net.Conn, error) {
	c, err := g.vms.ClientFor(wsID)
	if err != nil {
		return nil, fmt.Errorf("workspace %s not running: %w", wsID, err)
	}
	return c.DialTCP(ctx, port)
}
