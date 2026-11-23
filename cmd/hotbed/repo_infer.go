package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
)

// resolveRepo returns explicit when non-empty; otherwise it infers the repo ID
// from dir (or the current working directory when dir is empty):
//  1. A repo of a workspace in session whose SyncDir matches this git
//     checkout's root.
//  2. A registered repo on the controller whose normalized git URL matches one
//     of this checkout's remotes (origin checked first).
//  3. A registered repo whose ID matches the checkout's directory name (or the
//     main repository's directory name for a linked git worktree).
func (f controllerClient) resolveRepo(ctx context.Context, d deps, session, explicit, dir string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	root, err := gitCheckoutRoot(ctx, dir)
	if err != nil {
		return "", api.Errorf(api.CodeInvalidArgs, "repo is required outside a git checkout (%s)", dir).
			WithSuggestion("run from inside a repo checkout, set HOTBED_REPO, or pass -r REPO")
	}
	canonRoot := gitops.CanonicalPath(root)

	cl, err := f.clients(d.http)
	if err != nil {
		return "", err
	}

	// 1. Match an existing workspace of this session by its SyncDir.
	if session != "" {
		// Out of reach, the controller is not waited for: this machine
		// knows where it last found the session's workspaces.
		if list, lerr := cl.workspaces.ListWorkspaces(api.WithoutRideOut(ctx), &pb.ListWorkspacesRequest{SessionId: session}); lerr == nil {
			for _, w := range list.Workspaces {
				for _, r := range w.Repos {
					if r.SyncDir != "" && gitops.CanonicalPath(r.SyncDir) == canonRoot {
						return r.Repo, nil
					}
				}
			}
		} else if api.CodeOf(lerr) == api.CodeControllerUnreachable {
			if known, kerr := loadKnown(d, session); kerr == nil {
				for _, sp := range known.Workspaces {
					for repoID, dir := range sp.SyncDirs {
						if gitops.CanonicalPath(dir) == canonRoot {
							return repoID, nil
						}
					}
				}
			}
		}
	}

	// 2. Match checkout remotes or repo directory name against registered repos.
	repos, err := cl.cluster.ListRepos(ctx, &pb.ListReposRequest{})
	if err != nil {
		return "", err
	}
	remotes := gitRemoteURLs(ctx, root)
	if len(remotes) > 0 {
		for _, rem := range remotes {
			normRem := gitops.CanonicalRemote(rem)
			if normRem == "" {
				continue
			}
			for _, r := range repos.Repos {
				if gitops.CanonicalRemote(r.Url) == normRem {
					return r.Id, nil
				}
			}
		}
	}

	// 3. Fall back to matching the checkout's directory name (and common git dir
	// parent for linked worktrees) against registered repo IDs.
	names := gitRepoCandidateNames(ctx, root)
	for _, name := range names {
		for _, r := range repos.Repos {
			if r.Id == name {
				return r.Id, nil
			}
		}
	}

	return "", api.Errorf(api.CodeInvalidArgs, "could not infer hotbed repo from checkout %s", root).
		WithSuggestion("pass -r REPO, set HOTBED_REPO, or check registered repos with `hotbed repo ls`")
}

func gitCheckoutRoot(ctx context.Context, dir string) (string, error) {
	return gitops.Git{Dir: dir}.Run(ctx, "rev-parse", "--show-toplevel")
}

// gitRemoteURLs returns unique remote URLs for the git checkout at root, with
// origin first when present.
func gitRemoteURLs(ctx context.Context, root string) []string {
	out, err := gitops.Git{Dir: root}.Run(ctx, "remote", "-v")
	if err != nil || out == "" {
		return nil
	}
	var origin, others []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name, rawURL := fields[0], fields[1]
		if seen[rawURL] {
			continue
		}
		seen[rawURL] = true
		if name == "origin" {
			origin = append(origin, rawURL)
		} else {
			others = append(others, rawURL)
		}
	}
	return append(origin, others...)
}

func gitRepoCandidateNames(ctx context.Context, root string) []string {
	var names []string
	add := func(s string) {
		if s == "" || s == "." || s == "/" {
			return
		}
		for _, existing := range names {
			if existing == s {
				return
			}
		}
		names = append(names, s)
	}
	if common, err := (gitops.Git{Dir: root}).Run(ctx, "rev-parse", "--git-common-dir"); err == nil && common != "" {
		if !filepath.IsAbs(common) {
			common = filepath.Join(root, common)
		}
		common = gitops.CanonicalPath(common)
		if filepath.Base(common) == ".git" {
			add(filepath.Base(filepath.Dir(common)))
		} else {
			add(strings.TrimSuffix(filepath.Base(common), ".git"))
		}
	}
	add(filepath.Base(gitops.CanonicalPath(root)))
	return names
}
