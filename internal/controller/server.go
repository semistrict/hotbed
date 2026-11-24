package controller

import (
	"cmp"
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
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/daemonlog"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/gitops"
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
	// WatchSession streams, and Poll waits, at most this long, so agents
	// call them regularly, which keeps their sessions alive.
	maxWait = 2 * time.Minute
	// A session with a workspace on a worker out of reach waits this long
	// at most before it is abandoned like any other.
	offlineGrace = 24 * time.Hour
	// workerRPCTimeout bounds short unary controller->worker RPCs when the
	// caller has no tighter deadline. Streaming RPCs (TraceCommand,
	// SampleCommand) and slow CreateWorkspace calls rely on their
	// caller context rather than a transport ResponseHeaderTimeout.
	workerRPCTimeout = 30 * time.Second
	// workerProbeTimeout bounds placement probes of missing workspaces on workers.
	workerProbeTimeout = 5 * time.Second
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
	// Hosts are this machine's tailnet DNS names (without port), allowed in
	// Host headers in addition to Self IPs.
	Hosts []string
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
	// GitAuthHTTP calls GitHub's REST API when minting GitHub App tokens;
	// nil uses a default client.
	GitAuthHTTP *http.Client
	// WakeWait is how long CreateWorkspace waits for a node it woke before
	// it has the client call again (NODE_WAKING); 0 is 40 seconds.
	WakeWait time.Duration
	// DaemonLogs holds the controller's rotating WARN/ERROR slog store; nil disables persistence.
	DaemonLogs *daemonlog.Store
	// AllowLocalRepos allows file:// URLs and absolute local filesystem paths
	// in PutRepo. Intended for test harnesses that register local repos; false
	// in production.
	AllowLocalRepos bool
}

type workerClient struct {
	addr   string
	client hotbedv1connect.WorkspaceServiceClient
}

type sessionLock struct {
	mu   sync.Mutex
	refs int
}

// Server is the controller's API: the cluster, sessions, and workspaces,
// whose calls it forwards to the worker that holds each one.
type Server struct {
	hotbedv1connect.UnimplementedClusterServiceHandler
	cfg     ServerConfig
	gitAuth *gitAuth

	mu         sync.Mutex
	workers    map[string]workerClient
	// creatingWS is the workspaces in flight in make(), each closed once its
	// node has made it or failed to.
	creatingWS   map[string]chan struct{}
	sessionLocks map[string]*sessionLock

	// sessionWakes wakes a session's watchers when it gets events.
	sessionWakes wakeups

	// scheduling makes planning scheduled runs and changing schedules one at
	// a time, so no slot runs twice and runs of a schedule never overlap.
	// Slow worker RPCs to launch a run happen outside this lock while the run's
	// ID is recorded in startingRuns. shuttingDown blocks new starts once server
	// shutdown begins, and startsWG tracks all in-flight starts (cron and manual
	// RunSchedule) so Shutdown waits for them before the store closes.
	scheduling   sync.Mutex
	startingRuns map[int64]*startingRun
	shuttingDown bool
	startsWG     sync.WaitGroup
	// placing makes placing runs one at a time, so two runs of a session at
	// once never both give it a workspace on the same node, and a node is
	// never put to sleep while a run is being placed on it.
	placing sync.Mutex
	// power is the power of the nodes whose hosts run them in VMs.
	power standby
	// hostUsages stores the latest host-side usage reported by VM host supervisors.
	hostUsages hostUsageState

	// samplesMu guards samplesCache and samplesInFlight for GetSnapshot's 24h
	// sample aggregation.
	samplesMu       sync.Mutex
	samplesCache    *samplesSnapshot
	samplesInFlight *samplesFill
}

func defaultWorkerClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
	tr.TLSHandshakeTimeout = 10 * time.Second
	tr.IdleConnTimeout = 90 * time.Second
	// Leave ResponseHeaderTimeout unset (0): streaming RPCs like TraceCommand
	// and SampleCommand do not send response headers until the first chunk, and
	// CreateWorkspace can legitimately take longer than 30s. Short unary RPCs
	// bound themselves with context.WithTimeout instead.
	tr.ResponseHeaderTimeout = 0
	return &http.Client{Transport: tr}
}

func NewServer(cfg ServerConfig) *Server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Workers == nil {
		cfg.Workers = defaultWorkerClient()
	}
	if cfg.GitAuthHTTP == nil {
		cfg.GitAuthHTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.WorkerAddr == nil {
		cfg.WorkerAddr = func(h string) string { return net.JoinHostPort(h, strconv.Itoa(api.WorkerPort)) }
	}
	s := &Server{
		cfg:          cfg,
		workers:      map[string]workerClient{},
		creatingWS:   map[string]chan struct{}{},
		sessionLocks: map[string]*sessionLock{},
		startingRuns: map[int64]*startingRun{},
	}
	s.power.now, s.power.wakeWait = cfg.Now, cmp.Or(cfg.WakeWait, defaultWakeWait)
	s.gitAuth = newGitAuth(s)
	return s
}

// lockSession serializes lifecycle transitions (end, reap, resume) per session
// so a concurrent ResumeSession never collides with an in-flight finishEndSession.
func (s *Server) lockSession(id string) func() {
	s.mu.Lock()
	if s.sessionLocks == nil {
		s.sessionLocks = map[string]*sessionLock{}
	}
	l := s.sessionLocks[id]
	if l == nil {
		l = &sessionLock{}
		s.sessionLocks[id] = l
	}
	l.refs++
	s.mu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		s.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(s.sessionLocks, id)
		}
		s.mu.Unlock()
	}
}

func (s *Server) markCreatingWS(id string) {
	s.mu.Lock()
	s.creatingWS[id] = make(chan struct{})
	s.mu.Unlock()
}

func (s *Server) unmarkCreatingWS(id string) {
	s.mu.Lock()
	if made, ok := s.creatingWS[id]; ok {
		close(made)
		delete(s.creatingWS, id)
	}
	s.mu.Unlock()
}

func (s *Server) isCreatingWS(id string) bool { return s.creatingDone(id) != nil }

// creatingDone is closed once workspace id, in flight in make(), is made or
// has failed; nil when it is not in flight.
func (s *Server) creatingDone(id string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creatingWS[id]
}

func (s *Server) creatingWorkspacesSnapshot() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.creatingWS) == 0 {
		return nil
	}
	cp := make(map[string]bool, len(s.creatingWS))
	for id := range s.creatingWS {
		cp[id] = true
	}
	return cp
}

// Handler serves the API and the cockpit to the cluster's owner only.
// Handler serves the API and the cockpit, behind the gate that admits the
// cluster's owner alone.
func (s *Server) Handler() http.Handler {
	server := httpx.NewServer(remindToPoll, s.migratedInterceptor())
	hotbedv1connect.RegisterClusterServiceHandler(server, s)
	hotbedv1connect.RegisterSessionServiceHandler(server, &sessions{s})
	hotbedv1connect.RegisterWorkspaceServiceHandler(server, &workspaces{Server: s})
	hotbedv1connect.RegisterScheduleServiceHandler(server, &schedules{s})
	hotbedv1connect.RegisterQuestionServiceHandler(server, &questions{s})
	hotbedv1connect.RegisterReviewServiceHandler(server, &reviews{s})
	hotbedv1connect.RegisterGitAuthServiceHandler(server, s.gitAuth)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	mux.HandleFunc("GET /builds/{build}/{asset}", s.serveBuild)
	if s.cfg.UI != nil {
		mux.Handle("/", s.cfg.UI)
	}
	hosts := slices.Clone(s.cfg.Hosts)
	if s.cfg.Controller != "" && !slices.Contains(hosts, s.cfg.Controller) {
		hosts = append(hosts, s.cfg.Controller)
	}
	gated := httpx.Gate(s.cfg.Identifier, httpx.Policy{
		Owner: s.cfg.Cluster.Owner, Self: s.cfg.Self, Hosts: hosts, LocalUsers: s.cfg.LocalUsers,
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

// PushEvents folds a worker's events into its node's view, and wakes the
// sessions they concern.
func (s *Server) PushEvents(ctx context.Context, req *pb.PushEventsRequest) (*pb.PushEventsResponse, error) {
	peer, err := s.fromWorker(ctx, req.ClusterId)
	if err != nil {
		return nil, err
	}
	if mig := s.migrationForWorker(ctx, peer.NodeID); mig != nil {
		return &pb.PushEventsResponse{MigratedTo: mig}, nil
	}
	in, err := s.cfg.Store.Push(ctx, peer.NodeID, req, s.now(), s.creatingWorkspacesSnapshot())
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
	if mig := s.migrationForWorker(ctx, peer.NodeID); mig != nil {
		return &pb.HeartbeatResponse{MigratedTo: mig}, nil
	}
	now := s.now()
	hu := s.hostUsage(peer.Hostname, now)
	if err := s.cfg.Store.Beat(ctx, peer.NodeID, req, now); err != nil {
		return nil, s.notJoined(peer, err)
	}
	// A node waking, or one whose work changed, may now be up or idle.
	s.power.changes.wake(peer.Hostname)
	projects, release, err := s.hosting(ctx, peer)
	if err != nil {
		return nil, err
	}
	// Workers run Linux.
	build := s.build(update.Platform{OS: "linux", Arch: req.GetResources().GetArch()})
	return &pb.HeartbeatResponse{Projects: projects, Release: release, Build: build, HostUsage: hu}, nil
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

// joined returns the calling peer if it is a joined worker node in this cluster.
func (s *Server) joined(ctx context.Context) (tailnet.Peer, error) {
	peer := httpx.Peer(ctx)
	if peer.NodeID == "" {
		return peer, s.notJoined(peer, errNotJoined)
	}
	ok, err := s.cfg.Store.HasNode(ctx, peer.NodeID)
	if err != nil {
		return peer, err
	}
	if !ok {
		return peer, s.notJoined(peer, errNotJoined)
	}
	return peer, nil
}

// hosting is what a worker should host: the repos it keeps bases of, and
// the workspaces of ended sessions it still has, to release.
func (s *Server) hosting(ctx context.Context, peer tailnet.Peer) ([]*pb.HostedProject, []string, error) {
	projects, err := s.cfg.Store.Projects(ctx)
	if err != nil {
		return nil, nil, err
	}
	repos, err := s.cfg.Store.Repos(ctx)
	if err != nil {
		return nil, nil, err
	}
	var out []*pb.HostedProject
	for _, p := range projects {
		if len(p.Hosts) > 0 && !slices.Contains(p.Hosts, peer.Hostname) {
			continue
		}
		// A project its workspaces cannot be made of (it holds a remote no
		// repo has, or none) has no bases kept: they go once unused.
		if h, missing := hostedOf(p, repos); len(missing) == 0 && len(h.Repos) > 0 {
			out = append(out, h)
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

// withOnline fills in whether the node is online, whether it is asleep, and
// any fresh host-side usage / starvation signals reported by its host supervisor.
// A node that heartbeats but cannot open its storage (storage_error) serves
// nothing, so it is not online: nothing is placed on it or asked of it.
func (s *Server) withOnline(n *pb.Node) *pb.Node {
	now := s.now()
	n.Online = n.LastSeen != nil && now.Sub(time.UnixMilli(*n.LastSeen)) <= api.OnlineWindow && n.StorageError == ""
	n.Asleep = s.power.asleep(n)
	if n.Online && !n.Asleep {
		if hu := s.hostUsage(n.Hostname, now); hu != nil {
			n.HostUsage = hu
			n.Admission = mergeHostAdmission(n.Admission, n.Usage, hu, now)
		}
	}
	return n
}

var repoID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

func (s *Server) ListRepos(ctx context.Context, req *pb.ListReposRequest) (*pb.ListReposResponse, error) {
	repos, err := s.cfg.Store.Repos(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetProject() != "" {
		p, err := s.cfg.Store.Project(ctx, req.Project)
		if err != nil {
			return nil, err
		}
		var filtered []*pb.Repo
		for _, r := range repos {
			if slices.Contains(p.Repos, gitops.CanonicalRemote(r.Url)) {
				filtered = append(filtered, r)
			}
		}
		repos = filtered
	}
	return &pb.ListReposResponse{Repos: repos}, nil
}

func (s *Server) PutRepo(ctx context.Context, req *pb.PutRepoRequest) (*pb.Repo, error) {
	repo := req.GetRepo()
	if !repoID.MatchString(repo.GetId()) {
		return nil, api.Errorf(api.CodeInvalidArgs, "repo name %q must be lowercase letters, digits and dashes", repo.GetId())
	}
	validateURL := gitops.ValidateRepoURL
	if s.cfg.AllowLocalRepos {
		validateURL = gitops.ValidateRepoURLAllowLocal
	}
	if err := validateURL(repo.GetUrl()); err != nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "repo %s has invalid URL: %v", repo.Id, err)
	}
	if req.Project != "" {
		if _, err := s.cfg.Store.Project(ctx, req.Project); err != nil {
			return nil, err
		}
	}
	repo = proto.Clone(repo).(*pb.Repo)
	repo.CreatedAt = s.now().UnixMilli()
	repo.Reminder = nil
	if err := s.cfg.Store.PutRepo(ctx, repo); err != nil {
		return nil, err
	}
	var proj *pb.Project
	var err error
	if req.Project != "" {
		var remotes []string
		if remote := gitops.CanonicalRemote(repo.Url); remote != "" {
			remotes = []string{remote}
		}
		proj, err = s.cfg.Store.AddProject(ctx, req.Project, remotes, s.madeBy(ctx, "for registered repo "+repo.Id))
	} else {
		proj, err = s.ensureRepoProject(ctx, repo.Id, repo.Url)
	}
	if err != nil {
		return nil, err
	}
	reminder := setupStepsReminder(proj)
	repo.Reminder = &reminder
	return repo, nil
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
	switch node := s.withOnline(n.node); {
	case node.Asleep:
		return nil, host, (&api.Error{Code: api.CodeWorkerUnreachable, Message: host + " is asleep", Retryable: true}).
			WithSuggestion("a run placed on it wakes it")
	case !node.Online:
		return nil, host, &api.Error{Code: api.CodeWorkerUnreachable, Message: host + " is offline", Retryable: true}
	}
	addr := s.cfg.WorkerAddr(target)
	s.mu.Lock()
	defer s.mu.Unlock()
	if w, ok := s.workers[nodeID]; ok && w.addr == addr {
		return w.client, host, nil
	}
	conn, err := api.Dial(addr, api.WorkerPort, s.cfg.Workers)
	if err != nil {
		return nil, host, err
	}
	c := hotbedv1connect.NewWorkspaceServiceClient(conn)
	s.workers[nodeID] = workerClient{addr: addr, client: c}
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
	now := s.now()
	s.hostUsages.prune(now)
	cutoff := now.Add(-sessionIdle)
	ids, err := s.cfg.Store.IdleSessions(ctx, cutoff)
	if err != nil {
		return err
	}
	rows, err := s.cfg.Store.Nodes(ctx)
	if err != nil {
		return err
	}
	// A worker unheard of for longer than offlineGrace is gone, not out of
	// reach: its sessions end like any other. So do those of a node asleep:
	// no client uses it, and it releases their workspaces when it wakes.
	offline := map[string]bool{}
	for _, n := range rows {
		recent := n.node.LastSeen != nil && now.Sub(time.UnixMilli(*n.node.LastSeen)) < offlineGrace
		node := s.withOnline(n.node)
		offline[n.node.Id] = !node.Online && !node.Asleep && recent
	}
	var errs []error
	for _, id := range ids {
		if err := s.reapOne(ctx, id, cutoff, offline); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Server) reapOne(ctx context.Context, id string, cutoff time.Time, offline map[string]bool) error {
	unlock := s.lockSession(id)
	defer unlock()
	sess, err := s.cfg.Store.Session(ctx, id)
	if err != nil {
		return err
	}
	if sess.EndedAt != nil {
		return nil
	}
	if slices.ContainsFunc(sess.Workspaces, func(w *pb.SessionWorkspace) bool { return w.ReleasedAt == nil && offline[w.NodeId] }) {
		return nil
	}
	reason := fmt.Sprintf("abandoned: nothing called it for %s", sessionIdle)
	ended, wss, err := s.cfg.Store.EndSessionIfIdleAndWorkspaces(ctx, id, reason, s.now(), cutoff)
	if err != nil {
		return err
	}
	if !ended {
		return nil // touched or ended concurrently
	}
	slog.Info("ending abandoned session", "session", id)
	_, err = s.finishEndSession(ctx, id, reason, wss)
	return err
}

// endSession marks a session ended, settles its scheduled runs, and releases
// its workspaces (which stops their commands).
func (s *Server) endSession(ctx context.Context, id, reason string) (*pb.Session, error) {
	return s.endSessionWith(ctx, id, reason, false)
}

func (s *Server) endLiveSession(ctx context.Context, id, reason string) (*pb.Session, error) {
	return s.endSessionWith(ctx, id, reason, true)
}

func (s *Server) endSessionWith(ctx context.Context, id, reason string, requireLive bool) (*pb.Session, error) {
	unlock := s.lockSession(id)
	defer unlock()
	sess, err := s.cfg.Store.Session(ctx, id)
	if err != nil {
		return nil, err
	}
	if requireLive && sess.EndedAt != nil {
		return nil, sessionEndedError(id, sess.EndReason)
	}
	// Mark ended and capture its live workspaces atomically in one transaction
	// while holding the per-session lock, so concurrent ResumeSession waits
	// until cleanup completes and can never lose a newly created workspace.
	wss, err := s.cfg.Store.EndSessionAndWorkspaces(ctx, id, reason, s.now())
	if err != nil {
		return nil, err
	}
	return s.finishEndSession(ctx, id, reason, wss)
}

func (s *Server) finishEndSession(ctx context.Context, id, reason string, wss []workspaceRow) (*pb.Session, error) {
	// Its scheduled runs end because the session does: settled first, before
	// the worker's word that releasing the workspace cancelled their commands.
	if err := s.endRuns(ctx, id, reason); err != nil {
		return nil, err
	}
	var releaseErrs []error
	for _, ws := range wss {
		err := s.releaseWorkspace(ctx, ws.id)
		switch e := api.FromConnect(err); {
		case err == nil, e.Code == api.CodeNotFound:
		case e.Code == api.CodeWorkerUnreachable:
			// Its worker's next heartbeat releases it.
			slog.Warn("release workspace later", "workspace", ws.id, "err", err)
			if err := s.cfg.Store.ReleaseWorkspace(ctx, ws.id, s.now()); err != nil {
				releaseErrs = append(releaseErrs, err)
			}
		default:
			slog.Warn("release workspace on session end failed; will retry on heartbeat", "workspace", ws.id, "err", err)
			releaseErrs = append(releaseErrs, err)
		}
	}
	s.sessionWakes.wake(id)
	if len(releaseErrs) > 0 {
		return nil, errors.Join(releaseErrs...)
	}
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
	// Once the worker has it, the record follows though the caller goes,
	// bounded by workerRPCTimeout so a hung worker never stalls cleanup forever.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workerRPCTimeout)
	defer cancel()
	c, host, err := s.worker(ctx, w.node)
	if err != nil {
		return err
	}
	if _, err := c.ReleaseWorkspace(ctx, &pb.ReleaseWorkspaceRequest{Id: id}); err != nil && api.CodeOf(err) != api.CodeNotFound {
		return fromWorker(host, err)
	}
	return s.cfg.Store.ReleaseWorkspace(ctx, id, s.now())
}
