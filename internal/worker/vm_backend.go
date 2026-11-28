package worker

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/user"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	machinedv1 "github.com/semistrict/cladefs/go/machined/v1"
	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/cladefs"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitauth"
	"github.com/semistrict/hotbed/internal/guestd"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/storage"
	"github.com/semistrict/hotbed/internal/unit"
	"github.com/semistrict/hotbed/internal/vm"
	"google.golang.org/grpc"
)

// VMBackend is the cladefs backend whose workspaces each run in a microVM on
// their cell: Firecracker's under the host cladefsd, or machined's (whose
// embedded cladefs daemon holds the cells) when vm.machinedSock is set. It
// builds project bases in VMs too, and is the runtime.GuestResolver its
// GuestRuntime reaches each VM's hotbed-guestd by.
type VMBackend struct {
	cladefsCore
	mgr *vm.Manager
	// machined is the connection to machined when its machines run the
	// VMs (cfg.MachinedSock), nil under Firecracker.
	machined *grpc.ClientConn
	rt       *runtime.GuestRuntime

	mu           sync.Mutex
	cfg          statedir.VMConfig
	wsCells      map[string]cladefs.CellRef // wsID -> forked cell ref
	wsCapture    map[string]bool            // wsID -> true while cell is a bucket-backed uncaptured cell on cladefsd
	activeBuilds map[string]struct{}        // in-flight base-* / prov-* VM IDs owned by this worker
	// writes is what the VMs wrote, for VolumeWrites.
	writes vmWrites
	// boots serializes EnsureVM per VM (ID -> *sync.Mutex): a VM is down
	// between commands, and callers that find it so at once wait for one
	// boot rather than race it.
	boots sync.Map
}

// newVMBackend is a VMBackend for cfg keeping its state in workerDir, with
// no store or VM manager yet.
func newVMBackend(workerDir string, cfg statedir.VMConfig) *VMBackend {
	b := &VMBackend{
		cfg:          cfg,
		wsCells:      map[string]cladefs.CellRef{},
		wsCapture:    map[string]bool{},
		activeBuilds: map[string]struct{}{},
	}
	b.cladefsCore = newCladefsCore(vmFlavor, b, workerDir)
	b.rt = runtime.NewGuestRuntime(b)
	return b
}

func resolveVMUserIDs(vmUser string) (uid, gid uint32) {
	name := cmp.Or(vmUser, vm.DefaultVMUser)
	if name == "" {
		return 0, 0
	}
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0
	}
	uid64, uerr := strconv.ParseUint(u.Uid, 10, 32)
	gid64, gerr := strconv.ParseUint(u.Gid, 10, 32)
	if uerr != nil || gerr != nil {
		return 0, 0
	}
	return uint32(uid64), uint32(gid64)
}

// firecrackerStoreConfig is how a Firecracker node reaches the host
// cladefsd holding its VMs' cells.
func firecrackerStoreConfig(cfg statedir.VMConfig) cladefs.Config {
	vmUID, vmGID := resolveVMUserIDs(cfg.VMUser)
	return cladefs.Config{
		ControlSock: cfg.CladefsControlSock,
		VMSock:      cfg.CladefsVMSock,
		CellRoot:    tenantCellRoot(cmp.Or(cfg.CellRoot, defaultCellRoot), cfg.TenantID),
		TenantID:    cfg.TenantID,
		VMUID:       vmUID,
		VMGID:       vmGID,
		DefaultCap:  cfg.DiskBytes,
	}
}

// OpenVMBackend connects to the cladefs daemon holding the VMs' cells,
// initializes the VM manager, and reconnects to any VMs that survived a prior
// worker process: Firecracker's under cladefsd, or machined's (and the cladefs
// daemon it embeds) when cfg.MachinedSock is set.
func OpenVMBackend(ctx context.Context, workerDir string, cfg statedir.VMConfig, slice, gitAuthHostSock string) (*VMBackend, error) {
	var (
		cladefsCfg cladefs.Config
		vmmOpts    []vm.ManagerOption
		machined   *grpc.ClientConn
		metricsURL string
	)
	if cfg.MachinedSock != "" {
		if cfg.MachinedImages == "" {
			return nil, api.Errorf(api.CodeInvalidArgs, "vm.machinedSock is set without vm.machinedImages, machined's --images")
		}
		conn, err := vm.DialMachined(cfg.MachinedSock)
		if err != nil {
			return nil, err
		}
		machined = conn
		// The image tars ImportImage reads are written in the worker's own
		// directory: machined runs as the worker's user, on its machine.
		cladefsCfg = cladefs.Config{
			ControlSock: cfg.MachinedSock,
			VMSock:      cfg.MachinedSock,
			CellRoot:    filepath.Join(workerDir, "cells"),
			DefaultCap:  cfg.DiskBytes,
		}
		vmmOpts = append(vmmOpts, vm.WithVMM(vm.NewMachined(machinedv1.NewMachinedClient(conn), cfg.MachinedImages)))
		cfg = machinedVMConfig(cfg)
	} else {
		if goruntime.GOOS != "linux" {
			return nil, api.Errorf(api.CodePlatformNotSupported, "Firecracker VMs need Linux: on %s, set vm.machinedSock to run them on machined", goruntime.GOOS)
		}
		cladefsCfg = firecrackerStoreConfig(cfg)
		metricsURL = hostCladefsdMetricsURL
	}
	closeMachined := func() {
		if machined != nil {
			_ = machined.Close()
		}
	}
	cstore, err := cladefs.Open(ctx, cladefsCfg)
	if err != nil {
		closeMachined()
		return nil, fmt.Errorf("open cladefs store: %w", err)
	}
	inc, err := cstore.Incarnation(ctx)
	if err != nil {
		slog.Warn("read cladefsd incarnation failed; continuing without ephemeral incarnation", "err", err)
	}
	mgr, err := vm.NewManager(vm.Config{
		FirecrackerBin:     cfg.FirecrackerBin,
		KernelPath:         cfg.KernelPath,
		GuestdBin:          cfg.GuestdBin,
		RunDir:             filepath.Join(workerDir, "vms"),
		Slice:              slice,
		BenchSlice:         BenchSlice(slice),
		VMUser:             cfg.VMUser,
		DefaultVCPUs:       cfg.VCPUs,
		DefaultMemMiB:      cfg.MemMiB,
		DefaultDiskBytes:   cfg.DiskBytes,
		DAXWindowBytes:     cfg.DAXWindowBytes,
		DAXPrivateMaxBytes: cfg.DAXPrivateMaxBytes,
		DisableDAX:         cfg.DisableDAX,
		CacheType:          cfg.CacheType,
		BridgeName:         cfg.BridgeName,
		BridgeCIDR:         cfg.BridgeCIDR,
		DNSServers:         cfg.DNSServers,
		Nofile:             cfg.Nofile,
		GitAuthHostSock:    gitAuthHostSock,
	}, vmmOpts...)
	if err != nil {
		_ = cstore.Close()
		closeMachined()
		return nil, fmt.Errorf("create VM manager: %w", err)
	}
	n, err := mgr.Reconnect(ctx)
	if err != nil {
		slog.Warn("vm reconnect failed", "err", err)
	} else if n > 0 {
		slog.Info("reconnected surviving VMs", "count", n)
	}

	_ = ensureDirTraversable(workerDir)
	b := newVMBackend(workerDir, cfg)
	b.mgr = mgr
	b.machined = machined
	b.link = cladefsdLink{incarnation: inc}
	b.metricsURL = metricsURL
	b.store.Store(cstore)
	b.loadCachedBases()
	return b, nil
}

// machinedVMConfig is cfg as it holds on machined, whatever the cluster's
// config says for Firecracker nodes: machined answers its guests' DNS on
// their gateway, and its embedded daemon's cells are the default tenant's.
// Its machines have no DAX window until a fault in one stops hanging the
// guest on Hypervisor.framework (cladefs board 14210).
func machinedVMConfig(cfg statedir.VMConfig) statedir.VMConfig {
	cfg.DNSServers = []string{vm.MachinedGateway}
	cfg.TenantID = 0
	cfg.DisableDAX = true
	return cfg
}

func (b *VMBackend) configSnapshot() statedir.VMConfig {
	b.mu.Lock()
	defer b.mu.Unlock()
	cfg := b.cfg
	if cfg.DNSServers != nil {
		cfg.DNSServers = slices.Clone(cfg.DNSServers)
	}
	return cfg
}

func (b *VMBackend) settings() cladefsSettings {
	cfg := b.configSnapshot()
	return cladefsSettings{tenant: cfg.TenantID, peerAddr: cfg.CladefsPeerAddr}
}

// UpdateConfig hot-reloads VM tuning, kernel path, guestd binary path, DNS,
// peer address, and default-runtime flag for all subsequent VM boots and base
// builds without disrupting running VMs.
func (b *VMBackend) UpdateConfig(cfg statedir.VMConfig) {
	if b.machined != nil {
		cfg = machinedVMConfig(cfg)
	}
	b.mu.Lock()
	if cfg.DNSServers != nil {
		cfg.DNSServers = slices.Clone(cfg.DNSServers)
	}
	b.cfg = cfg
	b.mu.Unlock()
	if b.mgr != nil {
		b.mgr.UpdateConfig(vm.Config{
			FirecrackerBin:     cfg.FirecrackerBin,
			KernelPath:         cfg.KernelPath,
			GuestdBin:          cfg.GuestdBin,
			VMUser:             cfg.VMUser,
			DefaultVCPUs:       cfg.VCPUs,
			DefaultMemMiB:      cfg.MemMiB,
			DefaultDiskBytes:   cfg.DiskBytes,
			DAXWindowBytes:     cfg.DAXWindowBytes,
			DAXPrivateMaxBytes: cfg.DAXPrivateMaxBytes,
			DisableDAX:         cfg.DisableDAX,
			CacheType:          cfg.CacheType,
			DNSServers:         cfg.DNSServers,
			Nofile:             cfg.Nofile,
		})
	}
}

func (b *VMBackend) applyConfig(cfg statedir.WorkerConfig) {
	if cfg.VM != nil {
		b.UpdateConfig(*cfg.VM)
	}
}

// Close closes the cladefsd gRPC connection. Surviving Firecracker VMs remain
// running in systemd units so worker upgrades/restarts do not disrupt them.
// The store pointer stays non-nil (closed) so any racing goroutine receives a
// clean gRPC error rather than panicking on a nil pointer.
func (b *VMBackend) Close() error {
	var err error
	if st := b.currentStore(); st != nil {
		err = st.Close()
	}
	if b.machined != nil {
		err = errors.Join(err, b.machined.Close())
	}
	return err
}

// OnMachined reports whether machined runs the VMs, and the cladefs daemon
// it embeds holds their cells.
func (b *VMBackend) OnMachined() bool { return b.machined != nil }

// IsLocalOnly reports whether this VMBackend keeps all cells strictly on local
// disk without uploading to R2 (machined).
func (b *VMBackend) IsLocalOnly() bool { return b.OnMachined() }

// HeartbeatMeta omits cladefs peer and shared-base metadata on machined:
// machined keeps every cell strictly on local disk with no R2 bucket, so
// reporting held bases to the controller would hand local-only cell IDs to
// other arm64 workers (which then stall trying to fetch them from a remote
// bucket that does not exist).
func (b *VMBackend) HeartbeatMeta() (peerAddr string, incarnation uint64, held []*pb.SharedBase, retired []*pb.CladefsCell) {
	if b.IsLocalOnly() {
		return "", 0, nil, nil
	}
	return b.cladefsCore.HeartbeatMeta()
}

// ApplyHeartbeat skips shared peer-mesh and shared-bucket retirement on
// machined, keeping only local base retirement sweeps.
func (b *VMBackend) ApplyHeartbeat(ctx context.Context, peers []string, decommission []uint64, retire []*pb.CladefsCell) {
	if b.IsLocalOnly() {
		if st := b.currentStore(); st != nil {
			b.retirer.sweep(ctx, st, time.Now())
		}
		return
	}
	b.cladefsCore.ApplyHeartbeat(ctx, peers, decommission, retire)
}

// Runtime returns the GuestRuntime implementing runtime.WorkspaceRuntime.
func (b *VMBackend) Runtime() runtime.WorkspaceRuntime { return b.rt }

// runGuestCommand runs a command inside a booted VM, streams its output, and
// returns its combined output and error if exit code != 0.
func (b *VMBackend) runGuestCommand(ctx context.Context, vmID, cmdID, dir string, argv, env []string, uid, gid int) (string, error) {
	if err := b.rt.StartCommand(ctx, vmID, runtime.CommandSpec{
		ID:   cmdID,
		Argv: argv,
		Dir:  dir,
		Env:  env,
		UID:  uid,
		GID:  gid,
	}); err != nil {
		return "", err
	}
	var out bytes.Buffer
	if _, err := b.rt.StreamLogs(ctx, vmID, cmdID, 0, true, &out); err != nil {
		return out.String(), err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		stMap, err := b.rt.StatusMany(ctx, vmID, []string{cmdID})
		if err != nil {
			return out.String(), err
		}
		if st, ok := stMap[cmdID]; ok && st.Exited {
			if st.VMExit != nil {
				return out.String(), fmt.Errorf("guest command %s: %s", cmdID, vmExitReason(*st.VMExit))
			}
			if st.ExitCode != 0 {
				tail := out.String()
				if len(tail) > 2048 {
					tail = tail[len(tail)-2048:]
				}
				return out.String(), fmt.Errorf("guest command %s exited %d:\n%s", cmdID, st.ExitCode, strings.TrimSpace(tail))
			}
			return out.String(), nil
		}
		if time.Now().After(deadline) {
			return out.String(), fmt.Errorf("timed out waiting for guest command %s status", cmdID)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// baseKey identifies the OS-level VM rootfs base cell. hotbed-guestd is
// attached at VM boot time via a separate read-only virtio-blk tools disk, so
// guestd upgrades never invalidate OS or project VM bases.
func (b *VMBackend) baseKey(spec baseimage.Spec) string {
	cfg := b.configSnapshot()
	h := sha256.New()
	fmt.Fprintf(h, "disk=%d\ndns=%s\n", cfg.DiskBytes, strings.Join(cfg.DNSServers, ","))
	return spec.Key() + "-" + hex.EncodeToString(h.Sum(nil))[:8]
}

// trackBuild marks build VM vmID as this worker's, so ReconcileOrphans
// leaves it be, until the returned untrack.
func (b *VMBackend) trackBuild(vmID string) (untrack func()) {
	b.mu.Lock()
	if b.activeBuilds == nil {
		b.activeBuilds = map[string]struct{}{}
	}
	b.activeBuilds[vmID] = struct{}{}
	b.mu.Unlock()
	return sync.OnceFunc(func() {
		b.mu.Lock()
		delete(b.activeBuilds, vmID)
		b.mu.Unlock()
	})
}

func (b *VMBackend) stopBuildVM(ctx context.Context, vmID string, syncFS bool) error {
	timeout := 20 * time.Second
	if syncFS {
		// Allow full guest syscall.Sync() (SyncShutdownTimeout = 3m) plus
		// poweroff and systemd unit cleanup before aborting.
		timeout = vm.DefaultSyncShutdownTimeout + 30*time.Second
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	return b.mgr.Stop(sctx, vmID, syncFS)
}

// startBuildVM boots build VM vmID on cell.
func (b *VMBackend) startBuildVM(ctx context.Context, vmID string, cell cladefs.CellRef) error {
	_, err := b.mgr.Start(ctx, vm.LaunchSpec{
		ID:            vmID,
		CellHostPath:  cell.CellID(),
		CladefsVMSock: b.currentStore().VMSock(),
	})
	return err
}

// takeWithUploadInterval re-opens cell, synced to the bucket, with
// uploadInterval so cladefsd coalesces writes on local SSD until
// MakeVersionAndSync flushes them (question 534). It deletes the cell should
// that fail.
func (b *VMBackend) takeWithUploadInterval(ctx context.Context, cell cladefs.CellRef, uploadInterval time.Duration) (cladefs.CellRef, error) {
	st := b.currentStore()
	if err := st.DropCell(ctx, cell.Tenant, cell.Cell); err != nil {
		b.cleanupCell(ctx, cell.Tenant, cell.Cell)
		return cladefs.CellRef{}, fmt.Errorf("drop cell %d before take: %w", cell.Cell, err)
	}
	taken, err := st.TakeCell(ctx, cell.Tenant, cell.Cell, b.configSnapshot().DiskBytes, nil, uploadInterval)
	if err != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		if derr := st.DeleteBucketCell(cctx, cell.Tenant, cell.Cell); derr != nil {
			slog.Warn("delete a cell that could not be taken for a build", "cell", cell.Cell, "err", derr)
		}
		cancel()
		return cladefs.CellRef{}, fmt.Errorf("re-open cell %d with upload interval: %w", cell.Cell, err)
	}
	return taken, nil
}

func (b *VMBackend) takeForBuild(ctx context.Context, cell cladefs.CellRef) (cladefs.CellRef, error) {
	return b.takeWithUploadInterval(ctx, cell, baseBuildUploadInterval)
}

// provisionBaseImage imports spec's OS image into a new cell and, when spec
// has a provisioning script, runs it (installing git, gh, build-essential,
// rustup/cargo, tracers and the non-root user) in a VM booted on the cell.
func (b *VMBackend) provisionBaseImage(ctx context.Context, key string, spec baseimage.Spec, _ baseimage.Provisioner) (cladefs.CellRef, error) {
	cfg := b.configSnapshot()
	provID := "prov-" + key
	if b.mgr != nil {
		if err := b.mgr.EnsureNotRunning(ctx, provID); err != nil {
			return cladefs.CellRef{}, fmt.Errorf("prepare VM base image %s: %w", key, err)
		}
	}

	tarPath := filepath.Join(b.currentStore().PickRoot(), "vm-"+key+".tar")
	defer os.Remove(tarPath)
	if err := vm.BuildGuestTar(ctx, vm.GuestImageOptions{
		ImageRef:   spec.Image,
		Arch:       spec.Arch,
		DNSServers: cfg.DNSServers,
		OutTar:     tarPath,
	}); err != nil {
		return cladefs.CellRef{}, fmt.Errorf("build VM guest base tar: %w", err)
	}
	ref, err := b.currentStore().ImportImage(ctx, tarPath, cfg.DiskBytes)
	if err != nil {
		return cladefs.CellRef{}, fmt.Errorf("import VM base image into cladefsd: %w", err)
	}
	if spec.Script == "" {
		return ref, nil
	}

	if ref, err = b.takeForBuild(ctx, ref); err != nil {
		return cladefs.CellRef{}, fmt.Errorf("provision VM base image %s: %w", key, err)
	}
	untrack := b.trackBuild(provID)
	defer untrack()
	if err := b.startBuildVM(ctx, provID, ref); err != nil {
		b.cleanupCell(ctx, ref.Tenant, ref.Cell)
		return cladefs.CellRef{}, fmt.Errorf("boot provisioning VM %s: %w", provID, err)
	}
	_, runErr := b.runGuestCommand(ctx, provID, "provision", "/", []string{"/bin/bash", "-c", spec.Script}, []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
	}, 0, 0)
	stopErr := b.stopBuildVM(ctx, provID, runErr == nil)
	if runErr != nil {
		if stopErr == nil {
			b.cleanupCell(ctx, ref.Tenant, ref.Cell)
		}
		return cladefs.CellRef{}, fmt.Errorf("provision VM base image %s: %w", key, runErr)
	}
	if stopErr != nil {
		return cladefs.CellRef{}, fmt.Errorf("stop provisioning VM %s: %w", provID, stopErr)
	}
	return ref, nil
}

func buildVMID(baseID string) string { return "base-" + baseID }

func (b *VMBackend) prepareBaseBuild(ctx context.Context, baseID string) error {
	if b.mgr == nil {
		return nil
	}
	return b.mgr.EnsureNotRunning(ctx, buildVMID(baseID))
}

// openBaseBuild syncs fork's head, takes it with baseBuildUploadInterval and
// readies a VM to boot on it once the build's freshen is admitted.
func (b *VMBackend) openBaseBuild(ctx context.Context, fork cladefs.CellRef, t baseBuildTarget) (baseBuildCell, error) {
	if err := b.currentStore().SyncCell(ctx, fork.Tenant, fork.Cell); err != nil {
		b.cleanupCell(ctx, fork.Tenant, fork.Cell)
		return nil, fmt.Errorf("sync forked cell %d: %w", fork.Cell, err)
	}
	taken, err := b.takeForBuild(ctx, fork)
	if err != nil {
		return nil, err
	}
	vmID := buildVMID(t.baseID)
	return &vmBaseBuild{b: b, cell: taken, vmID: vmID, home: t.spec.Home, untrack: b.trackBuild(vmID)}, nil
}

// vmBaseBuild is a project base build in a VM booted on its cell when its
// freshen is admitted.
type vmBaseBuild struct {
	b       *VMBackend
	cell    cladefs.CellRef
	vmID    string
	home    string
	untrack func()
	started atomic.Bool // the VM is up

	mu               sync.Mutex
	startFailStopErr error // stopping the VM after its freshen failed to start
	bootStartMs      int64
	bootEndMs        int64
	bootSecs         float64

	// stopErr is how stopping the VM failed in finish: it may hold the cell
	// still, which then cannot be deleted.
	stopErr error
}

func (v *vmBaseBuild) ref() cladefs.CellRef { return v.cell }

func (v *vmBaseBuild) freshen(spec *managedFreshenSpec) {
	spec.Env = append(spec.Env,
		"HOME="+v.home,
		"HOTBED_GIT_CRED_SOCK="+gitauth.ChrootDir+"/git-cred.sock",
	)
	spec.VMID = v.vmID
	spec.BeforeStart = func(ctx context.Context) error {
		t0 := time.Now()
		if err := v.b.startBuildVM(ctx, v.vmID, v.cell); err != nil {
			return fmt.Errorf("boot project base VM %s: %w", v.vmID, err)
		}
		v.mu.Lock()
		v.bootStartMs, v.bootEndMs, v.bootSecs = t0.UnixMilli(), time.Now().UnixMilli(), time.Since(t0).Seconds()
		v.mu.Unlock()
		v.started.Store(true)
		return nil
	}
	spec.OnStartFail = func() {
		if v.started.Swap(false) {
			err := v.b.stopBuildVM(context.Background(), v.vmID, false)
			v.mu.Lock()
			v.startFailStopErr = err
			v.mu.Unlock()
		}
	}
}

// finish stops the build VM, its guest's filesystem synced when ran.
func (v *vmBaseBuild) finish(ctx context.Context, h *managedFreshenHandle, ran bool) (float64, error) {
	defer v.untrack()
	v.mu.Lock()
	bootStartMs, bootEndMs, bootSecs, stopErr := v.bootStartMs, v.bootEndMs, v.bootSecs, v.startFailStopErr
	v.mu.Unlock()
	if bootStartMs > 0 {
		h.RecordPhase("vm_boot", bootStartMs, bootEndMs, bootSecs, v.vmID)
	}
	var finalizeSecs float64
	if v.started.Swap(false) {
		if ran {
			h.SetFinalizing("vm_stop")
		}
		stopStart := time.Now()
		stopErr = v.b.stopBuildVM(ctx, v.vmID, ran)
		stopSecs := time.Since(stopStart).Seconds()
		h.RecordPhase("vm_stop", stopStart.UnixMilli(), time.Now().UnixMilli(), stopSecs, "")
		if ran {
			finalizeSecs = stopSecs
		}
	}
	v.stopErr = stopErr
	if stopErr != nil {
		return finalizeSecs, fmt.Errorf("stop project base VM %s: %w", v.vmID, stopErr)
	}
	return finalizeSecs, nil
}

// discard deletes the cell, unless its VM may not have let it go.
func (v *vmBaseBuild) discard(ctx context.Context) {
	v.untrack()
	if v.stopErr == nil {
		v.b.cleanupCell(ctx, v.cell.Tenant, v.cell.Cell)
	}
}

// PrepareWorkspace forks a workspace cell from baseRef and boots its microVM.
// Unless captureBase is true (on an R2-backed daemon), the fork is ephemeral:
// reads come from R2 through the local SSD CLADEFSD_PART_CACHE while writes
// stay strictly local-only (0 R2 bytes). When captureBase is true on cladefsd,
// the fork is bucket-backed with captureUploadInterval (24h) so writes remain
// local during execution and only flush to R2 if CaptureWorkspaceBase promotes it.
func (b *VMBackend) PrepareWorkspace(ctx context.Context, wsID string, baseRef cladefs.CellRef, fencingToken uint64, captureBase bool) (*pb.CladefsCell, error) {
	if b.mgr != nil {
		if err := b.mgr.EnsureNotRunning(ctx, wsID); err != nil {
			return nil, fmt.Errorf("prepare ephemeral cladefs workspace %s: %w: %v", wsID, storage.ErrBusy, err)
		}
	}
	b.mu.Lock()
	staleRef, hadStale := b.wsCells[wsID]
	if hadStale {
		delete(b.wsCells, wsID)
		delete(b.wsCapture, wsID)
	}
	b.mu.Unlock()
	if hadStale && staleRef.Cell > 0 {
		if err := b.currentStore().DeleteCell(ctx, staleRef.Tenant, staleRef.Cell); err != nil {
			return nil, fmt.Errorf("clear stale cladefs workspace cell %s: %w", wsID, err)
		}
	}
	// An ephemeral cell is one cladefsd never uploads to the bucket, kept
	// in its ephemeral cell directory. machined's embedded daemon keeps
	// every cell on its local disk, uploading none, and has no such
	// directory: a plain fork is the same there.
	bucketBacked := captureBase && !b.OnMachined()
	ephemeral := !b.OnMachined() && !bucketBacked
	wsRef, err := b.currentStore().ForkVersion(ctx, baseRef, ephemeral)
	if err != nil {
		return nil, fmt.Errorf("fork cladefs workspace %s: %w", wsID, err)
	}
	if bucketBacked {
		if err := b.currentStore().SyncCell(ctx, wsRef.Tenant, wsRef.Cell); err != nil {
			b.cleanupCell(ctx, wsRef.Tenant, wsRef.Cell)
			return nil, fmt.Errorf("sync capture workspace cell %s: %w", wsID, err)
		}
		wsRef, err = b.takeWithUploadInterval(ctx, wsRef, captureUploadInterval)
		if err != nil {
			return nil, fmt.Errorf("open capture workspace cell %s: %w", wsID, err)
		}
	}
	b.mu.Lock()
	if b.wsCells == nil {
		b.wsCells = map[string]cladefs.CellRef{}
	}
	if b.wsCapture == nil {
		b.wsCapture = map[string]bool{}
	}
	b.wsCells[wsID] = wsRef
	if bucketBacked {
		b.wsCapture[wsID] = true
	}
	b.mu.Unlock()

	if _, err := b.EnsureVM(ctx, wsID, unit.VMBoot{}); err != nil {
		b.cleanupCell(ctx, wsRef.Tenant, wsRef.Cell)
		b.mu.Lock()
		delete(b.wsCells, wsID)
		delete(b.wsCapture, wsID)
		b.mu.Unlock()
		return nil, err
	}
	return &pb.CladefsCell{
		Tenant:       wsRef.Tenant,
		Cell:         wsRef.Cell,
		Version:      wsRef.Version,
		FencingToken: fencingToken,
		Arch:         goruntime.GOARCH,
	}, nil
}

// CaptureWorkspaceBase versions wsID's stopped VM cell as project's new warm
// base, holds it on this node, and (on R2-backed cladefsd) drops the local
// bucket-backed copy and swaps wsID onto an ephemeral fork of the new version
// so subsequent commands in wsID incur zero R2 traffic.
func (b *VMBackend) CaptureWorkspaceBase(
	ctx context.Context,
	wsID, project, baseKey, setupHash string,
	stepHashes map[string]string,
	scratchEpoch int64,
	repos map[string]string,
	baseCommitTime, capturedAt int64,
) (*projectBase, *pb.CladefsCell, error) {
	b.mu.Lock()
	cellRef, ok := b.wsCells[wsID]
	b.mu.Unlock()
	if !ok || cellRef.Cell == 0 {
		return nil, nil, fmt.Errorf("capture workspace %s: no cladefs cell recorded", wsID)
	}
	ver, err := b.versionAndSync(ctx, cellRef)
	if err != nil {
		return nil, nil, fmt.Errorf("version & sync workspace %s: %w", wsID, err)
	}
	baseRef := cellRef
	baseRef.Version = ver
	baseID := fmt.Sprintf("%s-%s-cap-%s", project, b.baseIDTag, wsID)
	held := b.heldBases().hold(project, &projectBase{
		Ref:            baseRef,
		BaseID:         baseID,
		BaseKey:        baseKey,
		SetupHash:      setupHash,
		ScratchEpoch:   scratchEpoch,
		Repos:          maps.Clone(repos),
		StepHashes:     maps.Clone(stepHashes),
		BaseCommitTime: baseCommitTime,
		CapturedAt:     capturedAt,
		AutoCaptured:   true,
	}, true, true)

	// On R2-backed cladefsd, free the local bucket-backed copy now that its
	// version is durable in R2; on machined (local-only), the local cell IS
	// the base so keep it intact. In both cases, fork wsID onto its own child
	// cell so subsequent commands in wsID upload nothing to R2 and releasing
	// wsID later deletes only its child cell, never the held base cell.
	if !b.OnMachined() {
		if err := b.currentStore().DropCell(ctx, baseRef.Tenant, baseRef.Cell); err != nil {
			slog.Warn("drop local copy of captured workspace base", "workspace", wsID, "cell", baseRef.Cell, "err", err)
		}
	}
	ephemRef, err := b.currentStore().ForkVersion(ctx, baseRef, !b.OnMachined())
	if err != nil {
		b.mu.Lock()
		delete(b.wsCapture, wsID)
		// Never leave wsCells[wsID] pointing at the held base cell, or
		// DeleteWorkspace would delete the newly captured base on release.
		delete(b.wsCells, wsID)
		b.mu.Unlock()
		return held, nil, fmt.Errorf("fork workspace %s after base capture: %w", wsID, err)
	}
	b.mu.Lock()
	b.wsCells[wsID] = ephemRef
	delete(b.wsCapture, wsID)
	b.mu.Unlock()
	return held, &pb.CladefsCell{
		Tenant:  ephemRef.Tenant,
		Cell:    ephemRef.Cell,
		Version: ephemRef.Version,
		Arch:    goruntime.GOARCH,
	}, nil
}

// RestoreWorkspaceCell records the cladefs cell a workspace's saved record
// names, so a worker that restarted can boot that workspace's VM again. The
// record is the lasting copy; wsCells only caches it for this incarnation.
func (b *VMBackend) RestoreWorkspaceCell(wsID string, cell *pb.CladefsCell, captureBase bool) {
	if cell.GetCell() == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.wsCells[wsID] = cladefs.CellRef{
		HostPath: cladefs.FormatCellID(cell.GetTenant(), cell.GetCell()),
		Tenant:   cell.GetTenant(),
		Cell:     cell.GetCell(),
		Version:  cell.GetVersion(),
	}
	if captureBase && !b.OnMachined() {
		if b.wsCapture == nil {
			b.wsCapture = map[string]bool{}
		}
		b.wsCapture[wsID] = true
	}
}

// ReconcileOrphans stops orphaned Firecracker VMs (including prior worker
// incarnations' base/prov build VMs and zombie units whose RunDir was lost),
// releases orphaned TAPs, and cleans up orphaned local cladefs cells.
func (b *VMBackend) ReconcileOrphans(ctx context.Context, liveWorkspaces map[string]bool, startup bool) {
	if b.mgr == nil {
		return
	}
	owned := make(map[string]bool, len(liveWorkspaces))
	for id := range liveWorkspaces {
		owned[id] = true
	}
	if !startup {
		b.mu.Lock()
		for id := range b.activeBuilds {
			owned[id] = true
		}
		b.mu.Unlock()
	}

	reapedIDs, reapedCellPaths := b.mgr.ReconcileOrphans(ctx, owned)
	st := b.currentStore()
	if st == nil {
		return
	}

	handledCells := make(map[[2]uint64]bool)
	for _, id := range reapedIDs {
		if !vm.IsEphemeralBuildID(id) {
			b.mu.Lock()
			ref, ok := b.wsCells[id]
			delete(b.wsCells, id)
			b.mu.Unlock()
			if ok && ref.Cell > 0 {
				handledCells[[2]uint64{ref.Tenant, ref.Cell}] = true
				if err := st.DeleteCell(ctx, ref.Tenant, ref.Cell); err != nil {
					slog.Warn("delete cladefs cell for reaped orphan VM failed", "vm", id, "tenant", ref.Tenant, "cell", ref.Cell, "err", err)
				}
			}
		}
	}
	for _, cellPath := range reapedCellPaths {
		if cellPath == "" {
			continue
		}
		tenant, cell, ok := cladefs.ParseCellID(cellPath)
		if !ok || cell == 0 || handledCells[[2]uint64{tenant, cell}] {
			continue
		}
		handledCells[[2]uint64{tenant, cell}] = true
		// Drop local state (safe for both interrupted base builds and bucket cells);
		// for host-local ephemeral cells where DropCell is unsupported, delete them.
		if err := st.DropCell(ctx, tenant, cell); err != nil {
			if derr := st.DeleteCell(ctx, tenant, cell); derr != nil {
				slog.Warn("clean up cladefs cell for reaped orphan VM failed", "cell", cellPath, "dropErr", err, "deleteErr", derr)
			}
		}
	}
}

// DeleteWorkspace deletes the cladefsd rootfs cell backing wsID after its
// VM stops.
func (b *VMBackend) DeleteWorkspace(ctx context.Context, wsID string) error {
	b.mu.Lock()
	ref, ok := b.wsCells[wsID]
	delete(b.wsCells, wsID)
	delete(b.wsCapture, wsID)
	b.mu.Unlock()
	if !ok || ref.Cell == 0 {
		if b.mgr != nil {
			if inst := b.mgr.Get(wsID); inst != nil {
				if t, c, parsed := cladefs.ParseCellID(inst.CellPath); parsed && c > 0 {
					ref.Tenant, ref.Cell = t, c
				}
			}
		}
	}
	if ref.Cell == 0 {
		return nil
	}
	return b.currentStore().DeleteCell(ctx, ref.Tenant, ref.Cell)
}

// EnsureVM implements runtime.GuestResolver.
func (b *VMBackend) EnsureVM(ctx context.Context, wsID string, want unit.VMBoot) (*guestd.Client, error) {
	boot, _ := b.boots.LoadOrStore(wsID, &sync.Mutex{})
	boot.(*sync.Mutex).Lock()
	defer boot.(*sync.Mutex).Unlock()
	if c, err := b.mgr.ClientFor(wsID); err == nil {
		st, serr := b.mgr.Status(ctx, wsID)
		switch {
		case serr == nil && !st.Running:
			// Tracked Firecracker process already exited (e.g. exit 158 after
			// cladefsd restart): clean up its stale state and boot a fresh VM
			// on the cell.
			_ = b.mgr.Stop(ctx, wsID, false)
		case b.bootDiffers(wsID, want):
			// Booted otherwise before anything ran in it (to sync a
			// checkout, say): boot it again as asked.
			if err := b.mgr.Stop(ctx, wsID, true); err != nil {
				return nil, fmt.Errorf("stop VM %s to boot it at %d vCPUs, %d MiB%s: %w", wsID, want.VCPUs, want.MemMiB, benchSuffix(want), err)
			}
		default:
			return c, nil
		}
	}
	b.mu.Lock()
	ref, ok := b.wsCells[wsID]
	b.mu.Unlock()
	cellPath := ""
	if ok {
		cellPath = ref.CellID()
	}
	if cellPath == "" {
		if inst := b.mgr.Get(wsID); inst != nil {
			cellPath = inst.CellPath
		}
	}
	if cellPath == "" {
		return nil, fmt.Errorf("vm %s: no cladefs cell recorded for workspace", wsID)
	}
	inst, err := b.mgr.Start(ctx, vm.LaunchSpec{
		ID:            wsID,
		CellHostPath:  cellPath,
		CladefsVMSock: b.currentStore().VMSock(),
		VCPUs:         want.VCPUs,
		MemMiB:        want.MemMiB,
		BenchCPUs:     want.BenchCPUs,
	})
	if err != nil {
		return nil, err
	}
	return inst.Client, nil
}

// BootSince is how the VM of workspace wsID booted, when it booted at or
// after since: a command's start that booted it says how long that took.
func (b *VMBackend) BootSince(wsID string, since time.Time) (vm.BootTimes, bool) {
	inst := b.mgr.Get(wsID)
	if inst == nil || inst.StartedAt.Before(since) || inst.Boot.Total() == 0 {
		return vm.BootTimes{}, false
	}
	return inst.Boot, true
}

// MeasuredVM says whether wsID's VM, if it runs, was booted for a
// measurement (unit.VMBoot.BenchCPUs).
func (b *VMBackend) MeasuredVM(wsID string) bool {
	if b.mgr == nil {
		return false
	}
	inst := b.mgr.Get(wsID)
	return inst != nil && inst.BenchCPUs != ""
}

// bootDiffers reports whether wsID's running VM booted otherwise than a
// non-zero field of boot asks: another size, or not on its measurement CPUs.
func (b *VMBackend) bootDiffers(wsID string, boot unit.VMBoot) bool {
	inst := b.mgr.Get(wsID)
	if inst == nil {
		return false
	}
	return (boot.VCPUs > 0 && inst.VCPUs != boot.VCPUs) || (boot.MemMiB > 0 && inst.MemMiB != boot.MemMiB) ||
		(boot.BenchCPUs != "" && inst.BenchCPUs != boot.BenchCPUs)
}

// benchSuffix is how boot's measurement CPUs read after its size: "" for
// none.
func benchSuffix(boot unit.VMBoot) string {
	if boot.BenchCPUs == "" {
		return ""
	}
	return " on measurement CPUs " + boot.BenchCPUs
}

// ClientFor implements runtime.GuestResolver.
func (b *VMBackend) ClientFor(wsID string) (*guestd.Client, error) {
	return b.mgr.ClientFor(wsID)
}

// VMExit implements runtime.VMExitReporter: how wsID's Firecracker process
// ended, as systemd saw it, or false while it runs (or its state cannot be
// read).
func (b *VMBackend) VMExit(ctx context.Context, wsID string) (unit.VMExit, bool) {
	if b.mgr.Get(wsID) == nil {
		return unit.VMExit{Gone: true}, true
	}
	st, err := b.mgr.Status(ctx, wsID)
	if err != nil {
		slog.Warn("read a VM's state", "workspace", wsID, "err", err)
		return unit.VMExit{}, false
	}
	if st.Running {
		return unit.VMExit{}, false
	}
	return unit.VMExit{ExitCode: st.ExitCode, Signal: st.Signal, Result: st.Result, OOMKilled: st.OOMKilled, Detail: st.Detail}, true
}

// StopVM implements runtime.GuestResolver.
func (b *VMBackend) StopVM(ctx context.Context, wsID string) error {
	return b.mgr.StopVM(ctx, wsID)
}

// ConsoleTail returns the last maxBytes of wsID's VM console log (trimmed to
// whole lines), or "" when unavailable. Used to attach host-side kernel/hang
// context when guestd inside the VM stops answering.
func (b *VMBackend) ConsoleTail(wsID string, maxBytes int) string {
	if b == nil || wsID == "" || maxBytes <= 0 {
		return ""
	}
	var logPath string
	if b.mgr != nil {
		if inst := b.mgr.Get(wsID); inst != nil {
			logPath = inst.ConsoleLog
		}
	}
	if logPath == "" && b.workerDir != "" {
		logPath = filepath.Join(b.workerDir, "vms", wsID, "console.log")
	}
	if logPath == "" {
		return ""
	}
	f, err := os.Open(logPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() <= 0 {
		return ""
	}
	start := max(int64(0), st.Size()-int64(maxBytes))
	buf := make([]byte, st.Size()-start)
	n, err := f.ReadAt(buf, start)
	if n == 0 {
		return ""
	}
	s := strings.ReplaceAll(string(buf[:n]), "\r\n", "\n")
	if start > 0 {
		if idx := strings.IndexByte(s, '\n'); idx >= 0 {
			s = s[idx+1:]
		}
	}
	return strings.TrimSpace(s)
}

// CurrentKernelPath returns the active guest kernel path for new VM boots.
func (b *VMBackend) CurrentKernelPath() string {
	return cmp.Or(b.configSnapshot().KernelPath, vm.DefaultKernelPath)
}

// SetKernelPath hot-reloads the guest kernel path for all subsequent VM boots.
func (b *VMBackend) SetKernelPath(p string) {
	cfg := b.configSnapshot()
	cfg.KernelPath = p
	b.UpdateConfig(cfg)
}

// StopAllForUpgrade gracefully stops and syncs every tracked Firecracker VM
// prior to a disruptive cladefsd restart. It keeps the cladefsd connection
// open: gRPC dials the restarted daemon again by itself, so the worker keeps
// working even when ReconnectCladefs gives up before the daemon answers.
func (b *VMBackend) StopAllForUpgrade(ctx context.Context) error {
	var firstErr error
	if b.mgr != nil {
		for _, id := range b.mgr.List() {
			if err := b.mgr.Stop(ctx, id, true); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		// Also reap any untracked zombie Firecracker units so cladefsd restarts cleanly.
		b.mgr.ReconcileOrphans(ctx, nil)
	}
	b.mu.Lock()
	b.wsCells = map[string]cladefs.CellRef{}
	b.mu.Unlock()
	return firstErr
}

// ReconnectCladefs waits for cladefsd to come back online after a restart and
// swaps in a fresh gRPC client, noting the new incarnation; the next
// heartbeat applies the peer mesh to it.
func (b *VMBackend) ReconnectCladefs(ctx context.Context) (uint64, error) {
	return b.reconnectStore(ctx, firecrackerStoreConfig(b.configSnapshot()), func(ctx context.Context, s *cladefs.Store) (uint64, error) {
		return s.Incarnation(ctx)
	}, nil)
}

// SetGitAuthHostSock wires the worker's live git-auth Unix socket into the
// Firecracker VM manager for guest->host vsock port 5002 bridging.
func (b *VMBackend) SetGitAuthHostSock(sock string) {
	if b.mgr != nil {
		b.mgr.SetGitAuthHostSock(sock)
	}
}
