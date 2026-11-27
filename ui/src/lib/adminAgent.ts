import { useMutation, useQuery } from "@tanstack/react-query";
import { fetchJson } from "./http";

export type DoBindingName = "CONTROLLER" | "COMMAND" | "WORKER";

export interface AdminOrgScopeInfo {
  controllerDoName: string;
  orgId: string | null;
  clusterId: string | null;
  cladefsTenantId: number | null;
}

export interface AdminAgentStatus {
  ok: boolean;
  scope: AdminOrgScopeInfo;
  gcWorkerConfigured: boolean;
  bindings: DoBindingName[];
  controllers: Array<{ name: string; orgKey: string; tenantId: string | null }>;
  workers?: Array<{ nodeId: string; hostname: string; doName: string; controller: string }>;
  recentCommands: Array<{ commandId: string; doName: string; controller: string }>;
}

export interface DoTableColumn {
  cid: number;
  name: string;
  type: string;
  notNull: boolean;
  defaultValue: string | null;
  primaryKey: number;
}

export interface DoTableInfo {
  name: string;
  type: "table" | "view";
  rowCount: number | null;
  columns: DoTableColumn[];
  sql: string;
  internal: boolean;
}

export interface DoSchemaResponse {
  target: string;
  binding: DoBindingName;
  resolvedBy: "name" | "id";
  value: string;
  databaseSizeBytes: number;
  alarmAtMs: number | null;
  tables: DoTableInfo[];
  indexes: Array<{ name: string; tableName: string; sql: string }>;
  clusterId?: string;
  orgId?: string;
  cladefsTenantId?: string | null;
}

export interface DoSqlResponse {
  target: string;
  binding: DoBindingName;
  resolvedBy: "name" | "id";
  value: string;
  sql: string;
  columns: string[];
  rows: Record<string, unknown>[];
  rowCount: number;
  truncated: boolean;
  rowsRead: number;
  rowsWritten: number;
  durationMs: number;
  databaseSizeBytes: number;
}

export function useAdminAgentStatus() {
  return useQuery({
    queryKey: ["admin-agent-status"],
    queryFn: () => fetchJson<AdminAgentStatus>("/api/admin-agent/status"),
    staleTime: 30_000,
    retry: false,
  });
}

export function useInspectDoSchema(
  params: { binding: DoBindingName; name?: string; id?: string },
  enabled: boolean,
) {
  const sp = new URLSearchParams({ binding: params.binding });
  if (params.id?.trim()) sp.set("id", params.id.trim());
  else if (params.name?.trim()) sp.set("name", params.name.trim());

  return useQuery({
    queryKey: ["admin-agent-schema", params.binding, params.name ?? "", params.id ?? ""],
    queryFn: () => fetchJson<DoSchemaResponse>(`/api/admin-agent/schema?${sp.toString()}`),
    enabled,
    staleTime: 15_000,
    retry: false,
  });
}

export function useRunDoSql() {
  return useMutation({
    mutationFn: (input: {
      binding: DoBindingName;
      name?: string;
      id?: string;
      sql: string;
      maxRows?: number;
    }) =>
      fetchJson<DoSqlResponse>("/api/admin-agent/sql", {
        method: "POST",
        body: JSON.stringify(input),
      }),
  });
}
