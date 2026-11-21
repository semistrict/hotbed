import type { CommandState } from "../../api";

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
