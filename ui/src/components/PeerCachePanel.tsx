// cladefs's distributed part cache: every part of R2 has one owner among the
// nodes (rendezvous hashing over the peer mesh), and a node that misses its
// own SSD part cache asks the owner before R2. These views show whether the
// mesh is whole, how many bytes it keeps off R2, and where each node's part
// reads were found.

import { Link } from "@tanstack/react-router";
import { PartCacheShardState, type Node, type ObjectStoreStats, type PartFetches, type Snapshot } from "../api";
import { SegmentedBar } from "./charts/Bars";
import { LineChart } from "./charts/LineChart";
import { Card, MetricTile } from "./Panel";
import { bytes, bytesPerSec, compact, percent } from "../lib/format";

export interface PeerMember {
  node: Node;
  os: ObjectStoreStats;
  // Online peers this node's applied list lacks, and listed peers that are
  // not online members (stale or retired), by address.
  missingPeers: string[];
  extraPeers: string[];
  shards: number;
  degradedShards: number;
  // Share of part bytes it read from memory or its own SSD cache, since
  // cladefsd started; null before it read any.
  localHit: number | null;
}

// Part fetches by where they were found, summed as numbers.
export interface FetchSources {
  memory: number;
  memoryBytes: number;
  partCache: number;
  partCacheBytes: number;
  peer: number;
  peerBytes: number;
  bucket: number;
  bucketBytes: number;
  tailsFromPartCache: number;
  tailsFromBucket: number;
}

export interface PeerMeshSummary {
  members: PeerMember[];
  // Addresses of online members: the mesh every member should have applied.
  expected: string[];
  hostByAddress: Map<string, string>;
  inSync: boolean;
  asked: number;
  given: number;
  givenBytes: number;
  missing: number;
  failed: number;
  served: number;
  servedBytes: number;
  servedMissing: number;
  fetches: FetchSources;
  given24h: number;
  givenBytes24h: number;
  served24h: number;
  servedBytes24h: number;
  // Bytes got from peers over bytes got from peers plus bytes downloaded
  // from R2, last 24h.
  peerShare24h: number | null;
}

const FETCH_KEYS = [
  "memory",
  "memoryBytes",
  "partCache",
  "partCacheBytes",
  "peer",
  "peerBytes",
  "bucket",
  "bucketBytes",
  "tailsFromPartCache",
  "tailsFromBucket",
] as const satisfies readonly (keyof FetchSources & keyof PartFetches)[];

export function sumFetches(all: (PartFetches | undefined)[]): FetchSources {
  const out = Object.fromEntries(FETCH_KEYS.map((k) => [k, 0])) as unknown as FetchSources;
  for (const f of all) {
    if (!f) continue;
    for (const k of FETCH_KEYS) out[k] += Number(f[k]);
  }
  return out;
}

function localHit(f: FetchSources): number | null {
  const total = f.memoryBytes + f.partCacheBytes + f.peerBytes + f.bucketBytes;
  return total > 0 ? (f.memoryBytes + f.partCacheBytes) / total : null;
}

function degraded(state: PartCacheShardState): boolean {
  return state !== PartCacheShardState.ONLINE;
}

function shardStateLabel(state: PartCacheShardState): string {
  switch (state) {
    case PartCacheShardState.ONLINE:
      return "online";
    case PartCacheShardState.SUSPECT:
      return "suspect";
    case PartCacheShardState.OFFLINE:
      return "offline";
    case PartCacheShardState.RETURNING:
      return "returning";
    default:
      return "unknown";
  }
}

export function summarizePeerMesh(s: Snapshot): PeerMeshSummary {
  const all = s.nodes.flatMap((node) => (node.objectStore?.peerAddress ? [{ node, os: node.objectStore }] : []));
  const expected = all
    .filter((m) => m.node.online)
    .map((m) => m.os.peerAddress)
    .sort();
  const expectedSet = new Set(expected);
  const hostByAddress = new Map(all.map((m) => [m.os.peerAddress, m.node.hostname]));
  const members: PeerMember[] = all.map(({ node, os }) => {
    const applied = new Set(os.peers);
    return {
      node,
      os,
      missingPeers: node.online ? expected.filter((a) => !applied.has(a)) : [],
      extraPeers: node.online ? os.peers.filter((a) => !expectedSet.has(a)) : [],
      shards: os.partCacheShards.length,
      degradedShards: os.partCacheShards.filter((sh) => degraded(sh.state)).length,
      localHit: localHit(sumFetches([os.partFetches])),
    };
  });
  const sum = (pick: (os: ObjectStoreStats) => bigint) => members.reduce((n, m) => n + Number(pick(m.os)), 0);
  const givenBytes24h = Number(s.totals24h?.peerGivenBytes ?? 0n);
  const downloaded24h = Number(s.totals24h?.r2DownloadBytes ?? 0n);
  return {
    members,
    expected,
    hostByAddress,
    inSync: members.every((m) => m.missingPeers.length === 0 && m.extraPeers.length === 0),
    asked: sum((os) => os.peerPartsAsked),
    given: sum((os) => os.peerPartsGiven),
    givenBytes: sum((os) => os.peerGivenBytes),
    missing: sum((os) => os.peerPartsMissing),
    failed: sum((os) => os.peerPartsFailed),
    served: sum((os) => os.peerPartsServed),
    servedBytes: sum((os) => os.peerServedBytes),
    servedMissing: sum((os) => os.peerServedPartsMissing),
    fetches: sumFetches(members.map((m) => m.os.partFetches)),
    given24h: Number(s.totals24h?.peerPartsGiven ?? 0n),
    givenBytes24h,
    served24h: Number(s.totals24h?.peerPartsServed ?? 0n),
    servedBytes24h: Number(s.totals24h?.peerServedBytes ?? 0n),
    peerShare24h: givenBytes24h + downloaded24h > 0 ? givenBytes24h / (givenBytes24h + downloaded24h) : null,
  };
}

function MeshBadge({ inSync, members }: { inSync: boolean; members: number }) {
  return (
    <span className={`rounded-full px-2 py-0.5 font-mono text-[10px] ${inSync ? "bg-sprout/15 text-sprout" : "bg-serious/15 text-serious-ink"}`}>
      {inSync ? `${members}-node mesh` : "mesh out of sync"}
    </span>
  );
}

function AskOutcomes({ os }: { os: ObjectStoreStats }) {
  return (
    <SegmentedBar
      format={compact}
      parts={[
        { id: "given", label: "given by owner", value: Number(os.peerPartsGiven), color: "var(--sprout)" },
        { id: "missing", label: "owner had none", value: Number(os.peerPartsMissing), color: "var(--faint)" },
        { id: "failed", label: "failed", value: Number(os.peerPartsFailed), color: "var(--critical)" },
      ]}
    />
  );
}

function ServeOutcomes({ os }: { os: ObjectStoreStats }) {
  return (
    <SegmentedBar
      format={compact}
      parts={[
        { id: "served", label: "served", value: Number(os.peerPartsServed), color: "var(--series-1)" },
        { id: "missing", label: "held none", value: Number(os.peerServedPartsMissing), color: "var(--faint)" },
      ]}
    />
  );
}

// Where part reads were found, by bytes, and object tails by count.
function FetchSourcesBar({ f }: { f: FetchSources }) {
  const tails = f.tailsFromPartCache + f.tailsFromBucket;
  return (
    <div>
      <SegmentedBar
        format={bytes}
        parts={[
          { id: "memory", label: `memory · ${compact(f.memory)}`, value: f.memoryBytes, color: "var(--series-7)" },
          { id: "part-cache", label: `SSD cache · ${compact(f.partCache)}`, value: f.partCacheBytes, color: "var(--series-1)" },
          { id: "peer", label: `peers · ${compact(f.peer)}`, value: f.peerBytes, color: "var(--sprout)" },
          { id: "bucket", label: `R2 · ${compact(f.bucket)}`, value: f.bucketBytes, color: "var(--series-2)" },
        ]}
      />
      {tails > 0 && (
        <div className="mt-1 text-[10.5px] text-muted-foreground">
          object tails {compact(f.tailsFromPartCache)} from SSD cache, {compact(f.tailsFromBucket)} from R2
        </div>
      )}
    </div>
  );
}

function MeshProblems({ m, hostByAddress }: { m: PeerMember; hostByAddress: Map<string, string> }) {
  const name = (a: string) => hostByAddress.get(a) ?? a;
  if (m.missingPeers.length === 0 && m.extraPeers.length === 0) return null;
  return (
    <span className="text-serious-ink">
      {m.missingPeers.length > 0 && `lacks ${m.missingPeers.map(name).join(", ")}`}
      {m.missingPeers.length > 0 && m.extraPeers.length > 0 && " · "}
      {m.extraPeers.length > 0 && `stale ${m.extraPeers.map(name).join(", ")}`}
    </span>
  );
}

export function ClusterPeerCacheCard({ s, colors }: { s: Snapshot; colors: Map<string, string> }) {
  const sum = summarizePeerMesh(s);
  if (sum.members.length === 0) return null;
  const lines = sum.members
    .filter((m) => m.node.online)
    .map((m) => ({
      id: m.node.id,
      label: m.node.hostname,
      color: colors.get(m.node.id) ?? "var(--faint)",
      points: (s.series[m.node.id]?.samples ?? []).map((p) => ({ t: Number(p.t), v: p.peerGivenBytesPerSec })),
    }));

  return (
    <Card
      className="col-span-12"
      title="Distributed part cache"
      sub="Each R2 part has one owning node; a node that misses its own SSD cache asks the owner before R2"
      right={<MeshBadge inSync={sum.inSync} members={sum.expected.length} />}
    >
      <div className="grid gap-4 lg:grid-cols-12">
        <div className="grid grid-cols-2 gap-3 font-mono text-[11px] lg:col-span-4">
          <MetricTile label="from peers (24h)" value={bytes(sum.givenBytes24h)} sub={`${compact(sum.given24h)} parts not downloaded from R2`} />
          <MetricTile
            label="peer share (24h)"
            value={sum.peerShare24h !== null ? percent(sum.peerShare24h, 1) : "—"}
            sub="of bytes from peers + R2 downloads"
          />
          <MetricTile label="served (24h)" value={bytes(sum.servedBytes24h)} sub={`${compact(sum.served24h)} parts · ${compact(sum.servedMissing)} held none since start`} />
          <MetricTile
            label="failed asks"
            value={compact(sum.failed)}
            sub={`of ${compact(sum.asked)} asked · ${compact(sum.missing)} owner had none`}
            tone={sum.failed > 0 ? "serious" : "default"}
          />
        </div>
        <div className="lg:col-span-8">
          <div className="eyebrow mb-2">bytes from peers per node, last 15 minutes</div>
          <LineChart series={lines} format={bytesPerSec} height={150} />
        </div>
      </div>
      <div className="mt-4 font-mono text-[11px]">
        <div className="eyebrow mb-2">where parts were found, all nodes, since start · bytes</div>
        <FetchSourcesBar f={sum.fetches} />
      </div>
      <div className="mt-4 overflow-x-auto">
        <table className="w-full text-left font-mono text-[11px]">
          <thead>
            <tr className="border-b border-border text-[9.5px] tracking-wider text-faint uppercase">
              <th className="py-1.5 pr-2 font-medium">node</th>
              <th className="px-2 py-1.5 font-medium">address</th>
              <th className="px-2 py-1.5 text-right font-medium">peers</th>
              <th className="px-2 py-1.5 text-right font-medium">asked</th>
              <th className="px-2 py-1.5 text-right font-medium">↓ given</th>
              <th className="px-2 py-1.5 text-right font-medium">↓ bytes</th>
              <th className="px-2 py-1.5 text-right font-medium">missing</th>
              <th className="px-2 py-1.5 text-right font-medium">failed</th>
              <th className="px-2 py-1.5 text-right font-medium">↑ served</th>
              <th className="px-2 py-1.5 text-right font-medium">↑ bytes</th>
              <th className="px-2 py-1.5 text-right font-medium">local hit</th>
              <th className="py-1.5 pl-2 text-right font-medium">cache disks</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-border/70">
            {sum.members.map((m) => (
              <tr key={m.node.id} className="hover:bg-muted/40">
                <td className="py-1.5 pr-2">
                  <Link to="/nodes/$hostname" params={{ hostname: m.node.hostname }} className="flex items-center gap-1.5 text-foreground hover:text-primary">
                    <span className="inline-block size-1.5 rounded-full" style={{ background: colors.get(m.node.id) ?? "var(--faint)" }} />
                    <span>{m.node.hostname}</span>
                    {!m.node.online && <span className="text-faint">offline</span>}
                  </Link>
                </td>
                <td className="px-2 py-1.5 text-muted-foreground">
                  {m.os.peerAddress} <MeshProblems m={m} hostByAddress={sum.hostByAddress} />
                </td>
                <td className="num px-2 py-1.5 text-right text-muted-foreground">{m.os.peers.length}</td>
                <td className="num px-2 py-1.5 text-right text-muted-foreground">{compact(m.os.peerPartsAsked)}</td>
                <td className="num px-2 py-1.5 text-right text-foreground">{compact(m.os.peerPartsGiven)}</td>
                <td className="num px-2 py-1.5 text-right text-foreground">{bytes(m.os.peerGivenBytes)}</td>
                <td className="num px-2 py-1.5 text-right text-muted-foreground">{compact(m.os.peerPartsMissing)}</td>
                <td className={`num px-2 py-1.5 text-right ${m.os.peerPartsFailed > 0n ? "text-serious-ink" : "text-muted-foreground"}`}>{compact(m.os.peerPartsFailed)}</td>
                <td className="num px-2 py-1.5 text-right text-foreground">{compact(m.os.peerPartsServed)}</td>
                <td className="num px-2 py-1.5 text-right text-foreground">{bytes(m.os.peerServedBytes)}</td>
                <td className="num px-2 py-1.5 text-right text-muted-foreground">{m.localHit !== null ? percent(m.localHit, 1) : "—"}</td>
                <td className={`num py-1.5 pl-2 text-right ${m.degradedShards > 0 ? "text-serious-ink" : "text-muted-foreground"}`}>
                  {m.shards === 0 ? "—" : m.degradedShards > 0 ? `${m.degradedShards} of ${m.shards} degraded` : `${m.shards} online`}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        <div className="mt-2 text-[10px] text-faint">
          Counts are since each cladefsd started. Local hit is the share of part bytes read from memory or the node's own SSD cache.
        </div>
      </div>
    </Card>
  );
}

export function NodePeerCacheCard({ n, s }: { n: Node; s: Snapshot }) {
  const os = n.objectStore;
  if (!os?.peerAddress) return null;
  const sum = summarizePeerMesh(s);
  const me = sum.members.find((m) => m.node.id === n.id);
  if (!me) return null;
  const samples = s.series[n.id]?.samples ?? [];
  const line = (id: string, label: string, color: string, pick: (p: (typeof samples)[number]) => number) => ({
    id,
    label,
    color,
    points: samples.map((p) => ({ t: Number(p.t), v: pick(p) })),
  });
  const others = os.peers.filter((a) => a !== os.peerAddress);

  return (
    <Card
      title="Distributed part cache"
      sub={`Serves peers at ${os.peerAddress} · counts since cladefsd started`}
      right={<MeshBadge inSync={me.missingPeers.length === 0 && me.extraPeers.length === 0} members={os.peers.length} />}
    >
      <div className="grid gap-4 lg:grid-cols-2">
        <div className="space-y-4 font-mono text-[11px]">
          <div>
            <div className="eyebrow mb-2">
              asked owning peers · {compact(os.peerPartsAsked)} · got {bytes(os.peerGivenBytes)}
            </div>
            <AskOutcomes os={os} />
          </div>
          <div>
            <div className="eyebrow mb-2">
              peers asked it as owner · {compact(os.peerPartsServed + os.peerServedPartsMissing)} · served {bytes(os.peerServedBytes)}
            </div>
            <ServeOutcomes os={os} />
          </div>
          {os.partFetches && (
            <div>
              <div className="eyebrow mb-2">where parts were found · local hit {me.localHit !== null ? percent(me.localHit, 1) : "—"}</div>
              <FetchSourcesBar f={sumFetches([os.partFetches])} />
            </div>
          )}
          <div className="space-y-1 border-t border-border/60 pt-2 text-[10.5px] text-muted-foreground">
            <div>
              peers{" "}
              {others.length === 0 ? (
                <span className="text-serious-ink">none applied</span>
              ) : (
                others.map((a, i) => (
                  <span key={a}>
                    {i > 0 && ", "}
                    <span className="text-foreground">{sum.hostByAddress.get(a) ?? a}</span>
                  </span>
                ))
              )}{" "}
              <MeshProblems m={me} hostByAddress={sum.hostByAddress} />
            </div>
            {os.partCacheShards.length > 0 && (
              <div>
                cache disks{" "}
                {os.partCacheShards.map((sh, i) => (
                  <span key={sh.shard}>
                    {i > 0 && ", "}
                    <span className={degraded(sh.state) ? "text-serious-ink" : "text-foreground"}>
                      {sh.device} {shardStateLabel(sh.state)}
                    </span>
                  </span>
                ))}
              </div>
            )}
          </div>
        </div>
        <div>
          <div className="eyebrow mb-2">bytes per second, last 15 minutes</div>
          <LineChart
            series={[
              line("given", "from peers", "var(--sprout)", (p) => p.peerGivenBytesPerSec),
              line("served", "served to peers", "var(--series-1)", (p) => p.peerServedBytesPerSec),
              line("r2", "R2 downloads", "var(--series-2)", (p) => p.downloadBytesPerSec),
            ]}
            format={bytesPerSec}
            height={150}
          />
        </div>
      </div>
    </Card>
  );
}
