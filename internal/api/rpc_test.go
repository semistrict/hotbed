package api

import (
	"errors"
	"io"
	"net"
	"os"
	"syscall"
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

// A connection that broke under a call (the worker or controller
// restarting) is a peer out of reach for now, whatever code the transport
// gave it: retryable, as a refused one is.
func TestBrokenConnectionsAreRetryable(t *testing.T) {
	for _, cause := range []error{
		&net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)},
		&net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", syscall.EPIPE)},
		io.ErrUnexpectedEOF,
	} {
		for _, code := range []connect.Code{connect.CodeCanceled, connect.CodeUnknown, connect.CodeInternal} {
			e := FromConnect(connect.NewError(code, cause.Error()).WithCause(cause))
			if e.Code != CodeControllerUnreachable || !e.Retryable {
				t.Errorf("%v from %v = %+v, want unreachable and retryable", code, cause, e)
			}
		}
	}
	if e := FromConnect(connect.NewError(connect.CodeInternal, "boom").WithCause(errors.New("boom"))); e.Retryable {
		t.Errorf("an internal error = %+v, want it not retryable", e)
	}
}
