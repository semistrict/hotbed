package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/alecthomas/kong"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/controller"
	"github.com/semistrict/hotbed/internal/worker"
	"github.com/semistrict/hotbed/skills"
)

// deps is what commands use from the outside world, so tests can replace it.
type deps struct {
	tailnet controller.Tailnet
	// http calls the API; stream reads output, which has no deadline.
	http, stream *http.Client
	check        worker.Check
	stdout       io.Writer
	stderr       io.Writer
	// cache keeps per-session cursors for hotbed wait.
	cache string
}

// command is a leaf of the command tree. Exec returns the envelope's data.
type command interface {
	Exec(ctx context.Context, d deps) (any, error)
}

// rawCommand is a command whose output is not an envelope: run, attached,
// prints its command's output and exits with its status. When it fails
// before the command starts, it prints an envelope like any other.
type rawCommand interface {
	Raw(ctx context.Context, d deps) (exitCode int, printed bool, err error)
}

const description = `For AI agents, read this first:
  hotbed skills get hotbed     How to work in hotbed: sessions, run, wait, the JSON output, errors
  hotbed skills install        Install the guides into ~/.claude/skills
  hotbed COMMAND --help        Flags and arguments for one command

Every command prints one JSON envelope on stdout, except run (attached) and
logs, which print the command's output.

Run builds, tests and dev servers on a cluster of Linux machines on your
tailnet, each in a fork of a repo whose main is already built.`

type cli struct {
	Session sessionCmd `cmd:"" help:"Start or end a session; everything an agent runs belongs to one."`
	Add     addCmd     `cmd:"" help:"Give a session its workspace of a repo: a fork of the repo's built main."`
	Run     runCmd     `cmd:"" help:"Run a command in a session's workspace (syncing first if it follows a checkout)."`
	Sync    syncCmd    `cmd:"" help:"Send a checkout's state, uncommitted changes too, to a session's workspace."`
	Wait    waitCmd    `cmd:"" help:"Wait (up to two minutes) for something to happen to a session's commands."`
	Ps      psCmd      `cmd:"" help:"List a session's commands."`
	Logs    logsCmd    `cmd:"" help:"Print a command's output."`
	Stop    stopCmd    `cmd:"" help:"Stop a command."`
	Repo    repoCmd    `cmd:"" help:"Repos the cluster keeps built bases of (add, ls, rm)."`
	Nodes   nodesCmd   `cmd:"" help:"List the cluster's workers."`
	Init    initCmd    `cmd:"" help:"Create a cluster with this machine as its controller."`
	Join    joinCmd    `cmd:"" help:"Join this machine to a cluster as a worker (as root)."`
	Serve   serveCmd   `cmd:"" help:"Run the controller or worker whose state is in DIR, until stopped."`
	Skills  skillsCmd  `cmd:"" help:"Guides for AI agents, compiled into this binary (list, get, install)."`
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
	List    skillsListCmd    `cmd:"" help:"List the guides."`
	Get     skillsGetCmd     `cmd:"" help:"Print a guide, or one of its references."`
	Install skillsInstallCmd `cmd:"" help:"Write the guides into an agent's skills directory."`
}

type skillsListCmd struct{}

func (c *skillsListCmd) Exec(context.Context, deps) (any, error) {
	list, err := skills.List()
	return map[string]any{"skills": list}, err
}

type skillsGetCmd struct {
	Name string `arg:"" help:"A guide's name (hotbed), or a reference in it (hotbed/references/errors.md)."`
}

func (c *skillsGetCmd) Exec(context.Context, deps) (any, error) {
	content, err := skills.Get(c.Name)
	if err != nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "%v", err).WithSuggestion("hotbed skills list names every guide and its references")
	}
	return map[string]string{"name": c.Name, "content": content}, nil
}

type skillsInstallCmd struct {
	Dir string `type:"path" help:"Skills directory to write into (default ~/.claude/skills)."`
}

func (c *skillsInstallCmd) Exec(context.Context, deps) (any, error) {
	dir := c.Dir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(home, ".claude", "skills")
	}
	wrote, err := skills.Install(dir)
	return map[string]any{"dir": dir, "files": wrote}, err
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
