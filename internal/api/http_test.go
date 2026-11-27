package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewResilientTransportConfiguresHTTP2HealthChecks(t *testing.T) {
	rt := NewResilientTransport(nil)
	tr := rt.cur.Load()
	if tr.HTTP2 == nil {
		t.Fatal("expected HTTP2 config to be set")
	}
	if tr.HTTP2.SendPingTimeout <= 0 || tr.HTTP2.PingTimeout <= 0 || tr.HTTP2.WriteByteTimeout <= 0 {
		t.Fatalf("expected positive HTTP2 SendPingTimeout/PingTimeout/WriteByteTimeout, got %+v", tr.HTTP2)
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Fatalf("expected positive ResponseHeaderTimeout, got %v", tr.ResponseHeaderTimeout)
	}
}

type unwrapRoundTripper struct{ base http.RoundTripper }

func (u unwrapRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return u.base.RoundTrip(r)
}

func (u unwrapRoundTripper) Unwrap() http.RoundTripper { return u.base }

func TestNewResilientStreamClientConfiguresHTTP2PingsWithoutHeaderTimeout(t *testing.T) {
	hc := NewResilientStreamClient()
	if hc.Timeout != 0 {
		t.Fatalf("stream client Timeout = %v, want 0", hc.Timeout)
	}
	rt, ok := hc.Transport.(*ResilientTransport)
	if !ok {
		t.Fatalf("stream client Transport = %T, want *ResilientTransport", hc.Transport)
	}
	rt.cooldown = 0
	tr := rt.cur.Load()
	if tr.ResponseHeaderTimeout != 0 {
		t.Fatalf("stream transport ResponseHeaderTimeout = %v, want 0", tr.ResponseHeaderTimeout)
	}
	if tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout <= 0 || tr.HTTP2.PingTimeout <= 0 || tr.HTTP2.WriteByteTimeout <= 0 {
		t.Fatalf("expected positive HTTP2 SendPingTimeout/PingTimeout/WriteByteTimeout, got %+v", tr.HTTP2)
	}
	wrapped := &http.Client{Transport: unwrapRoundTripper{base: rt}}
	if !ResetClientTransport(wrapped) {
		t.Fatal("expected ResetClientTransport to unwrap and reset ResilientTransport")
	}
	if got := rt.Resets(); got != 1 {
		t.Fatalf("Resets() = %d, want 1", got)
	}
}

func TestResilientTransportResetEscapesWedgedHTTP2Connection(t *testing.T) {
	var attempts atomic.Int32
	unblock := make(chan struct{})
	defer close(unblock)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			// Simulate a half-open / stalled HTTP/2 stream that never sends headers.
			select {
			case <-unblock:
			case <-r.Context().Done():
			}
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, r.RemoteAddr)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	base := srv.Client().Transport.(*http.Transport)
	rt := NewResilientTransport(base)
	rt.cooldown = 0
	hc := &http.Client{Transport: rt}

	// 1. First request times out on connection 1.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel1()
	req1, _ := http.NewRequestWithContext(ctx1, http.MethodGet, srv.URL, nil)
	if _, err := hc.Do(req1); err == nil {
		t.Fatal("expected first request to time out")
	}

	// 2. Reset swaps in a fresh transport pool so retry dials a fresh socket.
	if !ResetClientTransport(hc) {
		t.Fatal("expected ResetClientTransport to reset transport")
	}
	if got := rt.Resets(); got != 1 {
		t.Fatalf("Resets() = %d, want 1", got)
	}

	// 3. Second request succeeds immediately on the fresh pool.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	req2, _ := http.NewRequestWithContext(ctx2, http.MethodGet, srv.URL, nil)
	resp, err := hc.Do(req2)
	if err != nil {
		t.Fatalf("second request after reset failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
