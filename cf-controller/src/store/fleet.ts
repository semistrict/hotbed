import {
	ClusterEventSchema,
	EventRefSchema,
	HeartbeatRequestSchema,
	NodeSchema,
	PlacementDecisionSchema,
	type ClusterEvent,
	type HeartbeatRequest,
	type Node,
	type PlacementDecision,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import { big, num, str } from "../sql-values.ts";
import { ErrorCode, hotbedError } from "../errors.ts";
import { create, clone } from "@bufbuild/protobuf";
import type { NodeRow } from "../placement.ts";
import { marshal, unmarshal, optBig, type Row, type SqlKernel } from "./kernel.ts";
import type { SessionStore } from "./sessions.ts";

export const KEEP_SAMPLES_MS = 25 * 3600_000;
export const KEEP_PLACEMENTS_MS = 24 * 3600_000;
export const KEEP_EVENTS = 5000;

/** One node's heartbeat sample: its load and what its object store and peers moved. */
export interface SampleRow {
	node: string;
	t: number;
	cpu: number;
	memory: number;
	psi: number;
	cpuPsi: number;
	holding: boolean;
	running: number;
	queued: number;
	parked: number;
	oom: number;
	r2Up: number;
	r2Down: number;
	r2Puts: number;
	r2Gets: number;
	r2Fails: number;
	r2WaitNs: number;
	peerGiven: number;
	peerServed: number;
	peerGivenBytes: number;
	peerServedBytes: number;
}

function toClusterEvent(r: Row): ClusterEvent {
	return create(ClusterEventSchema, {
		id: big(r.id),
		at: big(r.at),
		kind: str(r.kind),
		nodeId: str(r.node_id),
		text: str(r.text),
		ref:
			str(r.ref_type) || str(r.ref_id)
				? create(EventRefSchema, { type: str(r.ref_type), id: str(r.ref_id) })
				: undefined,
	});
}

/** The worker nodes: their rows and heartbeats, the samples those leave, cluster events and placement decisions. */
export class FleetStore {
	constructor(
		private readonly sql: SqlKernel,
		private readonly sessions: SessionStore,
	) {}

	join(id: string, hostname: string, login: string, nowMs: number): Node {
		this.sql.exec(
			`INSERT INTO nodes (id, hostname, login, joined_at)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT (id) DO UPDATE SET
			   hostname = excluded.hostname,
			   login = excluded.login,
			   retired_at = NULL`,
			id,
			hostname,
			login,
			nowMs,
		);
		return this.get(id).node;
	}

	retire(id: string, atMs: number): void {
		this.sql.tx(() => {
			this.sql.exec("UPDATE nodes SET retired_at = ? WHERE id = ?", atMs, id);
			// A node gone for good holds no base any more.
			this.sql.exec("DELETE FROM shared_base_holders WHERE node_id = ?", id);
		});
	}

	has(id: string): boolean {
		return this.sql.exec("SELECT 1 FROM nodes WHERE id = ?", id).toArray().length > 0;
	}

	list(): NodeRow[] {
		const rows = this.sql
			.exec(
				"SELECT id, hostname, login, joined_at, last_seen, heartbeat FROM nodes WHERE retired_at IS NULL ORDER BY hostname, id",
			)
			.toArray();
		return rows.map((r) => this.toNodeRow(r));
	}

	get(id: string): NodeRow {
		const rows = this.sql
			.exec(
				"SELECT id, hostname, login, joined_at, last_seen, heartbeat FROM nodes WHERE id = ?",
				id,
			)
			.toArray();
		if (rows.length === 0) {
			throw hotbedError(ErrorCode.NotFound, `no node ${id}`);
		}
		return this.toNodeRow(rows[0]);
	}

	private toNodeRow(r: Row): NodeRow {
		const node = create(NodeSchema, {
			id: str(r.id),
			hostname: str(r.hostname),
			login: str(r.login),
			joinedAt: big(r.joined_at),
			lastSeen: optBig(r.last_seen),
		});
		let hb: HeartbeatRequest | undefined;
		if (typeof r.heartbeat === "string" && r.heartbeat) {
			hb = unmarshal(HeartbeatRequestSchema, r.heartbeat);
			node.resources = hb.resources;
			node.usage = hb.usage;
			node.diskBytes = hb.diskBytes;
			node.disk = hb.disk;
			node.reservedMemoryBytes = hb.reservedMemoryBytes;
			node.storage = hb.storage;
			node.profiler = hb.profiler;
			node.daemonLogs = hb.daemonLogs;
			node.admission = hb.admission;
			node.storageError = hb.storageError;
			node.objectStore = hb.objectStore;
		}
		return { node, hb };
	}

	beat(nodeId: string, h: HeartbeatRequest, nowMs: number): void {
		this.sql.tx(() => {
			if (!this.has(nodeId)) {
				throw hotbedError(ErrorCode.NotJoined, `node ${nodeId} has not joined this cluster`);
			}
			const state = clone(HeartbeatRequestSchema, h);
			state.activeSessions = [];
			this.sql.exec(
				"UPDATE nodes SET last_seen = ?, heartbeat = ? WHERE id = ?",
				nowMs,
				marshal(HeartbeatRequestSchema, state),
				nodeId,
			);
			const counts = this.sql
				.exec(
					`SELECT
						COALESCE(SUM(CASE WHEN json_extract(doc, '$.state') = 'COMMAND_STATE_RUNNING' THEN 1 ELSE 0 END), 0) AS running,
						COALESCE(SUM(CASE WHEN json_extract(doc, '$.state') = 'COMMAND_STATE_QUEUED' THEN 1 ELSE 0 END), 0) AS queued,
						COALESCE(SUM(CASE WHEN json_extract(doc, '$.state') IN ('COMMAND_STATE_PARKED', 'COMMAND_STATE_PAUSED') THEN 1 ELSE 0 END), 0) AS parked
					 FROM node_state WHERE node_id = ? AND kind = 'command'`,
					nodeId,
				)
				.one();
			const mb = Number(h.resources?.memoryBytes ?? 0n);
			const memFrac = mb > 0 ? Number(h.usage?.memoryUsedBytes ?? 0n) / mb : 0;
			const os = h.objectStore;
			this.sql.exec(
				"INSERT OR REPLACE INTO samples (node_id, t, cpu, memory, psi, cpu_psi, holding, running, queued, parked, oom, r2_up, r2_down, r2_puts, r2_gets, r2_fails, r2_wait_ns, peer_given, peer_served, peer_given_bytes, peer_served_bytes) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
				nodeId,
				nowMs,
				h.usage?.cpuBusy ?? 0,
				memFrac,
				h.usage?.memoryPressure ?? 0,
				h.usage?.cpuPressure ?? 0,
				h.admission?.holding ? 1 : 0,
				num(counts.running),
				num(counts.queued),
				num(counts.parked),
				h.oomKills,
				os?.uploadedBytes ?? 0n,
				os?.downloadedBytes ?? 0n,
				os?.putRequests ?? 0n,
				os?.getRequests ?? 0n,
				os?.failedRequests ?? 0n,
				os?.commitWaitNs ?? 0n,
				os?.peerPartsGiven ?? 0n,
				os?.peerPartsServed ?? 0n,
				os?.peerGivenBytes ?? 0n,
				os?.peerServedBytes ?? 0n,
			);
			for (const session of h.activeSessions) {
				this.sessions.touch(session, nowMs);
			}
		});
	}

	samplesSince(sinceMs: number): SampleRow[] {
		return this.sql
			.exec(
				"SELECT node_id, t, cpu, memory, psi, cpu_psi, holding, running, queued, parked, oom, r2_up, r2_down, r2_puts, r2_gets, r2_fails, r2_wait_ns, peer_given, peer_served, peer_given_bytes, peer_served_bytes FROM samples WHERE t >= ? ORDER BY t",
				sinceMs,
			)
			.toArray()
			.map((r) => ({
				node: str(r.node_id),
				t: num(r.t),
				cpu: num(r.cpu),
				memory: num(r.memory),
				psi: num(r.psi),
				cpuPsi: num(r.cpu_psi),
				holding: num(r.holding) !== 0,
				running: num(r.running),
				queued: num(r.queued),
				parked: num(r.parked),
				oom: num(r.oom),
				r2Up: num(r.r2_up),
				r2Down: num(r.r2_down),
				r2Puts: num(r.r2_puts),
				r2Gets: num(r.r2_gets),
				r2Fails: num(r.r2_fails),
				r2WaitNs: num(r.r2_wait_ns),
				peerGiven: num(r.peer_given),
				peerServed: num(r.peer_served),
				peerGivenBytes: num(r.peer_given_bytes),
				peerServedBytes: num(r.peer_served_bytes),
			}));
	}

	recordEvent(
		nodeId: string,
		kind: string,
		text: string,
		nowMs: number,
		refType = "",
		refId = "",
	): void {
		this.sql.exec(
			"INSERT INTO events (at, kind, node_id, text, ref_type, ref_id) VALUES (?, ?, ?, ?, ?, ?)",
			nowMs,
			kind,
			nodeId,
			text,
			refType,
			refId,
		);
	}

	/** A node's events, newest first, read only as far as the caller iterates. */
	*nodeEventsNewestFirst(nodeId: string): Generator<ClusterEvent> {
		const cursor = this.sql.exec(
			"SELECT id, at, kind, node_id, text, ref_type, ref_id FROM events WHERE node_id = ? ORDER BY id DESC",
			nodeId,
		);
		for (const r of cursor) yield toClusterEvent(r);
	}

	recentEvents(limit: number): ClusterEvent[] {
		return this.sql
			.exec(
				"SELECT id, at, kind, node_id, text, ref_type, ref_id FROM events ORDER BY id DESC LIMIT ?",
				limit,
			)
			.toArray()
			.map(toClusterEvent);
	}

	addPlacement(d: PlacementDecision, nowMs: number): void {
		d.at = BigInt(nowMs);
		const stored = clone(PlacementDecisionSchema, d);
		stored.id = 0n;
		const id = this.sql.insert(
			"INSERT INTO placements (at, session_id, doc) VALUES (?, ?, ?)",
			nowMs,
			d.sessionId,
			marshal(PlacementDecisionSchema, stored),
		);
		d.id = BigInt(id);
	}

	placementsSince(sinceMs: number): PlacementDecision[] {
		return this.sql
			.exec("SELECT id, at, doc FROM placements WHERE at >= ? ORDER BY id DESC", sinceMs)
			.toArray()
			.map((r) => {
				const d = unmarshal(PlacementDecisionSchema, str(r.doc));
				d.id = big(r.id);
				d.at = big(r.at);
				return d;
			});
	}

	prune(nowMs: number): void {
		this.sql.exec("DELETE FROM samples WHERE t < ?", nowMs - KEEP_SAMPLES_MS);
		this.sql.exec(
			"DELETE FROM events WHERE id <= (SELECT MAX(e.id) FROM events e) - ?",
			KEEP_EVENTS,
		);
		this.sql.exec("DELETE FROM placements WHERE at < ?", nowMs - KEEP_PLACEMENTS_MS);
	}
}
