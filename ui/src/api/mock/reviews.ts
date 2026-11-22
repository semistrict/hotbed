import type { Finding, Location, Quick, Review, ReviewEvent, Stamp } from "../types";

const MIN = 60_000;

type Reported = Omit<Finding, "id" | "reviewId" | "decision" | "decided" | "found" | "quick" | "responses">;

/**
 * The simulated cluster's reviews, kept as the controller keeps them: every
 * change and response an event, findings counted from them. A finished
 * review of hotbed's questions with findings of every kind, one under way in
 * cladefs, and one waiting for a reviewer.
 */
export class MockReviews {
  private reviews: Review[] = [];
  private findings: Finding[] = [];
  private events: ReviewEvent[] = [];

  constructor(now: number) {
    const at = (m: number, session: string | null, reason = ""): Stamp => ({ at: new Date(now - m * MIN).toISOString(), session, host: session ? "asahimbp" : "laptop", reason });

    const done = this.request("hotbed", at(55, "brave-otter"), {
      title: "Questions: projects, outbox and history",
      instructions: "Look for races in the store's transactions and wrong state transitions. The UI's drag and drop too.",
      repo: "github.com/semistrict/hotbed",
      base: "8d70878e1f0c2b4a",
      head: "6cb0ca1f3730c4d2",
      branch: "questions",
      snapshotOf: null,
    });
    this.start(done, at(52, "calm-heron"));
    this.find(done, at(40, "calm-heron"), {
      title: "Dropping a card on itself moves it to the top",
      summary: "onDragOver fires on the dragged card too, so a drop on itself computes index -1, which placement clamps to 0.",
      failureScenario: "Inbox [3,2,1]; drag card 1 and release it where it started: it jumps to the top.",
      category: "correctness",
      severity: "important",
      verdict: "confirmed",
      locations: [
        loc("ui/src/pages/Questions.tsx", 134, 145, "const dropped = (e: DragEvent) => {\n  e.preventDefault();\n  if (dragged !== null && drop) {\n    const others = order.filter((x) => x !== dragged);\n    moveTo(dragged, others.indexOf(drop.id) + (drop.after ? 1 : 0));\n  }"),
        loc("ui/src/lib/questions.ts", 8, 13, "export function placement(order: number[], moved: number, to: number) {\n  const others = order.filter((id) => id !== moved);\n  const at = Math.max(0, Math.min(to, others.length));"),
      ],
    });
    this.find(done, at(38, "calm-heron"), {
      title: "ListSessions drops sessions past 500",
      summary: "RecentSessions caps at 500 rows across every project before the project filter runs.",
      failureScenario: "600 sessions in the last day: session ls -p hotbed misses some of hotbed's.",
      category: "correctness",
      severity: "pre-existing",
      verdict: "plausible",
      locations: [loc("internal/controller/sessions.go", 62, 71, "")],
    });
    this.find(done, at(36, "calm-heron"), {
      title: "Wait with no IDs never blocks once an answer is left",
      summary: "Done means the project's outbox is not empty, whoever asked; one unacked answer ends every wait at once.",
      failureScenario: "",
      category: "correctness",
      severity: "important",
      verdict: null,
      locations: [],
    });
    this.find(done, at(35, "calm-heron"), {
      title: "Doc comment repeats itself",
      summary: "Handler's doc comment says the same thing twice.",
      failureScenario: "",
      category: "docs",
      severity: "nit",
      verdict: null,
      locations: [loc("internal/controller/server.go", 106, 107, "// Handler serves the API and the cockpit to the cluster's owner only.\n// Handler serves the API and the cockpit, behind the gate that admits the")],
    });
    this.finish(done, at(34, "calm-heron"), "Two important bugs (drag and drop, wait), one pre-existing limit, one nit.");
    this.decide(done, 1, "accepted", at(30, "brave-otter", "fixing with a test"));
    this.decide(done, 4, "dismissed", at(29, "brave-otter", "pre-existing; leaving it"));
    this.respond(done, 3, "fix-if-verified", "and a test that leaves an answer behind", at(12, null));
    this.respond(done, null, null, "Good catch on the drag one.", at(11, null));

    const going = this.request("cladefs", at(20, "keen-badger"), {
      title: "virtiofs DAX window sizing",
      instructions: "",
      repo: "github.com/semistrict/cladefs",
      base: "1f2e3d4c5b6a7980",
      head: "0a9b8c7d6e5f4a3b",
      branch: "dax-window",
      snapshotOf: "5c4d3e2f1a0b9c8d",
    });
    this.start(going, at(18, "steady-gnu"));
    this.find(going, at(6, "steady-gnu"), {
      title: "Window size ignores huge pages",
      summary: "The DAX window is sized in 4 KiB pages even when the guest maps 2 MiB ones.",
      failureScenario: "A guest with hugepages=on maps past the window's end and faults.",
      category: "correctness",
      severity: "important",
      verdict: "plausible",
      locations: [loc("src/dax.rs", 88, 92, "let pages = len / 4096;\nlet window = pages * PAGE;")],
    });

    this.request("hotbed", at(3, "lucid-wren"), {
      title: "Reviews: data model and cockpit",
      instructions: "Check every RPC has a CLI command and a test.",
      repo: "github.com/semistrict/hotbed",
      base: "6cb0ca1f3730c4d2",
      head: "a1b2c3d4e5f60718",
      branch: "reviews",
      snapshotOf: null,
    });
  }

  list(project: string | null): Review[] {
    return [...this.reviews]
      .filter((r) => project === null || r.project === project)
      .sort((a, b) => b.requested.at.localeCompare(a.requested.at))
      .map((r) => ({ ...this.filled(r), findings: [], events: [] }));
  }

  get(id: number): Review {
    const r = this.reviews.find((x) => x.id === id);
    if (!r) throw new Error(`no review ${id}`);
    return this.filled(r);
  }

  /** A project's reviews requested or in progress, and those done with findings open. */
  counts(project: string): { active: number; toDecide: number } {
    const mine = this.list(project);
    return {
      active: mine.filter((r) => r.state === "requested" || r.state === "in-progress").length,
      toDecide: mine.filter((r) => r.state === "done" && r.counts.open > 0).length,
    };
  }

  respond(review: number, finding: number | null, quick: Quick | null, text: string, by: Stamp = user()): ReviewEvent {
    this.get(review);
    if (!quick && text.trim() === "") throw new Error("a response gives a quick response, a comment, or both");
    if (quick && finding === null) throw new Error("a quick response is to a finding: name it");
    if (finding !== null && !this.findings.some((f) => f.id === finding && f.reviewId === review)) throw new Error(`review ${review} has no finding ${finding}`);
    return structuredClone(this.event(review, finding, "responded", by, quick, text));
  }

  private filled(r: Review): Review {
    const findings = this.findings.filter((f) => f.reviewId === r.id).map((f) => ({ ...f }));
    const events = this.events.filter((e) => e.reviewId === r.id);
    for (const e of events) {
      const f = findings.find((x) => x.id === e.findingId);
      if (e.what !== "responded" || !f) continue;
      f.responses++;
      if (e.quick) f.quick = e.quick;
    }
    return structuredClone({
      ...r,
      findings,
      events,
      counts: {
        important: findings.filter((f) => f.severity === "important").length,
        preExisting: findings.filter((f) => f.severity === "pre-existing").length,
        nits: findings.filter((f) => f.severity === "nit").length,
        open: findings.filter((f) => f.decision === "open").length,
        responses: events.filter((e) => e.what === "responded").length,
      },
    });
  }

  private request(project: string, by: Stamp, v: Pick<Review, "title" | "instructions" | "repo" | "base" | "head" | "branch" | "snapshotOf">): number {
    const id = this.reviews.length + 1;
    this.reviews.push({
      id, project, ...v, state: "requested", summary: "", requested: by, started: null, ended: null, updatedAt: by.at,
      findings: [], events: [], counts: { important: 0, preExisting: 0, nits: 0, open: 0, responses: 0 },
    });
    this.event(id, null, "requested", by, null, "");
    return id;
  }

  private start(id: number, by: Stamp) {
    Object.assign(this.review(id), { state: "in-progress", started: by });
    this.event(id, null, "started", by, null, "");
  }

  private find(id: number, by: Stamp, f: Reported) {
    const finding: Finding = { ...f, id: this.findings.length + 1, reviewId: id, decision: "open", decided: null, found: by, quick: null, responses: 0 };
    this.findings.push(finding);
    this.event(id, finding.id, "found", by, null, "");
  }

  private finish(id: number, by: Stamp, summary: string) {
    Object.assign(this.review(id), { state: "done", summary, ended: by });
    this.event(id, null, "finished", by, null, "");
  }

  private decide(id: number, finding: number, decision: Finding["decision"], by: Stamp) {
    const f = this.findings.find((x) => x.id === finding);
    if (!f) throw new Error(`no finding ${finding}`);
    Object.assign(f, { decision, decided: by });
    this.event(id, finding, decision, by, null, by.reason);
  }

  private review(id: number): Review {
    const r = this.reviews.find((x) => x.id === id);
    if (!r) throw new Error(`no review ${id}`);
    return r;
  }

  private event(review: number, findingId: number | null, what: string, by: Stamp, quick: Quick | null, text: string): ReviewEvent {
    const e: ReviewEvent = { seq: this.events.length + 1, reviewId: review, findingId, what, by, quick, text };
    this.events.push(e);
    this.review(review).updatedAt = by.at;
    return e;
  }
}

function loc(path: string, startLine: number, endLine: number, excerpt: string): Location {
  return { path, startLine, endLine, excerpt };
}

/** The user, in the cockpit: no session. */
function user(): Stamp {
  return { at: new Date().toISOString(), session: null, host: "laptop", reason: "" };
}
