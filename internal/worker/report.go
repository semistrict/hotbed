package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"sort"
	"strconv"
	"syscall"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/sysinfo"
	"github.com/semistrict/hotbed/internal/unit"
)

const maxReportItems = 500

// Report is the worker's account of itself for the controller, with the
// finished commands and events it has not had acknowledged.
func (e *Engine) Report(clusterID string) (api.Report, error) {
	items, err := e.db.pending(maxReportItems)
	if err != nil {
		return api.Report{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.forgetReleased()
	r := api.Report{
		ClusterID: clusterID, Resources: e.resources, Usage: e.usage,
		DiskBytes: e.diskBytes, Disk: e.disk, Storage: e.cfg.Store.Kind(), OOMKills: e.oomKills,
		Bases: []api.Base{}, Workspaces: []api.Workspace{}, Commands: []api.Command{},
		Finished: []api.Sequenced[api.FinishedCommand]{}, Events: []api.Sequenced[api.WorkerEvent]{},
	}
	for _, c := range e.commands {
		if c.State == api.CommandRunning || c.State == api.CommandParked {
			r.Reserved += c.Limits.MemoryLowBytes
		}
		if c.Kind == "freshen" {
			continue
		}
		r.Commands = append(r.Commands, c.Command)
	}
	for _, b := range e.bases {
		if b.State == api.BaseBuilding {
			b.Progress = e.baseProgress(b)
		}
		r.Bases = append(r.Bases, *b)
	}
	for _, w := range e.workspaces {
		r.Workspaces = append(r.Workspaces, w.Workspace)
	}
	sort.Slice(r.Commands, func(i, j int) bool { return r.Commands[i].QueuedAt < r.Commands[j].QueuedAt })
	sort.Slice(r.Bases, func(i, j int) bool { return r.Bases[i].ID < r.Bases[j].ID })
	sort.Slice(r.Workspaces, func(i, j int) bool { return r.Workspaces[i].ID < r.Workspaces[j].ID })
	for _, it := range items {
		switch it.kind {
		case "finished":
			var f api.FinishedCommand
			if err := json.Unmarshal(it.doc, &f); err != nil {
				return api.Report{}, fmt.Errorf("outbox %d: %w", it.seq, err)
			}
			r.Finished = append(r.Finished, api.Sequenced[api.FinishedCommand]{Seq: it.seq, Item: f})
		case "event":
			var ev api.WorkerEvent
			if err := json.Unmarshal(it.doc, &ev); err != nil {
				return api.Report{}, fmt.Errorf("outbox %d: %w", it.seq, err)
			}
			r.Events = append(r.Events, api.Sequenced[api.WorkerEvent]{Seq: it.seq, Item: ev})
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
func (e *Engine) Command(id string) (api.Command, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.commands[id]
	if !ok {
		return api.Command{}, false
	}
	return c.Command, true
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
		return "", nil, api.Errorf(api.CodeNotFound, "no command %s on this node", id)
	}
	return path, func() {}, nil
}

// Live reports whether a command has yet to end.
func (e *Engine) Live(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.commands[id]
	return ok && c.State.Live()
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

// Systemd runs commands as systemd units.
type Systemd struct{}

func (Systemd) Start(ctx context.Context, s unit.Spec) error { return unit.Start(ctx, s) }
func (Systemd) Status(ctx context.Context, name string) (unit.State, error) {
	return unit.Status(ctx, name)
}
func (Systemd) Freeze(ctx context.Context, name string) error { return unit.Freeze(ctx, name) }
func (Systemd) Thaw(ctx context.Context, name string) error   { return unit.Thaw(ctx, name) }
func (Systemd) Reclaim(cgroup string, n int64) error          { return unit.Reclaim(cgroup, n) }
func (Systemd) Swap(cgroup string) (int64, error)             { return unit.Swap(cgroup) }
func (Systemd) Stop(ctx context.Context, name string) error   { return unit.Stop(ctx, name) }

// Linux measures the node from /proc.
type Linux struct{ cpu sysinfo.CPU }

func (*Linux) Resources() (api.Resources, error)  { return sysinfo.Resources() }
func (*Linux) Meminfo() (sysinfo.Mem, error)      { return sysinfo.Meminfo() }
func (*Linux) Pressure(r string) (float64, error) { return sysinfo.Pressure(r) }
func (l *Linux) CPUBusy() (float64, error)        { return l.cpu.Busy() }
func (*Linux) OOMKills() (int64, error)           { return sysinfo.OOMKills() }
