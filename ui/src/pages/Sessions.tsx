import { Link } from "@tanstack/react-router";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { CommandState, WorkspaceState, type Session, type Snapshot } from "../api";
import { ProjectTag } from "../components/ProjectTag";
import { Meter } from "../components/charts/Bars";
import { Card, Empty, StatTile } from "../components/Panel";
import { nodeName } from "../lib/data";
import { ago, cmpBig, duration } from "../lib/format";
import { isLive } from "../lib/live";
import { useProject, useScopedSnapshot } from "../lib/project";
import { idleLeftMs, SESSION_IDLE_MS, sessionState, type SessionState } from "../lib/sessions";
import { PageTitle } from "./Nodes";

export function SessionsPage() {
  const { data: s } = useScopedSnapshot();
  if (!s) return null;
  return <Sessions s={s} />;
}

const STATE_LABEL: Record<SessionState, { text: string; className: string }> = {
  working: { text: "● working", className: "text-primary" },
  idle: { text: "○ idle", className: "text-sprout" },
  ended: { text: "ended", className: "text-faint" },
};

export function SessionStateLabel({ s, session }: { s: Snapshot; session: Session }) {
  const st = STATE_LABEL[sessionState(s, session)];
  return <span className={`font-mono text-[11px] whitespace-nowrap ${st.className}`}>{st.text}</span>;
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
          <SessionTable s={s} sessions={[...live].sort((a, b) => rank(s, a) - rank(s, b) || cmpBig(a.lastActiveAt, b.lastActiveAt))} now={now} />
        )}
      </Card>
      <Card title="Ended in the last day" sub="By their agent, abandoned, or at the end of their scheduled run" delay={60}>
        {ended.length === 0 ? <Empty>None yet.</Empty> : <SessionTable s={s} sessions={ended} now={now} />}
      </Card>
    </div>
  );
}

// Working first; then, idle ones least recently touched, which are abandoned soonest.
function rank(s: Snapshot, x: Session): number {
  return sessionState(s, x) === "working" ? 0 : 1;
}

export function SessionTable({ s, sessions, now }: { s: Snapshot; sessions: Session[]; now: number }) {
  // Every project's sessions say whose they are.
  const [project] = useProject();
  const columns = ["session", ...(project ? [] : ["project"]), "state", "agent", "workspaces", "commands", "failed", "started", "last active", "abandoned in · ended"];
  return (
    <Table className="text-[12px]">
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
        {sessions.map((x) => {
          const finished = s.history.filter((c) => c.sessionId === x.id);
          const running = s.commands.filter((c) => c.sessionId === x.id && isLive(c)).length;
          const failed = finished.filter((c) => c.state === CommandState.FAILED).length;
          const left = idleLeftMs(x, now);
          return (
            <TableRow key={x.id} className={`border-border/60 hover:bg-muted/60 ${x.endedAt !== undefined ? "text-muted-foreground" : ""}`}>
              <TableCell className="px-2 py-2.5">
                <Link to="/sessions/$id" params={{ id: x.id }} className="display text-[16px] font-medium text-foreground hover:text-primary">
                  {x.id}
                </Link>
                {x.schedule && (
                  <Link to="/schedules" className="ml-2 text-[10.5px] text-muted-foreground hover:text-primary" title="a run of this scheduled command">
                    ⟳ {x.schedule}
                  </Link>
                )}
                {x.t3codeThreadId && (
                  <span className="ml-2 text-[10.5px] text-muted-foreground" title={`T3 Code thread ${x.t3codeThreadId}`}>
                    t3:{x.t3codeThreadId}
                  </span>
                )}
              </TableCell>
              {!project && (
                <TableCell className="px-2 py-2.5">
                  <ProjectTag name={x.project} />
                </TableCell>
              )}
              <TableCell className="px-2 py-2.5">
                <SessionStateLabel s={s} session={x} />
              </TableCell>
              <TableCell className="px-2 py-2.5 text-muted-foreground">{x.agent}</TableCell>
              <TableCell className="px-2 py-2.5">
                <div className="flex flex-wrap gap-1.5">
                  {x.workspaces.map((w) => (
                    <span key={w.id} className={`rounded border px-1.5 py-0.5 ${w.releasedAt !== undefined ? "border-border text-faint" : "border-border-strong text-foreground"}`} title={w.id}>
                      {nodeName(s, w.nodeId)}
                    </span>
                  ))}
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
                  <span className={x.endReason.startsWith("abandoned") ? "text-warning" : "text-faint"}>
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
      <div className={`mb-1 text-[10.5px] ${left < 10 * 60_000 ? "text-warning" : "text-muted-foreground"}`}>{duration(left / 1000)} unless touched</div>
      <Meter value={1 - left / SESSION_IDLE_MS} />
    </div>
  );
}
