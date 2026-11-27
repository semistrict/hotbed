import { MemoryMode, MemoryState, type Memory } from "../api";

export type MemoryTab = "active" | "history" | "all";

export const MEMORY_TAB_STATES: Record<MemoryTab, MemoryState[]> = {
  active: [MemoryState.APPROVED],
  history: [MemoryState.ARCHIVED, MemoryState.SUPERSEDED, MemoryState.REJECTED, MemoryState.WITHDRAWN],
  all: [
    MemoryState.APPROVED,
    MemoryState.ARCHIVED,
    MemoryState.SUPERSEDED,
    MemoryState.REJECTED,
    MemoryState.WITHDRAWN,
    MemoryState.PROPOSED,
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

export function memoryModeLabel(mode: MemoryMode): string {
  return mode === MemoryMode.ALWAYS ? "always-on" : "on-demand";
}
