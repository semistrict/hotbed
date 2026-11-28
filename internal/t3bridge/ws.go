package t3bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/encoding/protojson"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

const (
	defaultWSPingInterval = 25 * time.Second
	defaultWSReadTimeout  = 75 * time.Second
)

// WSURL converts a Hotbed controller HTTP(S) URL into its /ws/questions WebSocket URL.
func WSURL(controllerBaseURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(controllerBaseURL))
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
	u.Path = strings.TrimRight(u.Path, "/") + "/ws/questions"
	return u.String(), nil
}

// InterventionNotice describes an external stop/pause of a session's command
// delivered over /ws/questions so the owning T3 Code thread sees the stop and
// its reason right away.
type InterventionNotice struct {
	CommandID      string `json:"commandId"`
	Seq            int32  `json:"seq"`
	Kind           string `json:"kind"`
	SessionID      string `json:"sessionId"`
	T3CodeThreadID string `json:"t3codeThreadId"`
	Argv           string `json:"argv"`
	By             string `json:"by"`
	Comment        string `json:"comment"`
	Diagnosed      bool   `json:"diagnosed"`
	At             int64  `json:"at"`
}

// Key uniquely identifies an intervention on a command for deduplication.
func (iv *InterventionNotice) Key() string {
	if iv == nil || iv.CommandID == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d", iv.CommandID, iv.Seq)
}

type wireMessage struct {
	Type         string              `json:"type"`
	Event        string              `json:"event,omitempty"`
	Questions    []json.RawMessage   `json:"questions,omitempty"`
	Intervention *InterventionNotice `json:"intervention,omitempty"`
}

func decodeQuestions(raws []json.RawMessage) ([]*pb.Question, error) {
	unmarshaler := protojson.UnmarshalOptions{DiscardUnknown: true}
	out := make([]*pb.Question, 0, len(raws))
	for _, raw := range raws {
		var q pb.Question
		if err := unmarshaler.Unmarshal(raw, &q); err != nil {
			return nil, fmt.Errorf("decode question protojson: %w", err)
		}
		out = append(out, &q)
	}
	return out, nil
}

func runWSLoop(
	ctx context.Context,
	controllerBaseURL string,
	tokenFn func(context.Context) (string, error),
	hc *http.Client,
	pingInterval time.Duration,
	readTimeout time.Duration,
	emit func(Event),
	log *slog.Logger,
) error {
	wsURL, err := WSURL(controllerBaseURL)
	if err != nil {
		return err
	}
	if pingInterval <= 0 {
		pingInterval = defaultWSPingInterval
	}
	if readTimeout <= 0 {
		readTimeout = defaultWSReadTimeout
	}
	if log == nil {
		log = slog.Default()
	}

	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return nil
		}
		connectedAt := time.Now()
		err := serveWSOnce(ctx, wsURL, tokenFn, hc, pingInterval, readTimeout, emit, log)
		emit(Event{Kind: EventDisconnected})
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(connectedAt) > 15*time.Second {
			backoff = time.Second
		}
		log.Warn("t3-bridge WebSocket disconnected; reconnecting", "url", wsURL, "backoff", backoff, "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
			backoff = min(backoff*2, 30*time.Second)
		}
	}
}

func serveWSOnce(
	ctx context.Context,
	wsURL string,
	tokenFn func(context.Context) (string, error),
	hc *http.Client,
	pingInterval time.Duration,
	readTimeout time.Duration,
	emit func(Event),
	log *slog.Logger,
) error {
	headers := http.Header{}
	if tokenFn != nil {
		tokCtx, tokCancel := context.WithTimeout(ctx, 20*time.Second)
		tok, err := tokenFn(tokCtx)
		tokCancel()
		if err != nil {
			return fmt.Errorf("resolve controller auth token: %w", err)
		}
		if strings.TrimSpace(tok) != "" {
			headers.Set("Authorization", "Bearer "+strings.TrimSpace(tok))
		}
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		headers.Set("X-Hotbed-Host", host)
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
	conn.SetReadLimit(16 * 1024 * 1024)
	defer conn.CloseNow()

	log.Info("t3-bridge connected to controller", "url", wsURL)

	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	var writeMu sync.Mutex
	sendText := func(b []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		wctx, cancel := context.WithTimeout(connCtx, 15*time.Second)
		defer cancel()
		return conn.Write(wctx, websocket.MessageText, b)
	}

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

	for {
		readCtx, readCancel := context.WithTimeout(connCtx, readTimeout)
		_, data, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && connCtx.Err() == nil {
				return fmt.Errorf("read timeout after %s (no pong/frame received): %w", readTimeout, err)
			}
			return err
		}
		if bytes.Equal(data, []byte("pong")) || bytes.Equal(data, []byte("ping")) {
			continue
		}
		var frame wireMessage
		if err := json.Unmarshal(data, &frame); err != nil {
			log.Warn("t3-bridge ignoring malformed frame", "err", err)
			continue
		}
		switch frame.Type {
		case "snapshot":
			qs, err := decodeQuestions(frame.Questions)
			if err != nil {
				log.Warn("t3-bridge ignoring frame with undecodable question", "type", frame.Type, "err", err)
				continue
			}
			emit(Event{Kind: EventSnapshot, Questions: qs})
		case "update":
			qs, err := decodeQuestions(frame.Questions)
			if err != nil {
				log.Warn("t3-bridge ignoring frame with undecodable question", "type", frame.Type, "err", err)
				continue
			}
			emit(Event{Kind: EventUpdate, EventName: frame.Event, Questions: qs})
		case "intervention":
			if frame.Intervention != nil && frame.Intervention.CommandID != "" && frame.Intervention.T3CodeThreadID != "" {
				emit(Event{Kind: EventIntervention, Intervention: frame.Intervention})
			}
		}
	}
}
