package controller

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/httpx"
)

// A project is what sessions work on, schedules run in, and questions and
// reviews belong to: one or more repos, each named by its canonical git
// remote. A repo no project holds becomes a project of its own name.

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
	remotes, err := canonicalRemotes(req.Repos)
	if err != nil {
		return nil, err
	}
	if !req.Shared {
		// A repo in two projects leaves session start in its checkouts not
		// knowing which to join: say so before it happens, not after.
		for _, r := range remotes {
			holding, err := s.cfg.Store.ProjectsHolding(ctx, r)
			if err != nil {
				return nil, err
			}
			var others []string
			for _, p := range holding {
				if p.Name != req.Name {
					others = append(others, p.Name)
				}
			}
			if len(others) > 0 {
				return nil, api.Errorf(api.CodeConflict, "%s is in project %s already: in a second project, every session starting in its checkouts would have to name its project (-p)", r, strings.Join(others, ", ")).
					WithSuggestion("pass --shared to add it anyway, or take it out of the other first: hotbed project rm-repo " + others[0] + " --repo " + r)
			}
		}
	}
	by, _, err := s.userStamp(ctx, req.SessionId, "")
	if err != nil {
		return nil, err
	}
	return s.cfg.Store.AddProject(ctx, req.Name, remotes, by)
}

func (s *Server) RemoveProjectRepos(ctx context.Context, req *pb.RemoveProjectReposRequest) (*pb.Project, error) {
	if len(req.Repos) == 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "name the repos to take out of project %s", req.Name).WithSuggestion("hotbed project rm-repo NAME --repo REMOTE")
	}
	remotes, err := canonicalRemotes(req.Repos)
	if err != nil {
		return nil, err
	}
	by, _, err := s.userStamp(ctx, req.SessionId, "")
	if err != nil {
		return nil, err
	}
	return s.cfg.Store.RemoveProjectRepos(ctx, req.Name, remotes, by)
}

func (s *Server) GetProject(ctx context.Context, req *pb.GetProjectRequest) (*pb.Project, error) {
	ps, err := s.projects(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		if p.Name == req.Name {
			if p.History, err = s.cfg.Store.ProjectHistory(ctx, req.Name); err != nil {
				return nil, err
			}
			return p, nil
		}
	}
	return nil, noProject(req.Name)
}

// canonicalRemotes are git remotes, in any form, as projects hold them.
func canonicalRemotes(repos []string) ([]string, error) {
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		c := gitops.CanonicalRemote(r)
		if c == "" {
			return nil, api.Errorf(api.CodeInvalidArgs, "repo %q is not a git remote", r)
		}
		out = append(out, c)
	}
	return out, nil
}

// madeBy stamps a project the controller makes by itself, for why.
func (s *Server) madeBy(ctx context.Context, why string) *pb.Stamp {
	host := httpx.Peer(ctx).Hostname
	if host == "" {
		host = s.cfg.Controller
	}
	return &pb.Stamp{At: s.now().UnixMilli(), Host: host, Reason: why}
}

func (s *Server) ListProjects(ctx context.Context, _ *pb.ListProjectsRequest) (*pb.ListProjectsResponse, error) {
	ps, err := s.projects(ctx)
	return &pb.ListProjectsResponse{Projects: ps}, err
}

// projects is every project, with its live sessions, schedules, questions
// waiting on the user and on agents, and reviews under way or to decide.
func (s *Server) projects(ctx context.Context) ([]*pb.Project, error) {
	ps, err := s.cfg.Store.Projects(ctx)
	if err != nil {
		return nil, err
	}
	live, err := s.cfg.Store.LiveSessionsByProject(ctx)
	if err != nil {
		return nil, err
	}
	scheds, err := s.cfg.Store.SchedulesByProject(ctx)
	if err != nil {
		return nil, err
	}
	questions, err := s.cfg.Store.QuestionProjects(ctx)
	if err != nil {
		return nil, err
	}
	active, toDecide, err := s.cfg.Store.ReviewCounts(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		p.LiveSessions, p.ActiveReviews, p.ReviewsToDecide, p.Schedules = live[p.Name], active[p.Name], toDecide[p.Name], scheds[p.Name]
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
	return s.cfg.Store.AddProject(ctx, own, []string{remote}, s.madeBy(ctx, "for "+remote+", which a session started in and no project held"))
}

var notInName = regexp.MustCompile(`[^a-z0-9]+`)

// ensureRepoProject makes a single-repo project for repoURL when no project
// holds its canonical remote yet. If the preferred name is held by a project
// with other repos, it tries repoID and numbered suffixes so every registered
// repo belongs to a project.
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
	base := "repo"
	if len(candidates) > 0 {
		base = candidates[0]
	}
	for n := 2; n <= 99; n++ {
		suffix := fmt.Sprintf("-%d", n)
		prefix := base
		if len(prefix)+len(suffix) > 40 {
			prefix = strings.TrimRight(prefix[:40-len(suffix)], "-")
		}
		candidates = append(candidates, prefix+suffix)
	}
	for _, name := range candidates {
		p, err := s.cfg.Store.Project(ctx, name)
		switch {
		case err == nil && len(p.Repos) > 0:
			continue
		case err != nil && api.FromConnect(err).Code != api.CodeNotFound:
			return nil, err
		}
		return s.cfg.Store.AddProject(ctx, name, []string{remote}, s.madeBy(ctx, "for registered repo "+repoID+", which no project held"))
	}
	return nil, api.Errorf(api.CodeConflict, "could not find a free project name for repo %s", repoID).
		WithSuggestion("pass -p PROJECT to place the repo in an explicit project")
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
