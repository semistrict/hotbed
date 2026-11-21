// Package tailnet identifies hotbed nodes through the tailscaled running on
// the same machine. Tailscale already authenticates and encrypts every
// connection between nodes, so hotbed trusts the tailnet's answer to "who is
// this?" and needs no tokens or certificates of its own.
package tailnet

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"tailscale.com/client/local"

	"github.com/semistrict/hotbed/internal/api"
)

// Peer is a tailnet node and the user who owns it.
type Peer struct {
	NodeID   string // Tailscale's stable node ID
	Hostname string // MagicDNS name, without the tailnet's domain
	Login    string // owner's login name, such as user@example.com
}

// Self is this machine's own tailnet node.
type Self struct {
	Peer
	Addrs []netip.Addr
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
		Addrs: st.Self.TailscaleIPs,
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
	return Peer{
		NodeID:   string(w.Node.StableID),
		Hostname: w.Node.ComputedName,
		Login:    w.UserProfile.LoginName,
	}, nil
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
