import type { Answer, Prompt, Question, QuestionChange, QuestionFilter, QuestionList, QuestionState, Stamp } from "../types";

const MIN = 60_000;

/**
 * The simulated cluster's questions, kept as the controller keeps them:
 * each change saved with who made it, nothing removed. It holds to the
 * controller's rules, so the page meets the same refusals it would.
 */
export class MockQuestions {
  private questions: Question[] = [];
  private changes: { id: number; change: QuestionChange }[] = [];
  private seq = 0;

  constructor(now: number) {
    const ago = (m: number) => now - m * MIN;
    const at = (m: number, session: string | null, reason = ""): Stamp => ({ at: new Date(ago(m)).toISOString(), session, host: session ? "asahimbp" : "laptop", reason });
    const from = { dir: "/Users/ramon/src/hotbed", remote: "git@github.com:semistrict/hotbed.git", branch: "questions", head: "8d708783e1f0c2b4", dirtyFiles: 3, dirty: [" M internal/controller/questions.go", " M proto/hotbed/v1/hotbed.proto", "?? cmd/hotbed/question.go"] };

    this.ask("hotbed", at(190, "calm-heron"), from, {
      question: "Should handled answers stay in the cockpit's All tab forever, or age out after 30 days?",
      header: "Retention",
      multiSelect: false,
      options: [
        { label: "Forever (Recommended)", description: "Nothing is ever removed; the list is small.", preview: "" },
        { label: "30 days", description: "Older handled questions drop out of the All tab; the history keeps them.", preview: "" },
      ],
    });
    this.answer(1, { picked: ["Forever (Recommended)"], other: "", notes: "" }, at(170, null));
    this.handle(1, at(160, "calm-heron"));

    this.ask("hotbed", at(95, "brave-otter"), from, {
      question: "The parked command's swap stays high after it thaws. Reclaim it on thaw?",
      header: "Swap",
      multiSelect: false,
      options: [
        { label: "Leave it", description: "The kernel pages it back in as it is touched.", preview: "" },
        { label: "Swap off and on", description: "Forces it back now, at a burst of IO.", preview: "" },
      ],
    });
    this.delete(2, at(80, "brave-otter", "measured: the kernel brings it back within a minute"));

    this.ask("cladefs", at(42, "brave-otter"), { ...from, dir: "/Users/ramon/src/cladefs", remote: "git@github.com:semistrict/cladefs.git", branch: "nightly" }, {
      question: "Which machines should a nightly measurement run on?",
      header: "Nightly",
      multiSelect: true,
      options: [
        { label: "ubu1", description: "The controller, amd64, 16 cores.", preview: "" },
        { label: "asahimbp", description: "M3 Pro on Asahi; the only arm64.", preview: "" },
        { label: "oldpc", description: "Quiet, slow: steadiest numbers.", preview: "" },
      ],
    });
    this.answer(3, { picked: ["ubu1", "oldpc"], other: "", notes: "asahimbp when it has its bench CPUs" }, at(30, null));

    this.ask("hotbed", at(18, "quiet-lynx"), null, {
      question: "How should a step that fails on one node but not others be reported?",
      header: "Step fails",
      multiSelect: false,
      options: [
        { label: "Per node (Recommended)", description: "Each node's newest base says how its run went.", preview: "step ls -r app\n  build      ubu1 ✓  asahimbp ✕ exited 2" },
        { label: "Worst wins", description: "Failed anywhere is failed.", preview: "step ls -r app\n  build      ✕ failed on asahimbp" },
      ],
    });
    this.ask("hotbed", at(6, "brave-otter"), from, {
      question: "Name for the outbox's acknowledged state in the CLI?",
      header: "Naming",
      multiSelect: false,
      options: [
        { label: "handled", description: "An agent handled the answer.", preview: "" },
        { label: "archived", description: "Put away, still searchable.", preview: "" },
        { label: "done", description: "Short, but says little about who.", preview: "" },
      ],
    });
  }

  list({ project, states = [], order = "newest" }: QuestionFilter): QuestionList {
    const want: QuestionState[] = states.length > 0 ? states : ["open", "answered", "dismissed", "handled"];
    const picked = this.questions.filter((q) => want.includes(q.state) && (!project || q.project === project));
    const by: Record<NonNullable<QuestionFilter["order"]>, (a: Question, b: Question) => number> = {
      newest: (a, b) => b.asked.at.localeCompare(a.asked.at) || b.id - a.id,
      oldest: (a, b) => a.asked.at.localeCompare(b.asked.at) || a.id - b.id,
      updated: (a, b) => b.updatedAt.localeCompare(a.updatedAt) || b.id - a.id,
      rank: (a, b) => a.rank - b.rank || a.id - b.id,
    };
    return { questions: structuredClone(picked.sort(by[order])), total: picked.length };
  }

  history(id: number): QuestionChange[] {
    this.find(id);
    return structuredClone(this.changes.filter((c) => c.id === id).map((c) => c.change));
  }

  /** A project's questions waiting on the user and on agents; every project's for null. */
  counts(project: string | null): { open: number; outbox: number } {
    const mine = this.questions.filter((q) => project === null || q.project === project);
    return { open: mine.filter((q) => q.state === "open").length, outbox: mine.filter((q) => q.state === "answered" || q.state === "dismissed").length };
  }

  answer(id: number, answer: Answer, by: Stamp = user()): Question {
    const q = this.open(id, "answered");
    if (answer.picked.length === 0 && answer.other.trim() === "") throw new Error("an answer picks an option or says something of its own");
    if (answer.picked.length > 1 && !q.prompt.multiSelect) throw new Error(`question ${id} takes one option`);
    const missing = answer.picked.find((p) => !q.prompt.options.some((o) => o.label === p));
    if (missing) throw new Error(`question ${id} has no option "${missing}"`);
    return this.change(q, "answered", by, () => Object.assign(q, { state: "answered", answer, answered: by }));
  }

  dismiss(id: number, reason: string, by: Stamp = user(reason)): Question {
    const q = this.open(id, "dismissed");
    return this.change(q, "dismissed", by, () => Object.assign(q, { state: "dismissed", dismissed: by }));
  }

  move(id: number, before: number | null, by: Stamp = user()): Question {
    const q = this.open(id, "moved");
    let rank = Math.max(0, ...this.questions.map((x) => x.rank)) + 1;
    if (before !== null) {
      const next = this.find(before);
      if (next.state !== "open") throw new Error(`question ${before} is ${next.state}: only open questions have a place in the order`);
      rank = next.rank;
      for (const x of this.questions) if (x.rank >= rank && x.id !== id) x.rank++;
    }
    return this.change(q, "moved", by, () => (q.rank = rank));
  }

  private ask(project: string, by: Stamp, askedFrom: Question["askedFrom"], prompt: Prompt) {
    const top = Math.min(0, ...this.questions.map((q) => q.rank));
    const q: Question = {
      id: this.questions.length + 1,
      project,
      repo: askedFrom ? `github.com/semistrict/${project}` : null,
      prompt,
      state: "open",
      rank: top - 1,
      answer: null,
      asked: by,
      askedFrom,
      edited: null,
      answered: null,
      dismissed: null,
      handled: null,
      deleted: null,
      updatedAt: by.at,
    };
    this.questions.push(q);
    this.change(q, "asked", by, () => {});
  }

  private handle(id: number, by: Stamp) {
    const q = this.find(id);
    this.change(q, "handled", by, () => Object.assign(q, { state: "handled", handled: by }));
  }

  private delete(id: number, by: Stamp) {
    const q = this.find(id);
    this.change(q, "deleted", by, () => Object.assign(q, { state: "deleted", deleted: by }));
  }

  private change(q: Question, what: string, by: Stamp, edit: () => void): Question {
    edit();
    if (what !== "moved") q.updatedAt = by.at;
    this.changes.push({ id: q.id, change: { seq: ++this.seq, what, by, question: structuredClone(q) } });
    return structuredClone(q);
  }

  private open(id: number, done: string): Question {
    const q = this.find(id);
    if (q.state !== "open") throw new Error(`question ${id} is ${q.state}; only an open question can be ${done}`);
    return q;
  }

  private find(id: number): Question {
    const q = this.questions.find((x) => x.id === id);
    if (!q) throw new Error(`no question ${id}`);
    return q;
  }
}

/** The user, in the cockpit: no session. */
function user(reason = ""): Stamp {
  return { at: new Date().toISOString(), session: null, host: "laptop", reason };
}
