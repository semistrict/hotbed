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
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/worker"
)

// deps is what commands use from the outside world, so tests can replace it.
type deps struct {
	tailnet controller.Tailnet
	http    *http.Client
	probe   worker.Prober
}

// command is a leaf of the command tree. Exec returns the envelope's data.
type command interface {
	Exec(ctx context.Context, d deps) (any, error)
}

const description = `For AI agents, read this first:
  hotbed skills get hotbed     Core guide: setting up a cluster, the JSON output, error codes
  hotbed COMMAND --help        Flags and arguments for one command

Every command prints one JSON envelope on stdout.

Run builds, tests and dev servers on a cluster of Linux machines on your tailnet.`

type cli struct {
	Init    initCmd    `cmd:"" help:"Create a cluster with this machine as its controller."`
	Join    joinCmd    `cmd:"" help:"Join this machine to a cluster as a worker."`
	Run     runCmd     `cmd:"" help:"Run the controller or worker whose state is in DIR, until stopped."`
	Nodes   nodesCmd   `cmd:"" help:"List the cluster's workers."`
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

type initCmd struct {
	Dir  string `arg:"" type:"path" help:"State directory to create; holds the cluster's database."`
	Port int    `default:"7120" help:"Port to serve on, on this machine's tailnet addresses."`
}

func (c *initCmd) Exec(ctx context.Context, d deps) (any, error) {
	return controller.Init(ctx, c.Dir, c.Port, d.tailnet)
}

type joinCmd struct {
	Controller string `arg:"" help:"The controller's tailnet name or address, with :PORT unless it is 7120."`
	Dir        string `arg:"" type:"path" help:"State directory to create for this worker."`
}

func (c *joinCmd) Exec(ctx context.Context, d deps) (any, error) {
	return worker.Join(ctx, c.Dir, c.Controller, d.http, d.probe)
}

type runCmd struct {
	Dir string `arg:"" type:"path" help:"State directory made by init or join."`
}

type runResult struct {
	Role string `json:"role"`
}

func (c *runCmd) Exec(ctx context.Context, d deps) (any, error) {
	cfg, err := statedir.Load(c.Dir)
	if err != nil {
		return nil, err
	}
	if cfg.Controller != nil {
		return runResult{Role: "controller"}, controller.Run(ctx, c.Dir, *cfg.Controller, d.tailnet)
	}
	w, err := worker.New(*cfg.Worker, d.http, d.probe)
	if err != nil {
		return nil, err
	}
	return runResult{Role: "worker"}, w.Run(ctx)
}

type nodesCmd struct {
	Controller string `required:"" env:"HOTBED_CONTROLLER" help:"The controller's tailnet name or address."`
}

func (c *nodesCmd) Exec(ctx context.Context, d deps) (any, error) {
	client, err := api.NewClient(c.Controller, d.http)
	if err != nil {
		return nil, err
	}
	return client.Nodes(ctx)
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
