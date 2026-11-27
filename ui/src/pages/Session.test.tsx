import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";
import {
  CommandSchema,
  CommandState,
  GetSessionResponseSchema,
  ListCommandsResponseSchema,
  SessionSchema,
  SnapshotSchema,
} from "../api";
import { SessionPage } from "./Session";

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, className }: { children?: ReactNode; className?: string }) => (
    <a className={className}>{children}</a>
  ),
  useParams: () => ({ id: "old-otter" }),
  useNavigate: () => vi.fn(),
}));

const NOW = 5 * 24 * 3600_000;
const HOUR = 3600_000;

// Ended three days ago: out of the snapshot, which lists the last day's.
const old = create(SessionSchema, {
  id: "old-otter",
  agent: "Mac",
  project: "hotbed",
  startedAt: BigInt(NOW - 74 * HOUR),
  lastActiveAt: BigInt(NOW - 72 * HOUR),
  endedAt: BigInt(NOW - 72 * HOUR),
  endReason: "ended by its agent",
});

function render(seed: (qc: QueryClient) => void) {
  // retryOnMount off: a seeded failure is a call that already failed, not one to ask again.
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false, retryOnMount: false } },
  });
  qc.setQueryData(["snapshot"], create(SnapshotSchema, { now: BigInt(NOW) }));
  seed(qc);
  return renderToStaticMarkup(
    <QueryClientProvider client={qc}>{<SessionPage />}</QueryClientProvider>,
  );
}

describe("SessionPage", () => {
  it("shows a session ended days ago, with its commands from history", () => {
    const ran = (id: string, argv: string, state: CommandState, at: number) =>
      create(CommandSchema, {
        id,
        argv,
        state,
        sessionId: "old-otter",
        queuedAt: BigInt(at),
        startedAt: BigInt(at),
        endedAt: BigInt(at + 60_000),
      });
    const html = render((qc) => {
      qc.setQueryData(
        ["session", "old-otter"],
        create(GetSessionResponseSchema, { session: old, now: BigInt(NOW) }),
      );
      qc.setQueryData(
        ["session-commands", "old-otter"],
        create(ListCommandsResponseSchema, {
          commands: [
            ran("c1", "go build ./...", CommandState.SUCCEEDED, NOW - 74 * HOUR),
            ran("c2", "go test ./...", CommandState.FAILED, NOW - 73 * HOUR),
          ],
        }),
      );
    });
    expect(html).toContain("ended 3d ago: ended by its agent");
    expect(html).toContain(
      "Every command the session ran, newest first, as history keeps them (30 days)",
    );
    expect(html).toContain("go build ./...");
    expect(html).toContain("go test ./...");
    expect(html).toContain(">id<");
    expect(html).toContain("c1");
    expect(html).toContain("c2");
    expect(html).toContain("Filter commands…");
    expect(html).not.toContain("Nothing run yet.");
  });

  it("says it cannot load a session rather than that none has the name", () => {
    const html = render((qc) => {
      const q = qc.getQueryCache().build(qc, { queryKey: ["session", "old-otter"] });
      q.setState({
        ...q.state,
        status: "error",
        error: new ConnectError("controller restarting", Code.Unavailable),
        errorUpdatedAt: NOW,
        fetchStatus: "idle",
      });
    });
    expect(html).toBe(
      '<p class="rise rounded-xl border border-border bg-card/70 px-3.5 py-2.5 font-mono text-[12px] text-critical">Cannot load session old-otter: ConnectError: [unavailable] controller restarting</p>',
    );
  });
});
