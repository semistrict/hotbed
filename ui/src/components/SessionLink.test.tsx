import { create } from "@bufbuild/protobuf";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";
import { GetSessionResponseSchema, SessionSchema, SnapshotSchema, type Session } from "../api";
import { SessionLink, SessionPreview } from "./SessionLink";

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, className, params }: { children?: ReactNode; className?: string; params?: { id: string } }) => (
    <a href={`/sessions/${params?.id}`} className={className}>
      {children}
    </a>
  ),
  useNavigate: () => vi.fn(),
}));

const NOW = 10 * 3600_000;

/** Renders with the snapshot listing `listed` and GetSession knowing `older`. */
function render(node: ReactNode, { listed = [], older = [] }: { listed?: Session[]; older?: Session[] } = {}) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(["snapshot"], create(SnapshotSchema, { now: BigInt(NOW), sessions: listed }));
  for (const session of older) qc.setQueryData(["session", session.id], create(GetSessionResponseSchema, { session, now: BigInt(NOW) }));
  return renderToStaticMarkup(<QueryClientProvider client={qc}>{node}</QueryClientProvider>);
}

describe("SessionLink", () => {
  it("is a badge opening the session's page, the card left for hovering", () => {
    expect(render(<SessionLink id="brave-otter" className="shrink-0" />)).toBe(
      '<a href="/sessions/brave-otter" class="inline-flex items-center rounded border border-border/70 px-1 font-mono text-[11px] leading-[1.45] text-muted-foreground hover:border-primary/50 hover:text-primary shrink-0">brave-otter</a>',
    );
  });

  it("shows a dash for no session, or what the caller asks", () => {
    expect(render(<SessionLink id="" />)).toBe('<span class="font-mono text-[11px] text-faint">—</span>');
    expect(render(<SessionLink id="" empty={null} />)).toBe("");
  });
});

describe("SessionPreview", () => {
  it("shows a session the snapshot no longer lists, from GetSession, with a button opening its T3 Code thread", () => {
    const old = create(SessionSchema, {
      id: "old-otter",
      agent: "Mac",
      model: "claude-opus-5-5",
      project: "hotbed",
      startedAt: BigInt(NOW - 3 * 3600_000),
      lastActiveAt: BigInt(NOW - 2 * 3600_000),
      endedAt: BigInt(NOW - 3600_000),
      t3codeThreadId: "2e8f673f-81cd-4add-beed-80297a3c30c2",
      t3codeThreadUrl: "https://app.t3.codes/threads/env-1/2e8f673f-81cd-4add-beed-80297a3c30c2",
    });
    const html = render(<SessionPreview id="old-otter" />, { older: [old] });
    expect(html).toContain('<a href="/sessions/old-otter" class="font-mono text-[13px] font-medium text-foreground hover:text-primary">old-otter</a>');
    expect(html).toContain('<span class="font-mono text-[11px] whitespace-nowrap text-faint">ended</span>');
    expect(html).toContain('<span class="font-mono text-foreground/85">claude-opus-5-5</span>');
    expect(html).toContain("<span>started 3h ago</span><span>ended 1h ago</span>");
    expect(html).toContain(
      '<a href="https://app.t3.codes/threads/env-1/2e8f673f-81cd-4add-beed-80297a3c30c2" target="_blank" rel="noreferrer" title="Open T3 Code thread 2e8f673f-81cd-4add-beed-80297a3c30c2"',
    );
    expect(html).toContain('<span class="min-w-0 flex-1 truncate">Open in T3 Code</span>');
  });

  it("names a T3 Code thread it has no link for without linking it", () => {
    const live = create(SessionSchema, {
      id: "brave-otter",
      agent: "ubu1",
      startedAt: BigInt(NOW - 60_000),
      lastActiveAt: BigInt(NOW - 30_000),
      t3codeThreadId: "2e8f673f-81cd-4add-beed-80297a3c30c2",
    });
    const html = render(<SessionPreview id="brave-otter" />, { listed: [live] });
    expect(html).toContain('<span class="font-mono text-[11px] whitespace-nowrap text-sprout">○ idle</span>');
    expect(html).toContain('<div class="flex flex-wrap items-center gap-x-2 gap-y-1 text-[11px] text-faint"><span>started 1m ago</span><span>active 30s ago</span></div>');
    expect(html).toContain('<div class="flex items-center gap-2 rounded-md border border-border px-2 py-1.5 text-[12px] text-muted-foreground" title="T3 Code thread 2e8f673f-81cd-4add-beed-80297a3c30c2">');
    expect(html).toContain('<span class="min-w-0 flex-1 truncate">T3 Code thread 7a3c30c2</span>');
    expect(html).not.toContain("target=\"_blank\"");
  });
});
