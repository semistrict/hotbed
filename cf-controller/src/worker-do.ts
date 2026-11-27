import { DurableObject } from "cloudflare:workers";
import { create } from "@bufbuild/protobuf";
import {
	ClusterEventSchema,
	DaemonLogEntrySchema,
	DaemonLogLevel,
	DaemonLogSourceSchema,
	EventRefSchema,
	type ClusterEvent,
	type DaemonLogEntry,
	type DaemonLogSource,
} from "./gen/hotbed/v1/hotbed_pb.ts";
import {
	executeReadOnlySql,
	inspectDoSchema,
	type DoSchemaInfo,
	type DoSqlResult,
	type SqlParamValue,
} from "./do-sql.ts";
import { big, num, str } from "./sql-values.ts";
import { KEEP_DAEMON_LOGS_MS } from "./store/daemon-logs.ts";
import { KEEP_SAMPLES_MS, type SampleRow } from "./store/fleet.ts";

/** How long INFO-level streamed slog entries are kept in each WorkerDO. */
export const KEEP_WORKER_INFO_LOGS_MS = 48 * 3600_000;
/** Hard row caps so a chatty worker can never exhaust its WorkerDO SQLite quota. */
const MAX_INFO_LOG_ROWS = 100_000;
const MAX_WARN_ERROR_LOG_ROWS = 50_000;
/** How many cluster/feed events each worker keeps in its own WorkerDO. */
export const KEEP_WORKER_EVENTS = 5_000;
/** Periodic background cleanup interval inside each WorkerDO. */
const MAINTENANCE_INTERVAL_MS = 6 * 3600_000;

export interface WorkerLogRecord {
	at: number;
	level: number;
	msg: string;
	attrsJson: string;
	sourceLoc: string;
	seq: number;
}

export interface WorkerLogSummaryDTO {
	hasStreamedLogs: boolean;
	errors24h: number;
	warns24h: number;
	lastAt: number;
	diskBytes: number;
	totalEntries: number;
}

export interface WorkerLogQueryResultDTO {
	hasStreamedLogs: boolean;
	entries: WorkerLogRecord[];
	summary: WorkerLogSummaryDTO;
	truncated: boolean;
}

export interface WorkerEventRecord {
	id?: number;
	at: number;
	kind: string;
	text: string;
	refType: string;
	refId: string;
}

/**
 * Deterministic Durable Object name for a worker, scoped to the owning
 * organization/cluster so two orgs can never collide on a node ID.
 */
export function workerDoName(clusterId: string, nodeId: string): string {
	return `worker:${clusterId}:${nodeId}`;
}

function createSchema(sql: SqlStorage): void {
	sql.exec(`
		CREATE TABLE IF NOT EXISTS worker_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS worker_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			at INTEGER NOT NULL,
			level INTEGER NOT NULL,
			msg TEXT NOT NULL,
			attrs_json TEXT NOT NULL DEFAULT '',
			source_loc TEXT NOT NULL DEFAULT '',
			epoch TEXT NOT NULL DEFAULT '',
			seq INTEGER NOT NULL DEFAULT 0
		);
		CREATE UNIQUE INDEX IF NOT EXISTS worker_logs_epoch_seq
			ON worker_logs (epoch, seq) WHERE seq > 0;
		CREATE INDEX IF NOT EXISTS worker_logs_at
			ON worker_logs (at DESC, id DESC);
		CREATE INDEX IF NOT EXISTS worker_logs_level_at
			ON worker_logs (level, at DESC, id DESC);
		CREATE TABLE IF NOT EXISTS worker_samples (
			t INTEGER PRIMARY KEY,
			cpu REAL NOT NULL,
			memory REAL NOT NULL,
			psi REAL NOT NULL,
			cpu_psi REAL NOT NULL DEFAULT 0,
			holding INTEGER NOT NULL DEFAULT 0,
			running INTEGER NOT NULL,
			queued INTEGER NOT NULL,
			parked INTEGER NOT NULL,
			oom INTEGER NOT NULL,
			r2_up INTEGER NOT NULL DEFAULT 0,
			r2_down INTEGER NOT NULL DEFAULT 0,
			r2_puts INTEGER NOT NULL DEFAULT 0,
			r2_gets INTEGER NOT NULL DEFAULT 0,
			r2_fails INTEGER NOT NULL DEFAULT 0,
			r2_wait_ns INTEGER NOT NULL DEFAULT 0,
			peer_given INTEGER NOT NULL DEFAULT 0,
			peer_served INTEGER NOT NULL DEFAULT 0,
			peer_given_bytes INTEGER NOT NULL DEFAULT 0,
			peer_served_bytes INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE IF NOT EXISTS worker_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			at INTEGER NOT NULL,
			kind TEXT NOT NULL,
			text TEXT NOT NULL,
			ref_type TEXT NOT NULL DEFAULT '',
			ref_id TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS worker_events_at
			ON worker_events (at DESC, id DESC);
		CREATE INDEX IF NOT EXISTS worker_events_kind_at
			ON worker_events (kind, at DESC, id DESC);
	`);
}

/**
 * Per-org, per-worker SQLite Durable Object holding high-volume worker-scoped
 * data outside the main ControllerDO:
 *   - Streamed worker slog records (INFO, WARN, ERROR)
 *   - 5-second heartbeat telemetry samples (CPU, memory, PSI, R2/peer counters)
 *   - Per-worker cluster/feed events (admit, wait, hold, fork, base capture, upgrade)
 *
 * Accessed strictly via typed Cloudflare Workers RPC (no HTTP fetch handler).
 * Each WorkerDO prunes its own tables on a periodic DO alarm.
 */
export class WorkerDO extends DurableObject<Env> {
	private schemaReady = false;
	private insertsSincePrune = 0;
	private alarmScheduledUntil = 0;

	private get sql(): SqlStorage {
		const sql = this.ctx.storage.sql;
		if (!this.schemaReady) {
			createSchema(sql);
			this.schemaReady = true;
		}
		return sql;
	}

	private hasTable(name: string): boolean {
		if (this.schemaReady) return true;
		const rows = this.ctx.storage.sql
			.exec("SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?", name)
			.toArray();
		return rows.length > 0;
	}

	private getMeta(key: string): string | null {
		if (!this.hasTable("worker_meta")) return null;
		const rows = this.ctx.storage.sql
			.exec("SELECT value FROM worker_meta WHERE key = ?", key)
			.toArray();
		return rows.length > 0 ? str(rows[0].value) : null;
	}

	private setMeta(key: string, value: string): void {
		this.sql.exec(
			`INSERT INTO worker_meta (key, value) VALUES (?, ?)
			 ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
			key,
			value,
		);
	}

	private async ensureAlarm(): Promise<void> {
		const now = Date.now();
		if (this.alarmScheduledUntil > now) {
			return;
		}
		const desired = now + MAINTENANCE_INTERVAL_MS;
		const existing = await this.ctx.storage.getAlarm();
		if (existing !== null && existing > now && existing <= desired + 60_000) {
			this.alarmScheduledUntil = existing;
			return;
		}
		await this.ctx.storage.setAlarm(desired);
		this.alarmScheduledUntil = desired;
	}

	private readLogSummary(nowMs: number): WorkerLogSummaryDTO {
		const hasStreamedLogs = this.getMeta("has_streamed_logs") === "1";
		if (!hasStreamedLogs || !this.hasTable("worker_logs")) {
			return {
				hasStreamedLogs: false,
				errors24h: 0,
				warns24h: 0,
				lastAt: 0,
				diskBytes: 0,
				totalEntries: 0,
			};
		}
		const since = nowMs - 24 * 3600_000;
		const row = this.ctx.storage.sql
			.exec(
				`SELECT
					SUM(CASE WHEN level = ? AND at >= ? THEN 1 ELSE 0 END) AS errs,
					SUM(CASE WHEN level = ? AND at >= ? THEN 1 ELSE 0 END) AS warns,
					MAX(CASE WHEN level IN (?, ?) THEN at ELSE NULL END) AS last_warn_err_at,
					MAX(at) AS last_any_at,
					COUNT(*) AS cnt
				 FROM worker_logs`,
				DaemonLogLevel.ERROR,
				since,
				DaemonLogLevel.WARN,
				since,
				DaemonLogLevel.WARN,
				DaemonLogLevel.ERROR,
			)
			.one();
		return {
			hasStreamedLogs: true,
			errors24h: num(row.errs),
			warns24h: num(row.warns),
			lastAt: num(row.last_warn_err_at ?? row.last_any_at ?? 0),
			diskBytes: this.ctx.storage.sql.databaseSize,
			totalEntries: num(row.cnt),
		};
	}

	async appendLogs(params: {
		nodeId: string;
		host: string;
		epoch: string;
		entries: WorkerLogRecord[];
		nowMs?: number;
	}): Promise<{ ackedSeq: number }> {
		const nowMs = params.nowMs ?? Date.now();
		let ackedSeq = 0;
		this.ctx.storage.transactionSync(() => {
			if (params.nodeId) this.setMeta("node_id", params.nodeId);
			if (params.host) this.setMeta("hostname", params.host);
			this.setMeta("has_streamed_logs", "1");
			for (const e of params.entries) {
				const at = e.at > 0 ? e.at : nowMs;
				this.sql.exec(
					`INSERT OR IGNORE INTO worker_logs (at, level, msg, attrs_json, source_loc, epoch, seq)
					 VALUES (?, ?, ?, ?, ?, ?, ?)`,
					at,
					e.level,
					e.msg,
					e.attrsJson,
					e.sourceLoc,
					params.epoch,
					e.seq,
				);
				if (e.seq > ackedSeq) ackedSeq = e.seq;
			}
			if (params.epoch && ackedSeq === 0) {
				const row = this.sql
					.exec("SELECT COALESCE(MAX(seq), 0) AS s FROM worker_logs WHERE epoch = ?", params.epoch)
					.one();
				ackedSeq = num(row.s);
			}
			this.insertsSincePrune += params.entries.length;
			if (this.insertsSincePrune >= 2000) {
				this.insertsSincePrune = 0;
				this.pruneLogsLocked(nowMs);
			}
		});
		await this.ensureAlarm();
		return { ackedSeq };
	}

	queryLogs(params: {
		level: number;
		includeInfo: boolean;
		query: string;
		fromMs: number;
		toMs: number;
		limit: number;
		nowMs?: number;
	}): WorkerLogQueryResultDTO {
		const nowMs = params.nowMs ?? Date.now();
		const summary = this.readLogSummary(nowMs);
		if (!summary.hasStreamedLogs || !this.hasTable("worker_logs")) {
			return { hasStreamedLogs: false, entries: [], summary, truncated: false };
		}

		const clauses: string[] = [];
		const bind: unknown[] = [];
		if (params.level !== DaemonLogLevel.UNSPECIFIED) {
			clauses.push("level = ?");
			bind.push(params.level);
		} else if (!params.includeInfo) {
			clauses.push("level IN (?, ?)");
			bind.push(DaemonLogLevel.WARN, DaemonLogLevel.ERROR);
		}
		if (params.fromMs > 0) {
			clauses.push("at >= ?");
			bind.push(params.fromMs);
		}
		if (params.toMs > 0) {
			clauses.push("at <= ?");
			bind.push(params.toMs);
		}
		const qLower = params.query.trim().toLowerCase();
		if (qLower) {
			clauses.push(
				"(instr(lower(msg), ?) > 0 OR instr(lower(attrs_json), ?) > 0 OR instr(lower(source_loc), ?) > 0)",
			);
			bind.push(qLower, qLower, qLower);
		}
		const where = clauses.length > 0 ? `WHERE ${clauses.join(" AND ")}` : "";
		const rows = this.ctx.storage.sql
			.exec(
				`SELECT at, level, msg, attrs_json, source_loc, seq
				 FROM worker_logs ${where}
				 ORDER BY at DESC, id DESC
				 LIMIT ?`,
				...bind,
				params.limit + 1,
			)
			.toArray();

		const entries: WorkerLogRecord[] = [];
		let truncated = false;
		for (const r of rows) {
			if (entries.length >= params.limit) {
				truncated = true;
				break;
			}
			entries.push({
				at: num(r.at),
				level: num(r.level),
				msg: str(r.msg),
				attrsJson: str(r.attrs_json),
				sourceLoc: str(r.source_loc),
				seq: num(r.seq),
			});
		}
		return { hasStreamedLogs: true, entries, summary, truncated };
	}

	logSummary(nowMs = Date.now()): WorkerLogSummaryDTO {
		return this.readLogSummary(nowMs);
	}

	async recordSample(params: {
		nodeId: string;
		host: string;
		sample: Omit<SampleRow, "node">;
		nowMs?: number;
	}): Promise<void> {
		const s = params.sample;
		this.ctx.storage.transactionSync(() => {
			if (params.nodeId) this.setMeta("node_id", params.nodeId);
			if (params.host) this.setMeta("hostname", params.host);
			this.sql.exec(
				`INSERT OR REPLACE INTO worker_samples (
					t, cpu, memory, psi, cpu_psi, holding, running, queued, parked, oom,
					r2_up, r2_down, r2_puts, r2_gets, r2_fails, r2_wait_ns,
					peer_given, peer_served, peer_given_bytes, peer_served_bytes
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				s.t,
				s.cpu,
				s.memory,
				s.psi,
				s.cpuPsi,
				s.holding ? 1 : 0,
				s.running,
				s.queued,
				s.parked,
				s.oom,
				s.r2Up,
				s.r2Down,
				s.r2Puts,
				s.r2Gets,
				s.r2Fails,
				s.r2WaitNs,
				s.peerGiven,
				s.peerServed,
				s.peerGivenBytes,
				s.peerServedBytes,
			);
		});
		await this.ensureAlarm();
	}

	samplesSince(sinceMs: number): Array<Omit<SampleRow, "node">> {
		if (!this.hasTable("worker_samples")) return [];
		return this.ctx.storage.sql
			.exec(
				`SELECT t, cpu, memory, psi, cpu_psi, holding, running, queued, parked, oom,
				        r2_up, r2_down, r2_puts, r2_gets, r2_fails, r2_wait_ns,
				        peer_given, peer_served, peer_given_bytes, peer_served_bytes
				 FROM worker_samples WHERE t >= ? ORDER BY t`,
				sinceMs,
			)
			.toArray()
			.map((r) => ({
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

	async appendEvents(params: {
		nodeId: string;
		host: string;
		events: WorkerEventRecord[];
	}): Promise<void> {
		if (params.events.length === 0) return;
		this.ctx.storage.transactionSync(() => {
			if (params.nodeId) this.setMeta("node_id", params.nodeId);
			if (params.host) this.setMeta("hostname", params.host);
			for (const ev of params.events) {
				this.sql.exec(
					`INSERT INTO worker_events (at, kind, text, ref_type, ref_id)
					 VALUES (?, ?, ?, ?, ?)`,
					ev.at,
					ev.kind,
					ev.text,
					ev.refType,
					ev.refId,
				);
			}
		});
		await this.ensureAlarm();
	}

	recentEvents(params: { kinds?: string[]; limit: number }): WorkerEventRecord[] {
		if (!this.hasTable("worker_events")) return [];
		const kinds = (params.kinds ?? []).filter(Boolean);
		const limit = Math.max(1, params.limit);
		let rows: Array<Record<string, SqlStorageValue>>;
		if (kinds.length > 0) {
			const placeholders = kinds.map(() => "?").join(", ");
			rows = this.ctx.storage.sql
				.exec(
					`SELECT id, at, kind, text, ref_type, ref_id
					 FROM worker_events
					 WHERE kind IN (${placeholders})
					 ORDER BY at DESC, id DESC
					 LIMIT ?`,
					...kinds,
					limit,
				)
				.toArray();
		} else {
			rows = this.ctx.storage.sql
				.exec(
					`SELECT id, at, kind, text, ref_type, ref_id
					 FROM worker_events
					 ORDER BY at DESC, id DESC
					 LIMIT ?`,
					limit,
				)
				.toArray();
		}
		return rows.map((r) => ({
			id: num(r.id),
			at: num(r.at),
			kind: str(r.kind),
			text: str(r.text),
			refType: str(r.ref_type),
			refId: str(r.ref_id),
		}));
	}

	async markRetired(atMs: number): Promise<void> {
		if (!this.hasTable("worker_meta")) return;
		this.setMeta("retired_at", String(atMs));
		await this.ensureAlarm();
	}

	private pruneLogsLocked(nowMs: number): void {
		if (!this.hasTable("worker_logs")) return;
		this.sql.exec(
			"DELETE FROM worker_logs WHERE level = ? AND at < ?",
			DaemonLogLevel.INFO,
			nowMs - KEEP_WORKER_INFO_LOGS_MS,
		);
		this.sql.exec(
			"DELETE FROM worker_logs WHERE level != ? AND at < ?",
			DaemonLogLevel.INFO,
			nowMs - KEEP_DAEMON_LOGS_MS,
		);
		this.sql.exec(
			`DELETE FROM worker_logs
			 WHERE level = ?
			   AND id NOT IN (
				 SELECT id FROM worker_logs WHERE level = ? ORDER BY at DESC, id DESC LIMIT ?
			   )`,
			DaemonLogLevel.INFO,
			DaemonLogLevel.INFO,
			MAX_INFO_LOG_ROWS,
		);
		this.sql.exec(
			`DELETE FROM worker_logs
			 WHERE level != ?
			   AND id NOT IN (
				 SELECT id FROM worker_logs WHERE level != ? ORDER BY at DESC, id DESC LIMIT ?
			   )`,
			DaemonLogLevel.INFO,
			DaemonLogLevel.INFO,
			MAX_WARN_ERROR_LOG_ROWS,
		);
	}

	pruneNow(nowMs = Date.now()): void {
		if (!this.hasTable("worker_meta")) return;
		this.ctx.storage.transactionSync(() => {
			this.pruneLogsLocked(nowMs);
			if (this.hasTable("worker_samples")) {
				this.sql.exec("DELETE FROM worker_samples WHERE t < ?", nowMs - KEEP_SAMPLES_MS);
			}
			if (this.hasTable("worker_events")) {
				this.sql.exec(
					`DELETE FROM worker_events
					 WHERE id <= (SELECT COALESCE(MAX(id), 0) FROM worker_events) - ?`,
					KEEP_WORKER_EVENTS,
				);
			}
		});
	}

	override async alarm(): Promise<void> {
		const nowMs = Date.now();
		this.alarmScheduledUntil = 0;
		this.pruneNow(nowMs);
		const retiredAt = num(this.getMeta("retired_at") ?? 0);
		if (retiredAt > 0 && nowMs - retiredAt >= KEEP_DAEMON_LOGS_MS) {
			await this.ctx.storage.deleteAll();
			this.schemaReady = false;
			return;
		}
		const hasAnyData =
			(this.hasTable("worker_logs") &&
				this.ctx.storage.sql.exec("SELECT 1 FROM worker_logs LIMIT 1").toArray().length > 0) ||
			(this.hasTable("worker_samples") &&
				this.ctx.storage.sql.exec("SELECT 1 FROM worker_samples LIMIT 1").toArray().length > 0) ||
			(this.hasTable("worker_events") &&
				this.ctx.storage.sql.exec("SELECT 1 FROM worker_events LIMIT 1").toArray().length > 0);
		if (hasAnyData || retiredAt > 0) {
			const next = nowMs + MAINTENANCE_INTERVAL_MS;
			await this.ctx.storage.setAlarm(next);
			this.alarmScheduledUntil = next;
		}
	}

	/**
	 * Read-only SQL query executed via typed Workers RPC by the Admin Agent.
	 * Uses `this.ctx.storage.sql` directly so inspecting a nonexistent WorkerDO
	 * does not materialize empty tables.
	 */
	adminQuerySql(
		query: string,
		params: SqlParamValue[] = [],
		opts?: { maxRows?: number },
	): DoSqlResult {
		return executeReadOnlySql(this.ctx.storage.sql, query, params, opts);
	}

	/**
	 * Schema inspection executed via typed Workers RPC by the Admin Agent.
	 * Does not materialize empty tables on nonexistent WorkerDOs.
	 */
	adminInspectSchema(): Promise<DoSchemaInfo> {
		return inspectDoSchema(this.ctx.storage.sql, this.ctx.storage);
	}
}

export interface WorkerLogQueryResult {
	hasStreamedLogs: boolean;
	entries: DaemonLogEntry[];
	source: DaemonLogSource;
	truncated: boolean;
}

function dtoToLogSource(host: string, dto: WorkerLogSummaryDTO): DaemonLogSource {
	return create(DaemonLogSourceSchema, {
		source: host,
		role: "worker",
		host,
		reachable: true,
		errors24h: dto.errors24h,
		warns24h: dto.warns24h,
		lastAt: dto.lastAt > 0 ? BigInt(dto.lastAt) : undefined,
		diskBytes: BigInt(dto.diskBytes),
		files: 1,
		maxAgeDays: 14,
	});
}

/**
 * Typed client used by ControllerDO services to read/write per-worker data in
 * each worker's dedicated SQLite WorkerDO via Workers RPC.
 *
 * Always creates a fresh stub per RPC call and never caches in-flight Promises.
 */
export class WorkerStoreClient {
	private readonly ns: DurableObjectNamespace<WorkerDO>;
	private readonly clusterId: string;

	constructor(ns: DurableObjectNamespace<WorkerDO>, clusterId: string) {
		this.ns = ns;
		this.clusterId = clusterId;
	}

	private stub(nodeId: string): DurableObjectStub<WorkerDO> {
		return this.ns.getByName(workerDoName(this.clusterId, nodeId));
	}

	async appendLogs(
		nodeId: string,
		host: string,
		epoch: string,
		entries: DaemonLogEntry[],
		nowMs = Date.now(),
	): Promise<{ ackedSeq: number }> {
		const mapped: WorkerLogRecord[] = entries.map((e) => ({
			at: Number(e.at),
			level: e.level,
			msg: e.msg,
			attrsJson: e.attrsJson,
			sourceLoc: e.sourceLoc,
			seq: Number(e.seq),
		}));
		return this.stub(nodeId).appendLogs({ nodeId, host, epoch, entries: mapped, nowMs });
	}

	async queryLogs(
		nodeId: string,
		host: string,
		params: {
			level: DaemonLogLevel;
			includeInfo: boolean;
			query: string;
			fromMs: number;
			toMs: number;
			limit: number;
			nowMs: number;
		},
	): Promise<WorkerLogQueryResult> {
		const res = await this.stub(nodeId).queryLogs({
			level: params.level,
			includeInfo: params.includeInfo,
			query: params.query,
			fromMs: params.fromMs,
			toMs: params.toMs,
			limit: params.limit,
			nowMs: params.nowMs,
		});
		return {
			hasStreamedLogs: res.hasStreamedLogs,
			entries: res.entries.map((e) =>
				create(DaemonLogEntrySchema, {
					at: BigInt(e.at),
					level: e.level as DaemonLogLevel,
					source: host,
					role: "worker",
					host,
					msg: e.msg,
					attrsJson: e.attrsJson,
					sourceLoc: e.sourceLoc,
					seq: BigInt(e.seq),
				}),
			),
			source: dtoToLogSource(host, res.summary),
			truncated: res.truncated,
		};
	}

	async logSummary(
		nodeId: string,
		host: string,
		nowMs = Date.now(),
	): Promise<{ hasStreamedLogs: boolean; source: DaemonLogSource }> {
		const dto = await this.stub(nodeId).logSummary(nowMs);
		return {
			hasStreamedLogs: dto.hasStreamedLogs,
			source: dtoToLogSource(host, dto),
		};
	}

	async recordSample(nodeId: string, host: string, sample: SampleRow): Promise<void> {
		const { node: _node, ...rest } = sample;
		await this.stub(nodeId).recordSample({ nodeId, host, sample: rest, nowMs: sample.t });
	}

	async samplesSince(nodeId: string, sinceMs: number): Promise<SampleRow[]> {
		const rows = await this.stub(nodeId).samplesSince(sinceMs);
		return rows.map((r) => ({ ...r, node: nodeId }));
	}

	async appendEvents(nodeId: string, host: string, events: WorkerEventRecord[]): Promise<void> {
		if (events.length === 0) return;
		await this.stub(nodeId).appendEvents({ nodeId, host, events });
	}

	async recentEvents(
		nodeId: string,
		hostname: string,
		opts: { kinds?: string[]; limit: number },
	): Promise<ClusterEvent[]> {
		const rows = await this.stub(nodeId).recentEvents(opts);
		return rows.map((r) =>
			create(ClusterEventSchema, {
				id: big(r.id ?? 0),
				at: BigInt(r.at),
				kind: r.kind,
				nodeId,
				hostname,
				text: r.text,
				ref:
					r.refType || r.refId
						? create(EventRefSchema, { type: r.refType, id: r.refId })
						: undefined,
			}),
		);
	}

	async markRetired(nodeId: string, atMs: number): Promise<void> {
		await this.stub(nodeId).markRetired(atMs);
	}
}
