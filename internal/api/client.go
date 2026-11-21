package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
)

// Client talks to a controller.
type Client struct {
	base string
	http *http.Client
}

// NewClient returns a client for the controller at addr: a tailnet host name
// or address, with or without a port (DefaultPort when it has none).
func NewClient(addr string, hc *http.Client) (*Client, error) {
	return NewClientPort(addr, DefaultPort, hc)
}

// NewClientPort is NewClient with another default port, for workers.
func NewClientPort(addr string, port int, hc *http.Client) (*Client, error) {
	hostPort, err := HostPort(addr, port)
	if err != nil {
		return nil, err
	}
	return &Client{base: "http://" + hostPort, http: hc}, nil
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

// Join asks the controller to admit the calling node as a worker.
func (c *Client) Join(ctx context.Context) (JoinResponse, error) {
	var out JoinResponse
	err := c.Do(ctx, http.MethodPost, "/v1/join", nil, &out)
	return out, err
}

// Report sends a worker's report.
func (c *Client) Report(ctx context.Context, r Report) (ReportReply, error) {
	var out ReportReply
	err := c.Do(ctx, http.MethodPost, "/v1/report", r, &out)
	return out, err
}

// Nodes lists the cluster's workers.
func (c *Client) Nodes(ctx context.Context) (NodesResponse, error) {
	var out NodesResponse
	err := c.Do(ctx, http.MethodGet, "/v1/nodes", nil, &out)
	return out, err
}

// Do sends in as JSON (when not nil) and decodes the reply into out (when not nil).
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	return c.do(ctx, method, path, in, out)
}

// Stream sends body as is and returns the reply for the caller to read and
// close. A failure status comes back as an *Error.
func (c *Client) Stream(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, fmt.Errorf("build %s request: %w", path, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, unreachable(err)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, decodeError(resp)
	}
	return resp, nil
}

func unreachable(err error) *Error {
	return &Error{
		Code:       CodeControllerUnreachable,
		Message:    err.Error(),
		Suggestion: "check that `tailscale status` lists the controller and that `hotbed run` is running there",
		Retryable:  true,
	}
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode %s request: %w", path, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return fmt.Errorf("build %s request: %w", path, err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return unreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return decodeError(resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s reply: %w", path, err)
	}
	return nil
}

// decodeError reads the Error a controller sends with a failure status. A body
// that is not one (a proxy's error page, say) becomes an INTERNAL error that
// keeps the status and the start of the body.
func decodeError(resp *http.Response) error {
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("read error reply (HTTP %d): %w", resp.StatusCode, err)
	}
	var e Error
	if err := json.Unmarshal(b, &e); err != nil || e.Code == "" {
		return Errorf(CodeInternal, "controller replied HTTP %d: %.200s", resp.StatusCode, b)
	}
	return &e
}
