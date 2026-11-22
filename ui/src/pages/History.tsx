import { Link, useSearch } from "@tanstack/react-router";
import type { FinishedCommand, Snapshot } from "../api";
import { FAILURE, HistoryTable } from "../components/CommandTable";
import { Columns } from "../components/charts/Columns";
import { Card, StatTile } from "../components/ui/Card";
import { compact, duration, percent, quantile } from "../lib/format";
import { useScopedSnapshot } from "../lib/project";
import { compiled, failureRate, savings } from "../lib/stats";
import { PageTitle } from "./Nodes";

export function HistoryPage() {
  const { repo, outcome } = useSearch({ from: "/history" });
  const { data: s } = useScopedSnapshot();
  if (!s) return null;
  const rows = s.history.filter((c) => (!repo || c.repoId === repo) && (!outcome || c.state === outcome)).reverse();
  const scoped = s.history.filter((c) => !repo || c.repoId === repo);
  const day = savings(scoped);
  const times = compiled(scoped).map((c) => c.seconds);

  return (
    <div className="space-y-6">
      <PageTitle title="History" sub="Every command that ended in the last 24 hours, kept by the controller in SQLite." />

      <div className="rise flex flex-wrap items-center gap-2">
        <Chip active={!repo} to={{ outcome }}>
          every repo
        </Chip>
        {s.repos.map((r) => (
          <Chip key={r.id} active={repo === r.id} to={{ repo: r.id, outcome }}>
            {r.id}
          </Chip>
        ))}
        <span className="mx-2 h-4 w-px bg-border" />
        <Chip active={!outcome} to={{ repo }}>
          any outcome
        </Chip>
        <Chip active={outcome === "failed"} to={{ repo, outcome: "failed" }}>
          ✕ failed
        </Chip>
        <Chip active={outcome === "succeeded"} to={{ repo, outcome: "succeeded" }}>
          ✓ ok
        </Chip>
      </div>

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-6">
        <StatTile label="Commands" value={compact(scoped.length)} foot="ended, 24 h" />
        <StatTile label="Failed" value={percent(failureRate(scoped))} foot={`${scoped.filter((c) => c.state === "failed").length} commands`} />
        <StatTile label="Saved" value={duration(day.saved)} tone="ember" foot={`${day.ratio.toFixed(0)}× less compile time`} />
        <StatTile label="Build p50" value={duration(quantile(times, 0.5))} foot={`p95 ${duration(quantile(times, 0.95))}`} />
        <StatTile label="Parked" value={scoped.reduce((n, c) => n + c.parks, 0)} unit="times" tone="frost" foot="frozen, swapped, resumed" />
        <StatTile label="OOM kills" value={0} tone="good" foot="none, ever" />
      </div>

      <div className="grid gap-4 lg:grid-cols-3">
        <Card className="lg:col-span-2" title="Outcomes per hour">
          <Columns
            columns={perHour(s, scoped).map((h) => ({ key: String(h.hour), label: String(new Date(h.hour).getHours()).padStart(2, "0"), values: [h.ok, h.failed] }))}
            series={[
              { id: "ok", label: "ok", color: "var(--series-1)" },
              { id: "failed", label: "failed", color: "var(--series-8)" },
            ]}
            format={(v) => compact(v)}
            labelEvery={3}
          />
        </Card>
        <Card title="Why they failed">
          <FailureBars rows={scoped} />
        </Card>
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        <Card title="Failure rate by repo">
          <Columns
            columns={s.repos.map((r) => ({ key: r.id, label: r.id, values: [failureRate(s.history.filter((c) => c.repoId === r.id)) * 100] }))}
            series={[{ id: "rate", label: "failed", color: "var(--series-8)" }]}
            format={(v) => `${v.toFixed(0)}%`}
            height={150}
          />
        </Card>
        <Card title="Build time by repo, p50" sub="Warm forks; the cold build is the faint mark">
          <RepoTimes s={s} />
        </Card>
      </div>

      <Card title={`${rows.length} commands`}>
        <HistoryTable s={s} rows={rows} limit={100} />
      </Card>
    </div>
  );
}

function Chip({ active, to, children }: { active: boolean; to: { repo?: string; outcome?: "failed" | "succeeded" }; children: React.ReactNode }) {
  return (
    <Link
      to="/history"
      search={to}
      className={`rounded-full border px-3 py-1 font-mono text-[11px] ${active ? "border-primary bg-ember-wash text-foreground" : "border-border text-muted-foreground hover:border-border-strong"}`}
    >
      {children}
    </Link>
  );
}

function perHour(s: Snapshot, rows: FinishedCommand[]) {
  return s.hourly.map((h) => {
    const inHour = rows.filter((c) => c.endedAt >= h.hour && c.endedAt < h.hour + 3600_000);
    return { hour: h.hour, ok: inHour.filter((c) => c.state === "succeeded").length, failed: inHour.filter((c) => c.state === "failed").length };
  });
}

function FailureBars({ rows }: { rows: FinishedCommand[] }) {
  const failed = rows.filter((c) => c.failure);
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

function RepoTimes({ s }: { s: Snapshot }) {
  const rows = s.repos.map((r) => {
    const c = compiled(s.history.filter((x) => x.repoId === r.id));
    return { r, p50: quantile(c.map((x) => x.seconds), 0.5), cold: quantile(c.map((x) => x.coldSeconds), 0.5) };
  });
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
