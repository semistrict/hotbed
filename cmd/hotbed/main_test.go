package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"testing"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/tailnet"
)

var (
	ubu1   = tailnet.Self{Peer: tailnet.Peer{NodeID: "nUbu1", Hostname: "ubu1", Login: "ramon@"}, Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
	laptop = tailnet.Peer{NodeID: "nLaptop", Hostname: "laptop", Login: "ramon@"}
)

type output struct {
	Version string          `json:"version"`
	OK      bool            `json:"ok"`
	Command string          `json:"command"`
	Data    json.RawMessage `json:"data"`
	Error   *api.Error      `json:"error"`
}

// at is a machine whose controller is addr.
func at(addr string) func() (string, error) {
	return func() (string, error) { return addr, nil }
}

func TestTildePath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in, home, want string
	}{
		{"/Users/alice", "/Users/alice", "~"},
		{"/Users/alice/.hotbed/sessions.json", "/Users/alice", "~/.hotbed/sessions.json"},
		{"/Users/alice/.hotbed/sessions.json", "/Users/alice/", "~/.hotbed/sessions.json"},
		{"/Users/alice2/.hotbed", "/Users/alice", "/Users/alice2/.hotbed"},
		{"/var/lib/hotbed", "/Users/alice", "/var/lib/hotbed"},
		{"/var/lib/hotbed", "", "/var/lib/hotbed"},
	} {
		if got := tildePathWithHome(tc.in, tc.home); got != tc.want {
			t.Errorf("tildePathWithHome(%q, %q) = %q, want %q", tc.in, tc.home, got, tc.want)
		}
	}
}

func TestInterruptedExits130(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"nodes"}, deps{
		here:       newTestHere(t, nil).here(),
		http:       &http.Client{},
		stream:     &http.Client{},
		controller: at("127.0.0.1:1"),
		stdout:     &stdout,
		stderr:     &stderr,
	})
	if code != exitInterrupted {
		t.Fatalf("code = %d, want %d (stdout=%q stderr=%q)", code, exitInterrupted, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("want no output on interrupt, got stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
