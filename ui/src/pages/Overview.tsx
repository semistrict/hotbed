import { Link } from "@tanstack/react-router";
import { BaseState, CommandState, type Snapshot } from "../api";
import { Meter, SegmentedBar, Sparkline } from "../components/charts/Bars";
import { Columns } from "../components/charts/Columns";
import { Heatmap } from "../components/charts/Heatmap";
import { LineChart } from "../components/charts/LineChart";
import { SmallMultiples } from "../components/charts/SmallMultiples";
import { SpeedBars } from "../components/charts/SpeedBars";
import { EventFeed } from "../components/EventFeed";
import { ClusterObjectStoreSection } from "../components/ObjectStorePanel";
import { Card, StatTile } from "../components/ui/Card";
import { nodeName, useNodeColors, useSnapshot } from "../lib/data";
import { bytes, compact, duration, durationParts, percent, speedup } from "../lib/format";
import { clusterNow, compiled, rebuiltBuckets, savings, speedupBuckets, storage } from "../lib/stats";

export function OverviewPage() {
  const { data } = useSnapshot();
  const colors = useNodeColors(data?.nodes);
  if (!data) return null;
  return <Overview s={data} colors={colors} />;
}

function Overview({ s, colors }: { s: Snapshot; colors: Map<string, string> }) {
  const now = clusterNow(s);
  const day = savings(s.history);
  const store = storage(s);
  const live = s.nodes.filter((n) => n.online && colors.has(n.id));
  const recent = compiled(s.history).slice(-7).reverse();
  const speed = speedupBuckets(s.history);
  const rebuilt = rebuiltBuckets(s.history);
  const hourLabel = (t: number | bigint) => String(new Date(Number(t)).getHours()).padStart(2, "0");
  const lines = (pick: (v: { cpuBusy: number; memoryUsed: number; memoryPressure: number }) => number) =>
    live.map((n) => ({
      id: n.id,
      label: n.hostname,
      color: colors.get(n.id) ?? "var(--faint)",
      points: (s.series[n.id]?.samples ?? []).map((p) => ({ t: Number(p.t), v: pick(p) })),
    }));

  return (
    <div className="grid grid-cols-12 gap-4">
      {/* The headline: what warm forks saved. */}
      <section className="rise col-span-12 overflow-hidden rounded-3xl border border-border bg-card/80 lg:col-span-7">
        <div className="relative p-6 sm:p-8">
          <div className="pointer-events-none absolute -top-24 -left-16 size-80 rounded-full bg-primary/20 blur-3xl" />
          <div className="eyebrow relative">compile time saved · last 24 hours</div>
          <div className="relative mt-3 flex items-baseline gap-1 text-foreground">
            {durationParts(day.saved).map(([n, u]) => (
              <span key={u} className="flex items-baseline">
                <span className="display text-[88px] leading-[0.85] font-semibold sm:text-[120px]">{n}</span>
                <span className="display mr-4 text-[40px] font-light text-primary italic sm:text-[52px]">{u}</span>
              </span>
            ))}
          </div>
          <p className="relative mt-5 max-w-xl text-[14px] leading-relaxed text-muted-foreground">
            <span className="num font-mono text-foreground">{compact(day.builds)}</span> builds and tests started from a warm, forked base and ran in{" "}
            <span className="num font-mono text-foreground">{duration(day.spent)}</span>. From nothing they would have taken{" "}
            <span className="num font-mono text-foreground">{duration(day.cold)}</span> —{" "}
            <span className="display text-[18px] font-semibold text-primary italic">{speedup(day.cold, day.spent)} faster</span>.
          </p>
          <div className="relative mt-6">
            <Sparkline values={s.hourly.map((h) => h.savedSeconds)} height={46} />
            <div className="mt-1 flex justify-between font-mono text-[9.5px] text-faint">
              <span>24h ago</span>
              <span>saved per hour</span>
              <span>now</span>
            </div>
          </div>
        </div>
      </section>

      <Card className="col-span-12 lg:col-span-5" title="Latest builds" sub="Forked from a warm base, against the same command from nothing" delay={60} right={<Link to="/commands" className="font-mono text-[11px] text-muted-foreground hover:text-primary">commands →</Link>}>
        <SpeedBars
          rows={recent.map((c) => ({
            id: c.id,
            argv: c.argv,
            sub: `${c.repoId} · ${nodeName(s, c.nodeId)}${c.rebuilt !== undefined ? ` · ${c.rebuilt}/${c.of ?? 0} crates` : ""}`,
            seconds: c.seconds,
            coldSeconds: c.coldSeconds,
            failed: c.state === CommandState.FAILED,
          }))}
        />
      </Card>

      <div className="col-span-12 grid grid-cols-2 gap-3 sm:grid-cols-4 xl:grid-cols-8">
        <StatTile label="Nodes" value={now.online} unit={`/ ${now.nodes}`} foot={`${s.nodes.filter((n) => n.resources?.arch === "arm64").length} Apple Silicon · ${s.nodes.filter((n) => n.resources?.arch === "amd64").length} x86`} delay={80} />
        <StatTile label="Cores busy" value={now.coresBusy.toFixed(0)} unit={`/ ${now.cores}`} trend={<Meter value={now.coresBusy / now.cores} />} delay={100} />
        <StatTile label="Memory" value={(now.memoryUsed / 1024 ** 3).toFixed(0)} unit="GiB" trend={<Meter value={now.memoryUsed / now.memory} />} foot={`of ${bytes(now.memory)}`} delay={120} />
        <StatTile label="Running" value={now.running} tone="ember" foot={`${now.servers} dev servers`} delay={140} />
        <StatTile label="Queued" value={now.queued} foot="waiting for room" delay={160} />
        <StatTile label="Parked ❄" value={now.parked} tone="frost" foot={`${bytes(now.swapUsed)} in swap · ${s.totals24h?.parks ?? 0} today`} delay={180} />
        <StatTile label="OOM kills" value={s.totals24h?.oomKills ?? 0} tone="good" foot="frozen instead of killed" delay={200} />
        <StatTile label="Median fork" value={Number(s.totals24h?.medianForkMillis ?? 0n)} unit="ms" foot={`${compact(s.totals24h?.forks ?? 0)} forks today`} delay={220} />
      </div>

      <Card className="col-span-12 lg:col-span-6" title="CPU busy" sub="Share of each node's cores, last 15 minutes, on one scale" delay={240}>
        <SmallMultiples series={lines((p) => p.cpuBusy)} yMax={1} format={(v) => percent(v)} />
      </Card>
      <Card className="col-span-12 lg:col-span-6" title="Memory in use" sub="Share of each node's RAM, last 15 minutes; past 93%, or at 40% memory pressure, a node parks its newest command" delay={260}>
        <SmallMultiples series={lines((p) => p.memoryUsed)} yMax={1} format={(v) => percent(v)} />
      </Card>
      <Card className="col-span-12 lg:col-span-6" title="Memory pressure" sub="PSI some avg10: time any task waited on memory. Admission pauses past 22%." delay={280}>
        <LineChart series={lines((p) => p.memoryPressure)} format={(v) => `${v.toFixed(0)}%`} thresholds={[{ v: 22, label: "nodes hold new work" }]} />
      </Card>
      <Card className="col-span-12 lg:col-span-6" title="Commands" sub="Across the cluster, last 15 minutes" delay={300}>
        <LineChart
          series={[
            { id: "running", label: "running", color: "var(--series-1)", points: s.load.map((l) => ({ t: Number(l.t), v: l.running })) },
            { id: "queued", label: "queued", color: "var(--series-2)", points: s.load.map((l) => ({ t: Number(l.t), v: l.queued })) },
            { id: "parked", label: "parked", color: "var(--series-3)", points: s.load.map((l) => ({ t: Number(l.t), v: l.parked })) },
          ]}
          format={(v) => v.toFixed(0)}
        />
      </Card>

      <Card className="col-span-12 lg:col-span-8" title="Compile hours, per hour" sub="Each column is what the hour's builds would cost from nothing: the part they spent, and the part warm bases saved" delay={320}>
        <Columns
          columns={s.hourly.map((h) => ({ key: String(h.hour), label: hourLabel(h.hour), values: [h.spentSeconds / 3600, h.savedSeconds / 3600] }))}
          series={[
            { id: "spent", label: "spent", color: "var(--series-2)" },
            { id: "saved", label: "saved", color: "var(--series-1)" },
          ]}
          format={(v) => `${v.toFixed(v < 10 ? 1 : 0)}h`}
          labelEvery={3}
          current={String(s.hourly[s.hourly.length - 1]?.hour)}
        />
      </Card>
      <Card className="col-span-12 sm:col-span-6 lg:col-span-4" title="Speedup" sub="Builds and tests by cold time over actual, 24 h" delay={340}>
        <Columns columns={speed.map((b) => ({ key: b.label, label: b.label, values: [b.count] }))} series={[{ id: "n", label: "builds", color: "var(--series-1)" }]} format={(v) => compact(v)} />
      </Card>

      <Card className="col-span-12 lg:col-span-8" title="CPU by node and hour" sub="Mean share of cores busy, last 24 hours" delay={360}>
        <Heatmap
          rows={s.nodes.map((n) => ({
            id: n.id,
            label: n.hostname,
            cells: (s.nodeHourly[n.id]?.hours ?? []).map((h) => ({ key: String(h.hour), label: `${hourLabel(h.hour)}:00`, v: h.cpuBusy === 0 && !n.online ? null : h.cpuBusy })),
          }))}
          format={(v) => `${percent(v)} busy`}
        />
      </Card>
      <Card className="col-span-12 sm:col-span-6 lg:col-span-4" title="Storage" sub="Forks share their base's blocks; they own only what their builds rewrote" delay={380}>
        <div className="flex items-baseline gap-2">
          <span className="display text-[44px] leading-none font-semibold">{bytes(store.forks)}</span>
          <span className="font-mono text-[11px] text-muted-foreground">added by {store.workspaces} workspaces</span>
        </div>
        <p className="mt-2 text-[12px] leading-relaxed text-muted-foreground">
          As full copies they would take <span className="num font-mono text-foreground">{bytes(store.asCopies)}</span>. As forks they add only the blocks their builds rewrote:{" "}
          <span className="display text-[15px] font-semibold text-primary italic">{(store.asCopies / Math.max(1, store.forks)).toFixed(0)}× less</span>.
        </p>
        <div className="mt-5">
          <SegmentedBar
            tall
            format={bytes}
            parts={[
              { id: "bases", label: "warm bases", value: store.bases, color: "var(--series-1)" },
              { id: "forks", label: "fork-owned", value: store.forks, color: "var(--series-2)" },
              { id: "other", label: "other", value: store.other, color: "var(--faint)" },
              { id: "free", label: "free", value: store.free, color: "var(--muted)" },
            ]}
          />
        </div>
      </Card>

      <ClusterObjectStoreSection s={s} colors={colors} />

      <Card className="col-span-12 lg:col-span-7" title="Live" sub="Everything the cluster did, newest first" delay={400}>
        <EventFeed events={s.events} limit={20} />
      </Card>
      <div className="col-span-12 grid gap-4 lg:col-span-5">
        <Card title="Crates rebuilt per build" sub="How big a change each build carried, 24 h" delay={420}>
          <Columns columns={rebuilt.map((b) => ({ key: b.label, label: b.label, values: [b.count] }))} series={[{ id: "n", label: "builds", color: "var(--series-3)" }]} format={(v) => compact(v)} height={140} />
        </Card>
        <Card title="Bases" sub="Main, built and kept warm on each node that hosts the repo" delay={440}>
          <BaseList s={s} />
        </Card>
      </div>
    </div>
  );
}

function BaseList({ s }: { s: Snapshot }) {
  const rows = s.projects
    .map((p) => {
      const bases = s.bases.filter((b) => b.project === p.name && b.state !== BaseState.STALE);
      return {
        p,
        warm: bases.filter((b) => b.state === BaseState.WARM).length,
        building: bases.filter((b) => b.state === BaseState.BUILDING),
        failed: bases.filter((b) => b.state === BaseState.FAILED).length,
        gen: Math.max(0, ...bases.map((b) => b.generation)),
      };
    })
    .filter((row) => row.gen > 0);
  return (
    <ul className="divide-y divide-border text-[12px]">
      {rows.map(({ p, warm, building, failed, gen }) => (
        <li key={p.name} className="flex items-center gap-3 py-2">
          <span className="font-mono text-foreground">{p.name}</span>
          <span className="num font-mono text-faint">g{gen}</span>
          <span className="ml-auto text-muted-foreground">
            <span className="num font-mono text-foreground">{warm}</span> warm
            {failed > 0 && <span className="ml-2 font-mono text-critical">{failed} failed</span>}
          </span>
          {building.length > 0 ? (
            <span className="w-28">
              <Meter value={building[0]?.progress ?? 0} warn={2} danger={2} />
              <span className="font-mono text-[9.5px] text-primary">building g{building[0]?.generation}</span>
            </span>
          ) : (
            <span className="w-28 text-right font-mono text-[10px] text-faint">up to date</span>
          )}
        </li>
      ))}
    </ul>
  );
}
