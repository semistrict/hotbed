// AdminAgentDO: Cloudflare Agents SDK (AIChatAgent) Durable Object with direct
// read-only SQL & schema access to every Durable Object in the Hotbed project,
// CladeFS GC worker inspection, optional Cloudflare Account REST API / Workers
// Observability access, and Vercel json-render streaming UI specs.

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
	cloudflareAccountGet,
	getCloudflareApiConfig,
	listCfDoNamespaces,
	listCfDoObjects,
	queryCfObservability,
	resolveInspectableDo,
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

const doBindingEnum = z.enum(["CONTROLLER", "TENANT_REGISTRY", "COMMAND", "ADMIN_AGENT"]);

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

export function createAdminAgentTools(env: ControllerEnv): ToolSet {
	return {
		discover_durable_objects: tool({
			description:
				"Discover Cloudflare Durable Objects in this Hotbed deployment: TenantRegistryDO ('global'), all organization ControllerDO instances, recent CommandDO instances (with their exact `cmd:<clusterId>:<commandId>` names), and optionally live Cloudflare DO namespaces/objects when the Cloudflare REST API is configured.",
			inputSchema: z.object({
				includeRecentCommands: z
					.boolean()
					.optional()
					.describe(
						"Whether to include recent CommandDO names from each ControllerDO (default true).",
					),
				commandLimit: z
					.number()
					.int()
					.min(1)
					.max(50)
					.optional()
					.describe("Max recent commands to list per controller (default 15)."),
				includeCloudflareApi: z
					.boolean()
					.optional()
					.describe(
						"When CLOUDFLARE_API_TOKEN is set, also query Cloudflare REST API for namespaces (default true).",
					),
			}),
			execute: async ({
				includeRecentCommands = true,
				commandLimit = 15,
				includeCloudflareApi = true,
			}) => {
				const controllers: Array<{
					name: string;
					orgKey: string;
					tenantId: string | null;
					clusterId?: string;
					orgId?: string;
					connectedWorkers?: number;
					recentCommandDos?: Array<{
						commandId: string;
						doName: string;
						nodeId: string;
						sessionId: string;
						createdAt: number;
					}>;
					error?: string;
				}> = [];

				const seenNames = new Set<string>();
				const tenantMap = new Map<string, string>();

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
							seenNames.add(doName);
							tenantMap.set(doName, t.tenantId);
							controllers.push({ name: doName, orgKey: t.orgKey, tenantId: t.tenantId });
						}
					} catch (err) {
						controllers.push({
							name: "default",
							orgKey: "default",
							tenantId: null,
							error: `TenantRegistryDO lookup failed: ${err instanceof Error ? err.message : String(err)}`,
						});
						seenNames.add("default");
					}
				}

				if (!seenNames.has("default")) {
					controllers.unshift({
						name: "default",
						orgKey: "default",
						tenantId: tenantMap.get("default") ?? null,
					});
				}

				for (const ctrl of controllers) {
					try {
						const stub = env.CONTROLLER.getByName(ctrl.name);
						const health = await stub.getHealth();
						ctrl.clusterId = health.clusterId;
						ctrl.orgId = health.orgId;
						ctrl.connectedWorkers = health.connectedWorkers;
						if (includeRecentCommands && health.clusterId) {
							const sqlRes = await stub.adminQuerySql(
								`SELECT id, session_id, node_id, created_at
								 FROM commands
								 ORDER BY created_at DESC
								 LIMIT ?`,
								[commandLimit],
								{ maxRows: commandLimit },
							);
							ctrl.recentCommandDos = sqlRes.rows.map((r) => {
								const commandId = String(r.id ?? "");
								return {
									commandId,
									doName: commandDoName(health.clusterId, commandId),
									nodeId: String(r.node_id ?? ""),
									sessionId: String(r.session_id ?? ""),
									createdAt: Number(r.created_at ?? 0),
								};
							});
						}
					} catch (err) {
						ctrl.error = err instanceof Error ? err.message : String(err);
					}
				}

				let cloudflareNamespaces: unknown = null;
				const cfApiConfigured = Boolean(getCloudflareApiConfig(env));
				if (includeCloudflareApi && cfApiConfigured) {
					try {
						const nsRes = await listCfDoNamespaces(env, { script: "hotbed-controller" });
						cloudflareNamespaces = nsRes.ok ? nsRes.result : { error: nsRes.error };
					} catch (err) {
						cloudflareNamespaces = { error: err instanceof Error ? err.message : String(err) };
					}
				}

				return {
					bindings: {
						CONTROLLER:
							"One SQLite Durable Object per organization (e.g. 'default', 'org:<org_id>')",
						TENANT_REGISTRY: env.TENANT_REGISTRY
							? "Global singleton ('global') mapping orgs to CladeFS tenant IDs"
							: "not bound",
						COMMAND: "Per-command SQLite Durable Object ('cmd:<clusterId>:<commandId>')",
						ADMIN_AGENT: env.ADMIN_AGENT
							? "Per-thread Admin Agent SQLite Durable Object"
							: "not bound",
					},
					controllers,
					cloudflareApiConfigured: cfApiConfigured,
					cloudflareNamespaces,
				};
			},
		}),

		inspect_do_schema: tool({
			description:
				"Inspect the SQLite schema, tables, columns, row counts, indexes, database size in bytes, and scheduled alarm of any Cloudflare Durable Object in this project (ControllerDO, TenantRegistryDO, CommandDO, or AdminAgentDO). Address the object by either `name` or 64-hex `id`.",
			inputSchema: z.object({
				binding: doBindingEnum.describe("Which Durable Object binding to inspect."),
				name: z
					.string()
					.optional()
					.describe(
						"Durable Object name (e.g. 'default', 'global', 'cmd:<clusterId>:<commandId>').",
					),
				id: z
					.string()
					.optional()
					.describe(
						"64-character hex Cloudflare Durable Object ID (mutually exclusive with `name`).",
					),
			}),
			execute: async ({ binding, name, id }) => {
				try {
					const target = resolveInspectableDo(env, { binding: binding as DoBindingName, name, id });
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
				"Execute a read-only SQLite query (SELECT, WITH ... SELECT, EXPLAIN, or read-only PRAGMA) directly on any Cloudflare Durable Object in this project (ControllerDO, TenantRegistryDO, CommandDO, or AdminAgentDO) addressed by `name` or 64-hex `id`.",
			inputSchema: z.object({
				binding: doBindingEnum.describe("Which Durable Object binding to query."),
				name: z
					.string()
					.optional()
					.describe(
						"Durable Object name (e.g. 'default', 'org:<org_id>', 'global', 'cmd:<clusterId>:<commandId>').",
					),
				id: z
					.string()
					.optional()
					.describe(
						"64-character hex Cloudflare Durable Object ID (mutually exclusive with `name`).",
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
					const target = resolveInspectableDo(env, { binding: binding as DoBindingName, name, id });
					const result = await target.stub.adminQuerySql(sql, params ?? [], { maxRows });
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
				"Fetch live ControllerDO health (clusterId, orgId, cladefsTenantId, build, commit, version, connectedWorkers).",
			inputSchema: z.object({
				name: z.string().optional().describe("ControllerDO name (default 'default')."),
				id: z.string().optional().describe("Optional 64-hex ControllerDO ID."),
			}),
			execute: async ({ name, id }) => {
				try {
					const rawId = (id ?? "").trim();
					const stub = rawId
						? env.CONTROLLER.get(env.CONTROLLER.idFromString(rawId))
						: env.CONTROLLER.getByName((name ?? "").trim() || "default");
					return await stub.getHealth();
				} catch (err) {
					return { error: err instanceof Error ? err.message : String(err) };
				}
			},
		}),

		inspect_gc_worker: tool({
			description:
				"Inspect the bound CladeFS garbage-collector Cloudflare Worker (`cladefs-gc`): check health, R2 cell storage usage across tenants, or dry-run/live status of a specific cell.",
			inputSchema: z.object({
				action: z.enum(["status", "dry_run_cell", "live_cell"]).describe("Inspection action."),
				tenant: z.string().optional().describe("Tenant ID (for dry_run_cell / live_cell)."),
				cell: z.string().optional().describe("Cell ID (for dry_run_cell / live_cell)."),
			}),
			execute: async ({ action, tenant, cell }) => {
				try {
					if (action === "status") {
						const [health, usage] = await Promise.all([
							callGcWorkerAdmin(env, "/healthz"),
							callGcWorkerAdmin(env, "/usage"),
						]);
						return { health: health.body, usage: usage.body, ok: health.ok && usage.ok };
					}
					if (!tenant || !cell) {
						return { error: "tenant and cell are required for cell inspection" };
					}
					const path =
						action === "dry_run_cell"
							? `/dry-run/${encodeURIComponent(tenant)}/${encodeURIComponent(cell)}`
							: `/live/${encodeURIComponent(tenant)}/${encodeURIComponent(cell)}`;
					const res = await callGcWorkerAdmin(env, path);
					return { ok: res.ok, status: res.status, result: res.body };
				} catch (err) {
					return { error: err instanceof Error ? err.message : String(err) };
				}
			},
		}),

		list_cf_do_objects: tool({
			description:
				"Use the Cloudflare Account REST API to list Durable Object namespaces or enumerate live Durable Object 64-hex IDs inside a namespace (requires CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID secrets). Returned 64-hex IDs can be queried directly with `run_do_sql` or `inspect_do_schema`.",
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
		}),

		query_cf_observability: tool({
			description:
				"Query Cloudflare Workers Observability logs, invocations, and aggregate metrics for hotbed-controller or cladefs-gc (requires CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID secrets).",
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
		}),

		cf_api_get: tool({
			description:
				"Make a read-only GET request to the Cloudflare Account REST API (`/accounts/{account_id}/<path>`, e.g. `workers/scripts`, `workers/scripts/hotbed-controller/deployments`, `r2/buckets`, `queues`). Requires CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID secrets.",
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
		}),
	};
}

export class AdminAgentDO extends AIChatAgent<Env> {
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
		const requestedModel =
			typeof options?.body?.model === "string" ? options.body.model : undefined;
		const model = resolveAdminModel(controllerEnv, requestedModel);
		const tools = createAdminAgentTools(controllerEnv);
		const system = buildAdminAgentSystemPrompt({
			cloudflareApiConfigured: Boolean(getCloudflareApiConfig(controllerEnv)),
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
		});

		return createUIMessageStreamResponse({ stream });
	}
}
