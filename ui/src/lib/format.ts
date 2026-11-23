const GiB = 1024 ** 3;
const MiB = 1024 ** 2;

/** 1.4 GiB, 812 MiB, 2.1 TiB. */
export function bytes(n: number | bigint): string {
  const v = Number(n);
  if (!Number.isFinite(v) || v < 0) return "—";
  if (v >= 1024 * GiB) return `${trim(v / (1024 * GiB), 1)} TiB`;
  if (v >= GiB) return `${trim(v / GiB, v >= 100 * GiB ? 0 : 1)} GiB`;
  if (v >= MiB) return `${Math.round(v / MiB)} MiB`;
  if (v >= 1024) return `${Math.round(v / 1024)} KiB`;
  return `${Math.round(v)} B`;
}

/** A cgroup memory limit, where 0 sets none: "none", "4 GiB". */
export function limit(n: number | bigint): string {
  const v = Number(n);
  return v === 0 ? "none" : bytes(v);
}

/** A commit as people quote it: its first 10 hex digits. */
export function shortSha(sha: string): string {
  return sha.slice(0, 10);
}

/** 840 ms, 14.2 s, 6m 10s, 3h 12m, 41h 12m. */
export function duration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return "—";
  if (seconds < 1) return `${Math.round(seconds * 1000)} ms`;
  if (seconds < 60) return `${trim(seconds, seconds < 10 ? 1 : 0)} s`;
  if (seconds < 3600) {
    const m = Math.floor(seconds / 60);
    const s = Math.round(seconds % 60);
    return s === 60 ? `${m + 1}m 0s` : `${m}m ${s}s`;
  }
  const h = Math.floor(seconds / 3600);
  const m = Math.round((seconds % 3600) / 60);
  return m === 60 ? `${h + 1}h 0m` : `${h}h ${m}m`;
}

/** The two units of duration() separately, for a hero figure: ["41", "h", "12", "m"]. */
export function durationParts(seconds: number): [string, string][] {
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = Math.floor(seconds % 60);
  if (h > 0) return [[String(h), "h"], [String(m).padStart(2, "0"), "m"]];
  if (m > 0) return [[String(m), "m"], [String(s).padStart(2, "0"), "s"]];
  return [[String(s), "s"]];
}

/** 0.4123 → "41%". */
export function percent(f: number, digits = 0): string {
  if (!Number.isFinite(f)) return "—";
  return `${(f * 100).toFixed(digits)}%`;
}

/** 1,284 / 12.9K / 4.2M. */
export function compact(n: number | bigint): string {
  const v = Number(n);
  if (Math.abs(v) < 10_000) return Math.round(v).toLocaleString("en-US");
  if (Math.abs(v) < 1_000_000) return `${trim(v / 1000, 1)}K`;
  return `${trim(v / 1_000_000, 1)}M`;
}

/** "12s ago", "4m ago", "3h ago" from unix-ms timestamps. */
export function ago(t: number | bigint, now: number | bigint): string {
  const s = Math.max(0, (Number(now) - Number(t)) / 1000);
  if (s < 5) return "now";
  if (s < 60) return `${Math.floor(s)}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

/** 14:05 local. */
export function hhmm(t: number | bigint): string {
  const d = new Date(Number(t));
  return `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
}

/** 14:05:09 local. */
export function hhmmss(t: number | bigint): string {
  const d = new Date(Number(t));
  return `${hhmm(t)}:${String(d.getSeconds()).padStart(2, "0")}`;
}

/** Cold time over actual: "26×". */
export function speedup(cold: number, actual: number): string {
  if (actual <= 0) return "—";
  const x = cold / actual;
  return `${x >= 10 ? Math.round(x) : trim(x, 1)}×`;
}

function trim(n: number, digits: number): string {
  return n.toFixed(digits).replace(/\.0+$/, "");
}

/** The p-th quantile (0..1) of xs, which need not be sorted. */
export function quantile(xs: number[], p: number): number {
  if (xs.length === 0) return 0;
  const s = [...xs].sort((a, b) => a - b);
  const i = Math.min(s.length - 1, Math.max(0, Math.round(p * (s.length - 1))));
  return s[i] ?? 0;
}

/** Safe comparator for bigint timestamps and counters. */
export function cmpBig(a: bigint, b: bigint): number {
  return a < b ? -1 : a > b ? 1 : 0;
}
