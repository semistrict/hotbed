import { Link } from "@tanstack/react-router";
import { BaseState, type Node, type ObjectStoreStats, type Snapshot } from "../api";
import { Meter, SegmentedBar } from "./charts/Bars";
import { Columns } from "./charts/Columns";
import { LineChart } from "./charts/LineChart";
import { Card, StatTile } from "./Panel";
import { bytes, bytesPerSec, compact, duration, percent } from "../lib/format";

// Published Cloudflare R2 Standard pricing (USD, gross before free tier; $0 egress).
const R2_STORAGE_PER_GB_MO = 0.015;
const R2_CLASS_A_PER_MILLION = 4.5;
const R2_CLASS_B_PER_MILLION = 0.36;

export interface R2MonthlyCostEstimate {
  storageBytes: number;
  storageCost: number;
  classAOpsMonth: number;
  classACost: number;
  classBOpsMonth: number;
  classBCost: number;
  opsCost: number;
  totalMonthly: number;
}

export interface ClusterObjectStoreSummary {
  hasData: boolean;
  uploaded24h: number;
  downloaded24h: number;
  puts24h: number;
  gets24h: number;
  failed24h: number;
  commitWait24hSec: number;
  sinceBootUp: number;
  sinceBootDown: number;
  sinceBootSegments: number;
  sinceBootExtents: number;
  sinceBootCheckpoints: number;
  sinceBootOther: number;
  sinceBootPageIn: number;
  sinceBootFetched: number;
  sinceBootFails: number;
  liveUpBps: number;
  liveDownBps: number;
  cacheHitRatio: number | null;
  partCacheBytes: number;
  partCacheCapacityBytes: number;
  extentCacheBytes: number;
  extentCacheCapacityBytes: number;
  extentCachePinnedEntries: number;
  extentCacheIdleEntries: number;
  cacheTrims: number;
  activeVms: number;
  windowMappedBytes: number;
  daxWindowFull: number;
  daxWindowReclaims: number;
  daxRegionsFull: number;
  daxWritesHeldBySealsSec: number;
  daxDeviceReadBytes: number;
  daxDeviceReadsSec: number;
  cost: R2MonthlyCostEstimate;
}

/**
 * Estimates gross monthly Cloudflare R2 spend from 24h extrapolated Class A/B
 * request rates plus unique warm base storage (deduplicated by project+gen+arch).
 */
export function estimateR2MonthlyCost(s: Snapshot, puts24h: number, gets24h: number): R2MonthlyCostEstimate {
  const nodeArch = new Map(s.nodes.map((n) => [n.id, n.resources?.arch ?? ""]));
  const seenBase = new Map<string, number>();
  for (const b of s.bases) {
    if (b.state !== BaseState.WARM) continue;
    const arch = b.cladefsCell?.arch || nodeArch.get(b.nodeId) || "any";
    const key = `${b.project}:${b.generation}:${arch}`;
    const sz = Number(b.sizeBytes);
    const prev = seenBase.get(key) ?? 0;
    if (sz > prev) seenBase.set(key, sz);
  }
  let storageBytes = 0;
  for (const sz of seenBase.values()) storageBytes += sz;

  const classAOpsMonth = puts24h * 30;
  const classBOpsMonth = gets24h * 30;
  const storageCost = (storageBytes / 1e9) * R2_STORAGE_PER_GB_MO;
  const classACost = (classAOpsMonth / 1e6) * R2_CLASS_A_PER_MILLION;
  const classBCost = (classBOpsMonth / 1e6) * R2_CLASS_B_PER_MILLION;
  const opsCost = classACost + classBCost;
  return {
    storageBytes,
    storageCost,
    classAOpsMonth,
    classACost,
    classBOpsMonth,
    classBCost,
    opsCost,
    totalMonthly: storageCost + opsCost,
  };
}

export function formatUsd(n: number): string {
  if (!Number.isFinite(n) || n < 0) return "—";
  if (n === 0) return "$0.00";
  if (n < 0.01) return "<$0.01";
  return `$${n.toFixed(2)}`;
}

export function summarizeClusterObjectStore(s: Snapshot): ClusterObjectStoreSummary {
  let sinceBootUp = 0;
  let sinceBootDown = 0;
  let sinceBootSegments = 0;
  let sinceBootExtents = 0;
  let sinceBootCheckpoints = 0;
  let sinceBootOther = 0;
  let sinceBootPageIn = 0;
  let sinceBootFetched = 0;
  let sinceBootFails = 0;
  let partCacheBytes = 0;
  let partCacheCapacityBytes = 0;
  let extentCacheBytes = 0;
  let extentCacheCapacityBytes = 0;
  let extentCachePinnedEntries = 0;
  let extentCacheIdleEntries = 0;
  let cacheTrims = 0;
  let activeVms = 0;
  let windowMappedBytes = 0;
  let daxWindowFull = 0;
  let daxWindowReclaims = 0;
  let daxRegionsFull = 0;
  let daxWritesHeldBySealsNs = 0;
  let daxDeviceReadBytes = 0;
  let daxDeviceReadsNs = 0;
  let hasNodeStore = false;

  for (const n of s.nodes) {
    const os = n.objectStore;
    if (!os) continue;
    hasNodeStore = true;
    sinceBootUp += Number(os.uploadedBytes);
    sinceBootDown += Number(os.downloadedBytes);
    sinceBootSegments += Number(os.uploadedSegmentsBytes);
    sinceBootExtents += Number(os.uploadedExtentsBytes);
    sinceBootCheckpoints += Number(os.uploadedCheckpointsBytes);
    sinceBootOther += Number(os.uploadedOtherBytes);
    sinceBootPageIn += Number(os.pageInBytes);
    sinceBootFetched += Number(os.fetchedBytes);
    sinceBootFails += Number(os.failedRequests);
    partCacheBytes += Number(os.partCacheBytes);
    partCacheCapacityBytes += Number(os.partCacheCapacityBytes);
    extentCacheBytes += Number(os.extentCacheBytes);
    extentCacheCapacityBytes += Number(os.extentCacheCapacityBytes);
    extentCachePinnedEntries += Number(os.extentCachePinnedEntries);
    extentCacheIdleEntries += Number(os.extentCacheIdleEntries);
    cacheTrims += Number(os.cacheTrims);
    activeVms += os.activeVms;
    windowMappedBytes += Number(os.windowMappedBytes);
    daxWindowFull += Number(os.daxWindowFull);
    daxWindowReclaims += Number(os.daxWindowReclaims);
    daxRegionsFull += Number(os.daxRegionsFull);
    daxWritesHeldBySealsNs += Number(os.daxWritesHeldBySealsNs);
    daxDeviceReadBytes += Number(os.daxDeviceReadBytes);
    daxDeviceReadsNs += Number(os.daxDeviceReadsNs);
  }

  let liveUpBps = 0;
  let liveDownBps = 0;
  for (const n of s.nodes) {
    if (!n.online) continue;
    const pts = s.series[n.id]?.samples;
    const last = pts?.[pts.length - 1];
    if (last) {
      liveUpBps += last.uploadBytesPerSec;
      liveDownBps += last.downloadBytesPerSec;
    }
  }

  const t = s.totals24h;
  const uploaded24h = Number(t?.r2UploadBytes ?? 0n);
  const downloaded24h = Number(t?.r2DownloadBytes ?? 0n);
  const puts24h = Number(t?.r2PutRequests ?? 0n);
  const gets24h = Number(t?.r2GetRequests ?? 0n);
  const failed24h = Number(t?.r2FailedRequests ?? 0n);
  const commitWait24hSec = t?.r2CommitWaitSeconds ?? 0;

  const totalReads = sinceBootPageIn + sinceBootFetched;
  const cacheHitRatio = totalReads > 0 ? sinceBootPageIn / totalReads : null;
  const cost = estimateR2MonthlyCost(s, puts24h, gets24h);

  return {
    hasData: hasNodeStore || uploaded24h > 0 || downloaded24h > 0,
    uploaded24h,
    downloaded24h,
    puts24h,
    gets24h,
    failed24h,
    commitWait24hSec,
    sinceBootUp,
    sinceBootDown,
    sinceBootSegments,
    sinceBootExtents,
    sinceBootCheckpoints,
    sinceBootOther,
    sinceBootPageIn,
    sinceBootFetched,
    sinceBootFails,
    liveUpBps,
    liveDownBps,
    cacheHitRatio,
    partCacheBytes,
    partCacheCapacityBytes,
    extentCacheBytes,
    extentCacheCapacityBytes,
    extentCachePinnedEntries,
    extentCacheIdleEntries,
    cacheTrims,
    activeVms,
    windowMappedBytes,
    daxWindowFull,
    daxWindowReclaims,
    daxRegionsFull,
    daxWritesHeldBySealsSec: daxWritesHeldBySealsNs / 1e9,
    daxDeviceReadBytes,
    daxDeviceReadsSec: daxDeviceReadsNs / 1e9,
    cost,
  };
}

export function nodeCacheHit(os?: ObjectStoreStats): number | null {
  if (!os) return null;
  const local = Number(os.pageInBytes);
  const remote = Number(os.fetchedBytes);
  const total = local + remote;
  return total > 0 ? local / total : null;
}

const hourLabel = (t: number | bigint) => String(new Date(Number(t)).getHours()).padStart(2, "0");

export function ClusterObjectStoreSection({ s, colors }: { s: Snapshot; colors: Map<string, string> }) {
  const sum = summarizeClusterObjectStore(s);
  if (!sum.hasData) return null;

  const cladefsNodes = s.nodes.filter((n) => n.objectStore || (s.nodeHourly[n.id]?.hours ?? []).some((h) => h.uploadBytes > 0n || h.downloadBytes > 0n));
  const liveNodes = cladefsNodes.filter((n) => n.online && colors.has(n.id));
  const vmNodes = cladefsNodes.filter((n) => (n.objectStore?.activeVms ?? 0) > 0 || (n.objectStore?.windowMappedBytes ?? 0n) > 0n);
  const rateLines = (pick: (p: { uploadBytesPerSec: number; downloadBytesPerSec: number }) => number) =>
    liveNodes.map((n) => ({
      id: n.id,
      label: n.hostname,
      color: colors.get(n.id) ?? "var(--faint)",
      points: (s.series[n.id]?.samples ?? []).map((p) => ({ t: Number(p.t), v: pick(p) })),
    }));

  const daxThrashing = sum.daxWindowFull > 0 || sum.daxRegionsFull > 0;

  return (
    <section className="col-span-12 space-y-4">
      <h2 className="eyebrow">cladefs &amp; R2 object storage · uploads, caches, DAX health and estimated cost</h2>

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-6">
        <StatTile
          label="R2 uploaded (24h)"
          value={bytes(sum.uploaded24h)}
          tone={sum.liveUpBps > 1024 ? "ember" : "default"}
          foot={
            sum.liveUpBps > 0
              ? `now ${bytesPerSec(sum.liveUpBps)} · ${bytes(sum.sinceBootUp)} boot`
              : `${bytes(sum.sinceBootUp)} since daemon start`
          }
        />
        <StatTile
          label="R2 downloaded (24h)"
          value={bytes(sum.downloaded24h)}
          tone={sum.liveDownBps > 1024 ? "frost" : "default"}
          foot={
            sum.liveDownBps > 0
              ? `now ${bytesPerSec(sum.liveDownBps)} · ${bytes(sum.sinceBootDown)} boot`
              : `${bytes(sum.sinceBootDown)} since daemon start`
          }
        />
        <StatTile
          label="R2 requests (24h)"
          value={compact(sum.puts24h + sum.gets24h)}
          tone={sum.failed24h > 0 ? "ember" : "default"}
          foot={`${compact(sum.puts24h)} puts · ${compact(sum.gets24h)} gets${sum.failed24h > 0 ? ` · ${compact(sum.failed24h)} failed` : ""}`}
        />
        <StatTile
          label="Commit wait on R2"
          value={duration(sum.commitWait24hSec)}
          tone={sum.commitWait24hSec > 5 ? "ember" : "good"}
          foot="24h time blocked on R2 lag/sync"
        />
        <StatTile
          label="Local read hit"
          value={sum.cacheHitRatio !== null ? percent(sum.cacheHitRatio, 1) : "—"}
          tone={sum.cacheHitRatio !== null && sum.cacheHitRatio >= 0.9 ? "good" : "default"}
          foot={`${bytes(sum.sinceBootPageIn)} local · ${bytes(sum.sinceBootFetched)} R2`}
        />
        <StatTile
          label="Est. R2 / mo"
          value={formatUsd(sum.cost.totalMonthly)}
          foot={`ops ${formatUsd(sum.cost.opsCost)} · store ${formatUsd(sum.cost.storageCost)} · $0 egress`}
        />
      </div>

      <div className="grid grid-cols-12 gap-4">
        <Card className="col-span-12 lg:col-span-6" title="R2 upload rate" sub="Bytes/sec uploaded to R2 per node, last 15 minutes">
          <LineChart series={rateLines((p) => p.uploadBytesPerSec)} format={bytesPerSec} height={160} />
        </Card>
        <Card className="col-span-12 lg:col-span-6" title="R2 download rate" sub="Bytes/sec fetched from R2 per node, last 15 minutes">
          <LineChart series={rateLines((p) => p.downloadBytesPerSec)} format={bytesPerSec} height={160} />
        </Card>

        <Card className="col-span-12 lg:col-span-7" title="R2 transfer, per hour" sub="Uploaded and downloaded across all nodes, last 24 hours">
          <Columns
            columns={s.hourly.map((h) => ({
              key: String(h.hour),
              label: hourLabel(h.hour),
              values: [Number(h.uploadBytes), Number(h.downloadBytes)],
            }))}
            series={[
              { id: "up", label: "uploaded", color: "var(--series-1)" },
              { id: "down", label: "downloaded", color: "var(--series-2)" },
            ]}
            format={bytes}
            labelEvery={3}
            current={String(s.hourly[s.hourly.length - 1]?.hour)}
            height={170}
          />
        </Card>

        <Card className="col-span-12 lg:col-span-5" title="Upload mix & nodes" sub="Cumulative cladefsd counters since each daemon started">
          <SegmentedBar
            tall
            format={bytes}
            parts={[
              { id: "segments", label: "segments", value: sum.sinceBootSegments, color: "var(--series-1)" },
              { id: "extents", label: "extents", value: sum.sinceBootExtents, color: "var(--series-2)" },
              { id: "checkpoints", label: "checkpoints", value: sum.sinceBootCheckpoints, color: "var(--series-3)" },
              { id: "other", label: "other", value: sum.sinceBootOther, color: "var(--faint)" },
            ]}
          />
          <div className="mt-4 overflow-x-auto">
            <table className="w-full text-left font-mono text-[11px]">
              <thead>
                <tr className="border-b border-border text-[9.5px] tracking-wider text-faint uppercase">
                  <th className="py-1.5 pr-2 font-medium">node</th>
                  <th className="px-2 py-1.5 text-right font-medium">24h ↑</th>
                  <th className="px-2 py-1.5 text-right font-medium">24h ↓</th>
                  <th className="px-2 py-1.5 text-right font-medium">boot ↑</th>
                  <th className="px-2 py-1.5 text-right font-medium">wait</th>
                  <th className="py-1.5 pl-2 text-right font-medium">local</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border/70">
                {cladefsNodes.map((n) => {
                  const hrs = s.nodeHourly[n.id]?.hours ?? [];
                  const up24 = hrs.reduce((acc, h) => acc + Number(h.uploadBytes), 0);
                  const down24 = hrs.reduce((acc, h) => acc + Number(h.downloadBytes), 0);
                  const os = n.objectStore;
                  const hit = nodeCacheHit(os);
                  const waitSec = os ? Number(os.commitWaitNs) / 1e9 : 0;
                  return (
                    <tr key={n.id} className="hover:bg-muted/40">
                      <td className="py-1.5 pr-2">
                        <Link to="/nodes/$hostname" params={{ hostname: n.hostname }} className="flex items-center gap-1.5 text-foreground hover:text-primary">
                          <span className="inline-block size-1.5 rounded-full" style={{ background: colors.get(n.id) ?? "var(--faint)" }} />
                          <span>{n.hostname}</span>
                        </Link>
                      </td>
                      <td className="num px-2 py-1.5 text-right text-foreground">{bytes(up24)}</td>
                      <td className="num px-2 py-1.5 text-right text-muted-foreground">{bytes(down24)}</td>
                      <td className="num px-2 py-1.5 text-right text-muted-foreground">{os ? bytes(os.uploadedBytes) : "—"}</td>
                      <td className={`num px-2 py-1.5 text-right ${waitSec > 5 ? "text-primary" : "text-muted-foreground"}`}>{os ? duration(waitSec) : "—"}</td>
                      <td className="num py-1.5 pl-2 text-right text-muted-foreground">{hit !== null ? percent(hit) : "—"}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </Card>

        <Card
          className="col-span-12 lg:col-span-5"
          title="Part cache & extent cache"
          sub={`SSD part cache ${bytes(sum.partCacheBytes)} / ${bytes(sum.partCacheCapacityBytes)} · RAM extent cache ${bytes(sum.extentCacheBytes)} / ${bytes(sum.extentCacheCapacityBytes)}`}
        >
          <div className="space-y-4">
            {cladefsNodes.map((n) => {
              const os = n.objectStore;
              if (!os) return null;
              const partUsed = Number(os.partCacheBytes);
              const partCap = Number(os.partCacheCapacityBytes);
              const extUsed = Number(os.extentCacheBytes);
              const extCap = Number(os.extentCacheCapacityBytes);
              return (
                <div key={n.id} className="space-y-1.5 border-b border-border/60 pb-3 last:border-b-0 last:pb-0">
                  <div className="flex flex-wrap items-center justify-between gap-1 font-mono text-[11px]">
                    <Link to="/nodes/$hostname" params={{ hostname: n.hostname }} className="flex items-center gap-1.5 text-foreground hover:text-primary">
                      <span className="inline-block size-1.5 rounded-full" style={{ background: colors.get(n.id) ?? "var(--faint)" }} />
                      <span>{n.hostname}</span>
                    </Link>
                    <span className="text-[10px] text-faint">
                      {compact(os.extentCachePinnedEntries)} pinned · {compact(os.extentCacheIdleEntries)} idle · {compact(os.cacheTrims)} trims
                    </span>
                  </div>
                  <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                    <div>
                      <div className="mb-1 flex justify-between font-mono text-[10px] text-muted-foreground">
                        <span>SSD part cache</span>
                        <span className="num text-foreground">
                          {bytes(partUsed)}
                          {partCap > 0 ? ` / ${bytes(partCap)}` : ""}
                        </span>
                      </div>
                      <Meter value={partCap > 0 ? partUsed / partCap : 0} warn={0.85} danger={0.95} />
                    </div>
                    <div>
                      <div className="mb-1 flex justify-between font-mono text-[10px] text-muted-foreground">
                        <span>RAM extent cache</span>
                        <span className="num text-foreground">
                          {bytes(extUsed)}
                          {extCap > 0 ? ` / ${bytes(extCap)}` : ""}
                        </span>
                      </div>
                      <Meter value={extCap > 0 ? extUsed / extCap : 0} warn={0.9} danger={0.98} />
                    </div>
                  </div>
                </div>
              );
            })}
          </div>
        </Card>

        <Card
          className="col-span-12 lg:col-span-4"
          title="DAX window health"
          sub={
            sum.activeVms > 0
              ? `${sum.activeVms} active ${sum.activeVms === 1 ? "VM" : "VMs"} · ${bytes(sum.windowMappedBytes)} mapped`
              : "No active Firecracker VMs right now"
          }
          right={
            sum.activeVms > 0 && (
              <span
                className={`rounded-full px-2 py-0.5 font-mono text-[10px] ${
                  daxThrashing ? "bg-serious/15 text-serious" : "bg-sprout/15 text-sprout"
                }`}
              >
                {daxThrashing ? `${compact(sum.daxWindowFull)} full` : "healthy"}
              </span>
            )
          }
        >
          <div className="grid grid-cols-1 gap-3 font-mono text-[11px] sm:grid-cols-2">
            <div className="rounded-xl border border-border/70 bg-muted/25 p-2.5">
              <div className="eyebrow">window full / reclaims</div>
              <div className={`num mt-1 text-[16px] font-semibold ${daxThrashing ? "text-serious" : "text-foreground"}`}>
                {compact(sum.daxWindowFull)} <span className="text-[11px] font-normal text-muted-foreground">/ {compact(sum.daxWindowReclaims)}</span>
              </div>
              <div className="mt-0.5 text-[10px] text-faint">{compact(sum.daxRegionsFull)} region pool full</div>
            </div>
            <div className="rounded-xl border border-border/70 bg-muted/25 p-2.5">
              <div className="eyebrow">writes held by seals</div>
              <div className={`num mt-1 text-[16px] font-semibold ${sum.daxWritesHeldBySealsSec > 10 ? "text-primary" : "text-foreground"}`}>
                {duration(sum.daxWritesHeldBySealsSec)}
              </div>
              <div className="mt-0.5 text-[10px] text-faint">guest wait on commit seals</div>
            </div>
            <div className="rounded-xl border border-border/70 bg-muted/25 p-2.5 sm:col-span-2">
              <div className="flex flex-wrap items-baseline justify-between gap-1">
                <span className="eyebrow">non-DAX device reads</span>
                <span className="num text-[12px] text-foreground">
                  {bytes(sum.daxDeviceReadBytes)} · {duration(sum.daxDeviceReadsSec)}
                </span>
              </div>
              {vmNodes.length > 0 && (
                <div className="mt-2 space-y-1 border-t border-border/60 pt-1.5 text-[10.5px] text-muted-foreground">
                  {vmNodes.map((n) => {
                    const os = n.objectStore!;
                    return (
                      <div key={n.id} className="flex flex-wrap items-center justify-between gap-1">
                        <Link to="/nodes/$hostname" params={{ hostname: n.hostname }} className="text-foreground hover:text-primary">
                          {n.hostname} ({os.activeVms} VM)
                        </Link>
                        <span className="num">
                          {bytes(os.windowMappedBytes)} mapped · {compact(os.daxWindowFull)} full · {duration(Number(os.daxWritesHeldBySealsNs) / 1e9)} seal
                        </span>
                      </div>
                    );
                  })}
                </div>
              )}
            </div>
          </div>
        </Card>

        <Card
          className="col-span-12 lg:col-span-3"
          title="Estimated R2 monthly cost"
          sub="Gross projection from 24h op rates + warm base size · $0 egress"
        >
          <div className="display text-[38px] leading-none font-semibold text-foreground">{formatUsd(sum.cost.totalMonthly)}</div>
          <div className="mt-1 font-mono text-[10.5px] text-faint">projected / month (before R2 free tier)</div>
          <dl className="mt-4 space-y-2 border-t border-border/70 pt-3 font-mono text-[11px]">
            <div className="flex items-baseline justify-between gap-2">
              <dt className="text-muted-foreground">
                Class A ops <span className="text-faint">({compact(sum.cost.classAOpsMonth)}/mo)</span>
              </dt>
              <dd className="num text-foreground">{formatUsd(sum.cost.classACost)}</dd>
            </div>
            <div className="flex items-baseline justify-between gap-2">
              <dt className="text-muted-foreground">
                Class B ops <span className="text-faint">({compact(sum.cost.classBOpsMonth)}/mo)</span>
              </dt>
              <dd className="num text-foreground">{formatUsd(sum.cost.classBCost)}</dd>
            </div>
            <div className="flex items-baseline justify-between gap-2">
              <dt className="text-muted-foreground">
                Storage <span className="text-faint">({bytes(sum.cost.storageBytes)})</span>
              </dt>
              <dd className="num text-foreground">{formatUsd(sum.cost.storageCost)}</dd>
            </div>
            <div className="flex items-baseline justify-between gap-2">
              <dt className="text-muted-foreground">Egress</dt>
              <dd className="num text-sprout">$0.00</dd>
            </div>
          </dl>
        </Card>
      </div>
    </section>
  );
}

export function NodeObjectStoreSection({ n, s }: { n: Node; s: Snapshot }) {
  const os = n.objectStore;
  const series = s.series[n.id]?.samples ?? [];
  const hours = s.nodeHourly[n.id]?.hours ?? [];
  const up24 = hours.reduce((acc, h) => acc + Number(h.uploadBytes), 0);
  const down24 = hours.reduce((acc, h) => acc + Number(h.downloadBytes), 0);
  if (!os && up24 === 0 && down24 === 0) return null;

  const last = series[series.length - 1];
  const hit = nodeCacheHit(os);
  const waitSec = os ? Number(os.commitWaitNs) / 1e9 : 0;
  const totalReqs = os ? Number(os.putRequests + os.getRequests + os.otherRequests) : 0;
  const fails = os ? Number(os.failedRequests) : 0;
  const partUsed = Number(os?.partCacheBytes ?? 0n);
  const partCap = Number(os?.partCacheCapacityBytes ?? 0n);
  const extUsed = Number(os?.extentCacheBytes ?? 0n);
  const extCap = Number(os?.extentCacheCapacityBytes ?? 0n);
  const hasDax = os && (os.activeVms > 0 || os.windowMappedBytes > 0n);
  const daxThrashing = os ? os.daxWindowFull > 0n || os.daxRegionsFull > 0n : false;

  return (
    <section className="space-y-4">
      <h2 className="eyebrow">cladefs &amp; R2 object storage</h2>
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <StatTile
          label="R2 uploaded (24h)"
          value={bytes(up24)}
          tone={(last?.uploadBytesPerSec ?? 0) > 1024 ? "ember" : "default"}
          foot={
            os
              ? `${last && last.uploadBytesPerSec > 0 ? `now ${bytesPerSec(last.uploadBytesPerSec)} · ` : ""}${bytes(os.uploadedBytes)} since boot`
              : undefined
          }
        />
        <StatTile
          label="R2 downloaded (24h)"
          value={bytes(down24)}
          tone={(last?.downloadBytesPerSec ?? 0) > 1024 ? "frost" : "default"}
          foot={
            os
              ? `${last && last.downloadBytesPerSec > 0 ? `now ${bytesPerSec(last.downloadBytesPerSec)} · ` : ""}${bytes(os.downloadedBytes)} since boot`
              : undefined
          }
        />
        <StatTile
          label="R2 requests"
          value={compact(totalReqs)}
          tone={fails > 0 ? "ember" : "default"}
          foot={
            os
              ? `${compact(os.putRequests)} puts · ${compact(os.getRequests)} gets${fails > 0 ? ` · ${compact(fails)} failed` : ""}`
              : undefined
          }
        />
        <StatTile
          label="Commit wait on R2"
          value={os ? duration(waitSec) : "—"}
          tone={waitSec > 5 ? "ember" : "good"}
          foot={hit !== null ? `${percent(hit, 1)} local read hit` : "cumulative since daemon start"}
        />
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        <Card title="R2 throughput" sub="Upload and download rate, last 15 minutes">
          <LineChart
            series={[
              { id: "up", label: "upload", color: "var(--series-1)", points: series.map((p) => ({ t: Number(p.t), v: p.uploadBytesPerSec })) },
              { id: "down", label: "download", color: "var(--series-2)", points: series.map((p) => ({ t: Number(p.t), v: p.downloadBytesPerSec })) },
            ]}
            format={bytesPerSec}
            height={150}
          />
        </Card>
        <Card title="Commit wait on R2" sub="Milliseconds of commit wait per second waiting on R2 upload lag, last 15 minutes">
          <LineChart
            series={[
              { id: "wait", label: "commit wait", color: "var(--serious)", points: series.map((p) => ({ t: Number(p.t), v: p.commitWaitMsPerSec })) },
            ]}
            format={(v) => `${v.toFixed(0)} ms/s`}
            height={150}
          />
        </Card>
      </div>

      <div className="grid gap-4 lg:grid-cols-3">
        <Card className="lg:col-span-2" title="R2 transfer, 24 hours" sub="Uploaded and downloaded per hour">
          <Columns
            columns={hours.map((h) => ({
              key: String(h.hour),
              label: hourLabel(h.hour),
              values: [Number(h.uploadBytes), Number(h.downloadBytes)],
            }))}
            series={[
              { id: "up", label: "uploaded", color: "var(--series-1)" },
              { id: "down", label: "downloaded", color: "var(--series-2)" },
            ]}
            format={bytes}
            labelEvery={3}
            height={150}
          />
        </Card>
        {os && (
          <Card title="Upload mix & read path" sub="Cumulative cladefsd counters since daemon start">
            <div className="space-y-5">
              <div>
                <div className="eyebrow mb-2">uploaded by kind · {bytes(os.uploadedBytes)}</div>
                <SegmentedBar
                  tall
                  format={bytes}
                  parts={[
                    { id: "segments", label: "segments", value: Number(os.uploadedSegmentsBytes), color: "var(--series-1)" },
                    { id: "extents", label: "extents", value: Number(os.uploadedExtentsBytes), color: "var(--series-2)" },
                    { id: "checkpoints", label: "checkpoints", value: Number(os.uploadedCheckpointsBytes), color: "var(--series-3)" },
                    { id: "other", label: "other", value: Number(os.uploadedOtherBytes), color: "var(--faint)" },
                  ]}
                />
              </div>
              <div>
                <div className="eyebrow mb-2">extent reads · {hit !== null ? `${percent(hit, 1)} local` : "none yet"}</div>
                <SegmentedBar
                  tall
                  format={bytes}
                  parts={[
                    { id: "local", label: "local page-ins", value: Number(os.pageInBytes), color: "var(--sprout)" },
                    { id: "fetched", label: "fetched from R2", value: Number(os.fetchedBytes), color: "var(--series-2)" },
                  ]}
                />
              </div>
            </div>
          </Card>
        )}
      </div>

      {os && (
        <div className={`grid gap-4 ${hasDax ? "lg:grid-cols-2" : "lg:grid-cols-1"}`}>
          <Card title="Part cache & extent cache" sub="Local SSD part cache and cladefsd in-memory extent cache">
            <div className="space-y-3.5 font-mono text-[11px]">
              <div>
                <div className="mb-1 flex justify-between text-muted-foreground">
                  <span>SSD part cache</span>
                  <span className="num text-foreground">
                    {bytes(partUsed)}
                    {partCap > 0 ? ` / ${bytes(partCap)} (${percent(partUsed / partCap)})` : ""}
                  </span>
                </div>
                <Meter value={partCap > 0 ? partUsed / partCap : 0} warn={0.85} danger={0.95} />
              </div>
              <div>
                <div className="mb-1 flex justify-between text-muted-foreground">
                  <span>RAM extent cache</span>
                  <span className="num text-foreground">
                    {bytes(extUsed)}
                    {extCap > 0 ? ` / ${bytes(extCap)} (${percent(extUsed / extCap)})` : ""}
                  </span>
                </div>
                <Meter value={extCap > 0 ? extUsed / extCap : 0} warn={0.9} danger={0.98} />
              </div>
              <div className="flex flex-wrap gap-4 pt-1 text-[10.5px] text-faint">
                <span>
                  <span className="num text-foreground">{compact(os.extentCachePinnedEntries)}</span> pinned entries
                </span>
                <span>
                  <span className="num text-foreground">{compact(os.extentCacheIdleEntries)}</span> idle entries
                </span>
                <span>
                  <span className="num text-foreground">{compact(os.cacheTrims)}</span> cache trims
                </span>
              </div>
            </div>
          </Card>

          {hasDax && (
            <Card
              title="DAX window health"
              sub={`${os.activeVms} active ${os.activeVms === 1 ? "VM" : "VMs"} · ${bytes(os.windowMappedBytes)} mapped`}
              right={
                <span
                  className={`rounded-full px-2 py-0.5 font-mono text-[10px] ${
                    daxThrashing ? "bg-serious/15 text-serious" : "bg-sprout/15 text-sprout"
                  }`}
                >
                  {daxThrashing ? `${compact(os.daxWindowFull)} full` : "healthy"}
                </span>
              }
            >
              <div className="grid grid-cols-2 gap-3 font-mono text-[11px]">
                <div>
                  <div className="eyebrow">window full / reclaims</div>
                  <div className={`num mt-0.5 text-[15px] font-semibold ${daxThrashing ? "text-serious" : "text-foreground"}`}>
                    {compact(os.daxWindowFull)} / {compact(os.daxWindowReclaims)}
                  </div>
                  <div className="text-[10px] text-faint">{compact(os.daxRegionsFull)} region pool full</div>
                </div>
                <div>
                  <div className="eyebrow">writes held by seals</div>
                  <div className={`num mt-0.5 text-[15px] font-semibold ${Number(os.daxWritesHeldBySealsNs) > 10e9 ? "text-primary" : "text-foreground"}`}>
                    {duration(Number(os.daxWritesHeldBySealsNs) / 1e9)}
                  </div>
                  <div className="text-[10px] text-faint">guest stall on commit seals</div>
                </div>
                <div className="col-span-2 border-t border-border/60 pt-2 text-[10.5px] text-muted-foreground">
                  non-DAX device reads: <span className="num text-foreground">{bytes(os.daxDeviceReadBytes)}</span> in{" "}
                  <span className="num text-foreground">{duration(Number(os.daxDeviceReadsNs) / 1e9)}</span>
                </div>
              </div>
            </Card>
          )}
        </div>
      )}
    </section>
  );
}
