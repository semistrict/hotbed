import { describe, expect, it } from "vitest";
import type { Finding, Review, ReviewEvent } from "../api";
import { bySeverity, numbered, thread, versionLine, where } from "./reviews";

const finding = (id: number, severity: Finding["severity"], decision: Finding["decision"]) => ({ id, severity, decision }) as Finding;

describe("bySeverity", () => {
  it("groups the worst first, undecided first within each, leaving out empty groups", () => {
    const groups = bySeverity([finding(1, "nit", "open"), finding(2, "important", "accepted"), finding(3, "important", "open"), finding(4, "nit", "dismissed")]);
    expect(groups.map((g) => [g.severity, g.findings.map((f) => f.id)])).toEqual([
      ["important", [3, 2]],
      ["nit", [1, 4]],
    ]);
  });
});

describe("thread", () => {
  const e = (seq: number, what: string, findingId: number | null) => ({ seq, what, findingId }) as ReviewEvent;
  it("keeps a finding's responses, or the review's own", () => {
    const events = [e(1, "found", 1), e(2, "responded", 1), e(3, "responded", null), e(4, "accepted", 1), e(5, "responded", 1)];
    expect(thread(events, 1).map((x) => x.seq)).toEqual([2, 5]);
    expect(thread(events, null).map((x) => x.seq)).toEqual([3]);
  });
});

describe("locations", () => {
  it("number an excerpt from its first line", () => {
    expect(numbered({ path: "a.go", startLine: 12, endLine: 13, excerpt: "a\nb\n" })).toEqual([
      { n: 12, text: "a" },
      { n: 13, text: "b" },
    ]);
    expect(numbered({ path: "a.go", startLine: 0, endLine: 0, excerpt: "a" })).toEqual([{ n: null, text: "a" }]);
    expect(numbered({ path: "a.go", startLine: 3, endLine: 0, excerpt: "" })).toEqual([]);
  });
  it("say where they point", () => {
    expect(where({ path: "a.go", startLine: 0, endLine: 0, excerpt: "" })).toBe("a.go");
    expect(where({ path: "a.go", startLine: 4, endLine: 4, excerpt: "" })).toBe("a.go:4");
    expect(where({ path: "a.go", startLine: 4, endLine: 9, excerpt: "" })).toBe("a.go:4-9");
  });
});

describe("versionLine", () => {
  it("is base..head, or the whole tree at head", () => {
    expect(versionLine({ base: "8d708783e1", head: "6cb0ca1f37" } as Review)).toBe("8d708783..6cb0ca1f");
    expect(versionLine({ base: null, head: "6cb0ca1f37" } as Review)).toBe("6cb0ca1f (whole tree)");
  });
});
