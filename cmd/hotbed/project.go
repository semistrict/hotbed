package main

import (
	"context"
	"os"
	"slices"
	"strings"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
)

// Projects: what sessions work on and questions are about. A project holds
// one or more repos, each named by its canonical git remote, as a T3 Code
// project holds folders. The `project` CLI commands themselves are generated
// from hotbed.proto; this file holds shared project/checkout scope helpers
// for hand-written commands.

// projectFlag names the project a command is about.
type projectFlag struct {
	Project string `short:"p" env:"HOTBED_PROJECT" help:"The project (default: HOTBED_PROJECT, the session's project, or the one that holds this checkout's repo)."`
}

// resolve is the project the flag names; or else the session's, when one
// is bound here; or else the one project holding this checkout's repo.
func (f projectFlag) resolve(ctx context.Context, d deps, cc controllerClient, session sessionFlag) (string, error) {
	if f.Project != "" {
		return f.Project, nil
	}
	cl, err := cc.clients(d.http)
	if err != nil {
		return "", err
	}
	if b, found, err := session.lookupBinding(ctx, d); err != nil {
		return "", err
	} else if found {
		res, err := cl.sessions.GetSession(ctx, &pb.GetSessionRequest{Id: b.ID})
		if err == nil {
			return res.Session.Project, nil
		}
		if api.CodeOf(err) != api.CodeNotFound {
			return "", err
		}
	}
	remote, err := checkoutRemote(ctx)
	if err != nil {
		return "", err
	}
	if remote != "" {
		ps, err := cl.cluster.ListProjects(ctx, &pb.ListProjectsRequest{})
		if err != nil {
			return "", err
		}
		var holding []string
		for _, p := range ps.Projects {
			if slices.Contains(p.Repos, remote) {
				holding = append(holding, p.Name)
			}
		}
		switch len(holding) {
		case 1:
			return holding[0], nil
		case 0:
			return "", api.Errorf(api.CodeNotFound, "no project holds %s", remote).
				WithSuggestion("hotbed repo add NAME URL makes it a project of its own; or pass -p PROJECT")
		default:
			return "", api.Errorf(api.CodeConflict, "%s is in projects %s", remote, strings.Join(holding, ", ")).WithSuggestion("pass -p PROJECT to say which")
		}
	}
	return "", api.Errorf(api.CodeInvalidArgs, "no project named, no session here, and this is no checkout").
		WithSuggestion("pass -p PROJECT (hotbed project ls lists them), or run it in a checkout or session of the project")
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

// listScope is whose things a listing asks for, which the controller
// resolves: every project's, the project named, or else the session's, or
// else the one holding this checkout's remote.
type listScope struct {
	project string
	all     bool
	session string
	remote  string
}

// listing is a listing's scope: every project (all, over even
// HOTBED_PROJECT), the one -p names, or else the session's, or else the
// one that holds this checkout's repo.
func (f projectFlag) listing(ctx context.Context, d deps, session sessionFlag, all bool) (listScope, error) {
	// HOTBED_PROJECT fills -p too, and --all-projects outweighs it; only
	// a -p given beside --all-projects contradicts it.
	if all && f.Project != "" && f.Project != strings.TrimSpace(os.Getenv("HOTBED_PROJECT")) {
		return listScope{}, api.Errorf(api.CodeInvalidArgs, "--project and --all-projects can't be used together")
	}
	b, _, err := session.lookupBinding(ctx, d)
	if err != nil {
		return listScope{}, err
	}
	remote, err := checkoutRemote(ctx)
	if err != nil {
		return listScope{}, err
	}
	sc := listScope{all: all, session: b.ID, remote: remote}
	if !all {
		sc.project = f.Project
	}
	return sc, nil
}
