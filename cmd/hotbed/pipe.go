package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/semistrict/hotbed/internal/api"
)

// dialPipe opens a byte stream to a workspace's worker through the
// controller (its /ws/pipe, relayed over the worker's tunnel): kind "ssh" is
// the worker's shell server, kind "port" the port a command of the
// workspace serves on. It returns once the worker's end is open.
func (f controllerClient) dialPipe(ctx context.Context, d deps, workspace, kind string, port int) (net.Conn, error) {
	base, err := api.ControllerBaseURL(f.Controller)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/ws/pipe"
	q := url.Values{"workspace": {workspace}, "kind": {kind}}
	if port > 0 {
		q.Set("port", strconv.Itoa(port))
	}
	u.RawQuery = q.Encode()

	hdr := http.Header{}
	tok, err := resolveControllerAuthToken(ctx, d.getenv, f.Controller, d.http)
	if err != nil {
		return nil, err
	}
	if tok != "" {
		hdr.Set("Authorization", "Bearer "+tok)
	}
	if host, err := os.Hostname(); err == nil {
		hdr.Set("X-Hotbed-Host", host)
	}
	conn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPHeader: hdr, HTTPClient: d.stream})
	if err != nil {
		return nil, api.Errorf(api.CodeWorkerUnreachable, "open a %s pipe to workspace %s through the controller: %v", kind, workspace, err)
	}
	typ, msg, err := conn.Read(ctx)
	if err != nil {
		why := err.Error()
		var ce websocket.CloseError
		if errors.As(err, &ce) && ce.Reason != "" {
			why = ce.Reason
		}
		conn.CloseNow()
		return nil, api.Errorf(api.CodeWorkerUnreachable, "the worker of workspace %s opened no %s pipe: %s", workspace, kind, why)
	}
	var opened struct {
		Type string `json:"type"`
	}
	if typ != websocket.MessageText || json.Unmarshal(msg, &opened) != nil || opened.Type != "opened" {
		conn.CloseNow()
		return nil, api.Errorf(api.CodeInternal, "the controller began a %s pipe with %q, not opened", kind, msg)
	}
	conn.SetReadLimit(4 << 20)
	return websocket.NetConn(context.WithoutCancel(ctx), conn, websocket.MessageBinary), nil
}

// localPipe serves one connection to a listener on loopback by a pipe the
// controller relays to workspace's worker (dialPipe): what ssh connects to
// for hotbed shell. It returns the listener's address; the listener closes
// after its one connection, or when ctx ends.
func (f controllerClient) localPipe(ctx context.Context, d deps, workspace, kind string, port int) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	go func() {
		conn, err := ln.Accept()
		ln.Close()
		if err != nil {
			return
		}
		defer conn.Close()
		remote, err := f.dialPipe(ctx, d, workspace, kind, port)
		if err != nil {
			fmt.Fprintf(d.stderr, "hotbed: %v\n", toAPIError(err))
			return
		}
		defer remote.Close()
		done := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(remote, conn); done <- struct{}{} }()
		go func() { _, _ = io.Copy(conn, remote); done <- struct{}{} }()
		<-done
	}()
	return ln.Addr().String(), nil
}

// portServing reports whether a server takes connections on port in
// workspace, as the controller reaches it through the worker. A VM's port is
// a proxy on its worker, which accepts at once and closes the connection
// when it cannot reach the port in the VM: a pipe closed straight away is no
// server; one held open, or answered, is.
func (f controllerClient) portServing(ctx context.Context, d deps, workspace string, port int) bool {
	conn, err := f.dialPipe(ctx, d, workspace, "port", port)
	if err != nil {
		return false
	}
	return serves(conn)
}

// serves reports whether the server conn is connected to holds it open or
// answers within a second, rather than closing it straight away; it closes
// conn.
func serves(conn net.Conn) bool {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	n, err := conn.Read(b[:])
	var nerr net.Error
	return n > 0 || (errors.As(err, &nerr) && nerr.Timeout())
}
