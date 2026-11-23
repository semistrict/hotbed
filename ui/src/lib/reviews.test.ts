import { describe, expect, it } from "vitest";
import { commitUrl } from "./reviews";

describe("commitUrl", () => {
  const sha = "8edf54eddb3e676375fe8ef97f34e26a96f84ca3";
  it("links a GitHub repo's commit", () => {
    expect(commitUrl("github.com/semistrict/hotbed", sha)).toBe(`https://github.com/semistrict/hotbed/commit/${sha}`);
  });
  it("links nothing elsewhere, or with no repo", () => {
    expect(commitUrl("example.invalid/o/app", sha)).toBe(null);
    expect(commitUrl(null, sha)).toBe(null);
  });
});
