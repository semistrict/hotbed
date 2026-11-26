import { DurableObject } from "cloudflare:workers";
import { create, fromBinary, fromJsonString, toBinary, toJsonString } from "@bufbuild/protobuf";
import {
	CommandDiagnosticsSchema,
	CommandLogSchema,
	CommandProfileSchema,
	CommandSampleSchema,
	type CommandDiagnostics,
	type CommandLog,
	type CommandProfile,
	type CommandSample,
} from "./gen/hotbed/v1/hotbed_pb.ts";
import { KEEP_HISTORY_MS, MAX_COMMAND_SAMPLES } from "./store.ts";

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

function sqlStr(v: SqlStorageValue | undefined): string {
	return typeof v === "string" ? v : v === null || v === undefined ? "" : String(Number(v));
}

function sqlNum(v: SqlStorageValue | undefined): number {
	return typeof v === "number" ? v : typeof v === "string" ? Number(v) : 0;
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

/**
 * Per-command SQLite Durable Object storing heavy per-command data outside the
 * main ControllerDO: continuous eBPF profile, intervention diagnostics, kept
 * activity samples, and cached ended command output.
 *
 * Accessed strictly via typed Cloudflare Workers RPC (no HTTP fetch handler).
 * Each instance schedules its own DO alarm to deleteAll() after KEEP_HISTORY_MS.
 */
export class CommandDO extends DurableObject<Env> {
	private sql: SqlStorage;

	constructor(ctx: DurableObjectState, env: Env) {
		super(ctx, env);
		this.sql = ctx.storage.sql;
		void this.ctx.blockConcurrencyWhile(async () => {
			this.ensureSchema();
		});
	}

	private ensureSchema(): void {
		this.sql.exec(`
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
		`);
	}

	private async scheduleExpiry(nowMs: number, ttlMs = KEEP_HISTORY_MS): Promise<void> {
		const desired = Math.max(Date.now(), nowMs) + ttlMs;
		const rows = this.sql.exec("SELECT value FROM command_meta WHERE key = 'expires_at'").toArray();
		const current = rows.length > 0 ? sqlNum(rows[0].value) : 0;
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
		this.ensureSchema();
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
		this.ensureSchema();
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
		this.ensureSchema();
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
		this.ensureSchema();
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
		this.ensureSchema();
		const rows = this.sql.exec("SELECT doc FROM command_diagnostics WHERE seq = ?", seq).toArray();
		return rows.length > 0 ? sqlStr(rows[0].doc) : null;
	}

	listDiagnosticsAfter(afterSeq: number): string[] {
		this.ensureSchema();
		return this.sql
			.exec("SELECT doc FROM command_diagnostics WHERE seq > ? ORDER BY seq", afterSeq)
			.toArray()
			.map((r) => sqlStr(r.doc));
	}

	hasDiagnostics(): boolean {
		this.ensureSchema();
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
		this.ensureSchema();
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
		this.ensureSchema();
		return this.sql
			.exec("SELECT meta FROM command_samples ORDER BY seq")
			.toArray()
			.map((r) => sqlStr(r.meta));
	}

	getSample(seq: number): Uint8Array | null {
		this.ensureSchema();
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
		this.ensureSchema();
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
		this.ensureSchema();
		const rows = this.sql.exec("SELECT tail, cut, doc FROM command_output WHERE id = 1").toArray();
		if (rows.length === 0) return null;
		const doc = toUint8Array(rows[0].doc);
		if (!doc) return null;
		return {
			tail: sqlNum(rows[0].tail),
			cut: sqlNum(rows[0].cut) !== 0,
			doc,
		};
	}

	async clear(): Promise<void> {
		await this.ctx.storage.deleteAlarm();
		await this.ctx.storage.deleteAll();
	}

	override async alarm(): Promise<void> {
		this.ensureSchema();
		const rows = this.sql.exec("SELECT value FROM command_meta WHERE key = 'expires_at'").toArray();
		const expiresAt = rows.length > 0 ? sqlNum(rows[0].value) : 0;
		if (expiresAt > 0 && Date.now() < expiresAt) {
			await this.ctx.storage.setAlarm(expiresAt);
			return;
		}
		await this.ctx.storage.deleteAll();
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
}
