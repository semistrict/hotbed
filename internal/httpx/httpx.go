// Package httpx is what the controller's and workers' servers share: a gate
// that admits only the cluster's owner over the tailnet, in front of every
// handler, and Connect servers that send errors with hotbed's codes.
//
// The gate fails closed. It does not trust the host to keep strangers away
// (a firewall, the tailnet's ACLs, who else has an account on the machine):
// a request it cannot prove came from the owner, over the tailnet, gets a
// refusal before any handler runs.
package httpx

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/tailnet"
)

// Identifier names the tailnet node behind a connection.
type Identifier interface {
	WhoIs(ctx context.Context, remoteAddr string) (tailnet.Peer, error)
	// InTailnet says whether an address is one the tailnet gives its nodes.
	InTailnet(a netip.Addr) bool
	// LocalUID is the unix user that opened a connection made on this
	// machine itself, from client to server.
	LocalUID(client, server netip.AddrPort) (uint32, error)
}

// Policy is who a server admits. Its zero value admits no one.
type Policy struct {
	// Owner is the cluster owner's tailnet login; only their nodes, neither
	// tagged nor shared in from another tailnet, are let in.
	Owner string
	// Nodes, when set, are the only nodes let in, by stable ID: a worker
	// admits its controller alone.
	Nodes []string
	// Self is this node's tailnet addresses, the only ones a request may
	// arrive on.
	Self []netip.Addr
	// LocalUsers are the unix users, besides root, that may call from this
	// machine itself: the tailnet says such a connection is this node's, the
	// owner's, whoever on the machine opened it.
	LocalUsers []uint32
}

type peerKey struct{}

// Gate serves next only to requests that p admits; the others get the
// reason they were refused, in the caller's protocol.
func Gate(id Identifier, p Policy, next http.Handler) http.Handler {
	errs := connecthttp.NewErrorWriter()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, refusal := admit(r, id, p)
		if refusal != nil {
			slog.Warn("refused a request", "remote", r.RemoteAddr, "path", r.URL.Path, "why", refusal.Message)
			// hotbed's Connect calls are POSTs; a GET is the cockpit's, a browser's.
			if r.Method == http.MethodPost && errs.IsSupported(r) {
				if err := errs.Write(w, r, api.ToConnect(refusal.Error)); err != nil {
					slog.Warn("write refusal", "err", err)
				}
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(refusal.status)
			if err := json.NewEncoder(w).Encode(refusal.Error); err != nil {
				slog.Warn("write refusal", "err", err)
			}
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), peerKey{}, peer)))
	})
}

// A refusal is why a request was not let in, and the HTTP status a plain
// (non-Connect) caller gets: 401 when the caller could not be identified,
// 403 when it could and may not call.
type refusal struct {
	*api.Error
	status int
}

func refuse(format string, args ...any) *refusal {
	return &refusal{api.Errorf(api.CodeNotAuthorized, format, args...), http.StatusForbidden}
}

func unknown(format string, args ...any) *refusal {
	return &refusal{api.Errorf(api.CodeNotAuthorized, format, args...), http.StatusUnauthorized}
}

// admit returns who is calling, or why they may not.
func admit(r *http.Request, id Identifier, p Policy) (tailnet.Peer, *refusal) {
	if p.Owner == "" || len(p.Self) == 0 {
		return tailnet.Peer{}, refuse("this server has no owner or tailnet address to admit by")
	}
	remote, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return tailnet.Peer{}, refuse("unreadable remote address %q", r.RemoteAddr)
	}
	remote = netip.AddrPortFrom(remote.Addr().Unmap(), remote.Port())
	la, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	local, err := netip.ParseAddrPort(fmt.Sprint(la))
	if err != nil {
		return tailnet.Peer{}, refuse("unreadable local address %v", la)
	}
	local = netip.AddrPortFrom(local.Addr().Unmap(), local.Port())
	if !slices.Contains(p.Self, local.Addr()) {
		return tailnet.Peer{}, refuse("request arrived on %s, not this node's tailnet address", local.Addr())
	}
	if !id.InTailnet(remote.Addr()) {
		return tailnet.Peer{}, unknown("%s is not a tailnet address", remote.Addr())
	}
	peer, err := id.WhoIs(r.Context(), remote.String())
	switch {
	case err != nil:
		return tailnet.Peer{}, unknown("the tailnet does not know %s: %v", remote.Addr(), err)
	case peer.Login != p.Owner:
		return tailnet.Peer{}, refuse("%s is owned by %s; this cluster admits only %s", peer.Hostname, peer.Login, p.Owner)
	case len(peer.Tags) > 0:
		return tailnet.Peer{}, refuse("%s is a tagged node (%v), owned by no user", peer.Hostname, peer.Tags)
	case peer.Shared:
		return tailnet.Peer{}, refuse("%s is shared in from another tailnet", peer.Hostname)
	case len(p.Nodes) > 0 && !slices.Contains(p.Nodes, peer.NodeID):
		return tailnet.Peer{}, refuse("%s may not call this server; only its controller may", peer.Hostname)
	}
	if slices.Contains(p.Self, remote.Addr()) {
		uid, err := id.LocalUID(remote, local)
		if err != nil {
			return tailnet.Peer{}, refuse("cannot tell which user on this machine is calling: %v", err)
		}
		if uid != 0 && !slices.Contains(p.LocalUsers, uid) {
			return tailnet.Peer{}, refuse("unix user %d on this machine may not call this server", uid)
		}
	}
	return peer, nil
}

// Peer is the caller of a request the gate admitted.
func Peer(ctx context.Context) tailnet.Peer {
	p, _ := ctx.Value(peerKey{}).(tailnet.Peer)
	return p
}

// NewServer returns a Connect server that sends every handler error with
// its hotbed code, suggestion and whether to retry, and that refuses any
// call that did not come through Gate, should a server ever be wired
// without one.
func NewServer() *connect.Server {
	return connect.NewServer(hotbedErrors, gated)
}

func hotbedErrors(next connect.ServerFunc) connect.ServerFunc {
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

func gated(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		if _, ok := ctx.Value(peerKey{}).(tailnet.Peer); !ok {
			return refuse("this server admits no one: its gate is missing").Error
		}
		return next(ctx, spec, stream)
	}
}

// Protocols serves HTTP/1.1 and, without TLS, HTTP/2: the tailnet already
// encrypts, and HTTP/2 lets streams share a connection.
func Protocols() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return p
}
