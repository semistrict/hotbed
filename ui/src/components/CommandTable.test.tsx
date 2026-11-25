import { create } from "@bufbuild/protobuf";
import type { ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";
import {
  CommandSchema,
  CommandState,
  FinishedCommandSchema,
  NodeSchema,
  SnapshotSchema,
  WorkspaceSchema,
} from "../gen/hotbed/v1/hotbed_pb";

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, className }: { children: ReactNode; className?: string }) => <a className={className}>{children}</a>,
  useNavigate: () => vi.fn(),
  useSearch: () => ({}),
}));

vi.mock("../lib/project", () => ({
  useProject: () => [null, vi.fn()],
}));

import { commandRuntime, HistoryTable, LiveCommandTable } from "./CommandTable";

describe("CommandTable runtime column", () => {
  it("resolves command runtime directly, falling back to workspace runtime", () => {
    const s = create(SnapshotSchema, {
      workspaces: [
        create(WorkspaceSchema, { id: "ws-vm", runtime: "vm" }),
        create(WorkspaceSchema, { id: "ws-legacy", runtime: "" }),
      ],
    });
    expect(commandRuntime(s, { runtime: "cladefs", workspaceId: "" })).toBe("cladefs");
    expect(commandRuntime(s, { runtime: "", workspaceId: "ws-vm" })).toBe("vm");
    expect(commandRuntime(s, { runtime: "", workspaceId: "ws-legacy" })).toBe("");
    expect(commandRuntime(s, { runtime: "", workspaceId: "ws-gone" })).toBe("");
    expect(commandRuntime(s, { runtime: "", workspaceId: "" })).toBe("");
  });

  it("renders the runtime column in LiveCommandTable for workspace and sessionless freshen commands", () => {
    const s = create(SnapshotSchema, {
      now: 100_000n,
      nodes: [create(NodeSchema, { id: "n1", hostname: "vm1", online: true })],
      workspaces: [create(WorkspaceSchema, { id: "ws-1", runtime: "vm" })],
    });
    const commands = [
      create(CommandSchema, {
        id: "c-freshen",
        nodeId: "n1",
        project: "cladefs",
        kind: "freshen",
        runtime: "vm",
        argv: "freshen cladefs-vm-1234",
        state: CommandState.RUNNING,
        queuedAt: 90_000n,
        startedAt: 91_000n,
      }),
      create(CommandSchema, {
        id: "c-build",
        nodeId: "n1",
        workspaceId: "ws-1",
        sessionId: "warm-fox",
        project: "cladefs",
        repo: "cladefs",
        kind: "build",
        argv: "cargo check",
        state: CommandState.RUNNING,
        queuedAt: 92_000n,
        startedAt: 93_000n,
      }),
      create(CommandSchema, {
        id: "c-fuse",
        nodeId: "n1",
        project: "cladefs",
        kind: "freshen",
        runtime: "cladefs",
        argv: "freshen cladefs-fuse-5678",
        state: CommandState.QUEUED,
        queuedAt: 94_000n,
      }),
    ];

    const html = renderToStaticMarkup(<LiveCommandTable s={s} commands={commands} />);
    expect(html).toContain(">runtime<");
    expect(html).toContain("Firecracker microVM runtime");
    expect(html).toContain("cladefs host-FUSE runtime");
  });

  it("renders the runtime column in HistoryTable using recorded runtime even after workspace release", () => {
    const s = create(SnapshotSchema, {
      now: 200_000n,
      nodes: [create(NodeSchema, { id: "n1", hostname: "ubu1", online: true })],
    });
    const rows = [
      create(FinishedCommandSchema, {
        id: "c-done-vm",
        nodeId: "n1",
        workspaceId: "ws-released",
        sessionId: "fast-owl",
        repoId: "cladefs",
        argv: "cargo test",
        kind: "test",
        runtime: "vm",
        state: CommandState.SUCCEEDED,
        endedAt: 190_000n,
        seconds: 12.3,
      }),
      create(FinishedCommandSchema, {
        id: "c-done-cladefs",
        nodeId: "n1",
        workspaceId: "ws-bench",
        sessionId: "fast-owl",
        repoId: "cladefs",
        argv: "cargo bench",
        kind: "test",
        runtime: "cladefs",
        bench: true,
        state: CommandState.SUCCEEDED,
        endedAt: 185_000n,
        seconds: 40.0,
      }),
    ];

    const html = renderToStaticMarkup(<HistoryTable s={s} rows={rows} />);
    expect(html).toContain(">runtime<");
    expect(html).toContain("Firecracker microVM runtime");
    expect(html).toContain("cladefs host-FUSE runtime");
  });
});
