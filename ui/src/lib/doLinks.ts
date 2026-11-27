import { createContext, useContext } from "react";
import type { AdminOrgScopeInfo, DoBindingName } from "./adminAgent";

export interface DoTargetRef {
  binding: DoBindingName;
  name?: string;
  id?: string;
}

export interface DoLinkContextValue {
  scope: AdminOrgScopeInfo | undefined;
  allowedBindings: readonly DoBindingName[];
  onOpenDoTarget: (target: DoTargetRef) => void;
}

export interface DoLinkResolutionContext {
  scope?: AdminOrgScopeInfo;
  allowedBindings?: readonly DoBindingName[];
  currentBinding?: DoBindingName;
  currentTargetName?: string;
  currentTargetId?: string;
  sql?: string;
}

export const DoLinkContext = createContext<DoLinkContextValue | null>(null);

export function useDoLinkContext(): DoLinkContextValue | null {
  return useContext(DoLinkContext);
}

const HEX_DO_ID_RE = /^[0-9a-f]{64}$/i;
const VALID_BINDINGS = new Set<DoBindingName>(["CONTROLLER", "TENANT_REGISTRY", "COMMAND", "ADMIN_AGENT"]);

function parseBindingName(raw: unknown): DoBindingName | null {
  if (typeof raw !== "string") return null;
  const upper = raw.trim().toUpperCase() as DoBindingName;
  return VALID_BINDINGS.has(upper) ? upper : null;
}

/**
 * Extracts the primary table referenced after FROM in a single-table SELECT
 * query (ignoring JOIN queries where an unqualified `id` could belong to
 * another table).
 */
export function extractSingleFromTable(sql: string | undefined): string | null {
  if (!sql) return null;
  // Strip SQL comments and string literals before matching table names.
  const cleaned = sql
    .replace(/--[^\n]*/g, " ")
    .replace(/\/\*[\s\S]*?\*\//g, " ")
    .replace(/'(?:''|[^'])*'/g, "''");
  if (/\bjoin\b/i.test(cleaned)) return null;
  const match = /\bfrom\s+(?:["`\[]?[a-zA-Z_][a-zA-Z0-9_]*["`\]]?\.)?["`\[]?([a-zA-Z_][a-zA-Z0-9_]*)["`\]]?/i.exec(
    cleaned,
  );
  return match?.[1] ? match[1].toLowerCase() : null;
}

function isTargetAllowedInScope(
  target: DoTargetRef,
  ctx: DoLinkResolutionContext,
): boolean {
  if (ctx.allowedBindings && !ctx.allowedBindings.includes(target.binding)) {
    return false;
  }
  const scope = ctx.scope;
  if (!scope) return true;

  switch (target.binding) {
    case "CONTROLLER":
      if (target.name) return target.name === scope.controllerDoName;
      return Boolean(target.id && HEX_DO_ID_RE.test(target.id));
    case "COMMAND":
      if (target.name) {
        if (!scope.clusterId) return false;
        const prefix = `cmd:${scope.clusterId}:`;
        return target.name.startsWith(prefix) && target.name.length > prefix.length;
      }
      return Boolean(target.id && HEX_DO_ID_RE.test(target.id));
    case "ADMIN_AGENT": {
      if (!target.name) return false;
      const prefix = `admin:${scope.controllerDoName}:`;
      return target.name.startsWith(prefix) && target.name.length > prefix.length;
    }
    case "TENANT_REGISTRY":
      return scope.isGlobalAdmin && (!target.name || target.name === "global");
  }
}

function isSelfTarget(target: DoTargetRef, ctx: DoLinkResolutionContext): boolean {
  if (!ctx.currentBinding || target.binding !== ctx.currentBinding) return false;
  if (target.name && ctx.currentTargetName && target.name === ctx.currentTargetName) return true;
  if (target.id && ctx.currentTargetId && target.id.toLowerCase() === ctx.currentTargetId.toLowerCase()) {
    return true;
  }
  return false;
}

function acceptTarget(candidate: DoTargetRef | null, ctx: DoLinkResolutionContext): DoTargetRef | null {
  if (!candidate) return null;
  if (isSelfTarget(candidate, ctx)) return null;
  if (!isTargetAllowedInScope(candidate, ctx)) return null;
  return candidate;
}

function inferTargetFromExplicitName(val: string, rowBinding: DoBindingName | null): DoTargetRef | null {
  if (val.startsWith("cmd:") && val.split(":").length >= 3) {
    return { binding: "COMMAND", name: val };
  }
  if (val.startsWith("admin:") && val.split(":").length >= 3) {
    return { binding: "ADMIN_AGENT", name: val };
  }
  if (val.startsWith("org:") && val.length > 4) {
    return { binding: "CONTROLLER", name: val };
  }
  if (rowBinding) {
    return { binding: rowBinding, name: val };
  }
  if (val === "global") {
    return { binding: "TENANT_REGISTRY", name: "global" };
  }
  if (val === "default") {
    return { binding: "CONTROLLER", name: "default" };
  }
  return null;
}

function commandTarget(rawCommandId: string, clusterId: string | null | undefined): DoTargetRef | null {
  const val = rawCommandId.trim();
  if (!val) return null;
  if (val.startsWith("cmd:") && val.split(":").length >= 3) {
    return { binding: "COMMAND", name: val };
  }
  if (!clusterId) return null;
  return { binding: "COMMAND", name: `cmd:${clusterId}:${val}` };
}

/**
 * Resolves a SQL table cell or KeyValue item into a clickable Cloudflare
 * Durable Object target when the column/value represents another Durable Object
 * accessible within the active organization's scope.
 */
export function resolveCellDoLink(
  value: unknown,
  columnKey: string,
  row: Record<string, unknown>,
  ctx: DoLinkResolutionContext,
): DoTargetRef | null {
  if (typeof value !== "string") return null;
  const val = value.trim();
  if (!val) return null;

  const col = columnKey.toLowerCase();
  const rowBinding = parseBindingName(row.binding ?? row.do_binding ?? row.namespace);
  const clusterId = ctx.scope?.clusterId ?? null;

  // 1. Unambiguous Durable Object name prefixes in any column.
  if (val.startsWith("cmd:") && val.split(":").length >= 3) {
    return acceptTarget({ binding: "COMMAND", name: val }, ctx);
  }
  if (val.startsWith("admin:") && val.split(":").length >= 3) {
    return acceptTarget({ binding: "ADMIN_AGENT", name: val }, ctx);
  }
  if (val.startsWith("org:") && val.length > 4 && !/\s/.test(val)) {
    return acceptTarget({ binding: "CONTROLLER", name: val }, ctx);
  }

  // 2. Explicit command ID columns (e.g. schedule_runs.command_id or Agent Chat tables).
  if (col === "command_id" || col === "commandid") {
    return acceptTarget(commandTarget(val, clusterId), ctx);
  }

  // 3. Explicit ControllerDO name columns (e.g. admin_agent_scope.controller_do_name).
  if (col === "controller_do_name" || col === "controllerdoname" || col === "controller_name" || col === "controller") {
    return acceptTarget({ binding: "CONTROLLER", name: val }, ctx);
  }

  // 4. TenantRegistryDO.org_tenants.org_key -> ControllerDO name ("default" or "org:<key>").
  if (col === "org_key" || col === "orgkey") {
    const ctrlName = val === "default" || val.startsWith("org:") ? val : `org:${val}`;
    return acceptTarget({ binding: "CONTROLLER", name: ctrlName }, ctx);
  }

  // 5. Generic DO name columns (e.g. discover_durable_objects / Agent Chat tables).
  if (col === "do_name" || col === "doname" || col === "object_name" || col === "objectname") {
    return acceptTarget(inferTargetFromExplicitName(val, rowBinding), ctx);
  }

  // 6. 64-character hex Cloudflare Durable Object ID columns.
  if (
    HEX_DO_ID_RE.test(val) &&
    (col === "do_id" ||
      col === "doid" ||
      col === "object_id" ||
      col === "objectid" ||
      col === "durable_object_id" ||
      col === "command_do_id" ||
      col === "controller_do_id" ||
      (col === "id" && rowBinding !== null))
  ) {
    const binding: DoBindingName =
      rowBinding ??
      (col.includes("controller")
        ? "CONTROLLER"
        : col.includes("command")
          ? "COMMAND"
          : ctx.currentBinding ?? "COMMAND");
    return acceptTarget({ binding, id: val.toLowerCase() }, ctx);
  }

  // 7. ControllerDO table-aware references:
  //    - commands.id and history.id -> CommandDO
  //    - events.ref_id when ref_type = 'command' -> CommandDO
  if (ctx.currentBinding === "CONTROLLER") {
    if (col === "id") {
      const table = extractSingleFromTable(ctx.sql);
      if (table === "commands" || table === "history") {
        return acceptTarget(commandTarget(val, clusterId), ctx);
      }
    }
    if (col === "ref_id" || col === "refid") {
      const refType = String(row.ref_type ?? row.refType ?? "").toLowerCase();
      if (refType === "command") {
        return acceptTarget(commandTarget(val, clusterId), ctx);
      }
    }
  }

  return null;
}

export function formatDoTargetLabel(target: DoTargetRef): string {
  if (target.id) return `${target.binding}(id:${target.id.slice(0, 12)}…)`;
  return `${target.binding}(${target.name ?? "default"})`;
}
