import {
	SessionEventSchema,
	SessionSchema,
	SessionWorkspaceSchema,
	type Session,
	type SessionEvent,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import { big, num, str } from "../sql-values.ts";
import { ErrorCode, hotbedError } from "../errors.ts";
import { create } from "@bufbuild/protobuf";
import { idChunks, marshal, unmarshal, optBig, type Row, type SqlKernel } from "./kernel.ts";
import { KEEP_HISTORY_MS } from "./commands.ts";
import type { ProjectStore } from "./projects.ts";
import type { WorkspaceRow, WorkspaceStore } from "./workspaces.ts";

/** The T3 Code thread a session works in: its id, and the link T3 Code gives it ("" when not known). */
export interface T3CodeThread {
	id: string;
	url: string;
}

const SESSION_SELECT = `SELECT s.id, s.agent, p.name AS project, s.t3code_thread_id, s.t3code_thread_url, s.model, s.started_at, s.last_active_at, s.ended_at, s.end_reason
	FROM sessions s JOIN projects p ON p.id = s.project_id`;

/** Agent sessions and the events each is told about. */
export class SessionStore {
	constructor(
		private readonly sql: SqlKernel,
		private readonly projects: ProjectStore,
		private readonly workspaces: WorkspaceStore,
	) {}

	liveByProject(): Map<string, number> {
		const out = new Map<string, number>();
		for (const r of this.sql
			.exec(
				"SELECT p.name, COUNT(*) AS cnt FROM sessions s JOIN projects p ON p.id = s.project_id WHERE s.ended_at IS NULL GROUP BY p.name",
			)
			.toArray()) {
			out.set(str(r.name), num(r.cnt));
		}
		return out;
	}

	start(
		id: string,
		agent: string,
		project: string,
		t3codeThread: T3CodeThread,
		nowMs: number,
		model = "",
	): Session {
		const pid = this.projects.idByName(project);
		try {
			this.sql.exec(
				"INSERT INTO sessions (id, agent, project_id, t3code_thread_id, t3code_thread_url, model, started_at, last_active_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
				id,
				agent,
				pid,
				t3codeThread.id,
				t3codeThread.url,
				model,
				nowMs,
				nowMs,
			);
		} catch (err) {
			if (this.tryGet(id)) {
				throw hotbedError(ErrorCode.Conflict, `a session called ${id} exists`).withSuggestion(
					"pick another name, or leave it out for a random one",
				);
			}
			throw err;
		}
		return this.get(id);
	}

	tryGet(id: string): Session | null {
		const rows = this.sql.exec(`${SESSION_SELECT} WHERE s.id = ?`, id).toArray();
		if (rows.length === 0) return null;
		return this.hydrate(rows[0]);
	}

	get(id: string): Session {
		const s = this.tryGet(id);
		if (!s) {
			throw hotbedError(ErrorCode.NotFound, `no session ${id}`).withSuggestion(
				"start one with hotbed session start",
			);
		}
		return s;
	}

	live(id: string, nowMs: number): Session {
		const s = this.get(id);
		if (s.endedAt !== undefined) {
			throw sessionEndedError(id, s.endReason);
		}
		this.touch(id, nowMs);
		s.lastActiveAt = BigInt(nowMs);
		return s;
	}

	touch(id: string, atMs: number): void {
		if (!id) return;
		this.sql.exec(
			"UPDATE sessions SET last_active_at = MAX(last_active_at, ?) WHERE id = ? AND ended_at IS NULL",
			atMs,
			id,
		);
	}

	/**
	 * Associates the session with thread if it has no thread yet, and takes
	 * the thread's link when given one (T3 Code's latest, should it have
	 * changed): a session never changes threads, nor takes a link to another
	 * thread.
	 */
	adoptT3CodeThread(id: string, thread: T3CodeThread): void {
		if (!thread.id) return;
		this.sql.exec(
			`UPDATE sessions
			 SET t3code_thread_id = ?,
			     t3code_thread_url = CASE WHEN ? = '' THEN t3code_thread_url ELSE ? END
			 WHERE id = ? AND t3code_thread_id IN ('', ?)`,
			thread.id,
			thread.url,
			thread.url,
			id,
			thread.id,
		);
	}

	setModelIfEmpty(id: string, model: string): void {
		this.sql.exec("UPDATE sessions SET model = ? WHERE id = ? AND model = ''", model, id);
	}

	resume(id: string, nowMs: number): void {
		this.sql.exec(
			"UPDATE sessions SET ended_at = NULL, end_reason = '', last_active_at = ? WHERE id = ?",
			nowMs,
			id,
		);
	}

	endAndWorkspaces(id: string, reason: string, nowMs: number): WorkspaceRow[] {
		return this.sql.tx(() => {
			this.sql.exec(
				"UPDATE sessions SET ended_at = ?, end_reason = ? WHERE id = ? AND ended_at IS NULL",
				nowMs,
				reason,
				id,
			);
			return this.workspaces.ofSession(id);
		});
	}

	endIfIdleAndWorkspaces(
		id: string,
		reason: string,
		nowMs: number,
		cutoffMs: number,
	): { ended: boolean; workspaces: WorkspaceRow[] } {
		return this.sql.tx(() => {
			const res = this.sql.exec(
				"UPDATE sessions SET ended_at = ?, end_reason = ? WHERE id = ? AND ended_at IS NULL AND last_active_at <= ?",
				nowMs,
				reason,
				id,
				cutoffMs,
			);
			if (res.rowsWritten === 0) return { ended: false, workspaces: [] };
			return { ended: true, workspaces: this.workspaces.ofSession(id) };
		});
	}

	idle(cutoffMs: number): string[] {
		return this.sql
			.exec(
				`SELECT s.id FROM sessions s
				 WHERE s.ended_at IS NULL
				   AND s.last_active_at <= ?
				   AND NOT EXISTS (
				     SELECT 1 FROM schedule_runs sr WHERE sr.session_id = s.id AND sr.ended_at IS NULL
				   )`,
				cutoffMs,
			)
			.toArray()
			.map((r) => str(r.id));
	}

	recent(sinceMs: number, project: string, t3codeThreadId: string): Session[] {
		const rows = this.sql
			.exec(
				`${SESSION_SELECT}
				 WHERE (s.ended_at IS NULL OR s.ended_at >= ?)
				   AND (? = '' OR p.name = ?)
				   AND (? = '' OR s.t3code_thread_id = ?)
				 ORDER BY s.last_active_at DESC
				 LIMIT 500`,
				sinceMs,
				project,
				project,
				t3codeThreadId,
				t3codeThreadId,
			)
			.toArray();
		return rows.map((r) => this.hydrate(r));
	}

	searchByT3CodeThread(
		threadId: string,
		project: string,
		liveOnly: boolean,
		limit: number,
	): { sessions: Session[]; total: number } {
		const countRow = this.sql
			.exec(
				`SELECT COUNT(*) AS cnt FROM sessions s JOIN projects p ON p.id = s.project_id
				 WHERE s.t3code_thread_id = ? AND s.t3code_thread_id != ''
				   AND (? = 0 OR s.ended_at IS NULL)
				   AND (? = '' OR p.name = ?)`,
				threadId,
				liveOnly ? 1 : 0,
				project,
				project,
			)
			.one();
		const total = num(countRow.cnt);
		const lim = limit > 0 ? limit : -1;
		const rows = this.sql
			.exec(
				`${SESSION_SELECT}
				 WHERE s.t3code_thread_id = ? AND s.t3code_thread_id != ''
				   AND (? = 0 OR s.ended_at IS NULL)
				   AND (? = '' OR p.name = ?)
				 ORDER BY (s.ended_at IS NOT NULL) ASC, s.last_active_at DESC, s.started_at DESC, s.id ASC
				 LIMIT ?`,
				threadId,
				liveOnly ? 1 : 0,
				project,
				project,
				lim,
			)
			.toArray();
		return { sessions: rows.map((r) => this.hydrate(r)), total };
	}

	projectsFor(ids: string[]): Map<string, string> {
		const out = new Map<string, string>();
		for (const c of idChunks(ids)) {
			for (const r of this.sql.rows(
				`SELECT s.id, p.name AS project FROM sessions s JOIN projects p ON p.id = s.project_id WHERE s.id IN (${c.ph})`,
				...c.ids,
			)) {
				out.set(str(r.id), str(r.project));
			}
		}
		return out;
	}

	private hydrate(r: Row): Session {
		const id = str(r.id);
		const sess = create(SessionSchema, {
			id,
			agent: str(r.agent),
			project: str(r.project),
			t3codeThreadId: str(r.t3code_thread_id),
			t3codeThreadUrl: str(r.t3code_thread_url),
			model: str(r.model),
			startedAt: big(r.started_at),
			lastActiveAt: big(r.last_active_at),
			endedAt: optBig(r.ended_at),
			endReason: str(r.end_reason),
		});
		const schedRows = this.sql
			.exec(
				"SELECT schedule, (ended_at IS NULL) AS running FROM schedule_runs WHERE session_id = ? LIMIT 1",
				id,
			)
			.toArray();
		if (schedRows.length > 0) {
			sess.schedule = str(schedRows[0].schedule);
			sess.scheduleRunning = num(schedRows[0].running) !== 0;
		}
		const wsRows = this.sql
			.exec(
				"SELECT id, node_id, created_at, released_at FROM workspaces WHERE session_id = ? ORDER BY created_at, id",
				id,
			)
			.toArray();
		for (const wr of wsRows) {
			const sw = create(SessionWorkspaceSchema, {
				id: str(wr.id),
				nodeId: str(wr.node_id),
				createdAt: big(wr.created_at),
				releasedAt: optBig(wr.released_at),
			});
			if (sw.releasedAt === undefined) sess.workspaceIds.push(sw.id);
			sess.workspaces.push(sw);
		}
		return sess;
	}

	// ---- session_events -------------------------------------------------

	eventsAfter(sessionId: string, afterSeq: bigint): SessionEvent[] {
		return this.sql
			.exec(
				"SELECT seq, doc FROM session_events WHERE session_id = ? AND seq > ? ORDER BY seq",
				sessionId,
				afterSeq,
			)
			.toArray()
			.map((r) => {
				const ev = unmarshal(SessionEventSchema, str(r.doc));
				ev.seq = big(r.seq);
				return ev;
			});
	}

	recordEvent(sessionId: string, atMs: bigint, ev: SessionEvent): boolean {
		if (!sessionId) return false;
		ev.at = atMs;
		ev.sessionId = sessionId;
		this.sql.exec(
			"INSERT INTO session_events (session_id, doc) VALUES (?, ?)",
			sessionId,
			marshal(SessionEventSchema, ev),
		);
		return true;
	}

	tell(sessionId: string, atMs: bigint, ev: SessionEvent): boolean {
		if (!sessionId) return false;
		const rows = this.sql
			.exec("SELECT 1 FROM sessions WHERE id = ? AND ended_at IS NULL", sessionId)
			.toArray();
		if (rows.length === 0) return false;
		return this.recordEvent(sessionId, atMs, ev);
	}

	/** Drops the events of sessions that ended KEEP_HISTORY_MS ago. */
	prune(nowMs: number): void {
		this.sql.exec(
			`DELETE FROM session_events
			 WHERE session_id IN (SELECT id FROM sessions WHERE ended_at IS NOT NULL AND ended_at < ?)`,
			nowMs - KEEP_HISTORY_MS,
		);
	}
}

export function sessionEndedError(id: string, reason: string): Error {
	const msg = reason ? `session ${id} ended (${reason})` : `session ${id} ended`;
	return hotbedError(ErrorCode.NotFound, msg).withSuggestion(
		`start a new session (hotbed session start), or resume this one (hotbed session start --resume ${id})`,
	);
}
