package peercred

import (
	"net/netip"
	"os"
)

// SocketUID is the unix user that opened the connection from client, on
// this machine, to server.
func SocketUID(client, server netip.AddrPort) (uint32, error) {
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(table)
		if err != nil {
			return 0, err
		}
		if uid, ok := socketUIDFrom(b, client, server); ok {
			return uid, nil
		}
	}
	return 0, errNoSocket
}
