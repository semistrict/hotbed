//go:build !linux

package guestd

import (
	"errors"
	"net"
)

// ListenVsock is only supported on Linux.
func ListenVsock(port uint32) (net.Listener, error) {
	return nil, errors.New("vsock is only supported on Linux")
}

// DialVsock is only supported on Linux.
func DialVsock(cid, port uint32) (net.Conn, error) {
	return nil, errors.New("vsock is only supported on Linux")
}
