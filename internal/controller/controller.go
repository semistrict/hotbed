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
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/httpx"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/tailnet"
	"github.com/semistrict/hotbed/internal/update"
	"github.com/semistrict/hotbed/internal/version"
	"github.com/semistrict/hotbed/ui"
)

const dbName = "hotbed.db"

// Tailnet is what the controller needs from this machine's tailscaled.
type Tailnet interface {
	httpx.Identifier
	Self(ctx context.Context) (tailnet.Self, error)
}

// InitResult describes a new cluster.
type InitResult struct {
	ClusterID string   `json:"clusterId"`
	Dir       string   `json:"dir"`
	Owner     string   `json:"owner"`
	Address   string   `json:"address"` // what workers pass to hotbed join
	Next      []string `json:"next"`
}

// Init makes dir the state directory of a new cluster whose controller is
// this machine and whose owner is the owner of this machine's tailnet node.
// localUser, when set, is the unix user besides root who may call the
// controller from this machine.
func Init(ctx context.Context, dir string, port int, localUser string, tn Tailnet) (InitResult, error) {
	if port <= 0 || port > 65535 {
		return InitResult{}, api.Errorf(api.CodeInvalidArgs, "port %d is out of range", port)
	}
	if localUser != "" {
		if _, err := lookupUID(localUser); err != nil {
			return InitResult{}, err
		}
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
	cluster := statedir.ControllerConfig{ClusterID: newClusterID(), Owner: self.Login, Port: port, LocalUser: localUser}
	if err := statedir.Commit(dir, statedir.Config{Controller: &cluster}); err != nil {
		return InitResult{}, err
	}
	host := self.Hostname
	if self.DNSName != "" {
		host = self.DNSName
	}
	address := host
	if port != api.DefaultPort {
		address = net.JoinHostPort(host, strconv.Itoa(port))
	}
	return InitResult{
		ClusterID: cluster.ClusterID,
		Dir:       dir,
		Owner:     self.Login,
		Address:   address,
		Next: []string{
			"on this machine: hotbed serve " + dir,
			"on each worker: hotbed join " + address + " DIR, then sudo hotbed serve DIR",
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
	follow    *Follower
	api       *Server
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
	// Serve nothing unless this machine is the owner's: a machine logged into
	// the tailnet as someone else would admit the owner on that someone's
	// say-so.
	if self.Login != cluster.Owner {
		store.Close()
		return nil, api.Errorf(api.CodeNotAuthorized, "this machine is on the tailnet as %s, but the cluster belongs to %s: refusing to serve", self.Login, cluster.Owner).
			WithSuggestion("log this machine into the owner's tailnet account (tailscale up)")
	}
	var local []uint32
	if cluster.LocalUser != "" {
		uid, err := lookupUID(cluster.LocalUser)
		if err != nil {
			store.Close()
			return nil, err
		}
		local = append(local, uid)
	}
	builds := &update.Cache{Dir: filepath.Join(dir, "builds")}
	var follow *Follower
	if u := cluster.Updates; u != nil {
		token, err := os.ReadFile(u.TokenFile)
		if err != nil {
			store.Close()
			return nil, api.Errorf(api.CodeInvalidArgs, "the controller follows CI's builds of %s, but can't read its GitHub token: %v", u.Repo, err).
				WithSuggestion("write a token that may read the repo's releases to " + u.TokenFile + ", or remove updates from the config")
		}
		exe, err := os.Executable()
		if err != nil {
			store.Close()
			return nil, err
		}
		follow = &Follower{
			GitHub: &update.GitHub{API: "https://api.github.com", Repo: u.Repo, Token: strings.TrimSpace(string(token)), Client: &http.Client{Timeout: 5 * time.Minute}},
			Cache:  *builds, Exe: exe, Build: version.Number(),
		}
	}
	server := NewServer(ServerConfig{
		Store: store, Identifier: tn, Cluster: cluster, UI: ui.Handler(), Controller: self.Hostname,
		Self: self.Addrs, LocalUsers: local, Builds: builds, Build: version.Number(), Follower: follow,
	})
	c := &Controller{
		store:  store,
		follow: follow,
		api:    server,
		srv: &http.Server{
			Handler:           server.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
			Protocols:         httpx.Protocols(),
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
		c.api.Reap(gctx)
		return nil
	})
	g.Go(func() error {
		c.api.RunSchedules(gctx)
		return nil
	})
	if c.follow != nil {
		// Its update.ErrRestart ends the others, and Serve with it.
		g.Go(func() error { return c.follow.Run(gctx) })
	}
	g.Go(func() error {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			if err := c.store.Prune(gctx, time.Now()); err != nil {
				slog.Error("prune old samples and history", "err", err)
			}
			select {
			case <-gctx.Done():
				return nil
			case <-t.C:
			}
		}
	})
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

// lookupUID is a unix user's ID.
func lookupUID(name string) (uint32, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, api.Errorf(api.CodeInvalidArgs, "no unix user %q: %v", name, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("user %s has uid %q", name, u.Uid)
	}
	return uint32(uid), nil
}
