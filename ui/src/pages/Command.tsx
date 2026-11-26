import { useQuery } from "@tanstack/react-query";
import { Link, useParams, useSearch } from "@tanstack/react-router";
import { type ReactNode, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { api, CommandState, type Command, type CommandEvent, type CommandLimits, type FinishedCommand, type Snapshot } from "../api";
import { Meter } from "../components/charts/Bars";
import { SpeedBars } from "../components/charts/SpeedBars";
import { actsFor, ByHandButtons, ByHandForm, InterventionsCard, type ByHandAction } from "../components/ByHand";
import { CommandBlock } from "../components/CommandBlock";
import { Crumbs } from "../components/Crumbs";
import { commandRuntime, FAILURE } from "../components/CommandTable";
import { ContinuousProfilePanel } from "../components/ContinuousProfilePanel";
import { LiveDiagnosticsPanel } from "../components/LiveDiagnosticsPanel";
import { ProcessSamplePanel, type SampleTrigger } from "../components/ProcessSamplePanel";
import { Card, StatTile } from "../components/Panel";
import { BenchTag, KindTag, RuntimeBadge, StateBadge } from "../components/StateBadges";
import { nodeName, useSnapshot } from "../lib/data";
import { endedExit, endedHow } from "../lib/exit";
import { atEnd } from "../lib/follow";
import { ago, bytes, duration, hhmmss, limit, speedup } from "../lib/format";
import { isLive } from "../lib/live";
import { searchLog, type LogMatch } from "../lib/logsearch";
import { NotFound } from "./NotFound";

export function CommandPage() {
  const { id } = useParams({ from: "/commands/$id" });
  const search = useSearch({ from: "/commands/$id" });
  const { data: s } = useSnapshot();
  if (!s) return null;
  const live = s.commands.find((c) => c.id === id);
  const done = s.history.find((c) => c.id === id);
  if (!live && !done) return <NotFound />;
  return <CommandView s={s} live={live ?? null} done={done ?? null} id={id} autoSample={Boolean(search.sample)} autoSamplePid={search.pid} />;
}

function CommandView({
  s,
  live,
  done,
  id,
  autoSample,
  autoSamplePid,
}: {
  s: Snapshot;
  live: Command | null;
  done: FinishedCommand | null;
  id: string;
  autoSample: boolean;
  autoSamplePid?: number;
}) {
  const samplePanelRef = useRef<HTMLDivElement>(null);
  const [sampleTrigger, setSampleTrigger] = useState<SampleTrigger | null>(() => (autoSample ? { seq: 1, pid: autoSamplePid } : null));
  const triggerSample = (pid?: number) => {
    setSampleTrigger((prev) => ({ seq: (prev?.seq ?? 0) + 1, pid }));
    requestAnimationFrame(() => samplePanelRef.current?.scrollIntoView({ behavior: "smooth", block: "nearest" }));
  };
  useEffect(() => {
    if (autoSample) {
      requestAnimationFrame(() => samplePanelRef.current?.scrollIntoView({ behavior: "smooth", block: "nearest" }));
    }
  }, [autoSample, id]);

  const now = Number(s.now);
  const argv = live?.argv ?? done?.argv ?? "";
  const state = live?.state ?? done?.state ?? CommandState.SUCCEEDED;
  const workspaceId = live?.workspaceId ?? done?.workspaceId ?? "";
  const sessionId = live?.sessionId ?? done?.sessionId ?? "";
  const nodeId = live?.nodeId ?? done?.nodeId ?? "";
  const started = live ? Number(live.startedAt ?? live.queuedAt) : Number(done?.endedAt ?? s.now) - (done?.seconds ?? 0) * 1000;
  const ended = live?.endedAt !== undefined ? Number(live.endedAt) : done ? Number(done.endedAt) : null;
  const took = ((ended ?? now) - started) / 1000;
  const cold = live?.coldSeconds ?? done?.coldSeconds ?? null;
  const rebuilt = live?.rebuilt ? live.rebuilt : done?.rebuilt !== undefined && done ? { units: done.rebuilt, of: done.of ?? 0 } : null;
  const workspace = s.workspaces.find((w) => w.id === workspaceId);
  const repoId = done?.repoId || live?.repo || null;
  const canSample = Boolean(live && isLive(live) && live.state !== CommandState.QUEUED);
  const [act, setAct] = useState<ByHandAction | null>(null);
  const activeAct = live && act && actsFor(live).includes(act) ? act : null;

  const kind = live?.kind ?? done?.kind ?? "build";
  const runtime = commandRuntime(s, { runtime: live?.runtime || done?.runtime || "", workspaceId });
  const timeLimit = formatTimeLimit(kind, live?.limits);
  const unitLabel = repoUnitLabel(s, repoId);

  const stats: ReactNode[] = [
    <StatTile key="took" label={ended ? "Took" : "Running for"} value={duration(took)} tone="ember" />,
  ];
  if (cold !== null) {
    stats.push(
      <StatTile key="cold" label="Cold" value={duration(cold)} foot="same command from nothing" />,
      <StatTile key="speedup" label="Speedup" value={speedup(cold, took)} tone="ember" />,
      <StatTile key="units" label={unitLabel} value={rebuilt ? rebuilt.units : "—"} unit={rebuilt ? `/ ${rebuilt.of} rebuilt` : undefined} />,
    );
  } else if (live) {
    stats.push(<StatTile key="cpu" label="CPU" value={live.cpuCores.toFixed(2)} unit="cores" />);
    if (kind === "server" || live.url) {
      stats.push(
        <StatTile
          key="serves"
          label="Serves"
          value={live.url ? `:${live.url.split(":").pop()}` : "—"}
          foot={live.url?.replace("http://", "")}
        />,
      );
    }
    if (live.limits) {
      stats.push(<StatTile key="limit" label="Time limit" value={timeLimit.value} unit={timeLimit.unit} foot={timeLimit.foot} />);
    }
  } else if (done) {
    const exitDisplay = endedExit(done);
    const exitFoot = done.failure
      ? (FAILURE[done.failure] ?? done.failure)
      : done.state === CommandState.SUCCEEDED
        ? "succeeded"
        : done.state === CommandState.CANCELLED
          ? "cancelled"
          : undefined;
    stats.push(
      <StatTile
        key="exit"
        label="Exit code"
        value={done.state === CommandState.FAILED ? <span className="text-critical">{exitDisplay}</span> : exitDisplay}
        tone={done.state === CommandState.SUCCEEDED ? "good" : "default"}
        foot={exitFoot}
      />,
      <StatTile key="ended" label="Ended" value={ago(done.endedAt, now)} foot={hhmmss(done.endedAt)} />,
    );
    if (done.queuedSeconds > 0) {
      stats.push(<StatTile key="queued" label="Queued" value={duration(done.queuedSeconds)} foot="waited for room" />);
    }
  }
  stats.push(
    <StatTile key="memory" label="Memory" value={bytes(live?.memoryBytes ?? done?.peakMemoryBytes ?? 0)} foot={live ? "now" : "peak"} />,
    <StatTile key="parked" label="Parked" value={done?.parks ?? live?.timeline.filter((e) => e.what === "parked").length ?? 0} unit="times" tone="frost" />,
  );

  const statGridCols =
    stats.length <= 4 ? "xl:grid-cols-4" : stats.length === 5 ? "xl:grid-cols-5" : "xl:grid-cols-6";

  return (
    <div className="space-y-6">
      <Crumbs
        section={{ label: "commands", to: "/commands" }}
        project={workspace?.project ?? live?.project ?? done?.project}
        workspaceId={workspace ? workspaceId : undefined}
        repoId={repoId || null}
        here={id}
      />
      <div className="rise">
        <div className="flex flex-wrap items-center gap-3">
          <StateBadge state={state} />
          <KindTag kind={kind} />
          {runtime && <RuntimeBadge runtime={runtime} />}
          {(live?.bench ?? done?.bench) && <BenchTag />}
          {live && isLive(live) && (
            <div className="flex flex-wrap items-center gap-2 sm:ml-auto">
              <ByHandButtons command={live} open={activeAct} onOpen={setAct} />
              {canSample && (
                <Button
                  type="button"
                  size="sm"
                  onClick={() => triggerSample(0)}
                  title="Sample stacks, syscalls, open files, sockets, locks, cgroup PSI & PMU counters (like macOS Activity Monitor Sample)"
                  className="gap-1.5 font-mono text-[11.5px] font-semibold shadow-sm"
                >
                  <span aria-hidden>◎</span>
                  Sample Process
                </Button>
              )}
            </div>
          )}
        </div>
        {live && activeAct && <ByHandForm key={`${id}-${activeAct}`} command={live} act={activeAct} onDone={() => setAct(null)} />}
        <CommandBlock key={id} argv={argv} args={live?.args} />
        <p className="mt-2 text-[13px] text-muted-foreground">
          {sessionId && (
            <>
              session{" "}
              <Link to="/sessions/$id" params={{ id: sessionId }} className="font-mono text-foreground hover:text-primary">
                {sessionId}
              </Link>
              ,{" "}
            </>
          )}
          in{" "}
          {workspace ? (
            <Link to="/workspaces/$id" params={{ id: workspaceId }} className="font-mono text-foreground hover:text-primary">
              {workspaceId}
            </Link>
          ) : (
            <span className="font-mono">{workspaceId}</span>
          )}{" "}
          on{" "}
          <Link to="/nodes/$hostname" params={{ hostname: nodeName(s, nodeId) }} className="font-mono text-foreground hover:text-primary">
            {nodeName(s, nodeId)}
          </Link>
          {done && <> · {done.agent}</>}
          {live?.url && (
            <>
              {" "}
              ·{" "}
              <a href={live.url} target="_blank" rel="noreferrer" className="font-mono text-primary hover:underline">
                {live.url} ↗
              </a>
            </>
          )}
        </p>
        {live?.reason && <p className={`mt-3 inline-block rounded-lg px-3 py-1.5 font-mono text-[11.5px] ${state === CommandState.PARKED || state === CommandState.PAUSED ? "bg-frost-wash text-frost" : "bg-muted text-muted-foreground"}`}>{live.reason}</p>}
        {done?.failure && <p className="mt-3 inline-block rounded-lg bg-[color-mix(in_oklab,var(--critical)_12%,transparent)] px-3 py-1.5 font-mono text-[11.5px] text-critical">{FAILURE[done.failure] ?? done.failure} · {endedHow(done)}</p>}
      </div>

      <div className={`grid grid-cols-2 gap-3 sm:grid-cols-3 ${statGridCols}`}>{stats}</div>

      <InterventionsCard s={s} id={id} interventions={live ? live.interventions : null} />

      {cold !== null && (
        <Card title="Against a cold build">
          <SpeedBars rows={[{ id, argv, args: live?.args, sub: "this run", seconds: took, coldSeconds: cold, failed: state === CommandState.FAILED }]} />
        </Card>
      )}

      {/* Activity Monitor-style one-click Linux process sample: stacks, syscalls, open files, sockets, locks, cgroup PSI & PMU. */}
      {canSample && <ProcessSamplePanel key={`sample-${id}`} id={id} trigger={sampleTrigger} panelRef={samplePanelRef} />}

      {/* Opt-in Linux process inspection & active tracing (strace, perf, stacks, eBPF): zero overhead until clicked. */}
      {live && isLive(live) && <LiveDiagnosticsPanel key={`diag-${id}`} id={id} onSamplePid={(pid) => triggerSample(pid)} />}

      {/* Always-on OpenTelemetry eBPF continuous profile: queryable both while running and after exit. */}
      <ContinuousProfilePanel key={`prof-${id}`} id={id} live={!!live && isLive(live)} />

      {/* minmax(0,…) keeps long log lines from widening the column past the page: the log scrolls in its own box. */}
      {live ? (
        <div className="grid grid-cols-[minmax(0,1fr)] gap-4 lg:grid-cols-[minmax(0,1fr)_320px]">
          <LogCard id={id} live={isLive(live)} />
          <div className="space-y-4">
            {live.limits && (
              <Card title="Limits" sub="cgroup v2, set at admission">
                <dl className="space-y-3 text-[11.5px]">
                  <Limit label="cpu.weight" value={String(live.limits.cpuWeight)} />
                  <Limit label="memory.low" value={limit(live.limits.memoryLowBytes)} />
                  <Limit label="memory.high" value={limit(live.limits.memoryHighBytes)} />
                  <Limit label="memory.max" value="none — park, don't kill" />
                  {live.limits.softLimitSeconds > 0n && <Limit label="soft time limit" value={duration(Number(live.limits.softLimitSeconds))} />}
                  {live.limits.hardLimitSeconds > 0n && <Limit label="hard time limit" value={duration(Number(live.limits.hardLimitSeconds))} />}
                </dl>
                {live.limits.memoryHighBytes > 0n && (
                  <div className="mt-4">
                    <div className="mb-1 flex justify-between font-mono text-[10.5px] text-muted-foreground">
                      <span>{bytes(live.memoryBytes)} used</span>
                      <span>high {bytes(live.limits.memoryHighBytes)}</span>
                    </div>
                    <Meter value={Number(live.memoryBytes) / Number(live.limits.memoryHighBytes)} marker={Number(live.limits.memoryLowBytes) / Number(live.limits.memoryHighBytes)} />
                  </div>
                )}
              </Card>
            )}
            <Card title="Timeline">
              <Timeline events={live.timeline} />
            </Card>
          </div>
        </div>
      ) : (
        <LogCard id={id} live={false} />
      )}
    </div>
  );
}

export function repoUnitLabel(s: Snapshot, repoId: string | null): string {
  if (!repoId) return "Units";
  const repo = s.repos.find((r) => r.id === repoId);
  if (!repo?.language) return "Units";
  const lang = repo.language.toLowerCase();
  if (lang === "rust") return "Crates";
  if (lang === "go" || lang === "typescript" || lang === "javascript" || lang === "python") return "Packages";
  return "Units";
}

export function formatTimeLimit(kind: string, limits?: CommandLimits): { value: string; unit?: string; foot?: string } {
  if (!limits) {
    return { value: "—", foot: "not recorded in history" };
  }
  const soft = Number(limits.softLimitSeconds);
  const hard = Number(limits.hardLimitSeconds);
  if (soft > 0 && hard > 0) {
    return { value: duration(soft), unit: `/ ${duration(hard)}`, foot: "soft / hard" };
  }
  if (soft > 0) {
    return { value: duration(soft), foot: "soft limit" };
  }
  if (hard > 0) {
    return { value: duration(hard), foot: "hard limit" };
  }
  return {
    value: "none",
    foot: kind === "server" ? "dev servers run until released" : undefined,
  };
}

function Limit({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex justify-between gap-3">
      <dt className="font-mono text-muted-foreground">{label}</dt>
      <dd className="num font-mono text-foreground">{value}</dd>
    </div>
  );
}

function Timeline({ events }: { events: CommandEvent[] }) {
  const glyph: Record<string, string> = { queued: "◌", admitted: "▸", parked: "❄", thawed: "☀", paused: "⏸", resumed: "▶", "isolation lost": "⚠", exited: "■", cancelled: "⊘" };
  return (
    <ol className="space-y-3">
      {events.map((e, i) => (
        <li key={i} className="flex gap-2.5">
          <div className="flex flex-col items-center">
            <span className={`w-3.5 text-center text-[11px] leading-[1.35] ${e.what === "parked" || e.what === "paused" ? "text-frost" : e.what === "thawed" || e.what === "resumed" ? "text-primary" : e.what === "isolation lost" ? "text-critical" : "text-muted-foreground"}`}>{glyph[e.what] ?? "•"}</span>
            {i < events.length - 1 && <span aria-hidden className="mt-1 w-px flex-1 bg-border" />}
          </div>
          <div className="min-w-0 flex-1 pb-0.5">
            <div className="font-mono text-[11px] text-foreground">
              {e.what} <span className="text-faint">{hhmmss(e.at)}</span>
            </div>
            {e.detail && <div className="text-[11px] text-muted-foreground">{e.detail}</div>}
          </div>
        </li>
      ))}
    </ol>
  );
}

const EMPTY_LINES: string[] = [];

function LogCard({ id, live }: { id: string; live: boolean }) {
  const log = useQuery({ queryKey: ["log", id], queryFn: () => api.commandLog(id), refetchInterval: live ? 1000 : false });
  const [query, setQuery] = useState("");
  const lines = log.data?.lines ?? EMPTY_LINES;
  const matches = useMemo(() => searchLog(lines, query), [lines, query]);
  const searching = query.trim() !== "";
  return (
    <Card
      title="Output"
      sub={[
        log.isLoading ? "Loading" : log.isError ? "Unavailable" : searching ? `${matches.length} of ${lines.length} lines match` : live ? "Live" : "Captured",
        log.data?.cut ? `the last ${lines.length} lines that fit` : null,
      ]
        .filter(Boolean)
        .join(" · ")}
      right={
        <Input
          type="search"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="search output"
          aria-label="Search output"
          className="h-7 w-full rounded-full bg-background/40 font-mono text-[11px] placeholder:text-faint sm:w-60"
        />
      }
    >
      {log.isLoading ? (
        <div className={`${logBox} text-faint`}>loading output…</div>
      ) : log.isError ? (
        <div className={`${logBox} text-critical`}>Cannot load output: {String(log.error)}</div>
      ) : !live && lines.length === 0 ? (
        <div className={`${logBox} text-faint`}>No output captured.</div>
      ) : searching ? (
        <LogMatches lines={lines} matches={matches} />
      ) : (
        <LogView lines={lines} live={live} />
      )}
    </Card>
  );
}

const logBox = "max-h-[520px] overflow-auto rounded-lg border border-border bg-background/60 p-3 font-mono text-[11.5px] leading-[1.7]";

function LogView({ lines, live }: { lines: string[]; live: boolean }) {
  const box = useRef<HTMLDivElement>(null);
  const following = useRef(true);
  // Scroll only the log's own box (scrollIntoView would scroll the page too),
  // and only while the reader sits at its end.
  useLayoutEffect(() => {
    const el = box.current;
    if (el && live && following.current) el.scrollTop = el.scrollHeight;
  }, [lines.length, live]);
  return (
    <div ref={box} onScroll={(e) => (following.current = atEnd(e.currentTarget))} className={logBox}>
      {lines.map((l, i) => (
        <div key={i} className={`whitespace-pre ${tone(l)}`}>
          {l || " "}
        </div>
      ))}
      {live && <div className="ember-pulse text-primary">▍</div>}
    </div>
  );
}

/** Only the lines that match, numbered by where they sit in the log, matches marked. */
function LogMatches({ lines, matches }: { lines: string[]; matches: LogMatch[] }) {
  if (matches.length === 0) return <div className={`${logBox} text-faint`}>No line matches.</div>;
  return (
    <div className={logBox}>
      {matches.map(({ line, ranges }) => {
        const text = lines[line] ?? "";
        const parts: ReactNode[] = [];
        let at = 0;
        for (const [a, b] of ranges) {
          parts.push(text.slice(at, a), <mark key={a} className="rounded-sm bg-ember-wash text-primary">{text.slice(a, b)}</mark>);
          at = b;
        }
        parts.push(text.slice(at));
        return (
          <div key={line} className={`flex whitespace-pre ${tone(text)}`}>
            <span className="w-12 shrink-0 pr-3 text-right text-faint select-none">{line + 1}</span>
            <span>{parts}</span>
          </div>
        );
      })}
    </div>
  );
}

export function tone(line: string): string {
  if (/^\s*(error|FAIL|--- FAIL|thread .* panicked)/.test(line) || line.includes(" FAIL ")) return "text-critical";
  if (/^\s*Compiling/.test(line)) return "text-muted-foreground";
  if (/^\s*(Finished|Summary|ok )/.test(line) || line.includes(" PASS ")) return "text-good-ink";
  if (/^\s*warning/.test(line)) return "text-warning-ink";
  return "text-foreground/80";
}
