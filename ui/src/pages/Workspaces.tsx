import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import type { Snapshot, Workspace } from "../api";
import { Card, Empty, StatTile } from "../components/ui/Card";
import { nodeName } from "../lib/data";
import { ago, bytes, compact, duration, quantile } from "../lib/format";
import { isLive } from "../lib/live";
import { filterWorkspaces } from "../lib/workspaces";
import { useScopedSnapshot } from "../lib/project";
import { PageTitle } from "./Nodes";
import { stateTone } from "./Workspace";

export function WorkspacesPage() {
  const { repo, q } = useSearch({ from: "/workspaces" });
  const navigate = useNavigate({ from: "/workspaces" });
  const { data: s } = useScopedSnapshot();
  if (!s) return null;
  const now = Date.parse(s.now);
  const live = s.workspaces.filter((w) => w.state !== "released");
  const activeIds = new Set(s.commands.filter((c) => c.state === "running" || c.state === "parked").map((c) => c.workspaceId));
  const owned = live.reduce((n, w) => n + w.exclusiveBytes, 0);
  const shared = live.reduce((n, w) => n + w.sharedBytes, 0);
  const forkMs = live.map((w) => w.forkMillis);
  const rows = filterWorkspaces(s.workspaces, { repo, q }, (id) => nodeName(s, id));
  const r = s.repos.find((x) => x.id === repo);

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

      <div className="rise flex flex-wrap items-center gap-2">
        <RepoChip label="every repo" active={!repo} search={{ q }} count={live.length} />
        {s.repos.map((x) => (
          <RepoChip key={x.id} label={x.id} active={repo === x.id} search={{ repo: x.id, q }} count={live.filter((w) => w.repoId === x.id).length} />
        ))}
        <input
          type="search"
          value={q ?? ""}
          onChange={(e) => navigate({ search: (p) => ({ ...p, q: e.target.value || undefined }), replace: true, resetScroll: false })}
          placeholder="find a workspace, session, agent or node"
          aria-label="Find a workspace"
          className="ml-auto w-full rounded-full border border-border bg-background/40 px-3 py-1 font-mono text-[11px] text-foreground placeholder:text-faint focus:border-primary focus:outline-none sm:w-80"
        />
      </div>

      <Card
        title={r ? r.name : "All workspaces"}
        sub={r ? `${r.language} · ${r.units} ${r.language === "rust" ? "crates" : "packages"} · cold build ${duration(r.coldBuildSeconds)}` : "Live first, then released, by latest activity"}
        right={<span className="font-mono text-[10.5px] text-muted-foreground">{rows.length} shown</span>}
      >
        <WorkspaceTable s={s} rows={rows} now={now} />
      </Card>
    </div>
  );
}

function RepoChip({ label, active, search, count }: { label: string; active: boolean; search: { repo?: string; q?: string }; count: number }) {
  return (
    <Link
      to="/workspaces"
      search={search}
      resetScroll={false}
      className={`rounded-full border px-3 py-1 font-mono text-[11px] ${active ? "border-primary bg-ember-wash text-foreground" : "border-border text-muted-foreground hover:border-border-strong"}`}
    >
      {label} <span className="text-faint">{count}</span>
    </Link>
  );
}

function WorkspaceTable({ s, rows, now }: { s: Snapshot; rows: Workspace[]; now: number }) {
  if (rows.length === 0) return <Empty>No workspace matches.</Empty>;
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-[12px]">
        <thead className="eyebrow">
          <tr className="border-b border-border">
            {["workspace", "session", "agent", "repo", "node", "base", "files", "owns", "fork", "created", "active"].map((h) => (
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
                <td className="px-2 py-2">
                  {w.sessionId ? (
                    <Link to="/sessions/$id" params={{ id: w.sessionId }} className="text-muted-foreground hover:text-primary">
                      {w.sessionId}
                    </Link>
                  ) : (
                    <span className="text-faint">—</span>
                  )}
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
