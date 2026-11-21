// Package api is what every part of hotbed shares about talking to the
// others: ports and intervals, errors with codes, and Connect clients and
// servers for the services in proto/hotbed/v1.
package api

import "time"

const (
	// DefaultPort is the controller's port when an address names none.
	DefaultPort = 7120
	// WorkerPort is where workers serve the controller.
	WorkerPort = 7121
)

// ReportInterval is how often a worker reports to the controller, at the
// latest; it reports sooner when something changes. The controller counts a
// worker online while its last report is at most OnlineWindow old.
const (
	ReportInterval = 5 * time.Second
	OnlineWindow   = 3 * ReportInterval
)
