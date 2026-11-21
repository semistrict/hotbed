import { useQuery } from "@tanstack/react-query";
import { useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { api, type FileEntry, type Workspace } from "../api";
import { SegmentedBar } from "../components/charts/Bars";
import { HistoryTable, LiveCommandTable } from "../components/CommandTable";
import { Card, Empty, StatTile } from "../components/ui/Card";
import { nodeName, repoName, useSnapshot } from "../lib/data";
import { ago, bytes, duration } from "../lib/format";
import { isLive } from "../lib/live";
import { savings } from "../lib/stats";
import { NotFound } from "./NotFound";
import { PageTitle } from "./Nodes";

export const stateTone: Record<Workspace["state"], string> = {
  preparing: "text-warning",
  active: "text-sprout",
  idle: "text-sprout",
  released: "text-faint",
  failed: "text-critical",
};

/** A workspace's state as a word, colored by what it means for its agent. */
export function WorkspaceStateLabel({ w, now }: { w: Workspace; now: number }) {
  return (
    <span className={`font-mono text-[11px] ${stateTone[w.state]}`}>
      {w.state === "released" ? `released ${ago(w.lastActiveAt, now)}` : w.state}
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
  const now = Date.parse(s.now);
  const base = s.bases.find((b) => b.id === w.baseId);
  const live = s.commands.filter((c) => c.workspaceId === w.id && isLive(c));
  const history = s.history.filter((c) => c.workspaceId === w.id).reverse();
  const day = savings(history);

  return (
    <div className="space-y-6">
      <PageTitle
        title={w.id}
        sub={`${w.agent} on ${w.branch} · ${repoName(s, w.repoId)} · ${nodeName(s, w.nodeId)}`}
        right={<WorkspaceStateLabel w={w} now={now} />}
      />
      {w.reason && (w.state === "preparing" || w.state === "failed") && (
        <p className={`font-mono text-[11px] ${w.state === "failed" ? "text-critical" : "text-warning"}`}>
          {w.state === "failed" ? "✕ failed: " : "◌ "}
          {w.reason}
        </p>
      )}
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-6">
        <StatTile label="Forked in" value={w.forkMillis} unit="ms" tone="ember" foot={ago(w.createdAt, now)} />
        <StatTile label="Base" value={`g${base?.generation ?? "?"}`} foot={base ? `${base.commit} · ${base.message}` : "gone"} />
        <StatTile label="Owns" value={bytes(w.exclusiveBytes)} foot="rewritten by its builds" />
        <StatTile label="Shares" value={bytes(w.sharedBytes)} foot="with its base" />
        <StatTile label="Files changed" value={w.filesChanged} foot={`head ${w.head}`} />
        <StatTile label="Saved" value={duration(day.saved)} foot={`${day.builds} builds here`} />
      </div>

      <Card title="Blocks" sub="What this workspace reads from its base, against what it owns">
        <SegmentedBar
          tall
          format={bytes}
          parts={[
            { id: "shared", label: "shared with base", value: w.sharedBytes, color: "var(--series-1)" },
            { id: "owned", label: "owned", value: w.exclusiveBytes, color: "var(--series-2)" },
          ]}
        />
      </Card>

      <Card title="Files" sub={base ? `Against base g${base.generation} (${base.commit}). Changed lines are marked.` : undefined}>
        <FileBrowser workspaceId={w.id} selected={file} />
      </Card>

      <Card title="Commands now">
        <LiveCommandTable s={s} commands={live} />
      </Card>
      <Card title="Commands that ended" sub="Last 24 hours">
        <HistoryTable s={s} rows={history} />
      </Card>
      {base && (
        <Card title="Built into the base" sub={`${base.toolchain} · built in ${duration(base.buildSeconds)} · ${bytes(base.sizeBytes)}`}>
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
          <TreeRow key={f.path} f={f} active={f.path === path} onPick={() => navigate({ to: "/workspaces/$id", params: { id: workspaceId }, search: { file: f.path }, replace: true })} />
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

