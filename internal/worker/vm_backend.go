package worker

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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

	cladefsdv1 "github.com/semistrict/cladefs/go/cladefsd/v1"
	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/cladefs"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/guestd"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/storage"
	"github.com/semistrict/hotbed/internal/vm"
)

// VMBackend coordinates cladefsd cell management, Firecracker microVMs, peer
// mesh updates (SetPeers), retired-host decommissioning (DecommissionHost),
// and R2-backed warm project bases for a hotbed worker with statedir.VMConfig.
type VMBackend struct {
	cfg         statedir.VMConfig
	workerDir   string
	store       atomic.Pointer[cladefs.Store]
	mgr         *vm.Manager
	rt          *runtime.GuestRuntime
	incarnation uint64
	memSeq      atomic.Uint64

	baseMu         sync.Mutex // serializes base image provisioning
	projectLocks   sync.Map   // project -> *sync.Mutex
	mu             sync.Mutex
	baseImages     map[string]cladefs.CellRef // vmBaseKey -> cached R2 base cell ref
	wsCells        map[string]cladefs.CellRef // wsID -> forked cell ref
	projectBases   map[string]*vmProjectBase  // project -> warm project base in R2
	sharedBases    map[string]*pb.CladefsCell // project -> advertised R2 shared base
	activeBuilds   map[string]struct{}        // in-flight base-* / prov-* VM IDs owned by this worker
	lastPeers      []string
	decommissioned map[uint64]bool
	retirer        baseRetirer
}

type vmProjectBase struct {
	Ref          cladefs.CellRef   `json:"ref"`
	BaseID       string            `json:"baseId"`
	Recipe       string            `json:"recipe"`
	BaseKey      string            `json:"baseKey,omitempty"`
	ScratchEpoch int64             `json:"scratchEpoch,omitempty"`
	Repos        map[string]string `json:"repos"`                // repoID -> checked-out HEAD commit
	StepHashes   map[string]string `json:"stepHashes,omitempty"` // step name -> last executed script hash
}

// selectProjectBaseSteps returns the subset of p's setup steps that must run
// when freshening from prev (nil for a cold build from the OS base image).
// On an incremental freshen, steps whose script hash changed (or that are new)
// always run, and --every-pull steps run whenever any repo commit moved.
func selectProjectBaseSteps(p *pb.HostedProject, prev *vmProjectBase, heads map[string]string) []*pb.Step {
	steps := p.GetSteps()
	if prev == nil || prev.StepHashes == nil {
		return steps
	}
	repoMoved := false
	for _, r := range p.GetRepos() {
		want := heads[r.GetId()]
		if want == "" || prev.Repos[r.GetId()] != want {
			repoMoved = true
			break
		}
	}
	var out []*pb.Step
	for _, s := range steps {
		prevHash, ran := prev.StepHashes[s.GetName()]
		if !ran || prevHash != s.GetHash() || (s.GetEveryPull() && repoMoved) {
			out = append(out, s)
		}
	}
	return out
}

func projectStepHashes(p *pb.HostedProject) map[string]string {
	out := make(map[string]string, len(p.GetSteps()))
	for _, s := range p.GetSteps() {
		out[s.GetName()] = s.GetHash()
	}
	return out
}

// tenantCellRoot returns the cell root directory (or colon-separated JBOD list)
// for a cladefs tenant. Tenant 0 (default/legacy org) uses baseRoot directly to
// preserve existing on-disk cell paths; non-zero org tenants each get an
// isolated tenant-<id> subdirectory under every configured JBOD root so
// cladefsd.Grant (Dir + Dirs) isolates tenants without losing JBOD striping.
func tenantCellRoot(baseRoot string, tenantID uint64) string {
	if tenantID == 0 {
		return baseRoot
	}
	sub := fmt.Sprintf("tenant-%d", tenantID)
	parts := filepath.SplitList(baseRoot)
	var scoped []string
	for _, r := range parts {
		if r == "" {
			continue
		}
		scoped = append(scoped, filepath.Join(r, sub))
	}
	if len(scoped) == 0 {
		return filepath.Join(baseRoot, sub)
	}
	return strings.Join(scoped, string(filepath.ListSeparator))
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

// OpenVMBackend connects to cladefsd, initializes the Firecracker VM manager,
// and reconnects to any surviving Firecracker VMs from a prior worker process.
func OpenVMBackend(ctx context.Context, workerDir string, cfg statedir.VMConfig, slice, gitAuthHostSock string) (*VMBackend, error) {
	cellRoot := tenantCellRoot(cmp.Or(cfg.CellRoot, "/mnt/fxdax-scratch/cladefsd/cells/hotbed"), cfg.TenantID)
	vmUID, vmGID := resolveVMUserIDs(cfg.VMUser)
	cstore, err := cladefs.Open(ctx, cladefs.Config{
		ControlSock: cfg.CladefsControlSock,
		VMSock:      cfg.CladefsVMSock,
		CellRoot:    cellRoot,
		TenantID:    cfg.TenantID,
		VMUID:       vmUID,
		VMGID:       vmGID,
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
		DisableDAX:         cfg.DisableDAX,
		CacheType:          cfg.CacheType,
		BridgeName:         cfg.BridgeName,
		BridgeCIDR:         cfg.BridgeCIDR,
		DNSServers:         cfg.DNSServers,
		Nofile:             cfg.Nofile,
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

	_ = ensureDirTraversable(workerDir)
	b := &VMBackend{
		cfg:            cfg,
		workerDir:      workerDir,
		mgr:            mgr,
		incarnation:    inc,
		baseImages:     map[string]cladefs.CellRef{},
		wsCells:        map[string]cladefs.CellRef{},
		projectBases:   map[string]*vmProjectBase{},
		sharedBases:    map[string]*pb.CladefsCell{},
		activeBuilds:   map[string]struct{}{},
		decommissioned: map[uint64]bool{},
	}
	b.store.Store(cstore)
	b.loadCachedBases()
	b.rt = runtime.NewGuestRuntime(b)
	return b, nil
}

func (b *VMBackend) currentStore() *cladefs.Store {
	return b.store.Load()
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

// UpdateConfig hot-reloads VM tuning, kernel path, guestd binary path, DNS,
// peer address, and default-runtime flag for all subsequent VM boots and base
// builds without disrupting running VMs.
func (b *VMBackend) UpdateConfig(cfg statedir.VMConfig) {
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

func (b *VMBackend) cacheDir() string {
	if b.cfg.TenantID > 0 {
		return filepath.Join(b.workerDir, fmt.Sprintf("vm-bases-t%d", b.cfg.TenantID))
	}
	return filepath.Join(b.workerDir, "vm-bases")
}

func (b *VMBackend) loadCachedBases() {
	b.retirer.load(b.cacheDir())
	ents, err := os.ReadDir(b.cacheDir())
	if err != nil {
		return
	}
	wantTenant := b.cfg.TenantID
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
			continue
		}
		if proj, ok := strings.CutPrefix(ent.Name(), "project-"); ok {
			proj = strings.TrimSuffix(proj, ".json")
			var pbBase vmProjectBase
			if json.Unmarshal(raw, &pbBase) == nil && pbBase.Ref.Cell > 0 && pbBase.Ref.Version > 0 && pbBase.Ref.Tenant == wantTenant {
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
// The store pointer stays non-nil (closed) so any racing goroutine receives a
// clean gRPC error rather than panicking on a nil pointer.
func (b *VMBackend) Close() error {
	if st := b.currentStore(); st != nil {
		return st.Close()
	}
	return nil
}

// Runtime returns the GuestRuntime implementing runtime.WorkspaceRuntime.
func (b *VMBackend) Runtime() runtime.WorkspaceRuntime { return b.rt }

// DefaultRuntime returns "vm".
func (b *VMBackend) DefaultRuntime() string {
	return runtime.KindVM
}

// Runtimes returns the runtimes supported by this worker.
func (b *VMBackend) Runtimes() []string {
	return []string{runtime.KindVM}
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

// SampleObjectStore fetches cumulative cladefsd R2 object-store counters.
func (b *VMBackend) SampleObjectStore(ctx context.Context) *pb.ObjectStoreStats {
	return sampleCladefsObjectStore(ctx, b.currentStore())
}

func sampleCladefsObjectStore(ctx context.Context, store *cladefs.Store) *pb.ObjectStoreStats {
	if store == nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	resp, err := store.GetCounters(cctx)
	if err != nil {
		slog.Warn("sample cladefsd object-store counters", "err", err)
		return nil
	}
	out := objectStoreStatsFromCounters(resp)
	sctx, scancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer scancel()
	if stats, serr := store.GetStats(sctx, &cladefsdv1.GetStatsRequest{
		WaitMs:             500,
		LeaveOutHistograms: true,
	}); serr == nil {
		applyCladefsStats(out, stats)
	}
	scrapeLocalCladefsMetrics(sctx, "http://127.0.0.1:9184/metrics", out)
	return out
}

func applyCladefsStats(out *pb.ObjectStoreStats, stats *cladefsdv1.GetStatsResponse) {
	if out == nil || stats == nil {
		return
	}
	g := stats.GetGauges()
	out.ExtentCacheBytes = g["memory_bytes"]
	out.ExtentCacheCapacityBytes = g["memory_capacity_bytes"]
	out.ExtentCachePinnedEntries = g["extent_cache_pinned_entries"]
	out.ExtentCacheIdleEntries = g["extent_cache_idle_entries"]
	out.WindowMappedBytes = g["window_mapped_bytes"]
	out.OpenCells = uint32(g["stores_open"])
	out.HostDiskCellsBytes = g["disk_bytes"]
	if c := stats.GetCounters(); c != nil {
		out.CacheTrims = c["cache_trims"]
	}
	vms := stats.GetVms()
	out.ActiveVms = uint32(len(vms))
	for _, v := range vms {
		gc := v.GetGuest().GetCounters()
		if len(gc) == 0 {
			continue
		}
		out.DaxWindowFull += gc["window_full"]
		out.DaxWindowReclaims += gc["window_reclaims"]
		out.DaxRegionsFull += gc["window_regions_full"]
		out.DaxWritesHeldBySealsNs += gc["writes_held_by_seals_ns"]
		out.DaxDeviceReadBytes += gc["device_read_bytes"]
		out.DaxDeviceReadsNs += gc["device_reads_ns"]
	}
}

var cladefsMetricsHTTP = &http.Client{Timeout: 500 * time.Millisecond}

func scrapeLocalCladefsMetrics(ctx context.Context, url string, out *pb.ObjectStoreStats) bool {
	if out == nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := cladefsMetricsHTTP.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return false
	}
	return applyCladefsPrometheusMetrics(out, string(body))
}

func applyCladefsPrometheusMetrics(out *pb.ObjectStoreStats, text string) bool {
	if out == nil {
		return false
	}
	var (
		matched          bool
		hostDiskSize     uint64
		hostDiskFree     uint64
		hostDiskCells    uint64
		hostDiskCache    uint64
		hostDiskOther    uint64
		uploadPending    uint64
		sawHostDiskCells bool
	)
	for _, line := range strings.Split(text, "\n") {
		name, labels, val, ok := parsePromLine(line)
		if !ok {
			continue
		}
		switch name {
		case "cladefsd_part_cache_bytes":
			out.PartCacheBytes = val
			matched = true
		case "cladefsd_part_cache_capacity_bytes":
			out.PartCacheCapacityBytes = val
			matched = true
		case "cladefsd_host_disk_size_bytes":
			hostDiskSize += val
			matched = true
		case "cladefsd_host_disk_free_bytes":
			hostDiskFree += val
			matched = true
		case "cladefsd_host_disk_stores_bytes":
			hostDiskCells += val
			sawHostDiskCells = true
			matched = true
		case "cladefsd_host_disk_cache_bytes":
			hostDiskCache += val
			matched = true
		case "cladefsd_host_disk_other_bytes":
			hostDiskOther += val
			matched = true
		case "cladefsd_host_disk_pressure":
			if val > 0 {
				out.HostDiskPressure = true
			}
			matched = true
		case "cladefsd_mount_cells":
			if labels == `state="attached"` {
				out.MountCellsAttached = uint32(val)
				matched = true
			}
		case "cladefsd_page_cache_bytes":
			out.PageCacheBytes = val
			matched = true
		case "cladefsd_page_cache_dirty_bytes":
			out.PageCacheDirtyBytes = val
			matched = true
		case "cladefsd_page_cache_limit_bytes":
			out.PageCacheLimitBytes = val
			matched = true
		case "cladefsd_peer_parts_total":
			if labels == `outcome="given"` {
				out.PeerPartsGiven = val
				matched = true
			}
		case "cladefsd_peer_served_parts_total":
			if labels == `outcome="served"` {
				out.PeerPartsServed = val
				matched = true
			}
		case "cladefsd_upload_pending_bytes":
			uploadPending += val
			matched = true
		case "cladefsd_memory_pressure":
			if val > 0 {
				out.MemoryPressure = true
			}
			matched = true
		}
	}
	if hostDiskSize > 0 {
		out.HostDiskSizeBytes = hostDiskSize
		out.HostDiskFreeBytes = hostDiskFree
		out.HostDiskCacheBytes = hostDiskCache
		out.HostDiskOtherBytes = hostDiskOther
	}
	if sawHostDiskCells {
		out.HostDiskCellsBytes = hostDiskCells
	}
	if uploadPending > 0 {
		out.UploadPendingBytes = uploadPending
	}
	return matched
}

// parsePromLine splits a Prometheus text exposition sample line into its metric
// name, raw label block (without braces), and integer value.
func parsePromLine(line string) (name, labels string, val uint64, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] == '#' {
		return "", "", 0, false
	}
	var rest string
	if i := strings.IndexByte(line, '{'); i >= 0 {
		j := strings.IndexByte(line[i+1:], '}')
		if j < 0 {
			return "", "", 0, false
		}
		name = line[:i]
		labels = line[i+1 : i+1+j]
		rest = strings.TrimSpace(line[i+1+j+1:])
	} else {
		var found bool
		name, rest, found = strings.Cut(line, " ")
		if !found {
			return "", "", 0, false
		}
		rest = strings.TrimSpace(rest)
	}
	if sp := strings.IndexByte(rest, ' '); sp >= 0 {
		rest = rest[:sp]
	}
	v, err := parsePromUint64(rest)
	if err != nil {
		return "", "", 0, false
	}
	return name, labels, v, true
}

func parsePromUint64(s string) (uint64, error) {
	if v, err := strconv.ParseUint(s, 10, 64); err == nil {
		return v, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid metric value %q", s)
	}
	return uint64(f), nil
}

func objectStoreStatsFromCounters(resp interface {
	GetCommitWaitNs() uint64
	GetPageInBytes() uint64
	GetFetchedBytes() uint64
	GetObjectStorage() *cladefsdv1.ObjectStorageStats
}) *pb.ObjectStoreStats {
	if resp == nil {
		return nil
	}
	out := &pb.ObjectStoreStats{
		CommitWaitNs: resp.GetCommitWaitNs(),
		PageInBytes:  resp.GetPageInBytes(),
		FetchedBytes: resp.GetFetchedBytes(),
	}
	if os := resp.GetObjectStorage(); os != nil {
		out.DownloadedBytes = os.GetDownloadedBytes()
		for k, v := range os.GetUploadedBytes() {
			out.UploadedBytes += v
			switch k {
			case "segments":
				out.UploadedSegmentsBytes += v
			case "extents":
				out.UploadedExtentsBytes += v
			case "checkpoints":
				out.UploadedCheckpointsBytes += v
			default:
				out.UploadedOtherBytes += v
			}
		}
		for k, v := range os.GetRequests() {
			switch k {
			case "get", "get-range":
				out.GetRequests += v
			case "create", "update", "overwrite":
				out.PutRequests += v
			default:
				out.OtherRequests += v
			}
		}
		for _, v := range os.GetFailures() {
			out.FailedRequests += v
		}
	}
	return out
}

// ApplyHeartbeat reconciles cladefs peer mesh (SetPeers) and retired-host
// decommissioning (DecommissionHost) from HeartbeatResponse.
func (b *VMBackend) ApplyHeartbeat(ctx context.Context, peers []string, decommission []uint64) {
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
	b.retirer.sweep(ctx, st, "vm-retire", time.Now())
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

// vmBaseKey identifies the OS-level VM rootfs base cell. hotbed-guestd is
// attached at VM boot time via a separate read-only virtio-blk tools disk, so
// guestd upgrades never invalidate OS or project VM bases.
func (b *VMBackend) vmBaseKey(spec baseimage.Spec) string {
	cfg := b.configSnapshot()
	baseKey := spec.Key()
	h := sha256.New()
	fmt.Fprintf(h, "disk=%d\ndns=%s\n", cfg.DiskBytes, strings.Join(cfg.DNSServers, ","))
	return baseKey + "-" + hex.EncodeToString(h.Sum(nil))[:8]
}

func (b *VMBackend) trackBuild(vmID string) func() {
	b.mu.Lock()
	if b.activeBuilds == nil {
		b.activeBuilds = map[string]struct{}{}
	}
	b.activeBuilds[vmID] = struct{}{}
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		delete(b.activeBuilds, vmID)
		b.mu.Unlock()
	}
}

func (b *VMBackend) cleanupCell(ctx context.Context, rel string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	_ = b.currentStore().DeleteCell(cctx, rel)
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

// makeVersionAndSyncAfterStop versions and syncs rel after a build VM exits.
// cladefsd tears down the VM's virtio-extent session asynchronously on socket
// EOF, so an immediate MakeVersion can transiently fail with EBUSY (storage.ErrBusy)
// even after Firecracker has exited. Retrying EBUSY avoids discarding a finished
// multi-minute VM base build.
func makeVersionAndSyncAfterStop(
	ctx context.Context,
	makeAndSync func(context.Context) (uint64, error),
	maxWait time.Duration,
	initialBackoff time.Duration,
) (uint64, error) {
	deadline := time.Now().Add(maxWait)
	delay := initialBackoff
	for {
		ver, err := makeAndSync(ctx)
		if err == nil || !errors.Is(err, storage.ErrBusy) {
			return ver, err
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return 0, err
		}
		slog.Warn("cladefsd cell still busy right after build VM exit; retrying MakeVersion", "retryIn", delay, "err", err)
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return 0, ctx.Err()
		case <-t.C:
		}
		delay = min(delay*2, time.Second)
	}
}

// EnsureProvisionedBaseImage ensures the shared provisioned Debian + toolchains
// VM rootfs base cell exists, is versioned, and is synced to R2.
func (b *VMBackend) EnsureProvisionedBaseImage(ctx context.Context, spec baseimage.Spec) (cladefs.CellRef, error) {
	spec = spec.Normalized()
	cfg := b.configSnapshot()
	key := b.vmBaseKey(spec)

	b.baseMu.Lock()
	defer b.baseMu.Unlock()

	b.mu.Lock()
	if ref, ok := b.baseImages[key]; ok && ref.Cell > 0 && ref.Version > 0 {
		b.mu.Unlock()
		b.retirer.sweep(ctx, b.currentStore(), "vm-retire", time.Now())
		return ref, nil
	}
	b.mu.Unlock()

	rel := "baseimages/vm-" + key
	provID := "prov-" + key
	if b.mgr != nil {
		if err := b.mgr.EnsureNotRunning(ctx, provID); err != nil {
			return cladefs.CellRef{}, fmt.Errorf("prepare VM base image %s: %w", key, err)
		}
	}
	// Clean any interrupted previous build at rel.
	if err := b.currentStore().DeleteCell(ctx, rel); err != nil {
		return cladefs.CellRef{}, fmt.Errorf("clear stale VM base image cell %s: %w", key, err)
	}

	tarPath := filepath.Join(b.currentStore().PickRoot(), "vm-"+key+".tar")
	defer os.Remove(tarPath)
	slog.Info("building Firecracker VM base image", "key", key, "image", spec.Image)
	if err := vm.BuildGuestTar(ctx, vm.GuestImageOptions{
		ImageRef:   spec.Image,
		Arch:       spec.Arch,
		DNSServers: cfg.DNSServers,
		OutTar:     tarPath,
	}); err != nil {
		return cladefs.CellRef{}, fmt.Errorf("build VM guest base tar: %w", err)
	}
	rawRef, err := b.currentStore().ImportImage(ctx, rel, tarPath, cfg.DiskBytes)
	if err != nil {
		return cladefs.CellRef{}, fmt.Errorf("import VM base image into cladefsd: %w", err)
	}

	// Boot a temporary Firecracker VM on rawRef to run DefaultProvisionScript
	// (installs git, gh, build-essential, rustup/cargo, tracers, non-root user).
	if spec.Script != "" {
		// Re-open the already-synced imported cell with baseBuildUploadInterval
		// so cladefsd coalesces provisioning writes into fuller R2 segments;
		// MakeVersionAndSync flushes everything at the end (question 534).
		if err := b.currentStore().DropCell(ctx, rel); err != nil {
			b.cleanupCell(ctx, rel)
			return cladefs.CellRef{}, fmt.Errorf("drop VM base image %s before take: %w", key, err)
		}
		takenRef, err := b.currentStore().TakeCell(ctx, rel, rawRef.Tenant, rawRef.Cell, cfg.DiskBytes, nil, baseBuildUploadInterval)
		if err != nil {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			_ = b.currentStore().DeleteBucketCell(cctx, filepath.Join("vm-retire", fmt.Sprintf("fail-%d-%d", rawRef.Tenant, rawRef.Cell)), rawRef.Tenant, rawRef.Cell)
			cancel()
			return cladefs.CellRef{}, fmt.Errorf("re-open VM base image %s with upload interval: %w", key, err)
		}
		rawRef = takenRef

		untrack := b.trackBuild(provID)
		inst, err := b.mgr.Start(ctx, vm.LaunchSpec{
			ID:            provID,
			CellHostPath:  rawRef.HostPath,
			CladefsVMSock: b.currentStore().VMSock(),
		})
		if err != nil {
			untrack()
			b.cleanupCell(ctx, rel)
			return cladefs.CellRef{}, fmt.Errorf("boot provisioning VM %s: %w", provID, err)
		}
		_ = inst
		_, runErr := b.runGuestCommand(ctx, provID, "provision", "/", []string{"/bin/bash", "-c", spec.Script}, []string{
			"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			"HOME=/root",
		}, 0, 0)
		stopErr := b.stopBuildVM(ctx, provID, runErr == nil)
		untrack()
		if runErr != nil {
			if stopErr == nil {
				b.cleanupCell(ctx, rel)
			}
			return cladefs.CellRef{}, fmt.Errorf("provision VM base image %s: %w", key, runErr)
		}
		if stopErr != nil {
			return cladefs.CellRef{}, fmt.Errorf("stop provisioning VM %s: %w", provID, stopErr)
		}
	}

	ver, err := makeVersionAndSyncAfterStop(ctx, func(ctx context.Context) (uint64, error) {
		return b.currentStore().MakeVersionAndSync(ctx, rel, 2)
	}, 15*time.Second, 100*time.Millisecond)
	if err != nil {
		b.cleanupCell(ctx, rel)
		return cladefs.CellRef{}, fmt.Errorf("version & sync VM base image %s to R2: %w", key, err)
	}
	// Drop the local base cell directory now that its version is durable in R2;
	// subsequent forks read from R2 via the local SSD CLADEFSD_PART_CACHE.
	_ = b.currentStore().DropCell(ctx, rel)
	rawRef.Version = ver
	b.mu.Lock()
	if b.baseImages == nil {
		b.baseImages = map[string]cladefs.CellRef{}
	}
	superseded := collectSupersededBaseImages(b.baseImages, key, rawRef.Cell)
	b.baseImages[key] = rawRef
	b.mu.Unlock()
	b.saveCacheJSON("baseimage-"+key+".json", rawRef)
	now := time.Now()
	if b.workerDir != "" {
		retireSupersededBaseImages(b.cacheDir(), superseded, &b.retirer, now)
	} else {
		retireSupersededBaseImages("", superseded, &b.retirer, now)
	}
	b.retirer.sweep(ctx, b.currentStore(), "vm-retire", now)
	slog.Info("VM base image provisioned and synced to R2", "key", key, "cell", rawRef.Cell, "version", rawRef.Version)
	return rawRef, nil
}

func vmProjectRecipe(baseKey string, p *pb.HostedProject, heads map[string]string) string {
	h := sha256.New()
	fmt.Fprintf(h, "vm-v2\nbase=%s\narch=%s\nproject=%s\nepoch=%d\n", baseKey, goruntime.GOARCH, p.GetName(), p.GetScratchEpoch())
	for _, r := range p.GetRepos() {
		fmt.Fprintf(h, "repo=%s|%s|%s|%s\n", r.GetId(), r.GetUrl(), r.GetBranch(), heads[r.GetId()])
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
// warm cladefs project base with all project repos checked out at heads under
// /work/<repo> and setup steps executed inside a Firecracker VM, then uploads
// it to R2 and publishes it in sharedBases for heartbeat advertisement.
// When an existing project base matches baseKey and ScratchEpoch, it forks
// incrementally from that base and runs only new/changed and --every-pull steps.
func (b *VMBackend) EnsureProjectBase(
	ctx context.Context,
	p *pb.HostedProject,
	heads map[string]string,
	existingShared *pb.CladefsCell,
	spec baseimage.Spec,
	sandboxEnv []string,
	onBaseTarget func(baseID string),
	runFreshen managedFreshenRunner,
) (*vmProjectBase, error) {
	spec = spec.Normalized()
	baseKey := b.vmBaseKey(spec)
	recipe := vmProjectRecipe(baseKey, p, heads)
	baseID := fmt.Sprintf("%s-vm-%s", p.GetName(), recipe[:8])
	if onBaseTarget != nil {
		onBaseTarget(baseID)
	}

	lock := b.projectLock(p.GetName())
	lock.Lock()
	defer lock.Unlock()

	// Fast path 1: local cached project base matches recipe (including repo HEADs).
	b.mu.Lock()
	cur := b.projectBases[p.GetName()]
	if cur != nil && cur.Recipe == recipe && cur.Ref.Cell > 0 && cur.Ref.Version > 0 {
		out := *cur
		b.mu.Unlock()
		b.retirer.sweep(ctx, b.currentStore(), "vm-retire", time.Now())
		return &out, nil
	}
	var prevBase *vmProjectBase
	coldReason := ""
	switch {
	case cur == nil || cur.Ref.Cell == 0 || cur.Ref.Version == 0:
		coldReason = "no_previous_base"
	case cur.ScratchEpoch != p.GetScratchEpoch():
		coldReason = "scratch_epoch"
	case cur.BaseKey != baseKey:
		coldReason = "base_image_changed"
	default:
		cp := *cur
		prevBase = &cp
	}
	b.mu.Unlock()

	// Fast path 2: controller advertised an up-to-date shared base in R2!
	wantTenant := b.configSnapshot().TenantID
	if st := b.currentStore(); st != nil {
		wantTenant = st.TenantID()
	}
	if existingShared != nil && existingShared.Cell > 0 && existingShared.Version > 0 && existingShared.Recipe == recipe && existingShared.Tenant == wantTenant {
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
			BaseID:       cmp.Or(existingShared.BaseId, p.GetName()+"-vm-"+recipe[:8]),
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
		b.retirer.sweep(ctx, b.currentStore(), "vm-retire", time.Now())
		return adopted, nil
	}

	rel := filepath.Join("bases", p.GetName(), baseID)
	vmID := "base-" + baseID
	if b.mgr != nil {
		if err := b.mgr.EnsureNotRunning(ctx, vmID); err != nil {
			return nil, fmt.Errorf("prepare VM project base %s: %w", baseID, err)
		}
	}
	if err := b.currentStore().DeleteCell(ctx, rel); err != nil {
		return nil, fmt.Errorf("clear stale VM project base cell %s: %w", baseID, err)
	}

	createdAt := time.Now().UnixMilli()
	prepStart := time.Now()
	var prepPhases []*pb.FreshenPhase
	fromBaseID := ""

	// Prefer incremental fork from the previous warm project base when compatible.
	var forkRef cladefs.CellRef
	var err error
	if prevBase != nil {
		forkStart := time.Now()
		forkStartMs := forkStart.UnixMilli()
		forkRef, err = b.currentStore().ForkVersion(ctx, prevBase.Ref, rel, false)
		if err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			slog.Warn("incremental VM project base fork failed; falling back to cold base",
				"project", p.GetName(), "prevBase", prevBase.BaseID, "err", err)
			b.cleanupCell(ctx, rel)
			prevBase = nil
			coldReason = "incremental_fork_failed"
		} else {
			forkEndMs := time.Now().UnixMilli()
			fromBaseID = prevBase.BaseID
			prepPhases = append(prepPhases, &pb.FreshenPhase{
				Name:      "fork",
				StartedAt: forkStartMs,
				EndedAt:   &forkEndMs,
				Seconds:   time.Since(forkStart).Seconds(),
				Detail:    "from " + prevBase.BaseID,
			})
		}
	}
	if prevBase == nil {
		imgStart := time.Now()
		imgStartMs := imgStart.UnixMilli()
		baseImgRef, berr := b.EnsureProvisionedBaseImage(ctx, spec)
		if berr != nil {
			return nil, berr
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
		forkRef, err = b.currentStore().ForkVersion(ctx, baseImgRef, rel, false)
		if err != nil {
			return nil, fmt.Errorf("fork VM project base %s: %w", baseID, err)
		}
		forkEndMs := time.Now().UnixMilli()
		prepPhases = append(prepPhases, &pb.FreshenPhase{
			Name:      "fork",
			StartedAt: forkStartMs,
			EndedAt:   &forkEndMs,
			Seconds:   time.Since(forkStart).Seconds(),
			Detail:    "from " + baseKey,
		})
	}

	// Re-open the forked base cell via TakeCell with baseBuildUploadInterval so
	// cladefsd coalesces segment uploads while the VM clones repos and runs
	// setup steps; MakeVersionAndSync flushes everything at the end (question 534).
	cfg := b.configSnapshot()
	takeStart := time.Now()
	takeStartMs := takeStart.UnixMilli()
	if err := b.currentStore().SyncCell(ctx, rel); err != nil {
		b.cleanupCell(ctx, rel)
		return nil, fmt.Errorf("sync forked VM project base %s: %w", baseID, err)
	}
	if err := b.currentStore().DropCell(ctx, rel); err != nil {
		b.cleanupCell(ctx, rel)
		return nil, fmt.Errorf("drop forked VM project base %s before take: %w", baseID, err)
	}
	takenRef, err := b.currentStore().TakeCell(ctx, rel, forkRef.Tenant, forkRef.Cell, cfg.DiskBytes, nil, baseBuildUploadInterval)
	if err != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_ = b.currentStore().DeleteBucketCell(cctx, filepath.Join("vm-retire", fmt.Sprintf("fail-%d-%d", forkRef.Tenant, forkRef.Cell)), forkRef.Tenant, forkRef.Cell)
		cancel()
		return nil, fmt.Errorf("re-open VM project base %s with upload interval: %w", baseID, err)
	}
	forkRef = takenRef
	takeEndMs := time.Now().UnixMilli()
	prepPhases = append(prepPhases, &pb.FreshenPhase{
		Name:      "take_cell",
		StartedAt: takeStartMs,
		EndedAt:   &takeEndMs,
		Seconds:   time.Since(takeStart).Seconds(),
	})
	prepSeconds := time.Since(prepStart).Seconds()

	stepsToRun := selectProjectBaseSteps(p, prevBase, heads)
	script := buildCladefsFreshenScript(p, heads, stepsToRun, false)

	var (
		repoHeads  map[string]string
		handle     *managedFreshenHandle
		freshenRes managedFreshenResult
	)
	var finalizeSeconds float64
	if runFreshen != nil {
		env := append(slices.Clone(sandboxEnv),
			"HOME="+spec.Home,
			"HOTBED_GIT_CRED_SOCK=/run/hotbed/git-auth/git-cred.sock",
		)
		var (
			vmStarted        atomic.Bool
			stopMu           sync.Mutex
			startFailStopErr error
			bootPhaseMu      sync.Mutex
			bootStartMs      int64
			bootEndMs        int64
			bootSecs         float64
		)
		untrack := b.trackBuild(vmID)
		handle, err = runFreshen(ctx, managedFreshenSpec{
			Project:     p,
			BaseID:      baseID,
			Recipe:      recipe,
			Arch:        goruntime.GOARCH,
			Runtime:     runtime.KindVM,
			Cold:        prevBase == nil,
			ColdReason:  coldReason,
			FromBaseID:  fromBaseID,
			CreatedAt:   createdAt,
			PrepSeconds: prepSeconds,
			PrepPhases:  prepPhases,
			PrevBase:    prevBase,
			StepsToRun:  stepsToRun,
			Heads:       heads,
			Script:      script,
			Env:         env,
			VMID:        vmID,
			BeforeStart: func(startCtx context.Context) error {
				t0 := time.Now()
				s0 := t0.UnixMilli()
				if _, err := b.mgr.Start(startCtx, vm.LaunchSpec{
					ID:            vmID,
					CellHostPath:  forkRef.HostPath,
					CladefsVMSock: b.currentStore().VMSock(),
				}); err != nil {
					return fmt.Errorf("boot project base VM %s: %w", vmID, err)
				}
				bootPhaseMu.Lock()
				bootStartMs = s0
				bootEndMs = time.Now().UnixMilli()
				bootSecs = time.Since(t0).Seconds()
				bootPhaseMu.Unlock()
				vmStarted.Store(true)
				return nil
			},
			OnStartFail: func() {
				if vmStarted.Swap(false) {
					err := b.stopBuildVM(context.Background(), vmID, false)
					stopMu.Lock()
					startFailStopErr = err
					stopMu.Unlock()
				}
			},
		})
		if err != nil {
			untrack()
			b.cleanupCell(ctx, rel)
			return nil, fmt.Errorf("queue VM project base %s: %w", baseID, err)
		}
		var waitErr error
		freshenRes, waitErr = handle.Wait(ctx)
		bootPhaseMu.Lock()
		bs0, bs1, bsecs := bootStartMs, bootEndMs, bootSecs
		bootPhaseMu.Unlock()
		if bs0 > 0 {
			handle.RecordPhase("vm_boot", bs0, bs1, bsecs, vmID)
		}
		runOK := waitErr == nil && freshenRes.State == succeeded
		stopMu.Lock()
		stopErr := startFailStopErr
		stopMu.Unlock()
		if vmStarted.Swap(false) {
			if runOK {
				handle.SetFinalizing("vm_stop")
			}
			stopStart := time.Now()
			stopStartMs := stopStart.UnixMilli()
			stopErr = b.stopBuildVM(ctx, vmID, runOK)
			stopSecs := time.Since(stopStart).Seconds()
			handle.RecordPhase("vm_stop", stopStartMs, time.Now().UnixMilli(), stopSecs, "")
			if runOK {
				finalizeSeconds += stopSecs
			}
		}
		untrack()
		if waitErr != nil {
			if stopErr == nil {
				b.cleanupCell(ctx, rel)
			}
			freshenRes.FinalizeSeconds = finalizeSeconds
			handle.Finish(false, freshenRes, cladefs.CellRef{}, waitErr.Error())
			return nil, fmt.Errorf("freshen VM project base %s (command %s): %w", baseID, handle.CommandID(), waitErr)
		}
		if freshenRes.State != succeeded {
			if stopErr == nil {
				b.cleanupCell(ctx, rel)
			}
			detail := cmp.Or(freshenRes.Reason, "freshen "+stateName(freshenRes.State))
			freshenRes.FinalizeSeconds = finalizeSeconds
			handle.Finish(false, freshenRes, cladefs.CellRef{}, detail)
			return nil, fmt.Errorf("freshen VM project base %s failed (command %s): %s", baseID, handle.CommandID(), detail)
		}
		if stopErr != nil {
			freshenRes.FinalizeSeconds = finalizeSeconds
			handle.Finish(false, freshenRes, cladefs.CellRef{}, fmt.Sprintf("stop project base VM %s: %v", vmID, stopErr))
			return nil, fmt.Errorf("stop project base VM %s: %w", vmID, stopErr)
		}
		repoHeads = freshenRes.RepoHeads
	} else {
		untrack := b.trackBuild(vmID)
		if _, err := b.mgr.Start(ctx, vm.LaunchSpec{
			ID:            vmID,
			CellHostPath:  forkRef.HostPath,
			CladefsVMSock: b.currentStore().VMSock(),
		}); err != nil {
			untrack()
			b.cleanupCell(ctx, rel)
			return nil, fmt.Errorf("boot project base VM %s: %w", vmID, err)
		}
		env := append([]string{}, cacheEnv()...)
		env = append(env, sandboxEnv...)
		env = append(env,
			"HOME="+spec.Home,
			"XDG_RUNTIME_DIR="+fmt.Sprintf("/run/user/%d", spec.UID),
			"PATH="+strings.Join(cachePath, ":")+":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			"HOTBED_GIT_CRED_SOCK=/run/hotbed/git-auth/git-cred.sock",
		)
		out, runErr := b.runGuestCommand(ctx, vmID, "freshen-"+baseID, "/work", []string{"/bin/bash", "-c", script}, env, spec.UID, spec.GID)
		stopErr := b.stopBuildVM(ctx, vmID, runErr == nil)
		untrack()
		if runErr != nil {
			if stopErr == nil {
				b.cleanupCell(ctx, rel)
			}
			return nil, fmt.Errorf("freshen VM project base %s: %w", baseID, runErr)
		}
		if stopErr != nil {
			return nil, fmt.Errorf("stop project base VM %s: %w", vmID, stopErr)
		}
		repoHeads = map[string]string{}
		for _, line := range strings.Split(out, "\n") {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "::hotbed-head:: "); ok {
				parts := strings.Fields(rest)
				if len(parts) == 2 {
					repoHeads[parts[0]] = parts[1]
				}
			}
		}
	}

	handle.SetFinalizing("r2_sync")
	syncStart := time.Now()
	syncStartMs := syncStart.UnixMilli()
	ver, err := makeVersionAndSyncAfterStop(ctx, func(ctx context.Context) (uint64, error) {
		return b.currentStore().MakeVersionAndSync(ctx, rel, 2)
	}, 15*time.Second, 100*time.Millisecond)
	syncSecs := time.Since(syncStart).Seconds()
	finalizeSeconds += syncSecs
	handle.RecordPhase("r2_sync", syncStartMs, time.Now().UnixMilli(), syncSecs, "")
	freshenRes.FinalizeSeconds = finalizeSeconds
	if err != nil {
		b.cleanupCell(ctx, rel)
		handle.Finish(false, freshenRes, cladefs.CellRef{}, fmt.Sprintf("sync to R2: %v", err))
		return nil, fmt.Errorf("sync VM project base %s to R2: %w", baseID, err)
	}
	// Drop the local project base cell directory now that it is durable in R2;
	// local SSD stays strictly a cache (CLADEFSD_PART_CACHE) + active workspaces.
	_ = b.currentStore().DropCell(ctx, rel)
	forkRef.Version = ver
	handle.Finish(true, freshenRes, forkRef, "")

	if len(repoHeads) == 0 && len(heads) > 0 {
		repoHeads = make(map[string]string, len(heads))
		for k, v := range heads {
			repoHeads[k] = v
		}
	}

	res := &vmProjectBase{
		Ref:          forkRef,
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
		Tenant:  forkRef.Tenant,
		Cell:    forkRef.Cell,
		Version: forkRef.Version,
	}
	b.mu.Unlock()
	b.saveCacheJSON("project-"+p.GetName()+".json", res)
	if superseded != nil && superseded.Ref.Cell > 0 && superseded.Ref.Cell != forkRef.Cell {
		b.retirer.enqueue(superseded.Ref.Tenant, superseded.Ref.Cell, superseded.BaseID, time.Now())
	}
	b.retirer.sweep(ctx, b.currentStore(), "vm-retire", time.Now())
	slog.Info("VM project base built and published to R2",
		"project", p.GetName(), "baseId", baseID, "incremental", prevBase != nil,
		"steps", len(stepsToRun), "cell", forkRef.Cell, "version", forkRef.Version)
	return res, nil
}

// PrepareWorkspace forks an ephemeral workspace cell from baseRef (reads come
// from R2 through the local SSD CLADEFSD_PART_CACHE; writes stay local-only on
// the host SSD with FSYNC_LEVEL_ORDERED so fsync never waits on disk or R2 and
// big builds never hit R2 upload lag bounds), and boots the Firecracker microVM.
func (b *VMBackend) PrepareWorkspace(ctx context.Context, wsID string, baseRef cladefs.CellRef, fencingToken uint64) (*pb.CladefsCell, error) {
	if b.mgr != nil {
		if err := b.mgr.EnsureNotRunning(ctx, wsID); err != nil {
			return nil, fmt.Errorf("prepare ephemeral cladefs workspace %s: %w: %v", wsID, storage.ErrBusy, err)
		}
	}
	rel := "ws/" + wsID
	if err := b.currentStore().DeleteCell(ctx, rel); err != nil {
		return nil, fmt.Errorf("clear stale cladefs workspace cell %s: %w", wsID, err)
	}
	wsRef, err := b.currentStore().ForkVersion(ctx, baseRef, rel, true)
	if err != nil {
		return nil, fmt.Errorf("fork ephemeral cladefs workspace %s: %w", wsID, err)
	}
	b.mu.Lock()
	if b.wsCells == nil {
		b.wsCells = map[string]cladefs.CellRef{}
	}
	b.wsCells[wsID] = wsRef
	b.mu.Unlock()

	if _, err := b.EnsureVM(ctx, wsID); err != nil {
		b.cleanupCell(ctx, rel)
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

	for _, id := range reapedIDs {
		if !vm.IsEphemeralBuildID(id) {
			b.mu.Lock()
			delete(b.wsCells, id)
			b.mu.Unlock()
			if err := st.DeleteCell(ctx, "ws/"+id); err != nil {
				slog.Warn("delete cladefs cell for reaped orphan VM failed", "vm", id, "err", err)
			}
			b.deleteWorkspaceMemoryCells(ctx, id, nil)
		}
	}
	for _, cellPath := range reapedCellPaths {
		if cellPath == "" {
			continue
		}
		var rel string
		for _, root := range st.CellRoots() {
			r, err := filepath.Rel(root, cellPath)
			if err == nil && !strings.HasPrefix(r, "..") {
				rel = r
				break
			}
		}
		if rel == "" {
			continue
		}
		if strings.HasPrefix(rel, "bases"+string(filepath.Separator)) || strings.HasPrefix(rel, "baseimages"+string(filepath.Separator)) {
			if err := st.DropCellAbs(ctx, cellPath); err != nil {
				slog.Warn("drop local staging cell for reaped orphan build VM failed", "path", cellPath, "err", err)
			}
		}
	}

	if startup {
		for _, root := range st.CellRoots() {
			// Sweep orphaned local ws/<id> cells not belonging to any live workspace
			// (and not held by any still-alive VM). Use DeleteCellAbs/DropCellAbs
			// with the exact enumerated path so a stray entry in an earlier root
			// cannot redirect deletion away from this root.
			wsRoot := filepath.Join(root, "ws")
			if ents, err := os.ReadDir(wsRoot); err == nil {
				for _, ent := range ents {
					if !ent.IsDir() {
						continue
					}
					id := ent.Name()
					if owned[id] {
						continue
					}
					if err := b.mgr.EnsureNotRunning(ctx, id); err != nil {
						continue
					}
					full := filepath.Join(wsRoot, id)
					if err := st.DeleteCellAbs(ctx, full); err != nil {
						slog.Warn("sweep orphan cladefs workspace cell failed", "workspace", id, "path", full, "err", err)
					} else {
						slog.Info("swept orphan cladefs workspace cell", "workspace", id, "path", full)
					}
				}
			}
			// Sweep orphaned local mem/<id> memory cell directories not belonging to
			// any live workspace (and not held by any still-alive VM).
			memRoot := filepath.Join(root, "mem")
			if ents, err := os.ReadDir(memRoot); err == nil {
				for _, ent := range ents {
					if !ent.IsDir() {
						continue
					}
					id := ent.Name()
					if owned[id] {
						continue
					}
					if err := b.mgr.EnsureNotRunning(ctx, id); err != nil {
						continue
					}
					b.deleteWorkspaceMemoryCells(ctx, id, nil)
				}
			}
			// Drop leftover local staging dirs under bases/<project>/<baseID> and
			// baseimages/vm-* from interrupted builds (published bases are already
			// dropped locally after R2 sync; DropCell never deletes from R2).
			basesRoot := filepath.Join(root, "bases")
			if projEnts, err := os.ReadDir(basesRoot); err == nil {
				for _, proj := range projEnts {
					if !proj.IsDir() {
						continue
					}
					if baseEnts, err := os.ReadDir(filepath.Join(basesRoot, proj.Name())); err == nil {
						for _, be := range baseEnts {
							if !be.IsDir() {
								continue
							}
							if err := b.mgr.EnsureNotRunning(ctx, "base-"+be.Name()); err != nil {
								continue
							}
							full := filepath.Join(basesRoot, proj.Name(), be.Name())
							if err := st.DropCellAbs(ctx, full); err == nil {
								slog.Info("dropped leftover local cladefs base cell", "path", full)
							}
						}
					}
				}
			}
			baseImgRoot := filepath.Join(root, "baseimages")
			if ents, err := os.ReadDir(baseImgRoot); err == nil {
				for _, ent := range ents {
					if !ent.IsDir() {
						continue
					}
					key, ok := strings.CutPrefix(ent.Name(), "vm-")
					if !ok {
						continue
					}
					if err := b.mgr.EnsureNotRunning(ctx, "prov-"+key); err != nil {
						continue
					}
					full := filepath.Join(baseImgRoot, ent.Name())
					if err := st.DropCellAbs(ctx, full); err == nil {
						slog.Info("dropped leftover local cladefs baseimage cell", "path", full)
					}
				}
			}
		}
	}
}

func (b *VMBackend) allocMemCellPath(wsID string) (string, error) {
	st := b.currentStore()
	if st == nil {
		return "", errors.New("cladefs store is not open")
	}
	rel := filepath.Join("mem", wsID, fmt.Sprintf("g%d-%d", time.Now().UnixMilli(), b.memSeq.Add(1)))
	hostPath, err := st.NewCellPath(rel)
	if err != nil {
		return "", err
	}
	if err := st.EnsureCellParentDir(hostPath); err != nil {
		return "", fmt.Errorf("prepare memory cell parent for %s: %w", wsID, err)
	}
	return hostPath, nil
}

func (b *VMBackend) deleteWorkspaceMemoryCells(ctx context.Context, wsID string, extra []string) {
	st := b.currentStore()
	if st == nil {
		return
	}
	seen := map[string]struct{}{}
	del := func(p string) {
		if p == "" {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		if err := st.DeleteCellAbs(ctx, p); err != nil {
			slog.Warn("delete cladefs memory cell failed", "workspace", wsID, "path", p, "err", err)
		}
	}
	for _, p := range extra {
		del(p)
	}
	if b.mgr != nil {
		for _, p := range b.mgr.MemoryCellPaths(wsID) {
			del(p)
		}
	}
	for _, root := range st.CellRoots() {
		dir := filepath.Join(root, "mem", wsID)
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, ent := range ents {
			if !ent.IsDir() {
				continue
			}
			del(filepath.Join(dir, ent.Name()))
		}
		_ = os.Remove(dir)
	}
}

// DeleteWorkspaceCell deletes the cladefsd rootfs cell and any memory cells
// backing wsID after its VM stops.
func (b *VMBackend) DeleteWorkspaceCell(ctx context.Context, wsID string) error {
	b.mu.Lock()
	delete(b.wsCells, wsID)
	b.mu.Unlock()
	b.deleteWorkspaceMemoryCells(ctx, wsID, nil)
	return b.currentStore().DeleteCell(ctx, "ws/"+wsID)
}

// SuspendVM implements runtime.SuspendableGuestResolver by snapshotting wsID's
// Firecracker VM straight into a CladeFS memory cell and stopping the VMM.
func (b *VMBackend) SuspendVM(ctx context.Context, wsID string, workingSetBytes int64) error {
	if b.mgr == nil {
		return vm.ErrSuspendUnsupported
	}
	if b.mgr.IsSuspended(wsID) {
		return nil
	}
	var memPath string
	if inst := b.mgr.Get(wsID); inst != nil && !inst.FromMemoryCell {
		var err error
		memPath, err = b.allocMemCellPath(wsID)
		if err != nil {
			return err
		}
	}
	_, err := b.mgr.Suspend(ctx, wsID, memPath, workingSetBytes)
	if err != nil && memPath != "" {
		if st := b.currentStore(); st != nil {
			_ = st.DeleteCellAbs(context.WithoutCancel(ctx), memPath)
		}
	}
	return err
}

// IsVMSuspended implements runtime.SuspendableGuestResolver.
func (b *VMBackend) IsVMSuspended(wsID string) (bool, int64) {
	if b.mgr == nil {
		return false, 0
	}
	if s := b.mgr.Suspended(wsID); s != nil {
		return true, s.WorkingSetBytes
	}
	return false, 0
}

// EnsureVM implements runtime.GuestResolver. If wsID is currently suspended in
// a CladeFS memory cell, EnsureVM resumes it lazily via PUT /snapshot/load.
func (b *VMBackend) EnsureVM(ctx context.Context, wsID string) (*guestd.Client, error) {
	if b.mgr.IsSuspended(wsID) {
		nextMemPath, err := b.allocMemCellPath(wsID)
		if err != nil {
			return nil, err
		}
		inst, err := b.mgr.Resume(ctx, wsID, nextMemPath)
		if err != nil {
			if st := b.currentStore(); st != nil {
				_ = st.DeleteCellAbs(context.WithoutCancel(ctx), nextMemPath)
			}
			return nil, err
		}
		return inst.Client, nil
	}
	if c, err := b.mgr.ClientFor(wsID); err == nil {
		if st, serr := b.mgr.Status(ctx, wsID); serr != nil || st.Running {
			return c, nil
		}
		// Tracked Firecracker process already exited (e.g. exit 158 after cladefsd
		// restart): clean up its stale state and boot a fresh VM on the cell.
		_ = b.mgr.Stop(ctx, wsID, false)
	}
	b.mu.Lock()
	ref, ok := b.wsCells[wsID]
	b.mu.Unlock()
	cellPath := ref.HostPath
	if !ok || cellPath == "" {
		var err error
		cellPath, err = b.currentStore().CellPath("ws/" + wsID)
		if err != nil {
			return nil, err
		}
	}
	inst, err := b.mgr.Start(ctx, vm.LaunchSpec{
		ID:            wsID,
		CellHostPath:  cellPath,
		CladefsVMSock: b.currentStore().VMSock(),
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

// VMExited reports whether wsID's Firecracker process is no longer running.
func (b *VMBackend) VMExited(ctx context.Context, wsID string) (bool, int) {
	ex, code, _, _, _ := b.VMExitStatus(ctx, wsID)
	return ex, code
}

// VMExitStatus reports whether wsID's Firecracker process has exited along with
// systemd exit code, signal, result, and OOM-kill status. Suspended VMs are not
// considered exited.
func (b *VMBackend) VMExitStatus(ctx context.Context, wsID string) (exited bool, exitCode, signal int, result string, oomKilled bool) {
	if b.mgr.IsSuspended(wsID) {
		return false, 0, 0, "", false
	}
	if b.mgr.Get(wsID) == nil {
		return true, 0, 0, "", false
	}
	st, err := b.mgr.Status(ctx, wsID)
	if err != nil {
		return false, 0, 0, "", false
	}
	if !st.Running {
		return true, st.ExitCode, st.Signal, st.Result, st.OOMKilled
	}
	return false, 0, 0, "", false
}

// StopVM implements runtime.GuestResolver.
func (b *VMBackend) StopVM(ctx context.Context, wsID string) error {
	memPaths := b.mgr.MemoryCellPaths(wsID)
	err := b.mgr.StopVM(ctx, wsID)
	if err == nil {
		b.deleteWorkspaceMemoryCells(ctx, wsID, memPaths)
	}
	return err
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

// StopAllForUpgrade gracefully stops and syncs every tracked Firecracker VM and
// closes the cladefsd gRPC connection prior to a disruptive cladefsd restart.
// The store pointer stays non-nil (closed) so any racing background operation
// receives a clean gRPC error instead of panicking on a nil pointer.
func (b *VMBackend) StopAllForUpgrade(ctx context.Context) error {
	var firstErr error
	if b.mgr != nil {
		var memCleanupIDs []string
		memPathsByID := map[string][]string{}
		for _, id := range b.mgr.List() {
			memPathsByID[id] = b.mgr.MemoryCellPaths(id)
			memCleanupIDs = append(memCleanupIDs, id)
			if err := b.mgr.Stop(ctx, id, true); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		for _, id := range b.mgr.ListSuspended() {
			memPathsByID[id] = b.mgr.MemoryCellPaths(id)
			memCleanupIDs = append(memCleanupIDs, id)
		}
		// Also reap any untracked zombie Firecracker units (and suspended VMs
		// whose snapshots cannot safely cross a cladefsd wire upgrade) so
		// cladefsd restarts cleanly.
		b.mgr.ReconcileOrphans(ctx, nil)
		for _, id := range memCleanupIDs {
			b.deleteWorkspaceMemoryCells(ctx, id, memPathsByID[id])
		}
	}
	b.mu.Lock()
	b.wsCells = map[string]cladefs.CellRef{}
	b.mu.Unlock()
	if st := b.currentStore(); st != nil {
		if err := st.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ReconnectCladefs waits for cladefsd to come back online after a restart,
// refreshes the gRPC client and incarnation, and re-applies the peer mesh.
func (b *VMBackend) ReconnectCladefs(ctx context.Context) (uint64, error) {
	cfg := b.configSnapshot()
	cellRoot := tenantCellRoot(cmp.Or(cfg.CellRoot, "/mnt/fxdax-scratch/cladefsd/cells/hotbed"), cfg.TenantID)
	vmUID, vmGID := resolveVMUserIDs(cfg.VMUser)
	ccfg := cladefs.Config{
		ControlSock: cfg.CladefsControlSock,
		VMSock:      cfg.CladefsVMSock,
		CellRoot:    cellRoot,
		TenantID:    cfg.TenantID,
		VMUID:       vmUID,
		VMGID:       vmGID,
		DefaultCap:  cfg.DiskBytes,
	}
	st, inc, err := waitForCladefsReconnect(ctx, ccfg, 30*time.Second, func(ctx context.Context, s *cladefs.Store) (uint64, error) {
		return s.Incarnation(ctx)
	})
	if err != nil {
		return 0, err
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
		heads, err := e.remoteHeads(bgCtx, p)
		var projBase *vmProjectBase
		if err == nil {
			projBase, err = e.cfg.VM.EnsureProjectBase(
				bgCtx, p, heads, req.GetSharedBase(), baseSpec, e.sandboxEnv(),
				func(baseID string) { e.setPreparingWorkspaceBase(req.Id, baseID) },
				e.startManagedFreshen,
			)
		}
		if err == nil {
			e.recordWarmCladefsBase(p, projBase, goruntime.GOARCH)
		}
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
