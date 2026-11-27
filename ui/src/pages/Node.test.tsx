import { create } from "@bufbuild/protobuf";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { NodeSchema, ResourcesSchema, SnapshotSchema } from "../api";

let mockHostname = "old-box";
let mockRole = "member";
let mockNodes = [
  create(NodeSchema, {
    id: "n-offline",
    hostname: "old-box",
    online: false,
    resources: create(ResourcesSchema, { arch: "arm64", cpus: 8, memoryBytes: 16n * 1024n ** 3n }),
  }),
  create(NodeSchema, {
    id: "n-online",
    hostname: "ubu1",
    online: true,
    resources: create(ResourcesSchema, { arch: "amd64", cpus: 16, memoryBytes: 32n * 1024n ** 3n }),
  }),
];

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, className }: { children?: ReactNode; className?: string }) => <a className={className}>{children}</a>,
  useParams: () => ({ hostname: mockHostname }),
  useNavigate: () => vi.fn(),
}));

vi.mock("../lib/auth", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/auth")>();
  return {
    ...actual,
    useAuthMe: () => ({
      data: {
        authenticated: true,
        authMethod: "workos-cookie",
        workosEnabled: true,
        user: { id: "u1", email: "user@acme.test", firstName: "U", lastName: "S", name: "U S" },
        organization: { id: "org_1", name: "Acme", role: mockRole },
        organizations: [{ id: "org_1", name: "Acme", role: mockRole }],
      },
    }),
  };
});

vi.mock("../lib/data", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/data")>();
  return {
    ...actual,
    useSnapshot: () => ({
      data: create(SnapshotSchema, { now: 100_000n, nodes: mockNodes }),
    }),
  };
});

vi.mock("../components/ContinuousProfilePanel", () => ({ ContinuousProfilePanel: () => null }));
vi.mock("../components/DaemonLogsPanel", () => ({ DaemonLogsPanel: () => null }));

import { NodePage } from "./Node";

function renderNodePage() {
  const qc = new QueryClient();
  return renderToStaticMarkup(
    <QueryClientProvider client={qc}>
      <NodePage />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  mockHostname = "old-box";
  mockRole = "member";
});

describe("NodePage delete button", () => {
  it("shows an enabled Delete button for an offline node", () => {
    mockHostname = "old-box";
    const html = renderNodePage();
    const btn = html.match(/<button\b[^>]*aria-label="Delete old-box"[^>]*>/)?.[0] ?? "";
    expect(btn).toContain('aria-label="Delete old-box"');
    expect(btn).not.toMatch(/\sdisabled(?:=""|\s|>)/);
  });

  it("shows a disabled Delete button with guidance for an online node", () => {
    mockHostname = "ubu1";
    const html = renderNodePage();
    const btn = html.match(/<button\b[^>]*aria-label="Delete ubu1"[^>]*>/)?.[0] ?? "";
    expect(btn).toContain('aria-label="Delete ubu1"');
    expect(btn).toMatch(/\sdisabled(?:=""|\s|>)/);
    expect(html).toContain("Stop the worker on this node first (hotbed worker stop) before deleting it.");
  });

  it("hides the Delete button for read-only viewers", () => {
    mockHostname = "old-box";
    mockRole = "viewer";
    const html = renderNodePage();
    expect(html).not.toContain('aria-label="Delete old-box"');
  });
});
