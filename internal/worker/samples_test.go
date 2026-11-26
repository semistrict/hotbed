package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/inspect"
)

// sampleOnce takes one sample of command id as host, returning its final chunk.
func sampleOnce(t *testing.T, e *Engine, req *pb.SampleCommandRequest, host string) *pb.SampleCommandChunk {
	t.Helper()
	var final *pb.SampleCommandChunk
	if err := e.SampleCommand(t.Context(), req, host, func(ch *pb.SampleCommandChunk) error {
		if ch.Done {
			final = ch
		}
		return nil
	}); err != nil {
		t.Fatalf("SampleCommand: %v", err)
	}
	if final == nil {
		t.Fatal("SampleCommand emitted no final chunk")
	}
	return final
}

func TestSampleCommandKeepsNumberedSamples(t *testing.T) {
	now := time.Unix(1795300000, 0)
	units, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}
	insp := &fakeInspector{sample: &pb.ProcessSample{CommandId: "c-s", DurationMs: 3000}}
	e := newInterventionEngine(t, t.TempDir(), store, units, insp, &now)
	startTestCommand(t, e, store, units, "c-s")

	first := sampleOnce(t, e, &pb.SampleCommandRequest{Id: "c-s", SessionId: "sess-1"}, "laptop")
	second := sampleOnce(t, e, &pb.SampleCommandRequest{Id: "c-s", DurationSeconds: 5, Pid: 42}, "desk")
	if first.Kept.GetSeq() != 1 || first.NotKept != "" || second.Kept.GetSeq() != 2 || second.NotKept != "" {
		t.Fatalf("final chunks = (%v, %q), (%v, %q), want samples 1 and 2 kept", first.Kept, first.NotKept, second.Kept, second.NotKept)
	}
	cmd, err := e.current("c-s")
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Samples != 2 {
		t.Fatalf("Command.Samples = %d, want 2", cmd.Samples)
	}
	pending, err := e.PendingSamples()
	if err != nil {
		t.Fatal(err)
	}
	want := []*pb.CommandSample{
		{CommandId: "c-s", Seq: 1, By: &pb.Stamp{At: now.UnixMilli(), Session: "sess-1", Host: "laptop"}, DurationSeconds: 3, Sample: insp.sample},
		{CommandId: "c-s", Seq: 2, By: &pb.Stamp{At: now.UnixMilli(), Host: "desk"}, DurationSeconds: 5, Pid: 42, Sample: insp.sample},
	}
	if len(pending) != len(want) {
		t.Fatalf("PendingSamples = %d, want %d", len(pending), len(want))
	}
	for i, final := range []*pb.SampleCommandChunk{first, second} {
		if !proto.Equal(pending[i], want[i]) {
			t.Errorf("PendingSamples[%d] = %v, want %v", i, pending[i], want[i])
		}
		meta := proto.CloneOf(want[i])
		meta.Sample = nil
		if !proto.Equal(final.Kept, meta) {
			t.Errorf("final chunk %d kept = %v, want %v", i+1, final.Kept, meta)
		}
	}
}

func TestSampleCommandTrimsOversizedSample(t *testing.T) {
	now := time.Unix(1795300000, 0)
	units, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}
	frames := make([]*pb.ProfileFrame, 8)
	for i := range frames {
		frames[i] = &pb.ProfileFrame{Name: strings.Repeat("f", 200<<10)}
	}
	insp := &fakeInspector{sample: &pb.ProcessSample{CommandId: "c-big", Hostname: "asahimbp", Frames: frames}}
	e := newInterventionEngine(t, t.TempDir(), store, units, insp, &now)
	startTestCommand(t, e, store, units, "c-big")

	final := sampleOnce(t, e, &pb.SampleCommandRequest{Id: "c-big"}, "laptop")
	if final.Kept.GetSeq() != 1 || final.Kept.Truncated != "dropped stacks to fit 1 MiB" || final.NotKept != "" {
		t.Fatalf("final chunk kept %v not kept %q, want sample 1 kept trimmed", final.Kept, final.NotKept)
	}
	if len(final.Sample.Frames) != 0 || final.Sample.Hostname != "asahimbp" {
		t.Fatalf("final chunk carries frames=%d host=%q, want the trimmed sample as kept", len(final.Sample.Frames), final.Sample.Hostname)
	}
	pending, err := e.PendingSamples()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("PendingSamples = %d, want 1", len(pending))
	}
	if got, want := pending[0].Truncated, "dropped stacks to fit 1 MiB"; got != want {
		t.Errorf("Truncated = %q, want %q", got, want)
	}
	if sz := proto.Size(pending[0]); sz > maxSampleBytes {
		t.Errorf("kept sample is %d bytes, want <= %d", sz, maxSampleBytes)
	}
	// The inspector's own sample is left as it was.
	if len(insp.sample.Frames) != 8 {
		t.Errorf("trimming changed the inspector's sample: %d frames left", len(insp.sample.Frames))
	}
}

func TestSampleCommandDoesNotKeepUnshrinkableSample(t *testing.T) {
	now := time.Unix(1795300000, 0)
	units, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}
	insp := &fakeInspector{sample: &pb.ProcessSample{CommandId: "c-huge", Cgroup: strings.Repeat("x", maxSampleBytes+1)}}
	e := newInterventionEngine(t, t.TempDir(), store, units, insp, &now)
	startTestCommand(t, e, store, units, "c-huge")

	final := sampleOnce(t, e, &pb.SampleCommandRequest{Id: "c-huge"}, "laptop")
	if final.Kept != nil || !strings.Contains(final.NotKept, "past the 1048576 it may be") {
		t.Fatalf("final chunk kept %v not kept %q, want an unkept sample saying why", final.Kept, final.NotKept)
	}
	if final.Sample == nil {
		t.Fatal("an unkept sample must still reach whoever asked")
	}
	if pending, err := e.PendingSamples(); err != nil || len(pending) != 0 {
		t.Fatalf("PendingSamples = (%d, %v), want none", len(pending), err)
	}
	if cmd, _ := e.current("c-huge"); cmd.Samples != 0 {
		t.Fatalf("Command.Samples = %d, want 0", cmd.Samples)
	}
}

// endingInspector ends the command it samples before handing its sample back.
type endingInspector struct {
	fakeInspector
	end func()
}

func (f *endingInspector) Sample(ctx context.Context, target inspect.Target, req *pb.SampleCommandRequest, emit inspect.SampleEmit) error {
	f.end()
	return f.fakeInspector.Sample(ctx, target, req, emit)
}

func TestSampleCommandDoesNotKeepSampleOfEndedCommand(t *testing.T) {
	now := time.Unix(1795300000, 0)
	units, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}
	insp := &endingInspector{fakeInspector: fakeInspector{sample: &pb.ProcessSample{CommandId: "c-end"}}}
	e := newInterventionEngine(t, t.TempDir(), store, units, insp, &now)
	startTestCommand(t, e, store, units, "c-end")
	insp.end = func() {
		if _, err := e.Stop(t.Context(), "c-end", "done"); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}

	final := sampleOnce(t, e, &pb.SampleCommandRequest{Id: "c-end"}, "laptop")
	if final.Kept != nil || final.NotKept != "its command ended as it was sampled" {
		t.Fatalf("final chunk kept %v not kept %q, want unkept: its command ended", final.Kept, final.NotKept)
	}
	if pending, err := e.PendingSamples(); err != nil || len(pending) != 0 {
		t.Fatalf("PendingSamples = (%d, %v), want none", len(pending), err)
	}
}

type sampleDeliveryCluster struct {
	hotbedv1connect.ClusterServiceClient
	sizeLimitedCluster
	failNext int
	saved    []*pb.CommandSample
}

func (c *sampleDeliveryCluster) PushEvents(ctx context.Context, req *pb.PushEventsRequest) (*pb.PushEventsResponse, error) {
	return c.sizeLimitedCluster.PushEvents(ctx, req)
}

func (c *sampleDeliveryCluster) SaveCommandSample(_ context.Context, req *pb.SaveCommandSampleRequest) (*pb.SaveCommandSampleResponse, error) {
	if c.failNext > 0 {
		c.failNext--
		return nil, connect.NewError(connect.CodeUnavailable, "controller busy")
	}
	c.saved = append(c.saved, proto.Clone(req.Sample).(*pb.CommandSample))
	return &pb.SaveCommandSampleResponse{}, nil
}

func TestKeptSamplesDeliveredWithAcknowledgement(t *testing.T) {
	now := time.Unix(1795300000, 0)
	units, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}
	insp := &fakeInspector{sample: &pb.ProcessSample{CommandId: "c-deliv"}}
	e := newInterventionEngine(t, t.TempDir(), store, units, insp, &now)
	startTestCommand(t, e, store, units, "c-deliv")
	sampleOnce(t, e, &pb.SampleCommandRequest{Id: "c-deliv"}, "laptop")

	cluster := &sampleDeliveryCluster{
		sizeLimitedCluster: sizeLimitedCluster{maxBytes: 4 << 20, epoch: e.Epoch()},
		failNext:           1,
	}
	p := &pusher{eng: e, cluster: cluster, clusterID: "cl", known: true}
	var deliverErr error
	for range 10 {
		more, err := p.once(t.Context())
		if err != nil {
			deliverErr = err
			break
		}
		if !more {
			break
		}
	}
	if deliverErr == nil {
		t.Fatal("expected the first SaveCommandSample to fail")
	}
	if pending, err := e.PendingSamples(); err != nil || len(pending) != 1 {
		t.Fatalf("after a failed SaveCommandSample: pending = (%d, %v), want 1", len(pending), err)
	}
	if _, err := p.once(t.Context()); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(cluster.saved) != 1 || cluster.saved[0].CommandId != "c-deliv" || cluster.saved[0].Seq != 1 {
		t.Fatalf("saved = %v, want c-deliv sample 1", cluster.saved)
	}
	if pending, err := e.PendingSamples(); err != nil || len(pending) != 0 {
		t.Fatalf("after ack: pending = (%d, %v), want 0", len(pending), err)
	}
}
