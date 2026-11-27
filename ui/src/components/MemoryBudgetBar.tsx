import type { MemoryBudget } from "../api";
import { kib } from "../lib/format";

/**
 * What a session start recalls, as the controller counted it under the
 * startup caps: the page only shows the numbers, never recounts them.
 * `budget` is unset while the first answer loads; the bar keeps its shape.
 */
export function MemoryBudgetBar({ budget, project }: { budget: MemoryBudget | undefined; project: string | null }) {
  return (
    <div className="flex flex-wrap items-center gap-x-6 gap-y-2 rounded-xl border border-border bg-card/60 px-4 py-2.5 font-mono text-[11px] text-muted-foreground">
      <span className="text-foreground">Startup recall ({project ? `global + ${project}` : "global"}):</span>
      <BudgetPart
        label="Always-on"
        count={budget?.alwaysCount}
        bytes={budget?.alwaysBytes}
        maxCount={budget?.alwaysMaxCount}
        maxBytes={budget?.alwaysMaxBytes}
        omitted={budget?.omittedAlways ?? 0}
      />
      <BudgetPart
        label="On-demand index"
        count={budget?.onDemandCount}
        bytes={budget?.onDemandBytes}
        maxCount={budget?.onDemandMaxCount}
        maxBytes={budget?.onDemandMaxBytes}
        omitted={budget?.omittedOnDemand ?? 0}
      />
    </div>
  );
}

function BudgetPart({
  label,
  count,
  bytes,
  maxCount,
  maxBytes,
  omitted,
}: {
  label: string;
  count: number | undefined;
  bytes: number | undefined;
  maxCount: number | undefined;
  maxBytes: number | undefined;
  omitted: number;
}) {
  const loaded = count !== undefined && bytes !== undefined && maxCount !== undefined && maxBytes !== undefined;
  return (
    <span className={omitted > 0 ? "text-warning-ink" : ""}>
      {label} <strong className="text-foreground">{loaded ? count : "…"}</strong>/{loaded ? maxCount : "…"} ·{" "}
      {loaded ? `${kib(bytes)}/${kib(maxBytes)}` : "…"}
      {omitted > 0 ? ` · ${omitted} omitted` : ""}
    </span>
  );
}
