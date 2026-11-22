package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/shell"
)

type stepCmd struct {
	Add  stepAddCmd  `cmd:"" help:"Try a step in a session's workspace, twice to check it is idempotent, then save it to the repo; new bases run it."`
	Set  stepSetCmd  `cmd:"" help:"Save a step without trying it, such as a repo's build when setting it up."`
	Ls   stepLsCmd   `cmd:"" help:"A repo's steps, and whether each node's newest base has run them."`
	Show stepShowCmd `cmd:"" help:"Print a step's script."`
	Rm   stepRmCmd   `cmd:"" help:"Remove a step; bases keep what it did."`
}

// stepScript is how a step's script is given, on both add and set.
type stepScript struct {
	EveryPull bool     `help:"Run it after every pull too, not only when its script changes: builds, and installs from a lockfile."`
	Before    string   `help:"Put it before this step (default: where it was, or last)."`
	File      string   `type:"existingfile" help:"Read the script from this file instead of the words after --."`
	Args      []string `arg:"" optional:"" passthrough:"" help:"The script, after --: one word is a bash script; several are a command."`
}

func (s stepScript) script() (string, error) {
	args := s.Args
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	switch {
	case s.File != "" && len(args) > 0:
		return "", api.Errorf(api.CodeInvalidArgs, "give the script either with --file or after --, not both")
	case s.File != "":
		b, err := os.ReadFile(s.File)
		return string(b), err
	case len(args) == 1:
		return args[0], nil
	case len(args) > 1:
		return shell.Join(args), nil
	default:
		return "", api.Errorf(api.CodeInvalidArgs, "no script").
			WithSuggestion("hotbed step add SESSION REPO NAME -- 'pnpm exec playwright install chromium'")
	}
}

func (s stepScript) put(ctx context.Context, cl clients, repo, name, script string) (*pb.Repo, error) {
	return cl.cluster.PutStep(ctx, &pb.PutStepRequest{
		RepoId: repo, Before: s.Before, Step: &pb.Step{Name: name, Script: script, EveryPull: s.EveryPull},
	})
}

type stepAddCmd struct {
	controllerFlag
	Session string `arg:"" help:"The session whose workspace of the repo tries the step."`
	Arg1    string `arg:"" name:"repo-or-name" help:"The repo (omit inside a checkout), or the step's name."`
	Arg2    string `arg:"" optional:"" name:"name" help:"The step's name, when REPO is given."`
	stepScript
}

func (c *stepAddCmd) SetDashArgs(args []string) { c.Args = args }

func splitRepoAndName(arg1, arg2 string) (repo, name string) {
	if arg2 == "" {
		return "", arg1
	}
	return arg1, arg2
}

// Exec runs the step in the session's workspace, so the agent has what it
// installs at once, then again, because bases run a step on top of what
// they already have; only a step that passes both is saved.
func (c *stepAddCmd) Exec(ctx context.Context, d deps) (any, error) {
	script, err := c.script()
	if err != nil {
		return nil, err
	}
	rawRepo, name := splitRepoAndName(c.Arg1, c.Arg2)
	repo, err := c.resolveRepo(ctx, d, c.Session, rawRepo, "")
	if err != nil {
		return nil, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	ws, w, err := c.locate(ctx, d, c.Session, repo)
	if err != nil {
		return nil, err
	}
	if ws, err = waitReady(ctx, w, ws, d.stderr); err != nil {
		return nil, err
	}
	for try := 1; try <= 2; try++ {
		cmd, err := w.calls.RunCommand(ctx, &pb.RunCommandRequest{
			WorkspaceId: ws.Id, Args: []string{"bash", "-eo", "pipefail", "-c", script}, Kind: "shell", Attached: true,
			Limits: &pb.CommandLimits{CpuWeight: 100},
		})
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(d.stderr, "hotbed: trying step %s in %s, run %d of 2 (%s)\n", name, ws.Id, try, cmd.Id)
		// The step's output goes to stderr: stdout is the envelope.
		code, err := attach(ctx, w, cmd.Id, d.stderr, d.stderr)
		if err != nil {
			return nil, err
		}
		switch {
		case code != 0 && try == 1:
			return nil, api.Errorf(api.CodeStepFailed, "step %s exited %d in %s; it was not saved", name, code, ws.Id).
				WithSuggestion(fmt.Sprintf("read its output (hotbed logs %s %s), fix the script and add it again", c.Session, cmd.Id))
		case code != 0:
			return nil, api.Errorf(api.CodeStepNotIdempotent, "step %s worked once but exited %d run again in %s; it was not saved", name, code, ws.Id).
				WithSuggestion("bases run steps over what they already have: make the script safe to repeat (check before installing, mkdir -p, --force), then add it again")
		}
	}
	return c.put(ctx, cl, repo, name, script)
}

type stepSetCmd struct {
	controllerFlag
	Arg1 string `arg:"" name:"repo-or-name" help:"The repo (omit inside a checkout), or the step's name."`
	Arg2 string `arg:"" optional:"" name:"name" help:"The step's name, when REPO is given."`
	stepScript
}

func (c *stepSetCmd) SetDashArgs(args []string) { c.Args = args }

func (c *stepSetCmd) Exec(ctx context.Context, d deps) (any, error) {
	script, err := c.script()
	if err != nil {
		return nil, err
	}
	rawRepo, name := splitRepoAndName(c.Arg1, c.Arg2)
	repo, err := c.resolveRepo(ctx, d, "", rawRepo, "")
	if err != nil {
		return nil, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return c.put(ctx, cl, repo, name, script)
}

type stepLsCmd struct {
	controllerFlag
	Repo string `arg:"" optional:"" help:"The repo (default: inferred from the current checkout)."`
}

func (c *stepLsCmd) Exec(ctx context.Context, d deps) (any, error) {
	repo, err := c.resolveRepo(ctx, d, "", c.Repo, "")
	if err != nil {
		return nil, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.cluster.ListSteps(ctx, &pb.ListStepsRequest{RepoId: repo})
}

type stepShowCmd struct {
	controllerFlag
	Arg1 string `arg:"" name:"repo-or-name" help:"The repo (omit inside a checkout), or the step's name."`
	Arg2 string `arg:"" optional:"" name:"name" help:"The step's name, when REPO is given."`
}

// Raw prints the script as is, to read or to edit and add again.
func (c *stepShowCmd) Raw(ctx context.Context, d deps) (int, bool, error) {
	rawRepo, name := splitRepoAndName(c.Arg1, c.Arg2)
	repo, err := c.resolveRepo(ctx, d, "", rawRepo, "")
	if err != nil {
		return 0, false, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return 0, false, err
	}
	res, err := cl.cluster.ListSteps(ctx, &pb.ListStepsRequest{RepoId: repo})
	if err != nil {
		return 0, false, err
	}
	for _, s := range res.Steps {
		if s.Step.Name == name {
			script := s.Step.Script
			if !strings.HasSuffix(script, "\n") {
				script += "\n"
			}
			_, err := io.WriteString(d.stdout, script)
			return exitOK, true, err
		}
	}
	return 0, false, api.Errorf(api.CodeNotFound, "repo %s has no step %s", repo, name).WithSuggestion("hotbed step ls " + repo)
}

func (c *stepShowCmd) Exec(context.Context, deps) (any, error) {
	return nil, errors.New("step show is raw")
}

type stepRmCmd struct {
	controllerFlag
	Arg1 string `arg:"" name:"repo-or-name" help:"The repo (omit inside a checkout), or the step's name."`
	Arg2 string `arg:"" optional:"" name:"name" help:"The step's name, when REPO is given."`
}

func (c *stepRmCmd) Exec(ctx context.Context, d deps) (any, error) {
	rawRepo, name := splitRepoAndName(c.Arg1, c.Arg2)
	repo, err := c.resolveRepo(ctx, d, "", rawRepo, "")
	if err != nil {
		return nil, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.cluster.DeleteStep(ctx, &pb.DeleteStepRequest{RepoId: repo, Name: name})
}
