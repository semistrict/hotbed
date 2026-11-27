// Edge authentication and per-organization routing for the Hotbed Admin Explorer:
//   - /agents/admin-agent/:threadId/*  -> Org-isolated Agents SDK WebSocket & HTTP transport
//   - /api/admin-agent/status          -> Model, scope & capability status + org's DOs
//   - /api/admin-agent/sql             -> Org-scoped direct read-only SQL
//   - /api/admin-agent/schema          -> Org-scoped direct schema inspection
//   - /api/admin-agent/health          -> Org ControllerDO live health
//   - /api/admin-agent/gc              -> Tenant-scoped CladeFS R2 usage / GC inspection
//   - /api/admin-agent/cf/do-objects   -> Cluster-owner Cloudflare DO namespace/object listing
//   - /api/admin-agent/cf/observability -> Cluster-owner Workers Observability query
//   - /api/admin-agent/cf/get          -> Cluster-owner read-only Cloudflare REST API GET

import { getAgentByName } from "agents";
import { isOrgAdminCaller } from "./auth.ts";
import { inspectGcForScope, listAvailableAdminModels } from "./admin-agent-do.ts";
import {
	adminAgentDoName,
	AdminScopeError,
	allowedBindingsForScope,
	cloudflareAccountGet,
	getCloudflareApiConfig,
	listCfDoNamespaces,
	listCfDoObjects,
	queryCfObservability,
	resolveScopedInspectableDo,
	type AdminOrgScope,
	type DoBindingName,
} from "./admin-cf-api.ts";
import { commandDoName } from "./command-do.ts";
import { workerDoName } from "./worker-do.ts";
import type { ControllerEnv } from "./controller-do.ts";
import { validateReadOnlySql, type SqlParamValue } from "./do-sql.ts";
import { isWorkOSEnabled } from "./workos.ts";
import { edgeCaller, resolveEdgeRouting, withSetCookie } from "./workos-routes.ts";

const THREAD_ID_RE = /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$/;
const VALID_BINDINGS = new Set<DoBindingName>([
	"CONTROLLER",
	"TENANT_REGISTRY",
	"COMMAND",
	"WORKER",
	"ADMIN_AGENT",
]);

async function authorizeAdminAgentRequest(
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

		const caller = await edgeCaller(request, env, routing, { allowTopLevelNavigate: false });
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

		const isGlobalAdmin = !isWorkOSEnabled(env)
			? routing.doName === "default"
			: Boolean(routing.globalAdmin && routing.doName === "default");

		return {
			ok: true,
			setCookie,
			login: caller.login,
			scope: {
				controllerDoName: routing.doName,
				orgId: routing.orgId || caller.orgId,
				isGlobalAdmin,
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
			const [cmdRows, nodeRows] = await Promise.all([
				stub.adminQuerySql("SELECT id FROM commands ORDER BY created_at DESC LIMIT ?", [limit], {
					maxRows: limit,
				}),
				stub.adminQuerySql(
					"SELECT id, hostname FROM nodes WHERE retired_at IS NULL ORDER BY hostname, id LIMIT 100",
					[],
					{ maxRows: 100 },
				),
			]);
			for (const r of cmdRows.rows) {
				const id = String(r.id ?? "");
				if (id) {
					recentCommands.push({
						commandId: id,
						doName: commandDoName(clusterId, id),
						controller: ctrlName,
					});
				}
			}
			for (const r of nodeRows.rows) {
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

/**
 * Handles `/agents/admin-agent/*` (org-isolated Cloudflare Agents SDK WebSocket + HTTP)
 * and `/api/admin-agent/*` (org-scoped status, direct read-only SQL, schema inspection).
 */
export async function handleAdminAgentRoute(
	request: Request,
	url: URL,
	env: ControllerEnv,
): Promise<Response | null> {
	const isAgentsPath =
		url.pathname === "/agents/admin-agent" || url.pathname.startsWith("/agents/admin-agent/");
	const isApiPath = url.pathname.startsWith("/api/admin-agent/");
	if (!isAgentsPath && !isApiPath) return null;

	const auth = await authorizeAdminAgentRequest(request, url, env);
	if (!auth.ok) return auth.response;
	const { scope } = auth;

	const reply = (body: unknown, status = 200) =>
		withSetCookie(Response.json(body, { status }), auth.setCookie);

	if (isAgentsPath) {
		if (!env.ADMIN_AGENT) {
			return reply({ message: "AdminAgentDO binding is not configured" }, 503);
		}
		// URL form: /agents/admin-agent/<threadId>[/<subpath>]
		const rest = url.pathname.slice("/agents/admin-agent".length).replace(/^\/+/, "");
		const slashIdx = rest.indexOf("/");
		const rawThread = (slashIdx === -1 ? rest : rest.slice(0, slashIdx)) || "main";
		const threadId = decodeURIComponent(rawThread);
		if (!THREAD_ID_RE.test(threadId)) {
			return reply(
				{
					message: "invalid thread id (must be 1-64 alphanumeric, dot, dash, or underscore chars)",
				},
				400,
			);
		}

		// Physically isolate every organization's chat threads in its own DO namespace.
		const doInstanceName = adminAgentDoName(scope.controllerDoName, threadId);
		const stub = await getAgentByName(env.ADMIN_AGENT, doInstanceName);
		const bound = await stub.bindScope(scope);
		if (!bound.ok) {
			return reply({ message: bound.message }, bound.status);
		}

		const headers = new Headers(request.headers);
		headers.set("x-partykit-room", doInstanceName);
		headers.set("x-partykit-namespace", "admin-agent");
		const fwd = new Request(request, { headers });
		const res = await stub.fetch(fwd);
		return withSetCookie(res, auth.setCookie);
	}

	// ---- /api/admin-agent/* REST helpers ----
	const sub = url.pathname.slice("/api/admin-agent".length);
	const fail = (message: string, status: number) =>
		reply({ code: errorCodeForStatus(status), message }, status);

	if (request.method === "GET" && sub === "/status") {
		const rawLimit = Number(url.searchParams.get("limit") ?? "10");
		const limit = Number.isFinite(rawLimit) && rawLimit > 0 ? rawLimit : 10;
		const { defaultModel, models } = listAvailableAdminModels(env);
		const targets = await discoverOrgTargets(env, scope, limit);
		return reply({
			ok: true,
			scope: {
				controllerDoName: scope.controllerDoName,
				orgId: scope.orgId || null,
				clusterId: targets.clusterId || null,
				cladefsTenantId: targets.cladefsTenantId,
				isGlobalAdmin: scope.isGlobalAdmin,
			},
			aiConfigured: models.length > 0,
			workersAiBound: Boolean(env.AI),
			anthropicConfigured: Boolean((env.ANTHROPIC_API_KEY ?? "").trim()),
			openaiConfigured: Boolean((env.OPENAI_API_KEY ?? "").trim()),
			cloudflareApiConfigured: scope.isGlobalAdmin && Boolean(getCloudflareApiConfig(env)),
			gcWorkerConfigured: Boolean(
				(env.GC_WORKER || (env.GC_WORKER_URL ?? "").trim()) && (env.GC_TOKEN ?? "").trim(),
			),
			defaultModel,
			models,
			bindings: allowedBindingsForScope(scope),
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
			return fail(`invalid binding "${binding}"`, 400);
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
			return fail(`invalid binding "${binding}"`, 400);
		}
		try {
			validateReadOnlySql(body.sql);
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
			const result = await target.stub.adminQuerySql(body.sql, body.params ?? [], {
				maxRows: body.maxRows,
			});
			return reply({
				target: target.label,
				binding,
				resolvedBy: target.resolvedBy,
				value: target.value,
				sql: body.sql,
				...result,
			});
		} catch (err) {
			return fail(
				err instanceof Error ? err.message : String(err),
				err instanceof AdminScopeError ? 403 : 400,
			);
		}
	}

	// ---- Account-wide Cloudflare REST API routes (cluster-owner / global admin only) ----
	if (sub.startsWith("/cf/")) {
		if (!scope.isGlobalAdmin) {
			return fail(
				"access denied: Cloudflare account API tools are restricted to cluster-owner / default-organization admins",
				403,
			);
		}

		if (request.method === "GET" && sub === "/cf/do-objects") {
			const namespaceId = (url.searchParams.get("namespaceId") ?? "").trim();
			const script = url.searchParams.get("script") ?? undefined;
			const rawLimit = url.searchParams.get("limit");
			const limit = rawLimit ? Number(rawLimit) : undefined;
			const cursor = url.searchParams.get("cursor") ?? undefined;
			try {
				if (namespaceId) {
					const res = await listCfDoObjects(env, namespaceId, {
						limit: Number.isFinite(limit) ? limit : undefined,
						cursor,
					});
					return reply(res);
				}
				const res = await listCfDoNamespaces(env, {
					script: script === "*" ? undefined : (script ?? "hotbed-controller"),
				});
				return reply(res);
			} catch (err) {
				return fail(err instanceof Error ? err.message : String(err), 400);
			}
		}

		if (request.method === "POST" && sub === "/cf/observability") {
			const body = (await request.json().catch(() => null)) as {
				view?: "events" | "invocations" | "calculations";
				scriptName?: string;
				minutesBack?: number;
				limit?: number;
				search?: string;
				level?: "debug" | "info" | "warn" | "error";
			} | null;
			try {
				const res = await queryCfObservability(env, {
					view: body?.view ?? "events",
					scriptName: body?.scriptName,
					minutesBack: body?.minutesBack,
					limit: body?.limit,
					search: body?.search,
					level: body?.level,
				});
				return reply(res);
			} catch (err) {
				return fail(err instanceof Error ? err.message : String(err), 400);
			}
		}

		if (sub === "/cf/get" && (request.method === "POST" || request.method === "GET")) {
			let apiPath = "";
			let query: Record<string, string | number | boolean> | undefined;
			if (request.method === "POST") {
				const body = (await request.json().catch(() => null)) as {
					path?: string;
					query?: Record<string, string | number | boolean>;
				} | null;
				apiPath = (body?.path ?? "").trim();
				query = body?.query;
			} else {
				apiPath = (url.searchParams.get("path") ?? "").trim();
				const q: Record<string, string> = {};
				for (const [k, v] of url.searchParams.entries()) {
					if (k !== "path") q[k] = v;
				}
				if (Object.keys(q).length > 0) query = q;
			}
			if (!apiPath) {
				return fail("`path` is required", 400);
			}
			try {
				const res = await cloudflareAccountGet(env, apiPath, { query });
				return reply(res);
			} catch (err) {
				return fail(err instanceof Error ? err.message : String(err), 400);
			}
		}
	}

	return fail("not found", 404);
}
