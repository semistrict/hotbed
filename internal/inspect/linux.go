//go:build linux

package inspect

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// Linux implements Inspector using /proc, /sys/fs/cgroup, and Linux tracing
// tools (strace, perf, eu-stack/gdb, bpftrace). When BaseRootDir is set,
// tracers are resolved and executed, as root, inside that rootfs (the shared
// base image, which no command writes) in a private mount namespace with
// host /proc, /sys, and /dev rbind-mounted, requiring no host-installed
// tracer packages. Never a command's own rootfs: its sandbox's root may
// replace any program there (board 8145).
type Linux struct {
	BaseRootDir func() string
}

const (
	clockTicksPerSec = 100 // Linux USER_HZ is 100 on amd64 and arm64
	maxTracePIDs     = 64
	maxStackPIDs     = 16
	maxInspectFiles  = 4000
	maxBPFCgroups    = 32
	perfStatEvents   = "task-clock,context-switches,cpu-migrations,page-faults,cycles,instructions,branches,branch-misses,cache-references,cache-misses"
)

// bpfStartupHeadroom is the extra time allowed on top of the requested trace
// duration for bpftrace to compile and attach probes before SIGINTing it.
// Overridden in tests to exercise startup-timeout paths quickly.
var bpfStartupHeadroom = 10 * time.Second

func (l Linux) baseRootDir() string {
	if l.BaseRootDir != nil {
		return l.BaseRootDir()
	}
	return ""
}

func (l Linux) rootDirFor(Target) string { return l.baseRootDir() }

func (l Linux) Capabilities() []*pb.ToolCapability {
	return capabilitiesIn(l.baseRootDir())
}

func capabilitiesIn(rootDir string) []*pb.ToolCapability {
	tools := []struct {
		name     string
		hostHint string
		rootHint string
	}{
		{"strace", "install strace (apt install strace / dnf install strace)", "rebuild the worker base image so it includes strace"},
		{"perf", "install perf (apt install linux-tools-generic / dnf install perf)", "rebuild the worker base image so it includes linux-perf"},
		{"eu-stack", "install elfutils (apt install elfutils / dnf install elfutils)", "rebuild the worker base image so it includes elfutils"},
		{"gdb", "install gdb (apt install gdb / dnf install gdb)", "rebuild the worker base image so it includes gdb"},
		{"bpftrace", "install bpftrace (apt install bpftrace / dnf install bpftrace)", "rebuild the worker base image so it includes bpftrace"},
	}
	out := make([]*pb.ToolCapability, 0, len(tools))
	for _, t := range tools {
		p, err := lookToolInRoot(rootDir, t.name)
		if err == nil {
			out = append(out, &pb.ToolCapability{Name: t.name, Available: true, Detail: p})
		} else {
			hint := t.hostHint
			if rootDir != "" {
				hint = t.rootHint
			}
			out = append(out, &pb.ToolCapability{Name: t.name, Available: false, Detail: hint})
		}
	}
	return out
}

func (l Linux) Inspect(_ context.Context, target Target, req *pb.InspectCommandRequest) (*pb.InspectCommandResponse, error) {
	pids, err := resolvePIDs(target)
	if err != nil {
		return nil, err
	}
	selectedPIDs := pids
	if req.GetPid() > 0 {
		if !slices.Contains(pids, int(req.Pid)) {
			return nil, api.Errorf(api.CodeInvalidArgs, "PID %d does not belong to command %s", req.Pid, target.CommandID)
		}
		selectedPIDs = []int{int(req.Pid)}
	}
	res := &pb.InspectCommandResponse{
		CommandId:      target.CommandID,
		CollectedAt:    time.Now().UnixMilli(),
		Cgroup:         target.Cgroup,
		UnitName:       target.UnitName,
		AvailableTools: capabilitiesIn(l.rootDirFor(target)),
	}

	commByPID := map[int]string{}
	if WantSection(req, pb.InspectSection_INSPECT_SECTION_PROCS) {
		for _, pid := range pids {
			if p, ok := readProcessInfo(pid, req.GetPid() == int32(pid) || len(pids) <= 12); ok {
				commByPID[pid] = p.Comm
				res.Procs = append(res.Procs, p)
			}
		}
		sort.Slice(res.Procs, func(i, j int) bool { return res.Procs[i].Pid < res.Procs[j].Pid })
	}

	getComm := func(pid int) string {
		if c, ok := commByPID[pid]; ok {
			return c
		}
		if c := readProcComm(pid); c != "" {
			commByPID[pid] = c
			return c
		}
		return ""
	}

	if WantSection(req, pb.InspectSection_INSPECT_SECTION_FILES) {
		sockets := loadSockets()
		for _, pid := range selectedPIDs {
			files := readOpenFiles(pid, getComm(pid), sockets, maxInspectFiles-len(res.Files))
			res.Files = append(res.Files, files...)
			if len(res.Files) >= maxInspectFiles {
				break
			}
		}
	}

	if WantSection(req, pb.InspectSection_INSPECT_SECTION_CGROUP) && target.Cgroup != "" {
		res.CgroupStats = readCgroupStats(target.Cgroup)
	}

	if WantSection(req, pb.InspectSection_INSPECT_SECTION_KSTACK) {
		for _, pid := range selectedPIDs {
			res.Kstacks = append(res.Kstacks, readKernelStacks(pid, getComm(pid))...)
		}
	}

	if WantSection(req, pb.InspectSection_INSPECT_SECTION_MAPS) {
		for _, pid := range selectedPIDs {
			if m, ok := readMemoryMap(pid, getComm(pid)); ok {
				res.Maps = append(res.Maps, m)
			}
		}
	}

	if WantSection(req, pb.InspectSection_INSPECT_SECTION_LIMITS) {
		for _, pid := range selectedPIDs {
			if lim, ok := readProcessLimits(pid, getComm(pid)); ok {
				res.Limits = append(res.Limits, lim)
			}
		}
	}

	return res, nil
}

func (l Linux) Trace(ctx context.Context, target Target, req *pb.TraceCommandRequest, emit Emit) error {
	if req.Kind == pb.TraceKind_TRACE_KIND_UNSPECIFIED {
		return api.Errorf(api.CodeInvalidArgs, "trace kind is required")
	}
	if err := ValidateFilter(req.Filter); err != nil {
		return api.Errorf(api.CodeInvalidArgs, "%v", err)
	}
	pids, err := selectPIDs(target, req.GetPid(), maxTracePIDs, "trace")
	if err != nil {
		return err
	}

	rootDir := l.rootDirFor(target)
	switch req.Kind {
	case pb.TraceKind_TRACE_KIND_STRACE:
		return runStraceLive(ctx, rootDir, pids, req, emit)
	case pb.TraceKind_TRACE_KIND_STRACE_SUMMARY:
		return runStraceSummary(ctx, rootDir, pids, req, emit)
	case pb.TraceKind_TRACE_KIND_PERF_CPU:
		return runPerfCPU(ctx, rootDir, target, pids, req, emit)
	case pb.TraceKind_TRACE_KIND_PERF_STAT:
		return runPerfStat(ctx, rootDir, pids, req, emit)
	case pb.TraceKind_TRACE_KIND_STACKS:
		return runStacks(ctx, rootDir, pids, emit)
	case pb.TraceKind_TRACE_KIND_BPF_EXECSNOOP,
		pb.TraceKind_TRACE_KIND_BPF_OPENSNOOP,
		pb.TraceKind_TRACE_KIND_BPF_VFSSTAT,
		pb.TraceKind_TRACE_KIND_BPF_OFFCPU,
		pb.TraceKind_TRACE_KIND_BPF_NET:
		return runBPFTrace(ctx, rootDir, target, pids, req, emit)
	default:
		return api.Errorf(api.CodeInvalidArgs, "unsupported trace kind %v", req.Kind)
	}
}

func resolvePIDs(target Target) ([]int, error) {
	if target.Cgroup != "" {
		// A sandboxed command's processes are in cgroups below its unit's.
		pids, err := cgroupPIDs(filepath.Join("/sys/fs/cgroup", target.Cgroup))
		if err == nil {
			return pids, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if target.MainPID > 0 {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", target.MainPID)); err == nil {
			return []int{target.MainPID}, nil
		}
	}
	return nil, nil
}

// cgroupPIDs is every process in the cgroup dir and the cgroups below it,
// sorted.
func cgroupPIDs(dir string) ([]int, error) {
	var pids []int
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p != dir && errors.Is(err, os.ErrNotExist) {
				return nil // a cgroup gone while walking
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(filepath.Join(p, "cgroup.procs"))
		if err != nil {
			if p != dir && errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("read %s: %w", filepath.Join(p, "cgroup.procs"), err)
		}
		for _, line := range strings.Fields(string(b)) {
			if pid, err := strconv.Atoi(line); err == nil && pid > 0 {
				pids = append(pids, pid)
			}
		}
		return nil
	})
	sort.Ints(pids)
	return pids, err
}

func selectPIDs(target Target, reqPID int32, maxPIDs int, action string) ([]int, error) {
	pids, err := resolvePIDs(target)
	if err != nil {
		return nil, err
	}
	if len(pids) == 0 {
		return nil, api.Errorf(api.CodeConflict, "command %s has no live processes to %s", target.CommandID, action)
	}
	if reqPID > 0 {
		if !slices.Contains(pids, int(reqPID)) {
			return nil, api.Errorf(api.CodeInvalidArgs, "PID %d does not belong to command %s", reqPID, target.CommandID)
		}
		return []int{int(reqPID)}, nil
	}
	if maxPIDs > 0 && len(pids) > maxPIDs {
		pids = pids[:maxPIDs]
	}
	return pids, nil
}

func readProcComm(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

type procBase struct {
	stat    ProcStat
	status  ProcStatus
	ioRead  int64
	ioWrite int64
	cmdline string
	wchan   string
	oom     int32
	rss     int64
	vsize   int64
}

func readProcBase(pid int) (procBase, bool) {
	base := fmt.Sprintf("/proc/%d", pid)
	statBytes, err := os.ReadFile(base + "/stat")
	if err != nil {
		return procBase{}, false
	}
	st, err := ParseProcStat(string(statBytes))
	if err != nil {
		return procBase{}, false
	}
	statusBytes, _ := os.ReadFile(base + "/status")
	pst := ParseProcStatus(string(statusBytes))
	cmdBytes, _ := os.ReadFile(base + "/cmdline")
	ioBytes, _ := os.ReadFile(base + "/io")
	rbytes, wbytes := ParseProcIO(string(ioBytes))
	wchanBytes, _ := os.ReadFile(base + "/wchan")
	oomBytes, _ := os.ReadFile(base + "/oom_score")
	oomScore, _ := strconv.Atoi(strings.TrimSpace(string(oomBytes)))

	rss := pst.VmRSSBytes
	if rss == 0 {
		rss = st.RSS * int64(os.Getpagesize())
	}
	vsize := pst.VmSizeBytes
	if vsize == 0 {
		vsize = st.VSize
	}
	return procBase{
		stat:    st,
		status:  pst,
		ioRead:  rbytes,
		ioWrite: wbytes,
		cmdline: ParseProcCmdline(cmdBytes),
		wchan:   strings.TrimSpace(string(wchanBytes)),
		oom:     int32(oomScore),
		rss:     rss,
		vsize:   vsize,
	}, true
}

type threadSnap struct {
	tid       int32
	name      string
	state     string
	wchan     string
	cpuCore   int32
	utime     int64
	stime     int64
	runNs     int64
	waitNs    int64
	slices    int64
	volCtx    int64
	nonvolCtx int64
	syscall   *pb.ThreadSyscall
	kstack    []string
}

type threadReadOpts struct {
	arch          string // non-empty: read /syscall for this arch
	needKStack    bool   // read /stack
	requireKStack bool   // skip threads whose /stack cannot be read
	needDetails   bool   // read /status and /schedstat
}

func readProcThreads(pid int, opts threadReadOpts) []threadSnap {
	taskDir := fmt.Sprintf("/proc/%d/task", pid)
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return nil
	}
	out := make([]threadSnap, 0, len(entries))
	for _, ent := range entries {
		tid, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		tbase := filepath.Join(taskDir, ent.Name())
		var kstack []string
		if opts.requireKStack {
			stackBytes, err := os.ReadFile(tbase + "/stack")
			if err != nil {
				continue
			}
			kstack = ParseKernelStack(string(stackBytes))
		}
		var st ProcStat
		if statBytes, err := os.ReadFile(tbase + "/stat"); err == nil {
			st, err = ParseProcStat(string(statBytes))
			if err != nil && !opts.requireKStack {
				continue
			}
		} else if !opts.requireKStack {
			continue
		}
		name := st.Comm
		if name == "" {
			if tcomm, err := os.ReadFile(tbase + "/comm"); err == nil {
				name = strings.TrimSpace(string(tcomm))
			}
		}
		twchan, _ := os.ReadFile(tbase + "/wchan")
		ts := threadSnap{
			tid:     int32(tid),
			name:    name,
			state:   st.State,
			wchan:   strings.TrimSpace(string(twchan)),
			cpuCore: st.Processor,
			utime:   st.UTime,
			stime:   st.STime,
			kstack:  kstack,
		}
		if opts.needKStack && !opts.requireKStack {
			if stackBytes, err := os.ReadFile(tbase + "/stack"); err == nil {
				ts.kstack = ParseKernelStack(string(stackBytes))
			}
		}
		if opts.arch != "" {
			if scBytes, err := os.ReadFile(tbase + "/syscall"); err == nil {
				ts.syscall = ParseProcSyscall(opts.arch, string(scBytes))
			}
		}
		if opts.needDetails {
			tstatusBytes, _ := os.ReadFile(tbase + "/status")
			tpst := ParseProcStatus(string(tstatusBytes))
			ts.volCtx = tpst.VoluntaryCtxtSwitches
			ts.nonvolCtx = tpst.NonvoluntaryCtxtSwitches
			tsched, _ := os.ReadFile(tbase + "/schedstat")
			ts.runNs, ts.waitNs, ts.slices = ParseSchedStat(string(tsched))
		}
		out = append(out, ts)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].tid < out[j].tid })
	return out
}

func readProcessInfo(pid int, includeThreads bool) (*pb.ProcessInfo, bool) {
	pb0, ok := readProcBase(pid)
	if !ok {
		return nil, false
	}
	info := &pb.ProcessInfo{
		Pid:                      pb0.stat.PID,
		Ppid:                     pb0.stat.PPID,
		State:                    pb0.stat.State,
		Comm:                     pb0.stat.Comm,
		Cmdline:                  pb0.cmdline,
		ThreadsCount:             pb0.stat.Threads,
		RssBytes:                 pb0.rss,
		VsizeBytes:               pb0.vsize,
		CpuUserMs:                pb0.stat.UTime * 1000 / clockTicksPerSec,
		CpuSysMs:                 pb0.stat.STime * 1000 / clockTicksPerSec,
		IoReadBytes:              pb0.ioRead,
		IoWriteBytes:             pb0.ioWrite,
		Wchan:                    pb0.wchan,
		OomScore:                 pb0.oom,
		VoluntaryCtxtSwitches:    pb0.status.VoluntaryCtxtSwitches,
		NonvoluntaryCtxtSwitches: pb0.status.NonvoluntaryCtxtSwitches,
	}
	if includeThreads {
		for _, ts := range readProcThreads(pid, threadReadOpts{}) {
			info.Threads = append(info.Threads, &pb.ThreadInfo{
				Tid:       ts.tid,
				Name:      ts.name,
				State:     ts.state,
				Wchan:     ts.wchan,
				CpuUserMs: ts.utime * 1000 / clockTicksPerSec,
				CpuSysMs:  ts.stime * 1000 / clockTicksPerSec,
			})
		}
	}
	return info, true
}

func loadSockets() map[uint64]SocketInfo {
	m := map[uint64]SocketInfo{}
	for _, p := range []string{"tcp", "tcp6", "udp", "udp6"} {
		if b, err := os.ReadFile("/proc/net/" + p); err == nil {
			ParseProcNetInet(p, string(b), m)
		}
	}
	if b, err := os.ReadFile("/proc/net/unix"); err == nil {
		ParseProcNetUnix(string(b), m)
	}
	return m
}

type fdSnap struct {
	pid       int32
	comm      string
	fd        int32
	kind      string
	target    string
	mode      string
	pos       int64
	inode     uint64
	epollTFDs []int32
}

func readPIDFDs(pid int, comm string, maxCount int) []fdSnap {
	if maxCount <= 0 {
		return nil
	}
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return nil
	}
	var out []fdSnap
	for _, ent := range entries {
		if len(out) >= maxCount {
			break
		}
		fdNum, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		target, err := os.Readlink(filepath.Join(fdDir, ent.Name()))
		if err != nil {
			continue
		}
		kind, inode := ClassifyFDTarget(target)
		infoBytes, _ := os.ReadFile(fmt.Sprintf("/proc/%d/fdinfo/%d", pid, fdNum))
		pos, mode, fdInfoIno, epollTFDs := ParseFDInfoDetailed(string(infoBytes))
		if inode == 0 && fdInfoIno > 0 {
			inode = fdInfoIno
		}
		out = append(out, fdSnap{
			pid:       int32(pid),
			comm:      comm,
			fd:        int32(fdNum),
			kind:      kind,
			target:    target,
			mode:      mode,
			pos:       pos,
			inode:     inode,
			epollTFDs: epollTFDs,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].fd < out[j].fd })
	return out
}

func readOpenFiles(pid int, comm string, sockets map[uint64]SocketInfo, maxCount int) []*pb.OpenFile {
	snaps := readPIDFDs(pid, comm, maxCount)
	out := make([]*pb.OpenFile, 0, len(snaps))
	for _, f := range snaps {
		of := &pb.OpenFile{
			Pid:    f.pid,
			Comm:   f.comm,
			Fd:     f.fd,
			Kind:   f.kind,
			Target: f.target,
			Mode:   f.mode,
			Pos:    f.pos,
			Inode:  f.inode,
		}
		if f.kind == "socket" && f.inode > 0 {
			if si, ok := sockets[f.inode]; ok {
				of.SocketProto = si.Proto
				of.SocketState = si.State
				of.LocalAddr = si.Local
				of.RemoteAddr = si.Remote
			}
		}
		out = append(out, of)
	}
	return out
}

type cgroupSnap struct {
	stats           *pb.CgroupStats
	frozen          bool
	memHigh         int64
	memLow          int64
	psiCPUSomeTotal int64
	psiMemSomeTotal int64
	psiMemFullTotal int64
	psiIOSomeTotal  int64
	psiIOFullTotal  int64
	ioReadTotal     int64
	ioWriteTotal    int64
}

func readCgroupSnap(cgroup string) cgroupSnap {
	if cgroup == "" {
		return cgroupSnap{}
	}
	dir := filepath.Join("/sys/fs/cgroup", cgroup)
	if _, err := os.Stat(dir); err != nil {
		return cgroupSnap{}
	}
	st := &pb.CgroupStats{}
	if b, err := os.ReadFile(filepath.Join(dir, "cpu.stat")); err == nil {
		m := ParseKeyValueInt(string(b))
		st.CpuUsageUsec = m["usage_usec"]
		st.CpuUserUsec = m["user_usec"]
		st.CpuSystemUsec = m["system_usec"]
		st.NrThrottled = m["nr_throttled"]
		st.ThrottledUsec = m["throttled_usec"]
	}
	st.MemoryCurrentBytes = readIntFile(filepath.Join(dir, "memory.current"))
	st.MemoryPeakBytes = readIntFile(filepath.Join(dir, "memory.peak"))
	st.MemorySwapBytes = readIntFile(filepath.Join(dir, "memory.swap.current"))
	st.PidsCurrent = readIntFile(filepath.Join(dir, "pids.current"))
	st.PidsPeak = readIntFile(filepath.Join(dir, "pids.peak"))
	if b, err := os.ReadFile(filepath.Join(dir, "memory.stat")); err == nil {
		m := ParseKeyValueInt(string(b))
		st.MemoryAnonBytes = m["anon"]
		st.MemoryFileBytes = m["file"]
		st.MemoryKernelBytes = m["kernel"]
		st.MemorySlabBytes = m["slab"]
		st.MemorySockBytes = m["sock"]
		st.MemoryShmemBytes = m["shmem"]
		st.Pgfault = m["pgfault"]
		st.Pgmajfault = m["pgmajfault"]
	}
	cs := cgroupSnap{
		stats:   st,
		memHigh: readIntFile(filepath.Join(dir, "memory.high")),
		memLow:  readIntFile(filepath.Join(dir, "memory.low")),
	}
	if b, err := os.ReadFile(filepath.Join(dir, "cgroup.events")); err == nil {
		if ParseKeyValueInt(string(b))["frozen"] == 1 {
			cs.frozen = true
		}
	}
	if !cs.frozen && readIntFile(filepath.Join(dir, "cgroup.freeze")) == 1 {
		cs.frozen = true
	}
	if b, err := os.ReadFile(filepath.Join(dir, "cpu.pressure")); err == nil {
		st.PsiCpuSomeAvg10, st.PsiCpuFullAvg10, cs.psiCPUSomeTotal, _ = parsePSIAll(string(b))
	}
	if b, err := os.ReadFile(filepath.Join(dir, "memory.pressure")); err == nil {
		st.PsiMemSomeAvg10, st.PsiMemFullAvg10, cs.psiMemSomeTotal, cs.psiMemFullTotal = parsePSIAll(string(b))
	}
	if b, err := os.ReadFile(filepath.Join(dir, "io.pressure")); err == nil {
		st.PsiIoSomeAvg10, st.PsiIoFullAvg10, cs.psiIOSomeTotal, cs.psiIOFullTotal = parsePSIAll(string(b))
	}
	if b, err := os.ReadFile(filepath.Join(dir, "io.stat")); err == nil {
		st.Io = ParseCgroupIO(string(b))
		for _, d := range st.Io {
			cs.ioReadTotal += d.Rbytes
			cs.ioWriteTotal += d.Wbytes
		}
	}
	return cs
}

func readCgroupStats(cgroup string) *pb.CgroupStats {
	if st := readCgroupSnap(cgroup).stats; st != nil {
		return st
	}
	return &pb.CgroupStats{}
}

func readIntFile(path string) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return v
}

func readKernelStacks(pid int, comm string) []*pb.ThreadKernelStack {
	threads := readProcThreads(pid, threadReadOpts{requireKStack: true})
	out := make([]*pb.ThreadKernelStack, 0, len(threads))
	for _, ts := range threads {
		out = append(out, &pb.ThreadKernelStack{
			Pid:        int32(pid),
			Tid:        ts.tid,
			Comm:       comm,
			ThreadName: ts.name,
			State:      ts.state,
			Wchan:      ts.wchan,
			Frames:     ts.kstack,
		})
	}
	return out
}

func readMemoryMap(pid int, comm string) (*pb.ProcessMemoryMap, bool) {
	rollup, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", pid))
	if err != nil {
		return nil, false
	}
	m := ParseSmapsRollup(int32(pid), comm, string(rollup))
	if mapsBytes, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid)); err == nil {
		m.TopRegions = ParseProcMaps(string(mapsBytes), 25)
	}
	return m, true
}

func readProcessLimits(pid int, comm string) (*pb.ProcessLimits, bool) {
	limBytes, err := os.ReadFile(fmt.Sprintf("/proc/%d/limits", pid))
	if err != nil {
		return nil, false
	}
	statusBytes, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	pst := ParseProcStatus(string(statusBytes))
	return &pb.ProcessLimits{
		Pid:        int32(pid),
		Comm:       comm,
		Items:      ParseProcLimits(string(limBytes)),
		CapEff:     pst.CapEff,
		Seccomp:    pst.Seccomp,
		NoNewPrivs: pst.NoNewPrivs,
	}, true
}

// ---- Active trace runners --------------------------------------------------

func requireToolIn(rootDir, name, installHint string) (string, error) {
	p, err := lookToolInRoot(rootDir, name)
	if err != nil {
		if rootDir != "" {
			return "", api.Errorf(api.CodePlatformNotSupported, "%s is not installed in the workspace base image", name).
				WithSuggestion("rebuild the worker base image so it includes " + name)
		}
		return "", api.Errorf(api.CodePlatformNotSupported, "%s is not installed on this worker", name).
			WithSuggestion(installHint)
	}
	return p, nil
}

func int32Slice(pids []int) []int32 {
	out := make([]int32, len(pids))
	for i, p := range pids {
		out[i] = int32(p)
	}
	return out
}

func pidCSV(pids []int) string {
	s := make([]string, len(pids))
	for i, p := range pids {
		s[i] = strconv.Itoa(p)
	}
	return strings.Join(s, ",")
}

// runToolCommand builds an exec.Cmd that runs in its own process group and
// kills the entire process group (-pgid) on context cancellation or timeout,
// preventing orphaned chroot/unshare tracer child processes.
func runToolCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// runTimedTool starts cmd, streams its combined output via onLine if non-nil,
// sends SIGINT after duration so tracers like strace/bpftrace flush summaries
// cleanly, and kills the process group on cancellation.
func runTimedTool(ctx context.Context, dur time.Duration, name string, args []string, onLine func(string) error) (string, error) {
	var wrapped func(string, func(time.Duration)) error
	if onLine != nil {
		wrapped = func(line string, _ func(time.Duration)) error {
			return onLine(line)
		}
	}
	raw, _, err := runTimedToolWithDeadline(ctx, dur, name, args, wrapped)
	return raw, err
}

// runTimedToolWithDeadline is like runTimedTool, except onLine receives a
// resetTimer callback that can shorten or extend the remaining SIGINT deadline
// once a tracer signals it has finished attaching. It also reports whether the
// deadline timer fired (timedOut) and only normalizes SIGINT/SIGTERM/130/143
// to nil error when the timer initiated the shutdown.
func runTimedToolWithDeadline(ctx context.Context, dur time.Duration, name string, args []string, onLine func(line string, resetTimer func(time.Duration)) error) (string, bool, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pr, pw, err := os.Pipe()
	if err != nil {
		return "", false, err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return "", false, err
	}
	pw.Close()

	done := make(chan struct{})
	timerDone := make(chan struct{})
	abort := make(chan struct{})
	resetCh := make(chan time.Duration, 1)
	var timedOut atomic.Bool
	resetTimer := func(d time.Duration) {
		select {
		case <-resetCh:
		default:
		}
		select {
		case resetCh <- d:
		default:
		}
	}
	go func() {
		defer close(timerDone)
		timer := time.NewTimer(dur)
		defer timer.Stop()
		killAfter := func(grace time.Duration) {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
			graceTimer := time.NewTimer(grace)
			defer graceTimer.Stop()
			select {
			case <-done:
			case <-graceTimer.C:
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		}
		for {
			select {
			case <-done:
				return
			case newDur := <-resetCh:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(newDur)
			case <-timer.C:
				timedOut.Store(true)
				killAfter(3 * time.Second)
				return
			case <-ctx.Done():
				killAfter(1 * time.Second)
				return
			case <-abort:
				killAfter(1 * time.Second)
				return
			}
		}
	}()

	var buf bytes.Buffer
	const maxCaptureBytes = 2 << 20 // 2 MiB safety cap
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var emitErr error
	for sc.Scan() {
		line := sc.Text()
		if buf.Len() < maxCaptureBytes {
			buf.WriteString(line)
			buf.WriteByte('\n')
		}
		if onLine != nil && emitErr == nil {
			emitErr = onLine(line, resetTimer)
			if emitErr != nil {
				close(abort)
				break
			}
		}
	}
	_, _ = io.Copy(io.Discard, pr)
	pr.Close()
	waitErr := cmd.Wait()
	close(done)
	<-timerDone
	wasTimedOut := timedOut.Load()
	if emitErr != nil {
		return buf.String(), wasTimedOut, emitErr
	}
	// SIGINT-terminated tracers exit with signal 2 or code 130; that is normal
	// when our deadline timer initiated the shutdown.
	if waitErr != nil && ctx.Err() == nil {
		if wasTimedOut {
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok {
					if ws.Signaled() && (ws.Signal() == syscall.SIGINT || ws.Signal() == syscall.SIGTERM) {
						return buf.String(), true, nil
					}
				}
				if exitErr.ExitCode() == 130 || exitErr.ExitCode() == 143 {
					return buf.String(), true, nil
				}
			}
		}
		return buf.String(), wasTimedOut, waitErr
	}
	return buf.String(), wasTimedOut, nil
}

func runStraceLive(ctx context.Context, rootDir string, pids []int, req *pb.TraceCommandRequest, emit Emit) error {
	bin, err := requireToolIn(rootDir, "strace", "install strace on the worker (apt install strace)")
	if err != nil {
		return err
	}
	dur := time.Duration(ClampDuration(req.DurationSeconds)) * time.Second
	args := []string{"-f", "-T", "-yy", "-s", "120"}
	if req.Filter != "" {
		args = append(args, "-e", "trace="+req.Filter)
	}
	for _, p := range pids {
		args = append(args, "-p", strconv.Itoa(p))
	}
	cmdLine := "strace " + strings.Join(args, " ")
	execBin, execArgs := chrootCmd(rootDir, "", bin, args)
	started := time.Now()
	if err := emit(&pb.TraceCommandChunk{
		Status: fmt.Sprintf("attaching strace to %d PID(s) for %s", len(pids), dur),
	}); err != nil {
		return err
	}
	var batch strings.Builder
	lastFlush := time.Now()
	flush := func() error {
		if batch.Len() == 0 {
			return nil
		}
		s := batch.String()
		batch.Reset()
		lastFlush = time.Now()
		return emit(&pb.TraceCommandChunk{Text: s})
	}
	raw, err := runTimedTool(ctx, dur, execBin, execArgs, func(line string) error {
		batch.WriteString(line)
		batch.WriteByte('\n')
		if batch.Len() >= 4096 || time.Since(lastFlush) >= 150*time.Millisecond {
			return flush()
		}
		return nil
	})
	if ferr := flush(); ferr != nil {
		return ferr
	}
	if err != nil && ctx.Err() == nil && raw == "" {
		return api.Errorf(api.CodeInternal, "strace failed: %v", err)
	}
	ended := time.Now()
	return emit(&pb.TraceCommandChunk{
		Done: true,
		Summary: &pb.TraceSummary{
			Kind:         req.Kind,
			StartedAt:    started.UnixMilli(),
			EndedAt:      ended.UnixMilli(),
			DurationMs:   ended.Sub(started).Milliseconds(),
			PidsAttached: int32Slice(pids),
			CommandLine:  cmdLine,
		},
	})
}

func runStraceSummary(ctx context.Context, rootDir string, pids []int, req *pb.TraceCommandRequest, emit Emit) error {
	bin, err := requireToolIn(rootDir, "strace", "install strace on the worker (apt install strace)")
	if err != nil {
		return err
	}
	dur := time.Duration(ClampDuration(req.DurationSeconds)) * time.Second
	args := []string{"-f", "-c"}
	if req.Filter != "" {
		args = append(args, "-e", "trace="+req.Filter)
	}
	for _, p := range pids {
		args = append(args, "-p", strconv.Itoa(p))
	}
	cmdLine := "strace " + strings.Join(args, " ")
	execBin, execArgs := chrootCmd(rootDir, "", bin, args)
	started := time.Now()
	if err := emit(&pb.TraceCommandChunk{
		Status: fmt.Sprintf("collecting syscall summary across %d PID(s) for %s...", len(pids), dur),
	}); err != nil {
		return err
	}
	raw, err := runTimedTool(ctx, dur, execBin, execArgs, nil)
	if err != nil && ctx.Err() == nil && raw == "" {
		return api.Errorf(api.CodeInternal, "strace -c failed: %v", err)
	}
	ended := time.Now()
	return emit(&pb.TraceCommandChunk{
		Done: true,
		Summary: &pb.TraceSummary{
			Kind:         req.Kind,
			StartedAt:    started.UnixMilli(),
			EndedAt:      ended.UnixMilli(),
			DurationMs:   ended.Sub(started).Milliseconds(),
			PidsAttached: int32Slice(pids),
			CommandLine:  cmdLine,
			Syscalls:     ParseStraceSummary(raw),
			Raw:          raw,
		},
	})
}

// perfRecordArgs builds the argument list for `perf record`.
// When target.Cgroup is non-empty and req.GetPid() == 0, perf targets the
// cgroup directly (`-a -e cpu-clock -G <cgroup>`) so short-lived child
// processes exiting before attach cannot cause exit 244 ("No such process")
// and newly spawned children in the cgroup are captured as well. `-e cpu-clock`
// must precede `-G` (perf requires an event before cgroups) and works both on
// bare metal and inside VMs without hardware PMU counters. Otherwise perf
// targets the explicit PID list via `-p`.
func perfRecordArgs(target Target, pids []int, req *pb.TraceCommandRequest, perfDataArg string) []string {
	dur := ClampDuration(req.GetDurationSeconds())
	hz := ClampFrequency(req.GetFrequencyHz())
	args := []string{
		"record",
		"-F", strconv.Itoa(hz),
		"-g", "--call-graph", "dwarf,16384",
	}
	if target.Cgroup != "" && req.GetPid() == 0 {
		args = append(args, "-a", "-e", "cpu-clock", "-G", strings.TrimPrefix(target.Cgroup, "/"))
	} else {
		args = append(args, "-p", pidCSV(pids))
	}
	args = append(args, "-o", perfDataArg, "--", "sleep", strconv.Itoa(dur))
	return args
}

func validateTargetCgroup(cgroup string) (cgRel, cgPath string, err error) {
	cgRel = strings.TrimPrefix(cgroup, "/")
	// Validate the raw target cgroup before filepath.Join cleans away any ".." segments.
	rawCGPath := "/sys/fs/cgroup/" + cgRel
	if err := validateCgroupPath(rawCGPath); err != nil {
		return "", "", api.Errorf(api.CodeInvalidArgs, "%v", err)
	}
	cgPath = filepath.Join("/sys/fs/cgroup", cgRel)
	if err := validateCgroupPath(cgPath); err != nil {
		return "", "", api.Errorf(api.CodeInvalidArgs, "%v", err)
	}
	return cgRel, cgPath, nil
}

func isPerfNoSuchProcess(err error, stderr string) bool {
	if err == nil {
		return false
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 244 {
		return true
	}
	return strings.Contains(stderr, "No such process")
}

func filterLivePIDs(pids []int) []int {
	live := make([]int, 0, len(pids))
	for _, pid := range pids {
		if pid <= 0 {
			continue
		}
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err == nil {
			live = append(live, pid)
		}
	}
	return live
}

func runPerfCPU(ctx context.Context, rootDir string, target Target, pids []int, req *pb.TraceCommandRequest, emit Emit) error {
	bin, err := requireToolIn(rootDir, "perf", "install perf on the worker (apt install linux-tools-generic)")
	if err != nil {
		return err
	}
	useCgroup := target.Cgroup != "" && req.GetPid() == 0
	if useCgroup {
		_, cgPath, err := validateTargetCgroup(target.Cgroup)
		if err != nil {
			return err
		}
		if _, err := os.Stat(cgPath); err != nil {
			return api.Errorf(api.CodeConflict, "command cgroup %s is gone: %v", cgPath, err)
		}
	}
	dur := ClampDuration(req.DurationSeconds)
	hz := ClampFrequency(req.FrequencyHz)
	tmpDir, err := os.MkdirTemp("", "hotbed-perf-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	hostDataFile := filepath.Join(tmpDir, "perf.data")
	perfDataArg := hostDataFile
	tmpBind := ""
	if rootDir != "" {
		perfDataArg = "/tmp/perf.data"
		tmpBind = tmpDir
	}

	args := perfRecordArgs(target, pids, req, perfDataArg)
	cmdLine := "perf " + strings.Join(args, " ")
	started := time.Now()
	statusMsg := fmt.Sprintf("recording perf CPU profile (%d Hz, DWARF call-graph) on %d PID(s) for %ds...", hz, len(pids), dur)
	if useCgroup {
		statusMsg = fmt.Sprintf("recording perf CPU profile (%d Hz, DWARF call-graph) in cgroup %s for %ds...", hz, target.Cgroup, dur)
	}
	if err := emit(&pb.TraceCommandChunk{Status: statusMsg}); err != nil {
		return err
	}

	runRecord := func(recArgs []string) (string, error) {
		recBin, wrappedArgs := chrootCmd(rootDir, tmpBind, bin, recArgs)
		recCmd := runToolCommand(ctx, recBin, wrappedArgs...)
		var recErr bytes.Buffer
		recCmd.Stderr = &recErr
		err := recCmd.Run()
		return strings.TrimSpace(recErr.String()), err
	}

	stderrStr, err := runRecord(args)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// When targeting multiple PIDs via -p, a transient child may exit
		// between PID discovery and perf attach (exit 244 / "No such process").
		// Re-read live PIDs and retry once cleanly.
		if !useCgroup && req.GetPid() == 0 && len(pids) > 1 && isPerfNoSuchProcess(err, stderrStr) {
			if st, serr := os.Stat(hostDataFile); serr != nil || st.Size() == 0 {
				var retryPIDs []int
				if refreshed, rerr := selectPIDs(target, 0, maxTracePIDs, "trace"); rerr == nil {
					retryPIDs = filterLivePIDs(refreshed)
				}
				if len(retryPIDs) == 0 {
					retryPIDs = filterLivePIDs(pids)
				}
				if len(retryPIDs) > 0 {
					_ = os.Remove(hostDataFile)
					pids = retryPIDs
					args = perfRecordArgs(target, pids, req, perfDataArg)
					cmdLine = "perf " + strings.Join(args, " ")
					stderrStr, err = runRecord(args)
					if err != nil && ctx.Err() != nil {
						return ctx.Err()
					}
				}
			}
		}
		// If the process exited mid-sleep, perf.data may still have valid samples.
		if err != nil {
			if st, serr := os.Stat(hostDataFile); serr != nil || st.Size() == 0 {
				return api.Errorf(api.CodeInternal, "perf record failed: %v: %s", err, stderrStr)
			}
		}
	}

	if err := emit(&pb.TraceCommandChunk{Status: "symbolizing samples and building call graph..."}); err != nil {
		return err
	}

	scrBin, scrArgs := chrootCmd(rootDir, tmpBind, bin, []string{"script", "-i", perfDataArg})
	scriptOut, err := runToolCommand(ctx, scrBin, scrArgs...).Output()
	if err != nil && ctx.Err() == nil {
		return api.Errorf(api.CodeInternal, "perf script failed: %v", err)
	}
	frames, stacks, topFns := ParsePerfScript(string(scriptOut), 250)

	repBin, repArgs := chrootCmd(rootDir, tmpBind, bin, []string{"report", "--stdio", "--no-children", "--percent-limit", "0.5", "-i", perfDataArg})
	reportOut, _ := runToolCommand(ctx, repBin, repArgs...).Output()
	ended := time.Now()
	return emit(&pb.TraceCommandChunk{
		Done: true,
		Summary: &pb.TraceSummary{
			Kind:         req.Kind,
			StartedAt:    started.UnixMilli(),
			EndedAt:      ended.UnixMilli(),
			DurationMs:   ended.Sub(started).Milliseconds(),
			PidsAttached: int32Slice(pids),
			CommandLine:  cmdLine,
			Frames:       frames,
			Stacks:       stacks,
			TopFunctions: topFns,
			Raw:          string(reportOut),
		},
	})
}

func execPerfStat(ctx context.Context, rootDir, bin string, args []string) ([]byte, []*pb.PerfCounter, error) {
	execBin, execArgs := chrootCmd(rootDir, "", bin, args)
	out, err := runToolCommand(ctx, execBin, execArgs...).CombinedOutput()
	return out, ParsePerfStat(string(out)), err
}

func resolveStackTool(rootDir string) (toolName, bin string, ok bool) {
	if b, err := lookToolInRoot(rootDir, "eu-stack"); err == nil {
		return "eu-stack", b, true
	}
	if b, err := lookToolInRoot(rootDir, "gdb"); err == nil {
		return "gdb", b, true
	}
	return "", "", false
}

func execStackCapture(ctx context.Context, rootDir, tool, bin string, pid, maxFrames int, includeModules bool) []byte {
	if tool == "eu-stack" {
		var args []string
		if includeModules {
			args = append(args, "-m")
		}
		if maxFrames > 0 {
			args = append(args, "-n", strconv.Itoa(maxFrames))
		}
		args = append(args, "-p", strconv.Itoa(pid))
		b, a := chrootCmd(rootDir, "", bin, args)
		out, _ := runToolCommand(ctx, b, a...).CombinedOutput()
		return out
	}
	b, a := chrootCmd(rootDir, "", bin, gdbBacktraceArgs(pid, maxFrames))
	cmd := runToolCommand(ctx, b, a...)
	cmd.Env = sanitizedTracerEnv(os.Environ())
	out, _ := cmd.CombinedOutput()
	return out
}

func runPerfStat(ctx context.Context, rootDir string, pids []int, req *pb.TraceCommandRequest, emit Emit) error {
	bin, err := requireToolIn(rootDir, "perf", "install perf on the worker (apt install linux-tools-generic)")
	if err != nil {
		return err
	}
	dur := ClampDuration(req.DurationSeconds)
	args := []string{
		"stat",
		"-e", perfStatEvents,
		"-p", pidCSV(pids),
		"--", "sleep", strconv.Itoa(dur),
	}
	cmdLine := "perf " + strings.Join(args, " ")
	started := time.Now()
	if err := emit(&pb.TraceCommandChunk{
		Status: fmt.Sprintf("measuring hardware & software counters (perf stat) on %d PID(s) for %ds...", len(pids), dur),
	}); err != nil {
		return err
	}
	out, counters, err := execPerfStat(ctx, rootDir, bin, args)
	if err != nil && ctx.Err() == nil && len(out) == 0 {
		return api.Errorf(api.CodeInternal, "perf stat failed: %v", err)
	}
	ended := time.Now()
	return emit(&pb.TraceCommandChunk{
		Done: true,
		Summary: &pb.TraceSummary{
			Kind:         req.Kind,
			StartedAt:    started.UnixMilli(),
			EndedAt:      ended.UnixMilli(),
			DurationMs:   ended.Sub(started).Milliseconds(),
			PidsAttached: int32Slice(pids),
			CommandLine:  cmdLine,
			Counters:     counters,
			Raw:          string(out),
		},
	})
}

func runStacks(ctx context.Context, rootDir string, pids []int, emit Emit) error {
	toolName, toolBin, ok := resolveStackTool(rootDir)
	if !ok {
		if rootDir != "" {
			return api.Errorf(api.CodePlatformNotSupported, "neither eu-stack nor gdb is installed in the workspace base image").
				WithSuggestion("rebuild the worker base image so it includes elfutils or gdb")
		}
		return api.Errorf(api.CodePlatformNotSupported, "neither eu-stack nor gdb is installed on this worker").
			WithSuggestion("install elfutils (eu-stack) or gdb on the worker")
	}
	if len(pids) > maxStackPIDs {
		pids = pids[:maxStackPIDs]
	}
	started := time.Now()
	if err := emit(&pb.TraceCommandChunk{
		Status: fmt.Sprintf("capturing user + kernel thread stacks for %d PID(s) via %s...", len(pids), toolName),
	}); err != nil {
		return err
	}

	var buf strings.Builder
	for _, pid := range pids {
		comm := readProcComm(pid)
		fmt.Fprintf(&buf, "=== PID %d (%s) ===\n", pid, comm)
		buf.Write(execStackCapture(ctx, rootDir, toolName, toolBin, pid, 0, false))
		if !strings.HasSuffix(buf.String(), "\n") {
			buf.WriteByte('\n')
		}
		kstacks := readKernelStacks(pid, comm)
		if len(kstacks) > 0 {
			buf.WriteString("--- Kernel stacks ---\n")
			for _, ks := range kstacks {
				fmt.Fprintf(&buf, "TID %d (%s) [%s] wchan=%s\n", ks.Tid, ks.ThreadName, ks.State, ks.Wchan)
				for _, fr := range ks.Frames {
					fmt.Fprintf(&buf, "  %s\n", fr)
				}
			}
		}
		buf.WriteByte('\n')
	}
	ended := time.Now()
	return emit(&pb.TraceCommandChunk{
		Done: true,
		Summary: &pb.TraceSummary{
			Kind:         pb.TraceKind_TRACE_KIND_STACKS,
			StartedAt:    started.UnixMilli(),
			EndedAt:      ended.UnixMilli(),
			DurationMs:   ended.Sub(started).Milliseconds(),
			PidsAttached: int32Slice(pids),
			CommandLine:  fmt.Sprintf("%s -p %s", toolName, pidCSV(pids)),
			Raw:          buf.String(),
		},
	})
}

func parseCgroupV2Path(content string) (string, bool) {
	for _, line := range strings.Split(strings.TrimSpace(content), "\n") {
		if rel, ok := strings.CutPrefix(line, "0::"); ok && rel != "" && rel != "/" {
			return filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(rel, "/")), true
		}
	}
	return "", false
}

func readPIDCgroupPath(pid int) (string, bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", false
	}
	return parseCgroupV2Path(string(b))
}

// collectBPFCgroupDirs returns unitDir and any child cgroup directories below
// it. If the subtree has more than maxDirs directories, it returns an error
// rather than silently omitting cgroups.
func collectBPFCgroupDirs(unitDir string, maxDirs int) ([]string, error) {
	if _, err := os.Stat(unitDir); err != nil {
		return nil, err
	}
	root := unitDir
	var dirs []string
	total := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if p != root && errors.Is(walkErr, os.ErrNotExist) {
				return nil // transient sub-cgroup vanished while walking
			}
			return walkErr
		}
		if !d.IsDir() {
			return nil
		}
		total++
		if maxDirs <= 0 || len(dirs) < maxDirs {
			dirs = append(dirs, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if maxDirs > 0 && total > maxDirs {
		return nil, api.Errorf(api.CodeInvalidArgs, "command cgroup tree has %d sub-cgroups (limit %d); specify --pid to target a process", total, maxDirs).
			WithSuggestion("re-run with --pid <PID> to target a specific process")
	}
	sort.Strings(dirs)
	return dirs, nil
}

// resolveBPFCgroupPaths resolves the concrete leaf/subtree cgroup v2 paths to
// match in bpftrace for a command's unit cgroup cgPath. When pid > 0, it first
// checks /proc/<pid>/cgroup so explicit PID traces target that PID's exact
// cgroup (as long as it stays inside cgPath).
func resolveBPFCgroupPaths(cgPath string, pid int32) ([]string, error) {
	return resolveBPFCgroupPathsAt("/sys/fs/cgroup", cgPath, pid)
}

// resolveBPFCgroupPathsAt resolves the concrete leaf/subtree cgroup v2 paths
// under fsRoot (normally "/sys/fs/cgroup") for a command's logical cgroup
// cgPath (always rooted at "/sys/fs/cgroup/..."). Separating fsRoot lets unit
// tests verify cgroup-cap and path-validation errors on synthetic trees.
func resolveBPFCgroupPathsAt(fsRoot, cgPath string, pid int32) ([]string, error) {
	if err := validateCgroupPath(cgPath); err != nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "%v", err)
	}
	cgRel := strings.TrimPrefix(cgPath, "/sys/fs/cgroup/")
	unitDir := filepath.Join(fsRoot, cgRel)
	if _, err := os.Stat(unitDir); err != nil {
		return nil, api.Errorf(api.CodeConflict, "command cgroup %s is gone: %v", cgPath, err)
	}
	if pid > 0 {
		if pidCG, ok := readPIDCgroupPath(int(pid)); ok &&
			(pidCG == cgPath || strings.HasPrefix(pidCG, cgPath+"/")) {
			if err := validateCgroupPath(pidCG); err != nil {
				return nil, api.Errorf(api.CodeInvalidArgs, "cannot match PID %d cgroup %q in bpftrace: %v", pid, pidCG, err)
			}
			pidDir := filepath.Join(fsRoot, strings.TrimPrefix(pidCG, "/sys/fs/cgroup/"))
			if fi, err := os.Stat(pidDir); err == nil && fi.IsDir() {
				return []string{pidCG}, nil
			}
		}
	}
	dirs, err := collectBPFCgroupDirs(unitDir, maxBPFCgroups)
	if err != nil {
		var apiErr *api.Error
		if errors.As(err, &apiErr) {
			return nil, err
		}
		if errors.Is(err, os.ErrNotExist) {
			return nil, api.Errorf(api.CodeConflict, "command cgroup %s is gone: %v", cgPath, err)
		}
		return nil, api.Errorf(api.CodeInternal, "walk command cgroup %s: %v", cgPath, err)
	}
	valid := make([]string, 0, len(dirs))
	for _, d := range dirs {
		rel, err := filepath.Rel(fsRoot, d)
		if err != nil {
			return nil, api.Errorf(api.CodeInternal, "resolve sub-cgroup %q: %v", d, err)
		}
		logical := "/sys/fs/cgroup/" + filepath.ToSlash(rel)
		if err := validateCgroupPath(logical); err != nil {
			return nil, api.Errorf(api.CodeInvalidArgs, "cannot match sub-cgroup %q in bpftrace: %v; specify --pid to target a process", logical, err).
				WithSuggestion("re-run with --pid <PID> to target a specific process")
		}
		valid = append(valid, logical)
	}
	if len(valid) == 0 {
		return nil, api.Errorf(api.CodeConflict, "command cgroup %s has no valid cgroup directories", cgPath)
	}
	return valid, nil
}

func bpftraceArgs(prog string) []string {
	// -B none disables stdio block buffering when stdout is a pipe so streaming
	// probes (execsnoop, opensnoop) flush every event immediately and output is
	// never lost if bpftrace is interrupted.
	return []string{"-B", "none", "-e", prog}
}

func runBPFTrace(ctx context.Context, rootDir string, target Target, pids []int, req *pb.TraceCommandRequest, emit Emit) error {
	bin, err := requireToolIn(rootDir, "bpftrace", "install bpftrace on the worker (apt install bpftrace)")
	if err != nil {
		return err
	}
	if target.Cgroup == "" {
		return api.Errorf(api.CodePlatformNotSupported, "command %s has no cgroup v2 path to scope bpftrace", target.CommandID)
	}
	_, cgPath, err := validateTargetCgroup(target.Cgroup)
	if err != nil {
		return err
	}
	cgPaths, err := resolveBPFCgroupPaths(cgPath, req.GetPid())
	if err != nil {
		return err
	}
	dur := time.Duration(ClampDuration(req.DurationSeconds)) * time.Second
	durSec := int(dur.Seconds())

	label, prog, err := buildBPFTraceProgramForCgroups(req.Kind, cgPaths, durSec, req.GetPid())
	if err != nil {
		return err
	}

	started := time.Now()
	cgLabel := strings.TrimPrefix(cgPaths[0], "/sys/fs/cgroup")
	if len(cgPaths) > 1 {
		cgLabel = fmt.Sprintf("%s (+%d sub-cgroup(s))", cgLabel, len(cgPaths)-1)
	}
	status := fmt.Sprintf("running bpftrace %s in cgroup %s for %ds...", label, cgLabel, durSec)
	if req.GetPid() > 0 {
		status = fmt.Sprintf("running bpftrace %s in cgroup %s (PID %d) for %ds...", label, cgLabel, req.GetPid(), durSec)
	}
	if err := emit(&pb.TraceCommandChunk{Status: status}); err != nil {
		return err
	}

	args := bpftraceArgs(prog)
	execBin, execArgs := chrootCmd(rootDir, "", bin, args)
	isStreaming := req.Kind == pb.TraceKind_TRACE_KIND_BPF_EXECSNOOP || req.Kind == pb.TraceKind_TRACE_KIND_BPF_OPENSNOOP
	var onLine func(string) error
	if isStreaming {
		onLine = func(line string) error {
			return emit(&pb.TraceCommandChunk{Text: line + "\n"})
		}
	}
	// bpftrace's own `interval:s:N { exit(); }` terminates normally after dur
	// once probes attach. Keep an outer safety timeout with headroom for
	// multi-kprobe attach/detach (sending SIGINT so maps still flush), and
	// tighten it promptly once bpftrace v0.25+ emits "Attached ..." after
	// probes are live so a non-firing interval timer never stalls.
	var attached bool
	raw, timedOut, err := runTimedToolWithDeadline(ctx, dur+bpfStartupHeadroom, execBin, execArgs, func(line string, resetTimer func(time.Duration)) error {
		if strings.HasPrefix(line, "Attached ") {
			attached = true
			resetTimer(dur + 2*time.Second)
		}
		if onLine != nil {
			return onLine(line)
		}
		return nil
	})
	if ctx.Err() == nil {
		detail := strings.TrimSpace(raw)
		if !attached && timedOut {
			if detail != "" {
				return api.Errorf(api.CodeInternal, "bpftrace timed out before attaching probes: %s", detail)
			}
			return api.Errorf(api.CodeInternal, "bpftrace timed out before attaching probes")
		}
		if err != nil {
			if detail != "" {
				return api.Errorf(api.CodeInternal, "bpftrace failed: %v: %s", err, detail)
			}
			return api.Errorf(api.CodeInternal, "bpftrace failed: %v", err)
		}
		if !attached && detail == "" {
			return api.Errorf(api.CodeInternal, "bpftrace exited before attaching probes")
		}
	}
	ended := time.Now()
	return emit(&pb.TraceCommandChunk{
		Done: true,
		Summary: &pb.TraceSummary{
			Kind:         req.Kind,
			StartedAt:    started.UnixMilli(),
			EndedAt:      ended.UnixMilli(),
			DurationMs:   ended.Sub(started).Milliseconds(),
			PidsAttached: int32Slice(pids),
			CommandLine:  "bpftrace -B none -e '" + prog + "'",
			Raw:          raw,
		},
	})
}
