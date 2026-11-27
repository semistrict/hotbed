// AdminAgentDO: Organization-scoped Cloudflare Agents SDK (AIChatAgent) Durable
// Object with direct read-only SQL & schema access strictly confined to its
// owning organization's Durable Objects, tenant-filtered CladeFS GC inspection,
// optional cluster-owner Cloudflare REST API access, and Vercel json-render UI.

import { createAnthropic } from "@ai-sdk/anthropic";
import { createOpenAI } from "@ai-sdk/openai";
import { AIChatAgent, type OnChatMessageOptions } from "@cloudflare/ai-chat";
import { pipeJsonRender } from "@json-render/core";
import { createAI } from "agents/models/ai-sdk";
import {
	convertToModelMessages,
	createUIMessageStream,
	createUIMessageStreamResponse,
	stepCountIs,
	streamText,
	tool,
	type GenerateTextOnFinishCallback,
	type LanguageModel,
	type ToolSet,
} from "ai";
import { z } from "zod";
import { buildAdminAgentSystemPrompt } from "./admin-agent-catalog.ts";
import {
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
import type { ControllerEnv } from "./controller-do.ts";
import {
	executeReadOnlySql,
	inspectDoSchema,
	type DoSchemaInfo,
	type DoSqlResult,
	type SqlParamValue,
} from "./do-sql.ts";

export interface AdminModelOption {
	id: string;
	label: string;
	provider: "workers-ai" | "anthropic" | "openai";
}

export const WORKERS_AI_MODELS: readonly AdminModelOption[] = [
	{ id: "@cf/zai-org/glm-5.3", label: "GLM 5.3 (Workers AI)", provider: "workers-ai" },
	{ id: "@cf/openai/gpt-oss-120b", label: "GPT-OSS 120B (Workers AI)", provider: "workers-ai" },
	{ id: "@cf/qwen/qwen3.8-27b", label: "Qwen 3.8 27B (Workers AI)", provider: "workers-ai" },
	{
		id: "@cf/deepseek-ai/deepseek-v4.1-flash-1017",
		label: "DeepSeek V4.1 Flash (Workers AI)",
		provider: "workers-ai",
	},
	{
		id: "@cf/meta/llama-3.3-70b-instruct-fp8-fast",
		label: "Llama 3.3 70B Fast (Workers AI)",
		provider: "workers-ai",
	},
	{ id: "@cf/zai-org/glm-4.7-flash", label: "GLM 4.7 Flash (Workers AI)", provider: "workers-ai" },
];

export const ANTHROPIC_MODELS: readonly AdminModelOption[] = [
	{
		id: "anthropic:claude-sonnet-4-5",
		label: "Claude Sonnet 4.5 (Anthropic)",
		provider: "anthropic",
	},
	{ id: "anthropic:claude-opus-4-5", label: "Claude Opus 4.5 (Anthropic)", provider: "anthropic" },
];

export const OPENAI_MODELS: readonly AdminModelOption[] = [
	{ id: "openai:gpt-5", label: "GPT-5 (OpenAI)", provider: "openai" },
	{ id: "openai:gpt-4.1", label: "GPT-4.1 (OpenAI)", provider: "openai" },
];

export const DEFAULT_ADMIN_MODEL = "@cf/zai-org/glm-5.3";

export function listAvailableAdminModels(env: ControllerEnv): {
	defaultModel: string;
	models: AdminModelOption[];
} {
	const models: AdminModelOption[] = [];
	if (env.AI) {
		models.push(...WORKERS_AI_MODELS);
	}
	if ((env.ANTHROPIC_API_KEY ?? "").trim()) {
		models.push(...ANTHROPIC_MODELS);
	}
	if ((env.OPENAI_API_KEY ?? "").trim()) {
		models.push(...OPENAI_MODELS);
	}

	const configuredDefault = (env.ADMIN_AGENT_MODEL ?? "").trim();
	const defaultModel =
		configuredDefault || (models.length > 0 ? models[0].id : DEFAULT_ADMIN_MODEL);

	if (configuredDefault && !models.some((m) => m.id === configuredDefault)) {
		const provider: AdminModelOption["provider"] = configuredDefault.startsWith("anthropic:")
			? "anthropic"
			: configuredDefault.startsWith("openai:")
				? "openai"
				: "workers-ai";
		models.unshift({ id: configuredDefault, label: `${configuredDefault} (Default)`, provider });
	}

	return { defaultModel, models };
}

export function resolveAdminModel(env: ControllerEnv, requestedId?: string): LanguageModel {
	const { defaultModel } = listAvailableAdminModels(env);
	const modelId = (requestedId ?? "").trim() || defaultModel;

	if (modelId.startsWith("anthropic:")) {
		const apiKey = (env.ANTHROPIC_API_KEY ?? "").trim();
		if (!apiKey) {
			throw new Error("ANTHROPIC_API_KEY secret is not configured on hotbed-controller");
		}
		const anthropic = createAnthropic({ apiKey });
		return anthropic(modelId.slice("anthropic:".length));
	}

	if (modelId.startsWith("openai:")) {
		const apiKey = (env.OPENAI_API_KEY ?? "").trim();
		if (!apiKey) {
			throw new Error("OPENAI_API_KEY secret is not configured on hotbed-controller");
		}
		const openai = createOpenAI({ apiKey });
		return openai(modelId.slice("openai:".length));
	}

	if (!env.AI) {
		throw new Error(
			"No AI model provider is configured: bind Workers AI (`AI`) in cloudflare.config.ts or set ANTHROPIC_API_KEY / OPENAI_API_KEY",
		);
	}

	const ai = createAI({ binding: env.AI });
	return ai.languageModel(modelId);
}

async function callGcWorkerAdmin(
	env: ControllerEnv,
	path: string,
): Promise<{ ok: boolean; status: number; body: unknown }> {
	const token = (env.GC_TOKEN ?? "").trim();
	const workerUrl = (env.GC_WORKER_URL ?? "").trim();
	const hasBinding = Boolean(env.GC_WORKER && typeof env.GC_WORKER.fetch === "function");
	if ((!hasBinding && !workerUrl) || !token) {
		return {
			ok: false,
			status: 503,
			body: { error: "CladeFS GC worker is not configured on this controller" },
		};
	}
	const baseUrl = hasBinding ? "https://cladefs-gc.internal" : workerUrl.replace(/\/+$/, "");
	const headers: Record<string, string> = { Accept: "application/json" };
	if (path !== "/healthz") {
		headers.Authorization = `Bearer ${token}`;
	}
	const res = hasBinding
		? await env.GC_WORKER!.fetch(`${baseUrl}${path}`, { method: "GET", headers })
		: await fetch(`${baseUrl}${path}`, { method: "GET", headers });
	const text = await res.text();
	let body: unknown = {};
	try {
		body = text ? JSON.parse(text) : {};
	} catch {
		body = { raw: text };
	}
	return { ok: res.ok, status: res.status, body };
}

interface GcUsageCell {
	tenant?: string | number;
	cell?: string;
	measuredUnixMs?: number | null;
	objects?: number | null;
	bytes?: number | null;
	kinds?: Record<string, unknown> | null;
}

/**
 * Filters a cladefs-gc `/usage` payload to ONLY cells belonging to `tenantId`,
 * recomputing summary counts so an org never sees another org's R2 storage.
 */
export function filterGcUsageForTenant(usageBody: unknown, tenantId: string): unknown {
	if (!usageBody || typeof usageBody !== "object") return usageBody;
	const data = usageBody as { usage?: unknown[] } & Record<string, unknown>;
	const all = Array.isArray(data.usage) ? (data.usage as GcUsageCell[]) : [];
	const scoped = all.filter((u) => String(u.tenant ?? "") === tenantId);
	let measuredCells = 0;
	let totalObjects = 0;
	let totalBytes = 0;
	for (const u of scoped) {
		if (u.measuredUnixMs !== null && u.measuredUnixMs !== undefined) measuredCells++;
		if (typeof u.objects === "number") totalObjects += u.objects;
		if (typeof u.bytes === "number") totalBytes += u.bytes;
	}
	return {
		...data,
		tenantFilter: tenantId,
		cells: scoped.length,
		measuredCells,
		totalObjects,
		totalBytes,
		usage: scoped,
	};
}

export function createAdminAgentTools(env: ControllerEnv, scope: AdminOrgScope): ToolSet {
	const allowedBindings = allowedBindingsForScope(scope);
	const doBindingEnum = scope.isGlobalAdmin
		? z.enum(["CONTROLLER", "COMMAND", "ADMIN_AGENT", "TENANT_REGISTRY"])
		: z.enum(["CONTROLLER", "COMMAND", "ADMIN_AGENT"]);

	const tools: ToolSet = {
		discover_durable_objects: tool({
			description:
				"Discover Cloudflare Durable Objects belonging to this organization: its ControllerDO, its recent CommandDO instances (with their exact `cmd:<clusterId>:<commandId>` names), and its AdminAgentDO namespace.",
			inputSchema: z.object({
				includeRecentCommands: z
					.boolean()
					.optional()
					.describe(
						"Whether to include recent CommandDO names from this organization's ControllerDO (default true).",
					),
				commandLimit: z
					.number()
					.int()
					.min(1)
					.max(50)
					.optional()
					.describe("Max recent commands to list (default 15)."),
			}),
			execute: async ({ includeRecentCommands = true, commandLimit = 15 }) => {
				const ctrlName = scope.controllerDoName;
				const stub = env.CONTROLLER.getByName(ctrlName);
				let clusterId = "";
				let orgId = scope.orgId;
				let cladefsTenantId: number | undefined;
				let connectedWorkers = 0;
				let recentCommandDos: Array<{
					commandId: string;
					doName: string;
					nodeId: string;
					sessionId: string;
					createdAt: number;
				}> = [];
				let error: string | undefined;

				try {
					const health = await stub.getHealth();
					clusterId = health.clusterId;
					orgId = health.orgId ?? scope.orgId;
					cladefsTenantId = health.cladefsTenantId;
					connectedWorkers = health.connectedWorkers;
					if (includeRecentCommands && clusterId) {
						const sqlRes = await stub.adminQuerySql(
							`SELECT id, session_id, node_id, created_at
							 FROM commands
							 ORDER BY created_at DESC
							 LIMIT ?`,
							[commandLimit],
							{ maxRows: commandLimit },
						);
						recentCommandDos = sqlRes.rows.map((r) => {
							const commandId = String(r.id ?? "");
							return {
								commandId,
								doName: commandDoName(clusterId, commandId),
								nodeId: String(r.node_id ?? ""),
								sessionId: String(r.session_id ?? ""),
								createdAt: Number(r.created_at ?? 0),
							};
						});
					}
				} catch (err) {
					error = err instanceof Error ? err.message : String(err);
				}

				return {
					scope: {
						controllerDoName: ctrlName,
						orgId: orgId || null,
						cladefsTenantId: cladefsTenantId ?? null,
						isGlobalAdmin: scope.isGlobalAdmin,
					},
					allowedBindings,
					controller: {
						name: ctrlName,
						clusterId,
						orgId: orgId || null,
						cladefsTenantId: cladefsTenantId ?? null,
						connectedWorkers,
						recentCommandDos,
						...(error ? { error } : {}),
					},
					adminAgentNamespacePrefix: `admin:${ctrlName}:`,
					cloudflareApiConfigured: scope.isGlobalAdmin && Boolean(getCloudflareApiConfig(env)),
				};
			},
		}),

		inspect_do_schema: tool({
			description:
				"Inspect the SQLite schema, tables, columns, row counts, indexes, database size in bytes, and scheduled alarm of a Cloudflare Durable Object belonging to this organization.",
			inputSchema: z.object({
				binding: doBindingEnum.describe("Which Durable Object binding to inspect."),
				name: z
					.string()
					.optional()
					.describe(
						`Durable Object name within this organization (defaults to "${scope.controllerDoName}" for CONTROLLER).`,
					),
				id: z
					.string()
					.optional()
					.describe(
						"64-character hex Cloudflare Durable Object ID belonging to this organization.",
					),
			}),
			execute: async ({ binding, name, id }) => {
				try {
					const target = await resolveScopedInspectableDo(env, scope, {
						binding: binding as DoBindingName,
						name,
						id,
					});
					const schema = await target.stub.adminInspectSchema();
					return {
						target: target.label,
						binding,
						resolvedBy: target.resolvedBy,
						value: target.value,
						...schema,
					};
				} catch (err) {
					return { error: err instanceof Error ? err.message : String(err) };
				}
			},
		}),

		run_do_sql: tool({
			description:
				"Execute a read-only SQLite query (SELECT, WITH ... SELECT, EXPLAIN, or read-only PRAGMA) directly on a Cloudflare Durable Object belonging to this organization.",
			inputSchema: z.object({
				binding: doBindingEnum.describe("Which Durable Object binding to query."),
				name: z
					.string()
					.optional()
					.describe(
						`Durable Object name within this organization (defaults to "${scope.controllerDoName}" for CONTROLLER; for COMMAND use "cmd:<clusterId>:<commandId>").`,
					),
				id: z
					.string()
					.optional()
					.describe(
						"64-character hex Cloudflare Durable Object ID belonging to this organization.",
					),
				sql: z
					.string()
					.describe("Single read-only SQLite query (SELECT / WITH / EXPLAIN / read-only PRAGMA)."),
				params: z
					.array(z.union([z.string(), z.number(), z.boolean(), z.null()]))
					.optional()
					.describe("Optional positional `?` bind parameters."),
				maxRows: z
					.number()
					.int()
					.min(1)
					.max(500)
					.optional()
					.describe("Maximum rows to return (default 100, max 500)."),
			}),
			execute: async ({ binding, name, id, sql, params, maxRows }) => {
				try {
					const target = await resolveScopedInspectableDo(env, scope, {
						binding: binding as DoBindingName,
						name,
						id,
					});
					const result = await target.stub.adminQuerySql(sql, (params ?? []) as SqlParamValue[], {
						maxRows,
					});
					return {
						target: target.label,
						binding,
						resolvedBy: target.resolvedBy,
						value: target.value,
						sql,
						...result,
					};
				} catch (err) {
					return {
						binding,
						name,
						id,
						sql,
						error: err instanceof Error ? err.message : String(err),
					};
				}
			},
		}),

		get_controller_health: tool({
			description:
				"Fetch live health for this organization's ControllerDO (clusterId, orgId, cladefsTenantId, build, commit, version, connectedWorkers).",
			inputSchema: z.object({}),
			execute: async () => {
				try {
					const stub = env.CONTROLLER.getByName(scope.controllerDoName);
					return await stub.getHealth();
				} catch (err) {
					return { error: err instanceof Error ? err.message : String(err) };
				}
			},
		}),

		inspect_gc_worker: tool({
			description:
				"Inspect CladeFS R2 storage usage and GC dry-run/live status strictly for THIS organization's CladeFS tenant ID.",
			inputSchema: z.object({
				action: z.enum(["status", "dry_run_cell", "live_cell"]).describe("Inspection action."),
				cell: z.string().optional().describe("Cell ID (for dry_run_cell / live_cell)."),
			}),
			execute: async ({ action, cell }) => {
				try {
					const health = await env.CONTROLLER.getByName(scope.controllerDoName).getHealth();
					const tenantId =
						health.cladefsTenantId !== undefined && health.cladefsTenantId !== null
							? String(health.cladefsTenantId)
							: null;
					if (tenantId === null) {
						return {
							ok: false,
							error: "this organization does not have a CladeFS tenant ID assigned yet",
						};
					}

					if (action === "status") {
						const [gcHealth, usage] = await Promise.all([
							callGcWorkerAdmin(env, "/healthz"),
							callGcWorkerAdmin(env, "/usage"),
						]);
						return {
							ok: gcHealth.ok && usage.ok,
							tenantId,
							health: gcHealth.body,
							usage: usage.ok ? filterGcUsageForTenant(usage.body, tenantId) : usage.body,
						};
					}

					if (!cell) {
						return { error: "cell is required for cell inspection" };
					}
					const path =
						action === "dry_run_cell"
							? `/dry-run/${encodeURIComponent(tenantId)}/${encodeURIComponent(cell)}`
							: `/live/${encodeURIComponent(tenantId)}/${encodeURIComponent(cell)}`;
					const res = await callGcWorkerAdmin(env, path);
					return { ok: res.ok, status: res.status, tenantId, result: res.body };
				} catch (err) {
					return { error: err instanceof Error ? err.message : String(err) };
				}
			},
		}),
	};

	// Account-wide Cloudflare REST API tools span all organizations in the account,
	// so they are ONLY attached for cluster-owner / default-organization admins.
	if (scope.isGlobalAdmin) {
		tools.list_cf_do_objects = tool({
			description:
				"Use the Cloudflare Account REST API to list Durable Object namespaces or enumerate live Durable Object 64-hex IDs inside a namespace (cluster-owner only; requires CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID).",
			inputSchema: z.object({
				namespaceId: z
					.string()
					.optional()
					.describe(
						"If omitted, lists all Durable Object namespaces; if provided, lists object IDs in that namespace.",
					),
				script: z
					.string()
					.optional()
					.describe(
						"Filter namespaces by Worker script name (default 'hotbed-controller', pass '*' for all).",
					),
				limit: z.number().int().min(1).max(1000).optional(),
				cursor: z.string().optional(),
			}),
			execute: async ({ namespaceId, script, limit, cursor }) => {
				try {
					if (namespaceId) {
						return await listCfDoObjects(env, namespaceId, { limit, cursor });
					}
					return await listCfDoNamespaces(env, {
						script: script === "*" ? undefined : (script ?? "hotbed-controller"),
					});
				} catch (err) {
					return { ok: false, error: err instanceof Error ? err.message : String(err) };
				}
			},
		});

		tools.query_cf_observability = tool({
			description:
				"Query Cloudflare Workers Observability logs, invocations, and aggregate metrics for hotbed-controller or cladefs-gc (cluster-owner only; requires CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID).",
			inputSchema: z.object({
				view: z.enum(["events", "invocations", "calculations"]).default("events"),
				scriptName: z.string().optional().describe("Worker name (default 'hotbed-controller')."),
				minutesBack: z
					.number()
					.int()
					.min(1)
					.max(10080)
					.optional()
					.describe("Lookback window in minutes (default 60, max 7 days)."),
				limit: z.number().int().min(1).max(200).optional(),
				search: z
					.string()
					.optional()
					.describe("Full-text substring search across telemetry events."),
				level: z.enum(["debug", "info", "warn", "error"]).optional(),
			}),
			execute: async (input) => {
				try {
					return await queryCfObservability(env, input);
				} catch (err) {
					return { ok: false, error: err instanceof Error ? err.message : String(err) };
				}
			},
		});

		tools.cf_api_get = tool({
			description:
				"Make a read-only GET request to the Cloudflare Account REST API (`/accounts/{account_id}/<path>`, e.g. `workers/scripts`, `workers/scripts/hotbed-controller/deployments`, `r2/buckets`, `queues`). Cluster-owner only.",
			inputSchema: z.object({
				path: z.string().describe("Path relative to `/accounts/{account_id}/` (no leading slash)."),
				query: z.record(z.string(), z.union([z.string(), z.number(), z.boolean()])).optional(),
			}),
			execute: async ({ path, query }) => {
				try {
					return await cloudflareAccountGet(env, path, { query });
				} catch (err) {
					return { ok: false, error: err instanceof Error ? err.message : String(err) };
				}
			},
		});
	}

	return tools;
}

export class AdminAgentDO extends AIChatAgent<Env> {
	static options = { sendIdentityOnConnect: true };

	private ensureScopeTable(): void {
		this.ctx.storage.sql.exec(`
			CREATE TABLE IF NOT EXISTS admin_agent_scope (
				id INTEGER PRIMARY KEY CHECK (id = 1),
				controller_do_name TEXT NOT NULL,
				org_id TEXT NOT NULL,
				is_global_admin INTEGER NOT NULL
			)
		`);
	}

	/**
	 * Permanently binds this AdminAgentDO instance to a single organization scope
	 * (called by the edge router after cryptographic authentication). Refuses
	 * any attempt to rebind an existing instance to a different organization.
	 */
	bindScope(scope: AdminOrgScope): { ok: true } | { ok: false; status: 403; message: string } {
		this.ensureScopeTable();
		const rows = this.ctx.storage.sql
			.exec<{ controller_do_name: string; org_id: string; is_global_admin: number }>(
				"SELECT controller_do_name, org_id, is_global_admin FROM admin_agent_scope WHERE id = 1",
			)
			.toArray();
		if (rows.length > 0) {
			const existing = rows[0];
			if (existing.controller_do_name !== scope.controllerDoName) {
				return {
					ok: false,
					status: 403,
					message: `AdminAgentDO is permanently bound to "${existing.controller_do_name}", not "${scope.controllerDoName}"`,
				};
			}
			// Keep org_id / is_global_admin synced for the same bound controller DO.
			if (
				existing.org_id !== scope.orgId ||
				Boolean(existing.is_global_admin) !== scope.isGlobalAdmin
			) {
				this.ctx.storage.sql.exec(
					"UPDATE admin_agent_scope SET org_id = ?, is_global_admin = ? WHERE id = 1",
					scope.orgId,
					scope.isGlobalAdmin ? 1 : 0,
				);
			}
			return { ok: true };
		}
		this.ctx.storage.sql.exec(
			"INSERT INTO admin_agent_scope (id, controller_do_name, org_id, is_global_admin) VALUES (1, ?, ?, ?)",
			scope.controllerDoName,
			scope.orgId,
			scope.isGlobalAdmin ? 1 : 0,
		);
		return { ok: true };
	}

	private readBoundScope(): AdminOrgScope {
		this.ensureScopeTable();
		const rows = this.ctx.storage.sql
			.exec<{ controller_do_name: string; org_id: string; is_global_admin: number }>(
				"SELECT controller_do_name, org_id, is_global_admin FROM admin_agent_scope WHERE id = 1",
			)
			.toArray();
		if (rows.length === 0) {
			throw new AdminScopeError(
				"AdminAgentDO has no organization scope bound; refusing fail-closed",
			);
		}
		return {
			controllerDoName: String(rows[0].controller_do_name),
			orgId: String(rows[0].org_id),
			isGlobalAdmin: Number(rows[0].is_global_admin) !== 0,
		};
	}

	/** Read-only SQL access to the AdminAgentDO's own SQLite database. */
	adminQuerySql(
		query: string,
		params: SqlParamValue[] = [],
		opts?: { maxRows?: number },
	): DoSqlResult {
		return executeReadOnlySql(this.ctx.storage.sql, query, params, opts);
	}

	/** Schema inspection of the AdminAgentDO's own SQLite database. */
	adminInspectSchema(): Promise<DoSchemaInfo> {
		return inspectDoSchema(this.ctx.storage.sql, this.ctx.storage);
	}

	override async onChatMessage(
		_onFinish: GenerateTextOnFinishCallback<ToolSet>,
		options?: OnChatMessageOptions,
	): Promise<Response> {
		const controllerEnv = this.env as unknown as ControllerEnv;
		const scope = this.readBoundScope();
		const requestedModel =
			typeof options?.body?.model === "string" ? options.body.model : undefined;
		const model = resolveAdminModel(controllerEnv, requestedModel);
		const tools = createAdminAgentTools(controllerEnv, scope);
		const system = buildAdminAgentSystemPrompt({
			controllerDoName: scope.controllerDoName,
			orgId: scope.orgId,
			isGlobalAdmin: scope.isGlobalAdmin,
			cloudflareApiConfigured:
				scope.isGlobalAdmin && Boolean(getCloudflareApiConfig(controllerEnv)),
			gcWorkerConfigured: Boolean(
				(controllerEnv.GC_WORKER || (controllerEnv.GC_WORKER_URL ?? "").trim()) &&
				(controllerEnv.GC_TOKEN ?? "").trim(),
			),
		});

		const modelMessages = await convertToModelMessages(this.messages);

		const stream = createUIMessageStream({
			execute: async ({ writer }) => {
				const result = streamText({
					model,
					system,
					messages: modelMessages,
					tools,
					stopWhen: stepCountIs(12),
					abortSignal: options?.abortSignal,
				});
				writer.merge(pipeJsonRender(result.toUIMessageStream()));
			},
			onError: (err) => (err instanceof Error ? err.message : String(err)),
		});

		return createUIMessageStreamResponse({ stream });
	}
}
