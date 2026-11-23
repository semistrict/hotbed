// Package tailnet identifies hotbed nodes through the tailscaled running on
// the same machine. Tailscale already authenticates and encrypts every
// connection between nodes, so hotbed trusts the tailnet's answer to "who is
// this?" and needs no tokens or certificates of its own.
package tailnet

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"

	"tailscale.com/client/local"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/peercred"
)

// Peer is a tailnet node and the user who owns it.
type Peer struct {
	NodeID   string // Tailscale's stable node ID
	Hostname string // MagicDNS name, without the tailnet's domain
	Login    string // owner's login name, such as user@example.com
	// Addr is its tailnet address (IPv4 when it has one), to reach it
	// without depending on name resolution.
	Addr netip.Addr
	// Tags are its ACL tags; a tagged node belongs to no user.
	Tags []string
	// Shared says it is another tailnet's node, shared into this one.
	Shared bool
}

// Tailscale gives nodes addresses in these ranges alone: the CGNAT block and
// its IPv6 unique-local prefix.
var (
	cgnat = netip.MustParsePrefix("100.64.0.0/10")
	ula   = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// InTailnet says whether a is an address Tailscale gives its nodes.
func InTailnet(a netip.Addr) bool {
	a = a.Unmap()
	return cgnat.Contains(a) || ula.Contains(a)
}

// InTailnet says whether a is a tailnet address.
func (l *Local) InTailnet(a netip.Addr) bool { return InTailnet(a) }

// LocalUID is the unix user that opened a connection on this machine, from
// the kernel's socket tables.
func (l *Local) LocalUID(client, server netip.AddrPort) (uint32, error) {
	return peercred.SocketUID(client, server)
}

// Self is this machine's own tailnet node.
type Self struct {
	Peer
	// DNSName is its MagicDNS name, such as ubu1.tail1234.ts.net, which
	// resolves to its tailnet address even on itself, where /etc/hosts may
	// map the bare host name to loopback.
	DNSName string
	Addrs   []netip.Addr
}

// Local asks this machine's tailscaled.
type Local struct {
	client local.Client
}

// Self returns this machine's node and its tailnet addresses.
func (l *Local) Self(ctx context.Context) (Self, error) {
	st, err := l.client.Status(ctx)
	if err != nil {
		return Self{}, unavailable(err)
	}
	if st.Self == nil || len(st.Self.TailscaleIPs) == 0 {
		return Self{}, api.Errorf(api.CodeTailscaleUnavailable, "tailscaled is running but this machine is not on a tailnet (state %s)", st.BackendState).
			WithSuggestion("run `tailscale up`")
	}
	user, ok := st.User[st.Self.UserID]
	if !ok {
		return Self{}, api.Errorf(api.CodeTailscaleUnavailable, "tailscaled did not report this node's owner (user ID %d)", st.Self.UserID)
	}
	return Self{
		Peer: Peer{
			NodeID:   string(st.Self.ID),
			Hostname: firstLabel(st.Self.DNSName),
			Login:    user.LoginName,
		},
		DNSName: strings.TrimSuffix(st.Self.DNSName, "."),
		Addrs:   st.Self.TailscaleIPs,
	}, nil
}

// WhoIs returns the tailnet node behind a connection's remote address.
func (l *Local) WhoIs(ctx context.Context, remoteAddr string) (Peer, error) {
	w, err := l.client.WhoIs(ctx, remoteAddr)
	if err != nil {
		return Peer{}, fmt.Errorf("whois %s: %w", remoteAddr, err)
	}
	if w.Node == nil || w.UserProfile == nil {
		return Peer{}, fmt.Errorf("whois %s: tailscaled returned no node or owner", remoteAddr)
	}
	p := Peer{
		NodeID:   string(w.Node.StableID),
		Hostname: w.Node.ComputedName,
		Login:    w.UserProfile.LoginName,
		Tags:     w.Node.Tags,
		Shared:   w.Node.Sharer != 0,
	}
	for _, pfx := range w.Node.Addresses {
		if a := pfx.Addr(); !p.Addr.IsValid() || (a.Is4() && !p.Addr.Is4()) {
			p.Addr = a
		}
	}
	return p, nil
}

func unavailable(err error) error {
	return api.Errorf(api.CodeTailscaleUnavailable, "cannot reach tailscaled: %v", err).
		WithSuggestion("start tailscaled and check `tailscale status`")
}

// firstLabel returns "ubu1" for "ubu1.tail1234.ts.net.".
func firstLabel(fqdn string) string {
	name, _, _ := strings.Cut(fqdn, ".")
	return name
}

// Identifier is what NodeAt asks.
type Identifier interface {
	WhoIs(ctx context.Context, remoteAddr string) (Peer, error)
	InTailnet(a netip.Addr) bool
}

// NodeAt is the tailnet node at hostPort (a MagicDNS name or tailnet
// address, and a port), by the tailnet's own account: resolved to a tailnet
// address, then asked about.
func NodeAt(ctx context.Context, id Identifier, hostPort string) (Peer, error) {
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		return Peer{}, err
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return Peer{}, fmt.Errorf("resolve %s: %w", host, err)
	}
	for _, a := range addrs {
		if id.InTailnet(a) {
			return id.WhoIs(ctx, net.JoinHostPort(a.Unmap().String(), port))
		}
	}
	return Peer{}, fmt.Errorf("%s resolves to no tailnet address (%v)", host, addrs)
}
