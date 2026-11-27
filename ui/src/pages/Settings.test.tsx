import { create } from "@bufbuild/protobuf";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  GetWorkerConfigResponseSchema,
  GitCredentialSchema,
  NodeSchema,
  OrgControllerConfigSchema,
  ResourcesSchema,
  WorkerConfigSchema,
  WorkerFreshenConfigSchema,
  WorkerVMConfigSchema,
} from "../api";

let mockRole = "member";
let mockSearch: { scope?: string; tab?: string } = {};

vi.mock("@tanstack/react-router", () => ({
  useSearch: () => mockSearch,
  useNavigate: () => vi.fn(),
  Link: ({ children }: { children: unknown }) => children,
}));

vi.mock("../lib/auth", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../lib/auth")>()),
  useAuthMe: () => ({
    data: {
      authenticated: true,
      authMethod: "workos-cookie",
      workosEnabled: true,
      user: { id: "user_1", email: "user@acme.test", firstName: "Pat", lastName: "S", name: "Pat S" },
      organization: { id: "org_acme", name: "Acme", role: mockRole },
      organizations: [{ id: "org_acme", name: "Acme", role: mockRole }],
    },
  }),
}));

vi.mock("../lib/data", () => ({
  useSnapshot: () => ({
    data: {
      clusterId: "cluster_acme",
      now: 1795627000000n,
      nodes: [
        create(NodeSchema, {
          id: "n1",
          hostname: "ubu1",
          online: true,
          resources: create(ResourcesSchema, { arch: "amd64", cpus: 16 }),
        }),
        create(NodeSchema, {
          id: "n2",
          hostname: "asahimbp",
          online: true,
          resources: create(ResourcesSchema, { arch: "arm64", cpus: 11 }),
        }),
      ],
    },
  }),
}));

const mockWorkerConfigResp = create(GetWorkerConfigResponseSchema, {
  defaults: create(WorkerConfigSchema, {
    node: "",
    revision: 3n,
    updatedBy: "alice",
    freshen: create(WorkerFreshenConfigSchema, {
      maxDutyCycle: 0.05,
      minIntervalSeconds: 1800,
    }),
    controller: create(OrgControllerConfigSchema, {
      sessionIdleTimeoutSeconds: 3600,
    }),
    vm: create(WorkerVMConfigSchema, {
      enabled: true,
      vcpus: 8,
      memMib: 8192,
      tenantId: 7n,
    }),
  }),
  nodes: [
    create(WorkerConfigSchema, {
      node: "ubu1",
      revision: 2n,
      vm: create(WorkerVMConfigSchema, { memMib: 12288 }),
    }),
  ],
  effective: create(WorkerConfigSchema, {
    node: "ubu1",
    revision: 5n,
    vm: create(WorkerVMConfigSchema, { enabled: true, vcpus: 8, memMib: 12288, tenantId: 7n }),
  }),
});

vi.mock("../lib/settings", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/settings")>();
  return {
    ...actual,
    useWorkerConfig: () => ({ data: mockWorkerConfigResp, isLoading: false, error: null, refetch: vi.fn() }),
    useSetWorkerConfig: () => ({ mutateAsync: vi.fn(), isPending: false, error: null }),
    useDeleteWorkerConfig: () => ({ mutateAsync: vi.fn(), isPending: false, error: null }),
    useUpgradeSystem: () => ({ mutateAsync: vi.fn(), isPending: false, error: null }),
    useGitCredentials: () => ({
      data: [
        create(GitCredentialSchema, {
          name: "github",
          host: "github.com",
          username: "acme-bot",
          repos: ["acme/*"],
          refreshable: true,
        }),
      ],
      isLoading: false,
      error: null,
    }),
    usePutGitCredential: () => ({ mutateAsync: vi.fn(), isPending: false, error: null }),
    useDeleteGitCredential: () => ({ mutate: vi.fn(), isPending: false, error: null }),
    useStartGitHubLogin: () => ({ mutateAsync: vi.fn(), isPending: false, error: null }),
    useCompleteGitHubLogin: () => ({ mutateAsync: vi.fn(), isPending: false, error: null }),
  };
});

import { SettingsPage } from "./Settings";

function renderSettings() {
  const qc = new QueryClient();
  return renderToStaticMarkup(
    <QueryClientProvider client={qc}>
      <SettingsPage />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  mockRole = "member";
  mockSearch = {};
});

describe("SettingsPage per-org scoping and RBAC", () => {
  it("renders per-org header, tenant badge, and read-only banner for non-admin members", () => {
    mockRole = "member";
    const html = renderSettings();

    expect(html).toContain("Acme Settings");
    expect(html).toContain("org_acme");
    expect(html).toContain("cladefs tenant 7");
    expect(html).toContain("Read-only organization access");
    expect(html).not.toContain("Save changes");
    expect(html).not.toContain("Reset org defaults");
  });

  it("shows editable controls, presets, and node scopes for org admins", () => {
    mockRole = "admin";
    const html = renderSettings();

    expect(html).toContain("Save changes");
    expect(html).toContain("Reset org defaults");
    expect(html).toContain("Balanced (5% / 30m)");
    expect(html).toContain("ubu1");
    expect(html).toContain("asahimbp");
    expect(html).toContain("override");
    expect(html).not.toContain("Read-only organization access");
  });

  it("renders node-override scope with controller-policy pointer back to Org defaults", () => {
    mockRole = "admin";
    mockSearch = { scope: "ubu1", tab: "freshen" };
    const html = renderSettings();

    expect(html).toContain("Editing node override: ubu1");
    expect(html).toContain("Remove ubu1 override");
    expect(html).toContain("Edit in Org defaults");
  });

  it("gates Git Auth mutations by org admin role while listing credentials for all members", () => {
    mockSearch = { tab: "git" };
    mockRole = "member";
    const memberHtml = renderSettings();
    expect(memberHtml).toContain("acme-bot");
    expect(memberHtml).toContain("acme/*");
    expect(memberHtml).not.toContain("Start GitHub login");
    expect(memberHtml).not.toContain("Save credential");

    mockRole = "admin";
    const adminHtml = renderSettings();
    expect(adminHtml).toContain("Start GitHub login");
    expect(adminHtml).toContain("Save credential");
  });

  it("hides the node scope picker on the org-wide Git Auth tab", () => {
    mockRole = "admin";
    mockSearch = { tab: "git" };
    expect(renderSettings()).not.toContain("Configuration Scope");
    mockSearch = { tab: "freshen" };
    expect(renderSettings()).toContain("Configuration Scope");
  });

  it("renders a node override's VM fields from their specs, hinting at the org defaults it inherits", () => {
    mockRole = "admin";
    mockSearch = { scope: "ubu1", tab: "vm" };
    const html = renderSettings();
    expect(html).toContain('id="vm-mem"');
    expect(html).toContain('value="12288"');
    expect(html).toContain("12.0 GiB");
    expect(html).toContain("Org default: 8");
    expect(html).toContain("Managed Guest Kernels (vmlinux)");
    expect(html).toContain("managed tenant_id = 7");
  });
});
