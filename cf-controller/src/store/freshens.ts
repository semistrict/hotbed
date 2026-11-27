import {
	BaseFreshenSchema,
	FreshenStateSchema,
	type Base,
	type BaseFreshen,
	type Command,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import { enumName } from "../enums.ts";
import { str } from "../sql-values.ts";
import { freshenIdForBase, mergeBaseIntoFreshen, mergeCommandIntoFreshen } from "../freshens.ts";
import { KEEP_HISTORY_MS } from "./commands.ts";
import { marshal, unmarshal, type SqlKernel } from "./kernel.ts";

/** base_freshens: each freshen of a project base, merged from the base and command events that report it. */
export class FreshenStore {
	constructor(private readonly sql: SqlKernel) {}

	private save(f: BaseFreshen): void {
		this.sql.exec(
			`INSERT INTO base_freshens (id, node_id, base_id, project, runtime, state, cold, created_at, started_at, ended_at, doc)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (id) DO UPDATE SET
			   node_id = excluded.node_id,
			   base_id = excluded.base_id,
			   project = excluded.project,
			   runtime = excluded.runtime,
			   state = excluded.state,
			   cold = excluded.cold,
			   created_at = CASE WHEN base_freshens.created_at > 0 AND base_freshens.created_at < excluded.created_at THEN base_freshens.created_at ELSE excluded.created_at END,
			   started_at = COALESCE(excluded.started_at, base_freshens.started_at),
			   ended_at = COALESCE(excluded.ended_at, base_freshens.ended_at),
			   doc = excluded.doc`,
			f.id,
			f.nodeId,
			f.baseId,
			f.project,
			f.runtime || "",
			enumName(FreshenStateSchema, f.state),
			f.cold ? 1 : 0,
			f.createdAt,
			f.startedAt !== undefined ? Number(f.startedAt) : null,
			f.endedAt !== undefined ? Number(f.endedAt) : null,
			marshal(BaseFreshenSchema, f),
		);
	}

	upsertFromBase(nodeId: string, b: Base, what: string, detail: string, eventAtMs: number): void {
		const id = freshenIdForBase(nodeId, b);
		const existing = this.get(id);
		const merged = mergeBaseIntoFreshen(existing, nodeId, b, what, detail, eventAtMs);
		this.save(merged);
	}

	upsertFromCommand(
		nodeId: string,
		c: Command,
		what: string,
		detail: string,
		eventAtMs: number,
	): void {
		const existing = this.get(c.id);
		const merged = mergeCommandIntoFreshen(existing, nodeId, c, what, detail, eventAtMs);
		this.save(merged);
	}

	get(id: string): BaseFreshen | null {
		const rows = this.sql.exec("SELECT doc FROM base_freshens WHERE id = ?", id).toArray();
		if (rows.length === 0) return null;
		return unmarshal(BaseFreshenSchema, str(rows[0].doc));
	}

	/** The freshens that built this base, on any node, newest first. */
	ofBase(baseId: string): BaseFreshen[] {
		return this.sql
			.exec("SELECT doc FROM base_freshens WHERE base_id = ? ORDER BY created_at DESC", baseId)
			.toArray()
			.map((r) => unmarshal(BaseFreshenSchema, str(r.doc)));
	}

	list(sinceMs: number): BaseFreshen[] {
		const rows =
			sinceMs > 0
				? this.sql
						.exec(
							`SELECT doc FROM base_freshens
							 WHERE ended_at IS NULL OR created_at >= ? OR ended_at >= ?
							 ORDER BY created_at DESC`,
							sinceMs,
							sinceMs,
						)
						.toArray()
				: this.sql.exec("SELECT doc FROM base_freshens ORDER BY created_at DESC").toArray();
		return rows.map((r) => unmarshal(BaseFreshenSchema, str(r.doc)));
	}

	prune(nowMs: number): void {
		this.sql.exec(
			"DELETE FROM base_freshens WHERE ended_at IS NOT NULL AND ended_at < ?",
			nowMs - KEEP_HISTORY_MS,
		);
	}
}
