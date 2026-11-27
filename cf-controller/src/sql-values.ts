// Values read back from Durable Object SQLite (SqlStorageValue, or what a
// typed query declares), as the type a column holds.

export function num(v: unknown): number {
	return typeof v === "number" ? v : typeof v === "bigint" ? Number(v) : Number(v ?? 0);
}

export function big(v: unknown): bigint {
	return typeof v === "bigint" ? v : BigInt(Math.trunc(Number(v ?? 0)));
}

export function str(v: unknown): string {
	if (typeof v === "string") return v;
	if (typeof v === "number" || typeof v === "bigint" || typeof v === "boolean") return String(v);
	return "";
}
