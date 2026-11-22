import { Link, useSearch } from "@tanstack/react-router";
import type { CommandState } from "../api";
import { HistoryTable, LiveCommandTable } from "../components/CommandTable";
import { Columns } from "../components/charts/Columns";
import { Card, StatTile } from "../components/ui/Card";
import { bytes, duration } from "../lib/format";
import { isLive } from "../lib/live";
import { useScopedSnapshot } from "../lib/project";
import { clusterNow } from "../lib/stats";
import { PageTitle } from "./Nodes";

const STATES: { id: CommandState | "live"; label: string }[] = [
  { id: "live", label: "all live" },
  { id: "running", label: "running" },
  { id: "queued", label: "queued" },
  { id: "parked", label: "parked" },
];

export function CommandsPage() {
  const { state = "live", node } = useSearch({ from: "/commands" });
  const { data: s } = useScopedSnapshot();
  if (!s) return null;
  const now = clusterNow(s);
  const live = s.commands
    .filter((c) => (state === "live" ? isLive(c) : c.state === state))
    .filter((c) => !node || c.nodeId === node)
    .sort((a, b) => order(a.state) - order(b.state) || a.queuedAt.localeCompare(b.queuedAt));
  const ended = s.history.slice(-12).reverse();
  const waiting = s.commands.filter((c) => c.state === "queued");
  const reserved = s.nodes.reduce((n, x) => n + x.reservedMemoryBytes, 0);

  return (
    <div className="space-y-6">
      <PageTitle title="Commands" sub="Builds, tests and dev servers agents run in their workspaces. Each runs in its own cgroup: a CPU weight, a memory guarantee and a ceiling." />
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-6">
        <StatTile label="Running" value={now.running} tone="ember" foot={`${now.coresBusy.toFixed(0)} of ${now.cores} cores busy`} />
        <StatTile label="Queued" value={now.queued} foot={waiting[0]?.reason ?? "nothing waiting"} />
        <StatTile label="Parked ❄" value={now.parked} tone="frost" foot={`${bytes(now.swapUsed)} in swap`} />
        <StatTile label="Dev servers" value={now.servers} foot="no time limit" />
        <StatTile label="Guaranteed" value={bytes(reserved)} foot={`of ${bytes(now.memory)} RAM online`} />
        <StatTile label="Queue p95" value={duration(s.hourly[s.hourly.length - 1]?.queueWaitP95Seconds ?? 0)} foot="this hour" />
      </div>

      <div className="rise flex flex-wrap items-center gap-2">
        {STATES.map((x) => (
          <Link
            key={x.id}
            to="/commands"
            search={{ state: x.id === "live" ? undefined : x.id, node }}
            className={`rounded-full border px-3 py-1 font-mono text-[11px] ${state === x.id ? "border-primary bg-ember-wash text-foreground" : "border-border text-muted-foreground hover:border-border-strong"}`}
          >
            {x.label}
          </Link>
        ))}
        <span className="mx-2 h-4 w-px bg-border" />
        <Link to="/commands" search={{ state: state === "live" ? undefined : state }} className={`rounded-full border px-3 py-1 font-mono text-[11px] ${!node ? "border-primary text-foreground" : "border-border text-muted-foreground"}`}>
          every node
        </Link>
        {s.nodes
          .filter((n) => n.online)
          .map((n) => (
            <Link
              key={n.id}
              to="/commands"
              search={{ state: state === "live" ? undefined : state, node: n.id }}
              className={`rounded-full border px-3 py-1 font-mono text-[11px] ${node === n.id ? "border-primary text-foreground" : "border-border text-muted-foreground hover:border-border-strong"}`}
            >
              {n.hostname}
            </Link>
          ))}
      </div>

      <Card title={`${live.length} ${state === "live" ? "live" : state}`}>
        <LiveCommandTable s={s} commands={live} />
      </Card>

      <div className="grid gap-4">
        <Card title="Just ended" right={<Link to="/history" className="font-mono text-[11px] text-muted-foreground hover:text-primary">all history →</Link>}>
          <HistoryTable s={s} rows={ended} />
        </Card>
        <Card title="Queue wait, p95" sub="How long commands waited for room, per hour, last 24 hours">
          <Columns
            columns={s.hourly.map((h) => ({ key: String(h.hour), label: String(new Date(h.hour).getHours()).padStart(2, "0"), values: [h.queueWaitP95Seconds] }))}
            series={[{ id: "wait", label: "p95 wait", color: "var(--series-2)" }]}
            format={(v) => `${v.toFixed(0)}s`}
            labelEvery={2}
            height={140}
          />
        </Card>
      </div>
    </div>
  );
}

function order(s: CommandState): number {
  return { running: 0, parked: 1, queued: 2, succeeded: 3, failed: 4, cancelled: 5 }[s];
}
