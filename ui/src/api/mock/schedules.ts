import type { RunOutcome, ScheduledCommand, ScheduledRun } from "../types";

const DAY = 24 * 3600_000;

/**
 * The simulated cluster's scheduled commands: a nightly measurement pinned
 * to one node, two weeks of its runs (one failed, one missed while the
 * controller was down, one skipped behind a long run), and a paused soak.
 * Derived from the clock alone, so a reload shows the same runs.
 */
export function mockSchedules(now: number): ScheduledCommand[] {
  const today = new Date(now);
  today.setHours(2, 0, 0, 0);
  const lastSlot = today.getTime() <= now ? today.getTime() : today.getTime() - DAY;
  const runs: ScheduledRun[] = [];
  for (let n = 0; n < 14; n++) {
    const slot = lastSlot - n * DAY;
    runs.push(nightlyRun(14 - n, slot, OUTCOMES[n % OUTCOMES.length] ?? "succeeded"));
  }
  return [
    {
      name: "cladefs-nightly",
      cron: "0 2 * * *",
      repoId: "cladefs",
      node: "ubu1",
      argv: "scripts/nightly-host.sh",
      env: ["BOARD_URL=https://ubu1.tail2353d.ts.net/api/board"],
      paused: false,
      createdAt: new Date(lastSlot - 20 * DAY).toISOString(),
      updatedAt: new Date(lastSlot - 3 * DAY).toISOString(),
      nextAt: new Date(lastSlot + DAY).toISOString(),
      runs,
    },
    {
      name: "ratel-soak",
      cron: "30 */6 * * *",
      repoId: "ratel",
      node: null,
      argv: "cargo nextest run --profile soak",
      env: [],
      paused: true,
      createdAt: new Date(lastSlot - 9 * DAY).toISOString(),
      updatedAt: new Date(lastSlot - 2 * DAY).toISOString(),
      nextAt: null,
      runs: [],
    },
  ];
}

// The newest night first.
const OUTCOMES: RunOutcome[] = ["succeeded", "succeeded", "failed", "succeeded", "missed", "succeeded", "succeeded", "skipped", "succeeded"];

function nightlyRun(id: number, slot: number, outcome: RunOutcome): ScheduledRun {
  const started = slot + 4_000;
  const hours = outcome === "failed" ? 0.6 : 5.5 + (id % 3) * 0.7;
  const session = `cladefs-nightly-${stamp(slot)}`;
  const base = { id, schedule: "cladefs-nightly", slotAt: new Date(slot).toISOString(), startedAt: new Date(started).toISOString() };
  switch (outcome) {
    case "missed":
      return { ...base, endedAt: base.startedAt, outcome, sessionId: null, commandId: null, nodeId: null, exitCode: null, reason: "the controller was down: missed 1 slot since 02:00" };
    case "skipped":
      return { ...base, endedAt: base.startedAt, outcome, sessionId: null, commandId: null, nodeId: null, exitCode: null, reason: `the run due at 02:00 (cladefs-nightly-${stamp(slot - DAY)}) was still in progress` };
    default:
      return {
        ...base,
        endedAt: new Date(started + hours * 3600_000).toISOString(),
        outcome,
        sessionId: session,
        commandId: `n${id.toString(36).padStart(7, "0")}`,
        nodeId: "nUbu1",
        exitCode: outcome === "failed" ? 1 : 0,
        reason: outcome === "failed" ? "exited 1" : "",
      };
  }
}

/** A slot as its run's session names it: YYYYMMDD-HHMM, local time. */
function stamp(t: number): string {
  const d = new Date(t);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}-${p(d.getHours())}${p(d.getMinutes())}`;
}
