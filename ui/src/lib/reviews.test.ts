import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";
import { ActorKind, ReviewSchema, ReviewerSchema, StampSchema } from "../gen/hotbed/v1/hotbed_pb";
import { commitUrl, reviewerModels } from "./reviews";
import { actorSummary, isUserActor } from "./stamps";

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

describe("actor attribution and reviewer models", () => {
  it("distinguishes explicit USER and AGENT stamps as well as legacy empty-session stamps", () => {
    const user = create(StampSchema, { actorKind: ActorKind.USER, user: "ramon@example.com" });
    const legacyUser = create(StampSchema, { session: "" });
    const agent = create(StampSchema, {
      actorKind: ActorKind.AGENT,
      session: "brave-otter",
      model: "gemini-4-neon",
      subagent: "codex-adv",
    });
    expect(isUserActor(user)).toBe(true);
    expect(isUserActor(legacyUser)).toBe(true);
    expect(isUserActor(agent)).toBe(false);
    expect(actorSummary(user)).toBe("User (ramon@example.com)");
    expect(actorSummary(agent)).toBe("Agent (gemini-4-neon · codex-adv · brave-otter)");
  });

  it("deduplicates reviewer models in start order", () => {
    const r = create(ReviewSchema, {
      reviewers: [
        create(ReviewerSchema, { id: 1, model: "claude-opus-4-6" }),
        create(ReviewerSchema, { id: 2, model: "gpt-5.5-codex" }),
        create(ReviewerSchema, { id: 3, model: "claude-opus-4-6" }),
      ],
    });
    expect(reviewerModels(r)).toEqual(["claude-opus-4-6", "gpt-5.5-codex"]);
  });
});
