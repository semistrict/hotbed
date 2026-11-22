import { Link } from "@tanstack/react-router";
import type { Session, Snapshot } from "../api";
import { Meter } from "../components/charts/Bars";
import { Card, Empty, StatTile } from "../components/ui/Card";
import { nodeName, useSnapshot } from "../lib/data";
import { ago, duration } from "../lib/format";
import { isLive } from "../lib/live";
import { idleLeftMs, SESSION_IDLE_MS, sessionState, type SessionState } from "../lib/sessions";
import { PageTitle } from "./Nodes";

export function SessionsPage() {
  const { data: s } = useSnapshot();
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
  const now = Date.parse(s.now);
  const live = s.sessions.filter((x) => x.endedAt === null);
  const ended = s.sessions.filter((x) => x.endedAt !== null);
  const working = live.filter((x) => sessionState(s, x) === "working").length;
  const abandoned = ended.filter((x) => x.endReason.startsWith("abandoned")).length;
  const failedToday = s.history.filter((c) => c.state === "failed").length;

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
        <StatTile label="Workspaces" value={s.workspaces.filter((w) => w.state !== "released").length} foot="forks held by sessions" />
        <StatTile label="Agents" value={new Set(live.map((x) => x.agent)).size} foot="machines with live sessions" />
      </div>
      <Card title="Live" sub="Working sessions first, then idle ones by how soon they would be abandoned">
        {live.length === 0 ? (
          <Empty>No live sessions. An agent starts one with hotbed session start.</Empty>
        ) : (
          <SessionTable s={s} sessions={[...live].sort((a, b) => rank(s, a) - rank(s, b) || a.lastActiveAt.localeCompare(b.lastActiveAt))} now={now} />
        )}
      </Card>
      <Card title="Ended in the last day" sub="By their agent, or abandoned" delay={60}>
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
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-[12px]">
        <thead className="eyebrow">
          <tr className="border-b border-border">
            {["session", "state", "agent", "workspaces", "commands", "failed", "started", "last active", "abandoned in · ended"].map((h) => (
              <th key={h} className="px-2 py-2 font-normal whitespace-nowrap">
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="num font-mono text-[11px] [&_td]:whitespace-nowrap">
          {sessions.map((x) => {
            const finished = s.history.filter((c) => c.sessionId === x.id);
            const running = s.commands.filter((c) => c.sessionId === x.id && isLive(c)).length;
            const failed = finished.filter((c) => c.state === "failed").length;
            const left = idleLeftMs(x, now);
            return (
              <tr key={x.id} className={`border-b border-border/60 hover:bg-muted/60 ${x.endedAt ? "text-muted-foreground" : ""}`}>
                <td className="px-2 py-2.5">
                  <Link to="/sessions/$id" params={{ id: x.id }} className="display text-[16px] font-medium text-foreground hover:text-primary">
                    {x.id}
                  </Link>
                </td>
                <td className="px-2 py-2.5">
                  <SessionStateLabel s={s} session={x} />
                </td>
                <td className="px-2 py-2.5 text-muted-foreground">{x.agent}</td>
                <td className="px-2 py-2.5">
                  <div className="flex flex-wrap gap-1.5">
                    {x.workspaces.map((w) => (
                      <span key={w.id} className={`rounded border px-1.5 py-0.5 ${w.releasedAt ? "border-border text-faint" : "border-border-strong text-foreground"}`} title={w.id}>
                        {w.repoId}
                        <span className="text-faint"> @{nodeName(s, w.nodeId)}</span>
                      </span>
                    ))}
                  </div>
                </td>
                <td className="px-2 py-2.5">
                  {running > 0 && <span className="text-primary">{running} live · </span>}
                  {finished.length}
                </td>
                <td className={`px-2 py-2.5 ${failed > 0 ? "text-critical" : "text-faint"}`}>{failed}</td>
                <td className="px-2 py-2.5 text-muted-foreground">{ago(x.startedAt, now)}</td>
                <td className="px-2 py-2.5 text-muted-foreground">{ago(x.lastActiveAt, now)}</td>
                <td className="w-44 px-2 py-2.5">
                  {x.endedAt ? (
                    <span className={x.endReason.startsWith("abandoned") ? "text-warning" : "text-faint"}>
                      {ago(x.endedAt, now)} · {x.endReason.startsWith("abandoned") ? "abandoned" : "by its agent"}
                    </span>
                  ) : (
                    <IdleLeft left={left} />
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
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
