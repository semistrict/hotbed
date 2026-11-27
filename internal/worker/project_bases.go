package worker

import (
	"context"
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
	"time"

	"github.com/semistrict/hotbed/internal/cladefs"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// projectBase is a warm project base: a version of a cell in the shared
// bucket, which any node of its arch and runtime can fork, with what a freshen
// or auto-capture on top of it needs to know of it.
type projectBase struct {
	Ref            cladefs.CellRef   `json:"ref"`
	BaseID         string            `json:"baseId"`
	Recipe         string            `json:"recipe"`
	BaseKey        string            `json:"baseKey,omitempty"`
	SetupHash      string            `json:"setupHash,omitempty"`
	ScratchEpoch   int64             `json:"scratchEpoch,omitempty"`
	Repos          map[string]string `json:"repos"`                // repoID -> checked-out HEAD / anchor commit
	StepHashes     map[string]string `json:"stepHashes,omitempty"` // step name -> last executed script hash
	BaseCommitTime int64             `json:"baseCommitTime,omitempty"`
	CapturedAt     int64             `json:"capturedAt,omitempty"`
	AutoCaptured   bool              `json:"autoCaptured,omitempty"`
}

func (b *projectBase) usable() bool {
	return b != nil && b.Ref.Cell > 0 && b.Ref.Version > 0
}

// repoIDs names the repos b's checkouts hold, sorted; none for no base.
func (b *projectBase) repoIDs() []string {
	if b == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(b.Repos))
}

func (b *projectBase) clone() *projectBase {
	if b == nil {
		return nil
	}
	c := *b
	c.Repos = maps.Clone(b.Repos)
	c.StepHashes = maps.Clone(b.StepHashes)
	return &c
}

// sharedProjectBase reads the base the controller sent: nil unless it is a
// whole version of a cell of tenant.
func sharedProjectBase(sb *pb.SharedBase, tenant uint64) *projectBase {
	c := sb.GetCell()
	if c.GetCell() == 0 || c.GetVersion() == 0 || c.GetTenant() != tenant {
		return nil
	}
	return &projectBase{
		Ref:            cladefs.CellRef{Tenant: c.GetTenant(), Cell: c.GetCell(), Version: c.GetVersion()},
		BaseID:         c.GetBaseId(),
		Recipe:         c.GetRecipe(),
		BaseKey:        sb.GetBaseKey(),
		SetupHash:      sb.GetSetupHash(),
		ScratchEpoch:   sb.GetScratchEpoch(),
		Repos:          maps.Clone(sb.GetRepos()),
		StepHashes:     maps.Clone(sb.GetStepHashes()),
		BaseCommitTime: sb.GetBaseCommitTime(),
		CapturedAt:     sb.GetCapturedAt(),
		AutoCaptured:   sb.GetAutoCaptured(),
	}
}

// shared describes b to the controller as project's base for rt on this arch.
func (b *projectBase) shared(project, rt string) *pb.SharedBase {
	return &pb.SharedBase{
		Cell: &pb.CladefsCell{
			Tenant:  b.Ref.Tenant,
			Cell:    b.Ref.Cell,
			Version: b.Ref.Version,
			BaseId:  b.BaseID,
			Project: project,
			Arch:    goruntime.GOARCH,
			Recipe:  b.Recipe,
			Runtime: rt,
		},
		BaseKey:        b.BaseKey,
		SetupHash:      b.SetupHash,
		ScratchEpoch:   b.ScratchEpoch,
		Repos:          maps.Clone(b.Repos),
		StepHashes:     maps.Clone(b.StepHashes),
		BaseCommitTime: b.BaseCommitTime,
		CapturedAt:     b.CapturedAt,
		AutoCaptured:   b.AutoCaptured,
	}
}

// hasAllProjectSteps reports whether b has recorded matching hashes for every
// setup step defined on p. Projects with no steps trivially satisfy it.
func hasAllProjectSteps(b *projectBase, p *pb.HostedProject) bool {
	if p == nil || len(p.GetSteps()) == 0 {
		return true
	}
	for _, s := range p.GetSteps() {
		h, ok := b.StepHashes[s.GetName()]
		if !ok || h != s.GetHash() {
			return false
		}
	}
	return true
}

// pickAutoCaptureBase picks the best warm project base to fork immediately for
// a workspace without running a freshen job. Returns nil when fresh is true or
// neither held nor shared matches the current OS base image key, scratch epoch,
// and project setup steps.
func pickAutoCaptureBase(held, shared *projectBase, fresh, heldFresh bool, p *pb.HostedProject, baseKey string) *projectBase {
	if fresh {
		return nil
	}
	compatible := func(b *projectBase) bool {
		if !b.usable() {
			return false
		}
		if p != nil && b.ScratchEpoch < p.GetScratchEpoch() {
			return false
		}
		if baseKey != "" && b.BaseKey != "" && b.BaseKey != baseKey {
			return false
		}
		if !hasAllProjectSteps(b, p) {
			return false
		}
		return true
	}
	if !compatible(held) {
		held = nil
	}
	if !compatible(shared) {
		shared = nil
	}
	if held == nil {
		return shared
	}
	if shared == nil {
		return held
	}
	if held.Ref.Cell == shared.Ref.Cell && held.Ref.Version == shared.Ref.Version {
		return held
	}
	if heldFresh {
		return held
	}
	if held.CapturedAt > shared.CapturedAt {
		return held
	}
	return shared
}

// buildableOn reports whether a freshen of p can build on b.
func buildableOn(b *projectBase, p *pb.HostedProject, baseKey string) bool {
	_, cold := baseHasStructuralDrift(p, b, baseKey)
	return cold == ""
}

// pickBase picks the base to hand out or build on from the one this node
// holds and the cluster's newest (shared): whichever is up to date with recipe,
// else the newer of those a freshen can build on. The controller's newest is
// newer than the held one unless the node built that one since it last told
// the controller what it holds (fresh).
func pickBase(held, shared *projectBase, fresh bool, p *pb.HostedProject, baseKey, recipe string) *projectBase {
	if !held.usable() {
		held = nil
	}
	if !shared.usable() || (held != nil && held.Ref.Cell == shared.Ref.Cell) {
		return held
	}
	switch {
	case held != nil && held.Recipe == recipe:
		return held
	case shared.Recipe == recipe:
		return shared
	case !buildableOn(shared, p, baseKey):
		return held
	case held != nil && fresh && buildableOn(held, p, baseKey):
		return held
	}
	return shared
}

// heldBases is the base each project hands out on this node, for its one
// cladefs runtime. The node tells the controller it holds them
// (HeartbeatRequest.held_bases), so that no node deletes one while it is
// handed out here, and keeps them in its cache directory across restarts. The
// zero value holds nothing and keeps nothing on disk.
type heldBases struct {
	mu    sync.Mutex
	dir   string                  // cache directory, a project-<name>.json each
	bases map[string]*projectBase // project -> base
	// Projects whose held base this node built since a heartbeat last told
	// the controller of it: newer than any the controller hands out.
	fresh   map[string]bool
	retired []*pb.CladefsCell // deleted as the controller asked, not yet reported
	locks   sync.Map          // project -> *sync.Mutex, held while its base freshens
}

// load reads the bases cached in dir, keeping tenant's, and keeps them there from now on.
func (h *heldBases) load(dir string, tenant uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dir = dir
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, ent := range ents {
		name, ok := strings.CutPrefix(ent.Name(), "project-")
		if !ok || !strings.HasSuffix(name, ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			slog.Warn("read cached project base", "file", ent.Name(), "err", err)
			continue
		}
		var b projectBase
		if err := json.Unmarshal(raw, &b); err != nil {
			slog.Warn("decode cached project base", "file", ent.Name(), "err", err)
			continue
		}
		if b.usable() && b.Ref.Tenant == tenant {
			if h.bases == nil {
				h.bases = map[string]*projectBase{}
			}
			h.bases[strings.TrimSuffix(name, ".json")] = &b
		}
	}
}

// get returns project's held base, and whether it is fresh.
func (h *heldBases) get(project string) (*projectBase, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.bases[project].clone(), h.fresh[project]
}

// hold makes b the base project hands out here, and returns the base it
// holds: b, or a fresh one this node built while b, not up to date, was
// picked from older ones. built says this node just built b; upToDate that b
// is up to date with the project's heads.
func (h *heldBases) hold(project string, b *projectBase, built, upToDate bool) *projectBase {
	h.mu.Lock()
	defer h.mu.Unlock()
	cur := h.bases[project]
	if cur != nil && cur.Ref == b.Ref {
		return cur.clone()
	}
	if cur != nil && h.fresh[project] && !built && !upToDate {
		return cur.clone()
	}
	if h.bases == nil {
		h.bases = map[string]*projectBase{}
	}
	if h.fresh == nil {
		h.fresh = map[string]bool{}
	}
	h.bases[project] = b.clone()
	if built {
		h.fresh[project] = true
	} else {
		delete(h.fresh, project)
	}
	h.saveLocked(project, b)
	return b.clone()
}

// forget stops holding project's base if it is ref: its cell is gone.
func (h *heldBases) forget(project string, ref cladefs.CellRef) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cur := h.bases[project]; cur == nil || cur.Ref.Tenant != ref.Tenant || cur.Ref.Cell != ref.Cell {
		return
	}
	delete(h.bases, project)
	delete(h.fresh, project)
	if h.dir != "" {
		if err := os.Remove(filepath.Join(h.dir, "project-"+project+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("remove cached project base", "project", project, "err", err)
		}
	}
	slog.Warn("forgot project base whose cell is gone", "project", project, "tenant", ref.Tenant, "cell", ref.Cell)
}

func (h *heldBases) saveLocked(project string, b *projectBase) {
	if h.dir == "" {
		return
	}
	if err := writeJSONAtomic(filepath.Join(h.dir, "project-"+project+".json"), b); err != nil {
		slog.Warn("cache project base", "project", project, "err", err)
	}
}

func (h *heldBases) holds(tenant, cell uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, b := range h.bases {
		if b.Ref.Tenant == tenant && b.Ref.Cell == cell {
			return true
		}
	}
	return false
}

func (h *heldBases) lock(project string) *sync.Mutex {
	mu, _ := h.locks.LoadOrStore(project, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// report returns the bases held, as rt's, and the cells retired and not yet reported.
func (h *heldBases) report(rt string) (held []*pb.SharedBase, retired []*pb.CladefsCell) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for project, b := range h.bases {
		held = append(held, b.shared(project, rt))
	}
	for _, c := range h.retired {
		retired = append(retired, clone(c))
	}
	return held, retired
}

// delivered notes that a heartbeat told the controller of held and retired:
// those held bases are fresh no longer, and those cells need no more reporting.
func (h *heldBases) delivered(held []*pb.SharedBase, retired []*pb.CladefsCell) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, sb := range held {
		c := sb.GetCell()
		if cur := h.bases[c.GetProject()]; cur != nil && cur.Ref.Tenant == c.GetTenant() && cur.Ref.Cell == c.GetCell() {
			delete(h.fresh, c.GetProject())
		}
	}
	told := map[[2]uint64]bool{}
	for _, c := range retired {
		told[[2]uint64{c.GetTenant(), c.GetCell()}] = true
	}
	kept := h.retired[:0]
	for _, c := range h.retired {
		if !told[[2]uint64{c.GetTenant(), c.GetCell()}] {
			kept = append(kept, c)
		}
	}
	h.retired = kept
}

// retire deletes from the shared bucket the base cells the controller asks it
// to, but never one this node holds: its ask may predate this node taking it up.
func (h *heldBases) retire(ctx context.Context, store *cladefs.Store, cells []*pb.CladefsCell) {
	for _, c := range cells {
		if h.holds(c.GetTenant(), c.GetCell()) {
			continue
		}
		if err := store.DeleteBucketCell(ctx, c.GetTenant(), c.GetCell()); err != nil {
			slog.Warn("retire project base cell failed; the controller asks again", "baseId", c.GetBaseId(), "tenant", c.GetTenant(), "cell", c.GetCell(), "err", err)
			continue
		}
		slog.Info("retired project base cell", "baseId", c.GetBaseId(), "tenant", c.GetTenant(), "cell", c.GetCell())
		h.mu.Lock()
		h.retired = append(h.retired, clone(c))
		h.mu.Unlock()
	}
}

// baseRequest asks for a project's base on this node, whatever its runtime.
type baseRequest struct {
	project *pb.HostedProject
	heads   map[string]string
	// The cluster's newest base of the project for this node's arch and
	// runtime, as the controller last saw it; nil when there is none.
	shared  *pb.SharedBase
	runtime string // runtime.KindVM or runtime.KindCladefs
	tenant  uint64
	baseKey string
	recipe  string
	baseID  string
	// Called with the base's ID before it is built.
	onBaseTarget func(baseID string)
	opts         ensureBaseOpts
	// build builds plan's base and uploads it to the shared bucket.
	build func(ctx context.Context, plan basePlan) (builtBase, error)
}

// basePlan is a base to build.
type basePlan struct {
	baseID string
	recipe string
	// The base to build on top of; nil builds it from the base image.
	prev       *projectBase
	coldReason string
	required   bool
}

// builtBase is what building a base made.
type builtBase struct {
	ref       cladefs.CellRef // with the version uploaded
	repoHeads map[string]string
	steps     int
	active    time.Duration // time the build ran, for the freshen budget
}

// baseBuildError is a failed build, as the freshen budget and a fallback note tell it.
type baseBuildError struct {
	cmdID      string
	summary    string
	stepFailed bool
	active     time.Duration
	err        error
}

func (e *baseBuildError) Error() string { return e.err.Error() }
func (e *baseBuildError) Unwrap() error { return e.err }

// incrementalForkError is a build that could not fork the base it was to
// build on.
type incrementalForkError struct{ err error }

func (e *incrementalForkError) Error() string { return "fork the base to build on: " + e.err.Error() }
func (e *incrementalForkError) Unwrap() error { return e.err }

// ensure returns the base to fork a workspace of r.project from: the one this
// node holds or the cluster's newest, whichever is up to date, or else a new
// one built on top of the newer of them (or kept while the freshen budget says
// so). The base it returns is the one this node holds from then on.
func (h *heldBases) ensure(ctx context.Context, r baseRequest) (ensureBaseResult, error) {
	opts := r.opts
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	p := r.project
	name := p.GetName()
	setupHash := projectSetupHash(p)
	shared := sharedProjectBase(r.shared, r.tenant)
	pick := func() *projectBase {
		held, fresh := h.get(name)
		return pickBase(held, shared, fresh, p, r.baseKey, r.recipe)
	}
	handOut := func(b *projectBase, note string, deferBackground bool) (ensureBaseResult, error) {
		if held, _ := h.get(name); b.Recipe == r.recipe && (held == nil || held.Ref != b.Ref) {
			opts.Budget.recordAdopted(name, r.runtime)
		}
		b = h.hold(name, b, false, b.Recipe == r.recipe)
		return ensureBaseResult{Base: b, Note: note, DeferBackground: deferBackground}, nil
	}

	// A first look without the project's lock: hand out an up-to-date base,
	// or one the budget says to keep for now, rather than wait behind a
	// freshen under way.
	cur := pick()
	if cur.usable() && cur.Recipe == r.recipe {
		return handOut(cur, "", false)
	}
	if opts.Budget != nil {
		drift, cold := baseHasStructuralDrift(p, cur, r.baseKey)
		if !drift && cold == "" {
			dec := opts.Budget.decide(now(), name, r.runtime, r.recipe, setupHash,
				p.GetScratchEpoch(), cur.BaseID, true, false, opts.AllowBackground, opts.Background)
			switch dec.action {
			case freshenActionSkip:
				logThrottled(name, r.runtime, cur.BaseID, dec)
				return handOut(cur, dec.note, false)
			case freshenActionDeferBackground:
				return handOut(cur, "", true)
			}
		} else if opts.OnRequireBuild != nil {
			opts.OnRequireBuild()
		}
	}

	lock := h.lock(name)
	lock.Lock()
	defer lock.Unlock()

	fallback := pick()
	if fallback.usable() && fallback.Recipe == r.recipe {
		return handOut(fallback, "", false)
	}
	drift, coldReason := baseHasStructuralDrift(p, fallback, r.baseKey)
	plan := basePlan{baseID: r.baseID, recipe: r.recipe, coldReason: coldReason, required: drift}
	if coldReason == "" {
		plan.prev = fallback
	}
	if opts.Budget != nil {
		prevID := ""
		if fallback != nil {
			prevID = fallback.BaseID
		}
		dec := opts.Budget.decide(now(), name, r.runtime, r.recipe, setupHash,
			p.GetScratchEpoch(), prevID, fallback != nil, plan.required, opts.AllowBackground, opts.Background)
		switch dec.action {
		case freshenActionSkip:
			if fallback == nil {
				return ensureBaseResult{}, dec.err
			}
			logThrottled(name, r.runtime, fallback.BaseID, dec)
			return handOut(fallback, dec.note, false)
		case freshenActionDeferBackground:
			return handOut(fallback, "", true)
		}
	}

	if r.onBaseTarget != nil {
		r.onBaseTarget(r.baseID)
	}
	built, err := r.build(ctx, plan)
	// A base to build on that cannot be forked is gone (retired while this
	// node still had it to hand): build on the held base instead, if that is
	// another, else from the base image.
	for forkErr := (*incrementalForkError)(nil); errors.As(err, &forkErr); {
		failed := plan.prev
		slog.Warn("could not fork the base to build on",
			"project", name, "runtime", r.runtime, "prevBase", failed.BaseID, "err", forkErr.err)
		if cladefs.IsGone(forkErr.err) {
			h.forget(name, failed.Ref)
		}
		fallback, plan.prev = nil, nil
		plan.coldReason = "incremental_fork_failed"
		if held, _ := h.get(name); held.usable() && held.Ref != failed.Ref && buildableOn(held, p, r.baseKey) {
			fallback, plan.prev, plan.coldReason = held, held, ""
		}
		built, err = r.build(ctx, plan)
	}
	if err != nil {
		be := (*baseBuildError)(nil)
		if !errors.As(err, &be) {
			be = &baseBuildError{summary: fmt.Sprintf("base %s failed: %v", r.baseID, err), err: err}
		}
		shutdown := ctx.Err() != nil
		opts.Budget.recordFailure(now(), name, r.runtime, r.recipe, setupHash,
			p.GetScratchEpoch(), r.baseID, be.cmdID, be.summary, be.stepFailed, fallback != nil, shutdown, be.active)
		if fallback != nil && !opts.Background && !shutdown {
			slog.Warn("project base freshen failed; falling back to the warm base",
				"project", name, "runtime", r.runtime, "failedBase", r.baseID, "fallbackBase", fallback.BaseID, "err", be.err)
			return handOut(fallback, freshenFallbackNote(fallback.BaseID, be.summary), false)
		}
		return ensureBaseResult{}, be.err
	}
	opts.Budget.recordSuccess(now(), name, r.runtime, built.active)

	repos := built.repoHeads
	if len(repos) == 0 {
		repos = maps.Clone(r.heads)
	}
	res := h.hold(name, &projectBase{
		Ref:          built.ref,
		BaseID:       r.baseID,
		Recipe:       r.recipe,
		BaseKey:      r.baseKey,
		SetupHash:    setupHash,
		ScratchEpoch: p.GetScratchEpoch(),
		Repos:        repos,
		StepHashes:   projectStepHashes(p),
	}, true, true)
	fromBaseID := ""
	if plan.prev != nil {
		fromBaseID = plan.prev.BaseID
	}
	slog.Info("project base built and uploaded to the shared bucket",
		"project", name, "runtime", r.runtime, "baseId", r.baseID, "from", fromBaseID,
		"optional", !plan.required, "steps", built.steps, "cell", built.ref.Cell, "version", built.ref.Version)
	return ensureBaseResult{Base: res}, nil
}

func logThrottled(project, rt, warmBase string, dec freshenDecision) {
	if dec.throttledUntilMs > 0 {
		slog.Info("skipping optional base freshen (throttled)",
			"project", project, "runtime", rt, "warmBase", warmBase,
			"nextAllowedAt", time.UnixMilli(dec.throttledUntilMs).Format(time.RFC3339))
	}
}
