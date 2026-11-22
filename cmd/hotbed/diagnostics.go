package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/semistrict/hotbed/internal/api"
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
	session, err := c.sessionFlag.resolve(ctx, d)
	if err != nil {
		return nil, err
	}
	_, w, err := c.locateCommand(ctx, d, session, c.Command, true)
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
	Format   string `enum:"json,folded,report" default:"json" help:"Output format: json, folded (flamegraph stacks), or report (perf report --stdio)."`
}

func (c *tracePerfCmd) Raw(ctx context.Context, d deps) (int, bool, error) {
	return runTraceStream(ctx, d, c.controllerClient, c.sessionFlag, "trace perf", &pb.TraceCommandRequest{
		Id:              c.Command,
		Kind:            pb.TraceKind_TRACE_KIND_PERF_CPU,
		DurationSeconds: c.Duration,
		Pid:             c.PID,
		FrequencyHz:     c.Freq,
	}, c.Format != "json", c.Format)
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
	session, err := sf.resolve(ctx, d)
	if err != nil {
		return 0, false, err
	}
	_, w, err := cc.locateCommand(ctx, d, session, req.Id, true)
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
				fmt.Fprintf(d.stdout, "%s %d\n", st.Stack, st.Samples)
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
	writeEnvelope(d.stdout, success(cmdName, summary))
	return exitOK, true, nil
}

// ---- hotbed profile (always-on OpenTelemetry eBPF profiler) ----------------

type profileCmd struct {
	controllerClient
	sessionFlag
	Command   string `arg:"" help:"The command's ID (live or finished)."`
	Query     string `short:"q" help:"Filter stacks/functions containing this substring (case-insensitive)."`
	FrameType string `name:"type" help:"Filter by runtime/frame type (kernel, native, go, python, v8, ruby, hotspot, ...)."`
	Comm      string `help:"Filter by process name (comm)."`
	FromMs    int64  `name:"from" help:"Start of time window (unix ms)."`
	ToMs      int64  `name:"to" help:"End of time window (unix ms)."`
	Limit     int32  `default:"200" help:"Maximum stacks / top functions to return."`
	Format    string `enum:"json,folded,top" default:"json" help:"Output format: json (envelope), folded (flamegraph format), or top (table)."`
}

func (c *profileCmd) Raw(ctx context.Context, d deps) (int, bool, error) {
	req := &pb.GetCommandProfileRequest{
		Id:        c.Command,
		Query:     c.Query,
		FrameType: c.FrameType,
		Comm:      c.Comm,
		FromMs:    c.FromMs,
		ToMs:      c.ToMs,
		Limit:     c.Limit,
	}
	// Query via controller first so finished commands work even after their
	// worker forgets them or goes offline. Fall back to known workers if the
	// controller is unreachable.
	cl, err := c.clients(d.http)
	if err != nil {
		return 0, false, err
	}
	prof, err := cl.workspaces.GetCommandProfile(ctx, req)
	if err != nil && api.FromConnect(err).Code == api.CodeControllerUnreachable {
		if session, serr := c.sessionFlag.resolve(ctx, d); serr == nil {
			if _, w, werr := c.askKnownWorkers(ctx, d, session, c.Command, err); werr == nil {
				prof, err = w.calls.GetCommandProfile(ctx, req)
			}
		}
	}
	if err != nil {
		return 0, false, err
	}
	switch c.Format {
	case "folded":
		for _, st := range prof.Stacks {
			fmt.Fprintf(d.stdout, "%s %d\n", st.Stack, st.Samples)
		}
		return exitOK, true, nil
	case "top":
		fmt.Fprintf(d.stdout, "Profiler: %s (%d Hz)  Total: %d samples  Matched: %d (user: %d, kernel: %d)\n",
			prof.Profiler, prof.SampleHz, prof.TotalSamples, prof.MatchedSamples, prof.UserSamples, prof.KernelSamples)
		if prof.StatusNote != "" {
			fmt.Fprintf(d.stdout, "Note: %s\n", prof.StatusNote)
		}
		if len(prof.TopFunctions) == 0 {
			fmt.Fprintln(d.stdout, "(no matching samples)")
			return exitOK, true, nil
		}
		fmt.Fprintf(d.stdout, "%7s %7s %8s %8s  %-8s  %s\n", "SELF%", "TOTAL%", "SELF", "TOTAL", "TYPE", "FUNCTION")
		for _, fn := range prof.TopFunctions {
			loc := fn.Name
			if fn.Module != "" && !strings.Contains(loc, fn.Module) {
				loc += " (" + fn.Module + ")"
			}
			fmt.Fprintf(d.stdout, "%6.2f%% %6.2f%% %8d %8d  %-8s  %s\n",
				fn.SelfPct, fn.TotalPct, fn.SelfSamples, fn.TotalSamples, fn.FrameType, loc)
		}
		return exitOK, true, nil
	default:
		writeEnvelope(d.stdout, success("profile", prof))
		return exitOK, true, nil
	}
}

func (c *profileCmd) Exec(context.Context, deps) (any, error) {
	return nil, errors.New("profile is raw")
}
