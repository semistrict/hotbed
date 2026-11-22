import { describe, expect, it } from "vitest";
import { searchLog } from "./logsearch";

const log = ["ok   github.com/semistrict/hotbed/internal/api 1.020s", "?    github.com/semistrict/hotbed/internal/fake [no test files]", "--- FAIL: TestFork (0.01s)", "FAIL github.com/semistrict/hotbed/internal/worker"];

describe("searchLog", () => {
  it("finds the lines holding the query, ignoring case", () => {
    expect(searchLog(log, "fail")).toEqual([
      { line: 2, ranges: [[4, 8]] },
      { line: 3, ranges: [[0, 4]] },
    ]);
  });

  it("marks every match in a line without overlapping", () => {
    expect(searchLog(["aaaa"], "aa")).toEqual([{ line: 0, ranges: [[0, 2], [2, 4]] }]);
  });

  it("matches nothing for a blank query", () => {
    expect(searchLog(log, "  ")).toEqual([]);
  });

  it("keeps spaces inside the query", () => {
    expect(searchLog(log, "no test")).toEqual([{ line: 1, ranges: [[49, 56]] }]);
  });
});
