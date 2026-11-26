import { describe, expect, it } from "vitest";
import { atEnd, atStart, heldView } from "./follow";

describe("atEnd and atStart", () => {
  it("count a reader within a few pixels of the edge as at it", () => {
    expect(atEnd({ scrollTop: 497, scrollHeight: 1000, clientHeight: 500 })).toBe(true);
    expect(atEnd({ scrollTop: 400, scrollHeight: 1000, clientHeight: 500 })).toBe(false);
    expect(atStart({ scrollTop: 3 })).toBe(true);
    expect(atStart({ scrollTop: 40 })).toBe(false);
  });
});

describe("heldView", () => {
  const read = ["a", "b"];
  const newer = ["b", "c"];

  it("shows the newest data while the reader follows", () => {
    expect(heldView(newer, read, true, true)).toEqual({ shown: newer, holding: false, behind: false });
  });

  it("holds what the reader was reading once they scroll away, and says newer data waits", () => {
    expect(heldView(newer, read, true, false)).toEqual({ shown: read, holding: true, behind: true });
    expect(heldView(read, read, true, false)).toEqual({ shown: read, holding: true, behind: false });
  });

  it("never holds a view that is not live", () => {
    expect(heldView(newer, read, false, false)).toEqual({ shown: newer, holding: false, behind: false });
  });
});
