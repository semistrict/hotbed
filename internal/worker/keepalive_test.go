package worker

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/cmdlog"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/httpx"
)

// keepaliveClient is a client of an engine whose quiet streams send
// keepalives every 20ms, and the engine.
func keepaliveClient(t *testing.T) (hotbedv1connect.WorkspaceServiceClient, *Engine) {
	t.Helper()
	eng, _ := outputEngine(t, cmdlog.Limit{})
	eng.cfg.StreamKeepalive = 20 * time.Millisecond
	return clientAs(t, httpx.Peer{NodeID: "nClient", Hostname: "laptop", Login: "ramon@"}, eng), eng
}

// A WatchWorkspace of a workspace that does not change sends it again, as
// its keepalive.
func TestWatchWorkspaceSendsKeepalivesWhileQuiet(t *testing.T) {
	client, _ := keepaliveClient(t)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stream, err := client.WatchWorkspace(ctx, &pb.WatchWorkspaceRequest{Id: "w"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	first, err := stream.Receive()
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		ws, err := stream.Receive()
		if err != nil {
			t.Fatalf("keepalive %d: %v", i, err)
		}
		if !proto.Equal(ws, first) {
			t.Fatalf("keepalive %d = %v, want the workspace again: %v", i, ws, first)
		}
	}
}
