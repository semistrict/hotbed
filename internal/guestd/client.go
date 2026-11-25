package guestd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/inspect"
	"github.com/semistrict/hotbed/internal/unit"
)

// Client is the host worker's RPC client to a single workspace VM's hotbed-guestd.
// Control calls use fresh short-lived connections and retry on vsock resets
// (ECONNRESET / EPIPE / EOF) per cladefs's integrator-guide vsock rules.
type Client struct {
	dial DialFunc
	http *http.Client
}

// NewClient constructs a Client over dial (FirecrackerVsockDialer in production,
// UnixMapDialer in tests).
func NewClient(dial DialFunc) *Client {
	tr := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dial(ctx, ControlPort)
		},
	}
	return &Client{
		dial: dial,
		http: &http.Client{Transport: tr},
	}
}

func isTransientReset(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ENOTCONN) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF)
}

func (c *Client) doJSON(ctx context.Context, method, path string, reqBody, respDst any) error {
	var raw []byte
	if reqBody != nil {
		var err error
		raw, err = json.Marshal(reqBody)
		if err != nil {
			return err
		}
	}
	const maxAttempts = 5
	var lastErr error
	for attempt := range maxAttempts {
		var body io.Reader
		if raw != nil {
			body = bytes.NewReader(raw)
		}
		req, err := http.NewRequestWithContext(ctx, method, "http://guestd"+path, body)
		if err != nil {
			return err
		}
		if raw != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			if isTransientReset(err) && attempt+1 < maxAttempts && ctx.Err() == nil {
				select {
				case <-time.After(time.Duration(attempt+1) * 50 * time.Millisecond):
					continue
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return err
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		_ = resp.Body.Close()
		if rerr != nil {
			lastErr = rerr
			if isTransientReset(rerr) && attempt+1 < maxAttempts && ctx.Err() == nil {
				select {
				case <-time.After(time.Duration(attempt+1) * 50 * time.Millisecond):
					continue
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return rerr
		}
		if resp.StatusCode >= 400 {
			var e struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(data, &e) == nil && e.Error != "" {
				return fmt.Errorf("guestd %s %s (%d): %s", method, path, resp.StatusCode, e.Error)
			}
			return fmt.Errorf("guestd %s %s (%d): %s", method, path, resp.StatusCode, string(data))
		}
		if respDst != nil && len(data) > 0 {
			if err := json.Unmarshal(data, respDst); err != nil {
				return fmt.Errorf("decode guestd %s response: %w", path, err)
			}
		}
		return nil
	}
	return lastErr
}

// Health pings hotbed-guestd and returns guest runtime info.
func (c *Client) Health(ctx context.Context) (HealthResponse, error) {
	var resp HealthResponse
	err := c.doJSON(ctx, http.MethodGet, "/v1/health", nil, &resp)
	return resp, err
}

// WaitReady polls Health until the guest agent answers or ctx expires.
func (c *Client) WaitReady(ctx context.Context) (HealthResponse, error) {
	for {
		h, err := c.Health(ctx)
		if err == nil {
			return h, nil
		}
		select {
		case <-ctx.Done():
			return HealthResponse{}, fmt.Errorf("wait for hotbed-guestd: %w (last: %v)", ctx.Err(), err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// StartCommand idempotently starts a command inside the guest.
func (c *Client) StartCommand(ctx context.Context, req StartCommandRequest) (CommandStatus, error) {
	var resp CommandStatus
	err := c.doJSON(ctx, http.MethodPost, "/v1/commands/start", req, &resp)
	return resp, err
}

// StatusMany queries the state of ids (or all commands if ids is empty).
func (c *Client) StatusMany(ctx context.Context, ids []string) (map[string]CommandStatus, error) {
	var resp StatusResponse
	err := c.doJSON(ctx, http.MethodPost, "/v1/commands/status", StatusRequest{IDs: ids}, &resp)
	return resp.Commands, err
}

// StopCommand terminates a running or frozen command.
func (c *Client) StopCommand(ctx context.Context, id string, grace time.Duration) (CommandStatus, error) {
	var resp CommandStatus
	err := c.doJSON(ctx, http.MethodPost, "/v1/commands/stop", StopCommandRequest{
		ID:      id,
		GraceMs: int(grace / time.Millisecond),
	}, &resp)
	return resp, err
}

// FreezeCommand freezes a command's guest cgroup without freezing the VM.
func (c *Client) FreezeCommand(ctx context.Context, id string) (CommandStatus, error) {
	var resp CommandStatus
	err := c.doJSON(ctx, http.MethodPost, "/v1/commands/freeze", FreezeRequest{ID: id}, &resp)
	return resp, err
}

// ThawCommand unfreezes a frozen command inside the guest.
func (c *Client) ThawCommand(ctx context.Context, id string) (CommandStatus, error) {
	var resp CommandStatus
	err := c.doJSON(ctx, http.MethodPost, "/v1/commands/thaw", FreezeRequest{ID: id}, &resp)
	return resp, err
}

// Reclaim asks hotbed-guestd to drop clean guest page caches (and reclaim from
// command id's cgroup when id and bytes are set) so virtio-balloon
// free-page-reporting returns unused host RAM.
func (c *Client) Reclaim(ctx context.Context, id string, bytes int64) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/reclaim", ReclaimRequest{ID: id, Bytes: bytes}, nil)
}

func decodeAPIError(resp *http.Response, body []byte, op string) error {
	if resp.Header.Get(apiErrorHeader) == "1" {
		var ae api.Error
		if json.Unmarshal(body, &ae) == nil && ae.Code != "" {
			return &ae
		}
	}
	if resp.StatusCode == http.StatusNotFound {
		return api.Errorf(api.CodePlatformNotSupported, "VM guest agent (hotbed-guestd) does not support %s", op).
			WithSuggestion("recreate the workspace after upgrading hotbed-guestd")
	}
	return fmt.Errorf("guestd %s (%d): %s", op, resp.StatusCode, strings.TrimSpace(string(body)))
}

// InspectCommand runs an opt-in Linux inspection of a live command inside the guest VM.
func (c *Client) InspectCommand(ctx context.Context, req *pb.InspectCommandRequest) (*pb.InspectCommandResponse, error) {
	raw, err := proto.Marshal(req)
	if err != nil {
		return nil, err
	}
	const maxAttempts = 5
	var lastErr error
	for attempt := range maxAttempts {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://guestd/v1/commands/inspect", bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/x-protobuf")
		resp, err := c.http.Do(httpReq)
		if err != nil {
			lastErr = err
			if isTransientReset(err) && attempt+1 < maxAttempts && ctx.Err() == nil {
				select {
				case <-time.After(time.Duration(attempt+1) * 50 * time.Millisecond):
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return nil, err
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, maxStreamFrameLen))
		_ = resp.Body.Close()
		if rerr != nil {
			lastErr = rerr
			if isTransientReset(rerr) && attempt+1 < maxAttempts && ctx.Err() == nil {
				select {
				case <-time.After(time.Duration(attempt+1) * 50 * time.Millisecond):
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return nil, rerr
		}
		if resp.StatusCode >= 400 {
			return nil, decodeAPIError(resp, data, "in-guest inspection")
		}
		out := &pb.InspectCommandResponse{}
		if err := proto.Unmarshal(data, out); err != nil {
			return nil, fmt.Errorf("decode guestd inspect response: %w", err)
		}
		return out, nil
	}
	return nil, lastErr
}

func (c *Client) streamProto(ctx context.Context, path, op string, req proto.Message, onChunk func([]byte) error) error {
	raw, err := proto.Marshal(req)
	if err != nil {
		return err
	}
	const maxAttempts = 5
	var resp *http.Response
	var lastErr error
	for attempt := range maxAttempts {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://guestd"+path, bytes.NewReader(raw))
		if err != nil {
			return err
		}
		httpReq.Header.Set("Content-Type", "application/x-protobuf")
		resp, err = c.http.Do(httpReq)
		if err == nil {
			break
		}
		lastErr = err
		if isTransientReset(err) && attempt+1 < maxAttempts && ctx.Err() == nil {
			select {
			case <-time.After(time.Duration(attempt+1) * 50 * time.Millisecond):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return err
	}
	if resp == nil {
		return lastErr
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return decodeAPIError(resp, body, op)
	}
	for {
		typ, payload, err := readStreamFrame(resp.Body)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("guestd %s stream interrupted: %w", op, err)
		}
		switch typ {
		case streamFrameProto:
			if err := onChunk(payload); err != nil {
				return err
			}
		case streamFrameError:
			var ae api.Error
			if json.Unmarshal(payload, &ae) == nil && ae.Code != "" {
				return &ae
			}
			return fmt.Errorf("guestd %s: %s", op, string(payload))
		case streamFrameEOF:
			return nil
		default:
			return fmt.Errorf("guestd %s: unexpected frame type %d", op, typ)
		}
	}
}

// TraceCommand runs an opt-in active Linux trace on a live command inside the guest VM.
func (c *Client) TraceCommand(ctx context.Context, req *pb.TraceCommandRequest, emit inspect.Emit) error {
	return c.streamProto(ctx, "/v1/commands/trace", "in-guest tracing", req, func(payload []byte) error {
		ch := &pb.TraceCommandChunk{}
		if err := proto.Unmarshal(payload, ch); err != nil {
			return fmt.Errorf("decode guestd trace chunk: %w", err)
		}
		return emit(ch)
	})
}

// SampleCommand runs an Activity Monitor-style Linux sample of a live command inside the guest VM.
func (c *Client) SampleCommand(ctx context.Context, req *pb.SampleCommandRequest, emit inspect.SampleEmit) error {
	return c.streamProto(ctx, "/v1/commands/sample", "in-guest sampling", req, func(payload []byte) error {
		ch := &pb.SampleCommandChunk{}
		if err := proto.Unmarshal(payload, ch); err != nil {
			return fmt.Errorf("decode guestd sample chunk: %w", err)
		}
		return emit(ch)
	})
}

// StreamLogs streams command output starting at offset into dst until EOF (or
// until the command exits when follow=true).
func (c *Client) StreamLogs(ctx context.Context, id string, offset int64, follow bool, dst io.Writer) (int64, error) {
	q := url.Values{}
	q.Set("id", id)
	q.Set("offset", strconv.FormatInt(offset, 10))
	if follow {
		q.Set("follow", "1")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://guestd/v1/commands/logs?"+q.Encode(), nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 0, fmt.Errorf("guestd stream logs (%d): %s", resp.StatusCode, string(body))
	}
	return io.Copy(dst, resp.Body)
}

// ApplySync streams mainBundlePath and submodule bundles to the guest and runs
// gitops.Apply inside /work/<repo>.
func (c *Client) ApplySync(ctx context.Context, repo, mainBundlePath, commit, base string, subs []gitops.SubCheckout, env []string, uid, gid int) (int, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	meta := SyncRequest{
		Repo:   repo,
		Commit: commit,
		Base:   base,
		UID:    uid,
		GID:    gid,
		Env:    env,
	}
	for i, s := range subs {
		sm := SyncSubmodule{Path: s.Path, Commit: s.Commit}
		if s.Bundle != "" {
			sm.Part = fmt.Sprintf("sub-%d", i)
		}
		meta.Subs = append(meta.Subs, sm)
	}

	writeErr := make(chan error, 1)
	go func() {
		defer pw.Close()
		defer mw.Close()
		metaW, err := mw.CreateFormField("meta")
		if err != nil {
			writeErr <- err
			return
		}
		if err := json.NewEncoder(metaW).Encode(meta); err != nil {
			writeErr <- err
			return
		}
		if err := writeFilePart(mw, "bundle", mainBundlePath); err != nil {
			writeErr <- err
			return
		}
		for i, s := range subs {
			if s.Bundle == "" {
				continue
			}
			if err := writeFilePart(mw, fmt.Sprintf("sub-%d", i), s.Bundle); err != nil {
				writeErr <- err
				return
			}
		}
		writeErr <- nil
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://guestd/v1/sync", pr)
	if err != nil {
		_ = pr.Close()
		return 0, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.http.Do(req)
	_ = pr.Close()
	if werr := <-writeErr; werr != nil && err == nil {
		err = werr
	}
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("guestd sync (%d): %s", resp.StatusCode, string(data))
	}
	var sr SyncResponse
	if err := json.Unmarshal(data, &sr); err != nil {
		return 0, fmt.Errorf("decode sync response: %w", err)
	}
	if sr.Err == "" {
		return sr.FilesChanged, nil
	}
	if !sr.Mutated {
		return 0, errors.New(sr.Err)
	}
	ae := &gitops.ApplyError{Err: errors.New(sr.Err), Mutated: true, RolledBack: sr.RolledBack}
	if sr.RollbackErr != "" {
		ae.RollbackErr = errors.New(sr.RollbackErr)
	}
	return 0, ae
}

func writeFilePart(mw *multipart.Writer, field, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	pw, err := mw.CreateFormFile(field, field+".bundle")
	if err != nil {
		return err
	}
	_, err = io.Copy(pw, f)
	return err
}

// ListFiles lists tracked files across the given repos in the guest.
func (c *Client) ListFiles(ctx context.Context, repos []RepoBase) ([]*pb.FileEntry, error) {
	var resp ListFilesResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/files/list", ListFilesRequest{Repos: repos}, &resp); err != nil {
		return nil, err
	}
	return resp.Files, nil
}

// GetFile reads one regular file and its diff-changed lines from the guest.
func (c *Client) GetFile(ctx context.Context, repo, baseCommit, relPath string) (*pb.FileContent, error) {
	var resp GetFileResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/files/get", GetFileRequest{
		Repo:       repo,
		BaseCommit: baseCommit,
		RelPath:    relPath,
	}, &resp); err != nil {
		return nil, err
	}
	return &pb.FileContent{
		Path:         resp.Path,
		Text:         resp.Text,
		ChangedLines: resp.ChangedLines,
	}, nil
}

// Shutdown asks the guest agent to flush filesystems and optionally stop
// commands / power off the VM.
func (c *Client) Shutdown(ctx context.Context, req ShutdownRequest) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/shutdown", req, nil)
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
