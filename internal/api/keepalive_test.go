package api_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
)

// streamer serves FollowLog and TraceCommand: each sends one message, then
// keeps the stream open saying nothing (a stream whose relay died without
// a word), or, with keepalive set, sends an empty message that often until
// it has kept the stream open for hold, and ends it.
type streamer struct {
	hotbedv1connect.UnimplementedWorkspaceServiceHandler
	keepalive, hold time.Duration
}

func (s *streamer) serve(ctx context.Context, send func() error) error {
	if err := send(); err != nil {
		return err
	}
	if s.keepalive == 0 {
		<-ctx.Done()
		return nil
	}
	end := time.After(s.hold)
	tick := time.NewTicker(s.keepalive)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-end:
			return nil
		case <-tick.C:
			if err := send(); err != nil {
				return err
			}
		}
	}
}

func (s *streamer) FollowLog(ctx context.Context, _ *pb.FollowLogRequest, stream hotbedv1connect.WorkspaceServiceFollowLogServerStream) error {
	return s.serve(ctx, func() error { return stream.Send(&pb.LogChunk{}) })
}

func (s *streamer) TraceCommand(ctx context.Context, _ *pb.TraceCommandRequest, stream hotbedv1connect.WorkspaceServiceTraceCommandServerStream) error {
	return s.serve(ctx, func() error { return stream.Send(&pb.TraceCommandChunk{}) })
}

// listeningClient is a WorkspaceService client of s that takes a
// keepalive stream silent for silence for dead.
func listeningClient(t *testing.T, s *streamer, silence time.Duration) hotbedv1connect.WorkspaceServiceClient {
	t.Helper()
	cs := connect.NewServer()
	hotbedv1connect.RegisterWorkspaceServiceHandler(cs, s)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, cs)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	conn, err := api.Dial(strings.TrimPrefix(srv.URL, "http://"), api.WorkerPort, srv.Client(), api.HearKeepalives(silence))
	if err != nil {
		t.Fatal(err)
	}
	return hotbedv1connect.NewWorkspaceServiceClient(conn)
}

// receiveAll reads stream to its end, returning how many messages came
// and why it ended (nil at a clean end).
func receiveAll[T any](stream interface {
	Receive() (T, error)
	Close() error
}) (int, error) {
	defer stream.Close()
	n := 0
	for {
		if _, err := stream.Receive(); err != nil {
			if errors.Is(err, io.EOF) {
				return n, nil
			}
			return n, err
		}
		n++
	}
}

// A keepalive stream that goes silent ends, once silent for its silence,
// with a retryable CONTROLLER_UNREACHABLE error its caller can call again
// on, instead of waiting forever (issues #20, #21).
func TestHearKeepalivesEndsASilentStream(t *testing.T) {
	cl := listeningClient(t, &streamer{}, 200*time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	stream, err := cl.FollowLog(ctx, &pb.FollowLogRequest{Id: "c-1", Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	n, err := receiveAll[*pb.LogChunk](stream)
	took := time.Since(start)
	e := api.FromConnect(err)
	if n != 1 || e == nil || e.Code != api.CodeControllerUnreachable || !e.Retryable {
		t.Fatalf("silent FollowLog: %d messages, then %v; want 1, then a retryable CONTROLLER_UNREACHABLE", n, err)
	}
	if took < 200*time.Millisecond || took > 5*time.Second {
		t.Errorf("silent FollowLog ended after %s, want after its 200ms silence", took)
	}
}

// One that keeps sending keepalives lives as long as it is open, however
// much longer than its silence.
func TestHearKeepalivesKeepsAStreamThatSendsKeepalives(t *testing.T) {
	cl := listeningClient(t, &streamer{keepalive: 50 * time.Millisecond, hold: time.Second}, 200*time.Millisecond)
	stream, err := cl.FollowLog(t.Context(), &pb.FollowLogRequest{Id: "c-1", Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	n, err := receiveAll[*pb.LogChunk](stream)
	if err != nil || n < 10 {
		t.Fatalf("FollowLog with keepalives: %d messages, then %v; want at least 10, then its clean end", n, err)
	}
}

// A stream that promises no keepalives is never taken for dead: TraceCommand
// says nothing until its trace is done.
func TestHearKeepalivesLeavesOtherStreamsAlone(t *testing.T) {
	cl := listeningClient(t, &streamer{}, 100*time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	stream, err := cl.TraceCommand(ctx, &pb.TraceCommandRequest{Id: "c-1"})
	if err != nil {
		t.Fatal(err)
	}
	n, err := receiveAll[*pb.TraceCommandChunk](stream)
	if n != 1 || connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("silent TraceCommand: %d messages, then %v; want 1, then its caller's deadline", n, err)
	}
}

// A caller slow to take what came (its output piped to a pager no one
// scrolls) does not make a live stream look dead: only time spent waiting
// in Receive counts.
func TestHearKeepalivesCountsOnlyTimeSpentWaiting(t *testing.T) {
	cl := listeningClient(t, &streamer{keepalive: 50 * time.Millisecond, hold: time.Second}, 200*time.Millisecond)
	stream, err := cl.FollowLog(t.Context(), &pb.FollowLogRequest{Id: "c-1", Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Receive(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // the caller busy elsewhere, past its silence
	n, err := receiveAll[*pb.LogChunk](stream)
	if err != nil || n < 1 {
		t.Fatalf("FollowLog after a slow caller: %d more messages, then %v; want more, then its clean end", n, err)
	}
}
