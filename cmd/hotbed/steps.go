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
	Add  stepAddCmd  `cmd:"" help:"Try a step in /work of a session's workspace, twice to check it is idempotent, then save it to the session's project; new bases run it."`
	Set  stepSetCmd  `cmd:"" help:"Save a step without trying it, such as a project's build when setting it up."`
	Ls   stepLsCmd   `cmd:"" help:"A project's steps, and whether each node's newest base has run them."`
	Show stepShowCmd `cmd:"" help:"Print a step's script."`
	Rm   stepRmCmd   `cmd:"" help:"Remove a step; bases keep what it did."`
}

// stepScript is how a step's script is given, on both add and set.
type stepScript struct {
	EveryPull  bool     `help:"Run it after every pull too, not only when its script changes: builds, and installs from a lockfile."`
	Before     string   `help:"Put it before this step (default: where it was, or last)."`
	IfRevision int64    `name:"if-revision" help:"Refuse with CONFLICT unless the project's setup revision matches."`
	File       string   `type:"existingfile" help:"Read the script from this file instead of the words after --."`
	Args       []string `arg:"" optional:"" passthrough:"" help:"The script, after --: one word is a bash script; several are a command."`
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
			WithSuggestion("hotbed step add NAME -- 'pnpm exec playwright install chromium'")
	}
}

func (s stepScript) put(ctx context.Context, cl clients, project, name, script string) (*pb.Project, error) {
	return cl.cluster.PutStep(ctx, &pb.PutStepRequest{
		Project: project, Before: s.Before, IfRevision: s.IfRevision, Step: &pb.Step{Name: name, Script: script, EveryPull: s.EveryPull},
	})
}

type stepAddCmd struct {
	controllerClient
	sessionFlag
	Name string `arg:"" help:"The step's name, such as playwright-browsers; a step of that name is replaced."`
	stepScript
}

func (c *stepAddCmd) SetDashArgs(args []string) { c.Args = args }

// Exec runs the step in /work of the session's workspace, as bases run it,
// so the agent has what it installs at once, then again, because bases run
// a step on top of what they already have; only a step that passes both is
// saved, to the session's project.
func (c *stepAddCmd) Exec(ctx context.Context, d deps) (any, error) {
	script, err := c.script()
	if err != nil {
		return nil, err
	}
	session, err := c.sessionFlag.resolve(ctx, d)
	if err != nil {
		return nil, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	ws, w, err := c.workspaceFor(ctx, d, &pb.CreateWorkspaceRequest{SessionId: session, Placement: &pb.Placement{}})
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
				WithSuggestion(fmt.Sprintf("read its output (hotbed logs -s %s %s), fix the script and add it again", session, cmd.Id))
		case code != 0:
			return nil, api.Errorf(api.CodeStepNotIdempotent, "step %s worked once but exited %d run again in %s; it was not saved", c.Name, code, ws.Id).
				WithSuggestion("bases run steps over what they already have: make the script safe to repeat (check before installing, mkdir -p, --force), then add it again")
		}
	}
	return c.put(ctx, cl, ws.Project, c.Name, script)
}

type stepSetCmd struct {
	controllerClient
	sessionFlag
	projectFlag
	Name string `arg:"" help:"The step's name; a step of that name is replaced."`
	stepScript
}

func (c *stepSetCmd) SetDashArgs(args []string) { c.Args = args }

func (c *stepSetCmd) Exec(ctx context.Context, d deps) (any, error) {
	script, err := c.script()
	if err != nil {
		return nil, err
	}
	project, err := c.projectFlag.resolve(ctx, d, c.controllerClient, c.sessionFlag)
	if err != nil {
		return nil, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return c.put(ctx, cl, project, c.Name, script)
}

type stepLsCmd struct {
	controllerClient
	sessionFlag
	projectFlag
}

func (c *stepLsCmd) Exec(ctx context.Context, d deps) (any, error) {
	project, err := c.projectFlag.resolve(ctx, d, c.controllerClient, c.sessionFlag)
	if err != nil {
		return nil, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.cluster.ListSteps(ctx, &pb.ListStepsRequest{Project: project})
}

type stepShowCmd struct {
	controllerClient
	sessionFlag
	projectFlag
	Name string `arg:"" help:"The step."`
}

// Raw prints the script as is, to read or to edit and add again.
func (c *stepShowCmd) Raw(ctx context.Context, d deps) (int, bool, error) {
	project, err := c.projectFlag.resolve(ctx, d, c.controllerClient, c.sessionFlag)
	if err != nil {
		return 0, false, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return 0, false, err
	}
	res, err := cl.cluster.ListSteps(ctx, &pb.ListStepsRequest{Project: project})
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
	return 0, false, api.Errorf(api.CodeNotFound, "project %s has no step %s", project, c.Name).WithSuggestion("hotbed step ls -p " + project)
}

func (c *stepShowCmd) Exec(context.Context, deps) (any, error) {
	return nil, errors.New("step show is raw")
}

type stepRmCmd struct {
	controllerClient
	sessionFlag
	projectFlag
	IfRevision int64  `name:"if-revision" help:"Refuse with CONFLICT unless the project's setup revision matches."`
	Name       string `arg:"" help:"The step."`
}

func (c *stepRmCmd) Exec(ctx context.Context, d deps) (any, error) {
	project, err := c.projectFlag.resolve(ctx, d, c.controllerClient, c.sessionFlag)
	if err != nil {
		return nil, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.cluster.DeleteStep(ctx, &pb.DeleteStepRequest{Project: project, Name: c.Name, IfRevision: c.IfRevision})
}
