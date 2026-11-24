import { authenticateRequest, NOOP_TOKEN_LOOKUP, stripInternalAuthHeaders } from "./auth.ts";
import { ControllerDO, type ControllerEnv } from "./controller-do.ts";
import { fromUnknownError } from "./errors.ts";
import { clearWorkOSSessionCookie } from "./workos.ts";
import {
	attachInternalAuth,
	edgeConnectErrorResponse,
	handleAuthRoute,
	resolveEdgeRouting,
	withSetCookie,
} from "./workos-routes.ts";

export { ControllerDO };

const DO_PREFIXES = ["/hotbed.v1.", "/ws/", "/builds/", "/admin/"];

function isControllerRoute(pathname: string, testHarness: boolean): boolean {
	if (pathname === "/healthz") return true;
	if (testHarness && pathname.startsWith("/__test/")) return true;
	return DO_PREFIXES.some((p) => pathname.startsWith(p));
}

function acceptsHtml(req: Request): boolean {
	const accept = (req.headers.get("Accept") ?? "").toLowerCase();
	return accept.includes("text/html") || accept.includes("*/*");
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
			return env.CONTROLLER.getByName(doName).fetch(request);
		}

		// 2. Authentication, WorkOS OAuth, CLI auth, and organization management routes.
		if (url.pathname.startsWith("/auth/")) {
			const authRes = await handleAuthRoute(request, url, env);
			if (authRes) return authRes;
		}

		// 3. Controller API, worker WebSocket tunnel, build downloads, admin endpoints, /healthz.
		if (isControllerRoute(url.pathname, testHarness)) {
			let routing;
			try {
				routing = await resolveEdgeRouting(request, url, env);
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
				if (url.pathname.startsWith("/hotbed.v1.")) {
					return edgeConnectErrorResponse(he);
				}
				return Response.json(he.toJSON(), { status: 403 });
			}
			const stub = env.CONTROLLER.getByName(routing.doName);
			const fwd = attachInternalAuth(request, routing);
			const res = await stub.fetch(fwd);
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
				if (url.pathname !== "/") {
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
				if (url.pathname !== "/") {
					loginUrl.searchParams.set("return_to", `${url.pathname}${url.search}`);
				}
				return withSetCookie(Response.redirect(loginUrl.toString(), 302), clearCookie);
			}
			return withSetCookie(new Response("Authentication required", { status: 401 }), clearCookie);
		}

		if (env.ASSETS) {
			const assetRes = await env.ASSETS.fetch(request);
			if (assetRes.status !== 404) {
				return withSetCookie(assetRes, setCookie);
			}
			// SPA fallback to /index.html for browser routes without a file extension.
			if (request.method === "GET" && !url.pathname.includes(".")) {
				const indexUrl = new URL("/index.html", url);
				const indexRes = await env.ASSETS.fetch(new Request(indexUrl.toString(), request));
				return withSetCookie(indexRes, setCookie);
			}
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
