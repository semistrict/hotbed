import { Fragment, useEffect, useMemo, useReducer, useRef, useState } from "react";
import {
  api,
  InspectSection,
  TraceKind,
  type CgroupStats,
  type InspectCommandResponse,
  type OpenFile,
  type ProcessInfo,
  type ProcessLimits,
  type ProcessMemoryMap,
  type ThreadKernelStack,
  type ToolCapability,
  type TraceSummary,
} from "../api";
import { bytes, hhmmss } from "../lib/format";
import { SegmentedBar } from "./charts/Bars";
import { FlameGraph } from "./FlameGraph";
import { Card, Empty } from "./ui/Card";

const INSPECT_BUTTONS: { id: InspectSection; label: string; desc: string }[] = [
  { id: InspectSection.PROCS, label: "Processes", desc: "Process tree, threads, CPU, IO, wchan" },
  { id: InspectSection.FILES, label: "Open Files & Sockets", desc: "FDs, TCP/UDP/Unix sockets, pipes" },
  { id: InspectSection.CGROUP, label: "Cgroup & PSI", desc: "cgroup v2 CPU/memory/IO & PSI pressure" },
  { id: InspectSection.KSTACK, label: "Kernel Stacks", desc: "/proc/<pid>/task/<tid>/stack" },
  { id: InspectSection.MAPS, label: "Memory Maps", desc: "smaps_rollup RSS/PSS & regions" },
  { id: InspectSection.LIMITS, label: "Limits & Caps", desc: "rlimits, capabilities, seccomp" },
];

const TRACE_BUTTONS: { kind: TraceKind; label: string; tool: string; timed: boolean; hint: string }[] = [
  { kind: TraceKind.STRACE, label: "strace (live)", tool: "strace", timed: true, hint: "Live ptrace syscall stream (-f -T -yy)" },
  { kind: TraceKind.STRACE_SUMMARY, label: "strace -c", tool: "strace", timed: true, hint: "Syscall latency & error summary table" },
  { kind: TraceKind.PERF_CPU, label: "perf CPU", tool: "perf", timed: true, hint: "99 Hz DWARF call-graph CPU profile" },
  { kind: TraceKind.PERF_STAT, label: "perf stat", tool: "perf", timed: true, hint: "Hardware & software counters (IPC, cache, faults)" },
  { kind: TraceKind.STACKS, label: "User Stacks", tool: "eu-stack", timed: false, hint: "Thread backtraces via eu-stack/gdb + kernel" },
  { kind: TraceKind.BPF_EXECSNOOP, label: "eBPF execsnoop", tool: "bpftrace", timed: true, hint: "Process execve() events in this cgroup" },
  { kind: TraceKind.BPF_OPENSNOOP, label: "eBPF opensnoop", tool: "bpftrace", timed: true, hint: "File openat() events in this cgroup" },
  { kind: TraceKind.BPF_VFSSTAT, label: "eBPF vfsstat", tool: "bpftrace", timed: true, hint: "VFS read/write/open/fsync counts" },
  { kind: TraceKind.BPF_OFFCPU, label: "eBPF offcpu", tool: "bpftrace", timed: true, hint: "Off-CPU blocking / sleep stacks" },
  { kind: TraceKind.BPF_NET, label: "eBPF net", tool: "bpftrace", timed: true, hint: "TCP connect/sendmsg/recvmsg counts" },
];

export interface ScopedInspectState {
  pid: number;
  data: InspectCommandResponse;
}

/**
 * Merge a fresh inspect response into previous state. Per-PID sections
 * (`files`, `kstacks`, `maps`, `limits`) are only preserved across calls that
 * targeted the exact same PID; switching PIDs discards stale per-PID data.
 */
export function mergeInspectResult(
  prev: ScopedInspectState | null,
  res: InspectCommandResponse,
  pid: number,
  section: InspectSection,
): ScopedInspectState {
  const samePid = prev !== null && prev.pid === pid;
  return {
    pid,
    data: {
      ...res,
      files: section === InspectSection.FILES ? res.files : samePid ? prev.data.files : [],
      cgroupStats: section === InspectSection.CGROUP ? res.cgroupStats : (res.cgroupStats ?? prev?.data.cgroupStats),
      kstacks: section === InspectSection.KSTACK ? res.kstacks : samePid ? prev.data.kstacks : [],
      maps: section === InspectSection.MAPS ? res.maps : samePid ? prev.data.maps : [],
      limits: section === InspectSection.LIMITS ? res.limits : samePid ? prev.data.limits : [],
    },
  };
}

/**
 * Strictly opt-in Linux process inspection & active tracing workbench.
 * Zero worker calls are made until the user explicitly presses a button.
 */
export function LiveDiagnosticsPanel({ id, onSamplePid }: { id: string; onSamplePid?: (pid: number) => void }) {
  const [selectedPid, setSelectedPid] = useState<number>(0);
  const [inspectTab, setInspectTab] = useState<InspectSection | null>(null);
  const [inspectState, setInspectState] = useState<ScopedInspectState | null>(null);
  const [inspectLoading, setInspectLoading] = useState(false);
  const [inspectError, setInspectError] = useState<string | null>(null);

  const [durationSec, setDurationSec] = useState<number>(5);
  const [straceFilter, setStraceFilter] = useState<string>("");
  const [activeTraceKind, setActiveTraceKind] = useState<TraceKind | null>(null);
  const [traceRunning, setTraceRunning] = useState(false);
  const [traceStatus, setTraceStatus] = useState<string>("");
  const [traceStreamText, setTraceStreamText] = useState<string>("");
  const [traceSummary, setTraceSummary] = useState<TraceSummary | null>(null);
  const [traceError, setTraceError] = useState<string | null>(null);
  const inspectAbortRef = useRef<AbortController | null>(null);
  const inspectSeqRef = useRef(0);
  const traceAbortRef = useRef<AbortController | null>(null);
  const traceSeqRef = useRef(0);

  const inspectData = inspectState?.data ?? null;

  // Abort any in-flight inspect or trace if navigating away or switching commands.
  useEffect(() => {
    return () => {
      inspectSeqRef.current++;
      inspectAbortRef.current?.abort();
      inspectAbortRef.current = null;
      traceSeqRef.current++;
      traceAbortRef.current?.abort();
      traceAbortRef.current = null;
    };
  }, [id]);

  const toolMap = useMemo(() => {
    const m = new Map<string, ToolCapability>();
    for (const t of inspectData?.availableTools ?? []) {
      m.set(t.name, t);
    }
    return m;
  }, [inspectData]);

  const resetForPid = (nextPid: number) => {
    inspectSeqRef.current++;
    inspectAbortRef.current?.abort();
    inspectAbortRef.current = null;
    setInspectLoading(false);
    setInspectError(null);
    setInspectState((prev) =>
      prev
        ? {
            pid: nextPid,
            data: { ...prev.data, files: [], kstacks: [], maps: [], limits: [] },
          }
        : null,
    );

    traceSeqRef.current++;
    traceAbortRef.current?.abort();
    traceAbortRef.current = null;
    setActiveTraceKind(null);
    setTraceRunning(false);
    setTraceStatus("");
    setTraceStreamText("");
    setTraceSummary(null);
    setTraceError(null);
  };

  const runInspect = async (section: InspectSection, pidOverride?: number) => {
    const pid = pidOverride ?? selectedPid;
    inspectAbortRef.current?.abort();
    const ac = new AbortController();
    inspectAbortRef.current = ac;
    const seq = ++inspectSeqRef.current;

    setInspectTab(section);
    setInspectLoading(true);
    setInspectError(null);
    try {
      // Always include procs alongside the requested section so the PID picker stays fresh.
      const sections: InspectSection[] = section === InspectSection.PROCS ? [InspectSection.PROCS] : [InspectSection.PROCS, section];
      const res = await api.inspectCommand(id, { sections, pid: pid || undefined }, ac.signal);
      if (seq !== inspectSeqRef.current || ac.signal.aborted) return;
      setInspectState((prev) => mergeInspectResult(prev, res, pid, section));
    } catch (e) {
      if (seq !== inspectSeqRef.current || ac.signal.aborted || (e instanceof Error && e.name === "AbortError")) return;
      setInspectError(e instanceof Error ? e.message : String(e));
    } finally {
      if (seq === inspectSeqRef.current) {
        setInspectLoading(false);
        if (inspectAbortRef.current === ac) inspectAbortRef.current = null;
      }
    }
  };

  const runTrace = async (kind: TraceKind) => {
    traceAbortRef.current?.abort();
    const ac = new AbortController();
    traceAbortRef.current = ac;
    const seq = ++traceSeqRef.current;

    setActiveTraceKind(kind);
    setTraceRunning(true);
    setTraceStatus("Starting trace...");
    setTraceStreamText("");
    setTraceSummary(null);
    setTraceError(null);

    try {
      const summary = await api.traceCommand(
        id,
        {
          kind,
          durationSeconds: durationSec,
          pid: selectedPid || undefined,
          filter: kind === TraceKind.STRACE || kind === TraceKind.STRACE_SUMMARY ? straceFilter.trim() || undefined : undefined,
        },
        (chunk) => {
          if (seq !== traceSeqRef.current || ac.signal.aborted) return;
          if (chunk.status) setTraceStatus(chunk.status);
          if (chunk.text) setTraceStreamText((prev) => (prev.length > 250_000 ? prev.slice(-200_000) + chunk.text : prev + chunk.text));
          if (chunk.summary) setTraceSummary(chunk.summary);
        },
        ac.signal,
      );
      if (seq !== traceSeqRef.current || ac.signal.aborted) return;
      if (summary) setTraceSummary(summary);
      setTraceStatus("");
    } catch (e) {
      if (seq !== traceSeqRef.current) return;
      if (ac.signal.aborted || (e instanceof Error && e.name === "AbortError")) {
        setTraceStatus("Trace cancelled.");
      } else {
        setTraceError(e instanceof Error ? e.message : String(e));
        setTraceStatus("");
      }
    } finally {
      if (seq === traceSeqRef.current) {
        setTraceRunning(false);
        if (traceAbortRef.current === ac) traceAbortRef.current = null;
      }
    }
  };

  const stopTrace = () => {
    traceAbortRef.current?.abort();
  };

  const selectPid = (nextPid: number, targetTab?: InspectSection) => {
    setSelectedPid(nextPid);
    resetForPid(nextPid);
    const tab = targetTab ?? inspectTab;
    if (tab) void runInspect(tab, nextPid);
  };

  const headerRight = (
    <div className="flex flex-wrap items-center gap-2 font-mono text-[10.5px]">
      {inspectData && inspectData.procs.length > 0 && (
        <select
          aria-label="Target PID"
          value={selectedPid}
          onChange={(e) => selectPid(Number(e.target.value))}
          className="rounded-md border border-border bg-background/80 px-2 py-1 font-mono text-[11px] text-foreground"
        >
          <option value={0}>All PIDs ({inspectData.procs.length})</option>
          {inspectData.procs.map((p) => (
            <option key={p.pid} value={p.pid}>
              PID {p.pid} ({p.comm})
            </option>
          ))}
        </select>
      )}
      <span className="rounded-full border border-border bg-muted/60 px-2.5 py-0.5 text-muted-foreground">opt-in · zero overhead until clicked</span>
    </div>
  );

  return (
    <Card title="Linux Diagnostics" sub="Inspect /proc & cgroup v2 or attach strace, perf, thread stacks, and eBPF probes on demand" right={headerRight}>
      <div className="space-y-5">
        {/* Row 1: Instant /proc & cgroup inspection buttons */}
        <div className="space-y-2">
          <div className="flex items-center justify-between">
            <span className="eyebrow">Inspect (/proc & cgroup v2 · instant, non-intrusive)</span>
            {inspectData && (
              <span className="font-mono text-[10.5px] text-faint">
                {inspectData.cgroup || inspectData.unitName} · {hhmmss(inspectData.collectedAt)}
              </span>
            )}
          </div>
          <div className="flex flex-wrap gap-1.5">
            {INSPECT_BUTTONS.map((b) => {
              const active = inspectTab === b.id;
              return (
                <button
                  key={b.id}
                  type="button"
                  title={b.desc}
                  onClick={() => void runInspect(b.id)}
                  className={`rounded-lg border px-3 py-1.5 font-mono text-[11.5px] transition-colors ${
                    active
                      ? "border-primary bg-ember-wash text-primary"
                      : "border-border bg-background/60 text-foreground hover:border-border-strong hover:bg-muted/60"
                  }`}
                >
                  {b.label}
                </button>
              );
            })}
          </div>
        </div>

        {/* Inspect output area */}
        {inspectError && (
          <div className="rounded-lg border border-critical/30 bg-[color-mix(in_oklab,var(--critical)_10%,transparent)] px-3 py-2 font-mono text-[11.5px] text-critical">
            {inspectError}
          </div>
        )}
        {inspectLoading && <div className="font-mono text-[11.5px] text-muted-foreground">Reading /proc and cgroup v2 on worker...</div>}
        {inspectTab && inspectData && !inspectLoading && (
          <div className="rounded-xl border border-border bg-background/40 p-3">
            {inspectTab === InspectSection.PROCS && (
              <ProcsView
                procs={inspectData.procs}
                onPickPid={(pid) => selectPid(pid, InspectSection.FILES)}
                onSamplePid={onSamplePid}
              />
            )}
            {inspectTab === InspectSection.FILES && <FilesView files={inspectData.files} />}
            {inspectTab === InspectSection.CGROUP && <CgroupView stats={inspectData.cgroupStats ?? null} />}
            {inspectTab === InspectSection.KSTACK && <KernelStacksView kstacks={inspectData.kstacks} />}
            {inspectTab === InspectSection.MAPS && <MapsView maps={inspectData.maps} />}
            {inspectTab === InspectSection.LIMITS && <LimitsView limits={inspectData.limits} />}
          </div>
        )}

        {/* Row 2: Active opt-in Linux tracing tools */}
        <div className="space-y-2.5 border-t border-border pt-4">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <span className="eyebrow">Active Trace (opt-in Linux tracers · time-bounded & cancellable)</span>
            <div className="flex flex-wrap items-center gap-2">
              <label className="flex items-center gap-1.5 font-mono text-[11px] text-muted-foreground">
                <span>Duration</span>
                <select
                  aria-label="Trace duration"
                  value={durationSec}
                  onChange={(e) => setDurationSec(Number(e.target.value))}
                  disabled={traceRunning}
                  className="rounded-md border border-border bg-background/80 px-2 py-0.5 text-foreground"
                >
                  {[3, 5, 10, 20, 30].map((s) => (
                    <option key={s} value={s}>
                      {s}s
                    </option>
                  ))}
                </select>
              </label>
              <input
                type="text"
                value={straceFilter}
                onChange={(e) => setStraceFilter(e.target.value)}
                disabled={traceRunning}
                placeholder="strace -e (e.g. file,network)"
                aria-label="strace syscall filter"
                className="w-[240px] rounded-md border border-border bg-background/80 px-2.5 py-0.5 font-mono text-[11px] text-foreground placeholder:text-faint focus:border-primary focus:outline-none"
              />
              {traceRunning && (
                <button
                  type="button"
                  onClick={stopTrace}
                  className="rounded-md border border-critical bg-[color-mix(in_oklab,var(--critical)_15%,transparent)] px-2.5 py-0.5 font-mono text-[11px] text-critical hover:brightness-110"
                >
                  Stop trace
                </button>
              )}
            </div>
          </div>

          <div className="flex flex-wrap gap-1.5">
            {TRACE_BUTTONS.map((b) => {
              const cap = toolMap.get(b.tool);
              const altGdb = b.tool === "eu-stack" ? toolMap.get("gdb") : undefined;
              const unavailable = cap && !cap.available && !(altGdb && altGdb.available);
              const isSelected = activeTraceKind === b.kind;
              return (
                <button
                  key={b.kind}
                  type="button"
                  disabled={traceRunning || Boolean(unavailable)}
                  title={unavailable ? `${b.tool} unavailable: ${cap?.detail ?? ""}` : b.hint}
                  onClick={() => void runTrace(b.kind)}
                  className={`rounded-lg border px-2.5 py-1.5 font-mono text-[11px] transition-colors ${
                    unavailable
                      ? "cursor-not-allowed border-border/50 bg-muted/30 text-faint line-through"
                      : isSelected
                        ? "border-primary bg-ember-wash text-primary"
                        : "border-border bg-background/60 text-foreground hover:border-primary/60 hover:text-primary"
                  }`}
                >
                  {b.label}
                </button>
              );
            })}
          </div>
        </div>

        {/* Active trace output area */}
        {(traceRunning || traceStatus || traceError || traceStreamText || traceSummary) && (
          <div className="space-y-3 rounded-xl border border-border bg-background/50 p-3">
            <div className="flex flex-wrap items-center justify-between gap-2 font-mono text-[11px]">
              <div className="flex items-center gap-2">
                {traceRunning && <span className="live-dot h-2 w-2 rounded-full bg-primary" />}
                <span className="text-foreground">{traceStatus || (traceSummary ? `Completed in ${(Number(traceSummary.durationMs) / 1000).toFixed(2)}s` : "")}</span>
              </div>
              {traceSummary?.commandLine && (
                <code className="max-w-full truncate rounded bg-muted px-2 py-0.5 text-[10.5px] text-muted-foreground" title={traceSummary.commandLine}>
                  $ {traceSummary.commandLine}
                </code>
              )}
            </div>

            {traceError && (
              <div className="rounded-lg border border-critical/30 bg-[color-mix(in_oklab,var(--critical)_10%,transparent)] px-3 py-2 font-mono text-[11.5px] text-critical">
                {traceError}
              </div>
            )}

            {/* Structured perf CPU flamegraph + top functions */}
            {traceSummary && traceSummary.stacks.length > 0 && <FlameGraph stacks={traceSummary.stacks} frames={traceSummary.frames} />}

            {/* Structured perf stat counters */}
            {traceSummary && traceSummary.counters.length > 0 && (
              <div className="grid grid-cols-2 gap-2 sm:grid-cols-3 lg:grid-cols-5">
                {traceSummary.counters.map((c) => (
                  <div key={c.name} className="rounded-lg border border-border bg-card/70 px-3 py-2">
                    <div className="font-mono text-[10px] text-muted-foreground">{c.name}</div>
                    <div className="num mt-1 font-mono text-[14px] font-semibold text-foreground">
                      {c.value}
                      {c.unit && <span className="ml-1 text-[10.5px] font-normal text-muted-foreground">{c.unit}</span>}
                    </div>
                    {c.note && <div className="mt-0.5 truncate font-mono text-[10px] text-faint" title={c.note}>{c.note}</div>}
                  </div>
                ))}
              </div>
            )}

            {/* Structured strace -c syscall table */}
            {traceSummary && traceSummary.syscalls.length > 0 && (
              <div className="max-h-[340px] overflow-auto rounded-lg border border-border">
                <table className="w-full border-collapse font-mono text-[11.5px]">
                  <thead className="sticky top-0 bg-card text-left text-[10px] uppercase tracking-wider text-muted-foreground">
                    <tr className="border-b border-border">
                      <th className="px-3 py-2">Syscall</th>
                      <th className="px-3 py-2 text-right">% Time</th>
                      <th className="px-3 py-2 text-right">Seconds</th>
                      <th className="px-3 py-2 text-right">µs/call</th>
                      <th className="px-3 py-2 text-right">Calls</th>
                      <th className="px-3 py-2 text-right">Errors</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-border">
                    {traceSummary.syscalls.map((sc) => (
                      <tr key={sc.name} className="hover:bg-muted/40">
                        <td className="px-3 py-1.5 text-foreground">{sc.name}</td>
                        <td className="num px-3 py-1.5 text-right text-primary">{sc.pctTime.toFixed(1)}%</td>
                        <td className="num px-3 py-1.5 text-right text-muted-foreground">{sc.seconds.toFixed(4)}</td>
                        <td className="num px-3 py-1.5 text-right text-muted-foreground">{String(sc.usecsPerCall)}</td>
                        <td className="num px-3 py-1.5 text-right text-foreground">{sc.calls.toLocaleString()}</td>
                        <td className={`num px-3 py-1.5 text-right ${sc.errors > 0n ? "text-critical" : "text-faint"}`}>{sc.errors > 0n ? sc.errors.toLocaleString() : "—"}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}

            {/* Streamed or raw textual output */}
            {(traceStreamText || traceSummary?.raw) && (
              <pre className="max-h-[360px] overflow-auto rounded-lg border border-border bg-background/70 p-3 font-mono text-[11px] leading-[1.6] text-foreground/85 whitespace-pre">
                {traceStreamText || traceSummary?.raw}
              </pre>
            )}
          </div>
        )}
      </div>
    </Card>
  );
}

export interface InspectReportState {
  tab: InspectSection;
  selectedPid: number | null;
}

export type InspectReportAction =
  | { type: "set_tab"; tab: InspectSection }
  | { type: "pick_pid"; pid: number }
  | { type: "clear_pid" };

export function inspectReportReducer(state: InspectReportState, action: InspectReportAction): InspectReportState {
  switch (action.type) {
    case "pick_pid":
      return { tab: InspectSection.FILES, selectedPid: action.pid };
    case "clear_pid":
      return { ...state, selectedPid: null };
    case "set_tab":
      return { ...state, tab: action.tab };
  }
}

export function InspectReportView({
  result,
  tab,
  selectedPid,
  onSelectTab,
  onPickPid,
  onClearPid,
}: {
  result: InspectCommandResponse;
  tab: InspectSection;
  selectedPid: number | null;
  onSelectTab: (tab: InspectSection) => void;
  onPickPid: (pid: number) => void;
  onClearPid: () => void;
}) {
  const meta = [result.cgroup || result.unitName, result.collectedAt > 0n ? hhmmss(result.collectedAt) : null].filter(Boolean).join(" · ");
  const selectedProc = selectedPid !== null ? result.procs.find((p) => p.pid === selectedPid) : undefined;
  const files = selectedPid !== null ? result.files.filter((f) => f.pid === selectedPid) : result.files;
  const kstacks = selectedPid !== null ? result.kstacks.filter((k) => k.pid === selectedPid) : result.kstacks;
  const maps = selectedPid !== null ? result.maps.filter((m) => m.pid === selectedPid) : result.maps;
  const limits = selectedPid !== null ? result.limits.filter((l) => l.pid === selectedPid) : result.limits;

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-1.5">
        {INSPECT_BUTTONS.map((b) => (
          <button
            key={b.id}
            type="button"
            title={b.desc}
            aria-pressed={tab === b.id}
            onClick={() => onSelectTab(b.id)}
            className={`rounded-lg border px-3 py-1 font-mono text-[11.5px] transition-colors ${
              tab === b.id
                ? "border-primary bg-ember-wash text-primary"
                : "border-border bg-background/60 text-foreground hover:border-border-strong hover:bg-muted/60"
            }`}
          >
            {b.label}
          </button>
        ))}
        {meta && <span className="ml-auto font-mono text-[10.5px] text-faint">{meta}</span>}
      </div>
      {selectedPid !== null && (
        <div className="flex flex-wrap items-center gap-2 font-mono text-[11px]">
          <span className="inline-flex items-center gap-1.5 rounded-full border border-primary/40 bg-ember-wash px-2.5 py-0.5 text-primary">
            Filtered to PID {selectedPid}
            {selectedProc?.comm ? ` (${selectedProc.comm})` : ""}
          </span>
          <button
            type="button"
            onClick={onClearPid}
            className="rounded-md border border-border px-2 py-0.5 text-[10.5px] text-muted-foreground hover:border-border-strong hover:text-foreground"
          >
            Show all PIDs
          </button>
        </div>
      )}
      <div className="rounded-xl border border-border bg-background/40 p-3">
        {tab === InspectSection.PROCS && <ProcsView procs={result.procs} onPickPid={onPickPid} />}
        {tab === InspectSection.FILES && <FilesView key={selectedPid ?? "all"} files={files} selectedPid={selectedPid} />}
        {tab === InspectSection.CGROUP && <CgroupView stats={result.cgroupStats ?? null} />}
        {tab === InspectSection.KSTACK && <KernelStacksView kstacks={kstacks} />}
        {tab === InspectSection.MAPS && <MapsView maps={maps} />}
        {tab === InspectSection.LIMITS && <LimitsView limits={limits} />}
      </div>
    </div>
  );
}

/**
 * A whole inspection taken already, such as the one kept with a command
 * someone stopped or paused: one section at a time.
 */
export function InspectReport({
  result,
  initialTab = InspectSection.PROCS,
  initialPid = null,
}: {
  result: InspectCommandResponse;
  initialTab?: InspectSection;
  initialPid?: number | null;
}) {
  const [state, dispatch] = useReducer(inspectReportReducer, { tab: initialTab, selectedPid: initialPid });
  return (
    <InspectReportView
      result={result}
      tab={state.tab}
      selectedPid={state.selectedPid}
      onSelectTab={(tab) => dispatch({ type: "set_tab", tab })}
      onPickPid={(pid) => dispatch({ type: "pick_pid", pid })}
      onClearPid={() => dispatch({ type: "clear_pid" })}
    />
  );
}

// ---- Sub-views for Inspect sections ----------------------------------------

function ProcsView({
  procs,
  onPickPid,
  onSamplePid,
}: {
  procs: ProcessInfo[];
  onPickPid: (pid: number) => void;
  onSamplePid?: (pid: number) => void;
}) {
  const [expandedPid, setExpandedPid] = useState<number | null>(null);
  if (procs.length === 0) return <Empty>No live processes found in this command's cgroup.</Empty>;
  return (
    <div className="overflow-auto">
      <table className="w-full border-collapse font-mono text-[11.5px]">
        <thead className="text-left text-[10px] uppercase tracking-wider text-muted-foreground">
          <tr className="border-b border-border">
            <th className="px-2 py-1.5">PID</th>
            <th className="px-2 py-1.5">State</th>
            <th className="px-2 py-1.5">Comm / Cmdline</th>
            <th className="px-2 py-1.5 text-right">Threads</th>
            <th className="px-2 py-1.5 text-right">RSS</th>
            <th className="px-2 py-1.5 text-right">CPU</th>
            <th className="px-2 py-1.5 text-right">IO R/W</th>
            <th className="px-2 py-1.5">Wait channel (wchan)</th>
            <th className="px-2 py-1.5" />
          </tr>
        </thead>
        <tbody className="divide-y divide-border">
          {procs.map((p) => {
            const stateTone = p.state === "R" ? "bg-ember-wash text-primary" : p.state === "D" ? "bg-[color-mix(in_oklab,var(--critical)_15%,transparent)] text-critical" : "bg-muted text-muted-foreground";
            const expanded = expandedPid === p.pid;
            return (
              <Fragment key={p.pid}>
                <tr className="hover:bg-muted/40">
                  <td className="num px-2 py-1.5 text-foreground">{p.pid}</td>
                  <td className="px-2 py-1.5">
                    <span className={`inline-block rounded px-1.5 py-0.5 text-[10px] ${stateTone}`}>{p.state}</span>
                  </td>
                  <td className="max-w-[340px] px-2 py-1.5">
                    <div className="font-semibold text-foreground">{p.comm}</div>
                    {p.cmdline && (
                      <div className="truncate text-[10.5px] text-muted-foreground" title={p.cmdline}>
                        {p.cmdline}
                      </div>
                    )}
                  </td>
                  <td className="num px-2 py-1.5 text-right text-muted-foreground">
                    {p.threads.length > 0 ? (
                      <button type="button" onClick={() => setExpandedPid(expanded ? null : p.pid)} className="underline decoration-dotted hover:text-primary">
                        {p.threadsCount}
                      </button>
                    ) : (
                      p.threadsCount
                    )}
                  </td>
                  <td className="num px-2 py-1.5 text-right text-foreground">{bytes(p.rssBytes)}</td>
                  <td className="num px-2 py-1.5 text-right text-muted-foreground">{((Number(p.cpuUserMs) + Number(p.cpuSysMs)) / 1000).toFixed(2)}s</td>
                  <td className="num px-2 py-1.5 text-right text-muted-foreground">
                    {bytes(p.ioReadBytes)} / {bytes(p.ioWriteBytes)}
                  </td>
                  <td className="px-2 py-1.5 text-frost">{p.wchan || (p.state === "R" ? "on-cpu" : "—")}</td>
                  <td className="px-2 py-1.5 text-right whitespace-nowrap">
                    <div className="inline-flex items-center gap-1">
                      {onSamplePid && (
                        <button
                          type="button"
                          onClick={() => onSamplePid(p.pid)}
                          className="rounded border border-primary/40 bg-ember-wash px-1.5 py-0.5 text-[10px] text-primary hover:brightness-110"
                        >
                          Sample
                        </button>
                      )}
                      <button type="button" onClick={() => onPickPid(p.pid)} className="rounded border border-border px-1.5 py-0.5 text-[10px] text-muted-foreground hover:border-primary hover:text-primary">
                        FDs
                      </button>
                    </div>
                  </td>
                </tr>
                {expanded &&
                  p.threads.map((t) => (
                    <tr key={`${p.pid}-${t.tid}`} className="bg-muted/25 text-[10.5px]">
                      <td className="num px-2 py-1 pl-5 text-faint">↳ {t.tid}</td>
                      <td className="px-2 py-1 text-muted-foreground">{t.state}</td>
                      <td className="px-2 py-1 text-muted-foreground">{t.name}</td>
                      <td />
                      <td />
                      <td className="num px-2 py-1 text-right text-faint">{((Number(t.cpuUserMs) + Number(t.cpuSysMs)) / 1000).toFixed(2)}s</td>
                      <td />
                      <td className="px-2 py-1 text-frost" colSpan={2}>
                        {t.wchan || (t.state === "R" ? "on-cpu" : "—")}
                      </td>
                    </tr>
                  ))}
              </Fragment>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function FilesView({ files, selectedPid = null }: { files: OpenFile[]; selectedPid?: number | null }) {
  const [kindFilter, setKindFilter] = useState<string>("all");
  const [search, setSearch] = useState("");
  const counts = useMemo(() => {
    const c: Record<string, number> = { all: files.length };
    for (const f of files) c[f.kind] = (c[f.kind] ?? 0) + 1;
    return c;
  }, [files]);
  const shown = useMemo(() => {
    const q = search.trim().toLowerCase();
    return files.filter((f) => {
      if (kindFilter !== "all" && f.kind !== kindFilter) return false;
      if (!q) return true;
      return (
        f.target.toLowerCase().includes(q) ||
        f.comm.toLowerCase().includes(q) ||
        f.localAddr.toLowerCase().includes(q) ||
        f.remoteAddr.toLowerCase().includes(q) ||
        f.socketState.toLowerCase().includes(q)
      );
    });
  }, [files, kindFilter, search]);

  if (files.length === 0) {
    return <Empty>{selectedPid !== null ? `No open file descriptors found for PID ${selectedPid}.` : "No open file descriptors found."}</Empty>;
  }
  return (
    <div className="space-y-2.5">
      <div className="flex flex-wrap items-center gap-2">
        <div className="flex flex-wrap gap-1">
          {["all", "file", "socket", "pipe", "anon_inode", "deleted", "device"].map((k) => {
            const n = counts[k] ?? 0;
            if (k !== "all" && n === 0) return null;
            return (
              <button
                key={k}
                type="button"
                onClick={() => setKindFilter(k)}
                className={`rounded-md border px-2 py-0.5 font-mono text-[10.5px] ${
                  kindFilter === k ? "border-primary bg-ember-wash text-primary" : "border-border text-muted-foreground hover:text-foreground"
                }`}
              >
                {k} ({n})
              </button>
            );
          })}
        </div>
        <input
          type="search"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          placeholder="Filter paths, ports, sockets..."
          className="ml-auto min-w-[180px] rounded-md border border-border bg-background/80 px-2.5 py-0.5 font-mono text-[11px] text-foreground placeholder:text-faint"
        />
      </div>
      <div className="max-h-[380px] overflow-auto">
        <table className="w-full border-collapse font-mono text-[11px]">
          <thead className="sticky top-0 bg-card text-left text-[10px] uppercase tracking-wider text-muted-foreground">
            <tr className="border-b border-border">
              <th className="px-2 py-1.5">PID</th>
              <th className="px-2 py-1.5">FD</th>
              <th className="px-2 py-1.5">Kind</th>
              <th className="px-2 py-1.5">Mode</th>
              <th className="px-2 py-1.5">Target / Endpoint</th>
              <th className="px-2 py-1.5 text-right">Offset / State</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-border">
            {shown.map((f, i) => (
              <tr key={`${f.pid}-${f.fd}-${i}`} className="hover:bg-muted/40">
                <td className="num px-2 py-1 text-muted-foreground">
                  {f.pid} <span className="text-faint">{f.comm}</span>
                </td>
                <td className="num px-2 py-1 text-foreground">{f.fd}</td>
                <td className="px-2 py-1">
                  <span
                    className={`rounded px-1.5 py-0.5 text-[10px] ${
                      f.kind === "socket"
                        ? "bg-frost-wash text-frost"
                        : f.kind === "deleted"
                          ? "bg-[color-mix(in_oklab,var(--critical)_15%,transparent)] text-critical"
                          : "bg-muted text-muted-foreground"
                    }`}
                  >
                    {f.socketProto || f.kind}
                  </span>
                </td>
                <td className="px-2 py-1 text-faint">{f.mode || "—"}</td>
                <td className="max-w-[440px] truncate px-2 py-1 text-foreground" title={f.target}>
                  {f.localAddr ? `${f.localAddr}${f.remoteAddr && !f.remoteAddr.endsWith(":0") ? ` → ${f.remoteAddr}` : ""}` : f.target}
                </td>
                <td className="num px-2 py-1 text-right text-muted-foreground">
                  {f.socketState ? (
                    <span className={f.socketState === "LISTEN" ? "text-good-ink" : f.socketState === "ESTABLISHED" ? "text-frost" : ""}>{f.socketState}</span>
                  ) : f.pos > 0n ? (
                    bytes(f.pos)
                  ) : (
                    "—"
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function CgroupView({ stats }: { stats: CgroupStats | null }) {
  if (!stats) return <Empty>No cgroup v2 telemetry available for this command.</Empty>;
  return (
    <div className="space-y-4 font-mono text-[11.5px]">
      <div className="grid grid-cols-2 gap-2.5 sm:grid-cols-4">
        <MiniMetric label="CPU total" value={`${(Number(stats.cpuUsageUsec) / 1e6).toFixed(2)}s`} sub={`user ${(Number(stats.cpuUserUsec) / 1e6).toFixed(2)}s · sys ${(Number(stats.cpuSystemUsec) / 1e6).toFixed(2)}s`} />
        <MiniMetric label="CPU throttled" value={String(stats.nrThrottled)} sub={`${(Number(stats.throttledUsec) / 1e3).toFixed(1)} ms`} />
        <MiniMetric label="Memory / Peak" value={bytes(stats.memoryCurrentBytes)} sub={`peak ${bytes(stats.memoryPeakBytes)} · swap ${bytes(stats.memorySwapBytes)}`} />
        <MiniMetric label="PIDs / Faults" value={`${stats.pidsCurrent} / ${stats.pidsPeak}`} sub={`${stats.pgfault.toLocaleString()} minor · ${stats.pgmajfault.toLocaleString()} major`} />
      </div>

      <div>
        <div className="mb-1 text-[10px] uppercase tracking-wider text-muted-foreground">Memory breakdown</div>
        <SegmentedBar
          tall
          format={bytes}
          parts={[
            { id: "anon", label: "anon", value: Number(stats.memoryAnonBytes), color: "var(--series-2)" },
            { id: "file", label: "page cache", value: Number(stats.memoryFileBytes), color: "var(--series-1)" },
            { id: "kernel", label: "kernel", value: Number(stats.memoryKernelBytes), color: "var(--series-4)" },
            { id: "slab", label: "slab", value: Number(stats.memorySlabBytes), color: "var(--series-3)" },
            { id: "swap", label: "swap", value: Number(stats.memorySwapBytes), color: "var(--frost)" },
          ]}
        />
      </div>

      <div className="grid grid-cols-3 gap-2.5">
        <MiniMetric label="PSI CPU avg10" value={`${stats.psiCpuSomeAvg10.toFixed(2)}%`} sub={`full ${stats.psiCpuFullAvg10.toFixed(2)}%`} />
        <MiniMetric label="PSI Memory avg10" value={`${stats.psiMemSomeAvg10.toFixed(2)}%`} sub={`full ${stats.psiMemFullAvg10.toFixed(2)}%`} />
        <MiniMetric label="PSI IO avg10" value={`${stats.psiIoSomeAvg10.toFixed(2)}%`} sub={`full ${stats.psiIoFullAvg10.toFixed(2)}%`} />
      </div>

      {stats.io.length > 0 && (
        <div>
          <div className="mb-1 text-[10px] uppercase tracking-wider text-muted-foreground">Block IO (io.stat)</div>
          <div className="divide-y divide-border rounded-lg border border-border">
            {stats.io.map((d) => (
              <div key={d.device} className="flex flex-wrap items-center justify-between gap-2 px-3 py-1.5 text-[11px]">
                <span className="text-foreground">dev {d.device}</span>
                <span className="num text-muted-foreground">
                  read {bytes(d.rbytes)} ({d.rios.toLocaleString()} ops) · write {bytes(d.wbytes)} ({d.wios.toLocaleString()} ops)
                </span>
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}

function MiniMetric({ label, value, sub }: { label: string; value: string; sub: string }) {
  return (
    <div className="rounded-lg border border-border bg-card/60 px-3 py-2">
      <div className="text-[10px] text-muted-foreground">{label}</div>
      <div className="num mt-0.5 text-[15px] font-semibold text-foreground">{value}</div>
      <div className="mt-0.5 truncate text-[10px] text-faint" title={sub}>
        {sub}
      </div>
    </div>
  );
}

function KernelStacksView({ kstacks }: { kstacks: ThreadKernelStack[] }) {
  // Group threads sharing the exact same kernel stack so 50 parked threads collapse cleanly.
  const grouped = useMemo(() => {
    const m = new Map<string, { frames: string[]; threads: ThreadKernelStack[] }>();
    for (const ks of kstacks) {
      const key = ks.frames.join("\n") || "(empty / running on CPU)";
      const cur = m.get(key) ?? { frames: ks.frames, threads: [] };
      cur.threads.push(ks);
      m.set(key, cur);
    }
    return Array.from(m.values()).sort((a, b) => b.threads.length - a.threads.length);
  }, [kstacks]);

  if (grouped.length === 0) return <Empty>No kernel stacks readable (threads may have exited).</Empty>;
  return (
    <div className="max-h-[420px] space-y-2.5 overflow-auto">
      {grouped.map((g, idx) => (
        <div key={idx} className="rounded-lg border border-border bg-card/60 p-2.5 font-mono text-[11px]">
          <div className="mb-1.5 flex flex-wrap items-center gap-2">
            <span className="rounded-full bg-frost-wash px-2 py-0.5 text-[10px] text-frost">×{g.threads.length} thread{g.threads.length > 1 ? "s" : ""}</span>
            <span className="text-muted-foreground">
              {g.threads
                .slice(0, 6)
                .map((t) => `${t.threadName || t.comm}(${t.tid})[${t.state}]`)
                .join(", ")}
              {g.threads.length > 6 ? ` +${g.threads.length - 6} more` : ""}
            </span>
          </div>
          <pre className="overflow-x-auto text-[10.5px] leading-relaxed text-foreground/85">{g.frames.length > 0 ? g.frames.join("\n") : "(running on CPU)"}</pre>
        </div>
      ))}
    </div>
  );
}

function MapsView({ maps }: { maps: ProcessMemoryMap[] }) {
  if (maps.length === 0) return <Empty>No memory map data readable.</Empty>;
  return (
    <div className="max-h-[420px] space-y-3 overflow-auto font-mono text-[11px]">
      {maps.map((m) => (
        <div key={m.pid} className="space-y-2 rounded-lg border border-border bg-card/60 p-3">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <span className="font-semibold text-foreground">
              PID {m.pid} ({m.comm})
            </span>
            <span className="num text-muted-foreground">
              RSS {bytes(m.rssBytes)} · PSS {bytes(m.pssBytes)} · Private Dirty {bytes(m.privateDirtyBytes)} · Swap {bytes(m.swapBytes)}
            </span>
          </div>
          {m.topRegions.length > 0 && (
            <div className="divide-y divide-border/70">
              {m.topRegions.map((r, i) => (
                <div key={i} className="flex items-center gap-3 py-1">
                  <span className="w-10 shrink-0 text-faint">{r.perms}</span>
                  <span className="min-w-0 flex-1 truncate text-foreground" title={r.path}>
                    {r.path}
                  </span>
                  <span className="num shrink-0 text-muted-foreground">{bytes(r.sizeBytes)}</span>
                </div>
              ))}
            </div>
          )}
        </div>
      ))}
    </div>
  );
}

function LimitsView({ limits }: { limits: ProcessLimits[] }) {
  if (limits.length === 0) return <Empty>No limits readable.</Empty>;
  return (
    <div className="max-h-[420px] space-y-3 overflow-auto font-mono text-[11px]">
      {limits.map((l) => (
        <div key={l.pid} className="space-y-2 rounded-lg border border-border bg-card/60 p-3">
          <div className="flex flex-wrap items-center gap-3">
            <span className="font-semibold text-foreground">
              PID {l.pid} ({l.comm})
            </span>
            <span className="text-muted-foreground">CapEff: {l.capEff || "—"}</span>
            <span className="text-muted-foreground">Seccomp: {l.seccomp || "—"}</span>
            <span className="text-muted-foreground">NoNewPrivs: {l.noNewPrivs ? "yes" : "no"}</span>
          </div>
          <table className="w-full border-collapse text-[11px]">
            <thead>
              <tr className="border-b border-border text-left text-[10px] uppercase text-faint">
                <th className="py-1">Limit</th>
                <th className="py-1 text-right">Soft</th>
                <th className="py-1 text-right">Hard</th>
                <th className="py-1 text-right">Units</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-border/60">
              {l.items.map((it) => (
                <tr key={it.name}>
                  <td className="py-1 text-foreground">{it.name}</td>
                  <td className="num py-1 text-right text-muted-foreground">{it.soft}</td>
                  <td className="num py-1 text-right text-muted-foreground">{it.hard}</td>
                  <td className="py-1 text-right text-faint">{it.units}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ))}
    </div>
  );
}
