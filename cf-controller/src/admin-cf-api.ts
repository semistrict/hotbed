// Org-scoped Durable Object target resolver and tenant-scoped CladeFS GC
// inspector for the Hotbed Admin Explorer (UI SQL Explorer + `hotbed admin` CLI).
//
// Security invariant: EVERY organization (including the default organization)
// is strictly confined to THAT organization's own ControllerDO, its own
// CommandDOs (`cmd:<clusterId>:*`), and its own CladeFS tenant R2 usage.
// Global multi-tenant objects (TenantRegistryDO) and account-wide Cloudflare
// APIs are NEVER accessible here.

import { commandDoName } from "./command-do.ts";
import type { ControllerDO, ControllerEnv } from "./controller-do.ts";
import type { DoSchemaInfo, DoSqlRpcResult, SqlParamValue } from "./do-sql.ts";
import { workerDoName, type WorkerDO } from "./worker-do.ts";

const HEX_DO_ID_RE = /^[0-9a-f]{64}$/i;
const GC_CELL_SEGMENT_RE = /^[0-9a-hjkmnp-tv-z]{1,16}$/i;
const CROCKFORD_DIGITS = "0123456789abcdefghjkmnpqrstvwxyz";

/**
 * Encodes a non-negative uint64 CladeFS tenant ID as lowercase unpadded
 * Crockford base32 (`base32_id::Short` in cladefs-gc, matching `Base32ID`
 * in `internal/cladefs/client.go`). Example: 0 -> "0", 10 -> "a", 32 -> "10".
 */
export function encodeCrockfordBase32Short(id: number | bigint): string {
	let n = typeof id === "bigint" ? id : BigInt(Math.trunc(id));
	if (n < 0n) {
		throw new Error(`invalid negative CladeFS tenant ID: ${id}`);
	}
	if (n === 0n) return "0";
	let out = "";
	while (n > 0n) {
		out = CROCKFORD_DIGITS[Number(n % 32n)] + out;
		n /= 32n;
	}
	return out;
}

/**
 * Normalizes a Crockford base32 tenant string (padded or unpadded, case-
 * insensitive, supporting i/l -> 1 and o -> 0) to its canonical Short form.
 */
export function normalizeCrockfordBase32Short(raw: string): string | null {
	const cleaned = raw.trim().toLowerCase();
	if (!cleaned) return null;
	let normalized = "";
	for (const ch of cleaned) {
		const mapped = ch === "i" || ch === "l" ? "1" : ch === "o" ? "0" : ch;
		if (!CROCKFORD_DIGITS.includes(mapped)) return null;
		normalized += mapped;
	}
	const stripped = normalized.replace(/^0+/, "");
	return stripped || "0";
}

export type DoBindingName = "CONTROLLER" | "COMMAND" | "WORKER";

export interface AdminOrgScope {
	/** Logical ControllerDO name for this organization ("default" or "org:<org_id>"). */
	controllerDoName: string;
	/** WorkOS organization ID, or "" for single-tenant/default mode. */
	orgId: string;
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
	): Promise<DoSqlRpcResult>;
	adminInspectSchema(): Promise<
		DoSchemaInfo & { clusterId?: string; orgId?: string; cladefsTenantId?: string | null }
	>;
}

export function allowedBindingsForScope(): DoBindingName[] {
	return ["CONTROLLER", "COMMAND", "WORKER"];
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
	const [cmdRpc, histRpc] = await Promise.all([
		controllerStub.adminQuerySql("SELECT id FROM commands ORDER BY created_at DESC LIMIT 500", [], {
			maxRows: 500,
		}),
		controllerStub.adminQuerySql("SELECT id FROM history ORDER BY ended_at DESC LIMIT 500", [], {
			maxRows: 500,
		}),
	]);
	const cmdRows = cmdRpc.ok ? cmdRpc.result.rows : [];
	const histRows = histRpc.ok ? histRpc.result.rows : [];
	const seen = new Set<string>();
	for (const row of [...cmdRows, ...histRows]) {
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
	const nodeRpc = await controllerStub.adminQuerySql("SELECT id FROM nodes LIMIT 500", [], {
		maxRows: 500,
	});
	const nodeRows = nodeRpc.ok ? nodeRpc.result.rows : [];
	for (const row of nodeRows) {
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
 * is a global multi-tenant Durable Object.
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
					`access denied: this organization's Admin Explorer may only query ControllerDO "${expectedName}", not "${resolvedName}"`,
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
		default:
			throw new AdminScopeError(
				`access denied: Durable Object binding "${String(target.binding)}" is not accessible from Admin Explorer`,
			);
	}
}

// Suppress unused type-only import lint when referencing WorkerDO / ControllerDO.
export type _ScopedDoClasses = ControllerDO | WorkerDO;

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
 * Filters a cladefs-gc `/usage` payload to ONLY cells belonging to `tenantB32`
 * (Crockford base32 Short tenant ID), recomputing summary counts so an org
 * never sees another org's R2 storage.
 */
export function filterGcUsageForTenant(usageBody: unknown, tenantB32: string): unknown {
	if (!usageBody || typeof usageBody !== "object") return usageBody;
	const canonicalTarget = normalizeCrockfordBase32Short(tenantB32);
	if (!canonicalTarget) {
		return {
			...(usageBody as Record<string, unknown>),
			tenantFilter: tenantB32,
			cells: 0,
			measuredCells: 0,
			totalObjects: 0,
			totalBytes: 0,
			usage: [],
		};
	}
	const data = usageBody as { usage?: unknown[] } & Record<string, unknown>;
	const all = Array.isArray(data.usage) ? (data.usage as GcUsageCell[]) : [];
	const scoped = all.filter((u) => {
		const raw = u.tenant === undefined || u.tenant === null ? "" : String(u.tenant);
		return normalizeCrockfordBase32Short(raw) === canonicalTarget;
	});
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
		tenantFilter: canonicalTarget,
		cells: scoped.length,
		measuredCells,
		totalObjects,
		totalBytes,
		usage: scoped,
	};
}

export async function inspectGcForScope(
	env: ControllerEnv,
	scope: AdminOrgScope,
	input: { action: "status" | "dry_run_cell" | "live_cell"; cell?: string },
): Promise<{ ok: boolean; status: number; body: Record<string, unknown> }> {
	const token = (env.GC_TOKEN ?? "").trim();
	const workerUrl = (env.GC_WORKER_URL ?? "").trim();
	const hasBinding = Boolean(env.GC_WORKER && typeof env.GC_WORKER.fetch === "function");
	if ((!hasBinding && !workerUrl) || !token) {
		return {
			ok: false,
			status: 503,
			body: {
				ok: false,
				error: "CladeFS GC worker is not configured on this controller",
			},
		};
	}

	const health = await env.CONTROLLER.getByName(scope.controllerDoName).getHealth();
	const tenantNumeric =
		health.cladefsTenantId !== undefined && health.cladefsTenantId !== null
			? health.cladefsTenantId
			: null;
	if (tenantNumeric === null) {
		return {
			ok: false,
			status: 400,
			body: {
				ok: false,
				error: "this organization does not have a CladeFS tenant ID assigned yet",
			},
		};
	}
	const tenantB32 = encodeCrockfordBase32Short(tenantNumeric);

	if (input.action === "status") {
		const [gcHealth, usage] = await Promise.all([
			callGcWorkerAdmin(env, "/healthz"),
			callGcWorkerAdmin(env, "/usage"),
		]);
		const ok = gcHealth.ok && usage.ok;
		return {
			ok,
			status: ok ? 200 : !gcHealth.ok ? gcHealth.status : usage.status,
			body: {
				ok,
				tenantId: tenantB32,
				tenantNumericId: tenantNumeric,
				health: gcHealth.body,
				usage: usage.ok ? filterGcUsageForTenant(usage.body, tenantB32) : usage.body,
			},
		};
	}

	const cell = (input.cell ?? "").trim();
	if (!cell) {
		return {
			ok: false,
			status: 400,
			body: { ok: false, error: "cell is required for cell inspection" },
		};
	}
	if (!GC_CELL_SEGMENT_RE.test(cell)) {
		return {
			ok: false,
			status: 400,
			body: { ok: false, error: `invalid cell identifier "${cell}"` },
		};
	}
	const path =
		input.action === "dry_run_cell"
			? `/dry-run/${encodeURIComponent(tenantB32)}/${encodeURIComponent(cell)}`
			: `/live/${encodeURIComponent(tenantB32)}/${encodeURIComponent(cell)}`;
	const res = await callGcWorkerAdmin(env, path);
	return {
		ok: res.ok,
		status: res.status,
		body: {
			ok: res.ok,
			status: res.status,
			tenantId: tenantB32,
			tenantNumericId: tenantNumeric,
			cell,
			result: res.body,
		},
	};
}
