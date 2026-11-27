// Package httpx is what the controller's and workers' servers share: the
// caller a transport has already authenticated, carried in a request's
// context, and Connect servers that send errors with hotbed's codes, refuse
// requests that set a client's local field, and refuse any call that arrives
// with no caller.
//
// A worker listens on no TCP address. Its callers arrive over its outbound
// WebSocket tunnel to the controller, whose token says they are the
// controller, or over its Unix socket, whose file mode (0600) says who may
// connect; each transport names the caller with WithPeer or WithStaticPeer.
package httpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
)

// Peer is who is calling, as the transport that carried the call vouches.
type Peer struct {
	NodeID   string // the node's ID, as the controller names it
	Hostname string // its host name, without a domain
	Login    string // its owner's login, such as user@example.com
}

type peerKey struct{}

// PeerFrom is the caller of a request, the zero Peer when none was named.
func PeerFrom(ctx context.Context) Peer {
	p, _ := ctx.Value(peerKey{}).(Peer)
	return p
}

// WithPeer returns a child context carrying p as the caller.
func WithPeer(ctx context.Context, p Peer) context.Context {
	return context.WithValue(ctx, peerKey{}, p)
}

// WithStaticPeer serves next with every request's caller p: for a listener
// whose callers are all one, such as a Unix socket only its owner may
// connect to.
func WithStaticPeer(p Peer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(WithPeer(r.Context(), p)))
	})
}

// NewServer returns a Connect server that sends every handler error with
// its hotbed code, suggestion and whether to retry, refuses any call whose
// transport named no caller (WithPeer), and refuses requests that set a
// local field; more wrap the handlers inside those.
func NewServer(more ...connect.ServerInterceptor) *connect.Server {
	return connect.NewServer(append([]connect.ServerInterceptor{hotbedErrors, identified, noLocalFields}, more...)...)
}

// noLocalFields refuses each request that sets a local field: the
// client's alone, which its commands clear before the call.
func noLocalFields(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		return next(ctx, spec, localsRefused{stream})
	}
}

type localsRefused struct{ connect.ServerStream }

func (s localsRefused) Receive(msg any) error {
	if err := s.ServerStream.Receive(msg); err != nil {
		return err
	}
	m, ok := msg.(proto.Message)
	if !ok {
		return fmt.Errorf("received a %T, which is no protobuf message", msg)
	}
	return api.RefuseLocalFields(m.ProtoReflect())
}

func hotbedErrors(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		err := next(ctx, spec, stream)
		if err == nil {
			return nil
		}
		if connect.CodeOf(err) == connect.CodeUnimplemented {
			return err
		}
		if e := api.FromConnect(err); e.Code == api.CodeInternal && !isRequestCanceled(ctx, err) {
			slog.Error("call failed", "procedure", spec.Procedure, "err", err)
		}
		return api.ToConnect(err)
	}
}

// isRequestCanceled reports whether err resulted from the caller (or server
// shutdown) canceling the request context, rather than an internal server bug.
func isRequestCanceled(ctx context.Context, err error) bool {
	if !errors.Is(ctx.Err(), context.Canceled) {
		return false
	}
	return errors.Is(err, context.Canceled) ||
		connect.CodeOf(err) == connect.CodeCanceled ||
		strings.Contains(err.Error(), "context canceled")
}

// identified fails closed: a server wired without a transport that names
// its caller serves no one.
func identified(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		if _, ok := ctx.Value(peerKey{}).(Peer); !ok {
			return api.Errorf(api.CodeNotAuthorized, "this server admits no one: its transport named no caller")
		}
		return next(ctx, spec, stream)
	}
}

// Protocols serves HTTP/1.1 and, without TLS, HTTP/2: the worker's
// listeners are a local Unix socket and an in-process tunnel, and HTTP/2
// lets streams share a connection.
func Protocols() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return p
}
