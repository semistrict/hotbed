package controller

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/httpx"
)

var stepName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// PutStep adds a step to a project or replaces the one of its name.
// Workers see it in the project the next workspace request carries, and the
// base they freshen for it runs the step.
func (s *Server) PutStep(ctx context.Context, req *pb.PutStepRequest) (*pb.Project, error) {
	step := req.GetStep()
	if !stepName.MatchString(step.GetName()) || len(step.Name) > 40 {
		return nil, api.Errorf(api.CodeInvalidArgs, "step name %q must be lowercase words joined by dashes", step.GetName())
	}
	if strings.TrimSpace(step.Script) == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "step %s has no script", step.Name)
	}
	step = &pb.Step{
		Name: step.Name, Script: step.Script, EveryPull: step.EveryPull,
		Hash: api.StepHash(step.Script), UpdatedAt: s.now().UnixMilli(), UpdatedBy: httpx.Peer(ctx).Hostname,
	}
	return s.cfg.Store.EditProjectSetup(ctx, req.Project, func(p *pb.Project) error {
		if req.IfRevision > 0 && p.SetupRevision != req.IfRevision {
			return api.Errorf(api.CodeConflict, "project %s setup is at revision %d, not %d", p.Name, p.SetupRevision, req.IfRevision).
				WithSuggestion("hotbed step ls -p " + p.Name)
		}
		at := slices.IndexFunc(p.Steps, func(o *pb.Step) bool { return o.Name == step.Name })
		switch {
		case req.Before == step.Name:
			return api.Errorf(api.CodeInvalidArgs, "step %s cannot go before itself", step.Name)
		case req.Before != "":
			if at >= 0 {
				p.Steps = slices.Delete(p.Steps, at, at+1)
			}
			before := slices.IndexFunc(p.Steps, func(o *pb.Step) bool { return o.Name == req.Before })
			if before < 0 {
				return api.Errorf(api.CodeNotFound, "project %s has no step %s to put %s before", p.Name, req.Before, step.Name)
			}
			p.Steps = slices.Insert(p.Steps, before, step)
		case at >= 0:
			p.Steps[at] = step
		default:
			p.Steps = append(p.Steps, step)
		}
		return nil
	})
}

// DeleteStep removes a step. Bases keep what it did; the next generations
// just stop running it.
func (s *Server) DeleteStep(ctx context.Context, req *pb.DeleteStepRequest) (*pb.Project, error) {
	return s.cfg.Store.EditProjectSetup(ctx, req.Project, func(p *pb.Project) error {
		if req.IfRevision > 0 && p.SetupRevision != req.IfRevision {
			return api.Errorf(api.CodeConflict, "project %s setup is at revision %d, not %d", p.Name, p.SetupRevision, req.IfRevision).
				WithSuggestion("hotbed step ls -p " + p.Name)
		}
		at := slices.IndexFunc(p.Steps, func(o *pb.Step) bool { return o.Name == req.Name })
		if at < 0 {
			return api.Errorf(api.CodeNotFound, "project %s has no step %s", p.Name, req.Name).WithSuggestion("hotbed step ls -p " + p.Name)
		}
		p.Steps = slices.Delete(p.Steps, at, at+1)
		return nil
	})
}

// RebuildProject raises the project's scratch epoch, which has each worker
// build its next base from nothing.
func (s *Server) RebuildProject(ctx context.Context, req *pb.RebuildProjectRequest) (*pb.Project, error) {
	return s.cfg.Store.EditProjectSetup(ctx, req.Name, func(p *pb.Project) error {
		p.ScratchEpoch++
		return nil
	})
}

// SetProjectHosts says which nodes keep the project's bases. A node left
// out lets its bases go once no workspace uses them.
func (s *Server) SetProjectHosts(ctx context.Context, req *pb.SetProjectHostsRequest) (*pb.Project, error) {
	hosts := slices.Clone(req.Hosts)
	slices.Sort(hosts)
	hosts = slices.Compact(hosts)
	return s.cfg.Store.EditProjectSetup(ctx, req.Name, func(p *pb.Project) error {
		p.Hosts = hosts
		return nil
	})
}

// hosted is a project as the workers that keep its bases need it: with each
// of its repos the cluster has. A remote it holds that no repo has is
// refused: its checkout would be missing from every workspace.
func (s *Server) hosted(ctx context.Context, p *pb.Project) (*pb.HostedProject, error) {
	repos, err := s.cfg.Store.Repos(ctx)
	if err != nil {
		return nil, err
	}
	h, missing := hostedOf(p, repos)
	switch {
	case len(missing) > 0:
		return nil, api.Errorf(api.CodeNotFound, "project %s holds %s, which no repo of the cluster has", p.Name, strings.Join(missing, ", ")).
			WithSuggestion(fmt.Sprintf("hotbed repo add NAME %s -p %s registers it; or hotbed project rm-repo %s --repo %s", missing[0], p.Name, p.Name, missing[0]))
	case len(h.Repos) == 0:
		return nil, api.Errorf(api.CodeNotFound, "project %s holds no repos to check out", p.Name).
			WithSuggestion(fmt.Sprintf("hotbed project add %s --repo REMOTE, then hotbed repo add NAME REMOTE", p.Name))
	}
	return h, nil
}

// hostedOf is p with each of its repos among repos, and the remotes it
// holds that none of them has.
func hostedOf(p *pb.Project, repos []*pb.Repo) (h *pb.HostedProject, missing []string) {
	h = &pb.HostedProject{Name: p.Name, Steps: p.Steps, ScratchEpoch: p.ScratchEpoch}
	for _, remote := range p.Repos {
		i := slices.IndexFunc(repos, func(r *pb.Repo) bool { return gitops.CanonicalRemote(r.Url) == remote })
		if i < 0 {
			missing = append(missing, remote)
			continue
		}
		h.Repos = append(h.Repos, repos[i])
	}
	return h, missing
}

// ListSteps says, for each of a project's steps, whether each node's newest
// finished base ran the step as it is now.
func (s *Server) ListSteps(ctx context.Context, req *pb.ListStepsRequest) (*pb.ListStepsResponse, error) {
	r, err := s.cfg.Store.Project(ctx, req.Project)
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
			if b.Project == r.Name && b.State != pb.BaseState_BASE_STATE_BUILDING && (nb.base == nil || b.Generation > nb.base.Generation) {
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
