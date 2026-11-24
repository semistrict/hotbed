import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";
import { MemoryContentSchema, MemoryMode, MemorySchema, MemoryScope, MemoryState } from "../gen/hotbed/v1/hotbed_pb";
import { computeLineDiff, computeStartupBudget, mergeLatestMemories } from "./memories";

function mkMemory(id: number, slug: string, summary: string, mode: MemoryMode, state = MemoryState.APPROVED, revision = 1n) {
  return create(MemorySchema, {
    id,
    scope: MemoryScope.GLOBAL,
    state,
    revision,
    updatedAt: 100n,
    content: create(MemoryContentSchema, { slug, summary, body: "details", mode }),
  });
}

describe("computeStartupBudget", () => {
  it("tallies approved always-on and on-demand entries separately and ignores non-approved", () => {
    const budget = computeStartupBudget([
      mkMemory(1, "use-neon", "Use neon for subagents", MemoryMode.ALWAYS),
      mkMemory(2, "zfs-debug", "How to debug ZFS flakes", MemoryMode.ON_DEMAND),
      mkMemory(3, "pending-one", "Still proposed", MemoryMode.ALWAYS, MemoryState.PROPOSED),
    ]);
    expect(budget.alwaysCount).toBe(1);
    expect(budget.onDemandCount).toBe(1);
    expect(budget.alwaysBytes).toBe("use-neon: Use neon for subagents".length);
    expect(budget.onDemandBytes).toBe("zfs-debug: How to debug ZFS flakes".length);
  });
});

describe("computeLineDiff", () => {
  it("identifies unchanged, deleted, and added lines", () => {
    const diff = computeLineDiff("line 1\nold line\nline 3", "line 1\nnew line\nline 3\nline 4");
    expect(diff).toEqual([
      { kind: "same", text: "line 1" },
      { kind: "del", text: "old line" },
      { kind: "add", text: "new line" },
      { kind: "same", text: "line 3" },
      { kind: "add", text: "line 4" },
    ]);
  });
});

describe("mergeLatestMemories", () => {
  it("keeps the highest revision seen when a stale poll arrives", () => {
    const seen = new Map();
    const newer = mkMemory(1, "use-neon", "Updated summary", MemoryMode.ALWAYS, MemoryState.APPROVED, 3n);
    mergeLatestMemories(seen, [newer]);
    const stale = mkMemory(1, "use-neon", "Old summary", MemoryMode.ALWAYS, MemoryState.PROPOSED, 2n);
    const merged = mergeLatestMemories(seen, [stale], [MemoryState.PROPOSED]);
    expect(merged).toEqual([]);
    const approved = mergeLatestMemories(seen, [stale], [MemoryState.APPROVED]);
    expect(approved).toHaveLength(1);
    expect(approved[0]?.content?.summary).toBe("Updated summary");
  });
});
