// Package httpx is what the controller's and workers' HTTP servers share:
// tailnet authorization, and JSON replies and errors in the API's shapes.
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/tailnet"
)

// Identifier names the tailnet node behind a connection.
type Identifier interface {
	WhoIs(ctx context.Context, remoteAddr string) (tailnet.Peer, error)
}

type peerKey struct{}

// Owner admits only callers the tailnet says are owned by owner, and puts
// the caller in the request's context.
func Owner(id Identifier, owner string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, err := id.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil {
			slog.Warn("refused caller the tailnet does not know", "remote", r.RemoteAddr, "err", err)
			ErrorStatus(w, http.StatusUnauthorized, &api.Error{Code: api.CodeNotAuthorized, Message: "the tailnet does not know the caller at " + r.RemoteAddr, Suggestion: "call over the tailnet address"})
			return
		}
		if peer.Login != owner {
			slog.Warn("refused caller owned by another user", "peer", peer.Hostname, "login", peer.Login)
			Error(w, api.Errorf(api.CodeNotAuthorized, "%s is owned by %s; this cluster admits only %s", peer.Hostname, peer.Login, owner))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), peerKey{}, peer)))
	})
}

// Peer is the authorized caller of a request that went through Owner.
func Peer(ctx context.Context) tailnet.Peer {
	p, _ := ctx.Value(peerKey{}).(tailnet.Peer)
	return p
}

// Handle adapts a handler that returns an error: an *api.Error goes back as
// is, anything else as INTERNAL.
func Handle(h func(w http.ResponseWriter, r *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			var e *api.Error
			if !errors.As(err, &e) {
				slog.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
				e = &api.Error{Code: api.CodeInternal, Message: err.Error()}
			}
			Error(w, e)
		}
	}
}

// JSON writes v.
func JSON(w http.ResponseWriter, v any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(v)
}

// Decode reads a JSON body into v.
func Decode(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return api.Errorf(api.CodeInvalidArgs, "decode request: %v", err)
	}
	return nil
}

// Error writes e with the status its code means.
func Error(w http.ResponseWriter, e *api.Error) {
	ErrorStatus(w, Status(e.Code), e)
}

// ErrorStatus writes e with status.
func ErrorStatus(w http.ResponseWriter, status int, e *api.Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(e); err != nil {
		slog.Warn("write error reply", "err", err)
	}
}

// Status is the HTTP status for an error code.
func Status(c api.Code) int {
	switch c {
	case api.CodeInvalidArgs, api.CodeSyncRefused:
		return http.StatusBadRequest
	case api.CodeNotAuthorized:
		return http.StatusForbidden
	case api.CodeNotJoined:
		return http.StatusForbidden
	case api.CodeNotFound:
		return http.StatusNotFound
	case api.CodeConflict, api.CodeClusterMismatch:
		return http.StatusConflict
	case api.CodeNoWarmBase, api.CodeWorkerUnreachable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
