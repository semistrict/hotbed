package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"connectrpc.com/connect/v2"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/controller"
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
}

type clients struct {
	cluster    hotbedv1connect.ClusterServiceClient
	sessions   hotbedv1connect.SessionServiceClient
	workspaces hotbedv1connect.WorkspaceServiceClient
	schedules  hotbedv1connect.ScheduleServiceClient
	questions  hotbedv1connect.QuestionServiceClient
	reviews    hotbedv1connect.ReviewServiceClient
	gitAuth    hotbedv1connect.GitAuthServiceClient
}

// callController has the command call the controller at addr, riding out
// its restarts.
func (f *controllerClient) callController(addr string, d deps) {
	f.Controller, f.RideOut = addr, rideOut(d, "the controller at "+addr)
}

var defaultHotbedDir = setup.DefaultDir

// controllerAuthToken returns the bearer token for controller: HOTBED_TOKEN if
// set, otherwise ~/.hotbed/token when controller is an HTTPS URL.
func controllerAuthToken(controller string) string {
	if tok := strings.TrimSpace(os.Getenv("HOTBED_TOKEN")); tok != "" {
		return tok
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(controller)), "https://") {
		return ""
	}
	dir, err := defaultHotbedDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, "token"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func controllerAuthInterceptors(controller string) []connect.ClientInterceptor {
	tok := controllerAuthToken(controller)
	if tok == "" {
		return nil
	}
	host, _ := os.Hostname()
	return []connect.ClientInterceptor{api.AuthInterceptor(tok, "", host, "")}
}

func (f controllerClient) interceptors() []connect.ClientInterceptor {
	auth := controllerAuthInterceptors(f.Controller)
	if len(auth) == 0 {
		return f.RideOut
	}
	out := make([]connect.ClientInterceptor, 0, len(auth)+len(f.RideOut))
	out = append(out, auth...)
	out = append(out, f.RideOut...)
	return out
}

func (f controllerClient) clients(hc *http.Client) (clients, error) {
	conn, err := api.Dial(f.Controller, api.DefaultPort, hc, f.interceptors()...)
	if err != nil {
		return clients{}, err
	}
	return clients{
		cluster:    hotbedv1connect.NewClusterServiceClient(conn),
		sessions:   hotbedv1connect.NewSessionServiceClient(conn),
		workspaces: hotbedv1connect.NewWorkspaceServiceClient(conn),
		schedules:  hotbedv1connect.NewScheduleServiceClient(conn),
		questions:  hotbedv1connect.NewQuestionServiceClient(conn),
		reviews:    hotbedv1connect.NewReviewServiceClient(conn),
		gitAuth:    hotbedv1connect.NewGitAuthServiceClient(conn),
	}, nil
}

type initCmd struct {
	Dir       string `arg:"" type:"path" help:"State directory to create; holds the cluster's database."`
	Port      int    `default:"7120" help:"Port to serve on, on this machine's tailnet addresses."`
	LocalUser string `help:"Unix user, besides root, who may call the controller from this machine (default: whoever ran sudo, or you). Other users on it are refused."`
}

func (c *initCmd) Exec(ctx context.Context, d deps) (any, error) {
	local := c.LocalUser
	if local == "" {
		local = os.Getenv("SUDO_USER")
	}
	if local == "" {
		if u, err := user.Current(); err == nil && u.Uid != "0" {
			local = u.Username
		}
	}
	return controller.Init(ctx, c.Dir, c.Port, local, d.tailnet)
}

type joinCmd struct {
	Controller     string   `arg:"" help:"The controller's tailnet name or address (with :PORT unless 7120), or an HTTPS controller URL."`
	Dir            string   `arg:"" type:"path" help:"State directory to create, on btrfs or zfs; volumes live under it."`
	User           string   `required:"" help:"User every command runs as."`
	Token          string   `env:"HOTBED_TOKEN" help:"Bearer token for authenticating to a remote HTTPS controller."`
	ZFSDataset     string   `name:"zfs-dataset" help:"ZFS dataset for volumes, when the directory is on zfs (default <pool>/hotbed)."`
	UnitProperties []string `name:"unit-property" help:"systemd property every command gets, such as AmbientCapabilities=CAP_SYS_ADMIN or DeviceAllow=/dev/kvm. Repeatable."`
	Slice          string   `help:"systemd slice commands run in (default hotbed.slice). user-hotbed.slice puts them in user.slice, sharing its memory limit; hotbed admits and parks against the fullest limit above them."`
}

func (c *joinCmd) Exec(ctx context.Context, d deps) (any, error) {
	return worker.Join(ctx, worker.JoinOptions{
		Dir: c.Dir, Controller: c.Controller, User: c.User, Token: c.Token, Dataset: c.ZFSDataset, UnitProperties: c.UnitProperties, Slice: c.Slice,
	}, d.tailnet, d.http, d.check)
}

type serveCmd struct {
	Dir string `arg:"" type:"path" help:"State directory made by init or join."`
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
	// Where the binary is now: a new build replaces the file there.
	exe, err := d.exe()
	if err != nil {
		return nil, err
	}
	res := serveResult{Role: "worker"}
	if cfg.Controller != nil {
		res.Role = "controller"
		err = controller.Run(ctx, c.Dir, *cfg.Controller, d.tailnet)
	} else {
		err = worker.Run(ctx, c.Dir, *cfg.Worker, d.tailnet, d.http)
	}
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
