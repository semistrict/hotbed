import { Fragment, useEffect, useMemo, useReducer, useRef, useState } from "react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  clients,
  InspectSection,
  TraceKind,
  traceCommand,
  type InspectCommandResponse,
  type ProcessInfo,
  type ProcessLimits,
  type ThreadKernelStack,
  type ToolCapability,
  type TraceSummary,
} from "../api";
import { bytes, hhmmss } from "../lib/format";
import { CgroupView, FilesTable, MemoryMapsList, PmuCountersGrid } from "./DiagnosticsViews";
import { FlameGraph } from "./FlameGraph";
import { Card, Empty } from "./Panel";

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
export function LiveDiagnosticsPanel({ id, ended = false, onSamplePid }: { id: string; ended?: boolean; onSamplePid?: (pid: number) => void }) {
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
    if (ended) return;
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
      const res = await clients.workspaces.inspectCommand({ id, sections, pid }, { signal: ac.signal });
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
    if (ended) return;
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
      const summary = await traceCommand(
        {
          id,
          kind,
          durationSeconds: durationSec,
          pid: selectedPid,
          filter: kind === TraceKind.STRACE || kind === TraceKind.STRACE_SUMMARY ? straceFilter.trim() : "",
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

  const pidItems = inspectData
    ? [
        { value: "0", label: `All PIDs (${inspectData.procs.length})` },
        ...inspectData.procs.map((p) => ({ value: String(p.pid), label: `PID ${p.pid} (${p.comm})` })),
      ]
    : [];
  const traceDurationItems = [3, 5, 10, 20, 30].map((s) => ({ value: String(s), label: `${s}s` }));

  const headerRight = (
    <div className="flex flex-wrap items-center gap-2 font-mono text-[10.5px]">
      {inspectData && inspectData.procs.length > 0 && (
        <Select items={pidItems} value={String(selectedPid)} disabled={ended} onValueChange={(v) => v !== null && selectPid(Number(v))}>
          <SelectTrigger size="sm" aria-label="Target PID" className="h-7 bg-background/80 font-mono text-[11px] text-foreground">
            <SelectValue />
          </SelectTrigger>
          <SelectContent className="font-mono text-[11px]">
            {pidItems.map((item) => (
              <SelectItem key={item.value} value={item.value} className="text-[11px]">
                {item.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      )}
      <Badge variant="outline" className="bg-muted/60 font-mono text-[10.5px] font-normal text-muted-foreground">
        {ended ? "ended · what was taken stays here" : "opt-in · zero overhead until clicked"}
      </Badge>
    </div>
  );

  return (
    <Card title="Linux Diagnostics" sub="Inspect /proc & cgroup v2 or attach strace, perf, thread stacks, and eBPF probes on demand" right={headerRight}>
      <div className="space-y-5">
        {/* Row 1: Instant /proc & cgroup inspection buttons */}
        <div className="space-y-2">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <span className="eyebrow">Inspect (/proc & cgroup v2 · instant, non-intrusive)</span>
            {inspectData && (
              <span className="font-mono text-[10.5px] text-faint">
                {inspectData.cgroup || inspectData.unitName} · {hhmmss(inspectData.collectedAt)}
              </span>
            )}
          </div>
          <InspectTabs value={inspectTab} disabled={ended} onPick={(section) => void runInspect(section)} />
        </div>

        {/* Inspect output area */}
        {inspectError && (
          <Alert variant="destructive" className="border-critical/30 bg-[color-mix(in_oklab,var(--critical)_10%,transparent)] px-3 py-2 font-mono text-[11.5px]">
            <AlertDescription className="text-[11.5px] text-critical">{inspectError}</AlertDescription>
          </Alert>
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
            <InspectSectionView tab={inspectTab} result={inspectData} />
          </div>
        )}

        {/* Row 2: Active opt-in Linux tracing tools */}
        <div className="space-y-2.5 border-t border-border pt-4">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <span className="eyebrow">Active Trace (opt-in Linux tracers · time-bounded & cancellable)</span>
            <div className="flex flex-wrap items-center gap-2">
              <div className="flex items-center gap-1.5 font-mono text-[11px] text-muted-foreground">
                <span>Duration</span>
                <Select items={traceDurationItems} value={String(durationSec)} disabled={traceRunning} onValueChange={(v) => v !== null && setDurationSec(Number(v))}>
                  <SelectTrigger size="sm" aria-label="Trace duration" className="h-7 bg-background/80 font-mono text-[11px] text-foreground">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent className="font-mono text-[11px]">
                    {traceDurationItems.map((item) => (
                      <SelectItem key={item.value} value={item.value} className="text-[11px]">
                        {item.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <Input
                type="text"
                value={straceFilter}
                onChange={(e) => setStraceFilter(e.target.value)}
                disabled={traceRunning}
                placeholder="strace -e (e.g. file,network)"
                aria-label="strace syscall filter"
                className="h-7 w-full bg-background/80 font-mono text-[11px] placeholder:text-faint sm:w-[240px]"
              />
              {traceRunning && (
                <Button
                  type="button"
                  variant="destructive"
                  size="xs"
                  onClick={stopTrace}
                  className="border-critical font-mono text-[11px] text-critical"
                >
                  Stop trace
                </Button>
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
                  disabled={ended || traceRunning || Boolean(unavailable)}
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
            {traceSummary && traceSummary.counters.length > 0 && <PmuCountersGrid counters={traceSummary.counters} />}

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

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-1.5">
        <InspectTabs value={tab} onPick={onSelectTab} />
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
        <InspectSectionView key={selectedPid ?? "all"} tab={tab} result={result} selectedPid={selectedPid} />
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

/**
 * The inspect sections as tabs. `onPick` fires on every click, the current
 * tab's too, so a live panel can read a section again.
 */
function InspectTabs({ value, disabled = false, onPick }: { value: InspectSection | null; disabled?: boolean; onPick: (section: InspectSection) => void }) {
  return (
    <Tabs value={value}>
      <TabsList className="h-auto max-w-full flex-wrap justify-start border border-border bg-card/70">
        {INSPECT_BUTTONS.map((b) => (
          <TabsTrigger key={b.id} value={b.id} title={b.desc} disabled={disabled} onClick={() => onPick(b.id)} className="px-3 font-mono text-[11.5px]">
            {b.label}
          </TabsTrigger>
        ))}
      </TabsList>
    </Tabs>
  );
}

/**
 * One inspect section of a result, other than the process list (which needs
 * the caller's PID actions), narrowed to `selectedPid` when one is picked.
 */
function InspectSectionView({ tab, result, selectedPid = null }: { tab: InspectSection; result: InspectCommandResponse; selectedPid?: number | null }) {
  const forPid = <T extends { pid: number }>(xs: T[]) => (selectedPid === null ? xs : xs.filter((x) => x.pid === selectedPid));
  switch (tab) {
    case InspectSection.FILES:
      return (
        <FilesTable
          files={forPid(result.files)}
          empty={selectedPid !== null ? `No open file descriptors found for PID ${selectedPid}.` : "No open file descriptors found."}
        />
      );
    case InspectSection.CGROUP:
      return result.cgroupStats ? <CgroupView stats={result.cgroupStats} /> : <Empty>No cgroup v2 telemetry available for this command.</Empty>;
    case InspectSection.KSTACK:
      return <KernelStacksView kstacks={forPid(result.kstacks)} />;
    case InspectSection.MAPS:
      return <MemoryMapsList maps={forPid(result.maps)} />;
    case InspectSection.LIMITS:
      return <LimitsView limits={forPid(result.limits)} />;
    default:
      return null;
  }
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
