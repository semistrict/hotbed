import { MemoryMode, MemoryScope, MemoryState, type Memory } from "../api";

export type MemoryTab = "proposals" | "approved" | "history" | "all";

export const MEMORY_TAB_STATES: Record<MemoryTab, MemoryState[]> = {
  proposals: [MemoryState.PROPOSED],
  approved: [MemoryState.APPROVED],
  history: [MemoryState.REJECTED, MemoryState.SUPERSEDED, MemoryState.ARCHIVED, MemoryState.WITHDRAWN],
  all: [
    MemoryState.PROPOSED,
    MemoryState.APPROVED,
    MemoryState.REJECTED,
    MemoryState.SUPERSEDED,
    MemoryState.ARCHIVED,
    MemoryState.WITHDRAWN,
  ],
};

export const ALWAYS_MAX_COUNT = 20;
export const ALWAYS_MAX_BYTES = 4096;
export const ON_DEMAND_MAX_COUNT = 30;
export const ON_DEMAND_MAX_BYTES = 4096;

export interface StartupBudget {
  alwaysCount: number;
  alwaysBytes: number;
  alwaysMaxCount: number;
  alwaysMaxBytes: number;
  onDemandCount: number;
  onDemandBytes: number;
  onDemandMaxCount: number;
  onDemandMaxBytes: number;
}

/** Computes startup context budget usage for a set of approved memories. */
export function computeStartupBudget(memories: readonly Memory[]): StartupBudget {
  const enc = new TextEncoder();
  let alwaysCount = 0;
  let alwaysBytes = 0;
  let onDemandCount = 0;
  let onDemandBytes = 0;
  for (const m of memories) {
    if (m.state !== MemoryState.APPROVED || !m.content) continue;
    const bytes = enc.encode(`${m.content.slug}: ${m.content.summary}`).byteLength;
    if (m.content.mode === MemoryMode.ALWAYS) {
      alwaysCount++;
      alwaysBytes += bytes;
    } else {
      onDemandCount++;
      onDemandBytes += bytes;
    }
  }
  return {
    alwaysCount,
    alwaysBytes,
    alwaysMaxCount: ALWAYS_MAX_COUNT,
    alwaysMaxBytes: ALWAYS_MAX_BYTES,
    onDemandCount,
    onDemandBytes,
    onDemandMaxCount: ON_DEMAND_MAX_COUNT,
    onDemandMaxBytes: ON_DEMAND_MAX_BYTES,
  };
}

export interface DiffLine {
  kind: "same" | "add" | "del";
  text: string;
}

/** Simple LCS line diff for showing proposal updates against superseded targets. */
export function computeLineDiff(before: string, after: string): DiffLine[] {
  const a = before === "" ? [] : before.split("\n");
  const b = after === "" ? [] : after.split("\n");
  const m = a.length;
  const n = b.length;
  // Guard pathological inputs: fall back to full delete + add.
  if (m * n > 200_000) {
    return [
      ...a.map((text): DiffLine => ({ kind: "del", text })),
      ...b.map((text): DiffLine => ({ kind: "add", text })),
    ];
  }
  const dp = new Int32Array((m + 1) * (n + 1));
  const at = (r: number, c: number) => dp[r * (n + 1) + c] ?? 0;
  for (let i = m - 1; i >= 0; i--) {
    for (let j = n - 1; j >= 0; j--) {
      dp[i * (n + 1) + j] = a[i] === b[j] ? at(i + 1, j + 1) + 1 : Math.max(at(i + 1, j), at(i, j + 1));
    }
  }
  const out: DiffLine[] = [];
  let i = 0;
  let j = 0;
  while (i < m && j < n) {
    const ai = a[i] ?? "";
    const bj = b[j] ?? "";
    if (ai === bj) {
      out.push({ kind: "same", text: ai });
      i++;
      j++;
    } else if (at(i + 1, j) >= at(i, j + 1)) {
      out.push({ kind: "del", text: ai });
      i++;
    } else {
      out.push({ kind: "add", text: bj });
      j++;
    }
  }
  while (i < m) out.push({ kind: "del", text: a[i++] ?? "" });
  while (j < n) out.push({ kind: "add", text: b[j++] ?? "" });
  return out;
}

/** Revision-safe merge of polled memory lists so out-of-order responses never revert edits. */
export function mergeLatestMemories(
  seen: Map<number, Memory>,
  incoming: readonly Memory[],
  states?: readonly MemoryState[],
): Memory[] {
  const out: Memory[] = [];
  for (const m of incoming) {
    const prev = seen.get(m.id);
    const fresher =
      !prev || m.revision > prev.revision || (m.revision === prev.revision && m.updatedAt >= prev.updatedAt);
    const best = fresher ? m : prev;
    if (fresher) seen.set(m.id, m);
    if (!states || states.includes(best.state)) out.push(best);
  }
  return out;
}

export function memoryScopeLabel(m: Memory): string {
  return m.scope === MemoryScope.GLOBAL || !m.project ? "global" : m.project;
}

export function memoryModeLabel(mode: MemoryMode): string {
  return mode === MemoryMode.ALWAYS ? "always-on" : "on-demand";
}

export function formatBytesKB(bytes: number): string {
  return `${(bytes / 1024).toFixed(1)} KB`;
}
