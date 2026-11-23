//go:build linux

package inspect

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/flame"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

const (
	maxSamplePIDs      = 32
	maxSampleStackPIDs = 12
	maxSampleFiles     = 4000
	perPIDStackTimeout = 3 * time.Second
)

type procSnap struct {
	procBase
	exe     string
	cwd     string
	threads map[int32]threadSnap
}

type threadAccum struct {
	tid              int32
	stateCounts      map[string]int32
	syscallCounts    map[string]int32
	totalPolls       int32
	firstStackSig    string
	pollsSameStack   int32
	userStackSamples int32
	lastUserFrames   []*pb.StackFrame
	lastKernelFrames []string
	lastSyscall      *pb.ThreadSyscall
}

// Sample performs a unified, macOS Activity Monitor-style Linux process sample
// combining /proc deltas, multi-poll kernel stacks & syscalls, repeated
// user-space stack snapshots (eu-stack / gdb), enriched open files/sockets/pipes/
// epoll/locks, cgroup v2 & PSI stall deltas, perf stat counters, and the worker's
// always-on OTel eBPF continuous profile window.
func (l Linux) Sample(ctx context.Context, target Target, req *pb.SampleCommandRequest, emit SampleEmit) error {
	durSec := ClampSampleDuration(req.GetDurationSeconds())
	dur := time.Duration(durSec) * time.Second
	arch := target.Arch
	if arch == "" {
		arch = runtime.GOARCH
	}

	pids, err := selectPIDs(target, req.GetPid(), maxSamplePIDs, "sample")
	if err != nil {
		return err
	}

	startedAt := time.Now()
	deadline := startedAt.Add(dur)
	if err := emit(&pb.SampleCommandChunk{
		Status:      fmt.Sprintf("Snapshot T0: reading /proc, open files, locks & cgroup across %d PID(s)...", len(pids)),
		ProgressPct: 5,
	}); err != nil {
		return err
	}

	var collectors []*pb.SampleCollector

	// 1. T0 snapshots: processes, threads, open FDs, cgroup v2.
	t0Start := time.Now()
	cgT0 := readCgroupSnap(target.Cgroup)
	procsT0 := map[int]procSnap{}
	for _, pid := range pids {
		if ps, ok := readProcSnap(pid, arch); ok {
			procsT0[pid] = ps
		}
	}
	fdsT0 := readFDSnaps(pids, procsT0)
	t0Dur := time.Since(t0Start).Milliseconds()

	// 2. Launch `perf stat` in background across the sample window (non-ptrace,
	// zero interference with eu-stack). Skipped if cgroup is frozen.
	type perfResult struct {
		counters []*pb.PerfCounter
		ok       bool
		detail   string
		durMs    int64
	}
	rootDir := l.rootDirFor(target)
	pCtx, pCancel := context.WithDeadline(ctx, startedAt.Add(dur+4*time.Second))
	defer pCancel()
	perfCh := make(chan perfResult, 1)
	if cgT0.frozen {
		perfCh <- perfResult{ok: false, detail: "skipped: cgroup is frozen (parked/paused)"}
	} else if perfBin, perr := lookToolInRoot(rootDir, "perf"); perr != nil {
		perfCh <- perfResult{ok: false, detail: "skipped: perf not installed"}
	} else {
		go func() {
			pStart := time.Now()
			// Prefer -p with live PIDs; if perf fails due to transient PID exit, retry with -a -G <cgroup>
			// using only the remaining window time so fallback never doubles duration past pCtx.
			_, counters, runErr := execPerfStat(pCtx, rootDir, perfBin, []string{"stat", "-e", perfStatEvents, "-p", pidCSV(pids), "--", "sleep", strconv.Itoa(durSec)})
			if (runErr != nil || len(counters) == 0) && target.Cgroup != "" && req.GetPid() == 0 && pCtx.Err() == nil {
				rem := time.Until(deadline)
				if dl, ok := pCtx.Deadline(); ok {
					if avail := time.Until(dl) - 2*time.Second; avail < rem {
						rem = avail
					}
				}
				if sleepArg, ok := perfFallbackSleepArg(rem); ok {
					cgRel := strings.TrimPrefix(target.Cgroup, "/")
					_, c2, runErr2 := execPerfStat(pCtx, rootDir, perfBin, []string{"stat", "-a", "-e", perfStatEvents, "-G", cgRel, "--", "sleep", sleepArg})
					if len(c2) > 0 {
						counters = c2
						runErr = runErr2
					} else if runErr2 != nil {
						runErr = runErr2
					}
				}
			}
			dMs := time.Since(pStart).Milliseconds()
			if len(counters) > 0 {
				perfCh <- perfResult{counters: counters, ok: true, detail: fmt.Sprintf("%d hardware/software counters", len(counters)), durMs: dMs}
				return
			}
			detail := "no counters returned"
			if runErr != nil {
				detail = runErr.Error()
			}
			perfCh <- perfResult{ok: false, detail: detail, durMs: dMs}
		}()
	}

	// 3. Pure non-ptrace multi-poll loop over the sample window [T0 .. T1]:
	//    Lightweight /proc thread state + wchan + syscall + kernel stack polls
	//    every ~250ms. Keeping ptrace (eu-stack/gdb) strictly AFTER T1 ensures
	//    voluntary context switches, CPU deltas, schedstat, and perf stat are
	//    100% unperturbed by ptrace SIGSTOP stops.
	accum := map[int]map[int32]*threadAccum{} // pid -> tid -> accum

	pollInterval := 250 * time.Millisecond
	targetPolls := int(dur / pollInterval)
	if targetPolls < 4 {
		targetPolls = 4
		pollInterval = dur / time.Duration(targetPolls)
	}
	if targetPolls > 30 {
		targetPolls = 30
		pollInterval = dur / time.Duration(targetPolls)
	}
	recordPoll := func(curPIDs []int) {
		for _, pid := range curPIDs {
			tmap := accum[pid]
			if tmap == nil {
				tmap = map[int32]*threadAccum{}
				accum[pid] = tmap
			}
			for _, ts := range readProcThreads(pid, threadReadOpts{arch: arch, needKStack: true}) {
				tid := ts.tid
				acc := tmap[tid]
				if acc == nil {
					acc = &threadAccum{
						tid:           tid,
						stateCounts:   map[string]int32{},
						syscallCounts: map[string]int32{},
					}
					tmap[tid] = acc
				}
				acc.totalPolls++
				acc.stateCounts[ts.state]++
				scName := "running"
				scPC := ""
				if ts.syscall != nil {
					scName = ts.syscall.Name
					scPC = ts.syscall.Pc
					acc.lastSyscall = ts.syscall
				}
				acc.syscallCounts[scName]++
				if len(ts.kstack) > 0 {
					acc.lastKernelFrames = ts.kstack
				}
				sig := ts.state + "|" + scName + "|" + scPC + "|" + strings.Join(ts.kstack, ";")
				if acc.firstStackSig == "" {
					acc.firstStackSig = sig
					acc.pollsSameStack = 1
				} else if acc.firstStackSig == sig {
					acc.pollsSameStack++
				}
			}
		}
	}

	pollCount := 0
	for pollIdx := 0; pollIdx < targetPolls; pollIdx++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		curPIDs := pids
		if req.GetPid() == 0 {
			if fresh, err := resolvePIDs(target); err == nil && len(fresh) > 0 {
				if len(fresh) > maxSamplePIDs {
					fresh = fresh[:maxSamplePIDs]
				}
				curPIDs = fresh
			}
		}
		recordPoll(curPIDs)
		pollCount++

		pct := 10 + (65 * (pollIdx + 1) / targetPolls)
		if err := emit(&pb.SampleCommandChunk{
			Status:      fmt.Sprintf("Observing %d PID(s): poll %d/%d (non-intrusive /proc + kstack)...", len(curPIDs), pollIdx+1, targetPolls),
			ProgressPct: int32(pct),
		}); err != nil {
			return err
		}

		// Wait until next scheduled poll slot (or exit early if window reached).
		if pollIdx+1 < targetPolls {
			nextTick := startedAt.Add(time.Duration(pollIdx+1) * pollInterval)
			if now := time.Now(); now.Before(nextTick) && now.Before(deadline) {
				wait := nextTick.Sub(now)
				if rem := deadline.Sub(now); wait > rem {
					wait = rem
				}
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
		}
	}

	// 4. T1 end snapshots (still 100% ptrace-free for clean deltas!):
	//    processes, threads, open FDs, sockets, /proc/locks, cgroup v2.
	if err := emit(&pb.SampleCommandChunk{
		Status:      "Snapshot T1: reading end-of-window deltas, open files, sockets & locks...",
		ProgressPct: 78,
	}); err != nil {
		return err
	}

	finalPIDs := pids
	if req.GetPid() == 0 {
		if fresh, err := resolvePIDs(target); err == nil && len(fresh) > 0 {
			if len(fresh) > maxSamplePIDs {
				fresh = fresh[:maxSamplePIDs]
			}
			finalPIDs = fresh
		}
	}
	cgT1 := readCgroupSnap(target.Cgroup)
	procsT1 := map[int]procSnap{}
	for _, pid := range finalPIDs {
		if ps, ok := readProcSnap(pid, arch); ok {
			procsT1[pid] = ps
		}
	}
	fdsT1 := readFDSnaps(finalPIDs, procsT1)
	sockets := loadSockets()
	rawLocks := readProcLocks()
	endedAt := time.Now()
	durationMs := max(int64(1), endedAt.Sub(startedAt).Milliseconds())
	durFloatSec := float64(durationMs) / 1000.0

	// 5. Capture user-space stacks via eu-stack (or gdb fallback) AFTER T1 so
	//    ptrace attach/detach never perturbs T0->T1 context-switch or CPU deltas.
	//    Take 1 snapshot if every thread was completely stationary, or up to 3
	//    snapshots spaced 120ms apart when threads are actively moving.
	stackTool := ""
	stackBin := ""
	if !cgT0.frozen && !cgT1.frozen {
		stackTool, stackBin, _ = resolveStackTool(rootDir)
	}
	anyMoving := false
	for pid, p1 := range procsT1 {
		p0, hadT0 := procsT0[pid]
		for tid, t1 := range p1.threads {
			t0, hadT0T := p0.threads[tid]
			if !hadT0 || !hadT0T || t1.state == "R" || (t1.utime-t0.utime)+(t1.stime-t0.stime) > 0 || (t1.volCtx-t0.volCtx)+(t1.nonvolCtx-t0.nonvolCtx) > 0 {
				anyMoving = true
				break
			}
		}
		if anyMoving {
			break
		}
	}
	desiredUserSnaps := 1
	if anyMoving {
		desiredUserSnaps = 3
	}
	userSnapCount := 0
	var userStackDurMs int64
	userStackErrors := 0
	userSnapshotsByPID := map[int][]map[int32][]*pb.StackFrame{}
	if stackTool != "" && len(finalPIDs) > 0 {
		if err := emit(&pb.SampleCommandChunk{
			Status:      fmt.Sprintf("Unwinding user-space thread stacks via %s (%d snapshot(s))...", stackTool, desiredUserSnaps),
			ProgressPct: 86,
		}); err != nil {
			return err
		}
		for snapIdx := 0; snapIdx < desiredUserSnaps; snapIdx++ {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			uStart := time.Now()
			framesByPID := captureUserStacks(ctx, rootDir, stackTool, stackBin, finalPIDs, procsT1)
			userStackDurMs += time.Since(uStart).Milliseconds()
			if len(framesByPID) > 0 {
				userSnapCount++
				for pid, byTID := range framesByPID {
					userSnapshotsByPID[pid] = append(userSnapshotsByPID[pid], byTID)
					tmap := accum[pid]
					if tmap == nil {
						tmap = map[int32]*threadAccum{}
						accum[pid] = tmap
					}
					for tid, frames := range byTID {
						acc := tmap[tid]
						if acc == nil {
							acc = &threadAccum{tid: tid, stateCounts: map[string]int32{}, syscallCounts: map[string]int32{}}
							tmap[tid] = acc
						}
						acc.userStackSamples++
						acc.lastUserFrames = frames
					}
				}
			} else {
				userStackErrors++
			}
			if snapIdx+1 < desiredUserSnaps {
				timer := time.NewTimer(120 * time.Millisecond)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
		}
	}

	// Build collector summary list.
	collectors = append(collectors,
		&pb.SampleCollector{Name: "proc_delta", Ok: true, Detail: fmt.Sprintf("%d PID(s) · T0→T1 deltas over %.2fs", len(procsT1), durFloatSec), DurationMs: t0Dur},
		&pb.SampleCollector{Name: "kstack_poll", Ok: true, Detail: fmt.Sprintf("%d /proc thread + kstack + syscall polls", pollCount), DurationMs: durationMs},
	)
	if cgT0.frozen || cgT1.frozen {
		collectors = append(collectors, &pb.SampleCollector{Name: "user_stacks", Ok: false, Detail: "skipped: cgroup is frozen (parked/paused)"})
	} else if stackTool == "" {
		collectors = append(collectors, &pb.SampleCollector{Name: "user_stacks", Ok: false, Detail: "skipped: neither eu-stack nor gdb installed"})
	} else {
		detail := fmt.Sprintf("%s (%d snapshot(s))", stackTool, userSnapCount)
		if userStackErrors > 0 {
			detail += fmt.Sprintf(", %d failed/timed out", userStackErrors)
		}
		collectors = append(collectors, &pb.SampleCollector{Name: "user_stacks", Ok: userSnapCount > 0, Detail: detail, DurationMs: userStackDurMs})
	}

	// Build enriched open files (pos delta, sockets with tx/rx queues, pipe peers, epoll tfds, locks).
	t0PosByKey := map[[2]int32]int64{}
	for _, f := range fdsT0 {
		t0PosByKey[[2]int32{f.pid, f.fd}] = f.pos
	}
	// Index pipe inodes across all PIDs in this command so we can link pipe peers!
	type pipeHolder struct {
		pid  int32
		comm string
		fd   int32
		mode string
	}
	pipeHolders := map[uint64][]pipeHolder{}
	inodeToTarget := map[uint64]string{}
	cmdPIDSet := map[int32]string{}
	for pid, ps := range procsT1 {
		cmdPIDSet[int32(pid)] = ps.stat.Comm
	}
	for _, f := range fdsT1 {
		if f.kind == "pipe" && f.inode > 0 {
			pipeHolders[f.inode] = append(pipeHolders[f.inode], pipeHolder{pid: f.pid, comm: f.comm, fd: f.fd, mode: f.mode})
		}
		if f.inode > 0 && f.target != "" && inodeToTarget[f.inode] == "" {
			inodeToTarget[f.inode] = f.target
		}
	}

	// Correlate /proc/locks relevant to our PIDs or open file inodes.
	var matchedLocks []*pb.FileLock
	lockByInode := map[uint64]string{}
	for _, lk := range rawLocks {
		_, pidMatch := cmdPIDSet[lk.PID]
		targetPath, inoMatch := inodeToTarget[lk.Inode]
		if !pidMatch && !inoMatch {
			continue
		}
		comm := cmdPIDSet[lk.PID]
		if comm == "" {
			comm = readProcComm(int(lk.PID))
		}
		matchedLocks = append(matchedLocks, &pb.FileLock{
			Kind:    lk.Kind,
			Mode:    lk.Mode,
			Access:  lk.Access,
			Pid:     lk.PID,
			Comm:    comm,
			Inode:   lk.Inode,
			Range:   lk.Range,
			Blocked: lk.Blocked,
			Target:  targetPath,
		})
		if lk.Inode > 0 {
			tag := fmt.Sprintf("%s %s (PID %d)", lk.Kind, lk.Access, lk.PID)
			if lk.Blocked {
				tag = "WAITING " + tag
			}
			if cur := lockByInode[lk.Inode]; cur == "" {
				lockByInode[lk.Inode] = tag
			} else if !strings.Contains(cur, tag) {
				lockByInode[lk.Inode] = cur + "; " + tag
			}
		}
	}

	// Construct SampledOpenFile list and per-PID FD lookup map for syscall arg0 correlation!
	fdDescByPIDFD := map[[2]int32]string{}
	fdCountByPID := map[int32]int32{}
	sampledFiles := make([]*pb.SampledOpenFile, 0, len(fdsT1))
	for _, f := range fdsT1 {
		fdCountByPID[f.pid]++
		var posDelta int64
		if oldPos, ok := t0PosByKey[[2]int32{f.pid, f.fd}]; ok {
			posDelta = f.pos - oldPos
		}
		sf := &pb.SampledOpenFile{
			Pid:       f.pid,
			Comm:      f.comm,
			Fd:        f.fd,
			Kind:      f.kind,
			Target:    f.target,
			Mode:      f.mode,
			Pos:       f.pos,
			PosDelta:  posDelta,
			Inode:     f.inode,
			EpollTfds: f.epollTFDs,
			LockInfo:  lockByInode[f.inode],
		}
		if f.kind == "socket" && f.inode > 0 {
			if si, ok := sockets[f.inode]; ok {
				sf.SocketProto = si.Proto
				sf.SocketState = si.State
				sf.LocalAddr = si.Local
				sf.RemoteAddr = si.Remote
				sf.TxQueueBytes = si.TxQueue
				sf.RxQueueBytes = si.RxQueue
			}
		}
		if f.kind == "pipe" && f.inode > 0 {
			var peers []string
			for _, h := range pipeHolders[f.inode] {
				if h.pid == f.pid && h.fd == f.fd {
					continue
				}
				peers = append(peers, fmt.Sprintf("PID %d (%s) fd %d [%s]", h.pid, h.comm, h.fd, nonEmpty(h.mode, "?")))
			}
			if len(peers) > 0 {
				sf.Peer = strings.Join(peers, ", ")
			}
		}
		sampledFiles = append(sampledFiles, sf)

		// Short description for syscall arg0 correlation.
		desc := fmt.Sprintf("fd %d -> %s", f.fd, f.target)
		if sf.SocketProto != "" {
			addr := sf.LocalAddr
			if sf.RemoteAddr != "" && !strings.HasSuffix(sf.RemoteAddr, ":0") {
				addr += " -> " + sf.RemoteAddr
			}
			desc = fmt.Sprintf("fd %d -> %s %s %s", f.fd, sf.SocketProto, sf.SocketState, addr)
		} else if sf.Peer != "" {
			desc = fmt.Sprintf("fd %d -> %s <-> %s", f.fd, f.target, sf.Peer)
		}
		fdDescByPIDFD[[2]int32{f.pid, f.fd}] = desc
	}

	collectors = append(collectors,
		&pb.SampleCollector{Name: "net_sockets", Ok: true, Detail: fmt.Sprintf("%d open FD(s), %d socket table entries", len(sampledFiles), len(sockets))},
		&pb.SampleCollector{Name: "proc_locks", Ok: true, Detail: fmt.Sprintf("%d relevant lock(s) (%d system-wide)", len(matchedLocks), len(rawLocks))},
	)

	// Assemble SampledProcess list with per-thread deltas and correlated syscalls.
	// One frames table serves the sampled stacks and the on-CPU ones.
	var frames flame.Table
	type foldedKey struct {
		frames   string // flame.AppendID's key of the stack's frames
		leafType string
		comm     string
	}
	foldedCounts := map[foldedKey]int64{}
	recordFolded := func(comm string, pid int, path []*pb.ProfileFrame) {
		if len(path) == 0 {
			return
		}
		key := make([]byte, 0, 4*(len(path)+1))
		key = flame.AppendID(key, frames.ID(&pb.ProfileFrame{Name: nonEmpty(comm, fmt.Sprintf("pid-%d", pid)), FrameType: "process"}))
		leafType := "native"
		for _, fr := range path {
			switch fr.FrameType {
			case "kernel", "syscall":
				leafType = "kernel"
			default:
				leafType = "native"
			}
			key = flame.AppendID(key, frames.ID(fr))
		}
		foldedCounts[foldedKey{frames: string(key), leafType: leafType, comm: comm}]++
	}

	sort.Ints(finalPIDs)
	sampledProcs := make([]*pb.SampledProcess, 0, len(finalPIDs))
	for _, pid := range finalPIDs {
		p1, ok := procsT1[pid]
		if !ok {
			continue
		}
		p0, hadT0 := procsT0[pid]
		var (
			userDeltaMs, sysDeltaMs   float64
			ioReadDelta, ioWriteDelta int64
			volDelta, nonvolDelta     int64
			minFltDelta, majFltDelta  int64
		)
		if hadT0 {
			userDeltaMs = float64(max(int64(0), p1.stat.UTime-p0.stat.UTime)) * 1000.0 / clockTicksPerSec
			sysDeltaMs = float64(max(int64(0), p1.stat.STime-p0.stat.STime)) * 1000.0 / clockTicksPerSec
			ioReadDelta = max(int64(0), p1.ioRead-p0.ioRead)
			ioWriteDelta = max(int64(0), p1.ioWrite-p0.ioWrite)
			volDelta = max(int64(0), p1.status.VoluntaryCtxtSwitches-p0.status.VoluntaryCtxtSwitches)
			nonvolDelta = max(int64(0), p1.status.NonvoluntaryCtxtSwitches-p0.status.NonvoluntaryCtxtSwitches)
			minFltDelta = max(int64(0), p1.stat.MinFlt-p0.stat.MinFlt)
			majFltDelta = max(int64(0), p1.stat.MajFlt-p0.stat.MajFlt)
		}
		cpuPct := 100.0 * (userDeltaMs + sysDeltaMs) / float64(durationMs)

		memMap, _ := readMemoryMap(pid, p1.stat.Comm)
		limits, _ := readProcessLimits(pid, p1.stat.Comm)
		rss := p1.rss
		var pss, privDirty, swapBytes int64
		if memMap != nil {
			if memMap.RssBytes > 0 {
				rss = memMap.RssBytes
			}
			pss = memMap.PssBytes
			privDirty = memMap.PrivateDirtyBytes
			swapBytes = memMap.SwapBytes
		}

		// Per-thread assembly + folded stack path construction.
		tids := make([]int32, 0, len(p1.threads))
		for tid := range p1.threads {
			tids = append(tids, tid)
		}
		sort.Slice(tids, func(i, j int) bool { return tids[i] < tids[j] })
		threads := make([]*pb.SampledThread, 0, len(tids))
		tmap := accum[pid]
		uSnaps := userSnapshotsByPID[pid]
		for _, tid := range tids {
			t1 := p1.threads[tid]
			t0, hadT0Thread := p0.threads[tid]
			var (
				tuDelta, tsDelta      float64
				runDeltaMs, waitDelta float64
				slicesDelta           int64
				tVolDelta, tNonvol    int64
			)
			if hadT0 && hadT0Thread {
				tuDelta = float64(max(int64(0), t1.utime-t0.utime)) * 1000.0 / clockTicksPerSec
				tsDelta = float64(max(int64(0), t1.stime-t0.stime)) * 1000.0 / clockTicksPerSec
				runDeltaMs = float64(max(int64(0), t1.runNs-t0.runNs)) / 1e6
				waitDelta = float64(max(int64(0), t1.waitNs-t0.waitNs)) / 1e6
				slicesDelta = max(int64(0), t1.slices-t0.slices)
				tVolDelta = max(int64(0), t1.volCtx-t0.volCtx)
				tNonvol = max(int64(0), t1.nonvolCtx-t0.nonvolCtx)
			}
			sc := t1.syscall
			if sc != nil && SyscallTakesFDArg0(sc.Name) && len(sc.Args) > 0 {
				if fdNum, err := strconv.ParseInt(sc.Args[0], 0, 32); err == nil && fdNum >= 0 {
					if desc, ok := fdDescByPIDFD[[2]int32{int32(pid), int32(fdNum)}]; ok {
						sc.FdTarget = desc
					}
				}
			}
			st := &pb.SampledThread{
				Tid:                   tid,
				Name:                  t1.name,
				State:                 t1.state,
				Wchan:                 t1.wchan,
				CpuCore:               t1.cpuCore,
				CpuUserDeltaMs:        tuDelta,
				CpuSysDeltaMs:         tsDelta,
				SchedRunDeltaMs:       runDeltaMs,
				SchedWaitDeltaMs:      waitDelta,
				SchedSlicesDelta:      slicesDelta,
				VoluntaryCtxtDelta:    tVolDelta,
				NonvoluntaryCtxtDelta: tNonvol,
				CpuUserMs:             t1.utime * 1000 / clockTicksPerSec,
				CpuSysMs:              t1.stime * 1000 / clockTicksPerSec,
				Syscall:               sc,
				KernelFrames:          t1.kstack,
			}
			if acc := tmap[tid]; acc != nil {
				st.StateCounts = acc.stateCounts
				st.SyscallCounts = acc.syscallCounts
				st.PollsSameStack = acc.pollsSameStack
				st.TotalPolls = acc.totalPolls
				st.UserFrames = acc.lastUserFrames
				st.UserStackSamples = acc.userStackSamples
				if len(st.KernelFrames) == 0 && len(acc.lastKernelFrames) > 0 {
					st.KernelFrames = acc.lastKernelFrames
				}
				// A thread is stationary when it held the exact same stack/PC across all
				// polls and incurred virtually zero CPU and zero context switches.
				st.Stationary = acc.totalPolls >= 2 &&
					acc.pollsSameStack == acc.totalPolls &&
					t1.state != "R" &&
					(tuDelta+tsDelta) < 1.0 &&
					(tVolDelta+tNonvol) == 0
			}
			threads = append(threads, st)

			// Feed folded stacks cleanly: one entry per captured user snapshot for
			// this thread, or a single syscall+kernel path if no user frames were captured.
			addedUser := false
			for _, snap := range uSnaps {
				if uFrames := snap[tid]; len(uFrames) > 0 {
					path := buildCombinedPath(uFrames, sc, st.KernelFrames)
					if len(path) > 0 {
						recordFolded(p1.stat.Comm, pid, path)
						addedUser = true
					}
				}
			}
			if !addedUser {
				if path := buildCombinedPath(nil, sc, st.KernelFrames); len(path) > 0 {
					recordFolded(p1.stat.Comm, pid, path)
				}
			}
		}

		sampledProcs = append(sampledProcs, &pb.SampledProcess{
			Pid:                   int32(pid),
			Ppid:                  p1.stat.PPID,
			State:                 p1.stat.State,
			Comm:                  p1.stat.Comm,
			Cmdline:               p1.cmdline,
			Exe:                   p1.exe,
			Cwd:                   p1.cwd,
			ThreadsCount:          p1.stat.Threads,
			OpenFdsCount:          fdCountByPID[int32(pid)],
			RssBytes:              rss,
			PssBytes:              pss,
			PrivateDirtyBytes:     privDirty,
			SwapBytes:             swapBytes,
			VsizeBytes:            p1.stat.VSize,
			OomScore:              p1.oom,
			CpuPct:                cpuPct,
			CpuUserDeltaMs:        userDeltaMs,
			CpuSysDeltaMs:         sysDeltaMs,
			IoReadDeltaBytes:      ioReadDelta,
			IoWriteDeltaBytes:     ioWriteDelta,
			IoReadBytes:           p1.ioRead,
			IoWriteBytes:          p1.ioWrite,
			VoluntaryCtxtDelta:    volDelta,
			NonvoluntaryCtxtDelta: nonvolDelta,
			MinorFaultsDelta:      minFltDelta,
			MajorFaultsDelta:      majFltDelta,
			Wchan:                 p1.wchan,
			Threads:               threads,
			CapEff:                p1.status.CapEff,
			Seccomp:               p1.status.Seccomp,
			MemoryMap:             memMap,
			Limits:                limits,
		})
	}

	// Build cgroup v2 delta summary.
	var sampledCG *pb.SampledCgroup
	if cgT1.stats != nil {
		sampledCG = &pb.SampledCgroup{
			End:             cgT1.stats,
			MemoryHighBytes: cgT1.memHigh,
			MemoryLowBytes:  cgT1.memLow,
			Frozen:          cgT1.frozen,
		}
		if cgT0.stats != nil {
			usageDelta := max(int64(0), cgT1.stats.CpuUsageUsec-cgT0.stats.CpuUsageUsec)
			sampledCG.CpuCoresAvg = float64(usageDelta) / (durFloatSec * 1e6)
			sampledCG.CpuUserDeltaUsec = max(int64(0), cgT1.stats.CpuUserUsec-cgT0.stats.CpuUserUsec)
			sampledCG.CpuSystemDeltaUsec = max(int64(0), cgT1.stats.CpuSystemUsec-cgT0.stats.CpuSystemUsec)
			sampledCG.NrThrottledDelta = max(int64(0), cgT1.stats.NrThrottled-cgT0.stats.NrThrottled)
			sampledCG.ThrottledDeltaUsec = max(int64(0), cgT1.stats.ThrottledUsec-cgT0.stats.ThrottledUsec)
			sampledCG.IoReadDeltaBytes = max(int64(0), cgT1.ioReadTotal-cgT0.ioReadTotal)
			sampledCG.IoWriteDeltaBytes = max(int64(0), cgT1.ioWriteTotal-cgT0.ioWriteTotal)
			sampledCG.PgfaultDelta = max(int64(0), cgT1.stats.Pgfault-cgT0.stats.Pgfault)
			sampledCG.PgmajfaultDelta = max(int64(0), cgT1.stats.Pgmajfault-cgT0.stats.Pgmajfault)
			sampledCG.MemoryDeltaBytes = cgT1.stats.MemoryCurrentBytes - cgT0.stats.MemoryCurrentBytes
			sampledCG.PsiCpuSomeDeltaUsec = max(int64(0), cgT1.psiCPUSomeTotal-cgT0.psiCPUSomeTotal)
			sampledCG.PsiMemSomeDeltaUsec = max(int64(0), cgT1.psiMemSomeTotal-cgT0.psiMemSomeTotal)
			sampledCG.PsiMemFullDeltaUsec = max(int64(0), cgT1.psiMemFullTotal-cgT0.psiMemFullTotal)
			sampledCG.PsiIoSomeDeltaUsec = max(int64(0), cgT1.psiIOSomeTotal-cgT0.psiIOSomeTotal)
			sampledCG.PsiIoFullDeltaUsec = max(int64(0), cgT1.psiIOFullTotal-cgT0.psiIOFullTotal)
		}
		collectors = append(collectors, &pb.SampleCollector{Name: "cgroup_v2", Ok: true, Detail: target.Cgroup})
	} else {
		collectors = append(collectors, &pb.SampleCollector{Name: "cgroup_v2", Ok: false, Detail: "no cgroup v2 path"})
	}

	// Collect folded stacks from our multi-snapshot sampling.
	sampledStacks := make([]*pb.FoldedStack, 0, len(foldedCounts))
	for fk, cnt := range foldedCounts {
		sampledStacks = append(sampledStacks, &pb.FoldedStack{
			Frames:   flame.IDs(fk.frames),
			Samples:  cnt,
			LeafType: fk.leafType,
			Comm:     fk.comm,
		})
	}
	slices.SortFunc(sampledStacks, func(a, b *pb.FoldedStack) int {
		return cmp.Or(cmp.Compare(b.Samples, a.Samples), slices.Compare(a.Frames, b.Frames), cmp.Compare(a.Comm, b.Comm))
	})

	// Wait for background perf stat result aligned with pCtx's deadline.
	var perfCounters []*pb.PerfCounter
	select {
	case pr := <-perfCh:
		perfCounters = pr.counters
		collectors = append(collectors, &pb.SampleCollector{Name: "perf_stat", Ok: pr.ok, Detail: pr.detail, DurationMs: pr.durMs})
	case <-pCtx.Done():
		if ctx.Err() != nil {
			return ctx.Err()
		}
		collectors = append(collectors, &pb.SampleCollector{Name: "perf_stat", Ok: false, Detail: "timed out waiting for perf stat"})
	case <-ctx.Done():
		return ctx.Err()
	}

	// Query always-on OTel eBPF continuous profiler for this exact sample window!
	var onCPUStacks []*pb.FoldedStack
	var onCPUTopFns []*pb.ProfileFunction
	if target.ProfileWindow != nil {
		if prof := target.ProfileWindow(startedAt.UnixMilli(), endedAt.UnixMilli()); prof != nil {
			onCPUStacks = prof.Stacks
			onCPUTopFns = prof.TopFunctions
			frames.Rebase(prof.Frames, onCPUStacks, onCPUTopFns)
			detail := fmt.Sprintf("%d Hz · %d on-CPU samples in window", prof.SampleHz, prof.MatchedSamples)
			if !prof.Active && prof.StatusNote != "" {
				detail = prof.StatusNote
			}
			collectors = append(collectors, &pb.SampleCollector{Name: "otel_ebpf", Ok: prof.Active, Detail: detail})
		}
	}

	hostname := target.Hostname
	if hostname == "" {
		hostname, _ = os.Hostname()
	}
	kernel := target.Kernel
	if kernel == "" {
		if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
			kernel = strings.TrimSpace(string(b))
		}
	}

	sample := &pb.ProcessSample{
		CommandId:          target.CommandID,
		UnitName:           target.UnitName,
		Cgroup:             target.Cgroup,
		Hostname:           hostname,
		Kernel:             kernel,
		Arch:               arch,
		StartedAt:          startedAt.UnixMilli(),
		EndedAt:            endedAt.UnixMilli(),
		DurationMs:         durationMs,
		PollCount:          int32(pollCount),
		UserStackSnapshots: int32(userSnapCount),
		Collectors:         collectors,
		Procs:              sampledProcs,
		Files:              sampledFiles,
		Locks:              matchedLocks,
		CgroupStats:        sampledCG,
		SampledStacks:      sampledStacks,
		OnCpuStacks:        onCPUStacks,
		OnCpuTopFunctions:  onCPUTopFns,
		Frames:             frames.Frames(),
		PerfCounters:       perfCounters,
	}
	return emit(&pb.SampleCommandChunk{
		Status:      "Complete",
		ProgressPct: 100,
		Sample:      sample,
		Done:        true,
	})
}

// buildCombinedPath constructs a root->leaf call path combining user-space frames,
// active syscall (if any), and kernel stack frames (reversed from leaf->root).
func buildCombinedPath(userFrames []*pb.StackFrame, sc *pb.ThreadSyscall, kernelFrames []string) []*pb.ProfileFrame {
	var path []*pb.ProfileFrame
	// User frames are leaf->root; reverse to root->leaf.
	for i := len(userFrames) - 1; i >= 0; i-- {
		fr := userFrames[i]
		path = append(path, &pb.ProfileFrame{Name: fr.Symbol, Module: fr.Module, FrameType: "native"})
	}
	if sc != nil && sc.Name != "" && sc.Name != "running" {
		sym := sc.Name + "()"
		if sc.FdTarget != "" {
			sym = fmt.Sprintf("%s(%s)", sc.Name, sc.FdTarget)
		}
		path = append(path, &pb.ProfileFrame{Name: sym, FrameType: "syscall"})
	}
	// Kernel frames from /proc/<pid>/task/<tid>/stack are leaf->root; reverse to root->leaf.
	for i := len(kernelFrames) - 1; i >= 0; i-- {
		kf := kernelFrames[i]
		if plus := strings.Index(kf, "+0x"); plus > 0 {
			kf = kf[:plus]
		}
		path = append(path, &pb.ProfileFrame{Name: kf, FrameType: "kernel"})
	}
	return path
}

// hasDStateThread reports whether the process leader or any of its threads is
// in uninterruptible sleep ("D"), where ptrace attach would block.
func hasDStateThread(ps procSnap) bool {
	if ps.stat.State == "D" {
		return true
	}
	for _, ts := range ps.threads {
		if ts.state == "D" {
			return true
		}
	}
	return false
}

func captureUserStacks(ctx context.Context, rootDir, tool, bin string, pids []int, known map[int]procSnap) map[int]map[int32][]*pb.StackFrame {
	limit := min(len(pids), maxSampleStackPIDs)
	selfPID := os.Getpid()
	out := map[int]map[int32][]*pb.StackFrame{}
	for _, pid := range pids[:limit] {
		// Never ptrace ourselves or any process with a D-state (uninterruptible sleep) thread — ptrace attach blocks.
		if pid == selfPID {
			continue
		}
		if ps, ok := known[pid]; ok && hasDStateThread(ps) {
			continue
		}
		pCtx, cancel := context.WithTimeout(ctx, perPIDStackTimeout)
		raw := execStackCapture(pCtx, rootDir, tool, bin, pid, 48, true)
		cancel()
		var parsed map[int32][]*pb.StackFrame
		if tool == "eu-stack" {
			parsed = ParseEuStack(string(raw))
		} else {
			parsed = ParseGDBBacktrace(string(raw))
		}
		if len(parsed) > 0 {
			out[pid] = parsed
		}
	}
	return out
}

func readProcSnap(pid int, arch string) (procSnap, bool) {
	pb0, ok := readProcBase(pid)
	if !ok {
		return procSnap{}, false
	}
	base := fmt.Sprintf("/proc/%d", pid)
	exe, _ := os.Readlink(base + "/exe")
	cwd, _ := os.Readlink(base + "/cwd")
	list := readProcThreads(pid, threadReadOpts{arch: arch, needKStack: true, needDetails: true})
	threads := make(map[int32]threadSnap, len(list))
	for _, ts := range list {
		threads[ts.tid] = ts
	}
	return procSnap{
		procBase: pb0,
		exe:      exe,
		cwd:      cwd,
		threads:  threads,
	}, true
}

func readFDSnaps(pids []int, procs map[int]procSnap) []fdSnap {
	var out []fdSnap
	for _, pid := range pids {
		rem := maxSampleFiles - len(out)
		if rem <= 0 {
			break
		}
		out = append(out, readPIDFDs(pid, procs[pid].stat.Comm, rem)...)
	}
	return out
}

func readProcLocks() []RawFileLock {
	b, err := os.ReadFile("/proc/locks")
	if err != nil {
		return nil
	}
	return ParseProcLocks(string(b))
}
