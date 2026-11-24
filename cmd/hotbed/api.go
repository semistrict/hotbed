package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/apicmd"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1cli"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/gitops"
)

// The commands hotbed.proto declares (see "commands and tools" there) are
// made from its descriptors at run time, for the command line here and as
// tools by hotbed mcp; what they need of this machine beyond literal/env
// defaults is cliHooks.

// apiCommands is hotbed.proto's commands, and their kong types.
var apiCommands = sync.OnceValues(func() (*apicmd.Kong, error) {
	s, err := apiSchema()
	if err != nil {
		return nil, err
	}
	return s.Kong()
})

var apiSchema = sync.OnceValues(func() (*apicmd.Schema, error) {
	return apicmd.Load(hotbedv1cli.Descriptors)
})

// apiCommand is one of hotbed.proto's commands, as parsed.
type apiCommand struct {
	controllerClient
	apicmd.Parsed
}

func (c *apiCommand) Exec(ctx context.Context, d deps) (any, error) {
	if c.Command.Input != nil {
		b, err := jsonInput{File: c.File}.read(d, "pipe the command's JSON in, or pass --file FILE")
		if err != nil {
			return nil, err
		}
		c.Args.Input = b
	}
	r, err := c.runner(d, cliSessions{d})
	if err != nil {
		return nil, err
	}
	return r.Run(ctx, c.Command, c.Args)
}

// runner runs hotbed.proto's commands against the controller.
func (f controllerClient) runner(d deps, sessions apicmd.Sessions) (*apicmd.Runner, error) {
	calls, err := api.Dial(f.Controller, api.DefaultPort, d.http, f.interceptors()...)
	if err != nil {
		return nil, err
	}
	waits, err := api.Dial(f.Controller, api.DefaultPort, d.stream, f.interceptors()...)
	if err != nil {
		return nil, err
	}
	hooks := cliHooks{
		cache:   d.cache,
		stdin:   d.stdin,
		stderr:  d.stderr,
		gitAuth: hotbedv1connect.NewGitAuthServiceClient(calls),
		openURL: d.openURL,
	}
	return &apicmd.Runner{Hooks: hotbedv1cli.Table(hooks), Calls: calls, Waits: waits, Sessions: sessions}, nil
}

// cliSessions is the session a command on the command line acts as: -s,
// HOTBED_SESSION, or the one bound to this T3 thread or git worktree; none
// otherwise, which the controller refuses where a session is needed.
type cliSessions struct{ d deps }

func (s cliSessions) Session(ctx context.Context, given string) (string, error) {
	b, _, err := sessionFlag{Session: given}.lookupBinding(ctx, s.d)
	return b.ID, err
}

func (s cliSessions) Named(_ context.Context, given string) (string, error) {
	return sessionFlag{Session: given}.named(), nil
}

// cliHooks is the Go hooks hotbed.proto's commands name.
type cliHooks struct {
	cache   string // where this machine keeps each session's poll cursor
	stdin   io.Reader
	stderr  io.Writer
	gitAuth hotbedv1connect.GitAuthServiceClient
	openURL func(ctx context.Context, url string) error
}

var _ hotbedv1cli.Hooks = cliHooks{}

// cursorFile persists an int64 read cursor in the CLI cache directory.
type cursorFile struct {
	path, label, suggestion string
}

func (c cursorFile) read() (int64, error) {
	b, err := os.ReadFile(c.path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, api.Errorf(api.CodeInternal, "read the %s: %v", c.label, err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		e := api.Errorf(api.CodeInternal, "the %s in %s: %v", c.label, c.path, err)
		if c.suggestion != "" {
			e = e.WithSuggestion(c.suggestion)
		}
		return 0, e
	}
	return n, nil
}

func (c cursorFile) write(cursor int64) error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return api.Errorf(api.CodeInternal, "save the %s: %v", c.label, err)
	}
	if err := os.WriteFile(c.path, []byte(strconv.FormatInt(cursor, 10)), 0o644); err != nil {
		return api.Errorf(api.CodeInternal, "save the %s: %v", c.label, err)
	}
	return nil
}

func (h cliHooks) pollCursor(session string) cursorFile {
	return cursorFile{
		path:       filepath.Join(h.cache, "sessions", session+".cursor"),
		label:      "poll cursor",
		suggestion: "remove the file to poll from the session's first event, or pass --after",
	}
}

// PollCursor is where this machine's last poll of the session left off: 0
// for none yet.
func (h cliHooks) PollCursor(_ context.Context, _ *apicmd.Call, req *pb.PollRequest) (int64, error) {
	return h.pollCursor(req.SessionId).read()
}

// SavePollCursor records where the poll left off, once its reply is in
// hand, so the next one goes on from there and a lost reply loses nothing.
func (h cliHooks) SavePollCursor(_ context.Context, _ *apicmd.Call, req *pb.PollRequest, res *pb.SessionEvents) error {
	return h.pollCursor(req.SessionId).write(res.Cursor)
}

// RepoToPut builds PutRepoRequest.Repo from the local id, url, and branch flags.
func (cliHooks) RepoToPut(_ context.Context, _ *apicmd.Call, req *pb.PutRepoRequest) (*pb.Repo, error) {
	return &pb.Repo{Id: req.Id, Url: req.Url, Branch: req.Branch}, nil
}

// PsSession resolves the active session for `hotbed ps` unless --all or -p
// was given.
func (cliHooks) PsSession(ctx context.Context, c *apicmd.Call, req *pb.ListCommandsRequest) (string, error) {
	if req.All || req.Project != "" {
		return "", nil
	}
	s, err := c.Session(ctx)
	if err != nil {
		return "", err
	}
	if s == "" {
		return "", api.Errorf(api.CodeInvalidArgs, "no session specified and none bound to this thread or worktree").
			WithSuggestion("run `hotbed session start`, set HOTBED_SESSION, or pass -s SESSION")
	}
	return s, nil
}

// StartGitHubLoginInteractive initiates the GitHub OAuth Device Flow on the
// controller when `hotbed auth login` is called without --login-id, prints the
// one-time code and verification URL to stderr, optionally opens a browser,
// and populates req.LoginId for CompleteGitHubLogin to poll.
func (h cliHooks) StartGitHubLoginInteractive(ctx context.Context, _ *apicmd.Call, req *pb.CompleteGitHubLoginRequest) error {
	if req.LoginId != "" {
		return nil
	}
	ch, err := h.gitAuth.StartGitHubLogin(ctx, &pb.StartGitHubLoginRequest{
		Name:     req.Name,
		Host:     req.Host,
		Repos:    req.Repos,
		Scopes:   req.Scopes,
		ClientId: req.ClientId,
		ApiUrl:   req.ApiUrl,
		OauthUrl: req.OauthUrl,
	})
	if err != nil {
		return err
	}
	errOut := h.stderr
	if errOut == nil {
		errOut = os.Stderr
	}
	fmt.Fprintf(errOut, "! First copy your one-time code: %s\n", ch.UserCode)
	fmt.Fprintf(errOut, "Open this URL in your browser to authorize Hotbed: %s\n", ch.VerificationUri)
	if req.Web {
		opener := h.openURL
		if opener == nil {
			opener = openDefaultBrowser
		}
		if oerr := opener(ctx, ch.VerificationUri); oerr != nil {
			fmt.Fprintf(errOut, "Could not open browser automatically: %v\n", oerr)
		}
	}
	fmt.Fprintf(errOut, "Waiting for GitHub authorization on the controller...\n")
	req.LoginId = ch.LoginId
	return nil
}

// LoadGitCredentialSecrets reads --token-file (or stdin when "-") into the
// request before local fields are cleared on the wire.
func (h cliHooks) LoadGitCredentialSecrets(_ context.Context, _ *apicmd.Call, req *pb.PutGitCredentialRequest) error {
	if req.TokenFile != "" {
		if req.Token != "" {
			return api.Errorf(api.CodeInvalidArgs, "give --token or --token-file, not both")
		}
		b, err := h.readSecretFile(req.TokenFile, "--token-file")
		if err != nil {
			return err
		}
		req.Token = strings.TrimSpace(string(b))
	}
	return nil
}

func (h cliHooks) readSecretFile(path, flag string) ([]byte, error) {
	if path == "-" {
		in := h.stdin
		if in == nil {
			in = os.Stdin
		}
		b, err := io.ReadAll(io.LimitReader(in, 1<<20))
		if err != nil {
			return nil, api.Errorf(api.CodeInvalidArgs, "read %s from stdin: %v", flag, err)
		}
		return b, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "read %s %s: %v", flag, path, err)
	}
	return b, nil
}

func (cliHooks) AskedFrom(ctx context.Context, _ *apicmd.Call, _ *pb.AddQuestionsRequest) (*pb.AskedFrom, error) {
	return askedFrom(ctx)
}

// ListedRemote is this checkout's remote, whose project question ls lists
// when nothing else says whose.
func (cliHooks) ListedRemote(ctx context.Context, _ *apicmd.Call, _ *pb.ListQuestionsRequest) (string, error) {
	return checkoutRemote(ctx)
}

const editExample = `hotbed question edit 12 <<'EOF'
{"question": "Which store should sessions use?", "header": "Store",
 "options": [{"label": "SQLite", "description": "One file"}, {"label": "Postgres", "description": "A server"}]}
EOF`

// EditedPrompt is the question question edit rewrites one to.
func (cliHooks) EditedPrompt(_ context.Context, _ *apicmd.Call, input []byte) (*pb.Prompt, error) {
	return onePrompt(input)
}

// RevisedPrompt is the question question revise rewrites one to.
func (cliHooks) RevisedPrompt(_ context.Context, _ *apicmd.Call, input []byte) (*pb.Prompt, error) {
	return onePrompt(input)
}

// onePrompt is one question: its JSON as in question add's list, or
// question add's input holding just it.
func onePrompt(input []byte) (*pb.Prompt, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(input, &keys); err != nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "read the question's JSON: %v", err).WithSuggestion(editExample)
	}
	if _, wrapped := keys["questions"]; wrapped {
		req := &pb.AddQuestionsRequest{}
		if err := protojson.Unmarshal(input, req); err != nil {
			return nil, api.Errorf(api.CodeInvalidArgs, "read the question's JSON: %v", err).WithSuggestion(editExample)
		}
		if len(req.Questions) != 1 || len(keys) != 1 {
			return nil, api.Errorf(api.CodeInvalidArgs, "a question is edited into one question, and nothing else; the JSON has %d questions", len(req.Questions)).WithSuggestion(editExample)
		}
		return req.Questions[0], nil
	}
	p := &pb.Prompt{}
	if err := protojson.Unmarshal(input, p); err != nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "read the question's JSON: %v", err).WithSuggestion(editExample)
	}
	return p, nil
}

// dirtyShown is how many lines of git status a question keeps.
const dirtyShown = 20

// askedFrom is where this agent asks from: its directory and, in a git
// checkout, the checkout's top, origin, branch, commit and changed files.
func askedFrom(ctx context.Context) (*pb.AskedFrom, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	from := &pb.AskedFrom{Dir: dir}
	g := gitops.Git{Dir: dir}
	root, err := g.Run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return from, nil // not in a checkout: the directory is all there is
	}
	from.Dir = root
	g.Dir = root
	// Each of these may well be missing: a checkout with no origin, a
	// detached HEAD, a branch with no commit yet.
	from.Remote, _ = g.Run(ctx, "remote", "get-url", "origin")
	from.Branch, _ = g.Run(ctx, "branch", "--show-current")
	from.Head, _ = g.Run(ctx, "rev-parse", "--verify", "--quiet", "HEAD")
	dirty, err := g.Lines(ctx, "status", "--porcelain=v1")
	if err != nil {
		return nil, err
	}
	from.DirtyFiles = int32(len(dirty))
	from.Dirty = dirty[:min(len(dirty), dirtyShown)]
	return from, nil
}
