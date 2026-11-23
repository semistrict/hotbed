import { Link } from "@tanstack/react-router";
import type { Admission, AdmissionSignal, CostPart, PlacementDecision, Sample } from "../api";
import { ago, bytes, duration, hhmmss } from "../lib/format";
import { SegmentedBar } from "./charts/Bars";
import { Empty } from "./ui/Card";

export const SIGNAL_LABEL: Record<string, string> = {
  "cpu-pressure": "CPU pressure",
  "memory-pressure": "Memory pressure",
  "memory-reserved": "Memory reserved",
  "disk-free": "Disk free",
  "host-cpu-starvation": "Host CPU starvation",
  "host-memory-pressure": "Host memory pressure",
};

const fmt = (s: AdmissionSignal, v: number) => (s.unit === "bytes" ? bytes(v) : `${v.toFixed(0)}%`);

/** What the limit of a signal is, as the worker applies it. */
export function signalRule(s: AdmissionSignal): string {
  const rule = s.floor ? `holds under ${fmt(s, s.limit)}` : `holds at ${fmt(s, s.limit)}`;
  return s.resume > 0 ? `${rule} · resumes under ${fmt(s, s.resume)}` : rule;
}

/** Whether the node starts new work or holds it, and for how long. */
export function AdmissionBadge({ a, now }: { a: Admission | null | undefined; now: number | bigint }) {
  if (!a) return <span className="font-mono text-[10.5px] text-faint">admission unknown</span>;
  if (!a.holding) {
    return <span className="rounded-full bg-good/15 px-2.5 py-0.5 font-mono text-[10.5px] whitespace-nowrap text-good-ink">starting new work</span>;
  }
  const held = a.since > 0n ? duration((Number(now) - Number(a.since)) / 1000) : "";
  return (
    <span title={a.reason} className="rounded-full bg-serious/15 px-2.5 py-0.5 font-mono text-[10.5px] whitespace-nowrap text-serious">
      holding new work{held && ` · ${held}`}
    </span>
  );
}

/** A signal's value on a track, with a tick at its limit and, dashed, where held work resumes. */
function SignalBar({ s }: { s: AdmissionSignal }) {
  const top = s.unit === "%" ? 100 : Math.max(s.value, s.limit) * 1.25 || 1;
  const at = (v: number) => `${Math.min(1, v / top) * 100}%`;
  const color = s.holding ? "var(--serious)" : "var(--primary)";
  return (
    <div className="relative h-1.5 w-full rounded-full" style={{ background: `color-mix(in oklab, ${color} 16%, transparent)` }}>
      <div className="h-full rounded-full transition-[width] duration-700" style={{ width: at(s.value), background: color }} />
      <div className="absolute -top-1 h-3.5 w-[2px] rounded bg-warning" style={{ left: at(s.limit) }} title={signalRule(s)} />
      {s.resume > 0 && <div className="absolute -top-1 h-3.5 w-0 border-l-2 border-dashed border-warning/70" style={{ left: at(s.resume) }} title={`resumes under ${fmt(s, s.resume)}`} />}
    </div>
  );
}

/** How the node decides whether to start new work: its state and every signal against its limit. */
export function AdmissionPanel({ a, now }: { a: Admission | null | undefined; now: number | bigint }) {
  if (!a) return <Empty>The node is offline: what it said last no longer holds.</Empty>;
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-3">
        <AdmissionBadge a={a} now={now} />
        <span className="font-mono text-[11px] text-muted-foreground">{a.queued} queued</span>
      </div>
      {a.holding && <p className="text-[12px] text-foreground">{a.reason}</p>}
      <table className="w-full text-left text-[12px]">
        <thead className="eyebrow">
          <tr className="border-b border-border">
            {["signal", "", "now", "limit", "waiting", ""].map((h, i) => (
              <th key={i} className="px-2 py-2 font-normal whitespace-nowrap">
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {a.signals.map((s) => (
            <tr key={s.name} className="border-b border-border/60 align-top">
              <td className="px-2 py-2 whitespace-nowrap">
                <div className="text-foreground">{SIGNAL_LABEL[s.name] ?? s.name}</div>
                {s.detail && <div className="mt-0.5 max-w-64 text-[10.5px] text-faint">{s.detail}</div>}
              </td>
              <td className="w-full min-w-24 px-2 py-3">
                <SignalBar s={s} />
              </td>
              <td className="num px-2 py-2 font-mono text-[11px] whitespace-nowrap text-foreground">{fmt(s, s.value)}</td>
              <td className="px-2 py-2 font-mono text-[11px] whitespace-nowrap text-muted-foreground">{signalRule(s)}</td>
              <td className="num px-2 py-2 font-mono text-[11px] text-muted-foreground">{s.waiting || "—"}</td>
              <td className="px-2 py-2 font-mono text-[10.5px]">{s.holding ? <span className="text-serious">holding</span> : <span className="text-good-ink">ok</span>}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/** The spans of samples in which the node held new work. */
export function heldBands(samples: Sample[]): { from: number; to: number; label: string }[] {
  const out: { from: number; to: number; label: string }[] = [];
  let from: number | null = null;
  samples.forEach((p, i) => {
    const t = Number(p.t);
    if (p.holding && from === null) from = t;
    const end = !p.holding || i === samples.length - 1;
    if (from !== null && end) {
      out.push({ from, to: t, label: `held new work ${hhmmss(from)}–${hhmmss(t)}` });
      from = null;
    }
  });
  return out;
}

const PART_COLOR: Record<string, string> = {
  "cpu-busy": "var(--series-1)",
  queued: "var(--series-4)",
  "cpu-pressure": "var(--serious)",
  "host-cpu-starvation": "var(--serious)",
  holding: "var(--critical)",
  asleep: "var(--frost)",
  memory: "var(--frost)",
  move: "var(--faint)",
  cold: "var(--series-5)",
  broken: "var(--critical)",
};

function Parts({ parts }: { parts: CostPart[] }) {
  return (
    <SegmentedBar
      legend={false}
      format={(v) => v.toFixed(2)}
      parts={parts.map((p) => ({ id: p.name, label: `${p.name} (${p.detail})`, value: p.value, color: PART_COLOR[p.name] ?? "var(--muted-foreground)" }))}
    />
  );
}

/** What a placement meant for one node: chosen, passed over at what cost, or ruled out. */
function roleOf(d: PlacementDecision, nodeId: string): { text: string; className: string } {
  const c = d.candidates.find((x) => x.nodeId === nodeId);
  if (!c) return { text: "not weighed", className: "text-faint" };
  if (c.chosen) return { text: `chosen · cost ${c.cost.toFixed(2)}`, className: "text-good-ink" };
  if (c.unfit) return { text: "ruled out", className: "text-faint" };
  const won = d.candidates.find((x) => x.chosen);
  return { text: `passed over · ${c.cost.toFixed(2)}${won ? ` vs ${won.cost.toFixed(2)}` : ""}`, className: "text-warning" };
}

/**
 * Placement decisions, newest first, each opening to every node weighed:
 * its cost in parts, or why it was ruled out. With nodeId, each says what it
 * meant for that node.
 */
export function PlacementList({ placements, nodeId, limit = 20, now }: { placements: PlacementDecision[]; nodeId?: string; limit?: number; now: number | bigint }) {
  if (placements.length === 0) return <Empty>No placements in the last day.</Empty>;
  return (
    <ol className="divide-y divide-border">
      {placements.slice(0, limit).map((d) => {
        const role = nodeId ? roleOf(d, nodeId) : null;
        return (
          <li key={String(d.id)}>
            <details className="group">
              <summary className="flex cursor-pointer list-none items-center gap-2.5 py-2 font-mono text-[11px] hover:bg-muted/60">
                <span className="num shrink-0 text-faint" title={ago(d.at, now)}>
                  {hhmmss(d.at)}
                </span>
                <span className="w-3 shrink-0 text-center text-faint group-open:rotate-90">▸</span>
                {d.sessionId ? (
                  <Link to="/sessions/$id" params={{ id: d.sessionId }} className="shrink-0 text-foreground hover:text-primary" onClick={(e) => e.stopPropagation()}>
                    {d.sessionId}
                  </Link>
                ) : (
                  <span className="shrink-0 text-faint">—</span>
                )}
                <span className="shrink-0 text-faint">{d.project}</span>
                <span className={`shrink-0 ${d.nodeId ? "text-foreground" : "text-critical"}`}>{d.nodeId ? `→ ${d.hostname}` : "refused"}</span>
                <span className="truncate text-muted-foreground">{d.reason}</span>
                {role && <span className={`ml-auto shrink-0 ${role.className}`}>{role.text}</span>}
              </summary>
              <table className="mb-3 ml-8 w-[calc(100%-2rem)] text-left text-[11px]">
                <tbody>
                  {d.candidates.map((c) => (
                    <tr key={c.nodeId} className={`border-t border-border/50 ${c.nodeId === nodeId ? "bg-muted/50" : ""}`}>
                      <td className="w-28 px-2 py-1.5 font-mono whitespace-nowrap text-foreground">
                        {c.hostname}
                        {c.mine && <span className="ml-1.5 text-[9.5px] text-faint">session's</span>}
                      </td>
                      {c.unfit ? (
                        <td colSpan={2} className="px-2 py-1.5 text-faint">
                          ruled out: {c.unfit}
                        </td>
                      ) : (
                        <>
                          <td className="num w-16 px-2 py-1.5 font-mono text-foreground">{c.cost.toFixed(2)}</td>
                          <td className="px-2 py-1.5">
                            <Parts parts={c.parts} />
                            <div className="mt-1 flex flex-wrap gap-x-3 text-[10px] text-muted-foreground">
                              {c.parts
                                .filter((p) => p.value > 0)
                                .map((p) => (
                                  <span key={p.name} title={p.detail}>
                                    <span className="mr-1 inline-block size-1.5 rounded-[2px]" style={{ background: PART_COLOR[p.name] ?? "var(--muted-foreground)" }} />
                                    {p.name} {p.value.toFixed(2)} <span className="text-faint">({p.detail})</span>
                                  </span>
                                ))}
                            </div>
                          </td>
                        </>
                      )}
                      <td className="w-14 px-2 py-1.5 text-right font-mono text-[10px]">{c.chosen && <span className="text-good-ink">chosen</span>}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </details>
          </li>
        );
      })}
    </ol>
  );
}
