//go:build !linux && !darwin

package peercred

import (
	"errors"
	"net/netip"
)

// SocketUID cannot tell who on this machine is calling on unsupported OSes:
// a call from this machine itself is refused.
func SocketUID(netip.AddrPort, netip.AddrPort) (uint32, error) {
	return 0, errors.New("telling which user opened a connection needs Linux or macOS")
}
