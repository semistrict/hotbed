package httpx

import (
	"connectrpc.com/connect/v2"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

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
			r := httptest.NewRequest("GET", "/", nil)
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

// A Connect server wired without the gate serves no one.
func TestServerWithoutAGateRefuses(t *testing.T) {
	called := false
	next := gated(func(context.Context, connect.Spec, connect.ServerStream) error { called = true; return nil })
	if err := next(context.Background(), connect.Spec{}, nil); err == nil || called {
		t.Errorf("ungated call: err %v, handler called %t", err, called)
	}
}
