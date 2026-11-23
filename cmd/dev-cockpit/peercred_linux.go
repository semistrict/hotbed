//go:build linux

package main

import (
	"net/netip"

	"github.com/semistrict/hotbed/internal/peercred"
)

func lookupSocketUID(client, server netip.AddrPort) (uint32, error) {
	return peercred.SocketUID(client, server)
}
