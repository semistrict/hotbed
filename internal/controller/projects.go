package controller

import (
	"context"
	"regexp"
	"strings"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
)

// A project is what sessions work on and questions are about: one or more
// repos, each named by its canonical git remote. A repo no project holds is
// the project of its own name, made the first time a session starts in it.

var projectName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

func checkProjectName(name string) error {
	if !projectName.MatchString(name) {
		return api.Errorf(api.CodeInvalidArgs, "project name %q must be lowercase letters, digits and dashes, at most 40", name)
	}
	return nil
}

func (s *Server) AddProject(ctx context.Context, req *pb.AddProjectRequest) (*pb.Project, error) {
	if err := checkProjectName(req.Name); err != nil {
		return nil, err
	}
	remotes := make([]string, 0, len(req.Repos))
	for _, r := range req.Repos {
		c := gitops.CanonicalRemote(r)
		if c == "" {
			return nil, api.Errorf(api.CodeInvalidArgs, "repo %q is not a git remote", r)
		}
		remotes = append(remotes, c)
	}
	return s.cfg.Store.AddProject(ctx, req.Name, remotes, s.now())
}

func (s *Server) ListProjects(ctx context.Context, _ *pb.ListProjectsRequest) (*pb.ListProjectsResponse, error) {
	ps, err := s.cfg.Store.Projects(ctx)
	return &pb.ListProjectsResponse{Projects: ps}, err
}

// sessionProject is the project a session starts in: the one named, or
// else the one that holds remote (canonical), or else the project of
// remote's own name, made for it, with fallback the name to make when
// remote's own (its last part) is not one.
func (s *Server) sessionProject(ctx context.Context, name, remote, fallback string) (*pb.Project, error) {
	if name != "" {
		return s.cfg.Store.Project(ctx, name)
	}
	if remote == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "a session works on a project: outside a git checkout, name it").
			WithSuggestion("pass -p PROJECT or set HOTBED_PROJECT (hotbed project ls)")
	}
	holding, err := s.cfg.Store.ProjectsHolding(ctx, remote)
	if err != nil {
		return nil, err
	}
	switch len(holding) {
	case 1:
		return holding[0], nil
	case 0:
	default:
		names := make([]string, len(holding))
		for i, p := range holding {
			names[i] = p.Name
		}
		return nil, api.Errorf(api.CodeInvalidArgs, "%s is in projects %s: name the session's project", remote, strings.Join(names, ", ")).
			WithSuggestion("pass -p PROJECT or set HOTBED_PROJECT")
	}
	own := remote[strings.LastIndex(remote, "/")+1:]
	if checkProjectName(own) != nil {
		own = fallback
	}
	if err := checkProjectName(own); err != nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "no project holds %s, and its name makes none", remote).
			WithSuggestion("make one: hotbed project add NAME --repo " + remote)
	}
	if p, err := s.cfg.Store.Project(ctx, own); err == nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "no project holds %s, and project %s holds other repos (%s)", remote, own, strings.Join(p.Repos, ", ")).
			WithSuggestion("add it there (hotbed project add " + own + " --repo " + remote + "), or pass -p PROJECT")
	} else if api.FromConnect(err).Code != api.CodeNotFound {
		return nil, err
	}
	return s.cfg.Store.AddProject(ctx, own, []string{remote}, s.now())
}
