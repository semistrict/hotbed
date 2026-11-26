import { Fragment, useMemo, useState } from "react";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  type FileLock,
  type FoldedStack,
  type PerfCounter,
  type ProcessSample,
  type ProfileFrame,
  type SampledCgroup,
  type SampledOpenFile,
  type SampledProcess,
  type SampledThread,
} from "../api";
import { bytes, hhmmss, signedBytes } from "../lib/format";
import { SegmentedBar } from "./charts/Bars";
import { FlameGraph } from "./FlameGraph";
import { Empty } from "./Panel";

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
        <SampleMetric
          label="Processes / Threads"
          value={`${sample.procs.length} / ${counts.threads}`}
          sub={
            counts.dState > 0
              ? `${counts.dState} in D-state · ${counts.stationary} stationary`
              : `${counts.stationary}/${counts.threads} stationary`
          }
          tone={counts.dState > 0 ? "critical" : "default"}
        />
        <SampleMetric
          label="CPU in window"
          value={`${(sample.cgroupStats?.cpuCoresAvg ?? counts.cpuDeltaMs / Math.max(1, Number(sample.durationMs))).toFixed(2)} cores`}
          sub={`Δ${counts.cpuDeltaMs.toFixed(0)} ms · ${counts.ctxtDelta.toLocaleString()} ctx sw`}
          tone="ember"
        />
        <SampleMetric
          label="I/O in window"
          value={bytes(counts.ioRead + counts.ioWrite)}
          sub={`R ${bytes(counts.ioRead)} · W ${bytes(counts.ioWrite)}${counts.advancingFiles > 0 ? ` · ${counts.advancingFiles} active FD` : ""}`}
        />
        <SampleMetric
          label="Open FDs & Locks"
          value={`${sample.files.length} FDs`}
          sub={`${sample.locks.length} locks${counts.blockedLocks > 0 ? ` (${counts.blockedLocks} blocked!)` : ""}${counts.queuedSockets > 0 ? ` · ${counts.queuedSockets} queued sock` : ""}`}
          tone={counts.blockedLocks > 0 ? "critical" : "default"}
        />
        <SampleMetric
          label="Memory & PSI"
          value={bytes(sample.cgroupStats?.end?.memoryCurrentBytes ?? sample.procs.reduce((a, p) => a + Number(p.rssBytes), 0))}
          sub={
            sample.cgroupStats
              ? `PSI io +${(Number(sample.cgroupStats.psiIoSomeDeltaUsec) / 1000).toFixed(0)}ms · cpu +${(Number(sample.cgroupStats.psiCpuSomeDeltaUsec) / 1000).toFixed(0)}ms`
              : `${sample.hostname} · ${sample.arch}`
          }
        />
      </div>

      {/* Primary tabs */}
      <div className="flex flex-wrap items-center gap-1.5 border-b border-border pb-2.5 font-mono text-[11.5px]">
        {(
          [
            ["stacks", `Call Graph & Threads (${counts.threads})`],
            ["files", `Open Files, Sockets & Locks (${sample.files.length}${sample.locks.length ? ` · ${sample.locks.length} locks` : ""})`],
            ["system", `Cgroup, PSI & PMU${sample.perfCounters.length ? ` (${sample.perfCounters.length})` : ""}`],
          ] as const
        ).map(([k, label]) => (
          <button
            key={k}
            type="button"
            onClick={() => setTab(k)}
            className={`rounded-lg border px-3 py-1 transition-colors ${
              tab === k
                ? "border-primary bg-ember-wash text-primary"
                : "border-border bg-background/50 text-muted-foreground hover:border-border-strong hover:text-foreground"
            }`}
          >
            {label}
          </button>
        ))}
      </div>

      {/* Tab content */}
      {tab === "stacks" && <StacksAndThreadsTab sample={sample} onSamplePid={onSamplePid} />}
      {tab === "files" && <FilesAndLocksTab files={sample.files} locks={sample.locks} />}
      {tab === "system" && <SystemAndPmuTab cg={sample.cgroupStats ?? null} counters={sample.perfCounters} procs={sample.procs} />}
    </div>
  );
}

function SampleMetric({ label, value, sub, tone = "default" }: { label: string; value: string; sub: string; tone?: "default" | "ember" | "critical" }) {
  const valCls = tone === "critical" ? "text-critical" : tone === "ember" ? "text-primary" : "text-foreground";
  return (
    <div className="rounded-lg border border-border bg-background/50 px-3 py-2">
      <div className="text-[10px] text-muted-foreground">{label}</div>
      <div className={`num mt-0.5 text-[15px] font-semibold ${valCls}`}>{value}</div>
      <div className="mt-0.5 truncate text-[10px] text-faint" title={sub}>
        {sub}
      </div>
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
  const [kindFilter, setKindFilter] = useState<string>("all");
  const [search, setSearch] = useState("");

  const counts = useMemo(() => {
    const c: Record<string, number> = { all: files.length, advancing: 0, flagged: 0 };
    for (const f of files) {
      c[f.kind] = (c[f.kind] ?? 0) + 1;
      if (f.posDelta !== 0n) c.advancing = (c.advancing ?? 0) + 1;
      if (f.txQueueBytes > 0n || f.rxQueueBytes > 0n || f.lockInfo) c.flagged = (c.flagged ?? 0) + 1;
    }
    return c;
  }, [files]);

  const shown = useMemo(() => {
    const q = search.trim().toLowerCase();
    return files.filter((f) => {
      if (kindFilter === "advancing" && f.posDelta === 0n) return false;
      else if (kindFilter === "flagged" && f.txQueueBytes === 0n && f.rxQueueBytes === 0n && !f.lockInfo) return false;
      else if (kindFilter !== "all" && kindFilter !== "advancing" && kindFilter !== "flagged" && f.kind !== kindFilter) return false;
      if (!q) return true;
      return (
        f.target.toLowerCase().includes(q) ||
        f.comm.toLowerCase().includes(q) ||
        f.localAddr.toLowerCase().includes(q) ||
        f.remoteAddr.toLowerCase().includes(q) ||
        f.socketState.toLowerCase().includes(q) ||
        f.peer.toLowerCase().includes(q) ||
        f.lockInfo.toLowerCase().includes(q)
      );
    });
  }, [files, kindFilter, search]);

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

      {files.length === 0 ? (
        <Empty>No open file descriptors captured.</Empty>
      ) : (
        <div className="space-y-2.5">
          <div className="flex flex-wrap items-center gap-2">
            <div className="flex flex-wrap gap-1">
              {(
                [
                  ["all", "all"],
                  ["file", "file"],
                  ["socket", "socket"],
                  ["pipe", "pipe"],
                  ["anon_inode", "anon_inode"],
                  ["deleted", "deleted"],
                  ["device", "device"],
                  ["advancing", "Δpos active"],
                  ["flagged", "queued / locked"],
                ] as const
              ).map(([k, label]) => {
                const n = counts[k] ?? 0;
                if (k !== "all" && n === 0) return null;
                return (
                  <Button
                    key={k}
                    type="button"
                    variant="outline"
                    size="xs"
                    onClick={() => setKindFilter(k)}
                    className={`font-mono text-[10.5px] ${
                      kindFilter === k ? "border-primary bg-ember-wash text-primary hover:bg-ember-wash" : "text-muted-foreground"
                    }`}
                  >
                    {label} ({n})
                  </Button>
                );
              })}
            </div>
            <Input
              type="search"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="Filter paths, sockets, peers, locks..."
              className="h-7 w-full bg-background/80 font-mono text-[11px] placeholder:text-faint sm:ml-auto sm:w-56"
            />
          </div>

          {shown.length === 0 ? (
            <Empty>No open file descriptors match the current filter.</Empty>
          ) : (
            <div className="max-h-[420px] overflow-auto rounded-lg border border-border">
              <table className="w-full border-collapse font-mono text-[11px]">
                <thead className="sticky top-0 bg-card text-left text-[10px] uppercase tracking-wider text-muted-foreground">
                  <tr className="border-b border-border">
                    <th className="px-2 py-1.5">PID</th>
                    <th className="px-2 py-1.5">FD</th>
                    <th className="px-2 py-1.5">Kind</th>
                    <th className="px-2 py-1.5">Mode</th>
                    <th className="px-2 py-1.5">Target / Peer / Watched FDs</th>
                    <th className="px-2 py-1.5 text-right">State / Queues / ΔOffset</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-border">
                  {shown.map((f, i) => (
                    <tr key={`${f.pid}-${f.fd}-${i}`} className="hover:bg-muted/40">
                      <td className="num px-2 py-1.5 text-muted-foreground whitespace-nowrap">
                        {f.pid} <span className="text-faint">{f.comm}</span>
                      </td>
                      <td className="num px-2 py-1.5 text-foreground">{f.fd}</td>
                      <td className="px-2 py-1.5">
                        <span
                          className={`rounded px-1.5 py-0.5 text-[10px] ${
                            f.kind === "socket"
                              ? "bg-frost-wash text-frost"
                              : f.kind === "deleted"
                                ? "bg-[color-mix(in_oklab,var(--critical)_15%,transparent)] text-critical"
                                : f.kind === "pipe"
                                  ? "bg-ember-wash text-primary"
                                  : "bg-muted text-muted-foreground"
                          }`}
                        >
                          {f.socketProto || f.kind}
                        </span>
                      </td>
                      <td className="px-2 py-1.5 text-faint">{f.mode || "—"}</td>
                      <td className="max-w-[460px] px-2 py-1.5">
                        <div className="truncate text-foreground" title={f.target}>
                          {f.localAddr ? `${f.localAddr}${f.remoteAddr && !f.remoteAddr.endsWith(":0") ? ` → ${f.remoteAddr}` : ""}` : f.target}
                        </div>
                        {(f.peer || f.epollTfds.length > 0 || f.lockInfo) && (
                          <div className="mt-0.5 flex flex-wrap gap-2 text-[10px]">
                            {f.peer && <span className="text-primary">↔ {f.peer}</span>}
                            {f.epollTfds.length > 0 && <span className="text-frost">watching fds: {f.epollTfds.join(", ")}</span>}
                            {f.lockInfo && <span className="text-warning-ink">lock: {f.lockInfo}</span>}
                          </div>
                        )}
                      </td>
                      <td className="num px-2 py-1.5 text-right whitespace-nowrap">
                        {f.socketState ? (
                          <div>
                            <span className={f.socketState === "LISTEN" ? "text-good-ink" : f.socketState === "ESTABLISHED" ? "text-frost" : "text-warning-ink"}>
                              {f.socketState}
                            </span>
                            {(f.txQueueBytes > 0n || f.rxQueueBytes > 0n) && (
                              <div className="text-[10px] text-warning-ink">
                                tx {bytes(f.txQueueBytes)} · rx {bytes(f.rxQueueBytes)}
                              </div>
                            )}
                          </div>
                        ) : f.pos > 0n || f.posDelta !== 0n ? (
                          <div>
                            <span className="text-muted-foreground">{bytes(f.pos)}</span>
                            {f.posDelta !== 0n && <span className="ml-1.5 text-good-ink">({signedBytes(f.posDelta)})</span>}
                          </div>
                        ) : (
                          <span className="text-faint">—</span>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

// ---- Tab 3: Cgroup v2, PSI & Hardware PMU Counters -------------------------

function SystemAndPmuTab({ cg, counters, procs }: { cg: SampledCgroup | null; counters: PerfCounter[]; procs: SampledProcess[] }) {
  return (
    <div className="space-y-5 font-mono text-[11.5px]">
      {cg && cg.end ? (
        <div className="space-y-4">
          <div className="grid grid-cols-2 gap-2.5 sm:grid-cols-4">
            <SampleMetric
              label="Cgroup CPU avg"
              value={`${cg.cpuCoresAvg.toFixed(2)} cores`}
              sub={`user +${(Number(cg.cpuUserDeltaUsec) / 1000).toFixed(1)}ms · sys +${(Number(cg.cpuSystemDeltaUsec) / 1000).toFixed(1)}ms`}
              tone="ember"
            />
            <SampleMetric
              label="CPU Throttled (Δ)"
              value={`+${cg.nrThrottledDelta}`}
              sub={`+${(Number(cg.throttledDeltaUsec) / 1000).toFixed(1)} ms in window (total ${cg.end.nrThrottled})`}
              tone={cg.nrThrottledDelta > 0n ? "critical" : "default"}
            />
            <SampleMetric
              label="Memory / Peak"
              value={bytes(cg.end.memoryCurrentBytes)}
              sub={`Δ${signedBytes(cg.memoryDeltaBytes)} · peak ${bytes(cg.end.memoryPeakBytes)} · swap ${bytes(cg.end.memorySwapBytes)}`}
            />
            <SampleMetric
              label="Page Faults (Δ)"
              value={`+${cg.pgfaultDelta.toLocaleString()}`}
              sub={`+${cg.pgmajfaultDelta.toLocaleString()} major · PIDs ${cg.end.pidsCurrent}/${cg.end.pidsPeak}`}
            />
          </div>

          <div className="grid grid-cols-1 gap-2.5 sm:grid-cols-3">
            <SampleMetric
              label="PSI CPU stall (Δ)"
              value={`+${(Number(cg.psiCpuSomeDeltaUsec) / 1000).toFixed(1)} ms`}
              sub={`some avg10 ${cg.end.psiCpuSomeAvg10.toFixed(2)}%`}
            />
            <SampleMetric
              label="PSI Memory stall (Δ)"
              value={`+${(Number(cg.psiMemSomeDeltaUsec) / 1000).toFixed(1)} ms`}
              sub={`full +${(Number(cg.psiMemFullDeltaUsec) / 1000).toFixed(1)}ms · avg10 ${cg.end.psiMemSomeAvg10.toFixed(2)}%`}
            />
            <SampleMetric
              label="PSI I/O stall (Δ)"
              value={`+${(Number(cg.psiIoSomeDeltaUsec) / 1000).toFixed(1)} ms`}
              sub={`full +${(Number(cg.psiIoFullDeltaUsec) / 1000).toFixed(1)}ms · avg10 ${cg.end.psiIoSomeAvg10.toFixed(2)}%`}
            />
          </div>

          <div>
            <div className="mb-1 text-[10px] uppercase tracking-wider text-muted-foreground">Cgroup memory breakdown</div>
            <SegmentedBar
              tall
              format={bytes}
              parts={[
                { id: "anon", label: "anon", value: Number(cg.end.memoryAnonBytes), color: "var(--series-2)" },
                { id: "file", label: "page cache", value: Number(cg.end.memoryFileBytes), color: "var(--series-1)" },
                { id: "kernel", label: "kernel", value: Number(cg.end.memoryKernelBytes), color: "var(--series-4)" },
                { id: "slab", label: "slab", value: Number(cg.end.memorySlabBytes), color: "var(--series-3)" },
                { id: "swap", label: "swap", value: Number(cg.end.memorySwapBytes), color: "var(--frost)" },
              ]}
            />
          </div>
        </div>
      ) : (
        <Empty>No cgroup v2 stats available for this sample.</Empty>
      )}

      {counters.length > 0 && (
        <div className="space-y-2">
          <div className="eyebrow">Hardware & Software PMU Counters (perf stat)</div>
          <div className="grid grid-cols-2 gap-2 sm:grid-cols-3 lg:grid-cols-5">
            {counters.map((c) => (
              <div key={c.name} className="rounded-lg border border-border bg-card/70 px-3 py-2">
                <div className="text-[10px] text-muted-foreground">{c.name}</div>
                <div className="num mt-1 text-[14px] font-semibold text-foreground">
                  {c.value}
                  {c.unit && <span className="ml-1 text-[10.5px] font-normal text-muted-foreground">{c.unit}</span>}
                </div>
                {c.note && (
                  <div className="mt-0.5 truncate text-[10px] text-faint" title={c.note}>
                    {c.note}
                  </div>
                )}
              </div>
            ))}
          </div>
        </div>
      )}

      {procs.some((p) => p.memoryMap || p.limits) && (
        <div className="space-y-2">
          <div className="eyebrow">Per-Process Memory Maps & Security Context</div>
          <div className="space-y-2">
            {procs.map((p) => (
              <Fragment key={p.pid}>
                <div className="rounded-lg border border-border bg-card/60 p-3 text-[11px]">
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <span className="font-semibold text-foreground">
                      PID {p.pid} ({p.comm})
                    </span>
                    <span className="num text-muted-foreground">
                      RSS {bytes(p.rssBytes)} · PSS {bytes(p.pssBytes)} · Private Dirty {bytes(p.privateDirtyBytes)} · Swap {bytes(p.swapBytes)}
                      {p.seccomp && ` · seccomp: ${p.seccomp}`}
                    </span>
                  </div>
                  {p.memoryMap && p.memoryMap.topRegions.length > 0 && (
                    <div className="mt-2 divide-y divide-border/60">
                      {p.memoryMap.topRegions.slice(0, 8).map((r, i) => (
                        <div key={i} className="flex items-center gap-3 py-0.5 text-[10.5px]">
                          <span className="w-10 shrink-0 text-faint">{r.perms}</span>
                          <span className="min-w-0 flex-1 truncate text-foreground">{r.path}</span>
                          <span className="num shrink-0 text-muted-foreground">{bytes(r.sizeBytes)}</span>
                        </div>
                      ))}
                    </div>
                  )}
                </div>
              </Fragment>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}
