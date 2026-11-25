// Package runtime defines the workspace execution boundary between hotbed's
// worker engine and the underlying isolation mechanism (host-kernel systemd
// chroot or Firecracker microVM + cladefs via hotbed-guestd).
package runtime

import (
	"context"
	"io"
	"net"
	"time"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/inspect"
	"github.com/semistrict/hotbed/internal/unit"
)

// Kind identifies an execution backend.
const (
	KindVM      = "vm"
	KindCladefs = "cladefs"
)

// CommandSpec describes a command to run inside a workspace.
type CommandSpec struct {
	ID         string
	Argv       []string
	Dir        string
	Env        []string
	User       string
	UID        int
	GID        int
	CPUWeight  int
	MemoryLow  int64
	MemoryHigh int64
	LogPath    string
	// VM is the size of the VM it runs in, on a runtime that boots one for
	// it; zero fields for the node's defaults.
	VM unit.VMSize
}

// CommandStatus is the runtime's observed state of a single command.
type CommandStatus struct {
	ID         string
	Running    bool
	Frozen     bool
	Exited     bool
	ExitCode   int
	Signal     int
	Result     string
	OOMKilled  bool
	MainPID    int
	Cgroup     string
	MemoryAnon     int64
	MemorySwap     int64
	MemoryPeak     int64
	MemorySwapPeak int64
	CPUUsec        int64
}

// SyncSpec carries local bundle files to apply inside a workspace repo.
type SyncSpec struct {
	Repo       string
	BundlePath string
	Commit     string
	BaseCommit string
	Subs       []gitops.SubCheckout
	Env        []string
	UID        int
	GID        int
}

// RepoRef identifies a repo checkout and its base commit inside a workspace.
type RepoRef struct {
	Repo       string
	BaseCommit string
}

// ShellSpec describes an interactive or piped shell session inside a workspace.
type ShellSpec struct {
	Argv []string
	Dir  string
	Env  []string
	User string
	UID  int
	GID  int
}

// WorkspaceRuntime executes commands, syncs, interactive shells, file queries,
// and port forwarding inside a workspace without assuming the host can mount
// or directly read the workspace filesystem.
type WorkspaceRuntime interface {
	// Kind returns "vm" or "cladefs".
	Kind() string

	// EnsureReady makes sure the workspace execution environment (e.g. VM) is
	// booted and reachable.
	EnsureReady(ctx context.Context, wsID string) error
	// Teardown stops the workspace execution environment cleanly (flushing
	// guest filesystems before cell teardown/hand-over).
	Teardown(ctx context.Context, wsID string) error

	// Command lifecycle.
	StartCommand(ctx context.Context, wsID string, spec CommandSpec) error
	StatusMany(ctx context.Context, wsID string, ids []string) (map[string]CommandStatus, error)
	StopCommand(ctx context.Context, wsID, cmdID string, grace time.Duration) error
	FreezeCommand(ctx context.Context, wsID, cmdID string) error
	ThawCommand(ctx context.Context, wsID, cmdID string) error
	Reclaim(ctx context.Context, wsID, cmdID string, bytes int64) error
	StreamLogs(ctx context.Context, wsID, cmdID string, offset int64, follow bool, dst io.Writer) (int64, error)

	// In-guest live command diagnostics (inspect, trace, sample, continuous eBPF profile).
	InspectCommand(ctx context.Context, wsID string, req *pb.InspectCommandRequest) (*pb.InspectCommandResponse, error)
	TraceCommand(ctx context.Context, wsID string, req *pb.TraceCommandRequest, emit inspect.Emit) error
	SampleCommand(ctx context.Context, wsID string, req *pb.SampleCommandRequest, emit inspect.SampleEmit) error
	CommandProfile(ctx context.Context, wsID string, req *pb.GetProfileRequest) (*pb.CommandProfile, error)
	ForgetCommandProfile(ctx context.Context, wsID, cmdID string) error

	// Workspace I/O.
	ApplySync(ctx context.Context, wsID string, spec SyncSpec) (int, error)
	StartShell(ctx context.Context, wsID string, spec ShellSpec, sio unit.ShellIO) (unit.ShellHandle, error)
	ListFiles(ctx context.Context, wsID string, repos []RepoRef) ([]*pb.FileEntry, error)
	GetFile(ctx context.Context, wsID, repo, baseCommit, relPath string) (*pb.FileContent, error)

	// DialPort connects to a TCP port served inside the workspace (--port).
	DialPort(ctx context.Context, wsID string, port uint16) (net.Conn, error)
}

// SuspendableRuntime is an optional WorkspaceRuntime extension for VM runtimes
// that can suspend a workspace VM to a CladeFS memory cell (freeing 100% of
// host RAM and swap) and resume it lazily on demand.
type SuspendableRuntime interface {
	SuspendWorkspace(ctx context.Context, wsID string, workingSetBytes int64) error
	IsSuspended(wsID string) (suspended bool, workingSetBytes int64)
}
