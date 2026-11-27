// Edge authentication and per-organization routing for the Hotbed Admin Agent:
//   - /agents/admin-agent/:threadId/*  -> Org-isolated Agents SDK WebSocket & HTTP transport
//   - /api/admin-agent/status          -> Model, scope & capability status + org's DOs
//   - /api/admin-agent/sql             -> Org-scoped direct read-only SQL
//   - /api/admin-agent/schema          -> Org-scoped direct schema inspection

import { getAgentByName } from "agents";
import { isOrgAdminCaller } from "./auth.ts";
import { listAvailableAdminModels } from "./admin-agent-do.ts";
import {
	adminAgentDoName,
	AdminScopeError,
	allowedBindingsForScope,
	getCloudflareApiConfig,
	resolveScopedInspectableDo,
	type AdminOrgScope,
	type DoBindingName,
} from "./admin-cf-api.ts";
import { commandDoName } from "./command-do.ts";
import type { ControllerEnv } from "./controller-do.ts";
import { validateReadOnlySql, type SqlParamValue } from "./do-sql.ts";
import { isWorkOSEnabled } from "./workos.ts";
import { edgeCaller, resolveEdgeRouting, withSetCookie } from "./workos-routes.ts";

const THREAD_ID_RE = /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$/;
const VALID_BINDINGS = new Set<DoBindingName>([
	"CONTROLLER",
	"TENANT_REGISTRY",
	"COMMAND",
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
					Response.json({ message: "organization selection required" }, { status: 403 }),
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
						{ message: "Admin Agent is restricted to organization admins" },
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
			response: withSetCookie(Response.json({ message }, { status: 401 }), setCookie),
		};
	}
}

async function discoverOrgTargets(env: ControllerEnv, scope: AdminOrgScope) {
	const ctrlName = scope.controllerDoName;
	let clusterId = "";
	let cladefsTenantId: number | null = null;
	const recentCommands: Array<{ commandId: string; doName: string; controller: string }> = [];

	try {
		const stub = env.CONTROLLER.getByName(ctrlName);
		const health = await stub.getHealth();
		clusterId = health.clusterId;
		cladefsTenantId = health.cladefsTenantId ?? null;
		if (clusterId) {
			const rows = await stub.adminQuerySql(
				"SELECT id FROM commands ORDER BY created_at DESC LIMIT 10",
				[],
				{ maxRows: 10 },
			);
			for (const r of rows.rows) {
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
		recentCommands,
	};
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

	if (request.method === "GET" && sub === "/status") {
		const { defaultModel, models } = listAvailableAdminModels(env);
		const targets = await discoverOrgTargets(env, scope);
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

	if (request.method === "GET" && sub === "/schema") {
		const binding = (url.searchParams.get("binding") ?? "CONTROLLER").trim().toUpperCase();
		if (!VALID_BINDINGS.has(binding as DoBindingName)) {
			return reply({ message: `invalid binding "${binding}"` }, 400);
		}
		const name = url.searchParams.get("name") ?? undefined;
		const id = url.searchParams.get("id") ?? undefined;
		try {
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
			return reply(
				{ message: err instanceof Error ? err.message : String(err) },
				err instanceof AdminScopeError ? 403 : 400,
			);
		}
	}

	if (request.method === "POST" && sub === "/sql") {
		const body = (await request.json().catch(() => null)) as {
			binding?: string;
			name?: string;
			id?: string;
			sql?: string;
			params?: SqlParamValue[];
			maxRows?: number;
		} | null;
		if (!body || typeof body.sql !== "string") {
			return reply({ message: "`sql` string is required" }, 400);
		}
		const binding = (body.binding ?? "CONTROLLER").trim().toUpperCase();
		if (!VALID_BINDINGS.has(binding as DoBindingName)) {
			return reply({ message: `invalid binding "${binding}"` }, 400);
		}
		try {
			validateReadOnlySql(body.sql);
			const target = await resolveScopedInspectableDo(env, scope, {
				binding: binding as DoBindingName,
				name: body.name,
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
			return reply(
				{ message: err instanceof Error ? err.message : String(err) },
				err instanceof AdminScopeError ? 403 : 400,
			);
		}
	}

	return reply({ message: "not found" }, 404);
}
