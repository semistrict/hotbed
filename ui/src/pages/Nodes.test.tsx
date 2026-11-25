import { create } from "@bufbuild/protobuf";
import type { ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";
import {
  DiskUsageSchema,
  NodeSchema,
  NodeUsageSchema,
  ObjectStoreStatsSchema,
  ResourcesSchema,
  SnapshotSchema,
} from "../gen/hotbed/v1/hotbed_pb";

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, className }: { children: ReactNode; className?: string }) => <a className={className}>{children}</a>,
}));

import { NodeCard, NodeTable } from "./Nodes";

describe("Nodes cladefs stats rendering", () => {
  it("shows cladefs disk, part cache, extent cache, R2, and DAX stats on VM nodes instead of zfs", () => {
    const n = create(NodeSchema, {
      id: "n1",
      hostname: "ubu1",
      online: true,
      storage: "zfs",
      diskBytes: 2_000_000_000_000n,
      disk: create(DiskUsageSchema, { freeBytes: 1_500_000_000_000n }),
      resources: create(ResourcesSchema, { arch: "amd64", cpus: 32, memoryBytes: 64n * 1024n ** 3n }),
      usage: create(NodeUsageSchema, { cpuBusy: 0.2, memoryUsedBytes: 16n * 1024n ** 3n }),
      objectStore: create(ObjectStoreStatsSchema, {
        uploadedBytes: 65_000_000_000n,
        downloadedBytes: 2_000_000_000n,
        pageInBytes: 9_000_000_000n,
        fetchedBytes: 1_000_000_000n,
        partCacheBytes: 160n * 1024n ** 3n,
        partCacheCapacityBytes: 600n * 1024n ** 3n,
        extentCacheBytes: 4n * 1024n ** 3n,
        extentCacheCapacityBytes: 6n * 1024n ** 3n,
        hostDiskSizeBytes: 1000n * 1024n ** 3n,
        hostDiskFreeBytes: 700n * 1024n ** 3n,
        hostDiskCacheBytes: 160n * 1024n ** 3n,
        hostDiskCellsBytes: 40n * 1024n ** 3n,
        hostDiskOtherBytes: 100n * 1024n ** 3n,
        activeVms: 2,
        windowMappedBytes: 512n * 1024n ** 2n,
        peerPartsGiven: 120n,
        peerPartsServed: 45n,
      }),
    });
    const s = create(SnapshotSchema, { now: 100_000n, nodes: [n] });
    const cardHtml = renderToStaticMarkup(<NodeCard n={n} s={s} color="#fff" now={100_000} delay={0} />);
    expect(cardHtml).toContain("cladefs");
    expect(cardHtml).toContain("part cache");
    expect(cardHtml).toContain("extent cache");
    expect(cardHtml).toContain("90% local");
    expect(cardHtml).toContain("2 VMs · 512 MiB DAX");
    expect(cardHtml).toContain("peers ↓120 ↑45");
    expect(cardHtml).not.toContain(">zfs<");

    const tableHtml = renderToStaticMarkup(<NodeTable s={s} />);
    expect(tableHtml).toContain("part cache");
    expect(tableHtml).toContain("cache mem");
    expect(tableHtml).toContain("R2 ↑ / ↓");
    expect(tableHtml).toContain("local hit");
    expect(tableHtml).toContain("90%");
    expect(tableHtml).toContain("700 GiB");
    expect(tableHtml).toContain("sticky left-0 z-10 bg-card");
  });

  it("shows page cache and FUSE cells on host-FUSE cladefs nodes instead of btrfs", () => {
    const n = create(NodeSchema, {
      id: "n2",
      hostname: "asahimbp",
      online: true,
      storage: "btrfs",
      diskBytes: 400_000_000_000n,
      disk: create(DiskUsageSchema, { freeBytes: 70_000_000_000n }),
      resources: create(ResourcesSchema, { arch: "arm64", cpus: 10, memoryBytes: 32n * 1024n ** 3n }),
      usage: create(NodeUsageSchema, { cpuBusy: 0.1, memoryUsedBytes: 8n * 1024n ** 3n }),
      objectStore: create(ObjectStoreStatsSchema, {
        uploadedBytes: 1_000_000_000n,
        downloadedBytes: 500_000_000n,
        partCacheBytes: 70n * 1024n ** 3n,
        partCacheCapacityBytes: 150n * 1024n ** 3n,
        extentCacheBytes: 0n,
        extentCacheCapacityBytes: 3n * 1024n ** 3n,
        pageCacheBytes: 128n * 1024n ** 2n,
        pageCacheDirtyBytes: 4n * 1024n ** 2n,
        pageCacheLimitBytes: 512n * 1024n ** 2n,
        mountCellsAttached: 3,
      }),
    });
    const s = create(SnapshotSchema, { now: 100_000n, nodes: [n] });
    const cardHtml = renderToStaticMarkup(<NodeCard n={n} s={s} color="#fff" now={100_000} delay={0} />);
    expect(cardHtml).toContain("part cache");
    expect(cardHtml).toContain("page cache");
    expect(cardHtml).toContain("4 MiB dirty");
    expect(cardHtml).toContain("3 FUSE cells");
    expect(cardHtml).not.toContain(">btrfs<");
  });

  it("keeps legacy storage bar when a node has no cladefs objectStore telemetry", () => {
    const n = create(NodeSchema, {
      id: "n3",
      hostname: "legacy",
      online: true,
      storage: "zfs",
      diskBytes: 500n * 1024n ** 3n,
      disk: create(DiskUsageSchema, { freeBytes: 250n * 1024n ** 3n }),
      resources: create(ResourcesSchema, { arch: "amd64", cpus: 8, memoryBytes: 16n * 1024n ** 3n }),
      usage: create(NodeUsageSchema, { cpuBusy: 0.05, memoryUsedBytes: 4n * 1024n ** 3n }),
    });
    const s = create(SnapshotSchema, { now: 100_000n, nodes: [n] });
    const cardHtml = renderToStaticMarkup(<NodeCard n={n} s={s} color="#fff" now={100_000} delay={0} />);
    expect(cardHtml).toContain(">zfs<");
    expect(cardHtml).toContain("250 GiB free of 500 GiB");
  });
});
