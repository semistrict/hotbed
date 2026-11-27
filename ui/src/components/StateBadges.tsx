import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import { CommandState, RunOutcome } from "../api";

/** Command states, each an icon and a word, never color alone. */
const STATES: Record<CommandState, { label: string; icon: string; className: string }> = {
  [CommandState.UNSPECIFIED]: { label: "unknown", icon: "?", className: "text-muted-foreground bg-muted" },
  [CommandState.QUEUED]: { label: "queued", icon: "◌", className: "text-muted-foreground bg-muted" },
  [CommandState.RUNNING]: { label: "running", icon: "●", className: "text-primary bg-ember-wash" },
  [CommandState.PARKED]: { label: "parked", icon: "❄", className: "text-frost bg-frost-wash" },
  [CommandState.PAUSED]: { label: "paused", icon: "⏸", className: "text-frost bg-frost-wash" },
  [CommandState.SUCCEEDED]: { label: "ok", icon: "✓", className: "text-good-ink bg-[color-mix(in_oklab,var(--good)_12%,transparent)]" },
  [CommandState.FAILED]: { label: "failed", icon: "✕", className: "text-critical bg-[color-mix(in_oklab,var(--critical)_12%,transparent)]" },
  [CommandState.CANCELLED]: { label: "cancelled", icon: "⊘", className: "text-muted-foreground bg-muted" },
};

export function StateBadge({ state }: { state: CommandState }) {
  const s = STATES[state] ?? STATES[CommandState.UNSPECIFIED];
  return (
    <Badge variant="secondary" className={cn("h-auto rounded-md border-transparent px-1.5 py-0.5 font-mono text-[10.5px] font-normal", s.className)}>
      <span className={state === CommandState.RUNNING ? "ember-pulse" : undefined} aria-hidden>
        {s.icon}
      </span>
      {s.label}
    </Badge>
  );
}

/** A base freshen build currently running on a node. */
export function BuildingBadge({ project, generation, progress }: { project: string; generation?: number; progress?: number }) {
  const label = project ? `building ${project}` : "building";
  const title = [`Building base ${project}${generation != null ? ` g${generation}` : ""}`, progress != null ? `${Math.round(progress * 100)}%` : null]
    .filter(Boolean)
    .join(" · ");
  return (
    <Badge
      variant="secondary"
      title={title}
      className="h-auto max-w-full min-w-0 shrink rounded-md border-transparent bg-ember-wash px-1.5 py-0.5 font-mono text-[10.5px] font-normal text-primary"
    >
      <span className="ember-pulse shrink-0" aria-hidden>
        ●
      </span>
      <span className="truncate">{label}</span>
    </Badge>
  );
}

export function OnlineBadge({ online, storageError = "" }: { online: boolean; storageError?: string }) {
  if (!online && storageError) {
    // Its worker heartbeats but cannot open its storage: it serves nothing.
    return (
      <span className="inline-flex items-center gap-1.5 font-mono text-[10.5px] font-normal whitespace-nowrap text-serious-ink" title={storageError}>
        <span className="inline-block size-1.5 shrink-0 rounded-full bg-serious" aria-hidden />
        no storage
      </span>
    );
  }
  return online ? (
    <span className="inline-flex items-center gap-1.5 font-mono text-[10.5px] font-normal whitespace-nowrap text-good-ink">
      <span className="live-dot inline-block size-1.5 shrink-0 rounded-full bg-good text-good" aria-hidden />
      online
    </span>
  ) : (
    <span className="inline-flex items-center gap-1.5 font-mono text-[10.5px] font-normal whitespace-nowrap text-faint">
      <span className="inline-block size-1.5 shrink-0 rounded-full border border-faint" aria-hidden />
      offline
    </span>
  );
}

export function ArchBadge({ arch }: { arch: string }) {
  if (!arch) return null;
  return (
    <Badge variant="outline" className="h-auto rounded border-border-strong px-1 py-px font-mono text-[9.5px] font-normal tracking-wide text-muted-foreground uppercase">
      {arch}
    </Badge>
  );
}

export function KindTag({ kind }: { kind: string }) {
  return <span className="font-mono text-[10px] tracking-wider text-faint uppercase">{kind}</span>;
}

const RUNTIMES: Record<string, { label: string; title: string; className: string }> = {
  vm: {
    label: "vm",
    title: "Firecracker microVM runtime",
    className: "border-frost/40 bg-frost-wash text-frost",
  },
  cladefs: {
    label: "cladefs",
    title: "cladefs host-FUSE runtime",
    className: "border-good/35 bg-good/10 text-good-ink",
  },
};

export function RuntimeBadge({ runtime }: { runtime?: string }) {
  if (!runtime) return <span className="font-mono text-[11px] text-faint">—</span>;
  const r = RUNTIMES[runtime] ?? {
    label: runtime,
    title: `${runtime} runtime`,
    className: "border-border-strong text-muted-foreground",
  };
  return (
    <Badge
      variant="outline"
      title={r.title}
      className={cn("h-auto rounded px-1.5 py-0.5 font-mono text-[10px] font-normal tracking-wide", r.className)}
    >
      {r.label}
    </Badge>
  );
}

/** A scheduled run's outcome: a command's state while it ran, or why it never did. */
const OUTCOMES: Partial<Record<RunOutcome, { label: string; icon: string; className: string }>> = {
  [RunOutcome.NOT_STARTED]: { label: "not started", icon: "⚠", className: "text-critical bg-[color-mix(in_oklab,var(--critical)_12%,transparent)]" },
  [RunOutcome.SKIPPED]: { label: "skipped", icon: "⤼", className: "text-muted-foreground bg-muted" },
  [RunOutcome.MISSED]: { label: "missed", icon: "○", className: "text-warning-ink bg-[color-mix(in_oklab,var(--warning)_14%,transparent)]" },
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
  const o = OUTCOMES[outcome] ?? { label: "unknown", icon: "?", className: "text-muted-foreground bg-muted" };
  return (
    <Badge variant="secondary" className={cn("h-auto rounded-md border-transparent px-1.5 py-0.5 font-mono text-[10.5px] font-normal", o.className)}>
      <span aria-hidden>{o.icon}</span>
      {o.label}
    </Badge>
  );
}

/** Marks a measurement (hotbed run --bench), in word and icon. */
export function BenchTag() {
  return (
    <Badge
      variant="outline"
      className="h-auto rounded border-primary/40 px-1 py-0 font-mono text-[10px] font-normal tracking-wider text-primary uppercase"
      title="A measurement: alone on the node's measurement CPUs, isolated, never parked"
    >
      <span aria-hidden>⏱</span>
      measurement
    </Badge>
  );
}
