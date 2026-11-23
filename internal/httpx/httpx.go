// Package httpx is what the controller's and workers' servers share: a gate
// that admits only the cluster's owner over the tailnet, in front of every
// handler, and Connect servers that send errors with hotbed's codes and
// refuse requests that set a client's local field.
//
// The gate fails closed. It does not trust the host to keep strangers away
// (a firewall, the tailnet's ACLs, who else has an account on the machine):
// a request it cannot prove came from the owner, over the tailnet, gets a
// refusal before any handler runs.
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"google.golang.org/protobuf/proto"

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
	// Hosts are this node's DNS names (without port, such as "ubu1" and
	// "ubu1.tail1234.ts.net") that may appear in a request's Host header,
	// in addition to Self's IP addresses. Rejecting other hosts stops DNS
	// rebinding from a malicious webpage in the owner's browser.
	Hosts []string
	// LocalUsers are the unix users, besides root, that may call from this
	// machine itself: the tailnet says such a connection is this node's, the
	// owner's, whoever on the machine opened it.
	LocalUsers []uint32
}

type peerKey struct{}

type localCaller struct {
	local bool
	uid   uint32
}

type localCallerKey struct{}

// Gate serves next only to requests that p admits; the others get the
// reason they were refused, in the caller's protocol.
func Gate(id Identifier, p Policy, next http.Handler) http.Handler {
	errs := connecthttp.NewErrorWriter()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, lc, refusal := admit(r, id, p)
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
		ctx := context.WithValue(r.Context(), peerKey{}, peer)
		ctx = context.WithValue(ctx, localCallerKey{}, lc)
		next.ServeHTTP(w, r.WithContext(ctx))
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

// AdmitConn verifies that conn arrived from an admitted tailnet peer on one
// of p.Self's addresses, applying the same fail-closed owner and local-UID
// checks as Gate for non-HTTP protocols such as the worker's SSH server.
func AdmitConn(ctx context.Context, conn net.Conn, id Identifier, p Policy) (tailnet.Peer, error) {
	if conn == nil {
		return tailnet.Peer{}, refuse("missing connection").Error
	}
	peer, _, ref := admitAddr(ctx, conn.RemoteAddr().String(), conn.LocalAddr(), id, p)
	if ref != nil {
		return tailnet.Peer{}, ref.Error
	}
	return peer, nil
}

// admit returns who is calling, whether they are local and their UID, or why they may not.
func admit(r *http.Request, id Identifier, p Policy) (tailnet.Peer, localCaller, *refusal) {
	la, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	peer, lc, ref := admitAddr(r.Context(), r.RemoteAddr, la, id, p)
	if ref != nil {
		return tailnet.Peer{}, localCaller{}, ref
	}
	if refusal := checkBrowserRequest(r, p); refusal != nil {
		return tailnet.Peer{}, localCaller{}, refusal
	}
	return peer, lc, nil
}

func admitAddr(ctx context.Context, remoteAddr string, localAddr net.Addr, id Identifier, p Policy) (tailnet.Peer, localCaller, *refusal) {
	if p.Owner == "" || len(p.Self) == 0 {
		return tailnet.Peer{}, localCaller{}, refuse("this server has no owner or tailnet address to admit by")
	}
	remote, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return tailnet.Peer{}, localCaller{}, refuse("unreadable remote address %q", remoteAddr)
	}
	remote = netip.AddrPortFrom(remote.Addr().Unmap(), remote.Port())
	local, err := netip.ParseAddrPort(fmt.Sprint(localAddr))
	if err != nil {
		return tailnet.Peer{}, localCaller{}, refuse("unreadable local address %v", localAddr)
	}
	local = netip.AddrPortFrom(local.Addr().Unmap(), local.Port())
	if !slices.Contains(p.Self, local.Addr()) {
		return tailnet.Peer{}, localCaller{}, refuse("request arrived on %s, not this node's tailnet address", local.Addr())
	}
	if !id.InTailnet(remote.Addr()) {
		return tailnet.Peer{}, localCaller{}, unknown("%s is not a tailnet address", remote.Addr())
	}
	peer, err := id.WhoIs(ctx, remote.String())
	switch {
	case err != nil:
		return tailnet.Peer{}, localCaller{}, unknown("the tailnet does not know %s: %v", remote.Addr(), err)
	case peer.Login != p.Owner:
		return tailnet.Peer{}, localCaller{}, refuse("%s is owned by %s; this cluster admits only %s", peer.Hostname, peer.Login, p.Owner)
	case len(peer.Tags) > 0:
		return tailnet.Peer{}, localCaller{}, refuse("%s is a tagged node (%v), owned by no user", peer.Hostname, peer.Tags)
	case peer.Shared:
		return tailnet.Peer{}, localCaller{}, refuse("%s is shared in from another tailnet", peer.Hostname)
	case len(p.Nodes) > 0 && !slices.Contains(p.Nodes, peer.NodeID):
		return tailnet.Peer{}, localCaller{}, refuse("%s may not call this server; only its controller may", peer.Hostname)
	}
	var lc localCaller
	if slices.Contains(p.Self, remote.Addr()) {
		uid, err := id.LocalUID(remote, local)
		if err != nil {
			return tailnet.Peer{}, localCaller{}, refuse("cannot tell which user on this machine is calling: %v", err)
		}
		if uid != 0 && !slices.Contains(p.LocalUsers, uid) {
			return tailnet.Peer{}, localCaller{}, refuse("unix user %d on this machine may not call this server", uid)
		}
		lc = localCaller{local: true, uid: uid}
	}
	return peer, lc, nil
}

// checkBrowserRequest blocks DNS rebinding and cross-site browser requests.
// A browser runs on the owner's tailnet node, so any tab can reach this
// server with the owner's tailnet identity unless Host, Sec-Fetch-Site and
// Origin are checked.
func checkBrowserRequest(r *http.Request, p Policy) *refusal {
	if !allowedHost(r.Host, p) {
		return refuse("host %q is not this server", r.Host)
	}
	if site := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); site != "" && site != "same-origin" && site != "none" {
		return refuse("cross-site browser request (%s) is not allowed", site)
	}
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || !sameHostPort(u.Host, r.Host, u.Scheme) {
			return refuse("origin %q does not match host %q", origin, r.Host)
		}
	}
	return nil
}

func allowedHost(hostport string, p Policy) bool {
	h := stripPort(hostport)
	if h == "" {
		return false
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		return slices.Contains(p.Self, addr.Unmap())
	}
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	if h == "localhost" {
		for _, a := range p.Self {
			if a.IsLoopback() {
				return true
			}
		}
	}
	for _, allowed := range p.Hosts {
		if a := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(allowed)), "."); a != "" && h == a {
			return true
		}
	}
	return false
}

func stripPort(hostport string) string {
	hostport = strings.TrimSpace(hostport)
	if hostport == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]")
}

func sameHostPort(originHost, reqHost, scheme string) bool {
	if strings.EqualFold(originHost, reqHost) {
		return true
	}
	defaultPort := "80"
	if scheme == "https" {
		defaultPort = "443"
	}
	normalize := func(hp string) (string, string) {
		if h, port, err := net.SplitHostPort(hp); err == nil {
			return strings.TrimSuffix(strings.ToLower(h), "."), port
		}
		return strings.TrimSuffix(strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(hp, "["), "]")), "."), defaultPort
	}
	oh, op := normalize(originHost)
	rh, rp := normalize(reqHost)
	return oh == rh && op == rp
}

// Peer is the caller of a request the gate admitted.
func Peer(ctx context.Context) tailnet.Peer {
	p, _ := ctx.Value(peerKey{}).(tailnet.Peer)
	return p
}

// IsLocalNonRoot reports whether the gated request arrived from a non-root
// process on this machine itself.
func IsLocalNonRoot(ctx context.Context) bool {
	lc, _ := ctx.Value(localCallerKey{}).(localCaller)
	return lc.local && lc.uid != 0
}

// NewServer returns a Connect server that sends every handler error with
// its hotbed code, suggestion and whether to retry, and that refuses any
// call that did not come through Gate, should a server ever be wired
// without one; more wrap the handlers inside those.
func NewServer(more ...connect.ServerInterceptor) *connect.Server {
	return connect.NewServer(append([]connect.ServerInterceptor{hotbedErrors, gated, noLocalFields}, more...)...)
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
