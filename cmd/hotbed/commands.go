package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/alecthomas/kong"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/controller"
	"github.com/semistrict/hotbed/internal/skills"
	"github.com/semistrict/hotbed/internal/worker"
)

// deps is what commands use from the outside world, so tests can replace it.
type deps struct {
	tailnet controller.Tailnet
	// http calls the API; stream reads output, which has no deadline.
	http, stream *http.Client
	check        worker.Check
	stdout       io.Writer
	stderr       io.Writer
}

// command is a leaf of the command tree. Exec returns the envelope's data.
type command interface {
	Exec(ctx context.Context, d deps) (any, error)
}

// rawCommand is a command whose output is not an envelope: exec, attached,
// prints its command's output and exits with its status. When it fails
// before the command starts, it prints an envelope like any other.
type rawCommand interface {
	Raw(ctx context.Context, d deps) (exitCode int, raw bool, err error)
}

const description = `For AI agents, read this first:
  hotbed skills get hotbed     Core guide: setting up a cluster, the JSON output, error codes
  hotbed COMMAND --help        Flags and arguments for one command

Every command prints one JSON envelope on stdout.

Run builds, tests and dev servers on a cluster of Linux machines on your tailnet.`

type cli struct {
	Ws      wsCmd      `cmd:"" help:"Workspaces: forks of a repo's warm base (new, ls, rm)."`
	Sync    syncCmd    `cmd:"" help:"Send this checkout's changes, uncommitted ones too, to a workspace."`
	Exec    execCmd    `cmd:"" help:"Sync, then run a command in a workspace; prints its output and exits with its status."`
	Ps      psCmd      `cmd:"" help:"List running, queued and parked commands."`
	Logs    logsCmd    `cmd:"" help:"Print a command's output."`
	Stop    stopCmd    `cmd:"" help:"Stop a command."`
	Repo    repoCmd    `cmd:"" help:"Repos the cluster keeps warm bases of (add, ls, rm)."`
	Nodes   nodesCmd   `cmd:"" help:"List the cluster's workers."`
	Init    initCmd    `cmd:"" help:"Create a cluster with this machine as its controller."`
	Join    joinCmd    `cmd:"" help:"Join this machine to a cluster as a worker (as root)."`
	Run     runCmd     `cmd:"" help:"Run the controller or worker whose state is in DIR, until stopped."`
	Skills  skillsCmd  `cmd:"" help:"Guides for AI agents, compiled into this binary (list, get)."`
	Version versionCmd `cmd:"" help:"Show hotbed's version, OS and architecture."`
}

// exited is how parse learns that kong finished the run itself, as it does
// after printing --help.
type exited struct{ code int }

// parse returns the command args name and its name, such as "skills get".
// When kong handled args itself (--help), cmd is nil and exitCode is set.
func parse(args []string, stdout io.Writer) (cmd command, name string, exitCode *int, err error) {
	if len(args) == 0 {
		args = []string{"--help"}
	}
	var c cli
	parser, err := kong.New(&c,
		kong.Name("hotbed"),
		kong.Description(description),
		kong.Writers(stdout, stdout),
		kong.Exit(func(code int) { panic(exited{code}) }),
	)
	if err != nil {
		return nil, "", nil, err
	}
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(exited)
			if !ok {
				panic(r)
			}
			cmd, exitCode, err = nil, &e.code, nil
		}
	}()
	cmd, name, err = selected(parser.Parse(args))
	return cmd, name, nil, err
}

func selected(kctx *kong.Context, err error) (command, string, error) {
	// A parse error carries the context parsed so far, which names the command.
	var pe *kong.ParseError
	if kctx == nil && errors.As(err, &pe) {
		kctx = pe.Context
	}
	name := ""
	if kctx != nil && kctx.Selected() != nil {
		name = commandName(kctx.Selected())
	}
	if err != nil {
		return nil, name, err
	}
	cmd, ok := kctx.Selected().Target.Addr().Interface().(command)
	if !ok {
		return nil, name, fmt.Errorf("%q is not a command", name)
	}
	return cmd, name, nil
}

func commandName(n *kong.Node) string {
	var parts []string
	for ; n != nil && n.Type != kong.ApplicationNode; n = n.Parent {
		parts = append([]string{n.Name}, parts...)
	}
	return strings.Join(parts, " ")
}

type skillsCmd struct {
	List skillsListCmd `cmd:"" help:"List the guides."`
	Get  skillsGetCmd  `cmd:"" help:"Print one guide."`
}

type skillsListCmd struct{}

func (c *skillsListCmd) Exec(context.Context, deps) (any, error) {
	return map[string]any{"skills": skills.List()}, nil
}

type skillsGetCmd struct {
	Name string `arg:"" help:"The guide's name, from skills list."`
}

func (c *skillsGetCmd) Exec(context.Context, deps) (any, error) {
	content, ok := skills.Get(c.Name)
	if !ok {
		return nil, api.Errorf(api.CodeInvalidArgs, "no guide called %q", c.Name).
			WithSuggestion("hotbed skills list names every guide")
	}
	return map[string]string{"name": c.Name, "content": content}, nil
}

type versionCmd struct{}

type versionResult struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

func (c *versionCmd) Exec(context.Context, deps) (any, error) {
	v := versionResult{Version: "(devel)", OS: runtime.GOOS, Arch: runtime.GOARCH}
	if info, ok := debug.ReadBuildInfo(); ok {
		v.Version = info.Main.Version
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				v.Commit = s.Value
			}
		}
	}
	return v, nil
}
