import { describe, expect, it } from "vitest";
import type { Command, FinishedCommand, Session, Snapshot } from "../api";
import { idleLeftMs, sessionCommands, sessionState, workspaceRepo } from "./sessions";

const now = Date.parse("2026-11-21T12:00:00Z");
const session: Session = {
  id: "brave-otter",
  agent: "laptop",
  startedAt: "2026-11-21T11:00:00Z",
  lastActiveAt: "2026-11-21T11:50:00Z",
  endedAt: null,
  endReason: "",
  workspaces: [{ id: "ws1", repoId: "cladefs", nodeId: "nUbu1", createdAt: "2026-11-21T11:00:00Z", releasedAt: null }],
};

function command(id: string, state: Command["state"], startedAt: string, endedAt: string | null): Command {
  return {
    id, sessionId: "brave-otter", workspaceId: "ws1", nodeId: "nUbu1", argv: `cargo test ${id}`, kind: "test", state,
    queuedAt: startedAt, startedAt, endedAt, exitCode: null, limits: { cpuWeight: 100, memoryLowBytes: 0, memoryHighBytes: 0 },
    cpuCores: 0, memoryBytes: 0, swapBytes: 0, rebuilt: null, coldSeconds: null, reason: null, url: null, timeline: [],
  };
}

function finished(id: string, endedAt: string, seconds: number, state: FinishedCommand["state"]): FinishedCommand {
  return {
    id, sessionId: "brave-otter", repoId: "cladefs", nodeId: "nUbu1", workspaceId: "ws1", agent: "laptop", argv: `cargo build ${id}`,
    kind: "build", state, exitCode: state === "failed" ? 101 : 0, failure: state === "failed" ? "compile" : null,
    endedAt: Date.parse(endedAt), seconds, coldSeconds: 300, rebuilt: 1, of: 40, peakMemoryBytes: 0, parks: 0,
  };
}

describe("sessions", () => {
  it("merges a session's live and finished commands, newest first, a just-ended one once", () => {
    const s = {
      now: new Date(now).toISOString(),
      sessions: [session],
      workspaces: [],
      commands: [command("live", "running", "2026-11-21T11:59:00Z", null), command("done", "succeeded", "2026-11-21T11:40:00Z", "2026-11-21T11:40:10Z")],
      history: [finished("done", "2026-11-21T11:40:10Z", 10, "succeeded"), finished("old", "2026-11-21T11:10:30Z", 30, "failed")],
    } as unknown as Snapshot;
    const rows = sessionCommands(s, "brave-otter", workspaceRepo(s));
    expect(rows.map((r) => [r.id, r.state, r.live, r.repoId, Math.round(r.seconds)])).toEqual([
      ["live", "running", true, "cladefs", 60],
      ["done", "succeeded", false, "cladefs", 10],
      ["old", "failed", false, "cladefs", 30],
    ]);
    expect(sessionState(s, session)).toBe("working");
    expect(idleLeftMs(session, now)).toBe(50 * 60_000);
    expect(sessionState({ ...s, commands: [] }, session)).toBe("idle");
    expect(sessionState(s, { ...session, endedAt: "2026-11-21T11:55:00Z" })).toBe("ended");
  });
});
