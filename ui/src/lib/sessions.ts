import type { Command, CommandKind, CommandState, FailureKind, FinishedCommand, Session, Snapshot } from "../api";
import { isLive } from "./live";

/** A session nothing touches for this long is abandoned and ends (the controller's sessionIdle). */
export const SESSION_IDLE_MS = 60 * 60 * 1000;

/** working: a command runs or waits; idle: live, nothing running; ended. */
export type SessionState = "working" | "idle" | "ended";

export function sessionState(s: Snapshot, session: Session): SessionState {
  if (session.endedAt !== null) return "ended";
  return s.commands.some((c) => c.sessionId === session.id && isLive(c)) ? "working" : "idle";
}

/** How long until an untouched live session is abandoned, in ms; 0 once it is due. */
export function idleLeftMs(session: Session, now: number): number {
  return Math.max(0, Date.parse(session.lastActiveAt) + SESSION_IDLE_MS - now);
}

/** One command of a session, live or finished, as its timeline lists it. */
export interface SessionCommand {
  id: string;
  state: CommandState;
  kind: CommandKind;
  argv: string;
  repoId: string;
  nodeId: string;
  workspaceId: string;
  /** When it started (or was queued, while it waits). */
  at: number;
  /** How long it ran; for a live command, so far. */
  seconds: number;
  exitCode: number | null;
  failure: FailureKind | null;
  coldSeconds: number | null;
  /** Why it waits or sleeps, while live. */
  reason: string | null;
  url: string | null;
  live: boolean;
}

/**
 * Every command of a session the cockpit knows, newest first: the live ones
 * and those that ended in the last day. A command that just ended can be in
 * both lists; its finished record wins.
 */
export function sessionCommands(s: Snapshot, sessionId: string, repoOf: (workspaceId: string) => string): SessionCommand[] {
  const now = Date.parse(s.now);
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
    at: f.endedAt - f.seconds * 1000,
    seconds: f.seconds,
    exitCode: f.exitCode,
    failure: f.failure,
    coldSeconds: f.coldSeconds,
    reason: null,
    url: null,
    live: false,
  }));
  for (const c of s.commands) {
    if (c.sessionId !== sessionId || done.has(c.id)) continue;
    rows.push(fromCommand(c, now, repoOf(c.workspaceId)));
  }
  return rows.sort((a, b) => b.at - a.at);
}

function fromCommand(c: Command, now: number, repoId: string): SessionCommand {
  const at = Date.parse(c.startedAt ?? c.queuedAt);
  const end = c.endedAt ? Date.parse(c.endedAt) : now;
  return {
    id: c.id,
    state: c.state,
    kind: c.kind,
    argv: c.argv,
    repoId,
    nodeId: c.nodeId,
    workspaceId: c.workspaceId,
    at,
    seconds: (end - at) / 1000,
    exitCode: c.exitCode,
    failure: null,
    coldSeconds: c.coldSeconds,
    reason: c.reason,
    url: c.url,
    live: isLive(c),
  };
}

/** The repo a workspace of the snapshot is of, through its sessions when the workspace is gone. */
export function workspaceRepo(s: Snapshot): (workspaceId: string) => string {
  const repos = new Map<string, string>();
  for (const session of s.sessions) for (const w of session.workspaces) repos.set(w.id, w.repoId);
  for (const w of s.workspaces) repos.set(w.id, w.repoId);
  return (id) => repos.get(id) ?? "";
}
