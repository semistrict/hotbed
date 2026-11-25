import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, to }: { children: unknown; to: string }) => <a href={to}>{children as never}</a>,
}));

let mockRole = "member";
let mockJoinTokensState: { data?: { tokens: unknown[] }; isLoading: boolean; error: Error | null } = {
  data: { tokens: [] },
  isLoading: false,
  error: null,
};
let mockApiTokensState: { data?: { tokens: unknown[] }; isLoading: boolean; error: Error | null } = {
  data: { tokens: [] },
  isLoading: false,
  error: null,
};
let mockSwitchOrgError: Error | null = null;
let mockCreateJoinError: Error | null = null;
let mockRevokeJoinError: Error | null = null;
let mockCreateApiError: Error | null = null;
let mockRevokeApiError: Error | null = null;

vi.mock("../lib/auth", () => ({
  useAuthMe: () => ({
    data: {
      authenticated: true,
      authMethod: "workos-cookie",
      workosEnabled: true,
      user: { id: "user_bob", email: "bob@acme.test", firstName: "Bob", lastName: "M", name: "Bob M" },
      organization: { id: "org_acme", name: "Acme", role: mockRole },
      organizations: [{ id: "org_acme", name: "Acme", role: mockRole }],
    },
  }),
  useOrgMembers: () => ({
    data: {
      organization: { id: "org_acme", name: "Acme", role: mockRole },
      members: [
        {
          membershipId: "om_alice",
          userId: "user_alice",
          email: "alice@acme.test",
          firstName: "Alice",
          lastName: "A",
          name: "Alice A",
          role: "admin",
          status: "active",
          createdAt: "2026-01-01T00:00:00Z",
        },
        {
          membershipId: "om_bob",
          userId: "user_bob",
          email: "bob@acme.test",
          firstName: "Bob",
          lastName: "M",
          name: "Bob M",
          role: mockRole,
          status: "active",
          createdAt: "2026-01-02T00:00:00Z",
        },
      ],
      invitations: [
        {
          id: "inv_1",
          email: "cfo@acme.test",
          state: "pending",
          role: "admin",
          expiresAt: "2026-12-01T00:00:00Z",
          createdAt: "2026-11-01T00:00:00Z",
        },
      ],
    },
    isLoading: false,
    error: null,
  }),
  useJoinTokens: () => mockJoinTokensState,
  useApiTokens: () => mockApiTokensState,
  useSwitchOrganization: () => ({ mutate: vi.fn(), isPending: false, error: mockSwitchOrgError }),
  useCreateOrganization: () => ({ mutateAsync: vi.fn(), isPending: false, error: null }),
  useInviteMember: () => ({ mutateAsync: vi.fn(), isPending: false, error: null }),
  useRevokeInvitation: () => ({ mutate: vi.fn(), isPending: false, error: null }),
  useRemoveMember: () => ({ mutate: vi.fn(), isPending: false, error: null }),
  useCreateJoinToken: () => ({ mutateAsync: vi.fn(), isPending: false, error: mockCreateJoinError }),
  useRevokeJoinToken: () => ({ mutate: vi.fn(), isPending: false, error: mockRevokeJoinError }),
  useCreateApiToken: () => ({ mutateAsync: vi.fn(), isPending: false, error: mockCreateApiError }),
  useRevokeApiToken: () => ({ mutate: vi.fn(), isPending: false, error: mockRevokeApiError }),
}));

vi.mock("../lib/data", () => ({
  useSnapshot: () => ({ data: { clusterId: "cluster_acme", projects: [] } }),
}));

import { OrganizationPage } from "./Organization";

function renderOrgPage() {
  const qc = new QueryClient();
  return renderToStaticMarkup(
    <QueryClientProvider client={qc}>
      <OrganizationPage />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  mockRole = "member";
  mockJoinTokensState = { data: { tokens: [] }, isLoading: false, error: null };
  mockApiTokensState = { data: { tokens: [] }, isLoading: false, error: null };
  mockSwitchOrgError = null;
  mockCreateJoinError = null;
  mockRevokeJoinError = null;
  mockCreateApiError = null;
  mockRevokeApiError = null;
});

describe("OrganizationPage RBAC gating", () => {
  it("hides invite form, remove-member, and revoke-invitation controls for non-admin members", () => {
    mockRole = "member";
    const html = renderOrgPage();

    expect(html.includes("teammate@company.com")).toBe(false);
    expect(html.includes(">Remove<")).toBe(false);
    expect(html.includes(">Revoke<")).toBe(false);
  });

  it("shows invite form, remove-member, and revoke-invitation controls for org admins", () => {
    mockRole = "admin";
    const html = renderOrgPage();

    expect(html.includes("teammate@company.com")).toBe(true);
    expect(html.includes(">Remove<")).toBe(true);
    expect(html.includes(">Revoke<")).toBe(true);
  });
});

describe("OrganizationPage loading and error states", () => {
  it("renders loading states for join tokens and API tokens", () => {
    mockJoinTokensState = { data: undefined, isLoading: true, error: null };
    mockApiTokensState = { data: undefined, isLoading: true, error: null };
    const html = renderOrgPage();

    expect(html).toContain("loading join tokens…");
    expect(html).toContain("loading API tokens…");
  });

  it("renders query and mutation error alerts for org switch and tokens", () => {
    mockJoinTokensState = { data: undefined, isLoading: false, error: new Error("join list failed") };
    mockApiTokensState = { data: undefined, isLoading: false, error: new Error("api list failed") };
    mockSwitchOrgError = new Error("switch failed");
    mockCreateJoinError = new Error("mint join failed");
    mockCreateApiError = new Error("mint api failed");
    const html = renderOrgPage();

    expect(html).toContain("join list failed");
    expect(html).toContain("api list failed");
    expect(html).toContain("switch failed");
    expect(html).toContain("mint join failed");
    expect(html).toContain("mint api failed");
  });

  it("renders revoke mutation errors when token revocations fail", () => {
    mockRevokeJoinError = new Error("revoke join failed");
    mockRevokeApiError = new Error("revoke api failed");
    const html = renderOrgPage();

    expect(html).toContain("revoke join failed");
    expect(html).toContain("revoke api failed");
  });
});
