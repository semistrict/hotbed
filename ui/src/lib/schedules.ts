import type { Snapshot } from "../api";

/** The soonest run due among the schedules not paused. */
export function nextRun(s: Snapshot): { name: string; at: number } | null {
  let best: { name: string; at: number } | null = null;
  for (const sc of s.schedules) {
    if (sc.nextAt === undefined) continue;
    const at = Number(sc.nextAt);
    if (best === null || at < best.at) best = { name: sc.name, at };
  }
  return best;
}
