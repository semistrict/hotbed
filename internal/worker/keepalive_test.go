package worker

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
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

// A FollowLog that waits on a paused command (resuming it) sends keepalives
// while the resume waits on the command's freeze, which pushing its memory
// to swap can hold for long.
func TestFollowLogSendsKeepalivesWhileItsWaitResumes(t *testing.T) {
	client, eng := keepaliveClient(t)
	eng.mu.Lock()
	c := eng.newCommand(&pb.CommandRecord{
		Command: &pb.Command{Id: "frozen-1", WorkspaceId: "w", State: paused},
	})
	eng.commands["frozen-1"] = c
	eng.mu.Unlock()
	c.freezeMu.Lock() // a freeze still under way

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stream, err := client.FollowLog(ctx, &pb.FollowLogRequest{Id: "frozen-1", Offset: 7, Follow: true, Wait: true})
	if err != nil {
		c.freezeMu.Unlock()
		t.Fatal(err)
	}
	defer stream.Close()
	for i := range 3 {
		chunk, err := stream.Receive()
		if err != nil {
			c.freezeMu.Unlock()
			t.Fatalf("keepalive %d: %v", i, err)
		}
		if want := (&pb.LogChunk{NextOffset: 7}); !proto.Equal(chunk, want) {
			c.freezeMu.Unlock()
			t.Fatalf("keepalive %d = %v, want %v", i, chunk, want)
		}
	}
	c.freezeMu.Unlock()
}

// A client takes a stream silent for api.StreamSilence for dead and follows
// again: it must be back before the worker stops its attached command as
// orphaned, with room for a controller deploy's reconnect (issue #20).
func TestStreamSilenceLeavesRoomBeforeOrphanAfter(t *testing.T) {
	if room := orphanAfter - api.StreamSilence; room < 10*time.Second {
		t.Errorf("orphanAfter %s leaves %s past api.StreamSilence %s to follow again, want at least 10s", orphanAfter, room, api.StreamSilence)
	}
}
