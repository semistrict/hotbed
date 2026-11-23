package peercred

import (
	"net/netip"
	"testing"
)

// /proc/net/tcp on a little-endian machine: 100.73.156.10:7120 serving
// itself from port 51000, its client opened by uid 1000; and tcp6's form.
const tcp = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0A9C4964:1BD0 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0
   1: 0A9C4964:C738 0A9C4964:1BD0 01 00000000:00000000 00:00000000 00000000  1000        0 2 1 0000000000000000 20 4 30 10 -1
   2: 0A9C4964:1BD0 0A9C4964:C738 01 00000000:00000000 00:00000000 00000000     0        0 3 1 0000000000000000 20 4 30 10 -1
`

const tcp6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 5C117AFD0000E0A1000000000B9C3531:C738 5C117AFD0000E0A1000000000B9C3531:1BD0 01 00000000:00000000 00:00000000 00000000  1001        0 4 1 0000000000000000 20 4 30 10 -1
`

func TestSocketUIDFrom(t *testing.T) {
	server := netip.MustParseAddrPort("100.73.156.10:7120")
	client := netip.MustParseAddrPort("100.73.156.10:51000")
	if uid, ok := socketUIDFrom([]byte(tcp), client, server); !ok || uid != 1000 {
		t.Errorf("uid %d, %t; want the client's 1000", uid, ok)
	}
	server6 := netip.MustParseAddrPort("[fd7a:115c:a1e0::3135:9c0b]:7120")
	client6 := netip.MustParseAddrPort("[fd7a:115c:a1e0::3135:9c0b]:51000")
	if uid, ok := socketUIDFrom([]byte(tcp6), client6, server6); !ok || uid != 1001 {
		t.Errorf("tcp6 uid %d, %t; want 1001", uid, ok)
	}
	if _, ok := socketUIDFrom([]byte(tcp), netip.MustParseAddrPort("100.73.156.10:51001"), server); ok {
		t.Error("found a connection that is not there")
	}
}
