import { create } from "@bufbuild/protobuf";
import { createElement, type ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";
import {
  NodeSchema,
  ObjectStoreStatsSchema,
  PartCacheShardSchema,
  PartCacheShardState,
  PartFetchesSchema,
  SnapshotSchema,
  TotalsSchema,
} from "../api";

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, className }: { children: ReactNode; className?: string }) => createElement("a", { className }, children),
}));

import { ClusterPeerCacheCard, NodePeerCacheCard, summarizePeerMesh } from "./PeerCachePanel";

const UBU = "100.73.156.10:9185";
const ASAHI = "100.71.192.45:9185";
const VM1 = "100.111.63.55:9185";
const GONE = "100.99.99.99:9185";

function snapshot() {
  return create(SnapshotSchema, {
    nodes: [
      create(NodeSchema, {
        id: "n1",
        hostname: "ubu1",
        online: true,
        objectStore: create(ObjectStoreStatsSchema, {
          peerAddress: UBU,
          peers: [VM1, ASAHI, UBU],
          peerPartsServed: 29_844n,
          peerServedBytes: 31_000_000_000n,
          // Reads 3 GB from memory, 1 GB from its SSD cache, 1 GB from R2.
          partFetches: create(PartFetchesSchema, {
            memory: 3000n,
            memoryBytes: 3_000_000_000n,
            partCache: 1000n,
            partCacheBytes: 1_000_000_000n,
            bucket: 1000n,
            bucketBytes: 1_000_000_000n,
            tailsFromPartCache: 40n,
            tailsFromBucket: 2n,
          }),
          partCacheShards: [
            create(PartCacheShardSchema, { shard: 0, device: "230:16", state: PartCacheShardState.ONLINE }),
            create(PartCacheShardSchema, { shard: 1, device: "230:0", state: PartCacheShardState.SUSPECT }),
          ],
        }),
      }),
      create(NodeSchema, {
        id: "n2",
        hostname: "asahimbp",
        online: true,
        objectStore: create(ObjectStoreStatsSchema, {
          peerAddress: ASAHI,
          // Never got vm1, still lists a retired host.
          peers: [ASAHI, UBU, GONE],
          peerPartsAsked: 47_800n,
          peerPartsGiven: 47_428n,
          peerGivenBytes: 49_000_000_000n,
          partFetches: create(PartFetchesSchema, { peer: 47_428n, peerBytes: 49_000_000_000n, bucket: 100n, bucketBytes: 1_000_000_000n }),
          peerPartsFailed: 369n,
          peerPartsMissing: 3n,
          peerPartsServed: 1_034n,
          peerServedPartsMissing: 7n,
          partCacheShards: [create(PartCacheShardSchema, { shard: 0, device: "7:0", state: PartCacheShardState.ONLINE })],
        }),
      }),
      create(NodeSchema, {
        id: "n3",
        hostname: "vm1",
        online: true,
        objectStore: create(ObjectStoreStatsSchema, {
          peerAddress: VM1,
          peers: [VM1, ASAHI, UBU],
          peerPartsAsked: 2_148n,
          peerPartsGiven: 2_148n,
          peerPartsServed: 5_265n,
        }),
      }),
      // A node without a peer address takes no part in the mesh.
      create(NodeSchema, { id: "n4", hostname: "mac", online: true, objectStore: create(ObjectStoreStatsSchema, {}) }),
    ],
    totals24h: create(TotalsSchema, {
      peerPartsGiven: 30_000n,
      peerGivenBytes: 30_000_000_000n,
      peerPartsServed: 29_000n,
      peerServedBytes: 29_500_000_000n,
      r2GetRequests: 10_000n,
      r2DownloadBytes: 10_000_000_000n,
    }),
  });
}

describe("summarizePeerMesh", () => {
  it("finds each member's missing and stale peers and totals the outcomes", () => {
    const sum = summarizePeerMesh(snapshot());
    expect(sum.expected).toEqual([VM1, ASAHI, UBU]);
    expect(sum.members.map((m) => m.node.hostname)).toEqual(["ubu1", "asahimbp", "vm1"]);
    expect(sum.inSync).toBe(false);
    const asahi = sum.members[1]!;
    expect(asahi.missingPeers).toEqual([VM1]);
    expect(asahi.extraPeers).toEqual([GONE]);
    expect(sum.members[0]!.missingPeers).toEqual([]);
    expect(sum.members[0]!.degradedShards).toBe(1);
    expect(sum.members[0]!.shards).toBe(2);
    expect(sum.members[0]!.localHit).toBeCloseTo(0.8, 5);
    expect(sum.members[1]!.localHit).toBe(0);
    expect(sum.members[2]!.localHit).toBeNull();
    expect(sum.asked).toBe(49_948);
    expect(sum.given).toBe(49_576);
    expect(sum.givenBytes).toBe(49_000_000_000);
    expect(sum.servedBytes).toBe(31_000_000_000);
    expect(sum.fetches).toEqual({
      memory: 3000,
      memoryBytes: 3_000_000_000,
      partCache: 1000,
      partCacheBytes: 1_000_000_000,
      peer: 47_428,
      peerBytes: 49_000_000_000,
      bucket: 1100,
      bucketBytes: 2_000_000_000,
      tailsFromPartCache: 40,
      tailsFromBucket: 2,
    });
    expect(sum.failed).toBe(369);
    expect(sum.missing).toBe(3);
    expect(sum.served).toBe(36_143);
    expect(sum.servedMissing).toBe(7);
    expect(sum.given24h).toBe(30_000);
    expect(sum.served24h).toBe(29_000);
    expect(sum.givenBytes24h).toBe(30_000_000_000);
    expect(sum.servedBytes24h).toBe(29_500_000_000);
    expect(sum.peerShare24h).toBeCloseTo(0.75, 5);
  });

  it("is in sync when every online member applied exactly the online addresses", () => {
    const s = snapshot();
    s.nodes[1]!.objectStore!.peers = [ASAHI, UBU, VM1];
    expect(summarizePeerMesh(s).inSync).toBe(true);
  });
});

describe("peer cache cards", () => {
  it("renders the cluster mesh table with problems called out", () => {
    const html = renderToStaticMarkup(createElement(ClusterPeerCacheCard, { s: snapshot(), colors: new Map() }));
    expect(html).toContain("Distributed part cache");
    expect(html).toContain("mesh out of sync");
    expect(html).toContain("lacks vm1");
    expect(html).toContain(`stale ${GONE}`);
    expect(html).toContain("1 of 2 degraded");
    expect(html).toContain("75.0%");
    expect(html).toContain("of 49.9K asked");
    expect(html).toContain("object tails 40 from SSD cache, 2 from R2");
    expect(html).toContain("80.0%");
    expect(html).toContain('<td class="num px-2 py-1.5 text-right text-foreground">45.6 GiB</td>');
    expect(html).toContain(">27.9 GiB<");
    expect(html).toContain('<td class="num px-2 py-1.5 text-right text-foreground">47.4K</td>');
    expect(html).toContain('<td class="num px-2 py-1.5 text-right text-serious-ink">369</td>');
  });

  it("renders a node's peers by hostname and its cache disks", () => {
    const s = snapshot();
    const html = renderToStaticMarkup(createElement(NodePeerCacheCard, { n: s.nodes[0]!, s }));
    expect(html).toContain(`Serves peers at ${UBU}`);
    expect(html).toContain("3-node mesh");
    expect(html).toContain('<span class="text-foreground">vm1</span>');
    expect(html).toContain('<span class="text-foreground">asahimbp</span>');
    expect(html).toContain("230:0 suspect");
    expect(html).toContain("where parts were found · local hit 80.0%");
  });

  it("renders nothing for a node outside the mesh", () => {
    const s = snapshot();
    expect(renderToStaticMarkup(createElement(NodePeerCacheCard, { n: s.nodes[3]!, s }))).toBe("");
  });
});
