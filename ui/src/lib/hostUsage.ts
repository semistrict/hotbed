import type { HostUsage, Node, Sample } from "../api";
import type { LinePoint, LineSeries } from "../components/charts/LineChart";
import { percent } from "./format";

export const HOST_CPU_BUSY_HOLD_AT = 0.85;
export const HOST_LOAD_FACTOR_HOLD_AT = 1.25;
export const HOST_STEAL_GUEST_MIN = 0.5;
export const HOST_STEAL_DIFF_HOLD_AT = 0.35;
export const HOST_MEM_PRESSURE_WARN_AT = 50;
export const HOST_MEM_PRESSURE_HOLD_AT = 100;

/** Host-side usage for an online VM node, or undefined for bare-metal/offline nodes. */
export function nodeHostUsage(n: Node): HostUsage | undefined {
  return n.online ? n.hostUsage : undefined;
}

/** True when guest CPU looks busy while the VM's host CPU is much lower (>= 35% CPU steal). */
export function hasCpuStealWarning(guestBusy: number, host?: HostUsage): boolean {
  if (!host) return false;
  return guestBusy >= HOST_STEAL_GUEST_MIN && guestBusy - host.vmCpuBusy >= HOST_STEAL_DIFF_HOLD_AT;
}

/** True when host-side metrics cross any starvation threshold. */
export function isHostStarved(guestBusy: number, host?: HostUsage): boolean {
  if (!host) return false;
  if (host.hostMemoryPressure >= HOST_MEM_PRESSURE_HOLD_AT) return true;
  if (host.hostCpuBusy >= HOST_CPU_BUSY_HOLD_AT) return true;
  if (host.hostCpus > 0 && host.hostLoad1m >= host.hostCpus * HOST_LOAD_FACTOR_HOLD_AT) return true;
  return hasCpuStealWarning(guestBusy, host);
}

/** Human-readable macOS memory pressure state for 0/50/100 levels. */
export function hostMemoryPressureLabel(pressure: number): string {
  if (pressure >= HOST_MEM_PRESSURE_HOLD_AT) return "critical";
  if (pressure >= HOST_MEM_PRESSURE_WARN_AT) return "warn";
  return "normal";
}

/** Compact host summary shown in VM stat tiles and node cards. */
export function formatHostUsageFoot(host: HostUsage): string {
  const parts = [`host ${percent(host.hostCpuBusy)}`];
  if (host.hostCpus > 0) {
    parts.push(`load ${host.hostLoad1m.toFixed(1)}/${host.hostCpus}`);
  } else if (host.hostLoad1m > 0) {
    parts.push(`load ${host.hostLoad1m.toFixed(1)}`);
  }
  parts.push(`mem ${hostMemoryPressureLabel(host.hostMemoryPressure)}`);
  return parts.join(" · ");
}

/** Emits a full time-aligned point list with null gaps for missing host metrics, or [] if no sample has a value. */
export function hostCpuSeries(samples: Sample[], pick: (p: Sample) => number | undefined): LinePoint[] {
  const points = samples.map((p) => ({ t: Number(p.t), v: pick(p) ?? null }));
  return points.some((p) => p.v !== null) ? points : [];
}

/** Builds CPU LineChart series: guest CPU alone for bare-metal, overlaid with VM & host CPU for VM nodes. */
export function buildCpuChartSeries(nodeId: string, color: string, samples: Sample[]): LineSeries[] {
  const vmPoints = hostCpuSeries(samples, (p) => p.vmCpuBusy);
  const hostPoints = hostCpuSeries(samples, (p) => p.hostCpuBusy);
  const hasHost = vmPoints.length > 0 || hostPoints.length > 0;
  const out: LineSeries[] = [
    {
      id: nodeId,
      label: hasHost ? "guest CPU" : "cpu",
      color,
      points: samples.map((p) => ({ t: Number(p.t), v: p.cpuBusy })),
    },
  ];
  if (vmPoints.length > 0) {
    out.push({
      id: `${nodeId}-vm`,
      label: "VM CPU (host)",
      color: "var(--frost)",
      points: vmPoints,
    });
  }
  if (hostPoints.length > 0) {
    out.push({
      id: `${nodeId}-host`,
      label: "host CPU",
      color: "var(--warning)",
      points: hostPoints,
    });
  }
  return out;
}
