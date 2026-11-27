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
} from "../api";

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, className }: { children: ReactNode; className?: string }) => <a className={className}>{children}</a>,
  useNavigate: () => vi.fn(),
  useSearch: () => ({}),
}));

vi.mock("../lib/project", () => ({
  useProject: () => [null, vi.fn()],
}));

import {
  commandRuntime,
  finishedCommandSearchValues,
  HistoryTable,
  liveCommandSearchValues,
  LiveCommandTable,
} from "./CommandTable";
import { filterTableRows } from "./ui/table";

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

  it("supports custom empty text, hideColumns, units header, workspace mobile cell, and responsive breakpoints", () => {
    const s = create(SnapshotSchema, {
      now: 200_000n,
      nodes: [create(NodeSchema, { id: "n1", hostname: "ubu1", online: true })],
      workspaces: [create(WorkspaceSchema, { id: "ws-1", runtime: "vm" })],
    });

    expect(renderToStaticMarkup(<HistoryTable s={s} rows={[]} />)).toContain("Nothing matches.");
    expect(renderToStaticMarkup(<HistoryTable s={s} rows={[]} empty="No finished commands in this workspace yet." />)).toContain(
      "No finished commands in this workspace yet.",
    );

    const liveCmds = [
      create(CommandSchema, {
        id: "c-live",
        nodeId: "n1",
        workspaceId: "ws-1",
        sessionId: "warm-fox",
        project: "hotbed",
        repo: "hotbed",
        kind: "test",
        argv: "go test ./...",
        state: CommandState.RUNNING,
        queuedAt: 190_000n,
        startedAt: 191_000n,
      }),
    ];
    const liveHtml = renderToStaticMarkup(
      <LiveCommandTable s={s} commands={liveCmds} hideColumns={["workspace", "node", "project"]} />,
    );
    expect(liveHtml).toContain("lg:hidden");
    expect(liveHtml).toContain("hidden lg:block");
    expect(liveHtml).toContain(">units<");
    expect(liveHtml).not.toContain(">crates<");
    expect(liveHtml).not.toContain(">workspace<");
    expect(liveHtml).not.toContain(">node<");
    expect(liveHtml).not.toContain(">project<");
    expect(liveHtml).toContain("px-2 py-0.5");

    const doneRows = [
      create(FinishedCommandSchema, {
        id: "c-done",
        nodeId: "n1",
        workspaceId: "ws-1",
        sessionId: "warm-fox",
        repoId: "hotbed",
        argv: "go test -race ./...",
        kind: "test",
        failure: "test",
        state: CommandState.FAILED,
        endedAt: 195_000n,
        seconds: 4.2,
      }),
    ];
    const fullHistoryHtml = renderToStaticMarkup(<HistoryTable s={s} rows={doneRows} />);
    expect(fullHistoryHtml).toContain("xl:hidden");
    expect(fullHistoryHtml).toContain("hidden xl:block");
    expect(fullHistoryHtml).toContain(">units<");
    expect(fullHistoryHtml).not.toContain(">crates<");
    expect(fullHistoryHtml).toContain("whitespace-normal");
    // WorkspaceCell appears in both mobile card and desktop table when not hidden.
    expect(fullHistoryHtml.split("ws-1").length - 1).toBeGreaterThanOrEqual(2);

    const hiddenHistoryHtml = renderToStaticMarkup(
      <HistoryTable s={s} rows={doneRows} hideColumns={["workspace", "node", "project"]} />,
    );
    expect(hiddenHistoryHtml).not.toContain(">workspace<");
    expect(hiddenHistoryHtml).not.toContain(">node<");
    expect(hiddenHistoryHtml).not.toContain(">project<");
  });

  it("shows command IDs and filter bar in LiveCommandTable and HistoryTable, and filters across any column", () => {
    const s = create(SnapshotSchema, {
      now: 200_000n,
      nodes: [
        create(NodeSchema, { id: "n1", hostname: "ubu1", online: true }),
        create(NodeSchema, { id: "n2", hostname: "asahimbp", online: true }),
      ],
      workspaces: [create(WorkspaceSchema, { id: "ws-alpha", runtime: "vm" })],
    });
    const live = [
      create(CommandSchema, {
        id: "cmd-live-1",
        nodeId: "n1",
        workspaceId: "ws-alpha",
        sessionId: "swift-fox",
        project: "hotbed",
        repo: "hotbed",
        kind: "test",
        argv: "go test -race ./...",
        state: CommandState.QUEUED,
        queuedAt: 190_000n,
        reason: "waiting: CPU pressure 75%",
      }),
      create(CommandSchema, {
        id: "cmd-live-2",
        nodeId: "n2",
        workspaceId: "ws-beta",
        sessionId: "calm-owl",
        project: "cladefs",
        repo: "cladefs",
        kind: "build",
        runtime: "cladefs",
        argv: "cargo build --release",
        state: CommandState.RUNNING,
        queuedAt: 191_000n,
        startedAt: 192_000n,
      }),
    ];
    const liveHtml = renderToStaticMarkup(<LiveCommandTable s={s} commands={live} />);
    expect(liveHtml).toContain(">id<");
    expect(liveHtml).toContain("cmd-live-1");
    expect(liveHtml).toContain("cmd-live-2");
    expect(liveHtml).toContain("Filter commands…");

    const wsMap = new Map(s.workspaces.map((w) => [w.id, w]));
    const filterLive = (q: string) =>
      filterTableRows(live, q, (c) => liveCommandSearchValues(s, c, wsMap)).map((c) => c.id);
    expect(filterLive("cmd-live-1")).toEqual(["cmd-live-1"]);
    expect(filterLive("asahimbp")).toEqual(["cmd-live-2"]);
    expect(filterLive("swift-fox")).toEqual(["cmd-live-1"]);
    expect(filterLive("CPU pressure")).toEqual(["cmd-live-1"]);
    expect(filterLive("cargo build")).toEqual(["cmd-live-2"]);
    expect(filterLive("firecracker")).toEqual(["cmd-live-1"]);
    expect(filterLive("queued")).toEqual(["cmd-live-1"]);

    const history = [
      create(FinishedCommandSchema, {
        id: "cmd-hist-1",
        nodeId: "n1",
        workspaceId: "ws-alpha",
        sessionId: "swift-fox",
        repoId: "hotbed",
        argv: "go test ./...",
        kind: "test",
        runtime: "vm",
        state: CommandState.CANCELLED,
        reason: "nothing can queue for more than 15m",
        endedAt: 195_000n,
        seconds: 0,
      }),
      create(FinishedCommandSchema, {
        id: "cmd-hist-2",
        nodeId: "n2",
        workspaceId: "ws-beta",
        sessionId: "calm-owl",
        repoId: "cladefs",
        argv: "cargo test",
        kind: "test",
        runtime: "cladefs",
        state: CommandState.FAILED,
        failure: "oom",
        endedAt: 196_000n,
        seconds: 12,
      }),
    ];
    const histHtml = renderToStaticMarkup(<HistoryTable s={s} rows={history} />);
    expect(histHtml).toContain(">id<");
    expect(histHtml).toContain("cmd-hist-1");
    expect(histHtml).toContain("nothing can queue for more than 15m");
    expect(histHtml).toContain("Filter commands…");

    const filterHist = (q: string) =>
      filterTableRows(history, q, (c) => finishedCommandSearchValues(s, c, wsMap)).map((c) => c.id);
    expect(filterHist("cmd-hist-1")).toEqual(["cmd-hist-1"]);
    expect(filterHist("more than 15m")).toEqual(["cmd-hist-1"]);
    expect(filterHist("OOM killed")).toEqual(["cmd-hist-2"]);
    expect(filterHist("ws-beta")).toEqual(["cmd-hist-2"]);
    expect(filterHist("ubu1")).toEqual(["cmd-hist-1"]);
  });
});
