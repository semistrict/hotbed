import { eq } from "drizzle-orm";
import type { DrizzleSqliteDODatabase } from "drizzle-orm/durable-sqlite";
import {
	clone,
	create,
	fromBinary,
	fromJsonString,
	toBinary,
	toJsonString,
	type DescMessage,
	type MessageShape,
} from "@bufbuild/protobuf";
import * as dbSchema from "./db/schema.ts";
import {
	BaseSchema,
	ClusterEventSchema,
	CommandDiagnosticsSchema,
	CommandProfileSchema,
	CommandSchema,
	CommandState,
	CommandUpdate_What,
	CommandUpdateSchema,
	DaemonLogEntrySchema,
	DaemonLogLevel,
	DaemonLogSourceSchema,
	EventRefSchema,
	FindingDecision,
	FindingSchema,
	FinishedCommandSchema,
	GetDaemonLogsResponseSchema,
	GitCredentialSchema,
	HeartbeatRequestSchema,
	InterventionKind,
	NodeSchema,
	PlacementDecisionSchema,
	ProjectChangeSchema,
	ProjectSchema,
	QuestionChangeSchema,
	QuestionOrder,
	QuestionProjectSchema,
	QuestionSchema,
	QuestionState,
	QuestionUpdate_What,
	QuestionUpdateSchema,
	QuickResponse,
	RepoSchema,
	ReviewEventSchema,
	ReviewSchema,
	ReviewState,
	ReviewUpdate_What,
	ReviewUpdateSchema,
	ScheduledCommandSchema,
	ScheduledRunSchema,
	SessionEventSchema,
	SessionSchema,
	SessionWorkspaceSchema,
	StampSchema,
	WorkspaceSchema,
	WorkspaceState,
	WorkspaceUpdate_What,
	WorkspaceUpdateSchema,
	type Base,
	type Checkpoint,
	type ClusterEvent,
	type Command,
	type CommandDiagnostics,
	type CommandIntervention,
	type CommandProfile,
	type DaemonLogEntry,
	type DaemonLogSource,
	type Event,
	type Finding,
	type FinishedCommand,
	type GetDaemonLogsResponse,
	type GitCredential,
	type HeartbeatRequest,
	type Node,
	type PlacementDecision,
	type Project,
	type ProjectChange,
	type Prompt,
	type Question,
	type QuestionChange,
	type QuestionProject,
	type Repo,
	type Review,
	type ReviewEvent,
	type ScheduledCommand,
	type ScheduledRun,
	type Session,
	type SessionEvent,
	type Stamp,
	type Workspace,
	type Answer,
	type AskedFrom,
	type PushEventsRequest,
	CheckpointSchema,
} from "./gen/hotbed/v1/hotbed_pb.ts";
import { ErrorCode, hotbedError } from "./errors.ts";
import { firstLine } from "./gitops.ts";
import type { NodeRow } from "./placement.ts";
import { isCommandLive } from "./standby.ts";

export const KEEP_SAMPLES_MS = 25 * 3600_000;
export const KEEP_PLACEMENTS_MS = 24 * 3600_000;
export const KEEP_HISTORY_MS = 30 * 24 * 3600_000;
export const KEEP_EVENTS = 5000;
export const KEEP_RUNS = 200;
export const KEEP_DAEMON_LOGS_MS = 14 * 24 * 3600_000;
export const LOST_DETAIL = "lost: its worker no longer has it (the worker's state was reset)";

export interface WorkspaceRow {
	id: string;
	session: string;
	node: string;
	agent: string;
	released: boolean;
}

export interface NodeStateView {
	bases: Base[];
	workspaces: Workspace[];
	commands: Command[];
}

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
}

export interface ForkRow {
	at: number;
	millis: number;
}

export interface PushResult {
	applied: bigint;
	appliedAt: bigint;
	checkpointNeeded: boolean;
	finished: boolean;
	lost: string[];
	sessions: Set<string>;
}

export interface GitCredentialSecret {
	token: string;
	refreshToken?: string;
	clientId?: string;
	tokenExpiresAt?: number;
	refreshExpiresAt?: number;
}

export interface StoredGitCredential {
	cred: GitCredential;
	secret: GitCredentialSecret;
}

export interface CommandRoute {
	session: string;
	node: string;
	histChecked: boolean;
	baseChecked: boolean;
	base?: Base;
}

function marshal<T extends DescMessage>(schema: T, msg: MessageShape<T>): string {
	return toJsonString(schema, msg);
}

function unmarshal<T extends DescMessage>(schema: T, doc: string): MessageShape<T> {
	return fromJsonString(schema, doc || "{}", { ignoreUnknownFields: true });
}

function num(v: unknown): number {
	return typeof v === "number" ? v : typeof v === "bigint" ? Number(v) : Number(v ?? 0);
}

function big(v: unknown): bigint {
	return typeof v === "bigint" ? v : BigInt(Math.trunc(Number(v ?? 0)));
}

function str(v: unknown): string {
	if (typeof v === "string") return v;
	if (typeof v === "number" || typeof v === "bigint" || typeof v === "boolean") return String(v);
	return "";
}

function optBig(v: unknown): bigint | undefined {
	if (v === null || v === undefined) return undefined;
	return big(v);
}

export function questionStateName(s: QuestionState): string {
	switch (s) {
		case QuestionState.OPEN:
			return "QUESTION_STATE_OPEN";
		case QuestionState.ANSWERED:
			return "QUESTION_STATE_ANSWERED";
		case QuestionState.DISMISSED:
			return "QUESTION_STATE_DISMISSED";
		case QuestionState.HANDLED:
			return "QUESTION_STATE_HANDLED";
		case QuestionState.DELETED:
			return "QUESTION_STATE_DELETED";
		default:
			return "QUESTION_STATE_UNSPECIFIED";
	}
}

export function reviewStateName(s: ReviewState): string {
	switch (s) {
		case ReviewState.REQUESTED:
			return "REVIEW_STATE_REQUESTED";
		case ReviewState.IN_PROGRESS:
			return "REVIEW_STATE_IN_PROGRESS";
		case ReviewState.DONE:
			return "REVIEW_STATE_DONE";
		case ReviewState.FAILED:
			return "REVIEW_STATE_FAILED";
		case ReviewState.CANCELLED:
			return "REVIEW_STATE_CANCELLED";
		default:
			return "REVIEW_STATE_UNSPECIFIED";
	}
}

function quickResponseName(q: QuickResponse): string {
	switch (q) {
		case QuickResponse.VERIFY:
			return "QUICK_RESPONSE_VERIFY";
		case QuickResponse.FIX_IF_VERIFIED:
			return "QUICK_RESPONSE_FIX_IF_VERIFIED";
		case QuickResponse.WONT_FIX:
			return "QUICK_RESPONSE_WONT_FIX";
		default:
			return "QUICK_RESPONSE_UNSPECIFIED";
	}
}

function quickResponseFromName(s: string): QuickResponse {
	switch (s) {
		case "QUICK_RESPONSE_VERIFY":
			return QuickResponse.VERIFY;
		case "QUICK_RESPONSE_FIX_IF_VERIFIED":
			return QuickResponse.FIX_IF_VERIFIED;
		case "QUICK_RESPONSE_WONT_FIX":
			return QuickResponse.WONT_FIX;
		default:
			return QuickResponse.UNSPECIFIED;
	}
}

const COMMAND_WHAT: Record<string, CommandUpdate_What> = {
	admitted: CommandUpdate_What.ADMITTED,
	parked: CommandUpdate_What.PARKED,
	thawed: CommandUpdate_What.THAWED,
	paused: CommandUpdate_What.PAUSED,
	resumed: CommandUpdate_What.RESUMED,
	"isolation lost": CommandUpdate_What.ISOLATION_LOST,
	exited: CommandUpdate_What.EXITED,
	cancelled: CommandUpdate_What.CANCELLED,
};

const WORKSPACE_WHAT: Record<string, WorkspaceUpdate_What> = {
	ready: WorkspaceUpdate_What.READY,
	failed: WorkspaceUpdate_What.FAILED,
};

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

function questionResponseLine(q: Question): string {
	const head = `question ${q.id} (${q.prompt?.header ?? ""})`;
	if (q.state === QuestionState.DISMISSED) {
		return q.dismissed?.reason ? `${head} dismissed: ${q.dismissed.reason}` : `${head} dismissed`;
	}
	const parts = [...(q.answer?.picked ?? [])];
	if (q.answer?.other) parts.push(q.answer.other);
	return `${head} answered: ${parts.join("; ")}`;
}

function reviewResponseOn(about: string, e: ReviewEvent): string {
	const who = e.by?.session || "the user";
	const parts: string[] = [];
	if (e.quick !== QuickResponse.UNSPECIFIED) {
		const word =
			e.quick === QuickResponse.VERIFY
				? "verify"
				: e.quick === QuickResponse.FIX_IF_VERIFIED
					? "fix if verified"
					: "won't fix";
		parts.push(word);
	}
	if (e.text) parts.push(JSON.stringify(firstLine(e.text)));
	return `${who} on ${about}: ${parts.join("; ")}`;
}

export interface ControllerMigrationState {
	targetController: string;
	controllerNodeId: string;
	defaultToken: string;
	workerTokens: Record<string, string>;
	migratedAt: number;
}

export interface MigrationImportPayload {
	clusterId: string;
	force?: boolean;
	tables: Record<string, Array<Record<string, unknown>>>;
}

export interface MigrationImportResult {
	clusterId: string;
	importedTables: number;
	importedRows: number;
	activeNodes: Array<{ id: string; hostname: string }>;
}

/**
 * Durable controller tables transferred during controller migration, in
 * foreign-key dependency order (parents before children).
 * Transient node_state / samples / events / command_profiles are skipped:
 * workers re-checkpoint live state immediately on reconnect.
 */
export const MIGRATION_TABLES = [
	{
		name: "nodes",
		columns: [
			"id",
			"hostname",
			"login",
			"addr",
			"joined_at",
			"last_seen",
			"epoch",
			"applied",
			"applied_at",
			"heartbeat",
			"retired_at",
		],
		autoIncCol: null,
	},
	{ name: "repos", columns: ["id", "doc"], autoIncCol: null },
	{ name: "projects", columns: ["id", "name", "created_at", "setup"], autoIncCol: "id" },
	{ name: "project_repos", columns: ["project_id", "remote"], autoIncCol: null },
	{
		name: "project_changes",
		columns: ["seq", "project_id", "what", "repo", "at", "session", "host", "reason"],
		autoIncCol: "seq",
	},
	{
		name: "sessions",
		columns: [
			"id",
			"agent",
			"project_id",
			"t3code_thread_id",
			"started_at",
			"last_active_at",
			"ended_at",
			"end_reason",
		],
		autoIncCol: null,
	},
	{
		name: "workspaces",
		columns: ["id", "session_id", "node_id", "agent", "created_at", "fork_millis", "released_at"],
		autoIncCol: null,
	},
	{
		name: "commands",
		columns: ["id", "session_id", "node_id", "workspace_id", "created_at"],
		autoIncCol: null,
	},
	{ name: "session_events", columns: ["seq", "session_id", "doc"], autoIncCol: "seq" },
	{
		name: "history",
		columns: ["id", "node_id", "repo_id", "ended_at", "doc", "project_id", "session_id"],
		autoIncCol: null,
	},
	{
		name: "command_diagnostics",
		columns: ["command_id", "seq", "node_id", "created_at", "doc"],
		autoIncCol: null,
	},
	{ name: "placements", columns: ["id", "at", "session_id", "doc"], autoIncCol: "id" },
	{ name: "schedules", columns: ["name", "project_id", "doc", "next_at"], autoIncCol: null },
	{
		name: "schedule_runs",
		columns: ["id", "schedule", "session_id", "command_id", "ended_at", "doc"],
		autoIncCol: "id",
	},
	{
		name: "questions",
		columns: [
			"id",
			"project_id",
			"repo",
			"state",
			"rank",
			"asked_by",
			"asked_at",
			"updated_at",
			"doc",
		],
		autoIncCol: "id",
	},
	{
		name: "question_changes",
		columns: ["seq", "question_id", "what", "at", "session", "host", "reason", "doc"],
		autoIncCol: "seq",
	},
	{
		name: "reviews",
		columns: ["id", "project_id", "state", "requested_by", "requested_at", "updated_at", "doc"],
		autoIncCol: "id",
	},
	{ name: "review_findings", columns: ["id", "review_id", "doc"], autoIncCol: "id" },
	{
		name: "review_events",
		columns: [
			"seq",
			"review_id",
			"finding_id",
			"what",
			"at",
			"session",
			"host",
			"quick",
			"text",
			"doc",
		],
		autoIncCol: "seq",
	},
	{ name: "git_credentials", columns: ["name", "host", "doc", "secret"], autoIncCol: null },
] as const;

export class ControllerStore {
	private readonly storage: DurableObjectStorage;
	readonly db: DrizzleSqliteDODatabase<typeof dbSchema>;
	private readonly sql: {
		exec<T extends Record<string, SqlStorageValue> = Record<string, SqlStorageValue>>(
			query: string,
			...bindings: unknown[]
		): SqlStorageCursor<T>;
	};
	private samplesGen = 0;

	constructor(storage: DurableObjectStorage, db: DrizzleSqliteDODatabase<typeof dbSchema>) {
		this.storage = storage;
		this.db = db;
		const rawSql = storage.sql;
		// DO SQLite does not accept JS BigInt bind parameters; coerce them centrally
		// so protobuf int64 fields can never throw "Cannot convert a BigInt value to a number".
		this.sql = {
			exec<T extends Record<string, SqlStorageValue> = Record<string, SqlStorageValue>>(
				query: string,
				...bindings: unknown[]
			): SqlStorageCursor<T> {
				const coerced = bindings.map((b) => (typeof b === "bigint" ? Number(b) : b));
				return rawSql.exec<T>(query, ...coerced);
			},
		};
	}

	samplesVersion(): number {
		return this.samplesGen;
	}

	transactionSync<T>(fn: () => T): T {
		return this.storage.transactionSync(fn);
	}

	// ---- kv_meta (Drizzle ORM) ------------------------------------------

	getMeta(key: string): string | null {
		const row = this.db
			.select({ value: dbSchema.kvMeta.value })
			.from(dbSchema.kvMeta)
			.where(eq(dbSchema.kvMeta.key, key))
			.get();
		return row?.value ?? null;
	}

	setMeta(key: string, value: string): void {
		this.db
			.insert(dbSchema.kvMeta)
			.values({ key, value })
			.onConflictDoUpdate({
				target: dbSchema.kvMeta.key,
				set: { value },
			})
			.run();
	}

	// ---- controller_migration (Drizzle ORM) -----------------------------

	getControllerMigration(): ControllerMigrationState | null {
		const row = this.db
			.select()
			.from(dbSchema.controllerMigration)
			.where(eq(dbSchema.controllerMigration.id, 1))
			.get();
		if (!row) return null;
		let workerTokens: Record<string, string> = {};
		try {
			const parsed = JSON.parse(row.workerTokensJson) as unknown;
			if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
				workerTokens = parsed as Record<string, string>;
			}
		} catch {
			// keep empty map
		}
		return {
			targetController: row.targetController,
			controllerNodeId: row.controllerNodeId,
			defaultToken: row.defaultToken,
			workerTokens,
			migratedAt: row.migratedAt,
		};
	}

	setControllerMigration(state: ControllerMigrationState): void {
		const workerTokensJson = JSON.stringify(state.workerTokens);
		this.db
			.insert(dbSchema.controllerMigration)
			.values({
				id: 1,
				targetController: state.targetController,
				controllerNodeId: state.controllerNodeId,
				defaultToken: state.defaultToken,
				workerTokensJson,
				migratedAt: state.migratedAt,
			})
			.onConflictDoUpdate({
				target: dbSchema.controllerMigration.id,
				set: {
					targetController: state.targetController,
					controllerNodeId: state.controllerNodeId,
					defaultToken: state.defaultToken,
					workerTokensJson,
					migratedAt: state.migratedAt,
				},
			})
			.run();
	}

	exportSnapshot(clusterId: string): {
		clusterId: string;
		tables: Record<string, Array<Record<string, SqlStorageValue>>>;
	} {
		const tables: Record<string, Array<Record<string, SqlStorageValue>>> = {};
		for (const t of MIGRATION_TABLES) {
			const cols = t.columns.join(", ");
			tables[t.name] = this.sql.exec(`SELECT ${cols} FROM ${t.name}`).toArray();
		}
		return { clusterId, tables };
	}

	importSnapshot(payload: MigrationImportPayload): MigrationImportResult {
		const clusterId = (payload.clusterId ?? "").trim();
		if (!clusterId) {
			throw hotbedError(ErrorCode.InvalidArgs, "clusterId is required for import");
		}
		return this.transactionSync(() => {
			if (!payload.force) {
				const existing =
					num(this.sql.exec("SELECT COUNT(*) AS n FROM nodes").one().n) +
					num(this.sql.exec("SELECT COUNT(*) AS n FROM projects").one().n) +
					num(this.sql.exec("SELECT COUNT(*) AS n FROM repos").one().n) +
					num(this.sql.exec("SELECT COUNT(*) AS n FROM sessions").one().n);
				if (existing > 0) {
					throw hotbedError(
						ErrorCode.Conflict,
						"target controller already holds cluster state; pass force=true to overwrite",
					);
				}
			}

			// Clear existing data in reverse FK order (children before parents).
			const clearOrder = [
				"node_state",
				"samples",
				"events",
				"command_profiles",
				...[...MIGRATION_TABLES].reverse().map((t) => t.name),
			];
			for (const name of clearOrder) {
				this.sql.exec(`DELETE FROM ${name}`);
			}

			let importedTables = 0;
			let importedRows = 0;
			for (const spec of MIGRATION_TABLES) {
				const rows = payload.tables?.[spec.name];
				if (!Array.isArray(rows) || rows.length === 0) continue;
				importedTables++;
				const cols = spec.columns.join(", ");
				const ph = spec.columns.map(() => "?").join(", ");
				const insertSQL = `INSERT INTO ${spec.name} (${cols}) VALUES (${ph})`;
				let maxAutoInc = 0;
				for (const row of rows) {
					const vals = spec.columns.map((col) => {
						const v = row[col];
						if (v === undefined) return null;
						return v;
					});
					this.sql.exec(insertSQL, ...vals);
					importedRows++;
					if (spec.autoIncCol) {
						const idVal = Number(row[spec.autoIncCol] ?? 0);
						if (Number.isFinite(idVal) && idVal > maxAutoInc) maxAutoInc = idVal;
					}
				}
				if (spec.autoIncCol && maxAutoInc > 0) {
					this.sql.exec("DELETE FROM sqlite_sequence WHERE name = ?", spec.name);
					this.sql.exec(
						"INSERT INTO sqlite_sequence (name, seq) VALUES (?, ?)",
						spec.name,
						maxAutoInc,
					);
				}
			}

			this.setMeta("cluster_id", clusterId);

			const activeNodes = this.sql
				.exec("SELECT id, hostname FROM nodes WHERE retired_at IS NULL ORDER BY hostname")
				.toArray()
				.map((r) => ({ id: str(r.id), hostname: str(r.hostname) }));

			return { clusterId, importedTables, importedRows, activeNodes };
		});
	}

	// ---- worker_tokens (Drizzle ORM) ------------------------------------

	putWorkerToken(tokenHash: string, nodeId: string, hostname: string, nowMs: number): void {
		this.db
			.insert(dbSchema.workerTokens)
			.values({ tokenHash, nodeId, hostname, createdAt: nowMs })
			.onConflictDoUpdate({
				target: dbSchema.workerTokens.tokenHash,
				set: { nodeId, hostname },
			})
			.run();
	}

	findWorkerToken(tokenHash: string): { nodeId: string; hostname: string } | null {
		const row = this.db
			.select({
				nodeId: dbSchema.workerTokens.nodeId,
				hostname: dbSchema.workerTokens.hostname,
			})
			.from(dbSchema.workerTokens)
			.where(eq(dbSchema.workerTokens.tokenHash, tokenHash))
			.get();
		return row ?? null;
	}

	// ---- daemon_logs ----------------------------------------------------

	logDaemon(
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

	daemonLogSummary(host: string, nowMs: number): DaemonLogSource {
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

	queryDaemonLogs(
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
		const where = clauses.length > 0 ? `WHERE ${clauses.join(" AND ")}` : "";
		const rows = this.sql
			.exec(
				`SELECT at, level, msg, attrs_json, source_loc FROM daemon_logs ${where} ORDER BY at DESC, id DESC LIMIT ?`,
				...params,
				limit + 1,
			)
			.toArray();
		const qLower = query.trim().toLowerCase();
		const entries: DaemonLogEntry[] = [];
		let truncated = false;
		for (const r of rows) {
			const msg = str(r.msg);
			const attrsJson = str(r.attrs_json);
			if (
				qLower &&
				!msg.toLowerCase().includes(qLower) &&
				!attrsJson.toLowerCase().includes(qLower)
			) {
				continue;
			}
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
					msg,
					attrsJson,
					sourceLoc: str(r.source_loc),
				}),
			);
		}
		return create(GetDaemonLogsResponseSchema, {
			entries,
			sources: [this.daemonLogSummary(host, nowMs)],
			truncated,
		});
	}

	// ---- nodes ----------------------------------------------------------

	joinNode(id: string, hostname: string, login: string, addr: string, nowMs: number): Node {
		this.sql.exec(
			`INSERT INTO nodes (id, hostname, login, addr, joined_at)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT (id) DO UPDATE SET
			   hostname = excluded.hostname,
			   login = excluded.login,
			   addr = CASE WHEN excluded.addr != '' THEN excluded.addr ELSE nodes.addr END,
			   retired_at = NULL`,
			id,
			hostname,
			login,
			addr,
			nowMs,
		);
		return this.getNode(id).node;
	}

	retireNode(id: string, atMs: number): void {
		this.sql.exec("UPDATE nodes SET retired_at = ? WHERE id = ?", atMs, id);
	}

	hasNode(id: string): boolean {
		return this.sql.exec("SELECT 1 FROM nodes WHERE id = ?", id).toArray().length > 0;
	}

	listNodes(): NodeRow[] {
		const rows = this.sql
			.exec(
				"SELECT id, hostname, login, addr, joined_at, last_seen, heartbeat FROM nodes WHERE retired_at IS NULL ORDER BY hostname, id",
			)
			.toArray();
		return rows.map((r) => this.toNodeRow(r));
	}

	getNode(id: string): NodeRow {
		const rows = this.sql
			.exec(
				"SELECT id, hostname, login, addr, joined_at, last_seen, heartbeat FROM nodes WHERE id = ?",
				id,
			)
			.toArray();
		if (rows.length === 0) {
			throw hotbedError(ErrorCode.NotFound, `no node ${id}`);
		}
		return this.toNodeRow(rows[0]);
	}

	private toNodeRow(r: Record<string, SqlStorageValue>): NodeRow {
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
		}
		return { node, addr: str(r.addr), hb };
	}

	// ---- repos ----------------------------------------------------------

	putRepo(repo: Repo): void {
		const copy = clone(RepoSchema, repo);
		copy.reminder = undefined;
		this.sql.exec(
			"INSERT INTO repos (id, doc) VALUES (?, ?) ON CONFLICT (id) DO UPDATE SET doc = excluded.doc",
			repo.id,
			marshal(RepoSchema, copy),
		);
	}

	deleteRepo(id: string): boolean {
		const cur = this.sql.exec("DELETE FROM repos WHERE id = ?", id);
		return cur.rowsWritten > 0;
	}

	listRepos(): Repo[] {
		return this.sql
			.exec("SELECT doc FROM repos ORDER BY id")
			.toArray()
			.map((r) => unmarshal(RepoSchema, str(r.doc)));
	}

	// ---- projects -------------------------------------------------------

	private projectIdByName(name: string): number {
		const rows = this.sql.exec("SELECT id FROM projects WHERE name = ?", name).toArray();
		if (rows.length === 0) {
			throw hotbedError(ErrorCode.NotFound, `no project ${name}`).withSuggestion(
				"hotbed project ls",
			);
		}
		return num(rows[0].id);
	}

	addProject(name: string, remotes: string[], by: Stamp): Project {
		return this.transactionSync(() => {
			const ins = this.sql.exec(
				"INSERT INTO projects (name, created_at) VALUES (?, ?) ON CONFLICT (name) DO NOTHING",
				name,
				Number(by.at),
			);
			const pid = this.projectIdByName(name);
			if (ins.rowsWritten > 0) {
				this.sql.exec(
					"INSERT INTO project_changes (project_id, what, repo, at, session, host, reason) VALUES (?, ?, ?, ?, ?, ?, ?)",
					pid,
					"created",
					"",
					Number(by.at),
					by.session,
					by.host,
					by.reason,
				);
			}
			for (const remote of remotes) {
				const added = this.sql.exec(
					"INSERT INTO project_repos (project_id, remote) VALUES (?, ?) ON CONFLICT DO NOTHING",
					pid,
					remote,
				);
				if (added.rowsWritten > 0) {
					this.sql.exec(
						"INSERT INTO project_changes (project_id, what, repo, at, session, host, reason) VALUES (?, ?, ?, ?, ?, ?, ?)",
						pid,
						"added",
						remote,
						Number(by.at),
						by.session,
						by.host,
						by.reason,
					);
				}
			}
			return this.getProject(name);
		});
	}

	removeProjectRepos(name: string, remotes: string[], by: Stamp): Project {
		return this.transactionSync(() => {
			const pid = this.projectIdByName(name);
			for (const remote of remotes) {
				const del = this.sql.exec(
					"DELETE FROM project_repos WHERE project_id = ? AND remote = ?",
					pid,
					remote,
				);
				if (del.rowsWritten === 0) {
					throw hotbedError(
						ErrorCode.NotFound,
						`project ${name} does not hold ${remote}`,
					).withSuggestion(`hotbed project show ${name}`);
				}
				this.sql.exec(
					"INSERT INTO project_changes (project_id, what, repo, at, session, host, reason) VALUES (?, ?, ?, ?, ?, ?, ?)",
					pid,
					"removed",
					remote,
					Number(by.at),
					by.session,
					by.host,
					by.reason,
				);
			}
			return this.getProject(name);
		});
	}

	editProjectSetup(name: string, edit: (p: Project) => void): Project {
		return this.transactionSync(() => {
			const p = this.getProject(name);
			edit(p);
			p.setupRevision += 1n;
			const setupOnly = create(ProjectSchema, {
				steps: p.steps,
				hosts: p.hosts,
				scratchEpoch: p.scratchEpoch,
				setupRevision: p.setupRevision,
			});
			const pid = this.projectIdByName(name);
			this.sql.exec(
				"UPDATE projects SET setup = ? WHERE id = ?",
				marshal(ProjectSchema, setupOnly),
				pid,
			);
			return p;
		});
	}

	getProject(name: string): Project {
		const rows = this.sql
			.exec("SELECT id, name, created_at, setup FROM projects WHERE name = ?", name)
			.toArray();
		if (rows.length === 0) {
			throw hotbedError(ErrorCode.NotFound, `no project ${name}`).withSuggestion(
				"hotbed project ls",
			);
		}
		return this.hydrateProject(rows[0]);
	}

	listProjects(): Project[] {
		const rows = this.sql
			.exec("SELECT id, name, created_at, setup FROM projects ORDER BY name")
			.toArray();
		const repoRows = this.sql
			.exec("SELECT project_id, remote FROM project_repos ORDER BY project_id, remote")
			.toArray();
		const byPid = new Map<number, string[]>();
		for (const r of repoRows) {
			const pid = num(r.project_id);
			let list = byPid.get(pid);
			if (!list) {
				list = [];
				byPid.set(pid, list);
			}
			list.push(str(r.remote));
		}
		return rows.map((r) => this.projectFromRow(r, byPid.get(num(r.id)) ?? []));
	}

	projectsHolding(remote: string): Project[] {
		const rows = this.sql
			.exec(
				`SELECT p.id, p.name, p.created_at, p.setup
				 FROM projects p
				 JOIN project_repos r ON r.project_id = p.id
				 WHERE r.remote = ?
				 ORDER BY p.name`,
				remote,
			)
			.toArray();
		return rows.map((r) => this.hydrateProject(r));
	}

	projectHistory(name: string): ProjectChange[] {
		const pid = this.projectIdByName(name);
		return this.sql
			.exec(
				"SELECT seq, what, repo, at, session, host, reason FROM project_changes WHERE project_id = ? ORDER BY seq",
				pid,
			)
			.toArray()
			.map((r) =>
				create(ProjectChangeSchema, {
					seq: big(r.seq),
					what: str(r.what),
					repo: str(r.repo),
					by: create(StampSchema, {
						at: big(r.at),
						session: str(r.session),
						host: str(r.host),
						reason: str(r.reason),
					}),
				}),
			);
	}

	private hydrateProject(r: Record<string, SqlStorageValue>): Project {
		const pid = num(r.id);
		const remotes = this.sql
			.exec("SELECT remote FROM project_repos WHERE project_id = ? ORDER BY remote", pid)
			.toArray()
			.map((rr) => str(rr.remote));
		return this.projectFromRow(r, remotes);
	}

	private projectFromRow(r: Record<string, SqlStorageValue>, remotes: string[]): Project {
		const setup = unmarshal(ProjectSchema, str(r.setup) || "{}");
		return create(ProjectSchema, {
			name: str(r.name),
			repos: remotes,
			createdAt: big(r.created_at),
			steps: setup.steps,
			hosts: setup.hosts,
			scratchEpoch: setup.scratchEpoch,
			setupRevision: setup.setupRevision,
		});
	}

	liveSessionsByProject(): Map<string, number> {
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

	schedulesByProject(): Map<string, number> {
		const out = new Map<string, number>();
		for (const r of this.sql
			.exec(
				"SELECT p.name, COUNT(*) AS cnt FROM schedules s JOIN projects p ON p.id = s.project_id GROUP BY p.name",
			)
			.toArray()) {
			out.set(str(r.name), num(r.cnt));
		}
		return out;
	}

	// ---- sessions -------------------------------------------------------

	startSession(
		id: string,
		agent: string,
		project: string,
		t3codeThreadId: string,
		nowMs: number,
	): Session {
		const pid = this.projectIdByName(project);
		try {
			this.sql.exec(
				"INSERT INTO sessions (id, agent, project_id, t3code_thread_id, started_at, last_active_at) VALUES (?, ?, ?, ?, ?, ?)",
				id,
				agent,
				pid,
				t3codeThreadId,
				nowMs,
				nowMs,
			);
		} catch (err) {
			if (this.tryGetSession(id)) {
				throw hotbedError(ErrorCode.Conflict, `a session called ${id} exists`).withSuggestion(
					"pick another name, or leave it out for a random one",
				);
			}
			throw err;
		}
		return this.getSession(id);
	}

	tryGetSession(id: string): Session | null {
		const rows = this.sql
			.exec(
				`SELECT s.id, s.agent, p.name AS project, s.t3code_thread_id, s.started_at, s.last_active_at, s.ended_at, s.end_reason
				 FROM sessions s JOIN projects p ON p.id = s.project_id WHERE s.id = ?`,
				id,
			)
			.toArray();
		if (rows.length === 0) return null;
		return this.hydrateSession(rows[0]);
	}

	getSession(id: string): Session {
		const s = this.tryGetSession(id);
		if (!s) {
			throw hotbedError(ErrorCode.NotFound, `no session ${id}`).withSuggestion(
				"start one with hotbed session start",
			);
		}
		return s;
	}

	liveSession(id: string, nowMs: number): Session {
		const s = this.getSession(id);
		if (s.endedAt !== undefined) {
			throw sessionEndedError(id, s.endReason);
		}
		this.touchSession(id, nowMs);
		s.lastActiveAt = BigInt(nowMs);
		return s;
	}

	touchSession(id: string, atMs: number): void {
		if (!id) return;
		this.sql.exec(
			"UPDATE sessions SET last_active_at = MAX(last_active_at, ?) WHERE id = ? AND ended_at IS NULL",
			atMs,
			id,
		);
	}

	setSessionT3CodeThreadIdIfEmpty(id: string, threadId: string): void {
		this.sql.exec(
			"UPDATE sessions SET t3code_thread_id = ? WHERE id = ? AND t3code_thread_id = ''",
			threadId,
			id,
		);
	}

	resumeSession(id: string, nowMs: number): void {
		this.sql.exec(
			"UPDATE sessions SET ended_at = NULL, end_reason = '', last_active_at = ? WHERE id = ?",
			nowMs,
			id,
		);
	}

	endSessionAndWorkspaces(id: string, reason: string, nowMs: number): WorkspaceRow[] {
		return this.transactionSync(() => {
			this.sql.exec(
				"UPDATE sessions SET ended_at = ?, end_reason = ? WHERE id = ? AND ended_at IS NULL",
				nowMs,
				reason,
				id,
			);
			return this.sessionWorkspaces(id);
		});
	}

	endSessionIfIdleAndWorkspaces(
		id: string,
		reason: string,
		nowMs: number,
		cutoffMs: number,
	): { ended: boolean; workspaces: WorkspaceRow[] } {
		return this.transactionSync(() => {
			const res = this.sql.exec(
				"UPDATE sessions SET ended_at = ?, end_reason = ? WHERE id = ? AND ended_at IS NULL AND last_active_at <= ?",
				nowMs,
				reason,
				id,
				cutoffMs,
			);
			if (res.rowsWritten === 0) return { ended: false, workspaces: [] };
			return { ended: true, workspaces: this.sessionWorkspaces(id) };
		});
	}

	idleSessions(cutoffMs: number): string[] {
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

	recentSessions(sinceMs: number, project: string, t3codeThreadId: string): Session[] {
		const rows = this.sql
			.exec(
				`SELECT s.id, s.agent, p.name AS project, s.t3code_thread_id, s.started_at, s.last_active_at, s.ended_at, s.end_reason
				 FROM sessions s
				 JOIN projects p ON p.id = s.project_id
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
		return rows.map((r) => this.hydrateSession(r));
	}

	searchSessionsByT3CodeThread(
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
				`SELECT s.id, s.agent, p.name AS project, s.t3code_thread_id, s.started_at, s.last_active_at, s.ended_at, s.end_reason
				 FROM sessions s JOIN projects p ON p.id = s.project_id
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
		return { sessions: rows.map((r) => this.hydrateSession(r)), total };
	}

	sessionProjectsFor(ids: string[]): Map<string, string> {
		const out = new Map<string, string>();
		if (ids.length === 0) return out;
		const unique = [...new Set(ids)];
		const chunkSize = 80;
		for (let i = 0; i < unique.length; i += chunkSize) {
			const chunk = unique.slice(i, i + chunkSize);
			const ph = chunk.map(() => "?").join(",");
			for (const r of this.sql
				.exec(
					`SELECT s.id, p.name AS project FROM sessions s JOIN projects p ON p.id = s.project_id WHERE s.id IN (${ph})`,
					...chunk,
				)
				.toArray()) {
				out.set(str(r.id), str(r.project));
			}
		}
		return out;
	}

	private hydrateSession(r: Record<string, SqlStorageValue>): Session {
		const id = str(r.id);
		const sess = create(SessionSchema, {
			id,
			agent: str(r.agent),
			project: str(r.project),
			t3codeThreadId: str(r.t3code_thread_id),
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

	sessionEventsAfter(sessionId: string, afterSeq: bigint): SessionEvent[] {
		return this.sql
			.exec(
				"SELECT seq, doc FROM session_events WHERE session_id = ? AND seq > ? ORDER BY seq",
				sessionId,
				Number(afterSeq),
			)
			.toArray()
			.map((r) => {
				const ev = unmarshal(SessionEventSchema, str(r.doc));
				ev.seq = big(r.seq);
				return ev;
			});
	}

	private recordSessionEvent(sessionId: string, atMs: bigint, ev: SessionEvent): boolean {
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

	private tellSession(sessionId: string, atMs: bigint, ev: SessionEvent): boolean {
		if (!sessionId) return false;
		const rows = this.sql
			.exec("SELECT 1 FROM sessions WHERE id = ? AND ended_at IS NULL", sessionId)
			.toArray();
		if (rows.length === 0) return false;
		return this.recordSessionEvent(sessionId, atMs, ev);
	}

	// ---- workspaces & commands -----------------------------------------

	addLiveWorkspace(
		id: string,
		sessionId: string,
		nodeId: string,
		agent: string,
		nowMs: number,
	): boolean {
		const res = this.sql.exec(
			`INSERT INTO workspaces (id, session_id, node_id, agent, created_at)
			 SELECT ?, ?, ?, ?, ?
			 WHERE EXISTS (SELECT 1 FROM sessions WHERE id = ? AND ended_at IS NULL)`,
			id,
			sessionId,
			nodeId,
			agent,
			nowMs,
			sessionId,
		);
		return res.rowsWritten > 0;
	}

	dropWorkspace(id: string): void {
		this.sql.exec("DELETE FROM workspaces WHERE id = ?", id);
	}

	getWorkspace(id: string): WorkspaceRow {
		const rows = this.sql
			.exec("SELECT id, session_id, node_id, agent, released_at FROM workspaces WHERE id = ?", id)
			.toArray();
		if (rows.length === 0) {
			throw hotbedError(ErrorCode.NotFound, `no workspace ${id}`);
		}
		const r = rows[0];
		return {
			id: str(r.id),
			session: str(r.session_id),
			node: str(r.node_id),
			agent: str(r.agent),
			released: r.released_at !== null && r.released_at !== undefined,
		};
	}

	sessionWorkspaces(sessionId: string): WorkspaceRow[] {
		return this.sql
			.exec(
				"SELECT id, session_id, node_id, agent FROM workspaces WHERE session_id = ? AND released_at IS NULL ORDER BY created_at, id",
				sessionId,
			)
			.toArray()
			.map((r) => ({
				id: str(r.id),
				session: str(r.session_id),
				node: str(r.node_id),
				agent: str(r.agent),
				released: false,
			}));
	}

	releaseWorkspaceRow(id: string, atMs: number): void {
		this.sql.exec(
			"UPDATE workspaces SET released_at = ? WHERE id = ? AND released_at IS NULL",
			atMs,
			id,
		);
	}

	reconcileMissingWorkspace(id: string, atMs: number): void {
		this.transactionSync(() => {
			this.sql.exec("DELETE FROM node_state WHERE kind = 'workspace' AND id = ?", id);
			this.releaseWorkspaceRow(id, atMs);
		});
	}

	releasedWorkspaces(ids: string[]): Set<string> {
		const out = new Set<string>();
		if (ids.length === 0) return out;
		const unique = [...new Set(ids)];
		for (let i = 0; i < unique.length; i += 80) {
			const chunk = unique.slice(i, i + 80);
			const ph = chunk.map(() => "?").join(",");
			for (const r of this.sql
				.exec(`SELECT id FROM workspaces WHERE id IN (${ph}) AND released_at IS NOT NULL`, ...chunk)
				.toArray()) {
				out.add(str(r.id));
			}
		}
		return out;
	}

	forksSince(sinceMs: number): ForkRow[] {
		return this.sql
			.exec(
				"SELECT created_at, COALESCE(fork_millis, -1) AS fm FROM workspaces WHERE created_at >= ?",
				sinceMs,
			)
			.toArray()
			.map((r) => ({ at: num(r.created_at), millis: num(r.fm) }));
	}

	addCommand(
		id: string,
		sessionId: string,
		nodeId: string,
		workspaceId: string,
		nowMs: number,
	): void {
		this.sql.exec(
			"INSERT OR IGNORE INTO commands (id, session_id, node_id, workspace_id, created_at) VALUES (?, ?, ?, ?, ?)",
			id,
			sessionId,
			nodeId,
			workspaceId,
			nowMs,
		);
	}

	dropCommand(id: string): void {
		this.sql.exec("DELETE FROM commands WHERE id = ?", id);
	}

	lookupCommand(id: string): CommandRoute {
		const rows = this.sql
			.exec("SELECT session_id, node_id FROM commands WHERE id = ?", id)
			.toArray();
		if (rows.length > 0) {
			return {
				session: str(rows[0].session_id),
				node: str(rows[0].node_id),
				histChecked: false,
				baseChecked: false,
			};
		}
		const hRows = this.sql
			.exec("SELECT node_id, session_id FROM history WHERE id = ?", id)
			.toArray();
		if (hRows.length > 0) {
			return {
				session: str(hRows[0].session_id),
				node: str(hRows[0].node_id),
				histChecked: true,
				baseChecked: false,
			};
		}
		const b = this.baseByCommand(id);
		if (b) {
			return {
				session: "",
				node: b.nodeId,
				histChecked: true,
				baseChecked: true,
				base: b.base,
			};
		}
		throw hotbedError(ErrorCode.NotFound, `no command ${id}`);
	}

	viewCommand(id: string): { nodeId: string; cmd: Command } | null {
		const rows = this.sql
			.exec(
				"SELECT node_id, doc FROM node_state WHERE kind = 'command' AND id = ? ORDER BY node_id LIMIT 1",
				id,
			)
			.toArray();
		if (rows.length === 0) return null;
		return { nodeId: str(rows[0].node_id), cmd: unmarshal(CommandSchema, str(rows[0].doc)) };
	}

	baseByCommand(commandId: string): { nodeId: string; base: Base } | null {
		const rows = this.sql
			.exec(
				"SELECT node_id, doc FROM node_state WHERE kind = 'base' AND json_extract(doc, '$.command') = ? ORDER BY node_id, id LIMIT 1",
				commandId,
			)
			.toArray();
		if (rows.length === 0) return null;
		return { nodeId: str(rows[0].node_id), base: unmarshal(BaseSchema, str(rows[0].doc)) };
	}

	// ---- event stream & heartbeats --------------------------------------

	pushEvents(
		nodeId: string,
		req: PushEventsRequest,
		nowMs: number,
		creating: Set<string>,
	): PushResult {
		return this.transactionSync(() => {
			const out: PushResult = {
				applied: 0n,
				appliedAt: 0n,
				checkpointNeeded: false,
				finished: false,
				lost: [],
				sessions: new Set(),
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
				Number(out.applied),
				Number(out.appliedAt),
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
				this.sql.exec(
					"INSERT INTO events (at, kind, node_id, text, ref_type, ref_id) VALUES (?, ?, ?, ?, ?, ?)",
					Number(ev.feed.at),
					ev.feed.kind,
					nodeId,
					ev.feed.text,
					ev.feed.ref?.type ?? "",
					ev.feed.ref?.id ?? "",
				);
			}
			switch (ev.change.case) {
				case "base": {
					const bc = ev.change.value;
					if (bc.base) {
						if (bc.what === "collected") {
							this.dropState(nodeId, "base", bc.base.id);
						} else {
							this.putState(nodeId, "base", bc.base.id, marshal(BaseSchema, bc.base));
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
							this.noteWorkspace(nodeId, ws);
							if (wc.what === "ready" || wc.what === "failed") {
								if (
									this.recordSessionEvent(
										ws.sessionId,
										ev.at,
										workspaceUpdateEvent(WORKSPACE_WHAT[wc.what], ws.id, wc.detail),
									)
								) {
									out.sessions.add(ws.sessionId);
								}
							} else if (wc.what === "synced" || wc.what === "active") {
								this.touchSession(ws.sessionId, Number(ev.at));
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
							this.sql.exec(
								"INSERT OR IGNORE INTO commands (id, session_id, node_id, workspace_id, created_at) VALUES (?, ?, ?, ?, ?)",
								cmd.id,
								cmd.sessionId,
								nodeId,
								cmd.workspaceId,
								Number(ev.at),
							);
							if (cc.finished) {
								this.addHistory(nodeId, cc.finished);
								out.finished = true;
							}
							if (cc.profile) {
								this.saveCommandProfile(nodeId, Number(cmd.endedAt ?? ev.at), cc.profile);
							}
							if (cc.diagnostics) {
								this.saveCommandDiagnostics(nodeId, cc.diagnostics, Number(ev.at));
							}
							const what = COMMAND_WHAT[cc.what];
							if (what !== undefined) {
								if (
									this.recordSessionEvent(
										cmd.sessionId,
										ev.at,
										commandUpdateEvent(what, cmd, cc.detail),
									)
								) {
									out.sessions.add(cmd.sessionId);
								}
							} else if (cc.what === "queued") {
								this.touchSession(cmd.sessionId, Number(ev.at));
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
		const old = this.loadView(nodeId);
		const keptWS = new Set(cp.workspaces.map((w) => w.id));
		const keptCmd = new Set(cp.commands.map((c) => c.id));
		const at = BigInt(nowMs);
		const lostWS = new Set<string>();

		for (const w of old.workspaces) {
			if (keptWS.has(w.id) || w.state === WorkspaceState.RELEASED || !w.sessionId) continue;
			lostWS.add(w.id);
			this.releaseWorkspaceRow(w.id, nowMs);
			if (
				this.recordSessionEvent(
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
				this.releaseWorkspaceRow(id, nowMs);
			}
		}

		for (const c of old.commands) {
			if (keptCmd.has(c.id) || !isCommandLive(c.state)) continue;
			out.lost.push(c.id);
			const lost = clone(CommandSchema, c);
			lost.state = CommandState.CANCELLED;
			if (
				this.recordSessionEvent(
					c.sessionId,
					at,
					commandUpdateEvent(CommandUpdate_What.CANCELLED, lost, LOST_DETAIL),
				)
			) {
				out.sessions.add(c.sessionId);
			}
		}

		this.sql.exec("DELETE FROM node_state WHERE node_id = ?", nodeId);
		for (const b of cp.bases) this.putState(nodeId, "base", b.id, marshal(BaseSchema, b));
		for (const w of cp.workspaces) {
			this.putState(nodeId, "workspace", w.id, marshal(WorkspaceSchema, w));
			this.noteWorkspace(nodeId, w);
		}
		for (const c of cp.commands) {
			this.putState(nodeId, "command", c.id, marshal(CommandSchema, c));
			this.sql.exec(
				"INSERT OR IGNORE INTO commands (id, session_id, node_id, workspace_id, created_at) VALUES (?, ?, ?, ?, ?)",
				c.id,
				c.sessionId,
				nodeId,
				c.workspaceId,
				Number(c.queuedAt),
			);
		}
	}

	private noteWorkspace(nodeId: string, ws: Workspace): void {
		if (!ws.sessionId) return;
		this.sql.exec(
			"INSERT OR IGNORE INTO workspaces (id, session_id, node_id, agent, created_at) VALUES (?, ?, ?, ?, ?)",
			ws.id,
			ws.sessionId,
			nodeId,
			ws.agent,
			Number(ws.createdAt),
		);
		if (ws.forkMillis > 0n) {
			this.sql.exec(
				"UPDATE workspaces SET fork_millis = ? WHERE id = ? AND fork_millis IS NULL",
				Number(ws.forkMillis),
				ws.id,
			);
		}
		if (ws.state === WorkspaceState.RELEASED) {
			this.releaseWorkspaceRow(ws.id, Number(ws.lastActiveAt));
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

	beat(nodeId: string, h: HeartbeatRequest, nowMs: number): void {
		this.transactionSync(() => {
			if (!this.hasNode(nodeId)) {
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
			this.sql.exec(
				"INSERT OR REPLACE INTO samples (node_id, t, cpu, memory, psi, cpu_psi, holding, running, queued, parked, oom) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
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
			);
			for (const session of h.activeSessions) {
				this.touchSession(session, nowMs);
			}
		});
		this.samplesGen++;
	}

	loadView(nodeId: string): NodeStateView {
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

	nodeCheckpoint(nodeId: string): Checkpoint {
		const r = this.sql.exec("SELECT applied FROM nodes WHERE id = ?", nodeId).one();
		const v = this.loadView(nodeId);
		return create(CheckpointSchema, {
			seq: big(r.applied),
			bases: v.bases,
			workspaces: v.workspaces,
			commands: v.commands,
		});
	}

	// ---- history, profiles, diagnostics, samples, events, placements ----

	private addHistory(nodeId: string, f: FinishedCommand): void {
		let projectId: number | null = null;
		if (f.sessionId) {
			const rows = this.sql
				.exec("SELECT project_id FROM sessions WHERE id = ?", f.sessionId)
				.toArray();
			if (rows.length > 0) projectId = num(rows[0].project_id);
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
			Number(f.endedAt),
			marshal(FinishedCommandSchema, stored),
		);
	}

	finishedCommand(id: string): FinishedCommand | null {
		const rows = this.sql
			.exec(
				"SELECT h.doc, COALESCE(p.name, '') AS project FROM history h LEFT JOIN projects p ON p.id = h.project_id WHERE h.id = ?",
				id,
			)
			.toArray();
		if (rows.length === 0) return null;
		const f = unmarshal(FinishedCommandSchema, str(rows[0].doc));
		f.project = str(rows[0].project);
		return f;
	}

	historySince(sinceMs: number): FinishedCommand[] {
		return this.sql
			.exec(
				"SELECT h.doc, COALESCE(p.name, '') AS project FROM history h LEFT JOIN projects p ON p.id = h.project_id WHERE h.ended_at >= ? ORDER BY h.ended_at",
				sinceMs,
			)
			.toArray()
			.map((r) => {
				const f = unmarshal(FinishedCommandSchema, str(r.doc));
				f.project = str(r.project);
				return f;
			});
	}

	historyMatching(
		filter: { project: string; session: string; match: string },
		limit: number,
	): { items: FinishedCommand[]; total: number } {
		const total = num(
			this.sql
				.exec(
					`SELECT COUNT(*) AS cnt FROM history h LEFT JOIN projects p ON p.id = h.project_id
					 WHERE (? = '' OR p.name = ?)
					   AND (? = '' OR h.session_id = ?)
					   AND (? = '' OR instr(json_extract(h.doc, '$.argv'), ?) > 0)`,
					filter.project,
					filter.project,
					filter.session,
					filter.session,
					filter.match,
					filter.match,
				)
				.one().cnt,
		);
		const lim = limit > 0 ? limit : -1;
		const items = this.sql
			.exec(
				`SELECT h.doc, COALESCE(p.name, '') AS project FROM history h LEFT JOIN projects p ON p.id = h.project_id
				 WHERE (? = '' OR p.name = ?)
				   AND (? = '' OR h.session_id = ?)
				   AND (? = '' OR instr(json_extract(h.doc, '$.argv'), ?) > 0)
				 ORDER BY h.ended_at DESC
				 LIMIT ?`,
				filter.project,
				filter.project,
				filter.session,
				filter.session,
				filter.match,
				filter.match,
				lim,
			)
			.toArray()
			.map((r) => {
				const f = unmarshal(FinishedCommandSchema, str(r.doc));
				f.project = str(r.project);
				return f;
			});
		return { items, total };
	}

	inHistory(ids: string[]): Set<string> {
		const out = new Set<string>();
		if (ids.length === 0) return out;
		const unique = [...new Set(ids)];
		for (let i = 0; i < unique.length; i += 80) {
			const chunk = unique.slice(i, i + 80);
			const ph = chunk.map(() => "?").join(",");
			for (const r of this.sql
				.exec(`SELECT id FROM history WHERE id IN (${ph})`, ...chunk)
				.toArray()) {
				out.add(str(r.id));
			}
		}
		return out;
	}

	saveCommandProfile(nodeId: string, endedAtMs: number, profile: CommandProfile): void {
		const bytes = toBinary(CommandProfileSchema, profile);
		this.sql.exec(
			"INSERT INTO command_profiles (id, node_id, ended_at, doc) VALUES (?, ?, ?, ?) ON CONFLICT (id) DO UPDATE SET ended_at = excluded.ended_at, doc = excluded.doc",
			profile.commandId,
			nodeId,
			endedAtMs,
			bytes,
		);
	}

	getCommandProfile(id: string): CommandProfile | null {
		const rows = this.sql.exec("SELECT doc FROM command_profiles WHERE id = ?", id).toArray();
		if (rows.length === 0) return null;
		const raw = rows[0].doc;
		if (raw instanceof ArrayBuffer) return fromBinary(CommandProfileSchema, new Uint8Array(raw));
		return null;
	}

	saveCommandDiagnostics(nodeId: string, d: CommandDiagnostics, nowMs = Date.now()): void {
		const seq = d.intervention?.seq ?? 0;
		const at = Number(d.intervention?.by?.at || BigInt(nowMs));
		this.sql.exec(
			"INSERT INTO command_diagnostics (command_id, seq, node_id, created_at, doc) VALUES (?, ?, ?, ?, ?) ON CONFLICT (command_id, seq) DO UPDATE SET node_id = excluded.node_id, created_at = excluded.created_at, doc = excluded.doc",
			d.commandId,
			seq,
			nodeId,
			at,
			marshal(CommandDiagnosticsSchema, d),
		);
	}

	getCommandDiagnostic(commandId: string, seq: number): CommandDiagnostics | null {
		const rows = this.sql
			.exec("SELECT doc FROM command_diagnostics WHERE command_id = ? AND seq = ?", commandId, seq)
			.toArray();
		return rows.length > 0 ? unmarshal(CommandDiagnosticsSchema, str(rows[0].doc)) : null;
	}

	listCommandDiagnosticsAfter(commandId: string, afterSeq: number): CommandDiagnostics[] {
		return this.sql
			.exec(
				"SELECT doc FROM command_diagnostics WHERE command_id = ? AND seq > ? ORDER BY seq",
				commandId,
				afterSeq,
			)
			.toArray()
			.map((r) => unmarshal(CommandDiagnosticsSchema, str(r.doc)));
	}

	hasCommandDiagnostics(commandId: string): boolean {
		return (
			this.sql
				.exec("SELECT 1 FROM command_diagnostics WHERE command_id = ? LIMIT 1", commandId)
				.toArray().length > 0
		);
	}

	samplesSince(sinceMs: number): SampleRow[] {
		return this.sql
			.exec(
				"SELECT node_id, t, cpu, memory, psi, cpu_psi, holding, running, queued, parked, oom FROM samples WHERE t >= ? ORDER BY t",
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
			}));
	}

	recentEvents(limit: number): ClusterEvent[] {
		return this.sql
			.exec(
				"SELECT id, at, kind, node_id, text, ref_type, ref_id FROM events ORDER BY id DESC LIMIT ?",
				limit,
			)
			.toArray()
			.map((r) =>
				create(ClusterEventSchema, {
					id: big(r.id),
					at: big(r.at),
					kind: str(r.kind),
					nodeId: str(r.node_id),
					text: str(r.text),
					ref:
						str(r.ref_type) || str(r.ref_id)
							? create(EventRefSchema, { type: str(r.ref_type), id: str(r.ref_id) })
							: undefined,
				}),
			);
	}

	addPlacement(d: PlacementDecision, nowMs: number): void {
		d.at = BigInt(nowMs);
		const stored = clone(PlacementDecisionSchema, d);
		stored.id = 0n;
		this.sql.exec(
			"INSERT INTO placements (at, session_id, doc) VALUES (?, ?, ?)",
			nowMs,
			d.sessionId,
			marshal(PlacementDecisionSchema, stored),
		);
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

	// ---- schedules ------------------------------------------------------

	putSchedule(sc: ScheduledCommand): void {
		const pid = this.projectIdByName(sc.project);
		const stored = clone(ScheduledCommandSchema, sc);
		stored.project = "";
		stored.nextAt = undefined;
		stored.runs = [];
		stored.reminder = undefined;
		this.sql.exec(
			`INSERT INTO schedules (name, project_id, doc, next_at)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT (name) DO UPDATE SET project_id = excluded.project_id, doc = excluded.doc, next_at = excluded.next_at`,
			sc.name,
			pid,
			marshal(ScheduledCommandSchema, stored),
			sc.nextAt !== undefined ? Number(sc.nextAt) : null,
		);
	}

	getSchedule(name: string): ScheduledCommand {
		const rows = this.sql
			.exec(
				"SELECT s.doc, s.next_at, p.name AS project FROM schedules s JOIN projects p ON p.id = s.project_id WHERE s.name = ?",
				name,
			)
			.toArray();
		if (rows.length === 0) {
			throw hotbedError(ErrorCode.NotFound, `no scheduled command ${name}`).withSuggestion(
				"hotbed schedule ls",
			);
		}
		const sc = unmarshal(ScheduledCommandSchema, str(rows[0].doc));
		sc.project = str(rows[0].project);
		sc.nextAt = optBig(rows[0].next_at);
		return sc;
	}

	listSchedules(project: string): ScheduledCommand[] {
		return this.sql
			.exec(
				`SELECT s.doc, s.next_at, p.name AS project
				 FROM schedules s JOIN projects p ON p.id = s.project_id
				 WHERE (? = '' OR p.name = ?)
				 ORDER BY s.name`,
				project,
				project,
			)
			.toArray()
			.map((r) => {
				const sc = unmarshal(ScheduledCommandSchema, str(r.doc));
				sc.project = str(r.project);
				sc.nextAt = optBig(r.next_at);
				return sc;
			});
	}

	deleteSchedule(name: string): boolean {
		return this.sql.exec("DELETE FROM schedules WHERE name = ?", name).rowsWritten > 0;
	}

	setScheduleNextAt(name: string, nextAt: bigint | undefined): void {
		this.sql.exec(
			"UPDATE schedules SET next_at = ? WHERE name = ?",
			nextAt !== undefined ? Number(nextAt) : null,
			name,
		);
	}

	addScheduleRun(run: ScheduledRun): ScheduledRun {
		const stored = clone(ScheduledRunSchema, run);
		stored.id = 0n;
		this.sql.exec(
			"INSERT INTO schedule_runs (schedule, session_id, command_id, ended_at, doc) VALUES (?, ?, ?, ?, ?)",
			run.schedule,
			run.sessionId,
			run.commandId,
			run.endedAt !== undefined ? Number(run.endedAt) : null,
			marshal(ScheduledRunSchema, stored),
		);
		const id = num(this.sql.exec("SELECT last_insert_rowid() AS id").one().id);
		run.id = BigInt(id);
		return run;
	}

	updateLiveScheduleRun(run: ScheduledRun): boolean {
		const stored = clone(ScheduledRunSchema, run);
		stored.id = 0n;
		const res = this.sql.exec(
			"UPDATE schedule_runs SET session_id = ?, command_id = ?, ended_at = ?, doc = ? WHERE id = ? AND ended_at IS NULL",
			run.sessionId,
			run.commandId,
			run.endedAt !== undefined ? Number(run.endedAt) : null,
			marshal(ScheduledRunSchema, stored),
			Number(run.id),
		);
		return res.rowsWritten > 0;
	}

	getScheduleRun(id: bigint): ScheduledRun {
		const rows = this.sql
			.exec("SELECT id, doc FROM schedule_runs WHERE id = ?", Number(id))
			.toArray();
		if (rows.length === 0) throw hotbedError(ErrorCode.NotFound, `no scheduled run ${id}`);
		const r = unmarshal(ScheduledRunSchema, str(rows[0].doc));
		r.id = big(rows[0].id);
		return r;
	}

	liveScheduleRun(schedule: string): ScheduledRun | null {
		const rows = this.sql
			.exec(
				"SELECT id, doc FROM schedule_runs WHERE schedule = ? AND ended_at IS NULL ORDER BY id DESC LIMIT 1",
				schedule,
			)
			.toArray();
		if (rows.length === 0) return null;
		const r = unmarshal(ScheduledRunSchema, str(rows[0].doc));
		r.id = big(rows[0].id);
		return r;
	}

	listAllLiveScheduleRuns(): ScheduledRun[] {
		return this.sql
			.exec("SELECT id, doc FROM schedule_runs WHERE ended_at IS NULL ORDER BY id")
			.toArray()
			.map((row) => {
				const r = unmarshal(ScheduledRunSchema, str(row.doc));
				r.id = big(row.id);
				return r;
			});
	}

	listLiveScheduleRunsBySession(sessionId: string): ScheduledRun[] {
		return this.sql
			.exec(
				"SELECT id, doc FROM schedule_runs WHERE session_id = ? AND ended_at IS NULL",
				sessionId,
			)
			.toArray()
			.map((row) => {
				const r = unmarshal(ScheduledRunSchema, str(row.doc));
				r.id = big(row.id);
				return r;
			});
	}

	listScheduleRuns(schedule: string, limit: number): ScheduledRun[] {
		return this.sql
			.exec(
				"SELECT id, doc FROM schedule_runs WHERE schedule = ? ORDER BY id DESC LIMIT ?",
				schedule,
				limit,
			)
			.toArray()
			.map((row) => {
				const r = unmarshal(ScheduledRunSchema, str(row.doc));
				r.id = big(row.id);
				return r;
			});
	}

	recentScheduleRuns(project: string, perSchedule: number): Map<string, ScheduledRun[]> {
		const out = new Map<string, ScheduledRun[]>();
		const rows = this.sql
			.exec(
				`SELECT r.id, r.schedule, r.doc FROM (
					SELECT sr.id, sr.schedule, sr.doc,
					       ROW_NUMBER() OVER (PARTITION BY sr.schedule ORDER BY sr.id DESC) AS rn
					FROM schedule_runs sr
					JOIN schedules s ON s.name = sr.schedule
					JOIN projects p ON p.id = s.project_id
					WHERE (? = '' OR p.name = ?)
				) r WHERE r.rn <= ? ORDER BY r.schedule, r.id DESC`,
				project,
				project,
				perSchedule,
			)
			.toArray();
		for (const row of rows) {
			const sched = str(row.schedule);
			const run = unmarshal(ScheduledRunSchema, str(row.doc));
			run.id = big(row.id);
			let list = out.get(sched);
			if (!list) {
				list = [];
				out.set(sched, list);
			}
			list.push(run);
		}
		return out;
	}

	// ---- questions ------------------------------------------------------

	addQuestions(
		project: string,
		repo: string,
		prompts: Prompt[],
		decided: Answer[] | null,
		by: Stamp,
		from?: AskedFrom,
	): Question[] {
		return this.transactionSync(() => {
			const pid = this.projectIdByName(project);
			const top = num(this.sql.exec("SELECT COALESCE(MIN(rank), 0) AS m FROM questions").one().m);
			const out: Question[] = [];
			for (let i = 0; i < prompts.length; i++) {
				const rank = BigInt(top - (prompts.length - i));
				const item = create(QuestionSchema, {
					prompt: prompts[i],
					state: QuestionState.OPEN,
					rank,
					asked: by,
					askedFrom: from,
					updatedAt: by.at,
					project,
					repo,
				});
				this.sql.exec(
					"INSERT INTO questions (project_id, repo, state, rank, asked_by, asked_at, updated_at, doc) VALUES (?, ?, ?, ?, ?, ?, ?, '')",
					pid,
					repo,
					questionStateName(item.state),
					Number(rank),
					by.session,
					Number(by.at),
					Number(by.at),
				);
				item.id = num(this.sql.exec("SELECT last_insert_rowid() AS id").one().id);
				this.saveQuestion(item, "asked", by);
				if (decided) {
					item.state = QuestionState.ANSWERED;
					item.answer = decided[i];
					item.answered = by;
					this.saveQuestion(item, "answered", by);
					item.state = QuestionState.HANDLED;
					item.handled = by;
					this.saveQuestion(item, "handled", by);
				}
				out.push(item);
			}
			return out;
		});
	}

	getQuestion(id: number): Question {
		const rows = this.sql
			.exec(
				"SELECT q.id, q.rank, p.name AS project, q.doc FROM questions q JOIN projects p ON p.id = q.project_id WHERE q.id = ?",
				id,
			)
			.toArray();
		if (rows.length === 0) throw hotbedError(ErrorCode.NotFound, `no question ${id}`);
		const q = unmarshal(QuestionSchema, str(rows[0].doc));
		q.id = num(rows[0].id);
		q.rank = big(rows[0].rank);
		q.project = str(rows[0].project);
		return q;
	}

	questionHistory(id: number): QuestionChange[] {
		return this.sql
			.exec(
				"SELECT seq, what, at, session, host, reason, doc FROM question_changes WHERE question_id = ? ORDER BY seq",
				id,
			)
			.toArray()
			.map((r) =>
				create(QuestionChangeSchema, {
					seq: big(r.seq),
					what: str(r.what),
					by: create(StampSchema, {
						at: big(r.at),
						session: str(r.session),
						host: str(r.host),
						reason: str(r.reason),
					}),
					question: unmarshal(QuestionSchema, str(r.doc)),
				}),
			);
	}

	listQuestions(filter: {
		states: QuestionState[];
		order: QuestionOrder;
		project: string;
		askedBy: string;
		limit: number;
	}): { questions: Question[]; total: number } {
		const states =
			filter.states.length > 0
				? filter.states.map(questionStateName)
				: [questionStateName(QuestionState.OPEN)];
		const ph = states.map(() => "?").join(",");
		const total = num(
			this.sql
				.exec(
					`SELECT COUNT(*) AS total FROM questions q JOIN projects p ON p.id = q.project_id
					 WHERE q.state IN (${ph}) AND (? = '' OR p.name = ?) AND (? = '' OR q.asked_by = ?)`,
					...states,
					filter.project,
					filter.project,
					filter.askedBy,
					filter.askedBy,
				)
				.one().total,
		);
		const orderBy =
			filter.order === QuestionOrder.OLDEST
				? "q.asked_at ASC, q.id ASC"
				: filter.order === QuestionOrder.UPDATED
					? "q.updated_at DESC, q.id DESC"
					: filter.order === QuestionOrder.RANK
						? "q.rank ASC, q.id ASC"
						: "q.asked_at DESC, q.id DESC";
		const lim = filter.limit > 0 ? filter.limit : -1;
		const questions = this.sql
			.exec(
				`SELECT q.id, q.rank, p.name AS project, q.doc FROM questions q JOIN projects p ON p.id = q.project_id
				 WHERE q.state IN (${ph}) AND (? = '' OR p.name = ?) AND (? = '' OR q.asked_by = ?)
				 ORDER BY ${orderBy} LIMIT ?`,
				...states,
				filter.project,
				filter.project,
				filter.askedBy,
				filter.askedBy,
				lim,
			)
			.toArray()
			.map((r) => {
				const q = unmarshal(QuestionSchema, str(r.doc));
				q.id = num(r.id);
				q.rank = big(r.rank);
				q.project = str(r.project);
				return q;
			});
		return { questions, total };
	}

	questionProjects(): QuestionProject[] {
		return this.sql
			.exec(
				`SELECT p.name,
				        SUM(q.state = ?) AS open_count,
				        SUM(q.state IN (?, ?)) AS outbox_count,
				        COUNT(*) AS total
				 FROM questions q JOIN projects p ON p.id = q.project_id
				 GROUP BY p.name ORDER BY p.name`,
				questionStateName(QuestionState.OPEN),
				questionStateName(QuestionState.ANSWERED),
				questionStateName(QuestionState.DISMISSED),
			)
			.toArray()
			.map((r) =>
				create(QuestionProjectSchema, {
					name: str(r.name),
					open: num(r.open_count),
					outbox: num(r.outbox_count),
					total: num(r.total),
				}),
			);
	}

	openQuestionsCount(): number {
		return num(
			this.sql
				.exec(
					"SELECT COUNT(*) AS cnt FROM questions WHERE state = ?",
					questionStateName(QuestionState.OPEN),
				)
				.one().cnt,
		);
	}

	changeQuestion(id: number, what: string, by: Stamp, edit: (q: Question) => void): Question {
		return this.transactionSync(() => this.changeQuestionTx(id, what, by, edit));
	}

	changeQuestions(ids: number[], what: string, by: Stamp, edit: (q: Question) => void): Question[] {
		return this.transactionSync(() => ids.map((id) => this.changeQuestionTx(id, what, by, edit)));
	}

	reopenQuestion(id: number, by: Stamp, edit: (q: Question) => void): Question {
		return this.transactionSync(() => {
			const top = num(this.sql.exec("SELECT COALESCE(MIN(rank), 0) AS m FROM questions").one().m);
			return this.changeQuestionTx(id, "reopened", by, (item) => {
				edit(item);
				item.rank = BigInt(top - 1);
			});
		});
	}

	moveQuestion(
		id: number,
		place: { before?: number; first?: boolean },
		by: Stamp,
		check: (q: Question, before: Question | null) => void,
	): Question {
		return this.transactionSync(() => {
			let beforeId = place.before ?? 0;
			if (place.first) {
				const item = this.getQuestion(id);
				const rows = this.sql
					.exec(
						`SELECT q.id FROM questions q JOIN projects p ON p.id = q.project_id
						 WHERE p.name = ? AND q.state = ? ORDER BY q.rank, q.id LIMIT 1`,
						item.project,
						questionStateName(QuestionState.OPEN),
					)
					.toArray();
				if (rows.length === 0 || num(rows[0].id) === id) {
					check(item, null);
					return item;
				}
				beforeId = num(rows[0].id);
			}
			const next = beforeId !== 0 ? this.getQuestion(beforeId) : null;
			return this.changeQuestionTx(id, "moved", by, (item) => {
				check(item, next);
				if (!next) {
					const maxRank = num(
						this.sql.exec("SELECT COALESCE(MAX(rank), 0) AS m FROM questions").one().m,
					);
					item.rank = BigInt(maxRank + 1);
				} else {
					this.sql.exec(
						"UPDATE questions SET rank = rank + 1 WHERE rank >= ? AND id != ?",
						Number(next.rank),
						item.id,
					);
					item.rank = next.rank;
				}
			});
		});
	}

	private changeQuestionTx(
		id: number,
		what: string,
		by: Stamp,
		edit: (q: Question) => void,
	): Question {
		const item = this.getQuestion(id);
		edit(item);
		if (what !== "moved") item.updatedAt = by.at;
		this.saveQuestion(item, what, by);
		if (what === "answered" || what === "dismissed") {
			const stamp = what === "dismissed" ? item.dismissed : item.answered;
			const w = what === "dismissed" ? QuestionUpdate_What.DISMISSED : QuestionUpdate_What.ANSWERED;
			const copy = clone(QuestionSchema, item);
			copy.history = [];
			this.tellSession(
				item.asked?.session ?? "",
				stamp?.at ?? by.at,
				create(SessionEventSchema, {
					detail: questionResponseLine(item),
					event: {
						case: "question",
						value: create(QuestionUpdateSchema, { what: w, question: copy }),
					},
				}),
			);
		}
		return item;
	}

	private saveQuestion(item: Question, what: string, by: Stamp): void {
		item.revision += 1n;
		const stored = clone(QuestionSchema, item);
		stored.rank = 0n;
		stored.history = [];
		stored.project = "";
		this.sql.exec(
			"UPDATE questions SET state = ?, rank = ?, updated_at = ?, doc = ? WHERE id = ?",
			questionStateName(item.state),
			Number(item.rank),
			Number(item.updatedAt),
			marshal(QuestionSchema, stored),
			item.id,
		);
		stored.rank = item.rank;
		stored.project = item.project;
		this.sql.exec(
			"INSERT INTO question_changes (question_id, what, at, session, host, reason, doc) VALUES (?, ?, ?, ?, ?, ?, ?)",
			item.id,
			what,
			Number(by.at),
			by.session,
			by.host,
			by.reason,
			marshal(QuestionSchema, stored),
		);
	}

	// ---- reviews --------------------------------------------------------

	addReview(project: string, r: Review): Review {
		return this.transactionSync(() => {
			const pid = this.projectIdByName(project);
			const reqAt = Number(r.requested?.at ?? 0n);
			this.sql.exec(
				"INSERT INTO reviews (project_id, state, requested_by, requested_at, updated_at, doc) VALUES (?, ?, ?, ?, ?, '')",
				pid,
				reviewStateName(r.state),
				r.requested?.session ?? "",
				reqAt,
				reqAt,
			);
			r.id = num(this.sql.exec("SELECT last_insert_rowid() AS id").one().id);
			r.project = project;
			r.updatedAt = BigInt(reqAt);
			this.saveReview(r, "requested", r.requested!);
			return r;
		});
	}

	getReview(id: number, withDetail: boolean): Review {
		const rows = this.sql
			.exec(
				"SELECT r.id, p.name AS project, r.doc FROM reviews r JOIN projects p ON p.id = r.project_id WHERE r.id = ?",
				id,
			)
			.toArray();
		if (rows.length === 0) throw hotbedError(ErrorCode.NotFound, `no review ${id}`);
		const r = unmarshal(ReviewSchema, str(rows[0].doc));
		r.id = num(rows[0].id);
		r.project = str(rows[0].project);
		this.attachReviewCountsAndDetails(r, withDetail);
		return r;
	}

	listReviews(project: string, states: ReviewState[], limit: number): Review[] {
		const lim = limit > 0 ? limit : -1;
		let rows: Array<Record<string, SqlStorageValue>>;
		if (states.length > 0) {
			const names = states.map(reviewStateName);
			const ph = names.map(() => "?").join(",");
			rows = this.sql
				.exec(
					`SELECT r.id, p.name AS project, r.doc FROM reviews r JOIN projects p ON p.id = r.project_id
					 WHERE (? = '' OR p.name = ?) AND r.state IN (${ph})
					 ORDER BY r.requested_at DESC, r.id DESC LIMIT ?`,
					project,
					project,
					...names,
					lim,
				)
				.toArray();
		} else {
			rows = this.sql
				.exec(
					`SELECT r.id, p.name AS project, r.doc FROM reviews r JOIN projects p ON p.id = r.project_id
					 WHERE (? = '' OR p.name = ?)
					 ORDER BY r.requested_at DESC, r.id DESC LIMIT ?`,
					project,
					project,
					lim,
				)
				.toArray();
		}
		return rows.map((row) => {
			const r = unmarshal(ReviewSchema, str(row.doc));
			r.id = num(row.id);
			r.project = str(row.project);
			this.attachReviewCountsAndDetails(r, false);
			return r;
		});
	}

	reviewCounts(): { active: Map<string, number>; toDecide: Map<string, number> } {
		const active = new Map<string, number>();
		const toDecide = new Map<string, number>();
		for (const r of this.sql
			.exec(
				`SELECT p.name, COUNT(*) AS cnt FROM reviews r JOIN projects p ON p.id = r.project_id
				 WHERE r.state IN (?, ?) GROUP BY p.name`,
				reviewStateName(ReviewState.REQUESTED),
				reviewStateName(ReviewState.IN_PROGRESS),
			)
			.toArray()) {
			active.set(str(r.name), num(r.cnt));
		}
		for (const r of this.sql
			.exec(
				`SELECT p.name, COUNT(DISTINCT r.id) AS cnt
				 FROM reviews r
				 JOIN projects p ON p.id = r.project_id
				 JOIN review_findings f ON f.review_id = r.id
				 WHERE r.state = ?
				   AND COALESCE(json_extract(f.doc, '$.decision'), 'FINDING_DECISION_OPEN') IN ('FINDING_DECISION_OPEN', 'FINDING_DECISION_UNSPECIFIED')
				 GROUP BY p.name`,
				reviewStateName(ReviewState.DONE),
			)
			.toArray()) {
			toDecide.set(str(r.name), num(r.cnt));
		}
		return { active, toDecide };
	}

	changeReview(id: number, what: string, by: Stamp, edit: (r: Review) => void): Review {
		return this.transactionSync(() => {
			const r = this.getReview(id, false);
			edit(r);
			r.updatedAt = by.at;
			this.saveReview(r, what, by);
			if (what === "finished" || what === "failed") {
				const word = what === "finished" ? "is done" : "failed";
				let line = `review ${r.id} (${r.title}) ${word}`;
				if (r.summary) line += `: ${firstLine(r.summary)}`;
				const w = what === "finished" ? ReviewUpdate_What.FINISHED : ReviewUpdate_What.FAILED;
				this.tellSession(
					r.requested?.session ?? "",
					by.at,
					create(SessionEventSchema, {
						detail: line,
						event: {
							case: "review",
							value: create(ReviewUpdateSchema, { what: w, reviewId: r.id }),
						},
					}),
				);
			}
			return r;
		});
	}

	addFindings(
		reviewId: number,
		by: Stamp,
		check: (r: Review) => void,
		findings: Finding[],
		report: string,
	): Finding[] {
		return this.transactionSync(() => {
			const r = this.getReview(reviewId, false);
			check(r);
			if (report) {
				r.report = report;
				r.updatedAt = by.at;
				this.saveReview(r, "reported", by);
			}
			if (findings.length === 0) {
				this.insertReviewEvent(create(ReviewEventSchema, { reviewId, what: "found none", by }), "");
			}
			for (const f of findings) {
				this.sql.exec("INSERT INTO review_findings (review_id, doc) VALUES (?, '')", reviewId);
				f.id = num(this.sql.exec("SELECT last_insert_rowid() AS id").one().id);
				f.reviewId = reviewId;
				f.found = by;
				f.decision = FindingDecision.OPEN;
				this.saveFinding(f, "found", by);
			}
			this.sql.exec(
				"UPDATE reviews SET updated_at = MAX(updated_at, ?) WHERE id = ?",
				Number(by.at),
				reviewId,
			);
			return findings;
		});
	}

	changeFindings(
		reviewId: number,
		findingIds: number[],
		what: string,
		by: Stamp,
		edit: (r: Review, f: Finding) => void,
	): Finding[] {
		return this.transactionSync(() => {
			const r = this.getReview(reviewId, false);
			const out: Finding[] = [];
			for (const fid of findingIds) {
				const f = this.getFinding(reviewId, fid);
				edit(r, f);
				this.saveFinding(f, what, by);
				out.push(f);
			}
			this.sql.exec(
				"UPDATE reviews SET updated_at = MAX(updated_at, ?) WHERE id = ?",
				Number(by.at),
				reviewId,
			);
			return out;
		});
	}

	addReviewResponse(
		e: ReviewEvent,
		afterSeq: bigint,
		check: (r: Review, f: Finding | null) => void,
	): ReviewEvent {
		return this.transactionSync(() => {
			const r = this.getReview(e.reviewId, false);
			const f = e.findingId !== 0 ? this.getFinding(r.id, e.findingId) : null;
			check(r, f);
			if (afterSeq > 0n) {
				const newer = this.sql
					.exec(
						"SELECT seq FROM review_events WHERE review_id = ? AND seq > ? ORDER BY seq",
						r.id,
						Number(afterSeq),
					)
					.toArray();
				if (newer.length > 0) {
					const latest = big(newer[newer.length - 1].seq);
					throw hotbedError(
						ErrorCode.Conflict,
						`review ${r.id} has newer events (latest is ${latest}, not ${afterSeq})`,
					).withSuggestion(`hotbed review responses ${r.id}`);
				}
			}
			const about = f ? `finding ${f.id} (${f.title}) of review ${r.id}` : `review ${r.id}`;
			e.what = "responded";
			e.seq = this.insertReviewEvent(e, "");
			this.sql.exec(
				"UPDATE reviews SET updated_at = MAX(updated_at, ?) WHERE id = ?",
				Number(e.by?.at ?? 0n),
				r.id,
			);
			const requester = r.requested?.session ?? "";
			if (requester && requester !== (e.by?.session ?? "")) {
				this.tellSession(
					requester,
					e.by?.at ?? 0n,
					create(SessionEventSchema, {
						detail: reviewResponseOn(about, e),
						event: {
							case: "review",
							value: create(ReviewUpdateSchema, {
								what: ReviewUpdate_What.RESPONDED,
								reviewId: r.id,
							}),
						},
					}),
				);
			}
			return e;
		});
	}

	reviewEventsAfter(reviewId: number, afterSeq: bigint): ReviewEvent[] {
		return this.sql
			.exec(
				"SELECT seq, finding_id, what, at, session, host, quick, text FROM review_events WHERE review_id = ? AND seq > ? ORDER BY seq",
				reviewId,
				Number(afterSeq),
			)
			.toArray()
			.map((row) =>
				create(ReviewEventSchema, {
					seq: big(row.seq),
					reviewId,
					findingId: num(row.finding_id),
					what: str(row.what),
					by: create(StampSchema, {
						at: big(row.at),
						session: str(row.session),
						host: str(row.host),
					}),
					quick: quickResponseFromName(str(row.quick)),
					text: str(row.text),
				}),
			);
	}

	private getFinding(reviewId: number, fid: number): Finding {
		const rows = this.sql
			.exec("SELECT id, doc FROM review_findings WHERE review_id = ? AND id = ?", reviewId, fid)
			.toArray();
		if (rows.length === 0) {
			throw hotbedError(ErrorCode.NotFound, `review ${reviewId} has no finding ${fid}`);
		}
		const f = unmarshal(FindingSchema, str(rows[0].doc));
		f.id = num(rows[0].id);
		f.reviewId = reviewId;
		return f;
	}

	private attachReviewCountsAndDetails(r: Review, withDetail: boolean): void {
		const fRows = this.sql
			.exec("SELECT id, doc FROM review_findings WHERE review_id = ? ORDER BY id", r.id)
			.toArray();
		const findings: Finding[] = [];
		let important = 0;
		let preExisting = 0;
		let nits = 0;
		let open = 0;
		for (const fr of fRows) {
			const f = unmarshal(FindingSchema, str(fr.doc));
			f.id = num(fr.id);
			f.reviewId = r.id;
			if (f.severity === 1) important++;
			else if (f.severity === 2) preExisting++;
			else if (f.severity === 3) nits++;
			if (f.decision === FindingDecision.OPEN || f.decision === FindingDecision.UNSPECIFIED) open++;
			findings.push(f);
		}
		const events = this.reviewEventsAfter(r.id, 0n);
		let responses = 0;
		const byFinding = new Map<number, ReviewEvent[]>();
		for (const ev of events) {
			if (ev.what === "responded") {
				responses++;
				if (ev.findingId !== 0) {
					let arr = byFinding.get(ev.findingId);
					if (!arr) {
						arr = [];
						byFinding.set(ev.findingId, arr);
					}
					arr.push(ev);
				}
			}
		}
		r.important = important;
		r.preExisting = preExisting;
		r.nits = nits;
		r.open = open;
		r.responses = responses;
		if (withDetail) {
			for (const f of findings) {
				const fResps = byFinding.get(f.id) ?? [];
				f.responses = fResps.length;
				for (const ev of fResps) {
					if (ev.quick !== QuickResponse.UNSPECIFIED) {
						f.quick = ev.quick;
					}
				}
			}
			r.findings = findings;
			r.events = events;
		}
	}

	private saveReview(r: Review, what: string, by: Stamp): void {
		const stored = clone(ReviewSchema, r);
		stored.project = "";
		stored.findings = [];
		stored.events = [];
		stored.important = 0;
		stored.preExisting = 0;
		stored.nits = 0;
		stored.open = 0;
		stored.responses = 0;
		const doc = marshal(ReviewSchema, stored);
		this.sql.exec(
			"UPDATE reviews SET state = ?, updated_at = ?, doc = ? WHERE id = ?",
			reviewStateName(r.state),
			Number(r.updatedAt),
			doc,
			r.id,
		);
		this.insertReviewEvent(
			create(ReviewEventSchema, { reviewId: r.id, what, by, text: by.reason }),
			doc,
		);
	}

	private saveFinding(f: Finding, what: string, by: Stamp): void {
		const stored = clone(FindingSchema, f);
		stored.responses = 0;
		const doc = marshal(FindingSchema, stored);
		this.sql.exec("UPDATE review_findings SET doc = ? WHERE id = ?", doc, f.id);
		this.insertReviewEvent(
			create(ReviewEventSchema, {
				reviewId: f.reviewId,
				findingId: f.id,
				what,
				by,
				text: by.reason,
			}),
			doc,
		);
	}

	private insertReviewEvent(e: ReviewEvent, doc: string): bigint {
		this.sql.exec(
			"INSERT INTO review_events (review_id, finding_id, what, at, session, host, quick, text, doc) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
			e.reviewId,
			e.findingId,
			e.what,
			Number(e.by?.at ?? 0n),
			e.by?.session ?? "",
			e.by?.host ?? "",
			quickResponseName(e.quick),
			e.text,
			doc,
		);
		return big(this.sql.exec("SELECT last_insert_rowid() AS id").one().id);
	}

	// ---- git_credentials ------------------------------------------------

	putGitCredential(cred: GitCredential, secret: GitCredentialSecret): void {
		this.sql.exec(
			`INSERT INTO git_credentials (name, host, doc, secret) VALUES (?, ?, ?, ?)
			 ON CONFLICT (name) DO UPDATE SET host = excluded.host, doc = excluded.doc, secret = excluded.secret`,
			cred.name,
			cred.host,
			marshal(GitCredentialSchema, cred),
			JSON.stringify(secret),
		);
	}

	deleteGitCredential(name: string): boolean {
		return this.sql.exec("DELETE FROM git_credentials WHERE name = ?", name).rowsWritten > 0;
	}

	listGitCredentials(): GitCredential[] {
		return this.sql
			.exec("SELECT doc FROM git_credentials ORDER BY name")
			.toArray()
			.map((r) => unmarshal(GitCredentialSchema, str(r.doc)));
	}

	gitCredentialsByHost(host: string): StoredGitCredential[] {
		return this.sql
			.exec("SELECT doc, secret FROM git_credentials WHERE host = ? ORDER BY name", host)
			.toArray()
			.map((r) => ({
				cred: unmarshal(GitCredentialSchema, str(r.doc)),
				secret: JSON.parse(str(r.secret)) as GitCredentialSecret,
			}));
	}

	// ---- prune ----------------------------------------------------------

	prune(nowMs: number): void {
		const histCutoff = nowMs - KEEP_HISTORY_MS;
		this.sql.exec("DELETE FROM samples WHERE t < ?", nowMs - KEEP_SAMPLES_MS);
		this.sql.exec("DELETE FROM history WHERE ended_at < ?", histCutoff);
		this.sql.exec("DELETE FROM command_profiles WHERE ended_at < ?", histCutoff);
		this.sql.exec("DELETE FROM command_diagnostics WHERE created_at < ?", histCutoff);
		this.sql.exec(
			"DELETE FROM events WHERE id <= (SELECT MAX(e.id) FROM events e) - ?",
			KEEP_EVENTS,
		);
		this.sql.exec(
			`DELETE FROM commands WHERE created_at < ?
			 AND id NOT IN (SELECT id FROM node_state WHERE kind = 'command')
			 AND id NOT IN (SELECT id FROM history)`,
			histCutoff,
		);
		this.sql.exec(
			`DELETE FROM schedule_runs WHERE ended_at IS NOT NULL AND (
				schedule NOT IN (SELECT name FROM schedules) OR
				id NOT IN (SELECT id FROM schedule_runs r WHERE r.schedule = schedule_runs.schedule ORDER BY id DESC LIMIT ?)
			)`,
			KEEP_RUNS,
		);
		this.sql.exec("DELETE FROM placements WHERE at < ?", nowMs - KEEP_PLACEMENTS_MS);
		this.sql.exec("DELETE FROM daemon_logs WHERE at < ?", nowMs - KEEP_DAEMON_LOGS_MS);
		this.samplesGen++;
	}
}

export function sessionEndedError(id: string, reason: string): Error {
	const msg = reason ? `session ${id} ended (${reason})` : `session ${id} ended`;
	return hotbedError(ErrorCode.Conflict, msg).withSuggestion(
		`start a new session (hotbed session start), or resume this one (hotbed session start --resume ${id})`,
	);
}
