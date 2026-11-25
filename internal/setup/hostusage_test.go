package setup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
)

func TestParsePSProcCPU(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantDur  time.Duration
		wantPct  float64
		wantHas  bool
		wantErr  bool
	}{
		{
			name:    "minutes and centiseconds with pct",
			in:      "  12:34.56  150.2\n",
			wantDur: 12*time.Minute + 34*time.Second + 560*time.Millisecond,
			wantPct: 150.2,
			wantHas: true,
		},
		{
			name:    "hours minutes seconds",
			in:      "1:02:03.50 42.5",
			wantDur: time.Hour + 2*time.Minute + 3*time.Second + 500*time.Millisecond,
			wantPct: 42.5,
			wantHas: true,
		},
		{
			name:    "days hours minutes seconds",
			in:      "2-03:04:05 0.0",
			wantDur: 2*24*time.Hour + 3*time.Hour + 4*time.Minute + 5*time.Second,
			wantPct: 0,
			wantHas: true,
		},
		{
			name:    "cputime only",
			in:      "0:05.25",
			wantDur: 5*time.Second + 250*time.Millisecond,
			wantHas: false,
		},
		{
			name:    "empty",
			in:      "   ",
			wantErr: true,
		},
		{
			name:    "invalid",
			in:      "not-a-time",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dur, pct, hasPct, err := parsePSProcCPU(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got dur=%v pct=%v", dur, pct)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if dur != tc.wantDur {
				t.Errorf("dur = %v, want %v", dur, tc.wantDur)
			}
			if hasPct != tc.wantHas || math.Abs(pct-tc.wantPct) > 1e-6 {
				t.Errorf("pct = (%v, %v), want (%v, %v)", pct, hasPct, tc.wantPct, tc.wantHas)
			}
		})
	}
}

func TestParseIOStatCPUBusy(t *testing.T) {
	sample := `      cpu    load average
 us sy id   1m   5m   15m
 25 26 49  17.47 15.84 16.94
 17 43 40  16.15 15.60 16.84
`
	got, err := parseIOStatCPUBusy(sample)
	if err != nil {
		t.Fatalf("parseIOStatCPUBusy: %v", err)
	}
	if math.Abs(got-0.60) > 1e-6 {
		t.Errorf("busy = %v, want 0.60 (from second sample row)", got)
	}

	withDisks := `              disk0       cpu    load average
    KB/t  tps  MB/s  us sy id   1m   5m   15m
   24.05 1384 32.49  25 26 49  16.11 14.86 16.83
    9.96  216  2.11  80 15  5  16.11 14.86 16.83
`
	got, err = parseIOStatCPUBusy(withDisks)
	if err != nil {
		t.Fatalf("parseIOStatCPUBusy with disks: %v", err)
	}
	if math.Abs(got-0.95) > 1e-6 {
		t.Errorf("busy with disks = %v, want 0.95", got)
	}

	if _, err := parseIOStatCPUBusy("no header here"); err == nil {
		t.Error("expected error on missing header")
	}
}

func TestParseSysctlLoadAvg(t *testing.T) {
	v, err := parseSysctlLoadAvg("{ 16.11 14.86 16.83 }\n")
	if err != nil {
		t.Fatalf("parseSysctlLoadAvg: %v", err)
	}
	if math.Abs(v-16.11) > 1e-6 {
		t.Errorf("load1m = %v, want 16.11", v)
	}
	if _, err := parseSysctlLoadAvg("{}"); err == nil {
		t.Error("expected error on empty loadavg")
	}
}

func TestParseMemoryPressureLevel(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"1\n", 0},
		{"2\n", 50},
		{"4\n", 100},
	}
	for _, tc := range cases {
		got, err := parseMemoryPressureLevel(tc.in)
		if err != nil {
			t.Fatalf("parseMemoryPressureLevel(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("level %q -> %v, want %v", tc.in, got, tc.want)
		}
	}
	if _, err := parseMemoryPressureLevel("bad"); err == nil {
		t.Error("expected error on invalid pressure level")
	}
}

func TestHostUsageSamplerDeltaAndProbeErrors(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	cpuTime := 10 * time.Second
	pctCPU := 160.0 // 1.6 cores on a 4-vCPU VM = 0.40
	var (
		procErr error
		cpuErr  error
		loadErr error
		memErr  error
	)

	s := newHostUsageSampler(hostUsageProbes{
		procCPU: func(_ context.Context, _ int) (time.Duration, float64, bool, error) {
			if procErr != nil {
				return 0, 0, false, procErr
			}
			return cpuTime, pctCPU, true, nil
		},
		hostCPUBusy: func(_ context.Context) (float64, error) {
			if cpuErr != nil {
				return 0, cpuErr
			}
			return 0.88, nil
		},
		hostLoad1m: func(_ context.Context) (float64, error) {
			if loadErr != nil {
				return 0, loadErr
			}
			return 14.5, nil
		},
		hostMemPressure: func(_ context.Context) (float64, error) {
			if memErr != nil {
				return 0, memErr
			}
			return 50, nil
		},
		hostCPUs: func() int { return 12 },
		now:      func() time.Time { return now },
	})

	// First sample falls back to %CPU / vCPUs.
	u1, err := s.Sample(t.Context(), 4242, 4)
	if err != nil {
		t.Fatalf("first Sample: %v", err)
	}
	if math.Abs(u1.VmCpuBusy-0.40) > 1e-6 {
		t.Errorf("first VmCpuBusy = %v, want 0.40", u1.VmCpuBusy)
	}
	if math.Abs(u1.HostCpuBusy-0.88) > 1e-6 || math.Abs(u1.HostLoad_1M-14.5) > 1e-6 || u1.HostCpus != 12 || u1.HostMemoryPressure != 50 {
		t.Errorf("first sample = %+v", u1)
	}

	// Any failed critical probe must return an error rather than sending zero
	// (which would clear an active starvation hold).
	now = now.Add(2 * time.Second)
	cpuTime += 2 * time.Second
	memErr = errors.New("sysctl kern.memorystatus_vm_pressure_level unavailable")
	if u, err := s.Sample(t.Context(), 4242, 4); err == nil {
		t.Fatalf("Sample succeeded on memory pressure probe failure: %+v", u)
	}
	memErr = nil

	cpuErr = errors.New("iostat failed")
	if u, err := s.Sample(t.Context(), 4242, 4); err == nil {
		t.Fatalf("Sample succeeded on iostat probe failure: %+v", u)
	}
	cpuErr = nil

	loadErr = errors.New("sysctl vm.loadavg failed")
	if u, err := s.Sample(t.Context(), 4242, 4); err == nil {
		t.Fatalf("Sample succeeded on loadavg probe failure: %+v", u)
	}
	loadErr = nil

	procErr = errors.New("ps failed")
	if u, err := s.Sample(t.Context(), 4242, 4); err == nil {
		t.Fatalf("Sample succeeded on ps probe failure: %+v", u)
	}
	procErr = nil

	// Next successful sample 5s after the first successful sample computes the
	// delta cleanly: the VM used 5s of CPU across 4 vCPUs -> 1.0 core / 4 = 0.25.
	now = now.Add(3 * time.Second)
	cpuTime += 3 * time.Second
	pctCPU = 999 // ignored when delta is available

	u2, err := s.Sample(t.Context(), 4242, 4)
	if err != nil {
		t.Fatalf("second Sample: %v", err)
	}
	if math.Abs(u2.VmCpuBusy-0.25) > 1e-6 {
		t.Errorf("second VmCpuBusy = %v, want 0.25 (delta over 5s / 4 vCPUs)", u2.VmCpuBusy)
	}
	if u2.HostMemoryPressure != 50 {
		t.Errorf("HostMemoryPressure = %v, want 50", u2.HostMemoryPressure)
	}

	// Third sample with extreme delta clamps to 1.0.
	now = now.Add(5 * time.Second)
	cpuTime += 30 * time.Second // 6 cores on 4 vCPUs -> clamped to 1.0
	u3, err := s.Sample(t.Context(), 4242, 4)
	if err != nil {
		t.Fatalf("third Sample: %v", err)
	}
	if u3.VmCpuBusy != 1.0 {
		t.Errorf("clamped VmCpuBusy = %v, want 1.0", u3.VmCpuBusy)
	}
}

type fakeStandbyCluster struct {
	hotbedv1connect.ClusterServiceClient
	mu             sync.Mutex
	reports        []*pb.ReportHostUsageRequest
	failN          atomic.Int32
	hangFirst      atomic.Bool
	firstTimedOut  chan struct{}
	gotCh          chan *pb.ReportHostUsageRequest
}

func (f *fakeStandbyCluster) Standby(ctx context.Context, _ *pb.StandbyRequest) (hotbedv1connect.ClusterServiceStandbyClientStream, error) {
	<-ctx.Done()
	return hotbedv1connect.ClusterServiceStandbyClientStream{}, ctx.Err()
}

func (f *fakeStandbyCluster) ReportHostUsage(ctx context.Context, req *pb.ReportHostUsageRequest) (*pb.ReportHostUsageResponse, error) {
	if f.hangFirst.CompareAndSwap(true, false) {
		<-ctx.Done()
		if f.firstTimedOut != nil {
			close(f.firstTimedOut)
		}
		return nil, ctx.Err()
	}
	if f.failN.Add(-1) >= 0 {
		return nil, errors.New("transient controller restart")
	}
	f.mu.Lock()
	f.reports = append(f.reports, req)
	f.mu.Unlock()
	select {
	case f.gotCh <- req:
	default:
	}
	return &pb.ReportHostUsageResponse{}, nil
}

type fakeMetricsVM struct {
	running atomic.Bool
	sample  *pb.HostUsage
	calls   atomic.Int32
}

func (v *fakeMetricsVM) Running(_ context.Context) bool { return v.running.Load() }
func (v *fakeMetricsVM) Start(_ context.Context) error  { v.running.Store(true); return nil }
func (v *fakeMetricsVM) Stop(_ context.Context) error   { v.running.Store(false); return nil }
func (v *fakeMetricsVM) SampleHostUsage(_ context.Context) (*pb.HostUsage, error) {
	v.calls.Add(1)
	return v.sample, nil
}

func TestStandbyReportsHostUsageWhenRunning(t *testing.T) {
	oldInterval := hostUsageInterval
	hostUsageInterval = 15 * time.Millisecond
	t.Cleanup(func() { hostUsageInterval = oldInterval })

	cluster := &fakeStandbyCluster{gotCh: make(chan *pb.ReportHostUsageRequest, 8)}
	cluster.failN.Store(1) // first report fails transiently, next must succeed

	vm := &fakeMetricsVM{
		sample: &pb.HostUsage{
			VmCpuBusy:          0.20,
			HostCpuBusy:        0.92,
			HostLoad_1M:        16.0,
			HostCpus:           12,
			HostMemoryPressure: 50,
			SampledAt:          1_800_000_000_000,
		},
	}
	vm.running.Store(false)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() {
		defer close(done)
		Standby(ctx, cluster, "mac-worker", 30*time.Minute, vm, logger)
	}()

	// While VM is stopped, Standby must not sample or report host usage.
	time.Sleep(45 * time.Millisecond)
	if vm.calls.Load() != 0 {
		t.Fatalf("SampleHostUsage called %d times while VM stopped, want 0", vm.calls.Load())
	}

	// Once VM is running, Standby samples and calls ReportHostUsage (recovering from transient error).
	vm.running.Store(true)
	select {
	case req := <-cluster.gotCh:
		if req.Node != "mac-worker" {
			t.Errorf("Node = %q, want mac-worker", req.Node)
		}
		if req.Usage == nil || req.Usage.VmCpuBusy != 0.20 || req.Usage.HostCpuBusy != 0.92 || req.Usage.HostMemoryPressure != 50 {
			t.Errorf("Usage = %+v, want reported sample", req.Usage)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Standby to call ReportHostUsage")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Standby did not exit promptly on context cancel")
	}
}

func TestStandbyReportHostUsageTimeoutRecovers(t *testing.T) {
	oldInterval := hostUsageInterval
	oldTimeout := hostUsageRPCTimeout
	hostUsageInterval = 15 * time.Millisecond
	hostUsageRPCTimeout = 40 * time.Millisecond
	t.Cleanup(func() {
		hostUsageInterval = oldInterval
		hostUsageRPCTimeout = oldTimeout
	})

	cluster := &fakeStandbyCluster{
		firstTimedOut: make(chan struct{}),
		gotCh:         make(chan *pb.ReportHostUsageRequest, 8),
	}
	cluster.hangFirst.Store(true)

	vm := &fakeMetricsVM{
		sample: &pb.HostUsage{
			VmCpuBusy:          0.25,
			HostCpuBusy:        0.88,
			HostLoad_1M:        14.0,
			HostCpus:           12,
			HostMemoryPressure: 50,
			SampledAt:          1_800_000_000_000,
		},
	}
	vm.running.Store(true)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() {
		defer close(done)
		Standby(ctx, cluster, "mac-worker", 30*time.Minute, vm, logger)
	}()

	// First hung ReportHostUsage must be canceled by its per-tick deadline,
	// not block the reporting goroutine forever.
	select {
	case <-cluster.firstTimedOut:
	case <-time.After(2 * time.Second):
		t.Fatal("first hung ReportHostUsage did not time out via per-RPC context")
	}

	// Parent Standby context must remain alive and the next tick must succeed.
	if ctx.Err() != nil {
		t.Fatalf("parent Standby context unexpectedly canceled: %v", ctx.Err())
	}
	select {
	case req := <-cluster.gotCh:
		if req.Node != "mac-worker" || req.Usage.GetHostCpuBusy() != 0.88 {
			t.Errorf("second tick req = %+v, want mac-worker with HostCpuBusy=0.88", req)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for subsequent ReportHostUsage tick to succeed after timeout")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Standby did not exit promptly on context cancel")
	}
}
