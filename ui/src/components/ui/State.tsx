import type { CommandState, RunOutcome } from "../../api";

/** Command states, each an icon and a word, never color alone. */
const STATES: Record<CommandState, { label: string; icon: string; className: string }> = {
  queued: { label: "queued", icon: "◌", className: "text-muted-foreground bg-muted" },
  running: { label: "running", icon: "●", className: "text-primary bg-ember-wash" },
  parked: { label: "parked", icon: "❄", className: "text-frost bg-frost-wash" },
  succeeded: { label: "ok", icon: "✓", className: "text-good-ink bg-[color-mix(in_oklab,var(--good)_12%,transparent)]" },
  failed: { label: "failed", icon: "✕", className: "text-critical bg-[color-mix(in_oklab,var(--critical)_12%,transparent)]" },
  cancelled: { label: "cancelled", icon: "⊘", className: "text-faint bg-muted" },
};

export function StateBadge({ state }: { state: CommandState }) {
  const s = STATES[state];
  return (
    <span className={`inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 font-mono text-[10.5px] ${s.className}`}>
      <span className={state === "running" ? "ember-pulse" : undefined} aria-hidden>
        {s.icon}
      </span>
      {s.label}
    </span>
  );
}

export function OnlineBadge({ online }: { online: boolean }) {
  return online ? (
    <span className="inline-flex items-center gap-1.5 font-mono text-[10.5px] text-good-ink">
      <span className="live-dot inline-block size-1.5 rounded-full bg-good text-good" aria-hidden />
      online
    </span>
  ) : (
    <span className="inline-flex items-center gap-1.5 font-mono text-[10.5px] text-faint">
      <span className="inline-block size-1.5 rounded-full border border-faint" aria-hidden />
      offline
    </span>
  );
}

export function ArchBadge({ arch }: { arch: "arm64" | "amd64" }) {
  return <span className="rounded border border-border-strong px-1 py-px font-mono text-[9.5px] tracking-wide text-muted-foreground uppercase">{arch}</span>;
}

export function KindTag({ kind }: { kind: string }) {
  return <span className="font-mono text-[10px] tracking-wider text-faint uppercase">{kind}</span>;
}

/** A scheduled run's outcome: a command's state while it ran, or why it never did. */
const OUTCOMES: Record<"not-started" | "skipped" | "missed", { label: string; icon: string; className: string }> = {
  "not-started": { label: "not started", icon: "⚠", className: "text-critical bg-[color-mix(in_oklab,var(--critical)_12%,transparent)]" },
  skipped: { label: "skipped", icon: "⤼", className: "text-faint bg-muted" },
  missed: { label: "missed", icon: "○", className: "text-warning bg-[color-mix(in_oklab,var(--warning)_14%,transparent)]" },
};

export function OutcomeBadge({ outcome }: { outcome: RunOutcome }) {
  if (outcome === "running" || outcome === "succeeded" || outcome === "failed" || outcome === "cancelled") return <StateBadge state={outcome} />;
  const o = OUTCOMES[outcome];
  return (
    <span className={`inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 font-mono text-[10.5px] ${o.className}`}>
      <span aria-hidden>{o.icon}</span>
      {o.label}
    </span>
  );
}

/** Marks a measurement (hotbed run --bench), in word and icon. */
export function BenchTag() {
  return (
    <span className="inline-flex items-center gap-1 rounded border border-primary/40 px-1 font-mono text-[10px] tracking-wider text-primary uppercase" title="A measurement: alone on the node's measurement CPUs, isolated, never parked">
      <span aria-hidden>⏱</span>
      measurement
    </span>
  );
}
