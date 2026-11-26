import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";
import { CommandState } from "../api";
import { FinishedCommandSchema } from "../gen/hotbed/v1/hotbed_pb";
import { endedExit, endedHow } from "./exit";

describe("endedExit and endedHow", () => {
  it("never shows a command whose VM ended under it as killed by a signal", () => {
    const done = create(FinishedCommandSchema, { state: CommandState.FAILED, failure: "vm", reason: "its VM was killed by signal 9" });
    expect(endedExit(done)).toBe("—");
    expect(endedHow(done)).toBe("its VM was killed by signal 9");
  });

  it("shows a command's own exit, or the signal that ended it", () => {
    expect(endedExit(create(FinishedCommandSchema, { state: CommandState.FAILED, failure: "test", exitCode: 101 }))).toBe("101");
    expect(endedHow(create(FinishedCommandSchema, { state: CommandState.FAILED, failure: "test", exitCode: 101 }))).toBe("exit 101");
    expect(endedHow(create(FinishedCommandSchema, { state: CommandState.FAILED, failure: "signal" }))).toBe("exit signal");
    expect(endedExit(create(FinishedCommandSchema, { state: CommandState.SUCCEEDED }))).toBe("0");
    expect(endedExit(create(FinishedCommandSchema, { state: CommandState.CANCELLED }))).toBe("—");
  });
});
