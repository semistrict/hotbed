package guestd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// DialFunc opens a bidirectional stream to the given guestd port.
type DialFunc func(ctx context.Context, port uint32) (net.Conn, error)

// FirecrackerVsockDialer returns a DialFunc that connects to a guest port
// through Firecracker's host-side virtio-vsock Unix domain socket using
// Firecracker's standard handshake ("CONNECT <port>\n" -> "OK <local>\n").
func FirecrackerVsockDialer(udsPath string) DialFunc {
	return func(ctx context.Context, port uint32) (net.Conn, error) {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "unix", udsPath)
		if err != nil {
			return nil, fmt.Errorf("dial firecracker vsock %s: %w", udsPath, err)
		}
		if deadline, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(deadline)
		} else {
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		}
		if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("vsock CONNECT %d: %w", port, err)
		}
		br := bufio.NewReader(conn)
		line, err := br.ReadString('\n')
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("vsock handshake reply for port %d: %w", port, err)
		}
		if !strings.HasPrefix(line, "OK ") {
			_ = conn.Close()
			return nil, fmt.Errorf("vsock CONNECT %d refused: %s", port, strings.TrimSpace(line))
		}
		_ = conn.SetDeadline(time.Time{})
		if br.Buffered() > 0 {
			return &bufferedConn{Conn: conn, r: br}, nil
		}
		return conn, nil
	}
}

// UnixMapDialer returns a DialFunc that dials per-port Unix sockets (for tests
// and local non-KVM runs).
func UnixMapDialer(paths map[uint32]string) DialFunc {
	return func(ctx context.Context, port uint32) (net.Conn, error) {
		p, ok := paths[port]
		if !ok {
			return nil, fmt.Errorf("no unix socket configured for guestd port %d", port)
		}
		var d net.Dialer
		return d.DialContext(ctx, "unix", p)
	}
}

type bufferedConn struct {
	net.Conn
	r io.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.r.Read(p)
}

func (c *bufferedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}
