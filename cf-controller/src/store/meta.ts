import { eq } from "drizzle-orm";
import * as dbSchema from "../db/schema.ts";
import type { SqlKernel } from "./kernel.ts";

/** kv_meta: the controller's own settings and one-off markers, by key. */
export class MetaStore {
	constructor(private readonly sql: SqlKernel) {}

	get(key: string): string | null {
		const row = this.sql.db
			.select({ value: dbSchema.kvMeta.value })
			.from(dbSchema.kvMeta)
			.where(eq(dbSchema.kvMeta.key, key))
			.get();
		return row?.value ?? null;
	}

	set(key: string, value: string): void {
		this.sql.db
			.insert(dbSchema.kvMeta)
			.values({ key, value })
			.onConflictDoUpdate({ target: dbSchema.kvMeta.key, set: { value } })
			.run();
	}

	prune(nowMs: number): void {
		this.sql.exec(
			"DELETE FROM kv_meta WHERE key LIKE 'cli_handoff:%' AND CAST(value AS INTEGER) < ?",
			nowMs,
		);
	}
}
