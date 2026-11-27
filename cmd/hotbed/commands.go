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
	"slices"
	"strings"
	"time"

	"github.com/alecthomas/kong"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/apicmd"
	"github.com/semistrict/hotbed/internal/setup"
	"github.com/semistrict/hotbed/internal/version"
	"github.com/semistrict/hotbed/internal/worker"
	"github.com/semistrict/hotbed/skills"
)

// deps is what commands use from the outside world, so tests can replace it.
type deps struct {
	// here is where the command runs: its environment and working directory.
	here
	// http calls the API; stream reads output, which has no deadline.
	http, stream *http.Client
	check        worker.Check
	// controller is the controller client commands call (localController).
	controller func() (string, error)
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
	// rideOut is how long a call waits for the controller or a worker that
	// is out of reach, as when restarting, before failing; 0 not to wait.
	rideOut time.Duration
	// cache keeps cursors read on this machine: each session's polls', and
	// each review's responses'.
	cache string
	// sessionsFile maps T3CODE_THREAD_ID / git worktree roots to default sessions
	// (default ~/.hotbed/sessions.json).
	sessionsFile string
	// build is the client's build (0 for a dev build), exe its binary, and
	// reexec runs a binary: what following the controller's build needs.
	build   int64
	exe     func() (string, error)
	reexec  func(args ...string) error
	openURL func(ctx context.Context, url string) error
	// cockpit makes the cockpit's URLs; run sets it for a command that
	// calls the controller.
	cockpit cockpit
	// output controls how a command prints structured output (--json, --jq,
	// or plain text); run sets it from the parsed global flags.
	output outputOpts
	// execve replaces the process with another program (syscall.Exec).
	execve func(path string, argv, env []string) error
	// localWorker finds the worker on this machine that serves its clients
	// on a Unix socket; nil for setup.LocalWorker's.
	localWorker func() (setup.LocalWorkerSocket, error)
}

// dashDashCommand accepts passthrough arguments after "--", parsed before kong
// so optional positional arguments (like an inferred repo name) never swallow
// the command after "--". SetDashArgs returns what kong had passed through
// from before "--": an unknown flag there, and all after it, which would be
// dropped unnoticed.
type dashDashCommand interface {
	SetDashArgs(args []string) (before []string)
}

// takesDashArgs is cmd as one that takes the words after --, if it does: an
// API command does when it has a passthrough param.
func takesDashArgs(cmd command) (dashDashCommand, bool) {
	if ac, ok := cmd.(*apiCommand); ok && ac.passthrough() == nil {
		return nil, false
	}
	dd, ok := cmd.(dashDashCommand)
	return dd, ok
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
  hotbed skills get hotbed     How to work in hotbed: sessions, run, poll, errors
  hotbed skills install        Install the guides into ~/.claude/skills
  hotbed COMMAND --help        Flags and arguments for one command

Commands print concise plain text by default. Pass --json for one JSON
envelope, or --jq EXPR to filter the response JSON.

Run builds, tests and dev servers on a cluster of Linux machines, each in
a fork of a repo whose main is already built.`

type cli struct {
	JSON bool   `name:"json" help:"Print the response as one JSON envelope."`
	JQ   string `name:"jq" help:"Filter the response JSON with a jq expression."`

	Session   sessionCmd   `cmd:"" help:"Start or end a session; everything an agent runs belongs to one."`
	Run       runCmd       `cmd:"" help:"Run a command on the node the controller picks (--node, --arch narrow it), in the session's workspace there, which it gets if it has none; run from a checkout of the repo, it syncs it there first."`
	Shell     shellCmd     `cmd:"" help:"Open an interactive shell (over SSH) in the session's workspace (reuses an existing workspace when it has one; --sync pushes local checkout edits first)."`
	Sync      syncCmd      `cmd:"" help:"Send a checkout's state, uncommitted changes too, to every workspace the session has of its repo."`
	Logs      logsCmd      `cmd:"" help:"Print a command's output (all of it, its last lines, or until it ends)."`
	Stop      stopCmd      `cmd:"" help:"Stop a command; with --comment and --diagnose, the agent waiting on it is told why and how to read what it was doing."`
	Pause     pauseCmd     `cmd:"" help:"Pause a command by hand (frozen, its memory to swap), as stop takes a comment and diagnostics; it stays paused until hotbed resume."`
	Resume    resumeCmd    `cmd:"" help:"Resume a paused command."`
	Inspect   inspectCmd   `cmd:"" help:"Opt-in Linux inspection of a running command (procs, files, cgroup, kstack, maps, limits)."`
	Sample    sampleCmd    `cmd:"" help:"Activity Monitor-style Linux sample of a running command: user & kernel stacks, syscalls, open files/sockets/locks, cgroup/PSI deltas, and perf hardware counters."`
	Trace     traceCmd     `cmd:"" help:"Opt-in active Linux tracing of a running command (strace, perf, stat, stacks, bpf)."`
	Profile   profileCmd   `cmd:"" help:"Query the always-on OpenTelemetry eBPF continuous profile for a command (live or finished) or a node (--node HOST)."`
	Workspace workspaceCmd `cmd:"" help:"A session's workspaces (ls, show, files, file, release); hotbed run gives it one on each node it runs on."`
	Step      stepCmd      `cmd:"" help:"A repo's setup steps: what brings its bases up to date (add, set, ls, show, rm)."`
	Board     boardCmd     `cmd:"" help:"Project-scoped message board & task queue (read, post, reply, edit, delete, react, label, wait, tail, upload, download, actors, schema, schedule, task, import)."`
	UI        uiCmd        `cmd:"" help:"Open the controller's cockpit UI in the default browser."`
	Login     loginCmd     `cmd:"" help:"Sign in to a Hotbed controller via one-time device code (works locally and over SSH)."`
	Logout    logoutCmd    `cmd:"" help:"Remove locally stored credentials for a Hotbed controller."`
	Whoami    whoamiCmd    `cmd:"" help:"Show the signed-in user, active organization, and role."`
	Org       orgCmd       `cmd:"" help:"Manage WorkOS organizations, members, invitations, worker join tokens, and personal API tokens (ls, switch, create, members, invite, join-token, api-token)."`
	MCP       mcpCmd       `cmd:"" name:"mcp" help:"Serve hotbed's commands as MCP tools on stdin and stdout, for an agent to start. The server is a session: the one HOTBED_SESSION or this T3 thread or git worktree names (resumed if it ended), or a new one bound here; it ends when the server does, and the next server started here resumes it."`
	Daemon    daemonCmd    `cmd:"" name:"daemon" help:"Run or manage the local Hotbed host service (T3 Code question notifications, a Mac's worker and machined, and macOS menubar status app: run, install, uninstall, status)."`
	Join      joinCmd      `cmd:"" help:"Join this machine to a cluster as a worker (as root)."`
	Worker    workerCmd    `cmd:"" help:"Initialize and manage a worker node on this machine in ~/.hotbed (init, status, start, stop, trim, ssh, destroy)."`
	Serve     serveCmd     `cmd:"" help:"Run the worker whose state is in DIR, until stopped."`
	Skills    skillsCmd    `cmd:"" help:"Guides for AI agents, compiled into this binary (list, get, install)."`
	Version   versionCmd   `cmd:"" help:"Show hotbed's version, OS and architecture."`
	Upgrade   upgradeCmd   `cmd:"" help:"Install the controller's build over this hotbed (a client on a CI build does it by itself)."`
	API       rawAPICmd    `cmd:"" name:"api" help:"Make an authenticated HTTP or ConnectRPC request to the controller (like gh api)."`
}

// exited is how parse learns that kong finished the run itself, as it does
// after printing --help.
type exited struct{ code int }

// parse returns the command args name, its name, and its output options;
// what the command line does not give, the variables its env tags name in
// getenv do. When kong handled args itself (--help), cmd is nil and exitCode
// is set.
func parse(args []string, stdout io.Writer, getenv func(string) string) (cmd command, name string, opts outputOpts, exitCode *int, err error) {
	if len(args) == 0 {
		args = []string{"--help"}
	}
	if sep := slices.Index(args, "--"); sep >= 0 {
		cmd, name, opts, exitCode, err = parseOnce(args[:sep], stdout, getenv)
		if err == nil && exitCode == nil {
			if dd, ok := takesDashArgs(cmd); ok {
				if before := dd.SetDashArgs(args[sep+1:]); len(before) > 0 {
					what := "unexpected argument"
					if strings.HasPrefix(before[0], "-") {
						what = "unknown flag"
					}
					return nil, "", outputOpts{}, nil, api.Errorf(api.CodeInvalidArgs, "%s %s of hotbed %s (before --)", what, before[0], name).
						WithSuggestion(fmt.Sprintf("hotbed %s --help lists its flags; the command goes after --", name))
				}
				return cmd, name, opts, nil, nil
			}
		}
	}
	return parseOnce(args, stdout, getenv)
}

func parseOnce(args []string, stdout io.Writer, getenv func(string) string) (cmd command, name string, opts outputOpts, exitCode *int, err error) {
	var c cli
	generated, err := apiCommands()
	if err != nil {
		return nil, "", outputOpts{}, nil, api.Errorf(api.CodeInternal, "%v", err)
	}
	var envs envFlags
	parser, err := kong.New(&c, append([]kong.Option{
		kong.Name("hotbed"),
		kong.Description(description),
		kong.Writers(stdout, stdout),
		kong.Exit(func(code int) { panic(exited{code}) }),
		kong.ValueFormatter(func(v *kong.Value) string { return envs.helpValue(v) }),
	}, generated.Options()...)...)
	if err != nil {
		return nil, "", outputOpts{}, nil, err
	}
	envs = takeEnvs(parser.Model)
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(exited)
			if !ok {
				panic(r)
			}
			cmd, exitCode, err = nil, &e.code, nil
		}
	}()
	kctx, perr := parser.Parse(args)
	if perr == nil {
		perr = envs.fill(kctx, getenv)
	}
	opts = outputOpts{JSON: c.JSON, JQ: c.JQ}
	if perr != nil && !opts.JSON {
		opts.JSON = slices.Contains(args, "--json")
	}
	cmd, name, err = selected(generated, kctx, perr)
	return cmd, name, opts, nil, err
}

func selected(generated *apicmd.Kong, kctx *kong.Context, err error) (command, string, error) {
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
	if p, ok := generated.Selected(kctx.Selected()); ok {
		return &apiCommand{Parsed: p}, name, nil
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
	return map[string]any{"dir": tildePath(dir), "files": wrote}, err
}

type versionCmd struct{}

type versionResult struct {
	Version string `json:"version"`
	Build   int64  `json:"build"` // 0 for a dev build
	Commit  string `json:"commit,omitempty"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

func (c *versionCmd) Exec(context.Context, deps) (any, error) {
	return versionResult{Version: version.String(), Build: version.Number(), Commit: version.Commit(), OS: runtime.GOOS, Arch: runtime.GOARCH}, nil
}
