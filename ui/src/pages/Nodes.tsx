import { Link } from "@tanstack/react-router";
import { BaseState, CommandState, nodeAdmission, WorkspaceState, type Node, type Snapshot } from "../api";
import { AdmissionBadge, PlacementList } from "../components/Admission";
import { Meter, SegmentedBar, Sparkline } from "../components/charts/Bars";
import { Heatmap } from "../components/charts/Heatmap";
import { EventFeed } from "../components/EventFeed";
import { Card } from "../components/ui/Card";
import { ArchBadge, BuildingBadge, OnlineBadge, StateBadge } from "../components/ui/State";
import { useNodeColors, useSnapshot } from "../lib/data";
import { ago, bytes, percent } from "../lib/format";
import { formatHostUsageFoot, isHostStarved, nodeHostUsage } from "../lib/hostUsage";

export function NodesPage() {
  const { data } = useSnapshot();
  const colors = useNodeColors(data?.nodes);
  if (!data) return null;
  const nowMs = Number(data.now);
  return (
    <div className="space-y-6">
      <PageTitle title="Nodes" sub={`${data.nodes.filter((n) => n.online).length} of ${data.nodes.length} online · old PCs and Apple Silicon laptops on Asahi Linux`} />
      <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
        {data.nodes.map((n, i) => (
          <NodeCard key={n.id} n={n} s={data} color={colors.get(n.id) ?? "var(--faint)"} now={nowMs} delay={i * 50} />
        ))}
      </div>
      <Admission s={data} now={nowMs} />
      <Card title="All nodes" sub="Every figure on this page as a table" delay={400}>
        <NodeTable s={data} />
      </Card>
    </div>
  );
}

/** How every node decides whether to start new work, and where the controller put workspaces. */
function Admission({ s, now }: { s: Snapshot; now: number }) {
  const hosts = new Map(s.nodes.map((n) => [n.id, n.hostname]));
  const holding = s.nodes.filter((n) => nodeAdmission(n)?.holding);
  return (
    <section className="space-y-4">
      <h2 className="eyebrow">
        Admission · {holding.length === 0 ? "every node starts new work" : `${holding.map((n) => n.hostname).join(", ")} ${holding.length === 1 ? "holds" : "hold"} new work`}
      </h2>
      <Card title="Held new work by node and hour" sub="Share of each hour a node held new work, last 24 hours" delay={300}>
        <Heatmap
          rows={s.nodes.map((n) => ({
            id: n.id,
            label: n.hostname,
            cells: (s.nodeHourly[n.id]?.hours ?? []).map((h) => ({ key: String(h.hour), label: `${String(new Date(Number(h.hour)).getHours()).padStart(2, "0")}:00`, v: h.cpuBusy === 0 && !n.online ? null : h.holding })),
          }))}
          format={(v) => `held ${percent(v)} of the hour`}
          scale={{ low: "never", high: "held all hour" }}
        />
      </Card>
      <div className="grid gap-4 lg:grid-cols-2">
        <Card title="Held work" sub="Every time a node held new work, made a command wait, or started again" delay={340}>
          <EventFeed events={s.admissionEvents} limit={40} hosts={hosts} />
        </Card>
        <Card title="Placements" sub="Where the controller put each workspace: open one for every node's cost in parts" delay={360}>
          <PlacementList placements={s.placements} limit={30} now={now} />
        </Card>
      </div>
    </section>
  );
}

export function PageTitle({ title, sub, right }: { title: string; sub?: string; right?: React.ReactNode }) {
  return (
    <div className="rise flex flex-wrap items-end gap-4">
      <div>
        <h1 className="display text-[44px] leading-none font-semibold">{title}</h1>
        {sub && <p className="mt-2 text-[13px] text-muted-foreground">{sub}</p>}
      </div>
      {right && <div className="ml-auto">{right}</div>}
    </div>
  );
}

function NodeCard({ n, s, color, now, delay }: { n: Node; s: Snapshot; color: string; now: number; delay: number }) {
  const cmds = s.commands.filter(
    (c) =>
      c.nodeId === n.id &&
      (c.state === CommandState.RUNNING || c.state === CommandState.PARKED || c.state === CommandState.PAUSED || c.state === CommandState.QUEUED),
  );
  const buildingBases = s.bases.filter((b) => b.nodeId === n.id && b.state === BaseState.BUILDING);
  const pills = [
    ...buildingBases.map((b) => <BuildingBadge key={`base-${b.id}`} project={b.project} generation={b.generation} progress={b.progress} />),
    ...cmds.map((c) => <StateBadge key={c.id} state={c.state} />),
  ];
  const series = s.series[n.id]?.samples ?? [];
  const res = n.resources;
  const usage = n.usage;
  const disk = n.disk;
  const memBytes = Number(res?.memoryBytes ?? 0n);
  const memUsed = Number(usage?.memoryUsedBytes ?? 0n);
  const swapBytes = Number(res?.swapBytes ?? 0n);
  const swapUsed = Number(usage?.swapUsedBytes ?? 0n);
  const mem = memBytes > 0 ? memUsed / memBytes : 0;
  const bases = s.bases.filter((b) => b.nodeId === n.id && b.state !== BaseState.STALE).length;
  const ws = s.workspaces.filter((w) => w.nodeId === n.id && w.state !== WorkspaceState.RELEASED).length;
  const admission = nodeAdmission(n);
  const host = nodeHostUsage(n);
  const hostStarved = isHostStarved(usage?.cpuBusy ?? 0, host);
  return (
    <Link
      to="/nodes/$hostname"
      params={{ hostname: n.hostname }}
      className={`rise group block rounded-2xl border border-border bg-card/80 p-5 transition-colors hover:border-border-strong ${n.online ? "" : "opacity-60"}`}
      style={{ animationDelay: `${delay}ms` }}
    >
      <div className="flex items-start gap-3">
        <span className="mt-2 inline-block h-[3px] w-4 rounded-full" style={{ background: color }} />
        <div className="min-w-0">
          <div className="display text-[26px] leading-none font-semibold group-hover:text-primary">{n.hostname}</div>
          <div className="mt-1.5 flex items-center gap-2 text-[11.5px] text-muted-foreground">
            <ArchBadge arch={res?.arch ?? ""} />
            <span className="truncate">{res?.machine ?? ""}</span>
          </div>
        </div>
        <div className="ml-auto flex flex-col items-end gap-1 text-right">
          <OnlineBadge online={n.online} asleep={n.asleep} />
          {n.online && <AdmissionBadge a={admission} now={now} />}
          <div className="mt-1 font-mono text-[10px] text-faint">
            {n.online ? `${res?.cpus ?? 0} cpu · ${bytes(memBytes)}` : `seen ${ago(n.lastSeen ?? n.joinedAt, now)}`}
          </div>
        </div>
      </div>

      {n.online ? (
        <>
          <div className="mt-5 grid grid-cols-2 gap-5">
            <div>
              <div className="flex items-baseline justify-between">
                <span className="eyebrow">{host ? "guest cpu" : "cpu"}</span>
                <span className="num font-mono text-[13px] text-foreground">{percent(usage?.cpuBusy ?? 0)}</span>
              </div>
              {host && (
                <div className={`mt-0.5 flex items-baseline justify-between font-mono text-[10.5px] ${hostStarved ? "text-serious" : "text-muted-foreground"}`}>
                  <span>VM {percent(host.vmCpuBusy)}</span>
                  <span>host {percent(host.hostCpuBusy)}</span>
                </div>
              )}
              <div className="mt-2">
                <Sparkline values={series.map((p) => p.cpuBusy)} max={1} color={color} height={34} />
              </div>
            </div>
            <div>
              <div className="flex items-baseline justify-between">
                <span className="eyebrow">memory</span>
                <span className="num font-mono text-[13px] text-foreground">{percent(mem)}</span>
              </div>
              <div className="mt-2">
                <Sparkline values={series.map((p) => p.memoryUsed)} max={1} color={color} height={34} />
              </div>
            </div>
          </div>
          <div className="mt-4 space-y-2.5">
            {host && (
              <Row label="host" value={`VM ${percent(host.vmCpuBusy)} · ${formatHostUsageFoot(host)}`}>
                <Meter value={host.hostCpuBusy} warn={0.7} danger={0.85} />
              </Row>
            )}
            <Row label="RAM" value={`${bytes(memUsed)} · ${bytes(n.reservedMemoryBytes)} guaranteed`}>
              <Meter value={mem} warn={0.8} danger={0.93} marker={memBytes > 0 ? Number(n.reservedMemoryBytes) / memBytes : 0} />
            </Row>
            <Row label="swap" value={swapUsed > 0 ? `${bytes(swapUsed)} parked` : "none"}>
              <Meter value={swapBytes > 0 ? swapUsed / swapBytes : 0} warn={0.6} danger={0.85} />
            </Row>
            <Row label="PSI cpu" value={`${(usage?.cpuPressure ?? 0).toFixed(1)}% · holds at 60%`}>
              <Meter value={(usage?.cpuPressure ?? 0) / 100} warn={0.3} danger={0.6} />
            </Row>
            <Row label="PSI mem" value={`${(usage?.memoryPressure ?? 0).toFixed(1)}%`}>
              <Meter value={(usage?.memoryPressure ?? 0) / 50} warn={0.44} danger={0.7} />
            </Row>
            <div>
              <div className="mb-1.5 flex justify-between text-[10.5px] text-muted-foreground">
                <span className="eyebrow">{n.storage}</span>
                <span className="font-mono">
                  {bytes(disk?.freeBytes ?? 0n)} free of {bytes(n.diskBytes)}
                </span>
              </div>
              <SegmentedBar
                legend={false}
                format={bytes}
                parts={[
                  { id: "bases", label: "warm bases", value: Number(disk?.basesBytes ?? 0n), color: "var(--series-1)" },
                  { id: "forks", label: "fork-owned", value: Number(disk?.forksExclusiveBytes ?? 0n), color: "var(--series-2)" },
                  { id: "other", label: "other", value: Number(disk?.otherBytes ?? 0n), color: "var(--faint)" },
                  { id: "free", label: "free", value: Number(disk?.freeBytes ?? 0n), color: "var(--muted)" },
                ]}
              />
            </div>
          </div>
          <div className="mt-4 flex items-center gap-3 border-t border-border pt-3 font-mono text-[10.5px] whitespace-nowrap text-muted-foreground">
            <span>
              <span className="text-foreground">{bases}</span> bases
            </span>
            <span>
              <span className="text-foreground">{ws}</span> workspaces
            </span>
            {n.daemonLogs && (n.daemonLogs.errors24h > 0 || n.daemonLogs.warns24h > 0) && (
              <span
                className={n.daemonLogs.errors24h > 0 ? "text-critical" : "text-warning"}
                title={`${n.daemonLogs.errors24h} errors, ${n.daemonLogs.warns24h} warnings in 24h`}
              >
                {n.daemonLogs.errors24h > 0 ? `${n.daemonLogs.errors24h} err` : `${n.daemonLogs.warns24h} warn`}
              </span>
            )}
            <span className="ml-auto flex gap-1">
              {pills.slice(0, 3)}
              {pills.length > 3 && <span className="text-faint">+{pills.length - 3}</span>}
            </span>
          </div>
        </>
      ) : (
        <div className="mt-6 rounded-lg border border-dashed border-border-strong p-4 text-[12px] text-muted-foreground">
          Not reporting. Its bases stay on its disk; workspaces go to other nodes until it is back.
        </div>
      )}
    </Link>
  );
}

function Row({ label, value, children }: { label: string; value: string; children: React.ReactNode }) {
  return (
    <div>
      <div className="mb-1 flex justify-between text-[10.5px]">
        <span className="eyebrow">{label}</span>
        <span className="num font-mono text-muted-foreground">{value}</span>
      </div>
      {children}
    </div>
  );
}

export function NodeTable({ s }: { s: Snapshot }) {
  const now = Number(s.now);
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-[12px]">
        <thead className="eyebrow">
          <tr className="border-b border-border">
            {["node", "machine", "arch", "status", "admission", "cpus", "cpu busy", "psi cpu", "ram", "used", "guaranteed", "swap", "psi mem", "disk free", "kernel", "last seen"].map((h) => (
              <th key={h} className="px-2 py-2 font-normal whitespace-nowrap">
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="num font-mono text-[11px]">
          {s.nodes.map((n) => {
            const admission = nodeAdmission(n);
            return (
              <tr key={n.id} className="border-b border-border/60 hover:bg-muted/60">
                <td className="px-2 py-2">
                  <Link to="/nodes/$hostname" params={{ hostname: n.hostname }} className="text-foreground hover:text-primary">
                    {n.hostname}
                  </Link>
                </td>
                <td className="px-2 py-2 font-sans text-muted-foreground">{n.resources?.machine ?? ""}</td>
                <td className="px-2 py-2 text-muted-foreground">{n.resources?.arch || "—"}</td>
                <td className="px-2 py-2">
                  <OnlineBadge online={n.online} asleep={n.asleep} />
                </td>
                <td className="px-2 py-2" title={admission?.reason}>
                  {admission ? (admission.holding ? <span className="text-serious">holding</span> : "starting") : "—"}
                </td>
                <td className="px-2 py-2">{n.resources?.cpus ?? 0}</td>
                <td className="px-2 py-2 whitespace-nowrap">
                  <NodeCpuCell n={n} />
                </td>
                <td className="px-2 py-2">{n.online ? `${(n.usage?.cpuPressure ?? 0).toFixed(1)}%` : "—"}</td>
                <td className="px-2 py-2">{bytes(n.resources?.memoryBytes ?? 0n)}</td>
                <td className="px-2 py-2">{n.online ? bytes(n.usage?.memoryUsedBytes ?? 0n) : "—"}</td>
                <td className="px-2 py-2">{bytes(n.reservedMemoryBytes)}</td>
                <td className="px-2 py-2">{bytes(n.usage?.swapUsedBytes ?? 0n)}</td>
                <td className="px-2 py-2">{n.online ? `${(n.usage?.memoryPressure ?? 0).toFixed(1)}%` : "—"}</td>
                <td className="px-2 py-2">{bytes(n.disk?.freeBytes ?? 0n)}</td>
                <td className="px-2 py-2 text-muted-foreground">{n.resources?.kernel ?? ""}</td>
                <td className="px-2 py-2 text-muted-foreground">{ago(n.lastSeen ?? n.joinedAt, now)}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

export function NodeCpuCell({ n }: { n: Node }) {
  if (!n.online) return <>—</>;
  const guestBusy = n.usage?.cpuBusy ?? 0;
  const host = nodeHostUsage(n);
  if (!host) return <>{percent(guestBusy)}</>;
  const starved = isHostStarved(guestBusy, host);
  return (
    <span title={`Guest ${percent(guestBusy)} · VM ${percent(host.vmCpuBusy)} · ${formatHostUsageFoot(host)}`}>
      {percent(guestBusy)}{" "}
      <span className={starved ? "text-serious" : "text-muted-foreground"}>
        (VM {percent(host.vmCpuBusy)} · host {percent(host.hostCpuBusy)})
      </span>
    </span>
  );
}
