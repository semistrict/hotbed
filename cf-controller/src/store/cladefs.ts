import { SharedBaseSchema, type CladefsCell, type SharedBase } from "../gen/hotbed/v1/hotbed_pb.ts";
import { num, str } from "../sql-values.ts";
import { marshal, unmarshal, type SqlKernel } from "./kernel.ts";
import type { MetaStore } from "./meta.ts";

export interface RecordedSharedBaseKey {
	project: string;
	arch: string;
	runtime: string;
	tenant: string;
}

export interface SharedBaseWithMeta {
	base: SharedBase;
	seenAt: number;
}

/** cladefs coordination: fencing tokens, the shared bucket's project bases and retired worker incarnations. */
export class CladefsStore {
	constructor(
		private readonly sql: SqlKernel,
		private readonly meta: MetaStore,
	) {}

	nextFencingToken(): bigint {
		let next = 1n;
		this.sql.tx(() => {
			const cur = this.meta.get("cladefs_fencing_token");
			if (cur) {
				next = BigInt(cur) + 1n;
			}
			this.meta.set("cladefs_fencing_token", next.toString());
		});
		return next;
	}

	/**
	 * Records the project bases a worker holds (HeartbeatRequest.held_bases)
	 * and the base cells it retired since its last heartbeat. A base heard of
	 * for the first time is the newest of its chain, replacing the one before
	 * it, and its holder's to delete once it goes; the bases of retired nodes
	 * pass to this one, which can delete them as well: the bucket is shared.
	 */
	recordHeldBases(
		nodeId: string,
		arch: string,
		held: SharedBase[],
		retired: CladefsCell[],
		nowMs: number,
		allowedTenant?: bigint | null,
	): RecordedSharedBaseKey[] {
		const recorded: RecordedSharedBaseKey[] = [];
		this.sql.tx(() => {
			for (const c of retired) {
				this.sql.exec(
					"DELETE FROM shared_bases WHERE tenant = ? AND cell = ? AND node_id = ?",
					c.tenant.toString(),
					c.cell.toString(),
					nodeId,
				);
			}
			this.sql.exec("DELETE FROM shared_base_holders WHERE node_id = ?", nodeId);
			for (const b of held) {
				const c = b.cell;
				if (!c?.project || c.cell === 0n || c.version === 0n) continue;
				if (c.runtime !== "vm" && c.runtime !== "cladefs") continue;
				if (allowedTenant !== undefined && allowedTenant !== null && c.tenant !== allowedTenant) {
					continue;
				}
				const cellArch = c.arch || arch;
				if (!cellArch) continue;
				const tenant = c.tenant.toString();
				const cell = c.cell.toString();
				const known =
					this.sql
						.exec("SELECT 1 FROM shared_bases WHERE tenant = ? AND cell = ?", tenant, cell)
						.toArray().length > 0;
				if (!known) {
					this.sql.exec(
						`UPDATE shared_bases SET replaced_at = ?
						 WHERE project = ? AND arch = ? AND runtime = ? AND tenant = ? AND replaced_at IS NULL`,
						nowMs,
						c.project,
						cellArch,
						c.runtime,
						tenant,
					);
					this.sql.exec(
						`INSERT INTO shared_bases (tenant, cell, project, arch, runtime, seen_at, base, node_id)
						 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
						tenant,
						cell,
						c.project,
						cellArch,
						c.runtime,
						nowMs,
						marshal(SharedBaseSchema, b),
						nodeId,
					);
					recorded.push({ project: c.project, arch: cellArch, runtime: c.runtime, tenant });
				}
				this.sql.exec(
					"INSERT OR IGNORE INTO shared_base_holders (node_id, tenant, cell) VALUES (?, ?, ?)",
					nodeId,
					tenant,
					cell,
				);
			}
			this.sql.exec(
				"UPDATE shared_bases SET node_id = ? WHERE node_id IN (SELECT id FROM nodes WHERE retired_at IS NOT NULL)",
				nodeId,
			);
			const isHeld = `EXISTS (SELECT 1 FROM shared_base_holders h
				WHERE h.tenant = shared_bases.tenant AND h.cell = shared_bases.cell)`;
			this.sql.exec(
				`UPDATE shared_bases SET unheld_since = NULL WHERE unheld_since IS NOT NULL AND ${isHeld}`,
			);
			this.sql.exec(
				`UPDATE shared_bases SET unheld_since = ? WHERE unheld_since IS NULL AND NOT ${isHeld}`,
				nowMs,
			);
		});
		return recorded;
	}

	/**
	 * Removes every shared-base record and holder entry owned by nodeId (used
	 * for local-only darwin/machined workers whose cells never live in R2) and
	 * restores replaced_at = NULL on the newest remaining base in any chain
	 * left without an active head.
	 */
	purgeNodeSharedBases(nodeId: string): void {
		this.sql.tx(() => {
			this.sql.exec("DELETE FROM shared_base_holders WHERE node_id = ?", nodeId);
			const affected = this.sql
				.exec(
					`SELECT DISTINCT project, arch, runtime, tenant FROM shared_bases
					 WHERE node_id = ?`,
					nodeId,
				)
				.toArray();
			if (affected.length === 0) return;
			this.sql.exec("DELETE FROM shared_bases WHERE node_id = ?", nodeId);
			for (const row of affected) {
				const project = str(row.project);
				const arch = str(row.arch);
				const runtime = str(row.runtime);
				const tenant = str(row.tenant);
				const active = this.sql
					.exec(
						`SELECT 1 FROM shared_bases
						 WHERE project = ? AND arch = ? AND runtime = ? AND tenant = ? AND replaced_at IS NULL
						 LIMIT 1`,
						project,
						arch,
						runtime,
						tenant,
					)
					.toArray();
				if (active.length > 0) continue;
				const latest = this.sql
					.exec(
						`SELECT tenant, cell FROM shared_bases
						 WHERE project = ? AND arch = ? AND runtime = ? AND tenant = ?
						 ORDER BY seen_at DESC LIMIT 1`,
						project,
						arch,
						runtime,
						tenant,
					)
					.toArray();
				if (latest.length > 0) {
					this.sql.exec(
						"UPDATE shared_bases SET replaced_at = NULL WHERE tenant = ? AND cell = ?",
						str(latest[0].tenant),
						str(latest[0].cell),
					);
				}
			}
		});
	}

	/**
	 * The base cells for nodeId to delete from the shared bucket: a newer base
	 * of the same project, arch and runtime replaced each at least graceMs ago,
	 * and no worker has held it for graceMs. The newest base of each chain is
	 * kept, held or not.
	 */
	basesToRetire(nodeId: string, nowMs: number, graceMs: number): CladefsCell[] {
		const before = nowMs - graceMs;
		return this.sql
			.exec(
				`SELECT base FROM shared_bases
				 WHERE node_id = ? AND replaced_at <= ? AND unheld_since <= ?
				 ORDER BY seen_at`,
				nodeId,
				before,
				before,
			)
			.toArray()
			.map((r) => unmarshal(SharedBaseSchema, str(r.base)).cell)
			.filter((c): c is CladefsCell => c !== undefined);
	}

	/**
	 * The newest base of project for arch and runtime: the one a node forks
	 * its workspaces from, or builds the next base on.
	 */
	latestSharedBase(
		project: string,
		arch: string,
		allowedTenant: bigint | null | undefined,
		runtime: string,
	): SharedBase | undefined {
		const rows = this.sharedBaseRows(project, arch, allowedTenant, runtime, "base");
		return rows.length > 0 ? unmarshal(SharedBaseSchema, str(rows[0].base)) : undefined;
	}

	latestSharedBaseWithMeta(
		project: string,
		arch: string,
		allowedTenant: bigint | null | undefined,
		runtime: string,
	): SharedBaseWithMeta | undefined {
		if (!project || !arch || !runtime) return undefined;
		const conds = ["project = ?", "arch = ?", "runtime = ?"];
		const params: SqlStorageValue[] = [project, arch, runtime];
		if (allowedTenant !== undefined && allowedTenant !== null) {
			conds.push("tenant = ?");
			params.push(allowedTenant.toString());
		}
		const rows = this.sql
			.exec(
				`SELECT base, seen_at FROM shared_bases
				 WHERE replaced_at IS NULL AND ${conds.join(" AND ")} LIMIT 1`,
				...params,
			)
			.toArray();
		if (rows.length === 0) return undefined;
		return {
			base: unmarshal(SharedBaseSchema, str(rows[0].base)),
			seenAt: num(rows[0].seen_at),
		};
	}

	hasSharedBase(
		project: string,
		arch: string,
		allowedTenant: bigint | null | undefined,
		runtime: string,
	): boolean {
		return this.sharedBaseRows(project, arch, allowedTenant, runtime, "1").length > 0;
	}

	private sharedBaseRows(
		project: string,
		arch: string,
		allowedTenant: bigint | null | undefined,
		runtime: string,
		what: string,
	) {
		if (!project || !arch || !runtime) return [];
		const conds = ["project = ?", "arch = ?", "runtime = ?"];
		const params: SqlStorageValue[] = [project, arch, runtime];
		if (allowedTenant !== undefined && allowedTenant !== null) {
			conds.push("tenant = ?");
			params.push(allowedTenant.toString());
		}
		return this.sql
			.exec(
				`SELECT ${what} AS base FROM shared_bases
				 WHERE replaced_at IS NULL AND ${conds.join(" AND ")} LIMIT 1`,
				...params,
			)
			.toArray();
	}

	hasLegacyTenantZeroData(): boolean {
		return (
			this.sql.exec("SELECT 1 FROM shared_bases WHERE tenant = '0' LIMIT 1").toArray().length > 0
		);
	}

	recordRetiredIncarnation(
		incarnation: bigint,
		nodeId: string,
		hostname: string,
		nowMs: number,
	): void {
		if (incarnation <= 0n) return;
		this.sql.exec(
			`INSERT INTO cladefs_retired_incarnations (incarnation, node_id, hostname, retired_at)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT (incarnation) DO UPDATE SET retired_at = excluded.retired_at`,
			incarnation.toString(),
			nodeId,
			hostname,
			nowMs,
		);
	}

	listPendingDecommissions(sinceMs: number): bigint[] {
		return this.sql
			.exec(
				`SELECT incarnation FROM cladefs_retired_incarnations
				 WHERE retired_at >= ? ORDER BY retired_at ASC`,
				sinceMs,
			)
			.toArray()
			.map((r) => BigInt(str(r.incarnation)));
	}

	prune(nowMs: number): void {
		this.sql.exec(
			"DELETE FROM cladefs_retired_incarnations WHERE retired_at < ?",
			nowMs - 7 * 24 * 3600_000,
		);
	}
}
