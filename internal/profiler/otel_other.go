//go:build !(linux && (amd64 || arm64))

package profiler

import (
	"context"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// OTelProfiler is a stub on non-Linux hosts; continuous eBPF profiling runs on
// Linux amd64/arm64 workers.
type OTelProfiler struct {
	store    *Store
	sampleHz int32
}

func NewOTel(sampleHz int) *OTelProfiler {
	if sampleHz <= 0 {
		sampleHz = DefaultSampleHz
	}
	return &OTelProfiler{store: NewStore(int32(sampleHz)), sampleHz: int32(sampleHz)}
}

func (p *OTelProfiler) Start(context.Context) error { return nil }
func (p *OTelProfiler) Stop()                       {}

func (p *OTelProfiler) Status() *pb.ProfilerStatus {
	return &pb.ProfilerStatus{
		Active:   false,
		Engine:   EngineName,
		SampleHz: p.sampleHz,
		Error:    "continuous eBPF profiling requires a Linux amd64/arm64 worker",
	}
}

func (p *OTelProfiler) Query(cmdID string, req *pb.GetCommandProfileRequest, live bool) *pb.CommandProfile {
	return p.store.Query(cmdID, req, live, p.Status())
}

func (p *OTelProfiler) Snapshot(cmdID string) *pb.CommandProfile {
	return p.store.Snapshot(cmdID, p.Status())
}

func (p *OTelProfiler) Forget(cmdID string) {
	p.store.Forget(cmdID)
}
