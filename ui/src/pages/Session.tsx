import { Link, useParams } from "@tanstack/react-router";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { CommandState, type Session, type Snapshot } from "../api";
import { CommandInline } from "../components/CommandBlock";
import { FAILURE } from "../components/CommandTable";
import { Crumbs } from "../components/Crumbs";
import { Card, Empty, StatTile } from "../components/Panel";
import { BenchTag, KindTag, StateBadge } from "../components/StateBadges";
import { nodeName, repoName, useSnapshot } from "../lib/data";
import { ago, duration, hhmmss, speedup } from "../lib/format";
import { idleLeftMs, sessionCommands } from "../lib/sessions";
import { filesChanged } from "../lib/workspaces";
import { NotFound } from "./NotFound";
import { PageTitle } from "../components/PageTitle";
import { IdleLeft, SessionStateLabel } from "./Sessions";
import { WorkspaceStateLabel } from "./Workspace";

export function SessionPage() {
  const { id } = useParams({ from: "/sessions/$id" });
  const { data: s } = useSnapshot();
  if (!s) return null;
  const session = s.sessions.find((x) => x.id === id);
  if (!session) return <NotFound />;
  return <SessionView s={s} session={session} />;
}

function SessionView({ s, session }: { s: Snapshot; session: Session }) {
  const now = Number(s.now);
  const rows = sessionCommands(s, session.id);
  const failed = rows.filter((r) => r.state === CommandState.FAILED).length;
  const live = rows.filter((r) => r.live).length;
  const saved = rows.reduce((n, r) => n + (!r.live && r.coldSeconds !== null ? Math.max(0, r.coldSeconds - r.seconds) : 0), 0);
  const spent = rows.reduce((n, r) => n + r.seconds, 0);
  const lasted = ((session.endedAt !== undefined ? Number(session.endedAt) : now) - Number(session.startedAt)) / 1000;

  return (
    <div className="space-y-6">
      <Crumbs section={{ label: "sessions", to: "/sessions" }} project={session.project || null} here={session.id} />
      <PageTitle title={session.id} right={<SessionStateLabel s={s} session={session} />} />
      <p className="rise -mt-3 flex flex-wrap items-center gap-x-1.5 gap-y-0.5 text-[13px] text-muted-foreground">
        <span>{session.schedule ? `a run of the scheduled command ${session.schedule}` : session.agent}</span>
        {session.model && (
          <>
            <span>·</span>
            <span>
              model <span className="font-mono text-foreground/85">{session.model}</span>
            </span>
          </>
        )}
        {session.t3codeThreadId && (
          <>
            <span>·</span>
            <span className="font-mono" title={session.t3codeThreadId}>
              t3:{session.t3codeThreadId.slice(0, 8)}
            </span>
          </>
        )}
        <span>·</span>
        <span>started {ago(session.startedAt, now)}</span>
        <span>·</span>
        <span>last active {ago(session.lastActiveAt, now)}</span>
        {session.endedAt !== undefined && (
          <>
            <span>·</span>
            <span>
              ended {ago(session.endedAt, now)}: {session.endReason}
            </span>
          </>
        )}
      </p>
      {session.endedAt === undefined &&
        (session.scheduleRunning ? (
          <p className="rise rounded-xl border border-border bg-card/70 px-3.5 py-2.5 text-[12px] text-muted-foreground">
            A scheduled run: its session is not abandoned while the run lasts. It ends when the run succeeds; a failed run keeps it for an hour, for a look inside.{" "}
            <Link to="/schedules" className="text-primary hover:underline">
              Schedules
            </Link>
          </p>
        ) : (
          <div className="rise flex flex-col gap-1.5 rounded-xl border border-border bg-card/70 px-3.5 py-2.5 sm:flex-row sm:items-center sm:gap-4">
            <span className="eyebrow shrink-0">abandoned in</span>
            <div className="min-w-0 flex-1 sm:max-w-sm">
              <IdleLeft left={idleLeftMs(session, now)} />
            </div>
          </div>
        ))}
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-6">
        <StatTile label="Commands" value={rows.length} foot={`${live} live`} tone="ember" />
        <StatTile label="Failed" value={failed} foot={rows.length ? `${Math.round((100 * failed) / rows.length)}% of them` : "none run"} />
        <StatTile label="Ran for" value={duration(spent)} foot="all commands together" />
        <StatTile label="Saved" value={duration(saved)} tone="good" foot="against cold builds" />
        <StatTile label="Workspaces" value={session.workspaces.length} foot={`${session.workspaces.filter((w) => w.releasedAt === undefined).length} held`} />
        <StatTile label="Lasted" value={duration(lasted)} foot={session.endedAt !== undefined ? "start to end" : "so far"} />
      </div>

      <Card title="Workspaces" sub="One per node; each a fork of its project's built base, with every repo at /work/<repo>">
        {session.workspaces.length === 0 ? (
          <Empty>No workspaces created in this session yet.</Empty>
        ) : (
          <>
            <div className="space-y-2 md:hidden">
              {session.workspaces.map((sw) => {
                const w = s.workspaces.find((x) => x.id === sw.id);
                return (
                  <div key={sw.id} className="rounded-lg border border-border/70 bg-background/40 p-2.5 font-mono text-[11px]">
                    <div className="flex flex-wrap items-baseline justify-between gap-2">
                      {w ? (
                        <Link to="/workspaces/$id" params={{ id: sw.id }} className="font-medium text-foreground hover:text-primary">
                          {sw.id}
                        </Link>
                      ) : (
                        <span className="text-faint">{sw.id}</span>
                      )}
                      {w ? <WorkspaceStateLabel w={w} now={now} /> : <span className="text-faint">released {sw.releasedAt !== undefined ? ago(sw.releasedAt, now) : ""}</span>}
                    </div>
                    <div className="num mt-1.5 flex flex-wrap gap-x-3 gap-y-0.5 text-[10.5px] text-muted-foreground">
                      <span>{nodeName(s, sw.nodeId)}</span>
                      {w && <span>{w.repos.map((r) => r.repo).join(", ")}</span>}
                      <span>made {ago(sw.createdAt, now)}</span>
                      {w && <span>{filesChanged(w)} changed</span>}
                    </div>
                  </div>
                );
              })}
            </div>

            <div className="hidden md:block">
              <Table className="text-[12px]">
                <TableHeader className="eyebrow">
                  <TableRow className="border-border hover:bg-transparent">
                    {["workspace", "repos", "node", "state", "made", "files changed"].map((h) => (
                      <TableHead key={h} className="h-auto px-2 py-2 font-normal text-muted-foreground">
                        {h}
                      </TableHead>
                    ))}
                  </TableRow>
                </TableHeader>
                <TableBody className="num font-mono text-[11px]">
                  {session.workspaces.map((sw) => {
                    const w = s.workspaces.find((x) => x.id === sw.id);
                    return (
                      <TableRow key={sw.id} className="border-border/60 hover:bg-muted/60">
                        <TableCell className="px-2 py-2">
                          {w ? (
                            <Link to="/workspaces/$id" params={{ id: sw.id }} className="text-foreground hover:text-primary">
                              {sw.id}
                            </Link>
                          ) : (
                            <span className="text-faint">{sw.id}</span>
                          )}
                        </TableCell>
                        <TableCell className="px-2 py-2">{w ? w.repos.map((r) => r.repo).join(", ") : <span className="text-faint">—</span>}</TableCell>
                        <TableCell className="px-2 py-2 text-muted-foreground">{nodeName(s, sw.nodeId)}</TableCell>
                        <TableCell className="px-2 py-2">{w ? <WorkspaceStateLabel w={w} now={now} /> : <span className="text-faint">released {sw.releasedAt !== undefined ? ago(sw.releasedAt, now) : ""}</span>}</TableCell>
                        <TableCell className="px-2 py-2 text-muted-foreground">{ago(sw.createdAt, now)}</TableCell>
                        <TableCell className="px-2 py-2">{w ? filesChanged(w) : "—"}</TableCell>
                      </TableRow>
                    );
                  })}
                </TableBody>
              </Table>
            </div>
          </>
        )}
      </Card>

      <Card title="Commands" sub="Every command the session ran, newest first: live ones, and those that ended in the last day" delay={60}>
        {rows.length === 0 ? (
          <Empty>Nothing run yet.</Empty>
        ) : (
          <>
            <div className="space-y-2 md:hidden">
              {rows.map((r) => {
                const why = r.live ? r.reason : r.failure ? (FAILURE[r.failure] ?? r.failure) : r.state === CommandState.FAILED && r.exitCode !== null ? `exit ${r.exitCode}` : "";
                return (
                  <div key={r.id} className="rounded-lg border border-border/70 bg-background/40 p-2.5 font-mono text-[11px]">
                    <div className="flex flex-wrap items-center gap-2">
                      <StateBadge state={r.state} />
                      <KindTag kind={r.kind} />
                      {r.bench && <BenchTag />}
                      <span className="num ml-auto text-[10.5px] text-faint">{hhmmss(r.at)}</span>
                    </div>
                    <Link to="/commands/$id" params={{ id: r.id }} className="mt-1.5 block text-[12px] text-foreground hover:text-primary">
                      <CommandInline argv={r.argv} args={r.args} />
                    </Link>
                    <div className="num mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1 text-[10.5px] text-muted-foreground">
                      {r.repoId && <span>{repoName(s, r.repoId)}</span>}
                      <span>{nodeName(s, r.nodeId)}</span>
                      <span className="text-foreground">
                        {duration(r.seconds)}
                        {r.live && <span className="text-faint"> so far</span>}
                      </span>
                      {!r.live && r.coldSeconds !== null && <span className="text-primary">{speedup(r.coldSeconds, r.seconds)}</span>}
                      {r.url && (
                        <a href={r.url} className="text-primary hover:underline" target="_blank" rel="noreferrer">
                          {r.url.replace("http://", "")} ↗
                        </a>
                      )}
                    </div>
                    {why && <p className="mt-1 font-sans text-[11px] text-muted-foreground">{why}</p>}
                  </div>
                );
              })}
            </div>

            <div className="hidden md:block">
              <Table className="text-[12px]">
                <TableHeader className="eyebrow">
                  <TableRow className="border-border hover:bg-transparent">
                    {["at", "state", "command", "repo", "node", "took", "speedup", "why"].map((h) => (
                      <TableHead key={h} className="h-auto px-2 py-2 font-normal text-muted-foreground">
                        {h}
                      </TableHead>
                    ))}
                  </TableRow>
                </TableHeader>
                <TableBody className="num font-mono text-[11px]">
                  {rows.map((r) => (
                    <TableRow key={r.id} className="border-border/60 align-top hover:bg-muted/60">
                      <TableCell className="px-2 py-2 text-faint" title={new Date(r.at).toISOString()}>
                        {hhmmss(r.at)}
                      </TableCell>
                      <TableCell className="px-2 py-2">
                        <StateBadge state={r.state} />
                      </TableCell>
                      <TableCell className="max-w-[420px] px-2 py-2 whitespace-normal">
                        <Link to="/commands/$id" params={{ id: r.id }} className="block text-foreground hover:text-primary">
                          <CommandInline argv={r.argv} args={r.args} />
                        </Link>
                        <div className="mt-0.5 flex gap-2">
                          <KindTag kind={r.kind} />
                          {r.bench && <BenchTag />}
                          {r.url && (
                            <a href={r.url} className="text-[10px] text-primary hover:underline" target="_blank" rel="noreferrer">
                              {r.url.replace("http://", "")} ↗
                            </a>
                          )}
                        </div>
                      </TableCell>
                      <TableCell className="px-2 py-2 text-muted-foreground">{r.repoId ? repoName(s, r.repoId) : "—"}</TableCell>
                      <TableCell className="px-2 py-2 text-muted-foreground">{nodeName(s, r.nodeId)}</TableCell>
                      <TableCell className="px-2 py-2">{duration(r.seconds)}{r.live && <span className="text-faint"> so far</span>}</TableCell>
                      <TableCell className="px-2 py-2 text-primary">{!r.live && r.coldSeconds !== null ? speedup(r.coldSeconds, r.seconds) : "—"}</TableCell>
                      <TableCell className="max-w-[260px] px-2 py-2 font-sans whitespace-normal text-muted-foreground">
                        {r.live ? r.reason : r.failure ? (FAILURE[r.failure] ?? r.failure) : r.state === CommandState.FAILED && r.exitCode !== null ? `exit ${r.exitCode}` : ""}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          </>
        )}
      </Card>
    </div>
  );
}
