import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { fetchJson } from "./http";

export interface GcKindUsage {
  objects: number;
  bytes: number;
}

export interface GcCellUsage {
  tenant: string;
  cell: string;
  measuredUnixMs: number | null;
  objects: number | null;
  bytes: number | null;
  kinds: Record<string, GcKindUsage> | null;
}

export interface GcStatusResponse {
  configured: boolean;
  healthy: boolean;
  workerUrl: string | null;
  bucket: string;
  queue: string;
  schedule: string;
  cells: number;
  measuredCells: number;
  totalObjects: number;
  totalBytes: number;
  usage: GcCellUsage[];
  error?: string;
}

export interface GcSweepResponse {
  enqueued: number;
  sweepStartedMs: number;
  shreddedTenants: string[];
}

export interface GcDoomedEntry {
  key: string;
  kind: string;
  size: number;
}

export interface GcDryRunResponse {
  cell: string;
  doomed: GcDoomedEntry[];
  doomedBytes: number;
  retired: string[];
  fenced: boolean;
}

export interface GcCollectCellResponse {
  cell: string;
  deleted: number;
  deletedBytes: number;
  retired: number;
  skipped: number;
  removed: number;
  left?: "held" | "swept";
}

export function useGcStatus(pollFast = false) {
  return useQuery({
    queryKey: ["gc-status"],
    queryFn: () => fetchJson<GcStatusResponse>("/api/gc/status"),
    refetchInterval: pollFast ? 4_000 : 60_000,
    staleTime: pollFast ? 2_000 : 30_000,
    retry: false,
  });
}

export function useTriggerGcSweep() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => fetchJson<GcSweepResponse>("/api/gc/sweep", { method: "POST" }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["gc-status"] });
    },
  });
}

export function useGcDryRunCell() {
  return useMutation({
    mutationFn: (cell: string) => fetchJson<GcDryRunResponse>(`/api/gc/dry-run/${cell}`),
  });
}

export function useGcCollectCell() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (cell: string) =>
      fetchJson<GcCollectCellResponse>(`/api/gc/cell/${cell}`, { method: "POST" }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["gc-status"] });
    },
  });
}

export interface GcKindSummary {
  kind: string;
  objects: number;
  bytes: number;
}

/** Aggregates per-kind object and byte counts across all measured cells, sorted by bytes desc. */
export function summarizeGcKinds(usage: readonly GcCellUsage[]): GcKindSummary[] {
  const map = new Map<string, GcKindSummary>();
  for (const u of usage) {
    if (!u.kinds) continue;
    for (const [kind, sum] of Object.entries(u.kinds)) {
      const prev = map.get(kind);
      if (prev) {
        prev.objects += sum.objects;
        prev.bytes += sum.bytes;
      } else {
        map.set(kind, { kind, objects: sum.objects, bytes: sum.bytes });
      }
    }
  }
  return [...map.values()].sort((a, b) => b.bytes - a.bytes || b.objects - a.objects);
}

// Returns the next UTC 6-hour cron boundary (`0 */6 * * *`) strictly after `nowMs`.
export function nextSixHourCronMs(nowMs: number): number {
  const sixHoursMs = 6 * 3600 * 1000;
  return (Math.floor(nowMs / sixHoursMs) + 1) * sixHoursMs;
}

export type GcCellSort = "bytes" | "objects" | "unmeasured" | "oldest";

export function sortGcCells(usage: readonly GcCellUsage[], sort: GcCellSort, filter: string): GcCellUsage[] {
  const q = filter.trim().toLowerCase();
  const filtered = q ? usage.filter((u) => u.cell.toLowerCase().includes(q)) : [...usage];
  filtered.sort((a, b) => {
    if (sort === "unmeasured") {
      const am = a.measuredUnixMs === null ? 0 : 1;
      const bm = b.measuredUnixMs === null ? 0 : 1;
      if (am !== bm) return am - bm;
      return (b.bytes ?? -1) - (a.bytes ?? -1) || a.cell.localeCompare(b.cell);
    }
    if (sort === "oldest") {
      const at = a.measuredUnixMs ?? Number.MAX_SAFE_INTEGER;
      const bt = b.measuredUnixMs ?? Number.MAX_SAFE_INTEGER;
      if (at !== bt) return at - bt;
      return a.cell.localeCompare(b.cell);
    }
    if (sort === "objects") {
      return (b.objects ?? -1) - (a.objects ?? -1) || (b.bytes ?? -1) - (a.bytes ?? -1);
    }
    return (b.bytes ?? -1) - (a.bytes ?? -1) || (b.objects ?? -1) - (a.objects ?? -1) || a.cell.localeCompare(b.cell);
  });
  return filtered;
}
