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
	"net/netip"
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
	"github.com/semistrict/hotbed/internal/tailnet"
	"github.com/semistrict/hotbed/internal/update"
	"github.com/semistrict/hotbed/internal/version"
)

const (
	// A session nothing touches for this long is abandoned: its commands
	// stop and its workspaces go.
	sessionIdle = time.Hour
	// WatchSession streams at most this long, so agents call it regularly,
	// which keeps their sessions alive.
	maxWait = 2 * time.Minute
	// A session with a workspace on a worker out of reach waits this long
	// at most before it is abandoned like any other.
	offlineGrace = 24 * time.Hour
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
	// Self is this machine's tailnet addresses, the only ones the gate lets
	// requests in on.
	Self []netip.Addr
	// LocalUsers are the unix users, besides root, who may call from this
	// machine itself.
	LocalUsers []uint32
	// Builds keeps the builds the controller hands out, and Build is the one
	// it runs (0 for a dev build): nodes and clients install it over
	// themselves. Nil hands out none.
	Builds *update.Cache
	Build  int64
	// Follower checks CI for newer builds when configured; nil disables
	// controller self-upgrades.
	Follower *Follower
}

// Server is the controller's API: the cluster, sessions, and workspaces,
// whose calls it forwards to the worker that holds each one.
type Server struct {
	hotbedv1connect.UnimplementedClusterServiceHandler
	cfg ServerConfig

	mu      sync.Mutex
	workers map[string]hotbedv1connect.WorkspaceServiceClient

	// sessionWakes wakes a session's watchers when it gets events;
	// questionWakes wakes everyone waiting on questions when one changes.
	sessionWakes  wakeups
	questionWakes wakeups

	// repoEdits makes each change to a repo (its steps, its epoch) read and
	// write it in one go, so agents editing steps at once lose nothing.
	repoEdits sync.Mutex
	// scheduling makes starting scheduled runs and changing schedules one at
	// a time, so no slot runs twice and runs of a schedule never overlap.
	scheduling sync.Mutex
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
	return &Server{cfg: cfg, workers: map[string]hotbedv1connect.WorkspaceServiceClient{}}
}

// Handler serves the API and the cockpit to the cluster's owner only.
// Handler serves the API and the cockpit, behind the gate that admits the
// cluster's owner alone.
func (s *Server) Handler() http.Handler {
	server := httpx.NewServer()
	hotbedv1connect.RegisterClusterServiceHandler(server, s)
	hotbedv1connect.RegisterSessionServiceHandler(server, &sessions{s})
	hotbedv1connect.RegisterWorkspaceServiceHandler(server, &workspaces{s})
	hotbedv1connect.RegisterScheduleServiceHandler(server, &schedules{s})
	hotbedv1connect.RegisterQuestionServiceHandler(server, &questions{s})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	mux.HandleFunc("GET /builds/{build}/{asset}", s.serveBuild)
	if s.cfg.UI != nil {
		mux.Handle("/", s.cfg.UI)
	}
	gated := httpx.Gate(s.cfg.Identifier, httpx.Policy{
		Owner: s.cfg.Cluster.Owner, Self: s.cfg.Self, LocalUsers: s.cfg.LocalUsers,
	}, mux)
	// Every answer says the build the controller runs, so a client on
	// another notices.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(BuildHeader, strconv.FormatInt(s.cfg.Build, 10))
		gated.ServeHTTP(w, r)
	})
}

// BuildHeader is the response header that says the controller's build.
const BuildHeader = "Hotbed-Build"

// build is the controller's build for p, or nil when it keeps none: it runs
// a dev build, or a build not yet fetched.
func (s *Server) build(p update.Platform) *pb.Build {
	if s.cfg.Builds == nil || s.cfg.Build == 0 {
		return nil
	}
	_, sum, err := s.cfg.Builds.Binary(s.cfg.Build, p)
	if err != nil {
		return nil
	}
	return &pb.Build{Number: s.cfg.Build, Sha256: sum, Path: fmt.Sprintf("/builds/%d/%s", s.cfg.Build, p.Asset())}
}

// serveBuild serves a binary of the build the controller runs.
func (s *Server) serveBuild(w http.ResponseWriter, r *http.Request) {
	for _, p := range update.Platforms {
		if b := s.build(p); b != nil && r.URL.Path == b.Path {
			path, _, _ := s.cfg.Builds.Binary(s.cfg.Build, p)
			w.Header().Set("Content-Type", "application/octet-stream")
			http.ServeFile(w, r, path)
			return
		}
	}
	http.Error(w, fmt.Sprintf("the controller hands out only its own build's binaries; it runs %s", version.String()), http.StatusNotFound)
}

func (s *Server) GetBuild(_ context.Context, req *pb.GetBuildRequest) (*pb.Build, error) {
	p := update.Platform{OS: req.Os, Arch: req.Arch}
	if b := s.build(p); b != nil {
		return b, nil
	}
	if s.cfg.Build > 0 && slices.Contains(update.Platforms, p) {
		return nil, api.Errorf(api.CodeNotFound, "the controller runs build %d, which it has not fetched yet", s.cfg.Build).
			WithSuggestion("try again in a few minutes")
	}
	if s.cfg.Build > 0 {
		return nil, api.Errorf(api.CodeNotFound, "no build is made for %s", p)
	}
	return nil, api.Errorf(api.CodeNotFound, "the controller runs %s, a build it doesn't keep: nothing to upgrade to", version.String()).
		WithSuggestion("it moves to CI's newest build within minutes once it follows CI (its config's updates)")
}

func (s *Server) UpgradeController(ctx context.Context, _ *pb.UpgradeControllerRequest) (*pb.UpgradeControllerResponse, error) {
	if s.cfg.Follower == nil {
		return nil, api.Errorf(api.CodeNotFound, "this controller does not follow CI builds: nothing to upgrade from").
			WithSuggestion("configure updates (repo and tokenFile) in the controller's config")
	}
	b, restarting, err := s.cfg.Follower.Upgrade(ctx)
	if err != nil {
		return nil, api.Errorf(api.CodeInternal, "upgrade controller: %v", err)
	}
	return &pb.UpgradeControllerResponse{
		Build:      b,
		From:       s.cfg.Build,
		Restarting: restarting,
	}, nil
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

// Report takes a report from a worker before the event stream.
func (s *Server) Report(ctx context.Context, rep *pb.ReportRequest) (*pb.ReportResponse, error) {
	peer, err := s.fromWorker(ctx, rep.ClusterId)
	if err != nil {
		return nil, err
	}
	in, err := s.cfg.Store.Ingest(ctx, peer.NodeID, rep, s.now())
	if err != nil {
		return nil, s.notJoined(peer, err)
	}
	s.wakeAll(in.sessions)
	if len(rep.Finished) > 0 {
		s.settleSoon(ctx)
	}
	repos, release, err := s.hosting(ctx, peer)
	if err != nil {
		return nil, err
	}
	return &pb.ReportResponse{Ack: in.applied, Repos: repos, Release: release}, nil
}

// PushEvents folds a worker's events into its node's view, and wakes the
// sessions they concern.
func (s *Server) PushEvents(ctx context.Context, req *pb.PushEventsRequest) (*pb.PushEventsResponse, error) {
	peer, err := s.fromWorker(ctx, req.ClusterId)
	if err != nil {
		return nil, err
	}
	in, err := s.cfg.Store.Push(ctx, peer.NodeID, req, s.now())
	if err != nil {
		return nil, s.notJoined(peer, err)
	}
	if req.Checkpoint != nil {
		slog.Info("took a worker's checkpoint", "node", peer.Hostname, "epoch", req.Epoch, "seq", req.Checkpoint.Seq, "lost", len(in.lost))
	}
	s.wakeAll(in.sessions)
	if len(in.lost) > 0 {
		// Their ends will never come: their scheduled runs end now.
		if err := s.loseRuns(context.WithoutCancel(ctx), in.lost); err != nil {
			slog.Error("end the scheduled runs of lost commands", "err", err)
		}
	}
	if in.finished {
		s.settleSoon(ctx)
	}
	return &pb.PushEventsResponse{Applied: in.applied, CheckpointNeeded: in.checkpointNeeded}, nil
}

// Heartbeat records a worker's live usage and tells it what to host.
func (s *Server) Heartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	peer, err := s.fromWorker(ctx, req.ClusterId)
	if err != nil {
		return nil, err
	}
	if err := s.cfg.Store.Beat(ctx, peer.NodeID, req, s.now()); err != nil {
		return nil, s.notJoined(peer, err)
	}
	repos, release, err := s.hosting(ctx, peer)
	if err != nil {
		return nil, err
	}
	// Workers run Linux.
	build := s.build(update.Platform{OS: "linux", Arch: req.GetResources().GetArch()})
	return &pb.HeartbeatResponse{Repos: repos, Release: release, Build: build}, nil
}

// fromWorker is the calling worker, which must be of this cluster.
func (s *Server) fromWorker(ctx context.Context, clusterID string) (tailnet.Peer, error) {
	peer := httpx.Peer(ctx)
	if clusterID != s.cfg.Cluster.ClusterID {
		return peer, api.Errorf(api.CodeClusterMismatch, "%s joined cluster %s, but this controller runs cluster %s", peer.Hostname, clusterID, s.cfg.Cluster.ClusterID).
			WithSuggestion("join this cluster with a new state directory: hotbed join CONTROLLER DIR")
	}
	return peer, nil
}

func (s *Server) notJoined(peer tailnet.Peer, err error) error {
	if errors.Is(err, errNotJoined) {
		return api.Errorf(api.CodeNotJoined, "%s has not joined this cluster", peer.Hostname).
			WithSuggestion("join with a new state directory: hotbed join CONTROLLER DIR")
	}
	return err
}

// hosting is what a worker should host: the repos it keeps bases of, and
// the workspaces of ended sessions it still has, to release.
func (s *Server) hosting(ctx context.Context, peer tailnet.Peer) ([]*pb.Repo, []string, error) {
	repos, err := s.cfg.Store.Repos(ctx)
	if err != nil {
		return nil, nil, err
	}
	var out []*pb.Repo
	for _, repo := range repos {
		if len(repo.Hosts) == 0 || slices.Contains(repo.Hosts, peer.Hostname) {
			out = append(out, repo)
		}
	}
	release, err := s.cfg.Store.ToRelease(ctx, peer.NodeID)
	return out, release, err
}

// loseRuns ends, as cancelled, the scheduled runs whose commands a worker
// lost.
func (s *Server) loseRuns(ctx context.Context, commands []string) error {
	runs, err := s.cfg.Store.LiveRuns(ctx)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.CommandId == "" || !slices.Contains(commands, run.CommandId) {
			continue
		}
		ended := s.now().UnixMilli()
		run.Outcome, run.Reason, run.EndedAt = pb.RunOutcome_RUN_OUTCOME_CANCELLED, lostDetail, &ended
		if _, err := s.cfg.Store.UpdateRun(ctx, run); err != nil {
			return err
		}
	}
	return nil
}

// settleSoon settles the scheduled runs whose commands just ended: now,
// not at the next tick, even if the worker stops waiting for the answer.
func (s *Server) settleSoon(ctx context.Context) {
	if err := s.settleRuns(context.WithoutCancel(ctx)); err != nil {
		slog.Error("settle scheduled runs", "err", err)
	}
}

func (s *Server) wakeAll(sessions map[string]bool) {
	for session := range sessions {
		s.sessionWakes.wake(session)
	}
}

func (s *Server) ListNodes(ctx context.Context, _ *pb.ListNodesRequest) (*pb.ListNodesResponse, error) {
	rows, err := s.cfg.Store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	out := &pb.ListNodesResponse{ClusterId: s.cfg.Cluster.ClusterID, ControllerVersion: version.String()}
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

// ReapIdle ends every session nothing has touched for sessionIdle. A
// session with a workspace on a worker out of reach waits: clients may be
// using that worker directly, and only its heartbeats would say so.
func (s *Server) ReapIdle(ctx context.Context) error {
	ids, err := s.cfg.Store.IdleSessions(ctx, s.now().Add(-sessionIdle))
	if err != nil {
		return err
	}
	rows, err := s.cfg.Store.Nodes(ctx)
	if err != nil {
		return err
	}
	// A worker unheard of for longer than offlineGrace is gone, not out of
	// reach: its sessions end like any other.
	offline := map[string]bool{}
	for _, n := range rows {
		recent := n.node.LastSeen != nil && s.now().Sub(time.UnixMilli(*n.node.LastSeen)) < offlineGrace
		offline[n.node.Id] = !s.withOnline(n.node).Online && recent
	}
	var errs []error
	for _, id := range ids {
		sess, err := s.cfg.Store.Session(ctx, id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if slices.ContainsFunc(sess.Workspaces, func(w *pb.SessionWorkspace) bool { return w.ReleasedAt == nil && offline[w.NodeId] }) {
			continue
		}
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
	// Its scheduled runs end because the session does: settled first, before
	// the worker's word that releasing the workspace cancelled their commands.
	if err := s.endRuns(ctx, id, reason); err != nil {
		return nil, err
	}
	for _, ws := range sess.WorkspaceIds {
		err := s.releaseWorkspace(ctx, ws)
		switch e := api.FromConnect(err); {
		case err == nil, e.Code == api.CodeNotFound:
		case e.Code == api.CodeWorkerUnreachable:
			// Its worker's next heartbeat releases it.
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
	s.sessionWakes.wake(id)
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
