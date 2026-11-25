import { authenticateRequest, NOOP_TOKEN_LOOKUP } from "./auth.ts";
import type { ControllerEnv } from "./controller-do.ts";
import { isWorkOSEnabled } from "./workos.ts";
import { resolveEdgeRouting, withSetCookie } from "./workos-routes.ts";

const BASE32_SEGMENT_RE = /^[0-9a-hjkmnp-tv-z]+$/i;
const GC_SCHEDULE = "0 */6 * * *";
const GC_BUCKET = "hotbed-cladefs";
const GC_QUEUE = "cladefs-gc-cells";

export type GcFetch = (input: string, init?: RequestInit) => Promise<Response>;

function isValidCellSegments(tenant: string, cell: string): boolean {
	return (
		tenant.length > 0 &&
		tenant.length <= 16 &&
		cell.length > 0 &&
		cell.length <= 16 &&
		BASE32_SEGMENT_RE.test(tenant) &&
		BASE32_SEGMENT_RE.test(cell)
	);
}

async function authorizeGcRequest(
	request: Request,
	url: URL,
	env: ControllerEnv,
): Promise<{ ok: true; setCookie?: string } | { ok: false; response: Response }> {
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

		let isOwner = routing.caller?.role === "owner";
		if (!isOwner) {
			const caller = await authenticateRequest(request, env, NOOP_TOKEN_LOOKUP, {
				allowTopLevelNavigate: false,
			});
			isOwner = caller.role === "owner";
		}
		if (!isOwner) {
			return {
				ok: false,
				response: withSetCookie(
					Response.json({ message: "authentication required" }, { status: 401 }),
					setCookie,
				),
			};
		}

		// Cluster-wide R2 garbage collection belongs to the default/owner org alone.
		if (isWorkOSEnabled(env) && routing.doName !== "default" && !routing.globalAdmin) {
			return {
				ok: false,
				response: withSetCookie(
					Response.json(
						{ message: "cluster GC is restricted to the default organization" },
						{ status: 403 },
					),
					setCookie,
				),
			};
		}
		return { ok: true, setCookie };
	} catch (err) {
		const message = err instanceof Error ? err.message : "authentication required";
		return {
			ok: false,
			response: withSetCookie(Response.json({ message }, { status: 401 }), setCookie),
		};
	}
}

async function callGcWorker(
	baseUrl: string,
	token: string,
	path: string,
	method: "GET" | "POST",
	fetchImpl: GcFetch,
): Promise<{ status: number; ok: boolean; body: unknown }> {
	const url = `${baseUrl.replace(/\/+$/, "")}${path}`;
	const headers: Record<string, string> = { Accept: "application/json" };
	if (path !== "/healthz") {
		headers.Authorization = `Bearer ${token}`;
	}
	const res = await fetchImpl(url, { method, headers });
	const text = await res.text();
	let body: unknown = {};
	try {
		body = text ? JSON.parse(text) : {};
	} catch {
		body = { error: text || `HTTP ${res.status}` };
	}
	return { status: res.status, ok: res.ok, body };
}

/**
 * Proxies authenticated Cockpit GC requests (`/api/gc/*`) to the deployed `cladefs-gc`
 * Cloudflare Worker without exposing `GC_TOKEN` to browsers or waking `ControllerDO`.
 */
export async function handleGcRoute(
	request: Request,
	url: URL,
	env: ControllerEnv,
	fetchImpl?: GcFetch,
): Promise<Response | null> {
	if (!url.pathname.startsWith("/api/gc/")) {
		return null;
	}

	const auth = await authorizeGcRequest(request, url, env);
	if (!auth.ok) {
		return auth.response;
	}

	const reply = (body: unknown, status = 200) =>
		withSetCookie(Response.json(body, { status }), auth.setCookie);

	const workerUrl = (env.GC_WORKER_URL ?? "").trim();
	const token = (env.GC_TOKEN ?? "").trim();
	const hasBinding = Boolean(env.GC_WORKER && typeof env.GC_WORKER.fetch === "function");
	const configured = (hasBinding || workerUrl.length > 0) && token.length > 0;
	// Prefer Cloudflare Service Binding (`env.GC_WORKER`) to avoid same-account `workers.dev`
	// Worker-to-Worker fetch rejection (Cloudflare error 1042).
	const baseUrl = hasBinding ? "https://cladefs-gc.internal" : workerUrl;
	const doFetch: GcFetch =
		fetchImpl ??
		(hasBinding
			? (input, init) => env.GC_WORKER!.fetch(input, init)
			: (input, init) => fetch(input, init));

	const sub = url.pathname.slice("/api/gc".length);

	if (request.method === "GET" && sub === "/status") {
		if (!configured) {
			return reply({
				configured: false,
				healthy: false,
				workerUrl: workerUrl || null,
				bucket: GC_BUCKET,
				queue: GC_QUEUE,
				schedule: GC_SCHEDULE,
				cells: 0,
				measuredCells: 0,
				totalObjects: 0,
				totalBytes: 0,
				usage: [],
			});
		}

		try {
			const [health, usage] = await Promise.all([
				callGcWorker(baseUrl, token, "/healthz", "GET", doFetch),
				callGcWorker(baseUrl, token, "/usage", "GET", doFetch),
			]);
			if (!usage.ok) {
				const errMsg =
					typeof (usage.body as Record<string, unknown>)?.error === "string"
						? String((usage.body as Record<string, unknown>).error)
						: `cladefs-gc /usage returned HTTP ${usage.status}`;
				return reply(
					{
						configured: true,
						healthy: health.ok,
						workerUrl,
						bucket: GC_BUCKET,
						queue: GC_QUEUE,
						schedule: GC_SCHEDULE,
						cells: 0,
						measuredCells: 0,
						totalObjects: 0,
						totalBytes: 0,
						usage: [],
						error: errMsg,
					},
					502,
				);
			}
			const data = usage.body as Record<string, unknown>;
			return reply({
				configured: true,
				healthy: health.ok,
				workerUrl,
				bucket: GC_BUCKET,
				queue: GC_QUEUE,
				schedule: GC_SCHEDULE,
				cells: Number(data.cells ?? 0),
				measuredCells: Number(data.measuredCells ?? 0),
				totalObjects: Number(data.totalObjects ?? 0),
				totalBytes: Number(data.totalBytes ?? 0),
				usage: Array.isArray(data.usage) ? data.usage : [],
			});
		} catch (err) {
			return reply(
				{
					configured: true,
					healthy: false,
					workerUrl,
					bucket: GC_BUCKET,
					queue: GC_QUEUE,
					schedule: GC_SCHEDULE,
					cells: 0,
					measuredCells: 0,
					totalObjects: 0,
					totalBytes: 0,
					usage: [],
					error: err instanceof Error ? err.message : String(err),
				},
				502,
			);
		}
	}

	if (!configured) {
		return reply({ message: "GC worker is not configured on this controller" }, 503);
	}

	if (request.method === "POST" && sub === "/sweep") {
		const res = await callGcWorker(baseUrl, token, "/sweep", "POST", doFetch);
		return reply(res.body, res.ok ? 200 : res.status || 502);
	}

	const cellMatch = /^\/cell\/([^/]+)\/([^/]+)$/.exec(sub);
	if (request.method === "POST" && cellMatch) {
		const [, tenant, cell] = cellMatch;
		if (!isValidCellSegments(tenant, cell)) {
			return reply({ message: "invalid cell id" }, 400);
		}
		const res = await callGcWorker(baseUrl, token, `/cell/${tenant}/${cell}`, "POST", doFetch);
		return reply(res.body, res.ok ? 200 : res.status || 502);
	}

	const dryRunMatch = /^\/dry-run\/([^/]+)\/([^/]+)$/.exec(sub);
	if (request.method === "GET" && dryRunMatch) {
		const [, tenant, cell] = dryRunMatch;
		if (!isValidCellSegments(tenant, cell)) {
			return reply({ message: "invalid cell id" }, 400);
		}
		const res = await callGcWorker(baseUrl, token, `/dry-run/${tenant}/${cell}`, "GET", doFetch);
		return reply(res.body, res.ok ? 200 : res.status || 502);
	}

	const liveMatch = /^\/live\/([^/]+)\/([^/]+)$/.exec(sub);
	if (request.method === "GET" && liveMatch) {
		const [, tenant, cell] = liveMatch;
		if (!isValidCellSegments(tenant, cell)) {
			return reply({ message: "invalid cell id" }, 400);
		}
		const res = await callGcWorker(baseUrl, token, `/live/${tenant}/${cell}`, "GET", doFetch);
		return reply(res.body, res.ok ? 200 : res.status || 502);
	}

	return reply({ message: "not found" }, 404);
}
