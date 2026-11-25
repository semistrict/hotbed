package runtime

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/guestd"
	"github.com/semistrict/hotbed/internal/inspect"
	"github.com/semistrict/hotbed/internal/unit"
)

// GuestResolver maps a workspace ID to a live hotbed-guestd client and handles
// VM boot/teardown lifecycle (implemented by the Firecracker + cladefsd VM
// manager in Step 3, or a test harness).
type GuestResolver interface {
	EnsureVM(ctx context.Context, wsID string) (*guestd.Client, error)
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
	c, err := g.vms.EnsureVM(ctx, wsID)
	if err != nil {
		return err
	}
	_, err = c.WaitReady(ctx)
	return err
}

func (g *GuestRuntime) Teardown(ctx context.Context, wsID string) error {
	if c, err := g.vms.ClientFor(wsID); err == nil {
		// Best-effort clean flush & command stop before stopping the VM.
		shutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = c.Shutdown(shutCtx, guestd.ShutdownRequest{
			SyncFS:       true,
			StopCommands: true,
			PowerOff:     true,
		})
		cancel()
	}
	return g.vms.StopVM(ctx, wsID)
}

func (g *GuestRuntime) client(ctx context.Context, wsID string) (*guestd.Client, error) {
	// Always go through EnsureVM so a tracked Firecracker VMM that exited
	// unexpectedly (e.g. systemd-oomd kill or cladefsd exit 158) is detected,
	// cleaned up, and rebooted cleanly instead of returning a dead vsock client.
	return g.vms.EnsureVM(ctx, wsID)
}

func (g *GuestRuntime) StartCommand(ctx context.Context, wsID string, spec CommandSpec) error {
	c, err := g.client(ctx, wsID)
	if err != nil {
		return err
	}
	_, err = c.StartCommand(ctx, guestd.StartCommandRequest{
		ID:         spec.ID,
		Argv:       spec.Argv,
		Dir:        spec.Dir,
		Env:        spec.Env,
		User:       spec.User,
		UID:        spec.UID,
		GID:        spec.GID,
		CPUWeight:  spec.CPUWeight,
		MemoryLow:  spec.MemoryLow,
		MemoryHigh: spec.MemoryHigh,
	})
	return err
}

// vmExitChecker is optionally implemented by a GuestResolver that can report
// whether a workspace's Firecracker VMM process has already exited (e.g. exit
// 158 on cladefsd restart or exit 159 on DAX fault).
type vmExitChecker interface {
	VMExited(ctx context.Context, wsID string) (exited bool, exitCode int)
}

// vmExitStatusChecker is optionally implemented by a GuestResolver that can
// report rich systemd exit details (OOM kill, signal, result) when a
// workspace's Firecracker VMM process exits.
type vmExitStatusChecker interface {
	VMExitStatus(ctx context.Context, wsID string) (exited bool, exitCode, signal int, result string, oomKilled bool)
}

func (g *GuestRuntime) vmExitStatus(ctx context.Context, wsID string) (exited bool, exitCode, signal int, result string, oomKilled bool) {
	if c, ok := g.vms.(vmExitStatusChecker); ok {
		return c.VMExitStatus(ctx, wsID)
	}
	if c, ok := g.vms.(vmExitChecker); ok {
		ex, code := c.VMExited(ctx, wsID)
		return ex, code, 0, "", false
	}
	return false, 0, 0, "", false
}

func (g *GuestRuntime) StatusMany(ctx context.Context, wsID string, ids []string) (map[string]CommandStatus, error) {
	c, err := g.vms.ClientFor(wsID)
	if err == nil {
		raw, rerr := c.StatusMany(ctx, ids)
		if rerr == nil {
			out := make(map[string]CommandStatus, len(raw))
			for id, st := range raw {
				out[id] = CommandStatus{
					ID:         st.ID,
					Running:    st.State == guestd.StateRunning,
					Frozen:     st.State == guestd.StateFrozen,
					Exited:     st.State == guestd.StateExited,
					ExitCode:   st.ExitCode,
					MainPID:    st.MainPID,
					Cgroup:     st.Cgroup,
					MemoryAnon: st.MemoryAnon,
					MemorySwap: st.MemorySwap,
					CPUUsec:    st.CPUUsec,
				}
			}
			return out, nil
		}
		err = rerr
	}
	if len(ids) > 0 {
		if exited, code, sig, res, oom := g.vmExitStatus(ctx, wsID); exited {
			if code == 0 && sig == 0 && !oom {
				code = 137
			}
			out := make(map[string]CommandStatus, len(ids))
			for _, id := range ids {
				out[id] = CommandStatus{
					ID:        id,
					Exited:    true,
					ExitCode:  code,
					Signal:    sig,
					Result:    res,
					OOMKilled: oom,
				}
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
	if exited, _, _, _, _ := g.vmExitStatus(ctx, wsID); exited {
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
		// Older guestd builds without /v1/reclaim return 404; treat as a
		// best-effort no-op so rolling upgrades never log spurious errors.
		if strings.Contains(err.Error(), "(404)") {
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
	req := make([]guestd.RepoBase, len(repos))
	for i, r := range repos {
		req[i] = guestd.RepoBase{Repo: r.Repo, BaseCommit: r.BaseCommit}
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
