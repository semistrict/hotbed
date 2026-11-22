package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/user"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/controller"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/update"
	"github.com/semistrict/hotbed/internal/worker"
)

// controllerClient is in every command that calls the controller. Which
// controller is this machine's, never a flag: run sets it from deps.controller
// before the command runs.
type controllerClient struct {
	Controller string `kong:"-"`
}

type clients struct {
	cluster    hotbedv1connect.ClusterServiceClient
	sessions   hotbedv1connect.SessionServiceClient
	workspaces hotbedv1connect.WorkspaceServiceClient
	schedules  hotbedv1connect.ScheduleServiceClient
	questions  hotbedv1connect.QuestionServiceClient
	reviews    hotbedv1connect.ReviewServiceClient
}

// callController has the command call the controller at addr.
func (f *controllerClient) callController(addr string) { f.Controller = addr }

func (f controllerClient) clients(hc *http.Client) (clients, error) {
	conn, err := api.Dial(f.Controller, api.DefaultPort, hc)
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
	Controller     string   `arg:"" help:"The controller's tailnet name or address, with :PORT unless it is 7120."`
	Dir            string   `arg:"" type:"path" help:"State directory to create, on btrfs or zfs; volumes live under it."`
	User           string   `required:"" help:"User every command runs as."`
	ZFSDataset     string   `name:"zfs-dataset" help:"ZFS dataset for volumes, when the directory is on zfs (default <pool>/hotbed)."`
	UnitProperties []string `name:"unit-property" help:"systemd property every command gets, such as AmbientCapabilities=CAP_SYS_ADMIN or DeviceAllow=/dev/kvm. Repeatable."`
	TmpDir         string   `name:"tmp-dir" help:"Directory each command's TMPDIR is made in, a directory of its own removed when it ends (default the state directory's tmp), such as a scratch disk."`
	Slice          string   `help:"systemd slice commands run in (default hotbed.slice). user-hotbed.slice puts them in user.slice, sharing its memory limit; hotbed admits and parks against the fullest limit above them."`
}

func (c *joinCmd) Exec(ctx context.Context, d deps) (any, error) {
	return worker.Join(ctx, worker.JoinOptions{
		Dir: c.Dir, Controller: c.Controller, User: c.User, Dataset: c.ZFSDataset, UnitProperties: c.UnitProperties, Slice: c.Slice, TmpDir: c.TmpDir,
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

type nodesCmd struct {
	controllerClient
}

func (c *nodesCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.cluster.ListNodes(ctx, &pb.ListNodesRequest{})
}

type snapshotCmd struct {
	controllerClient
}

func (c *snapshotCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.cluster.GetSnapshot(ctx, &pb.GetSnapshotRequest{})
}

type repoCmd struct {
	Add     repoAddCmd     `cmd:"" help:"Add or update a repo; give it setup steps with hotbed step set."`
	Ls      repoLsCmd      `cmd:"" help:"List repos."`
	Rm      repoRmCmd      `cmd:"" help:"Remove a repo; workers delete its bases once no workspace uses them."`
	Rebuild repoRebuildCmd `cmd:"" help:"Build the repo's next base from nothing on every worker. Bases are otherwise built incrementally: pull, then the steps that need to run."`
}

type repoRebuildCmd struct {
	controllerClient
	repoFlag
}

func (c *repoRebuildCmd) Exec(ctx context.Context, d deps) (any, error) {
	id, err := c.resolveRepo(ctx, d, "", c.Repo, "")
	if err != nil {
		return nil, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.cluster.RebuildRepo(ctx, &pb.RebuildRepoRequest{Id: id})
}

type repoAddCmd struct {
	controllerClient
	ID     string   `arg:"" help:"Short name, such as ratel; workspaces see the checkout at /work/<name>."`
	URL    string   `arg:"" help:"What git clones."`
	Branch string   `help:"Branch bases follow (default: the remote's default branch)."`
	Host   []string `help:"Node (host name) that keeps bases; default every worker. Repeatable."`
}

func (c *repoAddCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.cluster.PutRepo(ctx, &pb.PutRepoRequest{Repo: &pb.Repo{Id: c.ID, Url: c.URL, Branch: c.Branch, Hosts: c.Host}})
}

type repoLsCmd struct {
	controllerClient
}

func (c *repoLsCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.cluster.ListRepos(ctx, &pb.ListReposRequest{})
}

type repoRmCmd struct {
	controllerClient
	ID string `arg:"" help:"The repo's name."`
}

func (c *repoRmCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.cluster.DeleteRepo(ctx, &pb.DeleteRepoRequest{Id: c.ID})
}
