// Cloudflare REST API v4 client (read-only) and org-scoped Durable Object
// target resolver for the Hotbed Admin Agent.
//
// Security invariant: every organization's Admin Agent is strictly confined to
// THAT organization's Durable Objects (its own ControllerDO, CommandDOs under
// its own clusterId, and its own AdminAgentDO threads). Non-default orgs never
// get access to TenantRegistryDO or account-wide Cloudflare REST APIs.

import type { AdminAgentDO } from "./admin-agent-do.ts";
import { commandDoName, type CommandDO } from "./command-do.ts";
import type { ControllerDO, ControllerEnv } from "./controller-do.ts";
import type { DoSchemaInfo, DoSqlResult, SqlParamValue } from "./do-sql.ts";
import type { TenantRegistryDO } from "./tenant-registry-do.ts";
import { workerDoName, type WorkerDO } from "./worker-do.ts";

const CF_API_BASE = "https://api.cloudflare.com/client/v4";
const HEX_DO_ID_RE = /^[0-9a-f]{64}$/i;

export type DoBindingName = "CONTROLLER" | "TENANT_REGISTRY" | "COMMAND" | "WORKER" | "ADMIN_AGENT";

export interface AdminOrgScope {
	/** Logical ControllerDO name for this organization ("default" or "org:<org_id>"). */
	controllerDoName: string;
	/** WorkOS organization ID, or "" for single-tenant/default mode. */
	orgId: string;
	/** Whether the caller is a cluster-level/default-org admin. */
	isGlobalAdmin: boolean;
}

export class AdminScopeError extends Error {
	readonly status = 403;
	constructor(message: string) {
		super(message);
		this.name = "AdminScopeError";
	}
}

export interface DoTargetInput {
	binding: DoBindingName;
	/** Logical Durable Object name passed to `getByName(name)`. */
	name?: string;
	/** 64-character hex Cloudflare Durable Object ID passed to `idFromString(id)`. */
	id?: string;
}

export interface AdminInspectableStub {
	adminQuerySql(
		query: string,
		params?: SqlParamValue[],
		opts?: { maxRows?: number },
	): Promise<DoSqlResult>;
	adminInspectSchema(): Promise<
		DoSchemaInfo & { clusterId?: string; orgId?: string; cladefsTenantId?: string | null }
	>;
}

export interface CloudflareApiConfig {
	accountId: string;
	apiToken: string;
}

export function getCloudflareApiConfig(env: ControllerEnv): CloudflareApiConfig | null {
	const accountId = (env.CLOUDFLARE_ACCOUNT_ID ?? "").trim();
	const apiToken = (env.CLOUDFLARE_API_TOKEN ?? "").trim();
	if (!accountId || !apiToken) return null;
	return { accountId, apiToken };
}

export function adminAgentDoName(controllerDoName: string, threadId: string): string {
	return `admin:${controllerDoName}:${threadId}`;
}

export function allowedBindingsForScope(scope: AdminOrgScope): DoBindingName[] {
	return scope.isGlobalAdmin
		? ["CONTROLLER", "COMMAND", "WORKER", "ADMIN_AGENT", "TENANT_REGISTRY"]
		: ["CONTROLLER", "COMMAND", "WORKER", "ADMIN_AGENT"];
}

function assertValidHexId(rawId: string): string {
	const cleaned = rawId.trim().toLowerCase();
	if (!HEX_DO_ID_RE.test(cleaned)) {
		throw new Error(
			`invalid Durable Object ID "${rawId}": expected a 64-character hexadecimal string`,
		);
	}
	return cleaned;
}

/**
 * Verifies that a 64-hex-char CommandDO ID corresponds to a command recorded in
 * the scoped organization's ControllerDO (`commands` or `history` table).
 */
async function verifyCommandHexIdBelongsToOrg(
	env: ControllerEnv,
	controllerStub: DurableObjectStub<ControllerDO>,
	clusterId: string,
	hexId: string,
): Promise<boolean> {
	const [cmdRows, histRows] = await Promise.all([
		controllerStub.adminQuerySql("SELECT id FROM commands ORDER BY created_at DESC LIMIT 500", [], {
			maxRows: 500,
		}),
		controllerStub.adminQuerySql("SELECT id FROM history ORDER BY ended_at DESC LIMIT 500", [], {
			maxRows: 500,
		}),
	]);
	const seen = new Set<string>();
	for (const row of [...cmdRows.rows, ...histRows.rows]) {
		const cmdId = String(row.id ?? "");
		if (!cmdId || seen.has(cmdId)) continue;
		seen.add(cmdId);
		const candidate = env.COMMAND.idFromName(commandDoName(clusterId, cmdId))
			.toString()
			.toLowerCase();
		if (candidate === hexId) return true;
	}
	return false;
}

async function verifyWorkerHexIdBelongsToOrg(
	env: ControllerEnv,
	controllerStub: DurableObjectStub<ControllerDO>,
	clusterId: string,
	hexId: string,
): Promise<boolean> {
	const nodeRows = await controllerStub.adminQuerySql("SELECT id FROM nodes LIMIT 500", [], {
		maxRows: 500,
	});
	for (const row of nodeRows.rows) {
		const nodeId = String(row.id ?? "");
		if (!nodeId) continue;
		const candidate = env.WORKER.idFromName(workerDoName(clusterId, nodeId))
			.toString()
			.toLowerCase();
		if (candidate === hexId) return true;
	}
	return false;
}

/**
 * Resolves a Durable Object target strictly within the caller's organization
 * scope. Throws AdminScopeError (403) if the target belongs to another org or
 * is outside the caller's allowed bindings.
 */
export async function resolveScopedInspectableDo(
	env: ControllerEnv,
	scope: AdminOrgScope,
	target: DoTargetInput,
): Promise<{
	stub: AdminInspectableStub;
	label: string;
	resolvedBy: "name" | "id";
	value: string;
}> {
	const rawId = (target.id ?? "").trim();
	const rawName = (target.name ?? "").trim();

	if (rawId && rawName) {
		throw new Error("specify either `name` or `id` for a Durable Object target, not both");
	}

	switch (target.binding) {
		case "CONTROLLER": {
			const expectedName = scope.controllerDoName;
			if (rawId) {
				const hexId = assertValidHexId(rawId);
				const expectedHex = env.CONTROLLER.idFromName(expectedName).toString().toLowerCase();
				if (hexId !== expectedHex) {
					throw new AdminScopeError(
						`access denied: ControllerDO ID ${hexId.slice(0, 12)}… does not belong to this organization (${expectedName})`,
					);
				}
				return {
					stub: env.CONTROLLER.get(
						env.CONTROLLER.idFromString(hexId),
					) as unknown as AdminInspectableStub,
					label: `CONTROLLER(id:${hexId.slice(0, 12)}…)`,
					resolvedBy: "id",
					value: hexId,
				};
			}
			const resolvedName = rawName || expectedName;
			if (resolvedName !== expectedName) {
				throw new AdminScopeError(
					`access denied: this organization's Admin Agent may only query ControllerDO "${expectedName}", not "${resolvedName}"`,
				);
			}
			return {
				stub: env.CONTROLLER.getByName(expectedName) as unknown as AdminInspectableStub,
				label: `CONTROLLER(${expectedName})`,
				resolvedBy: "name",
				value: expectedName,
			};
		}

		case "COMMAND": {
			const ctrlStub = env.CONTROLLER.getByName(scope.controllerDoName);
			const health = await ctrlStub.getHealth();
			const clusterId = health.clusterId;
			if (!clusterId) {
				throw new AdminScopeError("organization controller has no clusterId yet");
			}
			const prefix = `cmd:${clusterId}:`;

			if (rawId) {
				const hexId = assertValidHexId(rawId);
				const owned = await verifyCommandHexIdBelongsToOrg(env, ctrlStub, clusterId, hexId);
				if (!owned) {
					throw new AdminScopeError(
						`access denied: CommandDO ID ${hexId.slice(0, 12)}… does not match any command in this organization (${scope.controllerDoName})`,
					);
				}
				return {
					stub: env.COMMAND.get(env.COMMAND.idFromString(hexId)) as unknown as AdminInspectableStub,
					label: `COMMAND(id:${hexId.slice(0, 12)}…)`,
					resolvedBy: "id",
					value: hexId,
				};
			}

			if (!rawName) {
				throw new Error(`\`name\` is required for COMMAND (expected "${prefix}<commandId>")`);
			}
			if (!rawName.startsWith(prefix) || rawName.length <= prefix.length) {
				throw new AdminScopeError(
					`access denied: CommandDO name "${rawName}" is outside this organization's cluster prefix ("${prefix}")`,
				);
			}
			return {
				stub: env.COMMAND.getByName(rawName) as unknown as AdminInspectableStub,
				label: `COMMAND(${rawName})`,
				resolvedBy: "name",
				value: rawName,
			};
		}

		case "WORKER": {
			const ctrlStub = env.CONTROLLER.getByName(scope.controllerDoName);
			const health = await ctrlStub.getHealth();
			const clusterId = health.clusterId;
			if (!clusterId) {
				throw new AdminScopeError("organization controller has no clusterId yet");
			}
			const prefix = `worker:${clusterId}:`;

			if (rawId) {
				const hexId = assertValidHexId(rawId);
				const owned = await verifyWorkerHexIdBelongsToOrg(env, ctrlStub, clusterId, hexId);
				if (!owned) {
					throw new AdminScopeError(
						`access denied: WorkerDO ID ${hexId.slice(0, 12)}… does not match any worker in this organization (${scope.controllerDoName})`,
					);
				}
				return {
					stub: env.WORKER.get(env.WORKER.idFromString(hexId)) as unknown as AdminInspectableStub,
					label: `WORKER(id:${hexId.slice(0, 12)}…)`,
					resolvedBy: "id",
					value: hexId,
				};
			}

			if (!rawName) {
				throw new Error(`\`name\` is required for WORKER (expected "${prefix}<nodeId>")`);
			}
			if (!rawName.startsWith(prefix) || rawName.length <= prefix.length) {
				throw new AdminScopeError(
					`access denied: WorkerDO name "${rawName}" is outside this organization's cluster prefix ("${prefix}")`,
				);
			}
			return {
				stub: env.WORKER.getByName(rawName) as unknown as AdminInspectableStub,
				label: `WORKER(${rawName})`,
				resolvedBy: "name",
				value: rawName,
			};
		}

		case "ADMIN_AGENT": {
			if (!env.ADMIN_AGENT) {
				throw new Error("Durable Object binding ADMIN_AGENT is not bound in this environment");
			}
			const prefix = `admin:${scope.controllerDoName}:`;
			if (rawId) {
				throw new AdminScopeError(
					`address AdminAgentDO by its organization-scoped name ("${prefix}<thread>") rather than a raw hex ID`,
				);
			}
			const resolvedName = rawName || `${prefix}main`;
			if (!resolvedName.startsWith(prefix) || resolvedName.length <= prefix.length) {
				throw new AdminScopeError(
					`access denied: AdminAgentDO name "${resolvedName}" is outside this organization's namespace ("${prefix}*")`,
				);
			}
			return {
				stub: env.ADMIN_AGENT.getByName(resolvedName) as unknown as AdminInspectableStub,
				label: `ADMIN_AGENT(${resolvedName})`,
				resolvedBy: "name",
				value: resolvedName,
			};
		}

		case "TENANT_REGISTRY": {
			if (!scope.isGlobalAdmin) {
				throw new AdminScopeError(
					"access denied: TenantRegistryDO is global across organizations and is not accessible to organization-scoped Admin Agents",
				);
			}
			if (!env.TENANT_REGISTRY) {
				throw new Error("Durable Object binding TENANT_REGISTRY is not bound in this environment");
			}
			if (rawId) {
				const hexId = assertValidHexId(rawId);
				const expectedHex = env.TENANT_REGISTRY.idFromName("global").toString().toLowerCase();
				if (hexId !== expectedHex) {
					throw new AdminScopeError("TenantRegistryDO only permits the global singleton object");
				}
				return {
					stub: env.TENANT_REGISTRY.get(
						env.TENANT_REGISTRY.idFromString(hexId),
					) as unknown as AdminInspectableStub,
					label: `TENANT_REGISTRY(id:${hexId.slice(0, 12)}…)`,
					resolvedBy: "id",
					value: hexId,
				};
			}
			const resolvedName = rawName || "global";
			if (resolvedName !== "global") {
				throw new AdminScopeError('TenantRegistryDO only permits the "global" singleton');
			}
			return {
				stub: env.TENANT_REGISTRY.getByName("global") as unknown as AdminInspectableStub,
				label: "TENANT_REGISTRY(global)",
				resolvedBy: "name",
				value: "global",
			};
		}

		default:
			throw new Error(`unknown Durable Object binding "${String(target.binding)}"`);
	}
}

// Suppress unused type-only import lint when referencing AdminAgentDO / CommandDO / WorkerDO / TenantRegistryDO.
export type _ScopedDoClasses =
	| ControllerDO
	| TenantRegistryDO
	| CommandDO
	| WorkerDO
	| AdminAgentDO;

// ---- Cloudflare REST API v4 (optional, cluster-owner/default-org admins only) ----

interface CfApiEnvelope<T> {
	success?: boolean;
	result?: T;
	result_info?: Record<string, unknown>;
	errors?: Array<{ code?: number; message?: string }>;
	messages?: Array<{ code?: number; message?: string }>;
}

export interface CfApiFetchResult<T = unknown> {
	ok: boolean;
	status: number;
	result?: T;
	resultInfo?: Record<string, unknown>;
	error?: string;
}

export type CfFetch = (input: string, init?: RequestInit) => Promise<Response>;

const SAFE_CF_ACCOUNT_PATH_RE = /^[a-zA-Z0-9_./:-]+$/;

function requireCfApi(env: ControllerEnv): CloudflareApiConfig {
	const cfg = getCloudflareApiConfig(env);
	if (!cfg) {
		throw new Error(
			"Cloudflare REST API credentials are not configured on this Worker. Set CLOUDFLARE_ACCOUNT_ID and CLOUDFLARE_API_TOKEN secrets (`cf workers secrets update --worker hotbed-controller --name CLOUDFLARE_API_TOKEN --type secret_text`). In-cluster Durable Object SQL and schema inspection still work without them.",
		);
	}
	return cfg;
}

async function cfApiRequest<T>(
	cfg: CloudflareApiConfig,
	method: "GET" | "POST",
	accountRelativePath: string,
	opts?: {
		query?: Record<string, string | number | boolean | undefined>;
		body?: unknown;
		fetchImpl?: CfFetch;
	},
): Promise<CfApiFetchResult<T>> {
	const cleaned = accountRelativePath.replace(/^\/+/, "");
	if (!cleaned || cleaned.includes("..") || !SAFE_CF_ACCOUNT_PATH_RE.test(cleaned)) {
		throw new Error(`invalid Cloudflare API path "${accountRelativePath}"`);
	}

	const url = new URL(`${CF_API_BASE}/accounts/${encodeURIComponent(cfg.accountId)}/${cleaned}`);
	if (opts?.query) {
		for (const [k, v] of Object.entries(opts.query)) {
			if (v !== undefined && v !== "") {
				url.searchParams.set(k, String(v));
			}
		}
	}

	const doFetch = opts?.fetchImpl ?? ((input, init) => fetch(input, init));
	const res = await doFetch(url.toString(), {
		method,
		headers: {
			Authorization: `Bearer ${cfg.apiToken}`,
			Accept: "application/json",
			...(opts?.body !== undefined ? { "Content-Type": "application/json" } : {}),
		},
		body: opts?.body !== undefined ? JSON.stringify(opts.body) : undefined,
	});

	const text = await res.text();
	let parsed: CfApiEnvelope<T> | null = null;
	try {
		parsed = text ? (JSON.parse(text) as CfApiEnvelope<T>) : null;
	} catch {
		parsed = null;
	}

	if (!res.ok || (parsed && parsed.success === false)) {
		const errMsg =
			parsed?.errors
				?.map((e) => (e.code ? `[${e.code}] ${e.message}` : e.message))
				.filter(Boolean)
				.join("; ") ||
			text.slice(0, 500) ||
			`Cloudflare API returned HTTP ${res.status}`;
		return { ok: false, status: res.status, error: errMsg };
	}

	return {
		ok: true,
		status: res.status,
		result: parsed?.result,
		resultInfo: parsed?.result_info,
	};
}

export interface CfDoNamespace {
	id: string;
	name: string;
	script: string;
	class: string;
	use_sqlite?: boolean;
}

export interface CfDoObjectSummary {
	id: string;
	hasStoredData: boolean;
}

export async function listCfDoNamespaces(
	env: ControllerEnv,
	opts?: { script?: string; fetchImpl?: CfFetch },
): Promise<CfApiFetchResult<CfDoNamespace[]>> {
	const cfg = requireCfApi(env);
	const res = await cfApiRequest<CfDoNamespace[]>(
		cfg,
		"GET",
		"workers/durable_objects/namespaces",
		{
			query: { per_page: 100 },
			fetchImpl: opts?.fetchImpl,
		},
	);
	if (!res.ok || !res.result) return res;
	const filter = (opts?.script ?? "").trim();
	const filtered = filter
		? res.result.filter((ns) => ns.script === filter || ns.name.startsWith(`${filter}_`))
		: res.result;
	return { ...res, result: filtered };
}

export async function listCfDoObjects(
	env: ControllerEnv,
	namespaceId: string,
	opts?: { limit?: number; cursor?: string; fetchImpl?: CfFetch },
): Promise<CfApiFetchResult<CfDoObjectSummary[]>> {
	const cfg = requireCfApi(env);
	const id = namespaceId.trim();
	if (!/^[a-zA-Z0-9_-]+$/.test(id)) {
		throw new Error(`invalid Durable Object namespace ID "${namespaceId}"`);
	}
	const limit = Math.max(1, Math.min(1000, Math.floor(opts?.limit ?? 100)));
	return cfApiRequest<CfDoObjectSummary[]>(
		cfg,
		"GET",
		`workers/durable_objects/namespaces/${encodeURIComponent(id)}/objects`,
		{
			query: { limit, cursor: opts?.cursor },
			fetchImpl: opts?.fetchImpl,
		},
	);
}

export async function cloudflareAccountGet(
	env: ControllerEnv,
	path: string,
	opts?: { query?: Record<string, string | number | boolean | undefined>; fetchImpl?: CfFetch },
): Promise<CfApiFetchResult<unknown>> {
	const cfg = requireCfApi(env);
	return cfApiRequest<unknown>(cfg, "GET", path, opts);
}

export interface CfObservabilityQueryInput {
	view: "events" | "invocations" | "calculations";
	scriptName?: string;
	minutesBack?: number;
	limit?: number;
	search?: string;
	level?: string;
	filters?: Array<{
		key: string;
		operation: string;
		type: "string" | "number" | "boolean";
		value?: string | number | boolean;
	}>;
	calculations?: Array<{ operator: string; key?: string; keyType?: string; alias?: string }>;
	groupBys?: Array<{ type: "string" | "number" | "boolean"; value: string }>;
}

export async function queryCfObservability(
	env: ControllerEnv,
	input: CfObservabilityQueryInput,
	fetchImpl?: CfFetch,
): Promise<CfApiFetchResult<unknown>> {
	const cfg = requireCfApi(env);
	const nowMs = Date.now();
	const minutesBack = Math.max(1, Math.min(7 * 24 * 60, Math.floor(input.minutesBack ?? 60)));
	const fromMs = nowMs - minutesBack * 60_000;
	const scriptName = (input.scriptName ?? "hotbed-controller").trim();

	const filters: Array<Record<string, unknown>> = [];
	if (scriptName) {
		filters.push({
			key: "$metadata.service",
			operation: "eq",
			type: "string",
			value: scriptName,
		});
	}
	if (input.level) {
		filters.push({
			key: "$metadata.level",
			operation: "eq",
			type: "string",
			value: input.level,
		});
	}
	for (const f of input.filters ?? []) {
		filters.push({
			key: f.key,
			operation: f.operation,
			type: f.type,
			...(f.value !== undefined ? { value: f.value } : {}),
		});
	}

	const parameters: Record<string, unknown> = {
		datasets: ["cloudflare-workers"],
		filters,
		...(input.search ? { needle: { value: input.search, matchCase: false } } : {}),
		...(input.calculations ? { calculations: input.calculations } : {}),
		...(input.groupBys ? { groupBys: input.groupBys } : {}),
	};

	const body = {
		queryId: `hotbed-admin-${Date.now()}`,
		timeframe: { from: fromMs, to: nowMs },
		view: input.view,
		limit: Math.max(1, Math.min(200, Math.floor(input.limit ?? 50))),
		parameters,
	};

	return cfApiRequest<unknown>(cfg, "POST", "workers/observability/telemetry/query", {
		body,
		fetchImpl,
	});
}
