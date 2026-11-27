import { clone, create, fromJsonString, toJsonString } from "@bufbuild/protobuf";
import {
	BoardActorKind,
	BoardActorSchema,
	BoardAttachmentSchema,
	BoardAuthorSchema,
	BoardBlockSchema,
	BoardEventSchema,
	BoardMentionSchema,
	BoardMessageSchema,
	BoardReactionSchema,
	BoardScheduleSchema,
	BoardScheduleState,
	BoardSchemaSchema,
	BoardTaskBlockedSchema,
	BoardTaskClaimSchema,
	BoardTaskClaimViewSchema,
	BoardTaskSchema,
	BoardTaskState,
	BoardTaskStateSchema,
	BoardThreadSummarySchema,
	ImportBoardResponseSchema,
	StampSchema,
	type BatchBoardTaskSpec,
	type BoardActor,
	type BoardAttachment,
	type BoardAuthor,
	type BoardEvent,
	type BoardLabelFilter,
	type BoardMention,
	type BoardMessage,
	type BoardSchedule,
	type BoardSchema,
	type BoardTask,
	type BoardTaskClaim,
	type BoardTaskClaimView,
	type BoardThreadSummary,
	type ImportBoardRequest,
	type ImportBoardResponse,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import { enumFromWord, enumWord } from "../enums.ts";
import { ErrorCode, hotbedError } from "../errors.ts";
import type { ProjectStore } from "../store/projects.ts";
import { blocksIn, findBlocks } from "./blocks.ts";
import { canonicalEmoji, normalizeEmoji } from "./emoji.ts";
import { mentionTerm, mentionTerms, settleMentions } from "./mentions.ts";
import {
	BUILT_IN_SCHEMAS,
	SCHEMA_DESCRIPTION_MAX,
	SCHEMA_NAME,
	SCHEMA_VERSION_MAX,
	SCHEMAS_MAX,
	canonicalJson,
	checkDefinition,
	checkValue,
	isBuiltInName,
	schemaId,
	type SchemaDefinition,
} from "./schemas.ts";

export const MESSAGE_TEXT_MAX = 10_000;
export const ATTACHMENTS_PER_MESSAGE_MAX = 10;
export const ATTACHMENT_BYTES_MAX = 4 * 1024 * 1024;
export const ATTACHMENT_CHUNK_BYTES = 512 * 1024;
export const LABEL_KEY_MAX = 64;
export const LABEL_VALUE_MAX = 256;
export const RESERVED_LABEL_PREFIX = "board.";
export const TASK_LABEL = "board.task";
export const TASK_NEEDS_MAX = 20;
export const TASK_BATCH_MAX = 100;
export const TASK_KEY_MAX = 64;
export const TASK_NOTE_MAX = 1000;
export const NEEDS_WALK_MAX = 500;
export const LEASE_MIN_S = 30;
export const LEASE_DEFAULT_S = 900;
export const LEASE_CAP_S = 3600;
export const EDITED_HOLD = "edited";

const MENTION_TERM = "@";
const ATTACHMENT_TERM = "^";
const NEEDS_TERM = ">needs";
const SCHEDULE_MAX_AHEAD_MS = 366 * 24 * 3600_000;
const SCHEDULE_MIN_EVERY_S = 60;

const LABEL_KEY_RE = /^[a-z0-9][a-z0-9_.-]{0,63}$/;
const TASK_HOLD_KIND_RE = /^[a-z0-9][a-z0-9_.-]{0,63}$/;
const PROFILE_RE = /^[A-Za-z][A-Za-z0-9-]{0,31}$/;
const INHERITED = new Set([...Object.getOwnPropertyNames(Object.prototype), "prototype"]);

export interface BoardIdentity {
	author: BoardAuthor;
	actor: BoardActor;
	isAdmin: boolean;
}

export interface ProjectBoardCounts {
	threads: number;
	openTasks: number;
	readyTasks: number;
	blockedTasks: number;
}

export interface ThreadFilterOpts {
	projectId: number | null;
	labels: BoardLabelFilter[];
	by: string;
	mentions: string;
	callerActorId: string;
	callerHandle: string;
	text: string;
	after: number;
	before: string;
	limit: number;
}

export interface EventFilterOpts {
	projectId: number | null;
	re: number;
	labels: BoardLabelFilter[];
	by: string;
	mentions: string;
	callerActorId: string;
	callerHandle: string;
	text: string;
	after: number;
	last: number;
	limit: number;
	includeAll: boolean;
}

export interface TaskFilterOpts {
	projectId: number | null;
	states: BoardTaskState[];
	includeAll: boolean;
	labels: BoardLabelFilter[];
	by: string;
	owner: string;
	forActor: string;
	needs: number;
	after: number;
	limit: number;
	latest: boolean;
}

const BOOKKEEPING_EVENTS = new Set(["task.renewed", "actor.seen"]);

export function taskStateName(st: BoardTaskState): string {
	return st === BoardTaskState.UNSPECIFIED ? "" : enumWord(BoardTaskStateSchema, st);
}

export function parseTaskState(name: string): BoardTaskState {
	return enumFromWord(BoardTaskStateSchema, name.toLowerCase());
}

const OPEN_TASK_STATES: readonly BoardTaskState[] = [
	BoardTaskState.WAITING,
	BoardTaskState.READY,
	BoardTaskState.CLAIMED,
	BoardTaskState.BLOCKED,
];

function isFinished(t: BoardTask): boolean {
	return (
		t.state === BoardTaskState.COMPLETED ||
		t.state === BoardTaskState.FAILED ||
		t.state === BoardTaskState.CANCELED
	);
}

function isLapsed(t: BoardTask, nowMs: number): boolean {
	return (
		t.state === BoardTaskState.CLAIMED && t.claim !== undefined && Number(t.claim.until) <= nowMs
	);
}

function effectiveState(t: BoardTask, nowMs: number): BoardTaskState {
	return isLapsed(t, nowMs) ? BoardTaskState.READY : t.state;
}

function storable(value: string): boolean {
	return !value.includes("\u0000") && value.isWellFormed();
}

function cleanSql(value: string): string {
	return value.replaceAll("\u0000", "").toWellFormed();
}

const SHA256_K = new Uint32Array([
	0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
	0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
	0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
	0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
	0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
	0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
	0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
	0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
]);

function sha256HexSync(input: string): string {
	const data = new TextEncoder().encode(input);
	const bitLen = data.length * 8;
	const paddedLen = Math.ceil((data.length + 9) / 64) * 64;
	const buf = new Uint8Array(paddedLen);
	buf.set(data);
	buf[data.length] = 0x80;
	const view = new DataView(buf.buffer);
	view.setUint32(paddedLen - 8, Math.floor(bitLen / 0x100000000), false);
	view.setUint32(paddedLen - 4, bitLen >>> 0, false);

	let h0 = 0x6a09e667;
	let h1 = 0xbb67ae85;
	let h2 = 0x3c6ef372;
	let h3 = 0xa54ff53a;
	let h4 = 0x510e527f;
	let h5 = 0x9b05688c;
	let h6 = 0x1f83d9ab;
	let h7 = 0x5be0cd19;
	const w = new Uint32Array(64);

	for (let offset = 0; offset < paddedLen; offset += 64) {
		for (let i = 0; i < 16; i++) w[i] = view.getUint32(offset + i * 4, false);
		for (let i = 16; i < 64; i++) {
			const s0 =
				((w[i - 15] >>> 7) | (w[i - 15] << 25)) ^
				((w[i - 15] >>> 18) | (w[i - 15] << 14)) ^
				(w[i - 15] >>> 3);
			const s1 =
				((w[i - 2] >>> 17) | (w[i - 2] << 15)) ^
				((w[i - 2] >>> 19) | (w[i - 2] << 13)) ^
				(w[i - 2] >>> 10);
			w[i] = (w[i - 16] + s0 + w[i - 7] + s1) >>> 0;
		}
		let a = h0;
		let b = h1;
		let c = h2;
		let d = h3;
		let e = h4;
		let f = h5;
		let g = h6;
		let h = h7;
		for (let i = 0; i < 64; i++) {
			const S1 = ((e >>> 6) | (e << 26)) ^ ((e >>> 11) | (e << 21)) ^ ((e >>> 25) | (e << 7));
			const ch = (e & f) ^ (~e & g);
			const t1 = (h + S1 + ch + SHA256_K[i] + w[i]) >>> 0;
			const S0 = ((a >>> 2) | (a << 30)) ^ ((a >>> 13) | (a << 19)) ^ ((a >>> 22) | (a << 10));
			const maj = (a & b) ^ (a & c) ^ (b & c);
			const t2 = (S0 + maj) >>> 0;
			h = g;
			g = f;
			f = e;
			e = (d + t1) >>> 0;
			d = c;
			c = b;
			b = a;
			a = (t1 + t2) >>> 0;
		}
		h0 = (h0 + a) >>> 0;
		h1 = (h1 + b) >>> 0;
		h2 = (h2 + c) >>> 0;
		h3 = (h3 + d) >>> 0;
		h4 = (h4 + e) >>> 0;
		h5 = (h5 + f) >>> 0;
		h6 = (h6 + g) >>> 0;
		h7 = (h7 + h) >>> 0;
	}
	return [h0, h1, h2, h3, h4, h5, h6, h7].map((x) => x.toString(16).padStart(8, "0")).join("");
}

function randomBase64Url(bytes = 16): string {
	const arr = new Uint8Array(bytes);
	crypto.getRandomValues(arr);
	let bin = "";
	for (let i = 0; i < arr.length; i++) bin += String.fromCharCode(arr[i]);
	return btoa(bin).replaceAll("+", "-").replaceAll("/", "_").replaceAll("=", "");
}

function normalizeLeaseSeconds(seconds: number): number {
	if (seconds === 0) return LEASE_DEFAULT_S;
	if (seconds < LEASE_MIN_S || seconds > LEASE_CAP_S) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`lease must be between ${LEASE_MIN_S} and ${LEASE_CAP_S} seconds`,
		);
	}
	return seconds;
}

function validateLabelKey(key: string, allowBoard = false): void {
	if (!LABEL_KEY_RE.test(key) || INHERITED.has(key)) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`${JSON.stringify(key)} isn't a valid label key: use 1 to ${LABEL_KEY_MAX} lowercase letters, digits, '.', '_' or '-', starting with a letter or digit`,
		);
	}
	if (!allowBoard && key.startsWith(RESERVED_LABEL_PREFIX)) {
		const suggestion =
			key === TASK_LABEL
				? "use `hotbed board task` commands to change task state"
				: "labels under `board.` are reserved by the board";
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`label ${JSON.stringify(key)} is reserved by the board`,
			{ suggestion },
		);
	}
}

function validateLabelValue(key: string, value: string): void {
	if (!value) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`label ${JSON.stringify(key)} has an empty value: use --unset ${key} to remove it`,
		);
	}
	if (value.length > LABEL_VALUE_MAX) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`label ${JSON.stringify(key)} value is at most ${LABEL_VALUE_MAX} characters`,
		);
	}
	if (!storable(value) || /\p{Cc}/u.test(value)) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`label ${JSON.stringify(key)} value must not contain control characters`,
		);
	}
}

function validateLabels(
	labels: Record<string, string>,
	allowBoard = false,
): Record<string, string> {
	const out: Record<string, string> = {};
	for (const [k, v] of Object.entries(labels)) {
		validateLabelKey(k, allowBoard);
		validateLabelValue(k, v);
		out[k] = v;
	}
	return out;
}

export function validateProfileName(profile: string): string {
	const trimmed = profile.trim();
	if (!trimmed) return "";
	if (!PROFILE_RE.test(trimmed)) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`invalid profile name ${JSON.stringify(profile)}: start with a letter and use up to 32 letters, digits, or hyphens`,
		);
	}
	return trimmed.toLowerCase();
}

function validateMessageText(text: string, hasAttachments: boolean): void {
	if (!storable(text)) {
		throw hotbedError(ErrorCode.InvalidArgs, "message text contains NUL or invalid UTF-16");
	}
	if (text.length > MESSAGE_TEXT_MAX) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`message text is at most ${MESSAGE_TEXT_MAX} characters; attach longer content as a file`,
		);
	}
	if (!hasAttachments && text.trim().length === 0) {
		throw hotbedError(ErrorCode.InvalidArgs, "message text is empty (or attach at least one file)");
	}
}

function validateTaskNote(what: "result" | "reason", text: string, required: boolean): string {
	const trimmed = text.trim();
	if (!trimmed) {
		if (required) throw hotbedError(ErrorCode.InvalidArgs, `task ${what} is required`);
		return "";
	}
	if (!storable(trimmed)) {
		throw hotbedError(ErrorCode.InvalidArgs, `task ${what} contains invalid characters`);
	}
	if (trimmed.length > TASK_NOTE_MAX) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`task ${what} is at most ${TASK_NOTE_MAX} characters; put long details in a reply`,
		);
	}
	return trimmed;
}

function num(v: unknown): number {
	return typeof v === "number" ? v : typeof v === "bigint" ? Number(v) : Number(v ?? 0);
}

function str(v: unknown): string {
	if (typeof v === "string") return v;
	if (typeof v === "number" || typeof v === "bigint" || typeof v === "boolean") return String(v);
	return "";
}

function marshalMsg(msg: BoardMessage): string {
	return toJsonString(BoardMessageSchema, msg);
}

function unmarshalMsg(doc: string): BoardMessage {
	return fromJsonString(BoardMessageSchema, doc || "{}", { ignoreUnknownFields: true });
}

function marshalActor(actor: BoardActor): string {
	return toJsonString(BoardActorSchema, actor);
}

function unmarshalActor(doc: string): BoardActor {
	return fromJsonString(BoardActorSchema, doc || "{}", { ignoreUnknownFields: true });
}

function marshalSchedule(sched: BoardSchedule): string {
	return toJsonString(BoardScheduleSchema, sched);
}

function unmarshalSchedule(doc: string): BoardSchedule {
	return fromJsonString(BoardScheduleSchema, doc || "{}", { ignoreUnknownFields: true });
}

function marshalEvent(ev: BoardEvent): string {
	return toJsonString(BoardEventSchema, ev);
}

function unmarshalEvent(doc: string): BoardEvent {
	return fromJsonString(BoardEventSchema, doc || "{}", { ignoreUnknownFields: true });
}

function likeEscape(text: string): string {
	return text.replace(/[\\%_]/g, (c) => `\\${c}`);
}

function actorKindWord(k: BoardActorKind): string {
	switch (k) {
		case BoardActorKind.PERSON:
			return "person";
		case BoardActorKind.AGENT:
			return "agent";
		case BoardActorKind.EXTERNAL:
			return "external";
		default:
			return "person";
	}
}

export class BoardStore {
	private readonly storage: DurableObjectStorage;
	private readonly sql: SqlStorage;
	private readonly projects: ProjectStore;

	constructor(storage: DurableObjectStorage, projects: ProjectStore) {
		this.storage = storage;
		this.sql = storage.sql;
		this.projects = projects;
	}

	private tx<T>(fn: () => T): T {
		return this.storage.transactionSync(fn);
	}

	private projectNameById(id: number): string {
		const rows = this.sql.exec("SELECT name FROM projects WHERE id = ?", id).toArray();
		return rows.length > 0 ? str(rows[0].name) : "";
	}

	private latestEventCursor(): number {
		const rows = this.sql.exec("SELECT COALESCE(MAX(seq), 0) AS c FROM board_events").toArray();
		return rows.length > 0 ? num(rows[0].c) : 0;
	}

	// ---- Actors ----

	upsertActor(actor: BoardActor, nowMs: number): void {
		const id = cleanSql(actor.id);
		if (!id) return;
		const handle = cleanSql(actor.handle || actor.name || id);
		const doc = marshalActor(actor);
		this.sql.exec(
			`INSERT INTO board_actors (id, kind, handle, handle_lower, name, session_id, updated_at, doc)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(id) DO UPDATE SET
			   kind = excluded.kind,
			   handle = excluded.handle,
			   handle_lower = excluded.handle_lower,
			   name = excluded.name,
			   session_id = CASE WHEN excluded.session_id != '' THEN excluded.session_id ELSE board_actors.session_id END,
			   updated_at = excluded.updated_at,
			   doc = excluded.doc`,
			id,
			actorKindWord(actor.kind),
			handle,
			handle.toLowerCase(),
			cleanSql(actor.name || handle),
			cleanSql(actor.sessionId || ""),
			nowMs,
			doc,
		);
	}

	listActors(projectId: number | null, ids: string[]): { actors: BoardActor[]; cursor: bigint } {
		const cursor = BigInt(this.latestEventCursor());
		if (ids.length > 0) {
			const cleanIds = ids.slice(0, 100);
			const ph = cleanIds.map(() => "?").join(", ");
			const rows = this.sql
				.exec(
					`SELECT doc FROM board_actors WHERE id IN (${ph}) ORDER BY handle_lower ASC`,
					...cleanIds,
				)
				.toArray();
			return { actors: rows.map((r) => unmarshalActor(str(r.doc))), cursor };
		}
		if (projectId !== null) {
			const rows = this.sql
				.exec(
					`SELECT a.doc FROM board_actors a
					 WHERE EXISTS (SELECT 1 FROM board_messages m WHERE m.project_id = ? AND m.by_actor = a.id)
					 ORDER BY a.handle_lower ASC`,
					projectId,
				)
				.toArray();
			return { actors: rows.map((r) => unmarshalActor(str(r.doc))), cursor };
		}
		const rows = this.sql.exec("SELECT doc FROM board_actors ORDER BY handle_lower ASC").toArray();
		return { actors: rows.map((r) => unmarshalActor(str(r.doc))), cursor };
	}

	private allActors(): BoardActor[] {
		return this.sql
			.exec("SELECT doc FROM board_actors ORDER BY handle_lower ASC")
			.toArray()
			.map((r) => unmarshalActor(str(r.doc)));
	}

	private resolveByTarget(raw: string): { actors: string[]; principals: string[] } {
		const q = raw.trim().replace(/^@/, "");
		if (!q) return { actors: [], principals: [] };
		const actors = new Set<string>();
		const principals = new Set<string>();
		if (q.includes(":")) {
			actors.add(q);
			if (!q.includes("+")) principals.add(q);
		}
		const lower = q.toLowerCase();
		const rows = this.sql
			.exec(
				"SELECT id, handle_lower, doc FROM board_actors WHERE handle_lower = ? OR handle_lower LIKE ?",
				lower,
				`${lower}/%`,
			)
			.toArray();
		for (const r of rows) {
			const id = str(r.id);
			const hl = str(r.handle_lower);
			if (hl === lower) {
				actors.add(id);
				const a = unmarshalActor(str(r.doc));
				if (a.kind === BoardActorKind.PERSON && !id.includes("+")) {
					principals.add(id);
				}
			} else if (hl.startsWith(`${lower}/`)) {
				actors.add(id);
			}
		}
		return { actors: [...actors], principals: [...principals] };
	}

	private resolveMentionTarget(
		raw: string,
		callerActorId: string,
		callerHandle: string,
	): { terms: string[]; prefixes: string[] } {
		const q = raw.trim().replace(/^@/, "");
		const terms = new Set<string>();
		const prefixes = new Set<string>();
		if (!q) return { terms: [], prefixes: [] };
		if (q === "me") {
			if (callerActorId) terms.add(callerActorId);
			if (callerHandle) terms.add(callerHandle.toLowerCase());
			return { terms: [...terms], prefixes: [...prefixes] };
		}
		terms.add(mentionTerm(q));
		const by = this.resolveByTarget(q);
		for (const id of by.actors) terms.add(id);
		for (const p of by.principals) {
			terms.add(p);
			prefixes.add(`${p}+`);
		}
		const lower = q.toLowerCase();
		if (!q.includes(":") && !q.includes("/")) {
			prefixes.add(`${lower}/`);
		}
		return { terms: [...terms], prefixes: [...prefixes] };
	}

	private settleTextMentions(
		text: string,
		threadRootId: number | null,
	): { text: string; mentions: BoardMention[] } {
		if (!text.includes("@")) return { text, mentions: [] };
		const actors = this.allActors();
		const byId = new Map(actors.map((a) => [a.id, a]));
		let threadParticipants: Set<string> | null = null;
		const prefer = (candidates: readonly { id: string; handle: string; name: string }[]) => {
			if (threadRootId === null) return null;
			if (!threadParticipants) {
				const rows = this.sql
					.exec(
						"SELECT DISTINCT by_actor FROM board_messages WHERE id = ? OR re = ?",
						threadRootId,
						threadRootId,
					)
					.toArray();
				threadParticipants = new Set(rows.map((r) => str(r.by_actor)));
			}
			const inThread = candidates.filter((c) => threadParticipants!.has(c.id));
			return inThread.length === 1 ? inThread[0] : null;
		};
		const res = settleMentions(
			text,
			actors.map((a) => ({ id: a.id, handle: a.handle, name: a.name })),
			prefer,
		);
		const mentions = res.mentions.map((m) =>
			create(BoardMentionSchema, {
				written: m.written,
				actor: m.actor ? byId.get(m.actor.id) : undefined,
				candidates: m.candidates
					.map((c) => byId.get(c.id))
					.filter((a): a is BoardActor => a !== undefined),
			}),
		);
		return { text: res.text, mentions };
	}

	// ---- Schemas & Blocks ----

	listSchemas(): BoardSchema[] {
		const builtIns = BUILT_IN_SCHEMAS.map((s) =>
			create(BoardSchemaSchema, {
				id: schemaId(s.name, s.version),
				name: s.name,
				version: s.version,
				description: s.description,
				schemaJson: JSON.stringify(s.schema),
				builtIn: true,
			}),
		);
		const rows = this.sql
			.exec(
				"SELECT id, name, version, description, schema_json, by_actor, created_at FROM board_schemas ORDER BY name ASC, version ASC",
			)
			.toArray();
		const custom = rows.map((r) =>
			create(BoardSchemaSchema, {
				id: str(r.id),
				name: str(r.name),
				version: num(r.version),
				description: str(r.description),
				schemaJson: str(r.schema_json),
				builtIn: false,
				by: str(r.by_actor),
				createdAt: BigInt(num(r.created_at)),
			}),
		);
		return [...builtIns, ...custom];
	}

	getSchema(id: string): BoardSchema {
		const found = this.findSchemaDefinition(id);
		if (!found) {
			throw hotbedError(ErrorCode.NotFound, `schema ${id} not found`).withSuggestion(
				"hotbed board schema ls",
			);
		}
		return found.proto;
	}

	private findSchemaDefinition(id: string): { proto: BoardSchema; def: SchemaDefinition } | null {
		for (const s of BUILT_IN_SCHEMAS) {
			if (schemaId(s.name, s.version) === id) {
				return {
					proto: create(BoardSchemaSchema, {
						id,
						name: s.name,
						version: s.version,
						description: s.description,
						schemaJson: JSON.stringify(s.schema),
						builtIn: true,
					}),
					def: s.schema,
				};
			}
		}
		const rows = this.sql
			.exec(
				"SELECT id, name, version, description, schema_json, by_actor, created_at FROM board_schemas WHERE id = ?",
				id,
			)
			.toArray();
		if (rows.length === 0) return null;
		const r = rows[0];
		const schemaJson = str(r.schema_json);
		return {
			proto: create(BoardSchemaSchema, {
				id: str(r.id),
				name: str(r.name),
				version: num(r.version),
				description: str(r.description),
				schemaJson,
				builtIn: false,
				by: str(r.by_actor),
				createdAt: BigInt(num(r.created_at)),
			}),
			def: JSON.parse(schemaJson) as SchemaDefinition,
		};
	}

	createSchema(
		name: string,
		version: number,
		description: string,
		schemaJson: string,
		byActor: string,
		nowMs: number,
	): { schema: BoardSchema; created: boolean } {
		const cleanName = name.trim();
		if (!SCHEMA_NAME.test(cleanName)) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`invalid schema name ${JSON.stringify(name)}: lowercase letters, digits, ._- starting with a letter`,
			);
		}
		if (!Number.isInteger(version) || version < 1 || version > SCHEMA_VERSION_MAX) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`schema version must be an integer from 1 to ${SCHEMA_VERSION_MAX}`,
			);
		}
		if (isBuiltInName(cleanName)) {
			throw hotbedError(
				ErrorCode.Conflict,
				`${cleanName} is a built-in schema name and cannot be re-registered`,
			);
		}
		const desc = description.trim();
		if (desc.length > SCHEMA_DESCRIPTION_MAX || !storable(desc)) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`schema description is at most ${SCHEMA_DESCRIPTION_MAX} characters`,
			);
		}
		let parsed: unknown;
		try {
			parsed = JSON.parse(schemaJson);
		} catch {
			throw hotbedError(ErrorCode.InvalidArgs, "schema_json is not valid JSON");
		}
		const defProblem = checkDefinition(parsed);
		if (defProblem !== null) {
			throw hotbedError(ErrorCode.InvalidArgs, defProblem);
		}
		const id = schemaId(cleanName, version);
		const canon = canonicalJson(parsed);

		return this.tx(() => {
			const existing = this.findSchemaDefinition(id);
			if (existing) {
				if (canonicalJson(existing.def) === canon && existing.proto.description === desc) {
					return { schema: existing.proto, created: false };
				}
				throw hotbedError(
					ErrorCode.Conflict,
					`${id} is already registered with a different definition; register a new version instead`,
				).withSuggestion(`hotbed board schema show ${id}`);
			}
			const countRows = this.sql.exec("SELECT COUNT(*) AS n FROM board_schemas").toArray();
			if (num(countRows[0]?.n) >= SCHEMAS_MAX) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`at most ${SCHEMAS_MAX} custom schemas are allowed`,
				);
			}
			this.sql.exec(
				`INSERT INTO board_schemas (id, name, version, description, schema_json, by_actor, created_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				id,
				cleanName,
				version,
				desc,
				canon,
				byActor,
				nowMs,
			);
			return {
				schema: create(BoardSchemaSchema, {
					id,
					name: cleanName,
					version,
					description: desc,
					schemaJson: canon,
					builtIn: false,
					by: byActor,
					createdAt: BigInt(nowMs),
				}),
				created: true,
			};
		});
	}

	validateAndParseBlocks(text: string): { schema: string; version: number; dataJson: string }[] {
		if (!text.includes("json#")) return [];
		const found = findBlocks(text);
		for (const blk of found) {
			if (blk.problem) {
				throw hotbedError(ErrorCode.InvalidArgs, blk.problem);
			}
			const id = schemaId(blk.name!, blk.version!);
			const reg = this.findSchemaDefinition(id);
			if (!reg) {
				throw hotbedError(ErrorCode.InvalidArgs, `schema ${id} is not registered`).withSuggestion(
					"hotbed board schema ls",
				);
			}
			const valErr = checkValue(reg.def, blk.data);
			if (valErr !== null) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`the ${id} block doesn't match its schema: ${valErr}`,
				).withSuggestion(`hotbed board schema show ${id}`);
			}
		}
		return blocksIn(text);
	}

	// ---- Attachments ----

	uploadAttachment(
		name: string,
		contentType: string,
		data: Uint8Array,
		uploader: string,
		nowMs: number,
	): BoardAttachment {
		if (data.byteLength === 0) {
			throw hotbedError(ErrorCode.InvalidArgs, "attachment is empty");
		}
		if (data.byteLength > ATTACHMENT_BYTES_MAX) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`attachment is ${data.byteLength} bytes (max ${ATTACHMENT_BYTES_MAX} bytes)`,
			);
		}
		const cleanName = (name.trim().replace(/[\p{Cc}/\\]/gu, "") || "file").slice(0, 255);
		const cleanType = (contentType.trim() || "application/octet-stream").slice(0, 255);
		const id = randomBase64Url(16);
		return this.tx(() => {
			this.sql.exec(
				`INSERT INTO board_attachments (id, name, content_type, size, uploader, created_at)
				 VALUES (?, ?, ?, ?, ?, ?)`,
				id,
				cleanName,
				cleanType,
				data.byteLength,
				uploader,
				nowMs,
			);
			let chunkIdx = 0;
			for (let off = 0; off < data.byteLength; off += ATTACHMENT_CHUNK_BYTES) {
				const slice = data.subarray(off, Math.min(off + ATTACHMENT_CHUNK_BYTES, data.byteLength));
				this.sql.exec(
					"INSERT INTO board_attachment_chunks (attachment_id, chunk_index, data) VALUES (?, ?, ?)",
					id,
					chunkIdx++,
					slice,
				);
			}
			return create(BoardAttachmentSchema, {
				id,
				name: cleanName,
				contentType: cleanType,
				size: BigInt(data.byteLength),
			});
		});
	}

	downloadAttachment(id: string): { attachment: BoardAttachment; data: Uint8Array } {
		const metaRows = this.sql
			.exec("SELECT id, name, content_type, size FROM board_attachments WHERE id = ?", id)
			.toArray();
		if (metaRows.length === 0) {
			throw hotbedError(ErrorCode.NotFound, `attachment ${id} not found`);
		}
		const meta = metaRows[0];
		const size = num(meta.size);
		const chunks = this.sql
			.exec(
				"SELECT data FROM board_attachment_chunks WHERE attachment_id = ? ORDER BY chunk_index ASC",
				id,
			)
			.toArray();
		const out = new Uint8Array(size);
		let off = 0;
		for (const c of chunks) {
			const raw = c.data;
			const bytes =
				raw instanceof Uint8Array
					? raw
					: raw instanceof ArrayBuffer
						? new Uint8Array(raw)
						: ArrayBuffer.isView(raw)
							? new Uint8Array(raw.buffer, raw.byteOffset, raw.byteLength)
							: new Uint8Array(0);
			out.set(bytes, off);
			off += bytes.byteLength;
		}
		return {
			attachment: create(BoardAttachmentSchema, {
				id: str(meta.id),
				name: str(meta.name),
				contentType: str(meta.content_type),
				size: BigInt(size),
			}),
			data: out,
		};
	}

	private resolveAttachments(ids: string[]): BoardAttachment[] {
		if (ids.length === 0) return [];
		if (ids.length > ATTACHMENTS_PER_MESSAGE_MAX) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`at most ${ATTACHMENTS_PER_MESSAGE_MAX} attachments per message`,
			);
		}
		const seen = new Set<string>();
		const out: BoardAttachment[] = [];
		for (const id of ids) {
			if (seen.has(id)) continue;
			seen.add(id);
			const rows = this.sql
				.exec("SELECT id, name, content_type, size FROM board_attachments WHERE id = ?", id)
				.toArray();
			if (rows.length === 0) {
				throw hotbedError(ErrorCode.InvalidArgs, `unknown attachment ID ${id}`);
			}
			const r = rows[0];
			out.push(
				create(BoardAttachmentSchema, {
					id: str(r.id),
					name: str(r.name),
					contentType: str(r.content_type),
					size: BigInt(num(r.size)),
				}),
			);
		}
		return out;
	}

	// ---- Internal indexing & events ----

	private reindexTerms(msg: BoardMessage): void {
		const id = Number(msg.id);
		this.sql.exec("DELETE FROM board_terms WHERE message_id = ?", id);
		if (msg.deleted) return;
		const rows: Array<[string, string]> = [];
		for (const [k, v] of Object.entries(msg.labels)) {
			rows.push([cleanSql(k), cleanSql(v)]);
		}
		for (const m of mentionTerms(msg.text)) {
			rows.push([MENTION_TERM, cleanSql(m)]);
		}
		const attSeen = new Set<string>();
		for (const a of msg.attachments) {
			if (!attSeen.has(a.id)) {
				attSeen.add(a.id);
				rows.push([ATTACHMENT_TERM, cleanSql(a.id)]);
			}
		}
		if (msg.task) {
			for (const need of msg.task.needs) {
				rows.push([NEEDS_TERM, String(need)]);
			}
		}
		for (const [term, value] of rows) {
			this.sql.exec(
				"INSERT OR IGNORE INTO board_terms (message_id, term, value) VALUES (?, ?, ?)",
				id,
				term,
				value,
			);
		}
	}

	private appendEvent(opts: {
		at: number;
		projectId: number;
		projectName: string;
		type: string;
		messageId: number;
		threadId: number;
		actor?: BoardActor;
		beforeMsg?: BoardMessage;
		afterMsg?: BoardMessage;
		payload?: Record<string, unknown>;
	}): number {
		const ev = create(BoardEventSchema, {
			type: opts.type,
			project: opts.projectName,
			createdAt: BigInt(opts.at),
			messageId: BigInt(opts.messageId),
			threadId: BigInt(opts.threadId),
			message: opts.afterMsg,
			actor: opts.actor,
			payloadJson: opts.payload ? JSON.stringify(opts.payload) : "{}",
		});
		const ins = this.sql
			.exec(
				`INSERT INTO board_events (at, project_id, type, message_id, thread_id, actor_id, before_doc, after_doc, doc)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
				 RETURNING seq`,
				opts.at,
				opts.projectId,
				opts.type,
				opts.messageId,
				opts.threadId,
				opts.actor?.id ?? "",
				opts.beforeMsg ? marshalMsg(opts.beforeMsg) : null,
				opts.afterMsg ? marshalMsg(opts.afterMsg) : null,
				marshalEvent(ev),
			)
			.toArray();
		const seq = num(ins[0].seq);
		ev.cursor = BigInt(seq);
		if (opts.afterMsg) {
			opts.afterMsg.version = BigInt(seq);
			ev.message = opts.afterMsg;
			this.sql.exec(
				"UPDATE board_events SET after_doc = ?, doc = ? WHERE seq = ?",
				marshalMsg(opts.afterMsg),
				marshalEvent(ev),
				seq,
			);
		} else {
			this.sql.exec("UPDATE board_events SET doc = ? WHERE seq = ?", marshalEvent(ev), seq);
		}
		return seq;
	}

	private getMessageRow(id: number): {
		id: number;
		projectId: number;
		re: number | null;
		msg: BoardMessage;
	} {
		const rows = this.sql
			.exec("SELECT id, project_id, re, doc FROM board_messages WHERE id = ?", id)
			.toArray();
		if (rows.length === 0) {
			throw hotbedError(ErrorCode.NotFound, `board message #${id} not found`);
		}
		const r = rows[0];
		return {
			id: num(r.id),
			projectId: num(r.project_id),
			re: r.re === null || r.re === undefined ? null : num(r.re),
			msg: unmarshalMsg(str(r.doc)),
		};
	}

	private persistMessage(msg: BoardMessage): void {
		const hasTask = msg.task ? 1 : 0;
		const taskState = msg.task ? taskStateName(msg.task.state) : null;
		const taskOwner = msg.task ? msg.task.owner : null;
		const taskFor = msg.task && msg.task.forActor ? msg.task.forActor : null;
		const taskLeaseUntil =
			msg.task && msg.task.state === BoardTaskState.CLAIMED && msg.task.claim
				? Number(msg.task.claim.until)
				: null;
		this.sql.exec(
			`UPDATE board_messages SET
			   text_lower = ?,
			   deleted = ?,
			   edited_at = ?,
			   version = ?,
			   revision = ?,
			   has_task = ?,
			   task_state = ?,
			   task_owner = ?,
			   task_for = ?,
			   task_lease_until = ?,
			   doc = ?
			 WHERE id = ?`,
			cleanSql(msg.text.toLowerCase()),
			msg.deleted ? 1 : 0,
			msg.editedAt !== undefined ? Number(msg.editedAt) : null,
			Number(msg.version),
			Number(msg.revision),
			hasTask,
			taskState,
			taskOwner,
			taskFor,
			taskLeaseUntil,
			marshalMsg(msg),
			Number(msg.id),
		);
		this.reindexTerms(msg);
	}

	private syncTaskLabel(msg: BoardMessage): void {
		if (!msg.deleted && msg.task) {
			msg.labels[TASK_LABEL] = taskStateName(msg.task.state);
		}
	}

	private canEditMessage(msg: BoardMessage, identity: BoardIdentity): boolean {
		if (identity.isAdmin && !identity.author.profile && !identity.author.sessionId) return true;
		const by = msg.by;
		if (!by) return false;
		if (by.actor === identity.author.actor) return true;
		// Principal acting without a profile can edit/delete any of its profiles' messages.
		if (!identity.author.profile && by.principal === identity.author.principal) return true;
		return false;
	}

	private canOwnTask(task: BoardTask, identity: BoardIdentity): boolean {
		if (identity.isAdmin && !identity.author.profile && !identity.author.sessionId) return true;
		if (task.by === identity.author.actor) return true;
		if (!identity.author.profile && task.owner === identity.author.principal) return true;
		return false;
	}

	// ---- Message Writes ----

	postMessage(opts: {
		project: string;
		re: number;
		text: string;
		labels: Record<string, string>;
		attachmentIds: string[];
		idempotencyKey: string;
		afterId: number;
		identity: BoardIdentity;
		nowMs: number;
		schedule?: { id: number; n: number };
	}): { message: BoardMessage; mentions: BoardMention[]; outcome: string; project: string } {
		const idempKey = opts.idempotencyKey.trim();
		if (idempKey.length > 200) {
			throw hotbedError(ErrorCode.InvalidArgs, "idempotency_key is at most 200 characters");
		}
		const cleanLabels = validateLabels(opts.labels, false);

		return this.tx(() => {
			this.upsertActor(opts.identity.actor, opts.nowMs);
			const attachments = this.resolveAttachments(opts.attachmentIds);
			validateMessageText(opts.text, attachments.length > 0);
			const parsedBlocks = this.validateAndParseBlocks(opts.text);

			let projectId: number;
			let projectName: string;
			let rootId: number | null = null;
			let replyToId: number | null = null;

			if (opts.re > 0) {
				const target = this.getMessageRow(opts.re);
				projectId = target.projectId;
				projectName = target.msg.project;
				rootId = target.re ?? target.id;
				replyToId = target.re !== null ? target.id : null;
				if (opts.afterId > 0) {
					const latestRows = this.sql
						.exec(
							"SELECT MAX(id) AS newest FROM board_messages WHERE id = ? OR re = ?",
							rootId,
							rootId,
						)
						.toArray();
					const newest = num(latestRows[0]?.newest);
					if (newest > opts.afterId) {
						throw hotbedError(
							ErrorCode.Conflict,
							`newer_message: #${newest} came after #${opts.afterId}`,
						).withSuggestion(`hotbed board read ${rootId} --after ${opts.afterId}`);
					}
				}
			} else {
				if (opts.afterId > 0) {
					throw hotbedError(ErrorCode.InvalidArgs, "--after is only valid on replies");
				}
				projectName = opts.project;
				projectId = this.projects.idByName(projectName);
			}

			// Scoped idempotency key per principal
			const scopedKey = idempKey ? `${opts.identity.author.principal}:${idempKey}` : "";
			if (scopedKey) {
				const existingRows = this.sql
					.exec("SELECT doc FROM board_messages WHERE idempotency_key = ?", scopedKey)
					.toArray();
				if (existingRows.length > 0) {
					const existing = unmarshalMsg(str(existingRows[0].doc));
					if (
						existing.project === projectName &&
						Number(existing.re) === (rootId ?? 0) &&
						existing.text === opts.text
					) {
						return { message: existing, mentions: [], outcome: "posted", project: projectName };
					}
					throw hotbedError(
						ErrorCode.Conflict,
						`duplicate_id: idempotency key ${JSON.stringify(idempKey)} was already used for #${existing.id}`,
					);
				}
			}

			const { text: settledText, mentions } = this.settleTextMentions(opts.text, rootId);
			const blocks = parsedBlocks.map((b) =>
				create(BoardBlockSchema, { schema: b.schema, version: b.version, dataJson: b.dataJson }),
			);

			const msg = create(BoardMessageSchema, {
				project: projectName,
				text: settledText,
				re: BigInt(rootId ?? 0),
				replyTo: BigInt(replyToId ?? 0),
				labels: cleanLabels,
				by: opts.identity.author,
				author: opts.identity.actor,
				createdAt: BigInt(opts.nowMs),
				attachments,
				blocks,
				scheduleId: BigInt(opts.schedule?.id ?? 0),
				scheduleN: opts.schedule?.n ?? 0,
				revision: 1n,
			});

			const ins = this.sql
				.exec(
					`INSERT INTO board_messages
					 (project_id, idempotency_key, re, reply_to, by_actor, by_principal, by_profile,
					  text_lower, deleted, created_at, replies_count, last_activity_id, last_activity_at,
					  version, revision, has_task, doc)
					 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, 0, 0, ?, 0, 1, 0, ?)
					 RETURNING id`,
					projectId,
					scopedKey || null,
					rootId,
					replyToId,
					cleanSql(opts.identity.author.actor),
					cleanSql(opts.identity.author.principal),
					opts.identity.author.profile ? cleanSql(opts.identity.author.profile) : null,
					cleanSql(settledText.toLowerCase()),
					opts.nowMs,
					opts.nowMs,
					marshalMsg(msg),
				)
				.toArray();
			const id = num(ins[0].id);
			msg.id = BigInt(id);
			const threadId = rootId ?? id;
			const seq = this.appendEvent({
				at: opts.nowMs,
				projectId,
				projectName,
				type: "message.posted",
				messageId: id,
				threadId,
				actor: opts.identity.actor,
				afterMsg: msg,
				payload: { re: rootId, replyTo: replyToId },
			});
			msg.version = BigInt(seq);
			msg.revision = BigInt(seq);

			if (rootId === null) {
				this.sql.exec(
					"UPDATE board_messages SET last_activity_id = ?, last_activity_at = ?, version = ?, revision = ?, doc = ? WHERE id = ?",
					id,
					opts.nowMs,
					seq,
					seq,
					marshalMsg(msg),
					id,
				);
			} else {
				this.sql.exec(
					"UPDATE board_messages SET version = ?, revision = ?, doc = ? WHERE id = ?",
					seq,
					seq,
					marshalMsg(msg),
					id,
				);
				this.sql.exec(
					"UPDATE board_messages SET replies_count = replies_count + 1, last_activity_id = ?, last_activity_at = ? WHERE id = ?",
					id,
					opts.nowMs,
					rootId,
				);
			}
			this.reindexTerms(msg);
			return { message: msg, mentions, outcome: "posted", project: projectName };
		});
	}

	editMessage(opts: {
		id: number;
		text: string;
		ifRevision: bigint;
		identity: BoardIdentity;
		nowMs: number;
	}): { message: BoardMessage; mentions: BoardMention[]; outcome: string; project: string } {
		return this.tx(() => {
			this.upsertActor(opts.identity.actor, opts.nowMs);
			const row = this.getMessageRow(opts.id);
			const msg = row.msg;
			if (msg.deleted) {
				throw hotbedError(ErrorCode.Conflict, `message #${opts.id} was deleted`);
			}
			if (!this.canEditMessage(msg, opts.identity)) {
				throw hotbedError(ErrorCode.NotAuthorized, `only #${opts.id}'s author can edit it`);
			}
			if (opts.ifRevision > 0n && msg.revision !== opts.ifRevision) {
				throw hotbedError(
					ErrorCode.Conflict,
					`revision_conflict: #${opts.id} was edited since revision ${opts.ifRevision} (now at ${msg.revision})`,
				).withSuggestion(`hotbed board read ${opts.id}`);
			}
			validateMessageText(opts.text, msg.attachments.length > 0);
			const parsedBlocks = this.validateAndParseBlocks(opts.text);
			const before = clone(BoardMessageSchema, msg);
			const rootId = row.re ?? row.id;
			const { text: settledText, mentions } = this.settleTextMentions(opts.text, rootId);
			msg.text = settledText;
			msg.editedAt = BigInt(opts.nowMs);
			msg.blocks = parsedBlocks.map((b) =>
				create(BoardBlockSchema, { schema: b.schema, version: b.version, dataJson: b.dataJson }),
			);

			const seq = this.appendEvent({
				at: opts.nowMs,
				projectId: row.projectId,
				projectName: msg.project,
				type: "message.edited",
				messageId: row.id,
				threadId: rootId,
				actor: opts.identity.actor,
				beforeMsg: before,
				afterMsg: msg,
			});
			msg.version = BigInt(seq);
			msg.revision = BigInt(seq);

			// If someone other than the task owner edited a task's root, hold the task with kind "edited".
			if (
				msg.task &&
				!isFinished(msg.task) &&
				msg.task.state !== BoardTaskState.BLOCKED &&
				!this.canOwnTask(msg.task, opts.identity)
			) {
				const beforeBlock = clone(BoardMessageSchema, msg);
				msg.task.state = BoardTaskState.BLOCKED;
				msg.task.claim = undefined;
				msg.task.blocked = create(BoardTaskBlockedSchema, {
					kind: EDITED_HOLD,
					reason: `edited by ${opts.identity.actor.handle}`,
				});
				this.syncTaskLabel(msg);
				const bseq = this.appendEvent({
					at: opts.nowMs,
					projectId: row.projectId,
					projectName: msg.project,
					type: "task.blocked",
					messageId: row.id,
					threadId: rootId,
					actor: opts.identity.actor,
					beforeMsg: beforeBlock,
					afterMsg: msg,
					payload: { kind: EDITED_HOLD, reason: msg.task.blocked.reason },
				});
				msg.version = BigInt(bseq);
			}

			this.persistMessage(msg);
			return { message: msg, mentions, outcome: "edited", project: msg.project };
		});
	}

	deleteMessage(opts: { id: number; ifRevision: bigint; identity: BoardIdentity; nowMs: number }): {
		message: BoardMessage;
		project: string;
	} {
		return this.tx(() => {
			this.upsertActor(opts.identity.actor, opts.nowMs);
			const row = this.getMessageRow(opts.id);
			const msg = row.msg;
			if (!this.canEditMessage(msg, opts.identity)) {
				throw hotbedError(ErrorCode.NotAuthorized, `only #${opts.id}'s author can delete it`);
			}
			if (opts.ifRevision > 0n && msg.revision !== opts.ifRevision) {
				throw hotbedError(
					ErrorCode.Conflict,
					`revision_conflict: #${opts.id} is at revision ${msg.revision}, not ${opts.ifRevision}`,
				).withSuggestion(`hotbed board read ${opts.id}`);
			}
			if (msg.deleted) return { message: msg, project: msg.project };
			const before = clone(BoardMessageSchema, msg);
			const rootId = row.re ?? row.id;

			// Deleting a task root cancels it unless already completed/canceled.
			if (
				msg.task &&
				msg.task.state !== BoardTaskState.COMPLETED &&
				msg.task.state !== BoardTaskState.CANCELED
			) {
				msg.task.state = BoardTaskState.CANCELED;
				msg.task.claim = undefined;
				msg.task.blocked = undefined;
				msg.task.reason = "deleted";
				this.appendEvent({
					at: opts.nowMs,
					projectId: row.projectId,
					projectName: msg.project,
					type: "task.canceled",
					messageId: row.id,
					threadId: rootId,
					actor: opts.identity.actor,
					beforeMsg: before,
					afterMsg: msg,
					payload: { reason: "deleted" },
				});
			}

			msg.deleted = true;
			msg.text = "";
			msg.labels = {};
			msg.reactions = [];
			msg.attachments = [];
			msg.blocks = [];

			const seq = this.appendEvent({
				at: opts.nowMs,
				projectId: row.projectId,
				projectName: msg.project,
				type: "message.deleted",
				messageId: row.id,
				threadId: rootId,
				actor: opts.identity.actor,
				beforeMsg: before,
				afterMsg: msg,
			});
			msg.version = BigInt(seq);
			msg.revision = BigInt(seq);
			this.persistMessage(msg);

			if (row.re !== null) {
				const stats = this.sql
					.exec(
						`SELECT COUNT(*) AS cnt, MAX(id) AS last_id, MAX(created_at) AS last_at
						 FROM board_messages WHERE re = ? AND deleted = 0`,
						row.re,
					)
					.toArray()[0];
				const cnt = num(stats?.cnt);
				const rootRow = this.getMessageRow(row.re);
				const lastId = cnt > 0 ? num(stats?.last_id) : row.re;
				const lastAt = cnt > 0 ? num(stats?.last_at) : Number(rootRow.msg.createdAt);
				this.sql.exec(
					"UPDATE board_messages SET replies_count = ?, last_activity_id = ?, last_activity_at = ? WHERE id = ?",
					cnt,
					lastId,
					lastAt,
					row.re,
				);
			}
			return { message: msg, project: msg.project };
		});
	}

	reactMessage(opts: {
		id: number;
		emoji: string;
		remove: boolean;
		identity: BoardIdentity;
		nowMs: number;
	}): { message: BoardMessage; project: string } {
		const norm = normalizeEmoji(opts.emoji);
		if (!norm) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`unknown emoji ${JSON.stringify(opts.emoji)} (use a short name like eyes or thumbs_up, or an emoji character)`,
			);
		}
		return this.tx(() => {
			this.upsertActor(opts.identity.actor, opts.nowMs);
			const row = this.getMessageRow(opts.id);
			const msg = row.msg;
			if (msg.deleted) {
				throw hotbedError(ErrorCode.Conflict, `message #${opts.id} was deleted`);
			}
			const before = clone(BoardMessageSchema, msg);
			const actorId = opts.identity.actor.id;
			const idx = msg.reactions.findIndex((r) => canonicalEmoji(r.emoji) === norm);
			let changed = false;
			if (opts.remove) {
				if (idx >= 0) {
					const filtered = msg.reactions[idx].actors.filter((a) => a !== actorId);
					if (filtered.length !== msg.reactions[idx].actors.length) {
						changed = true;
						if (filtered.length > 0) msg.reactions[idx].actors = filtered;
						else msg.reactions.splice(idx, 1);
					}
				}
			} else {
				if (idx >= 0) {
					if (!msg.reactions[idx].actors.includes(actorId)) {
						msg.reactions[idx].actors.push(actorId);
						changed = true;
					}
				} else {
					msg.reactions.push(create(BoardReactionSchema, { emoji: norm, actors: [actorId] }));
					changed = true;
				}
			}
			if (!changed) return { message: msg, project: msg.project };
			const rootId = row.re ?? row.id;
			const seq = this.appendEvent({
				at: opts.nowMs,
				projectId: row.projectId,
				projectName: msg.project,
				type: opts.remove ? "reaction.removed" : "reaction.added",
				messageId: row.id,
				threadId: rootId,
				actor: opts.identity.actor,
				beforeMsg: before,
				afterMsg: msg,
				payload: { emoji: norm },
			});
			msg.version = BigInt(seq);
			this.persistMessage(msg);
			return { message: msg, project: msg.project };
		});
	}

	labelMessage(opts: {
		id: number;
		set: Record<string, string>;
		unset: string[];
		ifEquals: Record<string, string>;
		ifUnset: string[];
		identity: BoardIdentity;
		nowMs: number;
	}): { message: BoardMessage; project: string } {
		const setClean = validateLabels(opts.set, false);
		for (const k of opts.unset) validateLabelKey(k, false);
		if (Object.keys(setClean).length === 0 && opts.unset.length === 0) {
			throw hotbedError(ErrorCode.InvalidArgs, "give at least one label to set or unset");
		}
		const condCount = Object.keys(opts.ifEquals).length + opts.ifUnset.length;
		if (condCount > 16) {
			throw hotbedError(ErrorCode.InvalidArgs, "at most 16 label conditions per write");
		}
		return this.tx(() => {
			this.upsertActor(opts.identity.actor, opts.nowMs);
			const row = this.getMessageRow(opts.id);
			const msg = row.msg;
			if (msg.deleted) {
				throw hotbedError(ErrorCode.Conflict, `message #${opts.id} was deleted`);
			}
			for (const [k, expected] of Object.entries(opts.ifEquals)) {
				const actual = msg.labels[k];
				if (actual !== expected) {
					throw hotbedError(
						ErrorCode.Conflict,
						`label_conflict: ${k} is ${actual === undefined ? "unset" : JSON.stringify(actual)}, expected ${JSON.stringify(expected)}`,
					).withSuggestion(`hotbed board read ${opts.id}`);
				}
			}
			for (const k of opts.ifUnset) {
				if (Object.hasOwn(msg.labels, k)) {
					throw hotbedError(
						ErrorCode.Conflict,
						`label_conflict: ${k} is already set to ${JSON.stringify(msg.labels[k])}`,
					).withSuggestion(`hotbed board read ${opts.id}`);
				}
			}
			const before = clone(BoardMessageSchema, msg);
			for (const [k, v] of Object.entries(setClean)) {
				msg.labels[k] = v;
			}
			for (const k of opts.unset) {
				delete msg.labels[k];
			}
			const rootId = row.re ?? row.id;
			const seq = this.appendEvent({
				at: opts.nowMs,
				projectId: row.projectId,
				projectName: msg.project,
				type: "labels.changed",
				messageId: row.id,
				threadId: rootId,
				actor: opts.identity.actor,
				beforeMsg: before,
				afterMsg: msg,
				payload: { set: setClean, unset: opts.unset },
			});
			msg.version = BigInt(seq);
			this.persistMessage(msg);
			return { message: msg, project: msg.project };
		});
	}

	// ---- Reads & Filtering ----

	listThreads(opts: ThreadFilterOpts): {
		threads: BoardThreadSummary[];
		cursor: bigint;
		older: string;
	} {
		const limit = Math.min(Math.max(opts.limit || 50, 1), 500);
		if (opts.after > 0 && opts.before) {
			throw hotbedError(ErrorCode.InvalidArgs, "cannot combine --after and --before");
		}
		const where: string[] = ["r.re IS NULL"];
		const vals: unknown[] = [];

		if (opts.projectId !== null) {
			where.push("r.project_id = ?");
			vals.push(opts.projectId);
		}

		// Message-level filters: match thread if ANY message in thread matches.
		const msgWhere: string[] = [];
		const msgVals: unknown[] = [];
		if (opts.by.trim()) {
			const by = this.resolveByTarget(opts.by);
			const parts: string[] = [];
			if (by.actors.length > 0) {
				parts.push(`m.by_actor IN (${by.actors.map(() => "?").join(",")})`);
				msgVals.push(...by.actors);
			}
			if (by.principals.length > 0) {
				parts.push(`m.by_principal IN (${by.principals.map(() => "?").join(",")})`);
				msgVals.push(...by.principals);
			}
			msgWhere.push(parts.length > 0 ? `(${parts.join(" OR ")})` : "1 = 0");
		}
		if (opts.mentions.trim()) {
			const m = this.resolveMentionTarget(opts.mentions, opts.callerActorId, opts.callerHandle);
			const parts: string[] = [];
			if (m.terms.length > 0) {
				parts.push(`t.value IN (${m.terms.map(() => "?").join(",")})`);
				msgVals.push(...m.terms);
			}
			for (const p of m.prefixes) {
				parts.push("substr(t.value, 1, length(?)) = ?");
				msgVals.push(p, p);
			}
			msgWhere.push(
				parts.length > 0
					? `EXISTS (SELECT 1 FROM board_terms t WHERE t.message_id = m.id AND t.term = '@' AND (${parts.join(" OR ")}))`
					: "1 = 0",
			);
		}
		if (opts.text.trim()) {
			msgWhere.push("m.text_lower LIKE ? ESCAPE '\\'");
			msgVals.push(`%${likeEscape(opts.text.trim().toLowerCase())}%`);
		}
		for (const lf of opts.labels) {
			if (lf.values.length === 0) {
				msgWhere.push(
					"EXISTS (SELECT 1 FROM board_terms t WHERE (t.message_id = m.id OR t.message_id = m.re) AND t.term = ?)",
				);
				msgVals.push(lf.key);
			} else {
				msgWhere.push(
					`EXISTS (SELECT 1 FROM board_terms t WHERE (t.message_id = m.id OR t.message_id = m.re) AND t.term = ? AND t.value IN (${lf.values.map(() => "?").join(",")}))`,
				);
				msgVals.push(lf.key, ...lf.values);
			}
		}

		if (msgWhere.length > 0) {
			where.push(
				`r.id IN (SELECT COALESCE(m.re, m.id) FROM board_messages m WHERE ${msgWhere.join(" AND ")})`,
			);
			vals.push(...msgVals);
		}

		if (opts.after > 0) {
			where.push("r.last_activity_id > ?");
			vals.push(opts.after);
		}
		if (opts.before) {
			const parts = opts.before.split(".");
			if (parts.length !== 2 || !/^\d+$/.test(parts[0]) || !/^\d+$/.test(parts[1])) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`invalid --before cursor ${JSON.stringify(opts.before)}`,
				);
			}
			const bAt = Number(parts[0]);
			const bId = Number(parts[1]);
			where.push("(r.last_activity_at < ? OR (r.last_activity_at = ? AND r.id < ?))");
			vals.push(bAt, bAt, bId);
		}

		const forward = opts.after > 0;
		const order = forward ? "ASC" : "DESC";
		const fetchLimit = forward ? limit : limit + 1;
		vals.push(fetchLimit);

		const rows = this.sql
			.exec(
				`SELECT r.id, r.doc, r.replies_count, r.last_activity_at, l.doc AS last_doc
				 FROM board_messages r
				 LEFT JOIN board_messages l ON l.id = r.last_activity_id AND r.last_activity_id != r.id
				 WHERE ${where.join(" AND ")}
				 ORDER BY r.last_activity_at ${order}, r.id ${order}
				 LIMIT ?`,
				...vals,
			)
			.toArray();

		const cursor = BigInt(this.latestEventCursor());
		let older = "";
		let shown = rows;
		if (!forward) {
			if (rows.length > limit) {
				shown = rows.slice(0, limit);
				const oldest = shown[shown.length - 1];
				older = `${num(oldest.last_activity_at)}.${num(oldest.id)}`;
			}
			shown = [...shown].reverse();
		}

		const threads = shown.map((r) =>
			create(BoardThreadSummarySchema, {
				message: unmarshalMsg(str(r.doc)),
				replies: num(r.replies_count),
				last: r.last_doc ? unmarshalMsg(str(r.last_doc)) : undefined,
			}),
		);
		return { threads, cursor, older };
	}

	getThread(opts: { id: number; afterId: number; labels: BoardLabelFilter[]; limit: number }): {
		message: BoardMessage;
		replies: BoardMessage[];
		schedules: BoardSchedule[];
		cursor: bigint;
	} {
		const target = this.getMessageRow(opts.id);
		const rootId = target.re ?? target.id;
		const root = target.re === null ? target.msg : this.getMessageRow(rootId).msg;

		const where: string[] = ["re = ?"];
		const vals: unknown[] = [rootId];
		if (opts.afterId > 0) {
			where.push("id > ?");
			vals.push(opts.afterId);
		}
		for (const lf of opts.labels) {
			if (lf.values.length === 0) {
				where.push(
					"EXISTS (SELECT 1 FROM board_terms t WHERE (t.message_id = id OR t.message_id = re) AND t.term = ?)",
				);
				vals.push(lf.key);
			} else {
				where.push(
					`EXISTS (SELECT 1 FROM board_terms t WHERE (t.message_id = id OR t.message_id = re) AND t.term = ? AND t.value IN (${lf.values.map(() => "?").join(",")}))`,
				);
				vals.push(lf.key, ...lf.values);
			}
		}

		let replyRows: Record<string, unknown>[];
		if (opts.limit > 0) {
			const lim = Math.min(opts.limit, 500);
			if (opts.afterId > 0) {
				replyRows = this.sql
					.exec(
						`SELECT doc FROM board_messages WHERE ${where.join(" AND ")} ORDER BY id ASC LIMIT ?`,
						...vals,
						lim,
					)
					.toArray();
			} else {
				replyRows = this.sql
					.exec(
						`SELECT doc FROM board_messages WHERE ${where.join(" AND ")} ORDER BY id DESC LIMIT ?`,
						...vals,
						lim,
					)
					.toArray()
					.reverse();
			}
		} else {
			replyRows = this.sql
				.exec(
					`SELECT doc FROM board_messages WHERE ${where.join(" AND ")} ORDER BY id ASC`,
					...vals,
				)
				.toArray();
		}

		const schedRows = this.sql
			.exec(
				"SELECT doc FROM board_schedules WHERE re = ? AND state = 'pending' ORDER BY next_at ASC, id ASC",
				rootId,
			)
			.toArray();

		return {
			message: root,
			replies: replyRows.map((r) => unmarshalMsg(str(r.doc))),
			schedules: schedRows.map((r) => unmarshalSchedule(str(r.doc))),
			cursor: BigInt(this.latestEventCursor()),
		};
	}

	private messageMatchesFilters(
		msg: BoardMessage | null,
		rootMsg: BoardMessage | null,
		opts: {
			labels: BoardLabelFilter[];
			byResolved: { actors: string[]; principals: string[] } | null;
			mentionResolved: { terms: string[]; prefixes: string[] } | null;
			textLower: string;
		},
	): boolean {
		if (!msg) return false;
		if (opts.byResolved) {
			const actor = msg.by?.actor ?? "";
			const principal = msg.by?.principal ?? "";
			if (
				!opts.byResolved.actors.includes(actor) &&
				!opts.byResolved.principals.includes(principal)
			) {
				return false;
			}
		}
		if (opts.textLower && !msg.text.toLowerCase().includes(opts.textLower)) {
			return false;
		}
		if (opts.mentionResolved) {
			const terms = mentionTerms(msg.text);
			const hit = terms.some(
				(t) =>
					opts.mentionResolved!.terms.includes(t) ||
					opts.mentionResolved!.prefixes.some((p) => t.startsWith(p)),
			);
			if (!hit) return false;
		}
		for (const lf of opts.labels) {
			const val = msg.labels[lf.key] ?? rootMsg?.labels[lf.key];
			if (val === undefined) return false;
			if (lf.values.length > 0 && !lf.values.includes(val)) return false;
		}
		return true;
	}

	listEvents(opts: EventFilterOpts): { events: BoardEvent[]; cursor: bigint } {
		const limit = Math.min(Math.max(opts.limit || 100, 1), 500);
		const where: string[] = [];
		const vals: unknown[] = [];

		if (opts.projectId !== null) {
			where.push("project_id = ?");
			vals.push(opts.projectId);
		}
		if (opts.re > 0) {
			const target = this.getMessageRow(opts.re);
			const rootId = target.re ?? target.id;
			where.push("thread_id = ?");
			vals.push(rootId);
		}

		const byResolved = opts.by.trim() ? this.resolveByTarget(opts.by) : null;
		const mentionResolved = opts.mentions.trim()
			? this.resolveMentionTarget(opts.mentions, opts.callerActorId, opts.callerHandle)
			: null;
		const textLower = opts.text.trim().toLowerCase();
		const hasMsgFilters =
			opts.labels.length > 0 || byResolved !== null || mentionResolved !== null || textLower !== "";

		let rows: Record<string, unknown>[];
		if (opts.after > 0) {
			where.push("seq > ?");
			vals.push(opts.after);
			rows = this.sql
				.exec(
					`SELECT seq, type, thread_id, before_doc, after_doc, doc FROM board_events
					 ${where.length > 0 ? `WHERE ${where.join(" AND ")}` : ""}
					 ORDER BY seq ASC LIMIT ?`,
					...vals,
					Math.min(limit * 4, 1000),
				)
				.toArray();
		} else if (opts.last > 0) {
			const lastN = Math.min(opts.last, 100);
			rows = this.sql
				.exec(
					`SELECT seq, type, thread_id, before_doc, after_doc, doc FROM board_events
					 ${where.length > 0 ? `WHERE ${where.join(" AND ")}` : ""}
					 ORDER BY seq DESC LIMIT ?`,
					...vals,
					Math.min(lastN * 4, 1000),
				)
				.toArray()
				.reverse();
		} else {
			return { events: [], cursor: BigInt(this.latestEventCursor()) };
		}

		const rootCache = new Map<number, BoardMessage | null>();
		const getRoot = (threadId: number): BoardMessage | null => {
			if (rootCache.has(threadId)) return rootCache.get(threadId) ?? null;
			const found = this.sql
				.exec("SELECT doc FROM board_messages WHERE id = ?", threadId)
				.toArray();
			const m = found.length > 0 ? unmarshalMsg(str(found[0].doc)) : null;
			rootCache.set(threadId, m);
			return m;
		};

		const matched: BoardEvent[] = [];
		let lastSeenSeq = opts.after;
		for (const r of rows) {
			const seq = num(r.seq);
			lastSeenSeq = seq;
			const type = str(r.type);
			if (!opts.includeAll && BOOKKEEPING_EVENTS.has(type)) continue;
			if (hasMsgFilters) {
				const beforeMsg = r.before_doc ? unmarshalMsg(str(r.before_doc)) : null;
				const afterMsg = r.after_doc ? unmarshalMsg(str(r.after_doc)) : null;
				const threadId = num(r.thread_id);
				const rootMsg =
					(afterMsg && afterMsg.re === 0n ? afterMsg : null) ??
					(threadId > 0 ? getRoot(threadId) : null);
				const filterObj = { labels: opts.labels, byResolved, mentionResolved, textLower };
				const hit =
					this.messageMatchesFilters(beforeMsg, rootMsg, filterObj) ||
					this.messageMatchesFilters(afterMsg, rootMsg, filterObj);
				if (!hit) continue;
			}
			matched.push(unmarshalEvent(str(r.doc)));
			if (opts.after > 0 && matched.length >= limit) break;
		}

		const result =
			opts.last > 0 && opts.after === 0 ? matched.slice(-Math.min(opts.last, 100)) : matched;
		const cursor =
			opts.after > 0 && matched.length >= limit
				? BigInt(lastSeenSeq)
				: BigInt(Math.max(lastSeenSeq, this.latestEventCursor()));
		return { events: result, cursor };
	}

	// ---- Tasks ----

	private resolveAssignee(owner: string, rawFor: string): string {
		const trimmed = rawFor.trim();
		if (!trimmed) return "";
		if (trimmed.includes(":")) {
			if (!trimmed.startsWith(`${owner}+`)) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`for_actor must be one of the owner's profiles (${owner}+<profile>) or a bare profile name`,
				);
			}
			validateProfileName(trimmed.slice(owner.length + 1));
			return `${owner}+${trimmed.slice(owner.length + 1).toLowerCase()}`;
		}
		const prof = validateProfileName(trimmed);
		return `${owner}+${prof}`;
	}

	private validateNeeds(
		needIds: number[],
		selfId: number | null,
	): { ids: number[]; allCompleted: boolean } {
		const unique = [...new Set(needIds)];
		if (unique.length > TASK_NEEDS_MAX) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`invalid_needs: a task waits for at most ${TASK_NEEDS_MAX} tasks`,
			);
		}
		let allCompleted = true;
		for (const nid of unique) {
			if (nid <= 0) {
				throw hotbedError(ErrorCode.InvalidArgs, `invalid_needs: #${nid} isn't a task`);
			}
			if (selfId !== null && nid === selfId) {
				throw hotbedError(ErrorCode.InvalidArgs, `invalid_needs: #${nid} cannot wait for itself`);
			}
			const rows = this.sql.exec("SELECT doc FROM board_messages WHERE id = ?", nid).toArray();
			if (rows.length === 0) {
				throw hotbedError(ErrorCode.InvalidArgs, `invalid_needs: #${nid} isn't a task`);
			}
			const m = unmarshalMsg(str(rows[0].doc));
			if (!m.task || m.deleted) {
				throw hotbedError(ErrorCode.InvalidArgs, `invalid_needs: #${nid} isn't a task`);
			}
			if (m.task.state === BoardTaskState.CANCELED) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`invalid_needs: #${nid} was canceled and can never complete`,
				);
			}
			if (m.task.state !== BoardTaskState.COMPLETED) {
				allCompleted = false;
			}
		}
		return { ids: unique, allCompleted };
	}

	private checkNoCycle(taskId: number, addedNeeds: number[]): void {
		const from = new Map<number, number>(addedNeeds.map((n) => [n, n]));
		let level = [...addedNeeds];
		while (level.length > 0) {
			const next: number[] = [];
			for (const at of level) {
				const rows = this.sql.exec("SELECT doc FROM board_messages WHERE id = ?", at).toArray();
				if (rows.length === 0) continue;
				const m = unmarshalMsg(str(rows[0].doc));
				if (!m.task || m.task.state === BoardTaskState.COMPLETED) continue;
				for (const depBig of m.task.needs) {
					const dep = Number(depBig);
					if (dep === taskId) {
						throw hotbedError(
							ErrorCode.InvalidArgs,
							`invalid_needs: #${from.get(at)} already waits for #${taskId} (cycle)`,
						);
					}
					if (from.has(dep)) continue;
					from.set(dep, from.get(at)!);
					next.push(dep);
				}
			}
			if (from.size > NEEDS_WALK_MAX) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`invalid_needs: dependency graph exceeds ${NEEDS_WALK_MAX} tasks to verify acyclicity`,
				);
			}
			level = next;
		}
	}

	private allNeedsCompleted(needs: bigint[], completingId?: number): boolean {
		for (const nidBig of needs) {
			const nid = Number(nidBig);
			if (nid === completingId) continue;
			const rows = this.sql
				.exec("SELECT task_state FROM board_messages WHERE id = ?", nid)
				.toArray();
			if (rows.length === 0 || str(rows[0].task_state) !== "completed") return false;
		}
		return true;
	}

	private readyDependents(completedId: number, nowMs: number): string[] {
		const depRows = this.sql
			.exec(
				`SELECT m.id, m.project_id, m.doc FROM board_messages m
				 JOIN board_terms t ON t.message_id = m.id
				 WHERE t.term = ? AND t.value = ? AND m.task_state = 'waiting'`,
				NEEDS_TERM,
				String(completedId),
			)
			.toArray();
		const wokenProjects: string[] = [];
		for (const r of depRows) {
			const depMsg = unmarshalMsg(str(r.doc));
			if (!depMsg.task || depMsg.task.state !== BoardTaskState.WAITING) continue;
			if (!this.allNeedsCompleted(depMsg.task.needs, completedId)) continue;
			const before = clone(BoardMessageSchema, depMsg);
			depMsg.task.state = BoardTaskState.READY;
			this.syncTaskLabel(depMsg);
			const seq = this.appendEvent({
				at: nowMs,
				projectId: num(r.project_id),
				projectName: depMsg.project,
				type: "task.ready",
				messageId: num(r.id),
				threadId: num(r.id),
				beforeMsg: before,
				afterMsg: depMsg,
			});
			depMsg.version = BigInt(seq);
			this.persistMessage(depMsg);
			wokenProjects.push(depMsg.project);
		}
		return wokenProjects;
	}

	private applyClaim(
		row: { id: number; projectId: number; msg: BoardMessage },
		identity: BoardIdentity,
		leaseSeconds: number,
		nowMs: number,
	): BoardTaskClaim {
		const msg = row.msg;
		const task = msg.task!;
		const leaseS = normalizeLeaseSeconds(leaseSeconds);
		const untilMs = nowMs + leaseS * 1000;

		// If the previous claim lapsed, emit task.expired first.
		if (isLapsed(task, nowMs)) {
			const beforeExp = clone(BoardMessageSchema, msg);
			const oldEpoch = task.epoch;
			task.state = BoardTaskState.READY;
			task.claim = undefined;
			this.syncTaskLabel(msg);
			const eseq = this.appendEvent({
				at: nowMs,
				projectId: row.projectId,
				projectName: msg.project,
				type: "task.expired",
				messageId: row.id,
				threadId: row.id,
				beforeMsg: beforeExp,
				afterMsg: msg,
				payload: { epoch: oldEpoch },
			});
			msg.version = BigInt(eseq);
		}

		const beforeClaim = clone(BoardMessageSchema, msg);
		const epoch = task.epoch + 1;
		task.state = BoardTaskState.CLAIMED;
		task.epoch = epoch;
		task.claim = create(BoardTaskClaimViewSchema, {
			actor: identity.actor.id,
			until: BigInt(untilMs),
			leaseSeconds: leaseS,
		});
		this.syncTaskLabel(msg);

		const seq = this.appendEvent({
			at: nowMs,
			projectId: row.projectId,
			projectName: msg.project,
			type: "task.claimed",
			messageId: row.id,
			threadId: row.id,
			actor: identity.actor,
			beforeMsg: beforeClaim,
			afterMsg: msg,
			payload: { epoch, until: untilMs },
		});
		msg.version = BigInt(seq);
		this.persistMessage(msg);
		return create(BoardTaskClaimSchema, { epoch, until: BigInt(untilMs) });
	}

	// A claim belongs to the actor (principal + profile) that made it, so the caller's
	// identity is all the proof it needs: the task must be claimed by this very actor on a
	// live lease.
	private isHeldBy(task: BoardTask, identity: BoardIdentity, nowMs: number): boolean {
		return (
			task.state === BoardTaskState.CLAIMED &&
			task.claim?.actor === identity.actor.id &&
			!isLapsed(task, nowMs)
		);
	}

	// epoch, when nonzero, also names the claim the caller means: actors are shared (one
	// profile on several hosts, subagents of one session), so a worker whose claim lapsed
	// and was re-claimed by another under the same identity is refused.
	private requireHeld(
		id: number,
		task: BoardTask,
		identity: BoardIdentity,
		epoch: number,
		nowMs: number,
	): BoardTaskClaimView {
		if (this.isHeldBy(task, identity, nowMs)) {
			if (epoch && epoch !== task.epoch) {
				throw hotbedError(
					ErrorCode.Conflict,
					`claim_lost: claim ${epoch} on #${id} is over: #${id} was claimed again (claim ${task.epoch})`,
				).withSuggestion(`hotbed board read ${id}`);
			}
			return task.claim!;
		}
		const live = task.state === BoardTaskState.CLAIMED && !isLapsed(task, nowMs);
		const why = live
			? `it is claimed by ${task.claim?.actor ?? "someone"}`
			: `it is ${taskStateName(effectiveState(task, nowMs))}`;
		throw hotbedError(
			ErrorCode.Conflict,
			`claim_lost: ${identity.actor.id} does not hold #${id}: ${why}`,
		).withSuggestion(
			live
				? `act as its holder (--as <profile>), or hotbed board read ${id}`
				: `claim it first: hotbed board task claim ${id}`,
		);
	}

	// Gives the holder a fresh lease of leaseSeconds (0 keeps the claim's own length).
	private renewHeld(
		row: { id: number; projectId: number; msg: BoardMessage },
		identity: BoardIdentity,
		epoch: number,
		leaseSeconds: number,
		nowMs: number,
	): BoardTaskClaim {
		const msg = row.msg;
		const task = msg.task!;
		const held = this.requireHeld(row.id, task, identity, epoch, nowMs);
		const leaseS = normalizeLeaseSeconds(leaseSeconds > 0 ? leaseSeconds : held.leaseSeconds);
		const untilMs = nowMs + leaseS * 1000;
		const before = clone(BoardMessageSchema, msg);
		task.claim = create(BoardTaskClaimViewSchema, {
			actor: held.actor,
			until: BigInt(untilMs),
			leaseSeconds: leaseS,
		});
		const seq = this.appendEvent({
			at: nowMs,
			projectId: row.projectId,
			projectName: msg.project,
			type: "task.renewed",
			messageId: row.id,
			threadId: row.id,
			actor: identity.actor,
			beforeMsg: before,
			afterMsg: msg,
			payload: { epoch: task.epoch, until: untilMs },
		});
		msg.version = BigInt(seq);
		this.persistMessage(msg);
		return create(BoardTaskClaimSchema, { epoch: task.epoch, until: BigInt(untilMs) });
	}

	createTask(opts: {
		project: string;
		re: number;
		text: string;
		labels: Record<string, string>;
		attachmentIds: string[];
		idempotencyKey: string;
		needs: number[];
		forActor: string;
		claim: boolean;
		leaseSeconds: number;
		identity: BoardIdentity;
		nowMs: number;
	}): {
		task: BoardMessage;
		claim?: BoardTaskClaim;
		mentions: BoardMention[];
		outcome: string;
		project: string;
	} {
		const assigned = this.resolveAssignee(opts.identity.author.principal, opts.forActor);
		if (opts.claim) normalizeLeaseSeconds(opts.leaseSeconds);

		return this.tx(() => {
			this.upsertActor(opts.identity.actor, opts.nowMs);
			let row: { id: number; projectId: number; msg: BoardMessage };
			let mentions: BoardMention[] = [];

			if (opts.re > 0) {
				const target = this.getMessageRow(opts.re);
				if (target.re !== null) {
					throw hotbedError(
						ErrorCode.InvalidArgs,
						`#${opts.re} is a reply; pass its thread root #${target.re}`,
					);
				}
				if (target.msg.deleted) {
					throw hotbedError(ErrorCode.Conflict, `message #${opts.re} was deleted`);
				}
				if (target.msg.task) {
					throw hotbedError(ErrorCode.Conflict, `already_a_task: #${opts.re} is already a task`);
				}
				const needs = this.validateNeeds(opts.needs, target.id);
				const waits = needs.ids.length > 0 && !needs.allCompleted;
				if (opts.claim && waits) {
					throw hotbedError(
						ErrorCode.Conflict,
						`task_waiting: #${target.id} waits for ${needs.ids.map((n) => `#${n}`).join(", ")}`,
					);
				}
				if (opts.claim && assigned && assigned !== opts.identity.actor.id) {
					throw hotbedError(
						ErrorCode.Conflict,
						`task_assigned: #${target.id} is assigned to ${assigned}`,
					);
				}
				row = target;
				const before = clone(BoardMessageSchema, row.msg);
				row.msg.task = create(BoardTaskSchema, {
					state: waits ? BoardTaskState.WAITING : BoardTaskState.READY,
					owner: opts.identity.author.principal,
					by: opts.identity.author.actor,
					forActor: assigned,
					needs: needs.ids.map((n) => BigInt(n)),
					epoch: 0,
				});
				this.syncTaskLabel(row.msg);
				const seq = this.appendEvent({
					at: opts.nowMs,
					projectId: row.projectId,
					projectName: row.msg.project,
					type: "task.created",
					messageId: row.id,
					threadId: row.id,
					actor: opts.identity.actor,
					beforeMsg: before,
					afterMsg: row.msg,
					payload: { needs: needs.ids, for: assigned || null },
				});
				row.msg.version = BigInt(seq);
				this.persistMessage(row.msg);
			} else {
				const needs = this.validateNeeds(opts.needs, null);
				const waits = needs.ids.length > 0 && !needs.allCompleted;
				if (opts.claim && waits) {
					throw hotbedError(
						ErrorCode.Conflict,
						`task_waiting: task would wait for ${needs.ids.map((n) => `#${n}`).join(", ")}`,
					);
				}
				if (opts.claim && assigned && assigned !== opts.identity.actor.id) {
					throw hotbedError(
						ErrorCode.Conflict,
						`task_assigned: task would be assigned to ${assigned}`,
					);
				}
				const posted = this.postMessage({
					project: opts.project,
					re: 0,
					text: opts.text,
					labels: opts.labels,
					attachmentIds: opts.attachmentIds,
					idempotencyKey: opts.idempotencyKey,
					afterId: 0,
					identity: opts.identity,
					nowMs: opts.nowMs,
				});
				mentions = posted.mentions;
				row = this.getMessageRow(Number(posted.message.id));
				// Idempotent retry: already a task
				if (row.msg.task) {
					return { task: row.msg, mentions, outcome: "created", project: row.msg.project };
				}
				const before = clone(BoardMessageSchema, row.msg);
				row.msg.task = create(BoardTaskSchema, {
					state: waits ? BoardTaskState.WAITING : BoardTaskState.READY,
					owner: opts.identity.author.principal,
					by: opts.identity.author.actor,
					forActor: assigned,
					needs: needs.ids.map((n) => BigInt(n)),
					epoch: 0,
				});
				this.syncTaskLabel(row.msg);
				const seq = this.appendEvent({
					at: opts.nowMs,
					projectId: row.projectId,
					projectName: row.msg.project,
					type: "task.created",
					messageId: row.id,
					threadId: row.id,
					actor: opts.identity.actor,
					beforeMsg: before,
					afterMsg: row.msg,
					payload: { needs: needs.ids, for: assigned || null },
				});
				row.msg.version = BigInt(seq);
				this.persistMessage(row.msg);
			}

			let claim: BoardTaskClaim | undefined;
			if (opts.claim) {
				claim = this.applyClaim(row, opts.identity, opts.leaseSeconds, opts.nowMs);
			}
			return { task: row.msg, claim, mentions, outcome: "created", project: row.msg.project };
		});
	}

	batchCreateTasks(opts: {
		project: string;
		tasks: BatchBoardTaskSpec[];
		idempotencyKey: string;
		identity: BoardIdentity;
		nowMs: number;
	}): { tasks: BoardMessage[]; project: string } {
		if (opts.tasks.length === 0) {
			throw hotbedError(ErrorCode.InvalidArgs, "batch needs at least 1 task");
		}
		if (opts.tasks.length > TASK_BATCH_MAX) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`batch_too_large: at most ${TASK_BATCH_MAX} tasks per batch (got ${opts.tasks.length})`,
			);
		}
		// Validate keys, internal refs, and acyclicity up front.
		const keyToIdx = new Map<string, number>();
		for (let i = 0; i < opts.tasks.length; i++) {
			const k = opts.tasks[i].key.trim();
			if (!k) continue;
			if (k.length > TASK_KEY_MAX) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`tasks[${i}].key is at most ${TASK_KEY_MAX} characters`,
				);
			}
			if (keyToIdx.has(k)) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`invalid_needs: duplicate task key ${JSON.stringify(k)}`,
				);
			}
			keyToIdx.set(k, i);
		}
		const insideDeps: number[][] = opts.tasks.map(() => []);
		const outsideDeps: number[][] = opts.tasks.map(() => []);
		for (let i = 0; i < opts.tasks.length; i++) {
			const t = opts.tasks[i];
			const seen = new Set<string>();
			for (const need of t.needs) {
				if (need.ref.case === "key") {
					const target = keyToIdx.get(need.ref.value);
					if (target === undefined) {
						throw hotbedError(
							ErrorCode.InvalidArgs,
							`invalid_needs: unknown plan task key ${JSON.stringify(need.ref.value)}`,
						);
					}
					if (target === i) {
						throw hotbedError(
							ErrorCode.InvalidArgs,
							`invalid_needs: task ${JSON.stringify(t.key)} cannot wait for itself`,
						);
					}
					const tag = `k:${target}`;
					if (!seen.has(tag)) {
						seen.add(tag);
						insideDeps[i].push(target);
					}
				} else if (need.ref.case === "id") {
					const idNum = Number(need.ref.value);
					const tag = `i:${idNum}`;
					if (!seen.has(tag)) {
						seen.add(tag);
						outsideDeps[i].push(idNum);
					}
				}
			}
			if (seen.size > TASK_NEEDS_MAX) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`tasks[${i}].needs: at most ${TASK_NEEDS_MAX} prerequisites`,
				);
			}
		}
		// DFS cycle detection among internal batch keys
		const color = new Uint8Array(opts.tasks.length); // 0=unvisited, 1=visiting, 2=done
		const dfs = (u: number) => {
			color[u] = 1;
			for (const v of insideDeps[u]) {
				if (color[v] === 1) {
					throw hotbedError(ErrorCode.InvalidArgs, "invalid_needs: tasks in plan form a cycle");
				}
				if (color[v] === 0) dfs(v);
			}
			color[u] = 2;
		};
		for (let i = 0; i < opts.tasks.length; i++) {
			if (color[i] === 0) dfs(i);
		}

		const digest = sha256HexSync(
			JSON.stringify({
				project: opts.project,
				tasks: opts.tasks.map((t) => ({
					key: t.key,
					text: t.text,
					labels: t.labels,
					attachments: t.attachmentIds,
					for: t.forActor,
					needs: t.needs.map((n) =>
						n.ref.case === "id" ? { case: "id", value: n.ref.value.toString() } : n.ref,
					),
				})),
			}),
		);

		return this.tx(() => {
			const projectId = this.projects.idByName(opts.project);
			this.upsertActor(opts.identity.actor, opts.nowMs);
			const scopedKey = opts.idempotencyKey.trim()
				? `${opts.identity.author.principal}:${opts.idempotencyKey.trim()}`
				: "";
			if (scopedKey) {
				const found = this.sql
					.exec(
						"SELECT request_digest, task_ids_json FROM board_task_batches WHERE idempotency_key = ?",
						scopedKey,
					)
					.toArray();
				if (found.length > 0) {
					if (str(found[0].request_digest) !== digest) {
						throw hotbedError(
							ErrorCode.Conflict,
							`duplicate_id: batch idempotency key ${JSON.stringify(opts.idempotencyKey)} was already used for a different plan`,
						);
					}
					const ids = JSON.parse(str(found[0].task_ids_json)) as number[];
					return {
						tasks: ids.map((id) => this.getMessageRow(id).msg),
						project: opts.project,
					};
				}
			}

			// Validate all external needs up front
			const outsideCompleted = outsideDeps.map((ids) => this.validateNeeds(ids, null).allCompleted);
			const assigned = opts.tasks.map((t) =>
				this.resolveAssignee(opts.identity.author.principal, t.forActor),
			);

			// First create all message roots so every task gets its ID
			const createdIds: number[] = [];
			for (let i = 0; i < opts.tasks.length; i++) {
				const spec = opts.tasks[i];
				const posted = this.postMessage({
					project: opts.project,
					re: 0,
					text: spec.text,
					labels: spec.labels,
					attachmentIds: spec.attachmentIds,
					idempotencyKey: "",
					afterId: 0,
					identity: opts.identity,
					nowMs: opts.nowMs,
				});
				createdIds.push(Number(posted.message.id));
			}

			// Now attach task metadata with resolved internal + external needs
			const out: BoardMessage[] = [];
			for (let i = 0; i < opts.tasks.length; i++) {
				const row = this.getMessageRow(createdIds[i]);
				const waitsFor = [...outsideDeps[i], ...insideDeps[i].map((idx) => createdIds[idx])];
				const waits =
					insideDeps[i].length > 0 || (outsideDeps[i].length > 0 && !outsideCompleted[i]);
				const before = clone(BoardMessageSchema, row.msg);
				row.msg.task = create(BoardTaskSchema, {
					state: waits ? BoardTaskState.WAITING : BoardTaskState.READY,
					owner: opts.identity.author.principal,
					by: opts.identity.author.actor,
					forActor: assigned[i],
					needs: waitsFor.map((n) => BigInt(n)),
					epoch: 0,
				});
				this.syncTaskLabel(row.msg);
				const seq = this.appendEvent({
					at: opts.nowMs,
					projectId,
					projectName: opts.project,
					type: "task.created",
					messageId: row.id,
					threadId: row.id,
					actor: opts.identity.actor,
					beforeMsg: before,
					afterMsg: row.msg,
					payload: { needs: waitsFor, for: assigned[i] || null },
				});
				row.msg.version = BigInt(seq);
				this.persistMessage(row.msg);
				out.push(row.msg);
			}

			if (scopedKey) {
				this.sql.exec(
					`INSERT INTO board_task_batches (idempotency_key, request_digest, project_id, task_ids_json, created_at)
					 VALUES (?, ?, ?, ?, ?)`,
					scopedKey,
					digest,
					projectId,
					JSON.stringify(createdIds),
					opts.nowMs,
				);
			}
			return { tasks: out, project: opts.project };
		});
	}

	listTasks(opts: TaskFilterOpts, nowMs: number): { tasks: BoardThreadSummary[]; cursor: bigint } {
		const limit = Math.min(Math.max(opts.limit || 50, 1), 500);
		const where: string[] = ["r.re IS NULL", "r.has_task = 1", "r.deleted = 0"];
		const vals: unknown[] = [];

		if (opts.projectId !== null) {
			where.push("r.project_id = ?");
			vals.push(opts.projectId);
		}
		if (opts.after > 0) {
			where.push("r.id > ?");
			vals.push(opts.after);
		}

		const wantedStates: Set<BoardTaskState> | null = opts.includeAll
			? null
			: new Set(opts.states.length > 0 ? opts.states : OPEN_TASK_STATES);

		// SQL pre-filter on task_state (note: lapsed 'claimed' tasks behave as 'ready').
		if (wantedStates !== null) {
			const sqlStates = new Set<string>();
			for (const st of wantedStates) {
				const name = taskStateName(st);
				if (name) sqlStates.add(name);
				if (st === BoardTaskState.READY) sqlStates.add("claimed");
			}
			if (sqlStates.size > 0) {
				where.push(`r.task_state IN (${[...sqlStates].map(() => "?").join(",")})`);
				vals.push(...sqlStates);
			}
		}

		if (opts.by.trim()) {
			const by = this.resolveByTarget(opts.by);
			const parts: string[] = [];
			if (by.actors.length > 0) {
				parts.push(`r.by_actor IN (${by.actors.map(() => "?").join(",")})`);
				vals.push(...by.actors);
			}
			if (by.principals.length > 0) {
				parts.push(`r.by_principal IN (${by.principals.map(() => "?").join(",")})`);
				vals.push(...by.principals);
			}
			where.push(parts.length > 0 ? `(${parts.join(" OR ")})` : "1 = 0");
		}
		if (opts.owner.trim()) {
			const ownerRes = this.resolveByTarget(opts.owner);
			const owners = new Set([opts.owner.trim(), ...ownerRes.actors, ...ownerRes.principals]);
			where.push(`r.task_owner IN (${[...owners].map(() => "?").join(",")})`);
			vals.push(...owners);
		}
		if (opts.forActor.trim()) {
			const f = opts.forActor.trim();
			const res = this.resolveByTarget(f);
			const candidates = new Set([f, ...res.actors]);
			// Also match suffix +profile
			const suffix = f.toLowerCase();
			where.push(
				`(r.task_for IN (${[...candidates].map(() => "?").join(",")}) OR lower(r.task_for) LIKE ?)`,
			);
			vals.push(...candidates, `%+${suffix}`);
		}
		if (opts.needs > 0) {
			where.push(
				"EXISTS (SELECT 1 FROM board_terms t WHERE t.message_id = r.id AND t.term = ? AND t.value = ?)",
			);
			vals.push(NEEDS_TERM, String(opts.needs));
		}
		for (const lf of opts.labels) {
			if (lf.values.length === 0) {
				where.push("EXISTS (SELECT 1 FROM board_terms t WHERE t.message_id = r.id AND t.term = ?)");
				vals.push(lf.key);
			} else {
				where.push(
					`EXISTS (SELECT 1 FROM board_terms t WHERE t.message_id = r.id AND t.term = ? AND t.value IN (${lf.values.map(() => "?").join(",")}))`,
				);
				vals.push(lf.key, ...lf.values);
			}
		}

		const orderSql = opts.latest
			? "ORDER BY r.last_activity_at DESC, r.id DESC"
			: "ORDER BY r.id ASC";
		const rows = this.sql
			.exec(
				`SELECT r.id, r.doc, r.replies_count, l.doc AS last_doc
				 FROM board_messages r
				 LEFT JOIN board_messages l ON l.id = r.last_activity_id AND r.last_activity_id != r.id
				 WHERE ${where.join(" AND ")}
				 ${orderSql}
				 LIMIT ?`,
				...vals,
				Math.min(limit * 4, 2000),
			)
			.toArray();

		const kept: BoardThreadSummary[] = [];
		let lastId = 0;
		for (const r of rows) {
			const msg = unmarshalMsg(str(r.doc));
			if (!msg.task) continue;
			const eff = effectiveState(msg.task, nowMs);
			if (wantedStates !== null && !wantedStates.has(eff)) continue;
			// Present lapsed claims with effective state READY in list view
			if (eff !== msg.task.state) {
				msg.task.state = eff;
				msg.task.claim = undefined;
				msg.labels[TASK_LABEL] = taskStateName(eff);
			}
			lastId = num(r.id);
			kept.push(
				create(BoardThreadSummarySchema, {
					message: msg,
					replies: num(r.replies_count),
					last: r.last_doc ? unmarshalMsg(str(r.last_doc)) : undefined,
				}),
			);
			if (kept.length >= limit) break;
		}
		if (opts.latest) kept.reverse();
		const cursor =
			!opts.latest && kept.length === limit ? BigInt(lastId) : BigInt(this.latestEventCursor());
		return { tasks: kept, cursor };
	}

	claimTaskById(opts: {
		id: number;
		leaseSeconds: number;
		identity: BoardIdentity;
		nowMs: number;
	}): { task: BoardMessage; claim: BoardTaskClaim; project: string } {
		return this.tx(() => {
			this.upsertActor(opts.identity.actor, opts.nowMs);
			const row = this.getMessageRow(opts.id);
			const msg = row.msg;
			if (!msg.task) {
				throw hotbedError(ErrorCode.NotFound, `not_a_task: #${opts.id} isn't a task`);
			}
			const task = msg.task;
			// Claiming a task the caller already holds re-issues its lease.
			if (this.isHeldBy(task, opts.identity, opts.nowMs)) {
				const claim = this.renewHeld(row, opts.identity, 0, opts.leaseSeconds, opts.nowMs);
				return { task: msg, claim, project: msg.project };
			}
			if (task.state === BoardTaskState.WAITING) {
				throw hotbedError(
					ErrorCode.Conflict,
					`task_waiting: #${opts.id} waits for ${task.needs.map((n) => `#${n}`).join(", ")}`,
				);
			}
			if (task.state === BoardTaskState.BLOCKED) {
				throw hotbedError(
					ErrorCode.Conflict,
					`task_blocked: #${opts.id} is held: ${task.blocked?.reason ?? ""}`,
				);
			}
			if (isFinished(task)) {
				throw hotbedError(
					ErrorCode.Conflict,
					`task_finished: #${opts.id} is ${taskStateName(task.state)}`,
				);
			}
			if (task.forActor && task.forActor !== opts.identity.actor.id) {
				throw hotbedError(
					ErrorCode.Conflict,
					`task_assigned: #${opts.id} is assigned to ${task.forActor}`,
				);
			}
			if (task.state === BoardTaskState.CLAIMED && !isLapsed(task, opts.nowMs)) {
				throw hotbedError(
					ErrorCode.Conflict,
					`task_claimed: #${opts.id} is claimed by ${task.claim?.actor ?? "someone"}`,
				).withSuggestion(`hotbed board read ${opts.id}`);
			}
			const claim = this.applyClaim(row, opts.identity, opts.leaseSeconds, opts.nowMs);
			return { task: msg, claim, project: msg.project };
		});
	}

	claimNextReadyTask(opts: {
		projectId: number | null;
		labels: BoardLabelFilter[];
		fromOwners: string[];
		leaseSeconds: number;
		identity: BoardIdentity;
		nowMs: number;
	}): { task: BoardMessage; claim: BoardTaskClaim; project: string } | null {
		return this.tx(() => {
			this.upsertActor(opts.identity.actor, opts.nowMs);
			const where: string[] = [
				"r.re IS NULL",
				"r.has_task = 1",
				"r.deleted = 0",
				"r.task_state IN ('ready', 'claimed')",
			];
			const vals: unknown[] = [];
			if (opts.projectId !== null) {
				where.push("r.project_id = ?");
				vals.push(opts.projectId);
			}
			for (const lf of opts.labels) {
				if (lf.values.length === 0) {
					where.push(
						"EXISTS (SELECT 1 FROM board_terms t WHERE t.message_id = r.id AND t.term = ?)",
					);
					vals.push(lf.key);
				} else {
					where.push(
						`EXISTS (SELECT 1 FROM board_terms t WHERE t.message_id = r.id AND t.term = ? AND t.value IN (${lf.values.map(() => "?").join(",")}))`,
					);
					vals.push(lf.key, ...lf.values);
				}
			}

			const allowedOwners =
				opts.fromOwners.length > 0
					? new Set([opts.identity.author.principal, ...opts.fromOwners])
					: null;

			const rows = this.sql
				.exec(
					`SELECT r.id, r.project_id, r.doc FROM board_messages r
					 WHERE ${where.join(" AND ")}
					 ORDER BY r.id ASC
					 LIMIT 500`,
					...vals,
				)
				.toArray();

			let mine: { id: number; projectId: number; msg: BoardMessage } | null = null;
			let pool: { id: number; projectId: number; msg: BoardMessage } | null = null;
			for (const r of rows) {
				const msg = unmarshalMsg(str(r.doc));
				if (!msg.task) continue;
				if (effectiveState(msg.task, opts.nowMs) !== BoardTaskState.READY) continue;
				const candidate = { id: num(r.id), projectId: num(r.project_id), msg };
				if (msg.task.forActor === opts.identity.actor.id) {
					mine = candidate;
					break;
				}
				if (
					!msg.task.forActor &&
					pool === null &&
					(allowedOwners === null || allowedOwners.has(msg.task.owner))
				) {
					pool = candidate;
				}
			}
			const chosen = mine ?? pool;
			if (!chosen) return null;
			const claim = this.applyClaim(chosen, opts.identity, opts.leaseSeconds, opts.nowMs);
			return { task: chosen.msg, claim, project: chosen.msg.project };
		});
	}

	renewTask(opts: {
		id: number;
		leaseSeconds: number;
		epoch: number;
		identity: BoardIdentity;
		nowMs: number;
	}): {
		task: BoardMessage;
		claim: BoardTaskClaim;
		project: string;
	} {
		return this.tx(() => {
			this.upsertActor(opts.identity.actor, opts.nowMs);
			const row = this.getMessageRow(opts.id);
			const msg = row.msg;
			if (!msg.task) {
				throw hotbedError(ErrorCode.NotFound, `not_a_task: #${opts.id} isn't a task`);
			}
			const claim = this.renewHeld(row, opts.identity, opts.epoch, opts.leaseSeconds, opts.nowMs);
			return { task: msg, claim, project: msg.project };
		});
	}

	releaseTask(opts: { id: number; epoch: number; identity: BoardIdentity; nowMs: number }): {
		task: BoardMessage;
		project: string;
	} {
		return this.tx(() => {
			this.upsertActor(opts.identity.actor, opts.nowMs);
			const row = this.getMessageRow(opts.id);
			const msg = row.msg;
			if (!msg.task) throw hotbedError(ErrorCode.NotFound, `not_a_task: #${opts.id} isn't a task`);
			this.requireHeld(opts.id, msg.task, opts.identity, opts.epoch, opts.nowMs);
			const before = clone(BoardMessageSchema, msg);
			msg.task.state = BoardTaskState.READY;
			msg.task.claim = undefined;
			this.syncTaskLabel(msg);
			const seq = this.appendEvent({
				at: opts.nowMs,
				projectId: row.projectId,
				projectName: msg.project,
				type: "task.released",
				messageId: row.id,
				threadId: row.id,
				actor: opts.identity.actor,
				beforeMsg: before,
				afterMsg: msg,
				payload: { epoch: msg.task.epoch },
			});
			msg.version = BigInt(seq);
			this.persistMessage(msg);
			return { task: msg, project: msg.project };
		});
	}

	completeTask(opts: {
		id: number;
		result: string;
		epoch: number;
		identity: BoardIdentity;
		nowMs: number;
	}): {
		task: BoardMessage;
		projects: string[];
	} {
		const cleanResult = validateTaskNote("result", opts.result, false);
		return this.tx(() => {
			this.upsertActor(opts.identity.actor, opts.nowMs);
			const row = this.getMessageRow(opts.id);
			const msg = row.msg;
			if (!msg.task) throw hotbedError(ErrorCode.NotFound, `not_a_task: #${opts.id} isn't a task`);
			this.requireHeld(opts.id, msg.task, opts.identity, opts.epoch, opts.nowMs);
			const before = clone(BoardMessageSchema, msg);
			msg.task.state = BoardTaskState.COMPLETED;
			msg.task.claim = undefined;
			msg.task.result = cleanResult;
			this.syncTaskLabel(msg);
			const seq = this.appendEvent({
				at: opts.nowMs,
				projectId: row.projectId,
				projectName: msg.project,
				type: "task.completed",
				messageId: row.id,
				threadId: row.id,
				actor: opts.identity.actor,
				beforeMsg: before,
				afterMsg: msg,
				payload: { epoch: msg.task.epoch, result: cleanResult || null },
			});
			msg.version = BigInt(seq);
			this.persistMessage(msg);
			const woken = this.readyDependents(row.id, opts.nowMs);
			return { task: msg, projects: [msg.project, ...woken] };
		});
	}

	failTask(opts: {
		id: number;
		reason: string;
		epoch: number;
		identity: BoardIdentity;
		nowMs: number;
	}): {
		task: BoardMessage;
		project: string;
	} {
		const cleanReason = validateTaskNote("reason", opts.reason, true);
		return this.tx(() => {
			this.upsertActor(opts.identity.actor, opts.nowMs);
			const row = this.getMessageRow(opts.id);
			const msg = row.msg;
			if (!msg.task) throw hotbedError(ErrorCode.NotFound, `not_a_task: #${opts.id} isn't a task`);
			this.requireHeld(opts.id, msg.task, opts.identity, opts.epoch, opts.nowMs);
			const before = clone(BoardMessageSchema, msg);
			msg.task.state = BoardTaskState.FAILED;
			msg.task.claim = undefined;
			msg.task.reason = cleanReason;
			this.syncTaskLabel(msg);
			const seq = this.appendEvent({
				at: opts.nowMs,
				projectId: row.projectId,
				projectName: msg.project,
				type: "task.failed",
				messageId: row.id,
				threadId: row.id,
				actor: opts.identity.actor,
				beforeMsg: before,
				afterMsg: msg,
				payload: { epoch: msg.task.epoch, reason: cleanReason },
			});
			msg.version = BigInt(seq);
			this.persistMessage(msg);
			return { task: msg, project: msg.project };
		});
	}

	blockTask(opts: {
		id: number;
		reason: string;
		kind: string;
		epoch: number;
		identity: BoardIdentity;
		nowMs: number;
	}): { task: BoardMessage; project: string } {
		const cleanReason = validateTaskNote("reason", opts.reason, true);
		const cleanKind = opts.kind.trim().toLowerCase();
		if (cleanKind) {
			if (!TASK_HOLD_KIND_RE.test(cleanKind)) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					"kind must be up to 64 lowercase letters, digits, or _.-",
				);
			}
			if (cleanKind === EDITED_HOLD) {
				throw hotbedError(ErrorCode.InvalidArgs, `kind "${EDITED_HOLD}" is reserved by the board`);
			}
		}
		return this.tx(() => {
			this.upsertActor(opts.identity.actor, opts.nowMs);
			const row = this.getMessageRow(opts.id);
			const msg = row.msg;
			if (!msg.task) throw hotbedError(ErrorCode.NotFound, `not_a_task: #${opts.id} isn't a task`);
			const task = msg.task;
			if (isFinished(task)) {
				throw hotbedError(
					ErrorCode.Conflict,
					`task_finished: #${opts.id} is ${taskStateName(task.state)}`,
				);
			}
			if (this.isHeldBy(task, opts.identity, opts.nowMs)) {
				this.requireHeld(opts.id, task, opts.identity, opts.epoch, opts.nowMs);
			} else if (task.state === BoardTaskState.CLAIMED && !isLapsed(task, opts.nowMs)) {
				if (!this.canOwnTask(task, opts.identity)) {
					throw hotbedError(
						ErrorCode.Conflict,
						`task_claimed: #${opts.id} is claimed by ${task.claim?.actor ?? "someone"}; act as its holder or its owner`,
					);
				}
			} else if (!this.canOwnTask(task, opts.identity)) {
				throw hotbedError(
					ErrorCode.NotAuthorized,
					`task_not_owned: #${opts.id} is someone else's task`,
				);
			}
			const before = clone(BoardMessageSchema, msg);
			task.state = BoardTaskState.BLOCKED;
			task.claim = undefined;
			task.blocked = create(BoardTaskBlockedSchema, { kind: cleanKind, reason: cleanReason });
			this.syncTaskLabel(msg);
			const seq = this.appendEvent({
				at: opts.nowMs,
				projectId: row.projectId,
				projectName: msg.project,
				type: "task.blocked",
				messageId: row.id,
				threadId: row.id,
				actor: opts.identity.actor,
				beforeMsg: before,
				afterMsg: msg,
				payload: { kind: cleanKind || null, reason: cleanReason },
			});
			msg.version = BigInt(seq);
			this.persistMessage(msg);
			return { task: msg, project: msg.project };
		});
	}

	unblockTask(opts: { id: number; identity: BoardIdentity; nowMs: number }): {
		task: BoardMessage;
		project: string;
	} {
		return this.tx(() => {
			this.upsertActor(opts.identity.actor, opts.nowMs);
			const row = this.getMessageRow(opts.id);
			const msg = row.msg;
			if (!msg.task) throw hotbedError(ErrorCode.NotFound, `not_a_task: #${opts.id} isn't a task`);
			if (!this.canOwnTask(msg.task, opts.identity)) {
				throw hotbedError(
					ErrorCode.NotAuthorized,
					`task_not_owned: #${opts.id} is someone else's task`,
				);
			}
			if (msg.task.state !== BoardTaskState.BLOCKED) return { task: msg, project: msg.project };
			const before = clone(BoardMessageSchema, msg);
			const waits = msg.task.needs.length > 0 && !this.allNeedsCompleted(msg.task.needs);
			msg.task.state = waits ? BoardTaskState.WAITING : BoardTaskState.READY;
			msg.task.blocked = undefined;
			this.syncTaskLabel(msg);
			const seq = this.appendEvent({
				at: opts.nowMs,
				projectId: row.projectId,
				projectName: msg.project,
				type: "task.unblocked",
				messageId: row.id,
				threadId: row.id,
				actor: opts.identity.actor,
				beforeMsg: before,
				afterMsg: msg,
			});
			msg.version = BigInt(seq);
			this.persistMessage(msg);
			return { task: msg, project: msg.project };
		});
	}

	reopenTask(opts: { id: number; identity: BoardIdentity; nowMs: number }): {
		task: BoardMessage;
		project: string;
	} {
		return this.tx(() => {
			this.upsertActor(opts.identity.actor, opts.nowMs);
			const row = this.getMessageRow(opts.id);
			const msg = row.msg;
			if (!msg.task) throw hotbedError(ErrorCode.NotFound, `not_a_task: #${opts.id} isn't a task`);
			if (!this.canOwnTask(msg.task, opts.identity)) {
				throw hotbedError(
					ErrorCode.NotAuthorized,
					`task_not_owned: #${opts.id} is someone else's task`,
				);
			}
			if (msg.task.state !== BoardTaskState.FAILED) return { task: msg, project: msg.project };
			const before = clone(BoardMessageSchema, msg);
			msg.task.state = BoardTaskState.READY;
			msg.task.reason = "";
			this.syncTaskLabel(msg);
			const seq = this.appendEvent({
				at: opts.nowMs,
				projectId: row.projectId,
				projectName: msg.project,
				type: "task.reopened",
				messageId: row.id,
				threadId: row.id,
				actor: opts.identity.actor,
				beforeMsg: before,
				afterMsg: msg,
			});
			msg.version = BigInt(seq);
			this.persistMessage(msg);
			return { task: msg, project: msg.project };
		});
	}

	assignTask(opts: {
		id: number;
		forActor: string;
		unassign: boolean;
		identity: BoardIdentity;
		nowMs: number;
	}): { task: BoardMessage; project: string } {
		return this.tx(() => {
			this.upsertActor(opts.identity.actor, opts.nowMs);
			const row = this.getMessageRow(opts.id);
			const msg = row.msg;
			if (!msg.task) throw hotbedError(ErrorCode.NotFound, `not_a_task: #${opts.id} isn't a task`);
			if (!this.canOwnTask(msg.task, opts.identity)) {
				throw hotbedError(
					ErrorCode.NotAuthorized,
					`task_not_owned: #${opts.id} is someone else's task`,
				);
			}
			const target = opts.unassign ? "" : this.resolveAssignee(msg.task.owner, opts.forActor);
			if (msg.task.forActor === target) return { task: msg, project: msg.project };
			const before = clone(BoardMessageSchema, msg);
			msg.task.forActor = target;
			const seq = this.appendEvent({
				at: opts.nowMs,
				projectId: row.projectId,
				projectName: msg.project,
				type: "task.assigned",
				messageId: row.id,
				threadId: row.id,
				actor: opts.identity.actor,
				beforeMsg: before,
				afterMsg: msg,
				payload: { for: target || null },
			});
			msg.version = BigInt(seq);
			this.persistMessage(msg);
			return { task: msg, project: msg.project };
		});
	}

	setTaskNeeds(opts: { id: number; needs: number[]; identity: BoardIdentity; nowMs: number }): {
		task: BoardMessage;
		project: string;
	} {
		return this.tx(() => {
			this.upsertActor(opts.identity.actor, opts.nowMs);
			const row = this.getMessageRow(opts.id);
			const msg = row.msg;
			if (!msg.task) throw hotbedError(ErrorCode.NotFound, `not_a_task: #${opts.id} isn't a task`);
			const task = msg.task;
			if (!this.canOwnTask(task, opts.identity)) {
				throw hotbedError(
					ErrorCode.NotAuthorized,
					`task_not_owned: #${opts.id} is someone else's task`,
				);
			}
			if (isFinished(task)) {
				throw hotbedError(
					ErrorCode.Conflict,
					`task_finished: #${opts.id} is ${taskStateName(task.state)}`,
				);
			}
			if (task.state === BoardTaskState.CLAIMED && !isLapsed(task, opts.nowMs)) {
				throw hotbedError(
					ErrorCode.Conflict,
					`task_claimed: cannot change needs while #${opts.id} is claimed`,
				);
			}
			const validated = this.validateNeeds(opts.needs, row.id);
			const prevSet = new Set(task.needs.map((n) => Number(n)));
			const added = validated.ids.filter((n) => !prevSet.has(n));
			if (added.length > 0) this.checkNoCycle(row.id, added);

			const before = clone(BoardMessageSchema, msg);
			task.needs = validated.ids.map((n) => BigInt(n));
			if (task.state !== BoardTaskState.BLOCKED) {
				task.claim = undefined;
				task.state =
					validated.ids.length > 0 && !validated.allCompleted
						? BoardTaskState.WAITING
						: BoardTaskState.READY;
			}
			this.syncTaskLabel(msg);
			const seq = this.appendEvent({
				at: opts.nowMs,
				projectId: row.projectId,
				projectName: msg.project,
				type: "task.needs_changed",
				messageId: row.id,
				threadId: row.id,
				actor: opts.identity.actor,
				beforeMsg: before,
				afterMsg: msg,
				payload: { needs: validated.ids },
			});
			msg.version = BigInt(seq);
			this.persistMessage(msg);
			return { task: msg, project: msg.project };
		});
	}

	cancelTask(opts: { id: number; reason: string; identity: BoardIdentity; nowMs: number }): {
		task: BoardMessage;
		project: string;
	} {
		const cleanReason = validateTaskNote("reason", opts.reason, true);
		return this.tx(() => {
			this.upsertActor(opts.identity.actor, opts.nowMs);
			const row = this.getMessageRow(opts.id);
			const msg = row.msg;
			if (!msg.task) throw hotbedError(ErrorCode.NotFound, `not_a_task: #${opts.id} isn't a task`);
			const task = msg.task;
			if (!this.canOwnTask(task, opts.identity)) {
				throw hotbedError(
					ErrorCode.NotAuthorized,
					`task_not_owned: #${opts.id} is someone else's task`,
				);
			}
			if (task.state === BoardTaskState.COMPLETED || task.state === BoardTaskState.CANCELED) {
				throw hotbedError(
					ErrorCode.Conflict,
					`task_finished: #${opts.id} is ${taskStateName(task.state)}`,
				);
			}
			const before = clone(BoardMessageSchema, msg);
			task.state = BoardTaskState.CANCELED;
			task.claim = undefined;
			task.blocked = undefined;
			task.reason = cleanReason;
			this.syncTaskLabel(msg);
			const seq = this.appendEvent({
				at: opts.nowMs,
				projectId: row.projectId,
				projectName: msg.project,
				type: "task.canceled",
				messageId: row.id,
				threadId: row.id,
				actor: opts.identity.actor,
				beforeMsg: before,
				afterMsg: msg,
				payload: { reason: cleanReason },
			});
			msg.version = BigInt(seq);
			this.persistMessage(msg);
			return { task: msg, project: msg.project };
		});
	}

	expireDueLeases(nowMs: number): string[] {
		const expiredRows = this.sql
			.exec(
				"SELECT id, project_id, doc FROM board_messages WHERE task_state = 'claimed' AND task_lease_until IS NOT NULL AND task_lease_until <= ?",
				nowMs,
			)
			.toArray();
		if (expiredRows.length === 0) return [];
		const woken: string[] = [];
		this.tx(() => {
			for (const r of expiredRows) {
				const id = num(r.id);
				const fresh = this.getMessageRow(id);
				const msg = fresh.msg;
				if (!msg.task || !isLapsed(msg.task, nowMs)) continue;
				const before = clone(BoardMessageSchema, msg);
				const oldEpoch = msg.task.epoch;
				msg.task.state = BoardTaskState.READY;
				msg.task.claim = undefined;
				this.syncTaskLabel(msg);
				const seq = this.appendEvent({
					at: nowMs,
					projectId: fresh.projectId,
					projectName: msg.project,
					type: "task.expired",
					messageId: id,
					threadId: id,
					beforeMsg: before,
					afterMsg: msg,
					payload: { epoch: oldEpoch },
				});
				msg.version = BigInt(seq);
				this.persistMessage(msg);
				woken.push(msg.project);
			}
		});
		return woken;
	}

	// ---- Scheduled Board Messages ----

	createSchedule(opts: {
		project: string;
		re: number;
		text: string;
		labels: Record<string, string>;
		attachmentIds: string[];
		inSeconds: number;
		atMs: number;
		everySeconds: number;
		untilMs: number;
		forSeconds: number;
		times: number;
		identity: BoardIdentity;
		nowMs: number;
	}): { schedule: BoardSchedule; mentions: BoardMention[] } {
		if (opts.inSeconds > 0 && opts.atMs > 0) {
			throw hotbedError(ErrorCode.InvalidArgs, "give --in or --at, not both");
		}
		if (opts.inSeconds === 0 && opts.atMs === 0 && opts.everySeconds === 0) {
			throw hotbedError(ErrorCode.InvalidArgs, "give --in, --at, or --every");
		}
		if (opts.everySeconds > 0 && opts.everySeconds < SCHEDULE_MIN_EVERY_S) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`--every must be at least ${SCHEDULE_MIN_EVERY_S} seconds`,
			);
		}
		const stops =
			(opts.untilMs > 0 ? 1 : 0) + (opts.forSeconds > 0 ? 1 : 0) + (opts.times > 0 ? 1 : 0);
		if (stops > 1) {
			throw hotbedError(ErrorCode.InvalidArgs, "give at most one of --until, --for, or --times");
		}
		if (stops > 0 && opts.everySeconds === 0) {
			throw hotbedError(ErrorCode.InvalidArgs, "--until, --for, and --times require --every");
		}

		const firstAt =
			opts.atMs > 0
				? opts.atMs
				: opts.inSeconds > 0
					? opts.nowMs + opts.inSeconds * 1000
					: opts.nowMs + opts.everySeconds * 1000;
		if (firstAt <= opts.nowMs) {
			throw hotbedError(ErrorCode.InvalidArgs, "scheduled time must be in the future");
		}
		if (firstAt - opts.nowMs > SCHEDULE_MAX_AHEAD_MS) {
			throw hotbedError(ErrorCode.InvalidArgs, "schedules can reach at most one year ahead");
		}
		const untilAt =
			opts.untilMs > 0 ? opts.untilMs : opts.forSeconds > 0 ? firstAt + opts.forSeconds * 1000 : 0;
		const cleanLabels = validateLabels(opts.labels, false);

		return this.tx(() => {
			this.upsertActor(opts.identity.actor, opts.nowMs);
			const attachments = this.resolveAttachments(opts.attachmentIds);
			validateMessageText(opts.text, attachments.length > 0);
			this.validateAndParseBlocks(opts.text);

			let projectId: number;
			let projectName: string;
			let rootId: number | null = null;
			if (opts.re > 0) {
				const target = this.getMessageRow(opts.re);
				projectId = target.projectId;
				projectName = target.msg.project;
				rootId = target.re ?? target.id;
			} else {
				projectName = opts.project;
				projectId = this.projects.idByName(projectName);
			}
			const { text: settledText, mentions } = this.settleTextMentions(opts.text, rootId);
			const sched = create(BoardScheduleSchema, {
				project: projectName,
				re: BigInt(rootId ?? 0),
				text: settledText,
				labels: cleanLabels,
				attachments,
				by: opts.identity.author,
				author: opts.identity.actor,
				state: BoardScheduleState.PENDING,
				firstAt: BigInt(firstAt),
				nextAt: BigInt(firstAt),
				everySeconds: BigInt(opts.everySeconds),
				untilAt: BigInt(untilAt),
				times: opts.times,
				sent: 0,
				createdAt: BigInt(opts.nowMs),
			});
			const ins = this.sql
				.exec(
					`INSERT INTO board_schedules (project_id, re, state, next_at, by_actor, doc)
					 VALUES (?, ?, 'pending', ?, ?, ?)
					 RETURNING id`,
					projectId,
					rootId,
					firstAt,
					opts.identity.actor.id,
					marshalSchedule(sched),
				)
				.toArray();
			const id = num(ins[0].id);
			sched.id = BigInt(id);
			this.sql.exec("UPDATE board_schedules SET doc = ? WHERE id = ?", marshalSchedule(sched), id);
			return { schedule: sched, mentions };
		});
	}

	listSchedules(opts: {
		projectId: number | null;
		re: number;
		by: string;
		includeAll: boolean;
	}): BoardSchedule[] {
		const where: string[] = [];
		const vals: unknown[] = [];
		if (!opts.includeAll) {
			where.push("state = 'pending'");
		}
		if (opts.projectId !== null) {
			where.push("project_id = ?");
			vals.push(opts.projectId);
		}
		if (opts.re > 0) {
			const target = this.getMessageRow(opts.re);
			where.push("re = ?");
			vals.push(target.re ?? target.id);
		}
		if (opts.by.trim()) {
			const by = this.resolveByTarget(opts.by);
			if (by.actors.length === 0) return [];
			where.push(`by_actor IN (${by.actors.map(() => "?").join(",")})`);
			vals.push(...by.actors);
		}
		const rows = this.sql
			.exec(
				`SELECT doc FROM board_schedules
				 ${where.length > 0 ? `WHERE ${where.join(" AND ")}` : ""}
				 ORDER BY COALESCE(next_at, 0) ASC, id ASC`,
				...vals,
			)
			.toArray();
		return rows.map((r) => unmarshalSchedule(str(r.doc)));
	}

	cancelSchedule(
		id: number,
		identity: BoardIdentity,
		reason = "canceled",
	): { schedule: BoardSchedule; canceled: boolean } {
		return this.tx(() => {
			const rows = this.sql.exec("SELECT doc FROM board_schedules WHERE id = ?", id).toArray();
			if (rows.length === 0) {
				throw hotbedError(ErrorCode.NotFound, `board schedule #${id} not found`);
			}
			const sched = unmarshalSchedule(str(rows[0].doc));
			if (
				!identity.isAdmin &&
				sched.by?.actor !== identity.author.actor &&
				!(!identity.author.profile && sched.by?.principal === identity.author.principal)
			) {
				throw hotbedError(ErrorCode.NotAuthorized, `only schedule #${id}'s creator can cancel it`);
			}
			if (sched.state !== BoardScheduleState.PENDING) {
				return { schedule: sched, canceled: false };
			}
			sched.state = BoardScheduleState.CANCELED;
			sched.nextAt = 0n;
			sched.cancelReason = reason;
			this.sql.exec(
				"UPDATE board_schedules SET state = 'canceled', next_at = NULL, doc = ? WHERE id = ?",
				marshalSchedule(sched),
				id,
			);
			return { schedule: sched, canceled: true };
		});
	}

	tickDueSchedules(nowMs: number): string[] {
		const dueRows = this.sql
			.exec(
				"SELECT id, doc FROM board_schedules WHERE state = 'pending' AND next_at IS NOT NULL AND next_at <= ? ORDER BY next_at ASC, id ASC",
				nowMs,
			)
			.toArray();
		if (dueRows.length === 0) return [];
		const woken: string[] = [];
		for (const r of dueRows) {
			const id = num(r.id);
			const sched = unmarshalSchedule(str(r.doc));
			if (sched.state !== BoardScheduleState.PENDING) continue;
			const re = Number(sched.re);
			if (re > 0) {
				const rootRows = this.sql
					.exec("SELECT deleted FROM board_messages WHERE id = ?", re)
					.toArray();
				if (rootRows.length === 0 || num(rootRows[0].deleted) === 1) {
					sched.state = BoardScheduleState.CANCELED;
					sched.nextAt = 0n;
					sched.cancelReason = "thread_deleted";
					this.sql.exec(
						"UPDATE board_schedules SET state = 'canceled', next_at = NULL, doc = ? WHERE id = ?",
						marshalSchedule(sched),
						id,
					);
					continue;
				}
			}
			const identity: BoardIdentity = {
				author: sched.by ?? create(BoardAuthorSchema, { actor: "system", principal: "system" }),
				actor:
					sched.author ??
					create(BoardActorSchema, {
						id: sched.by?.actor ?? "system",
						kind: BoardActorKind.AGENT,
						handle: "system",
						name: "system",
					}),
				isAdmin: false,
			};
			try {
				const posted = this.postMessage({
					project: sched.project,
					re,
					text: sched.text,
					labels: sched.labels,
					attachmentIds: sched.attachments.map((a) => a.id),
					idempotencyKey: "",
					afterId: 0,
					identity,
					nowMs,
					schedule: { id, n: sched.sent },
				});
				woken.push(posted.project);
			} catch {
				// If posting fails (e.g. deleted project), cancel the schedule rather than spin-looping.
				sched.state = BoardScheduleState.CANCELED;
				sched.nextAt = 0n;
				sched.cancelReason = "failed";
				this.sql.exec(
					"UPDATE board_schedules SET state = 'canceled', next_at = NULL, doc = ? WHERE id = ?",
					marshalSchedule(sched),
					id,
				);
				continue;
			}
			sched.sent += 1;
			const everyMs = Number(sched.everySeconds) * 1000;
			let nextAt = 0;
			if (everyMs > 0) {
				const candidate = Number(sched.nextAt) + everyMs;
				const hitTimes = sched.times > 0 && sched.sent >= sched.times;
				const hitUntil = Number(sched.untilAt) > 0 && candidate > Number(sched.untilAt);
				if (!hitTimes && !hitUntil) {
					nextAt = candidate;
				}
			}
			if (nextAt > 0) {
				sched.nextAt = BigInt(nextAt);
				this.sql.exec(
					"UPDATE board_schedules SET next_at = ?, doc = ? WHERE id = ?",
					nextAt,
					marshalSchedule(sched),
					id,
				);
			} else {
				sched.state = BoardScheduleState.DONE;
				sched.nextAt = 0n;
				this.sql.exec(
					"UPDATE board_schedules SET state = 'done', next_at = NULL, doc = ? WHERE id = ?",
					marshalSchedule(sched),
					id,
				);
			}
		}
		return woken;
	}

	hasPendingWork(nowMs: number): boolean {
		const s = this.sql
			.exec("SELECT 1 FROM board_schedules WHERE state = 'pending' LIMIT 1")
			.toArray();
		if (s.length > 0) return true;
		const l = this.sql
			.exec(
				"SELECT 1 FROM board_messages WHERE task_state = 'claimed' AND task_lease_until IS NOT NULL LIMIT 1",
			)
			.toArray();
		void nowMs;
		return l.length > 0;
	}

	// ---- Counts for Snapshot / Projects ----

	projectBoardCounts(nowMs: number): Map<string, ProjectBoardCounts> {
		const out = new Map<string, ProjectBoardCounts>();
		const get = (name: string): ProjectBoardCounts => {
			let c = out.get(name);
			if (!c) {
				c = { threads: 0, openTasks: 0, readyTasks: 0, blockedTasks: 0 };
				out.set(name, c);
			}
			return c;
		};
		const rows = this.sql
			.exec(
				`SELECT p.name,
				        COUNT(*) AS threads,
				        SUM(CASE WHEN m.has_task = 1 AND m.deleted = 0 AND m.task_state IN ('waiting','ready','claimed','blocked') THEN 1 ELSE 0 END) AS open_tasks,
				        SUM(CASE WHEN m.has_task = 1 AND m.deleted = 0 AND (m.task_state = 'ready' OR (m.task_state = 'claimed' AND m.task_lease_until IS NOT NULL AND m.task_lease_until <= ?)) THEN 1 ELSE 0 END) AS ready_tasks,
				        SUM(CASE WHEN m.has_task = 1 AND m.deleted = 0 AND m.task_state = 'blocked' THEN 1 ELSE 0 END) AS blocked_tasks
				 FROM board_messages m
				 JOIN projects p ON p.id = m.project_id
				 WHERE m.re IS NULL AND m.deleted = 0
				 GROUP BY p.name`,
				nowMs,
			)
			.toArray();
		for (const r of rows) {
			const c = get(str(r.name));
			c.threads = num(r.threads);
			c.openTasks = num(r.open_tasks);
			c.readyTasks = num(r.ready_tasks);
			c.blockedTasks = num(r.blocked_tasks);
		}
		return out;
	}

	// ---- Import from existing @vercel/board ----

	importBoard(req: ImportBoardRequest, nowMs: number, importerLogin: string): ImportBoardResponse {
		return this.tx(() => {
			let actorsImported = 0;
			let schemasImported = 0;
			let messagesImported = 0;
			let messagesSkipped = 0;
			let tasksImported = 0;
			let projectsCreated = 0;
			const idMap: Record<string, bigint> = {};

			// 1. Actors
			for (const a of req.actors) {
				if (!a.id) continue;
				this.upsertActor(a, nowMs);
				actorsImported++;
			}

			// 2. Schemas
			for (const s of req.schemas) {
				if (isBuiltInName(s.name)) continue;
				const { created } = this.createSchema(
					s.name,
					s.version,
					s.description,
					s.schemaJson,
					importerLogin || "import",
					nowMs,
				);
				if (created) schemasImported++;
			}

			// Helper: resolve or create project
			const projectIdCache = new Map<string, number>();
			const resolveProject = (name: string): number => {
				const cached = projectIdCache.get(name);
				if (cached !== undefined) return cached;
				const rows = this.sql.exec("SELECT id FROM projects WHERE name = ?", name).toArray();
				if (rows.length > 0) {
					const pid = num(rows[0].id);
					projectIdCache.set(name, pid);
					return pid;
				}
				if (!req.createProjects) {
					throw hotbedError(
						ErrorCode.NotFound,
						`project ${name} does not exist (pass --create-projects or --map channel=project)`,
					).withSuggestion("hotbed project ls");
				}
				this.projects.add(
					name,
					[],
					create(StampSchema, {
						at: BigInt(nowMs),
						host: importerLogin || "import",
						reason: "created during board import",
					}),
				);
				projectsCreated++;
				const pid = this.projects.idByName(name);
				projectIdCache.set(name, pid);
				return pid;
			};

			// Helper: lookup already-imported external_id -> local id
			const lookupExt = (extId: string): number | null => {
				if (!extId) return null;
				const inMap = idMap[extId];
				if (inMap !== undefined) return Number(inMap);
				const rows = this.sql
					.exec("SELECT id FROM board_messages WHERE external_id = ?", extId)
					.toArray();
				if (rows.length > 0) {
					const local = num(rows[0].id);
					idMap[extId] = BigInt(local);
					return local;
				}
				return null;
			};

			// 3. Messages (roots and replies in chronological order)
			let maxInsertedId = 0;
			const touchedRoots = new Set<number>();

			for (const m of req.messages) {
				const extId = m.externalId.trim();
				if (extId) {
					const existing = lookupExt(extId);
					if (existing !== null) {
						messagesSkipped++;
						continue;
					}
				}
				const projectId = resolveProject(m.project);
				if (m.author?.id) {
					this.upsertActor(m.author, Number(m.createdAt) || nowMs);
				}

				const rootId = m.reExternalId ? lookupExt(m.reExternalId) : null;
				if (m.reExternalId && rootId === null) {
					throw hotbedError(
						ErrorCode.InvalidArgs,
						`reply ${extId} references root ${m.reExternalId}, which has not been imported yet (import roots before replies)`,
					);
				}
				const replyToId = m.replyToExternalId ? lookupExt(m.replyToExternalId) : null;

				// Preserve original numeric ID if preferredId > 0 and unused.
				let explicitId: number | null = null;
				const pref = Number(m.preferredId);
				if (pref > 0) {
					const clash = this.sql.exec("SELECT 1 FROM board_messages WHERE id = ?", pref).toArray();
					if (clash.length === 0) explicitId = pref;
				}

				const parsedBlocks = blocksIn(m.text).map((b) =>
					create(BoardBlockSchema, { schema: b.schema, version: b.version, dataJson: b.dataJson }),
				);
				const labels: Record<string, string> = { ...m.labels };
				let taskProto: BoardTask | undefined;
				let hasTask = 0;
				let taskStateStr: string | null = null;
				let taskOwner: string | null = null;
				let taskFor: string | null = null;
				let taskLeaseUntil: number | null = null;

				if (m.task && rootId === null) {
					const st =
						m.task.state === BoardTaskState.UNSPECIFIED ? BoardTaskState.READY : m.task.state;
					const resolvedNeeds: bigint[] = [];
					for (const needExt of m.task.needsExternalIds) {
						const local = lookupExt(needExt);
						if (local !== null) resolvedNeeds.push(BigInt(local));
					}
					const claimView =
						st === BoardTaskState.CLAIMED && m.task.claimActor
							? create(BoardTaskClaimViewSchema, {
									actor: m.task.claimActor,
									until: m.task.claimUntil,
								})
							: undefined;
					const blockedView =
						st === BoardTaskState.BLOCKED
							? create(BoardTaskBlockedSchema, {
									kind: m.task.blockedKind,
									reason: m.task.blockedReason,
								})
							: undefined;
					taskProto = create(BoardTaskSchema, {
						state: st,
						owner: m.task.owner || m.by?.principal || "",
						by: m.task.by || m.by?.actor || "",
						forActor: m.task.forActor,
						needs: resolvedNeeds,
						epoch: m.task.epoch,
						claim: claimView,
						blocked: blockedView,
						result: m.task.result,
						reason: m.task.reason,
					});
					hasTask = 1;
					taskStateStr = taskStateName(st);
					taskOwner = taskProto.owner;
					taskFor = taskProto.forActor || null;
					taskLeaseUntil = claimView ? Number(claimView.until) : null;
					if (!m.deleted) labels[TASK_LABEL] = taskStateStr;
					tasksImported++;
				}

				const createdAt = Number(m.createdAt) || nowMs;
				const msg = create(BoardMessageSchema, {
					id: BigInt(explicitId ?? 0),
					project: m.project,
					text: m.deleted ? "" : m.text,
					re: BigInt(rootId ?? 0),
					replyTo: BigInt(replyToId ?? 0),
					labels: m.deleted ? {} : labels,
					reactions: m.deleted ? [] : m.reactions,
					by: m.by,
					author: m.author,
					createdAt: BigInt(createdAt),
					editedAt: m.editedAt,
					deleted: m.deleted,
					attachments: m.deleted ? [] : m.attachments,
					blocks: m.deleted ? [] : parsedBlocks,
					task: taskProto,
					version: 1n,
					revision: 1n,
					externalId: extId,
				});

				let id: number;
				if (explicitId !== null) {
					this.sql.exec(
						`INSERT INTO board_messages
						 (id, project_id, external_id, re, reply_to, by_actor, by_principal, by_profile,
						  text_lower, deleted, created_at, edited_at, replies_count, last_activity_id, last_activity_at,
						  version, revision, has_task, task_state, task_owner, task_for, task_lease_until, doc)
						 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, 1, 1, ?, ?, ?, ?, ?, ?)`,
						explicitId,
						projectId,
						extId || null,
						rootId,
						replyToId,
						cleanSql(m.by?.actor ?? ""),
						cleanSql(m.by?.principal ?? ""),
						m.by?.profile ? cleanSql(m.by.profile) : null,
						cleanSql(msg.text.toLowerCase()),
						m.deleted ? 1 : 0,
						createdAt,
						m.editedAt !== undefined ? Number(m.editedAt) : null,
						explicitId,
						createdAt,
						hasTask,
						taskStateStr,
						taskOwner,
						taskFor,
						taskLeaseUntil,
						marshalMsg(msg),
					);
					id = explicitId;
				} else {
					const ins = this.sql
						.exec(
							`INSERT INTO board_messages
							 (project_id, external_id, re, reply_to, by_actor, by_principal, by_profile,
							  text_lower, deleted, created_at, edited_at, replies_count, last_activity_id, last_activity_at,
							  version, revision, has_task, task_state, task_owner, task_for, task_lease_until, doc)
							 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, 1, 1, ?, ?, ?, ?, ?, ?)
							 RETURNING id`,
							projectId,
							extId || null,
							rootId,
							replyToId,
							cleanSql(m.by?.actor ?? ""),
							cleanSql(m.by?.principal ?? ""),
							m.by?.profile ? cleanSql(m.by.profile) : null,
							cleanSql(msg.text.toLowerCase()),
							m.deleted ? 1 : 0,
							createdAt,
							m.editedAt !== undefined ? Number(m.editedAt) : null,
							createdAt,
							hasTask,
							taskStateStr,
							taskOwner,
							taskFor,
							taskLeaseUntil,
							marshalMsg(msg),
						)
						.toArray();
					id = num(ins[0].id);
					msg.id = BigInt(id);
					this.sql.exec(
						"UPDATE board_messages SET last_activity_id = ?, doc = ? WHERE id = ?",
						id,
						marshalMsg(msg),
						id,
					);
				}

				if (id > maxInsertedId) maxInsertedId = id;
				if (extId) idMap[extId] = BigInt(id);
				if (rootId !== null) touchedRoots.add(rootId);
				this.reindexTerms(msg);

				// Record any unresolved task needs for the finalize pass
				if (m.task && rootId === null && m.task.needsExternalIds.length > 0) {
					for (const needExt of m.task.needsExternalIds) {
						if (lookupExt(needExt) === null) {
							this.sql.exec(
								"INSERT OR IGNORE INTO board_import_needs (message_id, need_external_id) VALUES (?, ?)",
								id,
								needExt,
							);
						}
					}
				}
				messagesImported++;
			}

			// Recompute replies_count + last_activity for any roots that received replies
			for (const rootId of touchedRoots) {
				const stats = this.sql
					.exec(
						`SELECT COUNT(*) AS cnt, MAX(id) AS last_id, MAX(created_at) AS last_at
						 FROM board_messages WHERE re = ? AND deleted = 0`,
						rootId,
					)
					.toArray()[0];
				const cnt = num(stats?.cnt);
				const rootRow = this.getMessageRow(rootId);
				const lastId = cnt > 0 ? num(stats?.last_id) : rootId;
				const lastAt = cnt > 0 ? num(stats?.last_at) : Number(rootRow.msg.createdAt);
				this.sql.exec(
					"UPDATE board_messages SET replies_count = ?, last_activity_id = ?, last_activity_at = ? WHERE id = ?",
					cnt,
					lastId,
					lastAt,
					rootId,
				);
			}

			// 4. Finalize cross-batch task needs
			if (req.finalizeNeeds) {
				const pending = this.sql
					.exec("SELECT message_id, need_external_id FROM board_import_needs")
					.toArray();
				const byMsg = new Map<number, number[]>();
				for (const p of pending) {
					const mid = num(p.message_id);
					const local = lookupExt(str(p.need_external_id));
					if (local === null) continue;
					const list = byMsg.get(mid) ?? [];
					list.push(local);
					byMsg.set(mid, list);
				}
				for (const [mid, resolvedIds] of byMsg) {
					const row = this.getMessageRow(mid);
					if (!row.msg.task) continue;
					const existing = new Set(row.msg.task.needs.map((n) => Number(n)));
					for (const nid of resolvedIds) existing.add(nid);
					row.msg.task.needs = [...existing].map((n) => BigInt(n));
					this.persistMessage(row.msg);
				}
				this.sql.exec("DELETE FROM board_import_needs");
			}

			// Advance sqlite_sequence past maxInsertedId if needed
			if (maxInsertedId > 0) {
				const seqRows = this.sql
					.exec("SELECT seq FROM sqlite_sequence WHERE name = 'board_messages'")
					.toArray();
				if (seqRows.length === 0) {
					this.sql.exec(
						"INSERT INTO sqlite_sequence (name, seq) VALUES ('board_messages', ?)",
						maxInsertedId,
					);
				} else if (num(seqRows[0].seq) < maxInsertedId) {
					this.sql.exec(
						"UPDATE sqlite_sequence SET seq = ? WHERE name = 'board_messages'",
						maxInsertedId,
					);
				}
			}

			return create(ImportBoardResponseSchema, {
				actorsImported,
				schemasImported,
				messagesImported,
				messagesSkipped,
				tasksImported,
				projectsCreated,
				idMap,
			});
		});
	}
}
