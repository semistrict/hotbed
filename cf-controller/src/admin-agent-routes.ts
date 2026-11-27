// Edge authentication and routing for the Hotbed Admin Agent:
//   - /agents/admin-agent/:threadId/*  -> Agents SDK WebSocket & HTTP chat transport
//   - /api/admin-agent/status          -> Model & capability status + known DOs
//   - /api/admin-agent/sql             -> Direct read-only SQL on any project DO
//   - /api/admin-agent/schema          -> Direct schema inspection of any project DO

import { getAgentByName } from "agents";
import { isOrgAdminCaller } from "./auth.ts";
import { listAvailableAdminModels } from "./admin-agent-do.ts";
import {
	getCloudflareApiConfig,
	resolveInspectableDo,
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
): Promise<{ ok: true; setCookie?: string; login: string } | { ok: false; response: Response }> {
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

		// Cross-DO / cluster-wide inspection belongs to the default/owner org alone.
		if (isWorkOSEnabled(env) && routing.doName !== "default" && !routing.globalAdmin) {
			return {
				ok: false,
				response: withSetCookie(
					Response.json(
						{ message: "Admin Agent is restricted to the default organization" },
						{ status: 403 },
					),
					setCookie,
				),
			};
		}

		return { ok: true, setCookie, login: caller.login };
	} catch (err) {
		const message = err instanceof Error ? err.message : "authentication required";
		return {
			ok: false,
			response: withSetCookie(Response.json({ message }, { status: 401 }), setCookie),
		};
	}
}

async function discoverQuickTargets(env: ControllerEnv) {
	const controllers: Array<{ name: string; orgKey: string; tenantId: string | null }> = [];
	const seen = new Set<string>();

	if (env.TENANT_REGISTRY) {
		try {
			const tenants = await env.TENANT_REGISTRY.getByName("global").listTenants();
			const defaultOrgId = (env.DEFAULT_ORG_ID ?? "").trim();
			for (const t of tenants) {
				const doName =
					t.orgKey === "default" || (defaultOrgId && t.orgKey === defaultOrgId)
						? "default"
						: t.orgKey.startsWith("org:")
							? t.orgKey
							: `org:${t.orgKey}`;
				if (!seen.has(doName)) {
					seen.add(doName);
					controllers.push({ name: doName, orgKey: t.orgKey, tenantId: t.tenantId });
				}
			}
		} catch {
			// Fall back to default controller below.
		}
	}

	if (!seen.has("default")) {
		controllers.unshift({ name: "default", orgKey: "default", tenantId: null });
	}

	const recentCommands: Array<{ commandId: string; doName: string; controller: string }> = [];
	try {
		const defStub = env.CONTROLLER.getByName("default");
		const health = await defStub.getHealth();
		if (health.clusterId) {
			const rows = await defStub.adminQuerySql(
				"SELECT id FROM commands ORDER BY created_at DESC LIMIT 10",
				[],
				{ maxRows: 10 },
			);
			for (const r of rows.rows) {
				const id = String(r.id ?? "");
				if (id) {
					recentCommands.push({
						commandId: id,
						doName: commandDoName(health.clusterId, id),
						controller: "default",
					});
				}
			}
		}
	} catch {
		// Non-fatal: controller may not have commands yet.
	}

	return { controllers, recentCommands };
}

/**
 * Handles `/agents/admin-agent/*` (Cloudflare Agents SDK WebSocket + HTTP)
 * and `/api/admin-agent/*` (status, direct read-only SQL, schema inspection).
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

	const reply = (body: unknown, status = 200) =>
		withSetCookie(Response.json(body, { status }), auth.setCookie);

	if (isAgentsPath) {
		if (!env.ADMIN_AGENT) {
			return reply({ message: "AdminAgentDO binding is not configured" }, 503);
		}
		// URL form: /agents/admin-agent/<threadId>[/<subpath>]
		const rest = url.pathname.slice("/agents/admin-agent".length).replace(/^\/+/, "");
		const slashIdx = rest.indexOf("/");
		const rawThread = (slashIdx === -1 ? rest : rest.slice(0, slashIdx)) || "default";
		const threadId = decodeURIComponent(rawThread);
		if (!THREAD_ID_RE.test(threadId)) {
			return reply(
				{
					message: "invalid thread id (must be 1-64 alphanumeric, dot, dash, or underscore chars)",
				},
				400,
			);
		}

		const doInstanceName = `admin:${threadId}`;
		const stub = await getAgentByName(env.ADMIN_AGENT, doInstanceName);

		// PartyServer / Agents SDK expects these headers when routing manually via getAgentByName.
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
		const targets = await discoverQuickTargets(env);
		return reply({
			ok: true,
			aiConfigured: models.length > 0,
			workersAiBound: Boolean(env.AI),
			anthropicConfigured: Boolean((env.ANTHROPIC_API_KEY ?? "").trim()),
			openaiConfigured: Boolean((env.OPENAI_API_KEY ?? "").trim()),
			cloudflareApiConfigured: Boolean(getCloudflareApiConfig(env)),
			gcWorkerConfigured: Boolean(
				(env.GC_WORKER || (env.GC_WORKER_URL ?? "").trim()) && (env.GC_TOKEN ?? "").trim(),
			),
			defaultModel,
			models,
			bindings: ["CONTROLLER", "TENANT_REGISTRY", "COMMAND", "ADMIN_AGENT"],
			...targets,
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
			const target = resolveInspectableDo(env, {
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
			return reply({ message: err instanceof Error ? err.message : String(err) }, 400);
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
			// Validate read-only SQL up front before crossing the DO RPC boundary
			// (the DO itself also re-validates as defense in depth).
			validateReadOnlySql(body.sql);
			const target = resolveInspectableDo(env, {
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
			return reply({ message: err instanceof Error ? err.message : String(err) }, 400);
		}
	}

	return reply({ message: "not found" }, 404);
}
