//go:build linux && (amd64 || arm64)

package profiler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer/xconsumer"
	"go.opentelemetry.io/collector/pdata/pprofile"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/receiver/xreceiver"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	ebpfcollector "go.opentelemetry.io/ebpf-profiler/collector"
	ebpfconfig "go.opentelemetry.io/ebpf-profiler/collector/config"
	"go.opentelemetry.io/ebpf-profiler/interpreter/interpreterconfig"
	"go.opentelemetry.io/ebpf-profiler/libpf"
	"go.opentelemetry.io/ebpf-profiler/process"
	pm "go.opentelemetry.io/ebpf-profiler/processmanager"
	"go.opentelemetry.io/ebpf-profiler/reporter"
	"go.opentelemetry.io/ebpf-profiler/reporter/samples"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

var hotbedCmdMetaKey = libpf.Intern("hotbed.command")

// OTelProfiler embeds the official OpenTelemetry eBPF Profiler directly inside
// a hotbed Linux worker, always on at 20 Hz with <1% CPU overhead.
type OTelProfiler struct {
	store      *Store
	symbolizer *Symbolizer
	sampleHz   int

	mu       sync.Mutex
	recv     xreceiver.Profiles
	active   bool
	startErr string

	// Bounded cache for fallback PID -> commandID lookup when a process predated enrichment.
	pidMu    sync.RWMutex
	pidCache map[libpf.PID]pidCacheEntry
}

type pidCacheEntry struct {
	cmdID string
	at    time.Time
}

// NewOTel creates an always-on OpenTelemetry eBPF profiler backed by an
// in-memory Store and local ELF/Go symbolizer.
func NewOTel(sampleHz int) *OTelProfiler {
	if sampleHz <= 0 {
		sampleHz = DefaultSampleHz
	}
	return &OTelProfiler{
		store:      NewStore(int32(sampleHz)),
		symbolizer: NewSymbolizer(512),
		sampleHz:   sampleHz,
		pidCache:   map[libpf.PID]pidCacheEntry{},
	}
}

// Start loads and attaches the OpenTelemetry eBPF Profiler. If the kernel or
// privileges prevent eBPF loading, it records the reason in Status() and
// returns the error so the worker can log it without failing startup.
func (p *OTelProfiler) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active {
		return nil
	}

	cfg := &ebpfconfig.Config{
		ReporterInterval:       5 * time.Second,
		ReporterJitter:         0.2,
		MonitorInterval:        5 * time.Second,
		SamplesPerSecond:       p.sampleHz,
		FrameCacheSize:         uint(pm.DefaultFrameCacheSize),
		ProbabilisticInterval:  1 * time.Minute,
		ProbabilisticThreshold: 100, // always on (100% of intervals)
		Interpreters:           interpreterconfig.AllInterpreters(),
		ClockSyncInterval:      3 * time.Minute,
		MaxGRPCRetries:         1,
		MaxRPCMsgSize:          32 << 20,
		BPFFSRoot:              "/sys/fs/bpf/",
		ErrorMode:              ebpfconfig.PropagateError,
	}

	enricher := process.MetaEnricherFunc(func(procBase string, meta *process.Meta) {
		cmdID := ""
		if b, err := os.ReadFile(procBase + "cgroup"); err == nil {
			cmdID = ExtractCommandIDFromCgroup(string(b))
		}
		if cmdID == "" {
			if b, err := os.ReadFile(procBase + "environ"); err == nil {
				cmdID = ExtractCommandIDFromEnviron(b)
			}
		}
		if cmdID != "" {
			if meta.ExtraMeta == nil {
				meta.ExtraMeta = map[libpf.String]string{}
			}
			meta.ExtraMeta[hotbedCmdMetaKey] = cmdID
		}
	})

	exeRep := &localExecReporter{sym: p.symbolizer}
	repFactory := func(_ *reporter.Config, _ xconsumer.Profiles) (reporter.Reporter, error) {
		return &hotbedReporter{owner: p}, nil
	}

	createFn := ebpfcollector.BuildProfilesReceiver(
		ebpfcollector.WithProcessMetaEnricher(enricher),
		ebpfcollector.WithExecutableReporter(exeRep),
		ebpfcollector.WithReporterFactory(repFactory),
	)

	typ := component.MustNewType("hotbed_ebpf_profiler")
	rs := receiver.Settings{
		ID: component.NewID(typ),
		TelemetrySettings: component.TelemetrySettings{
			Logger:         zap.NewNop(),
			TracerProvider: tracenoop.NewTracerProvider(),
			MeterProvider:  metricnoop.NewMeterProvider(),
		},
	}

	nopConsumer, err := xconsumer.NewProfiles(func(context.Context, pprofile.Profiles) error { return nil })
	if err != nil {
		p.startErr = err.Error()
		return err
	}

	recv, err := createFn(ctx, rs, cfg, nopConsumer)
	if err != nil {
		p.startErr = err.Error()
		return fmt.Errorf("create opentelemetry ebpf profiler: %w", err)
	}
	if err := recv.Start(ctx, nopHost{}); err != nil {
		p.startErr = err.Error()
		return fmt.Errorf("start opentelemetry ebpf profiler: %w", err)
	}
	p.recv = recv
	p.active = true
	p.startErr = ""
	slog.Info("started always-on OpenTelemetry eBPF profiler", "hz", p.sampleHz)
	return nil
}

// Stop shuts down the eBPF profiler and detaches all probes cleanly.
func (p *OTelProfiler) Stop() {
	p.mu.Lock()
	recv := p.recv
	p.recv = nil
	p.active = false
	p.mu.Unlock()
	if recv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = recv.Shutdown(ctx)
	}
}

func (p *OTelProfiler) Status() *pb.ProfilerStatus {
	p.mu.Lock()
	active, errStr := p.active, p.startErr
	p.mu.Unlock()
	return &pb.ProfilerStatus{
		Active:       active,
		Engine:       EngineName,
		SampleHz:     int32(p.sampleHz),
		TotalSamples: p.store.TotalSamples(),
		Error:        errStr,
	}
}

func (p *OTelProfiler) Query(cmdID string, req *pb.GetProfileRequest, live bool) *pb.CommandProfile {
	return p.store.Query(cmdID, req, live, p.Status())
}

func (p *OTelProfiler) QueryNode(node string, req *pb.GetProfileRequest) *pb.CommandProfile {
	return p.store.QueryNode(node, req, p.Status())
}

func (p *OTelProfiler) Snapshot(cmdID string) *pb.CommandProfile {
	return p.store.Snapshot(cmdID, p.Status())
}

func (p *OTelProfiler) Forget(cmdID string) {
	p.store.Forget(cmdID)
}

func (p *OTelProfiler) lookupPIDCommand(pid libpf.PID) string {
	now := time.Now()
	p.pidMu.RLock()
	ent, ok := p.pidCache[pid]
	p.pidMu.RUnlock()
	if ok && now.Sub(ent.at) < 10*time.Second {
		return ent.cmdID
	}
	cmdID := ""
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid)); err == nil {
		cmdID = ExtractCommandIDFromCgroup(string(b))
	}
	p.pidMu.Lock()
	if len(p.pidCache) > 8192 {
		clear(p.pidCache)
	}
	p.pidCache[pid] = pidCacheEntry{cmdID: cmdID, at: now}
	p.pidMu.Unlock()
	return cmdID
}

// hotbedReporter receives every trace event directly from ebpf-profiler.
type hotbedReporter struct {
	owner *OTelProfiler
}

func (r *hotbedReporter) Start(context.Context) error { return nil }
func (r *hotbedReporter) Stop()                       {}

func (r *hotbedReporter) ReportTraceEvent(trace *libpf.Trace, meta *samples.TraceEventMeta) error {
	if trace == nil || meta == nil || len(trace.Frames) == 0 {
		return nil
	}
	cmdID := ""
	if meta.ExtraMeta != nil {
		cmdID = meta.ExtraMeta[hotbedCmdMetaKey]
	}
	if cmdID == "" {
		cmdID = r.owner.lookupPIDCommand(meta.PID)
	}

	// ebpf-profiler emits frames leaf-first (kernel leaf -> user leaf -> root).
	// Reverse to root -> leaf for standard flamegraph folded format.
	n := len(trace.Frames)
	frames := make([]ResolvedFrame, 0, n)
	for i := n - 1; i >= 0; i-- {
		fr := trace.Frames[i].Value()
		rf := r.owner.resolveFrame(fr)
		if rf.Name != "" {
			frames = append(frames, rf)
		}
	}
	if len(frames) == 0 {
		r.owner.store.totalSamples.Add(1)
		return nil
	}
	ts := time.Unix(0, int64(meta.Timestamp))
	if meta.Timestamp == 0 {
		ts = time.Now()
	}
	r.owner.store.Record(Sample{
		CommandID: cmdID,
		Timestamp: ts,
		Comm:      meta.Comm.String(),
		Frames:    frames,
	})
	return nil
}

func (p *OTelProfiler) resolveFrame(fr libpf.Frame) ResolvedFrame {
	ft := fr.Type.String()
	mod := ""
	fileID := ""
	if fr.Mapping.Valid() {
		mf := fr.Mapping.Value().File.Value()
		mod = mf.FileName.String()
		fileID = mf.FileID.StringNoQuotes()
	}
	rf := ResolvedFrame{
		Name:       fr.FunctionName.String(),
		Module:     filepath.Base(mod),
		FrameType:  ft,
		SourceFile: fr.SourceFile.String(),
		SourceLine: int32(fr.SourceLine),
	}
	if rf.Module == "." || rf.Module == "/" {
		rf.Module = ""
	}
	if rf.Name == "" && fileID != "" {
		if name, srcFile, srcLine, isGo, ok := p.symbolizer.Lookup(fileID, uint64(fr.AddressOrLineno)); ok {
			rf.Name = name
			if rf.SourceFile == "" {
				rf.SourceFile = srcFile
			}
			if rf.SourceLine == 0 {
				rf.SourceLine = srcLine
			}
			if isGo && (rf.FrameType == "native" || rf.FrameType == "") {
				rf.FrameType = "go"
			}
		} else if rf.Module != "" {
			rf.Name = fmt.Sprintf("%s+0x%x", rf.Module, uint64(fr.AddressOrLineno))
		} else {
			rf.Name = fmt.Sprintf("0x%x", uint64(fr.AddressOrLineno))
		}
	}
	return rf
}

type localExecReporter struct {
	sym *Symbolizer
}

func (e *localExecReporter) ReportExecutable(args *reporter.ExecutableMetadata) {
	if args == nil || args.Process == nil || args.Mapping == nil {
		return
	}
	mf := args.MappingFile.Value()
	fileID := mf.FileID.StringNoQuotes()
	if fileID == "" || e.sym.Has(fileID) {
		return
	}
	f, err := args.Process.OpenMappingFile(args.Mapping)
	if err != nil {
		return
	}
	defer f.Close()
	if ra, ok := f.(io.ReaderAt); ok {
		e.sym.IndexELF(fileID, ra)
	}
}

// nopHost satisfies component.Host for standalone receiver execution.
type nopHost struct{}

func (nopHost) GetExtensions() map[component.ID]component.Component { return nil }
