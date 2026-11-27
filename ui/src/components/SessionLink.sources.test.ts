import { describe, expect, it } from "vitest";

// Every page names a session through SessionLink, so each such name opens the
// session (at any age) and its T3 Code thread. A hand-made link drifts: some
// once linked only sessions the snapshot still listed, and printed older ones
// as text. The Sessions list is the one exception: its rows already show what
// the badge's card would.
const sources = import.meta.glob(["../**/*.tsx", "!../**/*.test.tsx", "!../gen/**"], { query: "?raw", import: "default", eager: true }) as Record<string, string>;
const ALLOWED = new Set(["./SessionLink.tsx", "../pages/Sessions.tsx"]);

describe("session links", () => {
  it("go through SessionLink", () => {
    expect(Object.keys(sources)).toContain("../pages/Board.tsx");
    const handMade = Object.entries(sources)
      .filter(([path, text]) => !ALLOWED.has(path) && /\bto(=|:\s*)\{?["'`]\/sessions\/\$id/.test(text))
      .map(([path]) => path);
    expect(handMade).toEqual([]);
  });

  it("are not gated on the snapshot listing the session", () => {
    const gated = Object.entries(sources)
      .filter(([, text]) => /sessions\.(some|find)\(\(\w+\) => \w+\.id ===|new Set\([\w.?]*sessions\.map\(\(\w+\) => \w+\.id\)\)/.test(text))
      .map(([path]) => path);
    expect(gated).toEqual([]);
  });
});
