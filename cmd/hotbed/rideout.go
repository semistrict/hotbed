package main

import (
	"fmt"
	"sync"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/semistrict/hotbed/internal/api"
)

// peerRideOut is how long the CLI waits for the controller or a worker it
// cannot reach before failing: long enough for a deploy to restart it, so
// no agent needs a retry loop of its own.
const peerRideOut = 3 * time.Minute

// rideOut is the interceptors that wait out a restart of the peer named
// who: a stream that falls silent is taken for dead (d.silence, none when
// 0), and a call that cannot reach the peer waits for it, for up to
// d.rideOut, saying so once on stderr (none when d.rideOut is 0).
func rideOut(d deps, who string) []connect.ClientInterceptor {
	var out []connect.ClientInterceptor
	if d.rideOut > 0 {
		var once sync.Once
		reset := func() {
			api.ResetClientTransport(d.http)
			api.ResetClientTransport(d.stream)
		}
		out = append(out, api.RideOutWithReset(d.rideOut, func(err error) {
			once.Do(func() {
				fmt.Fprintf(d.stderr, "hotbed: %s is out of reach (%s); waiting up to %s for it to come back\n", who, api.FromConnect(err).Message, d.rideOut)
			})
		}, reset))
	}
	if d.silence > 0 {
		out = append(out, api.HearKeepalives(d.silence))
	}
	return out
}
