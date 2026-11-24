package worker

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/cladefs"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/guestd"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/vm"
)

// VMBackend coordinates cladefsd cell management, Firecracker microVMs, peer
// mesh updates (SetPeers), retired-host decommissioning (DecommissionHost),
// and R2-backed warm project bases for a hotbed worker with statedir.VMConfig.
type VMBackend struct {
	cfg         statedir.VMConfig
	workerDir   string
	store       *cladefs.Store
	mgr         *vm.Manager
	rt          *runtime.GuestRuntime
	incarnation uint64

	baseMu         sync.Mutex // serializes base image provisioning
	projectLocks   sync.Map   // project -> *sync.Mutex
	mu             sync.Mutex
	baseImageRef   *cladefs.CellRef
	wsCells        map[string]cladefs.CellRef // wsID -> forked cell ref
	projectBases   map[string]*vmProjectBase  // project -> warm project base in R2
	sharedBases    map[string]*pb.CladefsCell // project -> advertised R2 shared base
	lastPeers      []string
	decommissioned map[uint64]bool
}

type vmProjectBase struct {
	Ref    cladefs.CellRef   `json:"ref"`
	BaseID string            `json:"baseId"`
	Recipe string            `json:"recipe"`
	Repos  map[string]string `json:"repos"` // repoID -> checked-out HEAD commit
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
		workerDir:      workerDir,
		store:          cstore,
		mgr:            mgr,
		incarnation:    inc,
		wsCells:        map[string]cladefs.CellRef{},
		projectBases:   map[string]*vmProjectBase{},
		sharedBases:    map[string]*pb.CladefsCell{},
		decommissioned: map[uint64]bool{},
	}
	b.loadCachedBases()
	b.rt = runtime.NewGuestRuntime(b)
	return b, nil
}

func (b *VMBackend) cacheDir() string {
	return filepath.Join(b.workerDir, "vm-bases")
}

func (b *VMBackend) loadCachedBases() {
	ents, err := os.ReadDir(b.cacheDir())
	if err != nil {
		return
	}
	for _, ent := range ents {
		if !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(b.cacheDir(), ent.Name()))
		if err != nil {
			continue
		}
		if strings.HasPrefix(ent.Name(), "baseimage-") {
			var ref cladefs.CellRef
			if json.Unmarshal(raw, &ref) == nil && ref.Cell > 0 && ref.Version > 0 {
				b.baseImageRef = &ref
			}
			continue
		}
		if proj, ok := strings.CutPrefix(ent.Name(), "project-"); ok {
			proj = strings.TrimSuffix(proj, ".json")
			var pbBase vmProjectBase
			if json.Unmarshal(raw, &pbBase) == nil && pbBase.Ref.Cell > 0 && pbBase.Ref.Version > 0 {
				b.projectBases[proj] = &pbBase
				b.sharedBases[proj] = &pb.CladefsCell{
					Project: proj,
					Arch:    goruntime.GOARCH,
					BaseId:  pbBase.BaseID,
					Recipe:  pbBase.Recipe,
					Tenant:  pbBase.Ref.Tenant,
					Cell:    pbBase.Ref.Cell,
					Version: pbBase.Ref.Version,
				}
			}
		}
	}
}

func (b *VMBackend) saveCacheJSON(name string, v any) {
	if b.workerDir == "" {
		return
	}
	if err := os.MkdirAll(b.cacheDir(), 0o755); err != nil {
		return
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return
	}
	p := filepath.Join(b.cacheDir(), name)
	tmp := p + ".tmp"
	if os.WriteFile(tmp, raw, 0o644) == nil {
		_ = os.Rename(tmp, p)
	}
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

// EnsureProvisionedBaseImage ensures the shared provisioned Debian + toolchains
// + hotbed-guestd base cell exists, is versioned, and is synced to R2.
func (b *VMBackend) EnsureProvisionedBaseImage(ctx context.Context, spec baseimage.Spec) (cladefs.CellRef, error) {
	spec = spec.Normalized()
	key := spec.Key()

	b.baseMu.Lock()
	defer b.baseMu.Unlock()

	b.mu.Lock()
	if b.baseImageRef != nil && strings.HasSuffix(b.baseImageRef.RelPath, key) {
		ref := *b.baseImageRef
		b.mu.Unlock()
		return ref, nil
	}
	b.mu.Unlock()

	rel := "baseimages/vm-" + key
	// Clean any interrupted previous build at rel.
	_ = b.store.DeleteCell(ctx, rel)

	tarPath := filepath.Join(b.store.CellRoot(), "vm-"+key+".tar")
	defer os.Remove(tarPath)
	slog.Info("building Firecracker VM base image", "key", key, "image", spec.Image)
	if err := vm.BuildGuestTar(ctx, vm.GuestImageOptions{
		ImageRef:   spec.Image,
		Arch:       spec.Arch,
		GuestdBin:  cmp.Or(b.cfg.GuestdBin, vm.DefaultGuestdBin),
		DNSServers: b.cfg.DNSServers,
		OutTar:     tarPath,
	}); err != nil {
		return cladefs.CellRef{}, fmt.Errorf("build VM guest base tar: %w", err)
	}
	rawRef, err := b.store.ImportImage(ctx, rel, tarPath, b.cfg.DiskBytes)
	if err != nil {
		return cladefs.CellRef{}, fmt.Errorf("import VM base image into cladefsd: %w", err)
	}

	// Boot a temporary Firecracker VM on rawRef to run DefaultProvisionScript
	// (installs git, gh, build-essential, rustup/cargo, tracers, non-root user).
	if spec.Script != "" {
		provID := "prov-" + key
		inst, err := b.mgr.Start(ctx, vm.LaunchSpec{
			ID:            provID,
			CellHostPath:  rawRef.HostPath,
			CladefsVMSock: b.store.VMSock(),
		})
		if err != nil {
			_ = b.store.DeleteCell(ctx, rel)
			return cladefs.CellRef{}, fmt.Errorf("boot provisioning VM %s: %w", provID, err)
		}
		_ = inst
		_, runErr := b.runGuestCommand(ctx, provID, "provision", "/", []string{"/bin/bash", "-c", spec.Script}, []string{
			"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			"HOME=/root",
		}, 0, 0)
		stopErr := b.mgr.Stop(ctx, provID, true)
		if runErr != nil {
			_ = b.store.DeleteCell(ctx, rel)
			return cladefs.CellRef{}, fmt.Errorf("provision VM base image %s: %w", key, runErr)
		}
		if stopErr != nil {
			_ = b.store.DeleteCell(ctx, rel)
			return cladefs.CellRef{}, fmt.Errorf("stop provisioning VM %s: %w", provID, stopErr)
		}
	}

	ver, err := b.store.MakeVersionAndSync(ctx, rel, 2)
	if err != nil {
		_ = b.store.DeleteCell(ctx, rel)
		return cladefs.CellRef{}, fmt.Errorf("version & sync VM base image %s to R2: %w", key, err)
	}
	rawRef.Version = ver
	b.mu.Lock()
	b.baseImageRef = &rawRef
	b.mu.Unlock()
	b.saveCacheJSON("baseimage-"+key+".json", rawRef)
	slog.Info("VM base image provisioned and synced to R2", "key", key, "cell", rawRef.Cell, "version", rawRef.Version)
	return rawRef, nil
}

func vmProjectRecipe(baseKey string, p *pb.HostedProject) string {
	h := sha256.New()
	fmt.Fprintf(h, "vm-v1\nbase=%s\narch=%s\nproject=%s\nepoch=%d\n", baseKey, goruntime.GOARCH, p.GetName(), p.GetScratchEpoch())
	for _, r := range p.GetRepos() {
		fmt.Fprintf(h, "repo=%s|%s|%s\n", r.GetId(), r.GetUrl(), r.GetBranch())
	}
	for _, s := range p.GetSteps() {
		fmt.Fprintf(h, "step=%s|%s|%v\n", s.GetName(), s.GetHash(), s.GetEveryPull())
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (b *VMBackend) projectLock(name string) *sync.Mutex {
	mu, _ := b.projectLocks.LoadOrStore(name, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// EnsureProjectBase builds (or reuses from local cache / R2 shared_bases) a
// warm cladefs project base with all project repos cloned at /work/<repo> and
// setup steps executed inside a Firecracker VM, then uploads it to R2 and
// publishes it in sharedBases for heartbeat advertisement.
func (b *VMBackend) EnsureProjectBase(
	ctx context.Context,
	p *pb.HostedProject,
	existingShared *pb.CladefsCell,
	spec baseimage.Spec,
	sandboxEnv []string,
) (*vmProjectBase, error) {
	spec = spec.Normalized()
	recipe := vmProjectRecipe(spec.Key(), p)

	lock := b.projectLock(p.GetName())
	lock.Lock()
	defer lock.Unlock()

	// Fast path 1: local cached project base matches recipe.
	b.mu.Lock()
	if cur, ok := b.projectBases[p.GetName()]; ok && cur.Recipe == recipe && cur.Ref.Cell > 0 && cur.Ref.Version > 0 {
		out := *cur
		b.mu.Unlock()
		return &out, nil
	}
	b.mu.Unlock()

	// Fast path 2: controller advertised an up-to-date shared base in R2!
	if existingShared != nil && existingShared.Cell > 0 && existingShared.Version > 0 && existingShared.Recipe == recipe {
		return &vmProjectBase{
			Ref: cladefs.CellRef{
				Tenant:  existingShared.Tenant,
				Cell:    existingShared.Cell,
				Version: existingShared.Version,
			},
			BaseID: cmp.Or(existingShared.BaseId, p.GetName()+"-vm-"+recipe[:8]),
			Recipe: recipe,
			Repos:  map[string]string{},
		}, nil
	}

	// Build project base from provisioned VM base image (or previous project base if not scratch).
	baseImgRef, err := b.EnsureProvisionedBaseImage(ctx, spec)
	if err != nil {
		return nil, err
	}

	baseID := fmt.Sprintf("%s-vm-%s", p.GetName(), recipe[:8])
	rel := filepath.Join("bases", p.GetName(), baseID)
	_ = b.store.DeleteCell(ctx, rel)

	forkRef, err := b.store.ForkVersion(ctx, baseImgRef, rel, false)
	if err != nil {
		return nil, fmt.Errorf("fork VM project base %s: %w", baseID, err)
	}

	vmID := "base-" + baseID
	if _, err := b.mgr.Start(ctx, vm.LaunchSpec{
		ID:            vmID,
		CellHostPath:  forkRef.HostPath,
		CladefsVMSock: b.store.VMSock(),
	}); err != nil {
		_ = b.store.DeleteCell(ctx, rel)
		return nil, fmt.Errorf("boot project base VM %s: %w", vmID, err)
	}

	env := append([]string{}, cacheEnv()...)
	env = append(env, sandboxEnv...)
	env = append(env,
		"HOME="+spec.Home,
		"PATH="+strings.Join(cachePath, ":")+":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOTBED_GIT_CRED_SOCK=/run/hotbed/git-auth/git-cred.sock",
	)

	// 1. Clone/fetch all project repos inside the Firecracker VM.
	var cloneScript strings.Builder
	cloneScript.WriteString("set -euo pipefail\nmkdir -p /work\ncd /work\n")
	for _, r := range p.GetRepos() {
		branch := cmp.Or(r.GetBranch(), "main")
		fmt.Fprintf(&cloneScript, `
if [ -d %q/.git ]; then
  git -C %q fetch --prune origin %q
  git -C %q checkout -B %q FETCH_HEAD
else
  git clone --branch %q --single-branch %q %q
fi
printf '::hotbed-head:: %s %%s\n' "$(git -C %q rev-parse HEAD)"
`, r.GetId(), r.GetId(), branch, r.GetId(), branch, branch, r.GetUrl(), r.GetId(), r.GetId(), r.GetId())
	}

	out, runErr := b.runGuestCommand(ctx, vmID, "freshen-repos-"+baseID, "/work", []string{"/bin/bash", "-c", cloneScript.String()}, env, spec.UID, spec.GID)
	// 2. Run each project setup step directly via bash -c (avoiding nested
	// shell quoting so variables like $dest expand inside the step's shell).
	if runErr == nil {
		for i, s := range p.GetSteps() {
			stepCmdID := fmt.Sprintf("freshen-step-%d-%s", i+1, baseID)
			if _, err := b.runGuestCommand(ctx, vmID, stepCmdID, "/work", []string{"/bin/bash", "-eo", "pipefail", "-c", s.GetScript()}, env, spec.UID, spec.GID); err != nil {
				runErr = fmt.Errorf("step %q: %w", s.GetName(), err)
				break
			}
		}
	}
	stopErr := b.mgr.Stop(ctx, vmID, true)
	if runErr != nil {
		_ = b.store.DeleteCell(ctx, rel)
		return nil, fmt.Errorf("freshen VM project base %s: %w", baseID, runErr)
	}
	if stopErr != nil {
		_ = b.store.DeleteCell(ctx, rel)
		return nil, fmt.Errorf("stop project base VM %s: %w", vmID, stopErr)
	}

	ver, err := b.store.MakeVersionAndSync(ctx, rel, 2)
	if err != nil {
		_ = b.store.DeleteCell(ctx, rel)
		return nil, fmt.Errorf("sync VM project base %s to R2: %w", baseID, err)
	}
	forkRef.Version = ver

	repoHeads := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "::hotbed-head:: "); ok {
			parts := strings.Fields(rest)
			if len(parts) == 2 {
				repoHeads[parts[0]] = parts[1]
			}
		}
	}

	res := &vmProjectBase{
		Ref:    forkRef,
		BaseID: baseID,
		Recipe: recipe,
		Repos:  repoHeads,
	}
	b.mu.Lock()
	b.projectBases[p.GetName()] = res
	b.sharedBases[p.GetName()] = &pb.CladefsCell{
		Project: p.GetName(),
		Arch:    goruntime.GOARCH,
		BaseId:  baseID,
		Recipe:  recipe,
		Tenant:  forkRef.Tenant,
		Cell:    forkRef.Cell,
		Version: forkRef.Version,
	}
	b.mu.Unlock()
	b.saveCacheJSON("project-"+p.GetName()+".json", res)
	slog.Info("VM project base built and published to R2", "project", p.GetName(), "baseId", baseID, "cell", forkRef.Cell, "version", forkRef.Version)
	return res, nil
}

// PrepareWorkspace forks the workspace cell from baseRef, applies fencingToken
// if non-zero, and boots the Firecracker microVM.
func (b *VMBackend) PrepareWorkspace(ctx context.Context, wsID string, baseRef cladefs.CellRef, fencingToken uint64) (*pb.CladefsCell, error) {
	rel := "ws/" + wsID
	wsRef, err := b.store.ForkVersion(ctx, baseRef, rel, false)
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
// from a warm R2 project base (freshening and publishing it to R2 on first use),
// without touching ZFS/btrfs volumes.
func (e *Engine) createVMWorkspace(ctx context.Context, req *pb.CreateWorkspaceRequest) (*pb.Workspace, error) {
	p := req.Project
	now := e.nowMs()
	var repos []*pb.WorkspaceRepo
	for _, r := range p.GetRepos() {
		repos = append(repos, &pb.WorkspaceRepo{Repo: r.GetId()})
	}
	w := &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{
			Id: req.Id, Project: p.Name, BaseId: "vm-base", NodeId: e.cfg.NodeID, Agent: req.Agent,
			Repos: repos, State: preparing, CreatedAt: now, LastActiveAt: now,
			SessionId: req.SessionId, SyncHost: req.SyncHost, Runtime: runtime.KindVM,
			Reason: "preparing Firecracker VM base",
		},
		Volume: "ws/" + req.Id, BaseSnapshot: "vm-base",
	}}
	e.mu.Lock()
	if _, dup := e.workspaces[req.Id]; dup {
		e.mu.Unlock()
		return nil, fmt.Errorf("workspace %s exists", req.Id)
	}
	w.forking = true
	done := make(chan struct{})
	w.forkDone = done
	e.vmWorkspaces.Store(req.Id, struct{}{})
	e.workspaces[req.Id] = w
	e.saveWorkspace(w, change{what: "created", detail: "cladefs + Firecracker VM"})
	e.mu.Unlock()

	finishVM := func(bgCtx context.Context) {
		start := e.now()
		var baseSpec baseimage.Spec
		if e.cfg.BaseImage != nil {
			baseSpec = e.cfg.BaseImage.Spec
		}
		projBase, err := e.cfg.VM.EnsureProjectBase(bgCtx, p, req.GetSharedBase(), baseSpec, e.sandboxEnv())
		var cell *pb.CladefsCell
		if err == nil {
			cell, err = e.cfg.VM.PrepareWorkspace(bgCtx, req.Id, projBase.Ref, req.GetFencingToken())
		}
		forkMs := e.now().Sub(start).Milliseconds()

		e.mu.Lock()
		w.forking = false
		w.forkDone = nil
		close(done)
		if w.ws().State == released || w.releasing {
			e.mu.Unlock()
			if err == nil {
				_ = e.cfg.VM.StopVM(context.Background(), req.Id)
				_ = e.cfg.VM.DeleteWorkspaceCell(context.Background(), req.Id)
			}
			return
		}
		if err != nil {
			w.ws().State = failedWS
			w.ws().Reason = "could not start Firecracker VM: " + err.Error()
			e.saveWorkspace(w, change{what: "failed", detail: w.ws().Reason})
			for _, c := range e.commands {
				if c.cmd().WorkspaceId == w.ws().Id && c.cmd().State == queued {
					e.end(c, failed, nil, "its workspace failed: "+w.ws().Reason)
				}
			}
			e.mu.Unlock()
			return
		}
		w.ws().BaseId = projBase.BaseID
		w.rec.BaseSnapshot = projBase.BaseID
		for _, r := range w.ws().Repos {
			if head, ok := projBase.Repos[r.Repo]; ok && head != "" {
				r.Head = head
				r.BaseCommit = head
			}
		}
		w.ws().State = idle
		if e.workspaceBusy(w.ws().Id) {
			w.ws().State = activeWS
		}
		w.ws().Reason = ""
		w.ws().ForkMillis = forkMs
		w.ws().CladefsCell = cell
		w.ws().LastActiveAt = e.nowMs()
		e.saveWorkspace(w, change{
			what:   "ready",
			detail: fmt.Sprintf("Firecracker VM ready in %d ms (cell %d/%d)", forkMs, cell.Tenant, cell.Cell),
			feed:   feed(feedFork, fmt.Sprintf("vm %s ready in %d ms · %s", req.Id, forkMs, req.Agent), ref("workspace", req.Id)),
		})
		e.stepSoon()
		e.mu.Unlock()
	}

	// Run VM base provisioning/freshening + boot asynchronously so cold base
	// builds never hit CreateWorkspace's RPC timeout; WatchWorkspace notifies
	// callers the instant the VM transitions from PREPARING to IDLE.
	e.mu.Lock()
	started := e.startBgLocked(finishVM)
	e.mu.Unlock()
	if !started {
		e.mu.Lock()
		w.forking = false
		w.forkDone = nil
		close(done)
		w.ws().State = failedWS
		w.ws().Reason = "worker is shutting down"
		e.mu.Unlock()
		return nil, fmt.Errorf("worker is shutting down")
	}
	return e.Workspace(req.Id)
}
