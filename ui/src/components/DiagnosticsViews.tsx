import { useMemo, useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import type { CgroupStats, MemoryRegion, OpenFile, PerfCounter, SampledCgroup, SampledOpenFile } from "../api";
import { bytes, signedBytes } from "../lib/format";
import { SegmentedBar } from "./charts/Bars";
import { Empty, MetricTile } from "./Panel";

/**
 * An open file as either a live inspection or an activity sample reports it;
 * a sample adds how far the offset moved, socket queues, the peer, the FDs an
 * epoll watches and any lock held.
 */
export type AnyOpenFile = Omit<OpenFile, "$typeName"> &
  Partial<Pick<SampledOpenFile, "posDelta" | "txQueueBytes" | "rxQueueBytes" | "peer" | "epollTfds" | "lockInfo">>;

const KIND_FILTERS = [
  ["all", "all"],
  ["file", "file"],
  ["socket", "socket"],
  ["pipe", "pipe"],
  ["anon_inode", "anon_inode"],
  ["deleted", "deleted"],
  ["device", "device"],
  ["advancing", "Δpos active"],
  ["flagged", "queued / locked"],
] as const;

type KindFilter = (typeof KIND_FILTERS)[number][0];

const advancing = (f: AnyOpenFile) => (f.posDelta ?? 0n) !== 0n;
const queued = (f: AnyOpenFile) => (f.txQueueBytes ?? 0n) > 0n || (f.rxQueueBytes ?? 0n) > 0n;
const flagged = (f: AnyOpenFile) => queued(f) || Boolean(f.lockInfo);

function matchesKind(f: AnyOpenFile, k: KindFilter): boolean {
  if (k === "all") return true;
  if (k === "advancing") return advancing(f);
  if (k === "flagged") return flagged(f);
  return f.kind === k;
}

function matchesSearch(f: AnyOpenFile, q: string): boolean {
  return [f.target, f.comm, f.localAddr, f.remoteAddr, f.socketState, f.peer ?? "", f.lockInfo ?? ""].some((s) => s.toLowerCase().includes(q));
}

function kindTone(kind: string): string {
  if (kind === "socket") return "bg-frost-wash text-frost";
  if (kind === "deleted") return "bg-[color-mix(in_oklab,var(--critical)_15%,transparent)] text-critical";
  if (kind === "pipe") return "bg-ember-wash text-primary";
  return "bg-muted text-muted-foreground";
}

/** Open file descriptors: pills by kind (each with its count), a search, and a table. */
export function FilesTable({ files, empty = "No open file descriptors found." }: { files: readonly AnyOpenFile[]; empty?: string }) {
  const [kind, setKind] = useState<KindFilter>("all");
  const [search, setSearch] = useState("");

  const counts = useMemo(() => {
    const c: Partial<Record<KindFilter, number>> = {};
    for (const [k] of KIND_FILTERS) c[k] = files.filter((f) => matchesKind(f, k)).length;
    return c;
  }, [files]);

  const shown = useMemo(() => {
    const q = search.trim().toLowerCase();
    return files.filter((f) => matchesKind(f, kind) && (!q || matchesSearch(f, q)));
  }, [files, kind, search]);

  if (files.length === 0) return <Empty>{empty}</Empty>;
  return (
    <div className="space-y-2.5">
      <div className="flex flex-wrap items-center gap-2">
        <div className="flex flex-wrap gap-1">
          {KIND_FILTERS.map(([k, label]) => {
            const n = counts[k] ?? 0;
            if (k !== "all" && n === 0) return null;
            return (
              <Button
                key={k}
                type="button"
                variant="outline"
                size="xs"
                aria-pressed={kind === k}
                onClick={() => setKind(k)}
                className={`font-mono text-[10.5px] ${kind === k ? "border-primary bg-ember-wash text-primary hover:bg-ember-wash" : "text-muted-foreground"}`}
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
                <th className="px-2 py-1.5">Target / Endpoint</th>
                <th className="px-2 py-1.5 text-right">State / Offset</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-border">
              {shown.map((f, i) => (
                <tr key={`${f.pid}-${f.fd}-${i}`} className="hover:bg-muted/40">
                  <td className="num px-2 py-1.5 whitespace-nowrap text-muted-foreground">
                    {f.pid} <span className="text-faint">{f.comm}</span>
                  </td>
                  <td className="num px-2 py-1.5 text-foreground">{f.fd}</td>
                  <td className="px-2 py-1.5">
                    <span className={`rounded px-1.5 py-0.5 text-[10px] ${kindTone(f.kind)}`}>{f.socketProto || f.kind}</span>
                  </td>
                  <td className="px-2 py-1.5 text-faint">{f.mode || "—"}</td>
                  <td className="max-w-[460px] px-2 py-1.5">
                    <div className="truncate text-foreground" title={f.target}>
                      {f.localAddr ? `${f.localAddr}${f.remoteAddr && !f.remoteAddr.endsWith(":0") ? ` → ${f.remoteAddr}` : ""}` : f.target}
                    </div>
                    {(f.peer || (f.epollTfds?.length ?? 0) > 0 || f.lockInfo) && (
                      <div className="mt-0.5 flex flex-wrap gap-2 text-[10px]">
                        {f.peer && <span className="text-primary">↔ {f.peer}</span>}
                        {f.epollTfds && f.epollTfds.length > 0 && <span className="text-frost">watching fds: {f.epollTfds.join(", ")}</span>}
                        {f.lockInfo && <span className="text-warning-ink">lock: {f.lockInfo}</span>}
                      </div>
                    )}
                  </td>
                  <td className="num px-2 py-1.5 text-right whitespace-nowrap">
                    <FileState f={f} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

function FileState({ f }: { f: AnyOpenFile }) {
  if (f.socketState) {
    const tone = f.socketState === "LISTEN" ? "text-good-ink" : f.socketState === "ESTABLISHED" ? "text-frost" : "text-warning-ink";
    return (
      <div>
        <span className={tone}>{f.socketState}</span>
        {queued(f) && (
          <div className="text-[10px] text-warning-ink">
            tx {bytes(f.txQueueBytes ?? 0n)} · rx {bytes(f.rxQueueBytes ?? 0n)}
          </div>
        )}
      </div>
    );
  }
  if (f.pos > 0n || advancing(f)) {
    return (
      <div>
        <span className="text-muted-foreground">{bytes(f.pos)}</span>
        {advancing(f) && <span className="ml-1.5 text-good-ink">({signedBytes(f.posDelta ?? 0n)})</span>}
      </div>
    );
  }
  return <span className="text-faint">—</span>;
}

const ms = (usec: bigint, digits = 1) => `${(Number(usec) / 1000).toFixed(digits)}`;
const secs = (usec: bigint) => `${(Number(usec) / 1e6).toFixed(2)}s`;

/**
 * A cgroup v2's CPU, memory, faults and pressure: totals since it started, or,
 * given the `window` of an activity sample, what changed over that window.
 */
export function CgroupView({ stats, window }: { stats: CgroupStats; window?: SampledCgroup }) {
  const w = window;
  return (
    <div className="space-y-4 font-mono text-[11.5px]">
      <div className="grid grid-cols-2 gap-2.5 sm:grid-cols-4">
        {w ? (
          <>
            <MetricTile
              label="Cgroup CPU avg"
              value={`${w.cpuCoresAvg.toFixed(2)} cores`}
              sub={`user +${ms(w.cpuUserDeltaUsec)}ms · sys +${ms(w.cpuSystemDeltaUsec)}ms`}
              tone="ember"
            />
            <MetricTile
              label="CPU Throttled (Δ)"
              value={`+${w.nrThrottledDelta}`}
              sub={`+${ms(w.throttledDeltaUsec)} ms in window (total ${stats.nrThrottled})`}
              tone={w.nrThrottledDelta > 0n ? "critical" : "default"}
            />
          </>
        ) : (
          <>
            <MetricTile label="CPU total" value={secs(stats.cpuUsageUsec)} sub={`user ${secs(stats.cpuUserUsec)} · sys ${secs(stats.cpuSystemUsec)}`} />
            <MetricTile label="CPU throttled" value={String(stats.nrThrottled)} sub={`${ms(stats.throttledUsec)} ms`} />
          </>
        )}
        <MetricTile
          label="Memory / Peak"
          value={bytes(stats.memoryCurrentBytes)}
          sub={`${w ? `Δ${signedBytes(w.memoryDeltaBytes)} · ` : ""}peak ${bytes(stats.memoryPeakBytes)} · swap ${bytes(stats.memorySwapBytes)}`}
        />
        {w ? (
          <MetricTile
            label="Page Faults (Δ)"
            value={`+${w.pgfaultDelta.toLocaleString()}`}
            sub={`+${w.pgmajfaultDelta.toLocaleString()} major · PIDs ${stats.pidsCurrent}/${stats.pidsPeak}`}
          />
        ) : (
          <MetricTile
            label="PIDs / Faults"
            value={`${stats.pidsCurrent} / ${stats.pidsPeak}`}
            sub={`${stats.pgfault.toLocaleString()} minor · ${stats.pgmajfault.toLocaleString()} major`}
          />
        )}
      </div>

      <div className="grid grid-cols-1 gap-2.5 sm:grid-cols-3">
        {w ? (
          <>
            <MetricTile label="PSI CPU stall (Δ)" value={`+${ms(w.psiCpuSomeDeltaUsec)} ms`} sub={`some avg10 ${stats.psiCpuSomeAvg10.toFixed(2)}%`} />
            <MetricTile
              label="PSI Memory stall (Δ)"
              value={`+${ms(w.psiMemSomeDeltaUsec)} ms`}
              sub={`full +${ms(w.psiMemFullDeltaUsec)}ms · avg10 ${stats.psiMemSomeAvg10.toFixed(2)}%`}
            />
            <MetricTile
              label="PSI I/O stall (Δ)"
              value={`+${ms(w.psiIoSomeDeltaUsec)} ms`}
              sub={`full +${ms(w.psiIoFullDeltaUsec)}ms · avg10 ${stats.psiIoSomeAvg10.toFixed(2)}%`}
            />
          </>
        ) : (
          <>
            <MetricTile label="PSI CPU avg10" value={`${stats.psiCpuSomeAvg10.toFixed(2)}%`} sub={`full ${stats.psiCpuFullAvg10.toFixed(2)}%`} />
            <MetricTile label="PSI Memory avg10" value={`${stats.psiMemSomeAvg10.toFixed(2)}%`} sub={`full ${stats.psiMemFullAvg10.toFixed(2)}%`} />
            <MetricTile label="PSI IO avg10" value={`${stats.psiIoSomeAvg10.toFixed(2)}%`} sub={`full ${stats.psiIoFullAvg10.toFixed(2)}%`} />
          </>
        )}
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

/** Hardware and software PMU counters, as perf stat reports them. */
export function PmuCountersGrid({ counters }: { counters: readonly PerfCounter[] }) {
  return (
    <div className="grid grid-cols-2 gap-2 font-mono sm:grid-cols-3 lg:grid-cols-5">
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
  );
}

/** One process's memory: its totals, security context, and largest mapped regions. */
export interface ProcessMemory {
  pid: number;
  comm: string;
  rssBytes: bigint;
  pssBytes: bigint;
  privateDirtyBytes: bigint;
  swapBytes: bigint;
  seccomp?: string;
  topRegions: readonly MemoryRegion[];
}

/** Each process's memory totals and largest regions (at most `maxRegions` of them). */
export function MemoryMapsList({ maps, maxRegions }: { maps: readonly ProcessMemory[]; maxRegions?: number }) {
  if (maps.length === 0) return <Empty>No memory map data readable.</Empty>;
  return (
    <div className="max-h-[420px] space-y-2 overflow-auto font-mono text-[11px]">
      {maps.map((m) => (
        <div key={m.pid} className="space-y-2 rounded-lg border border-border bg-card/60 p-3">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <span className="font-semibold text-foreground">
              PID {m.pid} ({m.comm})
            </span>
            <span className="num text-muted-foreground">
              RSS {bytes(m.rssBytes)} · PSS {bytes(m.pssBytes)} · Private Dirty {bytes(m.privateDirtyBytes)} · Swap {bytes(m.swapBytes)}
              {m.seccomp && ` · seccomp: ${m.seccomp}`}
            </span>
          </div>
          {m.topRegions.length > 0 && (
            <div className="divide-y divide-border/70">
              {m.topRegions.slice(0, maxRegions).map((r, i) => (
                <div key={i} className="flex items-center gap-3 py-1 text-[10.5px]">
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
