package api

import (
	"errors"
	"testing"

	"connectrpc.com/connect/v2"
)

func TestHostPort(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"ubu1", "ubu1:7120"},
		{"ubu1:9000", "ubu1:9000"},
		{"100.73.156.10", "100.73.156.10:7120"},
		{"[fd7a:115c:a1e0::1]:7120", "[fd7a:115c:a1e0::1]:7120"},
		{"fd7a:115c:a1e0::1", "[fd7a:115c:a1e0::1]:7120"},
	} {
		got, err := ControllerHostPort(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ControllerHostPort(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{"", "http://ubu1", ":7120", "ubu1:", "ubu1:0", "ubu1:70000", "ubu1:http"} {
		_, err := ControllerHostPort(in)
		var e *Error
		if !errors.As(err, &e) || e.Code != CodeInvalidArgs {
			t.Errorf("ControllerHostPort(%q) error = %v, want INVALID_ARGS", in, err)
		}
	}
}

// An error's code, suggestion and retryability survive the wire.
func TestErrorsRoundTrip(t *testing.T) {
	sent := Errorf(CodeRepoUnreachable, "cannot read ratel's remote").WithSuggestion("check the repo's URL")
	sent.Retryable = true
	wire := ToConnect(sent)
	var ce *connect.Error
	if !errors.As(wire, &ce) || ce.Code() != connect.CodeUnavailable {
		t.Fatalf("ToConnect = %v, want a connect Unavailable error", wire)
	}
	got := FromConnect(wire)
	if *got != *sent {
		t.Errorf("round trip = %+v, want %+v", got, sent)
	}
}

func TestUnreachableIsRetryable(t *testing.T) {
	got := FromConnect(connect.NewError(connect.CodeUnavailable, "dial tcp: connection refused"))
	if got.Code != CodeControllerUnreachable || !got.Retryable {
		t.Errorf("FromConnect(unavailable) = %+v", got)
	}
}
