import { Link } from "@tanstack/react-router";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { CommandState, type Command, type FinishedCommand, type Snapshot } from "../api";
import { nodeName } from "../lib/data";
import { ago, bytes, duration, limit, speedup } from "../lib/format";
import { useProject } from "../lib/project";
import { commandProject } from "../lib/projects";
import { Meter } from "./charts/Bars";
import { CommandInline } from "./CommandBlock";
import { Empty } from "./Panel";
import { ProjectTag } from "./ProjectTag";
import { SessionLink } from "./SessionLink";
import { BenchTag, KindTag, StateBadge } from "./StateBadges";

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
  const now = Number(s.now);
  const [project] = useProject();
  if (commands.length === 0) return <Empty>No commands here right now.</Empty>;
  const knownWorkspaces = new Set(s.workspaces.map((w) => w.id));
  return (
    <Table className="text-[12px]">
      <TableHeader className="eyebrow">
        <TableRow className="border-border hover:bg-transparent">
          {[...identityColumns(project), "for", "cpu", "memory · high", "crates", "why"].map((h) => (
            <TableHead key={h} className="h-auto px-2 py-2 font-normal text-muted-foreground">
              {h}
            </TableHead>
          ))}
        </TableRow>
      </TableHeader>
      <TableBody>
        {commands.map((c) => {
          const since = Number(c.startedAt ?? c.queuedAt);
          const repoId = c.repo;
          const memLow = c.limits?.memoryLowBytes ?? 0n;
          const memHigh = c.limits?.memoryHighBytes ?? 0n;
          return (
            <TableRow key={c.id} className="border-border/60 align-top hover:bg-muted/60">
              <TableCell className="px-2 py-2.5">
                <StateBadge state={c.state} />
              </TableCell>
              <TableCell className="max-w-[320px] px-2 py-2.5 whitespace-normal">
                <Link to="/commands/$id" params={{ id: c.id }} className="block font-mono text-[11.5px] text-foreground hover:text-primary">
                  <CommandInline argv={c.argv} args={c.args} />
                </Link>
                <div className="mt-0.5 flex flex-wrap items-center gap-2">
                  <KindTag kind={c.kind} />
                  {c.bench && <BenchTag />}
                  {c.state !== CommandState.QUEUED && (
                    <Link
                      to="/commands/$id"
                      params={{ id: c.id }}
                      search={{ sample: true }}
                      title="Sample stacks, syscalls, open files & Linux diagnostics"
                      className="rounded border border-primary/35 bg-ember-wash px-1.5 py-px font-mono text-[9.5px] text-primary hover:brightness-110"
                    >
                      ◎ Sample
                    </Link>
                  )}
                  {c.url && (
                    <a href={c.url} className="font-mono text-[10px] text-primary hover:underline" target="_blank" rel="noreferrer">
                      {c.url.replace("http://", "")} ↗
                    </a>
                  )}
                </div>
              </TableCell>
              <TableCell className="px-2 py-2.5">
                <SessionLink id={c.sessionId} />
              </TableCell>
              {!project && (
                <TableCell className="px-2 py-2.5">
                  <ProjectTag name={commandProject(s, c.sessionId, repoId || null)} />
                </TableCell>
              )}
              <TableCell className="px-2 py-2.5 font-mono text-[11px] text-muted-foreground">{repoId || "—"}</TableCell>
              <TableCell className="px-2 py-2.5">
                <WorkspaceCell id={c.workspaceId} known={knownWorkspaces.has(c.workspaceId)} />
              </TableCell>
              <TableCell className="px-2 py-2.5 font-mono text-[11px] text-muted-foreground">{nodeName(s, c.nodeId)}</TableCell>
              <TableCell className="num px-2 py-2.5 font-mono text-[11px] text-muted-foreground">{duration((now - since) / 1000)}</TableCell>
              <TableCell className="num px-2 py-2.5 font-mono text-[11px]">{c.state === CommandState.RUNNING ? `${c.cpuCores.toFixed(1)} cores` : "—"}</TableCell>
              <TableCell className="w-40 px-2 py-2.5">
                {c.state === CommandState.QUEUED ? (
                  <span className="font-mono text-[11px] text-faint">low {limit(memLow)}</span>
                ) : (
                  <>
                    <div className="num mb-1 font-mono text-[10.5px] text-muted-foreground">
                      {bytes(c.memoryBytes)} · {limit(memHigh)}
                      {c.swapBytes > 0n && <span className="text-frost"> +{bytes(c.swapBytes)} swap</span>}
                    </div>
                    {memHigh > 0n && <Meter value={Number(c.memoryBytes) / Number(memHigh)} marker={Number(memLow) / Number(memHigh)} />}
                  </>
                )}
              </TableCell>
              <TableCell className="num px-2 py-2.5 font-mono text-[11px] text-muted-foreground">{c.rebuilt ? `${c.rebuilt.units}/${c.rebuilt.of}` : "—"}</TableCell>
              <TableCell className="max-w-[260px] px-2 py-2.5 text-[11px] whitespace-normal text-muted-foreground">{c.reason || (c.state === CommandState.RUNNING ? `started ${ago(since, now)}` : "")}</TableCell>
            </TableRow>
          );
        })}
      </TableBody>
    </Table>
  );
}

/** Commands that ended: outcome, time against cold, and why it failed. */
export function HistoryTable({ s, rows, limit = 200 }: { s: Snapshot; rows: FinishedCommand[]; limit?: number }) {
  const now = Number(s.now);
  const [project] = useProject();
  if (rows.length === 0) return <Empty>Nothing matches.</Empty>;
  const knownWorkspaces = new Set(s.workspaces.map((w) => w.id));
  return (
    <div>
      <Table className="text-[12px]">
        <TableHeader className="eyebrow">
          <TableRow className="border-border hover:bg-transparent">
            {[...identityColumns(project), "ended", "took", "cold", "speedup", "crates", "peak mem", "why"].map((h) => (
              <TableHead key={h} className="h-auto px-2 py-2 font-normal text-muted-foreground">
                {h}
              </TableHead>
            ))}
          </TableRow>
        </TableHeader>
        <TableBody className="num font-mono text-[11px]">
          {rows.slice(0, limit).map((c) => (
            <TableRow key={c.id} className="border-border/60 hover:bg-muted/60">
              <TableCell className="px-2 py-2">
                <StateBadge state={c.state} />
              </TableCell>
              <TableCell className="max-w-[300px] px-2 py-2">
                <Link to="/commands/$id" params={{ id: c.id }} className="block text-foreground hover:text-primary">
                  <CommandInline argv={c.argv} />
                </Link>
                {c.bench && <BenchTag />}
              </TableCell>
              <TableCell className="px-2 py-2">
                <SessionLink id={c.sessionId} />
              </TableCell>
              {!project && (
                <TableCell className="px-2 py-2">
                  <ProjectTag name={commandProject(s, c.sessionId, c.repoId)} />
                </TableCell>
              )}
              <TableCell className="px-2 py-2 text-muted-foreground">{c.repoId || "—"}</TableCell>
              <TableCell className="px-2 py-2">
                <WorkspaceCell id={c.workspaceId} known={knownWorkspaces.has(c.workspaceId)} />
              </TableCell>
              <TableCell className="px-2 py-2 text-muted-foreground">{nodeName(s, c.nodeId)}</TableCell>
              <TableCell className="px-2 py-2 text-muted-foreground">{ago(c.endedAt, now)}</TableCell>
              <TableCell className="px-2 py-2">{duration(c.seconds)}</TableCell>
              <TableCell className="px-2 py-2 text-faint">{c.coldSeconds !== undefined ? duration(c.coldSeconds) : "—"}</TableCell>
              <TableCell className="px-2 py-2 text-primary">{c.coldSeconds !== undefined ? speedup(c.coldSeconds, c.seconds) : "—"}</TableCell>
              <TableCell className="px-2 py-2 text-muted-foreground">{c.rebuilt !== undefined ? `${c.rebuilt}/${c.of ?? 0}` : "—"}</TableCell>
              <TableCell className="px-2 py-2 text-muted-foreground">
                {bytes(c.peakMemoryBytes)}
                {c.parks > 0 && <span className="text-frost"> ❄{c.parks}</span>}
              </TableCell>
              <TableCell className="px-2 py-2 font-sans text-muted-foreground">{c.failure ? (FAILURE[c.failure] ?? c.failure) : ""}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
      {rows.length > limit && <p className="mt-3 text-center font-mono text-[10.5px] text-faint">showing {limit} of {rows.length}</p>}
    </div>
  );
}

export const FAILURE: Record<string, string> = {
  compile: "compile error",
  test: "tests failed",
  lint: "lint denied",
  oom: "OOM killed",
  signal: "killed by signal",
  exit: "non-zero exit",
};
