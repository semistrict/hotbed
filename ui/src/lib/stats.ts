import type { FinishedCommand, Snapshot } from "../api";

/** Builds and tests: the commands a warm base speeds up. */
export function compiled(h: FinishedCommand[]): (FinishedCommand & { coldSeconds: number })[] {
  return h.filter((c): c is FinishedCommand & { coldSeconds: number } => c.coldSeconds !== null && c.state !== "cancelled");
}

export interface ClusterNow {
  online: number;
  nodes: number;
  cores: number;
  coresBusy: number;
  memory: number;
  memoryUsed: number;
  swapUsed: number;
  running: number;
  queued: number;
  parked: number;
  servers: number;
}

export function clusterNow(s: Snapshot): ClusterNow {
  const on = s.nodes.filter((n) => n.online);
  const count = (state: string) => s.commands.filter((c) => c.state === state).length;
  return {
    online: on.length,
    nodes: s.nodes.length,
    cores: on.reduce((n, x) => n + x.cpus, 0),
    coresBusy: on.reduce((n, x) => n + x.cpus * x.usage.cpuBusy, 0),
    memory: on.reduce((n, x) => n + x.memoryBytes, 0),
    memoryUsed: on.reduce((n, x) => n + x.usage.memoryUsedBytes, 0),
    swapUsed: on.reduce((n, x) => n + x.usage.swapUsedBytes, 0),
    running: count("running"),
    queued: count("queued"),
    parked: count("parked"),
    servers: s.commands.filter((c) => c.kind === "server" && c.state === "running").length,
  };
}

/** What the window's builds cost, and what they would have cost from nothing. */
export function savings(h: FinishedCommand[]) {
  const c = compiled(h);
  const spent = c.reduce((n, x) => n + x.seconds, 0);
  const cold = c.reduce((n, x) => n + x.coldSeconds, 0);
  return { builds: c.length, spent, cold, saved: Math.max(0, cold - spent), ratio: spent > 0 ? cold / spent : 0 };
}

export interface Bucket {
  label: string;
  count: number;
}

/** Cold time over actual, in buckets a person reads at a glance. */
export function speedupBuckets(h: FinishedCommand[]): Bucket[] {
  const edges: [number, string][] = [
    [2, "<2×"],
    [5, "2–5×"],
    [10, "5–10×"],
    [20, "10–20×"],
    [50, "20–50×"],
    [100, "50–100×"],
    [Infinity, "100×+"],
  ];
  const out = edges.map(([, label]) => ({ label, count: 0 }));
  for (const c of compiled(h)) {
    const x = c.coldSeconds / Math.max(0.1, c.seconds);
    const i = edges.findIndex(([edge]) => x < edge);
    const b = out[i];
    if (b) b.count += 1;
  }
  return out;
}

/** How many units each build recompiled: the size of the change it carried. */
export function rebuiltBuckets(h: FinishedCommand[]): Bucket[] {
  const edges: [number, string][] = [
    [1, "0"],
    [2, "1"],
    [4, "2–3"],
    [8, "4–7"],
    [16, "8–15"],
    [32, "16–31"],
    [Infinity, "32+"],
  ];
  const out = edges.map(([, label]) => ({ label, count: 0 }));
  for (const c of h) {
    if (c.rebuilt === null) continue;
    const i = edges.findIndex(([edge]) => (c.rebuilt ?? 0) < edge);
    const b = out[i];
    if (b) b.count += 1;
  }
  return out;
}

/** Live workspaces' size if each were a full copy, against the blocks they really take. */
export function storage(s: Snapshot) {
  const live = s.workspaces.filter((w) => w.state !== "released");
  const asCopies = live.reduce((n, w) => n + w.sharedBytes + w.exclusiveBytes, 0);
  const on = s.nodes;
  const bases = on.reduce((n, x) => n + x.disk.basesBytes, 0);
  const forks = on.reduce((n, x) => n + x.disk.forksExclusiveBytes, 0);
  const other = on.reduce((n, x) => n + x.disk.otherBytes, 0);
  const free = on.reduce((n, x) => n + x.disk.freeBytes, 0);
  return { workspaces: live.length, asCopies, actual: bases + forks, bases, forks, other, free };
}

export function failureRate(h: FinishedCommand[]): number {
  const done = h.filter((c) => c.state !== "cancelled");
  return done.length ? done.filter((c) => c.state === "failed").length / done.length : 0;
}
