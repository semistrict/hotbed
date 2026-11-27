import { BaseState, type Snapshot } from "../api";

// Published Cloudflare R2 Standard pricing (USD, gross before free tier; $0 egress).
const R2_STORAGE_PER_GB_MO = 0.015;
const R2_CLASS_A_PER_MILLION = 4.5;
const R2_CLASS_B_PER_MILLION = 0.36;

export interface R2MonthlyCostEstimate {
  storageBytes: number;
  storageCost: number;
  classAOpsMonth: number;
  classACost: number;
  classBOpsMonth: number;
  classBCost: number;
  opsCost: number;
  totalMonthly: number;
}

/** What storing `bytes` in R2 Standard costs a month, gross. */
export function r2StorageCost(bytes: number): number {
  return (bytes / 1e9) * R2_STORAGE_PER_GB_MO;
}

/**
 * Estimates gross monthly Cloudflare R2 spend from 24h extrapolated Class A/B
 * request rates plus unique warm base storage (deduplicated by project+gen+arch).
 */
export function estimateR2MonthlyCost(s: Snapshot, puts24h: number, gets24h: number): R2MonthlyCostEstimate {
  const nodeArch = new Map(s.nodes.map((n) => [n.id, n.resources?.arch ?? ""]));
  const seenBase = new Map<string, number>();
  for (const b of s.bases) {
    if (b.state !== BaseState.WARM) continue;
    const arch = b.cladefsCell?.arch || nodeArch.get(b.nodeId) || "any";
    const key = `${b.project}:${b.generation}:${arch}`;
    const sz = Number(b.sizeBytes);
    const prev = seenBase.get(key) ?? 0;
    if (sz > prev) seenBase.set(key, sz);
  }
  let storageBytes = 0;
  for (const sz of seenBase.values()) storageBytes += sz;

  const classAOpsMonth = puts24h * 30;
  const classBOpsMonth = gets24h * 30;
  const storageCost = r2StorageCost(storageBytes);
  const classACost = (classAOpsMonth / 1e6) * R2_CLASS_A_PER_MILLION;
  const classBCost = (classBOpsMonth / 1e6) * R2_CLASS_B_PER_MILLION;
  const opsCost = classACost + classBCost;
  return {
    storageBytes,
    storageCost,
    classAOpsMonth,
    classACost,
    classBOpsMonth,
    classBCost,
    opsCost,
    totalMonthly: storageCost + opsCost,
  };
}

export function formatUsd(n: number): string {
  if (!Number.isFinite(n) || n < 0) return "—";
  if (n === 0) return "$0.00";
  if (n < 0.01) return "<$0.01";
  return `$${n.toFixed(2)}`;
}
