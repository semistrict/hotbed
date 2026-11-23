package controller

import (
	"cmp"
	"context"
	"log/slog"
	"slices"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// admissionKinds are the events that say a node held work, made it wait, or
// started it again.
var admissionKinds = []string{"hold", "unhold", "wait"}

// node finds a node by host name or ID.
func (s *Server) node(ctx context.Context, name string) (*pb.Node, error) {
	rows, err := s.cfg.Store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.node.Hostname == name || r.node.Id == name {
			return s.withOnline(r.node), nil
		}
	}
	return nil, api.Errorf(api.CodeNotFound, "no node is called %s", name).WithSuggestion("hotbed nodes lists the nodes")
}

// events is the newest events, newest first, of node (ID; "" for every
// node) and of kinds (none for every kind), at most limit, each with its
// node's host name.
func (s *Server) events(ctx context.Context, node string, kinds []string, limit int) ([]*pb.ClusterEvent, error) {
	all, err := s.cfg.Store.Events(ctx, keepEvents)
	if err != nil {
		return nil, err
	}
	rows, err := s.cfg.Store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	hosts := map[string]string{}
	for _, r := range rows {
		hosts[r.node.Id] = r.node.Hostname
	}
	var out []*pb.ClusterEvent
	for _, e := range all {
		if len(out) == limit {
			break
		}
		if (node != "" && e.NodeId != node) || (len(kinds) > 0 && !slices.Contains(kinds, e.Kind)) {
			continue
		}
		e.Hostname = hosts[e.NodeId]
		out = append(out, e)
	}
	return out, nil
}

// placements is the newest placement decisions of the last day that
// weighed node (ID; "" for any) for session ("" for any), at most limit.
func (s *Server) placements(ctx context.Context, node, session string, limit int) ([]*pb.PlacementDecision, error) {
	all, err := s.cfg.Store.Placements(ctx, s.now().Add(-keepPlacements))
	if err != nil {
		return nil, err
	}
	var out []*pb.PlacementDecision
	for _, d := range all {
		if len(out) == limit {
			break
		}
		weighed := node == "" || slices.ContainsFunc(d.Candidates, func(c *pb.PlacementCandidate) bool { return c.NodeId == node })
		if weighed && (session == "" || d.SessionId == session) {
			out = append(out, d)
		}
	}
	return out, nil
}

// RetireNode marks a node gone for good: it leaves the node list and
// placement, and its history stays. Online, it is refused: a worker still
// running would carry on unseen.
func (s *Server) RetireNode(ctx context.Context, req *pb.RetireNodeRequest) (*pb.Node, error) {
	n, err := s.node(ctx, req.Node)
	if err != nil {
		return nil, err
	}
	if n.Online {
		return nil, api.Errorf(api.CodeConflict, "%s is online: only a node gone for good is retired", n.Hostname).
			WithSuggestion("stop its worker first: hotbed worker stop (or destroy) on its machine")
	}
	if err := s.cfg.Store.RetireNode(ctx, n.Id, s.now()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	delete(s.workers, n.Hostname)
	s.mu.Unlock()
	slog.Info("node retired", "node", n.Hostname, "id", n.Id)
	return n, nil
}

func (s *Server) GetNode(ctx context.Context, req *pb.GetNodeRequest) (*pb.NodeDetail, error) {
	n, err := s.node(ctx, req.Node)
	if err != nil {
		return nil, err
	}
	if !n.Online {
		n.Admission = nil // what it said last no longer holds
	}
	limit := int(cmp.Or(req.Limit, 30))
	out := &pb.NodeDetail{Node: n}
	if out.Events, err = s.events(ctx, n.Id, admissionKinds, limit); err != nil {
		return nil, err
	}
	if out.Placements, err = s.placements(ctx, n.Id, "", limit); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Server) ListPlacements(ctx context.Context, req *pb.ListPlacementsRequest) (*pb.ListPlacementsResponse, error) {
	var node string
	if req.Node != "" {
		n, err := s.node(ctx, req.Node)
		if err != nil {
			return nil, err
		}
		node = n.Id
	}
	ds, err := s.placements(ctx, node, req.Session, int(cmp.Or(req.Limit, 50)))
	if err != nil {
		return nil, err
	}
	return &pb.ListPlacementsResponse{Placements: ds}, nil
}

func (s *Server) ListEvents(ctx context.Context, req *pb.ListEventsRequest) (*pb.ListEventsResponse, error) {
	var node string
	if req.Node != "" {
		n, err := s.node(ctx, req.Node)
		if err != nil {
			return nil, err
		}
		node = n.Id
	}
	evs, err := s.events(ctx, node, req.Kinds, int(cmp.Or(req.Limit, 100)))
	if err != nil {
		return nil, err
	}
	return &pb.ListEventsResponse{Events: evs}, nil
}
