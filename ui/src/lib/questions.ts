import { QuestionState, type Answer, type AnswerInput, type Question, type Stamp } from "../api";

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

/**
 * An answer as the form holds it: the options picked, whether the user
 * chose to answer in their own words (and what they wrote), and a note.
 */
export interface Draft {
  picked: string[];
  ownWords: boolean;
  other: string;
  notes: string;
}

export const EMPTY_DRAFT: Draft = { picked: [], ownWords: false, other: "", notes: "" };

/** A draft that starts from an answer given before: changing a response in the outbox. */
export function draftOf(a: Answer): Draft {
  return { picked: a.picked, ownWords: a.other !== "", other: a.other, notes: a.notes };
}

/** The answer a draft sends: its own words only while that choice is on. */
export function draftAnswer(d: Draft): AnswerInput {
  return { picked: d.picked, other: d.ownWords ? d.other : "", notes: d.notes };
}

/** A draft the controller takes: something chosen, and words written if own words is. */
export function draftReady(d: Draft): boolean {
  return (d.picked.length > 0 || d.ownWords) && (!d.ownWords || d.other.trim() !== "");
}

/**
 * What clicking option `label` does: a single-choice question picks it
 * alone and submits at once; a multi-select question toggles it and waits
 * for the Answer button.
 */
export function chooseOption(d: Draft, label: string, multi: boolean): { draft: Draft; submit: boolean } {
  if (!multi) return { draft: { ...d, picked: [label], ownWords: false }, submit: true };
  const picked = d.picked.includes(label) ? d.picked.filter((p) => p !== label) : [...d.picked, label];
  return { draft: { ...d, picked }, submit: false };
}

/** Choosing to answer in one's own words: instead of the options on a single choice, toggled beside them on a multi-select. */
export function chooseOwnWords(d: Draft, multi: boolean): Draft {
  if (!multi) return { ...d, picked: [], ownWords: true };
  return { ...d, ownWords: !d.ownWords };
}

/** The user's response in a line: `SQLite; and a note`, or `dismissed: not now`. */
export function responseLine(q: Question): string {
  if (q.dismissed) return q.dismissed.reason ? `dismissed: ${q.dismissed.reason}` : "dismissed";
  if (!q.answer) return "";
  return [...q.answer.picked, q.answer.other].filter((s) => s.trim() !== "").join("; ");
}

/** Who made a change: the session, or "you". */
export function who(st: Stamp): string {
  return st.session || "you";
}

/** The questions a tab of the page shows. */
export type QuestionTab = "inbox" | "outbox" | "handled" | "deleted" | "all";

export const TAB_STATES: Record<QuestionTab, QuestionState[]> = {
  inbox: [QuestionState.OPEN],
  outbox: [QuestionState.ANSWERED, QuestionState.DISMISSED],
  handled: [QuestionState.HANDLED],
  deleted: [QuestionState.DELETED],
  all: [QuestionState.OPEN, QuestionState.ANSWERED, QuestionState.DISMISSED, QuestionState.HANDLED, QuestionState.DELETED],
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

/**
 * Guard a polled list of questions against out-of-order stale responses.
 * Keeps the highest `revision` (breaking ties by `updatedAt`) seen per ID in
 * `seen` and synchronously returns the freshest known version of each incoming
 * question that matches `states` (when provided) and `project` (when non-empty).
 */
export function mergeLatestQuestions(
  seen: Map<number, Question>,
  incoming: readonly Question[],
  states?: readonly QuestionState[],
  project?: string | null,
): Question[] {
  const out: Question[] = [];
  for (const q of incoming) {
    if (project && q.project !== project) continue;
    const prev = seen.get(q.id);
    const fresher =
      !prev || q.revision > prev.revision || (q.revision === prev.revision && q.updatedAt >= prev.updatedAt);
    const best = fresher ? q : prev;
    if (fresher) seen.set(q.id, q);
    if (project && best.project !== project) continue;
    if (!states || states.includes(best.state)) out.push(best);
  }
  return out;
}
