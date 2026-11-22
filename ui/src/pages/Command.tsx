import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "@tanstack/react-router";
import { useEffect, useRef } from "react";
import { api, type Command, type CommandEvent, type FinishedCommand, type Snapshot } from "../api";
import { Meter } from "../components/charts/Bars";
import { SpeedBars } from "../components/charts/SpeedBars";
import { FAILURE } from "../components/CommandTable";
import { Card, StatTile } from "../components/ui/Card";
import { KindTag, StateBadge } from "../components/ui/State";
import { nodeName, useSnapshot } from "../lib/data";
import { bytes, duration, hhmmss, speedup } from "../lib/format";
import { isLive } from "../lib/live";
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
  const workspaceExists = s.workspaces.some((w) => w.id === workspaceId);

  return (
    <div className="space-y-6">
      <div className="rise">
        <div className="flex flex-wrap items-center gap-3">
          <StateBadge state={state} />
          <KindTag kind={live?.kind ?? done?.kind ?? "build"} />
          <span className="font-mono text-[11px] text-faint">{id}</span>
        </div>
        <h1 className="mt-3 font-mono text-[26px] leading-tight text-foreground sm:text-[32px]">
          <span className="text-primary">$</span> {argv}
        </h1>
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
          {workspaceExists ? (
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

      <div className="grid gap-4 lg:grid-cols-[1fr_320px]">
        <Card title="Output" sub={live && isLive(live) ? "Live" : "Captured"}>
          <LogView id={id} live={!!live && isLive(live)} />
        </Card>
        <div className="space-y-4">
          {live && (
            <Card title="Limits" sub="cgroup v2, set at admission">
              <dl className="space-y-3 text-[11.5px]">
                <Limit label="cpu.weight" value={String(live.limits.cpuWeight)} />
                <Limit label="memory.low" value={bytes(live.limits.memoryLowBytes)} />
                <Limit label="memory.high" value={bytes(live.limits.memoryHighBytes)} />
                <Limit label="memory.max" value="none — park, don't kill" />
              </dl>
              <div className="mt-4">
                <div className="mb-1 flex justify-between font-mono text-[10.5px] text-muted-foreground">
                  <span>{bytes(live.memoryBytes)} used</span>
                  <span>high {bytes(live.limits.memoryHighBytes)}</span>
                </div>
                <Meter value={live.memoryBytes / live.limits.memoryHighBytes} marker={live.limits.memoryLowBytes / live.limits.memoryHighBytes} />
              </div>
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
  const glyph: Record<CommandEvent["what"], string> = { queued: "◌", admitted: "▸", parked: "❄", thawed: "☀", exited: "■", cancelled: "⊘" };
  return (
    <ol className="relative ml-1 space-y-3 border-l border-border pl-4">
      {events.map((e, i) => (
        <li key={i} className="relative">
          <span className={`absolute -left-[22px] top-0 w-3 bg-card text-center text-[11px] ${e.what === "parked" ? "text-frost" : e.what === "thawed" ? "text-primary" : "text-muted-foreground"}`}>{glyph[e.what]}</span>
          <div className="font-mono text-[11px] text-foreground">
            {e.what} <span className="text-faint">{hhmmss(Date.parse(e.at))}</span>
          </div>
          <div className="text-[11px] text-muted-foreground">{e.detail}</div>
        </li>
      ))}
    </ol>
  );
}

function LogView({ id, live }: { id: string; live: boolean }) {
  const log = useQuery({ queryKey: ["log", id], queryFn: () => api.commandLog(id), refetchInterval: live ? 1000 : false });
  const end = useRef<HTMLDivElement>(null);
  const lines = log.data?.lines.length ?? 0;
  useEffect(() => {
    if (live) end.current?.scrollIntoView({ block: "nearest" });
  }, [lines, live]);
  return (
    <div className="max-h-[520px] overflow-auto rounded-lg border border-border bg-background/60 p-3 font-mono text-[11.5px] leading-[1.7]">
      {log.data?.lines.map((l, i) => (
        <div key={i} className={`whitespace-pre ${tone(l)}`}>
          {l || " "}
        </div>
      ))}
      {live && <div className="ember-pulse text-primary">▍</div>}
      <div ref={end} />
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
