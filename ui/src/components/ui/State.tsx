import { CommandState, RunOutcome } from "../../api";

/** Command states, each an icon and a word, never color alone. */
const STATES: Record<CommandState, { label: string; icon: string; className: string }> = {
  [CommandState.UNSPECIFIED]: { label: "unknown", icon: "?", className: "text-faint bg-muted" },
  [CommandState.QUEUED]: { label: "queued", icon: "◌", className: "text-muted-foreground bg-muted" },
  [CommandState.RUNNING]: { label: "running", icon: "●", className: "text-primary bg-ember-wash" },
  [CommandState.PARKED]: { label: "parked", icon: "❄", className: "text-frost bg-frost-wash" },
  [CommandState.PAUSED]: { label: "paused", icon: "⏸", className: "text-frost bg-frost-wash" },
  [CommandState.SUCCEEDED]: { label: "ok", icon: "✓", className: "text-good-ink bg-[color-mix(in_oklab,var(--good)_12%,transparent)]" },
  [CommandState.FAILED]: { label: "failed", icon: "✕", className: "text-critical bg-[color-mix(in_oklab,var(--critical)_12%,transparent)]" },
  [CommandState.CANCELLED]: { label: "cancelled", icon: "⊘", className: "text-faint bg-muted" },
};

export function StateBadge({ state }: { state: CommandState }) {
  const s = STATES[state] ?? STATES[CommandState.UNSPECIFIED];
  return (
    <span className={`inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 font-mono text-[10.5px] ${s.className}`}>
      <span className={state === CommandState.RUNNING ? "ember-pulse" : undefined} aria-hidden>
        {s.icon}
      </span>
      {s.label}
    </span>
  );
}

/** A base freshen build currently running on a node. */
export function BuildingBadge({ project, generation, progress }: { project: string; generation?: number; progress?: number }) {
  const label = project ? `building ${project}` : "building";
  const title = [`Building base ${project}${generation != null ? ` g${generation}` : ""}`, progress != null ? `${Math.round(progress * 100)}%` : null]
    .filter(Boolean)
    .join(" · ");
  return (
    <span title={title} className="inline-flex items-center gap-1 rounded-md bg-ember-wash px-1.5 py-0.5 font-mono text-[10.5px] text-primary">
      <span className="ember-pulse" aria-hidden>
        ●
      </span>
      {label}
    </span>
  );
}

export function OnlineBadge({ online, asleep }: { online: boolean; asleep: boolean }) {
  if (asleep) {
    return (
      <span className="inline-flex items-center gap-1.5 font-mono text-[10.5px] text-muted-foreground" title="Powered off while idle; a run placed on it wakes it">
        <span className="inline-block size-1.5 rounded-full bg-muted-foreground/50" aria-hidden />
        asleep
      </span>
    );
  }
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

export function ArchBadge({ arch }: { arch: string }) {
  if (!arch) return null;
  return <span className="rounded border border-border-strong px-1 py-px font-mono text-[9.5px] tracking-wide text-muted-foreground uppercase">{arch}</span>;
}

export function KindTag({ kind }: { kind: string }) {
  return <span className="font-mono text-[10px] tracking-wider text-faint uppercase">{kind}</span>;
}

/** A scheduled run's outcome: a command's state while it ran, or why it never did. */
const OUTCOMES: Partial<Record<RunOutcome, { label: string; icon: string; className: string }>> = {
  [RunOutcome.NOT_STARTED]: { label: "not started", icon: "⚠", className: "text-critical bg-[color-mix(in_oklab,var(--critical)_12%,transparent)]" },
  [RunOutcome.SKIPPED]: { label: "skipped", icon: "⤼", className: "text-faint bg-muted" },
  [RunOutcome.MISSED]: { label: "missed", icon: "○", className: "text-warning bg-[color-mix(in_oklab,var(--warning)_14%,transparent)]" },
};

const OUTCOME_TO_STATE: Partial<Record<RunOutcome, CommandState>> = {
  [RunOutcome.RUNNING]: CommandState.RUNNING,
  [RunOutcome.SUCCEEDED]: CommandState.SUCCEEDED,
  [RunOutcome.FAILED]: CommandState.FAILED,
  [RunOutcome.CANCELLED]: CommandState.CANCELLED,
};

export function OutcomeBadge({ outcome }: { outcome: RunOutcome }) {
  const st = OUTCOME_TO_STATE[outcome];
  if (st !== undefined) return <StateBadge state={st} />;
  const o = OUTCOMES[outcome] ?? { label: "unknown", icon: "?", className: "text-faint bg-muted" };
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
