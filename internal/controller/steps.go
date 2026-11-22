package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
	"strings"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/httpx"
)

var stepName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// PutStep adds a step to a repo or replaces the one of its name. Workers
// see it in the repo the next workspace request carries, and the base they
// freshen for it runs the step.
func (s *Server) PutStep(ctx context.Context, req *pb.PutStepRequest) (*pb.Repo, error) {
	step := req.GetStep()
	if !stepName.MatchString(step.GetName()) || len(step.Name) > 40 {
		return nil, api.Errorf(api.CodeInvalidArgs, "step name %q must be lowercase words joined by dashes", step.GetName())
	}
	if strings.TrimSpace(step.Script) == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "step %s has no script", step.Name)
	}
	s.repoEdits.Lock()
	defer s.repoEdits.Unlock()
	r, err := s.repo(ctx, req.RepoId)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(step.Script))
	step = &pb.Step{
		Name: step.Name, Script: step.Script, EveryPull: step.EveryPull,
		Hash: hex.EncodeToString(sum[:])[:12], UpdatedAt: s.now().UnixMilli(), UpdatedBy: httpx.Peer(ctx).Hostname,
	}
	at := slices.IndexFunc(r.Steps, func(o *pb.Step) bool { return o.Name == step.Name })
	switch {
	case req.Before == step.Name:
		return nil, api.Errorf(api.CodeInvalidArgs, "step %s cannot go before itself", step.Name)
	case req.Before != "":
		if at >= 0 {
			r.Steps = slices.Delete(r.Steps, at, at+1)
		}
		before := slices.IndexFunc(r.Steps, func(o *pb.Step) bool { return o.Name == req.Before })
		if before < 0 {
			return nil, api.Errorf(api.CodeNotFound, "repo %s has no step %s to put %s before", r.Id, req.Before, step.Name)
		}
		r.Steps = slices.Insert(r.Steps, before, step)
	case at >= 0:
		r.Steps[at] = step
	default:
		r.Steps = append(r.Steps, step)
	}
	return r, s.cfg.Store.PutRepo(ctx, r)
}

// DeleteStep removes a step. Bases keep what it did; the next generations
// just stop running it.
func (s *Server) DeleteStep(ctx context.Context, req *pb.DeleteStepRequest) (*pb.Repo, error) {
	s.repoEdits.Lock()
	defer s.repoEdits.Unlock()
	r, err := s.repo(ctx, req.RepoId)
	if err != nil {
		return nil, err
	}
	at := slices.IndexFunc(r.Steps, func(o *pb.Step) bool { return o.Name == req.Name })
	if at < 0 {
		return nil, api.Errorf(api.CodeNotFound, "repo %s has no step %s", r.Id, req.Name).WithSuggestion("hotbed step ls " + r.Id)
	}
	r.Steps = slices.Delete(r.Steps, at, at+1)
	return r, s.cfg.Store.PutRepo(ctx, r)
}

// ListSteps says, for each of a repo's steps, whether each node's newest
// finished base ran the step as it is now.
func (s *Server) ListSteps(ctx context.Context, req *pb.ListStepsRequest) (*pb.ListStepsResponse, error) {
	r, err := s.repo(ctx, req.RepoId)
	if err != nil {
		return nil, err
	}
	nvs, err := s.views(ctx)
	if err != nil {
		return nil, err
	}
	type newest struct {
		node string
		base *pb.Base
	}
	var nodes []newest
	for _, n := range nvs {
		if len(r.Hosts) > 0 && !slices.Contains(r.Hosts, n.node.Hostname) {
			continue
		}
		nb := newest{node: n.node.Hostname}
		for _, b := range n.bases {
			if b.RepoId == r.Id && b.State != pb.BaseState_BASE_STATE_BUILDING && (nb.base == nil || b.Generation > nb.base.Generation) {
				nb.base = b
			}
		}
		nodes = append(nodes, nb)
	}
	out := &pb.ListStepsResponse{}
	for _, step := range r.Steps {
		st := &pb.StepStatus{Step: step}
		for _, n := range nodes {
			on := &pb.StepOnNode{Node: n.node, State: pb.StepState_STEP_STATE_PENDING}
			if n.base != nil {
				on.BaseId = n.base.Id
				for _, a := range n.base.Steps {
					if a.Name != step.Name || a.Hash != step.Hash {
						continue
					}
					switch {
					case a.Ok:
						on.State, on.Seconds = pb.StepState_STEP_STATE_APPLIED, a.Seconds
					case a.Ran:
						on.State = pb.StepState_STEP_STATE_FAILED
					}
				}
			}
			st.Nodes = append(st.Nodes, on)
		}
		out.Steps = append(out.Steps, st)
	}
	return out, nil
}
