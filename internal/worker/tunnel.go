package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect/v2/connecthttp"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/httpx"
	"github.com/semistrict/hotbed/internal/tailnet"
)

// tunnelDownMessage is sent by the Cloudflare Workers controller DO to a worker
// over /ws/worker.
type tunnelDownMessage struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Procedure string `json:"procedure,omitempty"`
	Body      string `json:"body,omitempty"`
	Stream    bool   `json:"stream,omitempty"`
}

// tunnelUpMessage is sent by a worker back to the Cloudflare Workers controller
// DO over /ws/worker.
type tunnelUpMessage struct {
	Type       string `json:"type"`
	ID         string `json:"id"`
	Body       string `json:"body,omitempty"`
	Code       string `json:"code,omitempty"`
	Message    string `json:"message,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
	Retryable  bool   `json:"retryable,omitempty"`
}

// Tunnel maintains an outbound persistent WebSocket connection to a remote
// Cloudflare Workers controller (/ws/worker) and dispatches incoming
// controller->worker WorkspaceService RPCs against eng until ctx is cancelled.
func Tunnel(
	ctx context.Context,
	eng *Engine,
	controllerBaseURL string,
	token string,
	self tailnet.Self,
	workerAddr string,
	controllerNodeID string,
	owner string,
	hc *http.Client,
) error {
	return TunnelWithMiddleware(ctx, eng, controllerBaseURL, token, self, workerAddr, controllerNodeID, owner, hc, nil)
}

// TunnelWithMiddleware is Tunnel with an optional HTTP middleware around the
// incoming controller->worker WorkspaceService handler (used by tests for fault
// injection such as network partitions and workspace creation gates).
func TunnelWithMiddleware(
	ctx context.Context,
	eng *Engine,
	controllerBaseURL string,
	token string,
	self tailnet.Self,
	workerAddr string,
	controllerNodeID string,
	owner string,
	hc *http.Client,
	wrap func(http.Handler) http.Handler,
) error {
	handler := newTunnelHandler(eng, tailnet.Peer{
		NodeID:   controllerNodeID,
		Hostname: "controller",
		Login:    owner,
	})
	if wrap != nil {
		handler = wrap(handler)
	}
	return runTunnelLoop(ctx, handler, controllerBaseURL, token, self, workerAddr, hc)
}

func newTunnelHandler(eng *Engine, peer tailnet.Peer) http.Handler {
	server := httpx.NewServer()
	hotbedv1connect.RegisterWorkspaceServiceHandler(server, &Service{E: eng})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(httpx.WithPeer(r.Context(), peer)))
	})
}

func tunnelWSURL(controllerBaseURL string) (string, error) {
	u, err := url.Parse(controllerBaseURL)
	if err != nil {
		return "", fmt.Errorf("parse controller URL %q: %w", controllerBaseURL, err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported controller scheme %q", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/ws/worker"
	return u.String(), nil
}

func runTunnelLoop(
	ctx context.Context,
	handler http.Handler,
	controllerBaseURL string,
	token string,
	self tailnet.Self,
	workerAddr string,
	hc *http.Client,
) error {
	wsURL, err := tunnelWSURL(controllerBaseURL)
	if err != nil {
		return err
	}
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return nil
		}
		connectedAt := time.Now()
		err := serveTunnelOnce(ctx, handler, wsURL, token, self, workerAddr, hc)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(connectedAt) > 15*time.Second {
			backoff = time.Second
		}
		slog.Warn("controller WebSocket tunnel disconnected; reconnecting", "url", wsURL, "backoff", backoff, "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
			backoff = min(backoff*2, 30*time.Second)
		}
	}
}

// defaultTunnelPingInterval and defaultTunnelReadTimeout control WebSocket
// liveness detection. Cloudflare's WebSocket Auto-Response replies "pong" to
// every "ping" without waking the Durable Object; if no frame arrives within
// defaultTunnelReadTimeout (3 missed pongs), the socket is treated as
// half-open (e.g. after a Wi-Fi AP roam) and closed so runTunnelLoop reconnects.
const (
	defaultTunnelPingInterval = 25 * time.Second
	defaultTunnelReadTimeout  = 75 * time.Second
)

func serveTunnelOnce(
	ctx context.Context,
	handler http.Handler,
	wsURL string,
	token string,
	self tailnet.Self,
	workerAddr string,
	hc *http.Client,
) error {
	return serveTunnelOnceWithTimeouts(ctx, handler, wsURL, token, self, workerAddr, hc, defaultTunnelPingInterval, defaultTunnelReadTimeout)
}

func serveTunnelOnceWithTimeouts(
	ctx context.Context,
	handler http.Handler,
	wsURL string,
	token string,
	self tailnet.Self,
	workerAddr string,
	hc *http.Client,
	pingInterval time.Duration,
	readTimeout time.Duration,
) error {
	headers := http.Header{}
	if token != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	if self.NodeID != "" {
		headers.Set("X-Hotbed-Node-Id", self.NodeID)
	}
	if self.Hostname != "" {
		headers.Set("X-Hotbed-Host", self.Hostname)
	}
	if workerAddr != "" {
		headers.Set("X-Hotbed-Addr", workerAddr)
	}
	dialCtx, dialCancel := context.WithTimeout(ctx, 20*time.Second)
	defer dialCancel()

	opts := &websocket.DialOptions{HTTPHeader: headers}
	if hc != nil {
		opts.HTTPClient = hc
	}
	conn, _, err := websocket.Dial(dialCtx, wsURL, opts)
	if err != nil {
		return fmt.Errorf("dial %s: %w", wsURL, err)
	}
	// Allow large WorkspaceService responses (e.g. GetLogLines up to 1 MiB + base64 overhead).
	conn.SetReadLimit(16 * 1024 * 1024)
	defer conn.CloseNow()

	slog.Info("controller WebSocket tunnel connected", "url", wsURL, "node", self.Hostname)

	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	var writeMu sync.Mutex
	sendText := func(b []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		wctx, cancel := context.WithTimeout(connCtx, 30*time.Second)
		defer cancel()
		return conn.Write(wctx, websocket.MessageText, b)
	}
	sendMsg := func(msg tunnelUpMessage) {
		b, err := json.Marshal(msg)
		if err != nil {
			return
		}
		if err := sendText(b); err != nil && connCtx.Err() == nil {
			slog.Warn("write controller tunnel message", "id", msg.ID, "type", msg.Type, "err", err)
		}
	}

	// Keepalive "ping" frames handled by Cloudflare's WebSocket Auto-Response
	// ("ping" -> "pong") without waking the Durable Object from hibernation.
	go func() {
		t := time.NewTicker(pingInterval)
		defer t.Stop()
		for {
			select {
			case <-connCtx.Done():
				return
			case <-t.C:
				if err := sendText([]byte("ping")); err != nil {
					connCancel()
					return
				}
			}
		}
	}()

	var inflightMu sync.Mutex
	inflight := map[string]context.CancelFunc{}
	var wg sync.WaitGroup
	defer func() {
		connCancel()
		wg.Wait()
	}()

	for {
		readCtx, readCancel := context.WithTimeout(connCtx, readTimeout)
		_, data, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && connCtx.Err() == nil {
				return fmt.Errorf("tunnel read timeout after %s (no pong/frame received): %w", readTimeout, err)
			}
			return err
		}
		if bytes.Equal(data, []byte("pong")) || bytes.Equal(data, []byte("ping")) {
			continue
		}
		var req tunnelDownMessage
		if err := json.Unmarshal(data, &req); err != nil {
			slog.Warn("ignore malformed controller tunnel frame", "err", err)
			continue
		}
		switch req.Type {
		case "rpc_cancel":
			inflightMu.Lock()
			if cancel, ok := inflight[req.ID]; ok {
				cancel()
				delete(inflight, req.ID)
			}
			inflightMu.Unlock()
		case "rpc_req":
			callCtx, callCancel := context.WithCancel(connCtx)
			inflightMu.Lock()
			inflight[req.ID] = callCancel
			inflightMu.Unlock()
			wg.Add(1)
			go func(r tunnelDownMessage) {
				defer wg.Done()
				defer func() {
					callCancel()
					inflightMu.Lock()
					delete(inflight, r.ID)
					inflightMu.Unlock()
				}()
				dispatchTunnelRPC(callCtx, handler, r, sendMsg)
			}(req)
		}
	}
}

func dispatchTunnelRPC(
	ctx context.Context,
	handler http.Handler,
	req tunnelDownMessage,
	send func(tunnelUpMessage),
) {
	bodyBytes, err := base64.StdEncoding.DecodeString(req.Body)
	if err != nil {
		send(tunnelUpMessage{
			Type:    "rpc_err",
			ID:      req.ID,
			Code:    string(api.CodeInvalidArgs),
			Message: "invalid base64 request body: " + err.Error(),
		})
		return
	}
	if req.Stream {
		dispatchTunnelStream(ctx, handler, req.ID, req.Procedure, bodyBytes, send)
		return
	}
	dispatchTunnelUnary(ctx, handler, req.ID, req.Procedure, bodyBytes, send)
}

func dispatchTunnelUnary(
	ctx context.Context,
	handler http.Handler,
	id, procedure string,
	bodyBytes []byte,
	send func(tunnelUpMessage),
) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://tunnel.local"+procedure, bytes.NewReader(bodyBytes))
	if err != nil {
		send(tunnelUpMessage{Type: "rpc_err", ID: id, Code: string(api.CodeInternal), Message: err.Error()})
		return
	}
	httpReq.Header.Set("Content-Type", "application/proto")
	httpReq.Header.Set("Connect-Protocol-Version", "1")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httpReq)
	resp := rec.Result()
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusOK {
		send(tunnelUpMessage{
			Type: "rpc_msg",
			ID:   id,
			Body: base64.StdEncoding.EncodeToString(respBody),
		})
		return
	}
	send(connectJSONToTunnelErr(id, respBody, resp.Status))
}

func dispatchTunnelStream(
	ctx context.Context,
	handler http.Handler,
	id, procedure string,
	bodyBytes []byte,
	send func(tunnelUpMessage),
) {
	// Wrap the single unary request message in a Connect 5-byte envelope frame.
	envelope := make([]byte, 5+len(bodyBytes))
	envelope[0] = 0
	binary.BigEndian.PutUint32(envelope[1:5], uint32(len(bodyBytes)))
	copy(envelope[5:], bodyBytes)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://tunnel.local"+procedure, bytes.NewReader(envelope))
	if err != nil {
		send(tunnelUpMessage{Type: "rpc_err", ID: id, Code: string(api.CodeInternal), Message: err.Error()})
		return
	}
	httpReq.Header.Set("Content-Type", "application/connect+proto")
	httpReq.Header.Set("Connect-Protocol-Version", "1")

	w := &tunnelStreamWriter{
		header: make(http.Header),
		id:     id,
		send:   send,
	}
	handler.ServeHTTP(w, httpReq)
	w.finish()
}

type tunnelStreamWriter struct {
	header   http.Header
	status   int
	id       string
	send     func(tunnelUpMessage)
	buf      bytes.Buffer
	ended    bool
	errBytes bytes.Buffer
}

func (w *tunnelStreamWriter) Header() http.Header { return w.header }

func (w *tunnelStreamWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

func (w *tunnelStreamWriter) Flush() {}

func (w *tunnelStreamWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status != http.StatusOK {
		return w.errBytes.Write(p)
	}
	n, _ := w.buf.Write(p)
	w.drainFrames()
	return n, nil
}

func (w *tunnelStreamWriter) drainFrames() {
	for w.buf.Len() >= 5 {
		b := w.buf.Bytes()
		flags := b[0]
		length := int(binary.BigEndian.Uint32(b[1:5]))
		if w.buf.Len() < 5+length {
			return
		}
		w.buf.Next(5)
		payload := make([]byte, length)
		_, _ = w.buf.Read(payload)

		if flags&0x02 != 0 {
			// End-of-stream envelope: JSON object with optional "error" field.
			w.ended = true
			var endEnv struct {
				Error json.RawMessage `json:"error"`
			}
			if json.Unmarshal(payload, &endEnv) == nil && len(endEnv.Error) > 0 && string(endEnv.Error) != "null" {
				w.send(connectJSONToTunnelErr(w.id, endEnv.Error, "stream error"))
			} else {
				w.send(tunnelUpMessage{Type: "rpc_end", ID: w.id})
			}
			return
		}
		w.send(tunnelUpMessage{
			Type: "rpc_msg",
			ID:   w.id,
			Body: base64.StdEncoding.EncodeToString(payload),
		})
	}
}

func (w *tunnelStreamWriter) finish() {
	if w.ended {
		return
	}
	if w.status != 0 && w.status != http.StatusOK {
		w.send(connectJSONToTunnelErr(w.id, w.errBytes.Bytes(), http.StatusText(w.status)))
		return
	}
	w.drainFrames()
	if !w.ended {
		w.send(tunnelUpMessage{Type: "rpc_end", ID: w.id})
	}
}

func connectJSONToTunnelErr(id string, raw []byte, fallback string) tunnelUpMessage {
	var parsed struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details []struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"details"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		msg := strings.TrimSpace(string(raw))
		if msg == "" {
			msg = fallback
		}
		return tunnelUpMessage{Type: "rpc_err", ID: id, Code: string(api.CodeInternal), Message: msg}
	}
	out := tunnelUpMessage{
		Type:    "rpc_err",
		ID:      id,
		Code:    connectCodeStringToHotbed(parsed.Code),
		Message: parsed.Message,
	}
	for _, d := range parsed.Details {
		if d.Type != "hotbed.v1.ErrorInfo" {
			continue
		}
		b, err := base64.RawStdEncoding.DecodeString(d.Value)
		if err != nil {
			b, err = base64.StdEncoding.DecodeString(d.Value)
		}
		if err != nil {
			continue
		}
		var info pb.ErrorInfo
		if proto.Unmarshal(b, &info) == nil && info.Code != "" {
			out.Code = info.Code
			out.Suggestion = info.Suggestion
			out.Retryable = info.Retryable
			break
		}
	}
	if out.Message == "" {
		out.Message = fallback
	}
	return out
}

func connectCodeStringToHotbed(c string) string {
	switch c {
	case "invalid_argument":
		return string(api.CodeInvalidArgs)
	case "permission_denied", "unauthenticated":
		return string(api.CodeNotAuthorized)
	case "not_found":
		return string(api.CodeNotFound)
	case "already_exists":
		return string(api.CodeConflict)
	case "unavailable", "deadline_exceeded":
		return string(api.CodeWorkerUnreachable)
	case "unimplemented":
		return string(api.CodePlatformNotSupported)
	case "aborted":
		return string(api.CodeCommandStopped)
	default:
		return string(api.CodeInternal)
	}
}

// errUnused keeps errors import documented if refactored.
var _ = errors.Is
