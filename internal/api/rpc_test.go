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
