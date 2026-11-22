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
	ps, err := s.projects(ctx)
	return &pb.ListProjectsResponse{Projects: ps}, err
}

// projects is every project, with its live sessions and its questions
// waiting on the user and on agents counted.
func (s *Server) projects(ctx context.Context) ([]*pb.Project, error) {
	ps, err := s.cfg.Store.Projects(ctx)
	if err != nil {
		return nil, err
	}
	live, err := s.cfg.Store.LiveSessionsByProject(ctx)
	if err != nil {
		return nil, err
	}
	questions, err := s.cfg.Store.QuestionProjects(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		p.LiveSessions = live[p.Name]
		for _, q := range questions {
			if q.Name == p.Name {
				p.OpenQuestions, p.Outbox = q.Open, q.Outbox
			}
		}
	}
	return ps, nil
}

// sessionProject is the project a session starts in: the one named, or
// else the one that holds remote (canonical), or else the project named
// after remote (its last part, made a project's name), made for it or, when
// a project of that name holds no repo yet, given it.
func (s *Server) sessionProject(ctx context.Context, name, remote string) (*pb.Project, error) {
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
	own := projectNameFor(remote)
	if own == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "no project holds %s, and its name makes none", remote).
			WithSuggestion("make one: hotbed project add NAME --repo " + remote)
	}
	p, err := s.cfg.Store.Project(ctx, own)
	switch {
	case err == nil && len(p.Repos) > 0:
		return nil, api.Errorf(api.CodeInvalidArgs, "no project holds %s, and project %s holds other repos (%s)", remote, own, strings.Join(p.Repos, ", ")).
			WithSuggestion("add it there (hotbed project add " + own + " --repo " + remote + "), or pass -p PROJECT")
	case err != nil && api.FromConnect(err).Code != api.CodeNotFound:
		return nil, err
	}
	return s.cfg.Store.AddProject(ctx, own, []string{remote}, s.now())
}

var notInName = regexp.MustCompile(`[^a-z0-9]+`)

// ensureRepoProject makes a single-repo project for repoURL when no project
// holds its canonical remote yet. It returns the unique project holding the
// repo, or nil if several projects hold it or no candidate project name is
// free.
func (s *Server) ensureRepoProject(ctx context.Context, repoID, repoURL string) (*pb.Project, error) {
	remote := gitops.CanonicalRemote(repoURL)
	if remote == "" {
		return nil, nil
	}
	holding, err := s.cfg.Store.ProjectsHolding(ctx, remote)
	if err != nil {
		return nil, err
	}
	if len(holding) == 1 {
		return holding[0], nil
	}
	if len(holding) > 1 {
		return nil, nil
	}
	var candidates []string
	if own := projectNameFor(remote); own != "" {
		candidates = append(candidates, own)
	}
	if repoID != "" && (len(candidates) == 0 || candidates[0] != repoID) && checkProjectName(repoID) == nil {
		candidates = append(candidates, repoID)
	}
	for _, name := range candidates {
		p, err := s.cfg.Store.Project(ctx, name)
		switch {
		case err == nil && len(p.Repos) > 0:
			continue
		case err != nil && api.FromConnect(err).Code != api.CodeNotFound:
			return nil, err
		}
		return s.cfg.Store.AddProject(ctx, name, []string{remote}, s.now())
	}
	return nil, nil
}

// EnsureProjects makes sure every registered repo belongs to a project (making
// a single-repo project for any repo none holds yet) and attaches any session
// that has no project yet to its workspace's repo's project when unambiguous.
func (s *Server) EnsureProjects(ctx context.Context) error {
	repos, err := s.cfg.Store.Repos(ctx)
	if err != nil {
		return err
	}
	repoRemote := make(map[string]string, len(repos))
	for _, r := range repos {
		if _, err := s.ensureRepoProject(ctx, r.Id, r.Url); err != nil {
			return err
		}
		if remote := gitops.CanonicalRemote(r.Url); remote != "" {
			repoRemote[r.Id] = remote
		}
	}
	orphans, err := s.cfg.Store.SessionsWithoutProject(ctx)
	if err != nil {
		return err
	}
	for _, sess := range orphans {
		for _, repoID := range sess.repos {
			remote := repoRemote[repoID]
			if remote == "" {
				continue
			}
			holding, err := s.cfg.Store.ProjectsHolding(ctx, remote)
			if err != nil {
				return err
			}
			if len(holding) == 1 {
				if err := s.cfg.Store.SetSessionProject(ctx, sess.id, holding[0].Name); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

// projectNameFor is the project named after a repo: its remote's last
// part, lowercased, with anything else a name may not hold made a dash
// (github.com/vercel/Next.js is next-js); empty when nothing is left.
func projectNameFor(remote string) string {
	name := strings.ToLower(remote[strings.LastIndex(remote, "/")+1:])
	name = strings.Trim(notInName.ReplaceAllString(name, "-"), "-")
	if len(name) > 40 {
		name = strings.TrimRight(name[:40], "-")
	}
	return name
}
