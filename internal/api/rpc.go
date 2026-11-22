package api

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectproto"

	hotbedv1 "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
)

// Dial returns a Connect client for the node at addr: a tailnet host name or
// address, with or without a port (defaultPort when it has none).
func Dial(addr string, defaultPort int, hc *http.Client) (*connect.Client, error) {
	hostPort, err := HostPort(addr, defaultPort)
	if err != nil {
		return nil, err
	}
	return connect.NewClient(connecthttp.NewTransport(hc, "http://"+hostPort)), nil
}

// Controller returns clients for the controller at addr.
func Controller(addr string, hc *http.Client) (hotbedv1connect.ClusterServiceClient, hotbedv1connect.WorkspaceServiceClient, error) {
	c, err := Dial(addr, DefaultPort, hc)
	if err != nil {
		return nil, nil, err
	}
	return hotbedv1connect.NewClusterServiceClient(c), hotbedv1connect.NewWorkspaceServiceClient(c), nil
}

// ControllerHostPort turns a controller address as a person types it into
// host:port. "ubu1" becomes "ubu1:7120"; "ubu1:9000" and "[fd7a::1]:7120"
// stay as they are; a bare IPv6 address gets brackets and the default port.
func ControllerHostPort(addr string) (string, error) {
	return HostPort(addr, DefaultPort)
}

// HostPort is ControllerHostPort with another default port.
func HostPort(addr string, defaultPort int) (string, error) {
	invalid := func(why string) error {
		return Errorf(CodeInvalidArgs, "controller address %q %s", addr, why).
			WithSuggestion("give the controller's tailnet name or address, such as ubu1 or ubu1:7120")
	}
	if addr == "" {
		return "", invalid("is empty")
	}
	if strings.Contains(addr, "/") {
		return "", invalid("must be a host, not a URL")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// No port, or a bare IPv6 address, whose colons SplitHostPort reads as a port.
		return net.JoinHostPort(addr, strconv.Itoa(defaultPort)), nil
	}
	if host == "" {
		return "", invalid("has no host")
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
		return "", invalid("has an invalid port")
	}
	return addr, nil
}

// ToConnect is err as a Connect error with an ErrorInfo detail.
func ToConnect(err error) error {
	e := FromConnect(err)
	ce := connect.NewError(connectCode(e.Code), e.Message).WithCause(err)
	if d, derr := connectproto.NewErrorDetail(&hotbedv1.ErrorInfo{Code: string(e.Code), Suggestion: e.Suggestion, Retryable: e.Retryable}); derr == nil {
		ce = ce.WithDetail(d)
	}
	return ce
}

// FromConnect is any error as an *Error: hotbed's own errors as they are, a
// Connect error by its ErrorInfo detail, a failure to reach the other side
// as CONTROLLER_UNREACHABLE (retryable), and anything else as INTERNAL.
func FromConnect(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return &Error{Code: CodeInternal, Message: err.Error()}
	}
	for _, d := range ce.Details() {
		msg, derr := connectproto.UnmarshalErrorDetail(d)
		if derr != nil {
			continue
		}
		if info, ok := msg.(*hotbedv1.ErrorInfo); ok {
			return &Error{Code: Code(info.Code), Message: ce.Message(), Suggestion: info.Suggestion, Retryable: info.Retryable}
		}
	}
	code := ce.Code()
	if brokenConnection(err) {
		// The transport calls it canceled or unknown; it is a peer
		// restarting, out of reach for now.
		code = connect.CodeUnavailable
	}
	switch code {
	case connect.CodeUnavailable, connect.CodeDeadlineExceeded:
		return &Error{
			Code:       CodeControllerUnreachable,
			Message:    err.Error(),
			Suggestion: "check that `tailscale status` lists the controller and that `hotbed serve` is running there",
			Retryable:  true,
		}
	case connect.CodePermissionDenied, connect.CodeUnauthenticated:
		return &Error{Code: CodeNotAuthorized, Message: ce.Message()}
	default:
		return &Error{Code: CodeInternal, Message: err.Error()}
	}
}

// brokenConnection says whether err is a connection that broke under a
// call: reset, closed mid-write, or ended before its answer.
func brokenConnection(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, io.ErrUnexpectedEOF)
}

func connectCode(c Code) connect.Code {
	switch c {
	case CodeInvalidArgs:
		return connect.CodeInvalidArgument
	case CodeNotAuthorized:
		return connect.CodePermissionDenied
	case CodeNotFound:
		return connect.CodeNotFound
	case CodeConflict, CodeAlreadyInitialized:
		return connect.CodeAlreadyExists
	case CodeNotJoined, CodeClusterMismatch, CodeSyncRefused, CodeNotInitialized:
		return connect.CodeFailedPrecondition
	case CodeRepoUnreachable, CodeWorkerUnreachable, CodeControllerUnreachable, CodeTailscaleUnavailable:
		return connect.CodeUnavailable
	case CodePlatformNotSupported:
		return connect.CodeUnimplemented
	default:
		return connect.CodeInternal
	}
}
