import { describe, expect, it } from "vitest";
import { MockCluster } from "./sim";

const HOUR = 3600_000;
const t = Date.parse("2026-11-21T15:20:00Z");

describe("MockCluster", () => {
  it("replays the same cluster on every load, so deep links survive a reload", () => {
    // A page loaded at t and polled for 90 s, against a page loaded 90 s later.
    const polled = new MockCluster(7120, t);
    polled.advanceTo(t + 90_000);
    const reloaded = new MockCluster(7120, t + 90_000);
    const a = polled.snapshot();
    const b = reloaded.snapshot();
    expect(b.now).toBe(a.now);
    expect(b.workspaces.map((w) => w.id)).toEqual(a.workspaces.map((w) => w.id));
    expect(b.commands.map((c) => c.id)).toEqual(a.commands.map((c) => c.id));
    expect(b.history.map((h) => h.id)).toEqual(a.history.map((h) => h.id));
  });

  it("keeps 24 hours of history, oldest first, within the window", () => {
    const s = new MockCluster(7120, t).snapshot();
    expect(s.hourly).toHaveLength(24);
    const ends = s.history.map((h) => h.endedAt);
    expect(ends).toEqual([...ends].sort((a, b) => a - b));
    expect(ends[0]).toBeGreaterThanOrEqual(Date.parse(s.now) - 24 * HOUR);
  });

  it("never kills for memory and never reports impossible usage", () => {
    const c = new MockCluster(7120, t);
    c.advanceTo(t + 10 * 60_000);
    const s = c.snapshot();
    expect(s.totals24h.oomKills).toBe(0);
    for (const n of s.nodes.filter((x) => x.online)) {
      expect(n.usage.cpuBusy).toBeGreaterThanOrEqual(0);
      expect(n.usage.cpuBusy).toBeLessThanOrEqual(1);
      expect(n.usage.memoryUsedBytes).toBeLessThanOrEqual(n.memoryBytes);
    }
    for (const cmd of s.commands) {
      if (cmd.state === "parked") expect(cmd.swapBytes).toBeGreaterThan(0);
      if (cmd.state === "queued") expect(cmd.startedAt).toBeNull();
    }
  });

  it("puts every command and workspace in a session the snapshot lists", () => {
    const c = new MockCluster(7120, t);
    c.advanceTo(t + 10 * 60_000);
    const s = c.snapshot();
    const sessions = new Map(s.sessions.map((x) => [x.id, x]));
    for (const cmd of [...s.commands, ...s.history]) expect(sessions.has(cmd.sessionId)).toBe(true);
    for (const w of s.workspaces) {
      expect(sessions.get(w.sessionId)?.workspaces.map((sw) => sw.id)).toContain(w.id);
    }
    for (const x of s.sessions) {
      expect(x.id).toMatch(/^[a-z]+-[a-z]+$/);
      if (x.endedAt === null) expect(x.workspaces.some((sw) => sw.releasedAt === null)).toBe(true);
    }
    const active = s.sessions.map((x) => x.lastActiveAt);
    expect(active).toEqual([...active].sort().reverse());
  });
});
