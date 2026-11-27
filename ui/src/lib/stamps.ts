import { ActorKind, type Stamp } from "../api";

/** True when a stamp represents a human user (explicit USER or legacy empty session). */
export function isUserActor(st?: Stamp | null): boolean {
  if (!st) return false;
  if (st.actorKind === ActorKind.USER) return true;
  if (st.actorKind === ActorKind.AGENT) return false;
  return st.session === "";
}

/** Who made a change, in a word: the session, or "you". */
export function who(st: Stamp): string {
  return st.session || "you";
}

/** Short human-readable actor summary, distinguishing User vs Agent (model). */
export function actorSummary(st?: Stamp | null): string {
  if (!st) return "unknown";
  if (isUserActor(st)) {
    return st.user ? `User (${st.user})` : "User";
  }
  const parts: string[] = [];
  if (st.model) parts.push(st.model);
  if (st.subagent) parts.push(st.subagent);
  if (st.session) parts.push(st.session);
  return parts.length > 0 ? `Agent (${parts.join(" · ")})` : "Agent";
}
