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
	"maps"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/cladefs"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/storage"
)

// CladefsFUSEBackend is the cladefs backend whose workspaces' cells are
// mounted into the host filesystem via cladefsd's host FUSE mount
// (CLADEFSD_MOUNT), for nodes that do not run microVMs (e.g. 16 KiB arm64
// Apple Silicon or VMs without nested virtualization).
//
// Shared base images and project bases are versioned and synced to R2 and then
// dropped locally (DropCell), so local SSD holds only CLADEFSD_PART_CACHE and
// active workspace WALs. Workspaces run standard systemd chroot units against
// their mounted FUSE rootfs, leaving the host's local ZFS/btrfs store intact
// for --bench measurements.
type CladefsFUSEBackend struct {
	cladefsCore
	mountRoot       string
	mountStoresRoot string
	wsState         string // wsStatePath

	mu         sync.Mutex
	cfg        statedir.CladefsFUSEConfig
	wsMounts   map[string]fuseWSMount // wsID -> active FUSE mount
	unhealthy  bool
	recovering bool

	detachTimeout time.Duration      // optional MountDetach deadline override (tests; default 30s)
	cleanupMount  func(string, bool) // optional cleanupFUSEMountDir override (tests)

	// Testable hooks for FUSE mount health checks and ENOTCONN recovery.
	mountHealthFn func(string) error
	clearNosuidFn func(string) error
	isNotConnFn   func(error) bool
	detachDeadFn  func(string) error
	reconnectFn   func(context.Context) (uint64, error)
}

// newCladefsFUSEBackend is a CladefsFUSEBackend for cfg keeping its state in
// workerDir, with no store yet.
func newCladefsFUSEBackend(workerDir string, cfg statedir.CladefsFUSEConfig) *CladefsFUSEBackend {
	b := &CladefsFUSEBackend{
		cfg:             cfg,
		mountRoot:       cmp.Or(cfg.MountRoot, "/var/lib/cladefs/mnt"),
		mountStoresRoot: cmp.Or(cfg.MountStoresRoot, "/mnt/fxdax-scratch/cladefsd/mount-stores"),
		wsMounts:        map[string]fuseWSMount{},
	}
	b.cladefsCore = newCladefsCore(fuseFlavor, b, workerDir)
	b.wsState = filepath.Join(workerDir, "cladefs-fuse-ws.json")
	if cfg.TenantID > 0 {
		b.wsState = filepath.Join(workerDir, fmt.Sprintf("cladefs-fuse-ws-t%d.json", cfg.TenantID))
	}
	return b
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
	Tenant      uint64 `json:"tenant"`
	Cell        uint64 `json:"cell"`
	MountPath   string `json:"mountPath"`
	StorePath   string `json:"storePath,omitempty"`
	CaptureBase bool   `json:"captureBase,omitempty"`
}

// fuseStoreConfig is how a host-FUSE node reaches cladefsd.
func fuseStoreConfig(cfg statedir.CladefsFUSEConfig) cladefs.Config {
	return cladefs.Config{
		ControlSock: cfg.CladefsControlSock,
		CellRoot:    tenantCellRoot(cmp.Or(cfg.CellRoot, defaultCellRoot), cfg.TenantID),
		TenantID:    cfg.TenantID,
		DefaultCap:  fuseDiskBytes(cfg),
	}
}

func fuseDiskBytes(cfg statedir.CladefsFUSEConfig) uint64 {
	return cmp.Or(cfg.DiskBytes, uint64(64<<30))
}

// OpenCladefsFUSEBackend connects to cladefsd, loads cached R2 base refs and
// surviving workspace FUSE mounts, and verifies the Mount service is serving.
func OpenCladefsFUSEBackend(ctx context.Context, workerDir string, cfg statedir.CladefsFUSEConfig) (*CladefsFUSEBackend, error) {
	b := newCladefsFUSEBackend(workerDir, cfg)
	// If a previous cladefsd died and left mountRoot stuck at ENOTCONN, detach
	// it lazily so systemd-managed cladefsd can remount cleanly on startup.
	if err := detachDeadMount(b.mountRoot); err != nil {
		slog.Warn("detach dead cladefs FUSE mount on startup", "mountRoot", b.mountRoot, "err", err)
	}
	cstore, inc, err := waitForCladefsReconnect(ctx, fuseStoreConfig(cfg), 15*time.Second, func(ctx context.Context, s *cladefs.Store) (uint64, error) {
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
	if err := clearMountNosuid(b.mountRoot); err != nil {
		_ = cstore.Close()
		return nil, fmt.Errorf("enable setuid on cladefs FUSE mount %s: %w", b.mountRoot, err)
	}
	b.link = cladefsdLink{incarnation: inc}
	b.metricsURL = hostCladefsdMetricsURL
	b.store.Store(cstore)
	b.loadState()
	return b, nil
}

func (b *CladefsFUSEBackend) configSnapshot() statedir.CladefsFUSEConfig {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cfg
}

func (b *CladefsFUSEBackend) settings() cladefsSettings {
	cfg := b.configSnapshot()
	return cladefsSettings{tenant: cfg.TenantID, peerAddr: cfg.CladefsPeerAddr}
}

// UpdateConfig hot-reloads non-structural cladefs host-FUSE settings (default
// runtime, disk size, peer address) without disrupting active FUSE mounts.
func (b *CladefsFUSEBackend) UpdateConfig(cfg statedir.CladefsFUSEConfig) {
	b.mu.Lock()
	b.cfg = cfg
	b.mu.Unlock()
}

func (b *CladefsFUSEBackend) applyConfig(cfg statedir.WorkerConfig) {
	if cfg.CladefsFUSE != nil {
		b.UpdateConfig(*cfg.CladefsFUSE)
	}
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
// back online after a restart, clears nosuid on mountRoot, and refreshes the
// gRPC client, noting the new incarnation; the next heartbeat applies the
// peer mesh to it.
func (b *CladefsFUSEBackend) ReconnectCladefs(ctx context.Context) (uint64, error) {
	return b.reconnectStore(ctx, fuseStoreConfig(b.configSnapshot()), func(ctx context.Context, s *cladefs.Store) (uint64, error) {
		if _, err := s.MountList(ctx); err != nil {
			return 0, err
		}
		return s.Incarnation(ctx)
	}, func(*cladefs.Store) error {
		if err := clearMountNosuid(b.mountRoot); err != nil {
			return fmt.Errorf("enable setuid on cladefs FUSE mount %s: %w", b.mountRoot, err)
		}
		return nil
	})
}

// wsStatePath is where the backend keeps its workspace mounts across
// restarts, one file per tenant (which never changes while it runs).
func (b *CladefsFUSEBackend) wsStatePath() string { return b.wsState }

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

// loadState loads the cached bases and the workspace mounts a worker before
// left.
func (b *CladefsFUSEBackend) loadState() {
	b.loadCachedBases()
	if b.workerDir == "" {
		return
	}
	raw, err := os.ReadFile(b.wsStatePath())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("read cladefs FUSE workspace mounts", "path", b.wsStatePath(), "err", err)
		}
		return
	}
	var saved map[string]fuseWSMount
	if err := json.Unmarshal(raw, &saved); err != nil {
		slog.Warn("decode cladefs FUSE workspace mounts", "path", b.wsStatePath(), "err", err)
		return
	}
	b.mu.Lock()
	b.wsMounts = saved
	b.mu.Unlock()
}

func (b *CladefsFUSEBackend) saveWSStateLocked() {
	if b.workerDir == "" {
		return
	}
	if err := writeJSONAtomic(b.wsStatePath(), b.wsMounts); err != nil {
		slog.Warn("save cladefs FUSE workspace mounts", "path", b.wsStatePath(), "err", err)
	}
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

func (b *CladefsFUSEBackend) baseKey(spec baseimage.Spec) string {
	h := sha256.New()
	fmt.Fprintf(h, "fuse-v1\nbase=%s\ndisk=%d\n", spec.Key(), fuseDiskBytes(b.configSnapshot()))
	return spec.Key() + "-f" + hex.EncodeToString(h.Sum(nil))[:8]
}

func randomUnitSuffix() string {
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

// provisionBaseImage pulls spec's OS image into a new cell mounted on the
// host and runs prov's provisioning script chrooted in it.
func (b *CladefsFUSEBackend) provisionBaseImage(ctx context.Context, key string, spec baseimage.Spec, prov baseimage.Provisioner) (cladefs.CellRef, error) {
	cfg := b.configSnapshot()
	if err := b.ensureMountReady(ctx); err != nil {
		return cladefs.CellRef{}, fmt.Errorf("enable setuid on cladefs FUSE mount %s: %w", b.mountRoot, err)
	}
	cellID, shown, err := b.currentStore().MountAttach(ctx, cfg.TenantID, 0, fuseDiskBytes(cfg), nil, baseBuildUploadInterval)
	if err != nil {
		return cladefs.CellRef{}, fmt.Errorf("create cladefs FUSE base cell: %w", err)
	}
	mntDir := b.mountDir(cfg.TenantID, cellID)
	cleanupOnErr := func() {
		if err := b.detachMount(ctx, cfg.TenantID, cellID, mntDir, false, true); err != nil {
			slog.Warn("detach a failed cladefs FUSE base image", "cell", cellID, "err", err)
		}
		b.cleanupCell(ctx, cfg.TenantID, cellID)
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
	return cladefs.CellRef{
		RelPath:  "baseimages/fuse-" + key,
		HostPath: cladefs.FormatCellID(cfg.TenantID, cellID),
		Tenant:   cfg.TenantID,
		Cell:     cellID,
	}, nil
}

func (b *CladefsFUSEBackend) prepareBaseBuild(ctx context.Context, _ string) error {
	if err := b.ensureMountReady(ctx); err != nil {
		return fmt.Errorf("enable setuid on cladefs FUSE mount %s: %w", b.mountRoot, err)
	}
	return nil
}

// openBaseBuild syncs fork's head so Kind::Fxfs is visible to Mount.Attach,
// drops the local copy, and mounts it inside cladefsd's host FUSE mount
// with baseBuildUploadInterval.
func (b *CladefsFUSEBackend) openBaseBuild(ctx context.Context, fork cladefs.CellRef, t baseBuildTarget) (baseBuildCell, error) {
	st := b.currentStore()
	if err := st.SyncCell(ctx, fork.Tenant, fork.Cell); err != nil {
		b.cleanupCell(ctx, fork.Tenant, fork.Cell)
		return nil, fmt.Errorf("sync forked cell %d head: %w", fork.Cell, err)
	}
	if err := st.DropCell(ctx, fork.Tenant, fork.Cell); err != nil {
		b.cleanupCell(ctx, fork.Tenant, fork.Cell)
		return nil, fmt.Errorf("drop forked cell %d/%d: %w", fork.Tenant, fork.Cell, err)
	}
	deleteBucketCell := func(cell uint64) {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := st.DeleteBucketCell(cctx, fork.Tenant, cell); err != nil {
			slog.Warn("delete a forked cell that could not be mounted", "cell", cell, "err", err)
		}
	}
	cell, shown, err := st.MountAttach(ctx, fork.Tenant, fork.Cell, 0, nil, baseBuildUploadInterval)
	if err != nil {
		deleteBucketCell(fork.Cell)
		return nil, fmt.Errorf("mount attach forked cell %d: %w", fork.Cell, err)
	}
	if !shown {
		if err := b.detachMount(ctx, fork.Tenant, cell, "", false, true); err != nil {
			slog.Warn("detach a forked cell attached without its head", "cell", cell, "err", err)
		}
		deleteBucketCell(cell)
		return nil, fmt.Errorf("forked cell %d attached with shown=false (head kind missing)", cell)
	}
	return &fuseBaseBuild{b: b, tenant: fork.Tenant, cell: cell, mntDir: b.mountDir(fork.Tenant, cell),
		relPath: filepath.Join("bases", t.project, t.baseID)}, nil
}

// fuseBaseBuild is a project base build chrooted in its cell's host mount.
type fuseBaseBuild struct {
	b            *CladefsFUSEBackend
	tenant, cell uint64
	mntDir       string
	relPath      string
}

func (f *fuseBaseBuild) ref() cladefs.CellRef {
	return cladefs.CellRef{RelPath: f.relPath, HostPath: cladefs.FormatCellID(f.tenant, f.cell), Tenant: f.tenant, Cell: f.cell}
}

func (f *fuseBaseBuild) freshen(spec *managedFreshenSpec) { spec.RootDir = f.mntDir }

// finish detaches the mount, its writes flushed, once the freshen ran.
func (f *fuseBaseBuild) finish(ctx context.Context, h *managedFreshenHandle, ran bool) (float64, error) {
	if !ran {
		return 0, nil
	}
	h.SetFinalizing("mount_detach")
	start := time.Now()
	err := f.b.detachMount(ctx, f.tenant, f.cell, f.mntDir, true, false)
	secs := time.Since(start).Seconds()
	h.RecordPhase("mount_detach", start.UnixMilli(), time.Now().UnixMilli(), secs, "")
	if err != nil {
		return secs, fmt.Errorf("detach mount: %w", err)
	}
	return secs, nil
}

func (f *fuseBaseBuild) discard(ctx context.Context) {
	if err := f.b.detachMount(ctx, f.tenant, f.cell, f.mntDir, false, true); err != nil {
		slog.Warn("detach a failed cladefs FUSE base build", "cell", f.cell, "err", err)
	}
	f.b.cleanupCell(ctx, f.tenant, f.cell)
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

// forkCaptureAndMount forks an R2 version into a bucket-backed cladefs cell
// with uploadInterval so writes stay coalesced on local SSD until
// CaptureWorkspaceBase flushes them, and mounts it via cladefsd's host FUSE.
func (b *CladefsFUSEBackend) forkCaptureAndMount(ctx context.Context, from cladefs.CellRef, fencingToken uint64, uploadInterval time.Duration) (tenant, cell uint64, mountPath string, err error) {
	st := b.currentStore()
	forkRef, err := st.ForkVersion(ctx, from, false)
	if err != nil {
		return 0, 0, "", fmt.Errorf("fork capture R2 version %d/%x: %w", from.Cell, from.Version, err)
	}
	if err := st.SyncCell(ctx, forkRef.Tenant, forkRef.Cell); err != nil {
		b.cleanupCell(ctx, forkRef.Tenant, forkRef.Cell)
		return 0, 0, "", fmt.Errorf("sync capture cell %d: %w", forkRef.Cell, err)
	}
	if err := st.DropCell(ctx, forkRef.Tenant, forkRef.Cell); err != nil {
		b.cleanupCell(ctx, forkRef.Tenant, forkRef.Cell)
		return 0, 0, "", fmt.Errorf("drop capture cell %d: %w", forkRef.Cell, err)
	}
	var tok *uint64
	if fencingToken > 0 {
		t := fencingToken
		tok = &t
	}
	attachedCell, shown, err := st.MountAttach(ctx, forkRef.Tenant, forkRef.Cell, 0, tok, uploadInterval)
	if err != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		_ = st.DeleteBucketCell(cctx, forkRef.Tenant, forkRef.Cell)
		cancel()
		return 0, 0, "", fmt.Errorf("mount attach capture cell %d: %w", forkRef.Cell, err)
	}
	if !shown {
		_ = b.detachMount(ctx, forkRef.Tenant, attachedCell, "", false, true)
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		_ = st.DeleteBucketCell(cctx, forkRef.Tenant, attachedCell)
		cancel()
		return 0, 0, "", fmt.Errorf("capture cell %d attached with shown=false (kind missing)", attachedCell)
	}
	return forkRef.Tenant, attachedCell, b.mountDir(forkRef.Tenant, attachedCell), nil
}

// PrepareWorkspace forks baseRef into a cladefs cell and mounts it into
// cladefsd's host FUSE mount at /var/lib/cladefs/mnt/<tenant>/<cell>.
// Unless captureBase is true, the fork is ephemeral (0 R2 bytes).
func (b *CladefsFUSEBackend) PrepareWorkspace(ctx context.Context, wsID string, baseRef cladefs.CellRef, fencingToken uint64, captureBase bool) (*pb.CladefsCell, error) {
	b.mu.Lock()
	_, active := b.wsMounts[wsID]
	b.mu.Unlock()
	if active {
		return nil, fmt.Errorf("prepare cladefs FUSE workspace %s: active mount already exists: %w", wsID, storage.ErrBusy)
	}
	if err := b.ensureMountReady(ctx); err != nil {
		return nil, fmt.Errorf("enable setuid on cladefs FUSE mount %s: %w", b.mountRoot, err)
	}
	var (
		tenant uint64
		cellID uint64
		mntDir string
		err    error
	)
	if captureBase {
		tenant, cellID, mntDir, err = b.forkCaptureAndMount(ctx, baseRef, fencingToken, captureUploadInterval)
	} else {
		tenant, cellID, mntDir, err = b.forkEphemeralAndMount(ctx, baseRef)
	}
	if err != nil {
		return nil, fmt.Errorf("prepare cladefs FUSE workspace %s: %w", wsID, err)
	}
	b.mu.Lock()
	b.wsMounts[wsID] = fuseWSMount{Tenant: tenant, Cell: cellID, MountPath: mntDir, CaptureBase: captureBase}
	b.saveWSStateLocked()
	b.mu.Unlock()
	return &pb.CladefsCell{
		Tenant:       tenant,
		Cell:         cellID,
		FencingToken: fencingToken,
		Arch:         goruntime.GOARCH,
	}, nil
}

// CaptureWorkspaceBase detaches wsID's FUSE mount, versions and uploads the
// cell as project's new shared base, drops the local bucket copy, and re-mounts
// an ephemeral fork of the new version for subsequent commands in wsID.
func (b *CladefsFUSEBackend) CaptureWorkspaceBase(
	ctx context.Context,
	wsID, project, baseKey, setupHash string,
	stepHashes map[string]string,
	scratchEpoch int64,
	repos map[string]string,
	baseCommitTime, capturedAt int64,
) (*projectBase, *pb.CladefsCell, error) {
	b.mu.Lock()
	m, ok := b.wsMounts[wsID]
	b.mu.Unlock()
	if !ok || m.Cell == 0 {
		return nil, nil, fmt.Errorf("capture cladefs FUSE workspace %s: no active mount", wsID)
	}
	if err := b.detachMount(ctx, m.Tenant, m.Cell, m.MountPath, true, false); err != nil {
		return nil, nil, fmt.Errorf("detach cladefs FUSE workspace %s for capture: %w", wsID, err)
	}
	cellRef := cladefs.CellRef{Tenant: m.Tenant, Cell: m.Cell}
	ver, err := b.versionAndSync(ctx, cellRef)
	if err != nil {
		// Re-attach the existing cell so the workspace stays usable.
		_, _, _ = b.currentStore().MountAttach(ctx, m.Tenant, m.Cell, 0, nil, captureUploadInterval)
		return nil, nil, fmt.Errorf("version & sync cladefs FUSE workspace %s: %w", wsID, err)
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
	if err := b.currentStore().DropCell(ctx, baseRef.Tenant, baseRef.Cell); err != nil {
		slog.Warn("drop local copy of captured FUSE workspace base", "workspace", wsID, "cell", baseRef.Cell, "err", err)
	}
	tenant, newCellID, mntDir, err := b.forkEphemeralAndMount(ctx, baseRef)
	if err != nil {
		b.mu.Lock()
		delete(b.wsMounts, wsID)
		b.saveWSStateLocked()
		b.mu.Unlock()
		return held, nil, fmt.Errorf("re-mount ephemeral FUSE workspace %s after base capture: %w", wsID, err)
	}
	b.mu.Lock()
	b.wsMounts[wsID] = fuseWSMount{Tenant: tenant, Cell: newCellID, MountPath: mntDir, CaptureBase: false}
	b.saveWSStateLocked()
	b.mu.Unlock()
	return held, &pb.CladefsCell{
		Tenant: tenant,
		Cell:   newCellID,
		Arch:   goruntime.GOARCH,
	}, nil
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
