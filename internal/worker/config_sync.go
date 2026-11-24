package worker

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"sync"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/vm"
)

const controllerConfigFilename = "controller-config.json"

// LoadControllerConfig reads the last-persisted controller WorkerConfig from
// <workerDir>/controller-config.json. Returns (nil, nil) if none exists yet.
func LoadControllerConfig(workerDir string) (*pb.WorkerConfig, error) {
	if workerDir == "" {
		return nil, nil
	}
	b, err := os.ReadFile(filepath.Join(workerDir, controllerConfigFilename))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cfg := &pb.WorkerConfig{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// SaveControllerConfig atomically persists cfg to <workerDir>/controller-config.json
// (or removes the file when cfg is nil).
func SaveControllerConfig(workerDir string, cfg *pb.WorkerConfig) error {
	if workerDir == "" {
		return nil
	}
	dst := filepath.Join(workerDir, controllerConfigFilename)
	if cfg == nil {
		err := os.Remove(dst)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	b, err := (protojson.MarshalOptions{Multiline: true, Indent: "  "}).Marshal(cfg)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(workerDir, controllerConfigFilename+".*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return err
	}
	ok = true
	return nil
}

// MergeWorkerConfig layers controller-managed remote settings onto local
// statedir.WorkerConfig. Only explicitly set fields in remote override local.
func MergeWorkerConfig(local statedir.WorkerConfig, remote *pb.WorkerConfig) statedir.WorkerConfig {
	out := local
	if local.UnitProperties != nil {
		out.UnitProperties = slices.Clone(local.UnitProperties)
	}
	if local.VM != nil {
		v := *local.VM
		if v.DNSServers != nil {
			v.DNSServers = slices.Clone(v.DNSServers)
		}
		out.VM = &v
	}
	if local.CladefsFUSE != nil {
		f := *local.CladefsFUSE
		out.CladefsFUSE = &f
	}
	if remote == nil {
		return out
	}

	if remote.Slice != nil {
		out.Slice = remote.GetSlice()
	}
	if len(remote.UnitProperties) > 0 {
		out.UnitProperties = slices.Clone(remote.UnitProperties)
	}
	if rvm := remote.GetVm(); rvm != nil {
		if out.VM == nil {
			out.VM = &statedir.VMConfig{}
		}
		if rvm.Enabled != nil {
			out.VM.Enabled = rvm.GetEnabled()
		}
		if rvm.DefaultRuntime != nil {
			out.VM.Default = rvm.GetDefaultRuntime()
		}
		if rvm.CladefsControlSock != nil {
			out.VM.CladefsControlSock = rvm.GetCladefsControlSock()
		}
		if rvm.CladefsVmSock != nil {
			out.VM.CladefsVMSock = rvm.GetCladefsVmSock()
		}
		if rvm.CladefsPeerAddr != nil {
			out.VM.CladefsPeerAddr = rvm.GetCladefsPeerAddr()
		}
		if rvm.CellRoot != nil {
			out.VM.CellRoot = rvm.GetCellRoot()
		}
		if rvm.TenantId != nil {
			out.VM.TenantID = rvm.GetTenantId()
		}
		if rvm.FirecrackerBin != nil {
			out.VM.FirecrackerBin = rvm.GetFirecrackerBin()
		}
		if rvm.KernelPath != nil {
			out.VM.KernelPath = rvm.GetKernelPath()
		}
		if rvm.GuestdBin != nil {
			out.VM.GuestdBin = rvm.GetGuestdBin()
		}
		if rvm.VmUser != nil {
			out.VM.VMUser = rvm.GetVmUser()
		}
		if rvm.Vcpus != nil {
			out.VM.VCPUs = int(rvm.GetVcpus())
		}
		if rvm.MemMib != nil {
			out.VM.MemMiB = int(rvm.GetMemMib())
		}
		if rvm.DiskBytes != nil {
			out.VM.DiskBytes = rvm.GetDiskBytes()
		}
		if rvm.DaxWindowBytes != nil {
			out.VM.DAXWindowBytes = rvm.GetDaxWindowBytes()
		}
		if rvm.DaxPrivateMaxBytes != nil {
			out.VM.DAXPrivateMaxBytes = rvm.GetDaxPrivateMaxBytes()
		}
		if rvm.DisableDax != nil {
			out.VM.DisableDAX = rvm.GetDisableDax()
		}
		if rvm.CacheType != nil {
			out.VM.CacheType = rvm.GetCacheType()
		}
		if rvm.BridgeName != nil {
			out.VM.BridgeName = rvm.GetBridgeName()
		}
		if rvm.BridgeCidr != nil {
			out.VM.BridgeCIDR = rvm.GetBridgeCidr()
		}
		if len(rvm.DnsServers) > 0 {
			out.VM.DNSServers = slices.Clone(rvm.DnsServers)
		}
	}
	if rfuse := remote.GetCladefsFuse(); rfuse != nil {
		if out.CladefsFUSE == nil {
			out.CladefsFUSE = &statedir.CladefsFUSEConfig{}
		}
		if rfuse.Enabled != nil {
			out.CladefsFUSE.Enabled = rfuse.GetEnabled()
		}
		if rfuse.DefaultRuntime != nil {
			out.CladefsFUSE.Default = rfuse.GetDefaultRuntime()
		}
		if rfuse.CladefsControlSock != nil {
			out.CladefsFUSE.CladefsControlSock = rfuse.GetCladefsControlSock()
		}
		if rfuse.CladefsPeerAddr != nil {
			out.CladefsFUSE.CladefsPeerAddr = rfuse.GetCladefsPeerAddr()
		}
		if rfuse.CellRoot != nil {
			out.CladefsFUSE.CellRoot = rfuse.GetCellRoot()
		}
		if rfuse.MountRoot != nil {
			out.CladefsFUSE.MountRoot = rfuse.GetMountRoot()
		}
		if rfuse.MountStoresRoot != nil {
			out.CladefsFUSE.MountStoresRoot = rfuse.GetMountStoresRoot()
		}
		if rfuse.TenantId != nil {
			out.CladefsFUSE.TenantID = rfuse.GetTenantId()
		}
		if rfuse.DiskBytes != nil {
			out.CladefsFUSE.DiskBytes = rfuse.GetDiskBytes()
		}
	}
	// Top-level default_runtime overrides backend-specific default flags.
	if remote.DefaultRuntime != nil {
		switch remote.GetDefaultRuntime() {
		case runtime.KindVM:
			if out.VM != nil {
				out.VM.Default = true
			}
			if out.CladefsFUSE != nil {
				out.CladefsFUSE.Default = false
			}
		case runtime.KindCladefs:
			if out.VM != nil {
				out.VM.Default = false
			}
			if out.CladefsFUSE != nil {
				out.CladefsFUSE.Default = true
			}
		case runtime.KindChroot:
			if out.VM != nil {
				out.VM.Default = false
			}
			if out.CladefsFUSE != nil {
				out.CladefsFUSE.Default = false
			}
		}
	}
	return out
}

// RequiresConfigRestart reports whether moving from cur to next changes
// structural worker configuration that cannot be hot-reloaded in place.
func RequiresConfigRestart(cur, next statedir.WorkerConfig) bool {
	if cur.Slice != next.Slice || !slices.Equal(cur.UnitProperties, next.UnitProperties) {
		return true
	}
	curVMEnabled := cur.VM != nil && cur.VM.Enabled
	nextVMEnabled := next.VM != nil && next.VM.Enabled
	if curVMEnabled != nextVMEnabled {
		return true
	}
	if curVMEnabled && nextVMEnabled {
		if cur.VM.CladefsControlSock != next.VM.CladefsControlSock ||
			cur.VM.CladefsVMSock != next.VM.CladefsVMSock ||
			cur.VM.CellRoot != next.VM.CellRoot ||
			cur.VM.TenantID != next.VM.TenantID ||
			cur.VM.BridgeName != next.VM.BridgeName ||
			cur.VM.BridgeCIDR != next.VM.BridgeCIDR {
			return true
		}
	}
	curFuseEnabled := cur.CladefsFUSE != nil && cur.CladefsFUSE.Enabled
	nextFuseEnabled := next.CladefsFUSE != nil && next.CladefsFUSE.Enabled
	if curFuseEnabled != nextFuseEnabled {
		return true
	}
	if curFuseEnabled && nextFuseEnabled {
		if cur.CladefsFUSE.CladefsControlSock != next.CladefsFUSE.CladefsControlSock ||
			cur.CladefsFUSE.CellRoot != next.CladefsFUSE.CellRoot ||
			cur.CladefsFUSE.MountRoot != next.CladefsFUSE.MountRoot ||
			cur.CladefsFUSE.MountStoresRoot != next.CladefsFUSE.MountStoresRoot ||
			cur.CladefsFUSE.TenantID != next.CladefsFUSE.TenantID {
			return true
		}
	}
	return false
}

// ApplyCachedArtifacts overlays any already-cached managed kernel/guestd paths
// from remote onto cfg at worker startup (without blocking on network I/O).
func ApplyCachedArtifacts(store *ArtifactStore, arch string, cfg statedir.WorkerConfig, remote *pb.WorkerConfig) statedir.WorkerConfig {
	if store == nil || cfg.VM == nil || !cfg.VM.Enabled || remote == nil || remote.GetVm() == nil {
		return cfg
	}
	if art := remote.GetVm().GetKernels()[arch]; art != nil {
		if p, ok := store.CachedPath(artifactKernel, arch, art); ok {
			cfg.VM.KernelPath = p
		}
	}
	if art := remote.GetVm().GetGuestd()[arch]; art != nil {
		if p, ok := store.CachedPath(artifactGuestd, arch, art); ok {
			cfg.VM.GuestdBin = p
		}
	}
	return cfg
}

// ConfigSyncer reconciles controller-managed WorkerConfig and managed VM
// artifacts (hotbed-guestd, vmlinux) delivered in HeartbeatResponse.
type ConfigSyncer struct {
	WorkerDir string
	Local     statedir.WorkerConfig
	Artifacts *ArtifactStore
	Arch      string // defaults to runtime.GOARCH

	mu         sync.Mutex
	lastRemote *pb.WorkerConfig
	applied    statedir.WorkerConfig
}

// NewConfigSyncer constructs a ConfigSyncer initialized with the worker's
// startup local, remote, and effective configurations.
func NewConfigSyncer(workerDir string, local statedir.WorkerConfig, initialRemote *pb.WorkerConfig, initialEffective statedir.WorkerConfig, artifacts *ArtifactStore) *ConfigSyncer {
	var remCopy *pb.WorkerConfig
	if initialRemote != nil {
		remCopy = proto.Clone(initialRemote).(*pb.WorkerConfig)
	}
	return &ConfigSyncer{
		WorkerDir:  workerDir,
		Local:      local,
		Artifacts:  artifacts,
		Arch:       goruntime.GOARCH,
		lastRemote: remCopy,
		applied:    initialEffective,
	}
}

// Reconcile processes HeartbeatResponse's worker_config and build.guestd.
// Hot-reloadable changes (VM tuning, managed kernel, managed hotbed-guestd,
// default runtime) are applied live to eng. Structural changes are persisted
// to controller-config.json and return needsRestart=true so the caller can
// trigger an idle-deferred re-exec.
func (c *ConfigSyncer) Reconcile(ctx context.Context, eng *Engine, reply *pb.HeartbeatResponse) (needsRestart bool) {
	if c == nil || reply == nil {
		return false
	}
	arch := c.Arch
	if arch == "" {
		arch = goruntime.GOARCH
	}
	remote := reply.GetWorkerConfig()

	c.mu.Lock()
	remoteChanged := !proto.Equal(c.lastRemote, remote)
	curApplied := c.applied
	c.mu.Unlock()

	if remoteChanged {
		if err := SaveControllerConfig(c.WorkerDir, remote); err != nil {
			slog.Warn("save controller worker config failed", "err", err)
		} else {
			c.mu.Lock()
			if remote != nil {
				c.lastRemote = proto.Clone(remote).(*pb.WorkerConfig)
			} else {
				c.lastRemote = nil
			}
			c.mu.Unlock()
			slog.Info("saved controller-managed worker config", "revision", remote.GetRevision())
		}
	}

	desired := MergeWorkerConfig(c.Local, remote)

	// Sync managed VM artifacts (kernel + hotbed-guestd) when VM mode is active
	// (or about to become active).
	if desired.VM != nil && desired.VM.Enabled && c.Artifacts != nil {
		// 1. Managed guest kernel for this arch (from WorkerConfig.vm.kernels[arch]).
		if kArt := remote.GetVm().GetKernels()[arch]; kArt != nil && kArt.GetSha256() != "" {
			if p, err := c.Artifacts.Ensure(ctx, artifactKernel, arch, kArt); err != nil {
				slog.Warn("sync managed VM kernel failed", "arch", arch, "version", kArt.GetVersion(), "err", err)
			} else {
				desired.VM.KernelPath = p
			}
		}

		// 2. Managed hotbed-guestd for this arch: explicit config override wins,
		// otherwise follow the controller's current Build.guestd release artifact.
		gArt := remote.GetVm().GetGuestd()[arch]
		if gArt == nil || gArt.GetSha256() == "" {
			gArt = reply.GetBuild().GetGuestd()
		}
		if gArt != nil && gArt.GetSha256() != "" {
			if p, err := c.Artifacts.Ensure(ctx, artifactGuestd, arch, gArt); err != nil {
				slog.Warn("sync managed hotbed-guestd failed", "arch", arch, "version", gArt.GetVersion(), "err", err)
			} else {
				// Also update default /usr/local/bin/hotbed-guestd best-effort when
				// no custom guestd_bin path is configured, so host tools see it too.
				if desired.VM.GuestdBin == "" || desired.VM.GuestdBin == vm.DefaultGuestdBin {
					_ = syncFileAtomically(p, vm.DefaultGuestdBin, gArt.GetSha256(), 0o755)
				}
				desired.VM.GuestdBin = p
			}
		}
	}

	if RequiresConfigRestart(curApplied, desired) {
		slog.Info("controller worker config requires restart to apply structural changes")
		return true
	}

	if !workerConfigEqual(curApplied, desired) {
		if eng != nil {
			eng.ApplyWorkerConfig(desired)
		}
		c.mu.Lock()
		c.applied = desired
		c.mu.Unlock()
		slog.Info("hot-reloaded controller-managed worker config", "revision", remote.GetRevision())
	}
	return false
}

func workerConfigEqual(a, b statedir.WorkerConfig) bool {
	if a.Slice != b.Slice || !slices.Equal(a.UnitProperties, b.UnitProperties) {
		return false
	}
	if (a.VM == nil) != (b.VM == nil) {
		return false
	}
	if a.VM != nil {
		av, bv := *a.VM, *b.VM
		if av.Enabled != bv.Enabled ||
			av.Default != bv.Default ||
			av.CladefsControlSock != bv.CladefsControlSock ||
			av.CladefsVMSock != bv.CladefsVMSock ||
			av.CladefsPeerAddr != bv.CladefsPeerAddr ||
			av.CellRoot != bv.CellRoot ||
			av.TenantID != bv.TenantID ||
			av.FirecrackerBin != bv.FirecrackerBin ||
			av.KernelPath != bv.KernelPath ||
			av.GuestdBin != bv.GuestdBin ||
			av.VMUser != bv.VMUser ||
			av.VCPUs != bv.VCPUs ||
			av.MemMiB != bv.MemMiB ||
			av.DiskBytes != bv.DiskBytes ||
			av.DAXWindowBytes != bv.DAXWindowBytes ||
			av.DAXPrivateMaxBytes != bv.DAXPrivateMaxBytes ||
			av.DisableDAX != bv.DisableDAX ||
			av.CacheType != bv.CacheType ||
			av.BridgeName != bv.BridgeName ||
			av.BridgeCIDR != bv.BridgeCIDR ||
			!slices.Equal(av.DNSServers, bv.DNSServers) {
			return false
		}
	}
	if (a.CladefsFUSE == nil) != (b.CladefsFUSE == nil) {
		return false
	}
	if a.CladefsFUSE != nil && *a.CladefsFUSE != *b.CladefsFUSE {
		return false
	}
	return true
}
