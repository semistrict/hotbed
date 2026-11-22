package main

import (
	"encoding/json"
	"net/netip"

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
