package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect/v2"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/cliauth"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/setup"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/update"
	"github.com/semistrict/hotbed/internal/worker"
)

// controllerClient is in every command that calls the controller. Which
// controller is this machine's, never a flag: run sets it from deps.controller
// before the command runs.
type controllerClient struct {
	Controller string `kong:"-"`
	// RideOut waits out the controller's restarts (rideOut).
	RideOut []connect.ClientInterceptor `kong:"-"`
	// Getenv is the command's environment (deps.getenv), which can name the
	// token to call with (resolveControllerAuthToken).
	Getenv func(string) string `kong:"-"`
}

type clients struct {
	cluster    hotbedv1connect.ClusterServiceClient
	sessions   hotbedv1connect.SessionServiceClient
	workspaces hotbedv1connect.WorkspaceServiceClient
	questions  hotbedv1connect.QuestionServiceClient
	gitAuth    hotbedv1connect.GitAuthServiceClient
	board      hotbedv1connect.BoardServiceClient
}

// callController has the command call the controller at addr, riding out
// its restarts.
func (f *controllerClient) callController(addr string, d deps) {
	f.Controller, f.RideOut, f.Getenv = addr, rideOut(d, "the controller at "+addr), d.getenv
}

var defaultHotbedDir = setup.DefaultDir

// resolveControllerAuthToken returns the bearer token for controller:
// HOTBED_TOKEN if set, otherwise WorkOS credentials from ~/.hotbed/auth.json
// (auto-refreshing expiring JWTs and honoring HOTBED_ORG), otherwise legacy
// ~/.hotbed/token for HTTPS controllers when no WorkOS credentials or
// HOTBED_ORG override are active. Refresh failures fail closed and never
// silently fall back to legacy ~/.hotbed/token.
func resolveControllerAuthToken(ctx context.Context, getenv func(string) string, controller string, hc *http.Client) (string, error) {
	if tok := strings.TrimSpace(getenv("HOTBED_TOKEN")); tok != "" {
		return tok, nil
	}
	dir, err := defaultHotbedDir()
	if err != nil {
		return "", nil
	}
	targetOrg := strings.TrimSpace(getenv("HOTBED_ORG"))
	tok, err := cliauth.ResolveToken(ctx, dir, controller, targetOrg, hc)
	if err != nil {
		return "", err
	}
	if tok != "" {
		return tok, nil
	}
	if targetOrg != "" {
		return "", nil
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(controller)), "https://") {
		return "", nil
	}
	b, err := os.ReadFile(filepath.Join(dir, "token"))
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(b)), nil
}

// authorize sets on a plain HTTP request to the controller (one no RPC
// interceptor sees) what tells it who calls: this client's token for it, if
// any, and this machine's name (identify).
func (f controllerClient) authorize(ctx context.Context, d deps, h http.Header) error {
	tok, err := resolveControllerAuthToken(ctx, d.getenv, f.Controller, d.http)
	if err != nil {
		return err
	}
	identify(h, tok)
	return nil
}

// identify sets the headers that tell the controller who calls: tok as the
// bearer token (none for ""), and X-Hotbed-Host; one already set is kept.
func identify(h http.Header, tok string) {
	if tok != "" && h.Get("Authorization") == "" {
		h.Set("Authorization", "Bearer "+tok)
	}
	if host, err := os.Hostname(); err == nil && host != "" && h.Get("X-Hotbed-Host") == "" {
		h.Set("X-Hotbed-Host", host)
	}
}

// controllerAuthToken returns the bearer token for controller, failing closed
// (returning "") if WorkOS token refresh fails rather than falling back to
// legacy ~/.hotbed/token.
func controllerAuthToken(getenv func(string) string, controller string) string {
	tok, err := resolveControllerAuthToken(context.Background(), getenv, controller, nil)
	if err != nil {
		return ""
	}
	return tok
}

func controllerAuthInterceptors(getenv func(string) string, controller string, hc *http.Client) ([]connect.ClientInterceptor, error) {
	tok, err := resolveControllerAuthToken(context.Background(), getenv, controller, hc)
	if err != nil {
		return nil, err
	}
	if tok == "" {
		return nil, nil
	}
	host, _ := os.Hostname()
	return []connect.ClientInterceptor{api.AuthInterceptor(tok, "", host)}, nil
}

func (f controllerClient) interceptors(hc *http.Client) ([]connect.ClientInterceptor, error) {
	auth, err := controllerAuthInterceptors(f.Getenv, f.Controller, hc)
	if err != nil {
		return nil, err
	}
	if len(auth) == 0 {
		return f.RideOut, nil
	}
	out := make([]connect.ClientInterceptor, 0, len(auth)+len(f.RideOut))
	out = append(out, auth...)
	out = append(out, f.RideOut...)
	return out, nil
}

func (f controllerClient) clients(hc *http.Client) (clients, error) {
	ic, err := f.interceptors(hc)
	if err != nil {
		return clients{}, err
	}
	conn, err := api.Dial(f.Controller, api.DefaultPort, hc, ic...)
	if err != nil {
		return clients{}, err
	}
	return clients{
		cluster:    hotbedv1connect.NewClusterServiceClient(conn),
		sessions:   hotbedv1connect.NewSessionServiceClient(conn),
		workspaces: hotbedv1connect.NewWorkspaceServiceClient(conn),
		questions:  hotbedv1connect.NewQuestionServiceClient(conn),
		gitAuth:    hotbedv1connect.NewGitAuthServiceClient(conn),
		board:      hotbedv1connect.NewBoardServiceClient(conn),
	}, nil
}

type joinCmd struct {
	Controller     string   `arg:"" help:"The controller's HTTPS URL."`
	Dir            string   `arg:"" type:"path" help:"State directory to create, on btrfs or zfs; volumes live under it."`
	User           string   `required:"" help:"User every command runs as."`
	Token          string   `env:"HOTBED_TOKEN" help:"Bearer token for authenticating to a remote HTTPS controller."`
	ZFSDataset     string   `name:"zfs-dataset" help:"ZFS dataset for volumes, when the directory is on zfs (default <pool>/hotbed)."`
	UnitProperties []string `name:"unit-property" help:"systemd property every command gets, such as AmbientCapabilities=CAP_SYS_ADMIN or DeviceAllow=/dev/kvm. Repeatable."`
	Slice          string   `help:"systemd slice commands run in (default hotbed.slice). user-hotbed.slice puts them in user.slice, sharing its memory limit; hotbed admits and parks against the fullest limit above them."`
}

func (c *joinCmd) Exec(ctx context.Context, d deps) (any, error) {
	token := strings.TrimSpace(c.Token)
	if token == "" {
		token = controllerAuthToken(d.getenv, c.Controller)
	}
	return worker.Join(ctx, worker.JoinOptions{
		Dir: c.Dir, Controller: c.Controller, User: c.User, Token: token, Dataset: c.ZFSDataset, UnitProperties: c.UnitProperties, Slice: c.Slice,
	}, d.http, d.check)
}

type serveCmd struct {
	Dir string `arg:"" type:"path" help:"Worker state directory made by join."`
}

type serveResult struct {
	Role    string `json:"role"`
	Restart bool   `json:"restart,omitempty"` // it installed a new build, to be started as
}

func (c *serveCmd) Exec(ctx context.Context, d deps) (any, error) {
	cfg, err := statedir.Load(c.Dir)
	if err != nil {
		return nil, err
	}
	if cfg.Worker == nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "the standalone Go controller has been retired").
			WithSuggestion("deploy cf-controller on Cloudflare Workers instead")
	}
	// Where the binary is now: a new build replaces the file there.
	exe, err := d.exe()
	if err != nil {
		return nil, err
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	res := serveResult{Role: "worker"}
	err = worker.Run(ctx, c.Dir, *cfg.Worker, d.http)
	return afterServe(res, err, exe, os.Args, d.execve)
}

// afterServe starts the new build a node installed over itself (err is
// update.ErrRestart) in the node's place, with the same arguments: whatever
// runs the node may restart it only when it fails (a unit's
// Restart=on-failure), and a node that exited cleanly would stay down. It
// returns only when that fails.
func afterServe(res serveResult, err error, exe string, argv []string, execve func(string, []string, []string) error) (serveResult, error) {
	if !errors.Is(err, update.ErrRestart) {
		return res, err
	}
	if err := execve(exe, argv, os.Environ()); err != nil {
		return res, fmt.Errorf("start the new build %s: %w", exe, err)
	}
	res.Restart = true
	return res, nil
}
