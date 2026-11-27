import { MemoryMode, MemoryState } from "../api";

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

export function memoryModeLabel(mode: MemoryMode): string {
  return mode === MemoryMode.ALWAYS ? "always-on" : "on-demand";
}
