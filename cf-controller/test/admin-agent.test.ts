import { env } from "cloudflare:test";
import { pipeJsonRender } from "@json-render/core";
import { describe, expect, it } from "vitest";
import { buildAdminAgentSystemPrompt } from "../src/admin-agent-catalog.ts";
import {
	createAdminAgentTools,
	filterGcUsageForTenant,
	listAvailableAdminModels,
} from "../src/admin-agent-do.ts";
import { handleAdminAgentRoute } from "../src/admin-agent-routes.ts";
import {
	cloudflareAccountGet,
	listCfDoNamespaces,
	listCfDoObjects,
	queryCfObservability,
	type AdminOrgScope,
	type CfFetch,
} from "../src/admin-cf-api.ts";
import { commandDoName } from "../src/command-do.ts";
import type { ControllerEnv } from "../src/controller-do.ts";
import { validateReadOnlySql } from "../src/do-sql.ts";

const OWNER_AUTH = { Authorization: "Bearer test-owner-secret" };

function testEnv(overrides?: Partial<ControllerEnv>): ControllerEnv {
	return {
		...(env as unknown as ControllerEnv),
		...overrides,
	};
}

describe("validateReadOnlySql", () => {
	it("permits read-only SELECT, WITH, EXPLAIN, VALUES, and allowlisted PRAGMA queries", () => {
		expect(() => validateReadOnlySql("SELECT * FROM nodes LIMIT 5")).not.toThrow();
		expect(() =>
			validateReadOnlySql("  -- comment\nSELECT 'DROP TABLE nodes' AS safe;"),
		).not.toThrow();
		expect(() =>
			validateReadOnlySql(
				"/* block */ WITH recent AS (SELECT id FROM commands LIMIT 5) SELECT * FROM recent",
			),
		).not.toThrow();
		expect(() => validateReadOnlySql("EXPLAIN QUERY PLAN SELECT * FROM projects")).not.toThrow();
		expect(() => validateReadOnlySql("VALUES (1, 2, 3)")).not.toThrow();
		expect(() => validateReadOnlySql("PRAGMA table_info('nodes')")).not.toThrow();
		expect(() => validateReadOnlySql("PRAGMA page_count")).not.toThrow();
	});

	it("rejects write statements, PRAGMA assignments, and multi-statement smuggling", () => {
		expect(() => validateReadOnlySql("")).toThrow();
		expect(() => validateReadOnlySql("INSERT INTO nodes VALUES ('x')")).toThrow(/read-only/i);
		expect(() => validateReadOnlySql("UPDATE nodes SET hostname = 'x'")).toThrow(/read-only/i);
		expect(() => validateReadOnlySql("DELETE FROM nodes")).toThrow(/read-only/i);
		expect(() => validateReadOnlySql("DROP TABLE nodes")).toThrow(/read-only/i);
		expect(() => validateReadOnlySql("VACUUM")).toThrow(/read-only/i);
		expect(() => validateReadOnlySql("PRAGMA user_version = 99")).toThrow(/read-only/i);
		expect(() => validateReadOnlySql("PRAGMA writable_schema")).toThrow(/allowlist/i);
		expect(() => validateReadOnlySql("SELECT 1; SELECT 2")).toThrow(/single SQL statement/i);
		expect(() => validateReadOnlySql("SELECT 1; DROP TABLE nodes")).toThrow(
			/single SQL statement/i,
		);
		expect(() =>
			validateReadOnlySql("WITH x AS (SELECT 1) DELETE FROM nodes WHERE id IN (SELECT * FROM x)"),
		).toThrow(/read-only/i);
	});
});

describe("Durable Object SQL & schema RPCs", () => {
	it("runs read-only SQL and schema inspection on ControllerDO, TenantRegistryDO, and CommandDO (by name and by hex ID)", async () => {
		const e = testEnv();

		// 1. TenantRegistryDO (other test files share the "global" DO under isolate:false,
		// so tenant 0 may already belong to an earlier test org)
		const regStub = e.TENANT_REGISTRY!.getByName("global");
		const assigned = await regStub.assignTenant("org_admin_test", { isDefault: true });
		expect(Number.isInteger(Number(assigned))).toBe(true);
		expect(Number(assigned)).toBeGreaterThanOrEqual(0);

		const regSchema = await regStub.adminInspectSchema();
		expect(regSchema.tables.some((t) => t.name === "org_tenants")).toBe(true);
		const regSql = await regStub.adminQuerySql(
			"SELECT org_key, tenant_id FROM org_tenants WHERE org_key = ?",
			["org_admin_test"],
		);
		expect(regSql.rowCount).toBe(1);
		expect(regSql.rows[0]).toMatchObject({
			org_key: "org_admin_test",
			tenant_id: Number(assigned),
		});

		// 2. ControllerDO (default org)
		const ctrlStub = e.CONTROLLER.getByName("default");
		const ctrlSchema = await ctrlStub.adminInspectSchema();
		expect(ctrlSchema.tables.some((t) => t.name === "nodes")).toBe(true);
		expect(ctrlSchema.clusterId).toBeTruthy();

		const ctrlSql = await ctrlStub.adminQuerySql(
			"SELECT key, value FROM kv_meta WHERE key = 'cluster_id'",
		);
		expect(ctrlSql.rowCount).toBe(1);
		expect(ctrlSql.rows[0].value).toBe(ctrlSchema.clusterId);

		// 3. CommandDO: inspecting a nonexistent command must NOT materialize empty tables
		const emptyCmdName = commandDoName(ctrlSchema.clusterId, "cmd-nonexistent-inspect");
		const emptyCmdStub = e.COMMAND.getByName(emptyCmdName);
		const emptySchema = await emptyCmdStub.adminInspectSchema();
		expect(emptySchema.tables.filter((t) => !t.internal)).toEqual([]);

		// Populate a real CommandDO and query it by org-scoped name via REST route
		const populatedName = commandDoName(ctrlSchema.clusterId, "cmd-populated-1");
		const cmdStub = e.COMMAND.getByName(populatedName);
		await cmdStub.saveDiagnostics({
			nodeId: "node-1",
			seq: 1,
			createdAtMs: 1_700_000_000_000,
			doc: JSON.stringify({ commandId: "cmd-populated-1" }),
		});
		const cmdSql = await cmdStub.adminQuerySql(
			"SELECT seq, node_id, created_at FROM command_diagnostics",
		);
		expect(cmdSql.rowCount).toBe(1);
		expect(cmdSql.rows[0]).toMatchObject({
			seq: 1,
			node_id: "node-1",
			created_at: 1_700_000_000_000,
		});

		const routeRes = await handleAdminAgentRoute(
			new Request("https://hotbed.test/api/admin-agent/sql", {
				method: "POST",
				headers: { ...OWNER_AUTH, "Content-Type": "application/json" },
				body: JSON.stringify({
					binding: "COMMAND",
					name: populatedName,
					sql: "SELECT COUNT(*) AS n FROM command_diagnostics",
				}),
			}),
			new URL("https://hotbed.test/api/admin-agent/sql"),
			e,
		);
		expect(routeRes?.status).toBe(200);
		const routeBody = (await routeRes!.json()) as {
			rows: Array<{ n: number }>;
			resolvedBy: string;
		};
		expect(routeBody.resolvedBy).toBe("name");
		expect(routeBody.rows[0].n).toBe(1);
	});
});

describe("Per-organization tenant isolation", () => {
	it("strictly confines each organization's Admin Agent to its own ControllerDO, CommandDOs, AdminAgentDOs, and CladeFS tenant", async () => {
		const e = testEnv({
			CLOUDFLARE_ACCOUNT_ID: "acct_123",
			CLOUDFLARE_API_TOKEN: "cf-tok-secret",
		});

		const alphaHeaders = {
			...OWNER_AUTH,
			"X-Hotbed-Org-Id": "org_alpha",
			"Content-Type": "application/json",
		};
		const betaHeaders = {
			...OWNER_AUTH,
			"X-Hotbed-Org-Id": "org_beta",
			"Content-Type": "application/json",
		};

		// Initialize both orgs' ControllerDOs and seed a CommandDO in org_beta
		const alphaCtrl = e.CONTROLLER.getByName("org:org_alpha");
		const betaCtrl = e.CONTROLLER.getByName("org:org_beta");
		const [alphaHealth, betaHealth] = await Promise.all([
			alphaCtrl.getHealth(),
			betaCtrl.getHealth(),
		]);
		expect(alphaHealth.clusterId).not.toBe(betaHealth.clusterId);

		const alphaCmdName = commandDoName(alphaHealth.clusterId, "cmd-alpha-1");
		const betaCmdName = commandDoName(betaHealth.clusterId, "cmd-beta-secret");
		await e.COMMAND.getByName(alphaCmdName).saveDiagnostics({
			nodeId: "node-a",
			seq: 1,
			createdAtMs: 1_700_000_000_000,
			doc: JSON.stringify({ commandId: "cmd-alpha-1" }),
		});
		await e.COMMAND.getByName(betaCmdName).saveDiagnostics({
			nodeId: "node-b",
			seq: 1,
			createdAtMs: 1_700_000_000_000,
			doc: JSON.stringify({ commandId: "cmd-beta-secret" }),
		});

		// 1. org_alpha status only lists org_alpha's bindings (no TENANT_REGISTRY, no account-wide CF API)
		const statusRes = await handleAdminAgentRoute(
			new Request("https://hotbed.test/api/admin-agent/status", { headers: alphaHeaders }),
			new URL("https://hotbed.test/api/admin-agent/status"),
			e,
		);
		expect(statusRes?.status).toBe(200);
		const statusBody = (await statusRes!.json()) as {
			scope: { controllerDoName: string; orgId: string; isGlobalAdmin: boolean };
			bindings: string[];
			cloudflareApiConfigured: boolean;
			controllers: Array<{ name: string }>;
		};
		expect(statusBody.scope).toMatchObject({
			controllerDoName: "org:org_alpha",
			orgId: "org_alpha",
			isGlobalAdmin: false,
		});
		expect(statusBody.bindings).not.toContain("TENANT_REGISTRY");
		expect(statusBody.cloudflareApiConfigured).toBe(false);
		expect(statusBody.controllers.map((c) => c.name)).toEqual(["org:org_alpha"]);

		// 2. org_alpha CAN query its own ControllerDO and its own CommandDO
		const okCtrlRes = await handleAdminAgentRoute(
			new Request("https://hotbed.test/api/admin-agent/sql", {
				method: "POST",
				headers: alphaHeaders,
				body: JSON.stringify({
					binding: "CONTROLLER",
					name: "org:org_alpha",
					sql: "SELECT value FROM kv_meta WHERE key = 'cluster_id'",
				}),
			}),
			new URL("https://hotbed.test/api/admin-agent/sql"),
			e,
		);
		expect(okCtrlRes?.status).toBe(200);
		expect(((await okCtrlRes!.json()) as { rows: Array<{ value: string }> }).rows[0].value).toBe(
			alphaHealth.clusterId,
		);

		const okCmdRes = await handleAdminAgentRoute(
			new Request("https://hotbed.test/api/admin-agent/sql", {
				method: "POST",
				headers: alphaHeaders,
				body: JSON.stringify({
					binding: "COMMAND",
					name: alphaCmdName,
					sql: "SELECT node_id FROM command_diagnostics",
				}),
			}),
			new URL("https://hotbed.test/api/admin-agent/sql"),
			e,
		);
		expect(okCmdRes?.status).toBe(200);

		// 3. org_alpha is 403-BLOCKED from org_beta's ControllerDO (by name AND by 64-hex ID) and "default"
		const betaCtrlHexId = e.CONTROLLER.idFromName("org:org_beta").toString();
		const betaCmdHexId = e.COMMAND.idFromName(betaCmdName).toString();
		const blockedPayloads = [
			{ binding: "CONTROLLER", name: "org:org_beta", sql: "SELECT 1" },
			{ binding: "CONTROLLER", name: "default", sql: "SELECT 1" },
			{ binding: "CONTROLLER", id: betaCtrlHexId, sql: "SELECT 1" },
			{ binding: "COMMAND", name: betaCmdName, sql: "SELECT 1" },
			{ binding: "COMMAND", id: betaCmdHexId, sql: "SELECT 1" },
			{ binding: "ADMIN_AGENT", name: "admin:org:org_beta:main", sql: "SELECT 1" },
			{ binding: "TENANT_REGISTRY", name: "global", sql: "SELECT * FROM org_tenants" },
		];
		for (const payload of blockedPayloads) {
			const res = await handleAdminAgentRoute(
				new Request("https://hotbed.test/api/admin-agent/sql", {
					method: "POST",
					headers: alphaHeaders,
					body: JSON.stringify(payload),
				}),
				new URL("https://hotbed.test/api/admin-agent/sql"),
				e,
			);
			expect(res?.status, `expected 403 for ${JSON.stringify(payload)}`).toBe(403);
		}

		// 4. Org-scoped tools omit account-wide Cloudflare REST API tools
		const alphaScope: AdminOrgScope = {
			controllerDoName: "org:org_alpha",
			orgId: "org_alpha",
			isGlobalAdmin: false,
		};
		const alphaTools = createAdminAgentTools(e, alphaScope);
		expect(alphaTools.list_cf_do_objects).toBeUndefined();
		expect(alphaTools.query_cf_observability).toBeUndefined();
		expect(alphaTools.cf_api_get).toBeUndefined();

		// 5. AdminAgentDO threads are physically isolated per org and refuse cross-org rebinding
		const alphaAgentRes = await handleAdminAgentRoute(
			new Request("https://hotbed.test/agents/admin-agent/shared-name/get-messages", {
				headers: alphaHeaders,
			}),
			new URL("https://hotbed.test/agents/admin-agent/shared-name/get-messages"),
			e,
		);
		expect(alphaAgentRes?.status).toBe(200);
		const betaAgentRes = await handleAdminAgentRoute(
			new Request("https://hotbed.test/agents/admin-agent/shared-name/get-messages", {
				headers: betaHeaders,
			}),
			new URL("https://hotbed.test/agents/admin-agent/shared-name/get-messages"),
			e,
		);
		expect(betaAgentRes?.status).toBe(200);

		const alphaAgentStub = e.ADMIN_AGENT!.getByName("admin:org:org_alpha:shared-name");
		const rebind = await alphaAgentStub.bindScope({
			controllerDoName: "org:org_beta",
			orgId: "org_beta",
			isGlobalAdmin: false,
		});
		expect(rebind.ok).toBe(false);
		if (!rebind.ok) {
			expect(rebind.status).toBe(403);
			expect(rebind.message).toMatch(/permanently bound/i);
		}

		// 6. CladeFS GC usage filter strips every other tenant's cells
		const filtered = filterGcUsageForTenant(
			{
				cells: 2,
				measuredCells: 2,
				totalObjects: 300,
				totalBytes: 3000,
				usage: [
					{ tenant: "1", cell: "1/a", measuredUnixMs: 100, objects: 100, bytes: 1000 },
					{ tenant: "2", cell: "2/b", measuredUnixMs: 200, objects: 200, bytes: 2000 },
				],
			},
			"1",
		) as {
			cells: number;
			totalObjects: number;
			totalBytes: number;
			usage: Array<{ tenant: string }>;
		};
		expect(filtered.cells).toBe(1);
		expect(filtered.totalObjects).toBe(100);
		expect(filtered.totalBytes).toBe(1000);
		expect(filtered.usage.map((u) => u.tenant)).toEqual(["1"]);

		// 7. Account-wide /api/admin-agent/cf/* routes are denied (403) for non-global org admins
		for (const [method, path, body] of [
			["GET", "/api/admin-agent/cf/do-objects", undefined],
			["POST", "/api/admin-agent/cf/observability", JSON.stringify({ view: "events" })],
			["POST", "/api/admin-agent/cf/get", JSON.stringify({ path: "workers/scripts" })],
		] as const) {
			const cfRes = await handleAdminAgentRoute(
				new Request(`https://hotbed.test${path}`, {
					method,
					headers: { ...alphaHeaders, ...(body ? { "Content-Type": "application/json" } : {}) },
					body,
				}),
				new URL(`https://hotbed.test${path}`),
				e,
			);
			expect(cfRes?.status).toBe(403);
			const cfBody = (await cfRes!.json()) as { code: string; message: string };
			expect(cfBody.code).toBe("permission_denied");
		}

		// 8. Org-scoped /api/admin-agent/health and commandId shorthand work strictly within org_alpha
		const alphaHealthRes = await handleAdminAgentRoute(
			new Request("https://hotbed.test/api/admin-agent/health", { headers: alphaHeaders }),
			new URL("https://hotbed.test/api/admin-agent/health"),
			e,
		);
		expect(alphaHealthRes?.status).toBe(200);
		const alphaHealthBody = (await alphaHealthRes!.json()) as { clusterId: string };
		expect(alphaHealthBody.clusterId).toBe(alphaHealth.clusterId);

		const alphaCmdShorthand = await handleAdminAgentRoute(
			new Request("https://hotbed.test/api/admin-agent/sql", {
				method: "POST",
				headers: { ...alphaHeaders, "Content-Type": "application/json" },
				body: JSON.stringify({
					commandId: "cmd-alpha-1",
					sql: "SELECT key, value FROM command_meta",
				}),
			}),
			new URL("https://hotbed.test/api/admin-agent/sql"),
			e,
		);
		expect(alphaCmdShorthand?.status).toBe(200);
		const alphaCmdBody = (await alphaCmdShorthand!.json()) as { target: string; rowCount: number };
		expect(alphaCmdBody.target).toBe(`COMMAND(cmd:${alphaHealth.clusterId}:cmd-alpha-1)`);
		expect(alphaCmdBody.rowCount).toBe(1);
	});
});

describe("handleAdminAgentRoute & AdminAgentDO", () => {
	it("rejects unauthenticated requests fail-closed with 401", async () => {
		const req = new Request("https://hotbed.test/api/admin-agent/status");
		const res = await handleAdminAgentRoute(req, new URL(req.url), testEnv());
		expect(res?.status).toBe(401);

		const agentReq = new Request("https://hotbed.test/agents/admin-agent/main/get-messages");
		const agentRes = await handleAdminAgentRoute(agentReq, new URL(agentReq.url), testEnv());
		expect(agentRes?.status).toBe(401);
	});

	it("serves /api/admin-agent/status, /schema, /sql, and /agents/admin-agent/:thread/get-messages for authenticated owners", async () => {
		const e = testEnv();

		const statusReq = new Request("https://hotbed.test/api/admin-agent/status", {
			headers: OWNER_AUTH,
		});
		const statusRes = await handleAdminAgentRoute(statusReq, new URL(statusReq.url), e);
		expect(statusRes?.status).toBe(200);
		const status = (await statusRes!.json()) as {
			ok: boolean;
			bindings: string[];
			controllers: Array<{ name: string }>;
		};
		expect(status.ok).toBe(true);
		expect(status.bindings).toContain("CONTROLLER");
		expect(status.controllers.some((c) => c.name === "default")).toBe(true);

		const schemaReq = new Request(
			"https://hotbed.test/api/admin-agent/schema?binding=CONTROLLER&name=default",
			{ headers: OWNER_AUTH },
		);
		const schemaRes = await handleAdminAgentRoute(schemaReq, new URL(schemaReq.url), e);
		expect(schemaRes?.status).toBe(200);
		const schemaBody = (await schemaRes!.json()) as { tables: Array<{ name: string }> };
		expect(schemaBody.tables.some((t) => t.name === "projects")).toBe(true);

		const writeRejectReq = new Request("https://hotbed.test/api/admin-agent/sql", {
			method: "POST",
			headers: { ...OWNER_AUTH, "Content-Type": "application/json" },
			body: JSON.stringify({
				binding: "CONTROLLER",
				name: "default",
				sql: "DELETE FROM nodes",
			}),
		});
		const writeRejectRes = await handleAdminAgentRoute(
			writeRejectReq,
			new URL(writeRejectReq.url),
			e,
		);
		expect(writeRejectRes?.status).toBe(400);

		const msgUrl = new URL("https://hotbed.test/agents/admin-agent/test-thread-1/get-messages");
		const msgRes = await handleAdminAgentRoute(
			new Request(msgUrl.toString(), { headers: OWNER_AUTH }),
			msgUrl,
			e,
		);
		expect(msgRes?.status).toBe(200);
		expect(await msgRes!.json()).toEqual([]);
	});

	it("executes AdminAgent tools and Cloudflare REST API helpers cleanly", async () => {
		const e = testEnv({
			CLOUDFLARE_ACCOUNT_ID: "acct_123",
			CLOUDFLARE_API_TOKEN: "cf-tok-secret",
		});
		const tools = createAdminAgentTools(e, {
			controllerDoName: "default",
			orgId: "",
			isGlobalAdmin: true,
		});

		const sqlTool = tools.run_do_sql as {
			execute: (input: Record<string, unknown>, opts: unknown) => Promise<Record<string, unknown>>;
		};
		const sqlOut = await sqlTool.execute(
			{
				binding: "CONTROLLER",
				name: "default",
				sql: "SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name LIMIT 5",
			},
			{},
		);
		expect(sqlOut.error).toBeUndefined();
		expect(Number(sqlOut.rowCount)).toBeGreaterThan(0);

		const calls: Array<{ url: string; method: string; auth: string | null }> = [];
		const mockFetch: CfFetch = async (input, init) => {
			const h = new Headers(init?.headers);
			calls.push({ url: input, method: init?.method ?? "GET", auth: h.get("Authorization") });
			if (input.includes("/durable_objects/namespaces/ns_1/objects")) {
				return Response.json({
					success: true,
					result: [{ id: "a".repeat(64), hasStoredData: true }],
				});
			}
			if (input.includes("/durable_objects/namespaces")) {
				return Response.json({
					success: true,
					result: [
						{
							id: "ns_1",
							name: "hotbed-controller_ControllerDO",
							script: "hotbed-controller",
							class: "ControllerDO",
							use_sqlite: true,
						},
						{
							id: "ns_other",
							name: "other_DO",
							script: "other",
							class: "OtherDO",
							use_sqlite: true,
						},
					],
				});
			}
			if (input.includes("/observability/telemetry/query")) {
				return Response.json({ success: true, result: { events: { events: [] } } });
			}
			return Response.json({ success: true, result: { ok: true } });
		};

		const ns = await listCfDoNamespaces(e, { script: "hotbed-controller", fetchImpl: mockFetch });
		expect(ns.ok).toBe(true);
		expect(ns.result).toHaveLength(1);
		expect(ns.result![0].class).toBe("ControllerDO");

		const objs = await listCfDoObjects(e, "ns_1", { limit: 10, fetchImpl: mockFetch });
		expect(objs.ok).toBe(true);
		expect(objs.result).toHaveLength(1);

		const obs = await queryCfObservability(e, { view: "events", minutesBack: 30 }, mockFetch);
		expect(obs.ok).toBe(true);

		const generic = await cloudflareAccountGet(e, "workers/scripts", { fetchImpl: mockFetch });
		expect(generic.ok).toBe(true);
		expect(calls.every((c) => c.auth === "Bearer cf-tok-secret")).toBe(true);
	});

	it("builds json-render system prompt and transforms streamed spec fences into data-spec parts", async () => {
		const prompt = buildAdminAgentSystemPrompt({
			controllerDoName: "org:org_alpha",
			orgId: "org_alpha",
			isGlobalAdmin: false,
			cloudflareApiConfigured: false,
			gcWorkerConfigured: true,
		});
		expect(prompt).toContain("DataTable");
		expect(prompt).toContain("SqlBlock");
		expect(prompt).toContain("BarList");
		expect(prompt).toContain("org_alpha");
		expect(prompt).toContain("Strict Organization Isolation");

		const modelInfo = listAvailableAdminModels(
			testEnv({ ANTHROPIC_API_KEY: "sk-ant-test", OPENAI_API_KEY: "sk-test" }),
		);
		expect(modelInfo.models.some((m) => m.provider === "anthropic")).toBe(true);
		expect(modelInfo.models.some((m) => m.provider === "openai")).toBe(true);

		const inputChunks = [
			{ type: "text-start", id: "t1" },
			{
				type: "text-delta",
				id: "t1",
				delta:
					'Summary text.\n```spec\n{"op":"add","path":"/root","value":"m1"}\n{"op":"add","path":"/elements/m1","value":{"type":"Metric","props":{"label":"Nodes","value":3},"children":[]}}\n```\n',
			},
			{ type: "text-end", id: "t1" },
		];
		const src = new ReadableStream({
			start(controller) {
				for (const c of inputChunks) controller.enqueue(c);
				controller.close();
			},
		});
		const out: Array<Record<string, unknown>> = [];
		const reader = pipeJsonRender(src).getReader();
		for (;;) {
			const { done, value } = await reader.read();
			if (done) break;
			out.push(value as Record<string, unknown>);
		}
		expect(out.some((c) => c.type === "data-spec")).toBe(true);
		const joinedText = out
			.filter((c) => c.type === "text-delta")
			.map((c) => (typeof c.delta === "string" ? c.delta : ""))
			.join("");
		expect(joinedText).toContain("Summary text.");
	});

	it("serves /health, /gc (503 when unbound), and /cf/* (config error for global owner)", async () => {
		const e = testEnv();

		const healthRes = await handleAdminAgentRoute(
			new Request("https://hotbed.test/api/admin-agent/health", { headers: OWNER_AUTH }),
			new URL("https://hotbed.test/api/admin-agent/health"),
			e,
		);
		expect(healthRes?.status).toBe(200);
		const health = (await healthRes!.json()) as { ok: boolean; clusterId: string };
		expect(health.ok).toBe(true);
		expect(health.clusterId).toBeTruthy();

		const gcRes = await handleAdminAgentRoute(
			new Request("https://hotbed.test/api/admin-agent/gc?action=status", { headers: OWNER_AUTH }),
			new URL("https://hotbed.test/api/admin-agent/gc?action=status"),
			e,
		);
		expect(gcRes?.status).toBe(503);

		// Global admin reaches Cloudflare REST routes (fails with 400 missing secret rather than 403)
		const cfRes = await handleAdminAgentRoute(
			new Request("https://hotbed.test/api/admin-agent/cf/do-objects", { headers: OWNER_AUTH }),
			new URL("https://hotbed.test/api/admin-agent/cf/do-objects"),
			e,
		);
		expect(cfRes?.status).toBe(400);
		const cfBody = (await cfRes!.json()) as { message: string };
		expect(cfBody.message).toMatch(/CLOUDFLARE_API_TOKEN/);
	});
});
