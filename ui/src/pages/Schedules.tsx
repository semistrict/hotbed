import { useState } from "react";
import { filterTableRows, Table, TableBody, TableCell, TableFilterBar, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { RunOutcome, runOutcomeLabel, type ScheduledCommand, type ScheduledRun, type Snapshot } from "../api";
import { CommandBlock } from "../components/CommandBlock";
import { ProjectTag } from "../components/ProjectTag";
import { SessionLink } from "../components/SessionLink";
import { Card, Empty, StatTile } from "../components/Panel";
import { OutcomeBadge } from "../components/StateBadges";
import { shellJoinWords } from "../lib/command";
import { nodeName, repoName } from "../lib/data";
import { duration, hhmm } from "../lib/format";
import { useProject, useScopedSnapshot } from "../lib/project";
import { nextRun } from "../lib/schedules";
import { PageTitle } from "../components/PageTitle";

export function SchedulesPage() {
  const { data: s } = useScopedSnapshot();
  if (!s) return null;
  return <Schedules s={s} />;
}

const DAY = 24 * 3600_000;

function Schedules({ s }: { s: Snapshot }) {
  const [project] = useProject();
  const now = Number(s.now);
  const runs = s.schedules.flatMap((sc) => sc.runs);
  const running = s.sessions.filter((x) => x.scheduleRunning).length;
  const lastDay = runs.filter((r) => r.endedAt !== undefined && Number(r.endedAt) > now - DAY);
  const failed = lastDay.filter((r) => r.outcome === RunOutcome.FAILED || r.outcome === RunOutcome.NOT_STARTED).length;
  const unrun = lastDay.filter((r) => r.outcome === RunOutcome.MISSED || r.outcome === RunOutcome.SKIPPED).length;
  const next = nextRun(s);
  const paused = s.schedules.filter((sc) => sc.paused).length;
  const scopeLabel = project ?? "every project";

  return (
    <div className="space-y-6">
      <PageTitle
        title="Schedules"
        sub={`Recurring work the controller starts by itself in ${scopeLabel}. Each run is a session of its own, with a fresh workspace of the repo at its branch's head. A run that succeeds ends its session; one that fails keeps it for the idle hour, for an agent to look inside.`}
      />
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <StatTile label="Scheduled" value={s.schedules.length} foot={paused ? `${paused} paused` : "none paused"} />
        <StatTile label="Running now" value={running} tone="ember" foot="each in its own session" />
        <StatTile label="Last day" value={lastDay.length - unrun} foot={`${failed} failed, ${unrun} skipped or missed`} />
        <StatTile label="Next run" value={next ? until(next.at, now) : "—"} foot={next ? `${next.name} at ${hhmm(next.at)}` : "nothing due"} />
      </div>
      {s.schedules.length === 0 ? (
        <Card>
          <Empty>Nothing scheduled in {scopeLabel}. Add a command with hotbed schedule add NAME REPO --cron '0 2 * * *' -- CMD.</Empty>
        </Card>
      ) : (
        s.schedules.map((sc, i) => <ScheduleCard key={sc.name} s={s} sc={sc} now={now} delay={Math.min(i, 8) * 40} />)
      )}
    </div>
  );
}

function ScheduleCard({ s, sc, now, delay }: { s: Snapshot; sc: ScheduledCommand; now: number; delay: number }) {
  const [project] = useProject();
  const where = `${repoName(s, sc.repo)}${sc.arch ? ` (${sc.arch})` : ""}`;
  const when = sc.paused ? "paused" : sc.nextAt !== undefined ? `next ${until(Number(sc.nextAt), now)}, ${dayTime(Number(sc.nextAt))}` : "";
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
          <span className="font-mono text-foreground">{sc.cron}</span> · {where}
          {when && (
            <>
              {" "}· <span className={sc.paused ? "text-warning-ink" : ""}>{when}</span>
            </>
          )}
        </>
      }
      right={sc.runs.length > 0 ? <RunStrip runs={sc.runs} /> : undefined}
      delay={delay}
    >
      <CommandBlock argv={shellJoinWords(sc.command?.args ?? [])} args={sc.command?.args} env={sc.command?.env} variant="compact" maxLines={8} />
      {sc.runs.length === 0 ? <Empty>No runs yet.</Empty> : <RunTable s={s} runs={sc.runs} now={now} />}
    </Card>
  );
}

const MAX_STRIP_RUNS = 30;

/** The recent runs, oldest left, one mark each, so a streak of failures stands out. */
function RunStrip({ runs }: { runs: ScheduledRun[] }) {
  if (runs.length === 0) return null;
  return (
    <div className="flex flex-wrap items-center gap-0.5" aria-label="recent runs, oldest first">
      {[...runs.slice(0, MAX_STRIP_RUNS)].reverse().map((r) => (
        <span
          key={String(r.id)}
          title={`${r.slotAt !== undefined ? dayTime(Number(r.slotAt)) : "by hand"}: ${runOutcomeLabel(r.outcome)}`}
          className={`inline-block h-4 w-1.5 rounded-sm ${MARK[r.outcome] ?? "bg-faint"}`}
        />
      ))}
    </div>
  );
}

const MARK: Record<RunOutcome, string> = {
  [RunOutcome.UNSPECIFIED]: "bg-faint",
  [RunOutcome.RUNNING]: "bg-primary ember-pulse",
  [RunOutcome.SUCCEEDED]: "bg-good",
  [RunOutcome.FAILED]: "bg-critical",
  [RunOutcome.CANCELLED]: "bg-faint",
  [RunOutcome.NOT_STARTED]: "bg-critical",
  [RunOutcome.SKIPPED]: "border border-border-strong",
  [RunOutcome.MISSED]: "bg-warning",
};

function RunTable({ s, runs, now }: { s: Snapshot; runs: ScheduledRun[]; now: number }) {
  const [query, setQuery] = useState("");
  const filtered = filterTableRows(runs, query, (r) => {
    const started = Number(r.startedAt);
    return [
      r.slotAt !== undefined ? dayTime(Number(r.slotAt)) : `${dayTime(started)} by hand`,
      runOutcomeLabel(r.outcome),
      r.sessionId,
      r.nodeId ? nodeName(s, r.nodeId) : "",
      r.nodeId,
      r.commandId,
      r.reason,
    ];
  });
  return (
    <>
      <TableFilterBar
        value={query}
        onChange={setQuery}
        placeholder="Filter runs…"
        shown={filtered.length}
        total={runs.length}
      />
      {filtered.length === 0 ? (
        <Empty>No runs match “{query.trim()}”.</Empty>
      ) : (
        <>
          <div className="space-y-2 lg:hidden">
            {filtered.map((r) => {
              const started = Number(r.startedAt);
              const took = ((r.endedAt !== undefined ? Number(r.endedAt) : now) - started) / 1000;
              const ran = r.sessionId !== "" && r.outcome !== RunOutcome.NOT_STARTED;
              return (
                <div key={String(r.id)} className="rounded-lg border border-border/70 bg-background/40 p-2.5 font-mono text-[11px]">
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <OutcomeBadge outcome={r.outcome} />
                    <span className="text-[10.5px] text-muted-foreground" title={new Date(started).toISOString()}>
                      {r.slotAt !== undefined ? dayTime(Number(r.slotAt)) : `${dayTime(started)} · by hand`}
                    </span>
                  </div>
                  <div className="num mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1 text-[10.5px] text-muted-foreground">
                    <SessionLink id={r.sessionId} empty={null} />
                    {r.nodeId && <span>{nodeName(s, r.nodeId)}</span>}
                    <span>
                      {ran ? duration(took) : "—"}
                      {r.outcome === RunOutcome.RUNNING && <span className="text-faint"> so far</span>}
                    </span>
                  </div>
                  {r.reason && <p className="mt-1 font-sans text-[11.5px] text-muted-foreground">{r.reason}</p>}
                </div>
              );
            })}
          </div>

          <div className="hidden lg:block">
            <Table filterable={false} className="text-[12px]">
              <TableHeader className="eyebrow">
                <TableRow className="border-border hover:bg-transparent">
                  {["due", "outcome", "session", "node", "took", "why"].map((h) => (
                    <TableHead key={h} className="h-auto px-2 py-2 font-normal text-muted-foreground">
                      {h}
                    </TableHead>
                  ))}
                </TableRow>
              </TableHeader>
              <TableBody className="num font-mono text-[11px]">
                {filtered.map((r) => {
                  const started = Number(r.startedAt);
                  const took = ((r.endedAt !== undefined ? Number(r.endedAt) : now) - started) / 1000;
                  const ran = r.sessionId !== "" && r.outcome !== RunOutcome.NOT_STARTED;
                  return (
                    <TableRow key={String(r.id)} className="border-border/60 align-top hover:bg-muted/60">
                      <TableCell className="px-2 py-2 text-muted-foreground" title={new Date(started).toISOString()}>
                        {r.slotAt !== undefined ? dayTime(Number(r.slotAt)) : <span>{dayTime(started)} · by hand</span>}
                      </TableCell>
                      <TableCell className="px-2 py-2">
                        <OutcomeBadge outcome={r.outcome} />
                      </TableCell>
                      <TableCell className="px-2 py-2">
                        <SessionLink id={r.sessionId} />
                      </TableCell>
                      <TableCell className="px-2 py-2 text-muted-foreground">{r.nodeId ? nodeName(s, r.nodeId) : "—"}</TableCell>
                      <TableCell className="px-2 py-2">
                        {ran ? duration(took) : "—"}
                        {r.outcome === RunOutcome.RUNNING && <span className="text-faint"> so far</span>}
                      </TableCell>
                      <TableCell className="max-w-[420px] px-2 py-2 font-sans whitespace-normal text-muted-foreground">{r.reason}</TableCell>
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

/** "Nov 21 02:00", local. */
function dayTime(t: number): string {
  return `${new Date(t).toLocaleDateString(undefined, { month: "short", day: "numeric" })} ${hhmm(t)}`;
}

/** "in 5h", "in 12m", or "due" once it has come. */
function until(t: number, now: number): string {
  const s = (t - now) / 1000;
  return s <= 0 ? "due" : `in ${duration(s)}`;
}
