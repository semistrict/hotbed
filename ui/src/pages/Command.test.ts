import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";
import { CommandLimitsSchema } from "../gen/hotbed/v1/hotbed_pb";
import { formatTimeLimit } from "./Command";

describe("formatTimeLimit", () => {
  it("formats soft and hard time limits from CommandLimits", () => {
    const both = create(CommandLimitsSchema, { softLimitSeconds: 600n, hardLimitSeconds: 900n });
    expect(formatTimeLimit("test", both)).toEqual({ value: "10m 0s", unit: "/ 15m 0s", foot: "soft / hard" });

    const softOnly = create(CommandLimitsSchema, { softLimitSeconds: 300n, hardLimitSeconds: 0n });
    expect(formatTimeLimit("shell", softOnly)).toEqual({ value: "5m 0s", foot: "soft limit" });

    const hardOnly = create(CommandLimitsSchema, { softLimitSeconds: 0n, hardLimitSeconds: 1800n });
    expect(formatTimeLimit("freshen", hardOnly)).toEqual({ value: "30m 0s", foot: "hard limit" });
  });

  it("reports history-only commands without recorded limits as unavailable rather than unlimited", () => {
    expect(formatTimeLimit("test", undefined)).toEqual({ value: "—", foot: "not recorded in history" });
    expect(formatTimeLimit("shell", undefined)).toEqual({ value: "—", foot: "not recorded in history" });
    expect(formatTimeLimit("server", undefined)).toEqual({ value: "—", foot: "not recorded in history" });
  });

  it("shows none (and dev server wording for servers) only when live limits are explicitly zero", () => {
    const none = create(CommandLimitsSchema, { softLimitSeconds: 0n, hardLimitSeconds: 0n });
    expect(formatTimeLimit("server", none)).toEqual({ value: "none", foot: "dev servers run until released" });
    expect(formatTimeLimit("shell", none)).toEqual({ value: "none", foot: undefined });
    expect(formatTimeLimit("freshen", none)).toEqual({ value: "none", foot: undefined });
  });
});
