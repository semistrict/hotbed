import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";
import { CommandSampleSchema } from "../gen/hotbed/v1/hotbed_pb";
import { relisted, samplesShown } from "./CommandSamples";

const sample = (seq: number) => create(CommandSampleSchema, { commandId: "c1", seq });
const seqs = (ss: { seq: number }[]) => ss.map((s) => s.seq);

describe("sample lists", () => {
  it("show a sample kept here that the controller has not listed yet", () => {
    expect(seqs(samplesShown({ listed: [sample(1), sample(2)], seeded: [sample(3)] }))).toEqual([1, 2, 3]);
  });

  it("show a seed below the newest listed too, filling the gap a later sample left", () => {
    expect(seqs(samplesShown({ listed: [sample(1), sample(3)], seeded: [sample(2)] }))).toEqual([1, 2, 3]);
  });

  it("drop seeds once the controller lists them, keeping the rest", () => {
    const next = relisted([sample(1), sample(2), sample(3)], { listed: [sample(1)], seeded: [sample(3), sample(4)] });
    expect(seqs(next.listed)).toEqual([1, 2, 3]);
    expect(seqs(next.seeded)).toEqual([4]);
  });

  it("start from nothing when nothing was listed or seeded", () => {
    expect(relisted([sample(1)], undefined)).toEqual({ listed: [sample(1)], seeded: [] });
  });
});
