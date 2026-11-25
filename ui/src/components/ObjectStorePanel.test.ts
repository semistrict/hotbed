import { create } from "@bufbuild/protobuf";
import { createElement, type ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";
import {
  BaseSchema,
  BaseState,
  CladefsCellSchema,
  NodeHourSchema,
  NodeHoursSchema,
  NodeSchema,
  ObjectStoreStatsSchema,
  SampleSchema,
  SamplesSchema,
  SnapshotSchema,
  TotalsSchema,
} from "../gen/hotbed/v1/hotbed_pb";

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, className }: { children: ReactNode; className?: string }) => createElement("a", { className }, children),
}));

import {
  ClusterObjectStoreSection,
  estimateR2MonthlyCost,
  formatUsd,
  nodeCacheHit,
  NodeObjectStoreSection,
  summarizeClusterObjectStore,
} from "./ObjectStorePanel";

describe("summarizeClusterObjectStore", () => {
  it("reports hasData=false when no node has object store counters or 24h transfer", () => {
    const s = create(SnapshotSchema, {
      nodes: [create(NodeSchema, { id: "n1", hostname: "box1", online: true })],
    });
    expect(summarizeClusterObjectStore(s).hasData).toBe(false);
  });

  it("aggregates since-boot counters, caches, DAX health, live rates, 24h totals, and estimated monthly R2 cost", () => {
    const s = create(SnapshotSchema, {
      bases: [
        // Two copies of the same warm base on different nodes: deduplicated to one 50 GB base in R2.
        create(BaseSchema, { id: "b1", nodeId: "n1", project: "cladefs", generation: 67, state: BaseState.WARM, sizeBytes: 50_000_000_000n, cladefsCell: create(CladefsCellSchema, { arch: "amd64" }) }),
        create(BaseSchema, { id: "b2", nodeId: "n2", project: "cladefs", generation: 67, state: BaseState.WARM, sizeBytes: 48_000_000_000n, cladefsCell: create(CladefsCellSchema, { arch: "amd64" }) }),
        create(BaseSchema, { id: "b3", nodeId: "n2", project: "cladefs", generation: 67, state: BaseState.WARM, sizeBytes: 50_000_000_000n, cladefsCell: create(CladefsCellSchema, { arch: "arm64" }) }),
      ],
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
            partCacheBytes: 30_000_000_000n,
            partCacheCapacityBytes: 500_000_000_000n,
            extentCacheBytes: 6_000_000_000n,
            extentCacheCapacityBytes: 6_442_450_944n,
            extentCachePinnedEntries: 2400n,
            extentCacheIdleEntries: 10_000n,
            cacheTrims: 3n,
            activeVms: 2,
            windowMappedBytes: 4_000_000_000n,
            daxWindowFull: 0n,
            daxWindowReclaims: 1n,
            daxRegionsFull: 0n,
            daxWritesHeldBySealsNs: 2_500_000_000n,
            daxDeviceReadBytes: 80_000_000n,
            daxDeviceReadsNs: 800_000_000n,
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
            partCacheBytes: 10_000_000_000n,
            partCacheCapacityBytes: 200_000_000_000n,
            extentCacheBytes: 1_000_000_000n,
            extentCacheCapacityBytes: 3_221_225_472n,
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
        r2PutRequests: 10_000n,
        r2GetRequests: 50_000n,
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
    expect(sum.partCacheBytes).toBe(40_000_000_000);
    expect(sum.extentCacheBytes).toBe(7_000_000_000);
    expect(sum.extentCachePinnedEntries).toBe(2400);
    expect(sum.extentCacheIdleEntries).toBe(10_000);
    expect(sum.cacheTrims).toBe(3);
    expect(sum.activeVms).toBe(2);
    expect(sum.windowMappedBytes).toBe(4_000_000_000);
    expect(sum.daxWindowReclaims).toBe(1);
    expect(sum.daxWritesHeldBySealsSec).toBeCloseTo(2.5, 5);
    expect(nodeCacheHit(s.nodes[0]?.objectStore)).toBeCloseTo(0.9, 5);
    expect(nodeCacheHit(s.nodes[1]?.objectStore)).toBeCloseTo(1.0, 5);

    // 100 GB deduplicated warm bases * $0.015 = $1.50
    // 10k puts/day * 30 = 300k Class A * $4.50/M = $1.35
    // 50k gets/day * 30 = 1.5M Class B * $0.36/M = $0.54
    const cost = estimateR2MonthlyCost(s, 10_000, 50_000);
    expect(cost.storageBytes).toBe(100_000_000_000);
    expect(cost.storageCost).toBeCloseTo(1.5, 5);
    expect(cost.classACost).toBeCloseTo(1.35, 5);
    expect(cost.classBCost).toBeCloseTo(0.54, 5);
    expect(cost.totalMonthly).toBeCloseTo(3.39, 5);
    expect(formatUsd(cost.totalMonthly)).toBe("$3.39");
    expect(formatUsd(0.002)).toBe("<$0.01");
  });

  it("shows empty cache state and uses full-width 24h transfer grid when no live objectStore exists", () => {
    const n = create(NodeSchema, { id: "n1", hostname: "offline-box", online: false });
    const s = create(SnapshotSchema, {
      nodes: [n],
      totals24h: create(TotalsSchema, { r2UploadBytes: 1_000_000n }),
      nodeHourly: {
        n1: create(NodeHoursSchema, {
          hours: [create(NodeHourSchema, { hour: 1000n, uploadBytes: 1_000_000n })],
        }),
      },
    });
    const clusterHtml = renderToStaticMarkup(
      createElement(ClusterObjectStoreSection, { s, colors: new Map([["n1", "#fff"]]) }),
    );
    expect(clusterHtml).toContain("No nodes are currently reporting live cache telemetry.");
    expect(clusterHtml).toContain("lg:col-span-6 xl:col-span-3");

    const nodeHtml = renderToStaticMarkup(createElement(NodeObjectStoreSection, { n, s }));
    expect(nodeHtml).toContain("lg:grid-cols-1");
  });
});
