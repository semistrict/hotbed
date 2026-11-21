// Package controller is the node that admits workers and keeps the cluster's
// state.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/tailnet"
)

// Identifier names the tailnet node behind a connection.
type Identifier interface {
	WhoIs(ctx context.Context, remoteAddr string) (tailnet.Peer, error)
}

// ServerConfig is what a Server needs.
type ServerConfig struct {
	Store      *Store
	Identifier Identifier
	Cluster    statedir.ControllerConfig
	Now        func() time.Time // time.Now when nil
	// UI serves every path outside /v1; none when nil.
	UI http.Handler
}

// Server serves the controller's HTTP API.
type Server struct {
	cfg ServerConfig
}

func NewServer(cfg ServerConfig) *Server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Server{cfg: cfg}
}

// Handler routes the API. Every route requires a caller the tailnet says is
// owned by the cluster's owner.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/join", s.authorized(s.join))
	mux.HandleFunc("POST /v1/heartbeat", s.authorized(s.heartbeat))
	mux.HandleFunc("GET /v1/nodes", s.authorized(s.nodes))
	if s.cfg.UI != nil {
		mux.HandleFunc("GET /", s.authorized(func(w http.ResponseWriter, r *http.Request, _ tailnet.Peer) error {
			s.cfg.UI.ServeHTTP(w, r)
			return nil
		}))
	}
	return mux
}

type peerHandler func(w http.ResponseWriter, r *http.Request, peer tailnet.Peer) error

func (s *Server) authorized(h peerHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		peer, err := s.cfg.Identifier.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil {
			slog.Warn("refused caller the tailnet does not know", "remote", r.RemoteAddr, "err", err)
			writeError(w, http.StatusUnauthorized, api.Errorf(api.CodeNotAuthorized, "the tailnet does not know the caller at %s", r.RemoteAddr).
				WithSuggestion("call the controller over its tailnet address"))
			return
		}
		if peer.Login != s.cfg.Cluster.Owner {
			slog.Warn("refused caller owned by another user", "peer", peer.Hostname, "login", peer.Login)
			writeError(w, http.StatusForbidden, api.Errorf(api.CodeNotAuthorized, "%s is owned by %s; this cluster admits only nodes owned by %s", peer.Hostname, peer.Login, s.cfg.Cluster.Owner))
			return
		}
		if err := h(w, r, peer); err != nil {
			var apiErr *api.Error
			if errors.As(err, &apiErr) {
				writeError(w, statusOf(apiErr.Code), apiErr)
				return
			}
			slog.Error("request failed", "method", r.Method, "path", r.URL.Path, "peer", peer.Hostname, "err", err)
			writeError(w, http.StatusInternalServerError, &api.Error{Code: api.CodeInternal, Message: err.Error(), Retryable: true})
		}
	}
}

func (s *Server) join(w http.ResponseWriter, r *http.Request, peer tailnet.Peer) error {
	node, err := s.cfg.Store.Join(r.Context(), peer, s.cfg.Now())
	if err != nil {
		return err
	}
	slog.Info("node joined", "node", peer.Hostname, "id", peer.NodeID)
	return writeJSON(w, api.JoinResponse{ClusterID: s.cfg.Cluster.ClusterID, Node: s.withOnline(node)})
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request, peer tailnet.Peer) error {
	var hb api.Heartbeat
	if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
		return api.Errorf(api.CodeInvalidArgs, "decode heartbeat: %v", err)
	}
	if hb.ClusterID != s.cfg.Cluster.ClusterID {
		return api.Errorf(api.CodeClusterMismatch, "%s joined cluster %s, but this controller runs cluster %s", peer.Hostname, hb.ClusterID, s.cfg.Cluster.ClusterID).
			WithSuggestion("join this cluster with a new state directory: hotbed join CONTROLLER DIR")
	}
	err := s.cfg.Store.Heartbeat(r.Context(), peer.NodeID, hb.Resources, s.cfg.Now())
	if errors.Is(err, errNotJoined) {
		return api.Errorf(api.CodeNotJoined, "%s has not joined this cluster", peer.Hostname).
			WithSuggestion("join with a new state directory: hotbed join CONTROLLER DIR")
	}
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) nodes(w http.ResponseWriter, r *http.Request, _ tailnet.Peer) error {
	nodes, err := s.cfg.Store.Nodes(r.Context())
	if err != nil {
		return err
	}
	for i := range nodes {
		nodes[i] = s.withOnline(nodes[i])
	}
	return writeJSON(w, api.NodesResponse{ClusterID: s.cfg.Cluster.ClusterID, Nodes: nodes})
}

func (s *Server) withOnline(n api.Node) api.Node {
	n.Online = n.LastSeen != nil && s.cfg.Now().Sub(*n.LastSeen) <= api.OnlineWindow
	return n
}

func statusOf(c api.Code) int {
	switch c {
	case api.CodeInvalidArgs:
		return http.StatusBadRequest
	case api.CodeNotAuthorized, api.CodeNotJoined:
		return http.StatusForbidden
	case api.CodeClusterMismatch:
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func writeJSON(w http.ResponseWriter, v any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, e *api.Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(e); err != nil {
		slog.Warn("write error reply", "err", err)
	}
}
