import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Table } from "@/components/ui/table";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { clients, type ProfileBucket, type ProfileFrame } from "../api";
import { foldStack } from "../api/folded";
import { duration, hhmmss } from "../lib/format";
import { CopyButton } from "./CopyButton";
import { FlameGraph } from "./FlameGraph";
import { Card, Empty } from "./Panel";

type ProfileView = "flamegraph" | "top" | "folded";
type RangeKey = "all" | "1m" | "5m" | "15m";

const RANGE_MS: Record<RangeKey, number> = {
  "1m": 60_000,
  "5m": 5 * 60_000,
  "15m": 15 * 60_000,
  all: 0,
};

export interface ProfileTimeline {
  buckets: readonly ProfileBucket[];
  frames: readonly ProfileFrame[];
}

/**
 * Choose the timeline buckets and matching frame table to display.
 * When drilled into a single bucket, keep the full-range buckets paired with
 * the exact `frames` table that indexed their `topFrame` IDs, because filtered
 * responses renumber their own `frames` table.
 */
export function selectDisplayTimeline(
  windowBucket: ProfileBucket | null,
  rangeTimeline: ProfileTimeline | null,
  current: ProfileTimeline | null,
): ProfileTimeline {
  if (windowBucket && rangeTimeline && rangeTimeline.buckets.length > 1) {
    return rangeTimeline;
  }
  return current ?? { buckets: [], frames: [] };
}

export function resolveBucketTopFrame(bucket: ProfileBucket, frames: readonly ProfileFrame[]): string {
  return frames[bucket.topFrame]?.name ?? "";
}

const MAX_TIMELINE_BARS = 60;

/**
 * Downsample long timelines (e.g. 15m/30m profiles with 180–360 5s buckets)
 * into at most `maxBars` merged buckets so bars stay readable and never overflow.
 */
export function aggregateTimelineBuckets(
  buckets: readonly ProfileBucket[],
  maxBars = MAX_TIMELINE_BARS,
): ProfileBucket[] {
  if (buckets.length <= maxBars || maxBars < 1) return [...buckets];
  const groupSize = Math.ceil(buckets.length / maxBars);
  const out: ProfileBucket[] = [];
  for (let i = 0; i < buckets.length; i += groupSize) {
    const chunk = buckets.slice(i, i + groupSize);
    const first = chunk[0]!;
    const last = chunk[chunk.length - 1]!;
    let samples = 0n;
    let userSamples = 0n;
    let kernelSamples = 0n;
    let bestBucket = first;
    for (const b of chunk) {
      samples += b.samples;
      userSamples += b.userSamples;
      kernelSamples += b.kernelSamples;
      if (b.samples > bestBucket.samples) bestBucket = b;
    }
    const spanMs = last.t + last.durationMs - first.t;
    out.push({
      ...first,
      t: first.t,
      durationMs: spanMs > 0n ? spanMs : first.durationMs * BigInt(chunk.length),
      samples,
      userSamples,
      kernelSamples,
      topFrame: bestBucket.topFrame,
    });
  }
  return out;
}

type ContinuousProfileProps =
  | { id: string; node?: undefined; live: boolean }
  | { node: string; id?: undefined; live: boolean };

export function ContinuousProfilePanel({ id, node, live }: ContinuousProfileProps) {
  const isNode = node !== undefined;
  const targetKey = isNode ? node : id;

  const [queryText, setQueryText] = useState("");
  const [debouncedQuery, setDebouncedQuery] = useState("");
  const [frameType, setFrameType] = useState("");
  const [comm, setComm] = useState("");
  const [range, setRange] = useState<RangeKey>("all");
  const [windowBucket, setWindowBucket] = useState<ProfileBucket | null>(null);
  const [rangeTimeline, setRangeTimeline] = useState<ProfileTimeline | null>(null);
  const [view, setView] = useState<ProfileView>("flamegraph");

  // Debounce profile search text so typing does not fire a backend RPC per keystroke.
  useEffect(() => {
    const timer = setTimeout(() => setDebouncedQuery(queryText), 250);
    return () => clearTimeout(timer);
  }, [queryText]);

  // Reset bucket selection when switching targets or range.
  useEffect(() => {
    setWindowBucket(null);
    setRangeTimeline(null);
  }, [targetKey, range]);

  const profileQuery = useQuery({
    queryKey: [isNode ? "node-profile" : "command-profile", targetKey, debouncedQuery, frameType, comm, isNode ? range : "all", windowBucket ? Number(windowBucket.t) : 0],
    enabled: !isNode || live,
    queryFn: () => {
      const fromMs = windowBucket ? windowBucket.t : isNode && RANGE_MS[range] > 0 ? BigInt(Date.now() - RANGE_MS[range]) : 0n;
      const toMs = windowBucket ? windowBucket.t + windowBucket.durationMs - 1n : 0n;
      return clients.workspaces.getProfile({
        target: isNode ? { case: "node", value: node } : { case: "commandId", value: id },
        query: debouncedQuery,
        frameType,
        comm,
        fromMs,
        toMs,
        limit: 200,
      });
    },
    refetchInterval: live ? 5000 : false,
    placeholderData: keepPreviousData,
  });

  const p = profileQuery.data;

  // Preserve the full timeline bar and its matching frame table while narrowed
  // to a single 5-second bucket so users can click between buckets without
  // losing the timeline or resolving topFrame against renumbered frames.
  useEffect(() => {
    if (!windowBucket && p?.buckets) {
      setRangeTimeline({ buckets: p.buckets, frames: p.frames });
    }
  }, [windowBucket, p]);

  const displayTimeline = selectDisplayTimeline(
    windowBucket,
    rangeTimeline,
    p ? { buckets: p.buckets, frames: p.frames } : null,
  );
  const displayBuckets = aggregateTimelineBuckets(displayTimeline.buckets);
  const hasActiveFilter = Boolean(debouncedQuery.trim() || frameType || comm || windowBucket);
  const sampleHz = p?.sampleHz || 20;
  const userPct = p && p.matchedSamples > 0n ? Math.round((100 * Number(p.userSamples)) / Number(p.matchedSamples)) : 0;
  const kernelPct = p && p.matchedSamples > 0n ? 100 - userPct : 0;

  const foldedText = p?.stacks.map((s) => `${foldStack(s, p.frames)} ${s.samples}`).join("\n") ?? "";

  const downloadFolded = () => {
    if (!foldedText) return;
    const blob = new Blob([foldedText + "\n"], { type: "text/plain;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = `hotbed-${targetKey}-profile.folded`;
    a.click();
    URL.revokeObjectURL(url);
  };

  const statusBadge = p ? (
    <div className="flex flex-wrap items-center gap-2 font-mono text-[10.5px]">
      {p.active ? (
        <Badge variant="secondary" className="gap-1.5 bg-ember-wash px-2.5 font-mono text-[10.5px] font-normal text-primary">
          {p.live && <span className="live-dot h-1.5 w-1.5 rounded-full bg-primary" />}
          {p.live ? "sampling" : "captured"} · {p.sampleHz} Hz
        </Badge>
      ) : (
        <Badge variant="secondary" className="px-2.5 font-mono text-[10.5px] font-normal text-muted-foreground">
          inactive
        </Badge>
      )}
      {p.totalSamples > 0n && (
        <Badge variant="outline" className="num px-2.5 font-mono text-[10.5px] font-normal text-muted-foreground">
          {p.matchedSamples === p.totalSamples ? `${p.totalSamples.toLocaleString()} samples` : `${p.matchedSamples.toLocaleString()} / ${p.totalSamples.toLocaleString()} samples`}
          {" · "}
          {userPct}% user / {kernelPct}% kernel
        </Badge>
      )}
      <Button
        type="button"
        variant="outline"
        size="xs"
        onClick={() => profileQuery.refetch()}
        className="font-mono text-[10.5px] text-muted-foreground hover:text-foreground"
      >
        Refresh
      </Button>
    </div>
  ) : null;

  return (
    <Card
      title="Continuous Profile"
      sub={
        isNode
          ? "OpenTelemetry eBPF profiler · host-wide · rolling 30m ring buffer across all processes"
          : "OpenTelemetry eBPF profiler · always on · queryable live and after exit"
      }
      right={statusBadge}
    >
      {isNode && !live ? (
        <Empty>Node {node} is offline; host-wide continuous profiling is available while the worker is online.</Empty>
      ) : (
        <div className="space-y-4">
          {/* Search, range (node mode), & view controls */}
          <div className="flex flex-wrap items-center gap-2">
            <Input
              type="search"
              value={queryText}
              onChange={(e) => setQueryText(e.target.value)}
              placeholder="Filter functions, symbols, modules..."
              aria-label="Filter profile functions"
              className="w-full bg-background/70 font-mono text-[11.5px] placeholder:text-faint sm:min-w-[220px] sm:flex-1"
            />
            {isNode && (
              <div className="max-w-full overflow-x-auto no-scrollbar">
                <Tabs
                  value={windowBucket ? "" : range}
                  onValueChange={(v) => {
                    setRange(v as RangeKey);
                    setWindowBucket(null);
                  }}
                >
                  <TabsList className="h-8 border border-border font-mono text-[11px]">
                    {(
                      [
                        ["1m", "1m"],
                        ["5m", "5m"],
                        ["15m", "15m"],
                        ["all", "All (30m)"],
                      ] as const
                    ).map(([k, label]) => (
                      <TabsTrigger key={k} value={k} className="px-2 font-mono text-[11px]">
                        {label}
                      </TabsTrigger>
                    ))}
                  </TabsList>
                </Tabs>
              </div>
            )}
            <div className="max-w-full overflow-x-auto no-scrollbar">
              <Tabs value={view} onValueChange={(v) => setView(v as ProfileView)}>
                <TabsList className="h-8 border border-border font-mono text-[11px]">
                  {(
                    [
                      ["flamegraph", "Flamegraph"],
                      ["top", "Top functions"],
                      ["folded", "Folded stacks"],
                    ] as const
                  ).map(([k, label]) => (
                    <TabsTrigger key={k} value={k} className="px-2.5 font-mono text-[11px]">
                      {label}
                    </TabsTrigger>
                  ))}
                </TabsList>
              </Tabs>
            </div>
          </div>

          {/* Runtime type & process comm filter chips */}
          {((p && (p.frameTypes.length > 0 || p.comms.length > 0)) || windowBucket) && (
            <div className="flex flex-wrap items-center gap-x-3 gap-y-2 text-[11px]">
              {p && p.frameTypes.length > 0 && (
                <div className="flex flex-wrap items-center gap-1">
                  <span className="font-mono text-[10px] uppercase tracking-wider text-faint">Runtime:</span>
                  <FilterChip active={frameType === ""} onClick={() => setFrameType("")}>
                    all
                  </FilterChip>
                  {p.frameTypes.map((ft) => (
                    <FilterChip key={ft} active={frameType === ft} onClick={() => setFrameType(frameType === ft ? "" : ft)}>
                      {ft}
                    </FilterChip>
                  ))}
                </div>
              )}
              {p && p.comms.length > 0 && (
                <div className="flex flex-wrap items-center gap-1">
                  <span className="font-mono text-[10px] uppercase tracking-wider text-faint">Process:</span>
                  <FilterChip active={comm === ""} onClick={() => setComm("")}>
                    all
                  </FilterChip>
                  {p.comms.slice(0, isNode ? 12 : 10).map((c) => (
                    <FilterChip key={c} active={comm === c} onClick={() => setComm(comm === c ? "" : c)}>
                      {c}
                    </FilterChip>
                  ))}
                </div>
              )}
              {windowBucket && (
                <button
                  type="button"
                  onClick={() => setWindowBucket(null)}
                  className="ml-auto rounded-md border border-primary/50 bg-ember-wash px-2 py-0.5 font-mono text-[10.5px] text-primary"
                >
                  Window {hhmmss(windowBucket.t)} (+{duration(Number(windowBucket.durationMs) / 1000)}) ×
                </button>
              )}
            </div>
          )}

          {/* Timeline buckets: 5 s, longer for a long command */}
          {displayBuckets.length > 1 && (
            <div>
              <div className="mb-1 flex flex-wrap items-center justify-between gap-2 font-mono text-[10px] text-faint">
                <span>SAMPLES OVER TIME ({duration(Number(displayBuckets[0]!.durationMs) / 1000)} buckets · click to filter)</span>
                <span>
                  {hhmmss(displayBuckets[0]!.t)} — {hhmmss(displayBuckets[displayBuckets.length - 1]!.t)}
                </span>
              </div>
              <TimelineBuckets buckets={displayBuckets} frames={displayTimeline.frames} selected={windowBucket} onSelect={(b) => setWindowBucket(windowBucket?.t === b.t ? null : b)} />
            </div>
          )}

          {/* Main view */}
          {profileQuery.isLoading ? (
            <div className="py-8 text-center font-mono text-[11.5px] text-muted-foreground">Loading OpenTelemetry eBPF profile...</div>
          ) : profileQuery.error ? (
            <div className="rounded-lg border border-critical/30 bg-[color-mix(in_oklab,var(--critical)_10%,transparent)] px-3 py-2 font-mono text-[11.5px] text-critical">
              {String(profileQuery.error)}
            </div>
          ) : !p || p.totalSamples === 0n ? (
            <Empty>
              {hasActiveFilter
                ? "No profile samples match the active filter or selected time window."
                : p?.statusNote
                  ? `Continuous profiler: ${p.statusNote}`
                  : isNode
                    ? `Waiting for host-wide eBPF stack samples (collected continuously at ${sampleHz} Hz while processes execute on CPU)...`
                    : live
                      ? `Waiting for eBPF stack samples (collected continuously at ${sampleHz} Hz while processes execute on CPU)...`
                      : "No continuous eBPF samples were recorded for this command."}
            </Empty>
          ) : hasActiveFilter && p.matchedSamples === 0n ? (
            <Empty>No profile samples match the active filter ({p.totalSamples.toLocaleString()} total samples in range).</Empty>
          ) : view === "flamegraph" ? (
            <FlameGraph stacks={p.stacks} frames={p.frames} highlight={queryText} />
          ) : view === "top" ? (
            <Table
              containerClassName="max-h-[420px] overflow-auto rounded-lg border border-border"
              filterPlaceholder="Filter functions…"
              className="w-full border-collapse font-mono text-[11.5px]"
            >
              <thead className="sticky top-0 bg-card text-left text-[10px] uppercase tracking-wider text-muted-foreground">
                <tr className="border-b border-border">
                  <th className="px-3 py-2 text-right">Self %</th>
                  <th className="px-3 py-2 text-right">Total %</th>
                  <th className="px-3 py-2 text-right">Samples</th>
                  <th className="px-3 py-2">Runtime</th>
                  <th className="px-3 py-2">Function</th>
                  <th className="px-3 py-2">Module</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border">
                {p.topFunctions.map((fn, i) => {
                  const fr = p.frames[fn.frame];
                  const name = fr?.name ?? "[unknown]";
                  const module = fr?.module ?? "";
                  const frameType = fr?.frameType ?? "";
                  const sourceFile = fr?.sourceFile ?? "";
                  const sourceLine = fr?.sourceLine ?? 0;
                  return (
                    <tr key={`${name}-${i}`} className="hover:bg-muted/40">
                      <td className="num px-3 py-1.5 text-right text-primary">{fn.selfPct.toFixed(1)}%</td>
                      <td className="num px-3 py-1.5 text-right text-foreground">{fn.totalPct.toFixed(1)}%</td>
                      <td className="num px-3 py-1.5 text-right text-muted-foreground">
                        {String(fn.selfSamples)} / {String(fn.totalSamples)}
                      </td>
                      <td className="px-3 py-1.5">
                        <RuntimePill type={frameType} />
                      </td>
                      <td className="max-w-[380px] truncate px-3 py-1.5 text-foreground" title={name}>
                        {name}
                      </td>
                      <td className="max-w-[160px] truncate px-3 py-1.5 text-muted-foreground" title={sourceFile ? `${sourceFile}:${sourceLine}` : module}>
                        {module || "—"}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </Table>
          ) : (
            <div className="space-y-2">
              <div className="flex items-center justify-end gap-2">
                <CopyButton text={foldedText && `${foldedText}\n`} label="Copy folded" />
                <Button
                  type="button"
                  variant="outline"
                  size="xs"
                  disabled={!foldedText}
                  onClick={downloadFolded}
                  className="font-mono text-[10.5px] text-muted-foreground hover:text-foreground"
                >
                  Download .folded
                </Button>
              </div>
              <pre className="max-h-[360px] overflow-auto rounded-lg border border-border bg-background/60 p-3 font-mono text-[11px] leading-relaxed text-foreground/85">
                {foldedText || "(no matching stacks)"}
              </pre>
            </div>
          )}
        </div>
      )}
    </Card>
  );
}

function FilterChip({ active, onClick, children }: { active: boolean; onClick: () => void; children: string }) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`rounded-md border px-2 py-0.5 font-mono text-[10.5px] transition-colors ${
        active ? "border-primary bg-ember-wash text-primary" : "border-border bg-background/50 text-muted-foreground hover:border-border-strong hover:text-foreground"
      }`}
    >
      {children}
    </button>
  );
}

function RuntimePill({ type }: { type: string }) {
  const cls =
    type === "kernel"
      ? "bg-frost-wash text-frost"
      : type === "go" || type === "python" || type === "v8"
        ? "bg-[color-mix(in_oklab,var(--series-3)_15%,transparent)] text-good-ink"
        : "bg-ember-wash text-primary";
  return <span className={`inline-block rounded px-1.5 py-0.5 font-mono text-[10px] ${cls}`}>{type || "native"}</span>;
}

export function TimelineBuckets({
  buckets,
  frames,
  selected,
  onSelect,
}: {
  buckets: readonly ProfileBucket[];
  frames: readonly ProfileFrame[];
  selected: ProfileBucket | null;
  onSelect: (b: ProfileBucket) => void;
}) {
  // `buckets` arrive aggregated already (displayBuckets).
  const shown = buckets;
  const maxS = Math.max(1, ...shown.map((b) => Number(b.samples)));
  const gapCls = shown.length > 30 ? "gap-px" : "gap-1";
  return (
    <div className={`flex h-12 items-end overflow-hidden rounded-lg border border-border bg-background/50 px-2 py-1.5 ${gapCls}`}>
      {shown.map((b) => {
        const samples = Number(b.samples);
        const totalPct = samples > 0 ? Math.max(8, (samples / maxS) * 100) : 0;
        const kernelShare = samples > 0 ? (Number(b.kernelSamples) / samples) * 100 : 0;
        const isSel = selected?.t === b.t;
        const topFunction = resolveBucketTopFrame(b, frames);
        return (
          <button
            key={String(b.t)}
            type="button"
            onClick={() => onSelect(b)}
            title={`${hhmmss(b.t)} · ${samples} samples (${b.userSamples} user, ${b.kernelSamples} kernel)${topFunction ? ` · top: ${topFunction}` : ""}`}
            className={`group flex h-full min-w-0 flex-1 flex-col justify-end rounded-xs transition-opacity ${isSel ? "ring-1 ring-primary" : "opacity-85 hover:opacity-100"}`}
          >
            {samples === 0 ? (
              <div className="h-[2px] w-full rounded-full bg-muted-foreground/25" />
            ) : (
              <div className="flex w-full flex-col overflow-hidden rounded-t-[2px]" style={{ height: `${totalPct}%` }}>
                {kernelShare > 0 && <div style={{ height: `${kernelShare}%`, background: "var(--frost)" }} />}
                <div className="flex-1" style={{ background: "var(--primary)" }} />
              </div>
            )}
          </button>
        );
      })}
    </div>
  );
}
