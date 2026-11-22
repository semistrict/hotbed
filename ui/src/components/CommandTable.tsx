import { Link } from "@tanstack/react-router";
import type { Command, FinishedCommand, Snapshot } from "../api";
import { nodeName } from "../lib/data";
import { ago, bytes, duration, limit, speedup } from "../lib/format";
import { useProject } from "../lib/project";
import { commandProject } from "../lib/projects";
import { workspaceRepo } from "../lib/sessions";
import { Meter } from "./charts/Bars";
import { CommandInline } from "./CommandBlock";
import { ProjectTag } from "./ProjectTag";
import { SessionLink } from "./SessionLink";
import { Empty } from "./ui/Card";
import { BenchTag, KindTag, StateBadge } from "./ui/State";

function identityColumns(project: string | null): string[] {
  return ["state", "command", "session", ...(project ? [] : ["project"]), "repo", "workspace", "node"];
}

function WorkspaceCell({ id, known }: { id: string; known: boolean }) {
  if (!id) return <span className="text-faint">—</span>;
  if (!known) return <span className="font-mono text-[11px] text-faint">{id}</span>;
  return (
    <Link to="/workspaces/$id" params={{ id }} className="font-mono text-[11px] text-muted-foreground hover:text-primary">
      {id}
    </Link>
  );
}

/** Live commands: what each is doing now, against its limits. */
export function LiveCommandTable({ s, commands }: { s: Snapshot; commands: Command[] }) {
  const now = Date.parse(s.now);
  const [project] = useProject();
  if (commands.length === 0) return <Empty>No commands here right now.</Empty>;
  const repoOf = workspaceRepo(s);
  const knownWorkspaces = new Set(s.workspaces.map((w) => w.id));
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-[12px]">
        <thead className="eyebrow">
          <tr className="border-b border-border">
            {[...identityColumns(project), "for", "cpu", "memory · high", "crates", "why"].map((h) => (
              <th key={h} className="px-2 py-2 font-normal whitespace-nowrap">
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {commands.map((c) => {
            const since = Date.parse(c.startedAt ?? c.queuedAt);
            const repoId = repoOf(c.workspaceId);
            return (
              <tr key={c.id} className="border-b border-border/60 align-top hover:bg-muted/60">
                <td className="px-2 py-2.5">
                  <StateBadge state={c.state} />
                </td>
                <td className="max-w-[320px] px-2 py-2.5">
                  <Link to="/commands/$id" params={{ id: c.id }} className="block font-mono text-[11.5px] text-foreground hover:text-primary">
                    <CommandInline argv={c.argv} />
                  </Link>
                  <div className="mt-0.5 flex gap-2">
                    <KindTag kind={c.kind} />
                    {c.bench && <BenchTag />}
                    {c.url && (
                      <a href={c.url} className="font-mono text-[10px] text-primary hover:underline" target="_blank" rel="noreferrer">
                        {c.url.replace("http://", "")} ↗
                      </a>
                    )}
                  </div>
                </td>
                <td className="px-2 py-2.5 whitespace-nowrap">
                  <SessionLink id={c.sessionId} />
                </td>
                {!project && (
                  <td className="px-2 py-2.5 whitespace-nowrap">
                    <ProjectTag name={commandProject(s, c.sessionId, repoId || null)} />
                  </td>
                )}
                <td className="px-2 py-2.5 font-mono text-[11px] whitespace-nowrap text-muted-foreground">{repoId || "—"}</td>
                <td className="px-2 py-2.5 whitespace-nowrap">
                  <WorkspaceCell id={c.workspaceId} known={knownWorkspaces.has(c.workspaceId)} />
                </td>
                <td className="px-2 py-2.5 font-mono text-[11px] whitespace-nowrap text-muted-foreground">{nodeName(s, c.nodeId)}</td>
                <td className="num px-2 py-2.5 font-mono text-[11px] whitespace-nowrap text-muted-foreground">{duration((now - since) / 1000)}</td>
                <td className="num px-2 py-2.5 font-mono text-[11px] whitespace-nowrap">{c.state === "running" ? `${c.cpuCores.toFixed(1)} cores` : "—"}</td>
                <td className="w-40 px-2 py-2.5">
                  {c.state === "queued" ? (
                    <span className="font-mono text-[11px] text-faint">low {limit(c.limits.memoryLowBytes)}</span>
                  ) : (
                    <>
                      <div className="num mb-1 font-mono text-[10.5px] text-muted-foreground">
                        {bytes(c.memoryBytes)} · {limit(c.limits.memoryHighBytes)}
                        {c.swapBytes > 0 && <span className="text-frost"> +{bytes(c.swapBytes)} swap</span>}
                      </div>
                      {c.limits.memoryHighBytes > 0 && <Meter value={c.memoryBytes / c.limits.memoryHighBytes} marker={c.limits.memoryLowBytes / c.limits.memoryHighBytes} />}
                    </>
                  )}
                </td>
                <td className="num px-2 py-2.5 font-mono text-[11px] whitespace-nowrap text-muted-foreground">{c.rebuilt ? `${c.rebuilt.units}/${c.rebuilt.of}` : "—"}</td>
                <td className="max-w-[260px] px-2 py-2.5 text-[11px] text-muted-foreground">{c.reason ?? (c.state === "running" ? `started ${ago(since, now)}` : "")}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

/** Commands that ended: outcome, time against cold, and why it failed. */
export function HistoryTable({ s, rows, limit = 200 }: { s: Snapshot; rows: FinishedCommand[]; limit?: number }) {
  const now = Date.parse(s.now);
  const [project] = useProject();
  if (rows.length === 0) return <Empty>Nothing matches.</Empty>;
  const knownWorkspaces = new Set(s.workspaces.map((w) => w.id));
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-[12px]">
        <thead className="eyebrow">
          <tr className="border-b border-border">
            {[...identityColumns(project), "ended", "took", "cold", "speedup", "crates", "peak mem", "why"].map((h) => (
              <th key={h} className="px-2 py-2 font-normal whitespace-nowrap">
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="num font-mono text-[11px] [&_td]:whitespace-nowrap">
          {rows.slice(0, limit).map((c) => (
            <tr key={c.id} className="border-b border-border/60 hover:bg-muted/60">
              <td className="px-2 py-2">
                <StateBadge state={c.state} />
              </td>
              <td className="max-w-[300px] px-2 py-2">
                <Link to="/commands/$id" params={{ id: c.id }} className="block text-foreground hover:text-primary">
                  <CommandInline argv={c.argv} />
                </Link>
                {c.bench && <BenchTag />}
              </td>
              <td className="px-2 py-2">
                <SessionLink id={c.sessionId} />
              </td>
              {!project && (
                <td className="px-2 py-2">
                  <ProjectTag name={commandProject(s, c.sessionId, c.repoId)} />
                </td>
              )}
              <td className="px-2 py-2 text-muted-foreground">{c.repoId || "—"}</td>
              <td className="px-2 py-2">
                <WorkspaceCell id={c.workspaceId} known={knownWorkspaces.has(c.workspaceId)} />
              </td>
              <td className="px-2 py-2 text-muted-foreground">{nodeName(s, c.nodeId)}</td>
              <td className="px-2 py-2 whitespace-nowrap text-muted-foreground">{ago(c.endedAt, now)}</td>
              <td className="px-2 py-2">{duration(c.seconds)}</td>
              <td className="px-2 py-2 text-faint">{c.coldSeconds !== null ? duration(c.coldSeconds) : "—"}</td>
              <td className="px-2 py-2 text-primary">{c.coldSeconds !== null ? speedup(c.coldSeconds, c.seconds) : "—"}</td>
              <td className="px-2 py-2 text-muted-foreground">{c.rebuilt !== null ? `${c.rebuilt}/${c.of}` : "—"}</td>
              <td className="px-2 py-2 text-muted-foreground">
                {bytes(c.peakMemoryBytes)}
                {c.parks > 0 && <span className="text-frost"> ❄{c.parks}</span>}
              </td>
              <td className="px-2 py-2 font-sans text-muted-foreground">{c.failure ? FAILURE[c.failure] : ""}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {rows.length > limit && <p className="mt-3 text-center font-mono text-[10.5px] text-faint">showing {limit} of {rows.length}</p>}
    </div>
  );
}

export const FAILURE: Record<NonNullable<FinishedCommand["failure"]>, string> = {
  compile: "compile error",
  test: "tests failed",
  lint: "lint denied",
  signal: "killed by signal",
  exit: "non-zero exit",
};
