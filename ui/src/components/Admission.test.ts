import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";
import { AdmissionSignalSchema, PlacementDecisionSchema, PlacementSchema, SampleSchema } from "../gen/hotbed/v1/hotbed_pb";
import { heldBands, placementRequirements, signalRule } from "./Admission";

const sample = (t: bigint, holding: boolean) => create(SampleSchema, { t, holding });

describe("heldBands", () => {
  it("is each run of samples in which the node held new work, one still going included", () => {
    const bands = heldBands([sample(0n, false), sample(5n, true), sample(10n, true), sample(15n, false), sample(20n, true), sample(25n, true)]);
    expect(bands.map((b) => [b.from, b.to])).toEqual([
      [5, 15],
      [20, 25],
    ]);
  });
});

describe("signalRule", () => {
  it("says where a signal holds work and where held work resumes", () => {
    const cpu = create(AdmissionSignalSchema, { name: "cpu-pressure", value: 72, limit: 60, resume: 30, unit: "%", floor: false, holding: true });
    expect(signalRule(cpu)).toBe("holds at 60% · resumes under 30%");
    expect(signalRule(create(AdmissionSignalSchema, { ...cpu, name: "disk-free", unit: "bytes", floor: true, limit: 5 * 1024 ** 3, resume: 0 }))).toBe("holds under 5 GiB");
    expect(
      signalRule(create(AdmissionSignalSchema, { ...cpu, name: "disk-free", unit: "bytes", floor: true, limit: 5 * 1024 ** 3, resume: 10 * 1024 ** 3 })),
    ).toBe("holds under 5 GiB · resumes above 10 GiB");
  });
});

describe("placementRequirements", () => {
  it("returns null for legacy decisions without a recorded request", () => {
    expect(placementRequirements(create(PlacementDecisionSchema, { hostname: "ubu1", reason: "the node asked for" }))).toBeNull();
  });

  it("returns empty array for unconstrained decisions and marks pinned constraints", () => {
    expect(placementRequirements(create(PlacementDecisionSchema, { request: create(PlacementSchema) }))).toEqual([]);
    const pinned = create(PlacementDecisionSchema, {
      request: create(PlacementSchema, {
        node: "ubu1",
        arch: "amd64",
        runtime: "vm",
        bench: true,
        memoryLowBytes: 4n << 30n,
        vmCpus: 4,
        vmMemoryBytes: 8n << 30n,
      }),
      noSync: true,
      throwaway: true,
    });
    expect(placementRequirements(pinned)).toEqual([
      { label: "--node ubu1", pinned: true },
      { label: "--arch amd64", pinned: true },
      { label: "--runtime vm" },
      { label: "--bench", pinned: true },
      { label: "--mem-low 4 GiB" },
      { label: "--vm-cpus 4" },
      { label: "--vm-mem 8 GiB" },
      { label: "--no-sync", pinned: true },
      { label: "--fork" },
    ]);
  });
});
