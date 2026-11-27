import { ChevronRightIcon } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import type { Admission, AdmissionSignal, CostPart, PlacementDecision, Sample } from "../api";
import { SessionLink } from "./SessionLink";
import { ago, bytes, duration, hhmmss } from "../lib/format";
import { SegmentedBar } from "./charts/Bars";
import { Empty } from "./Panel";

export const SIGNAL_LABEL: Record<string, string> = {
  "cpu-pressure": "CPU pressure",
  "memory-pressure": "Memory pressure",
  "memory-reserved": "Memory reserved",
  "disk-free": "Disk free",
};

const fmt = (s: AdmissionSignal, v: number) => (s.unit === "bytes" ? bytes(v) : `${v.toFixed(0)}%`);

/** What the limit of a signal is, as the worker applies it. */
export function signalRule(s: AdmissionSignal): string {
  const rule = s.floor ? `holds under ${fmt(s, s.limit)}` : `holds at ${fmt(s, s.limit)}`;
  return s.resume > 0 ? `${rule} · ${s.floor ? "resumes above" : "resumes under"} ${fmt(s, s.resume)}` : rule;
}

/** Whether the node starts new work or holds it, and for how long. */
export function AdmissionBadge({ a, now }: { a: Admission | null | undefined; now: number | bigint }) {
  if (!a) return <span className="font-mono text-[10.5px] text-faint">admission unknown</span>;
  if (!a.holding) {
    return (
      <Badge variant="secondary" className="h-auto bg-good/15 px-2.5 py-0.5 font-mono text-[10.5px] font-normal text-good-ink">
        starting new work
      </Badge>
    );
  }
  const held = a.since > 0n ? duration((Number(now) - Number(a.since)) / 1000) : "";
  return (
    <Badge variant="secondary" title={a.reason} className="h-auto bg-serious/15 px-2.5 py-0.5 font-mono text-[10.5px] font-normal text-serious-ink">
      holding new work{held && ` · ${held}`}
    </Badge>
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
      {s.resume > 0 && (
        <div
          className="absolute -top-1 h-3.5 w-0 border-l-2 border-dashed border-warning/70"
          style={{ left: at(s.resume) }}
          title={`${s.floor ? "resumes above" : "resumes under"} ${fmt(s, s.resume)}`}
        />
      )}
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
      <Table className="text-[12px]">
        <TableHeader className="eyebrow">
          <TableRow className="border-border hover:bg-transparent">
            {["signal", "", "now", "limit", "waiting", ""].map((h, i) => (
              <TableHead key={i} className="h-auto px-2 py-2 font-normal text-muted-foreground">
                {h}
              </TableHead>
            ))}
          </TableRow>
        </TableHeader>
        <TableBody>
          {a.signals.map((s) => (
            <TableRow key={s.name} className="border-border/60 align-top">
              <TableCell className="px-2 py-2">
                <div className="text-foreground">{SIGNAL_LABEL[s.name] ?? s.name}</div>
                {s.detail && <div className="mt-0.5 max-w-64 text-[10.5px] whitespace-normal text-faint">{s.detail}</div>}
              </TableCell>
              <TableCell className="w-full min-w-24 px-2 py-3">
                <SignalBar s={s} />
              </TableCell>
              <TableCell className="num px-2 py-2 font-mono text-[11px] text-foreground">{fmt(s, s.value)}</TableCell>
              <TableCell className="px-2 py-2 font-mono text-[11px] text-muted-foreground">{signalRule(s)}</TableCell>
              <TableCell className="num px-2 py-2 font-mono text-[11px] text-muted-foreground">{s.waiting || "—"}</TableCell>
              <TableCell className="px-2 py-2 font-mono text-[10.5px]">{s.holding ? <span className="text-serious-ink">holding</span> : <span className="text-good-ink">ok</span>}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
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
  holding: "var(--critical)",
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
  return { text: `passed over · ${c.cost.toFixed(2)}${won ? ` vs ${won.cost.toFixed(2)}` : ""}`, className: "text-warning-ink" };
}

/** Requested placement constraints for a decision, or null for legacy records that predate request tracking. */
export function placementRequirements(d: PlacementDecision): { label: string; pinned?: boolean }[] | null {
  if (!d.request) return null;
  const reqs: { label: string; pinned?: boolean }[] = [];
  if (d.request.arch) reqs.push({ label: `--arch ${d.request.arch}`, pinned: true });
  if (d.request.runtime) reqs.push({ label: `--runtime ${d.request.runtime}` });
  if (d.request.bench) reqs.push({ label: "--bench", pinned: true });
  if (d.request.memoryLowBytes > 0n) reqs.push({ label: `--mem-low ${bytes(d.request.memoryLowBytes)}` });
  if (d.request.memoryEstimateBytes > 0n) reqs.push({ label: `--mem ${bytes(d.request.memoryEstimateBytes)}` });
  if (d.request.vmCpus > 0) reqs.push({ label: `--vm-cpus ${d.request.vmCpus}` });
  if (d.request.vmMemoryBytes > 0n) reqs.push({ label: `--vm-mem ${bytes(d.request.vmMemoryBytes)}` });
  if (d.noSync) reqs.push({ label: "--no-sync", pinned: true });
  if (d.throwaway) reqs.push({ label: "--fork" });
  return reqs;
}

export function PlacementReqBadges({ decision }: { decision: PlacementDecision }) {
  const reqs = placementRequirements(decision);
  if (reqs === null) return null;
  if (reqs.length === 0) {
    return (
      <Badge variant="secondary" className="h-auto bg-muted px-2 py-0.5 font-mono text-[10px] font-normal text-muted-foreground">
        any eligible node
      </Badge>
    );
  }
  return (
    <span className="inline-flex flex-wrap items-center gap-1">
      {reqs.map((r) => (
        <Badge
          key={r.label}
          variant="secondary"
          className={`h-auto px-2 py-0.5 font-mono text-[10px] font-normal ${r.pinned ? "bg-serious/15 text-serious-ink" : "bg-muted text-foreground"}`}
        >
          {r.label}
        </Badge>
      ))}
    </span>
  );
}

function PlacementCandidatesTable({ decision, nodeId }: { decision: PlacementDecision; nodeId?: string }) {
  if (decision.candidates.length === 0) return null;
  return (
    <Table className="text-[11px]">
      <TableBody>
        {decision.candidates.map((c) => (
          <TableRow key={c.nodeId} className={`border-border/50 ${c.nodeId === nodeId ? "bg-muted/50" : ""}`}>
            <TableCell className="w-28 px-2 py-1.5 font-mono text-foreground">
              {c.hostname}
              {c.mine && <span className="ml-1.5 text-[9.5px] text-faint">session's</span>}
            </TableCell>
            {c.unfit ? (
              <TableCell colSpan={2} className="px-2 py-1.5 text-faint whitespace-normal">
                ruled out: {c.unfit}
              </TableCell>
            ) : (
              <>
                <TableCell className="num w-16 px-2 py-1.5 font-mono text-foreground">{c.cost.toFixed(2)}</TableCell>
                <TableCell className="px-2 py-1.5 whitespace-normal">
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
                </TableCell>
              </>
            )}
            <TableCell className="w-14 px-2 py-1.5 text-right font-mono text-[10px]">{c.chosen && <span className="text-good-ink">chosen</span>}</TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

/** Full placement breakdown for one command: requested constraints, chosen host + reason, and every node weighed. */
export function PlacementDetail({ decision }: { decision: PlacementDecision }) {
  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5 font-mono text-[11.5px]">
        <span className="text-muted-foreground">requested</span>
        <PlacementReqBadges decision={decision} />
        <span className="text-faint">·</span>
        <span className={decision.nodeId ? "text-foreground" : "text-critical"}>
          {decision.hostname ? `→ ${decision.hostname}` : "refused"}
        </span>
        {decision.reason && <span className="text-muted-foreground">{decision.reason}</span>}
      </div>
      <PlacementCandidatesTable decision={decision} />
    </div>
  );
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
            <Collapsible>
              <div className="flex flex-wrap items-center gap-x-2.5 gap-y-1 py-2 font-mono text-[11px] hover:bg-muted/60">
                <CollapsibleTrigger className="group flex min-w-0 flex-1 cursor-pointer flex-wrap items-center gap-x-2.5 gap-y-1 text-left">
                  <span className="num shrink-0 text-faint" title={ago(d.at, now)}>
                    {hhmmss(d.at)}
                  </span>
                  <ChevronRightIcon className="size-3 shrink-0 text-faint transition-transform group-data-panel-open:rotate-90" />
                  <span className="shrink-0 text-faint">{d.project}</span>
                  <span className={`shrink-0 ${d.nodeId ? "text-foreground" : "text-critical"}`}>{d.nodeId ? `→ ${d.hostname}` : "refused"}</span>
                  <PlacementReqBadges decision={d} />
                  <span className="min-w-0 truncate text-muted-foreground">{d.reason}</span>
                  {role && <span className={`shrink-0 sm:ml-auto ${role.className}`}>{role.text}</span>}
                </CollapsibleTrigger>
                <SessionLink id={d.sessionId} className="shrink-0" />
              </div>
              <CollapsibleContent>
                <div className="mb-3 sm:ml-6">
                  <PlacementCandidatesTable decision={d} nodeId={nodeId} />
                </div>
              </CollapsibleContent>
            </Collapsible>
          </li>
        );
      })}
    </ol>
  );
}
