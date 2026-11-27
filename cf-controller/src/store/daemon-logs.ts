import {
	DaemonLogEntrySchema,
	DaemonLogLevel,
	DaemonLogSourceSchema,
	GetDaemonLogsResponseSchema,
	type DaemonLogEntry,
	type DaemonLogSource,
	type GetDaemonLogsResponse,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import { big, num, str } from "../sql-values.ts";
import { create } from "@bufbuild/protobuf";
import type { SqlKernel } from "./kernel.ts";

export const KEEP_DAEMON_LOGS_MS = 14 * 24 * 3600_000;

/** daemon_logs: the controller's own warnings and errors, kept KEEP_DAEMON_LOGS_MS. */
export class DaemonLogStore {
	constructor(private readonly sql: SqlKernel) {}

	log(
		level: DaemonLogLevel,
		msg: string,
		attrs?: Record<string, unknown>,
		nowMs = Date.now(),
	): void {
		const attrsJson = attrs && Object.keys(attrs).length > 0 ? JSON.stringify(attrs) : "";
		this.sql.exec(
			"INSERT INTO daemon_logs (at, level, msg, attrs_json) VALUES (?, ?, ?, ?)",
			nowMs,
			level,
			msg,
			attrsJson,
		);
	}

	summary(host: string, nowMs: number): DaemonLogSource {
		const since = nowMs - 24 * 3600_000;
		const row = this.sql
			.exec(
				`SELECT
					SUM(CASE WHEN level = ? AND at >= ? THEN 1 ELSE 0 END) AS errs,
					SUM(CASE WHEN level = ? AND at >= ? THEN 1 ELSE 0 END) AS warns,
					MAX(at) AS last_at,
					COUNT(*) AS cnt
				FROM daemon_logs`,
				DaemonLogLevel.ERROR,
				since,
				DaemonLogLevel.WARN,
				since,
			)
			.one();
		return create(DaemonLogSourceSchema, {
			source: "controller",
			role: "controller",
			host,
			reachable: true,
			errors24h: num(row.errs),
			warns24h: num(row.warns),
			lastAt: big(row.last_at ?? 0),
			files: 1,
			maxAgeDays: 14,
		});
	}

	query(
		host: string,
		level: DaemonLogLevel,
		query: string,
		fromMs: number,
		toMs: number,
		limit: number,
		nowMs: number,
	): GetDaemonLogsResponse {
		const clauses: string[] = [];
		const params: unknown[] = [];
		if (level !== DaemonLogLevel.UNSPECIFIED) {
			clauses.push("level = ?");
			params.push(level);
		}
		if (fromMs > 0) {
			clauses.push("at >= ?");
			params.push(fromMs);
		}
		if (toMs > 0) {
			clauses.push("at <= ?");
			params.push(toMs);
		}
		const qLower = query.trim().toLowerCase();
		if (qLower) {
			clauses.push("(instr(lower(msg), ?) > 0 OR instr(lower(attrs_json), ?) > 0)");
			params.push(qLower, qLower);
		}
		const where = clauses.length > 0 ? `WHERE ${clauses.join(" AND ")}` : "";
		const rows = this.sql
			.exec(
				`SELECT at, level, msg, attrs_json, source_loc FROM daemon_logs ${where} ORDER BY at DESC, id DESC LIMIT ?`,
				...params,
				limit + 1,
			)
			.toArray();
		const entries: DaemonLogEntry[] = [];
		let truncated = false;
		for (const r of rows) {
			if (entries.length >= limit) {
				truncated = true;
				break;
			}
			entries.push(
				create(DaemonLogEntrySchema, {
					at: big(r.at),
					level: num(r.level) as DaemonLogLevel,
					source: "controller",
					role: "controller",
					host,
					msg: str(r.msg),
					attrsJson: str(r.attrs_json),
					sourceLoc: str(r.source_loc),
				}),
			);
		}
		return create(GetDaemonLogsResponseSchema, {
			entries,
			sources: [this.summary(host, nowMs)],
			truncated,
		});
	}

	prune(nowMs: number): void {
		this.sql.exec("DELETE FROM daemon_logs WHERE at < ?", nowMs - KEEP_DAEMON_LOGS_MS);
	}
}
