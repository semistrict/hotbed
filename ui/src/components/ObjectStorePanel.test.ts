import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";
import {
  NodeSchema,
  ObjectStoreStatsSchema,
  SampleSchema,
  SamplesSchema,
  SnapshotSchema,
  TotalsSchema,
} from "../gen/hotbed/v1/hotbed_pb";
import { nodeCacheHit, summarizeClusterObjectStore } from "./ObjectStorePanel";

describe("summarizeClusterObjectStore", () => {
  it("reports hasData=false when no node has object store counters or 24h transfer", () => {
    const s = create(SnapshotSchema, {
      nodes: [create(NodeSchema, { id: "n1", hostname: "box1", online: true })],
    });
    expect(summarizeClusterObjectStore(s).hasData).toBe(false);
  });

  it("aggregates since-boot counters, live rates, 24h totals, and local cache hit ratio", () => {
    const s = create(SnapshotSchema, {
      nodes: [
        create(NodeSchema, {
          id: "n1",
          hostname: "ubu1",
          online: true,
          objectStore: create(ObjectStoreStatsSchema, {
            uploadedBytes: 20_000_000_000n,
            uploadedSegmentsBytes: 19_000_000_000n,
            uploadedExtentsBytes: 500_000_000n,
            uploadedCheckpointsBytes: 400_000_000n,
            uploadedOtherBytes: 100_000_000n,
            downloadedBytes: 2_000_000_000n,
            pageInBytes: 9_000_000_000n,
            fetchedBytes: 1_000_000_000n,
            failedRequests: 2n,
          }),
        }),
        create(NodeSchema, {
          id: "n2",
          hostname: "asahimbp",
          online: true,
          objectStore: create(ObjectStoreStatsSchema, {
            uploadedBytes: 1_000_000_000n,
            uploadedSegmentsBytes: 1_000_000_000n,
            downloadedBytes: 500_000_000n,
            pageInBytes: 1_000_000_000n,
            fetchedBytes: 0n,
          }),
        }),
      ],
      series: {
        n1: create(SamplesSchema, {
          samples: [
            create(SampleSchema, {
              t: 1000n,
              uploadBytesPerSec: 12_500_000,
              downloadBytesPerSec: 2_000_000,
            }),
          ],
        }),
        n2: create(SamplesSchema, {
          samples: [
            create(SampleSchema, {
              t: 1000n,
              uploadBytesPerSec: 500_000,
              downloadBytesPerSec: 1_000_000,
            }),
          ],
        }),
      },
      totals24h: create(TotalsSchema, {
        r2UploadBytes: 5_000_000_000n,
        r2DownloadBytes: 750_000_000n,
        r2PutRequests: 400n,
        r2GetRequests: 120n,
        r2FailedRequests: 1n,
        r2CommitWaitSeconds: 12.5,
      }),
    });

    const sum = summarizeClusterObjectStore(s);
    expect(sum.hasData).toBe(true);
    expect(sum.uploaded24h).toBe(5_000_000_000);
    expect(sum.downloaded24h).toBe(750_000_000);
    expect(sum.sinceBootUp).toBe(21_000_000_000);
    expect(sum.sinceBootSegments).toBe(20_000_000_000);
    expect(sum.liveUpBps).toBe(13_000_000);
    expect(sum.liveDownBps).toBe(3_000_000);
    expect(sum.commitWait24hSec).toBe(12.5);
    expect(sum.cacheHitRatio).toBeCloseTo(10 / 11, 5);
    expect(nodeCacheHit(s.nodes[0]?.objectStore)).toBeCloseTo(0.9, 5);
    expect(nodeCacheHit(s.nodes[1]?.objectStore)).toBeCloseTo(1.0, 5);
  });
});
