package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	goruntime "runtime"
	"sync"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/statedir"
)

const (
	// defaultFreshenMaxDutyCycle bounds active base freshen time to 5% of wall
	// time (both per-project and node-wide) via GCRA cooldown debt.
	defaultFreshenMaxDutyCycle = 0.05
	// defaultFreshenMinInterval enforces at least 30m between optional
	// commit-only freshens of the same project on a node.
	defaultFreshenMinInterval = 30 * time.Minute
	// defaultFreshenMaxCooldown caps any single duty-cycle or failure backoff.
	defaultFreshenMaxCooldown = 6 * time.Hour
	// defaultFreshenFailureCooldown is the base exponential backoff after a
	// failed freshen (15m -> 30m -> 1h -> 2h -> 4h -> 6h cap).
	defaultFreshenFailureCooldown = 15 * time.Minute
	// defaultFreshenColdFailureCooldown caps the initial retry backoff when a
	// project has no warm fallback base yet and failed for a non-step reason.
	defaultFreshenColdFailureCooldown = 2 * time.Minute
	// defaultFreshenMaxOptionalDuration caps optional commit-only freshen runs.
	defaultFreshenMaxOptionalDuration = 30 * time.Minute
	// defaultFreshenMaxRequiredDuration caps required (cold / setup / rebuild)
	// freshen runs while giving legitimate 22-31m cold builds plenty of room.
	defaultFreshenMaxRequiredDuration = 60 * time.Minute
	// defaultFreshenIdleDelay is how long the node must be quiet (no active user
	// commands, shells, or workspace prep/sync) before launching a deferred
	// low-priority background optional freshen.
	defaultFreshenIdleDelay = 15 * time.Second
	// defaultFreshenPendingTTL discards stale deferred background triggers that
	// never got an idle slot within maxQueueWait (15m).
	defaultFreshenPendingTTL = maxQueueWait

	// optionalFreshenPriority runs optional commit-only freshens at low priority
	// so normal user commands (priority 0) automatically preempt/park them and
	// never wait behind them.
	optionalFreshenPriority  int32 = -10
	optionalFreshenCPUWeight int32 = 20
)

type freshenPolicy struct {
	MaxDutyCycle        float64
	MinInterval         time.Duration
	MaxCooldown         time.Duration
	FailureCooldown     time.Duration
	MaxOptionalDuration time.Duration
	MaxRequiredDuration time.Duration
	IdleDelay           time.Duration
	DisableOptional     bool
}

func resolveFreshenPolicy(cfg *statedir.FreshenConfig) freshenPolicy {
	p := freshenPolicy{
		MaxDutyCycle:        defaultFreshenMaxDutyCycle,
		MinInterval:         defaultFreshenMinInterval,
		MaxCooldown:         defaultFreshenMaxCooldown,
		FailureCooldown:     defaultFreshenFailureCooldown,
		MaxOptionalDuration: defaultFreshenMaxOptionalDuration,
		MaxRequiredDuration: defaultFreshenMaxRequiredDuration,
		IdleDelay:           defaultFreshenIdleDelay,
	}
	if cfg == nil {
		return p
	}
	if cfg.MaxDutyCycle != nil && !math.IsNaN(*cfg.MaxDutyCycle) && !math.IsInf(*cfg.MaxDutyCycle, 0) && *cfg.MaxDutyCycle > 0 && *cfg.MaxDutyCycle <= 1 {
		p.MaxDutyCycle = *cfg.MaxDutyCycle
	}
	if cfg.MinIntervalSeconds != nil && *cfg.MinIntervalSeconds >= 0 {
		p.MinInterval = time.Duration(*cfg.MinIntervalSeconds) * time.Second
	}
	if cfg.MaxCooldownSeconds != nil && *cfg.MaxCooldownSeconds >= 0 {
		p.MaxCooldown = time.Duration(*cfg.MaxCooldownSeconds) * time.Second
	}
	if cfg.FailureCooldownSeconds != nil && *cfg.FailureCooldownSeconds >= 0 {
		p.FailureCooldown = time.Duration(*cfg.FailureCooldownSeconds) * time.Second
	}
	if cfg.MaxOptionalDurationSeconds != nil && *cfg.MaxOptionalDurationSeconds >= 0 {
		p.MaxOptionalDuration = time.Duration(*cfg.MaxOptionalDurationSeconds) * time.Second
	}
	if cfg.MaxRequiredDurationSeconds != nil && *cfg.MaxRequiredDurationSeconds >= 0 {
		p.MaxRequiredDuration = time.Duration(*cfg.MaxRequiredDurationSeconds) * time.Second
	}
	if cfg.IdleDelaySeconds != nil && *cfg.IdleDelaySeconds >= 0 {
		p.IdleDelay = time.Duration(*cfg.IdleDelaySeconds) * time.Second
	}
	if cfg.DisableOptional != nil {
		p.DisableOptional = *cfg.DisableOptional
	}
	return p
}

type freshenBudgetState struct {
	NodeNextAtMs int64                            `json:"nodeNextAtMs,omitempty"`
	Projects     map[string]*freshenProjectBudget `json:"projects,omitempty"`
}

type freshenProjectBudget struct {
	NextAtMs           int64   `json:"nextAtMs,omitempty"`
	LastBuiltAtMs      int64   `json:"lastBuiltAtMs,omitempty"`
	LastActiveSeconds  float64 `json:"lastActiveSeconds,omitempty"`
	FailedRecipe       string  `json:"failedRecipe,omitempty"`
	FailedSetupHash    string  `json:"failedSetupHash,omitempty"`
	FailedScratchEpoch int64   `json:"failedScratchEpoch,omitempty"`
	FailedBaseID       string  `json:"failedBaseId,omitempty"`
	FailedCommandID    string  `json:"failedCommandId,omitempty"`
	FailedReason       string  `json:"failedReason,omitempty"`
	FailedStep         bool    `json:"failedStep,omitempty"`
	FailureStreak      int     `json:"failureStreak,omitempty"`
	RetryAfterMs       int64   `json:"retryAfterMs,omitempty"`
}

func (ps *freshenProjectBudget) clearFailure() {
	ps.FailedRecipe = ""
	ps.FailedSetupHash = ""
	ps.FailedScratchEpoch = 0
	ps.FailedBaseID = ""
	ps.FailedCommandID = ""
	ps.FailedReason = ""
	ps.FailedStep = false
	ps.FailureStreak = 0
	ps.RetryAfterMs = 0
}

type freshenAction int

const (
	freshenActionRunForeground freshenAction = iota
	freshenActionDeferBackground
	freshenActionSkip
)

type freshenDecision struct {
	action           freshenAction
	required         bool
	note             string
	err              error
	throttledUntilMs int64
}

// freshenBudget enforces duty-cycle, minimum-interval, failure-suppression,
// exponential-backoff, and duration-cap policy across all worker base freshen
// paths (VM, CladefsFUSE, and local store). State is persisted in worker.db so
// restarts and auto-upgrades never wipe cooldown debt or failure backoff.
type freshenBudget struct {
	mu      sync.Mutex
	policy  freshenPolicy
	state   freshenBudgetState
	persist func([]byte) error
}

func newFreshenBudget(cfg *statedir.FreshenConfig, persist func([]byte) error) *freshenBudget {
	return &freshenBudget{
		policy: resolveFreshenPolicy(cfg),
		state: freshenBudgetState{
			Projects: map[string]*freshenProjectBudget{},
		},
		persist: persist,
	}
}

func (b *freshenBudget) load(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	var st freshenBudgetState
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}
	if st.Projects == nil {
		st.Projects = map[string]*freshenProjectBudget{}
	}
	b.mu.Lock()
	b.state = st
	b.mu.Unlock()
	return nil
}

func (b *freshenBudget) saveLocked() {
	if b.persist == nil {
		return
	}
	raw, err := json.Marshal(b.state)
	if err != nil {
		slog.Warn("marshal freshen budget state", "err", err)
		return
	}
	if err := b.persist(raw); err != nil {
		slog.Warn("persist freshen budget state", "err", err)
	}
}

func (b *freshenBudget) updateConfig(cfg *statedir.FreshenConfig) {
	b.mu.Lock()
	b.policy = resolveFreshenPolicy(cfg)
	b.mu.Unlock()
}

func (b *freshenBudget) currentPolicy() freshenPolicy {
	if b == nil {
		return resolveFreshenPolicy(nil)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.policy
}

func (b *freshenBudget) hardLimitSeconds(optional bool) int64 {
	p := b.currentPolicy()
	d := p.MaxRequiredDuration
	if optional {
		d = p.MaxOptionalDuration
	}
	if d <= 0 {
		return 0
	}
	return int64(d / time.Second)
}

func freshenBudgetKey(rt, project string) string {
	return rt + ":" + project
}

func (b *freshenBudget) projectLocked(rt, project string) *freshenProjectBudget {
	if b.state.Projects == nil {
		b.state.Projects = map[string]*freshenProjectBudget{}
	}
	key := freshenBudgetKey(rt, project)
	ps := b.state.Projects[key]
	if ps == nil {
		ps = &freshenProjectBudget{}
		b.state.Projects[key] = ps
	}
	return ps
}

// canStartOptional reports whether an optional commit-only freshen for
// (project, rt) is currently permitted by policy and duty-cycle/interval debt.
func (b *freshenBudget) canStartOptional(now time.Time, project, rt string) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.policy.DisableOptional {
		return false
	}
	nowMs := now.UnixMilli()
	if nowMs < b.state.NodeNextAtMs {
		return false
	}
	ps := b.state.Projects[freshenBudgetKey(rt, project)]
	if ps == nil {
		return true
	}
	if nowMs < ps.NextAtMs {
		return false
	}
	if ps.FailureStreak > 0 && nowMs < ps.RetryAfterMs {
		return false
	}
	return true
}

// decide evaluates whether a base freshen should run now, be deferred to an
// idle background slot, or be skipped in favor of the existing warm base (or a
// fast failure when no warm base exists).
func (b *freshenBudget) decide(
	now time.Time,
	project, rt, recipe, setupHash string,
	scratchEpoch int64,
	prevBaseID string,
	hasWarmBase, required, allowBackground, background bool,
) freshenDecision {
	if b == nil {
		return freshenDecision{action: freshenActionRunForeground, required: required}
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	nowMs := now.UnixMilli()
	key := freshenBudgetKey(rt, project)
	ps := b.state.Projects[key]

	// A project setup change (edited/added/removed steps or repos) or an
	// explicit `hotbed project rebuild` (higher scratchEpoch) immediately
	// clears failure suppression and backoff so the fix can be verified.
	if ps != nil && (ps.FailedRecipe != "" || ps.FailureStreak > 0) {
		setupChanged := ps.FailedSetupHash != "" && setupHash != "" && ps.FailedSetupHash != setupHash
		epochBumped := scratchEpoch > ps.FailedScratchEpoch
		if setupChanged || epochBumped {
			ps.clearFailure()
			b.saveLocked()
		}
	}

	// Failure suppression & exponential backoff.
	if ps != nil {
		sameFailedRecipe := ps.FailedRecipe != "" && ps.FailedRecipe == recipe
		inFailureBackoff := ps.FailureStreak > 0 && nowMs < ps.RetryAfterMs
		if sameFailedRecipe || inFailureBackoff {
			why := ps.FailedReason
			if why == "" {
				why = fmt.Sprintf("base %s failed to freshen", ps.FailedBaseID)
			}
			if hasWarmBase {
				return freshenDecision{
					action:   freshenActionSkip,
					required: required,
					note:     freshenFallbackNote(prevBaseID, why),
				}
			}
			// Cold project with no warm base yet.
			if sameFailedRecipe && ps.FailedStep {
				return freshenDecision{
					action:   freshenActionSkip,
					required: required,
					err: api.Errorf(api.CodeWorkspaceFailed, "%s", why).
						WithSuggestion("fix the step with hotbed step add (hotbed step ls shows them), then run again"),
				}
			}
			if inFailureBackoff {
				rem := (time.Duration(ps.RetryAfterMs-nowMs) * time.Millisecond).Round(time.Second)
				return freshenDecision{
					action:   freshenActionSkip,
					required: required,
					err: (&api.Error{
						Code:      api.CodeWorkspaceFailed,
						Message:   fmt.Sprintf("%s (retrying after %s backoff)", why, rem),
						Retryable: true,
					}).WithSuggestion(fmt.Sprintf("wait %s or run hotbed project rebuild %s to retry now", rem, project)),
				}
			}
		}
	}

	if required {
		return freshenDecision{action: freshenActionRunForeground, required: true}
	}

	// Commit-only drift with a structurally valid warm base: purely optional.
	if b.policy.DisableOptional {
		return freshenDecision{action: freshenActionSkip}
	}
	waitUntil := b.state.NodeNextAtMs
	if ps != nil && ps.NextAtMs > waitUntil {
		waitUntil = ps.NextAtMs
	}
	if nowMs < waitUntil {
		return freshenDecision{
			action:           freshenActionSkip,
			throttledUntilMs: waitUntil,
		}
	}
	if background {
		return freshenDecision{action: freshenActionRunForeground, required: false}
	}
	if allowBackground {
		return freshenDecision{action: freshenActionDeferBackground, required: false}
	}
	return freshenDecision{action: freshenActionRunForeground, required: false}
}

func (b *freshenBudget) chargeActiveLocked(now time.Time, ps *freshenProjectBudget, activeDur time.Duration) {
	if activeDur < 0 {
		activeDur = 0
	}
	var dutyWait time.Duration
	if b.policy.MaxDutyCycle > 0 && b.policy.MaxDutyCycle < 1 && activeDur > 0 {
		dutyWait = time.Duration(float64(activeDur) * ((1.0 / b.policy.MaxDutyCycle) - 1.0))
	}
	maxNext := now.Add(b.policy.MaxCooldown)
	advance := func(curMs int64, delta time.Duration) int64 {
		base := now
		if curMs > base.UnixMilli() {
			base = time.UnixMilli(curMs)
		}
		next := base.Add(delta)
		if next.After(maxNext) {
			next = maxNext
		}
		return next.UnixMilli()
	}
	if dutyWait > 0 {
		b.state.NodeNextAtMs = advance(b.state.NodeNextAtMs, dutyWait)
		ps.NextAtMs = advance(ps.NextAtMs, dutyWait)
	}
	if b.policy.MinInterval > 0 {
		minNext := now.Add(b.policy.MinInterval)
		if minNext.After(maxNext) {
			minNext = maxNext
		}
		if minNext.UnixMilli() > ps.NextAtMs {
			ps.NextAtMs = minNext.UnixMilli()
		}
	}
}

func (b *freshenBudget) recordSuccess(now time.Time, project, rt string, activeDur time.Duration) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	ps := b.projectLocked(rt, project)
	ps.clearFailure()
	ps.LastBuiltAtMs = now.UnixMilli()
	ps.LastActiveSeconds = activeDur.Seconds()
	b.chargeActiveLocked(now, ps, activeDur)
	b.saveLocked()
}

// recordAdopted clears any stale local failure state when a node adopts a
// matching warm shared base from R2 (which costs zero local build time).
func (b *freshenBudget) recordAdopted(project, rt string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	ps := b.state.Projects[freshenBudgetKey(rt, project)]
	if ps == nil || (ps.FailedRecipe == "" && ps.FailureStreak == 0) {
		return
	}
	ps.clearFailure()
	b.saveLocked()
}

func (b *freshenBudget) recordFailure(
	now time.Time,
	project, rt, recipe, setupHash string,
	scratchEpoch int64,
	baseID, cmdID, reason string,
	stepFailed, hasWarmBase, shutdown bool,
	activeDur time.Duration,
) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	ps := b.projectLocked(rt, project)
	if activeDur > 0 || !shutdown {
		b.chargeActiveLocked(now, ps, activeDur)
	}
	if !shutdown {
		ps.FailureStreak++
		ps.FailedRecipe = recipe
		ps.FailedSetupHash = setupHash
		ps.FailedScratchEpoch = scratchEpoch
		ps.FailedBaseID = baseID
		ps.FailedCommandID = cmdID
		ps.FailedReason = reason
		ps.FailedStep = stepFailed
		if b.policy.FailureCooldown > 0 {
			baseBackoff := b.policy.FailureCooldown
			if !hasWarmBase && baseBackoff > defaultFreshenColdFailureCooldown {
				baseBackoff = defaultFreshenColdFailureCooldown
			}
			shift := min(ps.FailureStreak-1, 6)
			backoff := baseBackoff << shift
			if backoff > b.policy.MaxCooldown {
				backoff = b.policy.MaxCooldown
			}
			retryAt := now.Add(backoff).UnixMilli()
			if retryAt > ps.RetryAfterMs {
				ps.RetryAfterMs = retryAt
			}
		}
	}
	b.saveLocked()
}

// projectSetupHash fingerprints everything about a project's base except repo
// HEAD commit SHAs: scratch epoch, repo IDs/URLs/branches, and step names,
// hashes, and --every-pull flags.
func projectSetupHash(p *pb.HostedProject) string {
	h := sha256.New()
	fmt.Fprintf(h, "setup-v1\nproject=%s\nepoch=%d\n", p.GetName(), p.GetScratchEpoch())
	for _, r := range p.GetRepos() {
		fmt.Fprintf(h, "repo=%s|%s|%s\n", r.GetId(), r.GetUrl(), r.GetBranch())
	}
	for _, s := range p.GetSteps() {
		fmt.Fprintf(h, "step=%s|%s|%v\n", s.GetName(), s.GetHash(), s.GetEveryPull())
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// baseHasStructuralDrift reports whether prev cannot structurally satisfy p
// (ignoring commit-only movement on existing repos).
func baseHasStructuralDrift(p *pb.HostedProject, prev *projectBase, baseKey string) (drift bool, coldReason string) {
	if prev == nil || prev.Ref.Cell == 0 || prev.Ref.Version == 0 {
		return true, "no_previous_base"
	}
	if prev.ScratchEpoch != p.GetScratchEpoch() {
		return true, "scratch_epoch"
	}
	if prev.BaseKey != baseKey {
		return true, "base_image_changed"
	}
	wantSetup := projectSetupHash(p)
	if prev.SetupHash != "" {
		return prev.SetupHash != wantSetup, ""
	}
	// Legacy cached project-*.json written before SetupHash existed: check
	// repo set and step hashes directly so upgrades don't force rebuilds.
	if len(prev.Repos) != len(p.GetRepos()) {
		return true, ""
	}
	for _, r := range p.GetRepos() {
		if _, ok := prev.Repos[r.GetId()]; !ok {
			return true, ""
		}
	}
	if len(prev.StepHashes) != len(p.GetSteps()) {
		return true, ""
	}
	for _, s := range p.GetSteps() {
		if prev.StepHashes[s.GetName()] != s.GetHash() {
			return true, ""
		}
	}
	return false, ""
}

// localBaseHasStructuralDrift reports whether a local warm pb.Base cannot
// structurally satisfy p (ignoring commit-only movement on existing repos).
func localBaseHasStructuralDrift(p *pb.HostedProject, prev *pb.Base, scratch bool) bool {
	if prev == nil || scratch {
		return true
	}
	if len(prev.Repos) != len(p.Repos) {
		return true
	}
	prevRepos := make(map[string]bool, len(prev.Repos))
	for _, r := range prev.Repos {
		prevRepos[r.Repo] = true
	}
	for _, r := range p.Repos {
		if !prevRepos[r.Id] {
			return true
		}
	}
	have := applied(prev)
	for _, s := range p.Steps {
		if have[s.Name] != s.Hash {
			return true
		}
	}
	return false
}

func freshenFallbackNote(prevBaseID, why string) string {
	return fmt.Sprintf("on %s, not up to date: %s", prevBaseID, why)
}

// ensureBaseOpts configures budget enforcement and background deferral for
// VMBackend and CladefsFUSEBackend EnsureProjectBase calls.
type ensureBaseOpts struct {
	Budget          *freshenBudget
	Now             func() time.Time
	AllowBackground bool
	Background      bool
	OnRequireBuild  func()
}

func managedFreshenFailureSummary(baseID, cmdID string, res managedFreshenResult, detail string) string {
	for _, s := range res.Steps {
		if s.Ran && !s.Ok {
			if cmdID != "" {
				return fmt.Sprintf("base %s failed at step %s (its log is command %s)", baseID, s.Name, cmdID)
			}
			return fmt.Sprintf("base %s failed at step %s", baseID, s.Name)
		}
	}
	if detail == "" {
		detail = res.Reason
	}
	if detail == "" {
		detail = "freshen failed"
	}
	if cmdID != "" {
		return fmt.Sprintf("base %s failed: %s (its log is command %s)", baseID, detail, cmdID)
	}
	return fmt.Sprintf("base %s failed: %s", baseID, detail)
}

type ensureBaseResult struct {
	Base            *projectBase
	Note            string
	DeferBackground bool
}

type pendingOptionalFreshen struct {
	project     *pb.HostedProject
	runtime     string
	sharedBase  *pb.SharedBase
	requestedAt time.Time
}

type bgFreshenHandle struct {
	cancel context.CancelFunc
}

// commandActiveSeconds returns how many seconds cmd actually ran (excluding
// time spent queued, parked, or paused) so duty-cycle accounting only charges
// active compute time and never penalizes a low-priority freshen for stepping
// aside while user commands run.
func commandActiveSeconds(cmd *pb.Command, endedAt int64) float64 {
	if cmd == nil || cmd.StartedAt == nil {
		return 0
	}
	startedAt := *cmd.StartedAt
	if endedAt <= startedAt {
		return 0
	}
	var activeMs int64
	runningSince := startedAt
	isRunning := true
	for _, ev := range cmd.GetTimeline() {
		if ev == nil {
			continue
		}
		at := ev.GetAt()
		if at < startedAt {
			continue
		}
		if at > endedAt {
			at = endedAt
		}
		switch ev.GetWhat() {
		case "parked", "paused":
			if isRunning {
				if at > runningSince {
					activeMs += at - runningSince
				}
				isRunning = false
			}
		case "thawed", "resumed":
			if !isRunning {
				runningSince = at
				isRunning = true
			}
		}
	}
	if isRunning && endedAt > runningSince {
		activeMs += endedAt - runningSince
	}
	return max(0, float64(activeMs)/1000)
}

func (e *Engine) recordUserActivityLocked(now time.Time) {
	if now.After(e.lastUserWorkAt) {
		e.lastUserWorkAt = now
	}
}

// scheduleOptionalFreshen queues a lightweight deferred optional base freshen
// to run in the background once the node has been quiet for policy.IdleDelay.
func (e *Engine) scheduleOptionalFreshen(p *pb.HostedProject, rt string, shared *pb.SharedBase) {
	if p == nil || p.GetName() == "" {
		return
	}
	key := freshenBudgetKey(rt, p.GetName())
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closing {
		return
	}
	now := e.now()
	e.recordUserActivityLocked(now)
	if e.pendingFreshens == nil {
		e.pendingFreshens = map[string]*pendingOptionalFreshen{}
	}
	var sb *pb.SharedBase
	if shared != nil {
		if b := e.cladefs(); b == nil || !b.IsLocalOnly() {
			sb = clone(shared)
		}
	}
	reqAt := now
	if existing, ok := e.pendingFreshens[key]; ok {
		// Preserve the first requested timestamp so continuous activity doesn't
		// starve FIFO ordering across projects, while refreshing project spec.
		reqAt = existing.requestedAt
	}
	e.pendingFreshens[key] = &pendingOptionalFreshen{
		project:     clone(p),
		runtime:     rt,
		sharedBase:  sb,
		requestedAt: reqAt,
	}
}

// cancelBgFreshen cancels any in-flight background optional freshen for project
// (for example when a required structural change or rebuild arrives and needs
// projectLock immediately).
func (e *Engine) cancelBgFreshen(project string) {
	e.mu.Lock()
	var cancels []context.CancelFunc
	for k, h := range e.bgFreshenCancels {
		if project == "" || k == project {
			if h != nil && h.cancel != nil {
				cancels = append(cancels, h.cancel)
			}
			delete(e.bgFreshenCancels, k)
		}
	}
	for key, pf := range e.pendingFreshens {
		if project == "" || pf.project.GetName() == project {
			delete(e.pendingFreshens, key)
		}
	}
	e.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// hasActiveUserWorkLocked reports whether any user command, shell, or workspace
// prep/sync is currently active on the node. Call with e.mu held.
func (e *Engine) hasActiveUserWorkLocked() bool {
	if len(e.shells) > 0 {
		return true
	}
	for _, w := range e.workspaces {
		ws := w.ws()
		if w.forking || ws.State == preparing || isSyncInProgress(ws.Reason) {
			return true
		}
	}
	for _, c := range e.commands {
		cmd := c.cmd()
		if !api.Live(cmd.State) || cmd.State == paused || cmd.Kind == "server" || cmd.Kind == "freshen" {
			continue
		}
		return true
	}
	return false
}

// hasLiveFreshenLocked reports whether any base freshen is currently active on
// this node (command, finalizing base, or background freshen goroutine).
// Call with e.mu held.
func (e *Engine) hasLiveFreshenLocked() bool {
	if len(e.bgFreshenCancels) > 0 || len(e.finalizingBases) > 0 {
		return true
	}
	for _, c := range e.commands {
		if c.isFreshen() && api.Live(c.cmd().State) {
			return true
		}
	}
	for _, b := range e.bases {
		if b.State == building {
			return true
		}
	}
	return false
}

// maybeStartPendingFreshens launches at most one deferred low-priority
// background optional freshen when the node is quiet, unheld, and within budget.
func (e *Engine) maybeStartPendingFreshens(ctx context.Context) {
	now := e.now()
	policy := e.freshenBudget.currentPolicy()

	e.mu.Lock()
	for k, pf := range e.pendingFreshens {
		if now.Sub(pf.requestedAt) >= defaultFreshenPendingTTL || e.projectRemoved(pf.project.GetName()) {
			delete(e.pendingFreshens, k)
		}
	}
	if e.hasActiveUserWorkLocked() {
		e.recordUserActivityLocked(now)
		e.mu.Unlock()
		return
	}
	if len(e.pendingFreshens) == 0 || e.closing || e.systemUpgrading {
		e.mu.Unlock()
		return
	}
	if e.hasLiveFreshenLocked() {
		e.mu.Unlock()
		return
	}
	if policy.IdleDelay > 0 && !e.lastUserWorkAt.IsZero() && now.Sub(e.lastUserWorkAt) < policy.IdleDelay {
		e.mu.Unlock()
		return
	}
	if cladefsHeld, _ := e.cladefsMemoryStarved(); cladefsHeld || e.held.kind != "" || e.diskLow() ||
		e.memPressureHolding || e.usage.GetMemoryPressure() >= admitPressure || e.cpu.holding {
		e.mu.Unlock()
		return
	}

	var chosenKey string
	var chosen *pendingOptionalFreshen
	for k, pf := range e.pendingFreshens {
		if now.Sub(pf.requestedAt) > defaultFreshenPendingTTL || e.projectRemoved(pf.project.GetName()) {
			delete(e.pendingFreshens, k)
			continue
		}
		if !e.freshenBudget.canStartOptional(now, pf.project.GetName(), pf.runtime) {
			continue
		}
		if chosen == nil || pf.requestedAt.Before(chosen.requestedAt) {
			chosenKey = k
			chosen = pf
		}
	}
	if chosen == nil {
		e.mu.Unlock()
		return
	}
	delete(e.pendingFreshens, chosenKey)
	if e.bgFreshenCancels == nil {
		e.bgFreshenCancels = map[string]*bgFreshenHandle{}
	}
	projName := chosen.project.GetName()
	pf := chosen
	parentCtx := e.bgCtx
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	runCtx, cancel := context.WithCancel(parentCtx)
	handle := &bgFreshenHandle{cancel: cancel}
	e.bgFreshenCancels[projName] = handle
	started := e.startBgLocked(func(_ context.Context) {
		defer func() {
			cancel()
			e.mu.Lock()
			if e.bgFreshenCancels[projName] == handle {
				delete(e.bgFreshenCancels, projName)
			}
			e.mu.Unlock()
		}()
		e.runBackgroundOptionalFreshen(runCtx, pf)
	})
	if !started {
		cancel()
		if e.bgFreshenCancels[projName] == handle {
			delete(e.bgFreshenCancels, projName)
		}
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()
}

func (e *Engine) runBackgroundOptionalFreshen(ctx context.Context, pf *pendingOptionalFreshen) {
	p := pf.project
	// Pick up the freshest HostedProject from the latest heartbeat if available.
	e.mu.Lock()
	if latest := e.projects[p.GetName()]; latest != nil {
		p = clone(latest)
	}
	e.mu.Unlock()

	heads, err := e.remoteHeads(ctx, p)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("background optional freshen remoteHeads failed", "project", p.GetName(), "err", err)
		}
		return
	}
	opts := ensureBaseOpts{
		Budget:     e.freshenBudget,
		Now:        e.now,
		Background: true,
	}
	baseSpec, prov := e.cfg.BaseSpec, e.baseProvisioner()

	if b := e.cladefs(); b != nil && pf.runtime == b.Kind() {
		res, err := b.ensureProjectBase(ctx, p, heads, pf.sharedBase, baseSpec, prov, e.sandboxEnv(), nil, e.startManagedFreshen, opts)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("background optional cladefs freshen failed", "project", p.GetName(), "runtime", b.Kind(), "err", err)
			}
			return
		}
		if res.Base != nil {
			e.recordWarmCladefsBase(p, res.Base, goruntime.GOARCH)
		}
		return
	}
	lock := e.projectLock(p.GetName())
	lock.Lock()
	_, _, _, err = e.baseForWithOpts(ctx, p, false, true)
	lock.Unlock()
	if err != nil && ctx.Err() == nil {
		slog.Warn("background optional local freshen failed", "project", p.GetName(), "err", err)
	}
}
