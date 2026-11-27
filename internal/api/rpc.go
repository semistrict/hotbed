package api

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectproto"

	hotbedv1 "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
)

// BuildHeader is the response header that says the controller's build number.
const BuildHeader = "Hotbed-Build"

// IsRemoteURL reports whether addr is an explicit HTTP(S) base URL (such as a
// Cloudflare Workers controller URL) rather than a host[:port].
func IsRemoteURL(addr string) bool {
	return strings.HasPrefix(addr, "https://") || strings.HasPrefix(addr, "http://")
}

// BaseURL normalizes a node address into a base URL without a trailing slash:
// an explicit http:// or https:// URL stays as given; a host[:port]
// becomes "http://<host>:<port>".
func BaseURL(addr string, defaultPort int) (string, error) {
	if IsRemoteURL(addr) {
		u, err := url.Parse(addr)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return "", Errorf(CodeInvalidArgs, "controller URL %q is invalid", addr).
				WithSuggestion("give a full URL such as https://hotbed-controller.example.workers.dev")
		}
		u.Path = strings.TrimRight(u.Path, "/")
		u.RawQuery = ""
		u.Fragment = ""
		return u.String(), nil
	}
	hostPort, err := HostPort(addr, defaultPort)
	if err != nil {
		return "", err
	}
	return "http://" + hostPort, nil
}

// ControllerBaseURL is BaseURL with DefaultPort.
func ControllerBaseURL(addr string) (string, error) {
	return BaseURL(addr, DefaultPort)
}

// AuthInterceptor attaches Bearer authentication and optional worker/client
// identity headers to outgoing Connect calls (used when calling a remote
// Cloudflare Workers controller).
func AuthInterceptor(token, nodeID, hostname string) connect.ClientInterceptor {
	return func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			if info, ok := connect.CallInfoForClientContext(ctx); ok {
				h := info.RequestHeader()
				if token != "" && h.Get("Authorization") == "" {
					h.Set("Authorization", "Bearer "+token)
				}
				if nodeID != "" && h.Get("X-Hotbed-Node-Id") == "" {
					h.Set("X-Hotbed-Node-Id", nodeID)
				}
				if hostname != "" && h.Get("X-Hotbed-Host") == "" {
					h.Set("X-Hotbed-Host", hostname)
				}
			}
			return next(ctx, spec)
		}
	}
}

// AuthRoundTripper wraps base to attach Bearer auth and optional identity
// headers on plain HTTP requests (such as /builds/{build}/{asset} downloads).
func AuthRoundTripper(base http.RoundTripper, token, nodeID, hostname string) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return authRoundTripper{base: base, token: token, nodeID: nodeID, hostname: hostname}
}

type authRoundTripper struct {
	base                    http.RoundTripper
	token, nodeID, hostname string
}

func (a authRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	if a.token != "" && clone.Header.Get("Authorization") == "" {
		clone.Header.Set("Authorization", "Bearer "+a.token)
	}
	if a.nodeID != "" && clone.Header.Get("X-Hotbed-Node-Id") == "" {
		clone.Header.Set("X-Hotbed-Node-Id", a.nodeID)
	}
	if a.hostname != "" && clone.Header.Get("X-Hotbed-Host") == "" {
		clone.Header.Set("X-Hotbed-Host", a.hostname)
	}
	return a.base.RoundTrip(clone)
}

// Dial returns a Connect client for the server at addr: a host name or
// address, with or without a port (defaultPort when it has none), or an
// explicit http(s):// base URL.
func Dial(addr string, defaultPort int, hc *http.Client, interceptors ...connect.ClientInterceptor) (*connect.Client, error) {
	base, err := BaseURL(addr, defaultPort)
	if err != nil {
		return nil, err
	}
	return connect.NewClient(connecthttp.NewTransport(hc, base), interceptors...), nil
}

// Controller returns clients for the controller at addr.
func Controller(addr string, hc *http.Client, interceptors ...connect.ClientInterceptor) (hotbedv1connect.ClusterServiceClient, hotbedv1connect.WorkspaceServiceClient, error) {
	c, err := Dial(addr, DefaultPort, hc, interceptors...)
	if err != nil {
		return nil, nil, err
	}
	return hotbedv1connect.NewClusterServiceClient(c), hotbedv1connect.NewWorkspaceServiceClient(c), nil
}

// ControllerAddr normalizes a controller address as a person types it: an
// http(s):// URL (such as a Cloudflare Workers controller) becomes its base
// URL (ControllerBaseURL), a host name or address its host:port
// (ControllerHostPort).
func ControllerAddr(addr string) (string, error) {
	if IsRemoteURL(addr) {
		return ControllerBaseURL(addr)
	}
	return ControllerHostPort(addr)
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
			WithSuggestion("give the controller's HTTPS URL, such as https://hotbed-controller.example.workers.dev")
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

// CodeOf is err's code as FromConnect reads it, and no code ("") for no
// error. Read a code with it, never as FromConnect(err).Code: FromConnect
// returns nil for nil, so that panics on the path where the call went well.
func CodeOf(err error) Code {
	if e := FromConnect(err); e != nil {
		return e.Code
	}
	return ""
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
			Suggestion: "check your network connection and that HOTBED_CONTROLLER names the controller, or run `hotbed login`",
			Retryable:  true,
		}
	case connect.CodePermissionDenied, connect.CodeUnauthenticated:
		return &Error{Code: CodeNotAuthorized, Message: ce.Message()}
	default:
		return &Error{Code: CodeInternal, Message: err.Error()}
	}
}

// brokenConnection says whether err is a connection that broke under a
// call: reset, aborted, closed mid-write, or ended before its answer.
func brokenConnection(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, io.ErrUnexpectedEOF)
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
	case CodeRepoUnreachable, CodeWorkerUnreachable, CodeControllerUnreachable:
		return connect.CodeUnavailable
	case CodePlatformNotSupported:
		return connect.CodeUnimplemented
	case CodeCommandStopped:
		return connect.CodeAborted
	default:
		return connect.CodeInternal
	}
}
