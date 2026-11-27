import { clone } from "@bufbuild/protobuf";
import { ErrorCode, hotbedError } from "../errors.ts";
import {
	BaseSchema,
	CommandSchema,
	FinishedCommandSchema,
	type Base,
	type Command,
	type FinishedCommand,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import { num, str } from "../sql-values.ts";
import type { FreshenStore } from "./freshens.ts";
import { idChunks, marshal, unmarshal, type Row, type SqlKernel } from "./kernel.ts";

/** How long a finished command (and a finished freshen) is kept. */
export const KEEP_HISTORY_MS = 30 * 24 * 3600_000;

/** Where a command lives: its session and node, and which tables were looked in to find them. */
export interface CommandRoute {
	session: string;
	node: string;
	histChecked: boolean;
	baseChecked: boolean;
	base?: Base;
}

const HISTORY_SELECT =
	"SELECT h.doc, COALESCE(p.name, '') AS project FROM history h LEFT JOIN projects p ON p.id = h.project_id";

function historyRow(r: Row): FinishedCommand {
	const f = unmarshal(FinishedCommandSchema, str(r.doc));
	f.project = str(r.project);
	return f;
}

/** Everything known of a command: its routing row, its node_state view, the base it built, and its history once finished. */
export class CommandStore {
	constructor(
		private readonly sql: SqlKernel,
		private readonly freshens: FreshenStore,
	) {}

	add(id: string, sessionId: string, nodeId: string, workspaceId: string, atMs: number): void {
		this.sql.exec(
			"INSERT OR IGNORE INTO commands (id, session_id, node_id, workspace_id, created_at) VALUES (?, ?, ?, ?, ?)",
			id,
			sessionId,
			nodeId,
			workspaceId,
			atMs,
		);
	}

	drop(id: string): void {
		this.sql.exec("DELETE FROM commands WHERE id = ?", id);
	}

	lookup(id: string): CommandRoute {
		const cmd = this.sql.first("SELECT session_id, node_id FROM commands WHERE id = ?", id);
		if (cmd) {
			return {
				session: str(cmd.session_id),
				node: str(cmd.node_id),
				histChecked: false,
				baseChecked: false,
			};
		}
		const hist = this.sql.first("SELECT node_id, session_id FROM history WHERE id = ?", id);
		if (hist) {
			return {
				session: str(hist.session_id),
				node: str(hist.node_id),
				histChecked: true,
				baseChecked: false,
			};
		}
		const b = this.baseBuiltBy(id);
		if (b) {
			return { session: "", node: b.nodeId, histChecked: true, baseChecked: true, base: b.base };
		}
		const bf = this.freshens.get(id);
		if (bf) {
			return { session: "", node: bf.nodeId, histChecked: true, baseChecked: true };
		}
		throw hotbedError(ErrorCode.NotFound, `no command ${id}`);
	}

	view(id: string): { nodeId: string; cmd: Command } | null {
		const r = this.sql.first(
			"SELECT node_id, doc FROM node_state WHERE kind = 'command' AND id = ? ORDER BY node_id LIMIT 1",
			id,
		);
		if (!r) return null;
		return { nodeId: str(r.node_id), cmd: unmarshal(CommandSchema, str(r.doc)) };
	}

	/** The base the command built, on the first node holding it. */
	baseBuiltBy(commandId: string): { nodeId: string; base: Base } | null {
		const r = this.sql.first(
			"SELECT node_id, doc FROM node_state WHERE kind = 'base' AND json_extract(doc, '$.command') = ? ORDER BY node_id, id LIMIT 1",
			commandId,
		);
		if (!r) return null;
		return { nodeId: str(r.node_id), base: unmarshal(BaseSchema, str(r.doc)) };
	}

	addHistory(nodeId: string, f: FinishedCommand): void {
		let projectId: number | null = null;
		if (f.sessionId) {
			const r = this.sql.first("SELECT project_id FROM sessions WHERE id = ?", f.sessionId);
			if (r) projectId = num(r.project_id);
		}
		const stored = clone(FinishedCommandSchema, f);
		stored.project = "";
		this.sql.exec(
			"INSERT OR IGNORE INTO history (id, node_id, repo_id, project_id, session_id, ended_at, doc) VALUES (?, ?, ?, ?, ?, ?, ?)",
			f.id,
			nodeId,
			f.repoId,
			projectId,
			f.sessionId,
			f.endedAt,
			marshal(FinishedCommandSchema, stored),
		);
	}

	finished(id: string): FinishedCommand | null {
		const r = this.sql.first(`${HISTORY_SELECT} WHERE h.id = ?`, id);
		return r ? historyRow(r) : null;
	}

	historySince(sinceMs: number): FinishedCommand[] {
		return this.sql
			.rows(`${HISTORY_SELECT} WHERE h.ended_at >= ? ORDER BY h.ended_at`, sinceMs)
			.map(historyRow);
	}

	historyMatching(
		filter: { project: string; session: string; match: string },
		limit: number,
	): { items: FinishedCommand[]; total: number } {
		const where = `WHERE (? = '' OR p.name = ?)
			   AND (? = '' OR h.session_id = ?)
			   AND (? = '' OR instr(json_extract(h.doc, '$.argv'), ?) > 0)`;
		const args = [
			filter.project,
			filter.project,
			filter.session,
			filter.session,
			filter.match,
			filter.match,
		];
		const total = num(
			this.sql
				.exec(
					`SELECT COUNT(*) AS cnt FROM history h LEFT JOIN projects p ON p.id = h.project_id ${where}`,
					...args,
				)
				.one().cnt,
		);
		const items = this.sql
			.rows(
				`${HISTORY_SELECT} ${where} ORDER BY h.ended_at DESC LIMIT ?`,
				...args,
				limit > 0 ? limit : -1,
			)
			.map(historyRow);
		return { items, total };
	}

	inHistory(ids: string[]): Set<string> {
		const out = new Set<string>();
		for (const c of idChunks(ids)) {
			for (const r of this.sql.rows(`SELECT id FROM history WHERE id IN (${c.ph})`, ...c.ids)) {
				out.add(str(r.id));
			}
		}
		return out;
	}

	/** Drops old history, then command rows nothing names any more. */
	prune(nowMs: number): void {
		const cutoff = nowMs - KEEP_HISTORY_MS;
		this.sql.exec("DELETE FROM history WHERE ended_at < ?", cutoff);
		this.sql.exec(
			`DELETE FROM commands WHERE created_at < ?
			 AND id NOT IN (SELECT id FROM node_state WHERE kind = 'command')
			 AND id NOT IN (SELECT id FROM history)
			 AND id NOT IN (SELECT id FROM base_freshens)`,
			cutoff,
		);
	}
}
