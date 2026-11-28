import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { Trash2 } from "lucide-react";
import { useState } from "react";
import { Button } from "@/components/ui/button";
import { BaseState, baseStateLabel, CommandState, nodeAdmission, WorkspaceState, type Base } from "../api";
import { SessionLink } from "../components/SessionLink";
import { Meter, SegmentedBar } from "../components/charts/Bars";
import { Columns } from "../components/charts/Columns";
import { LineChart } from "../components/charts/LineChart";
import { HistoryTable, LiveCommandTable } from "../components/CommandTable";
import { AdmissionBadge, AdmissionPanel, heldBands, PlacementList } from "../components/Admission";
import { ContinuousProfilePanel } from "../components/ContinuousProfilePanel";
import { DaemonLogsPanel } from "../components/DaemonLogsPanel";
import { DeleteNodeDialog } from "../components/DeleteNodeDialog";
import { EventFeed } from "../components/EventFeed";
import { NodeObjectStoreSection } from "../components/ObjectStorePanel";
import { Card, Empty, StatTile } from "../components/Panel";
import { ArchBadge, BuildingBadge, OnlineBadge } from "../components/StateBadges";
import { isReadOnlyViewer, useAuthMe } from "../lib/auth";
import { useNodeColors, useSnapshot } from "../lib/data";
import { ago, bytes, compact, duration, percent } from "../lib/format";
import { isLive } from "../lib/live";
import { failureRate, savings } from "../lib/stats";
import { PageTitle } from "../components/PageTitle";
import { NotFound } from "./NotFound";

export function NodePage() {
  const { hostname } = useParams({ from: "/nodes/$hostname" });
  const navigate = useNavigate();
  const { data: s } = useSnapshot();
  const { data: auth } = useAuthMe();
  const colors = useNodeColors(s?.nodes);
  const [deleteOpen, setDeleteOpen] = useState(false);
  if (!s) return null;
  const n = s.nodes.find((x) => x.hostname === hostname);
  if (!n) return <NotFound />;
  const isViewer = isReadOnlyViewer(auth);
  const now = Number(s.now);
  const color = colors.get(n.id) ?? "var(--series-1)";
  const series = s.series[n.id]?.samples ?? [];
  const live = s.commands.filter((c) => c.nodeId === n.id && isLive(c));
  const history = s.history.filter((c) => c.nodeId === n.id).reverse();
  const day = savings(history);
  const bases = s.bases.filter((b) => b.nodeId === n.id);
  const buildingBases = bases.filter((b) => b.state === BaseState.BUILDING);
  const ws = s.workspaces.filter((w) => w.nodeId === n.id && w.state !== WorkspaceState.RELEASED);
  const hours = s.nodeHourly[n.id]?.hours ?? [];
  const line = (label: string, pick: (p: (typeof series)[number]) => number) => [{ id: n.id, label, color, points: series.map((p) => ({ t: Number(p.t), v: pick(p) })) }];
  const held = s.admissionEvents.filter((e) => e.nodeId === n.id);
  const placements = s.placements.filter((d) => d.candidates.some((c) => c.nodeId === n.id));
  const admission = nodeAdmission(n);
  const cpuSignal = admission?.signals.find((x) => x.name === "cpu-pressure");
  const res = n.resources;
  const usage = n.usage;
  const disk = n.disk;

  return (
    <div className="space-y-6">
      <PageTitle
        title={n.hostname}
        sub={`${res?.machine ?? ""} · ${res?.kernel ?? ""} · ${res?.benchCpus ? `measurements on CPUs ${res.benchCpus}` : "no measurements"} · joined ${ago(n.joinedAt, now)}`}
        right={
          <div className="flex flex-wrap items-center gap-2.5">
            {n.profiler && (
              <span
                title={n.profiler.error || `${n.profiler.engine} · ${n.profiler.totalSamples.toLocaleString()} samples`}
                className={`rounded-full px-2.5 py-0.5 font-mono text-[10.5px] ${
                  n.profiler.active ? "bg-ember-wash text-primary" : "bg-muted text-muted-foreground"
                }`}
              >
                {n.profiler.active ? `OTel eBPF · ${n.profiler.sampleHz} Hz` : "OTel eBPF off"}
              </span>
            )}
            <AdmissionBadge a={admission} now={now} />
            <ArchBadge arch={res?.arch ?? ""} />
            <OnlineBadge online={n.online} />
            <Link
              to="/settings"
              search={{ scope: n.hostname }}
              className="rounded-full border border-border bg-card/70 px-2.5 py-0.5 font-mono text-[10.5px] text-muted-foreground transition-colors hover:border-primary/50 hover:text-primary"
            >
              configure →
            </Link>
            {!isViewer && (
              <span
                className="inline-flex"
                title={
                  n.online
                    ? "Stop the worker on this node first (hotbed worker stop) before deleting it."
                    : `Delete ${n.hostname} from the cluster`
                }
              >
                <Button
                  type="button"
                  variant="outline"
                  size="xs"
                  disabled={n.online}
                  onClick={() => setDeleteOpen(true)}
                  aria-label={`Delete ${n.hostname}`}
                  className="h-auto gap-1 rounded-full border-border bg-card/70 px-2.5 py-0.5 font-mono text-[10.5px] text-muted-foreground hover:border-critical/60 hover:text-critical"
                >
                  <Trash2 className="size-3" />
                  Delete
                </Button>
              </span>
            )}
          </div>
        }
      />
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-6">
        <StatTile label="CPU busy" value={n.online ? percent(usage?.cpuBusy ?? 0) : "—"} unit={`of ${res?.cpus ?? 0}`} />
        <StatTile label="Memory" value={n.online ? bytes(usage?.memoryUsedBytes ?? 0n) : "—"} unit={`/ ${bytes(res?.memoryBytes ?? 0n)}`} foot={`${bytes(n.reservedMemoryBytes)} guaranteed`} />
        <StatTile label="Swap" value={n.online ? bytes(usage?.swapUsedBytes ?? 0n) : "—"} unit={`/ ${bytes(res?.swapBytes ?? 0n)}`} tone={n.online && (usage?.swapUsedBytes ?? 0n) > 0n ? "frost" : "default"} />
        <StatTile label="Saved today" value={duration(day.saved)} tone="ember" foot={`${compact(day.builds)} builds`} />
        <StatTile label="Failure rate" value={percent(failureRate(history))} foot={`${history.filter((h) => h.state === CommandState.FAILED).length} failed today`} />
        <StatTile
          label="Live"
          value={live.length}
          unit="commands"
          foot={`${buildingBases.length > 0 ? `${buildingBases.length} base building · ` : ""}${ws.length} workspaces · ${bases.filter((b) => b.state !== BaseState.STALE).length} bases`}
        />
      </div>

      <div className="grid gap-4 lg:grid-cols-3">
        <Card title="CPU busy" sub="Last 15 minutes">
          <LineChart series={line("cpu", (p) => p.cpuBusy)} yMax={1} format={(v) => percent(v)} height={150} />
        </Card>
        <Card title="Memory in use" sub="Last 15 minutes">
          <LineChart series={line("memory", (p) => p.memoryUsed)} yMax={1} format={(v) => percent(v)} thresholds={[{ v: 0.93, label: "park" }]} height={150} />
        </Card>
        <Card title="Memory pressure" sub="PSI some avg10">
          <LineChart series={line("psi", (p) => p.memoryPressure)} format={(v) => `${v.toFixed(0)}%`} thresholds={[{ v: 22, label: "hold new work" }]} height={150} />
        </Card>
      </div>

      <section className="space-y-4">
        <h2 className="eyebrow">Admission · how this node decides whether to start new work</h2>
        <div className="grid gap-4 lg:grid-cols-5">
          <Card className="lg:col-span-3" title="Signals" sub="Each against the limit at which the node holds new work; measurements, on CPUs of their own, are held only by memory and disk">
            <AdmissionPanel a={admission} now={now} />
          </Card>
          <Card className="lg:col-span-2" title="CPU pressure" sub="PSI some avg10 · shaded where the node held new work">
            <LineChart
              series={line("cpu pressure", (p) => p.cpuPressure)}
              yMax={100}
              format={(v) => `${v.toFixed(0)}%`}
              thresholds={[
                { v: cpuSignal?.limit ?? 60, label: "hold" },
                { v: cpuSignal?.resume ?? 30, label: "resume" },
              ]}
              bands={heldBands(series)}
              height={190}
            />
          </Card>
        </div>
        <div className="grid gap-4 lg:grid-cols-2">
          <Card title="Held work" sub={`Every time it held new work, made a command wait, or started again · ${held.length} in the last day`}>
            <Columns
              columns={hours.map((h) => ({ key: String(h.hour), label: String(new Date(Number(h.hour)).getHours()).padStart(2, "0"), values: [h.holding * 100] }))}
              series={[{ id: "held", label: "held new work", color: "var(--serious)" }]}
              format={(v) => `${v.toFixed(0)}%`}
              labelEvery={3}
              height={90}
            />
            <div className="mt-3">
              <EventFeed events={held} limit={40} />
            </div>
          </Card>
          <Card title="Placements" sub={`Where the controller put workspaces, and why: ${placements.length} weighed this node in the last day`}>
            <PlacementList placements={placements} nodeId={n.id} limit={25} now={now} />
          </Card>
        </div>
      </section>

      <div className="grid gap-4 lg:grid-cols-3">
        <Card className="lg:col-span-2" title="24 hours" sub="Mean CPU busy per hour">
          <Columns
            columns={hours.map((h) => ({ key: String(h.hour), label: String(new Date(Number(h.hour)).getHours()).padStart(2, "0"), values: [h.cpuBusy * 100] }))}
            series={[{ id: "cpu", label: "cpu", color }]}
            format={(v) => `${v.toFixed(0)}%`}
            labelEvery={3}
            height={150}
          />
        </Card>
        {n.objectStore ? (
          <Card
            title={n.objectStore.hostDiskSizeBytes > 0n ? "cladefs disk" : "cladefs storage"}
            sub={
              n.objectStore.hostDiskSizeBytes > 0n
                ? `cladefs · ${bytes(n.objectStore.hostDiskSizeBytes)}${n.objectStore.hostDiskPressure ? " · pressure" : ""}`
                : `SSD part cache · ${bytes(n.objectStore.partCacheBytes)}${n.objectStore.partCacheCapacityBytes > 0n ? ` / ${bytes(n.objectStore.partCacheCapacityBytes)}` : ""}`
            }
          >
            {n.objectStore.hostDiskSizeBytes > 0n ? (
              <SegmentedBar
                tall
                format={bytes}
                parts={[
                  { id: "cache", label: "part cache", value: Number(n.objectStore.hostDiskCacheBytes), color: "var(--series-1)" },
                  { id: "cells", label: "cells", value: Number(n.objectStore.hostDiskCellsBytes), color: "var(--series-2)" },
                  { id: "other", label: "other", value: Number(n.objectStore.hostDiskOtherBytes), color: "var(--faint)" },
                  { id: "free", label: "free", value: Number(n.objectStore.hostDiskFreeBytes), color: "var(--muted)" },
                ]}
              />
            ) : (
              <Meter
                value={n.objectStore.partCacheCapacityBytes > 0n ? Number(n.objectStore.partCacheBytes) / Number(n.objectStore.partCacheCapacityBytes) : 0}
                warn={0.85}
                danger={0.95}
              />
            )}
            <BaseList bases={bases} />
          </Card>
        ) : (
          <Card title="Disk" sub={`${n.storage}, ${bytes(n.diskBytes)}`}>
            <SegmentedBar
              tall
              format={bytes}
              parts={[
                { id: "bases", label: "warm bases", value: Number(disk?.basesBytes ?? 0n), color: "var(--series-1)" },
                { id: "forks", label: "fork-owned", value: Number(disk?.forksExclusiveBytes ?? 0n), color: "var(--series-2)" },
                { id: "other", label: "other", value: Number(disk?.otherBytes ?? 0n), color: "var(--faint)" },
                { id: "free", label: "free", value: Number(disk?.freeBytes ?? 0n), color: "var(--muted)" },
              ]}
            />
            <BaseList bases={bases} />
          </Card>
        )}
      </div>

      <NodeObjectStoreSection n={n} s={s} />

      <ContinuousProfilePanel key={`node-prof-${n.hostname}`} node={n.hostname} live={n.online} />

      <DaemonLogsPanel
        key={`node-logs-${n.hostname}`}
        lockedSource={n.hostname}
        compact
        headerRight={
          <Link to="/logs" search={{ source: n.hostname }} className="text-muted-foreground hover:text-primary">
            all logs →
          </Link>
        }
      />

      <Card title="Running here" sub="Queued, running, parked, paused commands and base builds">
        {buildingBases.length > 0 && (
          <ul className={`divide-y divide-border text-[12px] ${live.length > 0 ? "mb-3 border-b border-border pb-2" : ""}`}>
            {buildingBases.map((b) => (
              <li key={b.id} className="flex flex-wrap items-center gap-3 py-2">
                <BuildingBadge project={b.project} generation={b.generation} progress={b.progress} />
                <span className="font-mono text-foreground">{b.project}</span>
                <span className="font-mono text-[11px] text-faint">g{b.generation}</span>
                <span className="num font-mono text-[11px] text-primary">{percent(b.progress)}</span>
                {b.command ? (
                  <Link to="/commands/$id" params={{ id: b.command }} className="ml-auto font-mono text-[11px] text-muted-foreground hover:text-primary">
                    {b.id} · {b.command}
                  </Link>
                ) : (
                  <span className="ml-auto font-mono text-[11px] text-muted-foreground">{b.id}</span>
                )}
              </li>
            ))}
          </ul>
        )}
        {(live.length > 0 || buildingBases.length === 0) && <LiveCommandTable s={s} commands={live} />}
      </Card>
      <div className="grid gap-4 lg:grid-cols-2">
        <Card title="Workspaces here">
          {ws.length === 0 ? (
            <Empty>No workspaces on this node.</Empty>
          ) : (
            <ul className="divide-y divide-border text-[12px]">
              {ws.map((w) => (
                <li key={w.id} className="flex flex-wrap items-center gap-x-3 gap-y-1 py-2">
                  <Link to="/workspaces/$id" params={{ id: w.id }} className="font-mono text-foreground hover:text-primary">
                    {w.id}
                  </Link>
                  <SessionLink id={w.sessionId} empty={null} />
                  <span className="truncate text-muted-foreground">{w.agent}</span>
                  <span className="ml-auto font-mono text-[11px] text-faint">{w.project}</span>
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
      <DeleteNodeDialog
        node={deleteOpen ? n : null}
        onOpenChange={setDeleteOpen}
        onDeleted={() => void navigate({ to: "/nodes" })}
      />
    </div>
  );
}

/** The node's bases on disk: each one's project, generation, state and size. */
function BaseList({ bases }: { bases: Base[] }) {
  return (
    <ul className="mt-4 divide-y divide-border text-[11.5px]">
      {bases.map((b) => (
        <li key={b.id} className="flex items-center gap-2 py-1.5 font-mono">
          <span className="text-foreground">{b.project}</span>
          <span className="text-faint">g{b.generation}</span>
          <span
            className={`text-[10px] ${
              b.state === BaseState.WARM || b.state === BaseState.BUILDING ? "text-primary" : b.state === BaseState.FAILED ? "text-critical" : "text-faint"
            }`}
          >
            {b.state === BaseState.BUILDING ? `building ${percent(b.progress)}` : baseStateLabel(b.state)}
          </span>
          <span className="num ml-auto text-muted-foreground">{bytes(b.sizeBytes)}</span>
        </li>
      ))}
    </ul>
  );
}
