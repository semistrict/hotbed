package worker

import (
	"cmp"
	"context"
	"crypto/rand"
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
	"sync/atomic"
	"time"

	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/cladefs"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/storage"
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
	store           atomic.Pointer[cladefs.Store]
	incarnation     uint64

	baseMu       sync.Mutex
	projectLocks sync.Map // project name -> *sync.Mutex

	mu             sync.Mutex
	lastPeers      []string
	decommissioned map[uint64]bool
	baseImages     map[string]cladefs.CellRef // fuseBaseKey -> cached R2 base cell ref
	projectBases   map[string]*vmProjectBase  // project -> latest cached R2 base
	sharedBases    map[string]*pb.CladefsCell
	wsMounts       map[string]fuseWSMount // wsID -> active FUSE mount
	retirer        baseRetirer
}

type fuseWSMount struct {
	Tenant    uint64 `json:"tenant"`
	Cell      uint64 `json:"cell"`
	MountPath string `json:"mountPath"`
	StorePath string `json:"storePath,omitempty"`
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
	if err := clearMountNosuid(mountRoot); err != nil {
		_ = cstore.Close()
		return nil, fmt.Errorf("enable setuid on cladefs FUSE mount %s: %w", mountRoot, err)
	}
	b := &CladefsFUSEBackend{
		cfg:             cfg,
		workerDir:       workerDir,
		mountRoot:       mountRoot,
		mountStoresRoot: mountStoresRoot,
		incarnation:     inc,
		decommissioned:  map[uint64]bool{},
		baseImages:      map[string]cladefs.CellRef{},
		projectBases:    map[string]*vmProjectBase{},
		sharedBases:     map[string]*pb.CladefsCell{},
		wsMounts:        map[string]fuseWSMount{},
	}
	b.store.Store(cstore)
	b.loadState()
	return b, nil
}

func (b *CladefsFUSEBackend) currentStore() *cladefs.Store {
	return b.store.Load()
}

func (b *CladefsFUSEBackend) configSnapshot() statedir.CladefsFUSEConfig {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cfg
}

// UpdateConfig hot-reloads non-structural cladefs host-FUSE settings (default
// runtime, disk size, peer address) without disrupting active FUSE mounts.
func (b *CladefsFUSEBackend) UpdateConfig(cfg statedir.CladefsFUSEConfig) {
	b.mu.Lock()
	b.cfg = cfg
	b.mu.Unlock()
}

func (b *CladefsFUSEBackend) Close() error {
	if st := b.currentStore(); st != nil {
		return st.Close()
	}
	return nil
}

// StopAllForUpgrade detaches all active host-FUSE workspace mounts and closes
// the cladefsd gRPC connection prior to a disruptive cladefsd restart.
func (b *CladefsFUSEBackend) StopAllForUpgrade(ctx context.Context) error {
	b.mu.Lock()
	ids := make([]string, 0, len(b.wsMounts))
	for id := range b.wsMounts {
		ids = append(ids, id)
	}
	b.mu.Unlock()

	var firstErr error
	for _, id := range ids {
		if err := b.DeleteWorkspace(ctx, id); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := b.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// ReconnectCladefs waits for cladefsd and its host FUSE Mount service to come
// back online after a restart, clears nosuid on mountRoot, refreshes the gRPC
// client and incarnation, and re-applies the peer mesh.
func (b *CladefsFUSEBackend) ReconnectCladefs(ctx context.Context) (uint64, error) {
	cfg := b.configSnapshot()
	cellRoot := cmp.Or(cfg.CellRoot, "/mnt/fxdax-scratch/cladefsd/cells/hotbed")
	ccfg := cladefs.Config{
		ControlSock: cfg.CladefsControlSock,
		CellRoot:    cellRoot,
		TenantID:    cfg.TenantID,
		DefaultCap:  cmp.Or(cfg.DiskBytes, uint64(64<<30)),
	}
	st, inc, err := waitForCladefsReconnect(ctx, ccfg, 30*time.Second, func(ctx context.Context, s *cladefs.Store) (uint64, error) {
		if _, err := s.MountList(ctx); err != nil {
			return 0, err
		}
		return s.Incarnation(ctx)
	})
	if err != nil {
		return 0, err
	}
	if err := clearMountNosuid(b.mountRoot); err != nil {
		_ = st.Close()
		return 0, fmt.Errorf("enable setuid on cladefs FUSE mount %s: %w", b.mountRoot, err)
	}
	old := b.store.Swap(st)
	b.mu.Lock()
	b.incarnation = inc
	prevPeers := slices.Clone(b.lastPeers)
	b.lastPeers = nil
	b.decommissioned = map[uint64]bool{}
	b.mu.Unlock()
	if old != nil && old != st {
		_ = old.Close()
	}
	if len(prevPeers) > 0 {
		b.ApplyHeartbeat(ctx, prevPeers, nil)
	}
	return inc, nil
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
	rel := filepath.Join(cladefs.Base32ID(tenant), cladefs.Base32ID(cell))
	roots := filepath.SplitList(b.mountStoresRoot)
	for _, root := range roots {
		if root == "" {
			continue
		}
		cand := filepath.Join(root, rel)
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	if len(roots) > 0 && roots[0] != "" {
		return filepath.Join(roots[0], rel)
	}
	return filepath.Join(b.mountStoresRoot, rel)
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
			if key, ok := strings.CutPrefix(ent.Name(), "baseimage-"); ok {
				key = strings.TrimSuffix(key, ".json")
				var ref cladefs.CellRef
				if json.Unmarshal(raw, &ref) == nil && ref.Cell > 0 && ref.Version > 0 {
					if b.baseImages == nil {
						b.baseImages = map[string]cladefs.CellRef{}
					}
					b.baseImages[key] = ref
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
	b.retirer.load(b.cacheDir())
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
	if b.configSnapshot().Default {
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

// SampleObjectStore fetches cumulative cladefsd R2 object-store counters.
func (b *CladefsFUSEBackend) SampleObjectStore(ctx context.Context) *pb.ObjectStoreStats {
	return sampleCladefsObjectStore(ctx, b.currentStore())
}

// ApplyHeartbeat reconciles cladefs peer mesh (SetPeers) and retired-host
// decommissioning (DecommissionHost) from HeartbeatResponse.
func (b *CladefsFUSEBackend) ApplyHeartbeat(ctx context.Context, peers []string, decommission []uint64) {
	st := b.currentStore()
	if st == nil {
		return
	}
	if len(peers) > 0 && b.configSnapshot().CladefsPeerAddr != "" {
		sorted := slices.Clone(peers)
		slices.Sort(sorted)
		b.mu.Lock()
		changed := !slices.Equal(b.lastPeers, sorted)
		b.mu.Unlock()
		if changed {
			if err := st.SetPeers(ctx, sorted); err != nil {
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
		retired, left, err := st.DecommissionHost(ctx, inc)
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
	b.retirer.sweep(ctx, st, "fuse-retire", time.Now())
}

func (b *CladefsFUSEBackend) fuseBaseKey(spec baseimage.Spec) string {
	cfg := b.configSnapshot()
	h := sha256.New()
	fmt.Fprintf(h, "fuse-v1\nbase=%s\ndisk=%d\n", spec.Key(), cmp.Or(cfg.DiskBytes, uint64(64<<30)))
	return spec.Key() + "-f" + hex.EncodeToString(h.Sum(nil))[:8]
}

func randomUnitSuffix() string {
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

func fuseProjectRecipe(baseKey string, p *pb.HostedProject, heads map[string]string) string {
	h := sha256.New()
	fmt.Fprintf(h, "fuse-v2\nbase=%s\narch=%s\nproject=%s\nepoch=%d\n", baseKey, goruntime.GOARCH, p.GetName(), p.GetScratchEpoch())
	for _, r := range p.GetRepos() {
		fmt.Fprintf(h, "repo=%s|%s|%s|%s\n", r.GetId(), r.GetUrl(), r.GetBranch(), heads[r.GetId()])
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
	cfg := b.configSnapshot()
	key := b.fuseBaseKey(spec)

	b.baseMu.Lock()
	defer b.baseMu.Unlock()

	b.mu.Lock()
	if ref, ok := b.baseImages[key]; ok && ref.Cell > 0 && ref.Version > 0 {
		b.mu.Unlock()
		b.retirer.sweep(ctx, b.currentStore(), "fuse-retire", time.Now())
		return ref, nil
	}
	b.mu.Unlock()

	if err := clearMountNosuid(b.mountRoot); err != nil {
		return cladefs.CellRef{}, fmt.Errorf("enable setuid on cladefs FUSE mount %s: %w", b.mountRoot, err)
	}
	capBytes := cmp.Or(cfg.DiskBytes, uint64(64<<30))
	slog.Info("building cladefs host-FUSE base image", "key", key, "image", spec.Image)
	cellID, shown, err := b.currentStore().MountAttach(ctx, cfg.TenantID, 0, capBytes, nil, baseBuildUploadInterval)
	if err != nil {
		return cladefs.CellRef{}, fmt.Errorf("create cladefs FUSE base cell: %w", err)
	}
	mntDir := b.mountDir(cfg.TenantID, cellID)
	storeDir := b.mountStoreDir(cfg.TenantID, cellID)
	cleanupOnErr := func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = b.currentStore().MountDetach(cctx, cfg.TenantID, cellID, true)
		_ = b.currentStore().DeleteCellAbs(cctx, storeDir)
	}
	if !shown {
		cleanupOnErr()
		return cladefs.CellRef{}, fmt.Errorf("cladefs FUSE base cell %d not shown", cellID)
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
	if err := b.currentStore().MountDetach(ctx, cfg.TenantID, cellID, false); err != nil {
		cleanupOnErr()
		return cladefs.CellRef{}, fmt.Errorf("detach cladefs FUSE base %s: %w", key, err)
	}
	ver, err := b.currentStore().MakeVersionAndSyncAbs(ctx, storeDir, 2)
	if err != nil {
		_ = b.currentStore().DeleteCellAbs(context.Background(), storeDir)
		return cladefs.CellRef{}, fmt.Errorf("version & sync cladefs FUSE base %s to R2: %w", key, err)
	}
	_ = b.currentStore().DropCellAbs(ctx, storeDir)

	ref := cladefs.CellRef{
		RelPath: "baseimages/fuse-" + key,
		Tenant:  cfg.TenantID,
		Cell:    cellID,
		Version: ver,
	}
	b.mu.Lock()
	if b.baseImages == nil {
		b.baseImages = map[string]cladefs.CellRef{}
	}
	superseded := collectSupersededBaseImages(b.baseImages, key, cellID)
	b.baseImages[key] = ref
	b.mu.Unlock()
	b.saveCacheJSON("baseimage-"+key+".json", ref)
	now := time.Now()
	if b.workerDir != "" {
		retireSupersededBaseImages(b.cacheDir(), superseded, &b.retirer, now)
	} else {
		retireSupersededBaseImages("", superseded, &b.retirer, now)
	}
	b.retirer.sweep(ctx, b.currentStore(), "fuse-retire", now)
	slog.Info("cladefs host-FUSE base image synced to R2", "key", key, "cell", cellID, "version", ver)
	return ref, nil
}

// forkAndMount forks an R2 version into a new bucket cell, syncs its head so
// Kind::Fxfs is visible to Mount.Attach, drops the staging dir, and mounts it
// inside cladefsd's host FUSE mount.
func (b *CladefsFUSEBackend) forkAndMount(ctx context.Context, from cladefs.CellRef, stageRel string, fencingToken uint64, uploadInterval time.Duration) (tenant, cell uint64, mountPath, storeDir string, err error) {
	if err := b.currentStore().DropCell(ctx, stageRel); err != nil {
		if derr := b.currentStore().DeleteCell(ctx, stageRel); derr != nil {
			return 0, 0, "", "", fmt.Errorf("clear stale staging cell %s: %w", stageRel, derr)
		}
	}
	forkRef, err := b.currentStore().ForkVersion(ctx, from, stageRel, false)
	if err != nil {
		return 0, 0, "", "", fmt.Errorf("fork R2 version %d/%x: %w", from.Cell, from.Version, err)
	}
	if err := b.currentStore().SyncCell(ctx, stageRel); err != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		_ = b.currentStore().DeleteCell(cctx, stageRel)
		cancel()
		return 0, 0, "", "", fmt.Errorf("sync forked cell %d head: %w", forkRef.Cell, err)
	}
	if err := b.currentStore().DropCell(ctx, stageRel); err != nil {
		_ = b.currentStore().DeleteCell(context.Background(), stageRel)
		return 0, 0, "", "", fmt.Errorf("drop staging dir %s: %w", stageRel, err)
	}
	var tok *uint64
	if fencingToken > 0 {
		t := fencingToken
		tok = &t
	}
	attachedCell, shown, err := b.currentStore().MountAttach(ctx, forkRef.Tenant, forkRef.Cell, 0, tok, uploadInterval)
	if err != nil {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = b.currentStore().DeleteBucketCell(cctx, filepath.Join("fuse-retire", fmt.Sprintf("fail-%d-%d", forkRef.Tenant, forkRef.Cell)), forkRef.Tenant, forkRef.Cell)
		cancel()
		return 0, 0, "", "", fmt.Errorf("mount attach forked cell %d: %w", forkRef.Cell, err)
	}
	if !shown {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_ = b.currentStore().MountDetach(cctx, forkRef.Tenant, attachedCell, true)
		_ = b.currentStore().DeleteBucketCell(cctx, filepath.Join("fuse-retire", fmt.Sprintf("fail-%d-%d", forkRef.Tenant, attachedCell)), forkRef.Tenant, attachedCell)
		cancel()
		return 0, 0, "", "", fmt.Errorf("forked cell %d attached with shown=false (head kind missing)", attachedCell)
	}
	return forkRef.Tenant, attachedCell, b.mountDir(forkRef.Tenant, attachedCell), b.mountStoreDir(forkRef.Tenant, attachedCell), nil
}

// forkEphemeralAndMount forks an R2 version into a host-local ephemeral cell
// (reads come from R2 via CLADEFSD_PART_CACHE, writes stay host-local with zero
// R2 segment uploads) and mounts it inside cladefsd's host FUSE mount.
func (b *CladefsFUSEBackend) forkEphemeralAndMount(ctx context.Context, from cladefs.CellRef, wsRel string) (tenant, cell uint64, mountPath, storeDir string, err error) {
	// Clear any stale unmounted ephemeral cell left by an earlier crash or
	// interrupted creation so ForkVersion is idempotent and never fails EEXIST.
	if err := b.currentStore().DeleteCell(ctx, wsRel); err != nil {
		return 0, 0, "", "", fmt.Errorf("clear stale ephemeral cell %s: %w", wsRel, err)
	}
	forkRef, err := b.currentStore().ForkVersion(ctx, from, wsRel, true)
	if err != nil {
		return 0, 0, "", "", fmt.Errorf("fork ephemeral R2 version %d/%x: %w", from.Cell, from.Version, err)
	}
	attachedCell, shown, err := b.currentStore().MountAttach(ctx, forkRef.Tenant, forkRef.Cell, 0, nil, 0)
	if err != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		_ = b.currentStore().DeleteCell(cctx, wsRel)
		cancel()
		return 0, 0, "", "", fmt.Errorf("mount attach ephemeral cell %d: %w", forkRef.Cell, err)
	}
	if !shown {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		_ = b.currentStore().MountDetach(cctx, forkRef.Tenant, attachedCell, true)
		_ = b.currentStore().DeleteCell(cctx, wsRel)
		cancel()
		return 0, 0, "", "", fmt.Errorf("ephemeral cell %d attached with shown=false (kind missing)", attachedCell)
	}
	return forkRef.Tenant, attachedCell, b.mountDir(forkRef.Tenant, attachedCell), forkRef.HostPath, nil
}

// EnsureProjectBase builds or reuses a warm R2 project base for host-FUSE nodes.
// When an existing project base matches baseKey and ScratchEpoch, it forks
// incrementally from that base and runs only new/changed and --every-pull steps.
func (b *CladefsFUSEBackend) EnsureProjectBase(
	ctx context.Context,
	p *pb.HostedProject,
	heads map[string]string,
	existingShared *pb.CladefsCell,
	spec baseimage.Spec,
	prov baseimage.Provisioner,
	runFreshen managedFreshenRunner,
	onBaseTarget func(baseID string),
	sandboxEnv []string,
) (*vmProjectBase, error) {
	spec = spec.Normalized()
	baseKey := b.fuseBaseKey(spec)
	recipe := fuseProjectRecipe(baseKey, p, heads)
	baseID := fmt.Sprintf("%s-fuse-%s", p.GetName(), recipe[:8])
	if onBaseTarget != nil {
		onBaseTarget(baseID)
	}

	lock := b.projectLock(p.GetName())
	lock.Lock()
	defer lock.Unlock()

	b.mu.Lock()
	cur := b.projectBases[p.GetName()]
	if cur != nil && cur.Recipe == recipe && cur.Ref.Cell > 0 && cur.Ref.Version > 0 {
		out := *cur
		b.mu.Unlock()
		b.retirer.sweep(ctx, b.currentStore(), "fuse-retire", time.Now())
		return &out, nil
	}
	var prevBase *vmProjectBase
	if cur != nil && cur.Ref.Cell > 0 && cur.Ref.Version > 0 &&
		cur.BaseKey == baseKey && cur.ScratchEpoch == p.GetScratchEpoch() {
		cp := *cur
		prevBase = &cp
	}
	b.mu.Unlock()

	if existingShared != nil && existingShared.Cell > 0 && existingShared.Version > 0 && existingShared.Recipe == recipe {
		repos := make(map[string]string, len(heads))
		for k, v := range heads {
			repos[k] = v
		}
		adopted := &vmProjectBase{
			Ref: cladefs.CellRef{
				Tenant:  existingShared.Tenant,
				Cell:    existingShared.Cell,
				Version: existingShared.Version,
			},
			BaseID:       cmp.Or(existingShared.BaseId, p.GetName()+"-fuse-"+recipe[:8]),
			Recipe:       recipe,
			BaseKey:      baseKey,
			ScratchEpoch: p.GetScratchEpoch(),
			Repos:        repos,
			StepHashes:   projectStepHashes(p),
		}
		if cur != nil && cur.Ref.Cell > 0 && cur.Ref.Cell != adopted.Ref.Cell {
			b.retirer.enqueue(cur.Ref.Tenant, cur.Ref.Cell, cur.BaseID, time.Now())
		}
		b.mu.Lock()
		b.projectBases[p.GetName()] = adopted
		b.sharedBases[p.GetName()] = &pb.CladefsCell{
			Project: p.GetName(),
			Arch:    goruntime.GOARCH,
			BaseId:  adopted.BaseID,
			Recipe:  recipe,
			Tenant:  adopted.Ref.Tenant,
			Cell:    adopted.Ref.Cell,
			Version: adopted.Ref.Version,
		}
		b.mu.Unlock()
		b.saveCacheJSON("project-"+p.GetName()+".json", adopted)
		b.retirer.sweep(ctx, b.currentStore(), "fuse-retire", time.Now())
		return adopted, nil
	}

	if err := clearMountNosuid(b.mountRoot); err != nil {
		return nil, fmt.Errorf("enable setuid on cladefs FUSE mount %s: %w", b.mountRoot, err)
	}
	if runFreshen == nil {
		return nil, fmt.Errorf("freshen cladefs FUSE project base %s: no freshen runner configured", baseID)
	}
	var tenant, cellID uint64
	var mntDir, storeDir string
	var err error
	if prevBase != nil {
		tenant, cellID, mntDir, storeDir, err = b.forkAndMount(ctx, prevBase.Ref, filepath.Join("fuse-bases", baseID), 0, baseBuildUploadInterval)
		if err != nil {
			slog.Warn("incremental cladefs FUSE project base fork failed; falling back to cold base",
				"project", p.GetName(), "prevBase", prevBase.BaseID, "err", err)
			prevBase = nil
		}
	}
	if prevBase == nil {
		baseImgRef, berr := b.EnsureProvisionedBaseImage(ctx, spec, prov)
		if berr != nil {
			return nil, berr
		}
		tenant, cellID, mntDir, storeDir, err = b.forkAndMount(ctx, baseImgRef, filepath.Join("fuse-bases", baseID), 0, baseBuildUploadInterval)
		if err != nil {
			return nil, fmt.Errorf("fork & mount cladefs FUSE project base %s: %w", baseID, err)
		}
	}
	cleanupOnErr := func() {
		_ = b.currentStore().MountDetach(context.Background(), tenant, cellID, true)
		_ = b.currentStore().DeleteCellAbs(context.Background(), storeDir)
	}

	stepsToRun := selectProjectBaseSteps(p, prevBase, heads)
	script := buildCladefsFreshenScript(p, heads, stepsToRun, true)
	handle, err := runFreshen(ctx, managedFreshenSpec{
		Project:    p,
		BaseID:     baseID,
		Recipe:     recipe,
		Arch:       goruntime.GOARCH,
		Cold:       prevBase == nil,
		PrevBase:   prevBase,
		StepsToRun: stepsToRun,
		Heads:      heads,
		Script:     script,
		Env:        slices.Clone(sandboxEnv),
		RootDir:    mntDir,
	})
	if err != nil {
		cleanupOnErr()
		return nil, fmt.Errorf("queue cladefs FUSE project base %s: %w", baseID, err)
	}
	freshenRes, waitErr := handle.Wait(ctx)
	if waitErr != nil {
		cleanupOnErr()
		handle.Finish(false, freshenRes, cladefs.CellRef{}, waitErr.Error())
		return nil, fmt.Errorf("freshen cladefs FUSE project base %s (command %s): %w", baseID, handle.CommandID(), waitErr)
	}
	if freshenRes.State != succeeded {
		cleanupOnErr()
		detail := cmp.Or(freshenRes.Reason, "freshen "+stateName(freshenRes.State))
		handle.Finish(false, freshenRes, cladefs.CellRef{}, detail)
		return nil, fmt.Errorf("freshen cladefs FUSE project base %s failed (command %s): %s", baseID, handle.CommandID(), detail)
	}

	if err := b.currentStore().MountDetach(ctx, tenant, cellID, false); err != nil {
		cleanupOnErr()
		handle.Finish(false, freshenRes, cladefs.CellRef{}, fmt.Sprintf("detach mount: %v", err))
		return nil, fmt.Errorf("detach cladefs FUSE project base %s: %w", baseID, err)
	}
	ver, err := b.currentStore().MakeVersionAndSyncAbs(ctx, storeDir, 2)
	if err != nil {
		_ = b.currentStore().DeleteCellAbs(context.Background(), storeDir)
		handle.Finish(false, freshenRes, cladefs.CellRef{}, fmt.Sprintf("sync to R2: %v", err))
		return nil, fmt.Errorf("sync cladefs FUSE project base %s to R2: %w", baseID, err)
	}
	_ = b.currentStore().DropCellAbs(ctx, storeDir)

	repoHeads := freshenRes.RepoHeads
	if len(repoHeads) == 0 && len(heads) > 0 {
		repoHeads = make(map[string]string, len(heads))
		for k, v := range heads {
			repoHeads[k] = v
		}
	}
	ref := cladefs.CellRef{
		RelPath: filepath.Join("bases", p.GetName(), baseID),
		Tenant:  tenant,
		Cell:    cellID,
		Version: ver,
	}
	handle.Finish(true, freshenRes, ref, "")
	res := &vmProjectBase{
		Ref:          ref,
		BaseID:       baseID,
		Recipe:       recipe,
		BaseKey:      baseKey,
		ScratchEpoch: p.GetScratchEpoch(),
		Repos:        repoHeads,
		StepHashes:   projectStepHashes(p),
	}
	b.mu.Lock()
	superseded := b.projectBases[p.GetName()]
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
	if superseded != nil && superseded.Ref.Cell > 0 && superseded.Ref.Cell != cellID {
		b.retirer.enqueue(superseded.Ref.Tenant, superseded.Ref.Cell, superseded.BaseID, time.Now())
	}
	b.retirer.sweep(ctx, b.currentStore(), "fuse-retire", time.Now())
	slog.Info("cladefs host-FUSE project base built and published to R2",
		"project", p.GetName(), "baseId", baseID, "incremental", prevBase != nil,
		"steps", len(stepsToRun), "cell", cellID, "version", ver)
	return res, nil
}

// PrepareWorkspace forks baseRef into a host-local ephemeral cladefs cell and
// mounts it into cladefsd's host FUSE mount at /var/lib/cladefs/mnt/<tenant>/<cell>.
func (b *CladefsFUSEBackend) PrepareWorkspace(ctx context.Context, wsID string, baseRef cladefs.CellRef, fencingToken uint64) (*pb.CladefsCell, string, error) {
	b.mu.Lock()
	_, active := b.wsMounts[wsID]
	b.mu.Unlock()
	if active {
		return nil, "", fmt.Errorf("prepare cladefs FUSE workspace %s: active mount already exists: %w", wsID, storage.ErrBusy)
	}
	if err := clearMountNosuid(b.mountRoot); err != nil {
		return nil, "", fmt.Errorf("enable setuid on cladefs FUSE mount %s: %w", b.mountRoot, err)
	}
	tenant, cellID, mntDir, storeDir, err := b.forkEphemeralAndMount(ctx, baseRef, filepath.Join("fuse-ws", wsID))
	if err != nil {
		return nil, "", fmt.Errorf("prepare cladefs FUSE workspace %s: %w", wsID, err)
	}
	b.mu.Lock()
	b.wsMounts[wsID] = fuseWSMount{Tenant: tenant, Cell: cellID, MountPath: mntDir, StorePath: storeDir}
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
	if err := b.currentStore().MountDetach(ctx, m.Tenant, m.Cell, true); err != nil {
		slog.Warn("detach cladefs FUSE workspace failed", "workspace", wsID, "cell", m.Cell, "err", err)
	}
	storeDir := cmp.Or(m.StorePath, b.mountStoreDir(m.Tenant, m.Cell))
	return b.currentStore().DeleteCellAbs(ctx, storeDir)
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

	finishFUSE := func(bgCtx context.Context) {
		start := e.now()
		var baseSpec baseimage.Spec
		var prov baseimage.Provisioner
		if e.cfg.BaseImage != nil {
			baseSpec = e.cfg.BaseImage.Spec
			prov = e.cfg.BaseImage.Provision
		}
		heads, err := e.remoteHeads(bgCtx, p)
		var projBase *vmProjectBase
		if err == nil {
			projBase, err = e.cfg.CladefsFUSE.EnsureProjectBase(
				bgCtx, p, heads, req.GetSharedBase(), baseSpec, prov,
				e.startManagedFreshen,
				func(baseID string) { e.setPreparingWorkspaceBase(req.Id, baseID) },
				e.sandboxEnv(),
			)
		}
		if err == nil {
			e.recordWarmCladefsBase(p, projBase, goruntime.GOARCH)
		}
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
