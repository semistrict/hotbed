import { create } from "@bufbuild/protobuf";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { FoldedStackSchema, ProfileFrameSchema } from "../gen/hotbed/v1/hotbed_pb";
import { buildTree, findNodeByPath, FlameGraph } from "./FlameGraph";

describe("FlameGraph findNodeByPath", () => {
  const frames = [
    create(ProfileFrameSchema, { name: "app", frameType: "process" }),
    create(ProfileFrameSchema, { name: "github.com/org/pkg.Run", frameType: "go" }),
    create(ProfileFrameSchema, { name: "vfs_write", frameType: "kernel" }),
  ];

  it("resolves updated node counts when stacks refresh without holding stale references", () => {
    const tree1 = buildTree(
      [create(FoldedStackSchema, { frames: [0, 1, 2], samples: 10n })],
      frames,
    );
    const zoomPath = ["app", "github.com/org/pkg.Run"];
    const node1 = findNodeByPath(tree1, zoomPath);
    expect(node1?.total).toBe(10);

    // Live profile refreshes with higher sample counts.
    const tree2 = buildTree(
      [
        create(FoldedStackSchema, { frames: [0, 1, 2], samples: 45n }),
        create(FoldedStackSchema, { frames: [0, 1], samples: 5n }),
      ],
      frames,
    );
    const node2 = findNodeByPath(tree2, zoomPath);
    expect(node2).not.toBe(node1);
    expect(node2?.total).toBe(50);
    expect(node2?.self).toBe(5);
  });

  it("returns null cleanly when the zoomed path disappears from updated stacks", () => {
    const zoomPath = ["app", "github.com/org/pkg.Run", "vfs_write_[k]"];
    const tree1 = buildTree(
      [create(FoldedStackSchema, { frames: [0, 1, 2], samples: 10n })],
      frames,
    );
    expect(findNodeByPath(tree1, zoomPath)?.total).toBe(10);

    // Subsequent window has no vfs_write samples.
    const tree2 = buildTree(
      [create(FoldedStackSchema, { frames: [0, 1], samples: 20n })],
      frames,
    );
    expect(findNodeByPath(tree2, zoomPath)).toBeNull();
    expect(findNodeByPath(tree2, null)).toBeNull();
    expect(findNodeByPath(tree2, [])).toBeNull();
  });

  it("renders search highlights with dark ink and caps vertical scroll height", () => {
    const html = renderToStaticMarkup(
      createElement(FlameGraph, {
        stacks: [create(FoldedStackSchema, { frames: [0, 1, 2], samples: 10n })],
        frames,
        highlight: "vfs_write",
      }),
    );
    expect(html).toContain("text-[#1c1713]");
    expect(html).not.toContain("text-background");
    expect(html).toContain("max-h-[480px]");
  });
});
