import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";

let mockRole = "member";

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
  useJoinTokens: () => ({ data: { tokens: [] } }),
  useApiTokens: () => ({ data: { tokens: [] } }),
  useSwitchOrganization: () => ({ mutate: vi.fn(), isPending: false }),
  useCreateOrganization: () => ({ mutateAsync: vi.fn(), isPending: false, error: null }),
  useInviteMember: () => ({ mutateAsync: vi.fn(), isPending: false, error: null }),
  useRevokeInvitation: () => ({ mutate: vi.fn(), isPending: false, error: null }),
  useRemoveMember: () => ({ mutate: vi.fn(), isPending: false, error: null }),
  useCreateJoinToken: () => ({ mutateAsync: vi.fn(), isPending: false }),
  useRevokeJoinToken: () => ({ mutate: vi.fn(), isPending: false }),
  useCreateApiToken: () => ({ mutateAsync: vi.fn(), isPending: false }),
  useRevokeApiToken: () => ({ mutate: vi.fn(), isPending: false }),
}));

vi.mock("../lib/data", () => ({
  useSnapshot: () => ({ data: { clusterId: "cluster_acme" } }),
}));

import { OrganizationPage } from "./Organization";

describe("OrganizationPage RBAC gating", () => {
  it("hides invite form, remove-member, and revoke-invitation controls for non-admin members", () => {
    mockRole = "member";
    const qc = new QueryClient();
    const html = renderToStaticMarkup(
      <QueryClientProvider client={qc}>
        <OrganizationPage />
      </QueryClientProvider>,
    );

    expect(html.includes("teammate@company.com")).toBe(false);
    expect(html.includes(">Remove<")).toBe(false);
    expect(html.includes(">Revoke<")).toBe(false);
  });

  it("shows invite form, remove-member, and revoke-invitation controls for org admins", () => {
    mockRole = "admin";
    const qc = new QueryClient();
    const html = renderToStaticMarkup(
      <QueryClientProvider client={qc}>
        <OrganizationPage />
      </QueryClientProvider>,
    );

    expect(html.includes("teammate@company.com")).toBe(true);
    expect(html.includes(">Remove<")).toBe(true);
    expect(html.includes(">Revoke<")).toBe(true);
  });
});
