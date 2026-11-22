import { Link } from "@tanstack/react-router";
import type { Command, FinishedCommand, Snapshot } from "../api";
import { nodeName } from "../lib/data";
import { ago, bytes, duration, speedup } from "../lib/format";
import { Meter } from "./charts/Bars";
import { SessionLink } from "./SessionLink";
import { Empty } from "./ui/Card";
import { KindTag, StateBadge } from "./ui/State";

/** Live commands: what each is doing now, against its limits. */
export function LiveCommandTable({ s, commands }: { s: Snapshot; commands: Command[] }) {
  const now = Date.parse(s.now);
  if (commands.length === 0) return <Empty>No commands here right now.</Empty>;
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-[12px]">
        <thead className="eyebrow">
          <tr className="border-b border-border">
            {["state", "command", "session", "workspace", "node", "for", "cpu", "memory · high", "crates", "why"].map((h) => (
              <th key={h} className="px-2 py-2 font-normal whitespace-nowrap">
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {commands.map((c) => {
            const since = Date.parse(c.startedAt ?? c.queuedAt);
            return (
              <tr key={c.id} className="border-b border-border/60 align-top hover:bg-muted/60">
                <td className="px-2 py-2.5">
                  <StateBadge state={c.state} />
                </td>
                <td className="max-w-[320px] px-2 py-2.5">
                  <Link to="/commands/$id" params={{ id: c.id }} className="block truncate font-mono text-[11.5px] text-foreground hover:text-primary">
                    {c.argv}
                  </Link>
                  <div className="mt-0.5 flex gap-2">
                    <KindTag kind={c.kind} />
                    {c.url && (
                      <a href={c.url} className="font-mono text-[10px] text-primary hover:underline" target="_blank" rel="noreferrer">
                        {c.url.replace("http://", "")} ↗
                      </a>
                    )}
                  </div>
                </td>
                <td className="px-2 py-2.5">
                  <SessionLink id={c.sessionId} />
                </td>
                <td className="px-2 py-2.5">
                  <Link to="/workspaces/$id" params={{ id: c.workspaceId }} className="font-mono text-[11px] text-muted-foreground hover:text-primary">
                    {c.workspaceId}
                  </Link>
                </td>
                <td className="px-2 py-2.5 font-mono text-[11px] text-muted-foreground">{nodeName(s, c.nodeId)}</td>
                <td className="num px-2 py-2.5 font-mono text-[11px] text-muted-foreground">{duration((now - since) / 1000)}</td>
                <td className="num px-2 py-2.5 font-mono text-[11px]">{c.state === "running" ? `${c.cpuCores.toFixed(1)} cores` : "—"}</td>
                <td className="w-40 px-2 py-2.5">
                  {c.state === "queued" ? (
                    <span className="font-mono text-[11px] text-faint">low {bytes(c.limits.memoryLowBytes)}</span>
                  ) : (
                    <>
                      <div className="num mb-1 font-mono text-[10.5px] text-muted-foreground">
                        {bytes(c.memoryBytes)} · {bytes(c.limits.memoryHighBytes)}
                        {c.swapBytes > 0 && <span className="text-frost"> +{bytes(c.swapBytes)} swap</span>}
                      </div>
                      <Meter value={c.memoryBytes / c.limits.memoryHighBytes} marker={c.limits.memoryLowBytes / c.limits.memoryHighBytes} />
                    </>
                  )}
                </td>
                <td className="num px-2 py-2.5 font-mono text-[11px] text-muted-foreground">{c.rebuilt ? `${c.rebuilt.units}/${c.rebuilt.of}` : "—"}</td>
                <td className="max-w-[260px] px-2 py-2.5 text-[11px] text-muted-foreground">{c.reason ?? (c.state === "running" ? `started ${ago(since, now)}` : "")}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

/** Commands that ended: outcome, time against cold, and why it failed. */
export function HistoryTable({ s, rows, limit = 200 }: { s: Snapshot; rows: FinishedCommand[]; limit?: number }) {
  const now = Date.parse(s.now);
  if (rows.length === 0) return <Empty>Nothing matches.</Empty>;
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-[12px]">
        <thead className="eyebrow">
          <tr className="border-b border-border">
            {["outcome", "command", "session", "repo", "node", "ended", "took", "cold", "speedup", "crates", "peak mem", "why"].map((h) => (
              <th key={h} className="px-2 py-2 font-normal whitespace-nowrap">
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="num font-mono text-[11px] [&_td]:whitespace-nowrap">
          {rows.slice(0, limit).map((c) => (
            <tr key={c.id} className="border-b border-border/60 hover:bg-muted/60">
              <td className="px-2 py-2">
                <StateBadge state={c.state} />
              </td>
              <td className="max-w-[300px] px-2 py-2">
                <Link to="/commands/$id" params={{ id: c.id }} className="block truncate text-foreground hover:text-primary">
                  {c.argv}
                </Link>
              </td>
              <td className="px-2 py-2">
                <SessionLink id={c.sessionId} />
              </td>
              <td className="px-2 py-2 text-muted-foreground">{c.repoId}</td>
              <td className="px-2 py-2 text-muted-foreground">{nodeName(s, c.nodeId)}</td>
              <td className="px-2 py-2 whitespace-nowrap text-muted-foreground">{ago(c.endedAt, now)}</td>
              <td className="px-2 py-2">{duration(c.seconds)}</td>
              <td className="px-2 py-2 text-faint">{c.coldSeconds !== null ? duration(c.coldSeconds) : "—"}</td>
              <td className="px-2 py-2 text-primary">{c.coldSeconds !== null ? speedup(c.coldSeconds, c.seconds) : "—"}</td>
              <td className="px-2 py-2 text-muted-foreground">{c.rebuilt !== null ? `${c.rebuilt}/${c.of}` : "—"}</td>
              <td className="px-2 py-2 text-muted-foreground">
                {bytes(c.peakMemoryBytes)}
                {c.parks > 0 && <span className="text-frost"> ❄{c.parks}</span>}
              </td>
              <td className="px-2 py-2 font-sans text-muted-foreground">{c.failure ? FAILURE[c.failure] : ""}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {rows.length > limit && <p className="mt-3 text-center font-mono text-[10.5px] text-faint">showing {limit} of {rows.length}</p>}
    </div>
  );
}

export const FAILURE: Record<NonNullable<FinishedCommand["failure"]>, string> = {
  compile: "compile error",
  test: "tests failed",
  lint: "lint denied",
  signal: "killed by signal",
  exit: "non-zero exit",
};
