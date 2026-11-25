import { describe, expect, it } from "vitest";
import { nextSixHourCronMs, sortGcCells, summarizeGcKinds, type GcCellUsage } from "../lib/gc";

const SAMPLE: GcCellUsage[] = [
  {
    tenant: "0",
    cell: "0/cell-small",
    measuredUnixMs: 1_700_000_200_000,
    objects: 50,
    bytes: 500_000,
    kinds: {
      segment: { objects: 48, bytes: 490_000 },
      checkpoint: { objects: 1, bytes: 9_000 },
      "gc-lease": { objects: 1, bytes: 1_000 },
    },
  },
  {
    tenant: "0",
    cell: "0/cell-big",
    measuredUnixMs: 1_700_000_100_000,
    objects: 20,
    bytes: 5_000_000,
    kinds: {
      segment: { objects: 18, bytes: 4_500_000 },
      checkpoint: { objects: 2, bytes: 500_000 },
    },
  },
  {
    tenant: "0",
    cell: "0/cell-pending",
    measuredUnixMs: null,
    objects: null,
    bytes: null,
    kinds: null,
  },
];

describe("summarizeGcKinds", () => {
  it("sums objects and bytes per kind across measured cells sorted by bytes descending", () => {
    expect(summarizeGcKinds(SAMPLE)).toEqual([
      { kind: "segment", objects: 66, bytes: 4_990_000 },
      { kind: "checkpoint", objects: 3, bytes: 509_000 },
      { kind: "gc-lease", objects: 1, bytes: 1_000 },
    ]);
  });
});

describe("nextSixHourCronMs", () => {
  it("returns the next 6-hour UTC boundary strictly after now", () => {
    const t = Date.UTC(2026, 10, 25, 14, 15, 0);
    expect(nextSixHourCronMs(t)).toBe(Date.UTC(2026, 10, 25, 18, 0, 0));

    const exact = Date.UTC(2026, 10, 25, 18, 0, 0);
    expect(nextSixHourCronMs(exact)).toBe(Date.UTC(2026, 10, 26, 0, 0, 0));
  });
});

describe("sortGcCells", () => {
  it("sorts by bytes, objects, pending-first, and oldest swept, and filters by substring", () => {
    expect(sortGcCells(SAMPLE, "bytes", "").map((c) => c.cell)).toEqual([
      "0/cell-big",
      "0/cell-small",
      "0/cell-pending",
    ]);
    expect(sortGcCells(SAMPLE, "objects", "").map((c) => c.cell)).toEqual([
      "0/cell-small",
      "0/cell-big",
      "0/cell-pending",
    ]);
    expect(sortGcCells(SAMPLE, "unmeasured", "").map((c) => c.cell)).toEqual([
      "0/cell-pending",
      "0/cell-big",
      "0/cell-small",
    ]);
    expect(sortGcCells(SAMPLE, "oldest", "").map((c) => c.cell)).toEqual([
      "0/cell-big",
      "0/cell-small",
      "0/cell-pending",
    ]);
    expect(sortGcCells(SAMPLE, "bytes", "pend").map((c) => c.cell)).toEqual(["0/cell-pending"]);
  });
});
