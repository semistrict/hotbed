import { env } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import { handleAdminAgentRoute } from "../src/admin-agent-routes.ts";
import { encodeCrockfordBase32Short, filterGcUsageForTenant } from "../src/admin-cf-api.ts";
import { INTERNAL_AUTH_HEADER, INTERNAL_ORG_HEADER, type CallerIdentity } from "../src/auth.ts";
import { commandDoName } from "../src/command-do.ts";
import type { ControllerEnv } from "../src/controller-do.ts";
import { validateReadOnlySql } from "../src/do-sql.ts";
import { TenantRegistryDO } from "../src/tenant-registry-do.ts";

const OWNER_AUTH = { Authorization: "Bearer test-owner-secret" };

function testEnv(overrides?: Partial<ControllerEnv>): ControllerEnv {
	return {
		...(env as unknown as ControllerEnv),
		...overrides,
	};
}

async function makeOrgToken(
	e: ControllerEnv,
	orgId: string,
	email: string,
	orgRole: "admin" | "owner" | "member",
): Promise<string> {
	const doName = `org:${orgId}`;
	const stub = e.CONTROLLER.getByName(doName);
	const caller: CallerIdentity = {
		role: "owner",
		nodeId: `workos:user_${orgId}_${orgRole}`,
		hostname: email,
		login: email,
		userId: `user_${orgId}_${orgRole}`,
		orgId,
		orgRole,
		authMethod: "workos-jwt",
	};
	const mintReq = new Request("https://hotbed.test/admin/api-tokens", {
		method: "POST",
		headers: {
			"Content-Type": "application/json",
			[INTERNAL_AUTH_HEADER]: JSON.stringify(caller),
			[INTERNAL_ORG_HEADER]: orgId,
		},
		body: JSON.stringify({ name: `${orgId}-${orgRole}-token` }),
	});
	const res = await stub.handleHttpRequest(mintReq);
	expect(res.status).toBe(201);
	const data = (await res.json()) as { token: string };
	expect(data.token).toMatch(new RegExp(`^hbu_${orgId}\\.`));
	return data.token;
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

	it("rejects write statements, PRAGMA assignments/function setters, recursive CTEs, blob DoS, denied tables, and multi-statement smuggling", () => {
		expect(() => validateReadOnlySql("")).toThrow();
		expect(() => validateReadOnlySql("INSERT INTO nodes VALUES ('x')")).toThrow(/not permitted/i);
		expect(() => validateReadOnlySql("UPDATE nodes SET hostname = 'x'")).toThrow(/not permitted/i);
		expect(() => validateReadOnlySql("DELETE FROM nodes")).toThrow(/not permitted/i);
		expect(() => validateReadOnlySql("DROP TABLE nodes")).toThrow(/not permitted/i);
		expect(() => validateReadOnlySql("VACUUM")).toThrow(/not permitted/i);
		expect(() => validateReadOnlySql("PRAGMA user_version = 99")).toThrow(/read-only/i);
		expect(() => validateReadOnlySql("PRAGMA foreign_keys(OFF)")).toThrow(/allowlist/i);
		expect(() => validateReadOnlySql("PRAGMA journal_mode(WAL)")).toThrow(/allowlist/i);
		expect(() => validateReadOnlySql("PRAGMA journal_mode(DELETE)")).toThrow(/not permitted/i);
		expect(() => validateReadOnlySql("PRAGMA page_count(1)")).toThrow(/takes no arguments/i);
		expect(() => validateReadOnlySql("PRAGMA integrity_check")).toThrow(/allowlist/i);
		expect(() => validateReadOnlySql("PRAGMA quick_check")).toThrow(/allowlist/i);
		expect(() => validateReadOnlySql("PRAGMA writable_schema")).toThrow(/allowlist/i);
		expect(() => validateReadOnlySql("SELECT 1; SELECT 2")).toThrow(/single SQL statement/i);
		expect(() => validateReadOnlySql("SELECT 1; DROP TABLE nodes")).toThrow(
			/single SQL statement/i,
		);
		expect(() =>
			validateReadOnlySql("WITH x AS (SELECT 1) DELETE FROM nodes WHERE id IN (SELECT * FROM x)"),
		).toThrow(/not permitted/i);
		expect(() =>
			validateReadOnlySql(
				"WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c) SELECT x FROM c",
			),
		).toThrow(/RECURSIVE/i);
		expect(() => validateReadOnlySql("SELECT randomblob(1000000)")).toThrow(/RANDOMBLOB/i);
		expect(() => validateReadOnlySql("SELECT zeroblob(1000000)")).toThrow(/ZEROBLOB/i);
		const denied = new Set(["git_credentials", "signup_codes", "kv_meta"]);
		expect(() =>
			validateReadOnlySql("SELECT * FROM git_credentials", { deniedTables: denied }),
		).toThrow(/restricted|sensitive/i);
		expect(() =>
			validateReadOnlySql('SELECT * FROM "GIT_CREDENTIALS"', { deniedTables: denied }),
		).toThrow(/restricted|sensitive/i);
		expect(() =>
			validateReadOnlySql("PRAGMA table_info('git_credentials')", { deniedTables: denied }),
		).toThrow(/restricted|sensitive/i);
	});
});

describe("Durable Object SQL & schema RPCs", () => {
	it("runs read-only SQL and schema inspection on ControllerDO and CommandDO without exposing TenantRegistryDO", async () => {
		const e = testEnv();

		// 1. ControllerDO (default org) — sensitive secret tables are hidden & blocked
		const ctrlStub = e.CONTROLLER.getByName("default");
		const ctrlSchema = await ctrlStub.adminInspectSchema();
		expect(ctrlSchema.tables.some((t) => t.name === "nodes")).toBe(true);
		expect(ctrlSchema.tables.some((t) => t.name === "git_credentials")).toBe(false);
		expect(ctrlSchema.tables.some((t) => t.name === "signup_codes")).toBe(false);
		expect(ctrlSchema.tables.some((t) => t.name === "kv_meta")).toBe(false);
		expect(ctrlSchema.clusterId).toBeTruthy();

		const ctrlSql = await ctrlStub.adminQuerySql("SELECT name FROM projects LIMIT 10");
		expect(ctrlSql.ok).toBe(true);
		if (ctrlSql.ok) {
			expect(ctrlSql.result.columns).toEqual(["name"]);
		}

		for (const sensitiveTable of ["git_credentials", "signup_codes", "kv_meta"]) {
			const denied = await ctrlStub.adminQuerySql(`SELECT * FROM ${sensitiveTable}`);
			expect(denied.ok).toBe(false);
			if (!denied.ok) {
				expect(denied.error).toMatch(/sensitive/i);
			}
		}

		// 2. CommandDO: inspecting a nonexistent command must NOT materialize empty tables
		const emptyCmdName = commandDoName(ctrlSchema.clusterId, "cmd-nonexistent-inspect");
		const emptyCmdStub = e.COMMAND.getByName(emptyCmdName);
		const emptySchema = await emptyCmdStub.adminInspectSchema();
		expect(emptySchema.tables.filter((t) => !t.internal)).toEqual([]);

		// Populate a real CommandDO and query it by org-scoped name
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
		expect(cmdSql.ok).toBe(true);
		if (cmdSql.ok) {
			expect(cmdSql.result.rowCount).toBe(1);
			expect(cmdSql.result.rows[0]).toMatchObject({ seq: 1, node_id: "node-1" });
		}

		// 3. TenantRegistryDO must NOT expose adminQuerySql or adminInspectSchema RPCs
		expect("adminQuerySql" in TenantRegistryDO.prototype).toBe(false);
		expect("adminInspectSchema" in TenantRegistryDO.prototype).toBe(false);
	});
});

describe("Per-organization tenant isolation (strict for ALL orgs, including default)", () => {
	it("denies TENANT_REGISTRY and cross-org access even to the default/owner organization", async () => {
		const e = testEnv();

		// Status only exposes CONTROLLER and COMMAND (never TENANT_REGISTRY or ADMIN_AGENT)
		const statusRes = await handleAdminAgentRoute(
			new Request("https://hotbed.test/api/admin-agent/status", { headers: OWNER_AUTH }),
			new URL("https://hotbed.test/api/admin-agent/status"),
			e,
		);
		expect(statusRes?.status).toBe(200);
		const status = (await statusRes!.json()) as { ok: boolean; bindings: string[] };
		expect(status.ok).toBe(true);
		expect(status.bindings).toEqual(["CONTROLLER", "COMMAND", "WORKER"]);

		// TENANT_REGISTRY schema & SQL are strictly denied (403) even for default owner
		const regSchemaRes = await handleAdminAgentRoute(
			new Request(
				"https://hotbed.test/api/admin-agent/schema?binding=TENANT_REGISTRY&name=global",
				{
					headers: OWNER_AUTH,
				},
			),
			new URL("https://hotbed.test/api/admin-agent/schema?binding=TENANT_REGISTRY&name=global"),
			e,
		);
		expect(regSchemaRes?.status).toBe(403);

		const regSqlRes = await handleAdminAgentRoute(
			new Request("https://hotbed.test/api/admin-agent/sql", {
				method: "POST",
				headers: { ...OWNER_AUTH, "Content-Type": "application/json" },
				body: JSON.stringify({
					binding: "TENANT_REGISTRY",
					name: "global",
					sql: "SELECT * FROM org_tenants",
				}),
			}),
			new URL("https://hotbed.test/api/admin-agent/sql"),
			e,
		);
		expect(regSqlRes?.status).toBe(403);

		// Removed /api/admin-agent/cf/* and /agents/admin-agent/* routes return 404 / null
		const cfRes = await handleAdminAgentRoute(
			new Request("https://hotbed.test/api/admin-agent/cf/do-objects", { headers: OWNER_AUTH }),
			new URL("https://hotbed.test/api/admin-agent/cf/do-objects"),
			e,
		);
		expect(cfRes?.status).toBe(404);

		const removedAgentRes = await handleAdminAgentRoute(
			new Request("https://hotbed.test/agents/admin-agent/main/get-messages", {
				headers: OWNER_AUTH,
			}),
			new URL("https://hotbed.test/agents/admin-agent/main/get-messages"),
			e,
		);
		expect(removedAgentRes).toBeNull();
	});

	it("confines each org admin strictly to that org's ControllerDO, CommandDOs, and CladeFS tenant", async () => {
		const e = testEnv();
		const alphaToken = await makeOrgToken(e, "org_alpha", "alice@alpha.test", "admin");
		const betaToken = await makeOrgToken(e, "org_beta", "bob@beta.test", "admin");
		const memberToken = await makeOrgToken(e, "org_alpha", "eve@alpha.test", "member");
		const alphaHeaders = { Authorization: `Bearer ${alphaToken}` };
		const betaHeaders = { Authorization: `Bearer ${betaToken}` };
		const memberHeaders = { Authorization: `Bearer ${memberToken}` };

		// Non-admin org member is denied (403)
		const memberRes = await handleAdminAgentRoute(
			new Request("https://hotbed.test/api/admin-agent/status", { headers: memberHeaders }),
			new URL("https://hotbed.test/api/admin-agent/status"),
			e,
		);
		expect(memberRes?.status).toBe(403);

		const alphaHealth = await e.CONTROLLER.getByName("org:org_alpha").getHealth();
		const betaHealth = await e.CONTROLLER.getByName("org:org_beta").getHealth();
		expect(alphaHealth.clusterId).toBeTruthy();
		expect(betaHealth.clusterId).toBeTruthy();
		expect(alphaHealth.clusterId).not.toBe(betaHealth.clusterId);

		const alphaCmdName = commandDoName(alphaHealth.clusterId, "cmd-alpha-1");
		const betaCmdName = commandDoName(betaHealth.clusterId, "cmd-beta-1");
		await e.COMMAND.getByName(alphaCmdName).saveDiagnostics({
			nodeId: "node-alpha",
			seq: 1,
			createdAtMs: 1_700_000_000_000,
			doc: JSON.stringify({ commandId: "cmd-alpha-1" }),
		});
		await e.COMMAND.getByName(betaCmdName).saveDiagnostics({
			nodeId: "node-beta",
			seq: 1,
			createdAtMs: 1_700_000_000_000,
			doc: JSON.stringify({ commandId: "cmd-beta-1" }),
		});

		// 1. Org alpha status sees ONLY org_alpha's ControllerDO
		const alphaStatusRes = await handleAdminAgentRoute(
			new Request("https://hotbed.test/api/admin-agent/status", { headers: alphaHeaders }),
			new URL("https://hotbed.test/api/admin-agent/status"),
			e,
		);
		expect(alphaStatusRes?.status).toBe(200);
		const alphaStatus = (await alphaStatusRes!.json()) as {
			scope: { controllerDoName: string; orgId: string };
			bindings: string[];
			controllers: Array<{ name: string }>;
		};
		expect(alphaStatus.scope.controllerDoName).toBe("org:org_alpha");
		expect(alphaStatus.scope.orgId).toBe("org_alpha");
		expect(alphaStatus.bindings).toEqual(["CONTROLLER", "COMMAND", "WORKER"]);
		expect(alphaStatus.controllers.map((c) => c.name)).toEqual(["org:org_alpha"]);

		// 2. Org alpha can query its OWN ControllerDO & CommandDO (by name & commandId shorthand)
		const ownCtrlRes = await handleAdminAgentRoute(
			new Request("https://hotbed.test/api/admin-agent/sql", {
				method: "POST",
				headers: { ...alphaHeaders, "Content-Type": "application/json" },
				body: JSON.stringify({
					binding: "CONTROLLER",
					sql: "SELECT COUNT(*) AS n FROM projects",
				}),
			}),
			new URL("https://hotbed.test/api/admin-agent/sql"),
			e,
		);
		expect(ownCtrlRes?.status).toBe(200);
		const ownCtrl = (await ownCtrlRes!.json()) as { target: string; rows: Array<{ n: number }> };
		expect(ownCtrl.target).toBe("CONTROLLER(org:org_alpha)");
		expect(typeof ownCtrl.rows[0].n).toBe("number");

		const ownCmdShorthand = await handleAdminAgentRoute(
			new Request("https://hotbed.test/api/admin-agent/sql", {
				method: "POST",
				headers: { ...alphaHeaders, "Content-Type": "application/json" },
				body: JSON.stringify({
					commandId: "cmd-alpha-1",
					sql: "SELECT seq, node_id FROM command_diagnostics",
				}),
			}),
			new URL("https://hotbed.test/api/admin-agent/sql"),
			e,
		);
		expect(ownCmdShorthand?.status).toBe(200);
		const ownCmdBody = (await ownCmdShorthand!.json()) as { target: string; rowCount: number };
		expect(ownCmdBody.target).toBe(`COMMAND(${alphaCmdName})`);
		expect(ownCmdBody.rowCount).toBe(1);

		// 3. Org alpha is BLOCKED (403) from org_beta's ControllerDO (by name & 64-hex ID) and "default"
		for (const forbidden of [
			{ binding: "CONTROLLER", name: "org:org_beta" },
			{ binding: "CONTROLLER", name: "default" },
			{
				binding: "CONTROLLER",
				id: e.CONTROLLER.idFromName("org:org_beta").toString(),
			},
		]) {
			const res = await handleAdminAgentRoute(
				new Request("https://hotbed.test/api/admin-agent/sql", {
					method: "POST",
					headers: { ...alphaHeaders, "Content-Type": "application/json" },
					body: JSON.stringify({ ...forbidden, sql: "SELECT 1" }),
				}),
				new URL("https://hotbed.test/api/admin-agent/sql"),
				e,
			);
			expect(res?.status).toBe(403);
		}

		// 4. Org alpha is BLOCKED (403) from org_beta's CommandDO (by name, commandId, and 64-hex ID)
		for (const forbidden of [
			{ binding: "COMMAND", name: betaCmdName },
			{ binding: "COMMAND", commandId: betaCmdName },
			{ binding: "COMMAND", id: e.COMMAND.idFromName(betaCmdName).toString() },
		]) {
			const res = await handleAdminAgentRoute(
				new Request("https://hotbed.test/api/admin-agent/sql", {
					method: "POST",
					headers: { ...alphaHeaders, "Content-Type": "application/json" },
					body: JSON.stringify({ ...forbidden, sql: "SELECT * FROM command_meta" }),
				}),
				new URL("https://hotbed.test/api/admin-agent/sql"),
				e,
			);
			expect(res?.status).toBe(403);
		}

		// 5. Org alpha /health returns org_alpha's health, org beta returns org_beta's
		const alphaHealthRes = await handleAdminAgentRoute(
			new Request("https://hotbed.test/api/admin-agent/health", { headers: alphaHeaders }),
			new URL("https://hotbed.test/api/admin-agent/health"),
			e,
		);
		expect(alphaHealthRes?.status).toBe(200);
		expect(((await alphaHealthRes!.json()) as { clusterId: string }).clusterId).toBe(
			alphaHealth.clusterId,
		);

		const betaHealthRes = await handleAdminAgentRoute(
			new Request("https://hotbed.test/api/admin-agent/health", { headers: betaHeaders }),
			new URL("https://hotbed.test/api/admin-agent/health"),
			e,
		);
		expect(betaHealthRes?.status).toBe(200);
		expect(((await betaHealthRes!.json()) as { clusterId: string }).clusterId).toBe(
			betaHealth.clusterId,
		);

		// 6. CladeFS GC tenant ID uses Crockford base32 Short (10 -> "a", 32 -> "10")
		//    so tenant 10 NEVER collides with tenant 32 ("10" in base32).
		expect(encodeCrockfordBase32Short(0)).toBe("0");
		expect(encodeCrockfordBase32Short(9)).toBe("9");
		expect(encodeCrockfordBase32Short(10)).toBe("a");
		expect(encodeCrockfordBase32Short(31)).toBe("z");
		expect(encodeCrockfordBase32Short(32)).toBe("10");

		const filtered = filterGcUsageForTenant(
			{
				cells: 3,
				measuredCells: 3,
				totalObjects: 600,
				totalBytes: 6000,
				usage: [
					{ tenant: "a", cell: "a/cell-t10", measuredUnixMs: 100, objects: 100, bytes: 1000 },
					{ tenant: "10", cell: "10/cell-t32", measuredUnixMs: 200, objects: 200, bytes: 2000 },
					{
						tenant: "000000000000a",
						cell: "a/padded",
						measuredUnixMs: 300,
						objects: 300,
						bytes: 3000,
					},
				],
			},
			encodeCrockfordBase32Short(10),
		) as {
			cells: number;
			totalObjects: number;
			totalBytes: number;
			usage: Array<{ cell: string }>;
		};
		expect(filtered.cells).toBe(2);
		expect(filtered.totalObjects).toBe(400);
		expect(filtered.totalBytes).toBe(4000);
		expect(filtered.usage.map((u) => u.cell)).toEqual(["a/cell-t10", "a/padded"]);
	});

	it("rejects unauthenticated requests with 401, write SQL and non-numeric maxRows with 400", async () => {
		const e = testEnv();
		const unauthRes = await handleAdminAgentRoute(
			new Request("https://hotbed.test/api/admin-agent/status"),
			new URL("https://hotbed.test/api/admin-agent/status"),
			e,
		);
		expect(unauthRes?.status).toBe(401);

		const writeRes = await handleAdminAgentRoute(
			new Request("https://hotbed.test/api/admin-agent/sql", {
				method: "POST",
				headers: { ...OWNER_AUTH, "Content-Type": "application/json" },
				body: JSON.stringify({ binding: "CONTROLLER", sql: "DELETE FROM nodes" }),
			}),
			new URL("https://hotbed.test/api/admin-agent/sql"),
			e,
		);
		expect(writeRes?.status).toBe(400);

		const badMaxRowsRes = await handleAdminAgentRoute(
			new Request("https://hotbed.test/api/admin-agent/sql", {
				method: "POST",
				headers: { ...OWNER_AUTH, "Content-Type": "application/json" },
				body: JSON.stringify({ binding: "CONTROLLER", sql: "SELECT 1", maxRows: "x" }),
			}),
			new URL("https://hotbed.test/api/admin-agent/sql"),
			e,
		);
		expect(badMaxRowsRes?.status).toBe(400);

		const secretTableRes = await handleAdminAgentRoute(
			new Request("https://hotbed.test/api/admin-agent/sql", {
				method: "POST",
				headers: { ...OWNER_AUTH, "Content-Type": "application/json" },
				body: JSON.stringify({ binding: "CONTROLLER", sql: "SELECT * FROM git_credentials" }),
			}),
			new URL("https://hotbed.test/api/admin-agent/sql"),
			e,
		);
		expect(secretTableRes?.status).toBe(400);

		const syntaxErrRes = await handleAdminAgentRoute(
			new Request("https://hotbed.test/api/admin-agent/sql", {
				method: "POST",
				headers: { ...OWNER_AUTH, "Content-Type": "application/json" },
				body: JSON.stringify({
					binding: "CONTROLLER",
					sql: "SELECT * FROM definitely_nonexistent_table",
				}),
			}),
			new URL("https://hotbed.test/api/admin-agent/sql"),
			e,
		);
		expect(syntaxErrRes?.status).toBe(400);

		const gcRes = await handleAdminAgentRoute(
			new Request("https://hotbed.test/api/admin-agent/gc?action=status", { headers: OWNER_AUTH }),
			new URL("https://hotbed.test/api/admin-agent/gc?action=status"),
			e,
		);
		expect(gcRes?.status).toBe(503);
	});
});
