import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "@tanstack/react-router";
import { type ReactNode, useLayoutEffect, useRef, useState } from "react";
import { api, type Command, type CommandEvent, type FinishedCommand, type Snapshot } from "../api";
import { Meter } from "../components/charts/Bars";
import { SpeedBars } from "../components/charts/SpeedBars";
import { CommandBlock } from "../components/CommandBlock";
import { Crumbs } from "../components/Crumbs";
import { FAILURE } from "../components/CommandTable";
import { Card, StatTile } from "../components/ui/Card";
import { BenchTag, KindTag, StateBadge } from "../components/ui/State";
import { nodeName, useSnapshot } from "../lib/data";
import { atEnd } from "../lib/follow";
import { bytes, duration, hhmmss, limit, speedup } from "../lib/format";
import { isLive } from "../lib/live";
import { searchLog, type LogMatch } from "../lib/logsearch";
import { NotFound } from "./NotFound";

export function CommandPage() {
  const { id } = useParams({ from: "/commands/$id" });
  const { data: s } = useSnapshot();
  if (!s) return null;
  const live = s.commands.find((c) => c.id === id);
  const done = s.history.find((c) => c.id === id);
  if (!live && !done) return <NotFound />;
  return <CommandView s={s} live={live ?? null} done={done ?? null} id={id} />;
}

function CommandView({ s, live, done, id }: { s: Snapshot; live: Command | null; done: FinishedCommand | null; id: string }) {
  const now = Date.parse(s.now);
  const argv = live?.argv ?? done?.argv ?? "";
  const state = live?.state ?? done?.state ?? "succeeded";
  const workspaceId = live?.workspaceId ?? done?.workspaceId ?? "";
  const sessionId = live?.sessionId ?? done?.sessionId ?? "";
  const nodeId = live?.nodeId ?? done?.nodeId ?? "";
  const started = live ? Date.parse(live.startedAt ?? live.queuedAt) : (done?.endedAt ?? now) - (done?.seconds ?? 0) * 1000;
  const ended = live?.endedAt ? Date.parse(live.endedAt) : done ? done.endedAt : null;
  const took = ((ended ?? now) - started) / 1000;
  const cold = live?.coldSeconds ?? done?.coldSeconds ?? null;
  const rebuilt = live?.rebuilt ? live.rebuilt : done?.rebuilt !== null && done ? { units: done.rebuilt ?? 0, of: done.of ?? 0 } : null;
  const workspace = s.workspaces.find((w) => w.id === workspaceId);
  const repoId = done?.repoId ?? workspace?.repoId;

  return (
    <div className="space-y-6">
      <Crumbs repoId={repoId} workspaceId={workspace ? workspaceId : undefined} here={id} />
      <div className="rise">
        <div className="flex flex-wrap items-center gap-3">
          <StateBadge state={state} />
          <KindTag kind={live?.kind ?? done?.kind ?? "build"} />
          {(live?.bench ?? done?.bench) && <BenchTag />}
        </div>
        <CommandBlock key={id} argv={argv} />
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
        {live?.reason && <p className={`mt-3 inline-block rounded-lg px-3 py-1.5 font-mono text-[11.5px] ${state === "parked" ? "bg-frost-wash text-frost" : "bg-muted text-muted-foreground"}`}>{live.reason}</p>}
        {done?.failure && <p className="mt-3 inline-block rounded-lg bg-[color-mix(in_oklab,var(--critical)_12%,transparent)] px-3 py-1.5 font-mono text-[11.5px] text-critical">{FAILURE[done.failure]} · exit {done.exitCode ?? "signal"}</p>}
      </div>

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-6">
        <StatTile label={ended ? "Took" : "Running for"} value={duration(took)} tone="ember" />
        {cold !== null ? (
          <>
            <StatTile label="Cold" value={duration(cold)} foot="same command from nothing" />
            <StatTile label="Speedup" value={speedup(cold, took)} tone="ember" />
            <StatTile label="Crates" value={rebuilt ? rebuilt.units : "—"} unit={rebuilt ? `/ ${rebuilt.of} rebuilt` : undefined} />
          </>
        ) : (
          <>
            <StatTile label="CPU" value={(live?.cpuCores ?? 0).toFixed(2)} unit="cores" />
            <StatTile label="Serves" value={live?.url ? `:${live.url.split(":").pop()}` : "—"} foot={live?.url?.replace("http://", "")} />
            <StatTile label="Time limit" value="none" foot="dev servers run until released" />
          </>
        )}
        <StatTile label="Memory" value={bytes(live?.memoryBytes ?? done?.peakMemoryBytes ?? 0)} foot={live ? "now" : "peak"} />
        <StatTile label="Parked" value={done?.parks ?? live?.timeline.filter((e) => e.what === "parked").length ?? 0} unit="times" tone="frost" />
      </div>

      {cold !== null && (
        <Card title="Against a cold build">
          <SpeedBars rows={[{ id, argv, sub: "this run", seconds: took, coldSeconds: cold, failed: state === "failed" }]} />
        </Card>
      )}

      {/* minmax(0,…) keeps long log lines from widening the column past the page: the log scrolls in its own box. */}
      <div className="grid grid-cols-[minmax(0,1fr)] gap-4 lg:grid-cols-[minmax(0,1fr)_320px]">
        <LogCard id={id} live={!!live && isLive(live)} />
        <div className="space-y-4">
          {live && (
            <Card title="Limits" sub="cgroup v2, set at admission">
              <dl className="space-y-3 text-[11.5px]">
                <Limit label="cpu.weight" value={String(live.limits.cpuWeight)} />
                <Limit label="memory.low" value={limit(live.limits.memoryLowBytes)} />
                <Limit label="memory.high" value={limit(live.limits.memoryHighBytes)} />
                <Limit label="memory.max" value="none — park, don't kill" />
              </dl>
              {live.limits.memoryHighBytes > 0 && (
                <div className="mt-4">
                  <div className="mb-1 flex justify-between font-mono text-[10.5px] text-muted-foreground">
                    <span>{bytes(live.memoryBytes)} used</span>
                    <span>high {bytes(live.limits.memoryHighBytes)}</span>
                  </div>
                  <Meter value={live.memoryBytes / live.limits.memoryHighBytes} marker={live.limits.memoryLowBytes / live.limits.memoryHighBytes} />
                </div>
              )}
            </Card>
          )}
          {live && (
            <Card title="Timeline">
              <Timeline events={live.timeline} />
            </Card>
          )}
        </div>
      </div>
    </div>
  );
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
  const glyph: Record<CommandEvent["what"], string> = { queued: "◌", admitted: "▸", parked: "❄", thawed: "☀", "isolation lost": "⚠", exited: "■", cancelled: "⊘" };
  return (
    <ol className="relative ml-1 space-y-3 border-l border-border pl-4">
      {events.map((e, i) => (
        <li key={i} className="relative">
          <span className={`absolute -left-[22px] top-0 w-3 bg-card text-center text-[11px] ${e.what === "parked" ? "text-frost" : e.what === "thawed" ? "text-primary" : e.what === "isolation lost" ? "text-critical" : "text-muted-foreground"}`}>{glyph[e.what]}</span>
          <div className="font-mono text-[11px] text-foreground">
            {e.what} <span className="text-faint">{hhmmss(Date.parse(e.at))}</span>
          </div>
          <div className="text-[11px] text-muted-foreground">{e.detail}</div>
        </li>
      ))}
    </ol>
  );
}

function LogCard({ id, live }: { id: string; live: boolean }) {
  const log = useQuery({ queryKey: ["log", id], queryFn: () => api.commandLog(id), refetchInterval: live ? 1000 : false });
  const [query, setQuery] = useState("");
  const lines = log.data?.lines ?? [];
  const matches = searchLog(lines, query);
  const searching = query.trim() !== "";
  return (
    <Card
      title="Output"
      sub={searching ? `${matches.length} of ${lines.length} lines match` : live ? "Live" : "Captured"}
      right={
        <input
          type="search"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="search output"
          aria-label="Search output"
          className="w-44 rounded-full border border-border bg-background/40 px-3 py-1 font-mono text-[11px] text-foreground placeholder:text-faint focus:border-primary focus:outline-none sm:w-60"
        />
      }
    >
      {searching ? <LogMatches lines={lines} matches={matches} /> : <LogView lines={lines} live={live} />}
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

function tone(line: string): string {
  if (/^\s*(error|FAIL|--- FAIL|thread .* panicked)/.test(line) || line.includes(" FAIL ")) return "text-critical";
  if (/^\s*Compiling/.test(line)) return "text-muted-foreground";
  if (/^\s*(Finished|Summary|ok )/.test(line) || line.includes(" PASS ")) return "text-good-ink";
  if (/^\s*warning/.test(line)) return "text-warning";
  return "text-foreground/80";
}
