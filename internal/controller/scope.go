package controller

import (
	"context"
	"strings"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
)

// scoped is a listing's request: whose things it lists, as a project
// named or every project (its scope oneof), or else the project of the
// session it names, or else the one project holding the caller's
// checkout's remote.
type scoped interface {
	GetProject() string
	GetAllProjects() bool
	GetSessionId() string
	GetRemote() string
}

// listedProject is the project a listing is about, "" for every project's.
func (s *Server) listedProject(ctx context.Context, req scoped) (string, error) {
	sess, err := s.namedSession(ctx, req.GetSessionId())
	if err != nil {
		return "", err
	}
	return s.projectOf(ctx, req, sess)
}

// namedSession is the session a request names, nil for none: one named
// must be one, even where it goes unused, so a mistyped name fails.
func (s *Server) namedSession(ctx context.Context, id string) (*pb.Session, error) {
	if id == "" {
		return nil, nil
	}
	return s.cfg.Store.Session(ctx, id)
}

// projectOf is the project req's scope names ("" for every project's),
// which must be one, or else sess's, or else the one holding req's remote.
func (s *Server) projectOf(ctx context.Context, req scoped, sess *pb.Session) (string, error) {
	switch {
	case req.GetAllProjects():
		return "", nil
	case req.GetProject() != "":
		if _, err := s.cfg.Store.Project(ctx, req.GetProject()); err != nil {
			return "", err
		}
		return req.GetProject(), nil
	case sess == nil && req.GetRemote() != "":
		return s.projectHolding(ctx, req.GetRemote())
	case sess == nil:
		return "", api.Errorf(api.CodeInvalidArgs, "say which project: name one, all of them, or the session whose project it is").
			WithSuggestion("pass -p PROJECT, --all-projects, or -s SESSION")
	case sess.Project == "":
		return "", api.Errorf(api.CodeInvalidArgs, "session %s has no project", sess.Id).WithSuggestion("pass -p PROJECT or --all-projects")
	}
	return sess.Project, nil
}

// projectHolding is the one project that holds remote, a listing's when
// nothing else says whose.
func (s *Server) projectHolding(ctx context.Context, remote string) (string, error) {
	remote = gitops.CanonicalRemote(remote)
	holding, err := s.cfg.Store.ProjectsHolding(ctx, remote)
	if err != nil {
		return "", err
	}
	switch len(holding) {
	case 1:
		return holding[0].Name, nil
	case 0:
		return "", api.Errorf(api.CodeNotFound, "say which project: no project holds %s, this checkout's repo", remote).
			WithSuggestion("pass -p PROJECT or --all-projects (hotbed project ls)")
	}
	names := make([]string, len(holding))
	for i, p := range holding {
		names[i] = p.Name
	}
	return "", api.Errorf(api.CodeInvalidArgs, "say which project: %s, this checkout's repo, is in projects %s", remote, strings.Join(names, ", ")).
		WithSuggestion("pass -p PROJECT or --all-projects")
}
