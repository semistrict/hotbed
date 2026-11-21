import { Link } from "@tanstack/react-router";
import type { Snapshot } from "../api";
import { Columns } from "../components/charts/Columns";
import { ForkTree } from "../components/charts/ForkTree";
import { Card, StatTile } from "../components/ui/Card";
import { nodeName, useSnapshot } from "../lib/data";
import { ago, bytes, compact, duration, quantile } from "../lib/format";
import { isLive } from "../lib/live";
import { PageTitle } from "./Nodes";
import { stateTone } from "./Workspace";

export function WorkspacesPage() {
  const { data: s } = useSnapshot();
  if (!s) return null;
  const now = Date.parse(s.now);
  const live = s.workspaces.filter((w) => w.state !== "released");
  const activeIds = new Set(s.commands.filter((c) => c.state === "running" || c.state === "parked").map((c) => c.workspaceId));
  const owned = live.reduce((n, w) => n + w.exclusiveBytes, 0);
  const shared = live.reduce((n, w) => n + w.sharedBytes, 0);
  const forkMs = live.map((w) => w.forkMillis);

  return (
    <div className="space-y-6">
      <PageTitle title="Workspaces" sub="Every workspace is a btrfs snapshot of a warm base: main, checked out and already built. Agents compile only what they changed." />
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-6">
        <StatTile label="Live" value={live.length} unit="workspaces" foot={`${activeIds.size} running a command`} />
        <StatTile label="Forks today" value={compact(s.totals24h.forks)} />
        <StatTile label="Fork p50" value={quantile(forkMs, 0.5)} unit="ms" tone="ember" foot={`p95 ${quantile(forkMs, 0.95)} ms`} />
        <StatTile label="Owned" value={bytes(owned)} foot="blocks only forks hold" />
        <StatTile label="Shared" value={bytes(shared)} foot="read from their bases" />
        <StatTile label="Warm bases" value={s.bases.filter((b) => b.state === "warm").length} foot={`${s.bases.filter((b) => b.state === "building").length} building`} />
      </div>

      {s.repos.map((r, i) => {
        const hosts = [...new Set(s.bases.filter((b) => b.repoId === r.id).map((b) => b.nodeId))];
        const forks = live.filter((w) => w.repoId === r.id);
        const lanes = hosts.map((id) => {
          const n = s.nodes.find((x) => x.id === id);
          return {
            nodeId: id,
            hostname: n?.hostname ?? id,
            online: n?.online ?? false,
            bases: s.bases.filter((b) => b.repoId === r.id && b.nodeId === id).sort((a, b) => a.generation - b.generation),
            forks: forks.filter((w) => w.nodeId === id),
          };
        });
        return (
          <Card
            key={r.id}
            delay={i * 60}
            title={
              <span className="flex items-baseline gap-3">
                <span className="display text-[24px] font-semibold">{r.id}</span>
                <span className="font-mono text-[11px] font-normal text-faint">{r.name}</span>
              </span>
            }
            sub={`${r.language} · ${r.units} ${r.language === "rust" ? "crates" : "packages"} · cold build ${duration(r.coldBuildSeconds)}`}
            right={
              <div className="flex gap-4 text-right font-mono text-[10.5px] text-muted-foreground">
                <div>
                  <div className="text-[15px] text-foreground">{forks.length}</div>forks
                </div>
                <div>
                  <div className="text-[15px] text-foreground">{bytes(forks.reduce((n, w) => n + w.exclusiveBytes, 0))}</div>owned
                </div>
              </div>
            }
          >
            <ForkTree lanes={lanes} now={now} activeIds={activeIds} />
            <div className="mt-2 flex flex-wrap gap-4 font-mono text-[10px] text-faint">
              <span className="flex items-center gap-1.5">
                <span className="inline-block size-2.5 rounded-[3px] bg-primary" /> warm base
              </span>
              <span className="flex items-center gap-1.5">
                <span className="inline-block size-2.5 rounded-[3px] border-2 border-faint" /> older base, kept for its forks
              </span>
              <span className="flex items-center gap-1.5">
                <span className="inline-block size-2.5 rounded-full border-2 border-primary" /> building
              </span>
              <span className="flex items-center gap-1.5">
                <span className="inline-block size-2.5 rounded-full bg-primary" /> fork running a command
              </span>
              <span className="flex items-center gap-1.5">
                <span className="inline-block size-2.5 rounded-full border-2 border-sprout" /> idle fork
              </span>
            </div>
          </Card>
        );
      })}

      <div className="grid gap-4 lg:grid-cols-3">
        <Card className="lg:col-span-2" title="All workspaces">
          <WorkspaceTable s={s} now={now} />
        </Card>
        <Card title="Owned storage by repo" sub="What forks' builds rewrote">
          <Columns
            columns={s.repos.map((r) => ({ key: r.id, label: r.id, values: [live.filter((w) => w.repoId === r.id).reduce((n, w) => n + w.exclusiveBytes, 0) / 1024 ** 3] }))}
            series={[{ id: "owned", label: "owned", color: "var(--series-2)" }]}
            format={(v) => `${v.toFixed(0)} GiB`}
          />
        </Card>
      </div>
    </div>
  );
}

function WorkspaceTable({ s, now }: { s: Snapshot; now: number }) {
  const rows = [...s.workspaces].sort((a, b) => Number(a.state === "released") - Number(b.state === "released") || b.lastActiveAt.localeCompare(a.lastActiveAt));
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-[12px]">
        <thead className="eyebrow">
          <tr className="border-b border-border">
            {["workspace", "agent", "repo", "node", "base", "files", "owns", "fork", "created", "active"].map((h) => (
              <th key={h} className="px-2 py-2 font-normal">
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="num font-mono text-[11px] [&_td]:whitespace-nowrap">
          {rows.map((w) => {
            const base = s.bases.find((b) => b.id === w.baseId);
            const live = s.commands.filter((c) => c.workspaceId === w.id && isLive(c)).length;
            return (
              <tr key={w.id} className={`border-b border-border/60 hover:bg-muted/60 ${w.state === "released" ? "opacity-45" : ""}`}>
                <td className="px-2 py-2">
                  <Link to="/workspaces/$id" params={{ id: w.id }} className="text-foreground hover:text-primary">
                    {w.id}
                  </Link>
                  {w.state !== "active" && w.state !== "idle" && <span className={`ml-2 text-[9.5px] ${stateTone[w.state]}`}>{w.state}</span>}
                </td>
                <td className="px-2 py-2 text-muted-foreground">{w.agent}</td>
                <td className="px-2 py-2 text-muted-foreground">{w.repoId}</td>
                <td className="px-2 py-2 text-muted-foreground">{nodeName(s, w.nodeId)}</td>
                <td className="px-2 py-2 text-faint">g{base?.generation ?? "?"}</td>
                <td className="px-2 py-2">{w.filesChanged}</td>
                <td className="px-2 py-2">{bytes(w.exclusiveBytes)}</td>
                <td className="px-2 py-2 text-faint">{w.forkMillis} ms</td>
                <td className="px-2 py-2 text-muted-foreground">{ago(w.createdAt, now)}</td>
                <td className="px-2 py-2">{live > 0 ? <span className="text-primary">{live} live</span> : <span className="text-faint">{ago(w.lastActiveAt, now)}</span>}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
