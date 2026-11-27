import * as dbSchema from "../db/schema.ts";
import { eq } from "drizzle-orm";
import type { SqlKernel } from "./kernel.ts";
import type { MetaStore } from "./meta.ts";

/** What admits callers: worker, join and API tokens (by hash), sign-up codes and the users they approved. */
export class TokenStore {
	constructor(
		private readonly sql: SqlKernel,
		private readonly meta: MetaStore,
	) {}

	putWorkerToken(tokenHash: string, nodeId: string, hostname: string, nowMs: number): void {
		this.sql.db
			.insert(dbSchema.workerTokens)
			.values({ tokenHash, nodeId, hostname, createdAt: nowMs, revokedAt: null })
			.onConflictDoUpdate({
				target: dbSchema.workerTokens.tokenHash,
				set: { nodeId, hostname, revokedAt: null },
			})
			.run();
	}

	findWorkerToken(tokenHash: string): { nodeId: string; hostname: string } | null {
		const row = this.sql.db
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
		this.sql.db
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
		this.sql.db
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
		const row = this.sql.db
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
		return this.sql.db
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
		const existing = this.sql.db
			.select({ id: dbSchema.joinTokens.id, revokedAt: dbSchema.joinTokens.revokedAt })
			.from(dbSchema.joinTokens)
			.where(eq(dbSchema.joinTokens.id, id))
			.get();
		if (!existing) return false;
		if (existing.revokedAt === null) {
			this.sql.db
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
		if (this.meta.get("signup_codes_seeded") !== null) return;
		this.sql.db
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
		this.meta.set("signup_codes_seeded", "1");
	}

	isUserSignupApproved(userId: string): boolean {
		const row = this.sql.db
			.select({ userId: dbSchema.approvedUsers.userId })
			.from(dbSchema.approvedUsers)
			.where(eq(dbSchema.approvedUsers.userId, userId))
			.get();
		return Boolean(row);
	}

	countApprovedUsers(): number {
		return this.sql.db
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
		return this.sql.db
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
		this.sql.db
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
		const existing = this.sql.db
			.select({ code: dbSchema.signupCodes.code, revokedAt: dbSchema.signupCodes.revokedAt })
			.from(dbSchema.signupCodes)
			.where(eq(dbSchema.signupCodes.code, normalized))
			.get();
		if (!existing) return false;
		if (existing.revokedAt === null) {
			this.sql.db
				.update(dbSchema.signupCodes)
				.set({ revokedAt: nowMs })
				.where(eq(dbSchema.signupCodes.code, normalized))
				.run();
		}
		return true;
	}

	recordSignupRedemption(userId: string, email: string, matchedCode: string, nowMs: number): void {
		this.sql.tx(() => {
			const codeRow = this.sql.db
				.select({ useCount: dbSchema.signupCodes.useCount })
				.from(dbSchema.signupCodes)
				.where(eq(dbSchema.signupCodes.code, matchedCode))
				.get();
			if (codeRow) {
				this.sql.db
					.update(dbSchema.signupCodes)
					.set({ useCount: codeRow.useCount + 1 })
					.where(eq(dbSchema.signupCodes.code, matchedCode))
					.run();
			}
			this.sql.db
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
		this.sql.db
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
		const row = this.sql.db
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
			this.sql.db
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
		const q = this.sql.db
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
		const row = this.sql.db
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
			this.sql.db
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
}
