package worker

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"sync"

	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/cladefs"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/guestd"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/vm"
)

// VMBackend coordinates cladefsd cell management, Firecracker microVMs, peer
// mesh updates (SetPeers), and retired-host decommissioning (DecommissionHost)
// for a hotbed worker running with statedir.VMConfig enabled.
type VMBackend struct {
	cfg         statedir.VMConfig
	store       *cladefs.Store
	mgr         *vm.Manager
	rt          *runtime.GuestRuntime
	incarnation uint64

	mu             sync.Mutex
	baseImageRef   *cladefs.CellRef
	wsCells        map[string]cladefs.CellRef // wsID -> forked cell ref
	sharedBases    map[string]*pb.CladefsCell // key -> uploaded warm base version
	lastPeers      []string
	decommissioned map[uint64]bool
}

// OpenVMBackend connects to cladefsd, initializes the Firecracker VM manager,
// and reconnects to any surviving Firecracker VMs from a prior worker process.
func OpenVMBackend(ctx context.Context, workerDir string, cfg statedir.VMConfig, slice, gitAuthHostSock string) (*VMBackend, error) {
	cellRoot := cmp.Or(cfg.CellRoot, "/mnt/fxdax-scratch/cladefsd/cells/hotbed")
	cstore, err := cladefs.Open(ctx, cladefs.Config{
		ControlSock: cfg.CladefsControlSock,
		VMSock:      cfg.CladefsVMSock,
		CellRoot:    cellRoot,
		TenantID:    cfg.TenantID,
		DefaultCap:  cfg.DiskBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("open cladefs store: %w", err)
	}
	inc, err := cstore.Incarnation(ctx)
	if err != nil {
		slog.Warn("read cladefsd incarnation failed; continuing without ephemeral incarnation", "err", err)
	}
	runDir := filepath.Join(workerDir, "vms")
	mgr, err := vm.NewManager(vm.Config{
		FirecrackerBin:     cfg.FirecrackerBin,
		KernelPath:         cfg.KernelPath,
		GuestdBin:          cfg.GuestdBin,
		RunDir:             runDir,
		Slice:              slice,
		VMUser:             cfg.VMUser,
		DefaultVCPUs:       cfg.VCPUs,
		DefaultMemMiB:      cfg.MemMiB,
		DefaultDiskBytes:   cfg.DiskBytes,
		DAXWindowBytes:     cfg.DAXWindowBytes,
		DAXPrivateMaxBytes: cfg.DAXPrivateMaxBytes,
		BridgeName:         cfg.BridgeName,
		BridgeCIDR:         cfg.BridgeCIDR,
		DNSServers:         cfg.DNSServers,
		GitAuthHostSock:    gitAuthHostSock,
	})
	if err != nil {
		_ = cstore.Close()
		return nil, fmt.Errorf("create Firecracker VM manager: %w", err)
	}
	n, err := mgr.Reconnect(ctx)
	if err != nil {
		slog.Warn("vm reconnect failed", "err", err)
	} else if n > 0 {
		slog.Info("reconnected surviving Firecracker VMs", "count", n)
	}

	b := &VMBackend{
		cfg:            cfg,
		store:          cstore,
		mgr:            mgr,
		incarnation:    inc,
		wsCells:        map[string]cladefs.CellRef{},
		sharedBases:    map[string]*pb.CladefsCell{},
		decommissioned: map[uint64]bool{},
	}
	b.rt = runtime.NewGuestRuntime(b)
	return b, nil
}

// Close closes the cladefsd gRPC connection. Surviving Firecracker VMs remain
// running in systemd units so worker upgrades/restarts do not disrupt them.
func (b *VMBackend) Close() error {
	if b.store != nil {
		return b.store.Close()
	}
	return nil
}

// Runtime returns the GuestRuntime implementing runtime.WorkspaceRuntime.
func (b *VMBackend) Runtime() runtime.WorkspaceRuntime { return b.rt }

// DefaultRuntime returns "vm" if configured as default, else "chroot".
func (b *VMBackend) DefaultRuntime() string {
	if b.cfg.Default {
		return runtime.KindVM
	}
	return runtime.KindChroot
}

// Runtimes returns the runtimes supported by this worker.
func (b *VMBackend) Runtimes() []string {
	return []string{runtime.KindVM, runtime.KindChroot}
}

// HeartbeatMeta returns cladefs peer address, incarnation, and advertised R2
// shared bases for HeartbeatRequest.
func (b *VMBackend) HeartbeatMeta() (peerAddr string, incarnation uint64, sharedBases []*pb.CladefsCell) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, sb := range b.sharedBases {
		sharedBases = append(sharedBases, clone(sb))
	}
	return b.cfg.CladefsPeerAddr, b.incarnation, sharedBases
}

// ApplyHeartbeat reconciles cladefs peer mesh (SetPeers) and retired-host
// decommissioning (DecommissionHost) from HeartbeatResponse.
func (b *VMBackend) ApplyHeartbeat(ctx context.Context, peers []string, decommission []uint64) {
	if b.store == nil {
		return
	}
	if len(peers) > 0 && b.cfg.CladefsPeerAddr != "" {
		sorted := slices.Clone(peers)
		slices.Sort(sorted)
		b.mu.Lock()
		changed := !slices.Equal(b.lastPeers, sorted)
		b.mu.Unlock()
		if changed {
			if err := b.store.SetPeers(ctx, sorted); err != nil {
				slog.Warn("cladefsd SetPeers failed", "peers", sorted, "err", err)
			} else {
				b.mu.Lock()
				b.lastPeers = sorted
				b.mu.Unlock()
				slog.Info("updated cladefsd peer mesh", "peers", sorted)
			}
		}
	}
	for _, inc := range decommission {
		if inc == 0 || inc == b.incarnation {
			continue
		}
		b.mu.Lock()
		done := b.decommissioned[inc]
		b.mu.Unlock()
		if done {
			continue
		}
		retired, left, err := b.store.DecommissionHost(ctx, inc)
		if err != nil {
			slog.Warn("cladefsd DecommissionHost failed", "incarnation", inc, "err", err)
			continue
		}
		slog.Info("decommissioned retired cladefs host", "incarnation", inc, "retired", retired, "left", left)
		if left == 0 {
			b.mu.Lock()
			b.decommissioned[inc] = true
			b.mu.Unlock()
		}
	}
}

// EnsureBaseImage ensures the shared Debian + hotbed-guestd base cell is
// imported, versioned, and synced to R2.
func (b *VMBackend) EnsureBaseImage(ctx context.Context) (cladefs.CellRef, error) {
	b.mu.Lock()
	if b.baseImageRef != nil {
		ref := *b.baseImageRef
		b.mu.Unlock()
		return ref, nil
	}
	b.mu.Unlock()

	rel := "baseimages/guestd-" + goruntime.GOARCH
	// Reuse existing cell if already imported on this host.
	if st, err := b.store.StatCell(ctx, rel); err == nil && st.GetCell() > 0 {
		ver, err := b.store.MakeVersionAndSync(ctx, rel, 2)
		if err == nil {
			hostPath, _ := b.store.CellPath(rel)
			ref := cladefs.CellRef{
				RelPath:  rel,
				HostPath: hostPath,
				Tenant:   st.GetTenant(),
				Cell:     st.GetCell(),
				Version:  ver,
			}
			b.mu.Lock()
			b.baseImageRef = &ref
			b.mu.Unlock()
			return ref, nil
		}
	}

	tarPath := filepath.Join(b.store.CellRoot(), "guestd-"+goruntime.GOARCH+".tar")
	defer os.Remove(tarPath)
	if err := vm.BuildGuestTar(ctx, vm.GuestImageOptions{
		ImageRef:   baseimage.DefaultImage,
		Arch:       goruntime.GOARCH,
		GuestdBin:  cmp.Or(b.cfg.GuestdBin, vm.DefaultGuestdBin),
		DNSServers: b.cfg.DNSServers,
		OutTar:     tarPath,
	}); err != nil {
		return cladefs.CellRef{}, fmt.Errorf("build VM guest base tar: %w", err)
	}
	ref, err := b.store.ImportImage(ctx, rel, tarPath, b.cfg.DiskBytes)
	if err != nil {
		return cladefs.CellRef{}, fmt.Errorf("import VM base image into cladefsd: %w", err)
	}
	b.mu.Lock()
	b.baseImageRef = &ref
	b.mu.Unlock()
	return ref, nil
}

// PrepareWorkspace forks the workspace cell from sharedBase (if provided in R2)
// or the local VM base image, applies fencingToken if non-zero, and boots the
// Firecracker microVM.
func (b *VMBackend) PrepareWorkspace(ctx context.Context, wsID string, sharedBase *pb.CladefsCell, fencingToken uint64) (*pb.CladefsCell, error) {
	var from cladefs.CellRef
	if sharedBase != nil && sharedBase.Cell > 0 && sharedBase.Version > 0 {
		from = cladefs.CellRef{
			Tenant:  sharedBase.Tenant,
			Cell:    sharedBase.Cell,
			Version: sharedBase.Version,
		}
	} else {
		var err error
		from, err = b.EnsureBaseImage(ctx)
		if err != nil {
			return nil, err
		}
	}

	rel := "ws/" + wsID
	wsRef, err := b.store.ForkVersion(ctx, from, rel, false)
	if err != nil {
		return nil, fmt.Errorf("fork cladefs workspace %s: %w", wsID, err)
	}
	if fencingToken > 0 {
		if err := b.store.PlaceCell(ctx, rel, fencingToken); err != nil {
			_ = b.store.DeleteCell(ctx, rel)
			return nil, fmt.Errorf("place cladefs workspace %s with token %d: %w", wsID, fencingToken, err)
		}
	}
	b.mu.Lock()
	b.wsCells[wsID] = wsRef
	b.mu.Unlock()

	if _, err := b.EnsureVM(ctx, wsID); err != nil {
		_ = b.store.DeleteCell(ctx, rel)
		b.mu.Lock()
		delete(b.wsCells, wsID)
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

// DeleteWorkspaceCell deletes the cladefsd cell backing wsID after its VM stops.
func (b *VMBackend) DeleteWorkspaceCell(ctx context.Context, wsID string) error {
	b.mu.Lock()
	delete(b.wsCells, wsID)
	b.mu.Unlock()
	return b.store.DeleteCell(ctx, "ws/"+wsID)
}

// EnsureVM implements runtime.GuestResolver.
func (b *VMBackend) EnsureVM(ctx context.Context, wsID string) (*guestd.Client, error) {
	if c, err := b.mgr.ClientFor(wsID); err == nil {
		return c, nil
	}
	b.mu.Lock()
	ref, ok := b.wsCells[wsID]
	b.mu.Unlock()
	cellPath := ref.HostPath
	if !ok || cellPath == "" {
		var err error
		cellPath, err = b.store.CellPath("ws/" + wsID)
		if err != nil {
			return nil, err
		}
	}
	inst, err := b.mgr.Start(ctx, vm.LaunchSpec{
		ID:            wsID,
		CellHostPath:  cellPath,
		CladefsVMSock: b.store.VMSock(),
	})
	if err != nil {
		return nil, err
	}
	return inst.Client, nil
}

// ClientFor implements runtime.GuestResolver.
func (b *VMBackend) ClientFor(wsID string) (*guestd.Client, error) {
	return b.mgr.ClientFor(wsID)
}

// StopVM implements runtime.GuestResolver.
func (b *VMBackend) StopVM(ctx context.Context, wsID string) error {
	return b.mgr.StopVM(ctx, wsID)
}

// SetGitAuthHostSock wires the worker's live gitAuthServer Unix socket into the
// Firecracker VM manager for guest->host vsock port 5002 bridging.
func (b *VMBackend) SetGitAuthHostSock(sock string) {
	if b.mgr != nil {
		b.mgr.SetGitAuthHostSock(sock)
	}
}

// createVMWorkspace creates a cladefsd + Firecracker microVM workspace directly
// from req.SharedBase (in R2) or the host's imported VM base image cell,
// without touching ZFS/btrfs volumes.
func (e *Engine) createVMWorkspace(ctx context.Context, req *pb.CreateWorkspaceRequest) (*pb.Workspace, error) {
	p := req.Project
	now := e.nowMs()
	baseID := "vm-base"
	if req.GetSharedBase().GetBaseId() != "" {
		baseID = req.GetSharedBase().GetBaseId()
	}
	var repos []*pb.WorkspaceRepo
	for _, r := range p.GetRepos() {
		repos = append(repos, &pb.WorkspaceRepo{Repo: r.GetId()})
	}
	w := &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{
			Id: req.Id, Project: p.Name, BaseId: baseID, NodeId: e.cfg.NodeID, Agent: req.Agent,
			Repos: repos, State: preparing, CreatedAt: now, LastActiveAt: now,
			SessionId: req.SessionId, SyncHost: req.SyncHost, Runtime: runtime.KindVM,
			Reason: "starting Firecracker microVM",
		},
		Volume: "ws/" + req.Id, BaseSnapshot: baseID,
	}}
	e.mu.Lock()
	if _, dup := e.workspaces[req.Id]; dup {
		e.mu.Unlock()
		return nil, fmt.Errorf("workspace %s exists", req.Id)
	}
	w.forking = true
	done := make(chan struct{})
	w.forkDone = done
	e.workspaces[req.Id] = w
	e.saveWorkspace(w, change{what: "created", detail: "cladefs + Firecracker VM"})
	e.mu.Unlock()

	start := e.now()
	cell, err := e.cfg.VM.PrepareWorkspace(ctx, req.Id, req.GetSharedBase(), req.GetFencingToken())
	forkMs := e.now().Sub(start).Milliseconds()

	e.mu.Lock()
	w.forking = false
	w.forkDone = nil
	close(done)
	if err != nil {
		w.ws().State = failedWS
		w.ws().Reason = "could not start Firecracker VM: " + err.Error()
		e.saveWorkspace(w, change{what: "failed", detail: w.ws().Reason})
		e.mu.Unlock()
		return nil, err
	}
	w.ws().State = idle
	w.ws().Reason = ""
	w.ws().ForkMillis = forkMs
	w.ws().CladefsCell = cell
	w.ws().LastActiveAt = e.nowMs()
	e.saveWorkspace(w, change{
		what:   "ready",
		detail: fmt.Sprintf("Firecracker VM ready in %d ms (cell %d/%d)", forkMs, cell.Tenant, cell.Cell),
		feed:   feed(feedFork, fmt.Sprintf("vm %s ready in %d ms · %s", req.Id, forkMs, req.Agent), ref("workspace", req.Id)),
	})
	e.mu.Unlock()
	return e.Workspace(req.Id)
}
