import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";
import { QuestionState } from "../api";
import { QuestionSchema } from "../gen/hotbed/v1/hotbed_pb";
import { mergeLatestQuestions, TAB_STATES } from "./questions";

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

describe("mergeLatestQuestions", () => {
  it("returns incoming questions synchronously on first call with an empty seen map", () => {
    const seen = new Map();
    const q1 = makeQuestion(1, 1n, QuestionState.OPEN);
    const q2 = makeQuestion(2, 1n, QuestionState.OPEN);

    const out = mergeLatestQuestions(seen, [q1, q2], TAB_STATES.inbox);
    expect(out.map((q) => q.id)).toEqual([1, 2]);
    expect(seen.size).toBe(2);
  });

  it("never regresses a newer revision to an older revision from a stale poll", () => {
    const seen = new Map();
    const newer = makeQuestion(1, 3n, QuestionState.ANSWERED);
    expect(mergeLatestQuestions(seen, [newer], TAB_STATES.outbox)).toEqual([newer]);

    // Stale poll returns revision 2 with state OPEN.
    const stale = makeQuestion(1, 2n, QuestionState.OPEN);
    // In inbox, the stale OPEN poll must NOT resurface an already-answered question.
    expect(mergeLatestQuestions(seen, [stale], TAB_STATES.inbox)).toEqual([]);
    // In outbox (or unfiltered), the newer revision wins.
    expect(mergeLatestQuestions(seen, [stale], TAB_STATES.outbox)).toEqual([newer]);
    expect(seen.get(1)?.revision).toBe(3n);
  });

  it("advances to a newer revision and breaks equal-revision ties by updatedAt", () => {
    const seen = new Map();
    const v1 = makeQuestion(5, 2n, QuestionState.OPEN, 2000n);
    const v1Older = makeQuestion(5, 2n, QuestionState.OPEN, 1500n);
    const v2 = makeQuestion(5, 3n, QuestionState.OPEN, 2500n);

    mergeLatestQuestions(seen, [v1]);
    expect(mergeLatestQuestions(seen, [v1Older])[0]?.updatedAt).toBe(2000n);
    expect(mergeLatestQuestions(seen, [v2])[0]?.revision).toBe(3n);
  });

  it("immediately filters out another project's questions when switching projects", () => {
    const seen = new Map();
    const qA1 = makeQuestion(1, 1n, QuestionState.OPEN, 1000n, "project-a");
    const qA2 = makeQuestion(2, 1n, QuestionState.OPEN, 1000n, "project-a");
    const qB1 = makeQuestion(3, 1n, QuestionState.OPEN, 1000n, "project-b");

    // Viewing project-a returns project-a's questions.
    expect(mergeLatestQuestions(seen, [qA1, qA2], TAB_STATES.inbox, "project-a").map((q) => q.id)).toEqual([1, 2]);

    // Switching to project-b while stale project-a rows are still present filters them out immediately.
    const seenB = new Map();
    expect(mergeLatestQuestions(seenB, [qA1, qA2], TAB_STATES.inbox, "project-b")).toEqual([]);
    expect(seenB.size).toBe(0);

    // Mixed or project-b rows only return project-b's questions.
    expect(mergeLatestQuestions(seenB, [qA1, qB1], TAB_STATES.inbox, "project-b").map((q) => q.id)).toEqual([3]);
    expect(Array.from(seenB.keys())).toEqual([3]);

    // Unscoped (null = every project) keeps questions from all projects.
    const seenAll = new Map();
    expect(mergeLatestQuestions(seenAll, [qA1, qB1], TAB_STATES.inbox, null).map((q) => q.id)).toEqual([1, 3]);
  });
});
