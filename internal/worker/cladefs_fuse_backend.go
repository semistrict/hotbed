package worker

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"

	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/cladefs"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/unit"
)

// CladefsFUSEBackend manages R2-backed cladefs cells mounted into the host
// filesystem via cladefsd's host FUSE mount (CLADEFSD_MOUNT) for nodes that do
// not run Firecracker microVMs (e.g. 16 KiB arm64 Apple Silicon or QEMU VMs).
//
// Shared base images and project bases are versioned and synced to R2 and then
// dropped locally (DropCell), so local SSD holds only CLADEFSD_PART_CACHE and
// active workspace WALs. Workspaces run standard systemd chroot units against
// their mounted FUSE rootfs, leaving the host's local ZFS/btrfs store intact
// for --runtime chroot comparisons and --bench measurements.
type CladefsFUSEBackend struct {
	cfg             statedir.CladefsFUSEConfig
	workerDir       string
	mountRoot       string
	mountStoresRoot string
	store           *cladefs.Store
	incarnation     uint64

	baseMu       sync.Mutex
	projectLocks sync.Map // project name -> *sync.Mutex

	mu             sync.Mutex
	lastPeers      []string
	decommissioned map[uint64]bool
	baseImageRef   *cladefs.CellRef
	projectBases   map[string]*vmProjectBase // project -> latest cached R2 base
	sharedBases    map[string]*pb.CladefsCell
	wsMounts       map[string]fuseWSMount // wsID -> active FUSE mount
}

type fuseWSMount struct {
	Tenant    uint64 `json:"tenant"`
	Cell      uint64 `json:"cell"`
	MountPath string `json:"mountPath"`
}

// OpenCladefsFUSEBackend connects to cladefsd, loads cached R2 base refs and
// surviving workspace FUSE mounts, and verifies the Mount service is serving.
func OpenCladefsFUSEBackend(ctx context.Context, workerDir string, cfg statedir.CladefsFUSEConfig) (*CladefsFUSEBackend, error) {
	cellRoot := cmp.Or(cfg.CellRoot, "/mnt/fxdax-scratch/cladefsd/cells/hotbed")
	mountRoot := cmp.Or(cfg.MountRoot, "/var/lib/cladefs/mnt")
	mountStoresRoot := cmp.Or(cfg.MountStoresRoot, "/mnt/fxdax-scratch/cladefsd/mount-stores")
	cstore, err := cladefs.Open(ctx, cladefs.Config{
		ControlSock: cfg.CladefsControlSock,
		CellRoot:    cellRoot,
		TenantID:    cfg.TenantID,
		DefaultCap:  cmp.Or(cfg.DiskBytes, uint64(64<<30)),
	})
	if err != nil {
		return nil, fmt.Errorf("open cladefs store: %w", err)
	}
	if _, err := cstore.MountList(ctx); err != nil {
		_ = cstore.Close()
		return nil, fmt.Errorf("cladefsd Mount service unavailable (set CLADEFSD_MOUNT and CLADEFSD_MOUNT_STORES): %w", err)
	}
	inc, err := cstore.Incarnation(ctx)
	if err != nil {
		slog.Warn("read cladefsd incarnation failed; continuing", "err", err)
	}
	b := &CladefsFUSEBackend{
		cfg:             cfg,
		workerDir:       workerDir,
		mountRoot:       mountRoot,
		mountStoresRoot: mountStoresRoot,
		store:           cstore,
		incarnation:     inc,
		decommissioned:  map[uint64]bool{},
		projectBases:    map[string]*vmProjectBase{},
		sharedBases:     map[string]*pb.CladefsCell{},
		wsMounts:        map[string]fuseWSMount{},
	}
	b.loadState()
	return b, nil
}

func (b *CladefsFUSEBackend) Close() error {
	if b.store != nil {
		return b.store.Close()
	}
	return nil
}

func (b *CladefsFUSEBackend) cacheDir() string {
	return filepath.Join(b.workerDir, "cladefs-fuse-bases")
}

func (b *CladefsFUSEBackend) wsStatePath() string {
	return filepath.Join(b.workerDir, "cladefs-fuse-ws.json")
}

func (b *CladefsFUSEBackend) mountDir(tenant, cell uint64) string {
	return filepath.Join(b.mountRoot, cladefs.Base32ID(tenant), cladefs.Base32ID(cell))
}

func (b *CladefsFUSEBackend) mountStoreDir(tenant, cell uint64) string {
	return filepath.Join(b.mountStoresRoot, cladefs.Base32ID(tenant), cladefs.Base32ID(cell))
}

func (b *CladefsFUSEBackend) loadState() {
	if ents, err := os.ReadDir(b.cacheDir()); err == nil {
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
	if raw, err := os.ReadFile(b.wsStatePath()); err == nil {
		var saved map[string]fuseWSMount
		if json.Unmarshal(raw, &saved) == nil {
			b.wsMounts = saved
		}
	}
}

func (b *CladefsFUSEBackend) saveCacheJSON(name string, v any) {
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

func (b *CladefsFUSEBackend) saveWSStateLocked() {
	if b.workerDir == "" {
		return
	}
	raw, err := json.MarshalIndent(b.wsMounts, "", "  ")
	if err != nil {
		return
	}
	p := b.wsStatePath()
	tmp := p + ".tmp"
	if os.WriteFile(tmp, raw, 0o644) == nil {
		_ = os.Rename(tmp, p)
	}
}

// DefaultRuntime returns "cladefs" if configured as default, else "chroot".
func (b *CladefsFUSEBackend) DefaultRuntime() string {
	if b.cfg.Default {
		return runtime.KindCladefs
	}
	return runtime.KindChroot
}

// Runtimes returns the runtimes supported by this host-FUSE cladefs worker.
func (b *CladefsFUSEBackend) Runtimes() []string {
	return []string{runtime.KindCladefs, runtime.KindChroot}
}

// MountPath returns the mounted host FUSE rootfs path for a workspace volume
// ("ws/<id>") if it belongs to CladefsFUSEBackend.
func (b *CladefsFUSEBackend) MountPath(vol string) (string, bool) {
	wsID, ok := strings.CutPrefix(vol, "ws/")
	if !ok || wsID == "" {
		return "", false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	m, found := b.wsMounts[wsID]
	if !found || m.MountPath == "" {
		return "", false
	}
	return m.MountPath, true
}

// HeartbeatMeta returns cladefs peer address, incarnation, and advertised R2
// shared bases for HeartbeatRequest.
func (b *CladefsFUSEBackend) HeartbeatMeta() (peerAddr string, incarnation uint64, sharedBases []*pb.CladefsCell) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, sb := range b.sharedBases {
		sharedBases = append(sharedBases, clone(sb))
	}
	return b.cfg.CladefsPeerAddr, b.incarnation, sharedBases
}

// ApplyHeartbeat reconciles cladefs peer mesh (SetPeers) and retired-host
// decommissioning (DecommissionHost) from HeartbeatResponse.
func (b *CladefsFUSEBackend) ApplyHeartbeat(ctx context.Context, peers []string, decommission []uint64) {
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

func (b *CladefsFUSEBackend) fuseBaseKey(spec baseimage.Spec) string {
	h := sha256.New()
	fmt.Fprintf(h, "fuse-v1\nbase=%s\ndisk=%d\n", spec.Key(), cmp.Or(b.cfg.DiskBytes, uint64(64<<30)))
	return spec.Key() + "-f" + hex.EncodeToString(h.Sum(nil))[:8]
}

func fuseProjectRecipe(baseKey string, p *pb.HostedProject) string {
	h := sha256.New()
	fmt.Fprintf(h, "fuse-v1\nbase=%s\narch=%s\nproject=%s\nepoch=%d\n", baseKey, goruntime.GOARCH, p.GetName(), p.GetScratchEpoch())
	for _, r := range p.GetRepos() {
		fmt.Fprintf(h, "repo=%s|%s|%s\n", r.GetId(), r.GetUrl(), r.GetBranch())
	}
	for _, s := range p.GetSteps() {
		fmt.Fprintf(h, "step=%s|%s|%v\n", s.GetName(), s.GetHash(), s.GetEveryPull())
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (b *CladefsFUSEBackend) projectLock(name string) *sync.Mutex {
	mu, _ := b.projectLocks.LoadOrStore(name, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// EnsureProvisionedBaseImage ensures the shared provisioned rootfs base cell
// exists in R2 for host-FUSE nodes, dropping the local cell dir after R2 sync.
func (b *CladefsFUSEBackend) EnsureProvisionedBaseImage(ctx context.Context, spec baseimage.Spec, prov baseimage.Provisioner) (cladefs.CellRef, error) {
	spec = spec.Normalized()
	key := b.fuseBaseKey(spec)

	b.baseMu.Lock()
	defer b.baseMu.Unlock()

	b.mu.Lock()
	if b.baseImageRef != nil && strings.HasSuffix(b.baseImageRef.RelPath, key) {
		ref := *b.baseImageRef
		b.mu.Unlock()
		return ref, nil
	}
	b.mu.Unlock()

	capBytes := cmp.Or(b.cfg.DiskBytes, uint64(64<<30))
	slog.Info("building cladefs host-FUSE base image", "key", key, "image", spec.Image)
	cellID, shown, err := b.store.MountAttach(ctx, b.cfg.TenantID, 0, capBytes, nil)
	if err != nil {
		return cladefs.CellRef{}, fmt.Errorf("create cladefs FUSE base cell: %w", err)
	}
	if !shown {
		_ = b.store.MountDetach(ctx, b.cfg.TenantID, cellID, true)
		return cladefs.CellRef{}, fmt.Errorf("cladefs FUSE base cell %d not shown", cellID)
	}
	mntDir := b.mountDir(b.cfg.TenantID, cellID)
	storeDir := b.mountStoreDir(b.cfg.TenantID, cellID)
	cleanupOnErr := func() {
		_ = b.store.MountDetach(context.Background(), b.cfg.TenantID, cellID, true)
		_ = b.store.DeleteCellAbs(context.Background(), storeDir)
	}

	if err := baseimage.PullOCI(ctx, spec.Image, spec.Arch, mntDir); err != nil {
		cleanupOnErr()
		return cladefs.CellRef{}, fmt.Errorf("pull OCI into cladefs FUSE base %s: %w", key, err)
	}
	if spec.Script != "" && prov != nil {
		if err := prov.Provision(ctx, "fuse-"+key, mntDir, spec.Script); err != nil {
			cleanupOnErr()
			return cladefs.CellRef{}, fmt.Errorf("provision cladefs FUSE base %s: %w", key, err)
		}
	}
	if err := b.store.MountDetach(ctx, b.cfg.TenantID, cellID, false); err != nil {
		cleanupOnErr()
		return cladefs.CellRef{}, fmt.Errorf("detach cladefs FUSE base %s: %w", key, err)
	}
	ver, err := b.store.MakeVersionAndSyncAbs(ctx, storeDir, 2)
	if err != nil {
		_ = b.store.DeleteCellAbs(context.Background(), storeDir)
		return cladefs.CellRef{}, fmt.Errorf("version & sync cladefs FUSE base %s to R2: %w", key, err)
	}
	_ = b.store.DropCellAbs(ctx, storeDir)

	ref := cladefs.CellRef{
		RelPath: "baseimages/fuse-" + key,
		Tenant:  b.cfg.TenantID,
		Cell:    cellID,
		Version: ver,
	}
	b.mu.Lock()
	b.baseImageRef = &ref
	b.mu.Unlock()
	b.saveCacheJSON("baseimage-"+key+".json", ref)
	slog.Info("cladefs host-FUSE base image synced to R2", "key", key, "cell", cellID, "version", ver)
	return ref, nil
}

// forkAndMount forks an R2 version into a new bucket cell, syncs its head so
// Kind::Fxfs is visible to Mount.Attach, drops the staging dir, and mounts it
// inside cladefsd's host FUSE mount.
func (b *CladefsFUSEBackend) forkAndMount(ctx context.Context, from cladefs.CellRef, stageRel string, fencingToken uint64) (tenant, cell uint64, mountPath, storeDir string, err error) {
	_ = b.store.DropCell(ctx, stageRel)
	forkRef, err := b.store.ForkVersion(ctx, from, stageRel, false)
	if err != nil {
		return 0, 0, "", "", fmt.Errorf("fork R2 version %d/%x: %w", from.Cell, from.Version, err)
	}
	if err := b.store.SyncCell(ctx, stageRel); err != nil {
		_ = b.store.DeleteCell(context.Background(), stageRel)
		return 0, 0, "", "", fmt.Errorf("sync forked cell %d head: %w", forkRef.Cell, err)
	}
	if err := b.store.DropCell(ctx, stageRel); err != nil {
		return 0, 0, "", "", fmt.Errorf("drop staging dir %s: %w", stageRel, err)
	}
	var tok *uint64
	if fencingToken > 0 {
		t := fencingToken
		tok = &t
	}
	attachedCell, shown, err := b.store.MountAttach(ctx, forkRef.Tenant, forkRef.Cell, 0, tok)
	if err != nil {
		return 0, 0, "", "", fmt.Errorf("mount attach forked cell %d: %w", forkRef.Cell, err)
	}
	if !shown {
		_ = b.store.MountDetach(context.Background(), forkRef.Tenant, attachedCell, true)
		return 0, 0, "", "", fmt.Errorf("forked cell %d attached with shown=false (head kind missing)", attachedCell)
	}
	return forkRef.Tenant, attachedCell, b.mountDir(forkRef.Tenant, attachedCell), b.mountStoreDir(forkRef.Tenant, attachedCell), nil
}

// EnsureProjectBase builds or reuses a warm R2 project base for host-FUSE nodes.
func (b *CladefsFUSEBackend) EnsureProjectBase(
	ctx context.Context,
	p *pb.HostedProject,
	existingShared *pb.CladefsCell,
	spec baseimage.Spec,
	prov baseimage.Provisioner,
	runStep func(ctx context.Context, rootDir, name string, argv, env []string) (string, error),
	sandboxEnv []string,
) (*vmProjectBase, error) {
	spec = spec.Normalized()
	baseKey := b.fuseBaseKey(spec)
	recipe := fuseProjectRecipe(baseKey, p)

	lock := b.projectLock(p.GetName())
	lock.Lock()
	defer lock.Unlock()

	b.mu.Lock()
	if cur, ok := b.projectBases[p.GetName()]; ok && cur.Recipe == recipe && cur.Ref.Cell > 0 && cur.Ref.Version > 0 {
		out := *cur
		b.mu.Unlock()
		return &out, nil
	}
	b.mu.Unlock()

	if existingShared != nil && existingShared.Cell > 0 && existingShared.Version > 0 && existingShared.Recipe == recipe {
		return &vmProjectBase{
			Ref: cladefs.CellRef{
				Tenant:  existingShared.Tenant,
				Cell:    existingShared.Cell,
				Version: existingShared.Version,
			},
			BaseID: cmp.Or(existingShared.BaseId, p.GetName()+"-fuse-"+recipe[:8]),
			Recipe: recipe,
			Repos:  map[string]string{},
		}, nil
	}

	baseImgRef, err := b.EnsureProvisionedBaseImage(ctx, spec, prov)
	if err != nil {
		return nil, err
	}

	baseID := fmt.Sprintf("%s-fuse-%s", p.GetName(), recipe[:8])
	tenant, cellID, mntDir, storeDir, err := b.forkAndMount(ctx, baseImgRef, filepath.Join("fuse-bases", baseID), 0)
	if err != nil {
		return nil, fmt.Errorf("fork & mount cladefs FUSE project base %s: %w", baseID, err)
	}
	cleanupOnErr := func() {
		_ = b.store.MountDetach(context.Background(), tenant, cellID, true)
		_ = b.store.DeleteCellAbs(context.Background(), storeDir)
	}

	env := append([]string{}, cacheEnv()...)
	env = append(env, sandboxEnv...)
	env = append(env, "HOME="+spec.Home)

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

	out, runErr := runStep(ctx, mntDir, "hotbed-fuse-repos-"+baseID, []string{"/bin/bash", "-c", cloneScript.String()}, env)
	if runErr == nil {
		for i, s := range p.GetSteps() {
			stepUnit := fmt.Sprintf("hotbed-fuse-step-%d-%s", i+1, baseID)
			if _, err := runStep(ctx, mntDir, stepUnit, []string{"/bin/bash", "-eo", "pipefail", "-c", s.GetScript()}, env); err != nil {
				runErr = fmt.Errorf("step %q: %w", s.GetName(), err)
				break
			}
		}
	}
	if runErr != nil {
		cleanupOnErr()
		return nil, fmt.Errorf("freshen cladefs FUSE project base %s: %w", baseID, runErr)
	}

	if err := b.store.MountDetach(ctx, tenant, cellID, false); err != nil {
		cleanupOnErr()
		return nil, fmt.Errorf("detach cladefs FUSE project base %s: %w", baseID, err)
	}
	ver, err := b.store.MakeVersionAndSyncAbs(ctx, storeDir, 2)
	if err != nil {
		_ = b.store.DeleteCellAbs(context.Background(), storeDir)
		return nil, fmt.Errorf("sync cladefs FUSE project base %s to R2: %w", baseID, err)
	}
	_ = b.store.DropCellAbs(ctx, storeDir)

	repoHeads := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "::hotbed-head:: "); ok {
			parts := strings.Fields(rest)
			if len(parts) == 2 {
				repoHeads[parts[0]] = parts[1]
			}
		}
	}
	ref := cladefs.CellRef{
		RelPath: filepath.Join("bases", p.GetName(), baseID),
		Tenant:  tenant,
		Cell:    cellID,
		Version: ver,
	}
	res := &vmProjectBase{
		Ref:    ref,
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
		Tenant:  tenant,
		Cell:    cellID,
		Version: ver,
	}
	b.mu.Unlock()
	b.saveCacheJSON("project-"+p.GetName()+".json", res)
	slog.Info("cladefs host-FUSE project base built and published to R2", "project", p.GetName(), "baseId", baseID, "cell", cellID, "version", ver)
	return res, nil
}

// PrepareWorkspace forks baseRef into a new cladefs cell and mounts it into
// cladefsd's host FUSE mount at /var/lib/cladefs/mnt/<tenant>/<cell>.
func (b *CladefsFUSEBackend) PrepareWorkspace(ctx context.Context, wsID string, baseRef cladefs.CellRef, fencingToken uint64) (*pb.CladefsCell, string, error) {
	tenant, cellID, mntDir, _, err := b.forkAndMount(ctx, baseRef, filepath.Join("fuse-ws", wsID), fencingToken)
	if err != nil {
		return nil, "", fmt.Errorf("prepare cladefs FUSE workspace %s: %w", wsID, err)
	}
	b.mu.Lock()
	b.wsMounts[wsID] = fuseWSMount{Tenant: tenant, Cell: cellID, MountPath: mntDir}
	b.saveWSStateLocked()
	b.mu.Unlock()
	return &pb.CladefsCell{
		Tenant:       tenant,
		Cell:         cellID,
		FencingToken: fencingToken,
		Arch:         goruntime.GOARCH,
	}, mntDir, nil
}

// DeleteWorkspace detaches and deletes the cladefs FUSE cell backing wsID.
func (b *CladefsFUSEBackend) DeleteWorkspace(ctx context.Context, wsID string) error {
	b.mu.Lock()
	m, ok := b.wsMounts[wsID]
	if ok {
		delete(b.wsMounts, wsID)
		b.saveWSStateLocked()
	}
	b.mu.Unlock()
	if !ok {
		return nil
	}
	if err := b.store.MountDetach(ctx, m.Tenant, m.Cell, true); err != nil {
		slog.Warn("detach cladefs FUSE workspace failed", "workspace", wsID, "cell", m.Cell, "err", err)
	}
	return b.store.DeleteCellAbs(ctx, b.mountStoreDir(m.Tenant, m.Cell))
}

// createCladefsFUSEWorkspace creates a cladefs host-FUSE workspace asynchronously
// from a warm R2 project base, leaving the local ZFS/btrfs store untouched.
func (e *Engine) createCladefsFUSEWorkspace(ctx context.Context, req *pb.CreateWorkspaceRequest) (*pb.Workspace, error) {
	p := req.Project
	now := e.nowMs()
	var repos []*pb.WorkspaceRepo
	for _, r := range p.GetRepos() {
		repos = append(repos, &pb.WorkspaceRepo{Repo: r.GetId()})
	}
	w := &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{
			Id: req.Id, Project: p.Name, BaseId: "cladefs-base", NodeId: e.cfg.NodeID, Agent: req.Agent,
			Repos: repos, State: preparing, CreatedAt: now, LastActiveAt: now,
			SessionId: req.SessionId, SyncHost: req.SyncHost, Runtime: runtime.KindCladefs,
			Reason: "preparing cladefs R2 base",
		},
		Volume: "ws/" + req.Id, BaseSnapshot: "cladefs-base",
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
	e.saveWorkspace(w, change{what: "created", detail: "cladefs host FUSE"})
	e.mu.Unlock()

	runInChroot := func(stepCtx context.Context, rootDir, unitName string, argv, env []string) (string, error) {
		if err := makeTmpDirs(rootDir, unitName, e.cred); err != nil {
			return "", err
		}
		tmpBase := filepath.Join(rootDir, tmpName, unitName)
		defer os.RemoveAll(tmpBase)
		tmpDir := filepath.Join(tmpBase, "tmp")
		runDir := filepath.Join(tmpBase, "run")
		runtimeTarget := e.runtimeChrootDir()
		logPath := filepath.Join(e.cfg.Dir, "logs", unitName+".log")
		_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
		stepEnv := append(append([]string(nil), env...), "XDG_RUNTIME_DIR="+runtimeTarget)
		spec := unit.Spec{
			Name:       unitName,
			Slice:      cmp.Or(e.cfg.Slice, unit.DefaultSlice),
			Argv:       argv,
			Dir:        WorkPath,
			Binds:      []unit.Bind{{Source: runDir, Target: runtimeTarget}},
			Tmp:        tmpDir,
			User:       e.cfg.User,
			Env:        stepEnv,
			PathPrefix: cachePath,
			Log:        logPath,
			Properties: e.cfg.UnitProperties,
		}
		e.inRootfs(&spec, rootDir)
		if err := e.cfg.Units.Start(stepCtx, spec); err != nil {
			return "", err
		}
		if err := e.cfg.Units.Wait(stepCtx, spec.Slice, spec.Name); err != nil {
			_ = e.cfg.Units.Stop(context.Background(), spec.Name)
			return "", err
		}
		st, err := e.cfg.Units.Status(stepCtx, spec.Name)
		outBytes, _ := os.ReadFile(logPath)
		out := string(outBytes)
		if err != nil {
			return out, err
		}
		if st.ExitCode != 0 {
			tail := out
			if len(tail) > 2048 {
				tail = tail[len(tail)-2048:]
			}
			return out, fmt.Errorf("chroot step %s exited %d:\n%s", unitName, st.ExitCode, strings.TrimSpace(tail))
		}
		return out, nil
	}

	finishFUSE := func(bgCtx context.Context) {
		start := e.now()
		var baseSpec baseimage.Spec
		var prov baseimage.Provisioner
		if e.cfg.BaseImage != nil {
			baseSpec = e.cfg.BaseImage.Spec
			prov = e.cfg.BaseImage.Provision
		}
		projBase, err := e.cfg.CladefsFUSE.EnsureProjectBase(bgCtx, p, req.GetSharedBase(), baseSpec, prov, runInChroot, e.sandboxEnv())
		var cell *pb.CladefsCell
		var mntDir string
		if err == nil {
			cell, mntDir, err = e.cfg.CladefsFUSE.PrepareWorkspace(bgCtx, req.Id, projBase.Ref, req.GetFencingToken())
		}
		if err == nil && len(projBase.Repos) == 0 {
			// Cross-node R2 fork: discover checked-out base commit per repo from the mounted rootfs.
			for _, r := range p.GetRepos() {
				out, gerr := exec.CommandContext(bgCtx, "git", "-C", filepath.Join(mntDir, "work", r.GetId()), "-c", "safe.directory=*", "rev-parse", "HEAD").Output()
				if gerr == nil {
					if head := strings.TrimSpace(string(out)); head != "" {
						projBase.Repos[r.GetId()] = head
					}
				}
			}
		}
		forkMs := e.now().Sub(start).Milliseconds()

		e.mu.Lock()
		w.forking = false
		w.forkDone = nil
		close(done)
		if w.ws().State == released || w.releasing {
			e.mu.Unlock()
			if err == nil {
				_ = e.cfg.CladefsFUSE.DeleteWorkspace(context.Background(), req.Id)
			}
			return
		}
		if err != nil {
			w.ws().State = failedWS
			w.ws().Reason = "could not prepare cladefs workspace: " + err.Error()
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
			detail: fmt.Sprintf("cladefs FUSE ready in %d ms (cell %d/%d)", forkMs, cell.Tenant, cell.Cell),
			feed:   feed(feedFork, fmt.Sprintf("cladefs %s ready in %d ms · %s", req.Id, forkMs, req.Agent), ref("workspace", req.Id)),
		})
		e.stepSoon()
		e.mu.Unlock()
	}

	e.mu.Lock()
	started := e.startBgLocked(finishFUSE)
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
