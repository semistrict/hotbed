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
	Repo    string `arg:"" help:"The repo."`
	Name    string `arg:"" help:"The step's name, such as playwright-browsers; a step of that name is replaced."`
	stepScript
}

// Exec runs the step in the session's workspace, so the agent has what it
// installs at once, then again, because bases run a step on top of what
// they already have; only a step that passes both is saved.
func (c *stepAddCmd) Exec(ctx context.Context, d deps) (any, error) {
	script, err := c.script()
	if err != nil {
		return nil, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	ws, w, err := c.locate(ctx, d, c.Session, c.Repo)
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
		fmt.Fprintf(d.stderr, "hotbed: trying step %s in %s, run %d of 2 (%s)\n", c.Name, ws.Id, try, cmd.Id)
		// The step's output goes to stderr: stdout is the envelope.
		code, err := attach(ctx, w, cmd.Id, d.stderr, d.stderr)
		if err != nil {
			return nil, err
		}
		switch {
		case code != 0 && try == 1:
			return nil, api.Errorf(api.CodeStepFailed, "step %s exited %d in %s; it was not saved", c.Name, code, ws.Id).
				WithSuggestion(fmt.Sprintf("read its output (hotbed logs %s %s), fix the script and add it again", c.Session, cmd.Id))
		case code != 0:
			return nil, api.Errorf(api.CodeStepNotIdempotent, "step %s worked once but exited %d run again in %s; it was not saved", c.Name, code, ws.Id).
				WithSuggestion("bases run steps over what they already have: make the script safe to repeat (check before installing, mkdir -p, --force), then add it again")
		}
	}
	return c.put(ctx, cl, c.Repo, c.Name, script)
}

type stepSetCmd struct {
	controllerFlag
	Repo string `arg:"" help:"The repo."`
	Name string `arg:"" help:"The step's name; a step of that name is replaced."`
	stepScript
}

func (c *stepSetCmd) Exec(ctx context.Context, d deps) (any, error) {
	script, err := c.script()
	if err != nil {
		return nil, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return c.put(ctx, cl, c.Repo, c.Name, script)
}

type stepLsCmd struct {
	controllerFlag
	Repo string `arg:"" help:"The repo."`
}

func (c *stepLsCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.cluster.ListSteps(ctx, &pb.ListStepsRequest{RepoId: c.Repo})
}

type stepShowCmd struct {
	controllerFlag
	Repo string `arg:"" help:"The repo."`
	Name string `arg:"" help:"The step."`
}

// Raw prints the script as is, to read or to edit and add again.
func (c *stepShowCmd) Raw(ctx context.Context, d deps) (int, bool, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return 0, false, err
	}
	res, err := cl.cluster.ListSteps(ctx, &pb.ListStepsRequest{RepoId: c.Repo})
	if err != nil {
		return 0, false, err
	}
	for _, s := range res.Steps {
		if s.Step.Name == c.Name {
			script := s.Step.Script
			if !strings.HasSuffix(script, "\n") {
				script += "\n"
			}
			_, err := io.WriteString(d.stdout, script)
			return exitOK, true, err
		}
	}
	return 0, false, api.Errorf(api.CodeNotFound, "repo %s has no step %s", c.Repo, c.Name).WithSuggestion("hotbed step ls " + c.Repo)
}

func (c *stepShowCmd) Exec(context.Context, deps) (any, error) {
	return nil, errors.New("step show is raw")
}

type stepRmCmd struct {
	controllerFlag
	Repo string `arg:"" help:"The repo."`
	Name string `arg:"" help:"The step."`
}

func (c *stepRmCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.cluster.DeleteStep(ctx, &pb.DeleteStepRequest{RepoId: c.Repo, Name: c.Name})
}
