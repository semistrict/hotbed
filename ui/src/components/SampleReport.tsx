import { useMemo, useState } from "react";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  type FileLock,
  type FoldedStack,
  type ProcessSample,
  type ProfileFrame,
  type SampledOpenFile,
  type SampledProcess,
  type SampledThread,
} from "../api";
import { bytes, hhmmss } from "../lib/format";
import { CgroupView, FilesTable, MemoryMapsList, PmuCountersGrid } from "./DiagnosticsViews";
import { FlameGraph } from "./FlameGraph";
import { Empty, MetricTile } from "./Panel";

type SampleTab = "stacks" | "files" | "system";
type StackSubView = "tree" | "threads" | "flamegraph";

export interface LocalCallTreeNode {
  symbol: string;
  module: string;
  frameType: string;
  samples: number;
  pct: number;
  children: LocalCallTreeNode[];
}

export interface CommCallTreeGroup {
  comm: string;
  procs: SampledProcess[];
  totalSamples: number;
  nodes: LocalCallTreeNode[];
}

export function buildCallTreeFromStacks(
  stacks: readonly FoldedStack[],
  frames: readonly ProfileFrame[],
  comm: string,
): LocalCallTreeNode[] {
  type Acc = { symbol: string; module: string; frameType: string; samples: number; children: Map<string, Acc> };
  const root = new Map<string, Acc>();
  let total = 0;
  for (const st of stacks) {
    if (st.comm !== comm) continue;
    const samples = Number(st.samples);
    if (samples <= 0) continue;
    const path: ProfileFrame[] = [];
    for (const id of st.frames) {
      if (id < 0 || id >= frames.length) continue;
      const f = frames[id];
      if (f !== undefined && f.frameType !== "process") {
        path.push(f);
      }
    }
    if (path.length === 0) continue;
    total += samples;
    let cur = root;
    for (const f of path) {
      const ft = f.frameType === "native" ? "user" : f.frameType || "user";
      const key = `${ft}\0${f.name}\0${f.module}`;
      let node = cur.get(key);
      if (!node) {
        node = { symbol: f.name, module: f.module, frameType: ft, samples: 0, children: new Map() };
        cur.set(key, node);
      }
      node.samples += samples;
      cur = node.children;
    }
  }
  if (total === 0) return [];
  const toNodes = (m: Map<string, Acc>): LocalCallTreeNode[] =>
    Array.from(m.values())
      .sort((a, b) => b.samples - a.samples || a.symbol.localeCompare(b.symbol))
      .map((n) => ({
        symbol: n.symbol,
        module: n.module,
        frameType: n.frameType,
        samples: n.samples,
        pct: (100 * n.samples) / total,
        children: toNodes(n.children),
      }));
  return toNodes(root);
}

export function buildCallTreesByComm(
  procs: readonly SampledProcess[],
  stacks: readonly FoldedStack[],
  frames: readonly ProfileFrame[],
): CommCallTreeGroup[] {
  const groups = new Map<string, SampledProcess[]>();
  for (const p of procs) {
    const list = groups.get(p.comm);
    if (list) list.push(p);
    else groups.set(p.comm, [p]);
  }
  for (const st of stacks) {
    if (Number(st.samples) <= 0 || !st.comm) continue;
    if (!groups.has(st.comm)) {
      groups.set(st.comm, []);
    }
  }
  const result: CommCallTreeGroup[] = [];
  for (const [comm, commProcs] of groups) {
    const nodes = buildCallTreeFromStacks(stacks, frames, comm);
    if (commProcs.length === 0 && nodes.length === 0) continue;
    const totalSamples = nodes.reduce((sum, n) => sum + n.samples, 0);
    result.push({ comm, procs: commProcs, totalSamples, nodes });
  }
  return result;
}

/** What a sample adds up to, across its processes and threads. */
function sampleCounts(sample: ProcessSample) {
  let threads = 0;
  let stationary = 0;
  let dState = 0;
  let ioRead = 0;
  let ioWrite = 0;
  let cpuDeltaMs = 0;
  let ctxtDelta = 0;
  for (const p of sample.procs) {
    ioRead += Number(p.ioReadDeltaBytes);
    ioWrite += Number(p.ioWriteDeltaBytes);
    cpuDeltaMs += p.cpuUserDeltaMs + p.cpuSysDeltaMs;
    ctxtDelta += Number(p.voluntaryCtxtDelta) + Number(p.nonvoluntaryCtxtDelta);
    for (const t of p.threads) {
      threads++;
      if (t.stationary) stationary++;
      if (t.state === "D" || (t.stateCounts["D"] ?? 0) > 0) dState++;
    }
  }
  const blockedLocks = sample.locks.filter((l) => l.blocked).length;
  const queuedSockets = sample.files.filter((f) => f.txQueueBytes > 0n || f.rxQueueBytes > 0n).length;
  const advancingFiles = sample.files.filter((f) => f.posDelta > 0n).length;
  return { threads, stationary, dState, ioRead, ioWrite, cpuDeltaMs, ctxtDelta, blockedLocks, queuedSockets, advancingFiles };
}

/**
 * One activity sample: its metadata, summary metrics, and call graph, files,
 * and cgroup/PMU tabs. `onSamplePid`, when given, lets a live command re-sample
 * a single PID; saved samples omit it.
 */
export function SampleReport({ sample, onSamplePid }: { sample: ProcessSample; onSamplePid?: (pid: number) => void }) {
  const [tab, setTab] = useState<SampleTab>("stacks");
  const counts = useMemo(() => sampleCounts(sample), [sample]);

  return (
    <div className="space-y-4">
      {/* Sample metadata & active collectors */}
      <div className="flex flex-wrap items-center justify-between gap-2 rounded-xl border border-border bg-background/50 px-3.5 py-2.5 font-mono text-[10.5px] text-muted-foreground">
        <span>
          {hhmmss(sample.endedAt)} · {(Number(sample.durationMs) / 1000).toFixed(2)}s · {sample.pollCount} polls · {sample.userStackSnapshots} stack snaps
        </span>
        {sample.collectors.length > 0 && (
          <div className="flex flex-wrap gap-1.5 text-[10px]">
            {sample.collectors.map((c) => (
              <span
                key={c.name}
                title={c.detail}
                className={`rounded-full border px-2 py-0.5 ${
                  c.ok ? "border-border bg-background/60 text-muted-foreground" : "border-border/50 bg-background/30 text-faint line-through"
                }`}
              >
                {c.name}
              </span>
            ))}
          </div>
        )}
      </div>

      {/* Summary metrics strip */}
      <div className="grid grid-cols-2 gap-2.5 font-mono text-[11.5px] sm:grid-cols-3 lg:grid-cols-5">
        <MetricTile
          label="Processes / Threads"
          value={`${sample.procs.length} / ${counts.threads}`}
          sub={
            counts.dState > 0
              ? `${counts.dState} in D-state · ${counts.stationary} stationary`
              : `${counts.stationary}/${counts.threads} stationary`
          }
          tone={counts.dState > 0 ? "critical" : "default"}
        />
        <MetricTile
          label="CPU in window"
          value={`${(sample.cgroupStats?.cpuCoresAvg ?? counts.cpuDeltaMs / Math.max(1, Number(sample.durationMs))).toFixed(2)} cores`}
          sub={`Δ${counts.cpuDeltaMs.toFixed(0)} ms · ${counts.ctxtDelta.toLocaleString()} ctx sw`}
          tone="ember"
        />
        <MetricTile
          label="I/O in window"
          value={bytes(counts.ioRead + counts.ioWrite)}
          sub={`R ${bytes(counts.ioRead)} · W ${bytes(counts.ioWrite)}${counts.advancingFiles > 0 ? ` · ${counts.advancingFiles} active FD` : ""}`}
        />
        <MetricTile
          label="Open FDs & Locks"
          value={`${sample.files.length} FDs`}
          sub={`${sample.locks.length} locks${counts.blockedLocks > 0 ? ` (${counts.blockedLocks} blocked!)` : ""}${counts.queuedSockets > 0 ? ` · ${counts.queuedSockets} queued sock` : ""}`}
          tone={counts.blockedLocks > 0 ? "critical" : "default"}
        />
        <MetricTile
          label="Memory & PSI"
          value={bytes(sample.cgroupStats?.end?.memoryCurrentBytes ?? sample.procs.reduce((a, p) => a + Number(p.rssBytes), 0))}
          sub={
            sample.cgroupStats
              ? `PSI io +${(Number(sample.cgroupStats.psiIoSomeDeltaUsec) / 1000).toFixed(0)}ms · cpu +${(Number(sample.cgroupStats.psiCpuSomeDeltaUsec) / 1000).toFixed(0)}ms`
              : `${sample.hostname} · ${sample.arch}`
          }
        />
      </div>

      <Tabs value={tab} onValueChange={(v) => setTab(v as SampleTab)}>
        <TabsList className="h-auto max-w-full flex-wrap justify-start border border-border bg-card/70">
          <TabsTrigger value="stacks" className="px-3 font-mono text-[11.5px]">
            Call Graph &amp; Threads ({counts.threads})
          </TabsTrigger>
          <TabsTrigger value="files" className="px-3 font-mono text-[11.5px]">
            Open Files, Sockets &amp; Locks ({sample.files.length}
            {sample.locks.length ? ` · ${sample.locks.length} locks` : ""})
          </TabsTrigger>
          <TabsTrigger value="system" className="px-3 font-mono text-[11.5px]">
            Cgroup, PSI &amp; PMU{sample.perfCounters.length ? ` (${sample.perfCounters.length})` : ""}
          </TabsTrigger>
        </TabsList>
      </Tabs>

      {/* Tab content */}
      {tab === "stacks" && <StacksAndThreadsTab sample={sample} onSamplePid={onSamplePid} />}
      {tab === "files" && <FilesAndLocksTab files={sample.files} locks={sample.locks} />}
      {tab === "system" && <SystemAndPmuTab sample={sample} />}
    </div>
  );
}

// ---- Tab 1: Call Graph & Threads -------------------------------------------

function StacksAndThreadsTab({ sample, onSamplePid }: { sample: ProcessSample; onSamplePid?: (pid: number) => void }) {
  const [subView, setSubView] = useState<StackSubView>("tree");
  const [filter, setFilter] = useState("");
  const [onlyStationary, setOnlyStationary] = useState(false);

  const callTreeGroups = useMemo(
    () => buildCallTreesByComm(sample.procs, sample.sampledStacks, sample.frames),
    [sample.procs, sample.sampledStacks, sample.frames],
  );

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-2">
        <div className="max-w-full overflow-x-auto no-scrollbar">
          <Tabs value={subView} onValueChange={(v) => setSubView(v as "tree" | "threads" | "flamegraph")}>
            <TabsList className="h-8 border border-border font-mono text-[11px]">
              <TabsTrigger value="tree" className="px-2.5 font-mono text-[11px]">
                Call Tree
              </TabsTrigger>
              <TabsTrigger value="threads" className="px-2.5 font-mono text-[11px]">
                <span>Threads</span>
                <span className="hidden sm:inline"> (User + Syscall + Kernel)</span>
              </TabsTrigger>
              <TabsTrigger value="flamegraph" className="px-2.5 font-mono text-[11px]">
                FlameGraph
              </TabsTrigger>
            </TabsList>
          </Tabs>
        </div>
        <Input
          type="search"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          placeholder="Filter symbols, syscalls, wchan, threads..."
          className="w-full bg-background/70 font-mono text-[11px] placeholder:text-faint sm:min-w-[220px] sm:flex-1"
        />
        {subView === "threads" && (
          <Label className="flex h-8 cursor-pointer items-center gap-1.5 rounded-md border border-border px-2.5 font-mono text-[10.5px] font-normal text-muted-foreground">
            <Checkbox checked={onlyStationary} onCheckedChange={(c) => setOnlyStationary(Boolean(c))} />
            stationary only
          </Label>
        )}
      </div>

      {subView === "flamegraph" ? (
        <div className="space-y-4">
          <div className="rounded-xl border border-border bg-background/40 p-3">
            <div className="mb-2 flex flex-wrap items-center justify-between gap-2 font-mono text-[10.5px] text-muted-foreground">
              <span className="uppercase tracking-wider">Sampled All-Thread Stacks (On-CPU + Blocked User & Kernel)</span>
              <span>{sample.sampledStacks.length} unique stack paths</span>
            </div>
            <FlameGraph stacks={sample.sampledStacks} frames={sample.frames} highlight={filter} />
          </div>
          {sample.onCpuStacks.length > 0 && (
            <div className="rounded-xl border border-border bg-background/40 p-3">
              <div className="mb-2 flex flex-wrap items-center justify-between gap-2 font-mono text-[10.5px] text-muted-foreground">
                <span className="uppercase tracking-wider">On-CPU Continuous eBPF Profile (OTel eBPF · Sample Window)</span>
                <span>{sample.onCpuStacks.reduce((a, b) => a + Number(b.samples), 0)} samples</span>
              </div>
              <FlameGraph stacks={sample.onCpuStacks} frames={sample.frames} highlight={filter} />
            </div>
          )}
        </div>
      ) : subView === "tree" ? (
        callTreeGroups.length === 0 ? (
          <Empty>No live processes captured in this sample.</Empty>
        ) : (
          <div className="space-y-3">
            {callTreeGroups.map((group) => (
              <CallTreeGroupCard key={group.comm || "(unknown)"} group={group} filter={filter} onSamplePid={onSamplePid} />
            ))}
          </div>
        )
      ) : sample.procs.length === 0 ? (
        <Empty>No live processes captured in this sample.</Empty>
      ) : (
        <div className="space-y-3">
          {sample.procs.map((p) => (
            <ProcessThreadsCard
              key={p.pid}
              proc={p}
              filter={filter}
              onlyStationary={onlyStationary}
              onSamplePid={onSamplePid}
            />
          ))}
        </div>
      )}
    </div>
  );
}

function ProcessHeader({ proc, onSamplePid }: { proc: SampledProcess; onSamplePid?: (pid: number) => void }) {
  const stateTone =
    proc.state === "R"
      ? "bg-ember-wash text-primary"
      : proc.state === "D"
        ? "bg-[color-mix(in_oklab,var(--critical)_15%,transparent)] text-critical"
        : "bg-muted text-muted-foreground";

  return (
    <div className="flex flex-wrap items-start justify-between gap-2 border-b border-border/70 pb-2.5 font-mono">
      <div className="min-w-0 space-y-0.5">
        <div className="flex flex-wrap items-center gap-2 text-[12px]">
          <span className={`rounded px-1.5 py-0.5 text-[10px] ${stateTone}`}>{proc.state}</span>
          <span className="font-semibold text-foreground">{proc.comm}</span>
          <span className="text-muted-foreground">PID {proc.pid}</span>
          <span className="text-[10.5px] text-faint">PPID {proc.ppid}</span>
          <span className="text-[10.5px] text-faint">· {proc.threads.length} threads · {proc.openFdsCount} FDs</span>
        </div>
        {proc.cmdline && (
          <div className="max-w-full truncate text-[10.5px] text-muted-foreground sm:max-w-[780px]" title={proc.cmdline}>
            {proc.cmdline}
          </div>
        )}
        {(proc.cwd || proc.exe) && (
          <div className="flex flex-wrap gap-3 text-[10px] text-faint">
            {proc.exe && <span title={proc.exe}>exe: {proc.exe}</span>}
            {proc.cwd && <span title={proc.cwd}>cwd: {proc.cwd}</span>}
          </div>
        )}
      </div>
      <div className="flex flex-wrap items-center gap-2 text-[10.5px]">
        <span className="num rounded bg-muted/70 px-2 py-0.5 text-foreground">CPU {proc.cpuPct.toFixed(1)}%</span>
        <span className="num rounded bg-muted/70 px-2 py-0.5 text-muted-foreground">RSS {bytes(proc.rssBytes)}</span>
        {(proc.ioReadDeltaBytes > 0n || proc.ioWriteDeltaBytes > 0n) && (
          <span className="num rounded bg-muted/70 px-2 py-0.5 text-muted-foreground">
            IO +{bytes(proc.ioReadDeltaBytes)} R / +{bytes(proc.ioWriteDeltaBytes)} W
          </span>
        )}
        <span className="num rounded bg-muted/70 px-2 py-0.5 text-faint">
          ctx +{String(proc.voluntaryCtxtDelta)}/{String(proc.nonvoluntaryCtxtDelta)}
        </span>
        {onSamplePid && (
          <button
            type="button"
            onClick={() => onSamplePid(proc.pid)}
            className="rounded border border-border px-2 py-0.5 text-[10px] text-muted-foreground hover:border-primary hover:text-primary"
          >
            Sample PID
          </button>
        )}
      </div>
    </div>
  );
}

function CallTreeGroupCard({
  group,
  filter,
  onSamplePid,
}: {
  group: CommCallTreeGroup;
  filter: string;
  onSamplePid?: (pid: number) => void;
}) {
  const q = filter.trim().toLowerCase();
  const singleProc = group.procs.length === 1 ? group.procs[0] : null;
  const totalCpuPct = group.procs.reduce((sum, p) => sum + p.cpuPct, 0);
  const totalRssBytes = group.procs.reduce((sum, p) => sum + p.rssBytes, 0n);
  const totalThreads = group.procs.reduce((sum, p) => sum + p.threads.length, 0);
  const totalFds = group.procs.reduce((sum, p) => sum + p.openFdsCount, 0);
  const pidList = group.procs.map((p) => p.pid).join(", ");

  return (
    <div className="rounded-xl border border-border bg-background/45 p-3.5">
      {singleProc ? (
        <ProcessHeader proc={singleProc} onSamplePid={onSamplePid} />
      ) : group.procs.length > 1 ? (
        <div className="space-y-2 border-b border-border/70 pb-2.5 font-mono">
          <div className="flex flex-wrap items-start justify-between gap-2">
            <div className="min-w-0 space-y-0.5">
              <div className="flex flex-wrap items-center gap-2 text-[12px]">
                <span className="font-semibold text-foreground">{group.comm}</span>
                <span
                  className="rounded-full border border-primary/30 bg-ember-wash px-2 py-0.5 text-[10px] text-primary"
                  title={`PIDs: ${pidList}`}
                >
                  {group.procs.length} PIDs: {pidList}
                </span>
                <span className="text-[10.5px] text-faint">
                  · {totalThreads} threads · {totalFds} FDs
                </span>
              </div>
              <div className="text-[10.5px] text-muted-foreground">
                Aggregated across {group.procs.length} {group.comm} processes · {group.totalSamples} samples
                {onSamplePid && " (click a PID to sample individually)"}
              </div>
            </div>
            <div className="flex flex-wrap items-center gap-2 text-[10.5px]">
              <span className="num rounded bg-muted/70 px-2 py-0.5 text-foreground">CPU {totalCpuPct.toFixed(1)}%</span>
              <span className="num rounded bg-muted/70 px-2 py-0.5 text-muted-foreground">RSS {bytes(totalRssBytes)}</span>
            </div>
          </div>
          {onSamplePid && (
            <div className="flex flex-wrap items-center gap-1.5 text-[10px]">
              {group.procs.map((p) => (
                <button
                  key={p.pid}
                  type="button"
                  onClick={() => onSamplePid(p.pid)}
                  title={`Sample PID ${p.pid} (${p.comm}) individually`}
                  className="rounded border border-border px-2 py-0.5 text-muted-foreground hover:border-primary hover:text-primary"
                >
                  Sample PID {p.pid} ({p.cpuPct.toFixed(1)}%)
                </button>
              ))}
            </div>
          )}
        </div>
      ) : (
        <div className="flex flex-wrap items-center justify-between gap-2 border-b border-border/70 pb-2.5 font-mono">
          <div className="flex flex-wrap items-center gap-2 text-[12px]">
            <span className="font-semibold text-foreground">{group.comm}</span>
            <span className="rounded bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground">exited during sample</span>
          </div>
          <span className="num text-[10.5px] text-muted-foreground">{group.totalSamples} samples</span>
        </div>
      )}

      <div className="mt-2.5">
        {group.nodes.length > 0 ? (
          <div className="max-h-[420px] overflow-auto font-mono text-[11px]">
            {group.nodes.map((node, i) => (
              <CallTreeRow key={`${node.symbol}-${i}`} node={node} depth={0} filter={q} />
            ))}
          </div>
        ) : (
          <div className="py-3 font-mono text-[11px] text-faint">No user or kernel call tree frames captured for {group.comm}.</div>
        )}
      </div>
    </div>
  );
}

function ProcessThreadsCard({
  proc,
  filter,
  onlyStationary,
  onSamplePid,
}: {
  proc: SampledProcess;
  filter: string;
  onlyStationary: boolean;
  onSamplePid?: (pid: number) => void;
}) {
  const q = filter.trim().toLowerCase();

  const shownThreads = useMemo(() => {
    return proc.threads.filter((t) => {
      if (onlyStationary && !t.stationary) return false;
      if (!q) return true;
      return (
        t.name.toLowerCase().includes(q) ||
        String(t.tid).includes(q) ||
        t.wchan.toLowerCase().includes(q) ||
        (t.syscall?.name.toLowerCase().includes(q) ?? false) ||
        (t.syscall?.fdTarget.toLowerCase().includes(q) ?? false) ||
        t.userFrames.some((f) => f.symbol.toLowerCase().includes(q) || f.module.toLowerCase().includes(q)) ||
        t.kernelFrames.some((f) => f.toLowerCase().includes(q))
      );
    });
  }, [proc.threads, onlyStationary, q]);

  return (
    <div className="rounded-xl border border-border bg-background/45 p-3.5">
      <ProcessHeader proc={proc} onSamplePid={onSamplePid} />
      <div className="mt-2.5">
        {shownThreads.length === 0 ? (
          <div className="py-3 font-mono text-[11px] text-faint">No threads match the current filter.</div>
        ) : (
          <div className="max-h-[480px] space-y-2 overflow-auto">
            {shownThreads.map((t) => (
              <ThreadRow key={t.tid} thread={t} />
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

function CallTreeRow({ node, depth, filter }: { node: LocalCallTreeNode; depth: number; filter: string }) {
  const [open, setOpen] = useState(depth < 6);
  const hasChildren = node.children.length > 0;
  const matched = filter !== "" && (node.symbol.toLowerCase().includes(filter) || node.module.toLowerCase().includes(filter));

  const badgeCls =
    node.frameType === "kernel"
      ? "bg-frost-wash text-frost"
      : node.frameType === "syscall"
        ? "bg-[color-mix(in_oklab,var(--warning)_16%,transparent)] text-warning-ink"
        : "bg-ember-wash text-primary";

  return (
    <div>
      <div
        className={`flex min-w-max items-center gap-2 rounded px-1.5 py-0.5 hover:bg-muted/50 ${matched ? "bg-[color-mix(in_oklab,var(--warning)_18%,transparent)]" : ""}`}
        style={{ paddingLeft: `${Math.min(depth, 18) * 16 + 4}px` }}
      >
        {hasChildren ? (
          <button type="button" onClick={() => setOpen((v) => !v)} className="w-3 shrink-0 text-faint hover:text-foreground">
            {open ? "▾" : "▸"}
          </button>
        ) : (
          <span className="w-3 shrink-0 text-center text-faint">·</span>
        )}
        <span className="num w-14 shrink-0 text-right text-muted-foreground">
          {node.samples} <span className="text-[10px] text-faint">({node.pct.toFixed(0)}%)</span>
        </span>
        <span className={`shrink-0 rounded px-1 py-px text-[9.5px] ${badgeCls}`}>{node.frameType || "user"}</span>
        <span className="whitespace-nowrap text-foreground" title={node.symbol}>
          {node.symbol}
        </span>
        {node.module && <span className="ml-auto shrink-0 pl-2 text-[10px] text-faint">{node.module}</span>}
      </div>
      {open &&
        hasChildren &&
        node.children.map((ch, i) => <CallTreeRow key={`${ch.symbol}-${i}`} node={ch} depth={depth + 1} filter={filter} />)}
    </div>
  );
}

function ThreadRow({ thread }: { thread: SampledThread }) {
  const [expanded, setExpanded] = useState(thread.stationary || thread.state === "D" || thread.state === "R");
  const stateSummary = Object.entries(thread.stateCounts)
    .sort((a, b) => b[1] - a[1])
    .map(([k, v]) => `${v}/${thread.totalPolls} ${k}`)
    .join(", ");

  const stateTone =
    thread.state === "D"
      ? "bg-[color-mix(in_oklab,var(--critical)_15%,transparent)] text-critical"
      : thread.state === "R"
        ? "bg-ember-wash text-primary"
        : "bg-muted text-muted-foreground";

  return (
    <div className={`rounded-lg border p-2.5 font-mono text-[11px] ${thread.state === "D" ? "border-critical/40 bg-[color-mix(in_oklab,var(--critical)_6%,transparent)]" : "border-border bg-card/60"}`}>
      <div className="flex flex-wrap items-center gap-2">
        <button type="button" onClick={() => setExpanded((v) => !v)} className="text-faint hover:text-foreground">
          {expanded ? "▾" : "▸"}
        </button>
        <span className={`rounded px-1.5 py-0.5 text-[10px] ${stateTone}`}>{thread.state}</span>
        <span className="font-semibold text-foreground">
          TID {thread.tid} <span className="font-normal text-muted-foreground">({thread.name})</span>
        </span>
        {thread.stationary && (
          <span className="rounded-full border border-warning/40 bg-[color-mix(in_oklab,var(--warning)_14%,transparent)] px-2 py-px text-[9.5px] text-warning-ink">
            STATIONARY {thread.pollsSameStack}/{thread.totalPolls}
          </span>
        )}
        {stateSummary && <span className="text-[10px] text-faint">{stateSummary}</span>}
        {thread.wchan && thread.wchan !== "0" && <span className="text-frost">wchan: {thread.wchan}</span>}
        <span className="num ml-auto text-[10.5px] text-muted-foreground">
          cpu +{(thread.cpuUserDeltaMs + thread.cpuSysDeltaMs).toFixed(1)}ms
          {thread.schedWaitDeltaMs > 0.5 && <span className="text-warning-ink"> · rq-wait +{thread.schedWaitDeltaMs.toFixed(1)}ms</span>}
          {" · "}ctx +{String(thread.voluntaryCtxtDelta)}/{String(thread.nonvoluntaryCtxtDelta)}
        </span>
      </div>

      {thread.syscall && thread.syscall.name && (
        <div className="mt-1.5 flex flex-wrap items-center gap-2 pl-5 text-[10.5px]">
          <span className="rounded bg-[color-mix(in_oklab,var(--warning)_14%,transparent)] px-1.5 py-px text-warning-ink">
            syscall: {thread.syscall.name}()
          </span>
          {thread.syscall.fdTarget && <span className="text-foreground">{thread.syscall.fdTarget}</span>}
          {thread.syscall.pc && <span className="text-faint">pc={thread.syscall.pc}</span>}
        </div>
      )}

      {expanded && (thread.userFrames.length > 0 || thread.kernelFrames.length > 0) && (
        <div className="mt-2 space-y-1.5 border-t border-border/60 pt-2 pl-5 text-[10.5px]">
          {thread.userFrames.length > 0 && (
            <div>
              <div className="mb-0.5 text-[9.5px] uppercase tracking-wider text-faint">
                User stack (leaf → root{thread.userStackSamples > 0 ? ` · ${thread.userStackSamples} snapshots` : ""})
              </div>
              {thread.userFrames.map((fr, i) => (
                <div key={i} className="flex flex-wrap items-baseline gap-2 leading-[1.55]">
                  <span className="w-6 shrink-0 text-right text-faint">#{i}</span>
                  <span className="text-foreground">{fr.symbol}</span>
                  {fr.module && <span className="text-muted-foreground">({fr.module})</span>}
                  {fr.source && <span className="text-frost">{fr.source}</span>}
                </div>
              ))}
            </div>
          )}
          {thread.kernelFrames.length > 0 && (
            <div>
              <div className="mb-0.5 text-[9.5px] uppercase tracking-wider text-frost/80">Kernel stack (/proc/task/stack)</div>
              {thread.kernelFrames.map((kf, i) => (
                <div key={i} className="flex items-baseline gap-2 leading-[1.5]">
                  <span className="w-6 shrink-0 text-right text-frost/60">[k]</span>
                  <span className="text-frost">{kf}</span>
                </div>
              ))}
            </div>
          )}
        </div>
      )}
    </div>
  );
}

// ---- Tab 2: Open Files, Sockets, Pipes, Epoll & Locks ----------------------

function FilesAndLocksTab({ files, locks }: { files: SampledOpenFile[]; locks: FileLock[] }) {
  return (
    <div className="space-y-4">
      {locks.length > 0 && (
        <div className="space-y-1.5">
          <div className="eyebrow">Active File Locks (/proc/locks · {locks.length})</div>
          <div className="overflow-auto rounded-lg border border-border">
            <table className="w-full border-collapse font-mono text-[11px]">
              <thead className="bg-card text-left text-[10px] uppercase tracking-wider text-muted-foreground">
                <tr className="border-b border-border">
                  <th className="px-2.5 py-1.5">Status</th>
                  <th className="px-2.5 py-1.5">PID</th>
                  <th className="px-2.5 py-1.5">Lock</th>
                  <th className="px-2.5 py-1.5">Range</th>
                  <th className="px-2.5 py-1.5">Target / Inode</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border">
                {locks.map((lk, i) => (
                  <tr key={i} className={lk.blocked ? "bg-[color-mix(in_oklab,var(--critical)_10%,transparent)]" : "hover:bg-muted/40"}>
                    <td className="px-2.5 py-1.5">
                      {lk.blocked ? (
                        <span className="rounded bg-[color-mix(in_oklab,var(--critical)_20%,transparent)] px-1.5 py-0.5 text-[10px] font-semibold text-critical">
                          BLOCKED →
                        </span>
                      ) : (
                        <span className="rounded bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground">HELD</span>
                      )}
                    </td>
                    <td className="num px-2.5 py-1.5 text-foreground">
                      {lk.pid} <span className="text-faint">{lk.comm}</span>
                    </td>
                    <td className="px-2.5 py-1.5 text-muted-foreground">
                      {lk.kind} {lk.access} ({lk.mode})
                    </td>
                    <td className="num px-2.5 py-1.5 text-muted-foreground">{lk.range}</td>
                    <td className="px-2.5 py-1.5 text-foreground">{lk.target || `inode ${lk.inode}`}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}

      <FilesTable files={files} empty="No open file descriptors captured." />
    </div>
  );
}

// ---- Tab 3: Cgroup v2, PSI & Hardware PMU Counters -------------------------

function SystemAndPmuTab({ sample }: { sample: ProcessSample }) {
  const cg = sample.cgroupStats;
  const maps = sample.procs
    .filter((p) => p.memoryMap || p.limits)
    .map((p) => ({ ...p, topRegions: p.memoryMap?.topRegions ?? [] }));
  return (
    <div className="space-y-5 font-mono text-[11.5px]">
      {cg?.end ? <CgroupView stats={cg.end} window={cg} /> : <Empty>No cgroup v2 stats available for this sample.</Empty>}

      {sample.perfCounters.length > 0 && (
        <div className="space-y-2">
          <div className="eyebrow">Hardware & Software PMU Counters (perf stat)</div>
          <PmuCountersGrid counters={sample.perfCounters} />
        </div>
      )}

      {maps.length > 0 && (
        <div className="space-y-2">
          <div className="eyebrow">Per-Process Memory Maps & Security Context</div>
          <MemoryMapsList maps={maps} maxRegions={8} />
        </div>
      )}
    </div>
  );
}
