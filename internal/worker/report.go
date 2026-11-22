package worker

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/user"
	"sort"
	"strconv"
	"syscall"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/sysinfo"
	"github.com/semistrict/hotbed/internal/unit"
)

// Heartbeat is the worker's live usage for the controller: lossy, the
// latest wins. It names the sessions clients used here since the last one,
// which keeps them alive.
func (e *Engine) Heartbeat(clusterID string) *pb.HeartbeatRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	h := &pb.HeartbeatRequest{
		ClusterId: clusterID, Resources: clone(e.resources), Usage: clone(e.usage),
		DiskBytes: e.diskBytes, Disk: clone(e.disk), Storage: e.cfg.Store.Kind(), OomKills: e.oomKills,
	}
	for _, c := range e.commands {
		cmd := c.cmd()
		if s := cmd.State; s == running || s == parked {
			h.ReservedMemoryBytes += cmd.Limits.MemoryLowBytes
		}
		if live(cmd.State) {
			h.Commands = append(h.Commands, &pb.CommandUsage{Id: cmd.Id, CpuCores: cmd.CpuCores, MemoryBytes: cmd.MemoryBytes, SwapBytes: cmd.SwapBytes})
		}
		if c.followers > 0 && cmd.SessionId != "" {
			e.active[cmd.SessionId] = true
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

// delivered forgets the sessions a heartbeat told the controller about;
// those of a heartbeat that failed go with the next one.
func (e *Engine) delivered(h *pb.HeartbeatRequest) {
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

const maxLegacyItems = 500

// legacyReport is a report as workers before the event stream sent it: the
// whole state and the old outbox's items, which a worker upgraded with
// items still queued delivers once that way.
func (e *Engine) legacyReport(clusterID string, items []outboxItem) (*pb.ReportRequest, error) {
	h := e.Heartbeat(clusterID)
	h.ActiveSessions = nil
	cp, err := e.db.checkpoint()
	if err != nil {
		return nil, err
	}
	r := &pb.ReportRequest{
		ClusterId: clusterID, Resources: h.Resources, Usage: h.Usage, DiskBytes: h.DiskBytes, Disk: h.Disk,
		ReservedMemoryBytes: h.ReservedMemoryBytes, OomKills: h.OomKills, Storage: h.Storage,
		Bases: cp.Bases, Workspaces: cp.Workspaces,
	}
	for _, c := range cp.Commands {
		if c.Kind != "freshen" {
			r.Commands = append(r.Commands, c)
		}
	}
	for _, it := range items {
		switch it.kind {
		case "finished":
			f := &pb.FinishedCommand{}
			if err := decode(it.doc, f); err != nil {
				return nil, fmt.Errorf("outbox %d: %w", it.seq, err)
			}
			r.Finished = append(r.Finished, &pb.SequencedFinished{Seq: it.seq, Item: f})
		case "event":
			ev := &pb.WorkerEvent{}
			if err := decode(it.doc, ev); err != nil {
				return nil, fmt.Errorf("outbox %d: %w", it.seq, err)
			}
			r.Events = append(r.Events, &pb.SequencedEvent{Seq: it.seq, Item: ev})
		case "session":
			ev := &pb.SessionEvent{}
			if err := decode(it.doc, ev); err != nil {
				return nil, fmt.Errorf("outbox %d: %w", it.seq, err)
			}
			r.SessionEvents = append(r.SessionEvents, &pb.SequencedSessionEvent{Seq: it.seq, Item: ev})
		}
	}
	return r, nil
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

// Commands lists the commands this node has, live or recently ended.
func (e *Engine) Commands() []*pb.Command {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []*pb.Command{}
	for _, c := range e.commands {
		if c.cmd().Kind != "freshen" {
			out = append(out, clone(c.cmd()))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].QueuedAt < out[j].QueuedAt })
	return out
}

// Follow marks a client as reading a command's output, so an attached
// command does not count as orphaned, and returns the log's path. Call the
// returned func when the client stops.
func (e *Engine) Follow(id string) (string, func(), error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c, ok := e.commands[id]; ok {
		c.followers++
		c.lastFollow = e.now()
		return e.logPath(id), func() {
			e.mu.Lock()
			defer e.mu.Unlock()
			c.followers--
			c.lastFollow = e.now()
		}, nil
	}
	path := e.logPath(id)
	if _, err := os.Stat(path); err != nil {
		return "", nil, notFound("command", id)
	}
	return path, func() {}, nil
}

// Live reports whether a command has yet to end, and returns a channel
// closed at the engine's next change.
func (e *Engine) Live(id string) (bool, <-chan struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.commands[id]
	return ok && live(c.cmd().State), e.updated
}

func lookupUser(name string) (*syscall.Credential, string, error) {
	if name == "" {
		home, err := os.UserHomeDir()
		return nil, home, err
	}
	u, err := user.Lookup(name)
	if err != nil {
		return nil, "", fmt.Errorf("look up user %s: %w", name, err)
	}
	uid, err1 := strconv.ParseUint(u.Uid, 10, 32)
	gid, err2 := strconv.ParseUint(u.Gid, 10, 32)
	if err1 != nil || err2 != nil {
		return nil, "", fmt.Errorf("user %s has uid %q gid %q", name, u.Uid, u.Gid)
	}
	if uint64(os.Getuid()) == uid {
		return nil, u.HomeDir, nil
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}, u.HomeDir, nil
}

// Systemd runs commands as systemd units in a slice.
type Systemd struct{ Slice string }

func (Systemd) Start(ctx context.Context, s unit.Spec) error { return unit.Start(ctx, s) }
func (Systemd) Status(ctx context.Context, name string) (unit.State, error) {
	return unit.Status(ctx, name)
}
func (Systemd) Freeze(ctx context.Context, name string) error { return unit.Freeze(ctx, name) }
func (Systemd) Thaw(ctx context.Context, name string) error   { return unit.Thaw(ctx, name) }
func (Systemd) Reclaim(cgroup string, n int64) error          { return unit.Reclaim(cgroup, n) }
func (Systemd) Swap(cgroup string) (int64, error)             { return unit.Swap(cgroup) }
func (Systemd) Stop(ctx context.Context, name string) error   { return unit.Stop(ctx, name) }
func (s Systemd) Wait(ctx context.Context, name string) error {
	return unit.Wait(ctx, cmp.Or(s.Slice, unit.DefaultSlice), name)
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
func (l *Linux) CPUBusy() (float64, error)         { return l.cpu.Busy() }
func (*Linux) OOMKills() (int64, error)            { return sysinfo.OOMKills() }
