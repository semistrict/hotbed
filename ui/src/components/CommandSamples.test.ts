import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";
import { CommandSampleSchema } from "../gen/hotbed/v1/hotbed_pb";
import { withKnown } from "./CommandSamples";

const sample = (seq: number) => create(CommandSampleSchema, { commandId: "c1", seq });

describe("withKnown", () => {
  it("keeps a sample known here that the listing does not have yet", () => {
    expect(withKnown([sample(1), sample(2)], [sample(1), sample(2), sample(3)]).map((s) => s.seq)).toEqual([1, 2, 3]);
  });

  it("takes the listing as it is for every sample it has", () => {
    const listed = [sample(1), sample(2), sample(3)];
    expect(withKnown(listed, [sample(3)])).toEqual(listed);
    expect(withKnown(listed, undefined)).toEqual(listed);
  });

  it("adds a just-kept sample to a list never fetched", () => {
    expect(withKnown([], [sample(4)]).map((s) => s.seq)).toEqual([4]);
  });
});
