// Package api is what every part of hotbed shares about talking to the
// others: ports and intervals, errors with codes, and Connect clients and
// servers for the services in proto/hotbed/v1.
package api

import (
	"context"
	"time"

	"connectrpc.com/connect/v2"
)

const (
	// DefaultPort is the controller's port when an address names none.
	DefaultPort = 7120
	// WorkerPort is where workers serve the controller.
	WorkerPort = 7121
	// WorkerSSHPort is where workers serve interactive workspace shells over SSH.
	WorkerSSHPort = 7122

	// ProgressHeader is a Connect/HTTP response header naming a cockpit path
	// (such as "/commands/f-cladefs-g5-9c34da") or URL where a waiting caller
	// can watch live progress. Any RPC that may wait can set it with
	// SetProgress; the CLI prints the resolved URL to stderr once per path.
	ProgressHeader = "Hotbed-Progress"
)

// SetProgress sets ProgressHeader on the current Connect RPC's response
// headers. Call before sending the first message (or returning from a unary
// handler) so waiting callers receive the progress link immediately.
func SetProgress(ctx context.Context, path string) {
	if path == "" {
		return
	}
	if info, ok := connect.CallInfoForServerContext(ctx); ok {
		info.ResponseHeader().Set(ProgressHeader, path)
	}
}

// ReportInterval is how often a worker reports to the controller, at the
// latest; it reports sooner when something changes. The controller counts a
// worker online while its last report is at most OnlineWindow old.
const (
	ReportInterval = 5 * time.Second
	OnlineWindow   = 3 * ReportInterval
)
