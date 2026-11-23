package tailnettest

import (
	"context"
	"net"
	"net/http"
	"testing"

	"github.com/semistrict/hotbed/internal/tailnet"
)

type staticAddr string

func (a staticAddr) Network() string { return "tcp" }
func (a staticAddr) String() string  { return string(a) }

// On Linux, two clients dialing different loopback servers can be assigned the
// exact same local ephemeral port. WhoIs must distinguish them by the 4-tuple
// using http.LocalAddrContextKey so one peer never overwrites another's identity.
func TestWhoIsDistinguishesSameLocalPortAcrossServers(t *testing.T) {
	f := New(tailnet.Self{Peer: tailnet.Peer{NodeID: "ctrl", Hostname: "ubu1", Login: "ramon@"}})
	worker := tailnet.Peer{NodeID: "n1", Hostname: "asahimbp", Login: "ramon@"}
	laptop := tailnet.Peer{NodeID: "n2", Hostname: "laptop", Login: "ramon@"}

	const sharedClient = "127.0.0.1:45678"
	const ctrlServer = "127.0.0.1:17120"
	const workerServer = "127.0.0.1:17121"

	f.mu.Lock()
	f.pairs[connPair{client: sharedClient, server: ctrlServer}] = worker
	f.peers[sharedClient] = worker
	// Second peer connects to a different server using the same ephemeral local port.
	f.pairs[connPair{client: sharedClient, server: workerServer}] = laptop
	f.peers[sharedClient] = laptop
	f.mu.Unlock()

	ctrlCtx := context.WithValue(t.Context(), http.LocalAddrContextKey, net.Addr(staticAddr(ctrlServer)))
	got, err := f.WhoIs(ctrlCtx, sharedClient)
	if err != nil {
		t.Fatalf("WhoIs(ctrl): %v", err)
	}
	if got.NodeID != worker.NodeID || got.Hostname != worker.Hostname {
		t.Errorf("WhoIs(ctrl) = %+v, want %+v", got, worker)
	}

	workerCtx := context.WithValue(t.Context(), http.LocalAddrContextKey, net.Addr(staticAddr(workerServer)))
	got, err = f.WhoIs(workerCtx, sharedClient)
	if err != nil {
		t.Fatalf("WhoIs(worker): %v", err)
	}
	if got.NodeID != laptop.NodeID || got.Hostname != laptop.Hostname {
		t.Errorf("WhoIs(worker) = %+v, want %+v", got, laptop)
	}
}
