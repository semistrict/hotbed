import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";
import { FoldedStackSchema, ProfileFrameSchema, SampledProcessSchema } from "../gen/hotbed/v1/hotbed_pb";
import { signedBytes } from "../lib/format";
import { buildCallTreeFromStacks, buildCallTreesByComm } from "./ProcessSamplePanel";

describe("ProcessSamplePanel call tree helpers", () => {
  const frames = [
    create(ProfileFrameSchema, { name: "rustc", frameType: "process" }),
    create(ProfileFrameSchema, { name: "main", module: "rustc", frameType: "native" }),
    create(ProfileFrameSchema, { name: "recurse", module: "rustc", frameType: "native" }),
    create(ProfileFrameSchema, { name: "vfs_write", module: "vmlinux", frameType: "kernel" }),
    create(ProfileFrameSchema, { name: "cargo", frameType: "process" }),
    create(ProfileFrameSchema, { name: "cargo::ops::compile", module: "cargo", frameType: "native" }),
  ];

  it("groups multiple PIDs sharing the same comm into a single aggregated call tree with accurate percentages", () => {
    const procs = [
      create(SampledProcessSchema, { pid: 101, comm: "rustc", cpuPct: 80 }),
      create(SampledProcessSchema, { pid: 102, comm: "rustc", cpuPct: 70 }),
      create(SampledProcessSchema, { pid: 103, comm: "rustc", cpuPct: 60 }),
      create(SampledProcessSchema, { pid: 200, comm: "cargo", cpuPct: 10 }),
    ];
    const stacks = [
      create(FoldedStackSchema, { comm: "rustc", frames: [0, 1, 3], samples: 30n }),
      create(FoldedStackSchema, { comm: "rustc", frames: [0, 1, 2], samples: 10n }),
      create(FoldedStackSchema, { comm: "cargo", frames: [4, 5], samples: 5n }),
    ];

    const groups = buildCallTreesByComm(procs, stacks, frames);
    expect(groups).toHaveLength(2);

    const rustc = groups[0]!;
    expect(rustc.comm).toBe("rustc");
    expect(rustc.procs.map((p) => p.pid)).toEqual([101, 102, 103]);
    expect(rustc.totalSamples).toBe(40);
    expect(rustc.nodes).toHaveLength(1);
    expect(rustc.nodes[0]?.symbol).toBe("main");
    expect(rustc.nodes[0]?.samples).toBe(40);
    expect(rustc.nodes[0]?.pct).toBeCloseTo(100);
    expect(rustc.nodes[0]?.children.map((c) => [c.symbol, c.samples, c.pct])).toEqual([
      ["vfs_write", 30, 75],
      ["recurse", 10, 25],
    ]);

    const cargo = groups[1]!;
    expect(cargo.comm).toBe("cargo");
    expect(cargo.procs.map((p) => p.pid)).toEqual([200]);
    expect(cargo.totalSamples).toBe(5);
    expect(cargo.nodes[0]?.symbol).toBe("cargo::ops::compile");
    expect(cargo.nodes[0]?.pct).toBeCloseTo(100);
  });

  it("handles out-of-bounds frame indices safely without throwing or corrupting counts", () => {
    const stacks = [
      create(FoldedStackSchema, { comm: "rustc", frames: [0, 999, 1, 500, 3], samples: 10n }),
      create(FoldedStackSchema, { comm: "rustc", frames: [999, 1000], samples: 25n }),
    ];

    const tree = buildCallTreeFromStacks(stacks, frames, "rustc");
    expect(tree).toHaveLength(1);
    expect(tree[0]?.symbol).toBe("main");
    expect(tree[0]?.samples).toBe(10);
    expect(tree[0]?.pct).toBeCloseTo(100);
    expect(tree[0]?.children[0]?.symbol).toBe("vfs_write");
    expect(tree[0]?.children[0]?.frameType).toBe("kernel");
    expect(tree[0]?.children[0]?.samples).toBe(10);
    expect(tree[0]?.children[0]?.pct).toBeCloseTo(100);
  });

  it("does not double-count recursive frames within a stack", () => {
    const stacks = [
      create(FoldedStackSchema, { comm: "rustc", frames: [0, 1, 2, 2, 3], samples: 10n }),
      create(FoldedStackSchema, { comm: "rustc", frames: [0, 1, 2], samples: 5n }),
    ];

    const tree = buildCallTreeFromStacks(stacks, frames, "rustc");
    expect(tree).toHaveLength(1);
    const main = tree[0]!;
    expect(main.symbol).toBe("main");
    expect(main.samples).toBe(15);
    expect(main.pct).toBeCloseTo(100);

    const rec1 = main.children[0]!;
    expect(rec1.symbol).toBe("recurse");
    expect(rec1.samples).toBe(15);
    expect(rec1.pct).toBeCloseTo(100);

    const rec2 = rec1.children[0]!;
    expect(rec2.symbol).toBe("recurse");
    expect(rec2.samples).toBe(10);
    expect(rec2.pct).toBeCloseTo((100 * 10) / 15);

    const leaf = rec2.children[0]!;
    expect(leaf.symbol).toBe("vfs_write");
    expect(leaf.samples).toBe(10);
    expect(leaf.pct).toBeCloseTo((100 * 10) / 15);
  });

  it("formats signed byte deltas for negative and positive values without returning an em-dash", () => {
    expect(signedBytes(2048n)).toBe("+2 KiB");
    expect(signedBytes(-4096n)).toBe("-4 KiB");
    expect(signedBytes(0n)).toBe("+0 B");
    expect(signedBytes(Number.NaN)).toBe("—");
  });
});
