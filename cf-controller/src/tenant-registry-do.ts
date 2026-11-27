import { DurableObject } from "cloudflare:workers";
import {
	executeReadOnlySql,
	inspectDoSchema,
	type DoSchemaInfo,
	type DoSqlResult,
	type SqlParamValue,
} from "./do-sql.ts";
import { num, str } from "./sql-values.ts";

export interface TenantAssignment {
	orgKey: string;
	tenantId: string;
	createdAt: number;
}

/**
 * Single global SQLite Durable Object ("global") that assigns each Hotbed
 * organization a permanent, unique cladefs uint64 tenant ID.
 *
 * Tenant `0` is reserved for the existing/default organization so legacy R2
 * cells, shared bases, and live workspaces continue without data loss. All
 * other organizations receive monotonically allocated positive IDs (1, 2, ...).
 */
export class TenantRegistryDO extends DurableObject<Env> {
	private sql: SqlStorage;

	constructor(ctx: DurableObjectState, env: Env) {
		super(ctx, env);
		this.sql = ctx.storage.sql;
		void this.ctx.blockConcurrencyWhile(async () => {
			this.sql.exec(`
				CREATE TABLE IF NOT EXISTS org_tenants (
					org_key TEXT PRIMARY KEY,
					tenant_id INTEGER NOT NULL UNIQUE,
					created_at INTEGER NOT NULL
				);
				CREATE TABLE IF NOT EXISTS registry_meta (
					key TEXT PRIMARY KEY,
					value TEXT NOT NULL
				);
			`);
		});
	}

	assignTenant(orgKeyRaw: string, opts?: { isDefault?: boolean }): string {
		const orgKey = orgKeyRaw.trim();
		if (!orgKey) {
			throw new Error("orgKey is required");
		}
		const isDefault = Boolean(opts?.isDefault);
		return this.ctx.storage.transactionSync(() => {
			const existing = this.sql
				.exec("SELECT tenant_id FROM org_tenants WHERE org_key = ?", orgKey)
				.toArray();
			if (existing.length > 0) {
				return String(num(existing[0].tenant_id));
			}

			const nowMs = Date.now();
			if (isDefault) {
				const zeroHolder = this.sql
					.exec("SELECT org_key FROM org_tenants WHERE tenant_id = 0")
					.toArray();
				if (zeroHolder.length === 0) {
					this.sql.exec(
						"INSERT INTO org_tenants (org_key, tenant_id, created_at) VALUES (?, 0, ?)",
						orgKey,
						nowMs,
					);
					return "0";
				}
				const holderKey = str(zeroHolder[0].org_key);
				if (holderKey === "default" && orgKey !== "default") {
					this.sql.exec(
						"UPDATE org_tenants SET org_key = ? WHERE org_key = 'default' AND tenant_id = 0",
						orgKey,
					);
					return "0";
				}
				// Another real organization already permanently owns tenant 0; fall
				// through to allocate a unique positive tenant ID so orgs never collide.
			}

			const metaRows = this.sql
				.exec("SELECT value FROM registry_meta WHERE key = 'next_tenant_id'")
				.toArray();
			const metaNext = metaRows.length > 0 ? num(metaRows[0].value) : 1;
			const maxRows = this.sql
				.exec("SELECT COALESCE(MAX(tenant_id), 0) AS max_id FROM org_tenants")
				.toArray();
			const maxId = maxRows.length > 0 ? num(maxRows[0].max_id) : 0;
			const assigned = Math.max(1, Number.isFinite(metaNext) ? metaNext : 1, maxId + 1);

			this.sql.exec(
				"INSERT INTO org_tenants (org_key, tenant_id, created_at) VALUES (?, ?, ?)",
				orgKey,
				assigned,
				nowMs,
			);
			this.sql.exec(
				`INSERT INTO registry_meta (key, value) VALUES ('next_tenant_id', ?)
				 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
				String(assigned + 1),
			);
			return String(assigned);
		});
	}

	getTenant(orgKeyRaw: string): string | null {
		const orgKey = orgKeyRaw.trim();
		if (!orgKey) return null;
		const rows = this.sql
			.exec("SELECT tenant_id FROM org_tenants WHERE org_key = ?", orgKey)
			.toArray();
		if (rows.length === 0) return null;
		return String(num(rows[0].tenant_id));
	}

	listTenants(): TenantAssignment[] {
		return this.sql
			.exec("SELECT org_key, tenant_id, created_at FROM org_tenants ORDER BY tenant_id ASC")
			.toArray()
			.map((r) => ({
				orgKey: str(r.org_key),
				tenantId: String(num(r.tenant_id)),
				createdAt: num(r.created_at),
			}));
	}

	/** Read-only SQL query executed via typed Workers RPC by the Admin Agent. */
	adminQuerySql(
		query: string,
		params: SqlParamValue[] = [],
		opts?: { maxRows?: number },
	): DoSqlResult {
		return executeReadOnlySql(this.sql, query, params, opts);
	}

	/** Schema inspection executed via typed Workers RPC by the Admin Agent. */
	adminInspectSchema(): Promise<DoSchemaInfo> {
		return inspectDoSchema(this.sql, this.ctx.storage);
	}
}
