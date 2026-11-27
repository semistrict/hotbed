import { QuestionState, type Answer, type AnswerInput, type Question } from "../api";

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

/** The questions a tab of the page shows. */
export type QuestionTab = "inbox" | "outbox" | "handled" | "deleted" | "all";

export const TAB_STATES: Record<QuestionTab, QuestionState[]> = {
  inbox: [QuestionState.OPEN],
  outbox: [QuestionState.ANSWERED, QuestionState.DISMISSED],
  handled: [QuestionState.HANDLED],
  deleted: [QuestionState.DELETED],
  all: [QuestionState.OPEN, QuestionState.ANSWERED, QuestionState.DISMISSED, QuestionState.HANDLED, QuestionState.DELETED],
};
