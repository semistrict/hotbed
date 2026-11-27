package api

import (
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultTransportResetCooldown = 5 * time.Second
)

// ResilientTransport wraps a cloneable *http.Transport configured with HTTP/2
// health pings (ReadIdleTimeout / PingTimeout) and ResponseHeaderTimeout so a
// half-open TCP/HTTP2 connection to a remote controller (e.g. Cloudflare
// Workers during an uplink burst) is detected quickly and cannot wedge
// subsequent RPCs. Calling Reset atomically swaps in a fresh *http.Transport
// clone and closes idle connections on the old pool so retries immediately dial
// a new socket instead of multiplexing onto a dead HTTP/2 connection.
type ResilientTransport struct {
	base     *http.Transport
	cur      atomic.Pointer[http.Transport]
	cooldown time.Duration

	resetMu   sync.Mutex
	lastReset time.Time
	resets    atomic.Int64
}

// NewResilientTransport clones base (or http.DefaultTransport when nil) and
// applies conservative timeouts and HTTP/2 PING health checks. The default
// ResponseHeaderTimeout (35s) exceeds the controller's 30s WORKER_RPC_TIMEOUT_MS
// so slow proxied worker RPCs return their real error rather than failing at
// the HTTP/2 header timeout, while wedged connections still fail promptly.
func NewResilientTransport(base *http.Transport) *ResilientTransport {
	return newResilientTransport(base, 35*time.Second)
}

// NewResilientStreamTransport clones base (or http.DefaultTransport when nil)
// with HTTP/2 PING health checks and TCP keepalives, but without a short
// ResponseHeaderTimeout so streaming and long-waiting RPCs (e.g. QueryOutput
// --wait, Poll, FollowLog) do not time out waiting for headers while still
// detecting half-open connections quickly.
func NewResilientStreamTransport(base *http.Transport) *ResilientTransport {
	return newResilientTransport(base, 0)
}

func newResilientTransport(base *http.Transport, defaultHeaderTimeout time.Duration) *ResilientTransport {
	var tr *http.Transport
	if base != nil {
		tr = base.Clone()
	} else if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = dt.Clone()
	} else {
		tr = &http.Transport{}
	}
	if tr.DialContext == nil {
		tr.DialContext = (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 15 * time.Second,
		}).DialContext
	}
	if tr.TLSHandshakeTimeout == 0 {
		tr.TLSHandshakeTimeout = 10 * time.Second
	}
	if tr.ResponseHeaderTimeout == 0 && defaultHeaderTimeout > 0 {
		tr.ResponseHeaderTimeout = defaultHeaderTimeout
	}
	if tr.IdleConnTimeout == 0 || tr.IdleConnTimeout > 60*time.Second {
		tr.IdleConnTimeout = 60 * time.Second
	}
	tr.ForceAttemptHTTP2 = true
	if tr.HTTP2 == nil {
		tr.HTTP2 = &http.HTTP2Config{}
	}
	if tr.HTTP2.SendPingTimeout == 0 {
		tr.HTTP2.SendPingTimeout = 15 * time.Second
	}
	if tr.HTTP2.PingTimeout == 0 {
		tr.HTTP2.PingTimeout = 5 * time.Second
	}
	if tr.HTTP2.WriteByteTimeout == 0 {
		tr.HTTP2.WriteByteTimeout = 15 * time.Second
	}
	rt := &ResilientTransport{
		base:     tr.Clone(),
		cooldown: defaultTransportResetCooldown,
	}
	rt.cur.Store(tr)
	return rt
}

// RoundTrip implements http.RoundTripper.
func (t *ResilientTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return t.cur.Load().RoundTrip(r)
}

// CloseIdleConnections closes idle connections on the active transport.
func (t *ResilientTransport) CloseIdleConnections() {
	if tr := t.cur.Load(); tr != nil {
		tr.CloseIdleConnections()
	}
}

// Reset atomically replaces the active *http.Transport with a fresh clone and
// closes idle connections on the previous pool. Calls within cooldown of a
// prior reset are coalesced so concurrent failing RPCs only recycle once.
func (t *ResilientTransport) Reset() bool {
	t.resetMu.Lock()
	if t.cooldown > 0 && !t.lastReset.IsZero() && time.Since(t.lastReset) < t.cooldown {
		t.resetMu.Unlock()
		return false
	}
	t.lastReset = time.Now()
	t.resetMu.Unlock()

	fresh := t.base.Clone()
	old := t.cur.Swap(fresh)
	t.resets.Add(1)
	if old != nil {
		old.CloseIdleConnections()
	}
	return true
}

// Resets reports how many times the underlying transport pool was recycled.
func (t *ResilientTransport) Resets() int64 {
	return t.resets.Load()
}

// NewResilientHTTPClient returns an *http.Client backed by NewResilientTransport.
func NewResilientHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: NewResilientTransport(nil),
	}
}

// NewResilientStreamClient returns a no-deadline *http.Client backed by
// NewResilientStreamTransport (HTTP/2 PINGs and TCP keepalives, no short
// ResponseHeaderTimeout).
func NewResilientStreamClient() *http.Client {
	return &http.Client{
		Transport: NewResilientStreamTransport(nil),
	}
}

type transportResetter interface {
	Reset() bool
}

type roundTripperUnwrapper interface {
	Unwrap() http.RoundTripper
}

// ResetClientTransport recycles hc's underlying connection pool when hc uses a
// ResilientTransport (including through AuthRoundTripper or Unwrap() wrappers),
// or falls back to hc.CloseIdleConnections.
func ResetClientTransport(hc *http.Client) bool {
	if hc == nil {
		return false
	}
	tr := hc.Transport
	for tr != nil {
		if r, ok := tr.(transportResetter); ok {
			return r.Reset()
		}
		if u, ok := tr.(roundTripperUnwrapper); ok {
			tr = u.Unwrap()
			continue
		}
		if a, ok := tr.(authRoundTripper); ok {
			tr = a.base
			continue
		}
		break
	}
	hc.CloseIdleConnections()
	return false
}
