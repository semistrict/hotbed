import { env } from "cloudflare:test";
import { pipeJsonRender } from "@json-render/core";
import { describe, expect, it } from "vitest";
import { buildAdminAgentSystemPrompt } from "../src/admin-agent-catalog.ts";
import { createAdminAgentTools, listAvailableAdminModels } from "../src/admin-agent-do.ts";
import { handleAdminAgentRoute } from "../src/admin-agent-routes.ts";
import {
	cloudflareAccountGet,
	listCfDoNamespaces,
	listCfDoObjects,
	queryCfObservability,
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

		// 1. TenantRegistryDO
		const regStub = e.TENANT_REGISTRY!.getByName("global");
		const assigned = await regStub.assignTenant("org_admin_test", { isDefault: true });
		expect(assigned).toBe("0");

		const regSchema = await regStub.adminInspectSchema();
		expect(regSchema.tables.some((t) => t.name === "org_tenants")).toBe(true);
		const regSql = await regStub.adminQuerySql(
			"SELECT org_key, tenant_id FROM org_tenants WHERE org_key = ?",
			["org_admin_test"],
		);
		expect(regSql.rowCount).toBe(1);
		expect(regSql.rows[0]).toMatchObject({ org_key: "org_admin_test", tenant_id: 0 });

		// 2. ControllerDO (by name and by 64-hex-char ID)
		const ctrlId = e.CONTROLLER.idFromName("admin-sql-ctrl");
		const ctrlStub = e.CONTROLLER.get(ctrlId);
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

		// Populate a real CommandDO and query it via SQL + hex ID
		const populatedName = commandDoName(ctrlSchema.clusterId, "cmd-populated-1");
		const cmdId = e.COMMAND.idFromName(populatedName);
		const cmdStub = e.COMMAND.get(cmdId);
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

		// Query CommandDO by 64-hex-char ID through the REST route too
		const hexRes = await handleAdminAgentRoute(
			new Request("https://hotbed.test/api/admin-agent/sql", {
				method: "POST",
				headers: { ...OWNER_AUTH, "Content-Type": "application/json" },
				body: JSON.stringify({
					binding: "COMMAND",
					id: cmdId.toString(),
					sql: "SELECT COUNT(*) AS n FROM command_diagnostics",
				}),
			}),
			new URL("https://hotbed.test/api/admin-agent/sql"),
			e,
		);
		expect(hexRes?.status).toBe(200);
		const hexBody = (await hexRes!.json()) as { rows: Array<{ n: number }>; resolvedBy: string };
		expect(hexBody.resolvedBy).toBe("id");
		expect(hexBody.rows[0].n).toBe(1);
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

		// Agents SDK HTTP endpoint on AdminAgentDO
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
		const tools = createAdminAgentTools(e);

		// Exercise run_do_sql tool directly
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

		// Exercise Cloudflare REST API helpers with mock fetch
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
			cloudflareApiConfigured: true,
			gcWorkerConfigured: true,
		});
		expect(prompt).toContain("DataTable");
		expect(prompt).toContain("SqlBlock");
		expect(prompt).toContain("BarList");
		expect(prompt).toContain("ControllerDO");

		const modelInfo = listAvailableAdminModels(
			testEnv({ ANTHROPIC_API_KEY: "sk-ant-test", OPENAI_API_KEY: "sk-test" }),
		);
		expect(modelInfo.models.some((m) => m.provider === "anthropic")).toBe(true);
		expect(modelInfo.models.some((m) => m.provider === "openai")).toBe(true);

		// Verify pipeJsonRender splits text + ```spec JSONL patches into data-spec chunks
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
});
