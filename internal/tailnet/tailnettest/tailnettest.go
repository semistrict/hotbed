// Package tailnettest is a tailnet for tests: every connection runs over
// loopback, and the fake answers WhoIs from the client that made it.
package tailnettest

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sync"

	"github.com/semistrict/hotbed/internal/tailnet"
)

// Fake is a tailnet whose nodes are HTTP clients made by Client.
type Fake struct {
	self tailnet.Self
	// CallerUID is the unix user its connections were opened by; root
	// unless a test says otherwise.
	CallerUID uint32

	mu    sync.Mutex
	peers map[string]tailnet.Peer // keyed by a client connection's local address
}

func New(self tailnet.Self) *Fake {
	return &Fake{self: self, peers: map[string]tailnet.Peer{}}
}

func (f *Fake) Self(context.Context) (tailnet.Self, error) { return f.self, nil }

// InTailnet treats loopback, where the fake's connections run, as the
// tailnet.
func (f *Fake) InTailnet(a netip.Addr) bool { return a.Unmap().IsLoopback() }

// LocalUID is CallerUID: every connection of the fake's is local.
func (f *Fake) LocalUID(netip.AddrPort, netip.AddrPort) (uint32, error) { return f.CallerUID, nil }

// WhoIs returns the peer whose client made the connection from remoteAddr.
func (f *Fake) WhoIs(_ context.Context, remoteAddr string) (tailnet.Peer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.peers[remoteAddr]
	if !ok {
		return tailnet.Peer{}, fmt.Errorf("no tailnet node at %s", remoteAddr)
	}
	return p, nil
}

// Client returns an HTTP client whose connections WhoIs attributes to p. It
// records each connection before the connection carries a request.
func (f *Fake) Client(p tailnet.Peer) *http.Client {
	var d net.Dialer
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			f.mu.Lock()
			f.peers[c.LocalAddr().String()] = p
			f.mu.Unlock()
			return c, nil
		},
	}}
}
