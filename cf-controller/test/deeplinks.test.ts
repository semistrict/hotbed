import { env } from "cloudflare:test";
import { describe, expect, it } from "vite-plus/test";
import type { ControllerEnv } from "../src/controller-do.ts";
import worker from "../src/index.ts";

const OWNER_TOKEN = "test-owner-token-secret";

/**
 * Models Cloudflare Workers Static Assets with its default
 * `html_handling: "auto-trailing-slash"` behavior:
 *   - GET /              -> 200 OK (dist/index.html)
 *   - GET /index.html    -> 307 Temporary Redirect -> /
 *   - GET /assets/app.js -> 200 OK
 *   - GET /favicon.svg   -> 200 OK
 *   - anything else      -> 404 Not Found
 */
function makeAutoTrailingSlashAssets(): Fetcher {
	return {
		async fetch(input: RequestInfo | URL): Promise<Response> {
			const reqUrl =
				typeof input === "string" ? input : input instanceof URL ? input.toString() : input.url;
			const { pathname } = new URL(reqUrl);
			if (pathname === "/") {
				return new Response("<!doctype html><html><body>hotbed-spa</body></html>", {
					status: 200,
					headers: { "Content-Type": "text/html; charset=utf-8" },
				});
			}
			if (pathname === "/index.html") {
				return new Response(null, {
					status: 307,
					headers: { Location: "/" },
				});
			}
			if (pathname === "/assets/app.js") {
				return new Response("console.log('ok');", {
					status: 200,
					headers: { "Content-Type": "application/javascript" },
				});
			}
			if (pathname === "/favicon.svg") {
				return new Response("<svg></svg>", {
					status: 200,
					headers: { "Content-Type": "image/svg+xml" },
				});
			}
			return new Response("Not Found", { status: 404 });
		},
	} as unknown as Fetcher;
}

function makeTestEnv(assets: Fetcher): ControllerEnv {
	return {
		...(env as unknown as ControllerEnv),
		OWNER_TOKEN,
		ASSETS: assets,
	};
}

async function loginAsOwner(testEnv: ControllerEnv, returnTo?: string): Promise<Response> {
	const form = new URLSearchParams({ token: OWNER_TOKEN });
	if (returnTo !== undefined) form.set("return_to", returnTo);
	return worker.fetch(
		new Request("https://controller.test/auth/login", {
			method: "POST",
			headers: { "Content-Type": "application/x-www-form-urlencoded" },
			body: form.toString(),
		}),
		testEnv,
	);
}

describe("Cockpit SPA deep links and auth return_to", () => {
	it("serves deep links directly with 200 OK instead of 307 redirecting to /", async () => {
		const testEnv = makeTestEnv(makeAutoTrailingSlashAssets());
		const loginRes = await loginAsOwner(testEnv);
		const setCookie = loginRes.headers.get("Set-Cookie") ?? "";
		expect(setCookie).toContain("SameSite=Lax");
		const cookie = setCookie.split(";")[0];

		const deepLinks = [
			"/questions",
			"/questions?tab=outbox",
			"/board",
			"/board/42",
			"/tasks",
			"/reviews/42",
			"/memories/17",
			"/sessions/tidy-owl",
			"/commands/cmd-99",
			"/nodes/ubu1.local",
			"/workspaces/ws-a.b",
			"/logs?level=error",
			"/cluster",
			"/org",
		];

		for (const path of deepLinks) {
			const res = await worker.fetch(
				new Request(`https://controller.test${path}`, {
					headers: {
						Cookie: cookie,
						Accept: "text/html,application/xhtml+xml",
						"Sec-Fetch-Site": "cross-site",
						"Sec-Fetch-Mode": "navigate",
						"Sec-Fetch-Dest": "document",
					},
				}),
				testEnv,
			);
			expect(res.status, `GET ${path} should serve SPA HTML without redirecting`).toBe(200);
			expect(res.headers.get("Location")).toBeNull();
			expect(await res.text()).toContain("hotbed-spa");
		}
	});

	it("returns 404 for missing static assets instead of SPA HTML", async () => {
		const testEnv = makeTestEnv(makeAutoTrailingSlashAssets());
		const loginRes = await loginAsOwner(testEnv);
		const cookie = (loginRes.headers.get("Set-Cookie") ?? "").split(";")[0];

		for (const missing of [
			"/assets/missing.js",
			"/assets/chunk-deadbeef.css",
			"/robots.txt",
			"/missing.png",
		]) {
			const res = await worker.fetch(
				new Request(`https://controller.test${missing}`, {
					headers: { Cookie: cookie, Accept: "*/*" },
				}),
				testEnv,
			);
			expect(res.status, `GET ${missing} should remain 404`).toBe(404);
		}

		// Existing static files still serve 200.
		const jsRes = await worker.fetch(
			new Request("https://controller.test/assets/app.js", {
				headers: { Cookie: cookie },
			}),
			testEnv,
		);
		expect(jsRes.status).toBe(200);
		expect(await jsRes.text()).toContain("console.log");
	});

	it("preserves return_to across unauthenticated deep-link redirect and token login", async () => {
		const testEnv = makeTestEnv(makeAutoTrailingSlashAssets());

		// 1. Unauthenticated browser GET on a deep link redirects to /auth/login?return_to=...
		const unauth = await worker.fetch(
			new Request("https://controller.test/reviews/42?tab=open", {
				headers: { Accept: "text/html" },
			}),
			testEnv,
		);
		expect(unauth.status).toBe(302);
		const loginLocation = unauth.headers.get("Location")!;
		expect(loginLocation).toBe(
			"https://controller.test/auth/login?return_to=%2Freviews%2F42%3Ftab%3Dopen",
		);

		// 2. GET /auth/login embeds return_to in the form
		const loginPage = await worker.fetch(new Request(loginLocation), testEnv);
		expect(loginPage.status).toBe(200);
		expect(await loginPage.text()).toContain('name="return_to" value="/reviews/42?tab=open"');

		// 3. POST /auth/login redirects back to the deep link
		const loggedIn = await loginAsOwner(testEnv, "/reviews/42?tab=open");
		expect(loggedIn.status).toBe(303);
		expect(loggedIn.headers.get("Location")).toBe("/reviews/42?tab=open");

		// 4. Open-redirect payloads are rejected back to /
		const evil = await loginAsOwner(testEnv, "//evil.example/steal");
		expect(evil.status).toBe(303);
		expect(evil.headers.get("Location")).toBe("/");
	});
});
