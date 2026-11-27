import { ErrorCode, hotbedError } from "../errors.ts";
import { WorkspaceState, type Workspace } from "../gen/hotbed/v1/hotbed_pb.ts";
import { num, str } from "../sql-values.ts";
import { idChunks, type Row, type SqlKernel } from "./kernel.ts";

export interface WorkspaceRow {
	id: string;
	session: string;
	node: string;
	agent: string;
	released: boolean;
	// A throwaway workspace (hotbed run --fork): never the session's
	// workspace on its node.
	throwaway: boolean;
}

/** When a workspace was made, and how long its fork took (-1 if unknown). */
export interface ForkRow {
	at: number;
	millis: number;
}

const WORKSPACE_COLUMNS = "id, session_id, node_id, agent, released_at, throwaway";

function workspaceRow(r: Row): WorkspaceRow {
	return {
		id: str(r.id),
		session: str(r.session_id),
		node: str(r.node_id),
		agent: str(r.agent),
		released: r.released_at !== null && r.released_at !== undefined,
		throwaway: Boolean(r.throwaway),
	};
}

/** The workspaces rows: which session holds which workspace on which node, and when it was released. */
export class WorkspaceStore {
	constructor(private readonly sql: SqlKernel) {}

	/** Adds the workspace if its session is live; false if the session has ended. */
	addLive(
		id: string,
		sessionId: string,
		nodeId: string,
		agent: string,
		nowMs: number,
		throwaway = false,
	): boolean {
		const res = this.sql.exec(
			`INSERT INTO workspaces (id, session_id, node_id, agent, created_at, throwaway)
			 SELECT ?, ?, ?, ?, ?, ?
			 WHERE EXISTS (SELECT 1 FROM sessions WHERE id = ? AND ended_at IS NULL)`,
			id,
			sessionId,
			nodeId,
			agent,
			nowMs,
			throwaway ? 1 : 0,
			sessionId,
		);
		return res.rowsWritten > 0;
	}

	/** Records a workspace a node reports, if it belongs to a session and is not yet known. */
	note(nodeId: string, ws: Workspace): void {
		if (!ws.sessionId) return;
		this.sql.exec(
			"INSERT OR IGNORE INTO workspaces (id, session_id, node_id, agent, created_at, throwaway) VALUES (?, ?, ?, ?, ?, ?)",
			ws.id,
			ws.sessionId,
			nodeId,
			ws.agent,
			ws.createdAt,
			ws.throwaway ? 1 : 0,
		);
		if (ws.forkMillis > 0n) {
			this.sql.exec(
				"UPDATE workspaces SET fork_millis = ? WHERE id = ? AND fork_millis IS NULL",
				ws.forkMillis,
				ws.id,
			);
		}
		if (ws.state === WorkspaceState.RELEASED) {
			this.release(ws.id, Number(ws.lastActiveAt));
		}
	}

	drop(id: string): void {
		this.sql.exec("DELETE FROM workspaces WHERE id = ?", id);
	}

	get(id: string): WorkspaceRow {
		const r = this.sql.first(`SELECT ${WORKSPACE_COLUMNS} FROM workspaces WHERE id = ?`, id);
		if (!r) throw hotbedError(ErrorCode.NotFound, `no workspace ${id}`);
		return workspaceRow(r);
	}

	/** The session's unreleased workspaces, oldest first. */
	ofSession(sessionId: string): WorkspaceRow[] {
		return this.sql
			.rows(
				`SELECT ${WORKSPACE_COLUMNS} FROM workspaces WHERE session_id = ? AND released_at IS NULL ORDER BY created_at, id`,
				sessionId,
			)
			.map(workspaceRow);
	}

	release(id: string, atMs: number): void {
		this.sql.exec(
			"UPDATE workspaces SET released_at = ? WHERE id = ? AND released_at IS NULL",
			atMs,
			id,
		);
	}

	/** Forgets a workspace its node no longer has, releasing its row. */
	reconcileMissing(id: string, atMs: number): void {
		this.sql.tx(() => {
			this.sql.exec("DELETE FROM node_state WHERE kind = 'workspace' AND id = ?", id);
			this.release(id, atMs);
		});
	}

	/** Which of ids are released. */
	released(ids: string[]): Set<string> {
		const out = new Set<string>();
		for (const c of idChunks(ids)) {
			for (const r of this.sql.rows(
				`SELECT id FROM workspaces WHERE id IN (${c.ph}) AND released_at IS NOT NULL`,
				...c.ids,
			)) {
				out.add(str(r.id));
			}
		}
		return out;
	}

	forksSince(sinceMs: number): ForkRow[] {
		return this.sql
			.rows(
				"SELECT created_at, COALESCE(fork_millis, -1) AS fm FROM workspaces WHERE created_at >= ?",
				sinceMs,
			)
			.map((r) => ({ at: num(r.created_at), millis: num(r.fm) }));
	}
}
