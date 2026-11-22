import type { Answer, Question, QuestionState, Stamp } from "../api";

/**
 * Where moving question `moved` to position `to` in the user's order puts
 * it, `to` counted among the others: just before which question, or last
 * (null). Undefined when it would stay where it is.
 */
export function placement(order: number[], moved: number, to: number): { before: number | null } | undefined {
  const others = order.filter((id) => id !== moved);
  const at = Math.max(0, Math.min(to, others.length));
  if (order.indexOf(moved) === at) return undefined;
  return { before: others[at] ?? null };
}

/** The list as it is once `moved` goes just before `before`, or last: what a move shows before the controller confirms it. */
export function reorder<T extends { id: number }>(list: T[], moved: number, before: number | null): T[] {
  const item = list.find((q) => q.id === moved);
  if (!item) return list;
  const others = list.filter((q) => q.id !== moved);
  const at = before === null ? others.length : others.findIndex((q) => q.id === before);
  if (at < 0) return list;
  return [...others.slice(0, at), item, ...others.slice(at)];
}

/** An answer the controller takes: something picked, or words of the user's own. */
export function answerReady(a: Answer): boolean {
  return a.picked.length > 0 || a.other.trim() !== "";
}

/** Picks `label` for an answer: alone for a single choice, toggled among others for multiSelect. */
export function pick(picked: string[], label: string, multi: boolean): string[] {
  if (!multi) return picked[0] === label ? [] : [label];
  return picked.includes(label) ? picked.filter((p) => p !== label) : [...picked, label];
}

/** The user's response in a line: `SQLite; and a note`, or `dismissed: not now`. */
export function responseLine(q: Question): string {
  if (q.dismissed) return q.dismissed.reason ? `dismissed: ${q.dismissed.reason}` : "dismissed";
  if (!q.answer) return "";
  return [...q.answer.picked, q.answer.other].filter((s) => s.trim() !== "").join("; ");
}

/** Who made a change: the session, or "you". */
export function who(st: Stamp): string {
  return st.session ?? "you";
}

/** The questions a tab of the page shows. */
export type QuestionTab = "inbox" | "outbox" | "handled" | "deleted" | "all";

export const TAB_STATES: Record<QuestionTab, QuestionState[]> = {
  inbox: ["open"],
  outbox: ["answered", "dismissed"],
  handled: ["handled"],
  deleted: ["deleted"],
  all: ["open", "answered", "dismissed", "handled", "deleted"],
};

/** A commit as people say it: its first 8 hex digits. */
export function shortHead(head: string): string {
  return head.slice(0, 8);
}

/**
 * Where a question dragged onto another goes, counted among the others: just
 * before the target, or just after it. Undefined for a drop on itself, which
 * moves nothing.
 */
export function dropAt(order: number[], dragged: number, target: number, after: boolean): number | undefined {
  if (dragged === target) return undefined;
  const at = order.filter((id) => id !== dragged).indexOf(target);
  return at < 0 ? undefined : at + (after ? 1 : 0);
}
