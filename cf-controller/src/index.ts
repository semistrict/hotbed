import { ControllerDO, type ControllerEnv } from "./controller-do.ts";

export { ControllerDO };

const DO_PREFIXES = ["/hotbed.v1.", "/ws/", "/builds/", "/auth/", "/admin/"];

function isControllerRoute(pathname: string): boolean {
	if (pathname === "/healthz") return true;
	return DO_PREFIXES.some((p) => pathname.startsWith(p));
}

function acceptsHtml(req: Request): boolean {
	const accept = (req.headers.get("Accept") ?? "").toLowerCase();
	return accept.includes("text/html") || accept.includes("*/*");
}

function buildAuthCheckRequest(req: Request): Request {
	const url = new URL(req.url);
	url.pathname = "/auth/check";
	url.search = "";
	const headers = new Headers();
	for (const key of [
		"Authorization",
		"Cookie",
		"Cf-Access-Jwt-Assertion",
		"Origin",
		"Sec-Fetch-Site",
		"X-Hotbed-Host",
		"X-Hotbed-Node-Id",
		"X-Hotbed-Addr",
	]) {
		const val = req.headers.get(key);
		if (val) headers.set(key, val);
	}
	return new Request(url.toString(), { method: "GET", headers });
}

export default {
	async fetch(request: Request, env: ControllerEnv): Promise<Response> {
		const url = new URL(request.url);
		const stub = env.CONTROLLER.getByName("default");

		if (isControllerRoute(url.pathname)) {
			return stub.fetch(request);
		}

		// Gate Cockpit UI behind the same controller authentication.
		const authRes = await stub.fetch(buildAuthCheckRequest(request));
		if (!authRes.ok) {
			if (request.method === "GET" && acceptsHtml(request)) {
				return Response.redirect(new URL("/auth/login", url).toString(), 302);
			}
			return new Response("Authentication required", { status: 401 });
		}

		if (env.ASSETS) {
			const assetRes = await env.ASSETS.fetch(request);
			if (assetRes.status !== 404) {
				return assetRes;
			}
			// SPA fallback to /index.html for browser routes without a file extension.
			if (request.method === "GET" && !url.pathname.includes(".")) {
				const indexUrl = new URL("/index.html", url);
				return env.ASSETS.fetch(new Request(indexUrl.toString(), request));
			}
			return assetRes;
		}

		return Response.json({
			service: "hotbed-controller",
			runtime: "cloudflare-workers-durable-object",
			hint: "Cockpit static assets are not bundled; API is served at /hotbed.v1.* and worker tunnel at /ws/worker.",
		});
	},
} satisfies ExportedHandler<ControllerEnv>;
