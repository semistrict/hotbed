import { useQuery } from "@tanstack/react-query";
import { Link, useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { api, WorkspaceState, workspaceStateLabel, type FileEntry, type Snapshot, type Workspace } from "../api";
import { SegmentedBar } from "../components/charts/Bars";
import { HistoryTable, LiveCommandTable } from "../components/CommandTable";
import { Crumbs } from "../components/Crumbs";
import { Card, Empty, StatTile } from "../components/ui/Card";
import { nodeName, useSnapshot } from "../lib/data";
import { ago, bytes, duration, shortSha } from "../lib/format";
import { isLive } from "../lib/live";
import { savings } from "../lib/stats";
import { filesChanged } from "../lib/workspaces";
import { NotFound } from "./NotFound";
import { PageTitle } from "./Nodes";

/** Each of a workspace's checkouts: where it stands against its base, and what was synced into it. */
function RepoTable({ s, w }: { s: Snapshot; w: Workspace }) {
  const base = s.bases.find((b) => b.id === w.baseId);
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-[12px]">
        <thead className="eyebrow">
          <tr className="border-b border-border">
            {["repo", "base", "head", "files changed", "synced from"].map((h) => (
              <th key={h} className="px-2 py-2 font-normal whitespace-nowrap">
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="num font-mono text-[11px] [&_td]:whitespace-nowrap">
          {w.repos.map((r) => {
            const at = base?.repos.find((b) => b.repo === r.repo);
            return (
              <tr key={r.repo} className="border-b border-border/60 last:border-b-0">
                <td className="px-2 py-2 text-foreground">{r.repo}</td>
                <td className="px-2 py-2 text-muted-foreground" title={at?.message}>
                  {at ? shortSha(at.commit) : "—"}
                </td>
                <td className="px-2 py-2 text-muted-foreground">{shortSha(r.head)}</td>
                <td className="px-2 py-2">{r.filesChanged}</td>
                <td className="px-2 py-2 text-faint">{r.syncDir || "nothing synced"}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

export const stateTone: Record<WorkspaceState, string> = {
  [WorkspaceState.UNSPECIFIED]: "text-faint",
  [WorkspaceState.PREPARING]: "text-warning",
  [WorkspaceState.ACTIVE]: "text-sprout",
  [WorkspaceState.IDLE]: "text-sprout",
  [WorkspaceState.RELEASED]: "text-faint",
  [WorkspaceState.FAILED]: "text-critical",
};

/** A workspace's state as a word, colored by what it means for its agent. */
export function WorkspaceStateLabel({ w, now }: { w: Workspace; now: number | bigint }) {
  return (
    <span className={`font-mono text-[11px] ${stateTone[w.state]}`}>
      {w.state === WorkspaceState.RELEASED ? `released ${ago(w.lastActiveAt, now)}` : workspaceStateLabel(w.state)}
    </span>
  );
}

export function WorkspacePage() {
  const { id } = useParams({ from: "/workspaces/$id" });
  const { file } = useSearch({ from: "/workspaces/$id" });
  const { data: s } = useSnapshot();
  if (!s) return null;
  const w = s.workspaces.find((x) => x.id === id);
  if (!w) return <NotFound />;
  const now = Number(s.now);
  const base = s.bases.find((b) => b.id === w.baseId);
  const live = s.commands.filter((c) => c.workspaceId === w.id && isLive(c));
  const history = s.history.filter((c) => c.workspaceId === w.id).reverse();
  const day = savings(history);

  return (
    <div className="space-y-6">
      <Crumbs project={w.project} here={w.id} />
      <PageTitle
        title={w.id}
        sub={`${w.project} · ${nodeName(s, w.nodeId)} · ${w.agent}`}
        right={<WorkspaceStateLabel w={w} now={now} />}
      />
      {w.sessionId && (
        <p className="rise -mt-3 text-[13px] text-muted-foreground">
          session{" "}
          <Link to="/sessions/$id" params={{ id: w.sessionId }} className="font-mono text-foreground hover:text-primary">
            {w.sessionId}
          </Link>
        </p>
      )}
      {w.reason && (w.state === WorkspaceState.PREPARING || w.state === WorkspaceState.FAILED) && (
        <p className={`font-mono text-[11px] ${w.state === WorkspaceState.FAILED ? "text-critical" : "text-warning"}`}>
          {w.state === WorkspaceState.FAILED ? "✕ failed: " : "◌ "}
          {w.reason}
        </p>
      )}
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-6">
        <StatTile label="Forked in" value={Number(w.forkMillis)} unit="ms" tone="ember" foot={ago(w.createdAt, now)} />
        <StatTile
          label="Base"
          value={`g${base?.generation ?? "?"}`}
          foot={base ? <span className="line-clamp-3 break-words">{base.repos.map((r) => `${r.repo} ${shortSha(r.commit)}`).join(" · ")}</span> : "gone"}
        />
        <StatTile label="Owns" value={bytes(w.exclusiveBytes)} foot="rewritten by its builds" />
        <StatTile label="Shares" value={bytes(w.sharedBytes)} foot="with its base" />
        <StatTile label="Files changed" value={filesChanged(w)} foot={`in ${w.repos.filter((r) => r.filesChanged > 0).length} of ${w.repos.length} repos`} />
        <StatTile label="Saved" value={duration(day.saved)} foot={`${day.builds} builds here`} />
      </div>

      <Card title="Blocks" sub="What this workspace reads from its base, against what it owns">
        <SegmentedBar
          tall
          format={bytes}
          parts={[
            { id: "shared", label: "shared with base", value: Number(w.sharedBytes), color: "var(--series-1)" },
            { id: "owned", label: "owned", value: Number(w.exclusiveBytes), color: "var(--series-2)" },
          ]}
        />
      </Card>

      <Card title="Repos" sub="Each checked out at /work/<repo>; a run syncs the checkout it is run in into its repo's">
        <RepoTable s={s} w={w} />
      </Card>

      <Card title="Files" sub={base ? `Against base g${base.generation}, below /work. Changed lines are marked.` : undefined}>
        <FileBrowser workspaceId={w.id} selected={file} />
      </Card>

      <Card title="Commands" sub="Running now, then those that ended in the last 24 hours">
        {live.length === 0 && history.length === 0 ? (
          <Empty>No commands have run here.</Empty>
        ) : (
          <div className="space-y-5">
            {live.length > 0 && <LiveCommandTable s={s} commands={live} />}
            {history.length > 0 && <HistoryTable s={s} rows={history} />}
          </div>
        )}
      </Card>
      {base && (
        <Card title="Built into the base" sub={[...base.repos.map((r) => r.toolchain).filter(Boolean), `built in ${duration(base.buildSeconds)}`, bytes(base.sizeBytes)].join(" · ")}>
          <ul className="space-y-1 font-mono text-[11.5px] text-muted-foreground">
            {base.baked.map((b) => (
              <li key={b}>
                <span className="text-primary">$</span> {b}
              </li>
            ))}
          </ul>
        </Card>
      )}
    </div>
  );
}

function FileBrowser({ workspaceId, selected }: { workspaceId: string; selected?: string }) {
  const navigate = useNavigate();
  const files = useQuery({ queryKey: ["files", workspaceId], queryFn: () => api.files(workspaceId) });
  const firstChanged = files.data?.find((f) => f.kind === "file" && f.change)?.path;
  const path = selected ?? firstChanged ?? files.data?.find((f) => f.kind === "file")?.path;
  const content = useQuery({ queryKey: ["file", workspaceId, path], queryFn: () => api.file(workspaceId, path ?? ""), enabled: !!path });

  if (files.isError) return <Empty>Cannot list files: {String(files.error)}</Empty>;
  if (!files.data) return <div className="font-mono text-[11px] text-faint">listing…</div>;
  const changed = new Set(content.data?.changedLines ?? []);

  return (
    <div className="grid gap-4 lg:grid-cols-[280px_1fr]">
      <ul className="max-h-[480px] overflow-y-auto rounded-lg border border-border bg-background/40 p-2 font-mono text-[11px]">
        {files.data.map((f) => (
          <TreeRow key={f.path} f={f} active={f.path === path} onPick={() => navigate({ to: "/workspaces/$id", params: { id: workspaceId }, search: { file: f.path }, replace: true, resetScroll: false })} />
        ))}
      </ul>
      <div className="min-w-0 overflow-hidden rounded-lg border border-border bg-background/40">
        <div className="flex items-center gap-2 border-b border-border px-3 py-2 font-mono text-[11px] text-muted-foreground">
          <span className="text-foreground">{path}</span>
          {changed.size > 0 && <span className="text-primary">{changed.size} changed lines</span>}
        </div>
        <pre className="max-h-[440px] overflow-auto py-2 font-mono text-[11.5px] leading-[1.65]">
          {content.data?.text.split("\n").map((line, i) => (
            <div key={i} className={`flex ${changed.has(i + 1) ? "bg-ember-wash" : ""}`}>
              <span className={`w-11 shrink-0 pr-3 text-right select-none ${changed.has(i + 1) ? "text-primary" : "text-faint"}`}>{i + 1}</span>
              <span className={`border-l-2 pl-3 ${changed.has(i + 1) ? "border-primary text-foreground" : "border-transparent text-muted-foreground"}`}>{line || " "}</span>
            </div>
          ))}
        </pre>
      </div>
    </div>
  );
}

function TreeRow({ f, active, onPick }: { f: FileEntry; active: boolean; onPick: () => void }) {
  const depth = f.path.split("/").length - 1;
  const name = f.path.split("/").pop();
  if (f.kind === "dir") {
    return (
      <li className="truncate py-0.5 text-faint" style={{ paddingLeft: depth * 12 + 4 }}>
        {name}/
      </li>
    );
  }
  return (
    <li>
      <button
        type="button"
        onClick={onPick}
        className={`flex w-full items-center gap-2 rounded px-1 py-0.5 text-left hover:bg-muted ${active ? "bg-muted text-foreground" : "text-muted-foreground"}`}
        style={{ paddingLeft: depth * 12 + 4 }}
      >
        <span className="truncate">{name}</span>
        {f.change && <span className={`ml-auto text-[9.5px] ${f.change === "added" ? "text-good-ink" : "text-primary"}`}>{f.change === "added" ? "A" : "M"}</span>}
      </button>
    </li>
  );
}

