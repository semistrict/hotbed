// Package peercred tells which unix user on this machine opened a TCP
// connection, from the kernel's socket tables.
package peercred

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// socketUIDFrom finds, in /proc/net/tcp or tcp6's text, the unix user that
// owns the socket at client connected to server: the calling end of a
// connection made on this machine.
func socketUIDFrom(table []byte, client, server netip.AddrPort) (uint32, bool) {
	sc := bufio.NewScanner(bytes.NewReader(table))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 8 || f[0] == "sl" {
			continue
		}
		local, ok1 := procAddr(f[1])
		remote, ok2 := procAddr(f[2])
		if !ok1 || !ok2 || local != client || remote != server {
			continue
		}
		uid, err := strconv.ParseUint(f[7], 10, 32)
		if err != nil {
			return 0, false
		}
		return uint32(uid), true
	}
	return 0, false
}

// procAddr reads /proc/net/tcp's ADDR:PORT, the address as 32-bit words in
// the host's byte order (little-endian on the machines hotbed runs on).
func procAddr(s string) (netip.AddrPort, bool) {
	a, p, ok := strings.Cut(s, ":")
	if !ok {
		return netip.AddrPort{}, false
	}
	raw, err := hex.DecodeString(a)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return netip.AddrPort{}, false
	}
	for i := 0; i < len(raw); i += 4 {
		binary.BigEndian.PutUint32(raw[i:], binary.LittleEndian.Uint32(raw[i:]))
	}
	port, err := strconv.ParseUint(p, 16, 16)
	if err != nil {
		return netip.AddrPort{}, false
	}
	addr, _ := netip.AddrFromSlice(raw)
	return netip.AddrPortFrom(addr.Unmap(), uint16(port)), true
}

var errNoSocket = fmt.Errorf("no such connection in /proc/net/tcp")
