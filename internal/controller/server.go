// Package controller is the node that admits workers, keeps the cluster's
// state, places workspaces, and serves the API and the cockpit.
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
	"net/http/httputil"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/httpx"
	"github.com/semistrict/hotbed/internal/statedir"
)

// Identifier names the tailnet node behind a connection.
type Identifier = httpx.Identifier

// ServerConfig is what a Server needs.
type ServerConfig struct {
	Store      *Store
	Identifier Identifier
	Cluster    statedir.ControllerConfig
	Now        func() time.Time // time.Now when nil
	// UI serves every path outside /v1; none when nil.
	UI http.Handler
	// Workers is how the controller calls workers; nil uses the default client.
	Workers *http.Client
	// WorkerAddr maps a worker's host name to the address its API listens
	// on; nil means host:api.WorkerPort.
	WorkerAddr func(hostname string) string
	// Controller is this machine's tailnet host name, for the cockpit.
	Controller string
}

// Server serves the controller's HTTP API.
type Server struct {
	cfg ServerConfig
}

func NewServer(cfg ServerConfig) *Server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Workers == nil {
		cfg.Workers = &http.Client{Timeout: 60 * time.Second}
	}
	if cfg.WorkerAddr == nil {
		cfg.WorkerAddr = func(h string) string { return net.JoinHostPort(h, strconv.Itoa(api.WorkerPort)) }
	}
	return &Server{cfg: cfg}
}

// Handler routes the API. Every route requires a caller the tailnet says is
// owned by the cluster's owner.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	h := httpx.Handle
	mux.HandleFunc("POST /v1/join", h(s.join))
	mux.HandleFunc("POST /v1/report", h(s.report))
	mux.HandleFunc("GET /v1/nodes", h(s.nodes))

	mux.HandleFunc("GET /v1/repos", h(s.repos))
	mux.HandleFunc("POST /v1/repos", h(s.putRepo))
	mux.HandleFunc("DELETE /v1/repos/{id}", h(s.deleteRepo))

	mux.HandleFunc("GET /v1/workspaces", h(s.workspaces))
	mux.HandleFunc("POST /v1/workspaces", h(s.createWorkspace))
	mux.HandleFunc("GET /v1/workspaces/{id}", h(s.toWorkspace))
	mux.HandleFunc("DELETE /v1/workspaces/{id}", h(s.releaseWorkspace))
	mux.HandleFunc("POST /v1/workspaces/{id}/commands", h(s.runCommand))
	mux.HandleFunc("POST /v1/workspaces/{id}/sync", h(s.toWorkspace))
	mux.HandleFunc("GET /v1/workspaces/{id}/files", h(s.toWorkspace))
	mux.HandleFunc("GET /v1/workspaces/{id}/files/{path...}", h(s.toWorkspace))

	mux.HandleFunc("GET /v1/commands", h(s.commands))
	mux.HandleFunc("GET /v1/commands/{id}", h(s.toCommand))
	mux.HandleFunc("POST /v1/commands/{id}/stop", h(s.toCommand))
	mux.HandleFunc("GET /v1/commands/{id}/log", h(s.toCommand))
	mux.HandleFunc("GET /v1/commands/{id}/lines", h(s.toCommand))

	mux.HandleFunc("GET /v1/ui/snapshot", h(s.snapshot))
	if s.cfg.UI != nil {
		mux.Handle("GET /", s.cfg.UI)
	}
	return httpx.Owner(s.cfg.Identifier, s.cfg.Cluster.Owner, mux)
}

func (s *Server) join(w http.ResponseWriter, r *http.Request) error {
	peer := httpx.Peer(r.Context())
	node, err := s.cfg.Store.Join(r.Context(), peer, s.cfg.Now())
	if err != nil {
		return err
	}
	slog.Info("node joined", "node", peer.Hostname, "id", peer.NodeID)
	return httpx.JSON(w, api.JoinResponse{ClusterID: s.cfg.Cluster.ClusterID, Node: s.withOnline(node)})
}

func (s *Server) report(w http.ResponseWriter, r *http.Request) error {
	peer := httpx.Peer(r.Context())
	var rep api.Report
	if err := httpx.Decode(r, &rep); err != nil {
		return err
	}
	if rep.ClusterID != s.cfg.Cluster.ClusterID {
		return api.Errorf(api.CodeClusterMismatch, "%s joined cluster %s, but this controller runs cluster %s", peer.Hostname, rep.ClusterID, s.cfg.Cluster.ClusterID).
			WithSuggestion("join this cluster with a new state directory: hotbed join CONTROLLER DIR")
	}
	ack, err := s.cfg.Store.Ingest(r.Context(), peer.NodeID, rep, s.cfg.Now())
	if errors.Is(err, errNotJoined) {
		return api.Errorf(api.CodeNotJoined, "%s has not joined this cluster", peer.Hostname).
			WithSuggestion("join with a new state directory: hotbed join CONTROLLER DIR")
	}
	if err != nil {
		return err
	}
	repos, err := s.cfg.Store.Repos(r.Context())
	if err != nil {
		return err
	}
	hosted := []api.Repo{}
	for _, repo := range repos {
		if len(repo.Hosts) == 0 || slices.Contains(repo.Hosts, peer.Hostname) {
			hosted = append(hosted, repo)
		}
	}
	return httpx.JSON(w, api.ReportReply{Ack: ack, Repos: hosted})
}

func (s *Server) nodes(w http.ResponseWriter, r *http.Request) error {
	rows, err := s.cfg.Store.Nodes(r.Context())
	if err != nil {
		return err
	}
	nodes := make([]api.Node, len(rows))
	for i, n := range rows {
		nodes[i] = s.withOnline(n.Node)
	}
	return httpx.JSON(w, api.NodesResponse{ClusterID: s.cfg.Cluster.ClusterID, Nodes: nodes})
}

func (s *Server) withOnline(n api.Node) api.Node {
	n.Online = n.LastSeen != nil && s.cfg.Now().Sub(*n.LastSeen) <= api.OnlineWindow
	return n
}

var repoID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

func (s *Server) repos(w http.ResponseWriter, r *http.Request) error {
	repos, err := s.cfg.Store.Repos(r.Context())
	if err != nil {
		return err
	}
	return httpx.JSON(w, repos)
}

func (s *Server) putRepo(w http.ResponseWriter, r *http.Request) error {
	var repo api.Repo
	if err := httpx.Decode(r, &repo); err != nil {
		return err
	}
	if !repoID.MatchString(repo.ID) {
		return api.Errorf(api.CodeInvalidArgs, "repo name %q must be lowercase letters, digits and dashes", repo.ID)
	}
	if repo.URL == "" {
		return api.Errorf(api.CodeInvalidArgs, "repo %s has no URL", repo.ID)
	}
	if repo.Freshen == nil {
		repo.Freshen = []string{}
	}
	if repo.Hosts == nil {
		repo.Hosts = []string{}
	}
	repo.CreatedAt = s.cfg.Now().UTC()
	if err := s.cfg.Store.PutRepo(r.Context(), repo); err != nil {
		return err
	}
	return httpx.JSON(w, repo)
}

func (s *Server) deleteRepo(w http.ResponseWriter, r *http.Request) error {
	found, err := s.cfg.Store.DeleteRepo(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	if !found {
		return api.Errorf(api.CodeNotFound, "no repo %s", r.PathValue("id"))
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// workspaces lists live workspaces as their nodes last reported them.
func (s *Server) workspaces(w http.ResponseWriter, r *http.Request) error {
	rows, err := s.cfg.Store.Nodes(r.Context())
	if err != nil {
		return err
	}
	out := []api.Workspace{}
	for _, n := range rows {
		if n.Report == nil {
			continue
		}
		for _, ws := range n.Report.Workspaces {
			if ws.State != api.WorkspaceReleased {
				out = append(out, ws)
			}
		}
	}
	slices.SortFunc(out, func(a, b api.Workspace) int { return strings.Compare(a.CreatedAt, b.CreatedAt) })
	return httpx.JSON(w, out)
}

// commands lists live and just-ended commands across the cluster.
func (s *Server) commands(w http.ResponseWriter, r *http.Request) error {
	rows, err := s.cfg.Store.Nodes(r.Context())
	if err != nil {
		return err
	}
	out := []api.Command{}
	for _, n := range rows {
		if n.Report != nil {
			out = append(out, n.Report.Commands...)
		}
	}
	slices.SortFunc(out, func(a, b api.Command) int { return strings.Compare(a.QueuedAt, b.QueuedAt) })
	return httpx.JSON(w, out)
}

// createWorkspace places a workspace on a node with a warm base of its repo,
// preferring the most free memory, and has that node fork it.
func (s *Server) createWorkspace(w http.ResponseWriter, r *http.Request) error {
	var req api.CreateWorkspace
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	if req.Agent == "" {
		req.Agent = httpx.Peer(r.Context()).Hostname
	}
	node, err := s.place(r, req)
	if err != nil {
		return err
	}
	id := "ws-" + randomHex(3)
	if err := s.cfg.Store.AddWorkspace(r.Context(), id, node.ID, req.Repo, req.Agent, s.cfg.Now()); err != nil {
		return err
	}
	var ws api.Workspace
	if err := s.call(r, node.Hostname, http.MethodPost, "/v1/workspaces/"+id, req, &ws); err != nil {
		if derr := s.cfg.Store.DropWorkspace(r.Context(), id); derr != nil {
			slog.Error("forget failed workspace", "workspace", id, "err", derr)
		}
		return err
	}
	return httpx.JSON(w, ws)
}

func (s *Server) place(r *http.Request, req api.CreateWorkspace) (nodeRow, error) {
	rows, err := s.cfg.Store.Nodes(r.Context())
	if err != nil {
		return nodeRow{}, err
	}
	var best *nodeRow
	var bestFree int64
	building := ""
	for i := range rows {
		n := rows[i]
		if !s.withOnline(n.Node).Online || n.Report == nil {
			continue
		}
		if req.Node != "" && n.Hostname != req.Node {
			continue
		}
		warm := false
		for _, b := range n.Report.Bases {
			if b.RepoID != req.Repo {
				continue
			}
			if b.State == api.BaseWarm {
				warm = true
			}
			if b.State == api.BaseBuilding && building == "" {
				building = fmt.Sprintf("g%d on %s is %.0f%% freshened", b.Generation, n.Hostname, 100*b.Progress)
			}
		}
		if !warm {
			continue
		}
		free := n.Report.Resources.MemoryBytes - max(n.Report.Usage.MemoryUsedBytes, n.Report.Reserved)
		if best == nil || free > bestFree {
			best, bestFree = &rows[i], free
		}
	}
	if best != nil {
		return *best, nil
	}
	repos, err := s.cfg.Store.Repos(r.Context())
	if err != nil {
		return nodeRow{}, err
	}
	if !slices.ContainsFunc(repos, func(x api.Repo) bool { return x.ID == req.Repo }) {
		return nodeRow{}, api.Errorf(api.CodeNotFound, "no repo %s", req.Repo).WithSuggestion("hotbed repo add NAME URL --freshen CMD")
	}
	e := &api.Error{Code: api.CodeNoWarmBase, Message: "no online node has a warm base of " + req.Repo, Retryable: true}
	if req.Node != "" {
		e.Message += " on " + req.Node
	}
	if building != "" {
		e.Suggestion = "wait: " + building
	} else {
		e.Suggestion = "check hotbed nodes and the repo's freshen commands in the cockpit"
	}
	return nodeRow{}, e
}

func (s *Server) releaseWorkspace(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	node, err := s.nodeOf(r, s.cfg.Store.WorkspaceNode, id)
	if err != nil {
		return err
	}
	if err := s.call(r, node, http.MethodDelete, "/v1/workspaces/"+id, nil, nil); err != nil {
		return err
	}
	if err := s.cfg.Store.ReleaseWorkspace(r.Context(), id, s.cfg.Now()); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) runCommand(w http.ResponseWriter, r *http.Request) error {
	ws := r.PathValue("id")
	var req api.RunCommand
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	nodeID, err := s.cfg.Store.WorkspaceNode(r.Context(), ws)
	if err != nil {
		return err
	}
	host, err := s.hostname(r, nodeID)
	if err != nil {
		return err
	}
	id := "c-" + randomHex(4)
	if err := s.cfg.Store.AddCommand(r.Context(), id, nodeID, ws, s.cfg.Now()); err != nil {
		return err
	}
	var c api.Command
	if err := s.call(r, host, http.MethodPost, "/v1/workspaces/"+ws+"/commands/"+id, req, &c); err != nil {
		if derr := s.cfg.Store.DropCommand(r.Context(), id); derr != nil {
			slog.Error("forget failed command", "command", id, "err", derr)
		}
		return err
	}
	return httpx.JSON(w, c)
}

func (s *Server) toWorkspace(w http.ResponseWriter, r *http.Request) error {
	node, err := s.nodeOf(r, s.cfg.Store.WorkspaceNode, r.PathValue("id"))
	if err != nil {
		return err
	}
	s.proxy(w, r, node)
	return nil
}

func (s *Server) toCommand(w http.ResponseWriter, r *http.Request) error {
	node, err := s.nodeOf(r, s.cfg.Store.CommandNode, r.PathValue("id"))
	if err != nil {
		return err
	}
	s.proxy(w, r, node)
	return nil
}

// nodeOf looks up the node an ID belongs to and returns its host name.
func (s *Server) nodeOf(r *http.Request, lookup func(ctx context.Context, id string) (string, error), id string) (string, error) {
	nodeID, err := lookup(r.Context(), id)
	if err != nil {
		return "", err
	}
	return s.hostname(r, nodeID)
}

func (s *Server) hostname(r *http.Request, nodeID string) (string, error) {
	n, err := s.cfg.Store.node(r.Context(), nodeID)
	if err != nil {
		return "", err
	}
	if !s.withOnline(n.Node).Online {
		return "", &api.Error{Code: api.CodeWorkerUnreachable, Message: n.Hostname + " is offline", Retryable: true}
	}
	return n.Hostname, nil
}

// proxy passes the request to the worker on host as is; output streams
// through as the worker writes it.
func (s *Server) proxy(w http.ResponseWriter, r *http.Request, host string) {
	target := &url.URL{Scheme: "http", Host: s.cfg.WorkerAddr(host)}
	p := &httputil.ReverseProxy{
		Rewrite:       func(pr *httputil.ProxyRequest) { pr.SetURL(target) },
		FlushInterval: -1,
		Transport:     s.cfg.Workers.Transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			httpx.Error(w, &api.Error{Code: api.CodeWorkerUnreachable, Message: fmt.Sprintf("%s: %v", host, err), Retryable: true})
		},
	}
	p.ServeHTTP(w, r)
}

// call calls a worker's API and decodes its reply.
func (s *Server) call(r *http.Request, host, method, path string, in, out any) error {
	c, err := api.NewClientPort(s.cfg.WorkerAddr(host), api.WorkerPort, s.cfg.Workers)
	if err != nil {
		return err
	}
	err = c.Do(r.Context(), method, path, in, out)
	var e *api.Error
	if errors.As(err, &e) && e.Code == api.CodeControllerUnreachable {
		return &api.Error{Code: api.CodeWorkerUnreachable, Message: host + ": " + e.Message, Retryable: true}
	}
	return err
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
