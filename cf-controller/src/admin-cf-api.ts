// Cloudflare REST API v4 client (read-only) and Durable Object target resolver
// for the Hotbed Admin Agent.
//
// In-cluster Durable Object SQL and schema inspection work directly via typed
// Workers RPC bindings (CONTROLLER, TENANT_REGISTRY, COMMAND, ADMIN_AGENT) with
// zero external credentials. When CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID
// are configured on the Worker, this module additionally enables account-level
// Cloudflare REST API inspection (listing all DO namespaces and object IDs,
// Workers deployments, and Workers Observability telemetry).

import type { AdminAgentDO } from "./admin-agent-do.ts";
import type { CommandDO } from "./command-do.ts";
import type { ControllerDO, ControllerEnv } from "./controller-do.ts";
import type { DoSchemaInfo, DoSqlResult, SqlParamValue } from "./do-sql.ts";
import type { TenantRegistryDO } from "./tenant-registry-do.ts";

const CF_API_BASE = "https://api.cloudflare.com/client/v4";
const HEX_DO_ID_RE = /^[0-9a-f]{64}$/i;

export type DoBindingName = "CONTROLLER" | "TENANT_REGISTRY" | "COMMAND" | "ADMIN_AGENT";

export interface DoTargetInput {
	binding: DoBindingName;
	/** Logical Durable Object name passed to `getByName(name)` (e.g. "default", "global", "cmd:<clusterId>:<cmdId>"). */
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

export function describeTarget(target: DoTargetInput): string {
	if (target.id?.trim()) return `${target.binding}[id:${target.id.trim().slice(0, 12)}…]`;
	return `${target.binding}[name:${(target.name ?? "").trim() || "(default)"}]`;
}

function getTargetStub<T extends ControllerDO | TenantRegistryDO | CommandDO | AdminAgentDO>(
	ns: DurableObjectNamespace<T>,
	target: DoTargetInput,
): { stub: AdminInspectableStub; label: string; resolvedBy: "name" | "id"; value: string } {
	const rawId = (target.id ?? "").trim();
	const rawName = (target.name ?? "").trim();

	if (rawId && rawName) {
		throw new Error("specify either `name` or `id` for a Durable Object target, not both");
	}

	if (rawId) {
		if (!HEX_DO_ID_RE.test(rawId)) {
			throw new Error(
				`invalid Durable Object ID "${rawId}": expected a 64-character hexadecimal string`,
			);
		}
		const doId = ns.idFromString(rawId.toLowerCase());
		const rawStub = ns.get(doId);
		return {
			stub: rawStub as unknown as AdminInspectableStub,
			label: `${target.binding}(id:${rawId.slice(0, 12)}…)`,
			resolvedBy: "id",
			value: rawId.toLowerCase(),
		};
	}

	const defaultName =
		target.binding === "TENANT_REGISTRY"
			? "global"
			: target.binding === "CONTROLLER"
				? "default"
				: "";
	const resolvedName = rawName || defaultName;
	if (!resolvedName) {
		throw new Error(
			`\`name\` (or 64-hex \`id\`) is required for ${target.binding} (e.g. "cmd:<clusterId>:<commandId>")`,
		);
	}

	const rawStub = ns.getByName(resolvedName);
	return {
		stub: rawStub as unknown as AdminInspectableStub,
		label: `${target.binding}(${resolvedName})`,
		resolvedBy: "name",
		value: resolvedName,
	};
}

/**
 * Resolves a Durable Object binding + name or 64-hex-char ID to a typed RPC stub.
 */
export function resolveInspectableDo(
	env: ControllerEnv,
	target: DoTargetInput,
): { stub: AdminInspectableStub; label: string; resolvedBy: "name" | "id"; value: string } {
	switch (target.binding) {
		case "CONTROLLER":
			return getTargetStub(env.CONTROLLER, target);
		case "TENANT_REGISTRY":
			if (!env.TENANT_REGISTRY) {
				throw new Error("Durable Object binding TENANT_REGISTRY is not bound in this environment");
			}
			return getTargetStub(env.TENANT_REGISTRY, target);
		case "COMMAND":
			return getTargetStub(env.COMMAND, target);
		case "ADMIN_AGENT":
			if (!env.ADMIN_AGENT) {
				throw new Error("Durable Object binding ADMIN_AGENT is not bound in this environment");
			}
			return getTargetStub(env.ADMIN_AGENT, target);
		default:
			throw new Error(`unknown Durable Object binding "${String(target.binding)}"`);
	}
}

// ---- Cloudflare REST API v4 (optional, requires CLOUDFLARE_API_TOKEN + CLOUDFLARE_ACCOUNT_ID) ----

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

/**
 * Queries Cloudflare Workers Observability telemetry for hotbed-controller (or
 * another script in the account), modeled after Cloudflare OS's observability client.
 */
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
