import { authenticateRequest, NOOP_TOKEN_LOOKUP, stripInternalAuthHeaders } from "./auth.ts";
import { CommandDO } from "./command-do.ts";
import { ControllerDO, type ControllerEnv } from "./controller-do.ts";
import { fromUnknownError } from "./errors.ts";
import { handleGcRoute } from "./gc.ts";
import { TenantRegistryDO } from "./tenant-registry-do.ts";
import { clearWorkOSSessionCookie } from "./workos.ts";
import {
	attachInternalAuth,
	edgeConnectErrorResponse,
	handleAuthRoute,
	resolveEdgeRouting,
	withSetCookie,
} from "./workos-routes.ts";

export { CommandDO, ControllerDO, TenantRegistryDO };

const DO_PREFIXES = ["/hotbed.v1.", "/ws/", "/builds/", "/admin/"];

const STATIC_ASSET_EXT_RE =
	/\.(?:js|mjs|cjs|css|map|svg|png|jpe?g|gif|webp|avif|ico|woff2?|ttf|otf|eot|json|txt|xml|webmanifest|wasm)$/i;

function isControllerRoute(pathname: string, testHarness: boolean): boolean {
	if (pathname === "/healthz") return true;
	if (testHarness && pathname.startsWith("/__test/")) return true;
	return DO_PREFIXES.some((p) => pathname.startsWith(p));
}

function acceptsHtml(req: Request): boolean {
	const accept = (req.headers.get("Accept") ?? "").toLowerCase();
	return accept.includes("text/html") || accept.includes("*/*");
}

function isSpaFallbackCandidate(request: Request, pathname: string): boolean {
	if (request.method !== "GET" && request.method !== "HEAD") return false;
	if (pathname.startsWith("/assets/")) return false;
	if (STATIC_ASSET_EXT_RE.test(pathname)) return false;
	return true;
}

/**
 * Fetches static assets with SPA deep-link fallback.
 *
 * Cloudflare Workers Static Assets defaults to `html_handling: "auto-trailing-slash"`,
 * which responds to `/index.html` with a `307 Temporary Redirect` to `/`. Fetching `/`
 * directly returns `200 OK` with `index.html` so deep links stay at their requested URL
 * instead of redirecting browsers back to the root `/` page.
 */
async function fetchCockpitAsset(assets: Fetcher, request: Request, url: URL): Promise<Response> {
	const assetRes = await assets.fetch(request);
	if (assetRes.status !== 404 || !isSpaFallbackCandidate(request, url.pathname)) {
		return assetRes;
	}
	const rootUrl = new URL("/", url);
	const rootRes = await assets.fetch(new Request(rootUrl.toString(), request));
	if (rootRes.status !== 404) {
		return rootRes;
	}
	// Fallback for environments configured with html_handling: "none".
	const indexUrl = new URL("/index.html", url);
	return assets.fetch(new Request(indexUrl.toString(), request));
}

function sanitizeIncomingRequest(request: Request): Request {
	let hasInternal = false;
	for (const key of request.headers.keys()) {
		if (key.toLowerCase().startsWith("x-hotbed-internal-")) {
			hasInternal = true;
			break;
		}
	}
	if (!hasInternal) return request;
	const headers = new Headers(request.headers);
	stripInternalAuthHeaders(headers);
	return new Request(request, { headers });
}

function stripRequestSignal(req: Request): Request {
	return new Request(req.url, {
		method: req.method,
		headers: req.headers,
		body: req.method === "GET" || req.method === "HEAD" ? undefined : req.body,
		redirect: req.redirect,
	});
}

export default {
	async fetch(rawRequest: Request, env: ControllerEnv): Promise<Response> {
		// Strip any untrusted client-supplied X-Hotbed-Internal-* headers before routing.
		const request = sanitizeIncomingRequest(rawRequest);
		const url = new URL(request.url);
		const testHarness = env.TEST_HARNESS === "1";

		// 1. Local test harness control endpoints bypass auth routing.
		if (testHarness && url.pathname.startsWith("/__test/")) {
			const doName =
				(request.headers.get("X-Hotbed-Test-DO") ?? url.searchParams.get("__do") ?? "").trim() ||
				"default";
			return env.CONTROLLER.getByName(doName).handleHttpRequest(stripRequestSignal(request));
		}

		// 2. Authentication, WorkOS OAuth, CLI auth, and organization management routes.
		if (url.pathname.startsWith("/auth/")) {
			const authRes = await handleAuthRoute(request, url, env);
			if (authRes) return authRes;
		}

		// 2.5. CladeFS garbage-collector proxy routes for the Cockpit Cluster page.
		if (url.pathname.startsWith("/api/gc/")) {
			const gcRes = await handleGcRoute(request, url, env);
			if (gcRes) return gcRes;
		}

		// 3. Controller API, worker WebSocket tunnel, build downloads, admin endpoints, /healthz.
		if (isControllerRoute(url.pathname, testHarness)) {
			// /healthz is unauthenticated: never route it to an arbitrary org DO based on an
			// unverified Bearer hbu_<org>.* prefix (which would leak that org's clusterId/orgId).
			const allowUnverifiedScopedRouting = url.pathname !== "/healthz";
			let routing;
			try {
				routing = await resolveEdgeRouting(request, url, env, {
					allowUnverifiedScopedRouting,
				});
			} catch (err) {
				const he = fromUnknownError(err);
				if (url.pathname.startsWith("/hotbed.v1.")) {
					return edgeConnectErrorResponse(he);
				}
				return Response.json(he.toJSON(), { status: 403 });
			}
			if (routing.needsOrgSelection) {
				const he = fromUnknownError(
					new Error("no active organization selected; choose or create an organization first"),
				);
				const res = url.pathname.startsWith("/hotbed.v1.")
					? edgeConnectErrorResponse(he)
					: Response.json(he.toJSON(), { status: 403 });
				return withSetCookie(res, routing.setCookie);
			}
			const stub = env.CONTROLLER.getByName(routing.doName);
			if (url.pathname === "/healthz" && request.method === "GET") {
				const health = await stub.getHealth();
				return withSetCookie(Response.json(health), routing.setCookie);
			}
			const fwd = attachInternalAuth(request, routing);
			// WebSocket upgrades (status 101 with webSocket pair) must use stub.fetch()
			// per Cloudflare Workers runtime; all other routes use typed Workers RPC.
			const res =
				url.pathname === "/ws/worker" ||
				url.pathname === "/ws/questions" ||
				url.pathname === "/ws/pipe"
					? await stub.fetch(fwd)
					: await stub.handleHttpRequest(stripRequestSignal(fwd));
			return withSetCookie(res, routing.setCookie);
		}

		// 4. Gate Cockpit UI static assets at the edge without waking ControllerDO.
		let authed = false;
		let needsOrgSelection = false;
		let setCookie: string | undefined;
		let clearWosCookie = false;
		try {
			const routing = await resolveEdgeRouting(request, url, env, {
				allowTopLevelNavigate: true,
			});
			setCookie = routing.setCookie;
			clearWosCookie = Boolean(routing.clearWorkOSCookie);
			needsOrgSelection = Boolean(routing.needsOrgSelection);
			if (routing.caller?.role === "owner") {
				authed = true;
			} else if (!needsOrgSelection) {
				const caller = await authenticateRequest(request, env, NOOP_TOKEN_LOOKUP, {
					allowTopLevelNavigate: true,
				});
				authed = caller.role === "owner";
			}
		} catch {
			authed = false;
		}

		if (needsOrgSelection) {
			if (request.method === "GET" && acceptsHtml(request)) {
				const orgsUrl = new URL("/auth/orgs", url);
				if (url.pathname !== "/" || url.search) {
					orgsUrl.searchParams.set("return_to", `${url.pathname}${url.search}`);
				}
				const res = Response.redirect(orgsUrl.toString(), 302);
				return withSetCookie(res, setCookie);
			}
			return withSetCookie(
				new Response("Organization selection required", { status: 403 }),
				setCookie,
			);
		}

		if (!authed) {
			const clearCookie = clearWosCookie
				? clearWorkOSSessionCookie(url.protocol === "https:")
				: undefined;
			if (request.method === "GET" && acceptsHtml(request)) {
				const loginUrl = new URL("/auth/login", url);
				if (url.pathname !== "/" || url.search) {
					loginUrl.searchParams.set("return_to", `${url.pathname}${url.search}`);
				}
				return withSetCookie(Response.redirect(loginUrl.toString(), 302), clearCookie);
			}
			return withSetCookie(new Response("Authentication required", { status: 401 }), clearCookie);
		}

		if (env.ASSETS) {
			const assetRes = await fetchCockpitAsset(env.ASSETS, request, url);
			return withSetCookie(assetRes, setCookie);
		}

		return withSetCookie(
			Response.json({
				service: "hotbed-controller",
				runtime: "cloudflare-workers-durable-object",
				hint: "Cockpit static assets are not bundled; API is served at /hotbed.v1.* and worker tunnel at /ws/worker.",
			}),
			setCookie,
		);
	},
} satisfies ExportedHandler<ControllerEnv>;
