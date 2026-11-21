package worker

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/unit"
)

// cacheEnv puts everything a build would keep in the home directory inside
// the workspace, so a base carries it and every fork starts with it.
func cacheEnv() []string {
	h := WorkPath + "/.hotbed"
	return []string{
		"RUSTUP_HOME=" + h + "/rustup",
		"CARGO_HOME=" + h + "/cargo",
		"RUSTUP_AUTO_INSTALL=1",
		"GOPATH=" + h + "/go",
		"GOMODCACHE=" + h + "/go/mod",
		"GOCACHE=" + h + "/cache/go-build",
		"XDG_CACHE_HOME=" + h + "/cache",
		"XDG_DATA_HOME=" + h + "/data",
		"XDG_STATE_HOME=" + h + "/state",
		"COREPACK_HOME=" + h + "/corepack",
		"npm_config_cache=" + h + "/cache/npm",
		"CARGO_TERM_COLOR=never",
	}
}

var cachePath = []string{WorkPath + "/.hotbed/cargo/bin", WorkPath + "/.hotbed/go/bin"}

// Exec queues a command in a workspace; it starts once the node admits it.
func (e *Engine) Exec(ctx context.Context, wsID, id string, req api.RunCommand) (api.Command, error) {
	if len(req.Args) == 0 {
		return api.Command{}, api.Errorf(api.CodeInvalidArgs, "no command to run")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	w, ok := e.workspaces[wsID]
	if !ok || w.State == api.WorkspaceReleased {
		return api.Command{}, api.Errorf(api.CodeNotFound, "no workspace %s on this node", wsID)
	}
	if _, dup := e.commands[id]; dup {
		return api.Command{}, api.Errorf(api.CodeConflict, "command %s exists", id)
	}
	kind := req.Kind
	if kind == "" {
		kind = inferKind(req.Args)
	}
	c := &command{
		Command: api.Command{
			ID: id, WorkspaceID: wsID, NodeID: e.cfg.NodeID,
			Argv: shellJoin(req.Args), Args: req.Args, Kind: kind,
			State: api.CommandQueued, QueuedAt: e.stamp(),
			Limits: withDefaults(req.Limits), Attached: req.Attached,
			Timeline: []api.CommandEvent{{At: e.stamp(), What: "queued", Detail: "by " + w.Agent}},
		},
		RepoID: w.RepoID, Agent: w.Agent, Volume: w.Volume,
		Dir: WorkPath + "/" + w.RepoID, Env: req.Env, lastFollow: e.now(),
	}
	if req.Port > 0 {
		u := fmt.Sprintf("http://%s:%d", e.cfg.Host, req.Port)
		c.URL = &u
	}
	if isBuild(kind) {
		if _, units, ok := e.coldBuild(w.RepoID); ok {
			c.Rebuilt = &api.Rebuilt{Of: units}
		}
	}
	w.LastActiveAt = e.stamp()
	w.State = api.WorkspaceActive
	e.saveWorkspace(w)
	e.commands[id] = c
	e.saveCommand(c)
	e.changed()
	return c.Command, nil
}

func withDefaults(l api.CommandLimits) api.CommandLimits {
	if l.CPUWeight == 0 {
		l.CPUWeight = 100
	}
	return l
}

func isBuild(kind string) bool { return kind == "build" || kind == "test" || kind == "check" }

// inferKind sorts a command by what it does, for history and the cockpit.
func inferKind(args []string) string {
	s := " " + strings.Join(args, " ") + " "
	switch {
	case strings.Contains(s, " test") || strings.Contains(s, "nextest") || strings.Contains(s, " vitest"):
		return "test"
	case strings.Contains(s, " clippy") || strings.Contains(s, " check") || strings.Contains(s, " lint") || strings.Contains(s, " vet"):
		return "check"
	case strings.Contains(s, " build"):
		return "build"
	case strings.Contains(s, " dev ") || strings.Contains(s, " serve") || strings.Contains(s, " run "):
		return "server"
	default:
		return "shell"
	}
}

// Stop ends a command: a queued one never starts, a running or parked one is
// stopped.
func (e *Engine) Stop(ctx context.Context, id, why string) error {
	e.mu.Lock()
	c, ok := e.commands[id]
	if !ok {
		e.mu.Unlock()
		return api.Errorf(api.CodeNotFound, "no command %s on this node", id)
	}
	state := c.State
	e.mu.Unlock()
	if !state.Live() {
		return nil
	}
	if state != api.CommandQueued {
		if state == api.CommandParked {
			// A frozen unit cannot be stopped until it is thawed.
			if err := e.cfg.Units.Thaw(ctx, c.unitName()); err != nil {
				slog.Warn("thaw before stop", "command", id, "err", err)
			}
		}
		if err := e.cfg.Units.Stop(ctx, c.unitName()); err != nil {
			return err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.end(c, api.CommandCancelled, nil, why)
	return nil
}

// step moves every command along: reads running units, admits queued
// commands, parks or thaws for memory, stops orphaned attached commands.
func (e *Engine) step(ctx context.Context) {
	e.mu.Lock()
	var live []*command
	for _, c := range e.commands {
		if c.State == api.CommandRunning || c.State == api.CommandParked {
			live = append(live, c)
		}
	}
	e.mu.Unlock()
	for _, c := range live {
		st, err := e.cfg.Units.Status(ctx, c.unitName())
		if err != nil {
			slog.Warn("read command", "command", c.ID, "err", err)
			continue
		}
		e.observe(ctx, c, st)
	}
	e.admit(ctx)
	e.park(ctx)
	e.reap(ctx)
	e.forget()
}

func (e *Engine) observe(ctx context.Context, c *command, st unit.State) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !c.State.Live() {
		return
	}
	now := e.now()
	if !st.Loaded {
		e.end(c, api.CommandFailed, nil, "its systemd unit disappeared")
		return
	}
	if st.Cgroup != "" {
		c.Cgroup = st.Cgroup
	}
	if !c.cpuAt.IsZero() && st.CPUNanos >= c.cpuNanos {
		if dt := now.Sub(c.cpuAt).Seconds(); dt > 0 {
			c.CPUCores = float64(st.CPUNanos-c.cpuNanos) / 1e9 / dt
		}
	}
	c.cpuNanos, c.cpuAt = st.CPUNanos, now
	if st.Memory > 0 {
		c.MemoryBytes = st.Memory
		c.Peak = max(c.Peak, st.Memory)
	}
	if c.Cgroup != "" {
		if swap, err := e.cfg.Units.Swap(c.Cgroup); err == nil {
			c.SwapBytes = swap
		}
	}
	if st.Exited {
		// Stop removes the unit; the status is already read.
		if err := e.cfg.Units.Stop(ctx, c.unitName()); err != nil {
			slog.Warn("remove unit", "command", c.ID, "err", err)
		}
		code := st.ExitCode
		if st.Signal != 0 {
			e.end(c, api.CommandFailed, nil, fmt.Sprintf("killed by signal %d", st.Signal))
			return
		}
		state := api.CommandSucceeded
		if code != 0 {
			state = api.CommandFailed
		}
		e.end(c, state, &code, "")
	}
}

// admit starts queued commands, oldest first, while they fit.
func (e *Engine) admit(ctx context.Context) {
	e.mu.Lock()
	var queued []*command
	for _, c := range e.commands {
		if c.State == api.CommandQueued {
			queued = append(queued, c)
		}
	}
	slices.SortFunc(queued, func(a, b *command) int { return strings.Compare(a.QueuedAt, b.QueuedAt) })
	e.mu.Unlock()
	for _, c := range queued {
		e.mu.Lock()
		reason := e.admissionBlocker(c)
		if reason != "" {
			c.Reason = &reason
			e.mu.Unlock()
			continue
		}
		c.Reason = nil
		spec := e.spec(c)
		e.mu.Unlock()
		err := e.cfg.Units.Start(ctx, spec)
		e.mu.Lock()
		if err != nil {
			slog.Error("start command", "command", c.ID, "err", err)
			e.end(c, api.CommandFailed, nil, "could not start: "+err.Error())
			e.mu.Unlock()
			continue
		}
		c.State = api.CommandRunning
		s := e.stamp()
		c.StartedAt = &s
		c.Timeline = append(c.Timeline, api.CommandEvent{At: s, What: "admitted", Detail: fmt.Sprintf("weight %d · low %s · high %s", c.Limits.CPUWeight, gib(c.Limits.MemoryLowBytes), gib(c.Limits.MemoryHighBytes))})
		e.saveCommand(c)
		if c.Kind != "freshen" {
			e.event("admit", fmt.Sprintf("admit %s · %s", c.Argv, c.WorkspaceID), &api.EventRef{Type: "command", ID: c.ID})
		}
		e.mu.Unlock()
	}
}

// admissionBlocker says why c cannot start now, or "" if it can. Call with
// e.mu held.
func (e *Engine) admissionBlocker(c *command) string {
	var reserved int64
	for _, o := range e.commands {
		if o.State == api.CommandRunning || o.State == api.CommandParked {
			reserved += o.Limits.MemoryLowBytes
		}
	}
	room := int64(float64(e.resources.MemoryBytes)*admitMemory) - reserved
	switch {
	case c.Limits.MemoryLowBytes > room:
		return fmt.Sprintf("waiting for memory: needs %s guaranteed, %s left", gib(c.Limits.MemoryLowBytes), gib(max(0, room)))
	case e.usage.MemoryPressure >= admitPressure:
		return fmt.Sprintf("waiting: memory pressure %.0f%%", e.usage.MemoryPressure)
	case e.diskBytes > 0 && float64(e.disk.FreeBytes) < float64(e.diskBytes)*minFreeDisk:
		return fmt.Sprintf("waiting for disk: %s free", gib(e.disk.FreeBytes))
	}
	return ""
}

// spec is the unit for c. Call with e.mu held.
func (e *Engine) spec(c *command) unit.Spec {
	env := append(cacheEnv(), c.Env...)
	env = append(env, "HOTBED_WORKSPACE="+c.WorkspaceID, "HOTBED_COMMAND="+c.ID)
	return unit.Spec{
		Name:       c.unitName(),
		Argv:       c.Args,
		Dir:        c.Dir,
		Binds:      []unit.Bind{{Source: e.cfg.Store.Path(c.Volume), Target: WorkPath}},
		User:       e.cfg.User,
		Env:        env,
		PathPrefix: cachePath,
		CPUWeight:  c.Limits.CPUWeight,
		MemoryLow:  c.Limits.MemoryLowBytes,
		MemoryHigh: c.Limits.MemoryHighBytes,
		Log:        e.logPath(c.ID),
		Properties: e.cfg.UnitProperties,
	}
}

// park freezes the newest running command when memory runs short, and
// thaws the oldest parked one when there is room for it again. Nothing is
// killed for memory.
func (e *Engine) park(ctx context.Context) {
	mem, err := e.cfg.System.Meminfo()
	if err != nil {
		slog.Warn("read memory", "err", err)
		return
	}
	e.mu.Lock()
	var running, parked []*command
	for _, c := range e.commands {
		switch {
		case c.State == api.CommandRunning && c.Kind != "server":
			running = append(running, c)
		case c.State == api.CommandParked:
			parked = append(parked, c)
		}
	}
	e.mu.Unlock()
	total := float64(mem.Total)
	if float64(mem.Used()) > total*parkAt && len(running) > 0 {
		slices.SortFunc(running, func(a, b *command) int { return strings.Compare(*b.StartedAt, *a.StartedAt) })
		victim := running[0]
		if err := e.cfg.Units.Freeze(ctx, victim.unitName()); err != nil {
			slog.Error("park", "command", victim.ID, "err", err)
			return
		}
		if victim.Cgroup != "" {
			// Push the frozen command's memory to swap so others can use it.
			if err := e.cfg.Units.Reclaim(victim.Cgroup, victim.MemoryBytes); err != nil {
				slog.Warn("reclaim parked command", "command", victim.ID, "err", err)
			}
		}
		e.mu.Lock()
		victim.State = api.CommandParked
		victim.Parks++
		victim.ParkedAt = e.now()
		victim.CPUCores = 0
		reason := fmt.Sprintf("frozen at %.0f%% memory in use; its memory goes to swap until there is room", 100*float64(mem.Used())/total)
		victim.Reason = &reason
		victim.Timeline = append(victim.Timeline, api.CommandEvent{At: e.stamp(), What: "parked", Detail: reason})
		e.saveCommand(victim)
		e.event("park", fmt.Sprintf("park %s · memory at %.0f%%", victim.Argv, 100*float64(mem.Used())/total), &api.EventRef{Type: "command", ID: victim.ID})
		e.mu.Unlock()
		return
	}
	if len(parked) == 0 {
		return
	}
	slices.SortFunc(parked, func(a, b *command) int { return a.ParkedAt.Compare(b.ParkedAt) })
	oldest := parked[0]
	if float64(mem.Used()+oldest.SwapBytes) > total*thawBelow {
		return
	}
	if err := e.cfg.Units.Thaw(ctx, oldest.unitName()); err != nil {
		slog.Error("thaw", "command", oldest.ID, "err", err)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	slept := e.now().Sub(oldest.ParkedAt).Round(time.Second)
	oldest.State = api.CommandRunning
	oldest.Reason = nil
	oldest.Timeline = append(oldest.Timeline, api.CommandEvent{At: e.stamp(), What: "thawed", Detail: "after " + slept.String()})
	e.saveCommand(oldest)
	e.event("thaw", fmt.Sprintf("thaw %s after %s", oldest.Argv, slept), &api.EventRef{Type: "command", ID: oldest.ID})
}

// reap stops attached commands whose client went away: no one has followed
// their output for orphanAfter.
func (e *Engine) reap(ctx context.Context) {
	e.mu.Lock()
	var orphans []string
	for _, c := range e.commands {
		if c.Attached && c.State.Live() && c.followers == 0 && e.now().Sub(c.lastFollow) > orphanAfter {
			orphans = append(orphans, c.ID)
		}
	}
	e.mu.Unlock()
	for _, id := range orphans {
		if err := e.Stop(ctx, id, "its client went away"); err != nil {
			slog.Warn("stop orphaned command", "command", id, "err", err)
		}
	}
}

// forget drops commands that ended a while ago; history keeps them.
func (e *Engine) forget() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, c := range e.commands {
		if c.State.Live() || c.EndedAt == nil {
			continue
		}
		ended, err := time.Parse(time.RFC3339Nano, *c.EndedAt)
		if err == nil && e.now().Sub(ended) > keepEnded {
			delete(e.commands, id)
			if err := e.db.delete("command", id); err != nil {
				slog.Error("forget command", "command", id, "err", err)
			}
		}
	}
}

// end records how c ended and tells the controller. Call with e.mu held.
func (e *Engine) end(c *command, state api.CommandState, code *int, why string) {
	if !c.State.Live() {
		return
	}
	now := e.now()
	s := e.stamp()
	c.State = state
	c.EndedAt = &s
	c.ExitCode = code
	c.CPUCores = 0
	if why != "" {
		c.Reason = &why
	} else {
		c.Reason = nil
	}
	what, detail := "exited", "exit "+codeText(code)
	if state == api.CommandCancelled {
		what, detail = "cancelled", why
	}
	c.Timeline = append(c.Timeline, api.CommandEvent{At: s, What: what, Detail: detail})

	queued, _ := time.Parse(time.RFC3339Nano, c.QueuedAt)
	started := queued
	if c.StartedAt != nil {
		started, _ = time.Parse(time.RFC3339Nano, *c.StartedAt)
	}
	seconds := now.Sub(started).Seconds()
	units, failure := scanLog(e.logPath(c.ID), state, code, c.Kind)
	if c.Rebuilt != nil {
		c.Rebuilt.Units = units
	}
	if cold, _, ok := e.coldBuild(c.RepoID); ok && isBuild(c.Kind) {
		// From nothing, this command would have had to build everything
		// first: the base's cold freshen, then itself.
		total := cold + seconds
		c.ColdSeconds = &total
	}
	e.saveCommand(c)

	if c.Kind == "freshen" {
		e.freshened(c, state == api.CommandSucceeded, seconds, units)
		return
	}
	f := api.FinishedCommand{
		ID: c.ID, RepoID: c.RepoID, NodeID: c.NodeID, WorkspaceID: c.WorkspaceID, Agent: c.Agent,
		Argv: c.Argv, Kind: c.Kind, State: state, ExitCode: code, Failure: failure,
		EndedAt: now.UnixMilli(), QueuedSeconds: started.Sub(queued).Seconds(), Seconds: seconds,
		ColdSeconds: c.ColdSeconds, PeakMemoryBytes: c.Peak, Parks: c.Parks,
	}
	if c.Rebuilt != nil {
		f.Rebuilt, f.Of = &c.Rebuilt.Units, &c.Rebuilt.Of
	}
	if _, err := e.db.enqueue("finished", f); err != nil {
		slog.Error("queue finished command", "command", c.ID, "err", err)
	}
	if w, ok := e.workspaces[c.WorkspaceID]; ok {
		w.LastActiveAt = s
		if !e.workspaceBusy(w.ID) {
			w.State = api.WorkspaceIdle
		}
		e.saveWorkspace(w)
	}
	ref := &api.EventRef{Type: "command", ID: c.ID}
	switch state {
	case api.CommandSucceeded:
		text := fmt.Sprintf("ok %s · %s · %s", c.Argv, c.RepoID, dur(seconds))
		if c.ColdSeconds != nil {
			text += fmt.Sprintf(" (cold %s, %.0f×)", dur(*c.ColdSeconds), *c.ColdSeconds/max(seconds, 0.1))
		}
		e.event("finish", text, ref)
	case api.CommandFailed:
		e.event("fail", fmt.Sprintf("fail %s · %s · %s", c.Argv, c.RepoID, orDefault(why, "exit "+codeText(code))), ref)
	default:
		e.event("finish", fmt.Sprintf("cancelled %s · %s", c.Argv, why), ref)
	}
}

func (e *Engine) workspaceBusy(id string) bool {
	for _, c := range e.commands {
		if c.WorkspaceID == id && c.State.Live() {
			return true
		}
	}
	return false
}

// coldBuild is the repo's most recent from-nothing freshen on this node:
// how long it took and how many units it compiled. Call with e.mu held.
func (e *Engine) coldBuild(repo string) (seconds float64, units int, ok bool) {
	var best *api.Base
	for _, b := range e.bases {
		if b.RepoID == repo && b.Cold && b.State != api.BaseBuilding && b.State != api.BaseFailed {
			if best == nil || b.Generation > best.Generation {
				best = b
			}
		}
	}
	if best == nil {
		return 0, 0, false
	}
	return best.BuildSeconds, best.Units, true
}

func (e *Engine) logPath(id string) string { return filepath.Join(e.cfg.Dir, "logs", id+".log") }

var (
	compiling   = regexp.MustCompile(`^\s+Compiling \S+ v`)
	rustError   = regexp.MustCompile(`^error(\[E\d+\])?: `)
	testFailure = regexp.MustCompile(`(^test result: FAILED|^\s+FAIL \[|^--- FAIL:|^FAIL\s|Tests:\s+\d+ failed)`)
)

// scanLog counts the units cargo compiled and sorts a failure into a kind
// people recognise, from the command's output.
func scanLog(path string, state api.CommandState, code *int, kind string) (units int, failure *string) {
	f, err := os.Open(path)
	if err != nil {
		return 0, nil
	}
	defer f.Close()
	var compileErr, testFail bool
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case compiling.MatchString(line):
			units++
		case rustError.MatchString(line):
			compileErr = true
		case testFailure.MatchString(line):
			testFail = true
		}
	}
	if state != api.CommandFailed {
		return units, nil
	}
	k := "exit"
	switch {
	case code == nil:
		k = "signal"
	case testFail:
		k = "test"
	case compileErr && kind == "check":
		k = "lint"
	case compileErr:
		k = "compile"
	}
	return units, &k
}

// shellJoin quotes args the way a shell would need them, for display.
func shellJoin(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if a != "" && strings.IndexFunc(a, func(r rune) bool {
			return !(r == '-' || r == '_' || r == '.' || r == '/' || r == '=' || r == ':' || r == ',' || r == '@' || r == '+' ||
				r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
		}) < 0 {
			out[i] = a
		} else {
			out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(out, " ")
}

func codeText(code *int) string {
	if code == nil {
		return "signal"
	}
	return strconv.Itoa(*code)
}

func gib(b int64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }

func dur(s float64) string {
	if s < 60 {
		return fmt.Sprintf("%.1f s", s)
	}
	return time.Duration(s * float64(time.Second)).Round(time.Second).String()
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
