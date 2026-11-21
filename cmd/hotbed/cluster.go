package main

import (
	"context"
	"net/http"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/controller"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/worker"
)

// controllerFlag is the flag every client command takes.
type controllerFlag struct {
	Controller string `required:"" env:"HOTBED_CONTROLLER" help:"The controller's tailnet name or address (or set HOTBED_CONTROLLER)."`
}

func (f controllerFlag) client(hc *http.Client) (*api.Client, error) {
	return api.NewClient(f.Controller, hc)
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
	return runResult{Role: "worker"}, worker.Run(ctx, c.Dir, *cfg.Worker, d.tailnet, d.http)
}

type nodesCmd struct {
	controllerFlag
}

func (c *nodesCmd) Exec(ctx context.Context, d deps) (any, error) {
	client, err := c.client(d.http)
	if err != nil {
		return nil, err
	}
	return client.Nodes(ctx)
}

type repoCmd struct {
	Add repoAddCmd `cmd:"" help:"Add or update a repo; workers start keeping warm bases of it."`
	Ls  repoLsCmd  `cmd:"" help:"List repos."`
	Rm  repoRmCmd  `cmd:"" help:"Remove a repo; workers stop keeping its bases."`
}

type repoAddCmd struct {
	controllerFlag
	ID      string   `arg:"" help:"Short name, such as ratel; workspaces mount the checkout at /work/<name>."`
	URL     string   `arg:"" help:"What git clones."`
	Branch  string   `help:"Branch bases follow (default: the remote's default branch)."`
	Freshen []string `help:"Command that brings a checkout up to date, run in it by every base, in order: pnpm install, cargo build --all-targets. Repeatable."`
	Host    []string `help:"Node (host name) that keeps bases; default every worker. Repeatable."`
}

func (c *repoAddCmd) Exec(ctx context.Context, d deps) (any, error) {
	client, err := c.client(d.http)
	if err != nil {
		return nil, err
	}
	var out api.Repo
	err = client.Do(ctx, http.MethodPost, "/v1/repos", api.Repo{ID: c.ID, URL: c.URL, Branch: c.Branch, Freshen: c.Freshen, Hosts: c.Host}, &out)
	return out, err
}

type repoLsCmd struct {
	controllerFlag
}

func (c *repoLsCmd) Exec(ctx context.Context, d deps) (any, error) {
	client, err := c.client(d.http)
	if err != nil {
		return nil, err
	}
	var out []api.Repo
	err = client.Do(ctx, http.MethodGet, "/v1/repos", nil, &out)
	return map[string]any{"repos": out}, err
}

type repoRmCmd struct {
	controllerFlag
	ID string `arg:"" help:"The repo's name."`
}

func (c *repoRmCmd) Exec(ctx context.Context, d deps) (any, error) {
	client, err := c.client(d.http)
	if err != nil {
		return nil, err
	}
	return map[string]string{"removed": c.ID}, client.Do(ctx, http.MethodDelete, "/v1/repos/"+c.ID, nil, nil)
}
