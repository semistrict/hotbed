import { Link } from "@tanstack/react-router";
import type { Node, Snapshot } from "../api";
import { Meter, SegmentedBar, Sparkline } from "../components/charts/Bars";
import { Card } from "../components/ui/Card";
import { ArchBadge, OnlineBadge, StateBadge } from "../components/ui/State";
import { useNodeColors, useSnapshot } from "../lib/data";
import { ago, bytes, percent } from "../lib/format";

export function NodesPage() {
  const { data } = useSnapshot();
  const colors = useNodeColors(data?.nodes);
  if (!data) return null;
  const nowMs = Date.parse(data.now);
  return (
    <div className="space-y-6">
      <PageTitle title="Nodes" sub={`${data.nodes.filter((n) => n.online).length} of ${data.nodes.length} online · old PCs and Apple Silicon laptops on Asahi Linux`} />
      <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
        {data.nodes.map((n, i) => (
          <NodeCard key={n.id} n={n} s={data} color={colors.get(n.id) ?? "var(--faint)"} now={nowMs} delay={i * 50} />
        ))}
      </div>
      <Card title="All nodes" sub="Every figure on this page as a table" delay={400}>
        <NodeTable s={data} />
      </Card>
    </div>
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
  const cmds = s.commands.filter((c) => c.nodeId === n.id && (c.state === "running" || c.state === "parked" || c.state === "queued"));
  const series = s.series[n.id] ?? [];
  const mem = n.usage.memoryUsedBytes / n.memoryBytes;
  const bases = s.bases.filter((b) => b.nodeId === n.id && b.state !== "stale").length;
  const ws = s.workspaces.filter((w) => w.nodeId === n.id && w.state !== "released").length;
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
            <ArchBadge arch={n.arch} />
            <span className="truncate">{n.machine}</span>
          </div>
        </div>
        <div className="ml-auto text-right">
          <OnlineBadge online={n.online} />
          <div className="mt-1 font-mono text-[10px] text-faint">{n.online ? `${n.cpus} cpu · ${bytes(n.memoryBytes)}` : `seen ${ago(n.lastSeen, now)}`}</div>
        </div>
      </div>

      {n.online ? (
        <>
          <div className="mt-5 grid grid-cols-2 gap-5">
            <div>
              <div className="flex items-baseline justify-between">
                <span className="eyebrow">cpu</span>
                <span className="num font-mono text-[13px] text-foreground">{percent(n.usage.cpuBusy)}</span>
              </div>
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
            <Row label="RAM" value={`${bytes(n.usage.memoryUsedBytes)} · ${bytes(n.reservedMemoryBytes)} guaranteed`}>
              <Meter value={mem} warn={0.8} danger={0.93} marker={n.reservedMemoryBytes / n.memoryBytes} />
            </Row>
            <Row label="swap" value={n.usage.swapUsedBytes > 0 ? `${bytes(n.usage.swapUsedBytes)} parked` : "none"}>
              <Meter value={n.usage.swapUsedBytes / n.swapBytes} warn={0.6} danger={0.85} />
            </Row>
            <Row label="PSI mem" value={`${n.usage.memoryPressure.toFixed(1)}%`}>
              <Meter value={n.usage.memoryPressure / 50} warn={0.44} danger={0.7} />
            </Row>
            <div>
              <div className="mb-1.5 flex justify-between text-[10.5px] text-muted-foreground">
                <span className="eyebrow">btrfs</span>
                <span className="font-mono">
                  {bytes(n.disk.freeBytes)} free of {bytes(n.diskBytes)}
                </span>
              </div>
              <SegmentedBar
                legend={false}
                format={bytes}
                parts={[
                  { id: "bases", label: "warm bases", value: n.disk.basesBytes, color: "var(--series-1)" },
                  { id: "forks", label: "fork-owned", value: n.disk.forksExclusiveBytes, color: "var(--series-2)" },
                  { id: "other", label: "other", value: n.disk.otherBytes, color: "var(--faint)" },
                  { id: "free", label: "free", value: n.disk.freeBytes, color: "var(--muted)" },
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
            <span className="ml-auto flex gap-1">
              {cmds.slice(0, 3).map((c) => (
                <StateBadge key={c.id} state={c.state} />
              ))}
              {cmds.length > 3 && <span className="text-faint">+{cmds.length - 3}</span>}
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
  const now = Date.parse(s.now);
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-[12px]">
        <thead className="eyebrow">
          <tr className="border-b border-border">
            {["node", "machine", "arch", "status", "cpus", "cpu busy", "ram", "used", "guaranteed", "swap", "psi mem", "disk free", "kernel", "last seen"].map((h) => (
              <th key={h} className="px-2 py-2 font-normal whitespace-nowrap">
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="num font-mono text-[11px]">
          {s.nodes.map((n) => (
            <tr key={n.id} className="border-b border-border/60 hover:bg-muted/60">
              <td className="px-2 py-2">
                <Link to="/nodes/$hostname" params={{ hostname: n.hostname }} className="text-foreground hover:text-primary">
                  {n.hostname}
                </Link>
              </td>
              <td className="px-2 py-2 font-sans text-muted-foreground">{n.machine}</td>
              <td className="px-2 py-2 text-muted-foreground">{n.arch}</td>
              <td className="px-2 py-2">
                <OnlineBadge online={n.online} />
              </td>
              <td className="px-2 py-2">{n.cpus}</td>
              <td className="px-2 py-2">{n.online ? percent(n.usage.cpuBusy) : "—"}</td>
              <td className="px-2 py-2">{bytes(n.memoryBytes)}</td>
              <td className="px-2 py-2">{n.online ? bytes(n.usage.memoryUsedBytes) : "—"}</td>
              <td className="px-2 py-2">{bytes(n.reservedMemoryBytes)}</td>
              <td className="px-2 py-2">{bytes(n.usage.swapUsedBytes)}</td>
              <td className="px-2 py-2">{n.online ? `${n.usage.memoryPressure.toFixed(1)}%` : "—"}</td>
              <td className="px-2 py-2">{bytes(n.disk.freeBytes)}</td>
              <td className="px-2 py-2 text-muted-foreground">{n.kernel}</td>
              <td className="px-2 py-2 text-muted-foreground">{ago(n.lastSeen, now)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
