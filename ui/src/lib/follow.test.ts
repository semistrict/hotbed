import { describe, expect, it } from "vitest";
import { atEnd } from "./follow";

describe("atEnd", () => {
  it("is at the end when scrolled to the bottom", () => {
    expect(atEnd({ scrollTop: 480, scrollHeight: 1000, clientHeight: 520 })).toBe(true);
  });

  it("forgives a subpixel shortfall", () => {
    expect(atEnd({ scrollTop: 477.5, scrollHeight: 1000, clientHeight: 520 })).toBe(true);
  });

  it("is not at the end once the reader scrolls up", () => {
    expect(atEnd({ scrollTop: 300, scrollHeight: 1000, clientHeight: 520 })).toBe(false);
  });

  it("is at the end when everything fits", () => {
    expect(atEnd({ scrollTop: 0, scrollHeight: 200, clientHeight: 520 })).toBe(true);
  });
});
