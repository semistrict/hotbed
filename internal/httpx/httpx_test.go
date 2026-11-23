package httpx

import (
	"bytes"
	"connectrpc.com/connect/v2"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/tailnet"
)

// fakeNet is a tailnet of nodes by address, its own machine at 100.64.0.1.
type fakeNet struct {
	peers map[netip.Addr]tailnet.Peer
	uid   uint32
	uidOK bool
}

func (n fakeNet) WhoIs(_ context.Context, remote string) (tailnet.Peer, error) {
	ap, err := netip.ParseAddrPort(remote)
	if err != nil {
		return tailnet.Peer{}, err
	}
	p, ok := n.peers[ap.Addr()]
	if !ok {
		return tailnet.Peer{}, errors.New("unknown node")
	}
	return p, nil
}

func (fakeNet) InTailnet(a netip.Addr) bool { return tailnet.InTailnet(a) }

func (n fakeNet) LocalUID(netip.AddrPort, netip.AddrPort) (uint32, error) {
	if !n.uidOK {
		return 0, errors.New("no such socket")
	}
	return n.uid, nil
}

var (
	self   = netip.MustParseAddr("100.64.0.1")
	laptop = netip.MustParseAddr("100.64.0.2")
)

func TestGateAdmitsOnlyTheOwnerOverTheTailnet(t *testing.T) {
	peers := map[netip.Addr]tailnet.Peer{
		self:                               {NodeID: "nSelf", Hostname: "ubu1", Login: "ramon@"},
		laptop:                             {NodeID: "nLaptop", Hostname: "laptop", Login: "ramon@"},
		netip.MustParseAddr("100.64.0.3"):  {NodeID: "nMallory", Hostname: "mallory", Login: "mallory@"},
		netip.MustParseAddr("100.64.0.4"):  {NodeID: "nTagged", Hostname: "ci", Login: "tagged-devices", Tags: []string{"tag:ci"}},
		netip.MustParseAddr("100.64.0.5"):  {NodeID: "nShared", Hostname: "friend", Login: "ramon@", Shared: true},
		netip.MustParseAddr("100.64.0.6"):  {NodeID: "nOwnTagged", Hostname: "srv", Login: "ramon@", Tags: []string{"tag:srv"}},
		netip.MustParseAddr("192.168.1.9"): {NodeID: "nLan", Hostname: "lan", Login: "ramon@"},
	}
	owner := Policy{Owner: "ramon@", Self: []netip.Addr{self}, LocalUsers: []uint32{1000}}
	controllerOnly := Policy{Owner: "ramon@", Self: []netip.Addr{self}, Nodes: []string{"nSelf"}}
	for _, tc := range []struct {
		name          string
		policy        Policy
		remote, local string
		uid           uint32
		uidOK         bool
		status        int
	}{
		{"the owner's laptop", owner, "100.64.0.2:5000", "100.64.0.1:7120", 0, false, 200},
		{"another user's node", owner, "100.64.0.3:5000", "100.64.0.1:7120", 0, false, 403},
		{"a tagged node", owner, "100.64.0.4:5000", "100.64.0.1:7120", 0, false, 403},
		{"the owner's tagged node", owner, "100.64.0.6:5000", "100.64.0.1:7120", 0, false, 403},
		{"a node shared in", owner, "100.64.0.5:5000", "100.64.0.1:7120", 0, false, 403},
		{"an address off the tailnet", owner, "192.168.1.9:5000", "100.64.0.1:7120", 0, false, 401},
		{"a node the tailnet does not know", owner, "100.64.0.9:5000", "100.64.0.1:7120", 0, false, 401},
		{"a request on another of the machine's addresses", owner, "100.64.0.2:5000", "127.0.0.1:7120", 0, false, 403},
		{"root on this machine", owner, "100.64.0.1:5000", "100.64.0.1:7120", 0, true, 200},
		{"the local user on this machine", owner, "100.64.0.1:5000", "100.64.0.1:7120", 1000, true, 200},
		{"another unix user on this machine", owner, "100.64.0.1:5000", "100.64.0.1:7120", 1001, true, 403},
		{"a local caller no one can tell", owner, "100.64.0.1:5000", "100.64.0.1:7120", 0, false, 403},
		{"the controller, to a worker", controllerOnly, "100.64.0.1:5000", "100.64.0.1:7121", 0, true, 200},
		{"the owner's laptop, to a worker", controllerOnly, "100.64.0.2:5000", "100.64.0.1:7121", 0, false, 403},
		{"a server with no policy", Policy{}, "100.64.0.2:5000", "100.64.0.1:7120", 0, false, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := Gate(fakeNet{peers: peers, uid: tc.uid, uidOK: tc.uidOK}, tc.policy, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, "in as "+Peer(r.Context()).Hostname)
			}))
			r := httptest.NewRequest("GET", "http://100.64.0.1:7120/", nil)
			r.RemoteAddr = tc.remote
			local, _ := net.ResolveTCPAddr("tcp", tc.local)
			r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, local))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Errorf("status %d, want %d: %s", w.Code, tc.status, w.Body)
			}
		})
	}
}

func TestGateBlocksDNSRebindingAndCrossSiteBrowserRequests(t *testing.T) {
	peers := map[netip.Addr]tailnet.Peer{
		laptop: {NodeID: "nLaptop", Hostname: "laptop", Login: "ramon@"},
	}
	policy := Policy{
		Owner: "ramon@",
		Self:  []netip.Addr{self},
		Hosts: []string{"ubu1", "ubu1.tail1234.ts.net"},
	}
	h := Gate(fakeNet{peers: peers}, policy, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	for _, tc := range []struct {
		name   string
		host   string
		site   string
		origin string
		status int
	}{
		{"CLI with IP host", "100.64.0.1:7120", "", "", 200},
		{"CLI with bare IP host", "100.64.0.1", "", "", 200},
		{"CLI with short MagicDNS host", "ubu1:7120", "", "", 200},
		{"CLI with full MagicDNS host", "ubu1.tail1234.ts.net:7120", "", "", 200},
		{"uppercase host with trailing dot", "UBU1.TAIL1234.TS.NET.:7120", "", "", 200},
		{"DNS rebinding host", "evil.example:7120", "", "", 403},
		{"empty host", "", "", "", 403},
		{"browser top-level navigation", "ubu1.tail1234.ts.net:7120", "none", "", 200},
		{"browser same-origin fetch", "ubu1.tail1234.ts.net:7120", "same-origin", "http://ubu1.tail1234.ts.net:7120", 200},
		{"browser cross-site fetch", "ubu1.tail1234.ts.net:7120", "cross-site", "https://evil.example", 403},
		{"browser same-site sibling tailnet host", "ubu1.tail1234.ts.net:7120", "same-site", "http://other.tail1234.ts.net:7120", 403},
		{"browser null origin", "ubu1.tail1234.ts.net:7120", "same-origin", "null", 403},
		{"browser mismatched origin without Sec-Fetch-Site", "ubu1.tail1234.ts.net:7120", "", "https://evil.example", 403},
		{"browser mismatched port in origin", "ubu1.tail1234.ts.net:7120", "same-origin", "http://ubu1.tail1234.ts.net:9999", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://100.64.0.1:7120/hotbed.v1.WorkspaceService/RunCommand", nil)
			r.Host = tc.host
			r.RemoteAddr = "100.64.0.2:5000"
			if tc.site != "" {
				r.Header.Set("Sec-Fetch-Site", tc.site)
			}
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			local, _ := net.ResolveTCPAddr("tcp", "100.64.0.1:7120")
			r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, local))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Errorf("status %d, want %d: %s", w.Code, tc.status, w.Body)
			}
		})
	}
}

// A Connect server wired without the gate serves no one.
func TestServerWithoutAGateRefuses(t *testing.T) {
	called := false
	next := gated(func(context.Context, connect.Spec, connect.ServerStream) error { called = true; return nil })
	if err := next(context.Background(), connect.Spec{}, nil); err == nil || called {
		t.Errorf("ungated call: err %v, handler called %t", err, called)
	}
}

// Client-canceled requests do not emit ERROR call failed logs, while genuine
// internal errors (and bugs returning context.Canceled when ctx is not
// canceled) still do (board 7699).
func TestHotbedErrorsSkipsClientCanceledLogs(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	canceledCtx, cancel := context.WithCancel(t.Context())
	cancel()

	spec := connect.Spec{Procedure: "/hotbed.v1.ClusterService/GetSnapshot"}

	for _, tc := range []struct {
		name    string
		ctx     context.Context
		err     error
		wantLog bool
	}{
		{"raw context.Canceled with canceled ctx", canceledCtx, context.Canceled, false},
		{"wrapped context.Canceled with canceled ctx", canceledCtx, fmt.Errorf("list nodes: %w", context.Canceled), false},
		{"forwarded worker canceled with canceled ctx", canceledCtx, api.Errorf(api.CodeInternal, "canceled: context canceled"), false},
		{"connect.CodeCanceled with canceled ctx", canceledCtx, connect.NewError(connect.CodeCanceled, "gone"), false},
		{"genuine internal error with canceled ctx", canceledCtx, errors.New("database disk image is malformed"), true},
		{"context.Canceled when request ctx is still live", t.Context(), context.Canceled, true},
		{"genuine internal error with live ctx", t.Context(), errors.New("boom"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs.Reset()
			fn := hotbedErrors(func(context.Context, connect.Spec, connect.ServerStream) error {
				return tc.err
			})
			_ = fn(tc.ctx, spec, nil)
			gotLog := strings.Contains(logs.String(), "call failed")
			if gotLog != tc.wantLog {
				t.Errorf("logged=%v, want %v (logs=%q)", gotLog, tc.wantLog, logs.String())
			}
		})
	}
}

type fakeConn struct {
	net.Conn
	local, remote net.Addr
}

func (c fakeConn) LocalAddr() net.Addr  { return c.local }
func (c fakeConn) RemoteAddr() net.Addr { return c.remote }

func TestAdmitConnMatchesGatePolicy(t *testing.T) {
	peers := map[netip.Addr]tailnet.Peer{
		self:                               {NodeID: "nSelf", Hostname: "ubu1", Login: "ramon@"},
		laptop:                             {NodeID: "nLaptop", Hostname: "laptop", Login: "ramon@"},
		netip.MustParseAddr("100.64.0.3"):  {NodeID: "nMallory", Hostname: "mallory", Login: "mallory@"},
		netip.MustParseAddr("100.64.0.4"):  {NodeID: "nTagged", Hostname: "ci", Login: "tagged-devices", Tags: []string{"tag:ci"}},
		netip.MustParseAddr("100.64.0.5"):  {NodeID: "nShared", Hostname: "friend", Login: "ramon@", Shared: true},
		netip.MustParseAddr("192.168.1.9"): {NodeID: "nLan", Hostname: "lan", Login: "ramon@"},
	}
	owner := Policy{Owner: "ramon@", Self: []netip.Addr{self}, LocalUsers: []uint32{1000}}
	for _, tc := range []struct {
		name          string
		remote, local string
		uid           uint32
		uidOK         bool
		wantOK        bool
	}{
		{"owner laptop", "100.64.0.2:5000", "100.64.0.1:7122", 0, false, true},
		{"other user", "100.64.0.3:5000", "100.64.0.1:7122", 0, false, false},
		{"tagged node", "100.64.0.4:5000", "100.64.0.1:7122", 0, false, false},
		{"shared node", "100.64.0.5:5000", "100.64.0.1:7122", 0, false, false},
		{"non-tailnet", "192.168.1.9:5000", "100.64.0.1:7122", 0, false, false},
		{"non-self local", "100.64.0.2:5000", "127.0.0.1:7122", 0, false, false},
		{"local root", "100.64.0.1:5000", "100.64.0.1:7122", 0, true, true},
		{"local allowed user", "100.64.0.1:5000", "100.64.0.1:7122", 1000, true, true},
		{"local other user", "100.64.0.1:5000", "100.64.0.1:7122", 1001, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := net.ResolveTCPAddr("tcp", tc.remote)
			l, _ := net.ResolveTCPAddr("tcp", tc.local)
			_, err := AdmitConn(t.Context(), fakeConn{local: l, remote: r}, fakeNet{peers: peers, uid: tc.uid, uidOK: tc.uidOK}, owner)
			if (err == nil) != tc.wantOK {
				t.Errorf("AdmitConn err = %v, wantOK = %v", err, tc.wantOK)
			}
		})
	}
	if _, err := AdmitConn(t.Context(), nil, fakeNet{peers: peers}, owner); err == nil {
		t.Error("AdmitConn(nil) succeeded, want error")
	}
}

func TestGateRecordsIsLocalNonRoot(t *testing.T) {
	peers := map[netip.Addr]tailnet.Peer{
		self:   {NodeID: "nSelf", Hostname: "ubu1", Login: "ramon@"},
		laptop: {NodeID: "nLaptop", Hostname: "laptop", Login: "ramon@"},
	}
	policy := Policy{Owner: "ramon@", Self: []netip.Addr{self}, LocalUsers: []uint32{1000}}
	for _, tc := range []struct {
		name   string
		remote string
		uid    uint32
		uidOK  bool
		want   bool
	}{
		{"remote peer", "100.64.0.2:5000", 1000, true, false},
		{"local root", "100.64.0.1:5000", 0, true, false},
		{"local non-root", "100.64.0.1:5000", 1000, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got bool
			var called bool
			h := Gate(fakeNet{peers: peers, uid: tc.uid, uidOK: tc.uidOK}, policy, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				got = IsLocalNonRoot(r.Context())
			}))
			r := httptest.NewRequest("GET", "http://100.64.0.1:7120/", nil)
			r.RemoteAddr = tc.remote
			local, _ := net.ResolveTCPAddr("tcp", "100.64.0.1:7120")
			r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, local))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if !called || w.Code != http.StatusOK {
				t.Fatalf("status=%d called=%v body=%s", w.Code, called, w.Body)
			}
			if got != tc.want {
				t.Errorf("IsLocalNonRoot = %v, want %v", got, tc.want)
			}
		})
	}
}
