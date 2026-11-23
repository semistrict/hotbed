import { create } from "@bufbuild/protobuf";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { buildLinePath, LineChart, resolveHoverPoints } from "../components/charts/LineChart";
import { HostUsageSchema, NodeSchema, NodeUsageSchema, ResourcesSchema, SampleSchema } from "../gen/hotbed/v1/hotbed_pb";
import { HostCpuTile, HostStealBadge } from "../pages/Node";
import { NodeCpuCell } from "../pages/Nodes";
import { percent } from "./format";
import { buildCpuChartSeries, formatHostUsageFoot, hasCpuStealWarning, hostCpuSeries, hostMemoryPressureLabel, isHostStarved } from "./hostUsage";

describe("hostUsage helpers and VM node rendering", () => {
  const starvedHost = create(HostUsageSchema, {
    vmCpuBusy: 0.18,
    hostCpuBusy: 0.92,
    hostLoad1m: 16.2,
    hostCpus: 12,
    hostMemoryPressure: 50,
    sampledAt: 1_800_000_000_000n,
  });

  const healthyHost = create(HostUsageSchema, {
    vmCpuBusy: 0.35,
    hostCpuBusy: 0.40,
    hostLoad1m: 4.5,
    hostCpus: 12,
    hostMemoryPressure: 0,
    sampledAt: 1_800_000_000_000n,
  });

  it("detects CPU steal and host starvation accurately", () => {
    expect(hasCpuStealWarning(0.85, starvedHost)).toBe(true);
    expect(isHostStarved(0.85, starvedHost)).toBe(true);
    expect(hasCpuStealWarning(0.40, healthyHost)).toBe(false);
    expect(isHostStarved(0.40, healthyHost)).toBe(false);
    expect(hostMemoryPressureLabel(0)).toBe("normal");
    expect(hostMemoryPressureLabel(50)).toBe("warn");
    expect(hostMemoryPressureLabel(100)).toBe("critical");
    expect(formatHostUsageFoot(starvedHost)).toBe("host 92% · load 16.2/12 · mem warn");
  });

  it("overlays VM CPU and host CPU series only when host samples exist", () => {
    const bareMetal = [
      create(SampleSchema, { t: 1000n, cpuBusy: 0.4 }),
      create(SampleSchema, { t: 6000n, cpuBusy: 0.5 }),
    ];
    const bmSeries = buildCpuChartSeries("n1", "var(--series-1)", bareMetal);
    expect(bmSeries).toHaveLength(1);
    expect(bmSeries[0]?.label).toBe("cpu");

    const vmSamples = [
      create(SampleSchema, { t: 1000n, cpuBusy: 0.85, vmCpuBusy: 0.18, hostCpuBusy: 0.92 }),
      create(SampleSchema, { t: 6000n, cpuBusy: 0.80, vmCpuBusy: 0.22, hostCpuBusy: 0.88 }),
    ];
    const vmSeries = buildCpuChartSeries("n2", "var(--series-2)", vmSamples);
    expect(vmSeries.map((s) => s.label)).toEqual(["guest CPU", "VM CPU (host)", "host CPU"]);
    expect(vmSeries[1]?.points).toEqual([
      { t: 1000, v: 0.18 },
      { t: 6000, v: 0.22 },
    ]);
  });

  it("preserves host telemetry outages as null gaps and renders separate SVG path segments", () => {
    const withOutage = [
      create(SampleSchema, { t: 1_000n, cpuBusy: 0.80, vmCpuBusy: 0.20, hostCpuBusy: 0.90 }),
      create(SampleSchema, { t: 6_000n, cpuBusy: 0.82, vmCpuBusy: 0.25, hostCpuBusy: 0.88 }),
      create(SampleSchema, { t: 11_000n, cpuBusy: 0.85 }),
      create(SampleSchema, { t: 16_000n, cpuBusy: 0.84 }),
      create(SampleSchema, { t: 21_000n, cpuBusy: 0.78, vmCpuBusy: 0.55, hostCpuBusy: 0.60 }),
      create(SampleSchema, { t: 26_000n, cpuBusy: 0.76, vmCpuBusy: 0.58, hostCpuBusy: 0.62 }),
    ];

    expect(hostCpuSeries(withOutage, (s) => s.vmCpuBusy)).toEqual([
      { t: 1_000, v: 0.20 },
      { t: 6_000, v: 0.25 },
      { t: 11_000, v: null },
      { t: 16_000, v: null },
      { t: 21_000, v: 0.55 },
      { t: 26_000, v: 0.58 },
    ]);

    const series = buildCpuChartSeries("n-vm", "var(--series-1)", withOutage);
    expect(series).toHaveLength(3);

    // Build paths directly and verify outage produces two separate M subpaths (never bridging across nulls).
    const vmPath = buildLinePath(series[1]!.points, (t) => t / 100, (v) => 100 - v * 100);
    expect(vmPath.match(/M/g)).toHaveLength(2);

    // Also verify rendered LineChart SVG emits 2 M segments for VM & host paths and 1 continuous M for guest.
    const svg = renderToStaticMarkup(createElement(LineChart, { series, width: 600, yMax: 1, format: percent }));
    const pathDs = Array.from(svg.matchAll(/<path d="([^"]+)"/g), (m) => m[1]!);
    expect(pathDs).toHaveLength(3);
    expect(pathDs[0]!.match(/M/g)).toHaveLength(1);
    expect(pathDs[1]!.match(/M/g)).toHaveLength(2);
    expect(pathDs[2]!.match(/M/g)).toHaveLength(2);

    // Hovering during the explicit outage shows only guest CPU, never bridging VM/host values across the gap.
    const hoveredDuringOutage = resolveHoverPoints(series, 11_000);
    expect(hoveredDuringOutage.map((h) => h.series.id)).toEqual(["n-vm"]);

    // Hovering after reporting resumes resolves all three series again.
    const hoveredAfterRecovery = resolveHoverPoints(series, 21_000);
    expect(hoveredAfterRecovery.map((h) => h.series.id)).toEqual(["n-vm", "n-vm-host", "n-vm-vm"]);
  });

  it("returns empty hostCpuSeries when no sample contains a host metric", () => {
    const bareMetal = [
      create(SampleSchema, { t: 1_000n, cpuBusy: 0.4 }),
      create(SampleSchema, { t: 6_000n, cpuBusy: 0.5 }),
    ];
    expect(hostCpuSeries(bareMetal, (s) => s.vmCpuBusy)).toEqual([]);
    expect(hostCpuSeries(bareMetal, (s) => s.hostCpuBusy)).toEqual([]);
  });

  it("renders host CPU tile and steal badge for VM nodes and nothing for bare-metal nodes", () => {
    const vmTileHtml = renderToStaticMarkup(HostCpuTile({ guestBusy: 0.85, vcpus: 7, host: starvedHost }));
    expect(vmTileHtml).toContain("VM CPU (host)");
    expect(vmTileHtml).toContain("18%");
    expect(vmTileHtml).toContain("of 7 vCPUs");
    expect(vmTileHtml).toContain("host 92% · load 16.2/12 · mem warn");

    const bmTileHtml = renderToStaticMarkup(HostCpuTile({ guestBusy: 0.85, vcpus: 16, host: undefined }));
    expect(bmTileHtml).toBe("");

    const stealBadgeHtml = renderToStaticMarkup(HostStealBadge({ guestBusy: 0.85, host: starvedHost }));
    expect(stealBadgeHtml).toContain("host CPU steal");
    expect(stealBadgeHtml).toContain("VM 18% vs guest 85%");

    const healthyBadgeHtml = renderToStaticMarkup(HostStealBadge({ guestBusy: 0.40, host: healthyHost }));
    expect(healthyBadgeHtml).toBe("");
  });

  it("renders Nodes table CPU cell with VM/host truth for VM nodes and guest-only for bare metal", () => {
    const vmNode = create(NodeSchema, {
      id: "n-vm",
      hostname: "mac-worker",
      online: true,
      resources: create(ResourcesSchema, { cpus: 7 }),
      usage: create(NodeUsageSchema, { cpuBusy: 0.85 }),
      hostUsage: starvedHost,
    });
    const bmNode = create(NodeSchema, {
      id: "n-bm",
      hostname: "ubu1",
      online: true,
      resources: create(ResourcesSchema, { cpus: 16 }),
      usage: create(NodeUsageSchema, { cpuBusy: 0.42 }),
    });

    const vmCellHtml = renderToStaticMarkup(NodeCpuCell({ n: vmNode }));
    expect(vmCellHtml).toContain("85%");
    expect(vmCellHtml).toContain("VM 18% · host 92%");
    expect(vmCellHtml).toContain("text-serious");

    const bmCellHtml = renderToStaticMarkup(NodeCpuCell({ n: bmNode }));
    expect(bmCellHtml).toBe("42%");
  });
});
