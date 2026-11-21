package controller

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect/v2/connecthttp"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/httpx"
	"github.com/semistrict/hotbed/internal/statedir"
)

const (
	// A session nothing touches for this long is abandoned: its commands
	// stop and its workspaces go.
	sessionIdle = time.Hour
	// WatchSession streams at most this long, so agents call it regularly,
	// which keeps their sessions alive.
	maxWait = 2 * time.Minute
)

// ServerConfig is what a Server needs.
type ServerConfig struct {
	Store      *Store
	Identifier httpx.Identifier
	Cluster    statedir.ControllerConfig
	Now        func() time.Time // time.Now when nil
	// UI serves every path outside the API; none when nil.
	UI http.Handler
	// Workers is how the controller calls workers; nil uses a default client.
	Workers *http.Client
	// WorkerAddr maps a worker's tailnet address (or host name, when the
	// tailnet gave no address) to where its API listens; nil means
	// addr:api.WorkerPort.
	WorkerAddr func(addr string) string
	// Controller is this machine's tailnet host name, for the cockpit.
	Controller string
}

// Server is the controller's API: the cluster, sessions, and workspaces,
// whose calls it forwards to the worker that holds each one.
type Server struct {
	hotbedv1connect.UnimplementedClusterServiceHandler
	cfg ServerConfig

	mu      sync.Mutex
	workers map[string]hotbedv1connect.WorkspaceServiceClient
	waiters map[string]chan struct{} // closed when a session gets events

	// repoEdits makes each change to a repo (its steps, its epoch) read and
	// write it in one go, so agents editing steps at once lose nothing.
	repoEdits sync.Mutex
}

func NewServer(cfg ServerConfig) *Server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Workers == nil {
		cfg.Workers = &http.Client{}
	}
	if cfg.WorkerAddr == nil {
		cfg.WorkerAddr = func(h string) string { return net.JoinHostPort(h, strconv.Itoa(api.WorkerPort)) }
	}
	return &Server{cfg: cfg, workers: map[string]hotbedv1connect.WorkspaceServiceClient{}, waiters: map[string]chan struct{}{}}
}

// Handler serves the API and the cockpit to the cluster's owner only.
func (s *Server) Handler() http.Handler {
	server := httpx.NewServer(s.cfg.Cluster.Owner)
	hotbedv1connect.RegisterClusterServiceHandler(server, s)
	hotbedv1connect.RegisterSessionServiceHandler(server, &sessions{s})
	hotbedv1connect.RegisterWorkspaceServiceHandler(server, &workspaces{s})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	if s.cfg.UI != nil {
		mux.Handle("/", httpx.RequireOwner(s.cfg.Cluster.Owner, s.cfg.UI))
	}
	return httpx.Identify(s.cfg.Identifier, mux)
}

func (s *Server) now() time.Time { return s.cfg.Now() }

// ---- ClusterService ---------------------------------------------------

func (s *Server) Join(ctx context.Context, _ *pb.JoinRequest) (*pb.JoinResponse, error) {
	peer := httpx.Peer(ctx)
	node, err := s.cfg.Store.Join(ctx, peer, s.now())
	if err != nil {
		return nil, err
	}
	slog.Info("node joined", "node", peer.Hostname, "id", peer.NodeID)
	return &pb.JoinResponse{ClusterId: s.cfg.Cluster.ClusterID, Node: s.withOnline(node)}, nil
}

func (s *Server) Report(ctx context.Context, rep *pb.ReportRequest) (*pb.ReportResponse, error) {
	peer := httpx.Peer(ctx)
	if rep.ClusterId != s.cfg.Cluster.ClusterID {
		return nil, api.Errorf(api.CodeClusterMismatch, "%s joined cluster %s, but this controller runs cluster %s", peer.Hostname, rep.ClusterId, s.cfg.Cluster.ClusterID).
			WithSuggestion("join this cluster with a new state directory: hotbed join CONTROLLER DIR")
	}
	in, err := s.cfg.Store.Ingest(ctx, peer.NodeID, rep, s.now())
	if errors.Is(err, errNotJoined) {
		return nil, api.Errorf(api.CodeNotJoined, "%s has not joined this cluster", peer.Hostname).
			WithSuggestion("join with a new state directory: hotbed join CONTROLLER DIR")
	}
	if err != nil {
		return nil, err
	}
	for session := range in.sessions {
		s.wake(session)
	}
	repos, err := s.cfg.Store.Repos(ctx)
	if err != nil {
		return nil, err
	}
	out := &pb.ReportResponse{Ack: in.ack}
	for _, ws := range rep.Workspaces {
		if ws.State == pb.WorkspaceState_WORKSPACE_STATE_RELEASED {
			continue
		}
		if w, err := s.cfg.Store.Workspace(ctx, ws.Id); err == nil && w.released {
			out.Release = append(out.Release, ws.Id)
		}
	}
	for _, repo := range repos {
		if len(repo.Hosts) == 0 || slices.Contains(repo.Hosts, peer.Hostname) {
			out.Repos = append(out.Repos, repo)
		}
	}
	return out, nil
}

func (s *Server) ListNodes(ctx context.Context, _ *pb.ListNodesRequest) (*pb.ListNodesResponse, error) {
	rows, err := s.cfg.Store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	out := &pb.ListNodesResponse{ClusterId: s.cfg.Cluster.ClusterID}
	for _, n := range rows {
		out.Nodes = append(out.Nodes, s.withOnline(n.node))
	}
	return out, nil
}

func (s *Server) withOnline(n *pb.Node) *pb.Node {
	n.Online = n.LastSeen != nil && s.now().Sub(time.UnixMilli(*n.LastSeen)) <= api.OnlineWindow
	return n
}

var repoID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

func (s *Server) ListRepos(ctx context.Context, _ *pb.ListReposRequest) (*pb.ListReposResponse, error) {
	repos, err := s.cfg.Store.Repos(ctx)
	return &pb.ListReposResponse{Repos: repos}, err
}

func (s *Server) PutRepo(ctx context.Context, req *pb.PutRepoRequest) (*pb.Repo, error) {
	repo := req.GetRepo()
	if !repoID.MatchString(repo.GetId()) {
		return nil, api.Errorf(api.CodeInvalidArgs, "repo name %q must be lowercase letters, digits and dashes", repo.GetId())
	}
	if repo.Url == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "repo %s has no URL", repo.Id)
	}
	repo.CreatedAt = s.now().UnixMilli()
	s.repoEdits.Lock()
	defer s.repoEdits.Unlock()
	// Updating a repo keeps its scratch epoch, so it does not rebuild
	// from nothing, and its steps, which have their own calls.
	if old, err := s.repo(ctx, repo.Id); err == nil {
		repo.ScratchEpoch, repo.Steps = old.ScratchEpoch, old.Steps
	}
	return repo, s.cfg.Store.PutRepo(ctx, repo)
}

// RebuildRepo raises the repo's scratch epoch, which has each worker build
// its next base from nothing.
func (s *Server) RebuildRepo(ctx context.Context, req *pb.RebuildRepoRequest) (*pb.Repo, error) {
	s.repoEdits.Lock()
	defer s.repoEdits.Unlock()
	r, err := s.repo(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	r.ScratchEpoch++
	return r, s.cfg.Store.PutRepo(ctx, r)
}

// repo is the repo called id.
func (s *Server) repo(ctx context.Context, id string) (*pb.Repo, error) {
	repos, err := s.cfg.Store.Repos(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range repos {
		if r.Id == id {
			return r, nil
		}
	}
	return nil, api.Errorf(api.CodeNotFound, "no repo %s", id).WithSuggestion("hotbed repo ls")
}

func (s *Server) DeleteRepo(ctx context.Context, req *pb.DeleteRepoRequest) (*pb.DeleteRepoResponse, error) {
	found, err := s.cfg.Store.DeleteRepo(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, api.Errorf(api.CodeNotFound, "no repo %s", req.Id)
	}
	return &pb.DeleteRepoResponse{}, nil
}

// ---- workers ------------------------------------------------------------

// worker is the client for the worker with node ID id, which must be online.
func (s *Server) worker(ctx context.Context, nodeID string) (hotbedv1connect.WorkspaceServiceClient, string, error) {
	n, err := s.cfg.Store.node(ctx, nodeID)
	if err != nil {
		return nil, "", err
	}
	host := n.node.Hostname
	target := n.addr
	if target == "" {
		target = host
	}
	if !s.withOnline(n.node).Online {
		return nil, host, &api.Error{Code: api.CodeWorkerUnreachable, Message: host + " is offline", Retryable: true}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.workers[host]; ok {
		return c, host, nil
	}
	conn, err := api.Dial(s.cfg.WorkerAddr(target), api.WorkerPort, s.cfg.Workers)
	if err != nil {
		return nil, host, err
	}
	c := hotbedv1connect.NewWorkspaceServiceClient(conn)
	s.workers[host] = c
	return c, host, nil
}

// fromWorker passes a worker's error on with its code; a worker that cannot
// be reached is WORKER_UNREACHABLE.
func fromWorker(host string, err error) error {
	e := api.FromConnect(err)
	if e.Code == api.CodeControllerUnreachable {
		return &api.Error{Code: api.CodeWorkerUnreachable, Message: host + ": " + e.Message, Retryable: true}
	}
	return e
}

func (s *Server) wake(session string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.waiters[session]; ok {
		close(ch)
		delete(s.waiters, session)
	}
}

func (s *Server) waiter(session string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.waiters[session]
	if !ok {
		ch = make(chan struct{})
		s.waiters[session] = ch
	}
	return ch
}

// newID is eight random base32 characters, lowercase.
func newID() string {
	b := make([]byte, 5)
	rand.Read(b)
	return strings.ToLower(base32.StdEncoding.EncodeToString(b))
}

// Reap ends sessions abandoned for sessionIdle, every minute until ctx ends.
func (s *Server) Reap(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if err := s.ReapIdle(ctx); err != nil {
			slog.Error("end abandoned sessions", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ReapIdle ends every session nothing has touched for sessionIdle.
func (s *Server) ReapIdle(ctx context.Context) error {
	ids, err := s.cfg.Store.IdleSessions(ctx, s.now().Add(-sessionIdle))
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		slog.Info("ending abandoned session", "session", id)
		if _, err := s.endSession(ctx, id, fmt.Sprintf("abandoned: nothing called it for %s", sessionIdle)); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// endSession releases a session's workspaces, which stops their commands,
// and marks it ended.
func (s *Server) endSession(ctx context.Context, id, reason string) (*pb.Session, error) {
	sess, err := s.cfg.Store.Session(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, ws := range sess.WorkspaceIds {
		err := s.releaseWorkspace(ctx, ws)
		switch e := api.FromConnect(err); {
		case err == nil, e.Code == api.CodeNotFound:
		case e.Code == api.CodeWorkerUnreachable:
			// Its worker releases it when it reports again.
			slog.Warn("release workspace later", "workspace", ws, "err", err)
			if err := s.cfg.Store.ReleaseWorkspace(ctx, ws, s.now()); err != nil {
				return nil, err
			}
		default:
			return nil, err
		}
	}
	if err := s.cfg.Store.EndSession(ctx, id, reason, s.now()); err != nil {
		return nil, err
	}
	s.wake(id)
	return s.cfg.Store.Session(ctx, id)
}

func (s *Server) releaseWorkspace(ctx context.Context, id string) error {
	w, err := s.cfg.Store.Workspace(ctx, id)
	if err != nil {
		return err
	}
	if w.released {
		return nil
	}
	c, host, err := s.worker(ctx, w.node)
	if err != nil {
		return err
	}
	if _, err := c.ReleaseWorkspace(ctx, &pb.ReleaseWorkspaceRequest{Id: id}); err != nil && api.FromConnect(err).Code != api.CodeNotFound {
		return fromWorker(host, err)
	}
	return s.cfg.Store.ReleaseWorkspace(ctx, id, s.now())
}
