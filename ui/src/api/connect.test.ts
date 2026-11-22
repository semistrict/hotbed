import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";
import { NodeSchema, SnapshotSchema } from "../gen/hotbed/v1/hotbed_pb";
import { toSnapshot } from "./connect";

describe("toSnapshot", () => {
  it("keeps each node's own storage, not one assumed for all", () => {
    const s = toSnapshot(
      create(SnapshotSchema, {
        nodes: [
          create(NodeSchema, { id: "a", hostname: "asahimbp", storage: "btrfs" }),
          create(NodeSchema, { id: "u", hostname: "ubu1", storage: "zfs" }),
        ],
      }),
    );
    expect(s.nodes.map((n) => [n.hostname, n.storage])).toEqual([
      ["asahimbp", "btrfs"],
      ["ubu1", "zfs"],
    ]);
  });
});
