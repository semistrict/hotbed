//go:build linux

package inspect

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// Linux implements Inspector using /proc, /sys/fs/cgroup, and host Linux
// tracing tools (strace, perf, eu-stack/gdb, bpftrace).
type Linux struct{}

const (
	clockTicksPerSec = 100 // Linux USER_HZ is 100 on amd64 and arm64
	maxTracePIDs     = 64
	maxStackPIDs     = 16
	maxInspectFiles  = 4000
)

func (Linux) Capabilities() []*pb.ToolCapability {
	tools := []struct {
		name string
		hint string
	}{
		{"strace", "install strace (apt install strace / dnf install strace)"},
		{"perf", "install perf (apt install linux-tools-generic / dnf install perf)"},
		{"eu-stack", "install elfutils (apt install elfutils / dnf install elfutils)"},
		{"gdb", "install gdb (apt install gdb / dnf install gdb)"},
		{"bpftrace", "install bpftrace (apt install bpftrace / dnf install bpftrace)"},
	}
	out := make([]*pb.ToolCapability, 0, len(tools))
	for _, t := range tools {
		p, err := exec.LookPath(t.name)
		if err == nil {
			out = append(out, &pb.ToolCapability{Name: t.name, Available: true, Detail: p})
		} else {
			out = append(out, &pb.ToolCapability{Name: t.name, Available: false, Detail: t.hint})
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
		AvailableTools: l.Capabilities(),
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
		if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err == nil {
			c := strings.TrimSpace(string(b))
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
	pids, err := resolvePIDs(target)
	if err != nil {
		return err
	}
	if len(pids) == 0 {
		return api.Errorf(api.CodeConflict, "command %s has no live processes to trace", target.CommandID)
	}
	if req.GetPid() > 0 {
		if !slices.Contains(pids, int(req.Pid)) {
			return api.Errorf(api.CodeInvalidArgs, "PID %d does not belong to command %s", req.Pid, target.CommandID)
		}
		pids = []int{int(req.Pid)}
	}
	if len(pids) > maxTracePIDs {
		pids = pids[:maxTracePIDs]
	}

	switch req.Kind {
	case pb.TraceKind_TRACE_KIND_STRACE:
		return runStraceLive(ctx, pids, req, emit)
	case pb.TraceKind_TRACE_KIND_STRACE_SUMMARY:
		return runStraceSummary(ctx, pids, req, emit)
	case pb.TraceKind_TRACE_KIND_PERF_CPU:
		return runPerfCPU(ctx, pids, req, emit)
	case pb.TraceKind_TRACE_KIND_PERF_STAT:
		return runPerfStat(ctx, pids, req, emit)
	case pb.TraceKind_TRACE_KIND_STACKS:
		return runStacks(ctx, pids, emit)
	case pb.TraceKind_TRACE_KIND_BPF_EXECSNOOP,
		pb.TraceKind_TRACE_KIND_BPF_OPENSNOOP,
		pb.TraceKind_TRACE_KIND_BPF_VFSSTAT,
		pb.TraceKind_TRACE_KIND_BPF_OFFCPU,
		pb.TraceKind_TRACE_KIND_BPF_NET:
		return runBPFTrace(ctx, target, pids, req, emit)
	default:
		return api.Errorf(api.CodeInvalidArgs, "unsupported trace kind %v", req.Kind)
	}
}

func resolvePIDs(target Target) ([]int, error) {
	if target.Cgroup != "" {
		p := filepath.Join("/sys/fs/cgroup", target.Cgroup, "cgroup.procs")
		b, err := os.ReadFile(p)
		if err == nil {
			var pids []int
			for _, line := range strings.Fields(string(b)) {
				if pid, err := strconv.Atoi(line); err == nil && pid > 0 {
					pids = append(pids, pid)
				}
			}
			sort.Ints(pids)
			return pids, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
	}
	if target.MainPID > 0 {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", target.MainPID)); err == nil {
			return []int{target.MainPID}, nil
		}
	}
	return nil, nil
}

func readProcessInfo(pid int, includeThreads bool) (*pb.ProcessInfo, bool) {
	base := fmt.Sprintf("/proc/%d", pid)
	statBytes, err := os.ReadFile(base + "/stat")
	if err != nil {
		return nil, false
	}
	st, err := ParseProcStat(string(statBytes))
	if err != nil {
		return nil, false
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

	info := &pb.ProcessInfo{
		Pid:                      st.PID,
		Ppid:                     st.PPID,
		State:                    st.State,
		Comm:                     st.Comm,
		Cmdline:                  ParseProcCmdline(cmdBytes),
		ThreadsCount:             st.Threads,
		RssBytes:                 rss,
		VsizeBytes:               vsize,
		CpuUserMs:                st.UTime * 1000 / clockTicksPerSec,
		CpuSysMs:                 st.STime * 1000 / clockTicksPerSec,
		IoReadBytes:              rbytes,
		IoWriteBytes:             wbytes,
		Wchan:                    strings.TrimSpace(string(wchanBytes)),
		OomScore:                 int32(oomScore),
		VoluntaryCtxtSwitches:    pst.VoluntaryCtxtSwitches,
		NonvoluntaryCtxtSwitches: pst.NonvoluntaryCtxtSwitches,
	}

	if includeThreads {
		if entries, err := os.ReadDir(base + "/task"); err == nil {
			for _, ent := range entries {
				tid, err := strconv.Atoi(ent.Name())
				if err != nil {
					continue
				}
				tbase := fmt.Sprintf("%s/task/%d", base, tid)
				tstatBytes, err := os.ReadFile(tbase + "/stat")
				if err != nil {
					continue
				}
				tst, err := ParseProcStat(string(tstatBytes))
				if err != nil {
					continue
				}
				twchan, _ := os.ReadFile(tbase + "/wchan")
				info.Threads = append(info.Threads, &pb.ThreadInfo{
					Tid:       int32(tid),
					Name:      tst.Comm,
					State:     tst.State,
					Wchan:     strings.TrimSpace(string(twchan)),
					CpuUserMs: tst.UTime * 1000 / clockTicksPerSec,
					CpuSysMs:  tst.STime * 1000 / clockTicksPerSec,
				})
			}
			sort.Slice(info.Threads, func(i, j int) bool { return info.Threads[i].Tid < info.Threads[j].Tid })
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

func readOpenFiles(pid int, comm string, sockets map[uint64]SocketInfo, maxCount int) []*pb.OpenFile {
	if maxCount <= 0 {
		return nil
	}
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return nil
	}
	var out []*pb.OpenFile
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
		pos, mode := ParseFDInfo(string(infoBytes))
		of := &pb.OpenFile{
			Pid:    int32(pid),
			Comm:   comm,
			Fd:     int32(fdNum),
			Kind:   kind,
			Target: target,
			Mode:   mode,
			Pos:    pos,
			Inode:  inode,
		}
		if kind == "socket" && inode > 0 {
			if si, ok := sockets[inode]; ok {
				of.SocketProto = si.Proto
				of.SocketState = si.State
				of.LocalAddr = si.Local
				of.RemoteAddr = si.Remote
			}
		}
		out = append(out, of)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Fd < out[j].Fd })
	return out
}

func readCgroupStats(cgroup string) *pb.CgroupStats {
	dir := filepath.Join("/sys/fs/cgroup", cgroup)
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
	if b, err := os.ReadFile(filepath.Join(dir, "cpu.pressure")); err == nil {
		st.PsiCpuSomeAvg10, st.PsiCpuFullAvg10 = ParsePSI(string(b))
	}
	if b, err := os.ReadFile(filepath.Join(dir, "memory.pressure")); err == nil {
		st.PsiMemSomeAvg10, st.PsiMemFullAvg10 = ParsePSI(string(b))
	}
	if b, err := os.ReadFile(filepath.Join(dir, "io.pressure")); err == nil {
		st.PsiIoSomeAvg10, st.PsiIoFullAvg10 = ParsePSI(string(b))
	}
	if b, err := os.ReadFile(filepath.Join(dir, "io.stat")); err == nil {
		st.Io = ParseCgroupIO(string(b))
	}
	return st
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
	taskDir := fmt.Sprintf("/proc/%d/task", pid)
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return nil
	}
	var out []*pb.ThreadKernelStack
	for _, ent := range entries {
		tid, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		tbase := filepath.Join(taskDir, ent.Name())
		stackBytes, err := os.ReadFile(filepath.Join(tbase, "stack"))
		if err != nil {
			continue
		}
		tcomm, _ := os.ReadFile(filepath.Join(tbase, "comm"))
		twchan, _ := os.ReadFile(filepath.Join(tbase, "wchan"))
		tstate := ""
		if statBytes, err := os.ReadFile(filepath.Join(tbase, "stat")); err == nil {
			if st, err := ParseProcStat(string(statBytes)); err == nil {
				tstate = st.State
			}
		}
		out = append(out, &pb.ThreadKernelStack{
			Pid:        int32(pid),
			Tid:        int32(tid),
			Comm:       comm,
			ThreadName: strings.TrimSpace(string(tcomm)),
			State:      tstate,
			Wchan:      strings.TrimSpace(string(twchan)),
			Frames:     ParseKernelStack(string(stackBytes)),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tid < out[j].Tid })
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

func requireTool(name, installHint string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
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

// runTimedTool starts cmd, streams its combined output via onLine if non-nil,
// sends SIGINT after duration so tracers like strace/bpftrace flush summaries
// cleanly, and kills the process group on cancellation.
func runTimedTool(ctx context.Context, dur time.Duration, name string, args []string, onLine func(string) error) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pr, pw, err := os.Pipe()
	if err != nil {
		return "", err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return "", err
	}
	pw.Close()

	done := make(chan struct{})
	go func() {
		timer := time.NewTimer(dur)
		defer timer.Stop()
		select {
		case <-done:
			return
		case <-timer.C:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
			select {
			case <-done:
				return
			case <-time.After(3 * time.Second):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		case <-ctx.Done():
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
			select {
			case <-done:
				return
			case <-time.After(1 * time.Second):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
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
			emitErr = onLine(line)
			if emitErr != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
				break
			}
		}
	}
	_, _ = io.Copy(io.Discard, pr)
	pr.Close()
	waitErr := cmd.Wait()
	close(done)
	if emitErr != nil {
		return buf.String(), emitErr
	}
	// SIGINT-terminated tracers exit with signal 2 or code 130; that is normal.
	if waitErr != nil && ctx.Err() == nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				if ws.Signaled() && (ws.Signal() == syscall.SIGINT || ws.Signal() == syscall.SIGTERM) {
					return buf.String(), nil
				}
			}
			if exitErr.ExitCode() == 130 || exitErr.ExitCode() == 143 {
				return buf.String(), nil
			}
		}
		return buf.String(), waitErr
	}
	return buf.String(), nil
}

func runStraceLive(ctx context.Context, pids []int, req *pb.TraceCommandRequest, emit Emit) error {
	bin, err := requireTool("strace", "install strace on the worker (apt install strace)")
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
	raw, err := runTimedTool(ctx, dur, bin, args, func(line string) error {
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

func runStraceSummary(ctx context.Context, pids []int, req *pb.TraceCommandRequest, emit Emit) error {
	bin, err := requireTool("strace", "install strace on the worker (apt install strace)")
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
	started := time.Now()
	if err := emit(&pb.TraceCommandChunk{
		Status: fmt.Sprintf("collecting syscall summary across %d PID(s) for %s...", len(pids), dur),
	}); err != nil {
		return err
	}
	raw, err := runTimedTool(ctx, dur, bin, args, nil)
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

func runPerfCPU(ctx context.Context, pids []int, req *pb.TraceCommandRequest, emit Emit) error {
	bin, err := requireTool("perf", "install perf on the worker (apt install linux-tools-generic)")
	if err != nil {
		return err
	}
	dur := ClampDuration(req.DurationSeconds)
	hz := ClampFrequency(req.FrequencyHz)
	tmpDir, err := os.MkdirTemp("", "hotbed-perf-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	dataFile := filepath.Join(tmpDir, "perf.data")

	args := []string{
		"record",
		"-F", strconv.Itoa(hz),
		"-g", "--call-graph", "dwarf,16384",
		"-p", pidCSV(pids),
		"-o", dataFile,
		"--", "sleep", strconv.Itoa(dur),
	}
	cmdLine := "perf " + strings.Join(args, " ")
	started := time.Now()
	if err := emit(&pb.TraceCommandChunk{
		Status: fmt.Sprintf("recording perf CPU profile (%d Hz, DWARF call-graph) on %d PID(s) for %ds...", hz, len(pids), dur),
	}); err != nil {
		return err
	}

	recCmd := exec.CommandContext(ctx, bin, args...)
	var recErr bytes.Buffer
	recCmd.Stderr = &recErr
	if err := recCmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// If the process exited mid-sleep, perf.data may still have valid samples.
		if st, serr := os.Stat(dataFile); serr != nil || st.Size() == 0 {
			return api.Errorf(api.CodeInternal, "perf record failed: %v: %s", err, strings.TrimSpace(recErr.String()))
		}
	}

	if err := emit(&pb.TraceCommandChunk{Status: "symbolizing samples and building call graph..."}); err != nil {
		return err
	}

	scriptOut, err := exec.CommandContext(ctx, bin, "script", "-i", dataFile).Output()
	if err != nil && ctx.Err() == nil {
		return api.Errorf(api.CodeInternal, "perf script failed: %v", err)
	}
	stacks, topFns := ParsePerfScript(string(scriptOut), 250)

	reportOut, _ := exec.CommandContext(ctx, bin, "report", "--stdio", "--no-children", "--percent-limit", "0.5", "-i", dataFile).Output()
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
			Stacks:       stacks,
			TopFunctions: topFns,
			Raw:          string(reportOut),
		},
	})
}

func runPerfStat(ctx context.Context, pids []int, req *pb.TraceCommandRequest, emit Emit) error {
	bin, err := requireTool("perf", "install perf on the worker (apt install linux-tools-generic)")
	if err != nil {
		return err
	}
	dur := ClampDuration(req.DurationSeconds)
	args := []string{
		"stat",
		"-e", "task-clock,context-switches,cpu-migrations,page-faults,cycles,instructions,branches,branch-misses,cache-references,cache-misses",
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
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.CombinedOutput()
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
			Counters:     ParsePerfStat(string(out)),
			Raw:          string(out),
		},
	})
}

func runStacks(ctx context.Context, pids []int, emit Emit) error {
	euBin, euErr := exec.LookPath("eu-stack")
	gdbBin, gdbErr := exec.LookPath("gdb")
	if euErr != nil && gdbErr != nil {
		return api.Errorf(api.CodePlatformNotSupported, "neither eu-stack nor gdb is installed on this worker").
			WithSuggestion("install elfutils (eu-stack) or gdb on the worker")
	}
	if len(pids) > maxStackPIDs {
		pids = pids[:maxStackPIDs]
	}
	toolName := "eu-stack"
	if euErr != nil {
		toolName = "gdb"
	}
	started := time.Now()
	if err := emit(&pb.TraceCommandChunk{
		Status: fmt.Sprintf("capturing user + kernel thread stacks for %d PID(s) via %s...", len(pids), toolName),
	}); err != nil {
		return err
	}

	var buf strings.Builder
	for _, pid := range pids {
		commBytes, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		comm := strings.TrimSpace(string(commBytes))
		fmt.Fprintf(&buf, "=== PID %d (%s) ===\n", pid, comm)
		var out []byte
		if euErr == nil {
			out, _ = exec.CommandContext(ctx, euBin, "-p", strconv.Itoa(pid)).CombinedOutput()
		} else {
			out, _ = exec.CommandContext(ctx, gdbBin, "-batch", "-ex", "set pagination off", "-ex", "thread apply all bt", "-p", strconv.Itoa(pid)).CombinedOutput()
		}
		buf.Write(out)
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

func runBPFTrace(ctx context.Context, target Target, pids []int, req *pb.TraceCommandRequest, emit Emit) error {
	bin, err := requireTool("bpftrace", "install bpftrace on the worker (apt install bpftrace)")
	if err != nil {
		return err
	}
	if target.Cgroup == "" {
		return api.Errorf(api.CodePlatformNotSupported, "command %s has no cgroup v2 path to scope bpftrace", target.CommandID)
	}
	cgPath := filepath.Join("/sys/fs/cgroup", target.Cgroup)
	if _, err := os.Stat(cgPath); err != nil {
		return api.Errorf(api.CodeConflict, "command cgroup %s is gone: %v", cgPath, err)
	}
	dur := time.Duration(ClampDuration(req.DurationSeconds)) * time.Second
	durSec := int(dur.Seconds())

	var prog string
	var label string
	switch req.Kind {
	case pb.TraceKind_TRACE_KIND_BPF_EXECSNOOP:
		label = "execsnoop (process exec events)"
		prog = fmt.Sprintf(`tracepoint:syscalls:sys_enter_execve /cgroup == cgroupid("%s")/ { printf("%%-8d %%-16s ", pid, comm); join(args.argv); } interval:s:%d { exit(); }`, cgPath, durSec)
	case pb.TraceKind_TRACE_KIND_BPF_OPENSNOOP:
		label = "opensnoop (file open events)"
		prog = fmt.Sprintf(`tracepoint:syscalls:sys_enter_openat /cgroup == cgroupid("%s")/ { printf("%%-8d %%-16s %%s\n", pid, comm, str(args.filename)); } interval:s:%d { exit(); }`, cgPath, durSec)
	case pb.TraceKind_TRACE_KIND_BPF_VFSSTAT:
		label = "vfsstat (VFS read/write/open/fsync counts)"
		prog = fmt.Sprintf(`kprobe:vfs_read,kprobe:vfs_write,kprobe:vfs_open,kprobe:vfs_fsync /cgroup == cgroupid("%s")/ { @[probe, comm] = count(); } interval:s:%d { exit(); }`, cgPath, durSec)
	case pb.TraceKind_TRACE_KIND_BPF_OFFCPU:
		label = "offcpu (blocking / sleep stack traces)"
		prog = fmt.Sprintf(`tracepoint:sched:sched_switch /cgroup == cgroupid("%s")/ { @start[args.prev_pid] = nsecs; } tracepoint:sched:sched_switch /@start[args.next_pid]/ { $d = (nsecs - @start[args.next_pid]) / 1000; delete(@start[args.next_pid]); @[kstack(8), comm] = sum($d); } interval:s:%d { exit(); } END { clear(@start); }`, cgPath, durSec)
	case pb.TraceKind_TRACE_KIND_BPF_NET:
		label = "tcp/net activity (connect, sendmsg, recvmsg)"
		prog = fmt.Sprintf(`kprobe:tcp_v4_connect,kprobe:tcp_v6_connect,kprobe:tcp_sendmsg,kprobe:tcp_recvmsg /cgroup == cgroupid("%s")/ { @[func, comm] = count(); } interval:s:%d { exit(); }`, cgPath, durSec)
	default:
		return api.Errorf(api.CodeInvalidArgs, "unsupported bpf trace kind %v", req.Kind)
	}

	started := time.Now()
	if err := emit(&pb.TraceCommandChunk{
		Status: fmt.Sprintf("running bpftrace %s in cgroup %s for %ds...", label, target.Cgroup, durSec),
	}); err != nil {
		return err
	}

	args := []string{"-e", prog}
	isStreaming := req.Kind == pb.TraceKind_TRACE_KIND_BPF_EXECSNOOP || req.Kind == pb.TraceKind_TRACE_KIND_BPF_OPENSNOOP
	var onLine func(string) error
	if isStreaming {
		onLine = func(line string) error {
			return emit(&pb.TraceCommandChunk{Text: line + "\n"})
		}
	}
	raw, err := runTimedTool(ctx, dur+5*time.Second, bin, args, onLine)
	if err != nil && ctx.Err() == nil && raw == "" {
		return api.Errorf(api.CodeInternal, "bpftrace failed: %v", err)
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
			CommandLine:  "bpftrace -e '" + prog + "'",
			Raw:          raw,
		},
	})
}
