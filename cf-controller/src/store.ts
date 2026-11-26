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
	ActorKind,
	BaseFreshenSchema,
	BaseSchema,
	CladefsCellSchema,
	ClusterEventSchema,
	CommandDiagnosticsSchema,
	type CommandSample,
	CommandSampleSchema,
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
	MemoryBriefingSchema,
	MemoryChangeSchema,
	MemoryIndexEntrySchema,
	MemoryMode,
	MemoryOrder,
	MemorySchema,
	MemoryScope,
	MemoryState,
	NodeSchema,
	PlacementDecisionSchema,
	ProjectChangeSchema,
	ProjectSchema,
	QuestionChangeSchema,
	QuestionOrder,
	QuestionProjectSchema,
	QuestionSchema,
	QuestionSessionSchema,
	QuestionState,
	QuestionUpdate_What,
	QuestionUpdateSchema,
	QuickResponse,
	RepoSchema,
	ReviewEventSchema,
	ReviewerSchema,
	ReviewerState,
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
	WorkerConfigSchema,
	WorkspaceSchema,
	WorkspaceState,
	WorkspaceUpdate_What,
	WorkspaceUpdateSchema,
	type Base,
	type BaseFreshen,
	type CladefsCell,
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
	type Memory,
	type MemoryBriefing,
	type MemoryChange,
	type MemoryContent,
	type MemoryIndexEntry,
	type MemoryProvenance,
	type Node,
	type PlacementDecision,
	type Project,
	type ProjectChange,
	type Prompt,
	type Question,
	type QuestionChange,
	type QuestionProject,
	type QuestionSession,
	type Repo,
	type Review,
	type ReviewEvent,
	type ScheduledCommand,
	type ScheduledRun,
	type Session,
	type SessionEvent,
	type Stamp,
	type WorkerConfig,
	type Workspace,
	type Answer,
	type AskedFrom,
	type PushEventsRequest,
	CheckpointSchema,
} from "./gen/hotbed/v1/hotbed_pb.ts";
import { ErrorCode, hotbedError } from "./errors.ts";
import {
	freshenIdForBase,
	freshenStateName,
	mergeBaseIntoFreshen,
	mergeCommandIntoFreshen,
} from "./freshens.ts";
import { firstLine } from "./gitops.ts";
import type { NodeRow } from "./placement.ts";
import { isCommandLive } from "./standby.ts";
import { mergeWorkerConfig, validateWorkerConfig } from "./worker-config.ts";

export const KEEP_SAMPLES_MS = 25 * 3600_000;
export const KEEP_PLACEMENTS_MS = 24 * 3600_000;
export const KEEP_HISTORY_MS = 30 * 24 * 3600_000;
export const KEEP_EVENTS = 5000;
export const KEEP_RUNS = 200;
export const KEEP_DAEMON_LOGS_MS = 14 * 24 * 3600_000;
/** How many of a command's activity samples are kept: its newest. */
export const MAX_COMMAND_SAMPLES = 32;
export const LOST_DETAIL = "lost: its worker no longer has it (the worker's state was reset)";

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
	r2Up: number;
	r2Down: number;
	r2Puts: number;
	r2Gets: number;
	r2Fails: number;
	r2WaitNs: number;
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

export function memoryStateName(s: MemoryState): string {
	switch (s) {
		case MemoryState.PROPOSED:
			return "MEMORY_STATE_PROPOSED";
		case MemoryState.APPROVED:
			return "MEMORY_STATE_APPROVED";
		case MemoryState.REJECTED:
			return "MEMORY_STATE_REJECTED";
		case MemoryState.SUPERSEDED:
			return "MEMORY_STATE_SUPERSEDED";
		case MemoryState.ARCHIVED:
			return "MEMORY_STATE_ARCHIVED";
		case MemoryState.WITHDRAWN:
			return "MEMORY_STATE_WITHDRAWN";
		default:
			return "MEMORY_STATE_UNSPECIFIED";
	}
}

export function memoryModeName(m: MemoryMode): string {
	switch (m) {
		case MemoryMode.ALWAYS:
			return "MEMORY_MODE_ALWAYS";
		case MemoryMode.ON_DEMAND:
			return "MEMORY_MODE_ON_DEMAND";
		default:
			return "MEMORY_MODE_UNSPECIFIED";
	}
}

function memorySearchText(c?: MemoryContent): string {
	if (!c) return "";
	return [c.slug, c.title, c.summary, ...c.tags, c.body].filter(Boolean).join("\n").toLowerCase();
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

function isUserStamp(by?: Stamp): boolean {
	if (!by) return true;
	if (by.actorKind === ActorKind.USER) return true;
	if (by.actorKind === ActorKind.AGENT) return false;
	return !by.session;
}

function reviewResponseOn(about: string, e: ReviewEvent): string {
	const who = isUserStamp(e.by) ? "the user" : e.by?.session || "an agent";
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

	// ---- worker_tokens, join_tokens, api_tokens (Drizzle ORM) -----------

	putWorkerToken(tokenHash: string, nodeId: string, hostname: string, nowMs: number): void {
		this.db
			.insert(dbSchema.workerTokens)
			.values({ tokenHash, nodeId, hostname, createdAt: nowMs, revokedAt: null })
			.onConflictDoUpdate({
				target: dbSchema.workerTokens.tokenHash,
				set: { nodeId, hostname, revokedAt: null },
			})
			.run();
	}

	findWorkerToken(tokenHash: string): { nodeId: string; hostname: string } | null {
		const row = this.db
			.select({
				nodeId: dbSchema.workerTokens.nodeId,
				hostname: dbSchema.workerTokens.hostname,
				revokedAt: dbSchema.workerTokens.revokedAt,
			})
			.from(dbSchema.workerTokens)
			.where(eq(dbSchema.workerTokens.tokenHash, tokenHash))
			.get();
		if (!row || row.revokedAt !== null) return null;
		return { nodeId: row.nodeId, hostname: row.hostname };
	}

	revokeWorkerTokensForNode(nodeId: string, nowMs: number): void {
		this.db
			.update(dbSchema.workerTokens)
			.set({ revokedAt: nowMs })
			.where(eq(dbSchema.workerTokens.nodeId, nodeId))
			.run();
	}

	putJoinToken(row: {
		id: string;
		tokenHash: string;
		label: string;
		createdBy: string;
		createdAt: number;
		expiresAt: number | null;
	}): void {
		this.db
			.insert(dbSchema.joinTokens)
			.values({
				id: row.id,
				tokenHash: row.tokenHash,
				label: row.label,
				createdBy: row.createdBy,
				createdAt: row.createdAt,
				expiresAt: row.expiresAt,
				revokedAt: null,
			})
			.run();
	}

	findJoinToken(tokenHash: string, nowMs: number): { id: string; label: string } | null {
		const row = this.db
			.select({
				id: dbSchema.joinTokens.id,
				label: dbSchema.joinTokens.label,
				expiresAt: dbSchema.joinTokens.expiresAt,
				revokedAt: dbSchema.joinTokens.revokedAt,
			})
			.from(dbSchema.joinTokens)
			.where(eq(dbSchema.joinTokens.tokenHash, tokenHash))
			.get();
		if (!row || row.revokedAt !== null) return null;
		if (row.expiresAt !== null && row.expiresAt <= nowMs) return null;
		return { id: row.id, label: row.label };
	}

	listJoinTokens(): Array<{
		id: string;
		label: string;
		createdBy: string;
		createdAt: number;
		expiresAt: number | null;
		revokedAt: number | null;
	}> {
		return this.db
			.select({
				id: dbSchema.joinTokens.id,
				label: dbSchema.joinTokens.label,
				createdBy: dbSchema.joinTokens.createdBy,
				createdAt: dbSchema.joinTokens.createdAt,
				expiresAt: dbSchema.joinTokens.expiresAt,
				revokedAt: dbSchema.joinTokens.revokedAt,
			})
			.from(dbSchema.joinTokens)
			.all()
			.sort((a, b) => b.createdAt - a.createdAt);
	}

	revokeJoinToken(id: string, nowMs: number): boolean {
		const existing = this.db
			.select({ id: dbSchema.joinTokens.id, revokedAt: dbSchema.joinTokens.revokedAt })
			.from(dbSchema.joinTokens)
			.where(eq(dbSchema.joinTokens.id, id))
			.get();
		if (!existing) return false;
		if (existing.revokedAt === null) {
			this.db
				.update(dbSchema.joinTokens)
				.set({ revokedAt: nowMs })
				.where(eq(dbSchema.joinTokens.id, id))
				.run();
		}
		return true;
	}

	// ---- signup_codes & approved_users (Drizzle ORM) --------------------

	seedInitialSignupCode(rawCode: string, nowMs: number): void {
		const normalized = rawCode.trim().toUpperCase();
		if (!normalized) return;
		if (this.getMeta("signup_codes_seeded") !== null) return;
		this.db
			.insert(dbSchema.signupCodes)
			.values({
				code: normalized,
				label: "initial",
				maxUses: null,
				useCount: 0,
				createdAt: nowMs,
				revokedAt: null,
			})
			.onConflictDoNothing()
			.run();
		this.setMeta("signup_codes_seeded", "1");
	}

	isUserSignupApproved(userId: string): boolean {
		const row = this.db
			.select({ userId: dbSchema.approvedUsers.userId })
			.from(dbSchema.approvedUsers)
			.where(eq(dbSchema.approvedUsers.userId, userId))
			.get();
		return Boolean(row);
	}

	countApprovedUsers(): number {
		return this.db
			.select({ userId: dbSchema.approvedUsers.userId })
			.from(dbSchema.approvedUsers)
			.all().length;
	}

	listSignupCodes(): Array<{
		code: string;
		label: string;
		maxUses: number | null;
		useCount: number;
		createdAt: number;
		revokedAt: number | null;
	}> {
		return this.db
			.select({
				code: dbSchema.signupCodes.code,
				label: dbSchema.signupCodes.label,
				maxUses: dbSchema.signupCodes.maxUses,
				useCount: dbSchema.signupCodes.useCount,
				createdAt: dbSchema.signupCodes.createdAt,
				revokedAt: dbSchema.signupCodes.revokedAt,
			})
			.from(dbSchema.signupCodes)
			.all()
			.sort((a, b) => b.createdAt - a.createdAt);
	}

	listActiveSignupCodes(): Array<{ code: string; maxUses: number | null; useCount: number }> {
		return this.listSignupCodes()
			.filter((c) => c.revokedAt === null && (c.maxUses === null || c.useCount < c.maxUses))
			.map(({ code, maxUses, useCount }) => ({ code, maxUses, useCount }));
	}

	putSignupCode(row: {
		code: string;
		label: string;
		maxUses: number | null;
		createdAt: number;
	}): void {
		const normalized = row.code.trim().toUpperCase();
		this.db
			.insert(dbSchema.signupCodes)
			.values({
				code: normalized,
				label: row.label,
				maxUses: row.maxUses,
				useCount: 0,
				createdAt: row.createdAt,
				revokedAt: null,
			})
			.onConflictDoUpdate({
				target: dbSchema.signupCodes.code,
				set: {
					label: row.label,
					maxUses: row.maxUses,
					revokedAt: null,
				},
			})
			.run();
	}

	revokeSignupCode(rawCode: string, nowMs: number): boolean {
		const normalized = rawCode.trim().toUpperCase();
		const existing = this.db
			.select({ code: dbSchema.signupCodes.code, revokedAt: dbSchema.signupCodes.revokedAt })
			.from(dbSchema.signupCodes)
			.where(eq(dbSchema.signupCodes.code, normalized))
			.get();
		if (!existing) return false;
		if (existing.revokedAt === null) {
			this.db
				.update(dbSchema.signupCodes)
				.set({ revokedAt: nowMs })
				.where(eq(dbSchema.signupCodes.code, normalized))
				.run();
		}
		return true;
	}

	recordSignupRedemption(userId: string, email: string, matchedCode: string, nowMs: number): void {
		this.storage.transactionSync(() => {
			const codeRow = this.db
				.select({ useCount: dbSchema.signupCodes.useCount })
				.from(dbSchema.signupCodes)
				.where(eq(dbSchema.signupCodes.code, matchedCode))
				.get();
			if (codeRow) {
				this.db
					.update(dbSchema.signupCodes)
					.set({ useCount: codeRow.useCount + 1 })
					.where(eq(dbSchema.signupCodes.code, matchedCode))
					.run();
			}
			this.db
				.insert(dbSchema.approvedUsers)
				.values({
					userId,
					email,
					signupCode: matchedCode,
					approvedAt: nowMs,
				})
				.onConflictDoNothing()
				.run();
		});
	}

	putApiToken(row: {
		id: string;
		tokenHash: string;
		name: string;
		userId: string;
		email: string;
		orgRole: string;
		createdAt: number;
		expiresAt: number | null;
	}): void {
		this.db
			.insert(dbSchema.apiTokens)
			.values({
				id: row.id,
				tokenHash: row.tokenHash,
				name: row.name,
				userId: row.userId,
				email: row.email,
				orgRole: row.orgRole,
				createdAt: row.createdAt,
				lastUsedAt: null,
				expiresAt: row.expiresAt,
				revokedAt: null,
			})
			.run();
	}

	findApiToken(
		tokenHash: string,
		nowMs: number,
	): { id: string; name: string; userId: string; email: string; orgRole: string } | null {
		const row = this.db
			.select({
				id: dbSchema.apiTokens.id,
				name: dbSchema.apiTokens.name,
				userId: dbSchema.apiTokens.userId,
				email: dbSchema.apiTokens.email,
				orgRole: dbSchema.apiTokens.orgRole,
				expiresAt: dbSchema.apiTokens.expiresAt,
				revokedAt: dbSchema.apiTokens.revokedAt,
				lastUsedAt: dbSchema.apiTokens.lastUsedAt,
			})
			.from(dbSchema.apiTokens)
			.where(eq(dbSchema.apiTokens.tokenHash, tokenHash))
			.get();
		if (!row || row.revokedAt !== null) return null;
		if (row.expiresAt !== null && row.expiresAt <= nowMs) return null;
		// Throttle last_used_at updates to once per minute to avoid write amplification.
		if (row.lastUsedAt === null || nowMs - row.lastUsedAt > 60_000) {
			this.db
				.update(dbSchema.apiTokens)
				.set({ lastUsedAt: nowMs })
				.where(eq(dbSchema.apiTokens.id, row.id))
				.run();
		}
		return {
			id: row.id,
			name: row.name,
			userId: row.userId,
			email: row.email,
			orgRole: row.orgRole,
		};
	}

	listApiTokens(filterUserId?: string): Array<{
		id: string;
		name: string;
		userId: string;
		email: string;
		orgRole: string;
		createdAt: number;
		lastUsedAt: number | null;
		expiresAt: number | null;
		revokedAt: number | null;
	}> {
		const q = this.db
			.select({
				id: dbSchema.apiTokens.id,
				name: dbSchema.apiTokens.name,
				userId: dbSchema.apiTokens.userId,
				email: dbSchema.apiTokens.email,
				orgRole: dbSchema.apiTokens.orgRole,
				createdAt: dbSchema.apiTokens.createdAt,
				lastUsedAt: dbSchema.apiTokens.lastUsedAt,
				expiresAt: dbSchema.apiTokens.expiresAt,
				revokedAt: dbSchema.apiTokens.revokedAt,
			})
			.from(dbSchema.apiTokens);
		const rows = filterUserId
			? q.where(eq(dbSchema.apiTokens.userId, filterUserId)).all()
			: q.all();
		return rows.sort((a, b) => b.createdAt - a.createdAt);
	}

	getApiToken(
		id: string,
	): { id: string; userId: string; email: string; revokedAt: number | null } | null {
		const row = this.db
			.select({
				id: dbSchema.apiTokens.id,
				userId: dbSchema.apiTokens.userId,
				email: dbSchema.apiTokens.email,
				revokedAt: dbSchema.apiTokens.revokedAt,
			})
			.from(dbSchema.apiTokens)
			.where(eq(dbSchema.apiTokens.id, id))
			.get();
		return row ?? null;
	}

	revokeApiToken(id: string, nowMs: number): boolean {
		const existing = this.getApiToken(id);
		if (!existing) return false;
		if (existing.revokedAt === null) {
			this.db
				.update(dbSchema.apiTokens)
				.set({ revokedAt: nowMs })
				.where(eq(dbSchema.apiTokens.id, id))
				.run();
		}
		return true;
	}

	revokeApiTokensForUser(userId: string, nowMs: number): number {
		if (!userId) return 0;
		const res = this.sql.exec(
			"UPDATE api_tokens SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL",
			nowMs,
			userId,
		);
		return res.rowsWritten;
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
			node.objectStore = hb.objectStore;
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
		model = "",
	): Session {
		const pid = this.projectIdByName(project);
		try {
			this.sql.exec(
				"INSERT INTO sessions (id, agent, project_id, t3code_thread_id, model, started_at, last_active_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
				id,
				agent,
				pid,
				t3codeThreadId,
				model,
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
				`SELECT s.id, s.agent, p.name AS project, s.t3code_thread_id, s.model, s.started_at, s.last_active_at, s.ended_at, s.end_reason
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

	setSessionModelIfEmpty(id: string, model: string): void {
		this.sql.exec("UPDATE sessions SET model = ? WHERE id = ? AND model = ''", model, id);
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
				`SELECT s.id, s.agent, p.name AS project, s.t3code_thread_id, s.model, s.started_at, s.last_active_at, s.ended_at, s.end_reason
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
				`SELECT s.id, s.agent, p.name AS project, s.t3code_thread_id, s.model, s.started_at, s.last_active_at, s.ended_at, s.end_reason
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

	dropWorkspace(id: string): void {
		this.sql.exec("DELETE FROM workspaces WHERE id = ?", id);
	}

	getWorkspace(id: string): WorkspaceRow {
		const rows = this.sql
			.exec(
				"SELECT id, session_id, node_id, agent, released_at, throwaway FROM workspaces WHERE id = ?",
				id,
			)
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
			throwaway: Boolean(r.throwaway),
		};
	}

	sessionWorkspaces(sessionId: string): WorkspaceRow[] {
		return this.sql
			.exec(
				"SELECT id, session_id, node_id, agent, throwaway FROM workspaces WHERE session_id = ? AND released_at IS NULL ORDER BY created_at, id",
				sessionId,
			)
			.toArray()
			.map((r) => ({
				id: str(r.id),
				session: str(r.session_id),
				node: str(r.node_id),
				agent: str(r.agent),
				released: false,
				throwaway: Boolean(r.throwaway),
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
		const bf = this.getBaseFreshen(id);
		if (bf) {
			return {
				session: "",
				node: bf.nodeId,
				histChecked: true,
				baseChecked: true,
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
							this.upsertBaseFreshenFromBase(nodeId, bc.base, bc.what, bc.detail, Number(ev.at));
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
							if (cmd.kind === "freshen") {
								this.upsertBaseFreshenFromCommand(nodeId, cmd, cc.what, cc.detail, Number(ev.at));
							}
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
		for (const b of cp.bases) {
			this.putState(nodeId, "base", b.id, marshal(BaseSchema, b));
			this.upsertBaseFreshenFromBase(nodeId, b, "", "", nowMs);
		}
		for (const w of cp.workspaces) {
			this.putState(nodeId, "workspace", w.id, marshal(WorkspaceSchema, w));
			this.noteWorkspace(nodeId, w);
		}
		for (const c of cp.commands) {
			this.putState(nodeId, "command", c.id, marshal(CommandSchema, c));
			if (c.kind === "freshen") {
				this.upsertBaseFreshenFromCommand(nodeId, c, "", "", nowMs);
			}
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
			"INSERT OR IGNORE INTO workspaces (id, session_id, node_id, agent, created_at, throwaway) VALUES (?, ?, ?, ?, ?, ?)",
			ws.id,
			ws.sessionId,
			nodeId,
			ws.agent,
			Number(ws.createdAt),
			ws.throwaway ? 1 : 0,
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
			const os = h.objectStore;
			this.sql.exec(
				"INSERT OR REPLACE INTO samples (node_id, t, cpu, memory, psi, cpu_psi, holding, running, queued, parked, oom, r2_up, r2_down, r2_puts, r2_gets, r2_fails, r2_wait_ns) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
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

	/**
	 * Keeps sample, one of a command's kept activity samples, with only its
	 * newest MAX_COMMAND_SAMPLES kept per command.
	 */
	saveCommandSample(nodeId: string, sample: CommandSample, nowMs = Date.now()): void {
		const meta = create(CommandSampleSchema, { ...sample, sample: undefined });
		this.sql.exec(
			"INSERT INTO command_samples (command_id, seq, node_id, created_at, meta, doc) VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (command_id, seq) DO UPDATE SET node_id = excluded.node_id, created_at = excluded.created_at, meta = excluded.meta, doc = excluded.doc",
			sample.commandId,
			sample.seq,
			nodeId,
			Number(sample.by?.at || BigInt(nowMs)),
			marshal(CommandSampleSchema, meta),
			toBinary(CommandSampleSchema, sample),
		);
		this.sql.exec(
			"DELETE FROM command_samples WHERE command_id = ? AND seq <= ?",
			sample.commandId,
			sample.seq - MAX_COMMAND_SAMPLES,
		);
	}

	/** A command's kept samples, oldest first, each without its sample. */
	listCommandSamples(commandId: string): CommandSample[] {
		return this.sql
			.exec("SELECT meta FROM command_samples WHERE command_id = ? ORDER BY seq", commandId)
			.toArray()
			.map((r) => unmarshal(CommandSampleSchema, str(r.meta)));
	}

	getCommandSample(commandId: string, seq: number): CommandSample | null {
		const rows = this.sql
			.exec("SELECT doc FROM command_samples WHERE command_id = ? AND seq = ?", commandId, seq)
			.toArray();
		if (rows.length === 0) return null;
		const raw = rows[0].doc;
		if (!(raw instanceof ArrayBuffer))
			throw new Error(`command sample ${commandId} #${seq} is not binary`);
		return fromBinary(CommandSampleSchema, new Uint8Array(raw));
	}

	samplesSince(sinceMs: number): SampleRow[] {
		return this.sql
			.exec(
				"SELECT node_id, t, cpu, memory, psi, cpu_psi, holding, running, queued, parked, oom, r2_up, r2_down, r2_puts, r2_gets, r2_fails, r2_wait_ns FROM samples WHERE t >= ? ORDER BY t",
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
			}));
	}

	recordClusterEvent(
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
		t3codeThreadId = "",
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
					t3codeThreadId,
				});
				this.sql.exec(
					"INSERT INTO questions (project_id, repo, state, rank, asked_by, t3code_thread_id, asked_at, updated_at, doc) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '')",
					pid,
					repo,
					questionStateName(item.state),
					Number(rank),
					by.session,
					t3codeThreadId,
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
				`SELECT q.id, q.rank, p.name AS project, q.doc,
				        q.t3code_thread_id AS q_t3code_thread_id,
				        COALESCE(s.t3code_thread_id, '') AS s_t3code_thread_id
				 FROM questions q
				 JOIN projects p ON p.id = q.project_id
				 LEFT JOIN sessions s ON s.id = q.asked_by
				 WHERE q.id = ?`,
				id,
			)
			.toArray();
		if (rows.length === 0) throw hotbedError(ErrorCode.NotFound, `no question ${id}`);
		const q = unmarshal(QuestionSchema, str(rows[0].doc));
		q.id = num(rows[0].id);
		q.rank = big(rows[0].rank);
		q.project = str(rows[0].project);
		q.t3codeThreadId =
			str(rows[0].q_t3code_thread_id) || str(rows[0].s_t3code_thread_id) || q.t3codeThreadId;
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
		t3codeThreadId: string;
		limit: number;
	}): { questions: Question[]; total: number; sessions: QuestionSession[] } {
		if (filter.project) {
			this.getProject(filter.project);
		}
		const states =
			filter.states.length > 0
				? filter.states.map(questionStateName)
				: [
						questionStateName(QuestionState.OPEN),
						questionStateName(QuestionState.ANSWERED),
						questionStateName(QuestionState.DISMISSED),
						questionStateName(QuestionState.HANDLED),
					];
		const ph = states.map(() => "?").join(",");
		const total = num(
			this.sql
				.exec(
					`SELECT COUNT(*) AS total
					 FROM questions q
					 JOIN projects p ON p.id = q.project_id
					 LEFT JOIN sessions s ON s.id = q.asked_by
					 WHERE q.state IN (${ph})
					   AND (? = '' OR p.name = ?)
					   AND (? = '' OR q.asked_by = ?)
					   AND (? = '' OR COALESCE(NULLIF(q.t3code_thread_id, ''), COALESCE(s.t3code_thread_id, '')) = ?)`,
					...states,
					filter.project,
					filter.project,
					filter.askedBy,
					filter.askedBy,
					filter.t3codeThreadId,
					filter.t3codeThreadId,
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
		const sessionsById = new Map<string, QuestionSession>();
		const sessions: QuestionSession[] = [];
		const questions = this.sql
			.exec(
				`SELECT q.id, q.rank, p.name AS project, q.doc,
				        q.t3code_thread_id AS q_t3code_thread_id,
				        COALESCE(s.t3code_thread_id, '') AS s_t3code_thread_id,
				        (s.id IS NOT NULL AND s.ended_at IS NULL) AS s_live
				 FROM questions q
				 JOIN projects p ON p.id = q.project_id
				 LEFT JOIN sessions s ON s.id = q.asked_by
				 WHERE q.state IN (${ph})
				   AND (? = '' OR p.name = ?)
				   AND (? = '' OR q.asked_by = ?)
				   AND (? = '' OR COALESCE(NULLIF(q.t3code_thread_id, ''), COALESCE(s.t3code_thread_id, '')) = ?)
				 ORDER BY ${orderBy} LIMIT ?`,
				...states,
				filter.project,
				filter.project,
				filter.askedBy,
				filter.askedBy,
				filter.t3codeThreadId,
				filter.t3codeThreadId,
				lim,
			)
			.toArray()
			.map((r) => {
				const q = unmarshal(QuestionSchema, str(r.doc));
				q.id = num(r.id);
				q.rank = big(r.rank);
				q.project = str(r.project);
				q.t3codeThreadId =
					str(r.q_t3code_thread_id) || str(r.s_t3code_thread_id) || q.t3codeThreadId;
				const sid = q.asked?.session ?? "";
				if (sid) {
					let entry = sessionsById.get(sid);
					if (!entry) {
						entry = create(QuestionSessionSchema, {
							sessionId: sid,
							project: q.project,
							t3codeThreadId: q.t3codeThreadId,
							live: num(r.s_live) !== 0,
							questionIds: [],
						});
						sessionsById.set(sid, entry);
						sessions.push(entry);
					} else if (!entry.t3codeThreadId && q.t3codeThreadId) {
						entry.t3codeThreadId = q.t3codeThreadId;
					}
					entry.questionIds.push(q.id);
					if (q.state === QuestionState.OPEN) entry.open += 1;
					else if (q.state === QuestionState.ANSWERED) entry.answered += 1;
					else if (q.state === QuestionState.DISMISSED) entry.dismissed += 1;
				}
				return q;
			});
		return { questions, total, sessions };
	}

	listT3OutboxQuestions(): Question[] {
		return this.sql
			.exec(
				`SELECT q.id, q.rank, p.name AS project, q.doc,
				        q.t3code_thread_id AS q_t3code_thread_id,
				        COALESCE(s.t3code_thread_id, '') AS s_t3code_thread_id
				 FROM questions q
				 JOIN projects p ON p.id = q.project_id
				 LEFT JOIN sessions s ON s.id = q.asked_by
				 WHERE q.state IN (?, ?)
				   AND COALESCE(NULLIF(q.t3code_thread_id, ''), COALESCE(s.t3code_thread_id, '')) != ''
				 ORDER BY q.updated_at ASC, q.id ASC`,
				questionStateName(QuestionState.ANSWERED),
				questionStateName(QuestionState.DISMISSED),
			)
			.toArray()
			.map((r) => {
				const q = unmarshal(QuestionSchema, str(r.doc));
				q.id = num(r.id);
				q.rank = big(r.rank);
				q.project = str(r.project);
				q.t3codeThreadId =
					str(r.q_t3code_thread_id) || str(r.s_t3code_thread_id) || q.t3codeThreadId;
				return q;
			});
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

	private normalizeLegacyReview(r: Review): void {
		if (r.reviewers.length === 0 && r.started) {
			const state =
				r.state === ReviewState.DONE
					? ReviewerState.DONE
					: r.state === ReviewState.FAILED
						? ReviewerState.FAILED
						: ReviewerState.IN_PROGRESS;
			r.reviewers.push(
				create(ReviewerSchema, {
					id: 1,
					model: r.model,
					session: r.started.session,
					t3codeThreadId: r.started.t3codeThreadId,
					state,
					started: r.started,
					ended: r.ended,
					summary: r.summary,
					report: r.report,
				}),
			);
		}
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
		this.normalizeLegacyReview(r);
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
			this.normalizeLegacyReview(r);
			r.report = "";
			for (const rv of r.reviewers) rv.report = "";
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

	changeReview(
		id: number,
		what: string,
		by: Stamp,
		edit: (r: Review) => {
			reviewerId?: number;
			stamp?: Stamp;
			notify?: { what: ReviewUpdate_What; detail: string } | null;
		} | void,
	): Review {
		return this.transactionSync(() => {
			const r = this.getReview(id, false);
			const res = edit(r) ?? {};
			const stamp = res.stamp ?? by;
			const reviewerId = res.reviewerId ?? 0;
			r.updatedAt = stamp.at;
			this.saveReview(r, what, stamp, reviewerId);
			if (res.notify !== undefined) {
				if (res.notify) {
					this.tellSession(
						r.requested?.session ?? "",
						stamp.at,
						create(SessionEventSchema, {
							detail: res.notify.detail,
							event: {
								case: "review",
								value: create(ReviewUpdateSchema, { what: res.notify.what, reviewId: r.id }),
							},
						}),
					);
				}
			} else if (what === "finished" || what === "failed") {
				const word = what === "finished" ? "is done" : "failed";
				let line = `review ${r.id} (${r.title}) ${word}`;
				if (r.summary) line += `: ${firstLine(r.summary)}`;
				const w = what === "finished" ? ReviewUpdate_What.FINISHED : ReviewUpdate_What.FAILED;
				this.tellSession(
					r.requested?.session ?? "",
					stamp.at,
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
		check: (r: Review) => { reviewerId: number; stamp: Stamp },
		findings: Finding[],
		report: string,
	): Finding[] {
		return this.transactionSync(() => {
			const r = this.getReview(reviewId, false);
			const { reviewerId, stamp } = check(r);
			if (report) {
				r.report = report;
				const rv = r.reviewers.find((x) => x.id === reviewerId);
				if (rv) rv.report = report;
				r.updatedAt = stamp.at;
				this.saveReview(r, "reported", stamp, reviewerId);
			}
			if (findings.length === 0) {
				this.insertReviewEvent(
					create(ReviewEventSchema, { reviewId, what: "found none", by: stamp, reviewerId }),
					"",
				);
			}
			for (const f of findings) {
				this.sql.exec("INSERT INTO review_findings (review_id, doc) VALUES (?, '')", reviewId);
				f.id = num(this.sql.exec("SELECT last_insert_rowid() AS id").one().id);
				f.reviewId = reviewId;
				f.reviewerId = reviewerId;
				f.found = stamp;
				f.decision = FindingDecision.OPEN;
				this.saveFinding(f, "found", stamp, reviewerId);
			}
			this.sql.exec(
				"UPDATE reviews SET updated_at = MAX(updated_at, ?) WHERE id = ?",
				Number(stamp.at),
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
		edit: (r: Review, f: Finding) => Stamp | void,
	): Finding[] {
		return this.transactionSync(() => {
			const r = this.getReview(reviewId, false);
			const out: Finding[] = [];
			let lastAt = by.at;
			for (const fid of findingIds) {
				const f = this.getFinding(reviewId, fid);
				const stamp = edit(r, f) ?? by;
				lastAt = stamp.at;
				this.saveFinding(f, what, stamp, 0);
				out.push(f);
			}
			this.sql.exec(
				"UPDATE reviews SET updated_at = MAX(updated_at, ?) WHERE id = ?",
				Number(lastAt),
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
				"SELECT seq, finding_id, what, at, session, host, quick, text, by_doc, reviewer_id FROM review_events WHERE review_id = ? AND seq > ? ORDER BY seq",
				reviewId,
				Number(afterSeq),
			)
			.toArray()
			.map((row) => {
				const byDoc = str(row.by_doc);
				let by: Stamp;
				if (byDoc) {
					by = unmarshal(StampSchema, byDoc);
				} else {
					const session = str(row.session);
					by = create(StampSchema, {
						at: big(row.at),
						session,
						host: str(row.host),
						actorKind: session ? ActorKind.AGENT : ActorKind.USER,
					});
				}
				return create(ReviewEventSchema, {
					seq: big(row.seq),
					reviewId,
					findingId: num(row.finding_id),
					what: str(row.what),
					by,
					quick: quickResponseFromName(str(row.quick)),
					text: str(row.text),
					reviewerId: num(row.reviewer_id),
				});
			});
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
		const byReviewer = new Map<number, number>();
		for (const fr of fRows) {
			const f = unmarshal(FindingSchema, str(fr.doc));
			f.id = num(fr.id);
			f.reviewId = r.id;
			if (f.severity === 1) important++;
			else if (f.severity === 2) preExisting++;
			else if (f.severity === 3) nits++;
			if (f.decision === FindingDecision.OPEN || f.decision === FindingDecision.UNSPECIFIED) open++;
			const rvid = f.reviewerId || (r.reviewers.length === 1 ? r.reviewers[0].id : 0);
			if (rvid > 0) {
				byReviewer.set(rvid, (byReviewer.get(rvid) ?? 0) + 1);
			}
			findings.push(f);
		}
		for (const rv of r.reviewers) {
			rv.findingsCount = byReviewer.get(rv.id) ?? 0;
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
						f.quickBy = ev.by;
					}
				}
			}
			r.findings = findings;
			r.events = events;
		}
	}

	private saveReview(r: Review, what: string, by: Stamp, reviewerId = 0): void {
		const stored = clone(ReviewSchema, r);
		stored.project = "";
		stored.findings = [];
		stored.events = [];
		stored.important = 0;
		stored.preExisting = 0;
		stored.nits = 0;
		stored.open = 0;
		stored.responses = 0;
		for (const rv of stored.reviewers) rv.findingsCount = 0;
		const doc = marshal(ReviewSchema, stored);
		this.sql.exec(
			"UPDATE reviews SET state = ?, updated_at = ?, doc = ? WHERE id = ?",
			reviewStateName(r.state),
			Number(r.updatedAt),
			doc,
			r.id,
		);
		this.insertReviewEvent(
			create(ReviewEventSchema, { reviewId: r.id, what, by, text: by.reason, reviewerId }),
			doc,
		);
	}

	private saveFinding(f: Finding, what: string, by: Stamp, reviewerId = 0): void {
		const stored = clone(FindingSchema, f);
		stored.responses = 0;
		stored.quickBy = undefined;
		const doc = marshal(FindingSchema, stored);
		this.sql.exec("UPDATE review_findings SET doc = ? WHERE id = ?", doc, f.id);
		this.insertReviewEvent(
			create(ReviewEventSchema, {
				reviewId: f.reviewId,
				findingId: f.id,
				what,
				by,
				text: by.reason,
				reviewerId,
			}),
			doc,
		);
	}

	private insertReviewEvent(e: ReviewEvent, doc: string): bigint {
		const byDoc = e.by ? marshal(StampSchema, e.by) : "";
		this.sql.exec(
			"INSERT INTO review_events (review_id, finding_id, what, at, session, host, quick, text, doc, by_doc, reviewer_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			e.reviewId,
			e.findingId,
			e.what,
			Number(e.by?.at ?? 0n),
			e.by?.session ?? "",
			e.by?.host ?? "",
			quickResponseName(e.quick),
			e.text,
			doc,
			byDoc,
			e.reviewerId,
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

	// ---- cladefs coordination -------------------------------------------

	nextFencingToken(): bigint {
		let next = 1n;
		this.transactionSync(() => {
			const cur = this.getMeta("cladefs_fencing_token");
			if (cur) {
				next = BigInt(cur) + 1n;
			}
			this.setMeta("cladefs_fencing_token", next.toString());
		});
		return next;
	}

	private inferSharedBaseRuntime(explicit: string, baseId: string, nodeRuntimes: string[]): string {
		if (explicit === "vm" || explicit === "cladefs") return explicit;
		if (baseId.includes("-vm-")) return "vm";
		if (baseId.includes("-fuse-")) return "cladefs";
		const valid = nodeRuntimes.filter((r) => r === "vm" || r === "cladefs");
		if (valid.length === 1) return valid[0];
		return "";
	}

	upsertSharedBases(
		nodeId: string,
		arch: string,
		cells: CladefsCell[],
		nowMs: number,
		allowedTenant?: bigint | null,
		nodeRuntimes: string[] = [],
	): void {
		if (cells.length === 0) return;
		this.transactionSync(() => {
			for (const c of cells) {
				if (!c.project || c.cell === 0n || c.version === 0n) continue;
				if (allowedTenant !== undefined && allowedTenant !== null && c.tenant !== allowedTenant) {
					continue;
				}
				const cellArch = c.arch || arch;
				if (!cellArch) continue;
				const cellRuntime = this.inferSharedBaseRuntime(c.runtime, c.baseId ?? "", nodeRuntimes);
				this.sql.exec(
					`INSERT INTO shared_bases (project, arch, recipe, base_id, runtime, tenant, cell, version, node_id, updated_at)
					 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
					 ON CONFLICT (project, arch, recipe) DO UPDATE SET
						base_id = excluded.base_id,
						runtime = CASE WHEN excluded.runtime != '' THEN excluded.runtime ELSE shared_bases.runtime END,
						tenant = excluded.tenant,
						cell = excluded.cell,
						version = excluded.version,
						node_id = excluded.node_id,
						updated_at = excluded.updated_at`,
					c.project,
					cellArch,
					c.recipe ?? "",
					c.baseId ?? "",
					cellRuntime,
					c.tenant.toString(),
					c.cell.toString(),
					c.version.toString(),
					nodeId,
					nowMs,
				);
			}
		});
	}

	latestSharedBase(
		project: string,
		arch: string,
		allowedTenant?: bigint | null,
		runtime?: string,
	): CladefsCell | undefined {
		if (!project || !arch) return undefined;
		const conds = ["project = ?", "arch = ?"];
		const params: SqlStorageValue[] = [project, arch];
		if (allowedTenant !== undefined && allowedTenant !== null) {
			conds.push("tenant = ?");
			params.push(allowedTenant.toString());
		}
		if (runtime === "vm") {
			conds.push("(runtime = 'vm' OR (runtime = '' AND base_id LIKE '%-vm-%'))");
		} else if (runtime === "cladefs") {
			conds.push("(runtime = 'cladefs' OR (runtime = '' AND base_id LIKE '%-fuse-%'))");
		}
		const rows = this.sql
			.exec(
				`SELECT project, arch, recipe, base_id, runtime, tenant, cell, version
				 FROM shared_bases
				 WHERE ${conds.join(" AND ")}
				 ORDER BY updated_at DESC LIMIT 1`,
				...params,
			)
			.toArray();
		if (rows.length === 0) return undefined;
		const r = rows[0];
		return create(CladefsCellSchema, {
			project: str(r.project),
			arch: str(r.arch),
			recipe: str(r.recipe),
			baseId: str(r.base_id),
			runtime: str(r.runtime),
			tenant: BigInt(str(r.tenant)),
			cell: BigInt(str(r.cell)),
			version: BigInt(str(r.version)),
		});
	}

	hasSharedBase(
		project: string,
		arch: string,
		allowedTenant?: bigint | null,
		runtime?: string,
	): boolean {
		if (!project || !arch) return false;
		const conds = ["project = ?", "arch = ?"];
		const params: SqlStorageValue[] = [project, arch];
		if (allowedTenant !== undefined && allowedTenant !== null) {
			conds.push("tenant = ?");
			params.push(allowedTenant.toString());
		}
		if (runtime === "vm") {
			conds.push("(runtime = 'vm' OR (runtime = '' AND base_id LIKE '%-vm-%'))");
		} else if (runtime === "cladefs") {
			conds.push("(runtime = 'cladefs' OR (runtime = '' AND base_id LIKE '%-fuse-%'))");
		}
		return (
			this.sql
				.exec(`SELECT 1 FROM shared_bases WHERE ${conds.join(" AND ")} LIMIT 1`, ...params)
				.toArray().length > 0
		);
	}

	hasLegacyTenantZeroData(): boolean {
		return (
			this.sql.exec("SELECT 1 FROM shared_bases WHERE tenant = '0' LIMIT 1").toArray().length > 0
		);
	}

	recordRetiredIncarnation(
		incarnation: bigint,
		nodeId: string,
		hostname: string,
		nowMs: number,
	): void {
		if (incarnation <= 0n) return;
		this.sql.exec(
			`INSERT INTO cladefs_retired_incarnations (incarnation, node_id, hostname, retired_at)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT (incarnation) DO UPDATE SET retired_at = excluded.retired_at`,
			incarnation.toString(),
			nodeId,
			hostname,
			nowMs,
		);
	}

	listPendingDecommissions(sinceMs: number): bigint[] {
		return this.sql
			.exec(
				`SELECT incarnation FROM cladefs_retired_incarnations
				 WHERE retired_at >= ? ORDER BY retired_at ASC`,
				sinceMs,
			)
			.toArray()
			.map((r) => BigInt(str(r.incarnation)));
	}

	// ---- memories -------------------------------------------------------

	static readonly BRIEFING_ALWAYS_MAX_COUNT = 20;
	static readonly BRIEFING_ALWAYS_MAX_BYTES = 4096;
	static readonly BRIEFING_ON_DEMAND_MAX_COUNT = 30;
	static readonly BRIEFING_ON_DEMAND_MAX_BYTES = 4096;

	proposeMemory(opts: {
		scope: MemoryScope;
		project: string;
		repo: string;
		content: MemoryContent;
		supersedesIds: number[];
		provenance?: MemoryProvenance;
		by: Stamp;
	}): Memory {
		return this.transactionSync(() => {
			const pid = opts.scope === MemoryScope.PROJECT ? this.projectIdByName(opts.project) : null;
			const scopeKey = opts.scope === MemoryScope.PROJECT ? opts.project : "";
			const top = num(
				this.sql
					.exec(
						"SELECT COALESCE(MIN(rank), 0) AS m FROM memories WHERE state = ?",
						memoryStateName(MemoryState.APPROVED),
					)
					.one().m,
			);
			const item = create(MemorySchema, {
				scope: opts.scope,
				project: scopeKey,
				repo: opts.repo,
				content: opts.content,
				state: MemoryState.APPROVED,
				rank: BigInt(top - 1),
				supersedesIds: opts.supersedesIds,
				provenance: opts.provenance,
				proposed: opts.by,
				decided: opts.by,
				updatedAt: opts.by.at,
			});

			// Validate supersedes targets exist and are APPROVED.
			const supersededTargets: Memory[] = [];
			for (const tid of opts.supersedesIds) {
				const target = this.getMemory(tid);
				if (target.state !== MemoryState.APPROVED) {
					throw hotbedError(
						ErrorCode.Conflict,
						`cannot supersede memory #${tid}: it is not approved`,
					).withSuggestion(`hotbed memory show ${tid}`);
				}
				supersededTargets.push(target);
			}

			// Guard against duplicate active slugs unless that memory is being superseded.
			const existingSlug = this.findApprovedByScopeSlug(scopeKey, opts.content.slug);
			if (existingSlug && !opts.supersedesIds.includes(existingSlug.id)) {
				const where = scopeKey ? `project ${scopeKey}` : "global";
				throw hotbedError(
					ErrorCode.Conflict,
					`an approved memory #${existingSlug.id} already uses slug "${opts.content.slug}" in ${where}`,
				).withSuggestion(
					`pass --supersede ${existingSlug.id} to update it, or pick a different --slug`,
				);
			}

			// Insert initially in PROPOSED state so we obtain item.id before freeing
			// superseded targets' slugs in the unique approved-slug index.
			this.sql.exec(
				`INSERT INTO memories (project_id, scope_key, repo, slug, mode, state, rank, proposed_by, proposed_at, updated_at, search_text, doc)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '')`,
				pid,
				scopeKey,
				opts.repo,
				opts.content.slug,
				memoryModeName(opts.content.mode),
				memoryStateName(MemoryState.PROPOSED),
				Number(item.rank),
				opts.by.session,
				Number(opts.by.at),
				Number(opts.by.at),
				memorySearchText(opts.content),
			);
			item.id = num(this.sql.exec("SELECT last_insert_rowid() AS id").one().id);

			if (supersededTargets.length > 0) {
				let minRank = supersededTargets[0].rank;
				for (const t of supersededTargets) {
					if (t.rank < minRank) minRank = t.rank;
					t.state = MemoryState.SUPERSEDED;
					t.supersededBy = item.id;
					t.updatedAt = opts.by.at;
					const tpid = t.scope === MemoryScope.PROJECT ? this.projectIdByName(t.project) : null;
					this.saveMemoryRow(t, tpid, "superseded", opts.by);
				}
				item.rank = minRank;
			}
			this.saveMemoryRow(item, pid, "created", opts.by);
			return item;
		});
	}

	/**
	 * Auto-approves any legacy MEMORY_STATE_PROPOSED rows left from before
	 * memories became automatically approved on creation. Idempotent and fast
	 * when no proposed rows exist.
	 */
	autoApproveLegacyProposedMemories(): number {
		const rows = this.sql
			.exec(
				`SELECT m.id, m.rank, COALESCE(p.name, '') AS project, m.doc
				 FROM memories m LEFT JOIN projects p ON p.id = m.project_id
				 WHERE m.state = ?
				 ORDER BY m.id ASC`,
				memoryStateName(MemoryState.PROPOSED),
			)
			.toArray();
		if (rows.length === 0) return 0;

		return this.transactionSync(() => {
			let approved = 0;
			for (const r of rows) {
				const item = this.decodeMemoryRow(r);
				const scopeKey = item.scope === MemoryScope.PROJECT ? item.project : "";
				const pid = item.scope === MemoryScope.PROJECT ? this.projectIdByName(item.project) : null;
				const by =
					item.proposed ??
					create(StampSchema, {
						at: item.updatedAt || BigInt(Date.now()),
					});

				// Supersede any targets that are still active.
				let minRank: bigint | null = null;
				for (const tid of item.supersedesIds) {
					if (tid === item.id) continue;
					let target: Memory | null = null;
					try {
						target = this.getMemory(tid);
					} catch {
						continue;
					}
					if (target.state !== MemoryState.APPROVED) continue;
					if (minRank === null || target.rank < minRank) minRank = target.rank;
					target.state = MemoryState.SUPERSEDED;
					target.supersededBy = item.id;
					target.updatedAt = by.at;
					const tpid =
						target.scope === MemoryScope.PROJECT ? this.projectIdByName(target.project) : null;
					this.saveMemoryRow(target, tpid, "superseded", by);
				}
				if (minRank !== null) {
					item.rank = minRank;
				}

				// Disambiguate slug if another active memory already holds it.
				if (item.content) {
					const conflict = this.findApprovedByScopeSlug(scopeKey, item.content.slug);
					if (conflict && conflict.id !== item.id) {
						const suffix = `-${item.id}`;
						const base = item.content.slug
							.slice(0, Math.max(1, 64 - suffix.length))
							.replace(/-+$/, "");
						item.content.slug = `${base}${suffix}`;
					}
				}

				item.state = MemoryState.APPROVED;
				item.decided = by;
				item.updatedAt = by.at;
				this.saveMemoryRow(item, pid, "approved", by);
				approved++;
			}
			return approved;
		});
	}

	getMemory(id: number): Memory {
		const rows = this.sql
			.exec(
				`SELECT m.id, m.rank, COALESCE(p.name, '') AS project, m.doc
				 FROM memories m LEFT JOIN projects p ON p.id = m.project_id
				 WHERE m.id = ?`,
				id,
			)
			.toArray();
		if (rows.length === 0) {
			throw hotbedError(ErrorCode.NotFound, `no memory #${id}`).withSuggestion("hotbed memory ls");
		}
		return this.decodeMemoryRow(rows[0]);
	}

	findApprovedByScopeSlug(scopeKey: string, slug: string): Memory | null {
		const rows = this.sql
			.exec(
				`SELECT m.id, m.rank, COALESCE(p.name, '') AS project, m.doc
				 FROM memories m LEFT JOIN projects p ON p.id = m.project_id
				 WHERE m.scope_key = ? AND m.slug = ? AND m.state = ?
				 LIMIT 1`,
				scopeKey,
				slug,
				memoryStateName(MemoryState.APPROVED),
			)
			.toArray();
		if (rows.length === 0) return null;
		return this.decodeMemoryRow(rows[0]);
	}

	findApprovedBySlugAnyScope(slug: string): Memory[] {
		return this.sql
			.exec(
				`SELECT m.id, m.rank, COALESCE(p.name, '') AS project, m.doc
				 FROM memories m LEFT JOIN projects p ON p.id = m.project_id
				 WHERE m.slug = ? AND m.state = ?
				 ORDER BY m.scope_key, m.id`,
				slug,
				memoryStateName(MemoryState.APPROVED),
			)
			.toArray()
			.map((r) => this.decodeMemoryRow(r));
	}

	memoryHistory(id: number): MemoryChange[] {
		return this.sql
			.exec(
				"SELECT seq, what, at, session, host, reason, doc FROM memory_changes WHERE memory_id = ? ORDER BY seq",
				id,
			)
			.toArray()
			.map((r) =>
				create(MemoryChangeSchema, {
					seq: big(r.seq),
					what: str(r.what),
					by: create(StampSchema, {
						at: big(r.at),
						session: str(r.session),
						host: str(r.host),
						reason: str(r.reason),
					}),
					memory: unmarshal(MemorySchema, str(r.doc)),
				}),
			);
	}

	listMemories(filter: {
		states: MemoryState[];
		scope: MemoryScope;
		project: string;
		allProjects: boolean;
		mode: MemoryMode;
		tag: string;
		query: string;
		order: MemoryOrder;
		limit: number;
		includeBody?: boolean;
	}): { memories: Memory[]; total: number } {
		if (filter.project) {
			this.getProject(filter.project);
		}
		const states =
			filter.states.length > 0
				? filter.states.map(memoryStateName)
				: [memoryStateName(MemoryState.APPROVED)];
		const ph = states.map(() => "?").join(",");

		const clauses: string[] = [`m.state IN (${ph})`];
		const args: unknown[] = [...states];

		if (filter.scope === MemoryScope.GLOBAL) {
			clauses.push("m.project_id IS NULL");
		} else if (filter.scope === MemoryScope.PROJECT) {
			if (filter.allProjects) {
				clauses.push("m.project_id IS NOT NULL");
			} else {
				clauses.push("m.scope_key = ?");
				args.push(filter.project);
			}
		} else if (!filter.allProjects) {
			if (filter.project) {
				clauses.push("(m.scope_key = '' OR m.scope_key = ?)");
				args.push(filter.project);
			} else {
				clauses.push("m.scope_key = ''");
			}
		}

		if (filter.mode !== MemoryMode.UNSPECIFIED) {
			clauses.push("m.mode = ?");
			args.push(memoryModeName(filter.mode));
		}

		const q = filter.query.trim().toLowerCase();
		if (q) {
			clauses.push("instr(m.search_text, ?) > 0");
			args.push(q);
		}

		const whereSql = clauses.join(" AND ");
		const isProposalOnly = filter.states.length === 1 && filter.states[0] === MemoryState.PROPOSED;
		const effectiveOrder =
			filter.order !== MemoryOrder.UNSPECIFIED
				? filter.order
				: isProposalOnly
					? MemoryOrder.NEWEST
					: MemoryOrder.RANK;

		const orderBy =
			effectiveOrder === MemoryOrder.NEWEST
				? "m.proposed_at DESC, m.id DESC"
				: effectiveOrder === MemoryOrder.OLDEST
					? "m.proposed_at ASC, m.id ASC"
					: effectiveOrder === MemoryOrder.UPDATED
						? "m.updated_at DESC, m.id DESC"
						: effectiveOrder === MemoryOrder.SLUG
							? "m.slug ASC, m.id ASC"
							: "m.rank ASC, m.updated_at DESC, m.id DESC";

		const rows = this.sql
			.exec(
				`SELECT m.id, m.rank, COALESCE(p.name, '') AS project, m.doc
				 FROM memories m LEFT JOIN projects p ON p.id = m.project_id
				 WHERE ${whereSql}
				 ORDER BY ${orderBy}`,
				...args,
			)
			.toArray();

		const tag = filter.tag.trim().toLowerCase();
		const matched: Memory[] = [];
		for (const r of rows) {
			const m = this.decodeMemoryRow(r);
			if (tag && !(m.content?.tags ?? []).includes(tag)) continue;
			if (!filter.includeBody && m.content) {
				m.content.body = "";
			}
			matched.push(m);
		}
		const total = matched.length;
		const memories = filter.limit > 0 ? matched.slice(0, filter.limit) : matched;
		return { memories, total };
	}

	memoryBriefing(project: string): MemoryBriefing {
		const approvedRows = this.sql
			.exec(
				`SELECT m.id, m.rank, COALESCE(p.name, '') AS project, m.doc
				 FROM memories m LEFT JOIN projects p ON p.id = m.project_id
				 WHERE m.state = ? AND (m.scope_key = '' OR m.scope_key = ?)
				 ORDER BY m.rank ASC, m.updated_at DESC, m.id DESC`,
				memoryStateName(MemoryState.APPROVED),
				project,
			)
			.toArray();

		const pendingProposals = num(
			this.sql
				.exec(
					`SELECT COUNT(*) AS cnt FROM memories
					 WHERE state = ? AND (scope_key = '' OR scope_key = ?)`,
					memoryStateName(MemoryState.PROPOSED),
					project,
				)
				.one().cnt,
		);

		const always: MemoryIndexEntry[] = [];
		const onDemand: MemoryIndexEntry[] = [];
		let alwaysBytes = 0;
		let onDemandBytes = 0;
		let omittedAlways = 0;
		let omittedOnDemand = 0;
		const enc = new TextEncoder();

		for (const r of approvedRows) {
			const m = this.decodeMemoryRow(r);
			const c = m.content;
			if (!c) continue;
			const entry = create(MemoryIndexEntrySchema, {
				id: m.id,
				slug: c.slug,
				scope: m.scope,
				project: m.project,
				repo: m.repo,
				mode: c.mode,
				summary: c.summary,
				hasBody: c.body.trim().length > 0,
				revision: m.revision,
				updatedAt: m.updatedAt,
			});
			const entryBytes = enc.encode(`${c.slug}: ${c.summary}`).byteLength;
			if (c.mode === MemoryMode.ALWAYS) {
				if (
					always.length < ControllerStore.BRIEFING_ALWAYS_MAX_COUNT &&
					(always.length === 0 ||
						alwaysBytes + entryBytes <= ControllerStore.BRIEFING_ALWAYS_MAX_BYTES)
				) {
					always.push(entry);
					alwaysBytes += entryBytes;
				} else {
					omittedAlways++;
				}
			} else {
				if (
					onDemand.length < ControllerStore.BRIEFING_ON_DEMAND_MAX_COUNT &&
					(onDemand.length === 0 ||
						onDemandBytes + entryBytes <= ControllerStore.BRIEFING_ON_DEMAND_MAX_BYTES)
				) {
					onDemand.push(entry);
					onDemandBytes += entryBytes;
				} else {
					omittedOnDemand++;
				}
			}
		}

		return create(MemoryBriefingSchema, {
			always,
			onDemand,
			omittedAlways,
			omittedOnDemand,
			totalApproved: approvedRows.length,
			pendingProposals,
		});
	}

	pendingMemoriesCounts(): { total: number; global: number; byProject: Map<string, number> } {
		const rows = this.sql
			.exec(
				`SELECT scope_key, COUNT(*) AS cnt FROM memories WHERE state = ? GROUP BY scope_key`,
				memoryStateName(MemoryState.PROPOSED),
			)
			.toArray();
		let total = 0;
		let global = 0;
		const byProject = new Map<string, number>();
		for (const r of rows) {
			const sk = str(r.scope_key);
			const cnt = num(r.cnt);
			total += cnt;
			if (sk === "") global += cnt;
			else byProject.set(sk, cnt);
		}
		return { total, global, byProject };
	}

	changeMemory(id: number, what: string, by: Stamp, edit: (m: Memory) => string | void): Memory {
		return this.transactionSync(() => this.changeMemoryTx(id, what, by, edit));
	}

	deleteMemory(id: number, check: (m: Memory) => void): Memory {
		return this.transactionSync(() => {
			const item = this.getMemory(id);
			check(item);
			this.sql.exec("DELETE FROM memory_changes WHERE memory_id = ?", id);
			this.sql.exec("DELETE FROM memories WHERE id = ?", id);
			return item;
		});
	}

	moveMemory(
		id: number,
		place: { before?: number; first?: boolean },
		by: Stamp,
		check: (m: Memory, before: Memory | null) => void,
	): Memory {
		return this.transactionSync(() => {
			let beforeId = place.before ?? 0;
			if (place.first) {
				const item = this.getMemory(id);
				const rows = this.sql
					.exec(
						`SELECT id FROM memories WHERE state = ? ORDER BY rank ASC, id ASC LIMIT 1`,
						memoryStateName(MemoryState.APPROVED),
					)
					.toArray();
				if (rows.length === 0 || num(rows[0].id) === id) {
					check(item, null);
					return item;
				}
				beforeId = num(rows[0].id);
			}
			const next = beforeId !== 0 ? this.getMemory(beforeId) : null;
			return this.changeMemoryTx(id, "moved", by, (item) => {
				check(item, next);
				if (!next) {
					const maxRank = num(
						this.sql
							.exec(
								"SELECT COALESCE(MAX(rank), 0) AS m FROM memories WHERE state = ?",
								memoryStateName(MemoryState.APPROVED),
							)
							.one().m,
					);
					item.rank = BigInt(maxRank + 1);
				} else {
					this.sql.exec(
						"UPDATE memories SET rank = rank + 1 WHERE rank >= ? AND id != ? AND state = ?",
						Number(next.rank),
						item.id,
						memoryStateName(MemoryState.APPROVED),
					);
					item.rank = next.rank;
				}
			});
		});
	}

	private changeMemoryTx(
		id: number,
		what: string,
		by: Stamp,
		edit: (m: Memory) => string | void,
	): Memory {
		const item = this.getMemory(id);
		const label = edit(item) || what;
		const scopeKey = item.scope === MemoryScope.PROJECT ? item.project : "";
		const pid = item.scope === MemoryScope.PROJECT ? this.projectIdByName(item.project) : null;
		if (item.state === MemoryState.APPROVED) {
			const slug = item.content?.slug ?? "";
			const conflict = this.findApprovedByScopeSlug(scopeKey, slug);
			if (conflict && conflict.id !== item.id) {
				const where = scopeKey ? `project ${scopeKey}` : "global";
				throw hotbedError(
					ErrorCode.Conflict,
					`an approved memory #${conflict.id} already uses slug "${slug}" in ${where}`,
				).withSuggestion(`pick a different slug or remove/archive #${conflict.id} first`);
			}
		}
		if (label !== "moved") item.updatedAt = by.at;
		this.saveMemoryRow(item, pid, label, by);
		return item;
	}

	private saveMemoryRow(item: Memory, pid: number | null, what: string, by: Stamp): void {
		item.revision += 1n;
		const scopeKey = item.scope === MemoryScope.PROJECT ? item.project : "";
		const stored = clone(MemorySchema, item);
		stored.rank = 0n;
		stored.history = [];
		stored.project = "";
		stored.reminder = undefined;
		stored.cockpitUrl = "";
		this.sql.exec(
			`UPDATE memories
			 SET project_id = ?, scope_key = ?, repo = ?, slug = ?, mode = ?, state = ?, rank = ?, updated_at = ?, search_text = ?, doc = ?
			 WHERE id = ?`,
			pid,
			scopeKey,
			item.repo,
			item.content?.slug ?? "",
			memoryModeName(item.content?.mode ?? MemoryMode.ON_DEMAND),
			memoryStateName(item.state),
			Number(item.rank),
			Number(item.updatedAt),
			memorySearchText(item.content),
			marshal(MemorySchema, stored),
			item.id,
		);
		stored.rank = item.rank;
		stored.project = item.project;
		this.sql.exec(
			`INSERT INTO memory_changes (memory_id, what, at, session, host, reason, doc)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			item.id,
			what,
			Number(by.at),
			by.session,
			by.host,
			by.reason,
			marshal(MemorySchema, stored),
		);
	}

	private decodeMemoryRow(r: Record<string, SqlStorageValue>): Memory {
		const m = unmarshal(MemorySchema, str(r.doc));
		m.id = num(r.id);
		m.rank = big(r.rank);
		m.project = str(r.project);
		return m;
	}

	// ---- worker configs -------------------------------------------------

	private decodeWorkerConfigRow(r: Record<string, SqlStorageValue>): WorkerConfig {
		const cfg = unmarshal(WorkerConfigSchema, str(r.doc));
		cfg.node = str(r.scope);
		cfg.revision = big(r.revision);
		cfg.updatedAt = big(r.updated_at);
		cfg.updatedBy = str(r.updated_by);
		return cfg;
	}

	getWorkerConfig(scope: string): WorkerConfig | null {
		const rows = this.sql
			.exec(
				"SELECT scope, revision, doc, updated_at, updated_by FROM worker_configs WHERE scope = ?",
				scope,
			)
			.toArray();
		if (rows.length === 0) return null;
		return this.decodeWorkerConfigRow(rows[0]);
	}

	listWorkerConfigs(): WorkerConfig[] {
		return this.sql
			.exec(
				"SELECT scope, revision, doc, updated_at, updated_by FROM worker_configs ORDER BY scope ASC",
			)
			.toArray()
			.map((r) => this.decodeWorkerConfigRow(r));
	}

	setWorkerConfig(
		scope: string,
		patch: WorkerConfig,
		opts: { replace: boolean; ifRevision: bigint; updatedBy: string; nowMs: number },
	): WorkerConfig {
		validateWorkerConfig(patch, scope);
		return this.storage.transactionSync(() => {
			const existing = this.getWorkerConfig(scope);
			const curRev = existing?.revision ?? 0n;
			if (opts.ifRevision > 0n && curRev !== opts.ifRevision) {
				const label = scope || "defaults";
				throw hotbedError(
					ErrorCode.Conflict,
					`worker config ${label} is at revision ${curRev}, not ${opts.ifRevision}`,
				).withSuggestion(scope ? `hotbed config show ${scope}` : "hotbed config show");
			}
			const next = mergeWorkerConfig(opts.replace ? null : existing, patch);
			validateWorkerConfig(next, scope);
			const nextRev = curRev + 1n;
			next.node = scope;
			next.revision = nextRev;
			next.updatedAt = BigInt(opts.nowMs);
			next.updatedBy = opts.updatedBy;
			const stored = clone(WorkerConfigSchema, next);
			stored.node = "";
			stored.revision = 0n;
			stored.updatedAt = 0n;
			stored.updatedBy = "";
			this.sql.exec(
				`INSERT INTO worker_configs (scope, revision, doc, updated_at, updated_by)
				 VALUES (?, ?, ?, ?, ?)
				 ON CONFLICT(scope) DO UPDATE SET
				   revision = excluded.revision,
				   doc = excluded.doc,
				   updated_at = excluded.updated_at,
				   updated_by = excluded.updated_by`,
				scope,
				Number(nextRev),
				marshal(WorkerConfigSchema, stored),
				opts.nowMs,
				opts.updatedBy,
			);
			return next;
		});
	}

	deleteWorkerConfig(scope: string): boolean {
		const existing = this.getWorkerConfig(scope);
		if (!existing) return false;
		this.sql.exec("DELETE FROM worker_configs WHERE scope = ?", scope);
		return true;
	}

	effectiveWorkerConfig(hostname: string, nodeId?: string): WorkerConfig | undefined {
		const defaults = this.getWorkerConfig("");
		const nodeOverride =
			(hostname ? this.getWorkerConfig(hostname) : null) ??
			(nodeId && nodeId !== hostname ? this.getWorkerConfig(nodeId) : null);
		if (!defaults && !nodeOverride) return undefined;
		let merged: WorkerConfig;
		if (defaults && nodeOverride) {
			merged = mergeWorkerConfig(defaults, nodeOverride);
		} else {
			merged = clone(WorkerConfigSchema, (nodeOverride ?? defaults)!);
		}
		merged.node = hostname || nodeId || "";
		merged.revision = (defaults?.revision ?? 0n) + (nodeOverride?.revision ?? 0n);
		const defAt = defaults?.updatedAt ?? 0n;
		const nodeAt = nodeOverride?.updatedAt ?? 0n;
		merged.updatedAt = nodeAt >= defAt ? nodeAt : defAt;
		merged.updatedBy =
			nodeAt >= defAt ? (nodeOverride?.updatedBy ?? "") : (defaults?.updatedBy ?? "");
		return merged;
	}

	// ---- base_freshens --------------------------------------------------

	private saveBaseFreshen(f: BaseFreshen): void {
		this.sql.exec(
			`INSERT INTO base_freshens (id, node_id, base_id, project, runtime, state, cold, created_at, started_at, ended_at, doc)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (id) DO UPDATE SET
			   node_id = excluded.node_id,
			   base_id = excluded.base_id,
			   project = excluded.project,
			   runtime = excluded.runtime,
			   state = excluded.state,
			   cold = excluded.cold,
			   created_at = CASE WHEN base_freshens.created_at > 0 AND base_freshens.created_at < excluded.created_at THEN base_freshens.created_at ELSE excluded.created_at END,
			   started_at = COALESCE(excluded.started_at, base_freshens.started_at),
			   ended_at = COALESCE(excluded.ended_at, base_freshens.ended_at),
			   doc = excluded.doc`,
			f.id,
			f.nodeId,
			f.baseId,
			f.project,
			f.runtime || "",
			freshenStateName(f.state),
			f.cold ? 1 : 0,
			Number(f.createdAt),
			f.startedAt !== undefined ? Number(f.startedAt) : null,
			f.endedAt !== undefined ? Number(f.endedAt) : null,
			marshal(BaseFreshenSchema, f),
		);
	}

	private upsertBaseFreshenFromBase(
		nodeId: string,
		b: Base,
		what: string,
		detail: string,
		eventAtMs: number,
	): void {
		const id = freshenIdForBase(nodeId, b);
		const existing = this.getBaseFreshen(id);
		const merged = mergeBaseIntoFreshen(existing, nodeId, b, what, detail, eventAtMs);
		this.saveBaseFreshen(merged);
	}

	private upsertBaseFreshenFromCommand(
		nodeId: string,
		c: Command,
		what: string,
		detail: string,
		eventAtMs: number,
	): void {
		const existing = this.getBaseFreshen(c.id);
		const merged = mergeCommandIntoFreshen(existing, nodeId, c, what, detail, eventAtMs);
		this.saveBaseFreshen(merged);
	}

	getBaseFreshen(id: string): BaseFreshen | null {
		const rows = this.sql.exec("SELECT doc FROM base_freshens WHERE id = ?", id).toArray();
		if (rows.length === 0) return null;
		return unmarshal(BaseFreshenSchema, str(rows[0].doc));
	}

	listBaseFreshens(sinceMs: number): BaseFreshen[] {
		const rows =
			sinceMs > 0
				? this.sql
						.exec(
							`SELECT doc FROM base_freshens
							 WHERE ended_at IS NULL OR created_at >= ? OR ended_at >= ?
							 ORDER BY created_at DESC`,
							sinceMs,
							sinceMs,
						)
						.toArray()
				: this.sql.exec("SELECT doc FROM base_freshens ORDER BY created_at DESC").toArray();
		return rows.map((r) => unmarshal(BaseFreshenSchema, str(r.doc)));
	}

	// ---- prune ----------------------------------------------------------

	prune(nowMs: number): void {
		const histCutoff = nowMs - KEEP_HISTORY_MS;
		this.sql.exec("DELETE FROM samples WHERE t < ?", nowMs - KEEP_SAMPLES_MS);
		this.sql.exec("DELETE FROM history WHERE ended_at < ?", histCutoff);
		this.sql.exec(
			"DELETE FROM base_freshens WHERE ended_at IS NOT NULL AND ended_at < ?",
			histCutoff,
		);
		this.sql.exec("DELETE FROM command_profiles WHERE ended_at < ?", histCutoff);
		this.sql.exec("DELETE FROM command_diagnostics WHERE created_at < ?", histCutoff);
		this.sql.exec("DELETE FROM command_samples WHERE created_at < ?", histCutoff);
		this.sql.exec(
			"DELETE FROM events WHERE id <= (SELECT MAX(e.id) FROM events e) - ?",
			KEEP_EVENTS,
		);
		this.sql.exec(
			`DELETE FROM commands WHERE created_at < ?
			 AND id NOT IN (SELECT id FROM node_state WHERE kind = 'command')
			 AND id NOT IN (SELECT id FROM history)
			 AND id NOT IN (SELECT id FROM base_freshens)`,
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
		this.sql.exec(
			`DELETE FROM session_events
			 WHERE session_id IN (SELECT id FROM sessions WHERE ended_at IS NOT NULL AND ended_at < ?)`,
			histCutoff,
		);
		this.sql.exec(
			"DELETE FROM cladefs_retired_incarnations WHERE retired_at < ?",
			nowMs - 7 * 24 * 3600_000,
		);
		this.sql.exec(
			"DELETE FROM kv_meta WHERE key LIKE 'cli_handoff:%' AND CAST(value AS INTEGER) < ?",
			nowMs,
		);
		this.samplesGen++;
	}
}

export function sessionEndedError(id: string, reason: string): Error {
	const msg = reason ? `session ${id} ended (${reason})` : `session ${id} ended`;
	return hotbedError(ErrorCode.NotFound, msg).withSuggestion(
		`start a new session (hotbed session start), or resume this one (hotbed session start --resume ${id})`,
	);
}
