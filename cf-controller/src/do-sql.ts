// Read-only SQL execution and schema inspection helpers shared across
// org-scoped SQLite-backed Durable Objects (ControllerDO and CommandDO).

export type SqlParamValue = string | number | boolean | null;
export type SqlCellValue = string | number | boolean | null;

export interface DoSqlResult {
	columns: string[];
	rows: Record<string, SqlCellValue>[];
	rowCount: number;
	truncated: boolean;
	rowsRead: number;
	rowsWritten: number;
	durationMs: number;
	databaseSizeBytes: number;
}

export type DoSqlRpcResult = { ok: true; result: DoSqlResult } | { ok: false; error: string };

/**
 * ControllerDO tables holding plaintext secrets or active auth handoffs that
 * must never be exposed via Admin Explorer SQL or schema inspection.
 */
export const CONTROLLER_ADMIN_DENIED_TABLES: ReadonlySet<string> = new Set([
	"git_credentials",
	"signup_codes",
	"kv_meta",
]);

export interface DoTableColumn {
	cid: number;
	name: string;
	type: string;
	notNull: boolean;
	defaultValue: string | null;
	primaryKey: number;
}

export interface DoTableInfo {
	name: string;
	type: "table" | "view";
	rowCount: number | null;
	columns: DoTableColumn[];
	sql: string;
	internal: boolean;
}

export interface DoIndexInfo {
	name: string;
	tableName: string;
	sql: string;
}

export interface DoSchemaInfo {
	databaseSizeBytes: number;
	alarmAtMs: number | null;
	tables: DoTableInfo[];
	indexes: DoIndexInfo[];
}

export const DEFAULT_MAX_SQL_ROWS = 100;
export const HARD_MAX_SQL_ROWS = 500;
const MAX_CELL_STRING_CHARS = 8192;
const MAX_BLOB_PREVIEW_BYTES = 32;

// Zero-argument read-only PRAGMAs (no `(...)` argument form permitted).
const ZERO_ARG_READONLY_PRAGMAS = new Set([
	"database_list",
	"page_count",
	"page_size",
	"freelist_count",
	"compile_options",
	"encoding",
]);

// Table/index introspection PRAGMAs that may take a single parenthesized name.
const TABLE_INTROSPECTION_PRAGMAS = new Set([
	"table_info",
	"table_xinfo",
	"table_list",
	"index_list",
	"index_info",
	"index_xinfo",
	"foreign_key_list",
]);

const FORBIDDEN_KEYWORDS = new Set([
	"INSERT",
	"UPDATE",
	"DELETE",
	"REPLACE",
	"CREATE",
	"DROP",
	"ALTER",
	"ATTACH",
	"DETACH",
	"VACUUM",
	"REINDEX",
	"BEGIN",
	"COMMIT",
	"ROLLBACK",
	"SAVEPOINT",
	"RELEASE",
	"RECURSIVE",
	"RANDOMBLOB",
	"ZEROBLOB",
	"LOAD_EXTENSION",
]);

interface SqlToken {
	kind: "word" | "symbol" | "string" | "ident" | "number";
	value: string;
}

/**
 * Minimal SQLite lexer that strips comments and separates identifiers, literals,
 * words, and punctuation so read-only validation cannot be bypassed via comments
 * or multi-statement smuggling.
 */
function tokenizeSql(input: string): SqlToken[] {
	const tokens: SqlToken[] = [];
	let i = 0;
	const n = input.length;

	while (i < n) {
		const ch = input[i];

		// Whitespace
		if (/\s/.test(ch)) {
			i++;
			continue;
		}

		// Line comment: -- ...
		if (ch === "-" && input[i + 1] === "-") {
			i += 2;
			while (i < n && input[i] !== "\n") i++;
			continue;
		}

		// Block comment: /* ... */
		if (ch === "/" && input[i + 1] === "*") {
			i += 2;
			let closed = false;
			while (i + 1 < n) {
				if (input[i] === "*" && input[i + 1] === "/") {
					i += 2;
					closed = true;
					break;
				}
				i++;
			}
			if (!closed) {
				throw new Error("unterminated block comment in SQL query");
			}
			continue;
		}

		// Single-quoted string literal
		if (ch === "'") {
			let out = "";
			i++;
			let closed = false;
			while (i < n) {
				if (input[i] === "'") {
					if (input[i + 1] === "'") {
						out += "'";
						i += 2;
						continue;
					}
					i++;
					closed = true;
					break;
				}
				out += input[i++];
			}
			if (!closed) throw new Error("unterminated string literal in SQL query");
			tokens.push({ kind: "string", value: out });
			continue;
		}

		// Double-quoted or backtick-quoted identifier
		if (ch === '"' || ch === "`") {
			const quote = ch;
			let out = "";
			i++;
			let closed = false;
			while (i < n) {
				if (input[i] === quote) {
					if (input[i + 1] === quote) {
						out += quote;
						i += 2;
						continue;
					}
					i++;
					closed = true;
					break;
				}
				out += input[i++];
			}
			if (!closed) throw new Error("unterminated quoted identifier in SQL query");
			tokens.push({ kind: "ident", value: out });
			continue;
		}

		// Bracket-quoted identifier: [...]
		if (ch === "[") {
			let out = "";
			i++;
			let closed = false;
			while (i < n) {
				if (input[i] === "]") {
					i++;
					closed = true;
					break;
				}
				out += input[i++];
			}
			if (!closed) throw new Error("unterminated bracket identifier in SQL query");
			tokens.push({ kind: "ident", value: out });
			continue;
		}

		// Word / keyword / unquoted identifier
		if (/[A-Za-z_]/.test(ch)) {
			let start = i;
			while (i < n && /[A-Za-z0-9_$]/.test(input[i])) i++;
			tokens.push({ kind: "word", value: input.slice(start, i) });
			continue;
		}

		// Number
		if (/[0-9]/.test(ch)) {
			let start = i;
			while (i < n && /[0-9.eE+-]/.test(input[i])) i++;
			tokens.push({ kind: "number", value: input.slice(start, i) });
			continue;
		}

		// Single punctuation symbol
		tokens.push({ kind: "symbol", value: ch });
		i++;
	}

	return tokens;
}

/**
 * Validates that a SQL query is a single read-only statement and does not
 * reference any denied sensitive tables.
 * Throws a descriptive Error if the query is empty, multi-statement, could
 * mutate state, could exhaust CPU/memory via recursive CTEs / huge blobs, or
 * accesses a restricted table.
 */
export function validateReadOnlySql(
	rawQuery: string,
	opts?: { deniedTables?: ReadonlySet<string> },
): string {
	const query = rawQuery.trim();
	if (!query) {
		throw new Error("SQL query must not be empty");
	}
	const tokens = tokenizeSql(query);
	if (tokens.length === 0) {
		throw new Error("SQL query must contain a statement");
	}

	// Reject multiple statements (a single optional trailing semicolon is allowed).
	const semiIdx = tokens.findIndex((t) => t.kind === "symbol" && t.value === ";");
	if (semiIdx !== -1 && semiIdx !== tokens.length - 1) {
		throw new Error("only a single SQL statement is allowed per query");
	}
	const body = semiIdx === -1 ? tokens : tokens.slice(0, semiIdx);
	if (body.length === 0) {
		throw new Error("SQL query must contain a statement");
	}

	// Check for any forbidden keyword or denied sensitive table anywhere in the statement.
	// Checking every word/ident token is sound because SQLite does not support dynamic table names.
	const denied = opts?.deniedTables;
	for (const tok of body) {
		if (tok.kind === "word" && FORBIDDEN_KEYWORDS.has(tok.value.toUpperCase())) {
			throw new Error(
				`keyword "${tok.value.toUpperCase()}" is not permitted; admin SQL is strictly read-only and bounded`,
			);
		}
		if (denied && (tok.kind === "word" || tok.kind === "ident")) {
			const lower = tok.value.toLowerCase();
			if (denied.has(lower)) {
				throw new Error(
					`table "${lower}" holds sensitive credentials or auth state and cannot be queried via admin SQL`,
				);
			}
		}
	}

	const first = body[0].kind === "word" ? body[0].value.toUpperCase() : "";
	if (first === "SELECT" || first === "WITH" || first === "VALUES") {
		return query;
	}

	if (first === "EXPLAIN") {
		if (body.length < 2) {
			throw new Error("EXPLAIN must be followed by a read-only query");
		}
		return query;
	}

	if (first === "PRAGMA") {
		// Reject PRAGMA assignments (`PRAGMA x = y`).
		if (body.some((t) => t.kind === "symbol" && t.value === "=")) {
			throw new Error("PRAGMA assignment is not permitted; admin SQL is read-only");
		}
		// Allow `PRAGMA name` or `PRAGMA main.name` or `PRAGMA table_info(...)`.
		let nameIdx = 1;
		if (body[2]?.kind === "symbol" && body[2].value === ".") {
			nameIdx = 3;
		}
		const pragmaTok = body[nameIdx];
		if (!pragmaTok || (pragmaTok.kind !== "word" && pragmaTok.kind !== "ident")) {
			throw new Error("invalid PRAGMA syntax");
		}
		const pragmaName = pragmaTok.value.toLowerCase();
		const rest = body.slice(nameIdx + 1);

		if (ZERO_ARG_READONLY_PRAGMAS.has(pragmaName)) {
			if (rest.length > 0) {
				throw new Error(
					`PRAGMA "${pragmaName}" takes no arguments in read-only mode (function-call setter syntax is not permitted)`,
				);
			}
			return query;
		}
		if (TABLE_INTROSPECTION_PRAGMAS.has(pragmaName)) {
			// Permit either `PRAGMA table_list` (no args) or `PRAGMA table_info('tbl')` (single arg).
			if (rest.length === 0) return query;
			if (
				rest.length === 3 &&
				rest[0].kind === "symbol" &&
				rest[0].value === "(" &&
				(rest[1].kind === "string" || rest[1].kind === "word" || rest[1].kind === "ident") &&
				rest[2].kind === "symbol" &&
				rest[2].value === ")"
			) {
				if (denied && denied.has(rest[1].value.toLowerCase())) {
					throw new Error(
						`table "${rest[1].value.toLowerCase()}" holds sensitive credentials or auth state and cannot be inspected via admin SQL`,
					);
				}
				return query;
			}
			throw new Error(`PRAGMA "${pragmaName}" only accepts a single table/index name argument`);
		}
		throw new Error(
			`PRAGMA "${pragmaName}" is not in the read-only allowlist (${[...ZERO_ARG_READONLY_PRAGMAS, ...TABLE_INTROSPECTION_PRAGMAS].join(", ")})`,
		);
	}

	throw new Error(
		`statement starting with "${first || body[0].value}" is not permitted; only SELECT, WITH, VALUES, EXPLAIN, and read-only PRAGMA queries are allowed`,
	);
}

function normalizeSqlParam(value: SqlParamValue): SqlStorageValue {
	if (value === null || value === undefined) return null;
	if (typeof value === "string" || typeof value === "number") return value;
	if (typeof value === "boolean") return value ? 1 : 0;
	return String(value);
}

function formatCellValue(value: SqlStorageValue | undefined): SqlCellValue {
	if (value === null || value === undefined) return null;
	if (typeof value === "number") return value;
	if (typeof value === "string") {
		if (value.length <= MAX_CELL_STRING_CHARS) return value;
		return `${value.slice(0, MAX_CELL_STRING_CHARS)}… [truncated ${value.length - MAX_CELL_STRING_CHARS} chars]`;
	}
	if (value instanceof ArrayBuffer) {
		const bytes = new Uint8Array(value);
		const preview = Array.from(bytes.slice(0, MAX_BLOB_PREVIEW_BYTES))
			.map((b) => b.toString(16).padStart(2, "0"))
			.join("");
		return `<blob ${bytes.byteLength} bytes${bytes.byteLength > 0 ? ` 0x${preview}${bytes.byteLength > MAX_BLOB_PREVIEW_BYTES ? "…" : ""}` : ""}>`;
	}
	return String(value);
}

/**
 * Executes a validated read-only SQL query against a Durable Object's SqlStorage.
 */
export function executeReadOnlySql(
	sql: SqlStorage,
	rawQuery: string,
	rawParams: SqlParamValue[] = [],
	opts?: { maxRows?: number; deniedTables?: ReadonlySet<string> },
): DoSqlResult {
	const query = validateReadOnlySql(rawQuery, { deniedTables: opts?.deniedTables });
	const requestedMax = Number(opts?.maxRows);
	const maxRows =
		Number.isFinite(requestedMax) && requestedMax > 0
			? Math.max(1, Math.min(HARD_MAX_SQL_ROWS, Math.floor(requestedMax)))
			: DEFAULT_MAX_SQL_ROWS;
	const params = rawParams.map(normalizeSqlParam);

	const started = Date.now();
	const cursor = sql.exec(query, ...params);
	const columns = [...cursor.columnNames];
	const rows: Record<string, SqlCellValue>[] = [];
	let truncated = false;

	for (const rawRow of cursor) {
		if (rows.length >= maxRows) {
			truncated = true;
			break;
		}
		const formatted: Record<string, SqlCellValue> = {};
		for (const [k, v] of Object.entries(rawRow)) {
			formatted[k] = formatCellValue(v);
		}
		rows.push(formatted);
	}

	if (cursor.rowsWritten > 0) {
		throw new Error("query unexpectedly wrote rows; admin SQL is strictly read-only");
	}

	return {
		columns,
		rows,
		rowCount: rows.length,
		truncated,
		rowsRead: cursor.rowsRead,
		rowsWritten: cursor.rowsWritten,
		durationMs: Date.now() - started,
		databaseSizeBytes: sql.databaseSize,
	};
}

/**
 * RPC-safe wrapper around `executeReadOnlySql` that returns structured errors
 * instead of throwing uncaught exceptions across the Durable Object RPC boundary.
 */
export function executeReadOnlySqlRpc(
	sql: SqlStorage,
	rawQuery: string,
	rawParams: SqlParamValue[] = [],
	opts?: { maxRows?: number; deniedTables?: ReadonlySet<string> },
): DoSqlRpcResult {
	try {
		return { ok: true, result: executeReadOnlySql(sql, rawQuery, rawParams, opts) };
	} catch (err) {
		return { ok: false, error: err instanceof Error ? err.message : String(err) };
	}
}

function quoteIdent(name: string): string {
	return `"${name.replace(/"/g, '""')}"`;
}

/**
 * Introspects a Durable Object's SQLite schema (tables, columns, row counts,
 * indexes, database size, and current alarm) without creating any tables.
 */
export async function inspectDoSchema(
	sql: SqlStorage,
	storage: DurableObjectStorage,
	opts?: { deniedTables?: ReadonlySet<string> },
): Promise<DoSchemaInfo> {
	const denied = opts?.deniedTables;
	const alarmAtMs = await storage.getAlarm();
	const masterRows = sql
		.exec<{ type: string; name: string; tbl_name: string; sql: string | null }>(
			`SELECT type, name, tbl_name, sql
			 FROM sqlite_master
			 WHERE type IN ('table', 'view', 'index')
			 ORDER BY type ASC, name ASC`,
		)
		.toArray();

	const tables: DoTableInfo[] = [];
	const indexes: DoIndexInfo[] = [];

	for (const row of masterRows) {
		if (row.type === "index") {
			if (row.sql && !(denied && denied.has(String(row.tbl_name).toLowerCase()))) {
				indexes.push({
					name: String(row.name),
					tableName: String(row.tbl_name),
					sql: String(row.sql),
				});
			}
			continue;
		}

		const name = String(row.name);
		if (denied && denied.has(name.toLowerCase())) {
			continue;
		}
		const tableType = row.type === "view" ? "view" : "table";
		const internal = name.startsWith("_cf_") || name.startsWith("sqlite_");

		let columns: DoTableColumn[] = [];
		try {
			const colRows = sql
				.exec<{
					cid: number;
					name: string;
					type: string;
					notnull: number;
					dflt_value: string | null;
					pk: number;
				}>(`PRAGMA table_info(${quoteIdent(name)})`)
				.toArray();
			columns = colRows.map((c) => ({
				cid: Number(c.cid),
				name: String(c.name),
				type: String(c.type ?? ""),
				notNull: Number(c.notnull) !== 0,
				defaultValue:
					c.dflt_value === null || c.dflt_value === undefined ? null : String(c.dflt_value),
				primaryKey: Number(c.pk),
			}));
		} catch {
			columns = [];
		}

		let rowCount: number | null = null;
		// Skip COUNT(*) on internal tables and views (views may run arbitrary joins).
		if (!internal && tableType === "table") {
			try {
				const countRows = sql
					.exec<{ n: number }>(`SELECT COUNT(*) AS n FROM ${quoteIdent(name)}`)
					.toArray();
				rowCount = countRows.length > 0 ? Number(countRows[0].n) : 0;
			} catch {
				rowCount = null;
			}
		}

		tables.push({
			name,
			type: tableType,
			rowCount,
			columns,
			sql: row.sql ? String(row.sql) : "",
			internal,
		});
	}

	return {
		databaseSizeBytes: sql.databaseSize,
		alarmAtMs,
		tables,
		indexes,
	};
}
