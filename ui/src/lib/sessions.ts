import type { Command, CommandState, FinishedCommand, Session, Snapshot } from "../api";
import { isLive } from "./live";

/** A session nothing touches for this long is abandoned and ends (the controller's sessionIdle). */
export const SESSION_IDLE_MS = 60 * 60 * 1000;

/** working: a command runs or waits; idle: live, nothing running; ended. */
export type SessionState = "working" | "idle" | "ended";

export function sessionState(s: Snapshot, session: Session): SessionState {
  if (session.endedAt !== undefined) return "ended";
  return s.commands.some((c) => c.sessionId === session.id && isLive(c)) ? "working" : "idle";
}

/** How long until an untouched live session is abandoned, in ms; 0 once it is due. */
export function idleLeftMs(session: Session, now: number | bigint): number {
  return Math.max(0, Number(session.lastActiveAt) + SESSION_IDLE_MS - Number(now));
}

/** One command of a session, live or finished, as its timeline lists it. */
export interface SessionCommand {
  id: string;
  state: CommandState;
  kind: string;
  argv: string;
  args?: string[];
  repoId: string;
  nodeId: string;
  workspaceId: string;
  /** When it started (or was queued, while it waits). */
  at: number;
  /** How long it ran; for a live command, so far. */
  seconds: number;
  exitCode: number | null;
  failure: string;
  coldSeconds: number | null;
  /** Why it waits or sleeps, while live. */
  reason: string;
  url: string;
  bench: boolean;
  live: boolean;
}

/**
 * Every command of a session the cockpit knows, newest first: the live ones
 * and those that ended in the last day. A command that just ended can be in
 * both lists; its finished record wins.
 */
export function sessionCommands(s: Snapshot, sessionId: string): SessionCommand[] {
  const now = Number(s.now);
  const done = new Map<string, FinishedCommand>();
  for (const f of s.history) if (f.sessionId === sessionId) done.set(f.id, f);
  const rows: SessionCommand[] = [...done.values()].map((f) => ({
    id: f.id,
    state: f.state,
    kind: f.kind,
    argv: f.argv,
    repoId: f.repoId,
    nodeId: f.nodeId,
    workspaceId: f.workspaceId,
    at: Number(f.endedAt) - f.seconds * 1000,
    seconds: f.seconds,
    exitCode: f.exitCode ?? null,
    failure: f.failure,
    coldSeconds: f.coldSeconds ?? null,
    reason: "",
    url: "",
    bench: f.bench,
    live: false,
  }));
  for (const c of s.commands) {
    if (c.sessionId !== sessionId || done.has(c.id)) continue;
    rows.push(fromCommand(c, now));
  }
  return rows.sort((a, b) => b.at - a.at);
}

/** A session's commands as ListCommands gives them (from history), newest first. */
export function commandRows(commands: Command[], now: number): SessionCommand[] {
  return commands.map((c) => fromCommand(c, now)).sort((a, b) => b.at - a.at);
}

function fromCommand(c: Command, now: number): SessionCommand {
  const at = Number(c.startedAt ?? c.queuedAt);
  const end = c.endedAt !== undefined ? Number(c.endedAt) : now;
  return {
    id: c.id,
    state: c.state,
    kind: c.kind,
    argv: c.argv,
    args: c.args,
    repoId: c.repo,
    nodeId: c.nodeId,
    workspaceId: c.workspaceId,
    at,
    seconds: (end - at) / 1000,
    exitCode: c.exitCode ?? null,
    failure: "",
    coldSeconds: c.coldSeconds ?? null,
    reason: c.reason,
    url: c.url,
    bench: c.bench,
    live: isLive(c),
  };
}
