import { describe, expect, it } from "vitest";
import { ago, bytes, compact, duration, durationParts, limit, percent, quantile, shortSha, speedup } from "./format";

const GiB = 1024 ** 3;

describe("bytes", () => {
  it("picks the unit", () => {
    expect(bytes(512)).toBe("512 B");
    expect(bytes(812 * 1024 ** 2)).toBe("812 MiB");
    expect(bytes(1.44 * GiB)).toBe("1.4 GiB");
    expect(bytes(24 * GiB)).toBe("24 GiB");
    expect(bytes(212.4 * GiB)).toBe("212 GiB");
    expect(bytes(1.9 * 1024 * GiB)).toBe("1.9 TiB");
  });
});

describe("limit", () => {
  it("reads 0 as no limit, not as zero bytes", () => {
    expect(limit(0)).toBe("none");
    expect(limit(4 * GiB)).toBe("4 GiB");
  });
});

describe("shortSha", () => {
  it("keeps the first 10 hex digits", () => {
    expect(shortSha("0082e3d3910223c703c9ab8d0b7f123bd34add2d")).toBe("0082e3d391");
  });
});

describe("duration", () => {
  it("reads like a person would say it", () => {
    expect(duration(0.038)).toBe("38 ms");
    expect(duration(4.24)).toBe("4.2 s");
    expect(duration(14.2)).toBe("14 s");
    expect(duration(370)).toBe("6m 10s");
    expect(duration(148_320)).toBe("41h 12m");
  });

  it("carries a rounded-up unit", () => {
    expect(duration(119.7)).toBe("2m 0s");
  });
});

describe("durationParts", () => {
  it("splits a hero figure", () => {
    expect(durationParts(148_320)).toEqual([["41", "h"], ["12", "m"]]);
    expect(durationParts(370)).toEqual([["6", "m"], ["10", "s"]]);
  });
});

describe("numbers", () => {
  it("formats percent, compact and speedup", () => {
    expect(percent(0.4123)).toBe("41%");
    expect(compact(1284)).toBe("1,284");
    expect(compact(12_940)).toBe("12.9K");
    expect(speedup(370, 14.2)).toBe("26×");
    expect(speedup(30, 12)).toBe("2.5×");
  });

  it("takes quantiles of unsorted input", () => {
    expect(quantile([5, 1, 3, 2, 4], 0.5)).toBe(3);
    expect(quantile([], 0.5)).toBe(0);
  });

  it("says how long ago", () => {
    const now = Date.parse("2026-11-21T12:00:00Z");
    expect(ago("2026-11-21T11:59:58Z", now)).toBe("now");
    expect(ago("2026-11-21T11:56:00Z", now)).toBe("4m ago");
    expect(ago("2026-11-21T08:48:00Z", now)).toBe("3h ago");
  });
});
