package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/tailnet"
)

const dbName = "hotbed.db"

// Tailnet is what the controller needs from this machine's tailscaled.
type Tailnet interface {
	Identifier
	Self(ctx context.Context) (tailnet.Self, error)
}

// InitResult describes a new cluster.
type InitResult struct {
	ClusterID string   `json:"cluster_id"`
	Dir       string   `json:"dir"`
	Owner     string   `json:"owner"`
	Address   string   `json:"address"` // what workers pass to hotbed join
	Next      []string `json:"next"`
}

// Init makes dir the state directory of a new cluster whose controller is
// this machine and whose owner is the owner of this machine's tailnet node.
func Init(ctx context.Context, dir string, port int, tn Tailnet) (InitResult, error) {
	if port <= 0 || port > 65535 {
		return InitResult{}, api.Errorf(api.CodeInvalidArgs, "port %d is out of range", port)
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return InitResult{}, err
	}
	self, err := tn.Self(ctx)
	if err != nil {
		return InitResult{}, err
	}
	if err := statedir.Prepare(dir); err != nil {
		return InitResult{}, err
	}
	// Creating the database first and the config last means a failure leaves
	// no config, and running init again opens the same database.
	store, err := OpenStore(ctx, filepath.Join(dir, dbName))
	if err != nil {
		return InitResult{}, err
	}
	if err := store.Close(); err != nil {
		return InitResult{}, err
	}
	cluster := statedir.ControllerConfig{ClusterID: newClusterID(), Owner: self.Login, Port: port}
	if err := statedir.Commit(dir, statedir.Config{Controller: &cluster}); err != nil {
		return InitResult{}, err
	}
	address := self.Hostname
	if port != api.DefaultPort {
		address = net.JoinHostPort(self.Hostname, strconv.Itoa(port))
	}
	return InitResult{
		ClusterID: cluster.ClusterID,
		Dir:       dir,
		Owner:     self.Login,
		Address:   address,
		Next: []string{
			"on this machine: hotbed run " + dir,
			"on each worker: hotbed join " + address + " DIR, then hotbed run DIR",
		},
	}, nil
}

func newClusterID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Run serves the cluster in dir until ctx ends.
func Run(ctx context.Context, dir string, cluster statedir.ControllerConfig, tn Tailnet) error {
	c, err := Listen(ctx, dir, cluster, tn)
	if err != nil {
		return err
	}
	return c.Serve(ctx)
}

// Controller is a controller that is listening but not yet serving.
type Controller struct {
	store     *Store
	srv       *http.Server
	listeners []net.Listener
}

// Listen opens the cluster's database and listens on this machine's tailnet
// addresses, and only those.
func Listen(ctx context.Context, dir string, cluster statedir.ControllerConfig, tn Tailnet) (*Controller, error) {
	self, err := tn.Self(ctx)
	if err != nil {
		return nil, err
	}
	store, err := OpenStore(ctx, filepath.Join(dir, dbName))
	if err != nil {
		return nil, err
	}
	c := &Controller{
		store: store,
		srv: &http.Server{
			Handler:           NewServer(ServerConfig{Store: store, Identifier: tn, Cluster: cluster}).Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		},
	}
	for _, addr := range self.Addrs {
		l, err := net.Listen("tcp", net.JoinHostPort(addr.String(), strconv.Itoa(cluster.Port)))
		if err != nil {
			c.close()
			return nil, fmt.Errorf("listen on tailnet address: %w", err)
		}
		slog.Info("controller listening", "addr", l.Addr().String(), "cluster", cluster.ClusterID)
		c.listeners = append(c.listeners, l)
	}
	return c, nil
}

// Addrs returns the addresses the controller listens on.
func (c *Controller) Addrs() []net.Addr {
	addrs := make([]net.Addr, len(c.listeners))
	for i, l := range c.listeners {
		addrs[i] = l.Addr()
	}
	return addrs
}

// Serve serves the API until ctx ends, then lets requests in flight finish.
func (c *Controller) Serve(ctx context.Context) error {
	defer c.store.Close()
	g, gctx := errgroup.WithContext(ctx)
	for _, l := range c.listeners {
		g.Go(func() error {
			if err := c.srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		})
	}
	g.Go(func() error {
		<-gctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return c.srv.Shutdown(shutdownCtx)
	})
	return g.Wait()
}

func (c *Controller) close() {
	for _, l := range c.listeners {
		l.Close()
	}
	if err := c.store.Close(); err != nil {
		slog.Warn("close database", "err", err)
	}
}
