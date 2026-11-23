package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/flame"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// ---- hotbed inspect --------------------------------------------------------

type inspectCmd struct {
	All    inspectAllCmd    `cmd:"" default:"withargs" help:"Full non-invasive Linux snapshot (procs, files, cgroup, kstack, maps, limits)."`
	Procs  inspectProcsCmd  `cmd:"" help:"Process tree, threads, CPU, IO, RSS, and kernel wait channels (wchan)."`
	Files  inspectFilesCmd  `cmd:"" help:"Open file descriptors, sockets (TCP/UDP/Unix), pipes, and epoll/eventfd."`
	Cgroup inspectCgroupCmd `cmd:"" help:"Cgroup v2 CPU/memory/IO stats and PSI pressure."`
	Kstack inspectKstackCmd `cmd:"" help:"Kernel stacks (/proc/<pid>/task/<tid>/stack) for all threads."`
	Maps   inspectMapsCmd   `cmd:"" help:"Memory maps (smaps_rollup RSS/PSS/swap and largest mapped regions)."`
	Limits inspectLimitsCmd `cmd:"" help:"Resource limits (rlimits), capabilities, seccomp, and context switches."`
}

type inspectCommon struct {
	controllerClient
	sessionFlag
	Command string `arg:"" help:"The running command's ID."`
	PID     int32  `help:"Restrict detailed output to one PID in the command's cgroup."`
}

func (c inspectCommon) run(ctx context.Context, d deps, sections ...pb.InspectSection) (any, error) {
	session, err := c.sessionFlag.optional(ctx, d)
	if err != nil {
		return nil, err
	}
	_, w, err := c.findCommand(ctx, d, session, c.Command)
	if err != nil {
		return nil, err
	}
	return w.calls.InspectCommand(ctx, &pb.InspectCommandRequest{
		Id:       c.Command,
		Sections: sections,
		Pid:      c.PID,
	})
}

type inspectAllCmd struct{ inspectCommon }
type inspectProcsCmd struct{ inspectCommon }
type inspectFilesCmd struct{ inspectCommon }
type inspectCgroupCmd struct{ inspectCommon }
type inspectKstackCmd struct{ inspectCommon }
type inspectMapsCmd struct{ inspectCommon }
type inspectLimitsCmd struct{ inspectCommon }

func (c *inspectAllCmd) Exec(ctx context.Context, d deps) (any, error) { return c.run(ctx, d) }
func (c *inspectProcsCmd) Exec(ctx context.Context, d deps) (any, error) {
	return c.run(ctx, d, pb.InspectSection_INSPECT_SECTION_PROCS)
}
func (c *inspectFilesCmd) Exec(ctx context.Context, d deps) (any, error) {
	return c.run(ctx, d, pb.InspectSection_INSPECT_SECTION_FILES)
}
func (c *inspectCgroupCmd) Exec(ctx context.Context, d deps) (any, error) {
	return c.run(ctx, d, pb.InspectSection_INSPECT_SECTION_CGROUP)
}
func (c *inspectKstackCmd) Exec(ctx context.Context, d deps) (any, error) {
	return c.run(ctx, d, pb.InspectSection_INSPECT_SECTION_KSTACK)
}
func (c *inspectMapsCmd) Exec(ctx context.Context, d deps) (any, error) {
	return c.run(ctx, d, pb.InspectSection_INSPECT_SECTION_MAPS)
}
func (c *inspectLimitsCmd) Exec(ctx context.Context, d deps) (any, error) {
	return c.run(ctx, d, pb.InspectSection_INSPECT_SECTION_LIMITS)
}

// ---- hotbed trace ----------------------------------------------------------

type traceCmd struct {
	Strace traceStraceCmd `cmd:"" help:"Opt-in ptrace syscall trace (strace -f -T -yy, or -c with --summary)."`
	Perf   tracePerfCmd   `cmd:"" help:"Opt-in CPU call-graph profile via perf record -g (DWARF) + perf report."`
	Stat   traceStatCmd   `cmd:"" help:"Opt-in hardware/software performance counters via perf stat."`
	Stacks traceStacksCmd `cmd:"" help:"Opt-in user-space + kernel-space stack trace dump of all threads (eu-stack/gdb)."`
	BPF    traceBPFCmd    `cmd:"" help:"Opt-in cgroup-scoped eBPF probe via bpftrace (execsnoop, opensnoop, vfsstat, offcpu, net)."`
}

type traceStraceCmd struct {
	controllerClient
	sessionFlag
	Command  string `arg:"" help:"The running command's ID."`
	Duration int32  `short:"d" default:"5" help:"Seconds to trace (1-60)."`
	Summary  bool   `short:"c" help:"Return a syscall count/latency summary table instead of streaming every syscall."`
	Filter   string `short:"e" help:"Syscall filter expression (e.g. file, network, process, openat,read,write)."`
	PID      int32  `help:"Trace only this PID inside the command's cgroup."`
	RawOut   bool   `name:"raw" help:"With --summary, print raw strace -c text instead of JSON."`
}

func (c *traceStraceCmd) Raw(ctx context.Context, d deps) (int, bool, error) {
	kind := pb.TraceKind_TRACE_KIND_STRACE
	if c.Summary {
		kind = pb.TraceKind_TRACE_KIND_STRACE_SUMMARY
	}
	return runTraceStream(ctx, d, c.controllerClient, c.sessionFlag, "trace strace", &pb.TraceCommandRequest{
		Id:              c.Command,
		Kind:            kind,
		DurationSeconds: c.Duration,
		Pid:             c.PID,
		Filter:          c.Filter,
	}, !c.Summary || c.RawOut, "")
}

func (c *traceStraceCmd) Exec(context.Context, deps) (any, error) {
	return nil, errors.New("trace strace is raw")
}

type tracePerfCmd struct {
	controllerClient
	sessionFlag
	Command  string `arg:"" help:"The running command's ID."`
	Duration int32  `short:"d" default:"5" help:"Seconds to record (1-60)."`
	Freq     int32  `short:"F" default:"99" help:"Sampling frequency in Hz (10-999)."`
	PID      int32  `help:"Profile only this PID inside the command's cgroup."`
	Format   string `enum:",json,folded,report" default:"" help:"Output format: json, folded (flamegraph stacks), or report (perf report --stdio)."`
}

func (c *tracePerfCmd) Raw(ctx context.Context, d deps) (int, bool, error) {
	return runTraceStream(ctx, d, c.controllerClient, c.sessionFlag, "trace perf", &pb.TraceCommandRequest{
		Id:              c.Command,
		Kind:            pb.TraceKind_TRACE_KIND_PERF_CPU,
		DurationSeconds: c.Duration,
		Pid:             c.PID,
		FrequencyHz:     c.Freq,
	}, c.Format != "" && c.Format != "json", c.Format)
}

func (c *tracePerfCmd) Exec(context.Context, deps) (any, error) {
	return nil, errors.New("trace perf is raw")
}

type traceStatCmd struct {
	controllerClient
	sessionFlag
	Command  string `arg:"" help:"The running command's ID."`
	Duration int32  `short:"d" default:"5" help:"Seconds to measure (1-60)."`
	PID      int32  `help:"Measure only this PID inside the command's cgroup."`
	RawOut   bool   `name:"raw" help:"Print raw perf stat output instead of JSON."`
}

func (c *traceStatCmd) Raw(ctx context.Context, d deps) (int, bool, error) {
	return runTraceStream(ctx, d, c.controllerClient, c.sessionFlag, "trace stat", &pb.TraceCommandRequest{
		Id:              c.Command,
		Kind:            pb.TraceKind_TRACE_KIND_PERF_STAT,
		DurationSeconds: c.Duration,
		Pid:             c.PID,
	}, c.RawOut, "report")
}

func (c *traceStatCmd) Exec(context.Context, deps) (any, error) {
	return nil, errors.New("trace stat is raw")
}

type traceStacksCmd struct {
	controllerClient
	sessionFlag
	Command string `arg:"" help:"The running command's ID."`
	PID     int32  `help:"Dump stacks only for this PID inside the command's cgroup."`
	RawOut  bool   `name:"raw" help:"Print raw thread backtraces instead of JSON."`
}

func (c *traceStacksCmd) Raw(ctx context.Context, d deps) (int, bool, error) {
	return runTraceStream(ctx, d, c.controllerClient, c.sessionFlag, "trace stacks", &pb.TraceCommandRequest{
		Id:   c.Command,
		Kind: pb.TraceKind_TRACE_KIND_STACKS,
		Pid:  c.PID,
	}, c.RawOut, "report")
}

func (c *traceStacksCmd) Exec(context.Context, deps) (any, error) {
	return nil, errors.New("trace stacks is raw")
}

type traceBPFCmd struct {
	controllerClient
	sessionFlag
	Command  string `arg:"" help:"The running command's ID."`
	Probe    string `short:"p" enum:"execsnoop,opensnoop,vfsstat,offcpu,net" default:"execsnoop" help:"Curated eBPF probe: execsnoop, opensnoop, vfsstat, offcpu, or net."`
	Duration int32  `short:"d" default:"5" help:"Seconds to trace (1-60)."`
	PID      int32  `help:"Optional PID filter inside the command's cgroup."`
	RawOut   bool   `name:"raw" help:"Print raw bpftrace output instead of JSON (always streaming for execsnoop/opensnoop)."`
}

func (c *traceBPFCmd) Raw(ctx context.Context, d deps) (int, bool, error) {
	var kind pb.TraceKind
	streaming := false
	switch c.Probe {
	case "execsnoop":
		kind, streaming = pb.TraceKind_TRACE_KIND_BPF_EXECSNOOP, true
	case "opensnoop":
		kind, streaming = pb.TraceKind_TRACE_KIND_BPF_OPENSNOOP, true
	case "vfsstat":
		kind = pb.TraceKind_TRACE_KIND_BPF_VFSSTAT
	case "offcpu":
		kind = pb.TraceKind_TRACE_KIND_BPF_OFFCPU
	case "net":
		kind = pb.TraceKind_TRACE_KIND_BPF_NET
	}
	return runTraceStream(ctx, d, c.controllerClient, c.sessionFlag, "trace bpf", &pb.TraceCommandRequest{
		Id:              c.Command,
		Kind:            kind,
		DurationSeconds: c.Duration,
		Pid:             c.PID,
	}, streaming || c.RawOut, "report")
}

func (c *traceBPFCmd) Exec(context.Context, deps) (any, error) {
	return nil, errors.New("trace bpf is raw")
}

func runTraceStream(
	ctx context.Context,
	d deps,
	cc controllerClient,
	sf sessionFlag,
	cmdName string,
	req *pb.TraceCommandRequest,
	rawOutput bool,
	format string,
) (int, bool, error) {
	session, err := sf.optional(ctx, d)
	if err != nil {
		return 0, false, err
	}
	_, w, err := cc.findCommand(ctx, d, session, req.Id)
	if err != nil {
		return 0, false, err
	}
	stream, err := w.stream.TraceCommand(ctx, req)
	if err != nil {
		return 0, false, err
	}
	defer stream.Close()

	var summary *pb.TraceSummary
	printed := false
	for {
		chunk, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				return 130, printed, nil
			}
			return 0, printed, err
		}
		if chunk.Status != "" {
			fmt.Fprintf(d.stderr, "hotbed: %s\n", chunk.Status)
		}
		if chunk.Text != "" && rawOutput {
			_, _ = io.WriteString(d.stdout, chunk.Text)
			printed = true
		}
		if chunk.Summary != nil {
			summary = chunk.Summary
		}
	}
	if summary == nil {
		return exitOK, printed, nil
	}
	if rawOutput {
		switch format {
		case "folded":
			for _, st := range summary.Stacks {
				fmt.Fprintf(d.stdout, "%s %d\n", safeFoldStack(summary.Frames, st), st.Samples)
			}
			printed = true
		default:
			if summary.Raw != "" && !printed {
				_, _ = io.WriteString(d.stdout, summary.Raw)
				printed = true
			}
		}
		return exitOK, printed, nil
	}
	out := d.output
	if format == "json" {
		out.JSON = true
	}
	if err := writeOutput(d.stdout, cmdName, nil, summary, out); err != nil {
		return 0, false, err
	}
	return exitOK, true, nil
}

// ---- hotbed sample (Activity Monitor-style Linux process sampler) ----------

type sampleCmd struct {
	controllerClient
	sessionFlag
	Command  string `arg:"" help:"The running command's ID."`
	Duration int32  `short:"d" default:"3" help:"Sampling window in seconds (1-30)."`
	PID      int32  `help:"Sample only this PID inside the command's cgroup."`
	Text     bool   `short:"t" help:"Print the human-readable Activity Monitor-style text report instead of JSON."`
}

func (c *sampleCmd) Raw(ctx context.Context, d deps) (int, bool, error) {
	session, err := c.sessionFlag.optional(ctx, d)
	if err != nil {
		return 0, false, err
	}
	_, w, err := c.findCommand(ctx, d, session, c.Command)
	if err != nil {
		return 0, false, err
	}
	stream, err := w.stream.SampleCommand(ctx, &pb.SampleCommandRequest{
		Id:              c.Command,
		DurationSeconds: c.Duration,
		Pid:             c.PID,
	})
	if err != nil {
		return 0, false, err
	}
	defer stream.Close()

	var sample *pb.ProcessSample
	for {
		chunk, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				return 130, false, nil
			}
			return 0, false, err
		}
		if chunk.Status != "" && !chunk.Done {
			fmt.Fprintf(d.stderr, "hotbed: %s\n", chunk.Status)
		}
		if chunk.Sample != nil {
			sample = chunk.Sample
		}
	}
	if sample == nil {
		return exitOK, false, nil
	}
	if c.Text || !d.output.machine() {
		report := formatSampleTextReport(sample)
		_, _ = io.WriteString(d.stdout, report)
		if !strings.HasSuffix(report, "\n") {
			_, _ = io.WriteString(d.stdout, "\n")
		}
		return exitOK, true, nil
	}
	if err := writeOutput(d.stdout, "sample", c, sample, d.output); err != nil {
		return 0, false, err
	}
	return exitOK, true, nil
}

func (c *sampleCmd) Exec(context.Context, deps) (any, error) {
	return nil, errors.New("sample is raw")
}

func sampleNonEmpty(a, b string) string {
	if a != "" && a != "0" {
		return a
	}
	return b
}

func formatSampleBytes(b int64) string {
	neg := b < 0
	if neg {
		b = -b
	}
	var out string
	switch {
	case b >= 1<<30:
		out = fmt.Sprintf("%.2f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		out = fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		out = fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	default:
		out = fmt.Sprintf("%d B", b)
	}
	if neg {
		return "-" + out
	}
	return out
}

// safeFoldStack formats st using frames, guarding against out-of-bounds or nil
// frame references from malformed or version-skewed responses.
func safeFoldStack(frames []*pb.ProfileFrame, st *pb.FoldedStack) string {
	if st == nil {
		return ""
	}
	n := uint32(len(frames))
	for _, id := range st.Frames {
		if id >= n || frames[id] == nil {
			var b strings.Builder
			for i, fid := range st.Frames {
				if i > 0 {
					b.WriteByte(';')
				}
				if fid < n && frames[fid] != nil {
					b.WriteString(flame.Label(frames[fid]))
				} else {
					fmt.Fprintf(&b, "[unknown#%d]", fid)
				}
			}
			return b.String()
		}
	}
	return flame.Fold(frames, st)
}

// formatSampleTextReport renders a ProcessSample as a human-readable plain-text
// report on the CLI side (never shipped inside RPC responses).
func formatSampleTextReport(s *pb.ProcessSample) string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Sampling command %s (%s)\n", s.CommandId, sampleNonEmpty(s.UnitName, s.Cgroup))
	fmt.Fprintf(&b, "Host:           %s (%s, Linux %s)\n", sampleNonEmpty(s.Hostname, "worker"), sampleNonEmpty(s.Arch, "linux"), sampleNonEmpty(s.Kernel, "unknown"))
	fmt.Fprintf(&b, "Cgroup:         %s\n", sampleNonEmpty(s.Cgroup, "—"))
	fmt.Fprintf(&b, "Date/Time:      %s → %s (%.2fs, %d /proc polls, %d user stack snapshots)\n",
		time.UnixMilli(s.StartedAt).UTC().Format(time.RFC3339),
		time.UnixMilli(s.EndedAt).UTC().Format(time.RFC3339),
		float64(s.DurationMs)/1000.0, s.PollCount, s.UserStackSnapshots)

	for _, p := range s.Procs {
		fmt.Fprintf(&b, "\n=== Process %d: %s [%s] ===\n", p.Pid, p.Comm, p.State)
		if p.Cmdline != "" {
			fmt.Fprintf(&b, "Cmdline: %s\n", p.Cmdline)
		}
		if p.Exe != "" || p.Cwd != "" {
			fmt.Fprintf(&b, "Exe: %s   Cwd: %s\n", sampleNonEmpty(p.Exe, "—"), sampleNonEmpty(p.Cwd, "—"))
		}
		fmt.Fprintf(&b, "CPU: %.1f%% (user +%.1f ms, sys +%.1f ms) | Mem: RSS %s, PSS %s, Swap %s | IO: +%s R / +%s W | CtxSw: +%d vol / +%d invol | FDs: %d\n",
			p.CpuPct, p.CpuUserDeltaMs, p.CpuSysDeltaMs,
			formatSampleBytes(p.RssBytes), formatSampleBytes(p.PssBytes), formatSampleBytes(p.SwapBytes),
			formatSampleBytes(p.IoReadDeltaBytes), formatSampleBytes(p.IoWriteDeltaBytes),
			p.VoluntaryCtxtDelta, p.NonvoluntaryCtxtDelta, p.OpenFdsCount)

		if len(p.Threads) > 0 {
			fmt.Fprintf(&b, "\nThreads (%d):\n", len(p.Threads))
			for _, t := range p.Threads {
				tag := ""
				if t.Stationary {
					tag = " [STATIONARY]"
				}
				scStr := ""
				if t.Syscall != nil && t.Syscall.Name != "" {
					scStr = " syscall=" + t.Syscall.Name
					if t.Syscall.FdTarget != "" {
						scStr += "(" + t.Syscall.FdTarget + ")"
					}
				}
				fmt.Fprintf(&b, "  TID %d (%s) [%s]%s wchan=%s%s | cpu +%.1fms | rq-wait +%.1fms | ctx +%d/%d\n",
					t.Tid, t.Name, t.State, tag, sampleNonEmpty(t.Wchan, "—"), scStr,
					t.CpuUserDeltaMs+t.CpuSysDeltaMs, t.SchedWaitDeltaMs, t.VoluntaryCtxtDelta, t.NonvoluntaryCtxtDelta)
				for i, fr := range t.UserFrames {
					mod := ""
					if fr.Module != "" {
						mod = " (" + fr.Module + ")"
					}
					src := ""
					if fr.Source != "" {
						src = " at " + fr.Source
					}
					fmt.Fprintf(&b, "    #%d %s%s%s\n", i, fr.Symbol, mod, src)
				}
				if len(t.KernelFrames) > 0 {
					b.WriteString("    --- kernel stack ---\n")
					for _, kf := range t.KernelFrames {
						fmt.Fprintf(&b, "    [k] %s\n", kf)
					}
				}
			}
		}
	}

	if len(s.SampledStacks) > 0 {
		type commGroup struct {
			comm   string
			total  int64
			stacks []*pb.FoldedStack
		}
		groupsByComm := make(map[string]*commGroup)
		var groups []*commGroup
		for _, st := range s.SampledStacks {
			if st == nil {
				continue
			}
			comm := st.Comm
			if comm == "" {
				comm = "(unknown)"
			}
			g := groupsByComm[comm]
			if g == nil {
				g = &commGroup{comm: comm}
				groupsByComm[comm] = g
				groups = append(groups, g)
			}
			g.total += st.Samples
			g.stacks = append(g.stacks, st)
		}
		sort.SliceStable(groups, func(i, j int) bool {
			if groups[i].total != groups[j].total {
				return groups[i].total > groups[j].total
			}
			return groups[i].comm < groups[j].comm
		})
		fmt.Fprintf(&b, "\n=== Sampled Stacks (%d) ===\n", len(s.SampledStacks))
		for _, g := range groups {
			sort.SliceStable(g.stacks, func(i, j int) bool {
				return g.stacks[i].Samples > g.stacks[j].Samples
			})
			fmt.Fprintf(&b, "Process %s (%d samples, %d unique stacks):\n", g.comm, g.total, len(g.stacks))
			for _, st := range g.stacks[:min(len(g.stacks), 15)] {
				pct := 0.0
				if g.total > 0 {
					pct = 100.0 * float64(st.Samples) / float64(g.total)
				}
				fmt.Fprintf(&b, "  %4d (%5.1f%%)  %s\n", st.Samples, pct, safeFoldStack(s.Frames, st))
			}
		}
	}

	if len(s.Files) > 0 {
		fmt.Fprintf(&b, "\n=== Open Files & Sockets (%d) ===\n", len(s.Files))
		for _, f := range s.Files {
			extra := ""
			if f.SocketProto != "" {
				extra = fmt.Sprintf(" %s %s %s->%s", f.SocketProto, f.SocketState, f.LocalAddr, f.RemoteAddr)
				if f.TxQueueBytes > 0 || f.RxQueueBytes > 0 {
					extra += fmt.Sprintf(" [tx_q=%s rx_q=%s]", formatSampleBytes(f.TxQueueBytes), formatSampleBytes(f.RxQueueBytes))
				}
			} else if f.Peer != "" {
				extra = " <-> " + f.Peer
			} else if len(f.EpollTfds) > 0 {
				tfds := make([]string, len(f.EpollTfds))
				for i, fd := range f.EpollTfds {
					tfds[i] = strconv.Itoa(int(fd))
				}
				extra = " watching fds [" + strings.Join(tfds, ",") + "]"
			} else if f.Pos > 0 || f.PosDelta != 0 {
				extra = fmt.Sprintf(" pos=%s", formatSampleBytes(f.Pos))
				if f.PosDelta > 0 {
					extra += fmt.Sprintf(" (+%s)", formatSampleBytes(f.PosDelta))
				} else if f.PosDelta < 0 {
					extra += fmt.Sprintf(" (%s)", formatSampleBytes(f.PosDelta))
				}
			}
			if f.LockInfo != "" {
				extra += " LOCK:" + f.LockInfo
			}
			fmt.Fprintf(&b, "  PID %-7d fd %-4d %-10s %-2s %s%s\n", f.Pid, f.Fd, f.Kind, sampleNonEmpty(f.Mode, "-"), f.Target, extra)
		}
	}

	if len(s.Locks) > 0 {
		fmt.Fprintf(&b, "\n=== File Locks (%d) ===\n", len(s.Locks))
		for _, lk := range s.Locks {
			wait := ""
			if lk.Blocked {
				wait = " [BLOCKED ->]"
			}
			target := ""
			if lk.Target != "" {
				target = " " + lk.Target
			}
			fmt.Fprintf(&b, "  PID %-7d (%s) %s %s %s inode=%d range=%s%s%s\n",
				lk.Pid, lk.Comm, lk.Kind, lk.Mode, lk.Access, lk.Inode, lk.Range, target, wait)
		}
	}

	if s.CgroupStats != nil && s.CgroupStats.End != nil {
		cg := s.CgroupStats
		b.WriteString("\n=== Cgroup v2 & PSI Deltas ===\n")
		fmt.Fprintf(&b, "  CPU avg: %.2f cores (user +%.1f ms, sys +%.1f ms) | Throttled: +%d (+%.1f ms)\n",
			cg.CpuCoresAvg, float64(cg.CpuUserDeltaUsec)/1000.0, float64(cg.CpuSystemDeltaUsec)/1000.0,
			cg.NrThrottledDelta, float64(cg.ThrottledDeltaUsec)/1000.0)
		fmt.Fprintf(&b, "  Memory: %s (delta %s, peak %s, swap %s) | Faults: +%d minor / +%d major\n",
			formatSampleBytes(cg.End.MemoryCurrentBytes), formatSampleBytes(cg.MemoryDeltaBytes), formatSampleBytes(cg.End.MemoryPeakBytes), formatSampleBytes(cg.End.MemorySwapBytes),
			cg.PgfaultDelta, cg.PgmajfaultDelta)
		fmt.Fprintf(&b, "  PSI stalls in window: CPU some +%.1f ms | Mem some +%.1f ms / full +%.1f ms | IO some +%.1f ms / full +%.1f ms\n",
			float64(cg.PsiCpuSomeDeltaUsec)/1000.0,
			float64(cg.PsiMemSomeDeltaUsec)/1000.0, float64(cg.PsiMemFullDeltaUsec)/1000.0,
			float64(cg.PsiIoSomeDeltaUsec)/1000.0, float64(cg.PsiIoFullDeltaUsec)/1000.0)
	}

	if len(s.PerfCounters) > 0 {
		b.WriteString("\n=== Hardware & Software PMU Counters (perf stat) ===\n")
		for _, c := range s.PerfCounters {
			unit := ""
			if c.Unit != "" {
				unit = " " + c.Unit
			}
			note := ""
			if c.Note != "" {
				note = "  # " + c.Note
			}
			fmt.Fprintf(&b, "  %-22s %16s%s%s\n", c.Name, c.Value, unit, note)
		}
	}

	if len(s.OnCpuTopFunctions) > 0 {
		b.WriteString("\n=== On-CPU Hot Functions (OTel eBPF) ===\n")
		for _, fn := range s.OnCpuTopFunctions[:min(len(s.OnCpuTopFunctions), 15)] {
			frameType := "unknown"
			name := fmt.Sprintf("[unknown#%d]", fn.Frame)
			if uint64(fn.Frame) < uint64(len(s.Frames)) && s.Frames[fn.Frame] != nil {
				f := s.Frames[fn.Frame]
				frameType = f.FrameType
				name = f.Name
			}
			fmt.Fprintf(&b, "  %6.1f%% self  %6.1f%% total  [%-6s] %s\n", fn.SelfPct, fn.TotalPct, frameType, name)
		}
	}

	return b.String()
}

func interventionKindWord(k pb.InterventionKind) string {
	return enumStateWord(k.String(), "INTERVENTION_KIND_")
}

// formatCommandDiagnosticsReport renders saved CommandDiagnostics (oldest
// first) as plain text: intervention metadata, activity sample text report,
// and inspection summary.
func formatCommandDiagnosticsReport(res *pb.GetCommandDiagnosticsResponse) string {
	if res == nil || len(res.Diagnostics) == 0 {
		return "(no diagnostics)\n"
	}
	var b strings.Builder
	wrote := false
	for _, d := range res.Diagnostics {
		if d == nil {
			continue
		}
		if wrote {
			b.WriteByte('\n')
		}
		wrote = true
		if iv := d.Intervention; iv != nil {
			fmt.Fprintf(&b, "== %d: %s by %s", iv.Seq, interventionKindWord(iv.Kind), api.Who(iv.By))
			if at := iv.GetBy().GetAt(); at > 0 {
				fmt.Fprintf(&b, " (%s)", time.UnixMilli(at).UTC().Format(time.RFC3339))
			}
			if iv.Comment != "" {
				fmt.Fprintf(&b, ": %s", iv.Comment)
			}
			b.WriteByte('\n')
			if iv.DiagnosticsError != "" {
				fmt.Fprintf(&b, "not all taken: %s\n", iv.DiagnosticsError)
			}
		}
		if d.Sample != nil {
			report := formatSampleTextReport(d.Sample)
			b.WriteString(report)
			if !strings.HasSuffix(report, "\n") {
				b.WriteByte('\n')
			}
		}
		if d.Inspect != nil {
			report := formatInspectTextReport(d.Inspect)
			if report != "" {
				if d.Sample != nil {
					b.WriteByte('\n')
				}
				b.WriteString(report)
				if !strings.HasSuffix(report, "\n") {
					b.WriteByte('\n')
				}
			}
		}
	}
	if !wrote {
		return "(no diagnostics)\n"
	}
	if res.HasMore {
		cmdID := ""
		for _, d := range res.Diagnostics {
			if d != nil && d.CommandId != "" {
				cmdID = d.CommandId
				break
			}
		}
		if cmdID != "" {
			fmt.Fprintf(&b, "\n(more diagnostics available: hotbed diagnostics %s --after-seq %d)\n", cmdID, res.NextAfterSeq)
		} else {
			fmt.Fprintf(&b, "\n(more diagnostics available: pass --after-seq %d)\n", res.NextAfterSeq)
		}
	}
	return b.String()
}

// formatInspectTextReport renders an InspectCommandResponse as a concise
// plain-text report alongside a ProcessSample in hotbed diagnostics.
func formatInspectTextReport(ins *pb.InspectCommandResponse) string {
	if ins == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "=== Inspection (%s) ===\n", sampleNonEmpty(ins.UnitName, sampleNonEmpty(ins.Cgroup, ins.CommandId)))
	if ins.CollectedAt > 0 {
		fmt.Fprintf(&b, "Collected: %s\n", time.UnixMilli(ins.CollectedAt).UTC().Format(time.RFC3339))
	}
	if len(ins.Procs) > 0 {
		fmt.Fprintf(&b, "\nProcesses (%d):\n", len(ins.Procs))
		for _, p := range ins.Procs {
			fmt.Fprintf(&b, "  PID %-7d PPID %-7d [%s] %s  threads=%d  RSS=%s  wchan=%s",
				p.Pid, p.Ppid, sampleNonEmpty(p.State, "?"), p.Comm, p.ThreadsCount,
				formatSampleBytes(p.RssBytes), sampleNonEmpty(p.Wchan, "—"))
			if p.Cmdline != "" {
				fmt.Fprintf(&b, "  cmd=%s", p.Cmdline)
			}
			b.WriteByte('\n')
		}
	}
	if len(ins.Kstacks) > 0 {
		fmt.Fprintf(&b, "\nKernel Stacks (%d):\n", len(ins.Kstacks))
		for _, ks := range ins.Kstacks {
			name := ks.Comm
			if ks.ThreadName != "" && ks.ThreadName != ks.Comm {
				name = ks.Comm + "/" + ks.ThreadName
			}
			fmt.Fprintf(&b, "  PID %d TID %d (%s) [%s] wchan=%s\n",
				ks.Pid, ks.Tid, name, sampleNonEmpty(ks.State, "?"), sampleNonEmpty(ks.Wchan, "—"))
			for _, fr := range ks.Frames {
				fmt.Fprintf(&b, "    [k] %s\n", fr)
			}
		}
	}
	if len(ins.Files) > 0 {
		fmt.Fprintf(&b, "\nOpen Files (%d):\n", len(ins.Files))
		for _, f := range ins.Files {
			extra := ""
			if f.SocketProto != "" {
				extra = fmt.Sprintf(" %s %s %s->%s", f.SocketProto, f.SocketState, f.LocalAddr, f.RemoteAddr)
			}
			fmt.Fprintf(&b, "  PID %-7d fd %-4d %-10s %-2s %s%s\n",
				f.Pid, f.Fd, f.Kind, sampleNonEmpty(f.Mode, "-"), f.Target, extra)
		}
	}
	if cg := ins.CgroupStats; cg != nil {
		b.WriteString("\nCgroup Stats:\n")
		fmt.Fprintf(&b, "  CPU: %.1f ms (user %.1f ms, sys %.1f ms) | Throttled: %d (%.1f ms)\n",
			float64(cg.CpuUsageUsec)/1000.0, float64(cg.CpuUserUsec)/1000.0, float64(cg.CpuSystemUsec)/1000.0,
			cg.NrThrottled, float64(cg.ThrottledUsec)/1000.0)
		fmt.Fprintf(&b, "  Memory: %s (peak %s, anon %s, file %s, swap %s) | PIDs: %d (peak %d)\n",
			formatSampleBytes(cg.MemoryCurrentBytes), formatSampleBytes(cg.MemoryPeakBytes),
			formatSampleBytes(cg.MemoryAnonBytes), formatSampleBytes(cg.MemoryFileBytes),
			formatSampleBytes(cg.MemorySwapBytes), cg.PidsCurrent, cg.PidsPeak)
		fmt.Fprintf(&b, "  PSI avg10: CPU some=%.2f%% full=%.2f%% | Mem some=%.2f%% full=%.2f%% | IO some=%.2f%% full=%.2f%%\n",
			cg.PsiCpuSomeAvg10, cg.PsiCpuFullAvg10,
			cg.PsiMemSomeAvg10, cg.PsiMemFullAvg10,
			cg.PsiIoSomeAvg10, cg.PsiIoFullAvg10)
	}
	if len(ins.Maps) > 0 {
		fmt.Fprintf(&b, "\nMemory Maps (%d):\n", len(ins.Maps))
		for _, m := range ins.Maps {
			fmt.Fprintf(&b, "  PID %-7d (%s) RSS=%s PSS=%s anon=%s swap=%s\n",
				m.Pid, m.Comm, formatSampleBytes(m.RssBytes), formatSampleBytes(m.PssBytes),
				formatSampleBytes(m.AnonBytes), formatSampleBytes(m.SwapBytes))
			for _, r := range m.TopRegions[:min(len(m.TopRegions), 5)] {
				fmt.Fprintf(&b, "    %-4s size=%-10s rss=%-10s pss=%-10s %s\n",
					r.Perms, formatSampleBytes(r.SizeBytes), formatSampleBytes(r.RssBytes),
					formatSampleBytes(r.PssBytes), sampleNonEmpty(r.Path, "[anon]"))
			}
		}
	}
	if len(ins.Limits) > 0 {
		fmt.Fprintf(&b, "\nLimits (%d):\n", len(ins.Limits))
		for _, l := range ins.Limits {
			fmt.Fprintf(&b, "  PID %-7d (%s) seccomp=%s cap_eff=%s no_new_privs=%t\n",
				l.Pid, l.Comm, sampleNonEmpty(l.Seccomp, "—"), sampleNonEmpty(l.CapEff, "—"), l.NoNewPrivs)
			for _, it := range l.Items {
				u := ""
				if it.Units != "" {
					u = " " + it.Units
				}
				fmt.Fprintf(&b, "    %-28s soft=%-12s hard=%s%s\n", it.Name, it.Soft, it.Hard, u)
			}
		}
	}
	return b.String()
}

// ---- hotbed profile (always-on OpenTelemetry eBPF profiler) ----------------

type profileCmd struct {
	controllerClient
	sessionFlag
	Command   string        `arg:"" optional:"" help:"The command's ID (live or finished). Mutually exclusive with --node."`
	Node      string        `help:"Query a node's host-wide continuous profile (hostname or ID) instead of one command."`
	Last      time.Duration `help:"Only include samples from the last duration (e.g. 5m, 15m) when --from is not set."`
	Query     string        `short:"q" help:"Filter stacks/functions containing this substring (case-insensitive)."`
	FrameType string        `name:"type" help:"Filter by runtime/frame type (kernel, native, go, python, v8, ruby, hotspot, ...)."`
	Comm      string        `help:"Filter by process name (comm)."`
	FromMs    int64         `name:"from" help:"Start of time window (unix ms)."`
	ToMs      int64         `name:"to" help:"End of time window (unix ms)."`
	Limit     int32         `default:"200" help:"Maximum stacks / top functions to return."`
	Format    string        `enum:",json,folded,top" default:"" help:"Output format: json (envelope), folded (flamegraph format), or top (table)."`
}

func (c *profileCmd) Raw(ctx context.Context, d deps) (int, bool, error) {
	if c.Command == "" && c.Node == "" {
		return 0, false, api.Errorf(api.CodeInvalidArgs, "say which command or node to profile").
			WithSuggestion("pass a command ID (`hotbed profile CMD`) or a node (`hotbed profile --node HOST`)")
	}
	if c.Command != "" && c.Node != "" {
		return 0, false, api.Errorf(api.CodeInvalidArgs, "cannot pass both a command ID and --node").
			WithSuggestion("use either `hotbed profile CMD` or `hotbed profile --node HOST`")
	}
	fromMs := c.FromMs
	if fromMs == 0 && c.Last > 0 {
		fromMs = time.Now().Add(-c.Last).UnixMilli()
	}

	cl, err := c.clients(d.http)
	if err != nil {
		return 0, false, err
	}
	req := &pb.GetProfileRequest{
		Query:     c.Query,
		FrameType: c.FrameType,
		Comm:      c.Comm,
		FromMs:    fromMs,
		ToMs:      c.ToMs,
		Limit:     c.Limit,
	}
	var prof *pb.CommandProfile
	if c.Node != "" {
		req.Target = &pb.GetProfileRequest_Node{Node: c.Node}
		prof, err = cl.workspaces.GetProfile(ctx, req)
	} else {
		req.Target = &pb.GetProfileRequest_CommandId{CommandId: c.Command}
		// Query via controller first so finished commands work even after their
		// worker forgets them or goes offline. Fall back to known workers if the
		// controller is unreachable, skipping RideOut when known workers exist.
		session, serr := c.sessionFlag.optional(ctx, d)
		call := ctx
		if serr == nil && hasKnownWorkers(d, session) {
			call = api.WithoutRideOut(ctx)
		}
		prof, err = cl.workspaces.GetProfile(call, req)
		if err != nil && api.CodeOf(err) == api.CodeControllerUnreachable && serr == nil {
			if _, w, werr := c.askKnownWorkers(ctx, d, session, c.Command, err); werr == nil {
				prof, err = w.calls.GetProfile(ctx, req)
			}
		}
	}
	if err != nil {
		return 0, false, err
	}
	switch c.Format {
	case "folded":
		for _, st := range prof.Stacks {
			fmt.Fprintf(d.stdout, "%s %d\n", safeFoldStack(prof.Frames, st), st.Samples)
		}
		return exitOK, true, nil
	case "top":
		scope := ""
		if prof.Node != "" {
			scope = "  Node: " + prof.Node
		}
		fmt.Fprintf(d.stdout, "Profiler: %s (%d Hz)%s  Total: %d samples  Matched: %d (user: %d, kernel: %d)\n",
			prof.Profiler, prof.SampleHz, scope, prof.TotalSamples, prof.MatchedSamples, prof.UserSamples, prof.KernelSamples)
		if prof.StatusNote != "" {
			fmt.Fprintf(d.stdout, "Note: %s\n", prof.StatusNote)
		}
		if len(prof.TopFunctions) == 0 {
			fmt.Fprintln(d.stdout, "(no matching samples)")
			return exitOK, true, nil
		}
		fmt.Fprintf(d.stdout, "%7s %7s %8s %8s  %-8s  %s\n", "SELF%", "TOTAL%", "SELF", "TOTAL", "TYPE", "FUNCTION")
		for _, fn := range prof.TopFunctions {
			frameType := "unknown"
			loc := fmt.Sprintf("[unknown#%d]", fn.Frame)
			if uint64(fn.Frame) < uint64(len(prof.Frames)) && prof.Frames[fn.Frame] != nil {
				f := prof.Frames[fn.Frame]
				frameType = f.FrameType
				loc = f.Name
				if f.Module != "" && !strings.Contains(loc, f.Module) {
					loc += " (" + f.Module + ")"
				}
			}
			fmt.Fprintf(d.stdout, "%6.2f%% %6.2f%% %8d %8d  %-8s  %s\n",
				fn.SelfPct, fn.TotalPct, fn.SelfSamples, fn.TotalSamples, frameType, loc)
		}
		return exitOK, true, nil
	case "json":
		out := d.output
		out.JSON = true
		if err := writeOutput(d.stdout, "profile", c, prof, out); err != nil {
			return 0, false, err
		}
		return exitOK, true, nil
	default:
		if err := writeOutput(d.stdout, "profile", c, prof, d.output); err != nil {
			return 0, false, err
		}
		return exitOK, true, nil
	}
}

func (c *profileCmd) Exec(context.Context, deps) (any, error) {
	return nil, errors.New("profile is raw")
}
