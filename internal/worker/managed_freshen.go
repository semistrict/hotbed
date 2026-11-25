package worker

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/cladefs"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/runtime"
)

// isCladefsBase reports whether b is an R2-backed cladefs base (Firecracker VM
// or cladefs host-FUSE) rather than a local ZFS/btrfs chroot base.
func isCladefsBase(b *pb.Base) bool {
	return b != nil && b.GetCladefsCell() != nil
}

// isManagedFreshen reports whether c is a VM or cladefs-FUSE base freshen
// managed by startManagedFreshen rather than a chroot Engine.freshened build.
func (c *command) isManagedFreshen() bool {
	return c != nil && c.managedFreshen != nil
}

// isVMFreshen reports whether c is a Firecracker VM base freshen (runs inside a
// temporary guest VM rather than a host chroot with host tmpDir).
func (c *command) isVMFreshen() bool {
	return c != nil && c.managedFreshen != nil && c.managedFreshen.vmID != ""
}

type managedFreshen struct {
	// rootDir, when non-empty, is the host FUSE mount rootfs for a cladefs-FUSE
	// base build (used instead of e.volPath(c.rec.Volume)).
	rootDir string
	// vmID, when non-empty, is the temporary Firecracker VM ID ("base-<baseID>")
	// for a VM base build; routed through runtimeUnits instead of host systemd.
	vmID string
	// beforeStart, when non-nil, runs inside admit() after admission checks pass
	// and before Units.Start (e.g. booting the temporary base Firecracker VM).
	beforeStart func(ctx context.Context) error
	// onStartFail, when non-nil, runs if Units.Start fails or the command is
	// cancelled after beforeStart succeeded (e.g. stopping the temporary base VM).
	onStartFail func()
	// done closes once endBy() finishes the command (and dropTmp completes for
	// host-chroot FUSE builds).
	done chan struct{}
}

// managedFreshenSpec describes a VM or cladefs-FUSE project base build to be
// tracked in e.bases + e.commands and scheduled through Engine admission control.
type managedFreshenSpec struct {
	Project     *pb.HostedProject
	BaseID      string
	Recipe      string
	Arch        string
	Runtime     string
	Cold        bool
	ColdReason  string
	FromBaseID  string
	Optional    bool
	CreatedAt   int64
	PrepSeconds float64
	PrepPhases  []*pb.FreshenPhase
	PrevBase    *vmProjectBase
	StepsToRun  []*pb.Step
	Heads       map[string]string
	Script      string
	Env         []string
	// Exactly one of RootDir (cladefs-FUSE chroot) or VMID (Firecracker VM) is set.
	RootDir     string
	VMID        string
	BeforeStart func(ctx context.Context) error
	OnStartFail func()
}

// managedFreshenResult summarizes the completed managed freshen command.
type managedFreshenResult struct {
	BaseID           string
	CommandID        string
	RepoHeads        map[string]string
	Steps            []*pb.AppliedStep
	Baked            []string
	Units            int
	PeakMemory       int64
	Seconds          float64
	ActiveRunSeconds float64
	Started          bool
	StepFailed       bool
	GitSeconds       float64
	FinalizeSeconds  float64
	State            pb.CommandState
	Reason           string
}

type managedFreshenRunner func(ctx context.Context, spec managedFreshenSpec) (*managedFreshenHandle, error)

type managedFreshenHandle struct {
	e          *Engine
	baseID     string
	cmdID      string
	vmID       string
	done       chan struct{}
	stubResult *managedFreshenResult
}

func (h *managedFreshenHandle) CommandID() string {
	if h == nil {
		return ""
	}
	return h.cmdID
}

// buildCladefsFreshenScript produces one bash script that clones/fetches/checks
// out all project repos at heads, prints ::hotbed-head:: lines, and executes
// stepsToRun wrapped with ::hotbed:: step markers so Engine.output tracks live
// step progress, timings, and compiled unit counts.
func buildCladefsFreshenScript(p *pb.HostedProject, heads map[string]string, stepsToRun []*pb.Step, checkGitmodules bool) string {
	var b strings.Builder
	b.WriteString("set -euo pipefail\nmkdir -p /work\ncd /work\nt_git=${EPOCHREALTIME//[!0-9]/}\n")
	for _, r := range p.GetRepos() {
		branch := cmp.Or(r.GetBranch(), "main")
		commit := heads[r.GetId()]
		fmt.Fprintf(&b, `
if [ -d %q/.git ]; then
  git -C %q fetch --prune origin %q
else
  git clone --quiet --filter=blob:none --branch %q --single-branch %q %q
fi
`, r.GetId(), r.GetId(), branch, branch, r.GetUrl(), r.GetId())
		if commit != "" {
			fmt.Fprintf(&b, `
if ! git -C %q cat-file -e %q^{commit} 2>/dev/null; then
  git -C %q fetch --quiet origin %q
fi
git -C %q checkout -B %q %q
`, r.GetId(), commit, r.GetId(), commit, r.GetId(), branch, commit)
		} else {
			fmt.Fprintf(&b, `
if git -C %q rev-parse --verify FETCH_HEAD >/dev/null 2>&1; then
  git -C %q checkout -B %q FETCH_HEAD
fi
`, r.GetId(), r.GetId(), branch)
		}
		if checkGitmodules {
			fmt.Fprintf(&b, `
if [ -f %q/.gitmodules ]; then
  git -C %q submodule update --quiet --init --recursive --force
fi
`, r.GetId(), r.GetId())
		} else {
			fmt.Fprintf(&b, `
git -C %q submodule update --quiet --init --recursive --force
`, r.GetId())
		}
		fmt.Fprintf(&b, `
if [ -f %q/Cargo.toml ] && [ ! -f %q/rust-toolchain.toml ] && [ ! -f %q/rust-toolchain ] && command -v rustup >/dev/null; then
  rustup show active-toolchain >/dev/null 2>&1 || rustup default stable
fi
printf '::hotbed-head:: %s %%s\n' "$(git -C %q rev-parse HEAD)"
`, r.GetId(), r.GetId(), r.GetId(), r.GetId(), r.GetId())
	}
	b.WriteString("printf '::hotbed:: phase-done git %d\\n' \"$(( ${EPOCHREALTIME//[!0-9]/} - t_git ))\"\ncd /work\n")
	b.WriteString(stepsScript(stepsToRun))
	return b.String()
}

// plannedManagedSteps builds the initial AppliedStep list for a VM or
// cladefs-FUSE base build from the previous vmProjectBase (if any).
func plannedManagedSteps(p *pb.HostedProject, prev *vmProjectBase, run []*pb.Step) []*pb.AppliedStep {
	var prevPB *pb.Base
	if prev != nil && len(prev.StepHashes) > 0 {
		prevPB = &pb.Base{}
		for name, hash := range prev.StepHashes {
			prevPB.Steps = append(prevPB.Steps, &pb.AppliedStep{Name: name, Hash: hash, Ok: true})
		}
	}
	return plannedSteps(p, prevPB, run)
}

// cladefsBaseRuntimeKind distinguishes VM and host-FUSE base IDs so warming a
// VM base does not mark a host-FUSE base stale (or vice versa) on the same node.
func cladefsBaseRuntimeKind(baseID string) string {
	switch {
	case strings.Contains(baseID, "-vm-"):
		return "vm"
	case strings.Contains(baseID, "-fuse-"):
		return "cladefs"
	default:
		return "r2"
	}
}

// nextCladefsBaseGenLocked returns the next monotonically increasing generation
// number for R2 bases of project on this node (starting at 1). Call with e.mu held.
func (e *Engine) nextCladefsBaseGenLocked(project string) int32 {
	var newest int32
	for _, b := range e.bases {
		if b.Project == project && isCladefsBase(b) && b.Generation > newest {
			newest = b.Generation
		}
	}
	return newest + 1
}

// setPreparingWorkspaceBase links a preparing VM or cladefs-FUSE workspace to
// the target baseID so workspaceView and WorkspaceFreshenCommand expose live
// queue reasons, step progress, and the freshen command's log URL immediately.
func (e *Engine) setPreparingWorkspaceBase(wsID, baseID string) {
	if wsID == "" || baseID == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	w, ok := e.workspaces[wsID]
	if !ok || w.ws().State != preparing || w.releasing {
		return
	}
	if w.ws().BaseId == baseID {
		return
	}
	w.ws().BaseId = baseID
	w.rec.BaseSnapshot = baseID
	w.ws().Reason = fmt.Sprintf("waiting for base %s to freshen", baseID)
	e.notify()
}

// recordWarmCladefsBase ensures an adopted or cached warm R2 project base has a
// warm record in e.bases so project bases listings and workspace views know it.
func (e *Engine) recordWarmCladefsBase(p *pb.HostedProject, pbBase *vmProjectBase, arch string) {
	if p == nil || pbBase == nil || pbBase.BaseID == "" || pbBase.Ref.Cell == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if cur, ok := e.bases[pbBase.BaseID]; ok && cur.State == warm {
		return
	}
	now := e.nowMs()
	gen := e.nextCladefsBaseGenLocked(p.Name)
	b := &pb.Base{
		Id:           pbBase.BaseID,
		Project:      p.Name,
		NodeId:       e.cfg.NodeID,
		Generation:   gen,
		Runtime:      cladefsBaseRuntimeKind(pbBase.BaseID),
		State:        warm,
		Progress:     1,
		BuiltAt:      &now,
		CreatedAt:    now,
		ScratchEpoch: pbBase.ScratchEpoch,
		Recipe:       pbBase.Recipe,
		CladefsCell: &pb.CladefsCell{
			Tenant:  pbBase.Ref.Tenant,
			Cell:    pbBase.Ref.Cell,
			Version: pbBase.Ref.Version,
			BaseId:  pbBase.BaseID,
			Project: p.Name,
			Arch:    arch,
			Recipe:  pbBase.Recipe,
			Runtime: cladefsBaseRuntimeKind(pbBase.BaseID),
		},
	}
	for _, r := range p.GetRepos() {
		b.Repos = append(b.Repos, &pb.BaseRepo{Repo: r.GetId(), Commit: pbBase.Repos[r.GetId()]})
	}
	for _, s := range p.GetSteps() {
		b.Baked = append(b.Baked, s.GetName())
		b.Steps = append(b.Steps, &pb.AppliedStep{Name: s.GetName(), Hash: s.GetHash(), Ok: true})
	}
	e.bases[b.Id] = b
	e.markSupersededCladefsBasesLocked(b)
	e.saveBase(b, change{what: "warm", detail: "adopted from R2"})
	e.collectSoon()
}

// markSupersededCladefsBasesLocked marks older warm R2 bases of the same
// project and runtime kind as stale. Call with e.mu held.
func (e *Engine) markSupersededCladefsBasesLocked(b *pb.Base) {
	kind := cladefsBaseRuntimeKind(b.Id)
	for _, o := range e.bases {
		if o == b || o.Project != b.Project || !isCladefsBase(o) || o.State != warm {
			continue
		}
		if cladefsBaseRuntimeKind(o.Id) != kind {
			continue
		}
		o.State = stale
		e.saveBase(o, change{what: "stale", detail: "superseded by " + b.Id})
	}
}

// startManagedFreshen registers a VM or cladefs-FUSE project base build in
// e.bases and queues its freshen command in e.commands so it passes through
// normal admission control, parking/thawing, live logging, and telemetry.
func (e *Engine) startManagedFreshen(_ context.Context, spec managedFreshenSpec) (*managedFreshenHandle, error) {
	if spec.Project == nil || spec.BaseID == "" {
		return nil, fmt.Errorf("managed freshen missing project or base ID")
	}
	cmdID := fmt.Sprintf("f-%s-%s", spec.BaseID, randomHex(3))
	if err := validateID(cmdID); err != nil {
		return nil, err
	}
	now := e.nowMs()
	done := make(chan struct{})

	e.mu.Lock()
	if e.closing {
		e.mu.Unlock()
		return nil, fmt.Errorf("worker is shutting down")
	}
	gen := e.nextCladefsBaseGenLocked(spec.Project.Name)
	createdAt := spec.CreatedAt
	if createdAt <= 0 {
		createdAt = now
	}
	rtKind := cmp.Or(spec.Runtime, cladefsBaseRuntimeKind(spec.BaseID))
	fromBaseID := spec.FromBaseID
	if fromBaseID == "" && !spec.Cold && spec.PrevBase != nil {
		fromBaseID = spec.PrevBase.BaseID
	}
	b := &pb.Base{
		Id:           spec.BaseID,
		Project:      spec.Project.Name,
		NodeId:       e.cfg.NodeID,
		Generation:   gen,
		Runtime:      rtKind,
		State:        building,
		Cold:         spec.Cold,
		ColdReason:   spec.ColdReason,
		FromBaseId:   fromBaseID,
		CreatedAt:    createdAt,
		PrepSeconds:  spec.PrepSeconds,
		TotalSteps:   int32(len(spec.StepsToRun)),
		Phases:       spec.PrepPhases,
		Command:      cmdID,
		ScratchEpoch: spec.Project.ScratchEpoch,
		Steps:        plannedManagedSteps(spec.Project, spec.PrevBase, spec.StepsToRun),
		Recipe:       spec.Recipe,
		CladefsCell: &pb.CladefsCell{
			BaseId:  spec.BaseID,
			Project: spec.Project.Name,
			Arch:    spec.Arch,
			Recipe:  spec.Recipe,
			Runtime: rtKind,
		},
	}
	for _, r := range spec.Project.GetRepos() {
		b.Repos = append(b.Repos, &pb.BaseRepo{Repo: r.GetId(), Commit: spec.Heads[r.GetId()]})
	}
	cmdRuntime := runtime.KindCladefs
	if spec.VMID != "" {
		cmdRuntime = runtime.KindVM
	}
	limits := &pb.CommandLimits{
		CpuWeight:        100,
		HardLimitSeconds: e.freshenBudget.hardLimitSeconds(spec.Optional),
	}
	if spec.Optional {
		limits.Priority = optionalFreshenPriority
		limits.CpuWeight = optionalFreshenCPUWeight
	}
	c := e.newCommand(&pb.CommandRecord{
		Command: &pb.Command{
			Id:       cmdID,
			NodeId:   e.cfg.NodeID,
			Argv:     fmt.Sprintf("freshen %s", spec.BaseID),
			Project:  spec.Project.Name,
			Args:     []string{"/bin/bash", "-c", spec.Script},
			Kind:     "freshen",
			Runtime:  cmdRuntime,
			State:    queued,
			QueuedAt: now,
			Limits:   limits,
			Timeline: []*pb.CommandEvent{{At: now, What: "queued", Detail: "base " + b.Id}},
		},
		Project: spec.Project.Name,
		Agent:   "hotbed",
		Dir:     WorkPath,
		Env:     spec.Env,
		BaseId:  b.Id,
	})
	c.managedFreshen = &managedFreshen{
		rootDir:     spec.RootDir,
		vmID:        spec.VMID,
		beforeStart: spec.BeforeStart,
		onStartFail: spec.OnStartFail,
		done:        done,
	}
	c.cmd().Limits.MemoryLowBytes = e.freshenGuarantee(spec.Project.Name)
	if spec.VMID != "" {
		e.vmWorkspaces.Store(spec.VMID, struct{}{})
	}
	from := "from nothing"
	if !spec.Cold && spec.PrevBase != nil {
		from = "from " + spec.PrevBase.BaseID
	}
	e.bases[b.Id] = b
	e.saveBase(b, change{
		what:   "building",
		detail: from,
		feed:   feed(feedBase, fmt.Sprintf("base %s building %s at %s", b.Id, from, describeHeads(b.Repos)), ref("command", cmdID)),
	})
	e.commands[cmdID] = c
	e.saveCommand(c, change{what: "queued", detail: "base " + b.Id})
	e.stepSoon()
	e.mu.Unlock()

	return &managedFreshenHandle{
		e:      e,
		baseID: b.Id,
		cmdID:  cmdID,
		vmID:   spec.VMID,
		done:   done,
	}, nil
}

// Wait blocks until the managed freshen command finishes (including tmp cleanup
// for host-chroot FUSE builds) or ctx is cancelled.
func (h *managedFreshenHandle) Wait(ctx context.Context) (managedFreshenResult, error) {
	if h.e == nil && h.stubResult != nil {
		return *h.stubResult, nil
	}
	select {
	case <-h.done:
	case <-ctx.Done():
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if _, err := h.e.Stop(stopCtx, h.cmdID, "base build cancelled"); err != nil && api.CodeOf(err) != api.CodeNotFound {
			slog.Warn("stop cancelled managed freshen command", "command", h.cmdID, "base", h.baseID, "err", err)
		}
		cancel()
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
		}
		return h.collectResult(), ctx.Err()
	}
	return h.collectResult(), nil
}

func (h *managedFreshenHandle) collectResult() managedFreshenResult {
	h.e.mu.Lock()
	defer h.e.mu.Unlock()
	res := managedFreshenResult{
		BaseID:    h.baseID,
		CommandID: h.cmdID,
		RepoHeads: map[string]string{},
	}
	c, ok := h.e.commands[h.cmdID]
	if !ok {
		res.State = failed
		res.Reason = "freshen command disappeared"
		return res
	}
	cmd := c.cmd()
	res.State = cmd.State
	res.Reason = cmd.Reason
	res.PeakMemory = c.rec.PeakHeldBytes
	startedAt := cmd.QueuedAt
	if cmd.StartedAt != nil {
		startedAt = *cmd.StartedAt
		res.Started = true
	}
	endedAt := h.e.nowMs()
	if cmd.EndedAt != nil {
		endedAt = *cmd.EndedAt
	}
	res.Seconds = max(0, float64(endedAt-startedAt)/1000)
	if res.Started {
		res.ActiveRunSeconds = commandActiveSeconds(cmd, endedAt)
	}
	res.Units, _ = c.out.result(cmd.State, cmd.ExitCode, cmd.Kind)
	res.GitSeconds = c.out.gitSeconds()
	res.RepoHeads = c.out.repoHeads()
	if b, ok := h.e.bases[h.baseID]; ok && b.Command == h.cmdID {
		res.Steps = c.out.settle(b.Steps)
		for _, s := range res.Steps {
			if s.Ok {
				res.Baked = append(res.Baked, s.Name)
			} else if s.Ran {
				res.StepFailed = true
			}
		}
	}
	return res
}

// RecordPhase appends a completed phase timing (e.g. vm_boot, vm_stop,
// mount_detach, r2_sync) to the managed freshen's Base record.
func (h *managedFreshenHandle) RecordPhase(name string, startedAt, endedAt int64, seconds float64, detail string) {
	if h == nil || h.e == nil || name == "" {
		return
	}
	h.e.mu.Lock()
	defer h.e.mu.Unlock()
	b, found := h.e.bases[h.baseID]
	if !found || b.Command != h.cmdID {
		return
	}
	b.Phases = append(b.Phases, &pb.FreshenPhase{
		Name:      name,
		StartedAt: startedAt,
		EndedAt:   &endedAt,
		Seconds:   seconds,
		Detail:    detail,
	})
}

// SetFinalizing marks the managed base as entering a post-script finalization
// phase (such as vm_stop, mount_detach, or r2_sync) and notifies the controller.
func (h *managedFreshenHandle) SetFinalizing(phase string) {
	if h == nil || h.e == nil {
		return
	}
	h.e.mu.Lock()
	defer h.e.mu.Unlock()
	b, found := h.e.bases[h.baseID]
	if !found || b.Command != h.cmdID || b.State != building {
		return
	}
	b.Progress = 1
	b.CurrentStep = ""
	b.CurrentStepIndex = 0
	b.CurrentStepStartedAt = 0
	h.e.saveBase(b, change{what: "finalizing", detail: phase})
}

// Finish records the final warm or failed state of the managed R2 base after
// post-command sync/cleanup completes, and releases the temporary VM routing
// entry if this was a VM base build.
func (h *managedFreshenHandle) Finish(ok bool, res managedFreshenResult, cell cladefs.CellRef, failDetail string) {
	if h == nil || h.e == nil {
		return
	}
	if h.vmID != "" {
		h.e.vmWorkspaces.Delete(h.vmID)
	}
	h.e.mu.Lock()
	defer h.e.mu.Unlock()
	b, found := h.e.bases[h.baseID]
	if !found || b.Command != h.cmdID {
		return
	}
	now := h.e.nowMs()
	b.PeakMemoryBytes = max(b.PeakMemoryBytes, res.PeakMemory)
	b.GitSeconds = max(b.GitSeconds, res.GitSeconds)
	b.FinalizeSeconds = max(b.FinalizeSeconds, res.FinalizeSeconds)
	b.CurrentStep = ""
	b.CurrentStepIndex = 0
	b.CurrentStepStartedAt = 0
	if len(res.Steps) > 0 {
		b.Steps = res.Steps
	}
	b.Baked = res.Baked
	if !ok {
		b.State = broken
		b.BuiltAt = &now
		b.BuildSeconds = res.Seconds
		why := baseFailure(b)
		if failDetail != "" && failedStep(b) == "" {
			why = fmt.Sprintf("base %s failed: %s (its log is command %s)", b.Id, failDetail, h.cmdID)
		}
		b.FailureReason = cmp.Or(failDetail, res.Reason, why)
		h.e.saveBase(b, change{what: "failed", detail: why, feed: feed(feedFail, why, ref("command", h.cmdID))})
		return
	}
	b.State = warm
	b.BuiltAt = &now
	b.BuildSeconds = res.Seconds
	b.Progress = 1
	if b.Cold {
		b.Units = int32(res.Units)
	}
	if b.CladefsCell == nil {
		b.CladefsCell = &pb.CladefsCell{BaseId: b.Id, Project: b.Project, Recipe: b.Recipe}
	}
	b.CladefsCell.Tenant = cell.Tenant
	b.CladefsCell.Cell = cell.Cell
	b.CladefsCell.Version = cell.Version
	for _, r := range b.Repos {
		if head, has := res.RepoHeads[r.Repo]; has && head != "" {
			r.Commit = head
		}
	}
	h.e.markSupersededCladefsBasesLocked(b)
	if h.e.projectRemoved(b.Project) {
		b.State = stale
	}
	what := "warm"
	if b.State == stale {
		what = "stale"
	}
	h.e.saveBase(b, change{
		what: what,
		feed: feed(feedBase, fmt.Sprintf("base %s warm · built in %s", b.Id, dur(res.Seconds)), nil),
	})
	h.e.collectSoon()
}
