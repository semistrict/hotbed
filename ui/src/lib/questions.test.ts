import { describe, expect, it } from "vitest";
import type { Question } from "../api";
import { answerReady, dropAt, pick, placement, reorder, responseLine } from "./questions";

describe("placement", () => {
  const order = [5, 3, 1, 2];
  it("puts a question before the one now at its new place", () => {
    expect(placement(order, 1, 0)).toEqual({ before: 5 });
    expect(placement(order, 5, 1)).toEqual({ before: 1 });
  });
  it("puts it last past the end", () => {
    expect(placement(order, 3, 3)).toEqual({ before: null });
    expect(placement(order, 3, 9)).toEqual({ before: null });
  });
  it("says nothing moves when it stays put", () => {
    expect(placement(order, 1, 2)).toBeUndefined();
    expect(placement(order, 5, -1)).toBeUndefined();
  });
});

describe("reorder", () => {
  const list = [{ id: 5 }, { id: 3 }, { id: 1 }];
  it("moves an item before another, or last", () => {
    expect(reorder(list, 1, 5).map((q) => q.id)).toEqual([1, 5, 3]);
    expect(reorder(list, 5, null).map((q) => q.id)).toEqual([3, 1, 5]);
  });
  it("leaves the list be when the move names what it lacks", () => {
    expect(reorder(list, 9, 5)).toBe(list);
    expect(reorder(list, 5, 9)).toBe(list);
  });
});

describe("pick", () => {
  it("picks one of a single choice, and unpicks it", () => {
    expect(pick([], "A", false)).toEqual(["A"]);
    expect(pick(["A"], "B", false)).toEqual(["B"]);
    expect(pick(["A"], "A", false)).toEqual([]);
  });
  it("toggles among several for multiSelect", () => {
    expect(pick(["A"], "B", true)).toEqual(["A", "B"]);
    expect(pick(["A", "B"], "A", true)).toEqual(["B"]);
  });
});

describe("answerReady", () => {
  it("wants a pick or words of the user's own", () => {
    expect(answerReady({ picked: [], other: " ", notes: "only a note" })).toBe(false);
    expect(answerReady({ picked: ["A"], other: "", notes: "" })).toBe(true);
    expect(answerReady({ picked: [], other: "neither", notes: "" })).toBe(true);
  });
});

describe("responseLine", () => {
  const base = { answer: null, dismissed: null } as unknown as Question;
  it("says what was picked and written, or why it was dismissed", () => {
    expect(responseLine({ ...base, answer: { picked: ["linux", "darwin"], other: "freebsd later", notes: "" } })).toBe("linux; darwin; freebsd later");
    expect(responseLine({ ...base, dismissed: { at: "", session: null, host: "laptop", reason: "after the release" } })).toBe("dismissed: after the release");
    expect(responseLine({ ...base, dismissed: { at: "", session: null, host: "laptop", reason: "" } })).toBe("dismissed");
  });
});

describe("dropAt", () => {
  const order = [3, 2, 1];
  it("puts a dragged question before or after the one it is dropped on", () => {
    expect(dropAt(order, 1, 3, false)).toBe(0);
    expect(dropAt(order, 1, 3, true)).toBe(1);
    expect(dropAt(order, 3, 1, true)).toBe(2);
  });
  it("moves nothing dropped on itself, or on what the list lacks", () => {
    expect(dropAt(order, 1, 1, false)).toBeUndefined();
    expect(dropAt(order, 1, 1, true)).toBeUndefined();
    expect(dropAt(order, 1, 9, true)).toBeUndefined();
  });
  it("leaves a question in place when dropped just beside itself", () => {
    const to = dropAt(order, 1, 2, true);
    expect(to).toBe(2);
    expect(placement(order, 1, to as number)).toBeUndefined();
  });
});
