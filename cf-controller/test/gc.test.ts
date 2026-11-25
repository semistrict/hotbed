import { env } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import type { ControllerEnv } from "../src/controller-do.ts";
import { handleGcRoute, type GcFetch } from "../src/gc.ts";

const OWNER_AUTH = { Authorization: "Bearer test-owner-secret" };

function baseEnv(overrides?: Partial<ControllerEnv>): ControllerEnv {
	return {
		...(env as unknown as ControllerEnv),
		GC_WORKER: undefined,
		GC_WORKER_URL: "https://gc.example.test",
		GC_TOKEN: "gc-secret-token",
		...overrides,
	};
}

describe("handleGcRoute", () => {
	it("rejects unauthenticated requests with 401", async () => {
		const req = new Request("https://hotbed.test/api/gc/status");
		const res = await handleGcRoute(req, new URL(req.url), baseEnv());
		expect(res?.status).toBe(401);
	});

	it("returns configured=false when GC_TOKEN is unset", async () => {
		const req = new Request("https://hotbed.test/api/gc/status", { headers: OWNER_AUTH });
		const res = await handleGcRoute(req, new URL(req.url), baseEnv({ GC_TOKEN: "" }));
		expect(res?.status).toBe(200);
		const body = (await res!.json()) as { configured: boolean; cells: number };
		expect(body.configured).toBe(false);
		expect(body.cells).toBe(0);
	});

	it("rejects malformed cell IDs before calling upstream", async () => {
		let called = false;
		const mockFetch: GcFetch = async () => {
			called = true;
			return new Response("ok");
		};
		const req = new Request("https://hotbed.test/api/gc/dry-run/0/bad!id", { headers: OWNER_AUTH });
		const res = await handleGcRoute(req, new URL(req.url), baseEnv(), mockFetch);
		expect(res?.status).toBe(400);
		expect(called).toBe(false);
	});

	it("proxies status, sweep, dry-run, and single-cell collection with GC_TOKEN", async () => {
		const seen: { url: string; method: string; auth: string | null }[] = [];
		const mockFetch: GcFetch = async (input, init) => {
			const headers = new Headers(init?.headers);
			seen.push({ url: input, method: init?.method ?? "GET", auth: headers.get("Authorization") });
			if (input.endsWith("/healthz")) {
				return Response.json({ ok: true, worker: "cladefs-gc" });
			}
			if (input.endsWith("/usage")) {
				return Response.json({
					cells: 2,
					measuredCells: 1,
					totalObjects: 100,
					totalBytes: 1_073_741_824,
					usage: [
						{
							tenant: "0",
							cell: "0/atzpk00005",
							measuredUnixMs: 1_700_000_000_000,
							objects: 100,
							bytes: 1_073_741_824,
							kinds: { segment: { objects: 99, bytes: 1_073_000_000 } },
						},
					],
				});
			}
			if (input.endsWith("/sweep")) {
				return Response.json({
					enqueued: 2,
					sweepStartedMs: 1_700_000_000_001,
					shreddedTenants: [],
				});
			}
			if (input.endsWith("/dry-run/0/atzpk00005")) {
				return Response.json({
					cell: "0/atzpk00005",
					doomed: [{ key: "k1", kind: "segment", size: 4096 }],
					doomedBytes: 4096,
					retired: [],
					fenced: false,
				});
			}
			if (input.endsWith("/cell/0/atzpk00005")) {
				return Response.json({
					cell: "0/atzpk00005",
					deleted: 1,
					deletedBytes: 4096,
					retired: 0,
					skipped: 0,
					removed: 0,
				});
			}
			return new Response("not found", { status: 404 });
		};

		const statusReq = new Request("https://hotbed.test/api/gc/status", { headers: OWNER_AUTH });
		const statusRes = await handleGcRoute(statusReq, new URL(statusReq.url), baseEnv(), mockFetch);
		expect(statusRes?.status).toBe(200);
		const status = (await statusRes!.json()) as {
			configured: boolean;
			healthy: boolean;
			cells: number;
			measuredCells: number;
		};
		expect(status).toMatchObject({ configured: true, healthy: true, cells: 2, measuredCells: 1 });

		const sweepReq = new Request("https://hotbed.test/api/gc/sweep", {
			method: "POST",
			headers: OWNER_AUTH,
		});
		const sweepRes = await handleGcRoute(sweepReq, new URL(sweepReq.url), baseEnv(), mockFetch);
		expect(sweepRes?.status).toBe(200);
		expect(await sweepRes!.json()).toMatchObject({ enqueued: 2 });

		const dryReq = new Request("https://hotbed.test/api/gc/dry-run/0/atzpk00005", {
			headers: OWNER_AUTH,
		});
		const dryRes = await handleGcRoute(dryReq, new URL(dryReq.url), baseEnv(), mockFetch);
		expect(dryRes?.status).toBe(200);
		expect(await dryRes!.json()).toMatchObject({ doomedBytes: 4096 });

		const collectReq = new Request("https://hotbed.test/api/gc/cell/0/atzpk00005", {
			method: "POST",
			headers: OWNER_AUTH,
		});
		const collectRes = await handleGcRoute(
			collectReq,
			new URL(collectReq.url),
			baseEnv(),
			mockFetch,
		);
		expect(collectRes?.status).toBe(200);
		expect(await collectRes!.json()).toMatchObject({ deleted: 1, deletedBytes: 4096 });

		// Verify GC_TOKEN was sent to every authenticated upstream endpoint (and omitted on /healthz).
		const authCalls = seen.filter((c) => !c.url.endsWith("/healthz"));
		expect(authCalls.every((c) => c.auth === "Bearer gc-secret-token")).toBe(true);
	});

	it("prefers env.GC_WORKER service binding over public fetch when bound", async () => {
		const boundUrls: string[] = [];
		const fakeBinding = {
			async fetch(input: RequestInfo | URL): Promise<Response> {
				const u =
					typeof input === "string" ? input : input instanceof URL ? input.toString() : input.url;
				boundUrls.push(u);
				if (u.endsWith("/healthz")) return Response.json({ ok: true });
				return Response.json({
					cells: 3,
					measuredCells: 3,
					totalObjects: 12,
					totalBytes: 4096,
					usage: [],
				});
			},
		} as unknown as Fetcher;

		const req = new Request("https://hotbed.test/api/gc/status", { headers: OWNER_AUTH });
		const res = await handleGcRoute(
			req,
			new URL(req.url),
			baseEnv({ GC_WORKER: fakeBinding, GC_WORKER_URL: "" }),
		);
		expect(res?.status).toBe(200);
		expect(await res!.json()).toMatchObject({ configured: true, healthy: true, cells: 3 });
		expect(boundUrls).toEqual([
			"https://cladefs-gc.internal/healthz",
			"https://cladefs-gc.internal/usage",
		]);
	});
});
