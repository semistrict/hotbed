import type { ScheduledCommand, ScheduledRun, Snapshot } from "../api";
import { CommandBlock } from "../components/CommandBlock";
import { ProjectTag } from "../components/ProjectTag";
import { SessionLink } from "../components/SessionLink";
import { Card, Empty, StatTile } from "../components/ui/Card";
import { OutcomeBadge } from "../components/ui/State";
import { nodeName, repoName } from "../lib/data";
import { duration, hhmm } from "../lib/format";
import { useProject, useScopedSnapshot } from "../lib/project";
import { nextRun } from "../lib/schedules";
import { PageTitle } from "./Nodes";

export function SchedulesPage() {
  const { data: s } = useScopedSnapshot();
  if (!s) return null;
  return <Schedules s={s} />;
}

const DAY = 24 * 3600_000;

function Schedules({ s }: { s: Snapshot }) {
  const now = Date.parse(s.now);
  const runs = s.schedules.flatMap((sc) => sc.runs);
  const running = s.sessions.filter((x) => x.scheduleRunning).length;
  const lastDay = runs.filter((r) => r.endedAt !== null && Date.parse(r.endedAt) > now - DAY);
  const failed = lastDay.filter((r) => r.outcome === "failed" || r.outcome === "not-started").length;
  const unrun = lastDay.filter((r) => r.outcome === "missed" || r.outcome === "skipped").length;
  const next = nextRun(s);
  const paused = s.schedules.filter((sc) => sc.paused).length;

  return (
    <div className="space-y-6">
      <PageTitle
        title="Schedules"
        sub="Recurring work the controller starts by itself. Each run is a session of its own, with a fresh workspace of the repo at its branch's head. A run that succeeds ends its session; one that fails keeps it for the idle hour, for an agent to look inside."
      />
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <StatTile label="Scheduled" value={s.schedules.length} foot={paused ? `${paused} paused` : "none paused"} />
        <StatTile label="Running now" value={running} tone="ember" foot="each in its own session" />
        <StatTile label="Last day" value={lastDay.length - unrun} foot={`${failed} failed, ${unrun} skipped or missed`} />
        <StatTile label="Next run" value={next ? until(next.at, now) : "—"} foot={next ? `${next.name} at ${hhmm(next.at)}` : "nothing due"} />
      </div>
      {s.schedules.length === 0 ? (
        <Card>
          <Empty>Nothing scheduled. Add a command with hotbed schedule add NAME REPO --cron '0 2 * * *' -- CMD.</Empty>
        </Card>
      ) : (
        s.schedules.map((sc, i) => <ScheduleCard key={sc.name} s={s} sc={sc} now={now} delay={60 * (i + 1)} />)
      )}
    </div>
  );
}

function ScheduleCard({ s, sc, now, delay }: { s: Snapshot; sc: ScheduledCommand; now: number; delay: number }) {
  const [project] = useProject();
  const where = `${repoName(s, sc.repoId)}${sc.node ? ` @${sc.node}` : ""}`;
  const when = sc.paused ? "paused" : sc.nextAt ? `next ${until(Date.parse(sc.nextAt), now)}, ${dayTime(Date.parse(sc.nextAt))}` : "";
  return (
    <Card
      title={
        <span className="inline-flex items-baseline gap-2.5">
          <span className="display text-[20px] font-medium">{sc.name}</span>
          {project === null && sc.project && <ProjectTag name={sc.project} />}
        </span>
      }
      sub={
        <>
          <span className="font-mono text-foreground">{sc.cron}</span> · {where} · <span className={sc.paused ? "text-warning" : ""}>{when}</span>
        </>
      }
      right={<RunStrip runs={sc.runs} />}
      delay={delay}
    >
      <CommandBlock argv={sc.argv} env={sc.env} variant="compact" maxLines={8} />
      {sc.runs.length === 0 ? <Empty>No runs yet.</Empty> : <RunTable s={s} runs={sc.runs} now={now} />}
    </Card>
  );
}

/** The recent runs, oldest left, one mark each, so a streak of failures stands out. */
function RunStrip({ runs }: { runs: ScheduledRun[] }) {
  return (
    <div className="flex items-center gap-0.5" aria-label="recent runs, oldest first">
      {[...runs].reverse().map((r) => (
        <span key={r.id} title={`${r.slotAt ? dayTime(Date.parse(r.slotAt)) : "by hand"}: ${r.outcome}`} className={`inline-block h-4 w-1.5 rounded-sm ${MARK[r.outcome]}`} />
      ))}
    </div>
  );
}

const MARK: Record<ScheduledRun["outcome"], string> = {
  running: "bg-primary ember-pulse",
  succeeded: "bg-good",
  failed: "bg-critical",
  cancelled: "bg-faint",
  "not-started": "bg-critical",
  skipped: "border border-border-strong",
  missed: "bg-warning",
};

function RunTable({ s, runs, now }: { s: Snapshot; runs: ScheduledRun[]; now: number }) {
  const listed = new Set(s.sessions.map((x) => x.id));
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-[12px]">
        <thead className="eyebrow">
          <tr className="border-b border-border">
            {["due", "outcome", "session", "node", "took", "why"].map((h) => (
              <th key={h} className="px-2 py-2 font-normal whitespace-nowrap">
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="num font-mono text-[11px]">
          {runs.map((r) => {
            const started = Date.parse(r.startedAt);
            const took = ((r.endedAt ? Date.parse(r.endedAt) : now) - started) / 1000;
            const ran = r.sessionId !== null && r.outcome !== "not-started";
            return (
              <tr key={r.id} className="border-b border-border/60 align-top hover:bg-muted/60">
                <td className="px-2 py-2 whitespace-nowrap text-muted-foreground" title={r.startedAt}>
                  {r.slotAt ? dayTime(Date.parse(r.slotAt)) : <span>{dayTime(started)} · by hand</span>}
                </td>
                <td className="px-2 py-2">
                  <OutcomeBadge outcome={r.outcome} />
                </td>
                <td className="px-2 py-2 whitespace-nowrap">
                  {r.sessionId === null ? <span className="text-faint">—</span> : listed.has(r.sessionId) ? <SessionLink id={r.sessionId} /> : <span className="text-faint" title="ended over a day ago">{r.sessionId}</span>}
                </td>
                <td className="px-2 py-2 whitespace-nowrap text-muted-foreground">{r.nodeId ? nodeName(s, r.nodeId) : "—"}</td>
                <td className="px-2 py-2 whitespace-nowrap">
                  {ran ? duration(took) : "—"}
                  {r.outcome === "running" && <span className="text-faint"> so far</span>}
                </td>
                <td className="max-w-[420px] px-2 py-2 font-sans text-muted-foreground">{r.reason}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

/** "Nov 21 02:00", local. */
function dayTime(t: number): string {
  return `${new Date(t).toLocaleDateString(undefined, { month: "short", day: "numeric" })} ${hhmm(t)}`;
}

/** "in 5h", "in 12m", or "due" once it has come. */
function until(t: number, now: number): string {
  const s = (t - now) / 1000;
  return s <= 0 ? "due" : `in ${duration(s)}`;
}
