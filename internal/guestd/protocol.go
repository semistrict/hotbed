// Package guestd implements the in-VM agent (`hotbed-guestd`), its wire
// protocol over virtio-vsock (or Unix sockets in tests), and the host-side
// client used by the hotbed VM runtime.
package guestd

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// Well-known virtio-vsock ports shared between hotbed-worker (host, CID 2)
// and hotbed-guestd (inside each workspace Firecracker microVM).
const (
	// ControlPort serves the HTTP/1.1 JSON control API (commands, sync, files,
	// shutdown, health).
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

// CommandState values reported by hotbed-guestd.
const (
	StateRunning = "running"
	StateFrozen  = "frozen"
	StateExited  = "exited"
)

// HealthResponse is returned by GET /v1/health.
type HealthResponse struct {
	Hostname string `json:"hostname"`
	Kernel   string `json:"kernel"`
	Arch     string `json:"arch"`
	PID1     bool   `json:"pid1"`
	CgroupV2 bool   `json:"cgroupV2"`
	WorkDir  string `json:"workDir"`
}

// StartCommandRequest starts a command inside the guest. Calls are idempotent
// on ID: starting an already-known ID returns its current status without
// spawning a duplicate process (required across vsock resets).
type StartCommandRequest struct {
	ID         string   `json:"id"`
	Argv       []string `json:"argv"`
	Dir        string   `json:"dir"`
	Env        []string `json:"env,omitempty"`
	User       string   `json:"user,omitempty"`
	UID        int      `json:"uid,omitempty"`
	GID        int      `json:"gid,omitempty"`
	CPUWeight  int      `json:"cpuWeight,omitempty"`
	MemoryLow  int64    `json:"memoryLow,omitempty"`
	MemoryHigh int64    `json:"memoryHigh,omitempty"`
	TmpDir     string   `json:"tmpDir,omitempty"`
}

// CommandStatus describes one command managed by hotbed-guestd.
type CommandStatus struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	ExitCode   int    `json:"exitCode"`
	MainPID    int    `json:"mainPid"`
	Cgroup     string `json:"cgroup,omitempty"`
	MemoryAnon int64  `json:"memoryAnon"`
	MemorySwap int64  `json:"memorySwap"`
	CPUUsec    int64  `json:"cpuUsec"`
	LogBytes   int64  `json:"logBytes"`
	StartedMs  int64  `json:"startedMs"`
	ExitedMs   int64  `json:"exitedMs,omitempty"`
	Error      string `json:"error,omitempty"`
}

// StatusRequest queries status for specific command IDs, or all commands when
// IDs is empty.
type StatusRequest struct {
	IDs []string `json:"ids,omitempty"`
}

// StatusResponse maps command ID -> status.
type StatusResponse struct {
	Commands map[string]CommandStatus `json:"commands"`
}

// StopCommandRequest terminates a running or frozen command.
type StopCommandRequest struct {
	ID      string `json:"id"`
	GraceMs int    `json:"graceMs,omitempty"`
}

// FreezeRequest freezes or thaws a command's guest cgroup (or process group).
type FreezeRequest struct {
	ID string `json:"id"`
}

// SyncSubmodule describes one submodule bundle carried in POST /v1/sync.
type SyncSubmodule struct {
	Path   string `json:"path"`
	Commit string `json:"commit"`
	Part   string `json:"part"` // multipart form field name, e.g. "sub-0"
}

// SyncRequest is the JSON metadata field ("meta") of POST /v1/sync.
type SyncRequest struct {
	Repo   string          `json:"repo"`
	Commit string          `json:"commit"`
	Base   string          `json:"base"`
	User   string          `json:"user,omitempty"`
	UID    int             `json:"uid,omitempty"`
	GID    int             `json:"gid,omitempty"`
	Env    []string        `json:"env,omitempty"`
	Subs   []SyncSubmodule `json:"subs,omitempty"`
}

// SyncResponse reports the result of gitops.Apply inside the guest.
type SyncResponse struct {
	FilesChanged int    `json:"filesChanged"`
	Err          string `json:"err,omitempty"`
	Mutated      bool   `json:"mutated,omitempty"`
	RolledBack   bool   `json:"rolledBack,omitempty"`
	RollbackErr  string `json:"rollbackErr,omitempty"`
}

// RepoBase identifies one checkout in /work for file listing.
type RepoBase struct {
	Repo       string `json:"repo"`
	BaseCommit string `json:"baseCommit"`
}

// ListFilesRequest lists tracked files across repos relative to /work.
type ListFilesRequest struct {
	Repos []RepoBase `json:"repos"`
}

// ListFilesResponse returns workspace file entries.
type ListFilesResponse struct {
	Files []*pb.FileEntry `json:"files"`
}

// GetFileRequest reads one regular file and its changed lines against base.
type GetFileRequest struct {
	Repo       string `json:"repo"`
	BaseCommit string `json:"baseCommit"`
	RelPath    string `json:"relPath"`
}

// GetFileResponse returns the file's content and changed line numbers.
type GetFileResponse struct {
	Path         string  `json:"path"`
	Text         string  `json:"text"`
	ChangedLines []int32 `json:"changedLines,omitempty"`
}

// ShutdownRequest flushes filesystems and optionally stops commands / powers off.
type ShutdownRequest struct {
	SyncFS       bool `json:"syncFs"`
	StopCommands bool `json:"stopCommands"`
	PowerOff     bool `json:"powerOff"`
}

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
