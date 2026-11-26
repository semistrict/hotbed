package worker

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
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
// not run Firecracker microVMs (e.g. 16 KiB arm64 Apple Silicon or VMs without nested virtualization).
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

	baseMu sync.Mutex
	held   heldBases // the project bases this node hands out

	mu             sync.Mutex
	lastPeers      []string
	decommissioned map[uint64]bool
	baseImages     map[string]cladefs.CellRef // fuseBaseKey -> cached R2 base cell ref
	wsMounts       map[string]fuseWSMount     // wsID -> active FUSE mount
	retirer        baseRetirer
	unhealthy      bool
	recovering     bool

	detachTimeout time.Duration      // optional MountDetach deadline override (tests; default 30s)
	cleanupMount  func(string, bool) // optional cleanupFUSEMountDir override (tests)

	// Testable hooks for FUSE mount health checks and ENOTCONN recovery.
	mountHealthFn func(string) error
	clearNosuidFn func(string) error
	isNotConnFn   func(error) bool
	detachDeadFn  func(string) error
	reconnectFn   func(context.Context) (uint64, error)
}

const defaultFUSEDetachTimeout = 30 * time.Second

// detachMount prepares mntDir (killing lingering processes referencing it,
// detaching leftover submounts, and optionally calling syncfs) and detaches the
// cell from cladefsd with a bounded timeout.
//
// It always passes force=true to MountDetach: in cladefsd, force=true still
// invalidates cached names and runs fs.unmount().await (writing back all dirty
// Fxfs state cleanly), whereas force=false transitions the cell to
// Attaching::Detaching and blocks indefinitely in forgotten_all(slot) if any
// kernel FUSE lookup lingers — causing any subsequent Detach(force=true) to
// fail EBUSY.
func (b *CladefsFUSEBackend) detachMount(ctx context.Context, tenant, cellID uint64, mntDir string, syncFS, ignoreCancel bool) error {
	if mntDir != "" {
		cleanup := b.cleanupMount
		if cleanup == nil {
			cleanup = cleanupFUSEMountDir
		}
		cleanup(mntDir, syncFS)
	}
	timeout := b.detachTimeout
	if timeout <= 0 {
		timeout = defaultFUSEDetachTimeout
	}
	baseCtx := ctx
	if ignoreCancel {
		baseCtx = context.WithoutCancel(ctx)
	}
	dctx, cancel := context.WithTimeout(baseCtx, timeout)
	defer cancel()
	return b.currentStore().MountDetach(dctx, tenant, cellID, true)
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
	cellRoot := tenantCellRoot(cmp.Or(cfg.CellRoot, "/mnt/fxdax-scratch/cladefsd/cells/hotbed"), cfg.TenantID)
	mountRoot := cmp.Or(cfg.MountRoot, "/var/lib/cladefs/mnt")
	mountStoresRoot := cmp.Or(cfg.MountStoresRoot, "/mnt/fxdax-scratch/cladefsd/mount-stores")
	// If a previous cladefsd died and left mountRoot stuck at ENOTCONN, detach
	// it lazily so systemd-managed cladefsd can remount cleanly on startup.
	if err := detachDeadMount(mountRoot); err != nil {
		slog.Warn("detach dead cladefs FUSE mount on startup", "mountRoot", mountRoot, "err", err)
	}
	ccfg := cladefs.Config{
		ControlSock: cfg.CladefsControlSock,
		CellRoot:    cellRoot,
		TenantID:    cfg.TenantID,
		DefaultCap:  cmp.Or(cfg.DiskBytes, uint64(64<<30)),
	}
	cstore, inc, err := waitForCladefsReconnect(ctx, ccfg, 15*time.Second, func(ctx context.Context, s *cladefs.Store) (uint64, error) {
		if _, err := s.MountList(ctx); err != nil {
			return 0, fmt.Errorf("cladefsd Mount service unavailable (set CLADEFSD_MOUNT and CLADEFSD_MOUNT_STORES): %w", err)
		}
		inc, err := s.Incarnation(ctx)
		if err != nil {
			slog.Warn("read cladefsd incarnation failed; continuing", "err", err)
		}
		return inc, nil
	})
	if err != nil {
		return nil, fmt.Errorf("open cladefs store: %w", err)
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
	cellRoot := tenantCellRoot(cmp.Or(cfg.CellRoot, "/mnt/fxdax-scratch/cladefsd/cells/hotbed"), cfg.TenantID)
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
		b.ApplyHeartbeat(ctx, prevPeers, nil, nil)
	}
	return inc, nil
}

func (b *CladefsFUSEBackend) cacheDir() string {
	if b.cfg.TenantID > 0 {
		return filepath.Join(b.workerDir, fmt.Sprintf("cladefs-fuse-bases-t%d", b.cfg.TenantID))
	}
	return filepath.Join(b.workerDir, "cladefs-fuse-bases")
}

func (b *CladefsFUSEBackend) wsStatePath() string {
	if b.cfg.TenantID > 0 {
		return filepath.Join(b.workerDir, fmt.Sprintf("cladefs-fuse-ws-t%d.json", b.cfg.TenantID))
	}
	return filepath.Join(b.workerDir, "cladefs-fuse-ws.json")
}

func (b *CladefsFUSEBackend) mountDir(tenant, cell uint64) string {
	return filepath.Join(b.mountRoot, cladefs.Base32ID(tenant), cladefs.Base32ID(cell))
}

func (b *CladefsFUSEBackend) mountStoreDir(tenant, cell uint64) (string, error) {
	rel := filepath.Join(cladefs.Base32ID(tenant), cladefs.Base32ID(cell))
	var roots []string
	for _, r := range filepath.SplitList(b.mountStoresRoot) {
		if r != "" {
			roots = append(roots, r)
		}
	}
	if len(roots) == 0 {
		return "", fmt.Errorf("cladefs FUSE mount stores root is empty")
	}
	for _, root := range roots {
		cand := filepath.Join(root, rel)
		fi, err := os.Stat(cand)
		if err == nil {
			if fi.IsDir() {
				return cand, nil
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("stat mount store %s: %w", cand, err)
		}
	}
	return filepath.Join(roots[0], rel), nil
}

func (b *CladefsFUSEBackend) loadState() {
	wantTenant := b.cfg.TenantID
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
				if json.Unmarshal(raw, &ref) == nil && ref.Cell > 0 && ref.Version > 0 && ref.Tenant == wantTenant {
					if b.baseImages == nil {
						b.baseImages = map[string]cladefs.CellRef{}
					}
					b.baseImages[key] = ref
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
	b.held.load(b.cacheDir(), wantTenant)
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

// DefaultRuntime returns "cladefs".
func (b *CladefsFUSEBackend) DefaultRuntime() string {
	return runtime.KindCladefs
}

// Runtimes returns the runtimes supported by this host-FUSE cladefs worker.
func (b *CladefsFUSEBackend) Runtimes() []string {
	return []string{runtime.KindCladefs}
}

func (b *CladefsFUSEBackend) checkHealth() error {
	if b.mountHealthFn != nil {
		return b.mountHealthFn(b.mountRoot)
	}
	return checkMountHealth(b.mountRoot)
}

func (b *CladefsFUSEBackend) clearNosuid() error {
	if b.clearNosuidFn != nil {
		return b.clearNosuidFn(b.mountRoot)
	}
	return clearMountNosuid(b.mountRoot)
}

func (b *CladefsFUSEBackend) isNotConnected(err error) bool {
	if b.isNotConnFn != nil {
		return b.isNotConnFn(err)
	}
	return isMountNotConnected(err)
}

func (b *CladefsFUSEBackend) detachDead() error {
	if b.detachDeadFn != nil {
		return b.detachDeadFn(b.mountRoot)
	}
	return detachDeadMount(b.mountRoot)
}

func (b *CladefsFUSEBackend) reconnect(ctx context.Context) (uint64, error) {
	if b.reconnectFn != nil {
		return b.reconnectFn(ctx)
	}
	return b.ReconnectCladefs(ctx)
}

// Healthy reports whether the host FUSE mount at mountRoot responds to statfs(2).
func (b *CladefsFUSEBackend) Healthy() (bool, error) {
	err := b.checkHealth()
	return err == nil, err
}

// RuntimesForHeartbeat returns the runtimes to advertise in heartbeats. When
// mountRoot is disconnected (e.g. ENOTCONN after a cladefsd OOM/crash), it
// drops "cladefs" from advertised runtimes so the controller stops placing new
// cladefs workspaces here and kicks off background ENOTCONN recovery.
func (b *CladefsFUSEBackend) RuntimesForHeartbeat() []string {
	ok, err := b.Healthy()
	if ok {
		b.mu.Lock()
		wasUnhealthy := b.unhealthy
		b.unhealthy = false
		b.mu.Unlock()
		if wasUnhealthy {
			slog.Info("cladefs host-FUSE mount recovered; re-advertising cladefs runtime", "mountRoot", b.mountRoot)
		}
		return b.Runtimes()
	}
	b.mu.Lock()
	wasUnhealthy := b.unhealthy
	b.unhealthy = true
	b.mu.Unlock()
	if !wasUnhealthy {
		slog.Warn("cladefs host-FUSE mount unhealthy; withdrawing cladefs runtime from heartbeats",
			"mountRoot", b.mountRoot, "err", err)
	}
	if b.isNotConnected(err) {
		b.triggerDeadMountRecovery()
	}
	return []string{}
}

func (b *CladefsFUSEBackend) triggerDeadMountRecovery() {
	b.mu.Lock()
	if b.recovering {
		b.mu.Unlock()
		return
	}
	b.recovering = true
	b.mu.Unlock()
	go func() {
		defer func() {
			b.mu.Lock()
			b.recovering = false
			b.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := b.recoverDeadMount(ctx); err != nil {
			slog.Warn("background cladefs FUSE mount recovery failed", "mountRoot", b.mountRoot, "err", err)
		}
	}()
}

// recoverDeadMount lazily detaches an ENOTCONN FUSE mount so systemd-managed
// cladefsd can remount cleanly, then reconnects the cladefsd gRPC client.
func (b *CladefsFUSEBackend) recoverDeadMount(ctx context.Context) error {
	if err := b.detachDead(); err != nil {
		return err
	}
	_, err := b.reconnect(ctx)
	return err
}

func (b *CladefsFUSEBackend) ensureMountReady(ctx context.Context) error {
	err := b.clearNosuid()
	if err == nil {
		return nil
	}
	if !b.isNotConnected(err) {
		return err
	}
	slog.Warn("cladefs host-FUSE mount returned ENOTCONN; attempting lazy detach and reconnect",
		"mountRoot", b.mountRoot, "err", err)
	if rerr := b.recoverDeadMount(ctx); rerr != nil {
		slog.Warn("cladefs host-FUSE ENOTCONN recovery failed", "mountRoot", b.mountRoot, "err", rerr)
		return err
	}
	return b.clearNosuid()
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

// HeartbeatMeta returns the cladefs peer address, incarnation, the project
// bases held here and the base cells retired since, for HeartbeatRequest.
func (b *CladefsFUSEBackend) HeartbeatMeta() (peerAddr string, incarnation uint64, held []*pb.SharedBase, retired []*pb.CladefsCell) {
	held, retired = b.held.report(runtime.KindCladefs)
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cfg.CladefsPeerAddr, b.incarnation, held, retired
}

// SampleObjectStore fetches cumulative cladefsd R2 object-store counters.
func (b *CladefsFUSEBackend) SampleObjectStore(ctx context.Context) *pb.ObjectStoreStats {
	return sampleCladefsObjectStore(ctx, b.currentStore())
}

// ApplyHeartbeat reconciles cladefs peer mesh (SetPeers), retired-host
// decommissioning (DecommissionHost) and the project base cells to retire
// from HeartbeatResponse.
func (b *CladefsFUSEBackend) ApplyHeartbeat(ctx context.Context, peers []string, decommission []uint64, retire []*pb.CladefsCell) {
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
	b.held.retire(ctx, st, retire)
	b.retirer.sweep(ctx, st, time.Now())
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
		b.retirer.sweep(ctx, b.currentStore(), time.Now())
		return ref, nil
	}
	b.mu.Unlock()

	if err := b.ensureMountReady(ctx); err != nil {
		return cladefs.CellRef{}, fmt.Errorf("enable setuid on cladefs FUSE mount %s: %w", b.mountRoot, err)
	}
	capBytes := cmp.Or(cfg.DiskBytes, uint64(64<<30))
	slog.Info("building cladefs host-FUSE base image", "key", key, "image", spec.Image)
	cellID, shown, err := b.currentStore().MountAttach(ctx, cfg.TenantID, 0, capBytes, nil, baseBuildUploadInterval)
	if err != nil {
		return cladefs.CellRef{}, fmt.Errorf("create cladefs FUSE base cell: %w", err)
	}
	mntDir := b.mountDir(cfg.TenantID, cellID)
	cleanupOnErr := func() {
		_ = b.detachMount(ctx, cfg.TenantID, cellID, mntDir, false, true)
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = b.currentStore().DeleteCell(cctx, cfg.TenantID, cellID)
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
	if err := b.detachMount(ctx, cfg.TenantID, cellID, mntDir, true, false); err != nil {
		cleanupOnErr()
		return cladefs.CellRef{}, fmt.Errorf("detach cladefs FUSE base %s: %w", key, err)
	}
	ver, err := b.currentStore().MakeVersionAndSync(ctx, cfg.TenantID, cellID, 2)
	if err != nil {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = b.currentStore().DeleteCell(cctx, cfg.TenantID, cellID)
		cancel()
		return cladefs.CellRef{}, fmt.Errorf("version & sync cladefs FUSE base %s to R2: %w", key, err)
	}
	_ = b.currentStore().DropCell(ctx, cfg.TenantID, cellID)

	ref := cladefs.CellRef{
		RelPath:  "baseimages/fuse-" + key,
		HostPath: cladefs.FormatCellID(cfg.TenantID, cellID),
		Tenant:   cfg.TenantID,
		Cell:     cellID,
		Version:  ver,
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
	b.retirer.sweep(ctx, b.currentStore(), now)
	slog.Info("cladefs host-FUSE base image synced to R2", "key", key, "cell", cellID, "version", ver)
	return ref, nil
}

// forkAndMount forks an R2 version into a new bucket cell, syncs its head so
// Kind::Fxfs is visible to Mount.Attach, drops the local copy, and mounts it
// inside cladefsd's host FUSE mount.
func (b *CladefsFUSEBackend) forkAndMount(ctx context.Context, from cladefs.CellRef, fencingToken uint64, uploadInterval time.Duration) (tenant, cell uint64, mountPath string, err error) {
	forkRef, err := b.currentStore().ForkVersion(ctx, from, false)
	if err != nil {
		return 0, 0, "", fmt.Errorf("fork R2 version %d/%x: %w", from.Cell, from.Version, err)
	}
	if err := b.currentStore().SyncCell(ctx, forkRef.Tenant, forkRef.Cell); err != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		_ = b.currentStore().DeleteCell(cctx, forkRef.Tenant, forkRef.Cell)
		cancel()
		return 0, 0, "", fmt.Errorf("sync forked cell %d head: %w", forkRef.Cell, err)
	}
	if err := b.currentStore().DropCell(ctx, forkRef.Tenant, forkRef.Cell); err != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		_ = b.currentStore().DeleteCell(cctx, forkRef.Tenant, forkRef.Cell)
		cancel()
		return 0, 0, "", fmt.Errorf("drop forked cell %d/%d: %w", forkRef.Tenant, forkRef.Cell, err)
	}
	var tok *uint64
	if fencingToken > 0 {
		t := fencingToken
		tok = &t
	}
	attachedCell, shown, err := b.currentStore().MountAttach(ctx, forkRef.Tenant, forkRef.Cell, 0, tok, uploadInterval)
	if err != nil {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = b.currentStore().DeleteBucketCell(cctx, forkRef.Tenant, forkRef.Cell)
		cancel()
		return 0, 0, "", fmt.Errorf("mount attach forked cell %d: %w", forkRef.Cell, err)
	}
	if !shown {
		_ = b.detachMount(ctx, forkRef.Tenant, attachedCell, "", false, true)
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_ = b.currentStore().DeleteBucketCell(cctx, forkRef.Tenant, attachedCell)
		cancel()
		return 0, 0, "", fmt.Errorf("forked cell %d attached with shown=false (head kind missing)", attachedCell)
	}
	return forkRef.Tenant, attachedCell, b.mountDir(forkRef.Tenant, attachedCell), nil
}

// forkEphemeralAndMount forks an R2 version into a host-local ephemeral cell
// (reads come from R2 via CLADEFSD_PART_CACHE, writes stay host-local with zero
// R2 segment uploads) and mounts it inside cladefsd's host FUSE mount.
func (b *CladefsFUSEBackend) forkEphemeralAndMount(ctx context.Context, from cladefs.CellRef) (tenant, cell uint64, mountPath string, err error) {
	forkRef, err := b.currentStore().ForkVersion(ctx, from, true)
	if err != nil {
		return 0, 0, "", fmt.Errorf("fork ephemeral R2 version %d/%x: %w", from.Cell, from.Version, err)
	}
	attachedCell, shown, err := b.currentStore().MountAttach(ctx, forkRef.Tenant, forkRef.Cell, 0, nil, 0)
	if err != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		_ = b.currentStore().DeleteCell(cctx, forkRef.Tenant, forkRef.Cell)
		cancel()
		return 0, 0, "", fmt.Errorf("mount attach ephemeral cell %d: %w", forkRef.Cell, err)
	}
	if !shown {
		_ = b.detachMount(ctx, forkRef.Tenant, attachedCell, "", false, true)
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		_ = b.currentStore().DeleteCell(cctx, forkRef.Tenant, forkRef.Cell)
		cancel()
		return 0, 0, "", fmt.Errorf("ephemeral cell %d attached with shown=false (kind missing)", attachedCell)
	}
	return forkRef.Tenant, attachedCell, b.mountDir(forkRef.Tenant, attachedCell), nil
}

// EnsureProjectBase returns a warm project base for p's host-FUSE workspaces:
// the one this node holds or the cluster's newest (shared), whichever is up to
// date, or a new one built on top of the newer of them and uploaded to the
// shared bucket.
func (b *CladefsFUSEBackend) EnsureProjectBase(
	ctx context.Context,
	p *pb.HostedProject,
	heads map[string]string,
	shared *pb.SharedBase,
	spec baseimage.Spec,
	prov baseimage.Provisioner,
	runFreshen managedFreshenRunner,
	onBaseTarget func(baseID string),
	sandboxEnv []string,
) (*projectBase, error) {
	res, err := b.ensureProjectBase(ctx, p, heads, shared, spec, prov, runFreshen, onBaseTarget, sandboxEnv, ensureBaseOpts{})
	return res.Base, err
}

func (b *CladefsFUSEBackend) ensureProjectBase(
	ctx context.Context,
	p *pb.HostedProject,
	heads map[string]string,
	shared *pb.SharedBase,
	spec baseimage.Spec,
	prov baseimage.Provisioner,
	runFreshen managedFreshenRunner,
	onBaseTarget func(baseID string),
	sandboxEnv []string,
	opts ensureBaseOpts,
) (ensureBaseResult, error) {
	spec = spec.Normalized()
	baseKey := b.fuseBaseKey(spec)
	recipe := fuseProjectRecipe(baseKey, p, heads)
	tenant := b.configSnapshot().TenantID
	if st := b.currentStore(); st != nil {
		tenant = st.TenantID()
	}
	return b.held.ensure(ctx, baseRequest{
		project:      p,
		heads:        heads,
		shared:       shared,
		runtime:      runtime.KindCladefs,
		tenant:       tenant,
		baseKey:      baseKey,
		recipe:       recipe,
		baseID:       fmt.Sprintf("%s-fuse-%s", p.GetName(), recipe[:8]),
		onBaseTarget: onBaseTarget,
		opts:         opts,
		build: func(ctx context.Context, plan basePlan) (builtBase, error) {
			return b.buildProjectBase(ctx, p, heads, spec, prov, baseKey, sandboxEnv, runFreshen, plan)
		},
	})
}

// buildProjectBase builds plan's base: it forks the base to build on (or the
// provisioned base image) and mounts the fork, freshens the repos and runs the
// steps that need to run in it, and uploads the result.
func (b *CladefsFUSEBackend) buildProjectBase(
	ctx context.Context,
	p *pb.HostedProject,
	heads map[string]string,
	spec baseimage.Spec,
	prov baseimage.Provisioner,
	baseKey string,
	sandboxEnv []string,
	runFreshen managedFreshenRunner,
	plan basePlan,
) (builtBase, error) {
	baseID, prevBase := plan.baseID, plan.prev
	fail := func(cmdID, summary string, stepFailed bool, active time.Duration, err error) (builtBase, error) {
		return builtBase{}, &baseBuildError{cmdID: cmdID, summary: summary, stepFailed: stepFailed, active: active, err: err}
	}

	if err := b.ensureMountReady(ctx); err != nil {
		return fail("", fmt.Sprintf("base %s failed: %v", baseID, err), false, 0, fmt.Errorf("enable setuid on cladefs FUSE mount %s: %w", b.mountRoot, err))
	}
	if runFreshen == nil {
		return fail("", fmt.Sprintf("base %s failed: no freshen runner configured", baseID), false, 0, fmt.Errorf("freshen cladefs FUSE project base %s: no freshen runner configured", baseID))
	}
	createdAt := time.Now().UnixMilli()
	prepStart := time.Now()
	var prepPhases []*pb.FreshenPhase
	fromBaseID := ""
	var tenant, cellID uint64
	var mntDir string
	var err error
	if prevBase != nil {
		forkStart := time.Now()
		forkStartMs := forkStart.UnixMilli()
		tenant, cellID, mntDir, err = b.forkAndMount(ctx, prevBase.Ref, 0, baseBuildUploadInterval)
		if err != nil {
			if ctx.Err() != nil {
				return fail("", fmt.Sprintf("base %s cancelled: %v", baseID, err), false, time.Since(prepStart), err)
			}
			return builtBase{}, &incrementalForkError{err: err}
		}
		forkEndMs := time.Now().UnixMilli()
		fromBaseID = prevBase.BaseID
		prepPhases = append(prepPhases, &pb.FreshenPhase{
			Name:      "fork_mount",
			StartedAt: forkStartMs,
			EndedAt:   &forkEndMs,
			Seconds:   time.Since(forkStart).Seconds(),
			Detail:    "from " + prevBase.BaseID,
		})
	} else {
		imgStart := time.Now()
		imgStartMs := imgStart.UnixMilli()
		baseImgRef, berr := b.EnsureProvisionedBaseImage(ctx, spec, prov)
		if berr != nil {
			return fail("", fmt.Sprintf("base %s failed: %v", baseID, berr), false, time.Since(prepStart), berr)
		}
		imgEndMs := time.Now().UnixMilli()
		prepPhases = append(prepPhases, &pb.FreshenPhase{
			Name:      "base_image",
			StartedAt: imgStartMs,
			EndedAt:   &imgEndMs,
			Seconds:   time.Since(imgStart).Seconds(),
			Detail:    baseKey,
		})
		forkStart := time.Now()
		forkStartMs := forkStart.UnixMilli()
		tenant, cellID, mntDir, err = b.forkAndMount(ctx, baseImgRef, 0, baseBuildUploadInterval)
		if err != nil {
			return fail("", fmt.Sprintf("base %s failed: %v", baseID, err), false, time.Since(prepStart), fmt.Errorf("fork & mount cladefs FUSE project base %s: %w", baseID, err))
		}
		forkEndMs := time.Now().UnixMilli()
		prepPhases = append(prepPhases, &pb.FreshenPhase{
			Name:      "fork_mount",
			StartedAt: forkStartMs,
			EndedAt:   &forkEndMs,
			Seconds:   time.Since(forkStart).Seconds(),
			Detail:    "from " + baseKey,
		})
	}
	prepSeconds := time.Since(prepStart).Seconds()
	cleanupOnErr := func() {
		_ = b.detachMount(ctx, tenant, cellID, mntDir, false, true)
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = b.currentStore().DeleteCell(cctx, tenant, cellID)
	}

	stepsToRun := selectProjectBaseSteps(p, prevBase, heads)
	script := buildCladefsFreshenScript(p, heads, stepsToRun, true)
	handle, err := runFreshen(ctx, managedFreshenSpec{
		Project:     p,
		BaseID:      baseID,
		Recipe:      plan.recipe,
		Arch:        goruntime.GOARCH,
		Runtime:     runtime.KindCladefs,
		Cold:        prevBase == nil,
		ColdReason:  plan.coldReason,
		FromBaseID:  fromBaseID,
		Optional:    !plan.required,
		CreatedAt:   createdAt,
		PrepSeconds: prepSeconds,
		PrepPhases:  prepPhases,
		PrevBase:    prevBase,
		StepsToRun:  stepsToRun,
		Heads:       heads,
		Script:      script,
		Env:         slices.Clone(sandboxEnv),
		RootDir:     mntDir,
	})
	if err != nil {
		cleanupOnErr()
		return fail("", fmt.Sprintf("base %s failed: %v", baseID, err), false, time.Duration(prepSeconds*float64(time.Second)), fmt.Errorf("queue cladefs FUSE project base %s: %w", baseID, err))
	}
	freshenRes, waitErr := handle.Wait(ctx)
	var finalizeSeconds float64
	activeDuration := func() time.Duration {
		secs := prepSeconds + freshenRes.ActiveRunSeconds + finalizeSeconds
		if secs < 0 {
			secs = 0
		}
		return time.Duration(secs * float64(time.Second))
	}
	if waitErr != nil {
		cleanupOnErr()
		handle.Finish(false, freshenRes, cladefs.CellRef{}, waitErr.Error())
		summary := managedFreshenFailureSummary(baseID, handle.CommandID(), freshenRes, waitErr.Error())
		return fail(handle.CommandID(), summary, freshenRes.StepFailed, activeDuration(), fmt.Errorf("freshen cladefs FUSE project base %s (command %s): %w", baseID, handle.CommandID(), waitErr))
	}
	if freshenRes.State != succeeded {
		cleanupOnErr()
		detail := cmp.Or(freshenRes.Reason, "freshen "+stateName(freshenRes.State))
		handle.Finish(false, freshenRes, cladefs.CellRef{}, detail)
		summary := managedFreshenFailureSummary(baseID, handle.CommandID(), freshenRes, detail)
		return fail(handle.CommandID(), summary, freshenRes.StepFailed, activeDuration(), fmt.Errorf("freshen cladefs FUSE project base %s failed (command %s): %s", baseID, handle.CommandID(), detail))
	}

	handle.SetFinalizing("mount_detach")
	detachStart := time.Now()
	detachStartMs := detachStart.UnixMilli()
	if err := b.detachMount(ctx, tenant, cellID, mntDir, true, false); err != nil {
		detachSecs := time.Since(detachStart).Seconds()
		finalizeSeconds += detachSecs
		handle.RecordPhase("mount_detach", detachStartMs, time.Now().UnixMilli(), detachSecs, "")
		freshenRes.FinalizeSeconds = detachSecs
		cleanupOnErr()
		detail := fmt.Sprintf("detach mount: %v", err)
		handle.Finish(false, freshenRes, cladefs.CellRef{}, detail)
		summary := managedFreshenFailureSummary(baseID, handle.CommandID(), freshenRes, detail)
		return fail(handle.CommandID(), summary, false, activeDuration(), fmt.Errorf("detach cladefs FUSE project base %s: %w", baseID, err))
	}
	detachSecs := time.Since(detachStart).Seconds()
	finalizeSeconds += detachSecs
	handle.RecordPhase("mount_detach", detachStartMs, time.Now().UnixMilli(), detachSecs, "")

	handle.SetFinalizing("r2_sync")
	syncStart := time.Now()
	syncStartMs := syncStart.UnixMilli()
	ver, err := b.currentStore().MakeVersionAndSync(ctx, tenant, cellID, 2)
	syncSecs := time.Since(syncStart).Seconds()
	finalizeSeconds += syncSecs
	handle.RecordPhase("r2_sync", syncStartMs, time.Now().UnixMilli(), syncSecs, "")
	freshenRes.FinalizeSeconds = finalizeSeconds
	if err != nil {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = b.currentStore().DeleteCell(cctx, tenant, cellID)
		cancel()
		detail := fmt.Sprintf("sync to R2: %v", err)
		handle.Finish(false, freshenRes, cladefs.CellRef{}, detail)
		summary := managedFreshenFailureSummary(baseID, handle.CommandID(), freshenRes, detail)
		return fail(handle.CommandID(), summary, false, activeDuration(), fmt.Errorf("sync cladefs FUSE project base %s to R2: %w", baseID, err))
	}
	_ = b.currentStore().DropCell(ctx, tenant, cellID)

	ref := cladefs.CellRef{
		RelPath:  filepath.Join("bases", p.GetName(), baseID),
		HostPath: cladefs.FormatCellID(tenant, cellID),
		Tenant:   tenant,
		Cell:     cellID,
		Version:  ver,
	}
	handle.Finish(true, freshenRes, ref, "")
	return builtBase{ref: ref, repoHeads: freshenRes.RepoHeads, steps: len(stepsToRun), active: activeDuration()}, nil
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
	if err := b.ensureMountReady(ctx); err != nil {
		return nil, "", fmt.Errorf("enable setuid on cladefs FUSE mount %s: %w", b.mountRoot, err)
	}
	tenant, cellID, mntDir, err := b.forkEphemeralAndMount(ctx, baseRef)
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
	if err := b.detachMount(ctx, m.Tenant, m.Cell, m.MountPath, false, true); err != nil {
		slog.Warn("detach cladefs FUSE workspace failed", "workspace", wsID, "cell", m.Cell, "err", err)
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return b.currentStore().DeleteCell(cctx, m.Tenant, m.Cell)
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
			SessionId: req.SessionId, SyncHost: req.SyncHost, Runtime: runtime.KindCladefs, Throwaway: req.Throwaway,
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
		var baseRes ensureBaseResult
		if err == nil {
			baseRes, err = e.cfg.CladefsFUSE.ensureProjectBase(
				bgCtx, p, heads, req.GetSharedBase(), baseSpec, prov,
				e.startManagedFreshen,
				func(baseID string) { e.setPreparingWorkspaceBase(req.Id, baseID) },
				e.sandboxEnv(),
				ensureBaseOpts{
					Budget:          e.freshenBudget,
					Now:             e.now,
					AllowBackground: req.GetSyncDir() != "",
					OnRequireBuild:  func() { e.cancelBgFreshen(p.Name) },
				},
			)
		}
		projBase := baseRes.Base
		if err == nil {
			e.recordWarmCladefsBase(p, projBase, goruntime.GOARCH)
			if baseRes.DeferBackground {
				e.scheduleOptionalFreshen(p, runtime.KindCladefs, req.GetSharedBase())
			}
		}
		var cell *pb.CladefsCell
		if err == nil {
			cell, _, err = e.cfg.CladefsFUSE.PrepareWorkspace(bgCtx, req.Id, projBase.Ref, req.GetFencingToken())
			if cladefs.IsGone(err) {
				e.cfg.CladefsFUSE.held.forget(p.Name, projBase.Ref)
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
		w.rec.Note = baseRes.Note
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
		w.ws().Reason = baseRes.Note
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
