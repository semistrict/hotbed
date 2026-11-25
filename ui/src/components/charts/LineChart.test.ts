import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import {
  buildLinePath,
  LineChart,
  matchWindowMs,
  resolveHoverPoints,
  resolveSeriesPoint,
  splitLineRuns,
  Tooltip,
  type LineSeries,
} from "./LineChart";

describe("LineChart gap-aware path building", () => {
  const x = (t: number) => t;
  const y = (v: number) => 100 - v * 100;

  it("splits paths into separate M subpaths across null gaps and never bridges them", () => {
    const points = [
      { t: 0, v: 0.2 },
      { t: 10, v: 0.4 },
      { t: 20, v: null },
      { t: 30, v: null },
      { t: 40, v: 0.6 },
      { t: 50, v: 0.8 },
    ];
    const runs = splitLineRuns(points);
    expect(runs).toEqual([
      [
        { t: 0, v: 0.2 },
        { t: 10, v: 0.4 },
      ],
      [
        { t: 40, v: 0.6 },
        { t: 50, v: 0.8 },
      ],
    ]);

    const d = buildLinePath(points, x, y);
    expect(d).toBe("M0.0,80.0L10.0,60.0M40.0,40.0L50.0,20.0");
    expect(d.match(/M/g)).toHaveLength(2);
  });

  it("renders isolated single non-null points cleanly without bridging across nulls", () => {
    const series: LineSeries[] = [
      {
        id: "host",
        label: "host CPU",
        color: "var(--warning)",
        points: [
          { t: 0, v: 0.2 },
          { t: 10, v: 0.3 },
          { t: 20, v: null },
          { t: 30, v: 0.7 },
          { t: 40, v: null },
        ],
      },
    ];

    const d = buildLinePath(series[0]!.points, x, y);
    expect(d).toBe("M0.0,80.0L10.0,70.0M30.0,30.0L30.0,30.0");

    const svg = renderToStaticMarkup(createElement(LineChart, { series, width: 400, yMax: 1, format: (v) => `${v}` }));
    // Isolated middle point renders a dot; trailing null suppresses the live endpoint r=4 circle.
    expect(svg).toContain('r="2.5"');
    expect(svg).not.toContain('r="4"');
  });
});

describe("LineChart nearest-timestamp hover resolution", () => {
  it("matches independently sampled multi-node series with offset timestamps within the sampling window", () => {
    const series: LineSeries[] = [
      {
        id: "node-a",
        label: "ubu1",
        color: "var(--series-1)",
        points: [
          { t: 1_000, v: 12 },
          { t: 6_000, v: 18 },
          { t: 11_000, v: 24 },
        ],
      },
      {
        id: "node-b",
        label: "mac-vm",
        color: "var(--series-2)",
        points: [
          { t: 1_050, v: 30 },
          { t: 6_050, v: 36 },
          { t: 11_050, v: 42 },
        ],
      },
      {
        id: "node-c",
        label: "late-node",
        color: "var(--series-3)",
        points: [
          { t: 25_000, v: 50 },
          { t: 30_000, v: 55 },
        ],
      },
    ];

    expect(matchWindowMs(series, series[1])).toBe(3_750);

    const at1000 = resolveHoverPoints(series, 1_000);
    expect(at1000.map((h) => ({ id: h.series.id, t: h.point.t, v: h.point.v }))).toEqual([
      { id: "node-b", t: 1_050, v: 30 },
      { id: "node-a", t: 1_000, v: 12 },
    ]);

    // Hovering at node-b's timestamp also resolves node-a, while node-c (outside window) is excluded.
    const at6050 = resolveHoverPoints(series, 6_050);
    expect(at6050.map((h) => ({ id: h.series.id, t: h.point.t, v: h.point.v }))).toEqual([
      { id: "node-b", t: 6_050, v: 36 },
      { id: "node-a", t: 6_000, v: 18 },
    ]);
  });

  it("respects explicit null gaps even when neighboring non-null points are within the sampling window", () => {
    const series: LineSeries[] = [
      {
        id: "guest",
        label: "guest CPU",
        color: "var(--series-1)",
        points: [
          { t: 1_000, v: 0.8 },
          { t: 2_000, v: 0.82 },
          { t: 3_000, v: 0.84 },
        ],
      },
      {
        id: "host",
        label: "host CPU",
        color: "var(--warning)",
        points: [
          { t: 1_000, v: 0.9 },
          { t: 2_000, v: null },
          { t: 3_000, v: 0.88 },
        ],
      },
      {
        id: "host-offset",
        label: "host offset",
        color: "var(--frost)",
        points: [
          { t: 1_050, v: 0.7 },
          { t: 2_050, v: null },
          { t: 3_050, v: 0.75 },
        ],
      },
    ];

    // Even with a generous window, exact null at t=2000 and nearest null at t=2050 do not match.
    expect(resolveSeriesPoint(series[1]!.points, 2_000, 10_000)).toBeUndefined();
    expect(resolveSeriesPoint(series[2]!.points, 2_000, 10_000)).toBeUndefined();

    const hovered = resolveHoverPoints(series, 2_000);
    expect(hovered.map((h) => h.series.id)).toEqual(["guest"]);
  });

  it("does not match points outside the bounded sampling window during a multi-node outage", () => {
    const series: LineSeries[] = [
      {
        id: "steady",
        label: "ubu1",
        color: "var(--series-1)",
        points: [
          { t: 0, v: 10 },
          { t: 5_000, v: 12 },
          { t: 10_000, v: 14 },
          { t: 15_000, v: 16 },
          { t: 20_000, v: 18 },
          { t: 25_000, v: 20 },
          { t: 30_000, v: 22 },
        ],
      },
      {
        id: "outage",
        label: "vm1",
        color: "var(--series-2)",
        points: [
          { t: 100, v: 5 },
          { t: 30_100, v: 8 },
        ],
      },
    ];

    // At t=0 and t=30000 both nodes match within the 3750ms window.
    expect(resolveHoverPoints(series, 0).map((h) => h.series.id)).toEqual(["steady", "outage"]);
    expect(resolveHoverPoints(series, 30_000).map((h) => h.series.id)).toEqual(["steady", "outage"]);

    // Mid-outage (t=15000), vm1's nearest sample is 14.9s away and must not match.
    expect(resolveHoverPoints(series, 15_000).map((h) => h.series.id)).toEqual(["steady"]);
  });
});

describe("LineChart axis layout and tooltip clamping", () => {
  it("expands padLeft dynamically for wide formatted Y-axis labels", () => {
    const series: LineSeries[] = [
      {
        id: "r2",
        label: "upload",
        color: "var(--series-1)",
        points: [
          { t: 1_000, v: 500 },
          { t: 6_000, v: 1000 },
        ],
      },
    ];
    const svg = renderToStaticMarkup(
      createElement(LineChart, { series, width: 400, format: (v) => `${v.toFixed(0)} ms/s` }),
    );
    const x1Match = svg.match(/<line x1="(\d+)"/);
    expect(x1Match).not.toBeNull();
    expect(Number(x1Match![1])).toBeGreaterThan(40);
  });

  it("emits a single X-axis tick when t0 === t1 instead of 5 overlapping labels", () => {
    const t = Date.UTC(2026, 10, 25, 14, 5, 0);
    const series: LineSeries[] = [
      { id: "only", label: "only", color: "var(--series-1)", points: [{ t, v: 42 }] },
    ];
    const svg = renderToStaticMarkup(createElement(LineChart, { series, width: 400, format: (v) => `${v}` }));
    const xTickTexts = svg.match(/y="174"/g) ?? [];
    expect(xTickTexts).toHaveLength(1);
  });

  it("clamps hover Tooltip position inside [0, width]", () => {
    const overflowRight = renderToStaticMarkup(createElement(Tooltip, { x: 450, width: 300, title: "14:05:00", children: "val" }));
    expect(overflowRight).toContain("right:14px");
    const overflowLeft = renderToStaticMarkup(createElement(Tooltip, { x: -50, width: 300, title: "14:05:00", children: "val" }));
    expect(overflowLeft).toContain("left:14px");
    const narrowLeft = renderToStaticMarkup(createElement(Tooltip, { x: 90, width: 180, title: "14:05:00", children: "val" }));
    expect(narrowLeft).toContain("left:20px");
    const narrowRight = renderToStaticMarkup(createElement(Tooltip, { x: 100, width: 180, title: "14:05:00", children: "val" }));
    expect(narrowRight).toContain("right:20px");
  });
});
