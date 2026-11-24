//go:build linux

package guestd

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// vsockAddr implements net.Addr for AF_VSOCK endpoints.
type vsockAddr struct {
	cid  uint32
	port uint32
}

func (a vsockAddr) Network() string { return "vsock" }
func (a vsockAddr) String() string  { return fmt.Sprintf("vsock(%d:%d)", a.cid, a.port) }

// vsockListener accepts incoming AF_VSOCK stream connections on a port.
type vsockListener struct {
	fd     int
	file   *os.File
	port   uint32
	closed chan struct{}
	once   sync.Once
}

// ListenVsock binds an AF_VSOCK stream listener on VMADDR_CID_ANY and port.
func ListenVsock(port uint32) (net.Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("socket(AF_VSOCK): %w", err)
	}
	sa := &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}
	if err := unix.Bind(fd, sa); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("bind(vsock port %d): %w", port, err)
	}
	if err := unix.Listen(fd, 128); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("listen(vsock port %d): %w", port, err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("setnonblock(vsock port %d): %w", port, err)
	}
	return &vsockListener{
		fd:     fd,
		file:   os.NewFile(uintptr(fd), fmt.Sprintf("vsock-listen-%d", port)),
		port:   port,
		closed: make(chan struct{}),
	}, nil
}

func (l *vsockListener) Accept() (net.Conn, error) {
	raw, err := l.file.SyscallConn()
	if err != nil {
		return nil, err
	}
	var (
		nfd  int
		rsa  unix.Sockaddr
		aerr error
	)
	err = raw.Read(func(fd uintptr) bool {
		nfd, rsa, aerr = unix.Accept4(int(fd), unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
		if errors.Is(aerr, unix.EAGAIN) || errors.Is(aerr, unix.EWOULDBLOCK) {
			return false
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	if aerr != nil {
		return nil, aerr
	}
	remote := vsockAddr{}
	if vm, ok := rsa.(*unix.SockaddrVM); ok {
		remote.cid = vm.CID
		remote.port = vm.Port
	}
	return newVsockConn(nfd, vsockAddr{cid: unix.VMADDR_CID_ANY, port: l.port}, remote), nil
}

func (l *vsockListener) Close() error {
	var err error
	l.once.Do(func() {
		close(l.closed)
		err = l.file.Close()
	})
	return err
}

func (l *vsockListener) Addr() net.Addr {
	return vsockAddr{cid: unix.VMADDR_CID_ANY, port: l.port}
}

// DialVsock connects over AF_VSOCK to (cid, port) on Linux.
func DialVsock(cid, port uint32) (net.Conn, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("socket(AF_VSOCK): %w", err)
	}
	sa := &unix.SockaddrVM{CID: cid, Port: port}
	if err := unix.Connect(fd, sa); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("connect(vsock %d:%d): %w", cid, port, err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("setnonblock(vsock %d:%d): %w", cid, port, err)
	}
	return newVsockConn(fd, vsockAddr{cid: unix.VMADDR_CID_ANY}, vsockAddr{cid: cid, port: port}), nil
}

// vsockConn wraps a non-blocking AF_VSOCK fd as a full net.Conn using Go's
// netpoller via os.File.
type vsockConn struct {
	file   *os.File
	local  vsockAddr
	remote vsockAddr
}

func newVsockConn(fd int, local, remote vsockAddr) *vsockConn {
	return &vsockConn{
		file:   os.NewFile(uintptr(fd), fmt.Sprintf("vsock-%d-%d", remote.cid, remote.port)),
		local:  local,
		remote: remote,
	}
}

func (c *vsockConn) Read(p []byte) (int, error)         { return c.file.Read(p) }
func (c *vsockConn) Write(p []byte) (int, error)        { return c.file.Write(p) }
func (c *vsockConn) Close() error                       { return c.file.Close() }
func (c *vsockConn) LocalAddr() net.Addr                { return c.local }
func (c *vsockConn) RemoteAddr() net.Addr               { return c.remote }
func (c *vsockConn) SetDeadline(t time.Time) error      { return c.file.SetDeadline(t) }
func (c *vsockConn) SetReadDeadline(t time.Time) error  { return c.file.SetReadDeadline(t) }
func (c *vsockConn) SetWriteDeadline(t time.Time) error { return c.file.SetWriteDeadline(t) }

func (c *vsockConn) CloseWrite() error {
	raw, err := c.file.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := raw.Control(func(fd uintptr) {
		serr = unix.Shutdown(int(fd), unix.SHUT_WR)
	}); err != nil {
		return err
	}
	return serr
}
