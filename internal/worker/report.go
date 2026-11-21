package worker

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"sort"
	"strconv"
	"syscall"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/sysinfo"
	"github.com/semistrict/hotbed/internal/unit"
)

const maxReportItems = 500

// Report is the worker's account of itself for the controller, with the
// finished commands and events it has not had acknowledged.
func (e *Engine) Report(clusterID string) (*pb.ReportRequest, error) {
	items, err := e.db.pending(maxReportItems)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.forgetReleased()
	r := &pb.ReportRequest{
		ClusterId: clusterID, Resources: clone(e.resources), Usage: clone(e.usage),
		DiskBytes: e.diskBytes, Disk: clone(e.disk), Storage: e.cfg.Store.Kind(), OomKills: e.oomKills,
	}
	for _, c := range e.commands {
		if s := c.cmd().State; s == running || s == parked {
			r.ReservedMemoryBytes += c.cmd().Limits.MemoryLowBytes
		}
		if c.cmd().Kind != "freshen" {
			r.Commands = append(r.Commands, clone(c.cmd()))
		}
	}
	for _, b := range e.bases {
		if b.State == building {
			b.Progress = e.baseProgress(b)
		}
		r.Bases = append(r.Bases, clone(b))
	}
	for _, w := range e.workspaces {
		r.Workspaces = append(r.Workspaces, clone(w.ws()))
	}
	sort.Slice(r.Commands, func(i, j int) bool { return r.Commands[i].QueuedAt < r.Commands[j].QueuedAt })
	sort.Slice(r.Bases, func(i, j int) bool { return r.Bases[i].Id < r.Bases[j].Id })
	sort.Slice(r.Workspaces, func(i, j int) bool { return r.Workspaces[i].Id < r.Workspaces[j].Id })
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

// Acknowledged drops what the controller has stored.
func (e *Engine) Acknowledged(seq int64) {
	if seq <= 0 {
		return
	}
	if err := e.db.ack(seq); err != nil {
		slog.Error("drop acknowledged items", "seq", seq, "err", err)
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
