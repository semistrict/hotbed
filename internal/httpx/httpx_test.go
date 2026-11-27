package httpx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"

	"github.com/semistrict/hotbed/internal/api"
)

// WithStaticPeer names every request's caller, which reaches the handler.
func TestWithStaticPeerNamesTheCaller(t *testing.T) {
	want := Peer{NodeID: "wabc", Hostname: "ubu1", Login: "ramon@"}
	var got Peer
	h := WithStaticPeer(want, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = PeerFrom(r.Context())
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "http://hotbed/", nil))
	if got != want {
		t.Errorf("caller = %+v, want %+v", got, want)
	}
}

// A Connect server wired without a transport that names its caller serves
// no one; one that names it serves the call.
func TestServerWithoutACallerRefuses(t *testing.T) {
	called := false
	next := identified(func(context.Context, connect.Spec, connect.ServerStream) error { called = true; return nil })
	err := next(t.Context(), connect.Spec{}, nil)
	if e := api.FromConnect(err); e.Code != api.CodeNotAuthorized || called {
		t.Errorf("call with no caller: err %v, handler called %t, want NOT_AUTHORIZED before the handler", err, called)
	}
	if err := next(WithPeer(t.Context(), Peer{NodeID: "wabc"}), connect.Spec{}, nil); err != nil || !called {
		t.Errorf("call with a caller: err %v, handler called %t, want it handled", err, called)
	}
}

// Client-canceled requests do not emit ERROR call failed logs, while genuine
// internal errors (and bugs returning context.Canceled when ctx is not
// canceled) still do (board 7699).
func TestHotbedErrorsSkipsClientCanceledLogs(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	canceledCtx, cancel := context.WithCancel(t.Context())
	cancel()

	spec := connect.Spec{Procedure: "/hotbed.v1.ClusterService/GetSnapshot"}

	for _, tc := range []struct {
		name    string
		ctx     context.Context
		err     error
		wantLog bool
	}{
		{"raw context.Canceled with canceled ctx", canceledCtx, context.Canceled, false},
		{"wrapped context.Canceled with canceled ctx", canceledCtx, fmt.Errorf("list nodes: %w", context.Canceled), false},
		{"forwarded worker canceled with canceled ctx", canceledCtx, api.Errorf(api.CodeInternal, "canceled: context canceled"), false},
		{"connect.CodeCanceled with canceled ctx", canceledCtx, connect.NewError(connect.CodeCanceled, "gone"), false},
		{"genuine internal error with canceled ctx", canceledCtx, errors.New("database disk image is malformed"), true},
		{"context.Canceled when request ctx is still live", t.Context(), context.Canceled, true},
		{"genuine internal error with live ctx", t.Context(), errors.New("boom"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs.Reset()
			fn := hotbedErrors(func(context.Context, connect.Spec, connect.ServerStream) error {
				return tc.err
			})
			_ = fn(tc.ctx, spec, nil)
			gotLog := strings.Contains(logs.String(), "call failed")
			if gotLog != tc.wantLog {
				t.Errorf("logged=%v, want %v (logs=%q)", gotLog, tc.wantLog, logs.String())
			}
		})
	}
}
