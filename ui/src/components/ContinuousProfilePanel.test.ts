import { create } from "@bufbuild/protobuf";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { ProfileBucketSchema, ProfileFrameSchema } from "../gen/hotbed/v1/hotbed_pb";
import { aggregateTimelineBuckets, resolveBucketTopFrame, selectDisplayTimeline, TimelineBuckets } from "./ContinuousProfilePanel";

describe("ContinuousProfilePanel timeline frame resolution", () => {
  it("resolves retained bucket topFrame IDs against the unfiltered frame table after zooming into a bucket", () => {
    const fullFrames = [
      create(ProfileFrameSchema, { name: "runtime.schedule", frameType: "go" }),
      create(ProfileFrameSchema, { name: "encodeResponse", frameType: "go" }),
      create(ProfileFrameSchema, { name: "vfs_write", frameType: "kernel" }),
    ];
    const b0 = create(ProfileBucketSchema, { t: 1000n, durationMs: 5000n, samples: 12n, userSamples: 10n, kernelSamples: 2n, topFrame: 0 });
    const b1 = create(ProfileBucketSchema, { t: 6000n, durationMs: 5000n, samples: 25n, userSamples: 20n, kernelSamples: 5n, topFrame: 1 });
    const b2 = create(ProfileBucketSchema, { t: 11000n, durationMs: 5000n, samples: 18n, userSamples: 4n, kernelSamples: 14n, topFrame: 2 });
    const rangeTimeline = { buckets: [b0, b1, b2], frames: fullFrames };

    // Drilling into b1 produces a filtered response whose frame table is
    // renumbered (e.g. only 1 frame at index 0, belonging to a different function
    // than fullFrames[0]).
    const filteredFrames = [
      create(ProfileFrameSchema, { name: "encodeResponse", frameType: "go" }),
    ];
    const filteredBucket = create(ProfileBucketSchema, { t: 6000n, durationMs: 5000n, samples: 25n, userSamples: 20n, kernelSamples: 5n, topFrame: 0 });
    const current = { buckets: [filteredBucket], frames: filteredFrames };

    const display = selectDisplayTimeline(b1, rangeTimeline, current);
    expect(display.buckets).toEqual([b0, b1, b2]);
    expect(display.buckets.map((b) => resolveBucketTopFrame(b, display.frames))).toEqual([
      "runtime.schedule",
      "encodeResponse",
      "vfs_write",
    ]);
  });

  it("uses the current response's buckets and frames when no bucket window is selected", () => {
    const oldFrames = [create(ProfileFrameSchema, { name: "oldFn", frameType: "go" })];
    const newFrames = [create(ProfileFrameSchema, { name: "newFn", frameType: "go" })];
    const bOld = create(ProfileBucketSchema, { t: 1000n, durationMs: 5000n, samples: 5n, topFrame: 0 });
    const bNew = create(ProfileBucketSchema, { t: 6000n, durationMs: 5000n, samples: 9n, topFrame: 0 });

    const display = selectDisplayTimeline(null, { buckets: [bOld, bNew], frames: oldFrames }, { buckets: [bNew], frames: newFrames });
    expect(display.buckets).toEqual([bNew]);
    expect(resolveBucketTopFrame(display.buckets[0]!, display.frames)).toBe("newFn");
  });

  it("downsamples 360 5s buckets to <= 60 merged buckets and renders 0-sample buckets as muted baselines", () => {
    const frames = [
      create(ProfileFrameSchema, { name: "idle", frameType: "kernel" }),
      create(ProfileFrameSchema, { name: "hotFn", frameType: "native" }),
    ];
    const buckets = Array.from({ length: 360 }, (_, i) =>
      create(ProfileBucketSchema, {
        t: BigInt(i * 5_000),
        durationMs: 5_000n,
        samples: i < 6 ? 0n : i === 10 ? 50n : BigInt((i % 10) + 1),
        userSamples: i < 6 ? 0n : i === 10 ? 50n : BigInt((i % 10) + 1),
        kernelSamples: 0n,
        topFrame: i === 10 ? 1 : 0,
      }),
    );

    const merged = aggregateTimelineBuckets(buckets, 60);
    expect(merged).toHaveLength(60);
    expect(merged[0]?.samples).toBe(0n);
    expect(merged[0]?.durationMs).toBe(30_000n);
    expect(merged[1]?.topFrame).toBe(1);
    const totalRaw = buckets.reduce((a, b) => a + b.samples, 0n);
    const totalMerged = merged.reduce((a, b) => a + b.samples, 0n);
    expect(totalMerged).toBe(totalRaw);

    const html = renderToStaticMarkup(
      createElement(TimelineBuckets, { buckets: merged, frames, selected: null, onSelect: () => {} }),
    );
    expect(html).toContain("bg-muted-foreground/25");
  });
});
