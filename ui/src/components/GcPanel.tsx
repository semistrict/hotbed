import { CheckCircle2, Play, RefreshCw, Search, Trash2 } from "lucide-react";
import { useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Table } from "@/components/ui/table";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  nextSixHourCronMs,
  sortGcCells,
  summarizeGcKinds,
  useGcCollectCell,
  useGcDryRunCell,
  useGcStatus,
  useTriggerGcSweep,
  type GcCellSort,
  type GcCellUsage,
  type GcCollectCellResponse,
  type GcDryRunResponse,
} from "../lib/gc";
import { ago, bytes, compact, duration, hhmm } from "../lib/format";
import { formatUsd, r2StorageCost } from "../lib/r2";
import { SegmentedBar } from "./charts/Bars";
import { Card, Empty, StatTile } from "./Panel";

const INITIAL_CELL_ROWS = 15;

const GC_SORTS: readonly (readonly [GcCellSort, string])[] = [
  ["bytes", "size"],
  ["objects", "objs"],
  ["unmeasured", "pending"],
  ["oldest", "oldest"],
];

const KIND_COLORS: Record<string, string> = {
  segment: "var(--series-1)",
  checkpoint: "var(--series-2)",
  extent: "var(--series-3)",
  head: "var(--frost)",
  "gc-lease": "var(--faint)",
};

export function ClusterGcSection({ now }: { now: number }) {
  const [pollFastUntil, setPollFastUntil] = useState(0);
  const pollFast = pollFastUntil > now;
  const { data, isLoading, isFetching, isError, error, refetch } = useGcStatus(pollFast);
  const sweep = useTriggerGcSweep();
  const dryRun = useGcDryRunCell();
  const collect = useGcCollectCell();

  const [sort, setSort] = useState<GcCellSort>("bytes");
  const [filter, setFilter] = useState("");
  const [showAll, setShowAll] = useState(false);
  const [sweepBanner, setSweepBanner] = useState<string | null>(null);
  const [dryResult, setDryResult] = useState<GcDryRunResponse | null>(null);
  const [collectResult, setCollectResult] = useState<GcCollectCellResponse | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [busyCell, setBusyCell] = useState<string | null>(null);

  useEffect(() => {
    if (!pollFast) return;
    const remaining = Math.max(0, pollFastUntil - Date.now());
    const id = window.setTimeout(() => setPollFastUntil(0), remaining);
    return () => window.clearTimeout(id);
  }, [pollFast, pollFastUntil]);

  // Hide silently when the controller does not have a GC worker configured.
  if (data && !data.configured) return null;

  const handleSweep = () => {
    setActionError(null);
    setSweepBanner(null);
    sweep.mutate(undefined, {
      onSuccess: (res) => {
        setSweepBanner(`Enqueued ${res.enqueued} cells to ${data?.queue ?? "cladefs-gc-cells"} · auto-refreshing`);
        setPollFastUntil(Date.now() + 60_000);
      },
      onError: (err) => {
        setActionError(err instanceof Error ? err.message : String(err));
      },
    });
  };

  const handleDryRun = (cell: string) => {
    setActionError(null);
    setCollectResult(null);
    setBusyCell(cell);
    dryRun.mutate(cell, {
      onSuccess: (res) => {
        setDryResult(res);
        setBusyCell(null);
      },
      onError: (err) => {
        setActionError(`${cell}: ${err instanceof Error ? err.message : String(err)}`);
        setBusyCell(null);
      },
    });
  };

  const handleCollect = (cell: string) => {
    setActionError(null);
    setDryResult(null);
    setBusyCell(cell);
    collect.mutate(cell, {
      onSuccess: (res) => {
        setCollectResult(res);
        setBusyCell(null);
      },
      onError: (err) => {
        setActionError(`${cell}: ${err instanceof Error ? err.message : String(err)}`);
        setBusyCell(null);
      },
    });
  };

  const usage = data?.usage ?? [];
  const kinds = summarizeGcKinds(usage);
  const measuredTimes = usage.map((u) => u.measuredUnixMs).filter((t): t is number => t !== null);
  const newestSweep = measuredTimes.length > 0 ? Math.max(...measuredTimes) : null;
  const oldestSweep = measuredTimes.length > 0 ? Math.min(...measuredTimes) : null;
  const unmeasuredCount = Math.max(0, (data?.cells ?? 0) - (data?.measuredCells ?? 0));
  const monthlyStorageCost = r2StorageCost(data?.totalBytes ?? 0);
  const nextCronMs = nextSixHourCronMs(now);
  const nextInSec = Math.max(0, (nextCronMs - now) / 1000);

  const sortedCells = sortGcCells(usage, sort, filter);
  const visibleCells = showAll ? sortedCells : sortedCells.slice(0, INITIAL_CELL_ROWS);

  const barParts = kinds.slice(0, 4).map((k, idx) => ({
    id: k.kind,
    label: k.kind,
    value: k.bytes,
    color: KIND_COLORS[k.kind] ?? `var(--series-${(idx % 3) + 1})`,
  }));
  const otherKindBytes = kinds.slice(4).reduce((acc, k) => acc + k.bytes, 0);
  if (otherKindBytes > 0) {
    barParts.push({ id: "other", label: "other", value: otherKindBytes, color: "var(--faint)" });
  }

  return (
    <section className="col-span-12 space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="eyebrow">
          cladefs garbage collection · Cloudflare Worker (WASM) + Queue fan-out · {data?.schedule ?? "0 */6 * * *"}
        </h2>
        <div className="flex items-center gap-2">
          <Button
            type="button"
            variant="outline"
            size="xs"
            onClick={() => {
              setActionError(null);
              void refetch();
            }}
            disabled={isFetching}
            title="Refresh GC usage from R2 collector leases"
          >
            <RefreshCw className={`size-3 ${isFetching ? "animate-spin" : ""}`} />
            <span>Refresh</span>
          </Button>
          <Button
            type="button"
            size="xs"
            onClick={handleSweep}
            disabled={sweep.isPending || isLoading}
          >
            <Play className="size-3" />
            <span>{sweep.isPending ? "Enqueuing…" : "Sweep now"}</span>
          </Button>
        </div>
      </div>

      {isError && (
        <div className="rounded-xl border border-critical/40 bg-card/80 px-4 py-2.5 font-mono text-[11.5px] text-critical">
          Failed to reach GC worker: {error instanceof Error ? error.message : String(error)}
        </div>
      )}

      {data?.error && (
        <div className="rounded-xl border border-critical/40 bg-card/80 px-4 py-2.5 font-mono text-[11.5px] text-critical">
          GC worker error: {data.error}
        </div>
      )}

      {sweepBanner && (
        <div className="flex items-center justify-between gap-2 rounded-xl border border-sprout/40 bg-sprout/10 px-4 py-2 font-mono text-[11.5px] text-good-ink">
          <span className="flex items-center gap-2">
            <CheckCircle2 className="size-3.5 shrink-0" />
            <span>{sweepBanner}</span>
          </span>
          <button
            type="button"
            className="text-[10.5px] text-muted-foreground hover:text-foreground"
            onClick={() => setSweepBanner(null)}
          >
            dismiss
          </button>
        </div>
      )}

      {actionError && (
        <div className="rounded-xl border border-critical/40 bg-card/80 px-4 py-2 font-mono text-[11.5px] text-critical">
          {actionError}
        </div>
      )}

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-5">
        <StatTile
          label="R2 bucket size"
          value={isLoading ? "…" : bytes(data?.totalBytes ?? 0)}
          tone="default"
          foot={
            data
              ? `${compact(data.totalObjects)} objects · ${formatUsd(monthlyStorageCost)}/mo storage`
              : "loading bucket usage"
          }
        />
        <StatTile
          label="Cells"
          value={isLoading ? "…" : compact(data?.cells ?? 0)}
          tone={unmeasuredCount > 0 ? "ember" : "good"}
          foot={
            data
              ? unmeasuredCount > 0
                ? `${data.measuredCells} measured · ${unmeasuredCount} pending sweep`
                : `all ${data.measuredCells} measured`
              : "—"
          }
        />
        <StatTile
          label="Last sweep"
          value={newestSweep ? ago(newestSweep, now) : "—"}
          foot={
            newestSweep
              ? `newest ${hhmm(newestSweep)}${oldestSweep && oldestSweep !== newestSweep ? ` · oldest ${ago(oldestSweep, now)}` : ""}`
              : "no cells measured yet"
          }
        />
        <StatTile
          label="GC Worker"
          value={isLoading ? "…" : data?.healthy ? "online" : "offline"}
          tone={data?.healthy ? "good" : "ember"}
          foot={`${data?.bucket ?? "hotbed-cladefs"} · ${data?.queue ?? "cladefs-gc-cells"}`}
        />
        <StatTile
          label="Next scheduled"
          value={`in ${duration(nextInSec)}`}
          tone="frost"
          foot={`at ${hhmm(nextCronMs)} local · cron ${data?.schedule ?? "0 */6 * * *"}`}
        />
      </div>

      <div className="grid grid-cols-12 gap-4">
        <Card
          className="col-span-12 lg:col-span-5"
          title="Bucket breakdown by kind"
          sub={`Measured objects across ${data?.measuredCells ?? 0} cells in ${data?.bucket ?? "hotbed-cladefs"}`}
        >
          {kinds.length === 0 ? (
            <Empty>{isLoading ? "Loading bucket breakdown…" : "No measured cells yet — trigger a sweep to measure."}</Empty>
          ) : (
            <>
              <SegmentedBar tall format={bytes} parts={barParts} />
              <div className="mt-4">
                <Table className="w-full text-left font-mono text-[11px]" filterPlaceholder="Filter kinds…">
                  <thead>
                    <tr className="border-b border-border text-[9.5px] tracking-wider text-faint uppercase">
                      <th className="py-1.5 pr-2 font-medium">kind</th>
                      <th className="px-2 py-1.5 text-right font-medium">objects</th>
                      <th className="px-2 py-1.5 text-right font-medium">bytes</th>
                      <th className="py-1.5 pl-2 text-right font-medium">share</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-border/70">
                    {kinds.map((k) => {
                      const share = data && data.totalBytes > 0 ? (k.bytes / data.totalBytes) * 100 : 0;
                      return (
                        <tr key={k.kind} className="hover:bg-muted/40">
                          <td className="py-1.5 pr-2 text-foreground">{k.kind}</td>
                          <td className="num px-2 py-1.5 text-right text-muted-foreground">{compact(k.objects)}</td>
                          <td className="num px-2 py-1.5 text-right text-foreground">{bytes(k.bytes)}</td>
                          <td className="num py-1.5 pl-2 text-right text-muted-foreground">
                            {share >= 0.1 ? `${share.toFixed(1)}%` : "<0.1%"}
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </Table>
              </div>
            </>
          )}
        </Card>

        <Card
          className="col-span-12 lg:col-span-7"
          title="Cells in bucket"
          sub="Recorded from each cell's collector lease · dry-run or collect any cell on demand"
          right={
            <div className="flex flex-wrap items-center gap-1.5">
              <div className="relative">
                <Search className="pointer-events-none absolute top-1/2 left-2 size-3 -translate-y-1/2 text-faint" />
                <Input
                  value={filter}
                  onChange={(e) => setFilter(e.target.value)}
                  placeholder="Filter cell…"
                  className="h-7 w-32 pl-6 font-mono text-[11px] sm:w-40"
                />
              </div>
              <Tabs value={sort} onValueChange={(v) => setSort(v as GcCellSort)}>
                <TabsList aria-label="Sort cells" className="h-7 border border-border bg-background/60">
                  {GC_SORTS.map(([key, label]) => (
                    <TabsTrigger key={key} value={key} className="px-2 font-mono text-[10px]">
                      {label}
                    </TabsTrigger>
                  ))}
                </TabsList>
              </Tabs>
            </div>
          }
        >
          {dryResult && (
            <div className="mb-3 flex flex-wrap items-center justify-between gap-2 rounded-xl border border-border-strong bg-muted/40 px-3 py-2 font-mono text-[11px]">
              <div className="space-y-0.5">
                <div className="text-foreground">
                  Dry run <span className="text-primary">{dryResult.cell}</span>:{" "}
                  {dryResult.doomed.length === 0 && dryResult.retired.length === 0 ? (
                    <span className="text-good-ink">clean (0 doomed objects)</span>
                  ) : (
                    <span>
                      {compact(dryResult.doomed.length)} doomed ({bytes(dryResult.doomedBytes)}) ·{" "}
                      {dryResult.retired.length} refs to retire
                    </span>
                  )}
                  {dryResult.fenced && <span className="ml-2 text-serious-ink">fenced</span>}
                </div>
              </div>
              <div className="flex items-center gap-1.5">
                {(dryResult.doomed.length > 0 || dryResult.retired.length > 0) && (
                  <Button
                    type="button"
                    size="xs"
                    onClick={() => handleCollect(dryResult.cell)}
                    disabled={busyCell !== null}
                  >
                    <Trash2 className="size-3" />
                    <span>Collect now</span>
                  </Button>
                )}
                <button
                  type="button"
                  onClick={() => setDryResult(null)}
                  className="px-1 text-[10.5px] text-muted-foreground hover:text-foreground"
                >
                  close
                </button>
              </div>
            </div>
          )}

          {collectResult && (
            <div className="mb-3 flex flex-wrap items-center justify-between gap-2 rounded-xl border border-sprout/40 bg-sprout/10 px-3 py-2 font-mono text-[11px] text-good-ink">
              <span>
                Collected <span className="font-semibold">{collectResult.cell}</span>:{" "}
                {collectResult.left
                  ? `left (${collectResult.left})`
                  : collectResult.skipped > 0
                    ? "unchanged (skipped)"
                    : `deleted ${compact(collectResult.deleted)} objs (${bytes(collectResult.deletedBytes)}) · retired ${collectResult.retired}${collectResult.removed > 0 ? " · cell removed" : ""}`}
              </span>
              <button
                type="button"
                onClick={() => setCollectResult(null)}
                className="text-[10.5px] text-muted-foreground hover:text-foreground"
              >
                close
              </button>
            </div>
          )}

          {visibleCells.length === 0 ? (
            <Empty>{isLoading ? "Loading cells…" : "No matching cells."}</Empty>
          ) : (
            <>
              <div className="overflow-x-auto">
                <table className="w-full text-left font-mono text-[11px]">
                  <thead>
                    <tr className="border-b border-border text-[9.5px] tracking-wider text-faint uppercase">
                      <th className="py-1.5 pr-2 font-medium">cell</th>
                      <th className="px-2 py-1.5 text-right font-medium">objects</th>
                      <th className="px-2 py-1.5 text-right font-medium">size</th>
                      <th className="px-2 py-1.5 text-right font-medium">checkpoint</th>
                      <th className="px-2 py-1.5 text-right font-medium">swept</th>
                      <th className="py-1.5 pl-2 text-right font-medium">actions</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-border/70">
                    {visibleCells.map((u: GcCellUsage) => {
                      const cp = u.kinds?.checkpoint;
                      const busy = busyCell === u.cell;
                      return (
                        <tr key={u.cell} className="hover:bg-muted/40">
                          <td className="py-1.5 pr-2 text-foreground">
                            <span>{u.cell}</span>
                            {u.measuredUnixMs === null && (
                              <span className="ml-1.5 rounded-full bg-primary/15 px-1.5 py-0.5 text-[9px] text-primary">
                                pending
                              </span>
                            )}
                          </td>
                          <td className="num px-2 py-1.5 text-right text-muted-foreground">
                            {u.objects !== null ? compact(u.objects) : "—"}
                          </td>
                          <td className="num px-2 py-1.5 text-right text-foreground">
                            {u.bytes !== null ? bytes(u.bytes) : "—"}
                          </td>
                          <td className="num px-2 py-1.5 text-right text-muted-foreground">
                            {cp ? `${cp.objects} · ${bytes(cp.bytes)}` : "—"}
                          </td>
                          <td className="num px-2 py-1.5 text-right text-muted-foreground">
                            {u.measuredUnixMs !== null ? ago(u.measuredUnixMs, now) : "never"}
                          </td>
                          <td className="py-1.5 pl-2 text-right">
                            <span className="inline-flex items-center gap-1">
                              <button
                                type="button"
                                disabled={busyCell !== null}
                                onClick={() => handleDryRun(u.cell)}
                                className="rounded px-1.5 py-0.5 text-[10px] text-muted-foreground transition-colors hover:bg-muted hover:text-foreground disabled:opacity-40"
                              >
                                {busy && dryRun.isPending ? "…" : "dry run"}
                              </button>
                              <button
                                type="button"
                                disabled={busyCell !== null}
                                onClick={() => handleCollect(u.cell)}
                                className="rounded px-1.5 py-0.5 text-[10px] text-primary transition-colors hover:bg-primary/15 disabled:opacity-40"
                              >
                                {busy && collect.isPending ? "…" : "collect"}
                              </button>
                            </span>
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
              {sortedCells.length > INITIAL_CELL_ROWS && (
                <div className="mt-3 flex justify-center">
                  <Button
                    type="button"
                    variant="ghost"
                    size="xs"
                    onClick={() => setShowAll((v) => !v)}
                  >
                    {showAll
                      ? `Show top ${INITIAL_CELL_ROWS}`
                      : `Show all ${sortedCells.length} cells`}
                  </Button>
                </div>
              )}
            </>
          )}
        </Card>
      </div>
    </section>
  );
}
