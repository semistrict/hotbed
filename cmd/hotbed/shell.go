package main

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

type shellCmd struct {
	controllerClient
	sessionFlag
	repoFlag
	Args []string `arg:"" optional:"" passthrough:"" help:"Command to run in the shell, after -- (omit for an interactive shell)."`
	Sync bool     `help:"Sync the checkout this is run in to the workspace before opening the shell (default: open the workspace as it is)."`
	Env  []string `help:"KEY=VALUE for the shell. Repeatable."`
	placementFlags
}

func (c *shellCmd) SetDashArgs(args []string) (before []string) {
	before, c.Args = c.Args, args
	return before
}

func (c *shellCmd) Exec(context.Context, deps) (any, error) { return nil, errors.New("shell is raw") }

// Raw connects to an existing workspace of the session (preferring a ready
// workspace over load-based migration to another node, creating one only if
// the session has none matching --node/--arch), optionally syncs the current
// checkout when --sync is set, requests a one-time SSH ticket, and runs ssh.
func (c *shellCmd) Raw(ctx context.Context, d deps) (int, bool, error) {
	if _, err := exec.LookPath("ssh"); err != nil {
		return 0, false, api.Errorf(api.CodePlatformNotSupported, "ssh binary not found on PATH: %v", err).
			WithSuggestion("install an OpenSSH client (ssh) to use `hotbed shell`")
	}
	args := c.Args
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	for _, kv := range c.Env {
		if !strings.Contains(kv, "=") {
			return 0, false, api.Errorf(api.CodeInvalidArgs, "invalid --env %q: want KEY=VALUE", kv)
		}
	}

	session, err := c.sessionFlag.resolve(ctx, d)
	if err != nil {
		return 0, false, err
	}
	repo, err := c.shellRepo(ctx, d, session)
	if err != nil {
		return 0, false, err
	}
	var dir string
	if c.Sync {
		if repo == "" {
			return 0, false, api.Errorf(api.CodeInvalidArgs, "--sync requires a checkout of one of session %s's repos", session).
				WithSuggestion("run from a checkout of the repo, or drop --sync")
		}
		if dir, err = c.checkoutOf(ctx, d, session, c.Repo, repo); err != nil {
			return 0, false, err
		}
		if dir == "" {
			return 0, false, api.Errorf(api.CodeInvalidArgs, "this is no checkout of %s", repo).
				WithSuggestion("run from a checkout of " + repo + ", or drop --sync")
		}
	}

	ws, w, err := c.workspaceForShell(ctx, d, session, repo, dir)
	if err != nil {
		return 0, false, err
	}
	if ws, err = waitReady(ctx, w, ws, d.stderr); err != nil {
		return 0, false, err
	}
	// If no repo was named or matched by CWD and the workspace holds a single
	// repo, start directly inside /work/<repo>.
	if repo == "" && len(ws.Repos) == 1 {
		repo = ws.Repos[0].Repo
	}
	if dir != "" {
		res, err := syncTo(ctx, w, ws, repo, dir)
		if err != nil {
			return 0, false, err
		}
		if !res.Skipped {
			fmt.Fprintf(d.stderr, "hotbed: synced %d changed files (%s) from %s\n", res.FilesChanged, size(res.BundleBytes), dir)
		}
	}

	pubKey, privPEM, err := ephemeralSSHKey()
	if err != nil {
		return 0, false, err
	}
	resp, err := w.calls.OpenShell(ctx, &pb.OpenShellRequest{
		WorkspaceId: ws.Id,
		Repo:        repo,
		Args:        args,
		Env:         c.Env,
		PublicKey:   pubKey,
	})
	if err != nil {
		return 0, false, err
	}
	host, port, err := net.SplitHostPort(resp.Addr)
	if err != nil {
		return 0, false, api.Errorf(api.CodeInternal, "worker returned invalid SSH address %q: %v", resp.Addr, err)
	}
	if strings.TrimSpace(resp.HostPublicKey) == "" {
		return 0, false, api.Errorf(api.CodeInternal, "worker returned empty SSH host public key")
	}

	tmpDir, err := os.MkdirTemp("", "hotbed-ssh-*")
	if err != nil {
		return 0, false, err
	}
	defer os.RemoveAll(tmpDir)

	keyPath := filepath.Join(tmpDir, "id_ed25519")
	if err := os.WriteFile(keyPath, privPEM, 0o600); err != nil {
		return 0, false, err
	}
	knownHostsPath := filepath.Join(tmpDir, "known_hosts")
	knownEntry := fmt.Sprintf("[%s]:%s %s\n", host, port, strings.TrimSpace(resp.HostPublicKey))
	if port == "22" {
		knownEntry = fmt.Sprintf("%s %s\n", host, strings.TrimSpace(resp.HostPublicKey))
	}
	if err := os.WriteFile(knownHostsPath, []byte(knownEntry), 0o600); err != nil {
		return 0, false, err
	}

	startDir := "/work"
	if repo != "" {
		startDir += "/" + repo
	}
	fmt.Fprintf(d.stderr, "hotbed: shell in %s on %s (%s)\n", ws.Id, ws.Node, startDir)

	sshArgs := []string{
		"-F", "/dev/null",
		"-i", keyPath,
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + knownHostsPath,
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-o", "LogLevel=ERROR",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-o", "SendEnv=COLORTERM",
		"-o", "SendEnv=LANG",
		"-o", "SendEnv=LC_*",
		"-p", port,
	}
	if isTerminal(d.stdin) {
		sshArgs = append(sshArgs, "-t")
	} else {
		sshArgs = append(sshArgs, "-T")
	}
	sshArgs = append(sshArgs, resp.User+"@"+host)

	cmd := exec.CommandContext(ctx, "ssh", sshArgs...)
	cmd.Stdin = d.stdin
	cmd.Stdout = d.stdout
	cmd.Stderr = d.stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), true, nil
		}
		if ctx.Err() != nil {
			return exitInterrupted, true, nil
		}
		return 0, true, err
	}
	return exitOK, true, nil
}

// workspaceForShell prefers an existing live workspace of session matching
// --node/--arch (preferring ready workspaces over preparing ones, and the most
// recently active when several exist) so shelling into a session never moves
// you to a different idle node just because the session's node is busy. It
// falls back to workspaceFor only when no matching existing workspace exists.
func (c *shellCmd) workspaceForShell(ctx context.Context, d deps, session, repo, dir string) (*pb.Workspace, workerClients, error) {
	if c.Arch == "" {
		if ws, w, ok, err := c.existingWorkspace(ctx, d, session, c.Node); err != nil {
			return nil, workerClients{}, err
		} else if ok {
			return ws, w, nil
		}
	}
	return c.workspaceFor(ctx, d, &pb.CreateWorkspaceRequest{SessionId: session, Repo: repo, Placement: &pb.Placement{Node: c.Node, Arch: c.Arch, Runtime: c.Runtime}, SyncDir: dir})
}

func (c *shellCmd) existingWorkspace(ctx context.Context, d deps, session, node string) (*pb.Workspace, workerClients, bool, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, workerClients{}, false, err
	}
	list, err := cl.workspaces.ListWorkspaces(ctx, &pb.ListWorkspacesRequest{SessionId: session})
	if err != nil {
		return nil, workerClients{}, false, err
	}
	var matches []*pb.Workspace
	for _, ws := range ownWorkspaces(list.Workspaces) {
		if ws.State == pb.WorkspaceState_WORKSPACE_STATE_RELEASED || ws.State == pb.WorkspaceState_WORKSPACE_STATE_FAILED {
			continue
		}
		if node != "" && ws.Node != node {
			continue
		}
		matches = append(matches, ws)
	}
	if len(matches) == 0 {
		return nil, workerClients{}, false, nil
	}
	slices.SortFunc(matches, func(a, b *pb.Workspace) int {
		// Ready workspaces (ACTIVE/IDLE) sort before PREPARING ones; ties break
		// by most recently active, then newest created.
		if p := cmp.Compare(shellWorkspaceRank(a.State), shellWorkspaceRank(b.State)); p != 0 {
			return p
		}
		if p := cmp.Compare(b.LastActiveAt, a.LastActiveAt); p != 0 {
			return p
		}
		return cmp.Compare(b.CreatedAt, a.CreatedAt)
	})
	chosen := matches[0]
	ws, w, err := c.onWorker(ctx, d, chosen)
	if err != nil {
		return nil, workerClients{}, false, err
	}
	remember(d, session, ws)
	return ws, w, true, nil
}

func shellWorkspaceRank(s pb.WorkspaceState) int {
	switch s {
	case pb.WorkspaceState_WORKSPACE_STATE_ACTIVE, pb.WorkspaceState_WORKSPACE_STATE_IDLE:
		return 0
	case pb.WorkspaceState_WORKSPACE_STATE_PREPARING:
		return 1
	default:
		return 2
	}
}

// shellRepo resolves the starting repo for hotbed shell. An explicit -r flag
// is validated strictly; when omitted, an unrelated local checkout (e.g.
// shelling into another project's session from a different repo directory) is
// ignored rather than failing.
func (c *shellCmd) shellRepo(ctx context.Context, d deps, session string) (string, error) {
	if c.Repo != "" {
		return c.resolveRepo(ctx, d, session, c.Repo, "")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if _, err := gitCheckoutRoot(ctx, cwd); err != nil {
		return "", nil
	}
	repo, err := c.resolveRepo(ctx, d, session, "", "")
	if err != nil {
		if api.CodeOf(err) == api.CodeInvalidArgs {
			return "", nil
		}
		return "", err
	}
	return repo, nil
}

func ephemeralSSHKey() (pubAuthorized string, privPEM []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, fmt.Errorf("generate ephemeral ssh key: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", nil, fmt.Errorf("encode ssh public key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return "", nil, fmt.Errorf("encode ssh private key: %w", err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))), pem.EncodeToMemory(block), nil
}

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
