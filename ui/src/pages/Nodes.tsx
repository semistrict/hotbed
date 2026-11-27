import { Link } from "@tanstack/react-router";
import { Trash2 } from "lucide-react";
import { useState } from "react";
import { Button } from "@/components/ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { BaseState, CommandState, nodeAdmission, WorkspaceState, type Node, type ObjectStoreStats, type Snapshot } from "../api";
import { AdmissionBadge, PlacementList } from "../components/Admission";
import { Meter, SegmentedBar, Sparkline } from "../components/charts/Bars";
import { Heatmap } from "../components/charts/Heatmap";
import { DeleteNodeDialog } from "../components/DeleteNodeDialog";
import { EventFeed } from "../components/EventFeed";
import { nodeCacheHit } from "../components/ObjectStorePanel";
import { Card } from "../components/Panel";
import { ArchBadge, BuildingBadge, OnlineBadge, StateBadge } from "../components/StateBadges";
import { Steady } from "../components/Steady";
import { isReadOnlyViewer, useAuthMe } from "../lib/auth";
import { useNodeColors, useSnapshot } from "../lib/data";
import { ago, bytes, compact, percent } from "../lib/format";
import { formatHostUsageFoot, isHostStarved, nodeHostUsage } from "../lib/hostUsage";

export function NodesPage() {
  const { data } = useSnapshot();
  const { data: auth } = useAuthMe();
  const colors = useNodeColors(data?.nodes);
  const [deleteTargetId, setDeleteTargetId] = useState<string | null>(null);
  if (!data) return null;
  const nowMs = Number(data.now);
  const isViewer = isReadOnlyViewer(auth);
  const onDelete = isViewer ? undefined : (n: Node) => setDeleteTargetId(n.id);
  const deleteTarget = deleteTargetId ? (data.nodes.find((n) => n.id === deleteTargetId) ?? null) : null;
  return (
    <div className="space-y-6">
      <PageTitle title="Nodes" sub={`${data.nodes.filter((n) => n.online).length} of ${data.nodes.length} online · old PCs and Apple Silicon laptops on Asahi Linux`} />
      <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
        {data.nodes.map((n, i) => (
          <NodeCard key={n.id} n={n} s={data} color={colors.get(n.id) ?? "var(--faint)"} now={nowMs} delay={i * 50} onDelete={onDelete} />
        ))}
      </div>
      <Admission s={data} now={nowMs} />
      <Card title="All nodes" sub="Every figure on this page as a table" delay={400}>
        <NodeTable s={data} onDelete={onDelete} />
      </Card>
      <DeleteNodeDialog
        node={deleteTarget}
        onOpenChange={(open) => {
          if (!open) setDeleteTargetId(null);
        }}
      />
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
    <div className="rise flex flex-wrap items-end gap-3 sm:gap-4">
      <div className="min-w-0 flex-1">
        <h1 className="display break-words text-[30px] leading-[1.05] font-semibold sm:text-[38px] lg:text-[44px]">{title}</h1>
        {sub && <p className="mt-1.5 break-words text-[12.5px] leading-relaxed text-muted-foreground sm:mt-2 sm:text-[13px]">{sub}</p>}
      </div>
      {right && <div className="min-w-0 sm:ml-auto">{right}</div>}
    </div>
  );
}

export function NodeCard({ n, s, color, now, delay, onDelete }: { n: Node; s: Snapshot; color: string; now: number; delay: number; onDelete?: (n: Node) => void }) {
  const buildingBases = s.bases.filter((b) => b.nodeId === n.id && b.state === BaseState.BUILDING);
  const buildingCmdIds = new Set(buildingBases.map((b) => b.command).filter(Boolean));
  const cmds = s.commands.filter(
    (c) =>
      c.nodeId === n.id &&
      !buildingCmdIds.has(c.id) &&
      (c.state === CommandState.RUNNING || c.state === CommandState.PARKED || c.state === CommandState.PAUSED || c.state === CommandState.QUEUED),
  );
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
    <div
      className={`rise group relative block min-w-0 rounded-2xl border border-border bg-card/80 p-4 transition-[color,border-color,opacity] hover:border-border-strong sm:p-5 ${n.online ? "" : "opacity-60 hover:opacity-100"}`}
      style={{ animationDelay: `${delay}ms` }}
    >
      <Steady>
        <div className="flex flex-wrap items-start gap-2.5 sm:flex-nowrap sm:gap-3">
          <span className="mt-2 inline-block h-[3px] w-4 shrink-0 rounded-full" style={{ background: color }} />
          <div className="min-w-0 flex-1">
            <Link
              to="/nodes/$hostname"
              params={{ hostname: n.hostname }}
              className="display block truncate text-[22px] leading-none font-semibold group-hover:text-primary after:absolute after:inset-0 sm:text-[26px]"
            >
              {n.hostname}
            </Link>
            <div className="mt-1.5 flex items-center gap-2 text-[11.5px] text-muted-foreground">
              <ArchBadge arch={res?.arch ?? ""} />
              <span className="truncate">{res?.machine ?? ""}</span>
            </div>
          </div>
          <div className="ml-auto flex shrink-0 flex-col items-end gap-1 text-right">
            <OnlineBadge online={n.online} asleep={n.asleep} storageError={n.storageError} />
            {n.online && <AdmissionBadge a={admission} now={now} />}
            <div className="mt-1 font-mono text-[10px] text-faint">
              {n.online ? `${res?.cpus ?? 0} cpu · ${bytes(memBytes)}` : `seen ${ago(n.lastSeen ?? n.joinedAt, now)}`}
            </div>
          </div>
        </div>
        {!n.online && n.storageError && <div className="mt-3 font-mono text-[11px] break-words text-serious-ink">{n.storageError}</div>}

        {n.online ? (
          <>
            <div className="mt-5 grid grid-cols-2 gap-5">
              <div className="flex flex-col justify-between">
                <div>
                  <div className="flex items-baseline justify-between">
                    <span className="eyebrow">{host ? "guest cpu" : "cpu"}</span>
                    <span className="num font-mono text-[13px] text-foreground">{percent(usage?.cpuBusy ?? 0)}</span>
                  </div>
                  {host && (
                    <div className={`mt-0.5 flex items-baseline justify-between font-mono text-[10.5px] ${hostStarved ? "text-serious-ink" : "text-muted-foreground"}`}>
                      <span>VM {percent(host.vmCpuBusy)}</span>
                      <span>host {percent(host.hostCpuBusy)}</span>
                    </div>
                  )}
                </div>
                <div className="mt-2">
                  <Sparkline values={series.map((p) => p.cpuBusy)} max={1} color={color} height={34} />
                </div>
              </div>
              <div className="flex flex-col justify-between">
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
              {n.objectStore ? (
                <NodeCladefsRows os={n.objectStore} />
              ) : (
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
              )}
            </div>
            <div className="mt-4 flex flex-wrap items-center gap-x-3 gap-y-1.5 border-t border-border pt-3 font-mono text-[10.5px] text-muted-foreground">
              <span>
                <span className="text-foreground">{bases}</span> bases
              </span>
              <span>
                <span className="text-foreground">{ws}</span> workspaces
              </span>
              {n.daemonLogs && (n.daemonLogs.errors24h > 0 || n.daemonLogs.warns24h > 0) && (
                <span
                  className={n.daemonLogs.errors24h > 0 ? "text-critical" : "text-warning-ink"}
                  title={`${n.daemonLogs.errors24h} errors, ${n.daemonLogs.warns24h} warnings in 24h`}
                >
                  {n.daemonLogs.errors24h > 0 ? `${n.daemonLogs.errors24h} err` : `${n.daemonLogs.warns24h} warn`}
                </span>
              )}
              <span className="ml-auto flex flex-wrap gap-1">
                {pills.slice(0, 3)}
                {pills.length > 3 && <span className="text-faint">+{pills.length - 3}</span>}
              </span>
            </div>
          </>
        ) : (
          <div className="mt-6 flex flex-wrap items-center justify-between gap-3 rounded-lg border border-dashed border-border-strong p-4 text-[12px] text-muted-foreground">
            <span>Not reporting. Its bases stay on its disk; workspaces go to other nodes until it is back.</span>
            {onDelete && (
              <Button
                type="button"
                variant="outline"
                size="xs"
                onClick={() => onDelete(n)}
                aria-label={`Delete ${n.hostname}`}
                className="relative z-10 gap-1 font-mono text-[10.5px] text-muted-foreground hover:border-critical/60 hover:text-critical"
              >
                <Trash2 className="size-3" />
                Delete
              </Button>
            )}
          </div>
        )}
      </Steady>
    </div>
  );
}

function usesPageCachePrimary(os: ObjectStoreStats): boolean {
  return os.pageCacheLimitBytes > 0n && os.extentCacheBytes === 0n && os.activeVms === 0;
}

function formatCladefsActivity(os: ObjectStoreStats): { parts: string[]; thrashing: boolean } {
  const thrashing = os.daxWindowFull > 0n || os.daxRegionsFull > 0n;
  const parts: string[] = [];
  if (os.activeVms > 0 || os.windowMappedBytes > 0n) {
    parts.push(
      `${os.activeVms} ${os.activeVms === 1 ? "VM" : "VMs"} · ${bytes(os.windowMappedBytes)} DAX${thrashing ? ` · ${compact(os.daxWindowFull)} full` : ""}`,
    );
  }
  if (os.mountCellsAttached > 0) {
    parts.push(`${os.mountCellsAttached} FUSE ${os.mountCellsAttached === 1 ? "cell" : "cells"}`);
  }
  if (os.peerPartsGiven > 0n || os.peerPartsServed > 0n) {
    parts.push(
      `peers ↓${compact(os.peerPartsGiven)} ↑${compact(os.peerPartsServed)}${os.peerPartsFailed > 0n ? ` · ${compact(os.peerPartsFailed)} failed` : ""}`,
    );
  }
  return { parts, thrashing };
}

function NodeCladefsRows({ os }: { os: ObjectStoreStats }) {
  const hit = nodeCacheHit(os);
  const partUsed = Number(os.partCacheBytes);
  const partCap = Number(os.partCacheCapacityBytes);
  const extUsed = Number(os.extentCacheBytes);
  const extCap = Number(os.extentCacheCapacityBytes);
  const pageUsed = Number(os.pageCacheBytes);
  const pageCap = Number(os.pageCacheLimitBytes);
  const showPageCache = usesPageCachePrimary(os);
  const activity = formatCladefsActivity(os);

  return (
    <>
      {os.hostDiskSizeBytes > 0n && (
        <div>
          <div className="mb-1.5 flex flex-wrap items-baseline justify-between gap-x-2 text-[10.5px] text-muted-foreground">
            <span className="eyebrow">
              cladefs{os.hostDiskPressure ? <span className="ml-1.5 text-serious-ink">· pressure</span> : null}
            </span>
            <span className="num font-mono">
              {bytes(os.hostDiskFreeBytes)} free of {bytes(os.hostDiskSizeBytes)}
            </span>
          </div>
          <SegmentedBar
            legend={false}
            format={bytes}
            parts={[
              { id: "cache", label: "part cache", value: Number(os.hostDiskCacheBytes), color: "var(--series-1)" },
              { id: "cells", label: "cells", value: Number(os.hostDiskCellsBytes), color: "var(--series-2)" },
              { id: "other", label: "other", value: Number(os.hostDiskOtherBytes), color: "var(--faint)" },
              { id: "free", label: "free", value: Number(os.hostDiskFreeBytes), color: "var(--muted)" },
            ]}
          />
        </div>
      )}
      <Row
        label="part cache"
        value={`${bytes(partUsed)}${partCap > 0 ? ` / ${bytes(partCap)}` : ""}`}
      >
        <Meter value={partCap > 0 ? partUsed / partCap : 0} warn={0.85} danger={0.95} />
      </Row>
      {showPageCache ? (
        <Row
          label="page cache"
          value={`${bytes(pageUsed)} / ${bytes(pageCap)}${os.pageCacheDirtyBytes > 0n ? ` · ${bytes(os.pageCacheDirtyBytes)} dirty` : ""}`}
        >
          <Meter value={pageCap > 0 ? pageUsed / pageCap : 0} warn={0.85} danger={0.95} />
        </Row>
      ) : (
        <Row
          label="extent cache"
          value={`${bytes(extUsed)}${extCap > 0 ? ` / ${bytes(extCap)}` : ""}${os.memoryFull ? " · full" : os.memoryPressure ? " · pressure" : ""}`}
        >
          <Meter value={extCap > 0 ? extUsed / extCap : 0} warn={0.9} danger={0.98} />
        </Row>
      )}
      <div className="flex flex-wrap items-baseline justify-between gap-x-2 text-[10.5px] text-muted-foreground">
        <span className="eyebrow">{os.hostDiskSizeBytes > 0n ? "R2" : "cladefs R2"}</span>
        <span className="num font-mono">
          ↑ {bytes(os.uploadedBytes)} · ↓ {bytes(os.downloadedBytes)}
          {hit !== null ? ` · ${percent(hit)} local` : ""}
          {os.uploadPendingBytes > 0n ? ` · ${bytes(os.uploadPendingBytes)} pending` : ""}
        </span>
      </div>
      {activity.parts.length > 0 && (
        <div className="flex flex-wrap items-baseline justify-between gap-x-2 text-[10.5px] text-muted-foreground">
          <span className="eyebrow">{os.activeVms > 0 || os.windowMappedBytes > 0n ? "DAX" : "cladefs"}</span>
          <span className={`num font-mono ${activity.thrashing ? "text-serious-ink" : ""}`}>
            {activity.parts.join(" · ")}
          </span>
        </div>
      )}
    </>
  );
}

function Row({ label, value, children }: { label: string; value: string; children: React.ReactNode }) {
  return (
    <div>
      <div className="mb-1 flex flex-wrap items-baseline justify-between gap-x-2 text-[10.5px]">
        <span className="eyebrow">{label}</span>
        <span className="num font-mono text-muted-foreground">{value}</span>
      </div>
      {children}
    </div>
  );
}

export function NodeTable({ s, onDelete }: { s: Snapshot; onDelete?: (n: Node) => void }) {
  const now = Number(s.now);
  const hasCladefs = s.nodes.some((n) => n.objectStore != null);
  const headers = hasCladefs
    ? ["node", "machine", "arch", "status", "admission", "cpus", "cpu busy", "psi cpu", "ram", "used", "guaranteed", "swap", "psi mem", "part cache", "cache mem", "R2 ↑ / ↓", "local hit", "cladefs", "disk free", "kernel", "last seen"]
    : ["node", "machine", "arch", "status", "admission", "cpus", "cpu busy", "psi cpu", "ram", "used", "guaranteed", "swap", "psi mem", "disk free", "kernel", "last seen"];
  return (
    <Table className="text-[12px]">
      <TableHeader className="eyebrow">
        <TableRow className="border-border hover:bg-transparent">
          {headers.map((h, i) => (
            <TableHead
              key={h}
              className={`h-auto px-2 py-2 font-normal text-muted-foreground ${i === 0 ? "sticky left-0 z-10 bg-card" : ""}`}
            >
              {h}
            </TableHead>
          ))}
          {onDelete && (
            <TableHead className="h-auto px-2 py-2 text-right font-normal text-muted-foreground">
              <span className="sr-only">Actions</span>
            </TableHead>
          )}
        </TableRow>
      </TableHeader>
      <TableBody className="num font-mono text-[11px]">
        {s.nodes.map((n) => {
          const admission = nodeAdmission(n);
          const os = n.objectStore;
          const hit = nodeCacheHit(os);
          const activity = os ? formatCladefsActivity(os) : null;
          const freeBytes = os && os.hostDiskSizeBytes > 0n ? os.hostDiskFreeBytes : (n.disk?.freeBytes ?? 0n);
          const diskTitle = os && os.hostDiskSizeBytes > 0n
            ? `cladefs host disk · ${bytes(os.hostDiskCacheBytes)} cache · ${bytes(os.hostDiskCellsBytes)} cells · ${bytes(os.hostDiskOtherBytes)} other · ${bytes(os.hostDiskSizeBytes)} total`
            : `${n.storage} · ${bytes(n.diskBytes)} total`;
          return (
            <TableRow key={n.id} className="border-border/60 hover:bg-muted/60">
              <TableCell className="sticky left-0 z-10 bg-card px-2 py-2">
                <Link to="/nodes/$hostname" params={{ hostname: n.hostname }} className="text-foreground hover:text-primary">
                  {n.hostname}
                </Link>
              </TableCell>
              <TableCell className="px-2 py-2 font-sans text-muted-foreground">{n.resources?.machine ?? ""}</TableCell>
              <TableCell className="px-2 py-2 text-muted-foreground">{n.resources?.arch || "—"}</TableCell>
              <TableCell className="px-2 py-2">
                <OnlineBadge online={n.online} asleep={n.asleep} storageError={n.storageError} />
              </TableCell>
              <TableCell className="px-2 py-2" title={admission?.reason}>
                {admission ? (admission.holding ? <span className="text-serious-ink">holding</span> : "starting") : "—"}
              </TableCell>
              <TableCell className="px-2 py-2">{n.resources?.cpus ?? 0}</TableCell>
              <TableCell className="px-2 py-2">
                <NodeCpuCell n={n} />
              </TableCell>
              <TableCell className="px-2 py-2">{n.online ? `${(n.usage?.cpuPressure ?? 0).toFixed(1)}%` : "—"}</TableCell>
              <TableCell className="px-2 py-2">{bytes(n.resources?.memoryBytes ?? 0n)}</TableCell>
              <TableCell className="px-2 py-2">{n.online ? bytes(n.usage?.memoryUsedBytes ?? 0n) : "—"}</TableCell>
              <TableCell className="px-2 py-2">{bytes(n.reservedMemoryBytes)}</TableCell>
              <TableCell className="px-2 py-2">{bytes(n.usage?.swapUsedBytes ?? 0n)}</TableCell>
              <TableCell className="px-2 py-2">{n.online ? `${(n.usage?.memoryPressure ?? 0).toFixed(1)}%` : "—"}</TableCell>
              {hasCladefs && (
                <>
                  <TableCell
                    className="px-2 py-2"
                    title={os && os.partCacheCapacityBytes > 0n ? `${bytes(os.partCacheBytes)} of ${bytes(os.partCacheCapacityBytes)} SSD part cache` : undefined}
                  >
                    {os ? `${bytes(os.partCacheBytes)}${os.partCacheCapacityBytes > 0n ? ` / ${bytes(os.partCacheCapacityBytes)}` : ""}` : "—"}
                  </TableCell>
                  <TableCell className="px-2 py-2">
                    {!os
                      ? "—"
                      : usesPageCachePrimary(os)
                        ? `${bytes(os.pageCacheBytes)} / ${bytes(os.pageCacheLimitBytes)}`
                        : `${bytes(os.extentCacheBytes)}${os.extentCacheCapacityBytes > 0n ? ` / ${bytes(os.extentCacheCapacityBytes)}` : ""}`}
                  </TableCell>
                  <TableCell className="px-2 py-2">
                    {os ? `↑ ${bytes(os.uploadedBytes)} · ↓ ${bytes(os.downloadedBytes)}` : "—"}
                  </TableCell>
                  <TableCell className="px-2 py-2">{hit !== null ? percent(hit) : "—"}</TableCell>
                  <TableCell className={`px-2 py-2 ${activity?.thrashing ? "text-serious-ink" : "text-muted-foreground"}`}>
                    {!os
                      ? "—"
                      : activity && activity.parts.length > 0
                        ? activity.parts.join(" · ")
                        : os.openCells > 0
                          ? `${os.openCells} open`
                          : "—"}
                  </TableCell>
                </>
              )}
              <TableCell className={`px-2 py-2 ${os?.hostDiskPressure ? "text-serious-ink" : ""}`} title={diskTitle}>
                {bytes(freeBytes)}
              </TableCell>
              <TableCell className="px-2 py-2 text-muted-foreground">{n.resources?.kernel ?? ""}</TableCell>
              <TableCell className="px-2 py-2 text-muted-foreground">{ago(n.lastSeen ?? n.joinedAt, now)}</TableCell>
              {onDelete && (
                <TableCell className="px-2 py-2 text-right">
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
                      onClick={() => onDelete(n)}
                      aria-label={`Delete ${n.hostname}`}
                      className="gap-1 font-mono text-[10.5px] text-muted-foreground hover:border-critical/60 hover:text-critical"
                    >
                      <Trash2 className="size-3" />
                      Delete
                    </Button>
                  </span>
                </TableCell>
              )}
            </TableRow>
          );
        })}
      </TableBody>
    </Table>
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
      <span className={starved ? "text-serious-ink" : "text-muted-foreground"}>
        (VM {percent(host.vmCpuBusy)} · host {percent(host.hostCpuBusy)})
      </span>
    </span>
  );
}
