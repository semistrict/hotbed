import { Link, useParams } from "@tanstack/react-router";
import { SegmentedBar } from "../components/charts/Bars";
import { Columns } from "../components/charts/Columns";
import { LineChart } from "../components/charts/LineChart";
import { HistoryTable, LiveCommandTable } from "../components/CommandTable";
import { EventFeed } from "../components/EventFeed";
import { Card, Empty, StatTile } from "../components/ui/Card";
import { ArchBadge, OnlineBadge } from "../components/ui/State";
import { useNodeColors, useSnapshot } from "../lib/data";
import { ago, bytes, compact, duration, percent } from "../lib/format";
import { isLive } from "../lib/live";
import { failureRate, savings } from "../lib/stats";
import { NotFound } from "./NotFound";
import { PageTitle } from "./Nodes";

export function NodePage() {
  const { hostname } = useParams({ from: "/nodes/$hostname" });
  const { data: s } = useSnapshot();
  const colors = useNodeColors(s?.nodes);
  if (!s) return null;
  const n = s.nodes.find((x) => x.hostname === hostname);
  if (!n) return <NotFound />;
  const now = Date.parse(s.now);
  const color = colors.get(n.id) ?? "var(--series-1)";
  const series = s.series[n.id] ?? [];
  const live = s.commands.filter((c) => c.nodeId === n.id && isLive(c));
  const history = s.history.filter((c) => c.nodeId === n.id).reverse();
  const day = savings(history);
  const bases = s.bases.filter((b) => b.nodeId === n.id);
  const ws = s.workspaces.filter((w) => w.nodeId === n.id && w.state !== "released");
  const hours = s.nodeHourly[n.id] ?? [];
  const line = (label: string, pick: (p: (typeof series)[number]) => number) => [{ id: n.id, label, color, points: series.map((p) => ({ t: p.t, v: pick(p) })) }];

  return (
    <div className="space-y-6">
      <PageTitle
        title={n.hostname}
        sub={`${n.machine} · ${n.kernel} · joined ${ago(n.joinedAt, now)}`}
        right={
          <div className="flex items-center gap-3">
            <ArchBadge arch={n.arch} />
            <OnlineBadge online={n.online} />
          </div>
        }
      />
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-6">
        <StatTile label="CPU busy" value={percent(n.usage.cpuBusy)} unit={`of ${n.cpus}`} />
        <StatTile label="Memory" value={bytes(n.usage.memoryUsedBytes)} unit={`/ ${bytes(n.memoryBytes)}`} foot={`${bytes(n.reservedMemoryBytes)} guaranteed`} />
        <StatTile label="Swap" value={bytes(n.usage.swapUsedBytes)} unit={`/ ${bytes(n.swapBytes)}`} tone={n.usage.swapUsedBytes > 0 ? "frost" : "default"} />
        <StatTile label="Saved today" value={duration(day.saved)} tone="ember" foot={`${compact(day.builds)} builds`} />
        <StatTile label="Failure rate" value={percent(failureRate(history))} foot={`${history.filter((h) => h.state === "failed").length} failed today`} />
        <StatTile label="Live" value={live.length} unit="commands" foot={`${ws.length} workspaces · ${bases.filter((b) => b.state !== "stale").length} bases`} />
      </div>

      <div className="grid gap-4 lg:grid-cols-3">
        <Card title="CPU busy" sub="Last 15 minutes">
          <LineChart series={line("cpu", (p) => p.cpuBusy)} yMax={1} format={(v) => percent(v)} height={150} />
        </Card>
        <Card title="Memory in use" sub="Last 15 minutes">
          <LineChart series={line("memory", (p) => p.memoryUsed)} yMax={1} format={(v) => percent(v)} threshold={{ v: 0.93, label: "park" }} height={150} />
        </Card>
        <Card title="Memory pressure" sub="PSI some avg10">
          <LineChart series={line("psi", (p) => p.memoryPressure)} format={(v) => `${v.toFixed(0)}%`} threshold={{ v: 22, label: "pause admission" }} height={150} />
        </Card>
      </div>

      <div className="grid gap-4 lg:grid-cols-3">
        <Card className="lg:col-span-2" title="24 hours" sub="Mean CPU and memory per hour">
          <Columns
            columns={hours.map((h) => ({ key: String(h.hour), label: String(new Date(h.hour).getHours()).padStart(2, "0"), values: [h.cpuBusy * 100] }))}
            series={[{ id: "cpu", label: "cpu", color }]}
            format={(v) => `${v.toFixed(0)}%`}
            labelEvery={3}
            height={150}
          />
        </Card>
        <Card title="Disk" sub={`btrfs, ${bytes(n.diskBytes)}`}>
          <SegmentedBar
            tall
            format={bytes}
            parts={[
              { id: "bases", label: "warm bases", value: n.disk.basesBytes, color: "var(--series-1)" },
              { id: "forks", label: "fork-owned", value: n.disk.forksExclusiveBytes, color: "var(--series-2)" },
              { id: "other", label: "other", value: n.disk.otherBytes, color: "var(--faint)" },
              { id: "free", label: "free", value: n.disk.freeBytes, color: "var(--muted)" },
            ]}
          />
          <ul className="mt-4 divide-y divide-border text-[11.5px]">
            {bases.map((b) => (
              <li key={b.id} className="flex items-center gap-2 py-1.5 font-mono">
                <span className="text-foreground">{b.repoId}</span>
                <span className="text-faint">g{b.generation}</span>
                <span className={`text-[10px] ${b.state === "warm" ? "text-primary" : "text-faint"}`}>{b.state === "building" ? `building ${percent(b.progress)}` : b.state}</span>
                <span className="num ml-auto text-muted-foreground">{bytes(b.sizeBytes)}</span>
              </li>
            ))}
          </ul>
        </Card>
      </div>

      <Card title="Running here" sub="Queued, running and parked commands">
        <LiveCommandTable s={s} commands={live} />
      </Card>
      <div className="grid gap-4 lg:grid-cols-2">
        <Card title="Workspaces here">
          {ws.length === 0 ? (
            <Empty>No workspaces on this node.</Empty>
          ) : (
            <ul className="divide-y divide-border text-[12px]">
              {ws.map((w) => (
                <li key={w.id} className="flex items-center gap-3 py-2">
                  <Link to="/workspaces/$id" params={{ id: w.id }} className="font-mono text-foreground hover:text-primary">
                    {w.id}
                  </Link>
                  <span className="truncate text-muted-foreground">{w.agent}</span>
                  <span className="ml-auto font-mono text-[11px] text-faint">{w.repoId}</span>
                  <span className="num font-mono text-[11px] text-muted-foreground">{bytes(w.exclusiveBytes)}</span>
                </li>
              ))}
            </ul>
          )}
        </Card>
        <Card title="Events here">
          <EventFeed events={s.events.filter((e) => e.nodeId === n.id)} limit={12} />
        </Card>
      </div>
      <Card title="Ended here, 24 h" sub={`${history.length} commands`}>
        <HistoryTable s={s} rows={history} limit={60} />
      </Card>
    </div>
  );
}
