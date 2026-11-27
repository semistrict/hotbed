import { WorkerConfigSchema, type WorkerConfig } from "../gen/hotbed/v1/hotbed_pb.ts";
import { big, str } from "../sql-values.ts";
import { ErrorCode, hotbedError } from "../errors.ts";
import { mergeWorkerConfig, validateWorkerConfig } from "../worker-config.ts";
import { clone } from "@bufbuild/protobuf";
import { marshal, unmarshal, type Row, type SqlKernel } from "./kernel.ts";

/** Worker configuration: the defaults (scope "") and per-node overrides, each revisioned. */
export class WorkerConfigStore {
	constructor(private readonly sql: SqlKernel) {}

	private decodeRow(r: Row): WorkerConfig {
		const cfg = unmarshal(WorkerConfigSchema, str(r.doc));
		cfg.node = str(r.scope);
		cfg.revision = big(r.revision);
		cfg.updatedAt = big(r.updated_at);
		cfg.updatedBy = str(r.updated_by);
		return cfg;
	}

	get(scope: string): WorkerConfig | null {
		const rows = this.sql
			.exec(
				"SELECT scope, revision, doc, updated_at, updated_by FROM worker_configs WHERE scope = ?",
				scope,
			)
			.toArray();
		if (rows.length === 0) return null;
		return this.decodeRow(rows[0]);
	}

	list(): WorkerConfig[] {
		return this.sql
			.exec(
				"SELECT scope, revision, doc, updated_at, updated_by FROM worker_configs ORDER BY scope ASC",
			)
			.toArray()
			.map((r) => this.decodeRow(r));
	}

	set(
		scope: string,
		patch: WorkerConfig,
		opts: { replace: boolean; ifRevision: bigint; updatedBy: string; nowMs: number },
	): WorkerConfig {
		validateWorkerConfig(patch, scope);
		return this.sql.tx(() => {
			const existing = this.get(scope);
			const curRev = existing?.revision ?? 0n;
			if (opts.ifRevision > 0n && curRev !== opts.ifRevision) {
				const label = scope || "defaults";
				throw hotbedError(
					ErrorCode.Conflict,
					`worker config ${label} is at revision ${curRev}, not ${opts.ifRevision}`,
				).withSuggestion(scope ? `hotbed config show ${scope}` : "hotbed config show");
			}
			const next = mergeWorkerConfig(opts.replace ? null : existing, patch);
			validateWorkerConfig(next, scope);
			const nextRev = curRev + 1n;
			next.node = scope;
			next.revision = nextRev;
			next.updatedAt = BigInt(opts.nowMs);
			next.updatedBy = opts.updatedBy;
			const stored = clone(WorkerConfigSchema, next);
			stored.node = "";
			stored.revision = 0n;
			stored.updatedAt = 0n;
			stored.updatedBy = "";
			this.sql.exec(
				`INSERT INTO worker_configs (scope, revision, doc, updated_at, updated_by)
				 VALUES (?, ?, ?, ?, ?)
				 ON CONFLICT(scope) DO UPDATE SET
				   revision = excluded.revision,
				   doc = excluded.doc,
				   updated_at = excluded.updated_at,
				   updated_by = excluded.updated_by`,
				scope,
				nextRev,
				marshal(WorkerConfigSchema, stored),
				opts.nowMs,
				opts.updatedBy,
			);
			return next;
		});
	}

	delete(scope: string): boolean {
		const existing = this.get(scope);
		if (!existing) return false;
		this.sql.exec("DELETE FROM worker_configs WHERE scope = ?", scope);
		return true;
	}

	/**
	 * The config a node runs: the defaults merged with its override (by
	 * hostname, else node ID). Its revision is the sum of the two, so it
	 * moves with either, and goes back down when an override is deleted:
	 * it labels what was sent, and is no version to order by. Workers apply
	 * a config when it differs from the last one (proto.Equal in
	 * internal/worker/config_sync.go Reconcile), never by its revision.
	 */
	effective(hostname: string, nodeId?: string): WorkerConfig | undefined {
		const defaults = this.get("");
		const nodeOverride =
			(hostname ? this.get(hostname) : null) ??
			(nodeId && nodeId !== hostname ? this.get(nodeId) : null);
		if (!defaults && !nodeOverride) return undefined;
		let merged: WorkerConfig;
		if (defaults && nodeOverride) {
			merged = mergeWorkerConfig(defaults, nodeOverride);
		} else {
			merged = clone(WorkerConfigSchema, (nodeOverride ?? defaults)!);
		}
		merged.node = hostname || nodeId || "";
		merged.revision = (defaults?.revision ?? 0n) + (nodeOverride?.revision ?? 0n);
		const defAt = defaults?.updatedAt ?? 0n;
		const nodeAt = nodeOverride?.updatedAt ?? 0n;
		merged.updatedAt = nodeAt >= defAt ? nodeAt : defAt;
		merged.updatedBy =
			nodeAt >= defAt ? (nodeOverride?.updatedBy ?? "") : (defaults?.updatedBy ?? "");
		return merged;
	}
}
