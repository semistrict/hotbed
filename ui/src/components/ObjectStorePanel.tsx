import { Link } from "@tanstack/react-router";
import type { Node, ObjectStoreStats, Snapshot } from "../api";
import { SegmentedBar } from "./charts/Bars";
import { Columns } from "./charts/Columns";
import { LineChart } from "./charts/LineChart";
import { Card, StatTile } from "./ui/Card";
import { bytes, bytesPerSec, compact, duration, percent } from "../lib/format";

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
  const rateLines = (pick: (p: { uploadBytesPerSec: number; downloadBytesPerSec: number }) => number) =>
    liveNodes.map((n) => ({
      id: n.id,
      label: n.hostname,
      color: colors.get(n.id) ?? "var(--faint)",
      points: (s.series[n.id]?.samples ?? []).map((p) => ({ t: Number(p.t), v: pick(p) })),
    }));

  return (
    <section className="col-span-12 space-y-4">
      <h2 className="eyebrow">cladefs &amp; R2 object storage · uploads, downloads, cache hits and commit stalls</h2>

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-5">
        <StatTile
          label="R2 uploaded (24h)"
          value={bytes(sum.uploaded24h)}
          tone={sum.liveUpBps > 1024 ? "ember" : "default"}
          foot={
            sum.liveUpBps > 0
              ? `now ${bytesPerSec(sum.liveUpBps)} · ${bytes(sum.sinceBootUp)} since boot`
              : `${bytes(sum.sinceBootUp)} since daemon start`
          }
        />
        <StatTile
          label="R2 downloaded (24h)"
          value={bytes(sum.downloaded24h)}
          tone={sum.liveDownBps > 1024 ? "frost" : "default"}
          foot={
            sum.liveDownBps > 0
              ? `now ${bytesPerSec(sum.liveDownBps)} · ${bytes(sum.sinceBootDown)} since boot`
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
          foot={`${bytes(sum.sinceBootPageIn)} local · ${bytes(sum.sinceBootFetched)} from R2`}
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
    </section>
  );
}
