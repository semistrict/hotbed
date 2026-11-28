package guestd

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectproto"

	"github.com/semistrict/hotbed/internal/api"
	guestdv1 "github.com/semistrict/hotbed/internal/gen/guestd/v1"
	"github.com/semistrict/hotbed/internal/gen/guestd/v1/guestdv1connect"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/inspect"
	"github.com/semistrict/hotbed/internal/unit"
)

const syncChunkBytes = 256 << 10

// Client is the host worker's RPC client to a single workspace VM's hotbed-guestd.
// Control calls use Buf Connect over fresh short-lived connections and retry on
// vsock resets (ECONNRESET / EPIPE / EOF) per cladefs's integrator-guide rules.
type Client struct {
	dial DialFunc
	rpc  guestdv1connect.GuestServiceClient
}

// NewClient constructs a Client over dial (FirecrackerVsockDialer in production,
// UnixMapDialer in tests).
func NewClient(dial DialFunc) *Client {
	hc := &http.Client{
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dial(ctx, ControlPort)
			},
		},
	}
	cc := connect.NewClient(connecthttp.NewTransport(
		hc,
		"http://guestd",
		connecthttp.WithReadMaxBytes(maxControlMsgBytes),
	))
	return &Client{
		dial: dial,
		rpc:  guestdv1connect.NewGuestServiceClient(cc),
	}
}

// RPC returns the underlying Connect GuestServiceClient.
func (c *Client) RPC() guestdv1connect.GuestServiceClient { return c.rpc }

func isTransientReset(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ENOTCONN) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// connecthttp turns transport-level io.ErrUnexpectedEOF / EOF into a
	// CodeUnavailable connect.Error whose message may not retain the original
	// sentinel via errors.Is.
	if connect.CodeOf(err) == connect.CodeUnavailable {
		msg := err.Error()
		if strings.Contains(msg, "unexpected EOF") || strings.Contains(msg, "connection reset by peer") || strings.Contains(msg, "broken pipe") {
			return true
		}
	}
	return false
}

func retryUnary[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	const maxAttempts = 5
	var zero T
	var lastErr error
	for attempt := range maxAttempts {
		res, err := fn()
		if err == nil {
			return res, nil
		}
		lastErr = err
		if !isTransientReset(err) || attempt+1 >= maxAttempts || ctx.Err() != nil {
			return zero, err
		}
		select {
		case <-time.After(time.Duration(attempt+1) * 50 * time.Millisecond):
		case <-ctx.Done():
			return zero, ctx.Err()
		}
	}
	return zero, lastErr
}

// decodeGuestError extracts structured hotbed.v1.ErrorInfo details into an
// *api.Error, and maps bare Connect CodeUnimplemented (e.g. older guestd
// binaries returning 404) to CodePlatformNotSupported.
func decodeGuestError(err error, op string) error {
	if err == nil {
		return nil
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		for _, d := range ce.Details() {
			msg, derr := connectproto.UnmarshalErrorDetail(d)
			if derr != nil {
				continue
			}
			if info, ok := msg.(*pb.ErrorInfo); ok {
				return &api.Error{
					Code:       api.Code(info.Code),
					Message:    ce.Message(),
					Suggestion: info.Suggestion,
					Retryable:  info.Retryable,
				}
			}
		}
		if ce.Code() == connect.CodeUnimplemented {
			return api.Errorf(api.CodePlatformNotSupported, "VM guest agent (hotbed-guestd) does not support %s", op).
				WithSuggestion("recreate the workspace after upgrading hotbed-guestd")
		}
	}
	return err
}

// Health pings hotbed-guestd and returns guest runtime info.
func (c *Client) Health(ctx context.Context) (*guestdv1.HealthResponse, error) {
	return retryUnary(ctx, func() (*guestdv1.HealthResponse, error) {
		return c.rpc.Health(ctx, &guestdv1.HealthRequest{})
	})
}

// WaitReady polls Health until the guest agent answers or ctx expires.
func (c *Client) WaitReady(ctx context.Context) (*guestdv1.HealthResponse, error) {
	for {
		h, err := c.Health(ctx)
		if err == nil {
			return h, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for hotbed-guestd: %w (last: %v)", ctx.Err(), err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// StartCommand idempotently starts a command inside the guest.
func (c *Client) StartCommand(ctx context.Context, req *guestdv1.StartCommandRequest) (*guestdv1.CommandStatus, error) {
	return retryUnary(ctx, func() (*guestdv1.CommandStatus, error) {
		return c.rpc.StartCommand(ctx, req)
	})
}

// StatusMany queries the state of ids (or all commands if ids is empty).
func (c *Client) StatusMany(ctx context.Context, ids []string) (map[string]*guestdv1.CommandStatus, error) {
	resp, err := retryUnary(ctx, func() (*guestdv1.StatusResponse, error) {
		return c.rpc.Status(ctx, &guestdv1.StatusRequest{Ids: ids})
	})
	if err != nil {
		return nil, err
	}
	return resp.GetCommands(), nil
}

// StopCommand terminates a running or frozen command.
func (c *Client) StopCommand(ctx context.Context, id string, grace time.Duration) (*guestdv1.CommandStatus, error) {
	return retryUnary(ctx, func() (*guestdv1.CommandStatus, error) {
		return c.rpc.StopCommand(ctx, &guestdv1.StopCommandRequest{
			Id:      id,
			GraceMs: int32(grace / time.Millisecond),
		})
	})
}

// FreezeCommand freezes a command's guest cgroup without freezing the VM.
func (c *Client) FreezeCommand(ctx context.Context, id string) (*guestdv1.CommandStatus, error) {
	return retryUnary(ctx, func() (*guestdv1.CommandStatus, error) {
		return c.rpc.FreezeCommand(ctx, &guestdv1.FreezeCommandRequest{Id: id})
	})
}

// ThawCommand unfreezes a frozen command inside the guest.
func (c *Client) ThawCommand(ctx context.Context, id string) (*guestdv1.CommandStatus, error) {
	return retryUnary(ctx, func() (*guestdv1.CommandStatus, error) {
		return c.rpc.ThawCommand(ctx, &guestdv1.ThawCommandRequest{Id: id})
	})
}

// SendInput writes data to the terminal of command id. It is not retried:
// input sent twice is typed twice.
func (c *Client) SendInput(ctx context.Context, id string, data []byte) error {
	_, err := c.rpc.SendInput(ctx, &guestdv1.SendInputRequest{Id: id, Data: data})
	return err
}

// Reclaim asks hotbed-guestd to drop clean guest page caches (and reclaim from
// command id's cgroup when id and bytes are set) so virtio-balloon
// free-page-reporting returns unused host RAM.
func (c *Client) Reclaim(ctx context.Context, id string, bytes int64) error {
	_, err := retryUnary(ctx, func() (*guestdv1.ReclaimResponse, error) {
		return c.rpc.Reclaim(ctx, &guestdv1.ReclaimRequest{Id: id, Bytes: bytes})
	})
	return err
}

// InspectCommand runs an opt-in Linux inspection of a live command inside the guest VM.
func (c *Client) InspectCommand(ctx context.Context, req *pb.InspectCommandRequest) (*pb.InspectCommandResponse, error) {
	resp, err := retryUnary(ctx, func() (*pb.InspectCommandResponse, error) {
		return c.rpc.InspectCommand(ctx, req)
	})
	if err != nil {
		return nil, decodeGuestError(err, "in-guest inspection")
	}
	return resp, nil
}

// TraceCommand runs an opt-in active Linux trace on a live command inside the guest VM.
func (c *Client) TraceCommand(ctx context.Context, req *pb.TraceCommandRequest, emit inspect.Emit) error {
	stream, err := retryUnary(ctx, func() (guestdv1connect.GuestServiceTraceCommandClientStream, error) {
		return c.rpc.TraceCommand(ctx, req)
	})
	if err != nil {
		return decodeGuestError(err, "in-guest tracing")
	}
	defer stream.Close()

	for {
		ch, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return decodeGuestError(err, "in-guest tracing")
		}
		if err := emit(ch); err != nil {
			return err
		}
	}
}

// SampleCommand runs an Activity Monitor-style Linux sample of a live command inside the guest VM.
func (c *Client) SampleCommand(ctx context.Context, req *pb.SampleCommandRequest, emit inspect.SampleEmit) error {
	stream, err := retryUnary(ctx, func() (guestdv1connect.GuestServiceSampleCommandClientStream, error) {
		return c.rpc.SampleCommand(ctx, req)
	})
	if err != nil {
		return decodeGuestError(err, "in-guest sampling")
	}
	defer stream.Close()

	for {
		ch, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return decodeGuestError(err, "in-guest sampling")
		}
		if err := emit(ch); err != nil {
			return err
		}
	}
}

// CommandProfile queries the in-guest always-on OpenTelemetry eBPF continuous
// profile for a live or recently exited command inside the guest VM.
func (c *Client) CommandProfile(ctx context.Context, req *pb.GetProfileRequest) (*pb.CommandProfile, error) {
	resp, err := retryUnary(ctx, func() (*pb.CommandProfile, error) {
		return c.rpc.GetProfile(ctx, req)
	})
	if err != nil {
		return nil, decodeGuestError(err, "in-guest continuous profiling")
	}
	return resp, nil
}

// ForgetCommandProfile drops in-memory eBPF profile samples for cmdID inside
// the guest VM after the host worker has persisted a snapshot.
func (c *Client) ForgetCommandProfile(ctx context.Context, cmdID string) error {
	_, err := retryUnary(ctx, func() (*guestdv1.ForgetCommandProfileResponse, error) {
		return c.rpc.ForgetCommandProfile(ctx, &guestdv1.ForgetCommandProfileRequest{Id: cmdID})
	})
	if err != nil && connect.CodeOf(err) == connect.CodeUnimplemented {
		return nil
	}
	return err
}

// StreamLogs streams command output starting at offset into dst until EOF (or
// until the command exits when follow=true).
func (c *Client) StreamLogs(ctx context.Context, id string, offset int64, follow bool, dst io.Writer) (int64, error) {
	stream, err := c.rpc.StreamLogs(ctx, &guestdv1.StreamLogsRequest{
		Id:     id,
		Offset: offset,
		Follow: follow,
	})
	if err != nil {
		return 0, err
	}
	defer stream.Close()

	var written int64
	for {
		chunk, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return written, nil
		}
		if err != nil {
			return written, err
		}
		if len(chunk.Data) == 0 {
			continue
		}
		n, werr := dst.Write(chunk.Data)
		written += int64(n)
		if werr != nil {
			return written, werr
		}
	}
}

// ApplySync streams mainBundlePath and submodule bundles to the guest and runs
// gitops.Apply inside /work/<repo>.
func (c *Client) ApplySync(ctx context.Context, repo, mainBundlePath, commit, base string, subs []gitops.SubCheckout, env []string, uid, gid int) (int, error) {
	header := &guestdv1.SyncHeader{
		Repo:   repo,
		Commit: commit,
		Base:   base,
		Uid:    int32(uid),
		Gid:    int32(gid),
		Env:    env,
	}
	type subFile struct {
		path string
		size int64
	}
	var subFiles []subFile
	for _, s := range subs {
		sm := &guestdv1.SyncSubmodule{Path: s.Path, Commit: s.Commit}
		if s.Bundle != "" {
			st, err := os.Stat(s.Bundle)
			if err != nil {
				return 0, err
			}
			sm.BundleBytes = st.Size()
			subFiles = append(subFiles, subFile{path: s.Bundle, size: st.Size()})
		}
		header.Subs = append(header.Subs, sm)
	}

	mainFile, err := os.Open(mainBundlePath)
	if err != nil {
		return 0, err
	}
	defer mainFile.Close()

	stream, err := c.rpc.Sync(ctx)
	if err != nil {
		return 0, err
	}
	if err := stream.Send(&guestdv1.SyncRequest{
		Msg: &guestdv1.SyncRequest_Header{Header: header},
	}); err != nil {
		_, cerr := stream.CloseAndReceive()
		return 0, errors.Join(err, cerr)
	}

	buf := make([]byte, syncChunkBytes)
	sendReader := func(r io.Reader) (total int64, readErr, sendErr error) {
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				total += int64(n)
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				if serr := stream.Send(&guestdv1.SyncRequest{
					Msg: &guestdv1.SyncRequest_Chunk{Chunk: chunk},
				}); serr != nil {
					return total, nil, serr
				}
			}
			if errors.Is(rerr, io.EOF) {
				return total, nil, nil
			}
			if rerr != nil {
				return total, rerr, nil
			}
		}
	}

	var localErr error
sendLoop:
	for _, sf := range subFiles {
		f, err := os.Open(sf.path)
		if err != nil {
			localErr = err
			break sendLoop
		}
		n, rerr, serr := sendReader(io.LimitReader(f, sf.size))
		_ = f.Close()
		if rerr != nil {
			localErr = rerr
			break sendLoop
		}
		if serr != nil {
			break sendLoop
		}
		if n != sf.size {
			localErr = fmt.Errorf("submodule bundle %s shrank (%d < %d bytes)", sf.path, n, sf.size)
			break sendLoop
		}
	}
	if localErr == nil {
		_, rerr, _ := sendReader(mainFile)
		if rerr != nil {
			localErr = rerr
		}
	}

	sr, err := stream.CloseAndReceive()
	if localErr != nil {
		return 0, localErr
	}
	if err != nil {
		return 0, err
	}
	if sr.Error == "" {
		return int(sr.FilesChanged), nil
	}
	if !sr.Mutated {
		return 0, errors.New(sr.Error)
	}
	ae := &gitops.ApplyError{Err: errors.New(sr.Error), Mutated: true, RolledBack: sr.RolledBack}
	if sr.RollbackError != "" {
		ae.RollbackErr = errors.New(sr.RollbackError)
	}
	return 0, ae
}

// ListFiles lists tracked files across the given repos in the guest.
func (c *Client) ListFiles(ctx context.Context, repos []*guestdv1.RepoBase) ([]*pb.FileEntry, error) {
	resp, err := retryUnary(ctx, func() (*guestdv1.ListFilesResponse, error) {
		return c.rpc.ListFiles(ctx, &guestdv1.ListFilesRequest{Repos: repos})
	})
	if err != nil {
		return nil, err
	}
	return resp.GetFiles(), nil
}

// GetFile reads one regular file and its diff-changed lines from the guest.
func (c *Client) GetFile(ctx context.Context, repo, baseCommit, relPath string) (*pb.FileContent, error) {
	return retryUnary(ctx, func() (*pb.FileContent, error) {
		return c.rpc.GetFile(ctx, &guestdv1.GetFileRequest{
			Repo:       repo,
			BaseCommit: baseCommit,
			RelPath:    relPath,
		})
	})
}

// Shutdown asks the guest agent to flush filesystems and optionally stop
// commands / power off the VM.
func (c *Client) Shutdown(ctx context.Context, req *guestdv1.ShutdownRequest) error {
	_, err := retryUnary(ctx, func() (*guestdv1.ShutdownResponse, error) {
		return c.rpc.Shutdown(ctx, req)
	})
	return err
}

// DialTCP opens a tunneled TCP connection to 127.0.0.1:<port> inside the guest.
func (c *Client) DialTCP(ctx context.Context, port uint16) (net.Conn, error) {
	conn, err := c.dial(ctx, TCPProxyPort)
	if err != nil {
		return nil, err
	}
	if err := json.NewEncoder(conn).Encode(TCPDialRequest{Port: port}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadBytes('\n')
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	var resp TCPDialResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if !resp.OK {
		_ = conn.Close()
		return nil, fmt.Errorf("guest tcp dial :%d: %s", port, resp.Err)
	}
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

// StartShell opens a framed interactive PTY or piped shell over ShellPort and
// returns a unit.ShellHandle compatible with hotbed's SSH server.
func (c *Client) StartShell(ctx context.Context, req ShellOpenRequest, sio unit.ShellIO) (unit.ShellHandle, error) {
	conn, err := c.dial(ctx, ShellPort)
	if err != nil {
		return nil, err
	}
	req.PTY = sio.PTY
	req.Cols = sio.Cols
	req.Rows = sio.Rows
	req.Term = sio.Term
	if err := writeJSONFrame(conn, frameOpen, req); err != nil {
		_ = conn.Close()
		return nil, err
	}

	h := &shellHandle{
		conn: conn,
		done: make(chan struct{}),
		exit: 1,
	}

	if sio.PTY {
		fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("socketpair for guest pty: %w", err)
		}
		// Mark both ends non-blocking so Go registers them with the netpoller
		// and Close() immediately unblocks any concurrent Read().
		_ = syscall.SetNonblock(fds[0], true)
		_ = syscall.SetNonblock(fds[1], true)
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
		h.ptyHost = os.NewFile(uintptr(fds[0]), "guestd-pty-host")
		h.ptyPeer = os.NewFile(uintptr(fds[1]), "guestd-pty-peer")

		// Pump local socketpair -> guest shell stdin.
		go func() {
			buf := make([]byte, 16<<10)
			for {
				n, err := h.ptyPeer.Read(buf)
				if n > 0 {
					if h.sendFrame(frameData, buf[:n]) != nil {
						return
					}
				}
				if err != nil {
					_ = h.sendFrame(frameCloseStdin, nil)
					return
				}
			}
		}()

		// Pump guest shell stdout -> local socketpair.
		go func() {
			defer close(h.done)
			defer func() {
				// Half-close write side first so io.Copy(ch, ptmx) sees EOF
				// cleanly even if the caller has not closed ptmx yet.
				_ = syscall.Shutdown(fds[1], syscall.SHUT_WR)
				_ = h.ptyPeer.Close()
			}()
			for {
				typ, payload, err := readFrame(conn)
				if err != nil {
					return
				}
				switch typ {
				case frameData:
					if len(payload) > 0 {
						_, _ = h.ptyPeer.Write(payload)
					}
				case frameExit:
					if len(payload) == 4 {
						h.setExit(int(int32(binary.BigEndian.Uint32(payload))))
					}
					return
				case frameError:
					h.setExit(1)
					return
				}
			}
		}()
	} else {
		if sio.Stdin != nil {
			go func() {
				buf := make([]byte, 16<<10)
				for {
					n, err := sio.Stdin.Read(buf)
					if n > 0 {
						if h.sendFrame(frameData, buf[:n]) != nil {
							return
						}
					}
					if err != nil {
						_ = h.sendFrame(frameCloseStdin, nil)
						return
					}
				}
			}()
		}
		go func() {
			defer close(h.done)
			for {
				typ, payload, err := readFrame(conn)
				if err != nil {
					return
				}
				switch typ {
				case frameData:
					if sio.Stdout != nil && len(payload) > 0 {
						_, _ = sio.Stdout.Write(payload)
					}
				case frameStderr:
					if sio.Stderr != nil && len(payload) > 0 {
						_, _ = sio.Stderr.Write(payload)
					}
				case frameExit:
					if len(payload) == 4 {
						h.setExit(int(int32(binary.BigEndian.Uint32(payload))))
					}
					return
				case frameError:
					if sio.Stderr != nil && len(payload) > 0 {
						_, _ = sio.Stderr.Write(payload)
					}
					h.setExit(1)
					return
				}
			}
		}()
	}

	go func() {
		select {
		case <-ctx.Done():
			_ = h.Close()
		case <-h.done:
		}
	}()

	return h, nil
}

type shellHandle struct {
	conn    net.Conn
	writeMu sync.Mutex
	ptyHost *os.File
	ptyPeer *os.File
	done    chan struct{}
	exitMu  sync.Mutex
	exit    int
	once    sync.Once
}

func (h *shellHandle) sendFrame(typ byte, payload []byte) error {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	return writeFrame(h.conn, typ, payload)
}

func (h *shellHandle) setExit(code int) {
	h.exitMu.Lock()
	h.exit = code
	h.exitMu.Unlock()
}

func (h *shellHandle) PTY() *os.File { return h.ptyHost }

func (h *shellHandle) Resize(cols, rows uint16) error {
	if h.ptyHost == nil {
		return nil
	}
	var payload [4]byte
	binary.BigEndian.PutUint16(payload[0:2], cols)
	binary.BigEndian.PutUint16(payload[2:4], rows)
	return h.sendFrame(frameResize, payload[:])
}

func (h *shellHandle) Wait() int {
	<-h.done
	h.exitMu.Lock()
	defer h.exitMu.Unlock()
	return h.exit
}

func (h *shellHandle) Close() error {
	var err error
	h.once.Do(func() {
		err = h.conn.Close()
		if h.ptyPeer != nil {
			_ = h.ptyPeer.Close()
		}
		if h.ptyHost != nil {
			_ = h.ptyHost.Close()
		}
	})
	return err
}
