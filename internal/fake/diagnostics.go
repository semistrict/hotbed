package fake

import (
	"context"
	"fmt"
	"time"

	"github.com/semistrict/hotbed/internal/flame"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/inspect"
	"github.com/semistrict/hotbed/internal/profiler"
)

// Inspector is an in-memory Inspector for tests that run without Linux root.
type Inspector struct{}

func (Inspector) Capabilities() []*pb.ToolCapability {
	return []*pb.ToolCapability{
		{Name: "strace", Available: true, Detail: "/usr/bin/strace"},
		{Name: "perf", Available: true, Detail: "/usr/bin/perf"},
		{Name: "eu-stack", Available: true, Detail: "/usr/bin/eu-stack"},
		{Name: "gdb", Available: true, Detail: "/usr/bin/gdb"},
		{Name: "bpftrace", Available: true, Detail: "/usr/bin/bpftrace"},
	}
}

func (i Inspector) Inspect(_ context.Context, target inspect.Target, req *pb.InspectCommandRequest) (*pb.InspectCommandResponse, error) {
	pid := int32(target.MainPID)
	if pid == 0 {
		pid = 4242
	}
	res := &pb.InspectCommandResponse{
		CommandId:      target.CommandID,
		CollectedAt:    time.Now().UnixMilli(),
		Cgroup:         target.Cgroup,
		UnitName:       target.UnitName,
		AvailableTools: i.Capabilities(),
	}
	if inspect.WantSection(req, pb.InspectSection_INSPECT_SECTION_PROCS) {
		res.Procs = []*pb.ProcessInfo{{
			Pid:          pid,
			Ppid:         1,
			State:        "S",
			Comm:         "fake-proc",
			Cmdline:      "fake-proc --test",
			ThreadsCount: 2,
			RssBytes:     32 << 20,
			VsizeBytes:   128 << 20,
			CpuUserMs:    120,
			CpuSysMs:     30,
			IoReadBytes:  4096,
			IoWriteBytes: 8192,
			Wchan:        "ep_poll",
			Threads: []*pb.ThreadInfo{
				{Tid: pid, Name: "fake-proc", State: "S", Wchan: "ep_poll", CpuUserMs: 100, CpuSysMs: 20},
				{Tid: pid + 1, Name: "worker-1", State: "S", Wchan: "futex_wait_queue", CpuUserMs: 20, CpuSysMs: 10},
			},
		}}
	}
	if inspect.WantSection(req, pb.InspectSection_INSPECT_SECTION_FILES) {
		res.Files = []*pb.OpenFile{
			{Pid: pid, Comm: "fake-proc", Fd: 3, Kind: "file", Target: "/work/app/main.go", Mode: "r", Pos: 128},
			{Pid: pid, Comm: "fake-proc", Fd: 4, Kind: "socket", Target: "socket:[9999]", Inode: 9999, SocketProto: "tcp", SocketState: "LISTEN", LocalAddr: "127.0.0.1:8080", RemoteAddr: "0.0.0.0:0"},
		}
	}
	if inspect.WantSection(req, pb.InspectSection_INSPECT_SECTION_CGROUP) {
		res.CgroupStats = &pb.CgroupStats{
			CpuUsageUsec:       150_000,
			CpuUserUsec:        120_000,
			CpuSystemUsec:      30_000,
			MemoryCurrentBytes: 64 << 20,
			MemoryPeakBytes:    96 << 20,
			MemoryAnonBytes:    48 << 20,
			MemoryFileBytes:    12 << 20,
			MemoryKernelBytes:  4 << 20,
			PidsCurrent:        1,
			PidsPeak:           2,
		}
	}
	if inspect.WantSection(req, pb.InspectSection_INSPECT_SECTION_KSTACK) {
		res.Kstacks = []*pb.ThreadKernelStack{{
			Pid: pid, Tid: pid, Comm: "fake-proc", ThreadName: "fake-proc", State: "S", Wchan: "ep_poll",
			Frames: []string{"do_epoll_wait+0x90/0xe0", "__x64_sys_epoll_pwait+0x70/0x110", "do_syscall_64+0x5c/0x90"},
		}}
	}
	if inspect.WantSection(req, pb.InspectSection_INSPECT_SECTION_MAPS) {
		res.Maps = []*pb.ProcessMemoryMap{{
			Pid: pid, Comm: "fake-proc", RssBytes: 32 << 20, PssBytes: 28 << 20, PrivateDirtyBytes: 24 << 20,
			TopRegions: []*pb.MemoryRegion{{Perms: "rw-p", Path: "[anon]", SizeBytes: 24 << 20}},
		}}
	}
	if inspect.WantSection(req, pb.InspectSection_INSPECT_SECTION_LIMITS) {
		res.Limits = []*pb.ProcessLimits{{
			Pid: pid, Comm: "fake-proc", CapEff: "0000000000000000", Seccomp: "disabled",
			Items: []*pb.ResourceLimit{{Name: "Max open files", Soft: "1024", Hard: "524288", Units: "files"}},
		}}
	}
	return res, nil
}

func (Inspector) Trace(_ context.Context, target inspect.Target, req *pb.TraceCommandRequest, emit inspect.Emit) error {
	if err := inspect.ValidateFilter(req.Filter); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	pid := int32(target.MainPID)
	if pid == 0 {
		pid = 4242
	}
	if err := emit(&pb.TraceCommandChunk{Status: fmt.Sprintf("fake trace %s on PID %d", req.Kind, pid)}); err != nil {
		return err
	}
	sum := &pb.TraceSummary{
		Kind:         req.Kind,
		StartedAt:    now,
		EndedAt:      now + 10,
		DurationMs:   10,
		PidsAttached: []int32{pid},
		CommandLine:  fmt.Sprintf("fake %s -p %d", req.Kind, pid),
	}
	switch req.Kind {
	case pb.TraceKind_TRACE_KIND_STRACE:
		if err := emit(&pb.TraceCommandChunk{Text: fmt.Sprintf("%d openat(AT_FDCWD, \"/work/app/main.go\", O_RDONLY) = 3 <0.000042>\n", pid)}); err != nil {
			return err
		}
	case pb.TraceKind_TRACE_KIND_STRACE_SUMMARY:
		sum.Syscalls = []*pb.SyscallStat{{Name: "futex", PctTime: 65.0, Seconds: 0.12, UsecsPerCall: 40, Calls: 3000}}
		sum.Raw = "% time     seconds  usecs/call     calls    errors syscall\n------ ----------- ----------- --------- --------- ----------------\n 65.00    0.120000          40      3000           futex\n------ ----------- ----------- --------- --------- ----------------\n"
	case pb.TraceKind_TRACE_KIND_PERF_CPU:
		sum.Frames = []*pb.ProfileFrame{
			{Name: "fake-proc", FrameType: "process"},
			{Name: "main.main", Module: "fake-proc", FrameType: "go"},
			{Name: "main.work", Module: "fake-proc", FrameType: "go"},
		}
		sum.Stacks = []*pb.FoldedStack{{Frames: []uint32{0, 1, 2}, Samples: 42, LeafType: "go", Comm: "fake-proc"}}
		sum.TopFunctions = []*pb.ProfileFunction{{Frame: 2, SelfSamples: 42, TotalSamples: 42, SelfPct: 100, TotalPct: 100}}
		sum.Raw = "100.00%  fake-proc  [.] main.work\n"
	case pb.TraceKind_TRACE_KIND_PERF_STAT:
		sum.Counters = []*pb.PerfCounter{
			{Name: "cycles", Value: "1,200,000,000", Note: "3.20 GHz"},
			{Name: "instructions", Value: "2,400,000,000", Note: "2.00 insn per cycle"},
		}
		sum.Raw = "1,200,000,000 cycles\n2,400,000,000 instructions # 2.00 insn per cycle\n"
	default:
		sum.Raw = fmt.Sprintf("PID %d fake-proc trace output\n", pid)
	}
	return emit(&pb.TraceCommandChunk{Done: true, Summary: sum})
}

func (Inspector) Sample(_ context.Context, target inspect.Target, req *pb.SampleCommandRequest, emit inspect.SampleEmit) error {
	now := time.Now().UnixMilli()
	pid := int32(target.MainPID)
	if pid == 0 {
		pid = 4242
	}
	if req.GetPid() > 0 {
		pid = req.GetPid()
	}
	durSec := inspect.ClampSampleDuration(req.GetDurationSeconds())
	if err := emit(&pb.SampleCommandChunk{Status: fmt.Sprintf("fake sampling PID %d (%ds)...", pid, durSec), ProgressPct: 50}); err != nil {
		return err
	}
	sample := &pb.ProcessSample{
		CommandId:          target.CommandID,
		UnitName:           target.UnitName,
		Cgroup:             target.Cgroup,
		Hostname:           "fake-worker",
		Kernel:             "6.12.0-fake",
		Arch:               "amd64",
		StartedAt:          now - int64(durSec)*1000,
		EndedAt:            now,
		DurationMs:         int64(durSec) * 1000,
		PollCount:          8,
		UserStackSnapshots: 3,
		Collectors: []*pb.SampleCollector{
			{Name: "proc_delta", Ok: true, Detail: "1 PID(s) · T0→T1 deltas"},
			{Name: "kstack_poll", Ok: true, Detail: "8 /proc thread + kstack + syscall polls"},
			{Name: "user_stacks", Ok: true, Detail: "eu-stack (3 snapshot(s))"},
			{Name: "net_sockets", Ok: true, Detail: "2 open FD(s)"},
			{Name: "proc_locks", Ok: true, Detail: "1 relevant lock(s)"},
			{Name: "cgroup_v2", Ok: true, Detail: target.Cgroup},
			{Name: "perf_stat", Ok: true, Detail: "2 hardware/software counters"},
		},
		Procs: []*pb.SampledProcess{{
			Pid:                   pid,
			Ppid:                  1,
			State:                 "S",
			Comm:                  "fake-proc",
			Cmdline:               "fake-proc --test",
			Exe:                   "/usr/bin/fake-proc",
			Cwd:                   "/work/app",
			ThreadsCount:          2,
			OpenFdsCount:          2,
			RssBytes:              32 << 20,
			PssBytes:              28 << 20,
			PrivateDirtyBytes:     24 << 20,
			VsizeBytes:            128 << 20,
			CpuPct:                0,
			VoluntaryCtxtDelta:    0,
			NonvoluntaryCtxtDelta: 0,
			Wchan:                 "futex_wait_queue",
			Threads: []*pb.SampledThread{
				{
					Tid: pid, Name: "fake-proc", State: "S", StateCounts: map[string]int32{"S": 8},
					Wchan: "futex_wait_queue", CpuCore: 1, Stationary: true, PollsSameStack: 8, TotalPolls: 8,
					Syscall:       &pb.ThreadSyscall{Number: 202, Name: "futex", Pc: "0x7f00001234"},
					SyscallCounts: map[string]int32{"futex": 8},
					UserFrames: []*pb.StackFrame{
						{Symbol: "runtime.futex", Module: "fake-proc", Address: "0x46f810"},
						{Symbol: "sync.(*Mutex).Lock", Module: "fake-proc", Address: "0x489210"},
						{Symbol: "main.work", Module: "fake-proc", Address: "0x510020"},
					},
					KernelFrames:     []string{"futex_wait_queue+0x60/0xa0", "do_futex+0x120/0x190", "do_syscall_64+0x5c/0x90"},
					UserStackSamples: 3,
				},
			},
		}},
		Files: []*pb.SampledOpenFile{
			{Pid: pid, Comm: "fake-proc", Fd: 3, Kind: "file", Target: "/work/app/main.go", Mode: "r", Pos: 128, PosDelta: 0, Inode: 1001},
			{Pid: pid, Comm: "fake-proc", Fd: 4, Kind: "socket", Target: "socket:[9999]", Mode: "rw", Inode: 9999, SocketProto: "tcp", SocketState: "LISTEN", LocalAddr: "127.0.0.1:8080", RemoteAddr: "0.0.0.0:0"},
		},
		Locks: []*pb.FileLock{
			{Kind: "FLOCK", Mode: "ADVISORY", Access: "WRITE", Pid: pid, Comm: "fake-proc", Inode: 1001, Range: "0..EOF", Target: "/work/app/main.go"},
		},
		CgroupStats: &pb.SampledCgroup{
			End: &pb.CgroupStats{
				MemoryCurrentBytes: 64 << 20,
				MemoryPeakBytes:    96 << 20,
				PidsCurrent:        1,
				PidsPeak:           2,
			},
		},
		Frames: []*pb.ProfileFrame{
			{Name: "fake-proc", FrameType: "process"},
			{Name: "main.work", FrameType: "native"},
			{Name: "sync.(*Mutex).Lock", FrameType: "native"},
			{Name: "futex()", FrameType: "syscall"},
			{Name: "futex_wait_queue", FrameType: "kernel"},
		},
		SampledStacks: []*pb.FoldedStack{
			{Frames: []uint32{0, 1, 2, 3, 4}, Samples: 3, LeafType: "kernel", Comm: "fake-proc"},
		},
		PerfCounters: []*pb.PerfCounter{
			{Name: "cycles", Value: "12,000,000", Note: "3.20 GHz"},
			{Name: "instructions", Value: "24,000,000", Note: "2.00 insn per cycle"},
		},
	}
	if target.ProfileWindow != nil {
		if prof := target.ProfileWindow(sample.StartedAt, sample.EndedAt); prof != nil {
			sample.OnCpuStacks = prof.Stacks
			sample.OnCpuTopFunctions = prof.TopFunctions
			t := flame.Table{}
			for _, f := range sample.Frames {
				t.ID(f)
			}
			t.Rebase(prof.Frames, sample.OnCpuStacks, sample.OnCpuTopFunctions)
			sample.Frames = t.Frames()
		}
	}
	return emit(&pb.SampleCommandChunk{Status: "Complete", ProgressPct: 100, Sample: sample, Done: true})
}

// Profiler is an in-memory always-on continuous Profiler for tests.
type Profiler struct {
	Store *profiler.Store
}

func NewProfiler() *Profiler {
	return &Profiler{Store: profiler.NewStore(profiler.DefaultSampleHz)}
}

func (p *Profiler) Start(context.Context) error { return nil }
func (p *Profiler) Stop()                       {}

func (p *Profiler) Status() *pb.ProfilerStatus {
	return &pb.ProfilerStatus{
		Active:       true,
		Engine:       profiler.EngineName,
		SampleHz:     profiler.DefaultSampleHz,
		TotalSamples: p.Store.TotalSamples(),
	}
}

func (p *Profiler) Query(cmdID string, req *pb.GetProfileRequest, live bool) *pb.CommandProfile {
	return p.Store.Query(cmdID, req, live, p.Status())
}

func (p *Profiler) QueryNode(node string, req *pb.GetProfileRequest) *pb.CommandProfile {
	return p.Store.QueryNode(node, req, p.Status())
}

func (p *Profiler) Snapshot(cmdID string) *pb.CommandProfile {
	return p.Store.Snapshot(cmdID, p.Status())
}

func (p *Profiler) Forget(cmdID string) {
	p.Store.Forget(cmdID)
}
