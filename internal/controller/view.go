package controller

import (
	"context"
	"log/slog"
	"slices"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// nodeView is a node, its view, and its live usage laid over the view.
type nodeView struct {
	nodeRow
	online bool
	*view
}

// views is every node with its view, each entity carrying where clients
// reach its worker and, while the node is online, its live usage from the
// node's last heartbeat.
func (s *Server) views(ctx context.Context) ([]nodeView, error) {
	rows, err := s.cfg.Store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	all, err := s.cfg.Store.Views(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]nodeView, 0, len(rows))
	for _, n := range rows {
		v := all[n.node.Id]
		if v == nil {
			v = &view{}
		}
		nv := nodeView{nodeRow: n, online: s.withOnline(n.node).Online, view: v}
		addr := s.addrOf(n)
		for _, w := range v.workspaces {
			w.NodeAddr = addr
		}
		for _, c := range v.commands {
			c.NodeAddr = addr
		}
		if nv.online && n.hb != nil {
			overlay(v, n.hb)
		}
		out = append(out, nv)
	}
	return out, nil
}

// overlay lays a heartbeat's live usage over a view.
func overlay(v *view, h *pb.HeartbeatRequest) {
	cmds := map[string]*pb.CommandUsage{}
	for _, u := range h.Commands {
		cmds[u.Id] = u
	}
	for _, c := range v.commands {
		if u := cmds[c.Id]; u != nil && live(c.State) {
			c.CpuCores, c.MemoryBytes, c.SwapBytes = u.CpuCores, u.MemoryBytes, u.SwapBytes
			if c.State == pb.CommandState_COMMAND_STATE_QUEUED && u.Reason != "" {
				c.Reason = u.Reason
			}
		}
	}
	bases := map[string]*pb.BaseUsage{}
	for _, u := range h.Bases {
		bases[u.Id] = u
	}
	for _, b := range v.bases {
		if u := bases[b.Id]; u != nil {
			b.SizeBytes = u.SizeBytes
			if b.State == pb.BaseState_BASE_STATE_BUILDING {
				b.Progress = u.Progress
			}
		}
	}
	wss := map[string]*pb.WorkspaceUsage{}
	for _, u := range h.Workspaces {
		wss[u.Id] = u
	}
	for _, w := range v.workspaces {
		if u := wss[w.Id]; u != nil {
			w.ExclusiveBytes, w.SharedBytes = u.ExclusiveBytes, u.SharedBytes
		}
	}
}

// addrOf is where clients reach a node's worker; empty for a worker from
// before the event stream, which admits only the controller.
func (s *Server) addrOf(n nodeRow) string {
	if !n.pushes {
		return ""
	}
	target := n.addr
	if target == "" {
		target = n.node.Hostname
	}
	return s.cfg.WorkerAddr(target)
}

// nodeAddr is where clients reach the worker with node ID id.
func (s *Server) nodeAddr(ctx context.Context, id string) string {
	n, err := s.cfg.Store.node(ctx, id)
	if err != nil {
		slog.Warn("look up a node's address", "node", id, "err", err)
		return ""
	}
	return s.addrOf(n)
}

// viewWorkspace is a workspace as its node's view has it.
func (s *Server) viewWorkspace(ctx context.Context, id string) (*pb.Workspace, error) {
	nvs, err := s.views(ctx)
	if err != nil {
		return nil, err
	}
	for _, nv := range nvs {
		if i := slices.IndexFunc(nv.workspaces, func(w *pb.Workspace) bool { return w.Id == id }); i >= 0 {
			return nv.workspaces[i], nil
		}
	}
	return nil, api.Errorf(api.CodeNotFound, "no workspace %s", id)
}

// viewCommand is a command as its node's view has it.
func (s *Server) viewCommand(ctx context.Context, id string) (*pb.Command, error) {
	nvs, err := s.views(ctx)
	if err != nil {
		return nil, err
	}
	for _, nv := range nvs {
		if i := slices.IndexFunc(nv.commands, func(c *pb.Command) bool { return c.Id == id }); i >= 0 {
			return nv.commands[i], nil
		}
	}
	return nil, api.Errorf(api.CodeNotFound, "no command %s", id)
}

// unreachable says whether err is a worker out of reach, which a view can
// stand in for.
func unreachable(err error) bool { return api.FromConnect(err).Code == api.CodeWorkerUnreachable }
