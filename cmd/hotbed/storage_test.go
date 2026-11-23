package main

import (
	"testing"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// A worker that cannot open its storage (a pool on a disk that came up
// at 0 B, board 9141) heartbeats with the reason: hotbed nodes lists it
// offline and says why, not only that it went quiet.
func TestNodesSayWhyAWorkerHasNoStorage(t *testing.T) {
	c := newFleet(t, 1)
	nd := c.nodes[0]
	nd.halt()
	const why = "ZFS pool not imported: pool hotbed, of dataset hotbed/hotbed, is not imported"
	if _, err := nd.cluster.Heartbeat(t.Context(), &pb.HeartbeatRequest{ClusterId: c.id, StorageError: why, Resources: &pb.Resources{Arch: "arm64", Cpus: 8}}); err != nil {
		t.Fatal(err)
	}
	want := "asahimbp          arm64   8 cpus  offline: cannot open its storage: " + why + "\n"
	if code, out, stderr := c.hotbed("nodes"); code != 0 || out != want {
		t.Fatalf("nodes: exit %d, stdout %q, stderr %q; want %q", code, out, stderr, want)
	}
}
