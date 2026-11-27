package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"connectrpc.com/connect/v2"

	"github.com/semistrict/hotbed/internal/cmdlog"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
)

type outputCaptureCluster struct {
	hotbedv1connect.ClusterServiceClient
	mu      sync.Mutex
	failOne bool
	reqs    []*pb.PushCommandOutputRequest
	chunks  map[string]map[int64]*pb.CommandOutputChunk
	eof     map[string]bool
}

func (c *outputCaptureCluster) PushCommandOutput(_ context.Context, req *pb.PushCommandOutputRequest) (*pb.PushCommandOutputResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failOne {
		c.failOne = false
		return nil, connect.NewError(connect.CodeUnavailable, "transient controller error")
	}
	c.reqs = append(c.reqs, req)
	if c.chunks == nil {
		c.chunks = map[string]map[int64]*pb.CommandOutputChunk{}
		c.eof = map[string]bool{}
	}
	byOff := c.chunks[req.CommandId]
	if byOff == nil {
		byOff = map[int64]*pb.CommandOutputChunk{}
		c.chunks[req.CommandId] = byOff
	}
	var maxNext int64
	for _, ch := range req.Chunks {
		if _, dup := byOff[ch.Offset]; !dup {
			byOff[ch.Offset] = ch
		}
		if ch.NextOffset > maxNext {
			maxNext = ch.NextOffset
		}
	}
	for _, ch := range byOff {
		if ch.NextOffset > maxNext {
			maxNext = ch.NextOffset
		}
	}
	if req.Eof {
		c.eof[req.CommandId] = true
	}
	return &pb.PushCommandOutputResponse{AckedOffset: maxNext}, nil
}

func TestOutputPusherStreamsIncrementallyRetriesAndFlushesEOF(t *testing.T) {
	eng, _ := outputEngine(t, cmdlog.Limit{})
	id := "c-push-out"
	logPath := filepath.Join(eng.logsDir(), id+".log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// Seed a running command in the engine.
	eng.mu.Lock()
	eng.commands[id] = &command{
		rec: &pb.CommandRecord{Command: &pb.Command{Id: id, State: pb.CommandState_COMMAND_STATE_RUNNING}},
	}
	eng.mu.Unlock()

	appendTimed := func(s string, ts int64) {
		t.Helper()
		if _, err := f.WriteString(s); err != nil {
			t.Fatal(err)
		}
		if _, err := cmdlog.RecordIfGrown(logPath, ts); err != nil {
			t.Fatal(err)
		}
	}

	cluster := &outputCaptureCluster{}
	p := newOutputPusher(eng, cluster)

	// Write line 1 while running -> pushed immediately with timestamp and eof=false.
	const ts1 = int64(1_700_000_010_000)
	appendTimed("line one\n", ts1)
	if more, err := p.once(t.Context()); err != nil || more {
		t.Fatalf("first push: more=%v err=%v", more, err)
	}
	if len(cluster.reqs) != 1 || cluster.reqs[0].Eof || len(cluster.reqs[0].Chunks) != 1 {
		t.Fatalf("first push reqs = %+v", cluster.reqs)
	}
	ch0 := cluster.reqs[0].Chunks[0]
	if string(ch0.Data) != "line one\n" || ch0.Offset != 0 || ch0.NextOffset != 9 || ch0.ReceivedAt != ts1 {
		t.Fatalf("first chunk = %+v", ch0)
	}

	// Write line 2 at a later timestamp; simulate a transient controller error then retry.
	const ts2 = int64(1_700_000_020_000)
	appendTimed("line two\n", ts2)
	cluster.failOne = true
	if _, err := p.once(t.Context()); err == nil {
		t.Fatal("expected transient error on push")
	}
	// Offset must NOT have advanced past line 1.
	if p.streams[id].offset != 9 {
		t.Fatalf("offset advanced on failed push: %d, want 9", p.streams[id].offset)
	}
	if more, err := p.once(t.Context()); err != nil || more {
		t.Fatalf("retry push: more=%v err=%v", more, err)
	}
	if len(cluster.reqs) != 2 || len(cluster.reqs[1].Chunks) != 1 {
		t.Fatalf("retry push reqs = %+v", cluster.reqs)
	}
	ch1 := cluster.reqs[1].Chunks[0]
	if string(ch1.Data) != "line two\n" || ch1.Offset != 9 || ch1.NextOffset != 18 || ch1.ReceivedAt != ts2 {
		t.Fatalf("second chunk = %+v", ch1)
	}

	// Command finishes with a final line -> pushed with eof=true, and subsequent passes are no-ops.
	const ts3 = int64(1_700_000_030_000)
	appendTimed("done\n", ts3)
	eng.mu.Lock()
	eng.commands[id].cmd().State = pb.CommandState_COMMAND_STATE_SUCCEEDED
	eng.mu.Unlock()

	if more, err := p.once(t.Context()); err != nil || more {
		t.Fatalf("final push: more=%v err=%v", more, err)
	}
	if len(cluster.reqs) != 3 || !cluster.reqs[2].Eof || len(cluster.reqs[2].Chunks) != 1 {
		t.Fatalf("final push reqs = %+v", cluster.reqs)
	}
	ch2 := cluster.reqs[2].Chunks[0]
	if string(ch2.Data) != "done\n" || ch2.Offset != 18 || ch2.NextOffset != 23 || ch2.ReceivedAt != ts3 {
		t.Fatalf("third chunk = %+v", ch2)
	}
	if !p.streams[id].eofSent || p.hasActiveWork() {
		t.Fatalf("after eof: eofSent=%v hasActiveWork=%v", p.streams[id].eofSent, p.hasActiveWork())
	}

	// Subsequent once() does not re-send.
	if more, err := p.once(t.Context()); err != nil || more || len(cluster.reqs) != 3 {
		t.Fatalf("post-eof push: more=%v err=%v reqs=%d", more, err, len(cluster.reqs))
	}
}

// A live command's bytes written but not yet stamped (a recorder writes
// the byte before its timestamp) wait for their timestamp: CommandDO keeps
// a chunk's timestamp for good, so one pushed early reads as unstamped
// forever (TestLogsTimestampsOption's tail lost a line's time on CI).
// Once the command ended, what is left goes as it is.
func TestOutputPusherHoldsBytesUntilStamped(t *testing.T) {
	eng, _ := outputEngine(t, cmdlog.Limit{})
	id := "c-push-stamp"
	logPath := filepath.Join(eng.logsDir(), id+".log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	eng.mu.Lock()
	eng.commands[id] = &command{
		rec: &pb.CommandRecord{Command: &pb.Command{Id: id, State: pb.CommandState_COMMAND_STATE_RUNNING}},
	}
	eng.mu.Unlock()
	cluster := &outputCaptureCluster{}
	p := newOutputPusher(eng, cluster)
	type pushed struct {
		data string
		ts   int64
		eof  bool
	}
	pushes := func() []pushed {
		var out []pushed
		for _, r := range cluster.reqs {
			if len(r.Chunks) == 0 {
				out = append(out, pushed{eof: r.Eof})
			}
			for _, ch := range r.Chunks {
				out = append(out, pushed{data: string(ch.Data), ts: ch.ReceivedAt, eof: r.Eof})
			}
		}
		return out
	}

	const ts1, ts2 = int64(1_700_000_010_000), int64(1_700_000_020_000)
	if err := os.WriteFile(logPath, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Written, not yet stamped: nothing goes.
	if _, err := p.once(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := pushes(); len(got) != 0 {
		t.Fatalf("pushed before its timestamp: %+v", got)
	}
	if err := cmdlog.RecordTimestamp(logPath, 6, ts1); err != nil {
		t.Fatal(err)
	}
	// "second\n" written, its timestamp not yet: only the stamped line goes.
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("second\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.once(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := cmdlog.RecordTimestamp(logPath, 13, ts2); err != nil {
		t.Fatal(err)
	}
	if _, err := p.once(t.Context()); err != nil {
		t.Fatal(err)
	}
	// It ends with a line never stamped (none of the recorders saw it):
	// that goes too, unstamped, with the end.
	if _, err := f.WriteString("tail\n"); err != nil {
		t.Fatal(err)
	}
	eng.mu.Lock()
	eng.commands[id].cmd().State = pb.CommandState_COMMAND_STATE_SUCCEEDED
	eng.mu.Unlock()
	if _, err := p.once(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := []pushed{{data: "first\n", ts: ts1}, {data: "second\n", ts: ts2}, {data: "tail\n", ts: 0, eof: true}}
	if got := pushes(); !slices.Equal(got, want) {
		t.Fatalf("pushed %+v, want %+v", got, want)
	}
}

func TestOutputPusherSkipsGracefullyWhenControllerUnimplemented(t *testing.T) {
	eng, _ := outputEngine(t, cmdlog.Limit{})
	id := "c-old-ctrl"
	eng.mu.Lock()
	eng.commands[id] = &command{
		rec: &pb.CommandRecord{Command: &pb.Command{Id: id, State: pb.CommandState_COMMAND_STATE_SUCCEEDED}},
	}
	eng.mu.Unlock()

	p := newOutputPusher(eng, unimplementedOutputCluster{})
	if more, err := p.once(t.Context()); err != nil || more || !p.unsupported {
		t.Fatalf("unimplemented push: more=%v err=%v unsupported=%v", more, err, p.unsupported)
	}
}

type unimplementedOutputCluster struct {
	hotbedv1connect.ClusterServiceClient
}

func (unimplementedOutputCluster) PushCommandOutput(context.Context, *pb.PushCommandOutputRequest) (*pb.PushCommandOutputResponse, error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("unimplemented").Error())
}
