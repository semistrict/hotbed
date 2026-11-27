package worker

import (
	"cmp"
	"context"
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

// cladefsBackend is a node's cladefs runtime: workspaces on cells forked
// from R2-backed project bases, built by the node or shared by the
// cluster. *VMBackend runs each workspace in a microVM on its cell;
// *CladefsFUSEBackend mounts the cell through cladefsd's host FUSE mount
// and runs chrooted in it. A node runs at most one of them (Engine.cladefs).
type cladefsBackend interface {
	// Kind is the runtime its workspaces have (runtime.KindVM or
	// runtime.KindCladefs).
	Kind() string
	DefaultRuntime() string
	Runtimes() []string
	// RuntimesForHeartbeat is the runtimes to advertise now: none while the
	// backend cannot take workspaces.
	RuntimesForHeartbeat() []string
	HeartbeatMeta() (peerAddr string, incarnation uint64, held []*pb.SharedBase, retired []*pb.CladefsCell)
	SampleObjectStore(ctx context.Context) *pb.ObjectStoreStats
	ApplyHeartbeat(ctx context.Context, peers []string, decommission []uint64, retire []*pb.CladefsCell)
	heldBases() *heldBases
	labels() cladefsLabels
	baseKeyFor(spec baseimage.Spec) string
	tenantID() uint64
	// IsLocalOnly reports whether this backend's cells live strictly on local
	// disk (never uploaded to R2, e.g. machined on macOS).
	IsLocalOnly() bool

	EnsureProvisionedBaseImage(ctx context.Context, spec baseimage.Spec, prov baseimage.Provisioner) (cladefs.CellRef, error)
	ensureProjectBase(ctx context.Context, p *pb.HostedProject, heads map[string]string, shared *pb.SharedBase,
		spec baseimage.Spec, prov baseimage.Provisioner, sandboxEnv []string, onBaseTarget func(baseID string),
		runFreshen managedFreshenRunner, opts ensureBaseOpts) (ensureBaseResult, error)

	// PrepareWorkspace forks baseRef into the cell wsID runs on. When
	// captureBase is true on an R2-backed daemon, it opens a bucket-backed cell
	// with captureUploadInterval so writes stay local until CaptureWorkspaceBase
	// flushes them; otherwise it opens a local-only ephemeral cell (0 R2 bytes).
	PrepareWorkspace(ctx context.Context, wsID string, baseRef cladefs.CellRef, fencingToken uint64, captureBase bool) (*pb.CladefsCell, error)
	// CaptureWorkspaceBase snapshots wsID's stopped/quiescent cell as the
	// project's new warm base, holds it for future forks, and (on R2-backed
	// daemons) drops the bucket cell and replaces wsID's backing cell with an
	// ephemeral fork of the new version so subsequent commands stay local-only.
	CaptureWorkspaceBase(ctx context.Context, wsID, project, baseKey string, scratchEpoch int64,
		repos map[string]string, baseCommitTime, capturedAt int64) (*projectBase, *pb.CladefsCell, error)
	// DeleteWorkspace deletes the cell wsID ran on, once nothing runs on it.
	DeleteWorkspace(ctx context.Context, wsID string) error
	// RestoreWorkspaceCell notes the cell a saved workspace record names,
	// for a worker that restarted.
	RestoreWorkspaceCell(wsID string, cell *pb.CladefsCell, captureBase bool)
	// MountPath is the host path of a workspace volume ("ws/<id>") mounted
	// on the host, if it is.
	MountPath(vol string) (string, bool)

	// StopAllForUpgrade stops what uses cladefsd before it restarts;
	// ReconnectCladefs reconnects once it is back.
	StopAllForUpgrade(ctx context.Context) error
	ReconnectCladefs(ctx context.Context) (uint64, error)
	applyConfig(cfg statedir.WorkerConfig)
	Close() error
}

// cladefs returns the node's cladefs backend, or nil when it runs none.
// EngineConfig's VM and CladefsFUSE are mutually exclusive (worker.Run
// opens at most one).
func (e *Engine) cladefs() cladefsBackend { return e.cfg.cladefs() }

// cladefsLabels is how a backend's workspaces are described in their
// records, events and feed.
type cladefsLabels struct {
	base      string // the base a workspace names while it prepares
	preparing string // its reason while it prepares
	created   string // the created event's detail
	failed    string // what its failure reason starts with
	ready     string // what is ready, in the ready event
	feed      string // the feed's word for it
}

// cladefsFlavor is what tells the two backends apart where they otherwise
// do the same.
type cladefsFlavor struct {
	kind      string // runtime.KindVM or runtime.KindCladefs
	label     string // in errors: "VM" or "cladefs FUSE"
	recipeTag string // salts the project base recipe
	baseIDTag string // in base IDs: <project>-<tag>-<recipe>
	cacheName string // the base cache directory under the worker's
	openPhase string // the freshen phase that readies a base build's fork
	ws        cladefsLabels
}

var (
	vmFlavor = cladefsFlavor{
		kind: runtime.KindVM, label: "VM", recipeTag: "vm-v2", baseIDTag: "vm",
		cacheName: "vm-bases", openPhase: "take_cell",
		ws: cladefsLabels{
			base: "vm-base", preparing: "preparing Firecracker VM base", created: "cladefs + Firecracker VM",
			failed: "could not start Firecracker VM", ready: "Firecracker VM", feed: "vm",
		},
	}
	fuseFlavor = cladefsFlavor{
		kind: runtime.KindCladefs, label: "cladefs FUSE", recipeTag: "fuse-v2", baseIDTag: "fuse",
		cacheName: "cladefs-fuse-bases", openPhase: "mount",
		ws: cladefsLabels{
			base: "cladefs-base", preparing: "preparing cladefs R2 base", created: "cladefs host FUSE",
			failed: "could not prepare cladefs workspace", ready: "cladefs FUSE", feed: "cladefs",
		},
	}
)

// cladefsPeer is what a backend does its own way, which its cladefsCore
// asks of it.
type cladefsPeer interface {
	// settings is its current tenant and cladefs peer address.
	settings() cladefsSettings
	// baseKey names the provisioned base image for spec on this node.
	baseKey(spec baseimage.Spec) string
	// provisionBaseImage builds the base image key from spec, returning
	// its cell unversioned and with nothing using it.
	provisionBaseImage(ctx context.Context, key string, spec baseimage.Spec, prov baseimage.Provisioner) (cladefs.CellRef, error)
	// prepareBaseBuild readies the node to build base baseID.
	prepareBaseBuild(ctx context.Context, baseID string) error
	// openBaseBuild readies fork, a fresh fork in the bucket, to build b on,
	// cleaning it up itself should it fail.
	openBaseBuild(ctx context.Context, fork cladefs.CellRef, b baseBuildTarget) (baseBuildCell, error)
}

type cladefsSettings struct {
	tenant   uint64
	peerAddr string
}

// baseBuildTarget is the project base a build makes.
type baseBuildTarget struct {
	project string
	baseID  string
	spec    baseimage.Spec
}

// baseBuildCell is a project base build's cell, as the backend runs the
// build's freshen on it.
type baseBuildCell interface {
	// ref is the cell, unversioned.
	ref() cladefs.CellRef
	// freshen fills in where spec's freshen runs.
	freshen(spec *managedFreshenSpec)
	// finish ends what ran the freshen, once it ended (ran: it succeeded),
	// leaving the cell ready to version when ran; it returns the seconds
	// that took toward finalizing the base.
	finish(ctx context.Context, h *managedFreshenHandle, ran bool) (float64, error)
	// discard gives the cell up after the build failed.
	discard(ctx context.Context)
}

// cladefsCore is what both cladefs backends keep and do alike: the cladefsd
// connection and its link, the provisioned base images, the project bases
// held, the base cells retiring, and the project base build pipeline.
type cladefsCore struct {
	cladefsFlavor
	peer      cladefsPeer
	workerDir string
	store     atomic.Pointer[cladefs.Store]
	link      cladefsdLink
	// metricsURL is the host cladefsd's Prometheus endpoint, empty where
	// none serves one (machined embeds its daemon; tests run none).
	metricsURL string

	baseMu     sync.Mutex // serializes base image provisioning
	held       heldBases  // the project bases this node hands out
	retirer    baseRetirer
	imagesMu   sync.Mutex
	baseImages map[string]cladefs.CellRef // baseKey -> R2 base image cell; guarded by imagesMu
}

func newCladefsCore(flavor cladefsFlavor, peer cladefsPeer, workerDir string) cladefsCore {
	return cladefsCore{cladefsFlavor: flavor, peer: peer, workerDir: workerDir, baseImages: map[string]cladefs.CellRef{}}
}

func (c *cladefsCore) currentStore() *cladefs.Store { return c.store.Load() }

func (c *cladefsCore) Kind() string                   { return c.kind }
func (c *cladefsCore) DefaultRuntime() string         { return c.kind }
func (c *cladefsCore) Runtimes() []string             { return []string{c.kind} }
func (c *cladefsCore) RuntimesForHeartbeat() []string { return c.Runtimes() }
func (c *cladefsCore) heldBases() *heldBases          { return &c.held }
func (c *cladefsCore) labels() cladefsLabels          { return c.ws }
func (c *cladefsCore) IsLocalOnly() bool              { return false }
func (c *cladefsCore) baseKeyFor(spec baseimage.Spec) string {
	return c.peer.baseKey(spec.Normalized())
}
func (c *cladefsCore) tenantID() uint64 {
	if st := c.currentStore(); st != nil {
		return st.TenantID()
	}
	return c.peer.settings().tenant
}

func (c *cladefsCore) RestoreWorkspaceCell(string, *pb.CladefsCell, bool) {}
func (c *cladefsCore) MountPath(string) (string, bool)              { return "", false }

// HeartbeatMeta returns the cladefs peer address, incarnation, the project
// bases held here and the base cells retired since, for HeartbeatRequest.
func (c *cladefsCore) HeartbeatMeta() (peerAddr string, incarnation uint64, held []*pb.SharedBase, retired []*pb.CladefsCell) {
	held, retired = c.held.report(c.kind)
	return c.peer.settings().peerAddr, c.link.Incarnation(), held, retired
}

// SampleObjectStore fetches cumulative cladefsd R2 object-store counters and
// the peer mesh this worker last applied.
func (c *cladefsCore) SampleObjectStore(ctx context.Context) *pb.ObjectStoreStats {
	return sampleCladefsObjectStore(ctx, c.currentStore(), c.metricsURL, c.peer.settings().peerAddr, c.link.Peers())
}

// ApplyHeartbeat reconciles cladefs peer mesh (SetPeers), retired-host
// decommissioning (DecommissionHost) and the project base cells to retire
// from HeartbeatResponse.
func (c *cladefsCore) ApplyHeartbeat(ctx context.Context, peers []string, decommission []uint64, retire []*pb.CladefsCell) {
	st := c.currentStore()
	if st == nil {
		return
	}
	c.link.sync(ctx, st, c.peer.settings().peerAddr, peers, decommission)
	c.held.retire(ctx, st, retire)
	c.retirer.sweep(ctx, st, time.Now())
}

// cacheDir is where the backend keeps what it knows of its bases across
// restarts, one directory per tenant; "" keeps nothing (a backend without a
// worker directory, in tests).
func (c *cladefsCore) cacheDir() string {
	if c.workerDir == "" {
		return ""
	}
	if t := c.peer.settings().tenant; t > 0 {
		return filepath.Join(c.workerDir, fmt.Sprintf("%s-t%d", c.cacheName, t))
	}
	return filepath.Join(c.workerDir, c.cacheName)
}

// loadCachedBases loads the base images, project bases held and base cells
// retiring that the cache directory records for the backend's tenant.
func (c *cladefsCore) loadCachedBases() {
	dir := c.cacheDir()
	tenant := c.peer.settings().tenant
	c.retirer.load(dir)
	c.held.load(dir, tenant)
	if dir == "" {
		return
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("read cached cladefs bases", "dir", dir, "err", err)
		}
		return
	}
	c.imagesMu.Lock()
	defer c.imagesMu.Unlock()
	for _, ent := range ents {
		key, ok := strings.CutPrefix(ent.Name(), "baseimage-")
		if !ok || !strings.HasSuffix(key, ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			slog.Warn("read cached base image", "file", ent.Name(), "err", err)
			continue
		}
		var ref cladefs.CellRef
		if json.Unmarshal(raw, &ref) == nil && ref.Cell > 0 && ref.Version > 0 && ref.Tenant == tenant {
			if c.baseImages == nil {
				c.baseImages = map[string]cladefs.CellRef{}
			}
			c.baseImages[strings.TrimSuffix(key, ".json")] = ref
		}
	}
}

// saveCacheJSON keeps v as name in the cache directory.
func (c *cladefsCore) saveCacheJSON(name string, v any) {
	dir := c.cacheDir()
	if dir == "" {
		return
	}
	if err := writeJSONAtomic(filepath.Join(dir, name), v); err != nil {
		slog.Warn("cache cladefs base", "file", name, "err", err)
	}
}

// cleanupCell deletes cell, however ctx ended.
func (c *cladefsCore) cleanupCell(ctx context.Context, tenant, cell uint64) {
	if cell == 0 {
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := c.currentStore().DeleteCell(cctx, tenant, cell); err != nil {
		slog.Warn("delete a cladefs cell a failed build left", "tenant", tenant, "cell", cell, "err", err)
	}
}

// swapStore takes st, connected to a cladefsd of incarnation inc, for the
// store before.
func (c *cladefsCore) swapStore(st *cladefs.Store, inc uint64) {
	old := c.store.Swap(st)
	c.link.restarted(inc)
	if old != nil && old != st {
		if err := old.Close(); err != nil {
			slog.Warn("close the cladefsd client of before its restart", "err", err)
		}
	}
}

// versionAndSync makes ref's cell's next version and syncs it to R2.
// cladefsd tears down what used the cell (a build VM's virtio-extent
// session, a FUSE mount) asynchronously, so MakeVersion can fail with
// EBUSY (storage.ErrBusy) right after it ended: that is retried rather
// than discard a finished multi-minute build.
func (c *cladefsCore) versionAndSync(ctx context.Context, ref cladefs.CellRef) (uint64, error) {
	return makeVersionAndSyncAfterStop(ctx, func(ctx context.Context) (uint64, error) {
		return c.currentStore().MakeVersionAndSync(ctx, ref.Tenant, ref.Cell, 2)
	}, 15*time.Second, 100*time.Millisecond)
}

// makeVersionAndSyncAfterStop calls makeAndSync until it does not fail with
// storage.ErrBusy, for up to maxWait, backing off from initialBackoff.
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
		slog.Warn("cladefsd cell still busy right after what used it ended; retrying MakeVersion", "retryIn", delay, "err", err)
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

// EnsureProvisionedBaseImage returns the provisioned base image cell for
// spec, versioned and synced to R2, building it unless the node has it.
func (c *cladefsCore) EnsureProvisionedBaseImage(ctx context.Context, spec baseimage.Spec, prov baseimage.Provisioner) (cladefs.CellRef, error) {
	spec = spec.Normalized()
	key := c.peer.baseKey(spec)

	c.baseMu.Lock()
	defer c.baseMu.Unlock()

	c.imagesMu.Lock()
	ref, ok := c.baseImages[key]
	c.imagesMu.Unlock()
	if ok && ref.Cell > 0 && ref.Version > 0 {
		c.retirer.sweep(ctx, c.currentStore(), time.Now())
		return ref, nil
	}

	slog.Info("building base image", "runtime", c.kind, "key", key, "image", spec.Image)
	ref, err := c.peer.provisionBaseImage(ctx, key, spec, prov)
	if err != nil {
		return cladefs.CellRef{}, err
	}
	ver, err := c.versionAndSync(ctx, ref)
	if err != nil {
		c.cleanupCell(ctx, ref.Tenant, ref.Cell)
		return cladefs.CellRef{}, fmt.Errorf("version & sync %s base image %s to R2: %w", c.label, key, err)
	}
	// Its version is durable in R2: forks read it from there through the
	// local part cache, so the local cell directory goes.
	if err := c.currentStore().DropCell(ctx, ref.Tenant, ref.Cell); err != nil {
		slog.Warn("drop the local copy of a base image synced to R2", "key", key, "cell", ref.Cell, "err", err)
	}
	ref.Version = ver

	c.imagesMu.Lock()
	if c.baseImages == nil {
		c.baseImages = map[string]cladefs.CellRef{}
	}
	superseded := collectSupersededBaseImages(c.baseImages, key, ref.Cell)
	c.baseImages[key] = ref
	c.imagesMu.Unlock()
	c.saveCacheJSON("baseimage-"+key+".json", ref)
	now := time.Now()
	retireSupersededBaseImages(c.cacheDir(), superseded, &c.retirer, now)
	c.retirer.sweep(ctx, c.currentStore(), now)
	slog.Info("base image provisioned and synced to R2", "runtime", c.kind, "key", key, "cell", ref.Cell, "version", ref.Version)
	return ref, nil
}

// projectRecipe hashes what a project base is made of: the base image, the
// node's arch, and the project's repos at heads and steps, salted by tag so
// the two backends' bases never share a recipe.
func projectRecipe(tag, baseKey string, p *pb.HostedProject, heads map[string]string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\nbase=%s\narch=%s\nproject=%s\nepoch=%d\n", tag, baseKey, goruntime.GOARCH, p.GetName(), p.GetScratchEpoch())
	for _, r := range p.GetRepos() {
		fmt.Fprintf(h, "repo=%s|%s|%s|%s\n", r.GetId(), r.GetUrl(), r.GetBranch(), heads[r.GetId()])
	}
	for _, s := range p.GetSteps() {
		fmt.Fprintf(h, "step=%s|%s|%v\n", s.GetName(), s.GetHash(), s.GetEveryPull())
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// selectProjectBaseSteps returns the subset of p's setup steps that must run
// when freshening from prev (nil for a cold build from the OS base image).
// On an incremental freshen, steps whose script hash changed (or that are new)
// always run, and --every-pull steps run whenever any repo commit moved.
func selectProjectBaseSteps(p *pb.HostedProject, prev *projectBase, heads map[string]string) []*pb.Step {
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

// EnsureProjectBase returns a warm project base for p's workspaces: the one
// this node holds or the cluster's newest (shared), whichever is up to date,
// or a new one built on top of the newer of them, with every repo at its
// head under /work/<repo> and the steps that need to run run, and uploaded
// to the shared bucket.
func (c *cladefsCore) EnsureProjectBase(
	ctx context.Context,
	p *pb.HostedProject,
	heads map[string]string,
	shared *pb.SharedBase,
	spec baseimage.Spec,
	prov baseimage.Provisioner,
	sandboxEnv []string,
	onBaseTarget func(baseID string),
	runFreshen managedFreshenRunner,
) (*projectBase, error) {
	res, err := c.ensureProjectBase(ctx, p, heads, shared, spec, prov, sandboxEnv, onBaseTarget, runFreshen, ensureBaseOpts{})
	return res.Base, err
}

func (c *cladefsCore) ensureProjectBase(
	ctx context.Context,
	p *pb.HostedProject,
	heads map[string]string,
	shared *pb.SharedBase,
	spec baseimage.Spec,
	prov baseimage.Provisioner,
	sandboxEnv []string,
	onBaseTarget func(baseID string),
	runFreshen managedFreshenRunner,
	opts ensureBaseOpts,
) (ensureBaseResult, error) {
	spec = spec.Normalized()
	baseKey := c.peer.baseKey(spec)
	recipe := projectRecipe(c.recipeTag, baseKey, p, heads)
	tenant := c.peer.settings().tenant
	if st := c.currentStore(); st != nil {
		tenant = st.TenantID()
	}
	return c.held.ensure(ctx, baseRequest{
		project:      p,
		heads:        heads,
		shared:       shared,
		runtime:      c.kind,
		tenant:       tenant,
		baseKey:      baseKey,
		recipe:       recipe,
		baseID:       fmt.Sprintf("%s-%s-%s", p.GetName(), c.baseIDTag, recipe[:8]),
		onBaseTarget: onBaseTarget,
		opts:         opts,
		build: func(ctx context.Context, plan basePlan) (builtBase, error) {
			return c.buildProjectBase(ctx, p, heads, spec, prov, baseKey, sandboxEnv, runFreshen, plan)
		},
	})
}

// buildProjectBase builds plan's base: it forks the base to build on (or the
// provisioned base image), readies the fork the backend's way, freshens the
// repos and runs the steps that need to run on it, and uploads the result.
func (c *cladefsCore) buildProjectBase(
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
	failPrep := func(active time.Duration, err error) (builtBase, error) {
		return fail("", fmt.Sprintf("base %s failed: %v", baseID, err), false, active, err)
	}
	if runFreshen == nil {
		return failPrep(0, fmt.Errorf("freshen %s project base %s: no freshen runner configured", c.label, baseID))
	}
	if err := c.peer.prepareBaseBuild(ctx, baseID); err != nil {
		return failPrep(0, fmt.Errorf("prepare %s project base %s: %w", c.label, baseID, err))
	}

	createdAt := time.Now().UnixMilli()
	prepStart := time.Now()
	var prepPhases []*pb.FreshenPhase
	phase := func(name string, start time.Time, detail string) {
		end := time.Now()
		endMs := end.UnixMilli()
		prepPhases = append(prepPhases, &pb.FreshenPhase{
			Name: name, StartedAt: start.UnixMilli(), EndedAt: &endMs, Seconds: end.Sub(start).Seconds(), Detail: detail,
		})
	}

	from, fromLabel, fromBaseID := cladefs.CellRef{}, baseKey, ""
	if prevBase != nil {
		from, fromLabel, fromBaseID = prevBase.Ref, prevBase.BaseID, prevBase.BaseID
	} else {
		imgStart := time.Now()
		ref, err := c.EnsureProvisionedBaseImage(ctx, spec, prov)
		if err != nil {
			return failPrep(time.Since(prepStart), err)
		}
		phase("base_image", imgStart, baseKey)
		from = ref
	}

	forkStart := time.Now()
	fork, err := c.currentStore().ForkVersion(ctx, from, false)
	if err != nil {
		if prevBase == nil {
			return failPrep(time.Since(prepStart), fmt.Errorf("fork %s project base %s: %w", c.label, baseID, err))
		}
		if ctx.Err() != nil {
			return fail("", fmt.Sprintf("base %s cancelled: %v", baseID, err), false, time.Since(prepStart), err)
		}
		return builtBase{}, &incrementalForkError{err: err}
	}
	phase("fork", forkStart, "from "+fromLabel)

	// The backend re-opens the fork with baseBuildUploadInterval so cladefsd
	// coalesces segment uploads while the build writes; MakeVersionAndSync
	// flushes everything at the end (question 534).
	openStart := time.Now()
	cell, err := c.peer.openBaseBuild(ctx, fork, baseBuildTarget{project: p.GetName(), baseID: baseID, spec: spec})
	if err != nil {
		return failPrep(time.Since(prepStart), fmt.Errorf("ready %s project base %s: %w", c.label, baseID, err))
	}
	phase(c.openPhase, openStart, "")
	prepSeconds := time.Since(prepStart).Seconds()

	stepsToRun := selectProjectBaseSteps(p, prevBase, heads)
	fspec := managedFreshenSpec{
		Project:     p,
		BaseID:      baseID,
		Recipe:      plan.recipe,
		Arch:        goruntime.GOARCH,
		Runtime:     c.kind,
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
		Script:      freshenScript(p.GetRepos(), heads, prevBase.repoIDs(), stepsToRun),
		Env:         slices.Clone(sandboxEnv),
	}
	cell.freshen(&fspec)

	var (
		freshenRes      managedFreshenResult
		finalizeSeconds float64
	)
	activeDuration := func() time.Duration {
		return time.Duration(max(prepSeconds+freshenRes.ActiveRunSeconds+finalizeSeconds, 0) * float64(time.Second))
	}
	handle, err := runFreshen(ctx, fspec)
	if err != nil {
		cell.discard(ctx)
		return failPrep(activeDuration(), fmt.Errorf("queue %s project base %s: %w", c.label, baseID, err))
	}
	freshenRes, waitErr := handle.Wait(ctx)
	ran := waitErr == nil && freshenRes.State == succeeded
	finishSeconds, finishErr := cell.finish(ctx, handle, ran)
	finalizeSeconds += finishSeconds
	freshenRes.FinalizeSeconds = finalizeSeconds
	failRun := func(detail string, stepFailed bool, err error) (builtBase, error) {
		cell.discard(ctx)
		handle.Finish(false, freshenRes, cladefs.CellRef{}, detail)
		summary := managedFreshenFailureSummary(baseID, handle.CommandID(), freshenRes, detail)
		return fail(handle.CommandID(), summary, stepFailed, activeDuration(), err)
	}
	switch {
	case waitErr != nil:
		return failRun(waitErr.Error(), freshenRes.StepFailed,
			fmt.Errorf("freshen %s project base %s (command %s): %w", c.label, baseID, handle.CommandID(), waitErr))
	case freshenRes.State != succeeded:
		detail := cmp.Or(freshenRes.Reason, "freshen "+stateName(freshenRes.State))
		return failRun(detail, freshenRes.StepFailed,
			fmt.Errorf("freshen %s project base %s failed (command %s): %s", c.label, baseID, handle.CommandID(), detail))
	case finishErr != nil:
		return failRun(finishErr.Error(), false, fmt.Errorf("finish %s project base %s: %w", c.label, baseID, finishErr))
	}

	ref := cell.ref()
	handle.SetFinalizing("r2_sync")
	syncStart := time.Now()
	ver, err := c.versionAndSync(ctx, ref)
	syncSecs := time.Since(syncStart).Seconds()
	finalizeSeconds += syncSecs
	handle.RecordPhase("r2_sync", syncStart.UnixMilli(), time.Now().UnixMilli(), syncSecs, "")
	freshenRes.FinalizeSeconds = finalizeSeconds
	if err != nil {
		c.cleanupCell(ctx, ref.Tenant, ref.Cell)
		detail := fmt.Sprintf("sync to R2: %v", err)
		handle.Finish(false, freshenRes, cladefs.CellRef{}, detail)
		summary := managedFreshenFailureSummary(baseID, handle.CommandID(), freshenRes, detail)
		return fail(handle.CommandID(), summary, false, activeDuration(), fmt.Errorf("sync %s project base %s to R2: %w", c.label, baseID, err))
	}
	// The local cell directory goes now that the base is durable in R2: local
	// SSD stays a cache (CLADEFSD_PART_CACHE) and active workspaces.
	if err := c.currentStore().DropCell(ctx, ref.Tenant, ref.Cell); err != nil {
		slog.Warn("drop the local copy of a project base synced to R2", "baseId", baseID, "cell", ref.Cell, "err", err)
	}
	ref.Version = ver
	handle.Finish(true, freshenRes, ref, "")
	return builtBase{ref: ref, repoHeads: freshenRes.RepoHeads, steps: len(stepsToRun), active: activeDuration()}, nil
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

// defaultCellRoot is where cladefsd keeps a node's cells unless configured.
const defaultCellRoot = "/mnt/fxdax-scratch/cladefsd/cells/hotbed"

// createCladefsWorkspace creates a workspace on b from the project's warm base
// (or the bare OS base image on a cold project / --fresh), without touching the
// host's ZFS/btrfs store. Synced runs and runs with an existing warm base fork
// immediately without polling remotes or running a freshen job.
func (e *Engine) createCladefsWorkspace(req *pb.CreateWorkspaceRequest, b cladefsBackend) (*pb.Workspace, error) {
	p := req.Project
	l := b.labels()
	now := e.nowMs()
	var repos []*pb.WorkspaceRepo
	for _, r := range p.GetRepos() {
		repos = append(repos, &pb.WorkspaceRepo{Repo: r.GetId()})
	}
	noCapture := req.GetNoCapture() || req.GetPlacement().GetBench()
	w := &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{
			Id: req.Id, Project: p.Name, BaseId: l.base, NodeId: e.cfg.NodeID, Agent: req.Agent,
			Repos: repos, State: preparing, CreatedAt: now, LastActiveAt: now,
			SessionId: req.SessionId, SyncHost: req.SyncHost, Runtime: b.Kind(), Throwaway: req.Throwaway,
			Reason: l.preparing,
		},
		Volume: wsVolume(req.Id), BaseSnapshot: l.base,
		NoCapture: noCapture, ScratchEpoch: p.GetScratchEpoch(),
	}}
	e.mu.Lock()
	if _, dup := e.workspaces[req.Id]; dup {
		e.mu.Unlock()
		return nil, fmt.Errorf("workspace %s exists", req.Id)
	}
	w.forking = true
	done := make(chan struct{})
	w.forkDone = done
	if b.Kind() == runtime.KindVM {
		e.vmWorkspaces.Store(req.Id, struct{}{})
	}
	e.workspaces[req.Id] = w
	e.saveWorkspace(w, change{what: "created", detail: l.created})
	e.mu.Unlock()

	prepare := func(bgCtx context.Context) {
		start := e.now()
		baseSpec, prov := e.cfg.BaseSpec, e.baseProvisioner()
		baseKey := b.baseKeyFor(baseSpec)
		held, heldFresh := b.heldBases().get(p.Name)
		shared := sharedProjectBase(req.GetSharedBase(), b.tenantID())
		projBase := pickAutoCaptureBase(held, shared, req.GetFresh(), heldFresh, p, baseKey)

		var (
			fromRef     cladefs.CellRef
			baseID      string
			note        string
			captureBase bool
			cell        *pb.CladefsCell
			err         error
		)
		// Fast path: synced runs, --fresh runs, and runs with an already-warm
		// project base fork directly without polling git remotes or running
		// freshen steps. Only unsynced cold starts (--no-sync / schedules on a
		// brand-new project with no captured base yet) fall back to ensureProjectBase.
		if req.GetSyncDir() != "" || req.GetFresh() || projBase != nil {
			if projBase != nil {
				projBase = b.heldBases().hold(p.Name, projBase, false, false)
				e.recordWarmCladefsBase(p, projBase, goruntime.GOARCH)
				fromRef = projBase.Ref
				baseID = projBase.BaseID
			} else {
				fromRef, err = b.EnsureProvisionedBaseImage(bgCtx, baseSpec, prov)
				baseID = "baseimage-" + baseKey
			}
			if !noCapture && (!req.GetThrowaway() || req.GetCaptureBase()) {
				if b.IsLocalOnly() {
					const localCaptureIntervalMs = int64(30 * time.Minute / time.Millisecond)
					captureBase = req.GetCaptureBase() || projBase == nil ||
						(e.nowMs()-projBase.CapturedAt >= localCaptureIntervalMs) ||
						(req.GetBaseCommitTime() > 0 && req.GetBaseCommitTime() > projBase.BaseCommitTime)
				} else {
					captureBase = req.GetCaptureBase()
				}
			}
			if err == nil {
				cell, err = b.PrepareWorkspace(bgCtx, req.Id, fromRef, req.GetFencingToken(), captureBase)
				if err != nil && projBase != nil && cladefs.IsGone(err) {
					b.heldBases().forget(p.Name, projBase.Ref)
					projBase = nil
					baseID = "baseimage-" + baseKey
					if imgRef, ierr := b.EnsureProvisionedBaseImage(bgCtx, baseSpec, prov); ierr == nil {
						cell, err = b.PrepareWorkspace(bgCtx, req.Id, imgRef, req.GetFencingToken(), captureBase)
					} else {
						err = ierr
					}
				}
			}
		} else {
			heads, herr := e.remoteHeads(bgCtx, p)
			err = herr
			var baseRes ensureBaseResult
			if err == nil {
				baseRes, err = b.ensureProjectBase(
					bgCtx, p, heads, req.GetSharedBase(), baseSpec, prov, e.sandboxEnv(),
					func(id string) { e.setPreparingWorkspaceBase(req.Id, id) },
					e.startManagedFreshen,
					ensureBaseOpts{
						Budget:          e.freshenBudget,
						Now:             e.now,
						AllowBackground: false,
						OnRequireBuild:  func() { e.cancelBgFreshen(p.Name) },
					},
				)
			}
			if err == nil {
				projBase = baseRes.Base
				baseID = projBase.BaseID
				note = baseRes.Note
				e.recordWarmCladefsBase(p, projBase, goruntime.GOARCH)
				cell, err = b.PrepareWorkspace(bgCtx, req.Id, projBase.Ref, req.GetFencingToken(), false)
				if cladefs.IsGone(err) {
					b.heldBases().forget(p.Name, projBase.Ref)
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
				e.dropCladefsWorkspace(context.Background(), b, req.Id)
			}
			return
		}
		if err != nil {
			w.ws().State = failedWS
			w.ws().Reason = l.failed + ": " + err.Error()
			e.saveWorkspace(w, change{what: "failed", detail: w.ws().Reason})
			for _, c := range e.commands {
				if c.cmd().WorkspaceId == w.ws().Id && c.cmd().State == queued {
					e.end(c, failed, nil, "its workspace failed: "+w.ws().Reason)
				}
			}
			e.mu.Unlock()
			return
		}
		w.ws().BaseId = baseID
		w.ws().CaptureBase = captureBase
		w.rec.BaseSnapshot = baseID
		w.rec.Note = note
		w.rec.CaptureBase = captureBase
		w.rec.BaseKey = baseKey
		w.rec.ScratchEpoch = p.GetScratchEpoch()
		if projBase != nil {
			for _, r := range w.ws().Repos {
				if head, ok := projBase.Repos[r.Repo]; ok && head != "" {
					r.Head = head
					r.BaseCommit = head
					r.AnchorCommit = head
					r.BaseCommitTime = projBase.BaseCommitTime
				}
			}
		}
		w.ws().State = idle
		if e.workspaceBusy(w.ws().Id) {
			w.ws().State = activeWS
		}
		w.ws().Reason = note
		w.ws().ForkMillis = forkMs
		w.ws().CladefsCell = cell
		w.ws().LastActiveAt = e.nowMs()
		e.saveWorkspace(w, change{
			what:   "ready",
			detail: fmt.Sprintf("%s ready in %d ms (cell %d/%d)", l.ready, forkMs, cell.Tenant, cell.Cell),
			feed:   feed(feedFork, fmt.Sprintf("%s %s ready in %d ms · %s", l.feed, req.Id, forkMs, req.Agent), ref("workspace", req.Id)),
		})
		e.stepSoon()
		e.mu.Unlock()
	}

	e.mu.Lock()
	started := e.startBgLocked(prepare)
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

// maybeStartCaptureLocked starts an asynchronous project-base capture for w if
// w is eligible, quiescent, and not already capturing or releasing. Call with
// e.mu held.
func (e *Engine) maybeStartCaptureLocked(w *workspace) {
	b := e.cladefs()
	if b == nil || w == nil {
		return
	}
	ws := w.ws()
	if !w.capturePending || w.capturing || w.releasing || w.forking || ws.State == released || ws.State == failedWS {
		return
	}
	if !w.rec.CaptureBase || w.rec.NoCapture || ws.Throwaway || ws.Runtime != b.Kind() {
		w.capturePending = false
		return
	}
	if e.usesRuntime(ws.Id) {
		if e.commandInVMLocked(ws.Id, nil) != nil || e.shellOpenLocked(ws.Id) || w.vmUsed {
			return
		}
	} else if e.workspaceBusy(ws.Id) {
		return
	}
	w.capturing = true
	done := make(chan struct{})
	w.captureDone = done
	started := e.startBgLocked(func(bgCtx context.Context) {
		e.runWorkspaceCapture(bgCtx, w, b, done)
	})
	if !started {
		w.capturing = false
		w.captureDone = nil
		close(done)
	}
}

func (e *Engine) runWorkspaceCapture(ctx context.Context, w *workspace, b cladefsBackend, done chan struct{}) {
	defer func() {
		e.mu.Lock()
		w.capturing = false
		if w.captureDone == done {
			w.captureDone = nil
		}
		close(done)
		e.stepSoon()
		e.mu.Unlock()
	}()

	w.vm.Lock()
	defer w.vm.Unlock()

	e.mu.Lock()
	ws := w.ws()
	if w.releasing || ws.State == released || ws.State == failedWS || !w.capturePending || !w.rec.CaptureBase || w.rec.NoCapture {
		w.capturePending = false
		e.mu.Unlock()
		return
	}
	if e.usesRuntime(ws.Id) {
		if e.commandInVMLocked(ws.Id, nil) != nil || e.shellOpenLocked(ws.Id) || w.vmUsed {
			e.mu.Unlock()
			return
		}
	} else if e.workspaceBusy(ws.Id) {
		e.mu.Unlock()
		return
	}
	wsID := ws.Id
	project := ws.Project
	baseKey := w.rec.BaseKey
	scratchEpoch := w.rec.ScratchEpoch
	repos := make(map[string]string, len(ws.Repos))
	var maxCommitTime int64
	for _, r := range ws.Repos {
		commit := cmp.Or(r.AnchorCommit, r.Head, r.BaseCommit)
		if commit != "" {
			repos[r.Repo] = commit
		}
		if r.BaseCommitTime > maxCommitTime {
			maxCommitTime = r.BaseCommitTime
		}
	}
	capturedAt := e.nowMs()
	e.mu.Unlock()

	captured, newCell, err := b.CaptureWorkspaceBase(ctx, wsID, project, baseKey, scratchEpoch, repos, maxCommitTime, capturedAt)
	if err != nil {
		slog.Warn("auto-capture workspace base failed (keeping previous project base)",
			"workspace", wsID, "project", project, "err", err)
		e.mu.Lock()
		w.capturePending = false
		if captured != nil {
			// The base cell was already promoted into heldBases; never leave
			// the workspace record pointing at it or a restart/release could
			// alias and delete the held base.
			w.rec.CaptureBase = false
			w.ws().CaptureBase = false
			w.ws().CladefsCell = nil
			e.saveWorkspace(w, change{what: "captured"})
		}
		e.mu.Unlock()
		if captured != nil {
			e.recordWarmCladefsBase(&pb.HostedProject{Name: project, ScratchEpoch: scratchEpoch}, captured, goruntime.GOARCH)
		}
		return
	}

	e.mu.Lock()
	w.capturePending = false
	w.rec.CaptureBase = false
	w.ws().CaptureBase = false
	w.ws().BaseId = captured.BaseID
	w.rec.BaseSnapshot = captured.BaseID
	if newCell != nil {
		w.ws().CladefsCell = newCell
	}
	e.saveWorkspace(w, change{
		what:   "captured",
		detail: fmt.Sprintf("captured project base %s (cell %d/%d v%d)", captured.BaseID, captured.Ref.Tenant, captured.Ref.Cell, captured.Ref.Version),
		feed:   feed(feedBase, fmt.Sprintf("captured %s ← %s", captured.BaseID, wsID), ref("workspace", wsID)),
	})
	e.mu.Unlock()
	e.recordWarmCladefsBase(&pb.HostedProject{Name: project, ScratchEpoch: scratchEpoch}, captured, goruntime.GOARCH)
	slog.Info("auto-captured workspace as project base",
		"workspace", wsID, "project", project, "baseId", captured.BaseID,
		"cell", captured.Ref.Cell, "version", captured.Ref.Version, "repos", maps.Clone(repos))
}

// dropCladefsWorkspace stops what runs workspace id and deletes its cell.
func (e *Engine) dropCladefsWorkspace(ctx context.Context, b cladefsBackend, id string) {
	if e.usesRuntime(id) {
		if err := e.cfg.Runtime.Teardown(ctx, id); err != nil {
			slog.Warn("tear down a workspace's runtime", "workspace", id, "err", err)
		}
	}
	if err := b.DeleteWorkspace(ctx, id); err != nil {
		slog.Warn("delete a workspace's cladefs cell", "workspace", id, "err", err)
	}
}

// reconnectStore waits for cladefsd, as ccfg reaches it, to answer probe,
// readies the new store with ready (when set), and takes it.
func (c *cladefsCore) reconnectStore(ctx context.Context, ccfg cladefs.Config, probe func(context.Context, *cladefs.Store) (uint64, error), ready func(*cladefs.Store) error) (uint64, error) {
	st, inc, err := waitForCladefsReconnect(ctx, ccfg, cladefsdStartWait, probe)
	if err != nil {
		return 0, err
	}
	if ready != nil {
		if err := ready(st); err != nil {
			_ = st.Close()
			return 0, err
		}
	}
	c.swapStore(st, inc)
	return inc, nil
}

// cladefs returns the cladefs backend cfg runs on, or nil.
func (cfg *EngineConfig) cladefs() cladefsBackend {
	switch {
	case cfg.VM != nil:
		return cfg.VM
	case cfg.CladefsFUSE != nil:
		return cfg.CladefsFUSE
	}
	return nil
}
