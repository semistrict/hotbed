package api

import (
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
		&net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNABORTED)},
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

// No error has no code; reading one does not panic.
func TestCodeOfNoErrorIsNone(t *testing.T) {
	if got := CodeOf(nil); got != "" {
		t.Errorf("CodeOf(nil) = %q, want none", got)
	}
	if got := CodeOf(Errorf(CodeNotFound, "gone")); got != CodeNotFound {
		t.Errorf("CodeOf(NOT_FOUND) = %q", got)
	}
}

// fromConnectCode is FromConnect(...).Code, which panics on the path where
// the call went well (board 7027): CodeOf reads a code safely.
var fromConnectCode = regexp.MustCompile(`FromConnect\([^()]*\)\.Code\b`)

// Nothing in the module reads a code as FromConnect(err).Code.
func TestCodesAreReadWithCodeOf(t *testing.T) {
	err := filepath.WalkDir("../..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == "gen" || (strings.HasPrefix(d.Name(), ".") && d.Name() != "." && d.Name() != "..")) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(src), "\n") {
			if fromConnectCode.MatchString(line) && !strings.HasPrefix(strings.TrimSpace(line), "//") {
				t.Errorf("%s:%d reads a code off FromConnect's result, which is nil for no error; use api.CodeOf", path, i+1)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
