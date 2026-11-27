import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";
import { MemoryContentSchema, MemoryMode, MemorySchema, MemoryScope, MemoryState, QuestionSchema, QuestionState } from "../api";
import { TAB_STATES } from "./questions";
import { dropAt, mergeLatestByRevision, placement, reorder } from "./rankedList";

const inProject = (project: string) => (q: { project: string }) => q.project === project;

function mkMemory(id: number, summary: string, state: MemoryState, revision: bigint) {
  return create(MemorySchema, {
    id,
    scope: MemoryScope.GLOBAL,
    state,
    revision,
    updatedAt: 100n,
    content: create(MemoryContentSchema, { slug: "use-neon", summary, body: "details", mode: MemoryMode.ALWAYS }),
  });
}

function makeQuestion(id: number, revision: bigint, state: QuestionState, updatedAt = revision * 1000n, project = "hotbed") {
  return create(QuestionSchema, {
    id,
    project,
    state,
    revision,
    updatedAt,
    prompt: { header: "Q", question: `Question ${id} rev ${revision}` },
  });
}

describe("mergeLatestByRevision", () => {
  it("returns incoming questions synchronously on first call with an empty seen map", () => {
    const seen = new Map();
    const q1 = makeQuestion(1, 1n, QuestionState.OPEN);
    const q2 = makeQuestion(2, 1n, QuestionState.OPEN);

    const out = mergeLatestByRevision(seen, [q1, q2], TAB_STATES.inbox);
    expect(out.map((q) => q.id)).toEqual([1, 2]);
    expect(seen.size).toBe(2);
  });

  it("never regresses a newer revision to an older revision from a stale poll", () => {
    const seen = new Map();
    const newer = makeQuestion(1, 3n, QuestionState.ANSWERED);
    expect(mergeLatestByRevision(seen, [newer], TAB_STATES.outbox)).toEqual([newer]);

    // Stale poll returns revision 2 with state OPEN.
    const stale = makeQuestion(1, 2n, QuestionState.OPEN);
    // In inbox, the stale OPEN poll must NOT resurface an already-answered question.
    expect(mergeLatestByRevision(seen, [stale], TAB_STATES.inbox)).toEqual([]);
    // In outbox (or unfiltered), the newer revision wins.
    expect(mergeLatestByRevision(seen, [stale], TAB_STATES.outbox)).toEqual([newer]);
    expect(seen.get(1)?.revision).toBe(3n);
  });

  it("advances to a newer revision and breaks equal-revision ties by updatedAt", () => {
    const seen = new Map();
    const v1 = makeQuestion(5, 2n, QuestionState.OPEN, 2000n);
    const v1Older = makeQuestion(5, 2n, QuestionState.OPEN, 1500n);
    const v2 = makeQuestion(5, 3n, QuestionState.OPEN, 2500n);

    mergeLatestByRevision(seen, [v1]);
    expect(mergeLatestByRevision(seen, [v1Older])[0]?.updatedAt).toBe(2000n);
    expect(mergeLatestByRevision(seen, [v2])[0]?.revision).toBe(3n);
  });

  it("immediately filters out what `keep` rejects, such as another project's questions after a switch", () => {
    const seen = new Map();
    const qA1 = makeQuestion(1, 1n, QuestionState.OPEN, 1000n, "project-a");
    const qA2 = makeQuestion(2, 1n, QuestionState.OPEN, 1000n, "project-a");
    const qB1 = makeQuestion(3, 1n, QuestionState.OPEN, 1000n, "project-b");

    // Viewing project-a returns project-a's questions.
    expect(mergeLatestByRevision(seen, [qA1, qA2], TAB_STATES.inbox, inProject("project-a")).map((q) => q.id)).toEqual([1, 2]);

    // Switching to project-b while stale project-a rows are still present filters them out immediately.
    const seenB = new Map();
    expect(mergeLatestByRevision(seenB, [qA1, qA2], TAB_STATES.inbox, inProject("project-b"))).toEqual([]);
    expect(seenB.size).toBe(0);

    // Mixed or project-b rows only return project-b's questions.
    expect(mergeLatestByRevision(seenB, [qA1, qB1], TAB_STATES.inbox, inProject("project-b")).map((q) => q.id)).toEqual([3]);
    expect(Array.from(seenB.keys())).toEqual([3]);

    // Unscoped (null = every project) keeps questions from all projects.
    const seenAll = new Map();
    expect(mergeLatestByRevision(seenAll, [qA1, qB1], TAB_STATES.inbox).map((q) => q.id)).toEqual([1, 3]);
  });

  it("keeps the highest revision seen when a stale poll arrives", () => {
    const seen = new Map();
    const newer = mkMemory(1, "Updated summary", MemoryState.APPROVED, 3n);
    mergeLatestByRevision(seen, [newer]);
    const stale = mkMemory(1, "Old summary", MemoryState.PROPOSED, 2n);
    const merged = mergeLatestByRevision(seen, [stale], [MemoryState.PROPOSED]);
    expect(merged).toEqual([]);
    const approved = mergeLatestByRevision(seen, [stale], [MemoryState.APPROVED]);
    expect(approved).toHaveLength(1);
    expect(approved[0]?.content?.summary).toBe("Updated summary");
  });
});

describe("moving an item in the user's order", () => {
  const order = [1, 2, 3, 4];

  it("places it before the item now at the target position, or last", () => {
    expect(placement(order, 4, 0)).toEqual({ before: 1 });
    expect(placement(order, 1, 3)).toEqual({ before: null });
    expect(placement(order, 2, 2)).toEqual({ before: 4 });
    expect(placement(order, 2, 1)).toBeUndefined();
  });

  it("shows the list in its new order before the controller confirms it", () => {
    const list = order.map((id) => ({ id }));
    expect(reorder(list, 4, 1).map((x) => x.id)).toEqual([4, 1, 2, 3]);
    expect(reorder(list, 1, null).map((x) => x.id)).toEqual([2, 3, 4, 1]);
    expect(reorder(list, 9, 1)).toBe(list);
  });

  it("drops a dragged item just before or after its target, and nowhere on itself", () => {
    expect(dropAt(order, 1, 3, false)).toBe(1);
    expect(dropAt(order, 1, 3, true)).toBe(2);
    expect(dropAt(order, 4, 1, false)).toBe(0);
    expect(dropAt(order, 2, 2, true)).toBeUndefined();
  });
});
