//go:build darwin && !cgo

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func lookupSocketUID(client, server netip.AddrPort) (uint32, error) {
	uid := uint32(os.Getuid())
	out, err := exec.Command("lsof", "-nP", "-a", "-u"+strconv.FormatUint(uint64(uid), 10), "-iTCP", "-sTCP:ESTABLISHED", "-Fn").Output()
	if err != nil {
		return 0, fmt.Errorf("lsof lookup for %s -> %s: %w", client, server, err)
	}
	wantClient := netip.AddrPortFrom(client.Addr().Unmap(), client.Port())
	wantServer := netip.AddrPortFrom(server.Addr().Unmap(), server.Port())
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "n") {
			continue
		}
		lhs, rhs, ok := strings.Cut(line[1:], "->")
		if !ok {
			continue
		}
		la, err1 := netip.ParseAddrPort(lhs)
		ra, err2 := netip.ParseAddrPort(rhs)
		if err1 != nil || err2 != nil {
			continue
		}
		la = netip.AddrPortFrom(la.Addr().Unmap(), la.Port())
		ra = netip.AddrPortFrom(ra.Addr().Unmap(), ra.Port())
		if la == wantClient && ra == wantServer {
			return uid, nil
		}
	}
	return 0, fmt.Errorf("no TCP socket %s -> %s owned by uid %d", client, server, uid)
}
