import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { fetchJson } from "./http";

export interface AuthUser {
  id: string;
  email: string;
  firstName: string;
  lastName: string;
  name: string;
}

export interface AuthOrganization {
  id: string;
  name: string;
  role: string;
  membershipId?: string;
}

export interface AuthMe {
  authenticated: boolean;
  authMethod: string;
  workosEnabled: boolean;
  user: AuthUser;
  organization: AuthOrganization | null;
  organizations: AuthOrganization[];
}

export interface OrgMember {
  membershipId: string;
  userId: string;
  email: string;
  firstName: string;
  lastName: string;
  name: string;
  role: string;
  status: string;
  createdAt: string;
}

export interface OrgInvitation {
  id: string;
  email: string;
  state: string;
  role: string;
  expiresAt: string;
  createdAt: string;
  acceptInvitationUrl?: string;
}

export interface OrgMembersResponse {
  organization: AuthOrganization;
  members: OrgMember[];
  invitations: OrgInvitation[];
}

export interface JoinTokenMeta {
  id: string;
  label: string;
  createdBy: string;
  createdAt: number;
  expiresAt: number | null;
  revokedAt: number | null;
}

export interface MintedJoinToken extends JoinTokenMeta {
  token: string;
  orgId?: string;
}

export interface ApiTokenMeta {
  id: string;
  name: string;
  userId: string;
  email: string;
  orgRole: string;
  createdAt: number;
  lastUsedAt: number | null;
  expiresAt: number | null;
  revokedAt: number | null;
}

export interface MintedApiToken extends ApiTokenMeta {
  token: string;
  orgId?: string;
}

export async function fetchAuthMe(): Promise<AuthMe | null> {
  const res = await fetch("/auth/me", {
    headers: { Accept: "application/json" },
    credentials: "same-origin",
  });
  if (res.status === 404) return null;
  if (res.status === 401) {
    return null;
  }
  if (!res.ok) {
    throw new Error(`GET /auth/me: HTTP ${res.status}`);
  }
  return (await res.json()) as AuthMe;
}

/** Whether the signed-in user may change the organization: always without WorkOS, else its admins and owners. */
export function isOrgAdmin(auth: AuthMe | null | undefined): boolean {
  if (!auth?.workosEnabled) return true;
  const role = auth.organization?.role;
  return role === "admin" || role === "owner";
}

export function isReadOnlyViewer(auth: AuthMe | null | undefined): boolean {
  return Boolean(auth?.workosEnabled && auth.organization?.role === "viewer");
}

export function useAuthMe() {
  return useQuery({
    queryKey: ["auth-me"],
    queryFn: fetchAuthMe,
    staleTime: 30_000,
    retry: false,
  });
}

export function useSwitchOrganization() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (organizationId: string) =>
      fetchJson<{ organization: AuthOrganization }>("/auth/org/switch", {
        method: "POST",
        body: JSON.stringify({ organizationId }),
      }),
    onSuccess: async () => {
      try {
        localStorage.removeItem("hotbed-project");
      } catch {
        // ignore storage errors
      }
      await qc.resetQueries();
    },
  });
}

export function useCreateOrganization() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (name: string) =>
      fetchJson<{ organization: AuthOrganization }>("/auth/org/create", {
        method: "POST",
        body: JSON.stringify({ name }),
      }),
    onSuccess: async () => {
      try {
        localStorage.removeItem("hotbed-project");
      } catch {
        // ignore
      }
      await qc.resetQueries();
    },
  });
}

export function useOrgMembers(enabled: boolean) {
  return useQuery({
    queryKey: ["org-members"],
    queryFn: () => fetchJson<OrgMembersResponse>("/auth/org/members"),
    enabled,
    staleTime: 10_000,
  });
}

export function useInviteMember() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { email: string; role?: string }) =>
      fetchJson<{ invitation: OrgInvitation }>("/auth/org/invite", {
        method: "POST",
        body: JSON.stringify(input),
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["org-members"] }),
  });
}

export function useRevokeInvitation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (invitationId: string) =>
      fetchJson<{ ok: boolean }>(`/auth/org/invitations/${encodeURIComponent(invitationId)}`, {
        method: "DELETE",
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["org-members"] }),
  });
}

export function useRemoveMember() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (membershipId: string) =>
      fetchJson<{ ok: boolean }>(`/auth/org/members/${encodeURIComponent(membershipId)}`, {
        method: "DELETE",
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["org-members"] }),
  });
}

export function useJoinTokens() {
  return useQuery({
    queryKey: ["admin-join-tokens"],
    queryFn: () => fetchJson<{ orgId?: string; tokens: JoinTokenMeta[] }>("/admin/join-tokens"),
    staleTime: 10_000,
  });
}

export function useCreateJoinToken() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { label?: string; expiresInHours?: number }) =>
      fetchJson<MintedJoinToken>("/admin/join-tokens", {
        method: "POST",
        body: JSON.stringify(input),
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["admin-join-tokens"] }),
  });
}

export function useRevokeJoinToken() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) =>
      fetchJson<{ ok: boolean }>(`/admin/join-tokens/${encodeURIComponent(id)}`, {
        method: "DELETE",
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["admin-join-tokens"] }),
  });
}

export function useApiTokens() {
  return useQuery({
    queryKey: ["admin-api-tokens"],
    queryFn: () => fetchJson<{ orgId?: string; tokens: ApiTokenMeta[] }>("/admin/api-tokens"),
    staleTime: 10_000,
  });
}

export function useCreateApiToken() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { name: string; expiresInDays?: number }) =>
      fetchJson<MintedApiToken>("/admin/api-tokens", {
        method: "POST",
        body: JSON.stringify(input),
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["admin-api-tokens"] }),
  });
}

export function useRevokeApiToken() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) =>
      fetchJson<{ ok: boolean }>(`/admin/api-tokens/${encodeURIComponent(id)}`, {
        method: "DELETE",
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["admin-api-tokens"] }),
  });
}

export async function signOut(): Promise<void> {
  try {
    const res = await fetchJson<{ ok: boolean; logoutUrl?: string }>("/auth/logout", {
      method: "POST",
    });
    window.location.href = res.logoutUrl || "/auth/login";
  } catch {
    window.location.href = "/auth/login";
  }
}
