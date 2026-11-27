import {
	BaseSchema,
	CommandSchema,
	CommandState,
	CommandUpdate_What,
	CommandUpdate_WhatSchema,
	CommandUpdateSchema,
	InterventionKind,
	SessionEventSchema,
	WorkspaceSchema,
	WorkspaceState,
	WorkspaceUpdate_What,
	WorkspaceUpdate_WhatSchema,
	WorkspaceUpdateSchema,
	CheckpointSchema,
	type Base,
	type Checkpoint,
	type Command,
	type CommandIntervention,
	type SessionEvent,
	type Stamp,
	type Workspace,
	type Event,
	type PushEventsRequest,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import { enumFromWord } from "../enums.ts";
import { big, str } from "../sql-values.ts";
import { ErrorCode, hotbedError } from "../errors.ts";
import { isCommandLive } from "../wakeups.ts";
import { create, clone } from "@bufbuild/protobuf";
import { marshal, unmarshal, type SqlKernel } from "./kernel.ts";
import type { CommandStore } from "./commands.ts";
import type { FreshenStore } from "./freshens.ts";
import type { SessionStore } from "./sessions.ts";
import type { WorkspaceStore } from "./workspaces.ts";

export const LOST_DETAIL = "lost: its worker no longer has it (the worker's state was reset)";

export interface NodeStateView {
	bases: Base[];
	workspaces: Workspace[];
	commands: Command[];
}

export interface FeedEventRecord {
	at: number;
	kind: string;
	text: string;
	refType: string;
	refId: string;
}

/** What applying a node's pushed events did. */
export interface PushResult {
	applied: bigint;
	appliedAt: bigint;
	checkpointNeeded: boolean;
	finished: boolean;
	lost: string[];
	sessions: Set<string>;
	feedEvents: FeedEventRecord[];
}

function whoStamp(by?: Stamp): string {
	if (by?.session && by?.host) return `session ${by.session} on ${by.host}`;
	if (by?.session) return `session ${by.session}`;
	if (by?.host) return by.host;
	return "someone";
}

function interventionNote(c: Command, iv: CommandIntervention): string {
	const verb =
		iv.kind === InterventionKind.STOP
			? "stopped"
			: iv.kind === InterventionKind.PAUSE
				? "paused"
				: iv.kind === InterventionKind.RESUME
					? "resumed"
					: "intervened on";
	let out = `${verb} by ${whoStamp(iv.by)}`;
	if (iv.comment) out += `: ${JSON.stringify(iv.comment)}`;
	out += ".";
	if (iv.diagnosed) {
		out += ` Diagnostics taken as it ${verb}: hotbed diagnostics ${c.id}`;
		if (iv.diagnosticsError) out += ` (not all: ${iv.diagnosticsError})`;
		out += ".";
	} else if (iv.diagnosticsError) {
		out += ` No diagnostics: ${iv.diagnosticsError}.`;
	}
	if (iv.kind === InterventionKind.PAUSE) {
		const sFlag = c.sessionId ? ` -s ${c.sessionId}` : "";
		out += ` It stays paused until resumed: hotbed resume${sFlag} ${c.id}, or its page in the cockpit.`;
	}
	return out;
}

function commandUpdateEvent(what: CommandUpdate_What, c: Command, detail: string): SessionEvent {
	let d = detail;
	if (!d && c.interventions.length > 0) {
		const last = c.interventions[c.interventions.length - 1];
		if (
			(what === CommandUpdate_What.CANCELLED && last.kind === InterventionKind.STOP) ||
			(what === CommandUpdate_What.PAUSED && last.kind === InterventionKind.PAUSE) ||
			(what === CommandUpdate_What.RESUMED && last.kind === InterventionKind.RESUME)
		) {
			d = interventionNote(c, last);
		}
	}
	return create(SessionEventSchema, {
		detail: d,
		event: {
			case: "command",
			value: create(CommandUpdateSchema, {
				what,
				commandId: c.id,
				argv: c.argv,
				workspaceId: c.workspaceId,
				state: c.state,
				exitCode: c.exitCode,
			}),
		},
	});
}

function workspaceUpdateEvent(
	what: WorkspaceUpdate_What,
	id: string,
	detail: string,
): SessionEvent {
	return create(SessionEventSchema, {
		detail,
		event: {
			case: "workspace",
			value: create(WorkspaceUpdateSchema, { what, workspaceId: id }),
		},
	});
}

/** node_state: each node's bases, workspaces and commands as its event stream and checkpoints report them. */
export class NodeStateStore {
	constructor(
		private readonly sql: SqlKernel,
		private readonly commands: CommandStore,
		private readonly freshens: FreshenStore,
		private readonly sessions: SessionStore,
		private readonly workspaces: WorkspaceStore,
	) {}

	push(nodeId: string, req: PushEventsRequest, nowMs: number, creating: Set<string>): PushResult {
		return this.sql.tx(() => {
			const out: PushResult = {
				applied: 0n,
				appliedAt: 0n,
				checkpointNeeded: false,
				finished: false,
				lost: [],
				sessions: new Set(),
				feedEvents: [],
			};
			const stRows = this.sql
				.exec("SELECT epoch, applied, applied_at FROM nodes WHERE id = ?", nodeId)
				.toArray();
			if (stRows.length === 0) {
				throw hotbedError(ErrorCode.NotJoined, `node ${nodeId} has not joined this cluster`);
			}
			let epoch = str(stRows[0].epoch);
			out.applied = big(stRows[0].applied);
			out.appliedAt = big(stRows[0].applied_at);

			if (req.checkpoint) {
				if (req.epoch !== epoch) {
					out.applied = 0n;
				}
				this.applyEvents(nodeId, req.events, out);
				this.applyCheckpoint(nodeId, req.checkpoint, nowMs, creating, out);
				epoch = req.epoch;
				out.applied = req.checkpoint.seq;
				if (
					req.events.length === 0 ||
					req.events[req.events.length - 1].seq !== req.checkpoint.seq
				) {
					out.appliedAt = 0n;
				}
			} else if (req.epoch !== epoch) {
				return { ...out, applied: 0n, checkpointNeeded: true };
			} else if (!this.follows(req, out)) {
				return { ...out, checkpointNeeded: true };
			} else {
				this.applyEvents(nodeId, req.events, out);
			}

			this.sql.exec(
				"UPDATE nodes SET epoch = ?, applied = ?, applied_at = ? WHERE id = ?",
				epoch,
				out.applied,
				out.appliedAt,
				nodeId,
			);
			return out;
		});
	}

	private follows(req: PushEventsRequest, at: PushResult): boolean {
		let afterAt = req.afterAt;
		for (const ev of req.events) {
			if (ev.seq <= at.applied) {
				if (ev.seq === at.applied) afterAt = ev.at;
				continue;
			}
			if (ev.seq !== at.applied + 1n) return false;
			return afterAt === 0n || at.appliedAt === 0n || afterAt === at.appliedAt;
		}
		return true;
	}

	private applyEvents(nodeId: string, events: Event[], out: PushResult): void {
		for (const ev of events) {
			if (ev.seq <= out.applied) continue;
			if (ev.feed) {
				out.feedEvents.push({
					at: Number(ev.feed.at),
					kind: ev.feed.kind,
					text: ev.feed.text,
					refType: ev.feed.ref?.type ?? "",
					refId: ev.feed.ref?.id ?? "",
				});
			}
			switch (ev.change.case) {
				case "base": {
					const bc = ev.change.value;
					if (bc.base) {
						if (bc.what === "collected") {
							this.dropState(nodeId, "base", bc.base.id);
						} else {
							this.putState(nodeId, "base", bc.base.id, marshal(BaseSchema, bc.base));
							this.freshens.upsertFromBase(nodeId, bc.base, bc.what, bc.detail, Number(ev.at));
						}
					}
					break;
				}
				case "workspace": {
					const wc = ev.change.value;
					const ws = wc.workspace;
					if (ws) {
						if (wc.what === "forgotten") {
							this.dropState(nodeId, "workspace", ws.id);
						} else {
							this.putState(nodeId, "workspace", ws.id, marshal(WorkspaceSchema, ws));
							this.workspaces.note(nodeId, ws);
							if (wc.what === "ready" || wc.what === "failed") {
								if (
									this.sessions.recordEvent(
										ws.sessionId,
										ev.at,
										workspaceUpdateEvent(
											enumFromWord(WorkspaceUpdate_WhatSchema, wc.what),
											ws.id,
											wc.detail,
										),
									)
								) {
									out.sessions.add(ws.sessionId);
								}
							} else if (wc.what === "synced" || wc.what === "active") {
								this.sessions.touch(ws.sessionId, Number(ev.at));
							}
						}
					}
					break;
				}
				case "command": {
					const cc = ev.change.value;
					const cmd = cc.command;
					if (cmd) {
						if (cc.what === "forgotten") {
							this.dropState(nodeId, "command", cmd.id);
						} else {
							this.putState(nodeId, "command", cmd.id, marshal(CommandSchema, cmd));
							if (cmd.kind === "freshen") {
								this.freshens.upsertFromCommand(nodeId, cmd, cc.what, cc.detail, Number(ev.at));
							}
							this.commands.add(cmd.id, cmd.sessionId, nodeId, cmd.workspaceId, Number(ev.at));
							if (cc.finished) {
								this.commands.addHistory(nodeId, cc.finished);
								out.finished = true;
							}
							const what: CommandUpdate_What = enumFromWord(CommandUpdate_WhatSchema, cc.what);
							if (what !== CommandUpdate_What.UNSPECIFIED) {
								if (
									this.sessions.recordEvent(
										cmd.sessionId,
										ev.at,
										commandUpdateEvent(what, cmd, cc.detail),
									)
								) {
									out.sessions.add(cmd.sessionId);
								}
							} else if (cc.what === "queued") {
								this.sessions.touch(cmd.sessionId, Number(ev.at));
							}
						}
					}
					break;
				}
			}
			out.applied = ev.seq;
			out.appliedAt = ev.at;
		}
	}

	private applyCheckpoint(
		nodeId: string,
		cp: Checkpoint,
		nowMs: number,
		creating: Set<string>,
		out: PushResult,
	): void {
		const old = this.view(nodeId);
		const keptWS = new Set(cp.workspaces.map((w) => w.id));
		const keptCmd = new Set(cp.commands.map((c) => c.id));
		const at = BigInt(nowMs);
		const lostWS = new Set<string>();

		for (const w of old.workspaces) {
			if (keptWS.has(w.id) || w.state === WorkspaceState.RELEASED || !w.sessionId) continue;
			lostWS.add(w.id);
			this.workspaces.release(w.id, nowMs);
			if (
				this.sessions.recordEvent(
					w.sessionId,
					at,
					workspaceUpdateEvent(WorkspaceUpdate_What.FAILED, w.id, LOST_DETAIL),
				)
			) {
				out.sessions.add(w.sessionId);
			}
		}

		const liveRows = this.sql
			.exec(
				"SELECT id, fork_millis FROM workspaces WHERE node_id = ? AND released_at IS NULL",
				nodeId,
			)
			.toArray();
		for (const w of liveRows) {
			const id = str(w.id);
			if (keptWS.has(id) || lostWS.has(id) || creating.has(id)) continue;
			if (w.fork_millis === null || w.fork_millis === undefined) {
				this.sql.exec("DELETE FROM workspaces WHERE id = ?", id);
			} else {
				this.workspaces.release(id, nowMs);
			}
		}

		for (const c of old.commands) {
			if (keptCmd.has(c.id) || !isCommandLive(c.state)) continue;
			out.lost.push(c.id);
			const lost = clone(CommandSchema, c);
			lost.state = CommandState.CANCELLED;
			if (
				this.sessions.recordEvent(
					c.sessionId,
					at,
					commandUpdateEvent(CommandUpdate_What.CANCELLED, lost, LOST_DETAIL),
				)
			) {
				out.sessions.add(c.sessionId);
			}
		}

		this.sql.exec("DELETE FROM node_state WHERE node_id = ?", nodeId);
		for (const b of cp.bases) {
			this.putState(nodeId, "base", b.id, marshal(BaseSchema, b));
			this.freshens.upsertFromBase(nodeId, b, "", "", nowMs);
		}
		for (const w of cp.workspaces) {
			this.putState(nodeId, "workspace", w.id, marshal(WorkspaceSchema, w));
			this.workspaces.note(nodeId, w);
		}
		for (const c of cp.commands) {
			this.putState(nodeId, "command", c.id, marshal(CommandSchema, c));
			if (c.kind === "freshen") {
				this.freshens.upsertFromCommand(nodeId, c, "", "", nowMs);
			}
			this.commands.add(c.id, c.sessionId, nodeId, c.workspaceId, Number(c.queuedAt));
		}
	}

	private putState(nodeId: string, kind: string, id: string, doc: string): void {
		this.sql.exec(
			"INSERT INTO node_state (node_id, kind, id, doc) VALUES (?, ?, ?, ?) ON CONFLICT (node_id, kind, id) DO UPDATE SET doc = excluded.doc",
			nodeId,
			kind,
			id,
			doc,
		);
	}

	private dropState(nodeId: string, kind: string, id: string): void {
		this.sql.exec(
			"DELETE FROM node_state WHERE node_id = ? AND kind = ? AND id = ?",
			nodeId,
			kind,
			id,
		);
	}

	view(nodeId: string): NodeStateView {
		const rows = this.sql
			.exec("SELECT kind, doc FROM node_state WHERE node_id = ? ORDER BY kind, id", nodeId)
			.toArray();
		const v: NodeStateView = { bases: [], workspaces: [], commands: [] };
		for (const r of rows) {
			this.foldRow(v, str(r.kind), str(r.doc));
		}
		return v;
	}

	allViews(): Map<string, NodeStateView> {
		const out = new Map<string, NodeStateView>();
		for (const r of this.sql
			.exec("SELECT node_id, kind, doc FROM node_state ORDER BY node_id, kind, id")
			.toArray()) {
			const nid = str(r.node_id);
			let v = out.get(nid);
			if (!v) {
				v = { bases: [], workspaces: [], commands: [] };
				out.set(nid, v);
			}
			this.foldRow(v, str(r.kind), str(r.doc));
		}
		return out;
	}

	private foldRow(v: NodeStateView, kind: string, doc: string): void {
		if (kind === "base") v.bases.push(unmarshal(BaseSchema, doc));
		else if (kind === "workspace") v.workspaces.push(unmarshal(WorkspaceSchema, doc));
		else if (kind === "command") v.commands.push(unmarshal(CommandSchema, doc));
	}

	toRelease(nodeId: string): string[] {
		return this.sql
			.exec(
				`SELECT s.id
				 FROM node_state s
				 JOIN workspaces w ON w.id = s.id
				 LEFT JOIN sessions sess ON sess.id = w.session_id
				 WHERE s.node_id = ?
				   AND s.kind = 'workspace'
				   AND (w.released_at IS NOT NULL OR sess.ended_at IS NOT NULL)
				   AND json_extract(s.doc, '$.state') IS NOT 'WORKSPACE_STATE_RELEASED'
				 ORDER BY s.id`,
				nodeId,
			)
			.toArray()
			.map((r) => str(r.id));
	}

	checkpoint(nodeId: string): Checkpoint {
		const r = this.sql.exec("SELECT applied FROM nodes WHERE id = ?", nodeId).one();
		const v = this.view(nodeId);
		return create(CheckpointSchema, {
			seq: big(r.applied),
			bases: v.bases,
			workspaces: v.workspaces,
			commands: v.commands,
		});
	}
}
