// Edge authentication and per-organization routing for the Hotbed Admin Explorer:
//   - /api/admin-agent/status -> Org scope, allowed DO bindings, and recent CommandDOs
//   - /api/admin-agent/schema -> Org-scoped read-only SQLite schema inspection
//   - /api/admin-agent/sql    -> Org-scoped read-only SQLite query execution
//   - /api/admin-agent/health -> Org ControllerDO live health
//   - /api/admin-agent/gc     -> Tenant-scoped CladeFS R2 usage / GC inspection
//
// Security invariant: EVERY organization (including the default organization)
// is strictly confined to its own ControllerDO, its own CommandDOs
// (`cmd:<clusterId>:*`), and its own CladeFS tenant R2 usage. Global
// TenantRegistryDO and account-wide Cloudflare APIs are never exposed here.

import { getBearerToken, isOrgAdminCaller, type CallerIdentity } from "./auth.ts";
import {
	AdminScopeError,
	allowedBindingsForScope,
	inspectGcForScope,
	resolveScopedInspectableDo,
	type AdminOrgScope,
	type DoBindingName,
} from "./admin-cf-api.ts";
import { commandDoName } from "./command-do.ts";
import { workerDoName } from "./worker-do.ts";
import type { ControllerEnv } from "./controller-do.ts";
import {
	CONTROLLER_ADMIN_DENIED_TABLES,
	validateReadOnlySql,
	type SqlParamValue,
} from "./do-sql.ts";
import { parseOrgScopedToken } from "./workos.ts";
import { edgeCaller, resolveEdgeRouting, withSetCookie } from "./workos-routes.ts";

const VALID_BINDINGS = new Set<DoBindingName>(["CONTROLLER", "COMMAND", "WORKER"]);

async function authorizeAdminRequest(
	request: Request,
	url: URL,
	env: ControllerEnv,
): Promise<
	| { ok: true; setCookie?: string; login: string; scope: AdminOrgScope }
	| { ok: false; response: Response }
> {
	let setCookie: string | undefined;
	try {
		const routing = await resolveEdgeRouting(request, url, env, {
			allowTopLevelNavigate: false,
		});
		setCookie = routing.setCookie;
		if (routing.needsOrgSelection) {
			return {
				ok: false,
				response: withSetCookie(
					Response.json(
						{ code: "permission_denied", message: "organization selection required" },
						{ status: 403 },
					),
					setCookie,
				),
			};
		}

		let caller: CallerIdentity;
		const bearer = getBearerToken(request);
		const scoped = !routing.caller && bearer ? parseOrgScopedToken(bearer) : null;
		if (scoped && scoped.kind === "api") {
			const info = await env.CONTROLLER.getByName(routing.doName).checkApiToken({
				bearer,
				orgId: routing.orgId,
				isDefaultDo: routing.doName === "default",
			});
			if (!info.ok) {
				return {
					ok: false,
					response: withSetCookie(
						Response.json(
							{
								code: errorCodeForStatus(info.status),
								message: info.error.message || "authentication required",
							},
							{ status: info.status },
						),
						setCookie,
					),
				};
			}
			caller = {
				role: "owner",
				nodeId: info.userId ? `api:${info.userId}` : `api:${info.login}`,
				hostname: info.login,
				login: info.login,
				userId: info.userId ?? "",
				orgId: info.orgId || routing.orgId,
				orgRole: info.orgRole ?? "",
				authMethod: "api-token",
			};
		} else {
			caller = await edgeCaller(request, env, routing, { allowTopLevelNavigate: false });
		}

		if (!isOrgAdminCaller(caller)) {
			return {
				ok: false,
				response: withSetCookie(
					Response.json(
						{
							code: "permission_denied",
							message: "Admin tools are restricted to organization admins",
						},
						{ status: 403 },
					),
					setCookie,
				),
			};
		}

		return {
			ok: true,
			setCookie,
			login: caller.login,
			scope: {
				controllerDoName: routing.doName,
				orgId: routing.orgId || caller.orgId,
			},
		};
	} catch (err) {
		const message = err instanceof Error ? err.message : "authentication required";
		return {
			ok: false,
			response: withSetCookie(
				Response.json({ code: "unauthenticated", message }, { status: 401 }),
				setCookie,
			),
		};
	}
}

async function discoverOrgTargets(env: ControllerEnv, scope: AdminOrgScope, commandLimit = 10) {
	const limit = Math.max(1, Math.min(50, Math.floor(commandLimit) || 10));
	const ctrlName = scope.controllerDoName;
	let clusterId = "";
	let cladefsTenantId: number | null = null;
	const recentCommands: Array<{ commandId: string; doName: string; controller: string }> = [];
	const workers: Array<{ nodeId: string; hostname: string; doName: string; controller: string }> =
		[];

	try {
		const stub = env.CONTROLLER.getByName(ctrlName);
		const health = await stub.getHealth();
		clusterId = health.clusterId;
		cladefsTenantId = health.cladefsTenantId ?? null;
		if (clusterId) {
			const [cmdRpc, nodeRpc] = await Promise.all([
				stub.adminQuerySql("SELECT id FROM commands ORDER BY created_at DESC LIMIT ?", [limit], {
					maxRows: limit,
				}),
				stub.adminQuerySql(
					"SELECT id, hostname FROM nodes WHERE retired_at IS NULL ORDER BY hostname, id LIMIT 100",
					[],
					{ maxRows: 100 },
				),
			]);
			if (cmdRpc.ok) {
				for (const r of cmdRpc.result.rows) {
					const id = String(r.id ?? "");
					if (id) {
						recentCommands.push({
							commandId: id,
							doName: commandDoName(clusterId, id),
							controller: ctrlName,
						});
					}
				}
			}
			if (nodeRpc.ok) {
				for (const r of nodeRpc.result.rows) {
					const id = String(r.id ?? "");
					if (id) {
						workers.push({
							nodeId: id,
							hostname: String(r.hostname ?? ""),
							doName: workerDoName(clusterId, id),
							controller: ctrlName,
						});
					}
				}
			}
		}
	} catch {
		// Non-fatal: controller may not have commands yet.
	}

	return {
		clusterId,
		cladefsTenantId,
		controllers: [
			{
				name: ctrlName,
				orgKey: scope.orgId || ctrlName,
				tenantId: cladefsTenantId !== null ? String(cladefsTenantId) : null,
			},
		],
		workers,
		recentCommands,
	};
}

async function resolveCommandIdToName(
	env: ControllerEnv,
	scope: AdminOrgScope,
	commandId: string,
): Promise<string> {
	const cleaned = commandId.trim();
	if (!cleaned) {
		throw new Error("commandId must not be empty");
	}
	if (cleaned.startsWith("cmd:")) {
		return cleaned;
	}
	const health = await env.CONTROLLER.getByName(scope.controllerDoName).getHealth();
	if (!health.clusterId) {
		throw new AdminScopeError("organization controller has no clusterId yet");
	}
	return commandDoName(health.clusterId, cleaned);
}

function errorCodeForStatus(status: number): string {
	switch (status) {
		case 400:
			return "invalid_args";
		case 401:
			return "unauthenticated";
		case 403:
			return "permission_denied";
		case 404:
			return "not_found";
		default:
			return "internal";
	}
}

function rejectDisallowedBinding(binding: string): { status: number; message: string } {
	if (binding === "TENANT_REGISTRY") {
		return {
			status: 403,
			message:
				"access denied: TenantRegistryDO is global across organizations and is never accessible from Admin Explorer",
		};
	}
	if (binding === "ADMIN_AGENT") {
		return {
			status: 403,
			message: "access denied: AdminAgentDO has been removed",
		};
	}
	return { status: 400, message: `invalid binding "${binding}"` };
}

/**
 * Handles `/api/admin-agent/*` (org-scoped status, direct read-only SQL,
 * schema inspection, controller health, and tenant-scoped CladeFS GC).
 */
export async function handleAdminAgentRoute(
	request: Request,
	url: URL,
	env: ControllerEnv,
): Promise<Response | null> {
	if (!url.pathname.startsWith("/api/admin-agent/")) return null;

	const auth = await authorizeAdminRequest(request, url, env);
	if (!auth.ok) return auth.response;
	const { scope } = auth;

	const reply = (body: unknown, status = 200) =>
		withSetCookie(Response.json(body, { status }), auth.setCookie);
	const fail = (message: string, status: number) =>
		reply({ code: errorCodeForStatus(status), message }, status);

	const sub = url.pathname.slice("/api/admin-agent".length);

	if (request.method === "GET" && sub === "/status") {
		const rawLimit = Number(url.searchParams.get("limit") ?? "10");
		const limit = Number.isFinite(rawLimit) && rawLimit > 0 ? rawLimit : 10;
		const targets = await discoverOrgTargets(env, scope, limit);
		return reply({
			ok: true,
			scope: {
				controllerDoName: scope.controllerDoName,
				orgId: scope.orgId || null,
				clusterId: targets.clusterId || null,
				cladefsTenantId: targets.cladefsTenantId,
			},
			gcWorkerConfigured: Boolean(
				(env.GC_WORKER || (env.GC_WORKER_URL ?? "").trim()) && (env.GC_TOKEN ?? "").trim(),
			),
			bindings: allowedBindingsForScope(),
			controllers: targets.controllers,
			recentCommands: targets.recentCommands,
		});
	}

	if (request.method === "GET" && sub === "/health") {
		try {
			const health = await env.CONTROLLER.getByName(scope.controllerDoName).getHealth();
			return reply(health);
		} catch (err) {
			return fail(err instanceof Error ? err.message : String(err), 500);
		}
	}

	if (request.method === "GET" && sub === "/gc") {
		const rawAction = (url.searchParams.get("action") ?? "status").trim();
		if (rawAction !== "status" && rawAction !== "dry_run_cell" && rawAction !== "live_cell") {
			return fail(
				`invalid gc action "${rawAction}" (expected status, dry_run_cell, or live_cell)`,
				400,
			);
		}
		const cell = url.searchParams.get("cell") ?? undefined;
		try {
			const res = await inspectGcForScope(env, scope, { action: rawAction, cell });
			if (!res.ok) {
				const errMsg =
					typeof res.body.error === "string"
						? res.body.error
						: `GC worker inspection failed (HTTP ${res.status})`;
				return reply(
					{ code: errorCodeForStatus(res.status), message: errMsg, ...res.body },
					res.status,
				);
			}
			return reply(res.body);
		} catch (err) {
			return fail(err instanceof Error ? err.message : String(err), 500);
		}
	}

	if (request.method === "GET" && sub === "/schema") {
		const commandId = (url.searchParams.get("commandId") ?? "").trim();
		const binding = (url.searchParams.get("binding") ?? (commandId ? "COMMAND" : "CONTROLLER"))
			.trim()
			.toUpperCase();
		if (!VALID_BINDINGS.has(binding as DoBindingName)) {
			const rej = rejectDisallowedBinding(binding);
			return fail(rej.message, rej.status);
		}
		let name = url.searchParams.get("name") ?? undefined;
		const id = url.searchParams.get("id") ?? undefined;
		try {
			if (commandId) {
				if (binding !== "COMMAND") {
					return fail("commandId may only be used with binding COMMAND", 400);
				}
				if (name?.trim() || id?.trim()) {
					return fail("specify either commandId or name/id, not both", 400);
				}
				name = await resolveCommandIdToName(env, scope, commandId);
			}
			const target = await resolveScopedInspectableDo(env, scope, {
				binding: binding as DoBindingName,
				name,
				id,
			});
			const schema = await target.stub.adminInspectSchema();
			return reply({
				target: target.label,
				binding,
				resolvedBy: target.resolvedBy,
				value: target.value,
				...schema,
			});
		} catch (err) {
			return fail(
				err instanceof Error ? err.message : String(err),
				err instanceof AdminScopeError ? 403 : 400,
			);
		}
	}

	if (request.method === "POST" && sub === "/sql") {
		const body = (await request.json().catch(() => null)) as {
			binding?: string;
			name?: string;
			id?: string;
			commandId?: string;
			sql?: string;
			params?: SqlParamValue[];
			maxRows?: number;
		} | null;
		if (!body || typeof body.sql !== "string") {
			return fail("`sql` string is required", 400);
		}
		const commandId = (body.commandId ?? "").trim();
		const binding = (body.binding ?? (commandId ? "COMMAND" : "CONTROLLER")).trim().toUpperCase();
		if (!VALID_BINDINGS.has(binding as DoBindingName)) {
			const rej = rejectDisallowedBinding(binding);
			return fail(rej.message, rej.status);
		}
		if (body.maxRows !== undefined) {
			if (typeof body.maxRows !== "number" || !Number.isInteger(body.maxRows) || body.maxRows < 1) {
				return fail("`maxRows` must be a positive integer", 400);
			}
		}
		try {
			validateReadOnlySql(body.sql, {
				deniedTables: binding === "CONTROLLER" ? CONTROLLER_ADMIN_DENIED_TABLES : undefined,
			});
			let name = body.name;
			if (commandId) {
				if (binding !== "COMMAND") {
					return fail("commandId may only be used with binding COMMAND", 400);
				}
				if (body.name?.trim() || body.id?.trim()) {
					return fail("specify either commandId or name/id, not both", 400);
				}
				name = await resolveCommandIdToName(env, scope, commandId);
			}
			const target = await resolveScopedInspectableDo(env, scope, {
				binding: binding as DoBindingName,
				name,
				id: body.id,
			});
			const rpcResult = await target.stub.adminQuerySql(body.sql, body.params ?? [], {
				maxRows: body.maxRows,
			});
			if (!rpcResult.ok) {
				return fail(rpcResult.error, 400);
			}
			return reply({
				target: target.label,
				binding,
				resolvedBy: target.resolvedBy,
				value: target.value,
				sql: body.sql,
				...rpcResult.result,
			});
		} catch (err) {
			return fail(
				err instanceof Error ? err.message : String(err),
				err instanceof AdminScopeError ? 403 : 400,
			);
		}
	}

	return fail("not found", 404);
}
