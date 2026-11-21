package main

import (
	"context"
	"net/http"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/controller"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/worker"
)

// controllerFlag is the flag every client command takes.
type controllerFlag struct {
	Controller string `required:"" env:"HOTBED_CONTROLLER" help:"The controller's tailnet name or address (or set HOTBED_CONTROLLER)."`
}

type clients struct {
	cluster    hotbedv1connect.ClusterServiceClient
	sessions   hotbedv1connect.SessionServiceClient
	workspaces hotbedv1connect.WorkspaceServiceClient
}

func (f controllerFlag) clients(hc *http.Client) (clients, error) {
	conn, err := api.Dial(f.Controller, api.DefaultPort, hc)
	if err != nil {
		return clients{}, err
	}
	return clients{
		cluster:    hotbedv1connect.NewClusterServiceClient(conn),
		sessions:   hotbedv1connect.NewSessionServiceClient(conn),
		workspaces: hotbedv1connect.NewWorkspaceServiceClient(conn),
	}, nil
}

type initCmd struct {
	Dir  string `arg:"" type:"path" help:"State directory to create; holds the cluster's database."`
	Port int    `default:"7120" help:"Port to serve on, on this machine's tailnet addresses."`
}

func (c *initCmd) Exec(ctx context.Context, d deps) (any, error) {
	return controller.Init(ctx, c.Dir, c.Port, d.tailnet)
}

type joinCmd struct {
	Controller     string   `arg:"" help:"The controller's tailnet name or address, with :PORT unless it is 7120."`
	Dir            string   `arg:"" type:"path" help:"State directory to create, on btrfs or zfs; volumes live under it."`
	User           string   `required:"" help:"User every command runs as."`
	ZFSDataset     string   `name:"zfs-dataset" help:"ZFS dataset for volumes, when the directory is on zfs (default <pool>/hotbed)."`
	UnitProperties []string `name:"unit-property" help:"systemd property every command gets, such as AmbientCapabilities=CAP_SYS_ADMIN or DeviceAllow=/dev/kvm. Repeatable."`
}

func (c *joinCmd) Exec(ctx context.Context, d deps) (any, error) {
	return worker.Join(ctx, worker.JoinOptions{
		Dir: c.Dir, Controller: c.Controller, User: c.User, Dataset: c.ZFSDataset, UnitProperties: c.UnitProperties,
	}, d.http, d.check)
}

type serveCmd struct {
	Dir string `arg:"" type:"path" help:"State directory made by init or join."`
}

type serveResult struct {
	Role string `json:"role"`
}

func (c *serveCmd) Exec(ctx context.Context, d deps) (any, error) {
	cfg, err := statedir.Load(c.Dir)
	if err != nil {
		return nil, err
	}
	if cfg.Controller != nil {
		return serveResult{Role: "controller"}, controller.Run(ctx, c.Dir, *cfg.Controller, d.tailnet)
	}
	return serveResult{Role: "worker"}, worker.Run(ctx, c.Dir, *cfg.Worker, d.tailnet, d.http)
}

type nodesCmd struct {
	controllerFlag
}

func (c *nodesCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.cluster.ListNodes(ctx, &pb.ListNodesRequest{})
}

type repoCmd struct {
	Add repoAddCmd `cmd:"" help:"Add or update a repo; workers start keeping built bases of it."`
	Ls  repoLsCmd  `cmd:"" help:"List repos."`
	Rm  repoRmCmd  `cmd:"" help:"Remove a repo; workers stop keeping its bases."`
}

type repoAddCmd struct {
	controllerFlag
	ID      string   `arg:"" help:"Short name, such as ratel; workspaces see the checkout at /work/<name>."`
	URL     string   `arg:"" help:"What git clones."`
	Branch  string   `help:"Branch bases follow (default: the remote's default branch)."`
	Freshen []string `help:"Command that brings a checkout up to date after a pull, such as pnpm install or cargo build --all-targets; bases run them in order. Repeatable."`
	Host    []string `help:"Node (host name) that keeps bases; default every worker. Repeatable."`
}

func (c *repoAddCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.cluster.PutRepo(ctx, &pb.PutRepoRequest{Repo: &pb.Repo{Id: c.ID, Url: c.URL, Branch: c.Branch, Freshen: c.Freshen, Hosts: c.Host}})
}

type repoLsCmd struct {
	controllerFlag
}

func (c *repoLsCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.cluster.ListRepos(ctx, &pb.ListReposRequest{})
}

type repoRmCmd struct {
	controllerFlag
	ID string `arg:"" help:"The repo's name."`
}

func (c *repoRmCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.cluster.DeleteRepo(ctx, &pb.DeleteRepoRequest{Id: c.ID})
}
