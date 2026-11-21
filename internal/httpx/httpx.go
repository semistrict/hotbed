// Package httpx is what the controller's and workers' servers share: who is
// calling, by the tailnet's account, and Connect servers that admit only the
// cluster's owner and send errors with hotbed's codes.
package httpx

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"connectrpc.com/connect/v2"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/tailnet"
)

// Identifier names the tailnet node behind a connection.
type Identifier interface {
	WhoIs(ctx context.Context, remoteAddr string) (tailnet.Peer, error)
}

type peerKey struct{}

type caller struct {
	peer tailnet.Peer
	err  error
}

// Identify asks the tailnet who is calling and puts the answer in the
// request's context, for the owner checks below.
func Identify(id Identifier, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, err := id.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil {
			slog.Warn("caller the tailnet does not know", "remote", r.RemoteAddr, "err", err)
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), peerKey{}, caller{peer, err})))
	})
}

// Peer is the caller of a request that went through Identify.
func Peer(ctx context.Context) tailnet.Peer {
	c, _ := ctx.Value(peerKey{}).(caller)
	return c.peer
}

// checkOwner says why the caller in ctx may not use the cluster, if it may not.
func checkOwner(ctx context.Context, owner string) *api.Error {
	c, ok := ctx.Value(peerKey{}).(caller)
	switch {
	case !ok || c.err != nil:
		return &api.Error{Code: api.CodeNotAuthorized, Message: "the tailnet does not know the caller", Suggestion: "call over the tailnet address"}
	case c.peer.Login != owner:
		slog.Warn("refused caller owned by another user", "peer", c.peer.Hostname, "login", c.peer.Login)
		return api.Errorf(api.CodeNotAuthorized, "%s is owned by %s; this cluster admits only %s", c.peer.Hostname, c.peer.Login, owner)
	}
	return nil
}

// NewServer returns a Connect server that admits only owner's tailnet nodes
// and sends every handler error with its hotbed code, suggestion and
// whether to retry.
func NewServer(owner string) *connect.Server {
	return connect.NewServer(errors, ownerOnly(owner))
}

func errors(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		err := next(ctx, spec, stream)
		if err == nil {
			return nil
		}
		if e := api.FromConnect(err); e.Code == api.CodeInternal {
			slog.Error("call failed", "procedure", spec.Procedure, "err", err)
		}
		return api.ToConnect(err)
	}
}

func ownerOnly(owner string) connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			if e := checkOwner(ctx, owner); e != nil {
				return e
			}
			return next(ctx, spec, stream)
		}
	}
}

// RequireOwner admits only owner's tailnet nodes to a plain HTTP handler,
// such as the cockpit's files.
func RequireOwner(owner string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e := checkOwner(r.Context(), owner); e != nil {
			status := http.StatusForbidden
			if c, _ := r.Context().Value(peerKey{}).(caller); c.err != nil {
				status = http.StatusUnauthorized
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if err := json.NewEncoder(w).Encode(e); err != nil {
				slog.Warn("write error reply", "err", err)
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Protocols serves HTTP/1.1 and, without TLS, HTTP/2: the tailnet already
// encrypts, and HTTP/2 lets streams share a connection.
func Protocols() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return p
}
