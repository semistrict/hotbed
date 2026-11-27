import type { MemoryBudget } from "../api";
import { kib } from "../lib/format";

/**
 * What a session start recalls, as the controller counted it under the
 * startup caps: each memory's summary, which agents read in full once it
 * applies. The page only shows the numbers, never recounts them. `budget` is
 * unset while the first answer loads; the bar keeps its shape.
 */
export function MemoryBudgetBar({ budget, project }: { budget: MemoryBudget | undefined; project: string | null }) {
  const loaded = budget !== undefined;
  const omitted = budget?.omitted ?? 0;
  return (
    <div className="flex flex-wrap items-center gap-x-6 gap-y-2 rounded-xl border border-border bg-card/60 px-4 py-2.5 font-mono text-[11px] text-muted-foreground">
      <span className="text-foreground">Startup recall ({project ? `global + ${project}` : "global"}):</span>
      <span className={omitted > 0 ? "text-warning-ink" : ""}>
        Summaries <strong className="text-foreground">{loaded ? budget.count : "…"}</strong>/{loaded ? budget.maxCount : "…"} ·{" "}
        {loaded ? `${kib(budget.bytes)}/${kib(budget.maxBytes)}` : "…"}
        {omitted > 0 ? ` · ${omitted} omitted` : ""}
      </span>
    </div>
  );
}
