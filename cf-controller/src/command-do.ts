import { DurableObject } from "cloudflare:workers";
import { create, fromBinary, fromJsonString, toBinary, toJsonString } from "@bufbuild/protobuf";
import {
	CommandDiagnosticsSchema,
	CommandLogSchema,
	CommandProfileSchema,
	CommandSampleSchema,
	type CommandDiagnostics,
	type CommandLog,
	type CommandOutputChunk,
	type CommandProfile,
	type CommandSample,
} from "./gen/hotbed/v1/hotbed_pb.ts";
import { extractLogLines, type TimedOutputChunk } from "./settle.ts";
import {
	executeReadOnlySql,
	inspectDoSchema,
	type DoSchemaInfo,
	type DoSqlResult,
	type SqlParamValue,
} from "./do-sql.ts";
import { num, str } from "./sql-values.ts";
import { KEEP_HISTORY_MS } from "./store/index.ts";

// Bounded per-CommandDO output storage: keep up to 32 MiB total (2 MiB head + 30 MiB tail)
// so a runaway command can never exhaust a Durable Object's SQLite quota.
const MAX_STORED_OUTPUT_BYTES = 32 << 20;
const KEEP_HEAD_OUTPUT_BYTES = 2 << 20;
const KEEP_TAIL_OUTPUT_BYTES = 28 << 20;
const DEFAULT_LOG_LINES_MAX_BYTES = 1 << 20;
/** How many of a command's activity samples are kept: its newest. */
const MAX_COMMAND_SAMPLES = 32;

export interface StoredOutputLog {
	tail: number;
	cut: boolean;
	doc: Uint8Array;
}

export interface CommandOutputCache {
	tail: number;
	cut: boolean;
	log: CommandLog;
}

export interface CommandOutputStreamRead {
	chunks: TimedOutputChunk[];
	eof: boolean;
	hasOutput: boolean;
}

export interface CommandOutputLinesResult {
	lines: string[];
	lineTimestamps: number[];
	cut: boolean;
	eof: boolean;
	hasOutput: boolean;
}

function toUint8Array(raw: SqlStorageValue | undefined): Uint8Array | null {
	if (raw instanceof ArrayBuffer) return new Uint8Array(raw);
	return null;
}

/**
 * Deterministic Durable Object name for a command, scoped to the owning
 * cluster/organization so two orgs can never collide on a command ID.
 */
export function commandDoName(clusterId: string, commandId: string): string {
	return `cmd:${clusterId}:${commandId}`;
}

function createSchema(sql: SqlStorage): void {
	sql.exec(`
		CREATE TABLE IF NOT EXISTS command_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS command_profile (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			node_id TEXT NOT NULL,
			ended_at INTEGER NOT NULL,
			doc BLOB NOT NULL
		);
		CREATE TABLE IF NOT EXISTS command_diagnostics (
			seq INTEGER PRIMARY KEY,
			node_id TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			doc TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS command_samples (
			seq INTEGER PRIMARY KEY,
			node_id TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			meta TEXT NOT NULL,
			doc BLOB NOT NULL
		);
		CREATE TABLE IF NOT EXISTS command_output (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			tail INTEGER NOT NULL,
			cut INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			doc BLOB NOT NULL
		);
		CREATE TABLE IF NOT EXISTS command_output_chunks (
			offset INTEGER PRIMARY KEY,
			next_offset INTEGER NOT NULL,
			received_at INTEGER NOT NULL,
			data BLOB NOT NULL
		);
	`);
}

/**
 * Per-command SQLite Durable Object storing heavy per-command data outside the
 * main ControllerDO: continuous eBPF profile, intervention diagnostics, kept
 * activity samples, and cached ended command output.
 *
 * Accessed strictly via typed Cloudflare Workers RPC (no HTTP fetch handler).
 * Each instance schedules its own DO alarm to deleteAll() after KEEP_HISTORY_MS.
 */
export class CommandDO extends DurableObject<Env> {
	private schemaReady = false;

	/**
	 * The command's SQLite, its tables created on first use: after a cold
	 * start, and again after deleteAll() (clear, expiry) wiped them while this
	 * instance stays alive. Expiry leaves no empty tables behind.
	 */
	private get sql(): SqlStorage {
		const sql = this.ctx.storage.sql;
		if (!this.schemaReady) {
			createSchema(sql);
			this.schemaReady = true;
		}
		return sql;
	}

	private async deleteAll(): Promise<void> {
		await this.ctx.storage.deleteAll();
		this.schemaReady = false;
	}

	private async scheduleExpiry(nowMs: number, ttlMs = KEEP_HISTORY_MS): Promise<void> {
		const desired = Math.max(Date.now(), nowMs) + ttlMs;
		const rows = this.sql.exec("SELECT value FROM command_meta WHERE key = 'expires_at'").toArray();
		const current = rows.length > 0 ? num(rows[0].value) : 0;
		const nextExpiry = Math.max(current, desired);
		if (nextExpiry > current) {
			this.sql.exec(
				`INSERT INTO command_meta (key, value) VALUES ('expires_at', ?)
				 ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
				String(nextExpiry),
			);
		}
		const alarm = await this.ctx.storage.getAlarm();
		if (alarm === null || alarm < nextExpiry) {
			await this.ctx.storage.setAlarm(nextExpiry);
		}
	}

	async expireNowForTest(): Promise<void> {
		const past = Date.now() - 1000;
		this.sql.exec(
			`INSERT INTO command_meta (key, value) VALUES ('expires_at', ?)
			 ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
			String(past),
		);
		await this.ctx.storage.setAlarm(past);
	}

	async saveProfile(params: {
		nodeId: string;
		endedAtMs: number;
		doc: Uint8Array;
		nowMs?: number;
	}): Promise<void> {
		this.sql.exec(
			`INSERT INTO command_profile (id, node_id, ended_at, doc) VALUES (1, ?, ?, ?)
			 ON CONFLICT (id) DO UPDATE SET node_id = excluded.node_id, ended_at = excluded.ended_at, doc = excluded.doc`,
			params.nodeId,
			params.endedAtMs,
			params.doc,
		);
		await this.scheduleExpiry(params.nowMs ?? params.endedAtMs ?? Date.now());
	}

	getProfile(): Uint8Array | null {
		const rows = this.sql.exec("SELECT doc FROM command_profile WHERE id = 1").toArray();
		if (rows.length === 0) return null;
		return toUint8Array(rows[0].doc);
	}

	async saveDiagnostics(params: {
		nodeId: string;
		seq: number;
		createdAtMs: number;
		doc: string;
		nowMs?: number;
	}): Promise<void> {
		this.sql.exec(
			`INSERT INTO command_diagnostics (seq, node_id, created_at, doc) VALUES (?, ?, ?, ?)
			 ON CONFLICT (seq) DO UPDATE SET node_id = excluded.node_id, created_at = excluded.created_at, doc = excluded.doc`,
			params.seq,
			params.nodeId,
			params.createdAtMs,
			params.doc,
		);
		await this.scheduleExpiry(params.nowMs ?? params.createdAtMs ?? Date.now());
	}

	getDiagnostic(seq: number): string | null {
		const rows = this.sql.exec("SELECT doc FROM command_diagnostics WHERE seq = ?", seq).toArray();
		return rows.length > 0 ? str(rows[0].doc) : null;
	}

	listDiagnosticsAfter(afterSeq: number): string[] {
		return this.sql
			.exec("SELECT doc FROM command_diagnostics WHERE seq > ? ORDER BY seq", afterSeq)
			.toArray()
			.map((r) => str(r.doc));
	}

	hasDiagnostics(): boolean {
		return this.sql.exec("SELECT 1 FROM command_diagnostics LIMIT 1").toArray().length > 0;
	}

	async saveSample(params: {
		nodeId: string;
		seq: number;
		createdAtMs: number;
		meta: string;
		doc: Uint8Array;
		maxSamples?: number;
		nowMs?: number;
	}): Promise<void> {
		const maxSamples = params.maxSamples ?? MAX_COMMAND_SAMPLES;
		this.ctx.storage.transactionSync(() => {
			this.sql.exec(
				`INSERT INTO command_samples (seq, node_id, created_at, meta, doc) VALUES (?, ?, ?, ?, ?)
				 ON CONFLICT (seq) DO UPDATE SET node_id = excluded.node_id, created_at = excluded.created_at, meta = excluded.meta, doc = excluded.doc`,
				params.seq,
				params.nodeId,
				params.createdAtMs,
				params.meta,
				params.doc,
			);
			this.sql.exec("DELETE FROM command_samples WHERE seq <= ?", params.seq - maxSamples);
		});
		await this.scheduleExpiry(params.nowMs ?? params.createdAtMs ?? Date.now());
	}

	listSamples(): string[] {
		return this.sql
			.exec("SELECT meta FROM command_samples ORDER BY seq")
			.toArray()
			.map((r) => str(r.meta));
	}

	getSample(seq: number): Uint8Array | null {
		const rows = this.sql.exec("SELECT doc FROM command_samples WHERE seq = ?", seq).toArray();
		if (rows.length === 0) return null;
		return toUint8Array(rows[0].doc);
	}

	async saveOutputLog(params: {
		tail: number;
		cut: boolean;
		doc: Uint8Array;
		nowMs?: number;
	}): Promise<void> {
		const nowMs = params.nowMs ?? Date.now();
		this.sql.exec(
			`INSERT INTO command_output (id, tail, cut, updated_at, doc) VALUES (1, ?, ?, ?, ?)
			 ON CONFLICT (id) DO UPDATE SET tail = excluded.tail, cut = excluded.cut, updated_at = excluded.updated_at, doc = excluded.doc`,
			params.tail,
			params.cut ? 1 : 0,
			nowMs,
			params.doc,
		);
		await this.scheduleExpiry(nowMs);
	}

	getOutputLog(): StoredOutputLog | null {
		const rows = this.sql.exec("SELECT tail, cut, doc FROM command_output WHERE id = 1").toArray();
		if (rows.length === 0) return null;
		const doc = toUint8Array(rows[0].doc);
		if (!doc) return null;
		return {
			tail: num(rows[0].tail),
			cut: num(rows[0].cut) !== 0,
			doc,
		};
	}

	async appendOutputChunks(params: {
		chunks: TimedOutputChunk[];
		eof: boolean;
		nowMs?: number;
	}): Promise<{ ackedOffset: number }> {
		const nowMs = params.nowMs ?? Date.now();
		let ackedOffset = 0;
		this.ctx.storage.transactionSync(() => {
			for (const c of params.chunks) {
				if (c.offset < 0 || c.nextOffset < c.offset) continue;
				if (c.data.byteLength === 0 && c.nextOffset === c.offset) continue;
				this.sql.exec(
					`INSERT OR IGNORE INTO command_output_chunks (offset, next_offset, received_at, data)
					 VALUES (?, ?, ?, ?)`,
					c.offset,
					c.nextOffset,
					c.receivedAt,
					c.data,
				);
			}
			if (params.eof) {
				this.sql.exec(
					`INSERT INTO command_meta (key, value) VALUES ('output_eof', '1')
					 ON CONFLICT (key) DO UPDATE SET value = '1'`,
				);
			}
			this.compactOutputChunksLocked();
			const ackRows = this.sql
				.exec("SELECT COALESCE(MAX(next_offset), 0) AS ack FROM command_output_chunks")
				.toArray();
			ackedOffset = ackRows.length > 0 ? num(ackRows[0].ack) : 0;
			if (ackedOffset > 0) {
				const prevRows = this.sql
					.exec("SELECT value FROM command_meta WHERE key = 'output_acked_offset'")
					.toArray();
				const prev = prevRows.length > 0 ? num(prevRows[0].value) : 0;
				if (ackedOffset > prev) {
					this.sql.exec(
						`INSERT INTO command_meta (key, value) VALUES ('output_acked_offset', ?)
						 ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
						String(ackedOffset),
					);
				} else {
					ackedOffset = prev;
				}
			}
		});
		await this.scheduleExpiry(nowMs);
		return { ackedOffset };
	}

	private compactOutputChunksLocked(): void {
		const sizeRows = this.sql
			.exec("SELECT COALESCE(SUM(LENGTH(data)), 0) AS total FROM command_output_chunks")
			.toArray();
		const total = sizeRows.length > 0 ? num(sizeRows[0].total) : 0;
		if (total <= MAX_STORED_OUTPUT_BYTES) return;

		const metaRows = this.sql
			.exec(
				"SELECT offset, next_offset, LENGTH(data) AS sz FROM command_output_chunks ORDER BY offset",
			)
			.toArray();
		if (metaRows.length < 3) return;

		let headKeepEndIdx = 0;
		let headBytes = 0;
		for (let i = 0; i < metaRows.length; i++) {
			const sz = num(metaRows[i].sz);
			if (headBytes + sz > KEEP_HEAD_OUTPUT_BYTES && i > 0) break;
			headBytes += sz;
			headKeepEndIdx = i;
		}
		let tailKeepStartIdx = metaRows.length - 1;
		let tailBytes = 0;
		for (let i = metaRows.length - 1; i > headKeepEndIdx; i--) {
			const sz = num(metaRows[i].sz);
			if (tailBytes + sz > KEEP_TAIL_OUTPUT_BYTES && i < metaRows.length - 1) break;
			tailBytes += sz;
			tailKeepStartIdx = i;
		}
		if (tailKeepStartIdx <= headKeepEndIdx + 1) return;

		const dropFromOffset = num(metaRows[headKeepEndIdx + 1].offset);
		const dropToOffset = num(metaRows[tailKeepStartIdx - 1].offset);
		const droppedNextOffset = num(metaRows[tailKeepStartIdx - 1].next_offset);
		const droppedBytes = Math.max(0, droppedNextOffset - dropFromOffset);
		this.sql.exec(
			"DELETE FROM command_output_chunks WHERE offset >= ? AND offset <= ?",
			dropFromOffset,
			dropToOffset,
		);
		const marker = new TextEncoder().encode(
			`\n[hotbed: ${droppedBytes} bytes dropped here (output cap)]\n`,
		);
		this.sql.exec(
			`INSERT OR REPLACE INTO command_output_chunks (offset, next_offset, received_at, data)
			 VALUES (?, ?, 0, ?)`,
			dropFromOffset,
			droppedNextOffset,
			marker,
		);
		this.sql.exec(
			`INSERT INTO command_meta (key, value) VALUES ('output_cut', '1')
			 ON CONFLICT (key) DO UPDATE SET value = '1'`,
		);
	}

	readOutputChunks(afterOffset = 0): CommandOutputStreamRead {
		const eof =
			this.sql.exec("SELECT 1 FROM command_meta WHERE key = 'output_eof' AND value = '1'").toArray()
				.length > 0;
		const anyRow =
			this.sql.exec("SELECT 1 FROM command_output_chunks LIMIT 1").toArray().length > 0;
		if (!eof && !anyRow) {
			return { chunks: [], eof: false, hasOutput: false };
		}
		const rows = this.sql
			.exec(
				"SELECT offset, next_offset, received_at, data FROM command_output_chunks WHERE next_offset > ? ORDER BY offset",
				afterOffset,
			)
			.toArray();
		const chunks: TimedOutputChunk[] = [];
		for (const r of rows) {
			const data = toUint8Array(r.data);
			if (!data) continue;
			chunks.push({
				offset: num(r.offset),
				nextOffset: num(r.next_offset),
				receivedAt: num(r.received_at),
				data,
			});
		}
		return { chunks, eof, hasOutput: true };
	}

	getOutputLines(tail = 2000, maxBytes = DEFAULT_LOG_LINES_MAX_BYTES): CommandOutputLinesResult {
		const eof =
			this.sql.exec("SELECT 1 FROM command_meta WHERE key = 'output_eof' AND value = '1'").toArray()
				.length > 0;
		const anyRow =
			this.sql.exec("SELECT 1 FROM command_output_chunks LIMIT 1").toArray().length > 0;
		if (!eof && !anyRow) {
			return { lines: [], lineTimestamps: [], cut: false, eof: false, hasOutput: false };
		}
		const alreadyCut =
			this.sql.exec("SELECT 1 FROM command_meta WHERE key = 'output_cut' AND value = '1'").toArray()
				.length > 0;

		// Read from the tail so a huge stored log never blows up DO memory.
		const readBudget = Math.max(maxBytes * 2, 2 << 20);
		// The cursor reads rows as the loop asks: stopping at the budget leaves
		// the older rest of the log unread.
		const rows = this.sql.exec(
			"SELECT offset, next_offset, received_at, data FROM command_output_chunks ORDER BY offset DESC",
		);
		const picked: TimedOutputChunk[] = [];
		let totalBytes = 0;
		let trimmedEarly = false;
		for (const r of rows) {
			const data = toUint8Array(r.data);
			if (!data) continue;
			if (picked.length > 0 && totalBytes + data.byteLength > readBudget) {
				trimmedEarly = true;
				break;
			}
			totalBytes += data.byteLength;
			picked.push({
				offset: num(r.offset),
				nextOffset: num(r.next_offset),
				receivedAt: num(r.received_at),
				data,
			});
		}
		picked.reverse();
		const extracted = extractLogLines(picked, tail, maxBytes, alreadyCut || trimmedEarly);
		return {
			lines: extracted.lines,
			lineTimestamps: extracted.lineTimestamps,
			cut: extracted.cut,
			eof,
			hasOutput: true,
		};
	}

	async clear(): Promise<void> {
		await this.ctx.storage.deleteAlarm();
		await this.deleteAll();
	}

	override async alarm(): Promise<void> {
		const rows = this.sql.exec("SELECT value FROM command_meta WHERE key = 'expires_at'").toArray();
		const expiresAt = rows.length > 0 ? num(rows[0].value) : 0;
		if (expiresAt > 0 && Date.now() < expiresAt) {
			await this.ctx.storage.setAlarm(expiresAt);
			return;
		}
		await this.deleteAll();
	}

	/**
	 * Read-only SQL query executed via typed Workers RPC by the Admin Agent.
	 * Uses `this.ctx.storage.sql` directly instead of `this.sql` so inspecting
	 * a nonexistent or expired CommandDO does not materialize empty tables.
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
	 * Does not materialize empty tables on nonexistent/expired CommandDOs.
	 */
	adminInspectSchema(): Promise<DoSchemaInfo> {
		return inspectDoSchema(this.ctx.storage.sql, this.ctx.storage);
	}
}

/**
 * Typed client used by ControllerDO services to read/write per-command data in
 * each command's dedicated SQLite CommandDO via Workers RPC.
 *
 * Always creates a fresh stub per RPC call and never caches in-flight Promises.
 */
export class CommandStoreClient {
	private readonly ns: DurableObjectNamespace<CommandDO>;
	private readonly clusterId: string;

	constructor(ns: DurableObjectNamespace<CommandDO>, clusterId: string) {
		this.ns = ns;
		this.clusterId = clusterId;
	}

	private stub(commandId: string): DurableObjectStub<CommandDO> {
		return this.ns.getByName(commandDoName(this.clusterId, commandId));
	}

	async saveProfile(
		nodeId: string,
		endedAtMs: number,
		profile: CommandProfile,
		nowMs?: number,
	): Promise<void> {
		await this.stub(profile.commandId).saveProfile({
			nodeId,
			endedAtMs,
			doc: toBinary(CommandProfileSchema, profile),
			nowMs,
		});
	}

	async getProfile(commandId: string): Promise<CommandProfile | null> {
		const raw = await this.stub(commandId).getProfile();
		if (!raw) return null;
		return fromBinary(CommandProfileSchema, raw);
	}

	async saveDiagnostics(nodeId: string, d: CommandDiagnostics, nowMs = Date.now()): Promise<void> {
		const seq = d.intervention?.seq ?? 0;
		const createdAtMs = Number(d.intervention?.by?.at || BigInt(nowMs));
		await this.stub(d.commandId).saveDiagnostics({
			nodeId,
			seq,
			createdAtMs,
			doc: toJsonString(CommandDiagnosticsSchema, d),
			nowMs,
		});
	}

	async getDiagnostic(commandId: string, seq: number): Promise<CommandDiagnostics | null> {
		const raw = await this.stub(commandId).getDiagnostic(seq);
		return raw !== null
			? fromJsonString(CommandDiagnosticsSchema, raw, { ignoreUnknownFields: true })
			: null;
	}

	async listDiagnosticsAfter(commandId: string, afterSeq: number): Promise<CommandDiagnostics[]> {
		const raws = await this.stub(commandId).listDiagnosticsAfter(afterSeq);
		return raws.map((r) =>
			fromJsonString(CommandDiagnosticsSchema, r, { ignoreUnknownFields: true }),
		);
	}

	async hasDiagnostics(commandId: string): Promise<boolean> {
		return this.stub(commandId).hasDiagnostics();
	}

	async saveSample(nodeId: string, sample: CommandSample, nowMs = Date.now()): Promise<void> {
		const meta = create(CommandSampleSchema, { ...sample, sample: undefined });
		await this.stub(sample.commandId).saveSample({
			nodeId,
			seq: sample.seq,
			createdAtMs: Number(sample.by?.at || BigInt(nowMs)),
			meta: toJsonString(CommandSampleSchema, meta),
			doc: toBinary(CommandSampleSchema, sample),
			maxSamples: MAX_COMMAND_SAMPLES,
			nowMs,
		});
	}

	async listSamples(commandId: string): Promise<CommandSample[]> {
		const raws = await this.stub(commandId).listSamples();
		return raws.map((r) => fromJsonString(CommandSampleSchema, r, { ignoreUnknownFields: true }));
	}

	async getSample(commandId: string, seq: number): Promise<CommandSample | null> {
		const raw = await this.stub(commandId).getSample(seq);
		if (!raw) return null;
		return fromBinary(CommandSampleSchema, raw);
	}

	async saveOutputLog(
		commandId: string,
		tail: number,
		log: CommandLog,
		nowMs = Date.now(),
	): Promise<void> {
		await this.stub(commandId).saveOutputLog({
			tail,
			cut: log.cut,
			doc: toBinary(CommandLogSchema, log),
			nowMs,
		});
	}

	async getOutputLog(commandId: string): Promise<CommandOutputCache | null> {
		const stored = await this.stub(commandId).getOutputLog();
		if (!stored) return null;
		return {
			tail: stored.tail,
			cut: stored.cut,
			log: fromBinary(CommandLogSchema, stored.doc),
		};
	}

	async appendOutputChunks(
		commandId: string,
		chunks: CommandOutputChunk[],
		eof: boolean,
		nowMs = Date.now(),
	): Promise<{ ackedOffset: number }> {
		const mapped: TimedOutputChunk[] = chunks.map((c) => ({
			offset: Number(c.offset),
			nextOffset: Number(c.nextOffset),
			receivedAt: Number(c.receivedAt),
			data: c.data,
		}));
		return this.stub(commandId).appendOutputChunks({ chunks: mapped, eof, nowMs });
	}

	async readOutputChunks(commandId: string, afterOffset = 0): Promise<CommandOutputStreamRead> {
		return this.stub(commandId).readOutputChunks(afterOffset);
	}

	async getOutputLines(
		commandId: string,
		tail = 2000,
		maxBytes = DEFAULT_LOG_LINES_MAX_BYTES,
	): Promise<CommandOutputLinesResult> {
		return this.stub(commandId).getOutputLines(tail, maxBytes);
	}
}
