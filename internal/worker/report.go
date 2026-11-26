package worker

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"syscall"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/sysinfo"
	"github.com/semistrict/hotbed/internal/unit"
)

// Heartbeat is the worker's live usage for the controller: lossy, the
// latest wins. It names the sessions clients used here since the last one,
// which keeps them alive.
func (e *Engine) Heartbeat(clusterID string) *pb.HeartbeatRequest {
	var fuseRuntimes []string
	if e.cfg.CladefsFUSE != nil {
		fuseRuntimes = e.cfg.CladefsFUSE.RuntimesForHeartbeat()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	h := &pb.HeartbeatRequest{
		ClusterId: clusterID, Resources: clone(e.resources), Usage: clone(e.usage),
		DiskBytes: e.diskBytes, Disk: clone(e.disk), Storage: e.cfg.Store.Kind(), OomKills: e.oomKills,
	}
	if e.cfg.VM != nil {
		h.CladefsPeerAddr, h.CladefsIncarnation, h.HeldBases, h.RetiredBases = e.cfg.VM.HeartbeatMeta()
	} else if e.cfg.CladefsFUSE != nil {
		h.CladefsPeerAddr, h.CladefsIncarnation, h.HeldBases, h.RetiredBases = e.cfg.CladefsFUSE.HeartbeatMeta()
		if h.Resources != nil {
			h.Resources.Runtimes = fuseRuntimes
		}
	}
	if e.objectStore != nil {
		h.ObjectStore = clone(e.objectStore)
	}
	if e.cfg.Profiler != nil {
		h.Profiler = e.cfg.Profiler.Status()
	}
	if e.cfg.DaemonLogs != nil {
		host := cmp.Or(e.cfg.Host, e.cfg.NodeID)
		h.DaemonLogs = e.cfg.DaemonLogs.Summary(host, "worker", host)
	}
	h.Admission = e.admission()
	h.ReservedMemoryBytes = e.reserved()
	for _, c := range e.commands {
		cmd := c.cmd()
		if api.Live(cmd.State) {
			u := &pb.CommandUsage{Id: cmd.Id, CpuCores: cmd.CpuCores, MemoryBytes: cmd.MemoryBytes, SwapBytes: cmd.SwapBytes, DroppedOutputBytes: cmd.DroppedOutputBytes}
			if cmd.State == queued {
				u.Reason = cmd.Reason
			}
			h.Commands = append(h.Commands, u)
		}
		if c.followers > 0 && cmd.SessionId != "" {
			e.active[cmd.SessionId] = true
		}
	}
	for _, sh := range e.shells {
		if sh.sessionID != "" {
			e.active[sh.sessionID] = true
		}
	}
	for _, b := range e.bases {
		u := &pb.BaseUsage{Id: b.Id, SizeBytes: b.SizeBytes, Progress: b.Progress}
		if b.State == building {
			u.Progress = e.baseProgress(b)
		}
		h.Bases = append(h.Bases, u)
	}
	for _, w := range e.workspaces {
		ws := w.ws()
		h.Workspaces = append(h.Workspaces, &pb.WorkspaceUsage{Id: ws.Id, ExclusiveBytes: ws.ExclusiveBytes, SharedBytes: ws.SharedBytes})
	}
	for s := range e.active {
		h.ActiveSessions = append(h.ActiveSessions, s)
	}
	sort.Slice(h.Commands, func(i, j int) bool { return h.Commands[i].Id < h.Commands[j].Id })
	sort.Slice(h.Bases, func(i, j int) bool { return h.Bases[i].Id < h.Bases[j].Id })
	sort.Slice(h.Workspaces, func(i, j int) bool { return h.Workspaces[i].Id < h.Workspaces[j].Id })
	sort.Strings(h.ActiveSessions)
	return h
}

// delivered forgets the sessions, held bases and retired base cells a
// heartbeat told the controller about; those of a heartbeat that failed go
// with the next one.
func (e *Engine) delivered(h *pb.HeartbeatRequest) {
	switch {
	case e.cfg.VM != nil:
		e.cfg.VM.held.delivered(h.HeldBases, h.RetiredBases)
	case e.cfg.CladefsFUSE != nil:
		e.cfg.CladefsFUSE.held.delivered(h.HeldBases, h.RetiredBases)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range h.ActiveSessions {
		delete(e.active, s)
	}
}

// usedWorkspace and usedCommand note that a client used a session's
// workspace or command on this worker, for the next heartbeat.
func (e *Engine) usedWorkspace(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if w, ok := e.workspaces[id]; ok && w.ws().SessionId != "" {
		e.active[w.ws().SessionId] = true
	}
}

func (e *Engine) usedCommand(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c, ok := e.commands[id]; ok && c.cmd().SessionId != "" {
		e.active[c.cmd().SessionId] = true
	}
}

// Command returns a command this node has, live or recently ended.
func (e *Engine) Command(id string) (*pb.Command, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.commands[id]
	if !ok {
		return nil, false
	}
	return clone(c.cmd()), true
}

// Commands lists the commands this node has, live or recently ended, base
// builds included (a caller asking for one session skips them by SessionId).
func (e *Engine) Commands() []*pb.Command {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []*pb.Command{}
	for _, c := range e.commands {
		out = append(out, clone(c.cmd()))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].QueuedAt < out[j].QueuedAt })
	return out
}

// Follow marks a client as reading a command's output, so an attached
// command does not count as orphaned, and returns the log's path. Call the
// returned func when the client stops.
func (e *Engine) Follow(id string) (string, func(), error) {
	path, err := e.logPath(id)
	if err != nil {
		return "", nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if c, ok := e.commands[id]; ok {
		c.followers++
		c.lastFollow = e.now()
		return path, func() {
			e.mu.Lock()
			defer e.mu.Unlock()
			c.followers--
			c.lastFollow = e.now()
		}, nil
	}
	if _, err := os.Stat(path); err != nil {
		return "", nil, noLog(id)
	}
	return path, func() {}, nil
}

// Going reports whether a command has more output to come: it has yet to
// end and is not paused. It returns a channel closed at the engine's next
// change.
func (e *Engine) Going(id string) (bool, <-chan struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.commands[id]
	return ok && api.Live(c.cmd().State) && c.cmd().State != paused, e.updated
}

// LastInterventionSeq returns the sequence number of the latest intervention
// recorded on command id, or 0 if none exists.
func (e *Engine) LastInterventionSeq(id string) int32 {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.commands[id]
	if !ok {
		return 0
	}
	ivs := c.cmd().Interventions
	if len(ivs) == 0 {
		return 0
	}
	return ivs[len(ivs)-1].GetSeq()
}

// FollowState reports whether command id has more output to come, along with
// its state and the manual intervention (if any) at the moment following ends.
// If a manual PAUSE intervention occurred with seq > afterSeq even if the
// command was immediately resumed afterward, FollowState still reports PAUSED
// and that PAUSE intervention so a quick resume cannot hide the hand pause.
func (e *Engine) FollowState(id string, afterSeq int32) (going bool, state pb.CommandState, endedBy *pb.CommandIntervention, updated <-chan struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.commands[id]
	if !ok {
		return false, pb.CommandState_COMMAND_STATE_UNSPECIFIED, nil, e.updated
	}
	cmd := c.cmd()
	if !api.Live(cmd.State) {
		var by *pb.CommandIntervention
		if iv := api.ByHand(cmd); iv != nil {
			by = clone(iv)
		}
		return false, cmd.State, by, e.updated
	}
	if cmd.State == paused {
		var by *pb.CommandIntervention
		if iv := api.ByHand(cmd); iv != nil {
			by = clone(iv)
		}
		return false, paused, by, e.updated
	}
	for i := len(cmd.Interventions) - 1; i >= 0; i-- {
		iv := cmd.Interventions[i]
		if iv.GetSeq() <= afterSeq {
			break
		}
		if iv.GetKind() == pb.InterventionKind_INTERVENTION_KIND_PAUSE {
			return false, paused, clone(iv), e.updated
		}
	}
	return true, cmd.State, nil, e.updated
}

func lookupUser(name string) (*syscall.Credential, string, error) {
	if name == "" {
		home, err := os.UserHomeDir()
		return nil, home, err
	}
	u, err := lookupOSUser(name)
	if err != nil {
		return nil, "", fmt.Errorf("look up user %s: %w", name, err)
	}
	uid, err1 := strconv.ParseUint(u.Uid, 10, 32)
	gid, err2 := strconv.ParseUint(u.Gid, 10, 32)
	if err1 != nil || err2 != nil {
		return nil, "", fmt.Errorf("user %s has uid %q gid %q", name, u.Uid, u.Gid)
	}
	if uid == 0 {
		return nil, "", api.Errorf(api.CodeInvalidArgs, "command user %q resolves to uid 0 (root); commands must run as a non-root user", name).
			WithSuggestion("pass a non-root user with --user")
	}
	if gid == 0 {
		return nil, "", api.Errorf(api.CodeInvalidArgs, "command user %q resolves to gid 0 (root); commands must run as a non-root user", name).
			WithSuggestion("pass a non-root user with --user")
	}
	if uint64(os.Getuid()) == uid {
		return nil, u.HomeDir, nil
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}, u.HomeDir, nil
}

// Systemd runs commands as systemd units.
type Systemd struct{}

func (Systemd) Start(ctx context.Context, s unit.Spec) error { return unit.Start(ctx, s) }
func (Systemd) Status(ctx context.Context, name string) (unit.State, error) {
	return unit.Status(ctx, name)
}
func (Systemd) Freeze(ctx context.Context, name string) error { return unit.Freeze(ctx, name) }
func (Systemd) Thaw(ctx context.Context, name string) error   { return unit.Thaw(ctx, name) }
func (Systemd) Reclaim(cgroup string, n int64) error          { return unit.Reclaim(cgroup, n) }
func (Systemd) Memory(cgroup string) (unit.MemoryUse, error)  { return unit.Memory(cgroup) }
func (Systemd) Stop(ctx context.Context, name string) error   { return unit.Stop(ctx, name) }
func (Systemd) Wait(ctx context.Context, slice, name string) error {
	return unit.Wait(ctx, cmp.Or(slice, unit.DefaultSlice), name)
}
func (Systemd) StartShell(ctx context.Context, s unit.Spec, sio unit.ShellIO) (unit.ShellHandle, error) {
	return unit.StartShell(ctx, s, sio)
}
func (Systemd) Exec(ctx context.Context, name, slice string, c unit.ExecSpec) (*exec.Cmd, error) {
	return unit.Exec(ctx, name, slice, c)
}
func (Systemd) StartSlice(ctx context.Context, slice string) error {
	return unit.StartSlice(ctx, slice)
}
func (Systemd) SetProperties(ctx context.Context, name string, props ...string) error {
	return unit.SetProperties(ctx, name, props...)
}
func (Systemd) Show(ctx context.Context, name string, props ...string) (map[string][]string, error) {
	return unit.Show(ctx, name, props...)
}

// Linux measures the node from /proc.
// Linux measures this machine as commands in a slice get it.
type Linux struct {
	Slice string
	cpu   sysinfo.CPU
}

func (l *Linux) sliceDir() string { return unit.SliceDir(cmp.Or(l.Slice, unit.DefaultSlice)) }

func (l *Linux) Resources() (*pb.Resources, error) { return sysinfo.Resources(l.sliceDir()) }
func (l *Linux) Meminfo() (sysinfo.Mem, error)     { return sysinfo.Memory(l.sliceDir()) }
func (*Linux) Pressure(r string) (float64, error)  { return sysinfo.Pressure(r) }
func (l *Linux) CPU() (sysinfo.CPUShare, error)    { return l.cpu.Sample() }
func (*Linux) OOMKills() (int64, error)            { return sysinfo.OOMKills() }
