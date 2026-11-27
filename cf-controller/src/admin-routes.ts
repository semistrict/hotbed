// /admin/* REST routes, served inside ControllerDO: minting worker tokens,
// an organization's join tokens and personal API tokens, and the default
// organization's sign-up codes.
import {
	INTERNAL_DEFAULT_DO_HEADER,
	INTERNAL_GLOBAL_ADMIN_HEADER,
	INTERNAL_ORG_HEADER,
	sha256Hex,
	type CallerIdentity,
} from "./auth.ts";
import { decodePathSegment } from "./encoding.ts";
import { ErrorCode, fromUnknownError, type HotbedErrorCode } from "./errors.ts";
import { randomHex } from "./ids.ts";
import type { TokenStore } from "./store/tokens.ts";
import { formatOrgToken } from "./workos.ts";

export interface AdminRouteDeps {
	tokens: TokenStore;
	/** The organization the controller is bound to ("" when not yet bound). */
	orgId(): string;
	defaultOrgId: string;
	now(): number;
	authenticate(request: Request): Promise<CallerIdentity>;
}

function fail(
	code: HotbedErrorCode,
	message: string,
	status: number,
	suggestion?: string,
): Response {
	return Response.json({ code, message, ...(suggestion ? { suggestion } : {}) }, { status });
}

async function jsonBody<T>(request: Request): Promise<T> {
	return (await request.json().catch(() => ({}))) as T;
}

/** ms from now, for a positive finite count of units of unitMs; null (never) otherwise. */
function expiry(nowMs: number, count: unknown, unitMs: number): number | null {
	if (typeof count !== "number" || !Number.isFinite(count) || count <= 0) return null;
	return nowMs + Math.round(count * unitMs);
}

/**
 * Serves pathname if it is an /admin/* route, or returns null. Each route
 * authenticates first; an error it throws answers 403 when it refuses the
 * caller, 500 otherwise.
 */
export async function handleAdminRoute(
	request: Request,
	pathname: string,
	deps: AdminRouteDeps,
): Promise<Response | null> {
	const route = matchRoute(pathname, request.method);
	if (!route) return null;
	try {
		const caller = await deps.authenticate(request);
		return await route(request, pathname, caller, deps);
	} catch (err) {
		const he = fromUnknownError(err);
		return Response.json(he.toJSON(), { status: he.code === ErrorCode.NotAuthorized ? 403 : 500 });
	}
}

type Route = (
	request: Request,
	pathname: string,
	caller: CallerIdentity,
	deps: AdminRouteDeps,
) => Promise<Response>;

function matchRoute(pathname: string, method: string): Route | null {
	if (pathname === "/admin/worker-token") return method === "POST" ? workerToken : null;
	for (const [prefix, route] of [
		["/admin/join-tokens", joinTokens],
		["/admin/api-tokens", apiTokens],
		["/admin/signup-codes", signupCodes],
	] as const) {
		if (pathname === prefix || pathname.startsWith(`${prefix}/`)) return route;
	}
	return null;
}

const methodNotAllowed = () => new Response("Method Not Allowed", { status: 405 });

/** POST /admin/worker-token: a token pinned to one node, minted by an organization member. */
async function workerToken(
	request: Request,
	_pathname: string,
	caller: CallerIdentity,
	deps: AdminRouteDeps,
): Promise<Response> {
	if (caller.role !== "owner") {
		return fail(ErrorCode.NotAuthorized, "owner token required", 403);
	}
	if (caller.orgRole === "viewer") {
		return fail(
			ErrorCode.NotAuthorized,
			"viewer role is read-only and cannot mint worker tokens",
			403,
		);
	}
	const body = await jsonBody<{ nodeId?: string; hostname?: string }>(request);
	const nodeId = (body.nodeId ?? "").trim();
	const hostname = (body.hostname ?? "").trim();
	if (!nodeId || !hostname) {
		return fail(ErrorCode.InvalidArgs, "nodeId and hostname are required", 400);
	}
	const token = formatOrgToken("hbw", deps.orgId() || caller.orgId);
	deps.tokens.putWorkerToken(await sha256Hex(token), nodeId, hostname, deps.now());
	return Response.json(
		{ token, nodeId, hostname, orgId: deps.orgId() || undefined },
		{ status: 201 },
	);
}

/** GET/POST /admin/join-tokens, DELETE /admin/join-tokens/:id: an organization's worker join tokens. */
async function joinTokens(
	request: Request,
	pathname: string,
	caller: CallerIdentity,
	deps: AdminRouteDeps,
): Promise<Response> {
	if (caller.role !== "owner") {
		return fail(ErrorCode.NotAuthorized, "organization member authentication required", 403);
	}
	const collection = pathname === "/admin/join-tokens";
	if (collection && request.method === "GET") {
		return Response.json({
			orgId: deps.orgId() || undefined,
			tokens: deps.tokens.listJoinTokens(),
		});
	}
	if (collection && request.method === "POST") {
		if (caller.orgRole === "viewer") {
			return fail(
				ErrorCode.NotAuthorized,
				"viewer role is read-only and cannot mint join tokens",
				403,
			);
		}
		const body = await jsonBody<{ label?: string; expiresInHours?: number }>(request);
		const label = (body.label ?? "").trim().slice(0, 100);
		const nowMs = deps.now();
		const expiresAt = expiry(nowMs, body.expiresInHours, 3600_000);
		const id = `jt_${randomHex(8)}`;
		const token = formatOrgToken("hbj", deps.orgId() || caller.orgId);
		deps.tokens.putJoinToken({
			id,
			tokenHash: await sha256Hex(token),
			label,
			createdBy: caller.login,
			createdAt: nowMs,
			expiresAt,
		});
		return Response.json(
			{
				id,
				token,
				label,
				createdBy: caller.login,
				createdAt: nowMs,
				expiresAt,
				orgId: deps.orgId() || undefined,
			},
			{ status: 201 },
		);
	}
	if (!collection && request.method === "DELETE") {
		if (caller.orgRole === "viewer") {
			return fail(
				ErrorCode.NotAuthorized,
				"viewer role is read-only and cannot revoke join tokens",
				403,
			);
		}
		const id = decodePathSegment(pathname.slice("/admin/join-tokens/".length));
		if (id === null) {
			return fail(ErrorCode.InvalidArgs, "invalid join token id encoding", 400);
		}
		if (!id || !deps.tokens.revokeJoinToken(id, deps.now())) {
			return fail(ErrorCode.NotFound, `no join token ${id}`, 404);
		}
		return Response.json({ ok: true, id });
	}
	return methodNotAllowed();
}

/** GET/POST /admin/api-tokens, DELETE /admin/api-tokens/:id: personal API tokens; admins see and revoke everyone's. */
async function apiTokens(
	request: Request,
	pathname: string,
	caller: CallerIdentity,
	deps: AdminRouteDeps,
): Promise<Response> {
	if (caller.role !== "owner") {
		return fail(ErrorCode.NotAuthorized, "organization member authentication required", 403);
	}
	const isAdmin = caller.orgRole === "admin" || caller.orgRole === "owner" || !caller.userId;
	const collection = pathname === "/admin/api-tokens";
	if (collection && request.method === "GET") {
		return Response.json({
			orgId: deps.orgId() || undefined,
			tokens: deps.tokens.listApiTokens(isAdmin ? undefined : caller.userId),
		});
	}
	if (collection && request.method === "POST") {
		const body = await jsonBody<{ name?: string; expiresInDays?: number }>(request);
		const name = (body.name ?? "").trim().slice(0, 100);
		if (!name) {
			return fail(ErrorCode.InvalidArgs, "token name is required", 400);
		}
		const nowMs = deps.now();
		const expiresAt = expiry(nowMs, body.expiresInDays, 24 * 3600_000);
		const id = `at_${randomHex(8)}`;
		const token = formatOrgToken("hbu", deps.orgId() || caller.orgId);
		const row = {
			id,
			name,
			userId: caller.userId,
			email: caller.login,
			orgRole: caller.orgRole || "member",
			createdAt: nowMs,
			expiresAt,
		};
		deps.tokens.putApiToken({ ...row, tokenHash: await sha256Hex(token) });
		return Response.json({ ...row, token, orgId: deps.orgId() || undefined }, { status: 201 });
	}
	if (!collection && request.method === "DELETE") {
		const id = decodePathSegment(pathname.slice("/admin/api-tokens/".length));
		if (id === null) {
			return fail(ErrorCode.InvalidArgs, "invalid api token id encoding", 400);
		}
		const existing = id ? deps.tokens.getApiToken(id) : null;
		if (!existing) {
			return fail(ErrorCode.NotFound, `no api token ${id}`, 404);
		}
		if (!isAdmin && existing.userId !== caller.userId) {
			return fail(ErrorCode.NotAuthorized, "cannot revoke another user's API token", 403);
		}
		deps.tokens.revokeApiToken(id, deps.now());
		return Response.json({ ok: true, id });
	}
	return methodNotAllowed();
}

/** GET/POST /admin/signup-codes, DELETE /admin/signup-codes/:code: the default organization's sign-up codes. */
async function signupCodes(
	request: Request,
	pathname: string,
	caller: CallerIdentity,
	deps: AdminRouteDeps,
): Promise<Response> {
	const edgeGlobalAdmin = request.headers.get(INTERNAL_GLOBAL_ADMIN_HEADER) === "1";
	const isGlobalAdmin =
		caller.role === "owner" &&
		(edgeGlobalAdmin ||
			caller.authMethod === "bearer" ||
			(Boolean(deps.defaultOrgId) &&
				caller.orgId === deps.defaultOrgId &&
				(caller.orgRole === "admin" || caller.orgRole === "owner")));
	if (!isGlobalAdmin) {
		return fail(
			ErrorCode.NotAuthorized,
			"cluster owner or default-organization admin required",
			403,
		);
	}
	// `/auth/signup-code` redeems codes in the default DO only; a code
	// written to an organization's DO (OWNER_TOKEN + X-Hotbed-Org-Id)
	// would be accepted here and never honored.
	const isDefaultDo = request.headers.get(INTERNAL_DEFAULT_DO_HEADER) === "1";
	const boundOrgId = deps.orgId() || (request.headers.get(INTERNAL_ORG_HEADER) ?? "").trim();
	if (!isDefaultDo && boundOrgId) {
		return fail(
			ErrorCode.InvalidArgs,
			`sign-up codes live in the default organization, not ${boundOrgId}`,
			400,
			"omit X-Hotbed-Org-Id when managing sign-up codes",
		);
	}
	const collection = pathname === "/admin/signup-codes";
	if (collection && request.method === "GET") {
		return Response.json({
			codes: deps.tokens.listSignupCodes(),
			approvedUsersCount: deps.tokens.countApprovedUsers(),
		});
	}
	if (collection && request.method === "POST") {
		const body = await jsonBody<{ code?: string; label?: string; maxUses?: number | null }>(
			request,
		);
		const code = (body.code ?? "").trim().toUpperCase();
		if (code.length < 3 || code.length > 64 || /\s/.test(code)) {
			return fail(
				ErrorCode.InvalidArgs,
				"sign-up code must be 3-64 non-whitespace characters",
				400,
			);
		}
		const label = (body.label ?? "").trim().slice(0, 100);
		let maxUses: number | null = null;
		if (body.maxUses !== undefined && body.maxUses !== null) {
			if (typeof body.maxUses !== "number" || !Number.isInteger(body.maxUses) || body.maxUses < 1) {
				return fail(ErrorCode.InvalidArgs, "maxUses must be a positive integer", 400);
			}
			maxUses = body.maxUses;
		}
		const nowMs = deps.now();
		deps.tokens.putSignupCode({ code, label, maxUses, createdAt: nowMs });
		return Response.json({ code, label, maxUses, createdAt: nowMs }, { status: 201 });
	}
	if (!collection && request.method === "DELETE") {
		const code = decodePathSegment(pathname.slice("/admin/signup-codes/".length));
		if (code === null || !code) {
			return fail(ErrorCode.InvalidArgs, "valid sign-up code is required", 400);
		}
		if (!deps.tokens.revokeSignupCode(code, deps.now())) {
			return fail(ErrorCode.NotFound, `no sign-up code ${code}`, 404);
		}
		return Response.json({ ok: true, code: code.toUpperCase() });
	}
	return methodNotAllowed();
}
