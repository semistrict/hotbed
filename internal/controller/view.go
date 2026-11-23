package controller

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

const liveQueryTimeout = 2 * time.Second

// nodeView is a node, its view, and its live usage laid over the view.
type nodeView struct {
	nodeRow
	online bool
	// wakeFailed says the node, asleep, did not come up when last woken:
	// it takes no runs for now.
	wakeFailed bool
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
	seenIDs := map[string]struct{}{}
	var sessIDs []string
	for _, v := range all {
		for _, w := range v.workspaces {
			if w.SessionId != "" {
				if _, ok := seenIDs[w.SessionId]; !ok {
					seenIDs[w.SessionId] = struct{}{}
					sessIDs = append(sessIDs, w.SessionId)
				}
			}
		}
		for _, c := range v.commands {
			if c.SessionId != "" {
				if _, ok := seenIDs[c.SessionId]; !ok {
					seenIDs[c.SessionId] = struct{}{}
					sessIDs = append(sessIDs, c.SessionId)
				}
			}
		}
	}
	sessProj, err := s.cfg.Store.SessionProjectsFor(ctx, sessIDs)
	if err != nil {
		return nil, err
	}
	out := make([]nodeView, 0, len(rows))
	for _, n := range rows {
		v := all[n.node.Id]
		if v == nil {
			v = &view{}
		}
		nv := nodeView{nodeRow: n, online: s.withOnline(n.node).Online, wakeFailed: s.power.wakeFailed(n.node), view: v}
		addr := s.addrOf(n)
		for _, w := range v.workspaces {
			w.NodeAddr, w.Node = addr, n.node.Hostname
			if p := sessProj[w.SessionId]; p != "" {
				w.Project = p
			}
		}
		for _, c := range v.commands {
			c.NodeAddr = addr
			if p := sessProj[c.SessionId]; p != "" {
				c.Project = p
			}
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
		if u := cmds[c.Id]; u != nil && api.Live(c.State) {
			c.CpuCores, c.MemoryBytes, c.SwapBytes = u.CpuCores, u.MemoryBytes, u.SwapBytes
			c.DroppedOutputBytes = u.DroppedOutputBytes
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

// addrOf is where clients reach a node's worker.
func (s *Server) addrOf(n nodeRow) string {
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

// locateWorkspace fills in where the workspace is: its node's host name,
// and where clients reach its worker.
func (s *Server) locateWorkspace(ctx context.Context, ws *pb.Workspace, nodeID string) {
	n, err := s.cfg.Store.node(ctx, nodeID)
	if err != nil {
		slog.Warn("look up a workspace's node", "workspace", ws.Id, "node", nodeID, "err", err)
		return
	}
	ws.NodeAddr, ws.Node = s.addrOf(n), n.node.Hostname
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
func unreachable(err error) bool { return api.CodeOf(err) == api.CodeWorkerUnreachable }

// liveNodeCommands queries reachable online nodes in parallel for their
// authoritative command lists (optionally filtered to sessionID and restricted
// to nodeIDs when non-nil). Nodes that are offline or fail to answer within
// liveQueryTimeout are omitted so callers fall back to their cached views.
func (s *Server) liveNodeCommands(ctx context.Context, nvs []nodeView, nodeIDs map[string]bool, sessionID string) map[string][]*pb.Command {
	var targets []nodeView
	for _, nv := range nvs {
		if !nv.online {
			continue
		}
		if nodeIDs != nil && !nodeIDs[nv.node.Id] {
			continue
		}
		targets = append(targets, nv)
	}
	if len(targets) == 0 {
		return nil
	}
	var (
		mu   sync.Mutex
		live = make(map[string][]*pb.Command, len(targets))
		wg   sync.WaitGroup
	)
	for _, nv := range targets {
		wg.Add(1)
		go func(nv nodeView) {
			defer wg.Done()
			qctx, cancel := context.WithTimeout(ctx, liveQueryTimeout)
			defer cancel()
			c, _, err := s.worker(qctx, nv.node.Id)
			if err != nil {
				return
			}
			res, err := c.ListCommands(qctx, &pb.ListCommandsRequest{SessionId: sessionID})
			if err != nil {
				return
			}
			addr := s.addrOf(nv.nodeRow)
			for _, cmd := range res.Commands {
				cmd.NodeAddr = addr
			}
			mu.Lock()
			live[nv.node.Id] = res.Commands
			mu.Unlock()
		}(nv)
	}
	wg.Wait()
	seenIDs := map[string]struct{}{}
	var sessIDs []string
	for _, cmds := range live {
		for _, cmd := range cmds {
			if cmd.SessionId != "" {
				if _, ok := seenIDs[cmd.SessionId]; !ok {
					seenIDs[cmd.SessionId] = struct{}{}
					sessIDs = append(sessIDs, cmd.SessionId)
				}
			}
		}
	}
	if sessProj, err := s.cfg.Store.SessionProjectsFor(ctx, sessIDs); err == nil {
		for _, cmds := range live {
			for _, cmd := range cmds {
				if p := sessProj[cmd.SessionId]; p != "" {
					cmd.Project = p
				}
			}
		}
	}
	return live
}

// liveNodeWorkspaces queries reachable online nodes in parallel for their
// authoritative workspace lists (restricted to nodeIDs when non-nil). Nodes
// that are offline or fail to answer within liveQueryTimeout are omitted so
// callers fall back to their cached views.
func (s *Server) liveNodeWorkspaces(ctx context.Context, nvs []nodeView, nodeIDs map[string]bool) map[string][]*pb.Workspace {
	var targets []nodeView
	for _, nv := range nvs {
		if !nv.online {
			continue
		}
		if nodeIDs != nil && !nodeIDs[nv.node.Id] {
			continue
		}
		targets = append(targets, nv)
	}
	if len(targets) == 0 {
		return nil
	}
	var (
		mu   sync.Mutex
		live = make(map[string][]*pb.Workspace, len(targets))
		wg   sync.WaitGroup
	)
	for _, nv := range targets {
		wg.Add(1)
		go func(nv nodeView) {
			defer wg.Done()
			qctx, cancel := context.WithTimeout(ctx, liveQueryTimeout)
			defer cancel()
			c, _, err := s.worker(qctx, nv.node.Id)
			if err != nil {
				return
			}
			res, err := c.ListWorkspaces(qctx, &pb.ListWorkspacesRequest{})
			if err != nil {
				return
			}
			addr := s.addrOf(nv.nodeRow)
			for _, ws := range res.Workspaces {
				ws.NodeAddr, ws.Node = addr, nv.node.Hostname
			}
			mu.Lock()
			live[nv.node.Id] = res.Workspaces
			mu.Unlock()
		}(nv)
	}
	wg.Wait()
	seenIDs := map[string]struct{}{}
	var sessIDs []string
	for _, wss := range live {
		for _, ws := range wss {
			if ws.SessionId != "" {
				if _, ok := seenIDs[ws.SessionId]; !ok {
					seenIDs[ws.SessionId] = struct{}{}
					sessIDs = append(sessIDs, ws.SessionId)
				}
			}
		}
	}
	if sessProj, err := s.cfg.Store.SessionProjectsFor(ctx, sessIDs); err == nil {
		for _, wss := range live {
			for _, ws := range wss {
				if p := sessProj[ws.SessionId]; p != "" {
					ws.Project = p
				}
			}
		}
	}
	return live
}
