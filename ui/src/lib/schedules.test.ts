import { describe, expect, it } from "vitest";
import type { ScheduledCommand, Snapshot } from "../api";
import { nextRun } from "./schedules";

function schedule(name: string, nextAt: string | null): ScheduledCommand {
  return {
    name, cron: "0 2 * * *", repoId: "cladefs", node: "ubu1", argv: "scripts/nightly-host.sh", env: [], paused: nextAt === null,
    createdAt: "2026-11-01T00:00:00Z", updatedAt: "2026-11-01T00:00:00Z", nextAt, runs: [], project: null,
  };
}

describe("nextRun", () => {
  it("names the soonest run due, skipping paused schedules", () => {
    const s = { schedules: [schedule("nightly", "2026-11-22T02:00:00Z"), schedule("soak", null), schedule("hourly", "2026-11-21T13:00:00Z")] } as Snapshot;
    expect(nextRun(s)).toEqual({ name: "hourly", at: Date.parse("2026-11-21T13:00:00Z") });
    expect(nextRun({ schedules: [schedule("soak", null)] } as Snapshot)).toBeNull();
  });
});
