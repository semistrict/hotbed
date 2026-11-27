// SqlKernel: the one way ControllerDO's SQLite is reached. Each domain store
// (store/*.ts) is built over it and holds its own tables' SQL and invariants.
import {
	create,
	fromJsonString,
	toJsonString,
	type DescMessage,
	type MessageShape,
} from "@bufbuild/protobuf";
import type { DrizzleSqliteDODatabase } from "drizzle-orm/durable-sqlite";
import type * as dbSchema from "../db/schema.ts";
import { ErrorCode, hotbedError } from "../errors.ts";
import { StampSchema, type Stamp } from "../gen/hotbed/v1/hotbed_pb.ts";
import { big, num, str } from "../sql-values.ts";

export type Row = Record<string, SqlStorageValue>;

/**
 * A bind parameter as DO SQLite takes it. SQLite has no JS BigInt binding,
 * and protobuf int64/uint64 fields are bigints: one Number holds exactly is
 * bound as a Number, and one it cannot hold throws rather than binding a
 * neighbouring integer (a corrupted ID, rank or counter).
 */
export function bindValue(b: unknown): unknown {
	if (typeof b !== "bigint") return b;
	if (b > BigInt(Number.MAX_SAFE_INTEGER) || b < BigInt(Number.MIN_SAFE_INTEGER)) {
		throw hotbedError(
			ErrorCode.Internal,
			`${b} does not fit a SQLite number exactly; store it as text`,
		);
	}
	return Number(b);
}

export class SqlKernel {
	readonly db: DrizzleSqliteDODatabase<typeof dbSchema>;
	private readonly storage: DurableObjectStorage;

	constructor(storage: DurableObjectStorage, db: DrizzleSqliteDODatabase<typeof dbSchema>) {
		this.storage = storage;
		this.db = db;
	}

	exec<T extends Row = Row>(query: string, ...bindings: unknown[]): SqlStorageCursor<T> {
		return this.storage.sql.exec<T>(query, ...bindings.map(bindValue));
	}

	rows(query: string, ...bindings: unknown[]): Row[] {
		return this.exec(query, ...bindings).toArray();
	}

	/** The query's first row, if any. */
	first(query: string, ...bindings: unknown[]): Row | undefined {
		return this.rows(query, ...bindings)[0];
	}

	/** Runs an INSERT and returns the row ID it gave the new row. */
	insert(query: string, ...bindings: unknown[]): number {
		this.exec(query, ...bindings);
		return num(this.exec("SELECT last_insert_rowid() AS id").one().id);
	}

	tx<T>(fn: () => T): T {
		return this.storage.transactionSync(fn);
	}
}

export function marshal<T extends DescMessage>(schema: T, msg: MessageShape<T>): string {
	return toJsonString(schema, msg);
}

export function unmarshal<T extends DescMessage>(schema: T, doc: string): MessageShape<T> {
	return fromJsonString(schema, doc || "{}", { ignoreUnknownFields: true });
}

export function optBig(v: unknown): bigint | undefined {
	if (v === null || v === undefined) return undefined;
	return big(v);
}

/** IDs as SQL placeholders, a chunk at a time: SQLite caps a statement's bindings. */
export function* idChunks(ids: string[]): Generator<{ ids: string[]; ph: string }> {
	const unique = [...new Set(ids)];
	for (let i = 0; i < unique.length; i += 80) {
		const chunk = unique.slice(i, i + 80);
		yield { ids: chunk, ph: chunk.map(() => "?").join(",") };
	}
}

// ---- audit trails ---------------------------------------------------------

/** A table whose every save appends a full copy to its <name>_changes table. */
export interface AuditTrail {
	/** The changes table, e.g. question_changes. */
	table: string;
	/** Its column naming the changed row, e.g. question_id. */
	key: string;
}

export interface AuditChange {
	seq: bigint;
	what: string;
	by: Stamp;
	doc: string;
}

export function appendAuditChange(
	sql: SqlKernel,
	trail: AuditTrail,
	id: number,
	what: string,
	by: Stamp,
	doc: string,
): void {
	sql.exec(
		`INSERT INTO ${trail.table} (${trail.key}, what, at, session, host, reason, doc) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id,
		what,
		by.at,
		by.session,
		by.host,
		by.reason,
		doc,
	);
}

export function auditChanges(sql: SqlKernel, trail: AuditTrail, id: number): AuditChange[] {
	return sql
		.rows(
			`SELECT seq, what, at, session, host, reason, doc FROM ${trail.table} WHERE ${trail.key} = ? ORDER BY seq`,
			id,
		)
		.map((r) => ({
			seq: big(r.seq),
			what: str(r.what),
			by: create(StampSchema, {
				at: big(r.at),
				session: str(r.session),
				host: str(r.host),
				reason: str(r.reason),
			}),
			doc: str(r.doc),
		}));
}

// ---- ranked lists ---------------------------------------------------------

export interface RankedItem {
	id: number;
	rank: bigint;
}

/** A table whose rows people order by hand: rank ascending, the first on top. */
export interface RankedList<T extends RankedItem> {
	table: string;
	/** Which of the table's rows share one ranking, as a WHERE condition. */
	scope: string;
	scopeArgs: SqlStorageValue[];
	get(id: number): T;
	/** The ID of the row that `first` puts item before: the top of its list. */
	topOf(item: T): number | undefined;
	/** Loads id, applies edit and saves it with its audit trail, in the caller's transaction. */
	change(id: number, what: string, by: Stamp, edit: (item: T) => void): T;
}

/** The top row's rank (0 for none): a new or reopened row goes above it. */
export function topRank<T extends RankedItem>(sql: SqlKernel, list: RankedList<T>): number {
	return num(
		sql
			.exec(
				`SELECT COALESCE(MIN(rank), 0) AS m FROM ${list.table} WHERE ${list.scope}`,
				...list.scopeArgs,
			)
			.one().m,
	);
}

/**
 * Moves id to just before place.before, to the top (place.first), or to the
 * bottom (neither), shifting the rows below down one. check sees the item
 * and the one it lands before, and throws to refuse the move.
 */
export function moveRankedItem<T extends RankedItem>(
	sql: SqlKernel,
	list: RankedList<T>,
	id: number,
	place: { before?: number; first?: boolean },
	by: Stamp,
	check: (item: T, before: T | null) => void,
): T {
	return sql.tx(() => {
		let beforeId = place.before ?? 0;
		if (place.first) {
			const item = list.get(id);
			const top = list.topOf(item);
			if (top === undefined || top === id) {
				check(item, null);
				return item;
			}
			beforeId = top;
		}
		const next = beforeId !== 0 ? list.get(beforeId) : null;
		return list.change(id, "moved", by, (item) => {
			check(item, next);
			if (!next) {
				const maxRank = num(
					sql
						.exec(
							`SELECT COALESCE(MAX(rank), 0) AS m FROM ${list.table} WHERE ${list.scope}`,
							...list.scopeArgs,
						)
						.one().m,
				);
				item.rank = BigInt(maxRank + 1);
			} else {
				sql.exec(
					`UPDATE ${list.table} SET rank = rank + 1 WHERE rank >= ? AND id != ? AND ${list.scope}`,
					next.rank,
					item.id,
					...list.scopeArgs,
				);
				item.rank = next.rank;
			}
		});
	});
}
