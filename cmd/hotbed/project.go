package main

import (
	"context"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
)

// Projects: what sessions work on and questions are about. A project holds
// one or more repos, each named by its canonical git remote, as a T3 Code
// project holds folders.

type projectCmd struct {
	Add    projectAddCmd    `cmd:"" help:"Make a project, or add repos to one. A repo no project holds is a project of its own name, made when a session first starts in it. A repo another project holds needs --shared."`
	RmRepo projectRmRepoCmd `cmd:"" name:"rm-repo" help:"Take repos out of a project; sessions already in it stay."`
	Ls     projectLsCmd     `cmd:"" help:"List the projects and the repos each holds."`
	Show   projectShowCmd   `cmd:"" help:"A project, with every change made to it: who made it, and added or took out each repo, when."`
}

// projectFlag names the project a command is about.
type projectFlag struct {
	Project string `short:"p" env:"HOTBED_PROJECT" help:"The project (default: HOTBED_PROJECT, or the one that holds this checkout's repo)."`
}

type projectAddCmd struct {
	controllerClient
	sessionFlag
	Name   string   `arg:"" help:"Lowercase letters, digits and dashes."`
	Repo   []string `help:"A git remote it holds, in any form (git@github.com:o/r.git, https://github.com/o/r, github.com/o/r). Repeatable."`
	Shared bool     `help:"Add a repo another project holds too; sessions starting in its checkouts will then have to name their project (-p)."`
}

func (c *projectAddCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	// Recorded as the session named with -s, if any (see sessionFlag.named).
	return cl.cluster.AddProject(ctx, &pb.AddProjectRequest{Name: c.Name, Repos: c.Repo, Shared: c.Shared, SessionId: c.named()})
}

type projectRmRepoCmd struct {
	controllerClient
	sessionFlag
	Name string   `arg:"" help:"The project."`
	Repo []string `required:"" help:"A git remote it holds, in any form. Repeatable."`
}

func (c *projectRmRepoCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.cluster.RemoveProjectRepos(ctx, &pb.RemoveProjectReposRequest{Name: c.Name, Repos: c.Repo, SessionId: c.named()})
}

type projectShowCmd struct {
	controllerClient
	Name string `arg:"" help:"The project."`
}

func (c *projectShowCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.cluster.GetProject(ctx, &pb.GetProjectRequest{Name: c.Name})
}

type projectLsCmd struct {
	controllerClient
}

func (c *projectLsCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.cluster.ListProjects(ctx, &pb.ListProjectsRequest{})
}

// checkoutRemote is the canonical remote of the git checkout this command
// runs in (origin's, or its first remote's); empty outside one.
func checkoutRemote(ctx context.Context) (string, error) {
	root, err := gitCheckoutRoot(ctx, ".")
	if err != nil {
		return "", nil // not in a checkout
	}
	remotes := gitRemoteURLs(ctx, root)
	if len(remotes) == 0 {
		return "", nil
	}
	return gitops.CanonicalRemote(remotes[0]), nil
}

// listed is the project a listing is about: every one (all, over even
// HOTBED_PROJECT), the one named, or else the session's.
func (f projectFlag) listed(ctx context.Context, d deps, cl clients, session sessionFlag, all bool) (string, error) {
	switch {
	case all:
		return "", nil
	case f.Project != "":
		return f.Project, nil
	}
	id, err := session.resolve(ctx, d)
	if err != nil {
		return "", api.Errorf(api.CodeInvalidArgs, "say which project: no session is named or bound here to take it from").
			WithSuggestion("pass -p PROJECT, --all-projects, or -s SESSION")
	}
	res, err := cl.sessions.GetSession(ctx, &pb.GetSessionRequest{Id: id})
	if err != nil {
		return "", err
	}
	sess := res.Session
	if sess.GetProject() == "" {
		return "", api.Errorf(api.CodeInvalidArgs, "session %s has no project", id).WithSuggestion("pass -p PROJECT or --all-projects")
	}
	return sess.Project, nil
}
