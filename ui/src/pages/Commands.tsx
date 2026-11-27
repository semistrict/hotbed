import { Link, useSearch } from "@tanstack/react-router";
import { useState, type ReactNode } from "react";
import { COMMAND_STATE_FROM_SLUG, CommandState, type CommandStateSlug, type FinishedCommand, type Snapshot } from "../api";
import { FAILURE, HistoryTable, LiveCommandTable } from "../components/CommandTable";
import { Columns } from "../components/charts/Columns";
import { Card, Empty, StatTile } from "../components/Panel";
import { bytes, cmpBig, compact, duration, percent, quantile } from "../lib/format";
import { isLive } from "../lib/live";
import { useScopedSnapshot } from "../lib/project";
import { clusterNow, compiled, failureRate, savings } from "../lib/stats";
import type { CommandsSearch } from "../router";
import { PageTitle } from "./Nodes";

const STATES: { id: CommandStateSlug | "live"; label: string }[] = [
  { id: "live", label: "all live" },
  { id: "running", label: "running" },
  { id: "queued", label: "queued" },
  { id: "parked", label: "parked" },
  { id: "paused", label: "paused" },
];

const INITIAL_HISTORY = 20;
const MAX_HISTORY = 200;

export function CommandsPage() {
  const { state = "live", node, repo, outcome } = useSearch({ from: "/commands" });
  const { data: s } = useScopedSnapshot();
  const [showAll, setShowAll] = useState(false);
  if (!s) return null;

  const now = clusterNow(s);
  const live = s.commands
    .filter((c) => (state === "live" ? isLive(c) : c.state === COMMAND_STATE_FROM_SLUG[state]))
    .filter((c) => !node || c.nodeId === node)
    .filter((c) => !repo || c.repo === repo)
    .sort((a, b) => order(a.state) - order(b.state) || cmpBig(a.queuedAt, b.queuedAt));

  const scopedHistory = s.history.filter((c) => (!repo || c.repoId === repo) && (!node || c.nodeId === node));
  const outcomeState = outcome ? COMMAND_STATE_FROM_SLUG[outcome] : undefined;
  const ended = scopedHistory.filter((c) => outcomeState === undefined || c.state === outcomeState).reverse();
  const day = savings(scopedHistory);
  const times = compiled(scopedHistory).map((c) => c.seconds);

  const waiting = s.commands.filter((c) => c.state === CommandState.QUEUED);
  const reserved = s.nodes.reduce((n, x) => n + Number(x.reservedMemoryBytes), 0);
  const baseSearch: CommandsSearch = { state: state === "live" ? undefined : state, node, repo, outcome };
  const limit = showAll ? MAX_HISTORY : INITIAL_HISTORY;
  // The cluster's OOM total is counted from node samples and cannot be narrowed
  // to a node or repo; a filtered view counts the OOM-killed commands it shows.
  const oomKills = node || repo ? scopedHistory.filter((c) => c.failure === "oom").length : (s.totals24h?.oomKills ?? 0);
  const repoFailureCols = s.repos
    .filter((r) => scopedHistory.some((c) => c.repoId === r.id))
    .map((r) => ({ key: r.id, label: r.id, values: [failureRate(scopedHistory.filter((c) => c.repoId === r.id)) * 100] }));
  const historyToggleLabel = showAll
    ? `show latest ${INITIAL_HISTORY}`
    : ended.length > MAX_HISTORY
      ? `show latest ${MAX_HISTORY}`
      : `show all ${ended.length}`;

  return (
    <div className="space-y-6">
      <PageTitle
        title="Commands"
        sub="Live builds, tests and dev servers in their cgroups, plus the last 24 hours of completed runs and stats."
      />

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-6">
        <StatTile label="Running" value={now.running} tone="ember" foot={`${now.coresBusy.toFixed(0)} of ${now.cores} cores busy`} />
        <StatTile label="Queued" value={now.queued} foot={waiting[0]?.reason || "nothing waiting"} />
        <StatTile label="Parked ❄" value={now.parked} tone="frost" foot={`${bytes(now.swapUsed)} in swap`} />
        <StatTile label="Dev servers" value={now.servers} foot="no time limit" />
        <StatTile label="Guaranteed" value={bytes(reserved)} foot={`of ${bytes(now.memory)} RAM online`} />
        <StatTile label="Queue p95" value={duration(s.hourly[s.hourly.length - 1]?.queueWaitP95Seconds ?? 0)} foot="this hour" />
      </div>

      <div className="rise flex flex-wrap items-center gap-2">
        <div className="flex flex-wrap items-center gap-1.5">
          {STATES.map((x) => (
            <Chip key={x.id} active={state === x.id} search={{ ...baseSearch, state: x.id === "live" ? undefined : x.id }}>
              {x.label}
            </Chip>
          ))}
        </div>
        <span className="mx-1 hidden h-4 w-px bg-border sm:inline-block" />
        <div className="flex flex-wrap items-center gap-1.5">
          <Chip active={!node} search={{ ...baseSearch, node: undefined }}>
            every node
          </Chip>
          {s.nodes
            .filter((n) => n.online || n.id === node)
            .map((n) => (
              <Chip key={n.id} active={node === n.id} search={{ ...baseSearch, node: !n.online && node === n.id ? undefined : n.id }}>
                {n.hostname}
                {!n.online && " (offline)"}
              </Chip>
            ))}
          {node && !s.nodes.some((n) => n.id === node) && (
            <Chip active search={{ ...baseSearch, node: undefined }}>
              {node} ×
            </Chip>
          )}
        </div>
        {s.repos.length > 1 && (
          <>
            <span className="mx-1 hidden h-4 w-px bg-border sm:inline-block" />
            <div className="flex flex-wrap items-center gap-1.5">
              <Chip active={!repo} search={{ ...baseSearch, repo: undefined }}>
                every repo
              </Chip>
              {s.repos.map((r) => (
                <Chip key={r.id} active={repo === r.id} search={{ ...baseSearch, repo: r.id }}>
                  {r.id}
                </Chip>
              ))}
            </div>
          </>
        )}
      </div>

      <Card title={`${live.length} ${state === "live" ? "live" : state}`}>
        <LiveCommandTable s={s} commands={live} />
      </Card>

      <div className="space-y-3">
        <div className="rise flex flex-wrap items-center gap-2">
          <Chip active={!outcome} search={{ ...baseSearch, outcome: undefined }}>
            any outcome
          </Chip>
          <Chip active={outcome === "failed"} search={{ ...baseSearch, outcome: "failed" }}>
            ✕ failed
          </Chip>
          <Chip active={outcome === "succeeded"} search={{ ...baseSearch, outcome: "succeeded" }}>
            ✓ ok
          </Chip>
          <a href="#command-stats" className="ml-auto font-mono text-[11px] text-muted-foreground hover:text-primary">
            24h stats ↓
          </a>
        </div>

        <Card
          title={`${ended.length} ended`}
          sub="Last 24 hours, newest first"
          right={
            ended.length > INITIAL_HISTORY ? (
              <button
                type="button"
                onClick={() => setShowAll((v) => !v)}
                className="font-mono text-[11px] text-muted-foreground hover:text-primary"
              >
                {historyToggleLabel}
              </button>
            ) : undefined
          }
        >
          <HistoryTable s={s} rows={ended} limit={limit} />
        </Card>
      </div>

      <section id="command-stats" className="scroll-mt-16 space-y-4 pt-2">
        <header className="rise">
          <h2 className="display text-[22px] leading-none font-medium">Last 24 hours</h2>
          <p className="mt-1 text-[12.5px] text-muted-foreground">Outcomes, compile time saved, failure breakdown and build times across ended commands.</p>
        </header>

        <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-6">
          <StatTile label="Commands" value={compact(scopedHistory.length)} foot="ended, 24 h" />
          <StatTile label="Failed" value={percent(failureRate(scopedHistory))} foot={`${scopedHistory.filter((c) => c.state === CommandState.FAILED).length} commands`} />
          <StatTile label="Saved" value={duration(day.saved)} tone="ember" foot={`${day.ratio.toFixed(0)}× less compile time`} />
          <StatTile label="Build p50" value={duration(quantile(times, 0.5))} foot={`p95 ${duration(quantile(times, 0.95))}`} />
          <StatTile label="Parked" value={scopedHistory.reduce((n, c) => n + c.parks, 0)} unit="times" tone="frost" foot="frozen, swapped, resumed" />
          <StatTile
            label="OOM kills"
            value={oomKills > 0 ? <span className="text-critical">{oomKills}</span> : 0}
            tone={oomKills > 0 ? "default" : "good"}
            foot={oomKills > 0 ? "in last 24h" : "none in 24h"}
          />
        </div>

        <div className="grid gap-4 lg:grid-cols-3">
          <Card className="lg:col-span-2" title="Outcomes per hour">
            <Columns
              columns={perHour(s, scopedHistory).map((h) => ({ key: String(h.hour), label: String(new Date(Number(h.hour)).getHours()).padStart(2, "0"), values: [h.ok, h.failed] }))}
              series={[
                { id: "ok", label: "ok", color: "var(--series-1)" },
                { id: "failed", label: "failed", color: "var(--series-8)" },
              ]}
              format={(v) => compact(v)}
              labelEvery={3}
            />
          </Card>
          <Card title="Why they failed">
            <FailureBars rows={scopedHistory} />
          </Card>
        </div>

        <div className="grid gap-4 lg:grid-cols-2">
          <Card title="Failure rate by repo">
            {repoFailureCols.length === 0 ? (
              <Empty>No repo commands in the last 24 hours.</Empty>
            ) : (
              <Columns
                columns={repoFailureCols}
                series={[{ id: "rate", label: "failed", color: "var(--series-8)" }]}
                format={(v) => `${v.toFixed(0)}%`}
                height={150}
              />
            )}
          </Card>
          <Card title="Build time by repo, p50" sub="Warm forks; the cold build is the faint mark">
            <RepoTimes repos={s.repos} history={scopedHistory} />
          </Card>
        </div>

        <Card title="Queue wait, p95" sub="How long commands waited for room, per hour, last 24 hours">
          <Columns
            columns={s.hourly.map((h) => ({ key: String(h.hour), label: String(new Date(Number(h.hour)).getHours()).padStart(2, "0"), values: [h.queueWaitP95Seconds] }))}
            series={[{ id: "wait", label: "p95 wait", color: "var(--series-2)" }]}
            format={(v) => `${v.toFixed(0)}s`}
            labelEvery={2}
            height={140}
          />
        </Card>
      </section>
    </div>
  );
}

function Chip({ active, search, children }: { active: boolean; search: CommandsSearch; children: ReactNode }) {
  return (
    <Link
      to="/commands"
      search={search}
      className={`rounded-full border px-3 py-1 font-mono text-[11px] ${active ? "border-primary bg-ember-wash text-foreground" : "border-border text-muted-foreground hover:border-border-strong"}`}
    >
      {children}
    </Link>
  );
}

function order(s: CommandState): number {
  const map: Record<CommandState, number> = {
    [CommandState.RUNNING]: 0,
    [CommandState.PARKED]: 1,
    [CommandState.PAUSED]: 2,
    [CommandState.QUEUED]: 3,
    [CommandState.SUCCEEDED]: 4,
    [CommandState.FAILED]: 5,
    [CommandState.CANCELLED]: 6,
    [CommandState.UNSPECIFIED]: 7,
  };
  return map[s] ?? 7;
}

function perHour(s: Snapshot, rows: FinishedCommand[]) {
  return s.hourly.map((h) => {
    const inHour = rows.filter((c) => c.endedAt >= h.hour && c.endedAt < h.hour + 3600_000n);
    return {
      hour: h.hour,
      ok: inHour.filter((c) => c.state === CommandState.SUCCEEDED).length,
      failed: inHour.filter((c) => c.state === CommandState.FAILED).length,
    };
  });
}

function FailureBars({ rows }: { rows: FinishedCommand[] }) {
  const failed = rows.filter((c) => c.failure);
  if (failed.length === 0) return <Empty>No failures in the last 24 hours.</Empty>;
  const kinds = (Object.keys(FAILURE) as (keyof typeof FAILURE)[]).map((k) => ({ k, n: failed.filter((c) => c.failure === k).length })).sort((a, b) => b.n - a.n);
  const max = Math.max(1, ...kinds.map((x) => x.n));
  return (
    <ul className="space-y-3">
      {kinds.map(({ k, n }) => (
        <li key={k}>
          <div className="mb-1 flex justify-between text-[11.5px]">
            <span className="text-muted-foreground">{FAILURE[k]}</span>
            <span className="num font-mono text-foreground">{n}</span>
          </div>
          <div className="h-2 rounded-r-[4px] bg-[var(--series-8)]" style={{ width: `${(n / max) * 100}%`, minWidth: n ? 3 : 0 }} />
        </li>
      ))}
    </ul>
  );
}

function RepoTimes({ repos, history }: { repos: Snapshot["repos"]; history: FinishedCommand[] }) {
  const rows = repos
    .map((r) => {
      const c = compiled(history.filter((x) => x.repoId === r.id));
      return { r, count: c.length, p50: quantile(c.map((x) => x.seconds), 0.5), cold: quantile(c.map((x) => x.coldSeconds), 0.5) };
    })
    .filter((x) => x.count > 0);
  if (rows.length === 0) return <Empty>No compiled builds in the last 24 hours.</Empty>;
  const max = Math.max(1, ...rows.map((x) => x.cold));
  return (
    <ul className="space-y-3">
      {rows.map(({ r, p50, cold }) => (
        <li key={r.id}>
          <div className="mb-1 flex justify-between text-[11.5px]">
            <span className="font-mono text-foreground">{r.id}</span>
            <span className="num font-mono text-muted-foreground">
              <span className="text-foreground">{duration(p50)}</span> · cold {duration(cold)}
            </span>
          </div>
          <div className="relative h-2">
            <div className="absolute inset-y-0 left-0 rounded-r-[4px] border border-border-strong" style={{ width: `${(cold / max) * 100}%` }} />
            <div className="absolute inset-y-0 left-0 rounded-r-[4px] bg-primary" style={{ width: `max(3px, ${(p50 / max) * 100}%)` }} />
          </div>
        </li>
      ))}
    </ul>
  );
}
