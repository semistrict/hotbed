//go:build darwin

package peercred

import (
	"net"
	"net/netip"
	"os"
	"testing"
)

func TestSocketUIDDarwin(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-accepted
	defer server.Close()

	clientAP := client.LocalAddr().(*net.TCPAddr).AddrPort()
	serverAP := server.LocalAddr().(*net.TCPAddr).AddrPort()
	uid, err := SocketUID(clientAP, serverAP)
	if err != nil {
		t.Fatalf("SocketUID(%s, %s): %v", clientAP, serverAP, err)
	}
	if want := uint32(os.Getuid()); uid != want {
		t.Errorf("SocketUID = %d, want %d", uid, want)
	}

	bogus := netip.AddrPortFrom(clientAP.Addr(), 1)
	if _, err := SocketUID(bogus, serverAP); err == nil {
		t.Errorf("SocketUID(%s, %s) unexpectedly succeeded", bogus, serverAP)
	}
}
