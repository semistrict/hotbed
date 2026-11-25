// Package guestd implements the in-VM agent (`hotbed-guestd`), its Buf Connect
// control API and stream protocols over virtio-vsock (or Unix sockets in tests),
// and the host-side client used by the hotbed VM runtime.
package guestd

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Well-known virtio-vsock ports shared between hotbed-worker (host, CID 2)
// and hotbed-guestd (inside each workspace Firecracker microVM).
const (
	// ControlPort serves the guestd.v1.GuestService Connect RPC API (commands,
	// logs, inspect/trace/sample/profile, sync, files, reclaim, shutdown, health).
	ControlPort uint32 = 5000
	// ShellPort serves framed interactive PTY and piped shell streams.
	ShellPort uint32 = 5001
	// GitAuthPort is dialed by the guest back to the host worker's git-auth
	// server (credential helper, SSH proxy, and gh API shim).
	GitAuthPort uint32 = 5002
	// TCPProxyPort accepts host-initiated TCP forwarding requests for guest
	// dev servers (--port).
	TCPProxyPort uint32 = 5003

	// HostCID is the standard virtio-vsock context ID of the host.
	HostCID uint32 = 2
)

// ShellOpenRequest is the initial JSON header sent on ShellPort.
type ShellOpenRequest struct {
	Argv []string `json:"argv"`
	Dir  string   `json:"dir"`
	Env  []string `json:"env,omitempty"`
	User string   `json:"user,omitempty"`
	UID  int      `json:"uid,omitempty"`
	GID  int      `json:"gid,omitempty"`
	PTY  bool     `json:"pty"`
	Cols uint16   `json:"cols,omitempty"`
	Rows uint16   `json:"rows,omitempty"`
	Term string   `json:"term,omitempty"`
}

// TCPDialRequest is the initial JSON header sent on TCPProxyPort.
type TCPDialRequest struct {
	Port uint16 `json:"port"`
}

// TCPDialResponse is the JSON acknowledgement returned on TCPProxyPort before
// switching the connection to raw proxying.
type TCPDialResponse struct {
	OK  bool   `json:"ok"`
	Err string `json:"err,omitempty"`
}

// Binary frame types on ShellPort after the initial ShellOpenRequest frame.
const (
	frameOpen       byte = 1
	frameData       byte = 2 // stdin (host->guest) or pty/stdout (guest->host)
	frameStderr     byte = 3 // stderr when PTY=false (guest->host)
	frameResize     byte = 4 // 4 bytes: big-endian uint16 cols, uint16 rows
	frameCloseStdin byte = 5 // EOF on stdin (host->guest)
	frameExit       byte = 6 // 4 bytes: big-endian int32 exit code (guest->host)
	frameError      byte = 7 // UTF-8 error message (guest->host)
)

const maxFrameLen = 4 << 20

func writeFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > maxFrameLen {
		return fmt.Errorf("guestd frame too large: %d bytes", len(payload))
	}
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func readFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrameLen {
		return 0, nil, fmt.Errorf("guestd frame exceeds limit: %d bytes", n)
	}
	var payload []byte
	if n > 0 {
		payload = make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			return 0, nil, err
		}
	}
	return hdr[0], payload, nil
}

func writeJSONFrame(w io.Writer, typ byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeFrame(w, typ, b)
}

func readJSONFrame(r io.Reader, wantType byte, dst any) error {
	typ, payload, err := readFrame(r)
	if err != nil {
		return err
	}
	if typ == frameError {
		return fmt.Errorf("guestd: %s", string(payload))
	}
	if typ != wantType {
		return fmt.Errorf("guestd: unexpected frame type %d (want %d)", typ, wantType)
	}
	return json.Unmarshal(payload, dst)
}
