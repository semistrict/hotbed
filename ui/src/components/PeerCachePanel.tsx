// cladefs's distributed part cache: every part of R2 has one owner among the
// nodes (rendezvous hashing over the peer mesh), and a node that misses its
// own SSD part cache asks the owner before R2. These views show whether the
// mesh is whole and how much R2 traffic it saves.

import { Link } from "@tanstack/react-router";
import { PartCacheShardState, type Node, type ObjectStoreStats, type Snapshot } from "../api";
import { SegmentedBar } from "./charts/Bars";
import { LineChart } from "./charts/LineChart";
import { Card } from "./Panel";
import { compact, percent } from "../lib/format";

export interface PeerMember {
  node: Node;
  os: ObjectStoreStats;
  // Online peers this node's applied list lacks, and listed peers that are
  // not online members (stale or retired), by address.
  missingPeers: string[];
  extraPeers: string[];
  shards: number;
  degradedShards: number;
}

export interface PeerMeshSummary {
  members: PeerMember[];
  // Addresses of online members: the mesh every member should have applied.
  expected: string[];
  hostByAddress: Map<string, string>;
  inSync: boolean;
  given: number;
  missing: number;
  failed: number;
  served: number;
  servedMissing: number;
  given24h: number;
  served24h: number;
  // Parts got from peers over parts got from peers plus R2 GETs, last 24h.
  peerShare24h: number | null;
}

const perSec = (v: number) => `${v < 10 ? v.toFixed(1) : compact(v)}/s`;

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
    };
  });
  const sum = (pick: (os: ObjectStoreStats) => bigint) => members.reduce((n, m) => n + Number(pick(m.os)), 0);
  const given24h = Number(s.totals24h?.peerPartsGiven ?? 0n);
  const gets24h = Number(s.totals24h?.r2GetRequests ?? 0n);
  return {
    members,
    expected,
    hostByAddress,
    inSync: members.every((m) => m.missingPeers.length === 0 && m.extraPeers.length === 0),
    given: sum((os) => os.peerPartsGiven),
    missing: sum((os) => os.peerPartsMissing),
    failed: sum((os) => os.peerPartsFailed),
    served: sum((os) => os.peerPartsServed),
    servedMissing: sum((os) => os.peerServedPartsMissing),
    given24h,
    served24h: Number(s.totals24h?.peerPartsServed ?? 0n),
    peerShare24h: given24h + gets24h > 0 ? given24h / (given24h + gets24h) : null,
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
      points: (s.series[m.node.id]?.samples ?? []).map((p) => ({ t: Number(p.t), v: p.peerPartsGivenPerSec })),
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
          <div className="rounded-xl border border-border/70 bg-muted/25 p-2.5">
            <div className="eyebrow">from peers (24h)</div>
            <div className="num mt-1 text-[16px] font-semibold text-foreground">{compact(sum.given24h)}</div>
            <div className="mt-0.5 text-[10px] text-faint">R2 GETs saved</div>
          </div>
          <div className="rounded-xl border border-border/70 bg-muted/25 p-2.5">
            <div className="eyebrow">peer share (24h)</div>
            <div className="num mt-1 text-[16px] font-semibold text-foreground">{sum.peerShare24h !== null ? percent(sum.peerShare24h, 1) : "—"}</div>
            <div className="mt-0.5 text-[10px] text-faint">of parts from peers + R2 GETs</div>
          </div>
          <div className="rounded-xl border border-border/70 bg-muted/25 p-2.5">
            <div className="eyebrow">served (24h)</div>
            <div className="num mt-1 text-[16px] font-semibold text-foreground">{compact(sum.served24h)}</div>
            <div className="mt-0.5 text-[10px] text-faint">{compact(sum.servedMissing)} held none since start</div>
          </div>
          <div className="rounded-xl border border-border/70 bg-muted/25 p-2.5">
            <div className="eyebrow">failed asks</div>
            <div className={`num mt-1 text-[16px] font-semibold ${sum.failed > 0 ? "text-serious-ink" : "text-foreground"}`}>{compact(sum.failed)}</div>
            <div className="mt-0.5 text-[10px] text-faint">{compact(sum.missing)} owner had none</div>
          </div>
        </div>
        <div className="lg:col-span-8">
          <div className="eyebrow mb-2">parts from peers per node, last 15 minutes</div>
          <LineChart series={lines} format={perSec} height={150} />
        </div>
      </div>
      <div className="mt-4 overflow-x-auto">
        <table className="w-full text-left font-mono text-[11px]">
          <thead>
            <tr className="border-b border-border text-[9.5px] tracking-wider text-faint uppercase">
              <th className="py-1.5 pr-2 font-medium">node</th>
              <th className="px-2 py-1.5 font-medium">address</th>
              <th className="px-2 py-1.5 text-right font-medium">peers</th>
              <th className="px-2 py-1.5 text-right font-medium">↓ given</th>
              <th className="px-2 py-1.5 text-right font-medium">missing</th>
              <th className="px-2 py-1.5 text-right font-medium">failed</th>
              <th className="px-2 py-1.5 text-right font-medium">↑ served</th>
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
                <td className="num px-2 py-1.5 text-right text-foreground">{compact(m.os.peerPartsGiven)}</td>
                <td className="num px-2 py-1.5 text-right text-muted-foreground">{compact(m.os.peerPartsMissing)}</td>
                <td className={`num px-2 py-1.5 text-right ${m.os.peerPartsFailed > 0n ? "text-serious-ink" : "text-muted-foreground"}`}>{compact(m.os.peerPartsFailed)}</td>
                <td className="num px-2 py-1.5 text-right text-foreground">{compact(m.os.peerPartsServed)}</td>
                <td className={`num py-1.5 pl-2 text-right ${m.degradedShards > 0 ? "text-serious-ink" : "text-muted-foreground"}`}>
                  {m.shards === 0 ? "—" : m.degradedShards > 0 ? `${m.degradedShards} of ${m.shards} degraded` : `${m.shards} online`}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        <div className="mt-2 text-[10px] text-faint">Counts are since each cladefsd started.</div>
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
              asked owning peers · {compact(os.peerPartsGiven + os.peerPartsMissing + os.peerPartsFailed)}
            </div>
            <AskOutcomes os={os} />
          </div>
          <div>
            <div className="eyebrow mb-2">peers asked it as owner · {compact(os.peerPartsServed + os.peerServedPartsMissing)}</div>
            <ServeOutcomes os={os} />
          </div>
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
          <div className="eyebrow mb-2">parts per second, last 15 minutes</div>
          <LineChart
            series={[
              line("given", "from peers", "var(--sprout)", (p) => p.peerPartsGivenPerSec),
              line("served", "served to peers", "var(--series-1)", (p) => p.peerPartsServedPerSec),
              line("gets", "R2 GETs", "var(--series-2)", (p) => p.r2GetsPerSec),
            ]}
            format={perSec}
            height={150}
          />
        </div>
      </div>
    </Card>
  );
}
