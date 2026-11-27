import { Link } from "@tanstack/react-router";
import { useState } from "react";
import { filterTableRows, Table, TableBody, TableCell, TableFilterBar, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { CommandState, WorkspaceState, type Session, type Snapshot } from "../api";
import { ProjectTag } from "../components/ProjectTag";
import { Meter } from "../components/charts/Bars";
import { Card, Empty, StatTile } from "../components/Panel";
import { nodeName } from "../lib/data";
import { ago, cmpBig, duration } from "../lib/format";
import { isLive } from "../lib/live";
import { useProject, useScopedSnapshot } from "../lib/project";
import { idleLeftMs, SESSION_IDLE_MS, sessionState } from "../lib/sessions";
import { SessionStateLabel } from "../components/SessionLink";
import { PageTitle } from "../components/PageTitle";
import { T3ThreadTag } from "../components/T3ThreadTag";

export function SessionsPage() {
  const { data: s } = useScopedSnapshot();
  if (!s) return null;
  return <Sessions s={s} />;
}

function Sessions({ s }: { s: Snapshot }) {
  const now = Number(s.now);
  const live = s.sessions.filter((x) => x.endedAt === undefined);
  const ended = s.sessions.filter((x) => x.endedAt !== undefined);
  const working = live.filter((x) => sessionState(s, x) === "working").length;
  const abandoned = ended.filter((x) => x.endReason.startsWith("abandoned")).length;
  const failedToday = s.history.filter((c) => c.state === CommandState.FAILED).length;

  return (
    <div className="space-y-6">
      <PageTitle
        title="Sessions"
        sub="Everything an agent runs belongs to a session: its workspaces, one per repo, and every command it runs in them. A session nothing touches for an hour is abandoned."
      />
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-6">
        <StatTile label="Live" value={live.length} tone="ember" foot={`${working} working, ${live.length - working} idle`} />
        <StatTile label="Ended today" value={ended.length} foot={`${abandoned} abandoned`} />
        <StatTile label="Commands today" value={s.history.length} foot={`${failedToday} failed`} />
        <StatTile label="Running now" value={s.commands.filter(isLive).length} foot="across live sessions" />
        <StatTile label="Workspaces" value={s.workspaces.filter((w) => w.state !== WorkspaceState.RELEASED).length} foot="forks held by sessions" />
        <StatTile label="Agents" value={new Set(live.map((x) => x.agent)).size} foot="machines with live sessions" />
      </div>
      <Card title="Live" sub="Working sessions first, then idle ones by how soon they would be abandoned">
        {live.length === 0 ? (
          <Empty>No live sessions. An agent starts one with hotbed session start.</Empty>
        ) : (
          <SessionTable s={s} sessions={[...live].sort((a, b) => rank(s, a) - rank(s, b) || cmpBig(a.lastActiveAt, b.lastActiveAt))} now={now} mode="live" />
        )}
      </Card>
      <Card title="Ended in the last day" sub="By their agent, abandoned, or at the end of their scheduled run" delay={60}>
        {ended.length === 0 ? <Empty>None yet.</Empty> : <SessionTable s={s} sessions={ended} now={now} mode="ended" />}
      </Card>
    </div>
  );
}

// Working first; then, idle ones least recently touched, which are abandoned soonest.
function rank(s: Snapshot, x: Session): number {
  return sessionState(s, x) === "working" ? 0 : 1;
}

export function SessionTable({
  s,
  sessions,
  now,
  mode,
}: {
  s: Snapshot;
  sessions: Session[];
  now: number;
  mode?: "live" | "ended";
}) {
  // Every project's sessions say whose they are.
  const [project] = useProject();
  const [query, setQuery] = useState("");
  const workspaceIds = new Set(s.workspaces.map((w) => w.id));
  const lastColumn = mode === "live" ? "abandoned in" : mode === "ended" ? "ended" : "abandoned in · ended";
  const columns = ["session", ...(project ? [] : ["project"]), "state", "agent", "workspaces", "commands", "failed", "started", "last active", lastColumn];
  const filtered = filterTableRows(sessions, query, (x) => [
    x.id,
    x.schedule,
    x.t3codeThreadId,
    x.project,
    sessionState(s, x),
    x.agent,
    x.model,
    ...x.workspaces.flatMap((w) => [w.id, nodeName(s, w.nodeId)]),
    x.endReason,
    x.endedAt !== undefined ? endedHow(x) : "",
  ]);
  return (
    <>
      <TableFilterBar
        value={query}
        onChange={setQuery}
        placeholder="Filter sessions…"
        shown={filtered.length}
        total={sessions.length}
      />
      {filtered.length === 0 ? (
        <Empty>No sessions match “{query.trim()}”.</Empty>
      ) : (
        <>
          <div className="space-y-2.5 md:hidden">
            {filtered.map((x) => {
              const finished = s.history.filter((c) => c.sessionId === x.id);
              const running = s.commands.filter((c) => c.sessionId === x.id && isLive(c)).length;
              const failed = finished.filter((c) => c.state === CommandState.FAILED).length;
              const left = idleLeftMs(x, now);
              return (
                <div
                  key={x.id}
                  className={`rounded-xl border border-border/70 bg-background/40 p-3 font-mono text-[11px] ${x.endedAt !== undefined ? "text-muted-foreground" : ""}`}
                >
                  <div className="flex flex-wrap items-baseline justify-between gap-2">
                    <div className="flex flex-wrap items-baseline gap-2">
                      <Link to="/sessions/$id" params={{ id: x.id }} className="display text-[17px] font-medium text-foreground hover:text-primary">
                        {x.id}
                      </Link>
                      {!project && <ProjectTag name={x.project} />}
                    </div>
                    <SessionStateLabel s={s} session={x} />
                  </div>

                  {(x.schedule || x.t3codeThreadId) && (
                    <div className="mt-1 flex flex-wrap items-center gap-2 text-[10.5px] text-muted-foreground">
                      {x.schedule && (
                        <Link to="/schedules" className="hover:text-primary">
                          ⟳ {x.schedule}
                        </Link>
                      )}
                      {x.t3codeThreadId && (
                        <T3ThreadTag id={x.t3codeThreadId} url={x.t3codeThreadUrl} />
                      )}
                    </div>
                  )}

                  <div className="num mt-2 flex flex-wrap items-center gap-x-3 gap-y-1 text-[10.5px] text-muted-foreground">
                    <span>{x.agent}</span>
                    {x.model && <span className="rounded bg-muted px-1 py-0.5 text-foreground">{x.model}</span>}
                    <span>
                      {running > 0 && <span className="text-primary">{running} live · </span>}
                      {finished.length} cmds
                    </span>
                    {failed > 0 && <span className="text-critical">{failed} failed</span>}
                    <span className="text-faint">active {ago(x.lastActiveAt, now)}</span>
                  </div>

                  {x.workspaces.length > 0 && (
                    <div className="mt-2 flex flex-wrap gap-1.5">
                      {x.workspaces.map((w) => {
                        const cls = `rounded border px-1.5 py-0.5 text-[10px] ${w.releasedAt !== undefined ? "border-border text-faint" : "border-border-strong text-foreground"}`;
                        return workspaceIds.has(w.id) ? (
                          <Link key={w.id} to="/workspaces/$id" params={{ id: w.id }} className={`${cls} hover:border-primary/50 hover:text-primary`} title={w.id}>
                            {nodeName(s, w.nodeId)}
                          </Link>
                        ) : (
                          <span key={w.id} className={cls} title={w.id}>
                            {nodeName(s, w.nodeId)}
                          </span>
                        );
                      })}
                    </div>
                  )}

                  <div className="mt-2.5 border-t border-border/60 pt-2">
                    {x.endedAt !== undefined ? (
                      <span className={`text-[10.5px] ${x.endReason.startsWith("abandoned") ? "text-warning-ink" : "text-faint"}`}>
                        ended {ago(x.endedAt, now)} · {endedHow(x)}
                      </span>
                    ) : x.scheduleRunning ? (
                      <span className="text-[10.5px] text-muted-foreground">scheduled run · not abandoned while running</span>
                    ) : (
                      <IdleLeft left={left} />
                    )}
                  </div>
                </div>
              );
            })}
          </div>

          <div className="hidden md:block">
            <Table filterable={false} className="text-[12px]">
              <TableHeader className="eyebrow">
                <TableRow className="border-border hover:bg-transparent">
                  {columns.map((h) => (
                    <TableHead key={h} className="h-auto px-2 py-2 font-normal text-muted-foreground">
                      {h}
                    </TableHead>
                  ))}
                </TableRow>
              </TableHeader>
              <TableBody className="num font-mono text-[11px]">
                {filtered.map((x) => {
                  const finished = s.history.filter((c) => c.sessionId === x.id);
                  const running = s.commands.filter((c) => c.sessionId === x.id && isLive(c)).length;
                  const failed = finished.filter((c) => c.state === CommandState.FAILED).length;
                  const left = idleLeftMs(x, now);
                  return (
                    <TableRow key={x.id} className={`border-border/60 align-top hover:bg-muted/60 ${x.endedAt !== undefined ? "text-muted-foreground" : ""}`}>
                      <TableCell className="max-w-[260px] px-2 py-2.5 whitespace-normal">
                        <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
                          <Link to="/sessions/$id" params={{ id: x.id }} className="display text-[16px] font-medium text-foreground hover:text-primary">
                            {x.id}
                          </Link>
                          {x.schedule && (
                            <Link to="/schedules" className="text-[10.5px] text-muted-foreground hover:text-primary" title="a run of this scheduled command">
                              ⟳ {x.schedule}
                            </Link>
                          )}
                          {x.t3codeThreadId && (
                            <T3ThreadTag id={x.t3codeThreadId} url={x.t3codeThreadUrl} className="text-[10.5px] text-muted-foreground" />
                          )}
                        </div>
                      </TableCell>
                      {!project && (
                        <TableCell className="px-2 py-2.5">
                          <ProjectTag name={x.project} />
                        </TableCell>
                      )}
                      <TableCell className="px-2 py-2.5">
                        <SessionStateLabel s={s} session={x} />
                      </TableCell>
                      <TableCell className="px-2 py-2.5 text-muted-foreground">
                        {x.agent}
                        {x.model && <span className="ml-1.5 rounded bg-muted px-1 py-0.5 text-[10.5px] text-foreground">{x.model}</span>}
                      </TableCell>
                      <TableCell className="px-2 py-2.5">
                        <div className="flex flex-wrap gap-1.5">
                          {x.workspaces.map((w) => {
                            const cls = `rounded border px-1.5 py-0.5 ${w.releasedAt !== undefined ? "border-border text-faint" : "border-border-strong text-foreground"}`;
                            return workspaceIds.has(w.id) ? (
                              <Link key={w.id} to="/workspaces/$id" params={{ id: w.id }} className={`${cls} hover:border-primary/50 hover:text-primary`} title={w.id}>
                                {nodeName(s, w.nodeId)}
                              </Link>
                            ) : (
                              <span key={w.id} className={cls} title={w.id}>
                                {nodeName(s, w.nodeId)}
                              </span>
                            );
                          })}
                        </div>
                      </TableCell>
                      <TableCell className="px-2 py-2.5">
                        {running > 0 && <span className="text-primary">{running} live · </span>}
                        {finished.length}
                      </TableCell>
                      <TableCell className={`px-2 py-2.5 ${failed > 0 ? "text-critical" : "text-faint"}`}>{failed}</TableCell>
                      <TableCell className="px-2 py-2.5 text-muted-foreground">{ago(x.startedAt, now)}</TableCell>
                      <TableCell className="px-2 py-2.5 text-muted-foreground">{ago(x.lastActiveAt, now)}</TableCell>
                      <TableCell className="w-44 px-2 py-2.5">
                        {x.endedAt !== undefined ? (
                          <span className={x.endReason.startsWith("abandoned") ? "text-warning-ink" : "text-faint"}>
                            {ago(x.endedAt, now)} · {endedHow(x)}
                          </span>
                        ) : x.scheduleRunning ? (
                          <span className="text-[10.5px] text-muted-foreground">not while its run lasts</span>
                        ) : (
                          <IdleLeft left={left} />
                        )}
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          </div>
        </>
      )}
    </>
  );
}

/** Why a session ended, in a word or two. */
function endedHow(x: Session): string {
  if (x.endReason.startsWith("abandoned")) return "abandoned";
  if (x.schedule) return x.endReason.replace(/^its scheduled /, "");
  return "by its agent";
}

/**
 * How long a live session has before it is abandoned. The bar is the hour of
 * idleness it may have, filling as it goes unused: full is abandoned.
 */
export function IdleLeft({ left }: { left: number }) {
  return (
    <div>
      <div className={`mb-1 text-[10.5px] ${left < 10 * 60_000 ? "text-warning-ink" : "text-muted-foreground"}`}>{duration(left / 1000)} unless touched</div>
      <Meter value={1 - left / SESSION_IDLE_MS} />
    </div>
  );
}
