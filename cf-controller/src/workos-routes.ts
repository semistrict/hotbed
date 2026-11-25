import { create, toBinary } from "@bufbuild/protobuf";
import {
	authenticateRequest,
	checkSameOrigin,
	clearSessionCookie,
	getBearerToken,
	getCookie,
	INTERNAL_AUTH_HEADER,
	INTERNAL_ORG_HEADER,
	makeSessionCookie,
	NOOP_TOKEN_LOOKUP,
	renderLoginPage,
	timingSafeStringEqual,
	type CallerIdentity,
} from "./auth.ts";
import type { ControllerEnv } from "./controller-do.ts";
import { ErrorCode, HotbedError, fromUnknownError, hotbedError } from "./errors.ts";
import { ErrorInfoSchema } from "./gen/hotbed/v1/hotbed_pb.ts";
import { randomHex } from "./gitops.ts";
import {
	authenticateWithCode,
	authenticateWithOrgSelection,
	authenticateWithRefreshToken,
	buildAuthorizeUrl,
	buildLogoutUrl,
	clearShortAuthCookie,
	clearWorkOSSessionCookie,
	createOrganization,
	createOrganizationMembership,
	doNameForOrg,
	formatDisplayName,
	getInvitation,
	getOrganization,
	getOrganizationMembership,
	getUser,
	isAllowedCliRedirectUri,
	isSafeReturnTo,
	isValidOrgId,
	isWorkOSEnabled,
	listOrganizationInvitations,
	listOrganizationMembers,
	listUserOrganizations,
	makeShortAuthCookie,
	makeWorkOSSessionCookie,
	parseOrgScopedToken,
	pkceS256,
	pollDeviceToken,
	randomBase64Url,
	removeOrganizationMembership,
	renderOrgPickerPage,
	revokeOrganizationInvitation,
	sealPayload,
	sendOrganizationInvitation,
	startDeviceAuthorization,
	unsealPayload,
	verifyWorkOSAccessToken,
	WORKOS_OAUTH_STATE_COOKIE,
	WORKOS_PENDING_ORG_COOKIE,
	WORKOS_SESSION_COOKIE,
	workosAuthProvider,
	workosClientId,
	type CliHandoffPayload,
	type OAuthStatePayload,
	type PendingOrgSelectionPayload,
	type WorkOSOrganizationSummary,
	type WorkOSSessionPayload,
	type WorkOSUser,
} from "./workos.ts";

const REFRESH_MARGIN_MS = 60_000;

function isOrgAdmin(role: string): boolean {
	return role === "admin" || role === "owner";
}

function decodePathSegment(raw: string): string | null {
	try {
		return decodeURIComponent(raw).trim();
	} catch {
		return null;
	}
}

function encodeBase64(bytes: Uint8Array): string {
	let bin = "";
	const chunk = 0x8000;
	for (let i = 0; i < bytes.length; i += chunk) {
		bin += String.fromCharCode(...bytes.subarray(i, i + chunk));
	}
	return btoa(bin);
}

export function edgeConnectErrorResponse(err: HotbedError): Response {
	const info = create(ErrorInfoSchema, {
		code: err.code,
		suggestion: err.suggestion,
		retryable: err.retryable,
	});
	const connectCode =
		err.code === ErrorCode.NotAuthorized
			? "permission_denied"
			: err.code === ErrorCode.InvalidArgs
				? "invalid_argument"
				: "internal";
	const httpStatus =
		err.code === ErrorCode.NotAuthorized ? 403 : err.code === ErrorCode.InvalidArgs ? 400 : 500;
	return new Response(
		JSON.stringify({
			code: connectCode,
			message: err.message,
			details: [
				{
					type: ErrorInfoSchema.typeName,
					value: encodeBase64(toBinary(ErrorInfoSchema, info)),
				},
			],
		}),
		{
			status: httpStatus,
			headers: { "Content-Type": "application/json" },
		},
	);
}

export interface EdgeAuthResult {
	doName: string;
	orgId: string;
	caller: CallerIdentity | null;
	session?: WorkOSSessionPayload;
	setCookie?: string;
	clearWorkOSCookie?: boolean;
	needsOrgSelection?: boolean;
}

function defaultDoName(request: Request, url: URL, env: ControllerEnv): string {
	if (env.TEST_HARNESS === "1") {
		return (
			(request.headers.get("X-Hotbed-Test-DO") ?? url.searchParams.get("__do") ?? "").trim() ||
			"default"
		);
	}
	return "default";
}

function clientIp(req: Request): string | undefined {
	return req.headers.get("CF-Connecting-IP") ?? req.headers.get("X-Forwarded-For") ?? undefined;
}

function clientUa(req: Request): string | undefined {
	return req.headers.get("User-Agent") ?? undefined;
}

async function resolveSessionCookie(
	request: Request,
	env: ControllerEnv,
	opts?: { allowTopLevelNavigate?: boolean; allowNoOrg?: boolean },
): Promise<{
	session: WorkOSSessionPayload;
	caller: CallerIdentity | null;
	setCookie?: string;
	needsOrgSelection?: boolean;
} | null> {
	if (!isWorkOSEnabled(env)) return null;
	const raw = getCookie(request, WORKOS_SESSION_COOKIE);
	if (!raw) return null;
	let session = await unsealPayload<WorkOSSessionPayload>(env, "session", raw);
	if (!session || !session.accessToken) return null;

	const csrfErr = checkSameOrigin(request, {
		allowTopLevelNavigate: opts?.allowTopLevelNavigate,
	});
	if (csrfErr) {
		throw hotbedError(ErrorCode.NotAuthorized, csrfErr);
	}

	const nowMs = Date.now();
	let claims = await verifyWorkOSAccessToken(session.accessToken, env, nowMs);
	let setCookie: string | undefined;

	const needsRefresh = !claims || claims.exp - nowMs < REFRESH_MARGIN_MS;
	if (needsRefresh && session.refreshToken) {
		try {
			const refreshed = await authenticateWithRefreshToken(env, {
				refreshToken: session.refreshToken,
				organizationId: session.organizationId || undefined,
				ipAddress: clientIp(request),
				userAgent: clientUa(request),
			});
			const refreshedClaims = await verifyWorkOSAccessToken(refreshed.accessToken, env, Date.now());
			if (!refreshedClaims) {
				if (!claims) return null;
			} else {
				let orgName = session.organizationName;
				if (refreshedClaims.orgId && refreshedClaims.orgId !== session.organizationId) {
					try {
						orgName = (await getOrganization(env, refreshedClaims.orgId)).name;
					} catch {
						orgName = refreshedClaims.orgId;
					}
				} else if (!refreshedClaims.orgId) {
					orgName = "";
				}
				session = {
					userId: refreshed.user.id,
					email: refreshed.user.email,
					firstName: refreshed.user.firstName,
					lastName: refreshed.user.lastName,
					organizationId: refreshedClaims.orgId,
					organizationName: orgName,
					role: refreshedClaims.role || "member",
					sessionId: refreshed.sessionId || session.sessionId,
					accessToken: refreshed.accessToken,
					refreshToken: refreshed.refreshToken,
					expiresAt: refreshed.expiresAt,
				};
				claims = refreshedClaims;
				const sealed = await sealPayload(env, "session", session);
				const secure = new URL(request.url).protocol === "https:";
				setCookie = makeWorkOSSessionCookie(sealed, secure);
			}
		} catch {
			if (!claims) {
				return null;
			}
		}
	}

	if (!claims) return null;

	const orgId = claims.orgId;
	if (!orgId) {
		return {
			session: { ...session, organizationId: "" },
			caller: null,
			setCookie,
			needsOrgSelection: true,
		};
	}

	const headerHost = (request.headers.get("X-Hotbed-Host") ?? "").trim();
	const login = session.email || claims.email || claims.sub;
	const effectiveRole = claims.role || "member";
	const caller: CallerIdentity = {
		role: "owner",
		nodeId: `workos:${claims.sub}`,
		hostname: headerHost || login,
		login,
		userId: claims.sub,
		orgId,
		orgRole: effectiveRole,
		addr: "",
		authMethod: "workos-cookie",
	};
	return { session: { ...session, organizationId: orgId, role: effectiveRole }, caller, setCookie };
}

/**
 * Resolves edge authentication and Durable Object routing for any non-/auth request.
 * Throws HotbedError for invalid WorkOS JWTs or CSRF violations.
 */
export async function resolveEdgeRouting(
	request: Request,
	url: URL,
	env: ControllerEnv,
	opts?: { allowTopLevelNavigate?: boolean; allowUnverifiedScopedRouting?: boolean },
): Promise<EdgeAuthResult> {
	const fallbackDo = defaultDoName(request, url, env);
	const bearer = getBearerToken(request);
	const ownerToken = (env.OWNER_TOKEN ?? "").trim();
	const joinToken = (env.JOIN_TOKEN ?? "").trim();
	const headerHost = (request.headers.get("X-Hotbed-Host") ?? "").trim();
	const headerNodeId = (request.headers.get("X-Hotbed-Node-Id") ?? "").trim();
	const headerAddr = (request.headers.get("X-Hotbed-Addr") ?? "").trim();

	// 1. Bearer tokens
	if (bearer) {
		// 1a. Legacy / superadmin OWNER_TOKEN
		if (ownerToken && (await timingSafeStringEqual(bearer, ownerToken))) {
			const explicitOrg = (request.headers.get("X-Hotbed-Org-Id") ?? "").trim();
			if (explicitOrg && isValidOrgId(explicitOrg)) {
				return {
					doName: doNameForOrg(explicitOrg, env),
					orgId: explicitOrg,
					caller: null,
				};
			}
			return { doName: fallbackDo, orgId: "", caller: null };
		}

		// 1b. Org-scoped worker / join / API tokens (hbw_<org>.<sec>, hbj_<org>.<sec>, hbu_<org>.<sec>)
		const scoped = parseOrgScopedToken(bearer);
		if (scoped) {
			if (scoped.orgId && opts?.allowUnverifiedScopedRouting !== false) {
				return {
					doName: doNameForOrg(scoped.orgId, env),
					orgId: scoped.orgId,
					caller: null,
				};
			}
			return { doName: fallbackDo, orgId: "", caller: null };
		}

		// 1c. Legacy global JOIN_TOKEN
		if (joinToken && (await timingSafeStringEqual(bearer, joinToken))) {
			return { doName: fallbackDo, orgId: "", caller: null };
		}

		// 1d. WorkOS JWT access token
		if (isWorkOSEnabled(env) && bearer.includes(".")) {
			const claims = await verifyWorkOSAccessToken(bearer, env);
			if (!claims) {
				throw hotbedError(
					ErrorCode.NotAuthorized,
					"invalid or expired WorkOS access token",
				).withSuggestion("run `hotbed login` to refresh your credentials");
			}
			if (!claims.orgId) {
				throw hotbedError(
					ErrorCode.NotAuthorized,
					"this WorkOS token has no active organization selected",
				).withSuggestion(
					"select or create an organization with `hotbed org switch` or `hotbed org create`",
				);
			}
			const login = claims.email || claims.sub;
			const caller: CallerIdentity = {
				role: "owner",
				nodeId: headerNodeId || `workos:${claims.sub}`,
				hostname: headerHost || login,
				login,
				userId: claims.sub,
				orgId: claims.orgId,
				orgRole: claims.role || "member",
				addr: headerAddr,
				authMethod: "workos-jwt",
			};
			return {
				doName: doNameForOrg(claims.orgId, env),
				orgId: claims.orgId,
				caller,
			};
		}

		// Unknown bearer token — forward to default DO to return standard invalid bearer error
		return { doName: fallbackDo, orgId: "", caller: null };
	}

	// 2. WorkOS sealed session cookie
	if (isWorkOSEnabled(env)) {
		const rawCookie = getCookie(request, WORKOS_SESSION_COOKIE);
		if (rawCookie) {
			const resolved = await resolveSessionCookie(request, env, {
				allowTopLevelNavigate: opts?.allowTopLevelNavigate,
			});
			if (!resolved) {
				return { doName: fallbackDo, orgId: "", caller: null, clearWorkOSCookie: true };
			}
			if (resolved.needsOrgSelection || !resolved.caller) {
				return {
					doName: fallbackDo,
					orgId: "",
					caller: null,
					session: resolved.session,
					setCookie: resolved.setCookie,
					needsOrgSelection: true,
				};
			}
			return {
				doName: doNameForOrg(resolved.caller.orgId, env),
				orgId: resolved.caller.orgId,
				caller: resolved.caller,
				session: resolved.session,
				setCookie: resolved.setCookie,
			};
		}
	}

	// 3. Legacy session cookie / CF Access / unauthenticated
	return { doName: fallbackDo, orgId: "", caller: null };
}

export function attachInternalAuth(request: Request, routing: EdgeAuthResult): Request {
	const headers = new Headers(request.headers);
	if (routing.orgId) {
		headers.set(INTERNAL_ORG_HEADER, routing.orgId);
	}
	if (routing.caller) {
		headers.set(INTERNAL_AUTH_HEADER, JSON.stringify(routing.caller));
	}
	return new Request(request, { headers });
}

export function withSetCookie(res: Response, cookie?: string): Response {
	if (!cookie || res.status === 101) return res;
	const headers = new Headers(res.headers);
	headers.append("Set-Cookie", cookie);
	return new Response(res.body, {
		status: res.status,
		statusText: res.statusText,
		headers,
	});
}

// ---- Auth & Org HTTP Routes (/auth/*) ---------------------------------

function wantsJson(req: Request): boolean {
	const accept = (req.headers.get("Accept") ?? "").toLowerCase();
	const ct = (req.headers.get("Content-Type") ?? "").toLowerCase();
	return accept.includes("application/json") || ct.includes("application/json");
}

async function readBodyParams(req: Request): Promise<Record<string, string>> {
	const ct = (req.headers.get("Content-Type") ?? "").toLowerCase();
	if (ct.includes("application/json")) {
		const raw = (await req.json().catch(() => ({}))) as Record<string, unknown>;
		const out: Record<string, string> = {};
		for (const [k, v] of Object.entries(raw)) {
			if (typeof v === "string") out[k] = v;
		}
		return out;
	}
	const form = await req.formData().catch(() => null);
	const out: Record<string, string> = {};
	if (form) {
		for (const [k, v] of form.entries()) {
			if (typeof v === "string") out[k] = v;
		}
	}
	return out;
}

async function finalizeWorkOSLogin(
	request: Request,
	url: URL,
	env: ControllerEnv,
	auth: {
		user: WorkOSUser;
		organizationId: string;
		accessToken: string;
		refreshToken: string;
		sessionId: string;
		role: string;
		expiresAt: number;
	},
	flow: {
		returnTo: string;
		cliRedirectUri?: string;
		cliState?: string;
		cliCodeChallenge?: string;
	},
	clearCookies: string[] = [],
): Promise<Response> {
	const secure = url.protocol === "https:";
	let active = auth;
	let orgs: WorkOSOrganizationSummary[] = [];
	try {
		orgs = await listUserOrganizations(env, active.user.id);
	} catch {
		orgs = [];
	}

	// If no organization is active yet and the user belongs to exactly one org, activate it automatically.
	if (!active.organizationId && orgs.length === 1 && active.refreshToken) {
		try {
			active = await authenticateWithRefreshToken(env, {
				refreshToken: active.refreshToken,
				organizationId: orgs[0].id,
				ipAddress: clientIp(request),
				userAgent: clientUa(request),
			});
		} catch {
			// Fall through to org picker if auto-activation fails.
		}
	}

	let orgName = "";
	if (active.organizationId) {
		orgName = orgs.find((o) => o.id === active.organizationId)?.name ?? "";
		if (!orgName) {
			try {
				orgName = (await getOrganization(env, active.organizationId)).name;
			} catch {
				orgName = active.organizationId;
			}
		}
	}

	const session: WorkOSSessionPayload = {
		userId: active.user.id,
		email: active.user.email,
		firstName: active.user.firstName,
		lastName: active.user.lastName,
		organizationId: active.organizationId,
		organizationName: orgName,
		role: active.role || orgs.find((o) => o.id === active.organizationId)?.role || "member",
		sessionId: active.sessionId,
		accessToken: active.accessToken,
		refreshToken: active.refreshToken,
		expiresAt: active.expiresAt,
	};
	const headers = new Headers();
	for (const c of clearCookies) headers.append("Set-Cookie", c);

	// CLI loopback handoff (if initiated by `hotbed login` via /auth/cli/authorize).
	// Do NOT also set hotbed_wos_session on the browser with the same single-use refresh token,
	// or the first refresh from either client invalidates the other.
	if (flow.cliRedirectUri && isAllowedCliRedirectUri(flow.cliRedirectUri)) {
		const handoff: CliHandoffPayload = {
			jti: randomHex(16),
			accessToken: session.accessToken,
			refreshToken: session.refreshToken,
			expiresAt: session.expiresAt,
			user: active.user,
			organizationId: session.organizationId,
			organizationName: session.organizationName,
			role: session.role,
			codeChallenge: flow.cliCodeChallenge || "",
			redirectUri: flow.cliRedirectUri,
			exp: Date.now() + 120_000,
		};
		const code = await sealPayload(env, "cli-handoff", handoff);
		const dest = new URL(flow.cliRedirectUri);
		dest.searchParams.set("code", code);
		if (flow.cliState) dest.searchParams.set("state", flow.cliState);
		headers.set("Location", dest.toString());
		return new Response(null, { status: 303, headers });
	}

	const sealed = await sealPayload(env, "session", session);
	headers.append("Set-Cookie", makeWorkOSSessionCookie(sealed, secure));

	if (!session.organizationId) {
		const orgsUrl = new URL("/auth/orgs", url);
		if (flow.returnTo && flow.returnTo !== "/") {
			orgsUrl.searchParams.set("return_to", flow.returnTo);
		}
		headers.set("Location", orgsUrl.toString());
		return new Response(null, { status: 303, headers });
	}

	headers.set("Location", isSafeReturnTo(flow.returnTo));
	return new Response(null, { status: 303, headers });
}

async function resolveWorkOSCallerForOrgRoute(
	request: Request,
	env: ControllerEnv,
	opts?: { allowNoOrg?: boolean },
): Promise<{
	userId: string;
	email: string;
	firstName: string;
	lastName: string;
	organizationId: string;
	organizationName: string;
	role: string;
	authMethod: "workos-cookie" | "workos-jwt";
	session?: WorkOSSessionPayload;
	setCookie?: string;
}> {
	if (!isWorkOSEnabled(env)) {
		throw hotbedError(ErrorCode.InvalidArgs, "WorkOS is not configured on this controller");
	}
	const bearer = getBearerToken(request);
	if (bearer) {
		const claims = await verifyWorkOSAccessToken(bearer, env);
		if (!claims) {
			throw hotbedError(ErrorCode.NotAuthorized, "invalid or expired WorkOS access token");
		}
		if (!claims.orgId && !opts?.allowNoOrg) {
			throw hotbedError(ErrorCode.NotAuthorized, "no active organization selected");
		}
		let user: WorkOSUser = {
			id: claims.sub,
			email: claims.email || claims.sub,
			firstName: "",
			lastName: "",
		};
		try {
			user = await getUser(env, claims.sub);
		} catch {
			// Fall back to claims if user lookup fails.
		}
		let orgName = "";
		if (claims.orgId) {
			try {
				orgName = (await getOrganization(env, claims.orgId)).name;
			} catch {
				orgName = claims.orgId;
			}
		}
		return {
			userId: user.id,
			email: user.email,
			firstName: user.firstName,
			lastName: user.lastName,
			organizationId: claims.orgId,
			organizationName: orgName,
			role: claims.role || "member",
			authMethod: "workos-jwt",
		};
	}

	const resolved = await resolveSessionCookie(request, env, { allowNoOrg: opts?.allowNoOrg });
	if (!resolved) {
		throw hotbedError(ErrorCode.NotAuthorized, "authentication required");
	}
	if (!resolved.session.organizationId && !opts?.allowNoOrg) {
		throw hotbedError(ErrorCode.NotAuthorized, "no active organization selected");
	}
	return {
		userId: resolved.session.userId,
		email: resolved.session.email,
		firstName: resolved.session.firstName,
		lastName: resolved.session.lastName,
		organizationId: resolved.session.organizationId,
		organizationName: resolved.session.organizationName,
		role: resolved.session.role || "member",
		authMethod: "workos-cookie",
		session: resolved.session,
		setCookie: resolved.setCookie,
	};
}

export async function handleAuthRoute(
	request: Request,
	url: URL,
	env: ControllerEnv,
): Promise<Response | null> {
	const pathname = url.pathname;
	if (!pathname.startsWith("/auth/")) return null;
	const workosEnabled = isWorkOSEnabled(env);
	const ownerToken = (env.OWNER_TOKEN ?? "").trim();
	const secure = url.protocol === "https:";

	// 1. Public discovery config for CLI & Cockpit
	if (pathname === "/auth/config" && request.method === "GET") {
		return Response.json({
			workosEnabled,
			authProvider: workosEnabled ? workosAuthProvider(env) : "",
			clientId: workosEnabled ? workosClientId(env) : "",
			deviceAuthSupported: workosEnabled,
			tokenLoginEnabled: Boolean(ownerToken),
		});
	}

	// 2. GET/POST /auth/login
	if (pathname === "/auth/login") {
		if (request.method === "GET") {
			const mode = url.searchParams.get("mode");
			const returnTo = isSafeReturnTo(url.searchParams.get("return_to"));
			if (workosEnabled && mode !== "token") {
				const state = randomHex(16);
				const codeVerifier = randomBase64Url(32);
				const codeChallenge = await pkceS256(codeVerifier);
				const provider = url.searchParams.get("provider")?.trim() || undefined;
				const organizationId = url.searchParams.get("organization_id")?.trim() || undefined;
				const screenHint = url.searchParams.get("screen_hint")?.trim() || undefined;
				const statePayload: OAuthStatePayload = {
					state,
					codeVerifier,
					returnTo,
					exp: Date.now() + 10 * 60_000,
				};
				const sealedState = await sealPayload(env, "oauth-state", statePayload);
				const authorizeUrl = buildAuthorizeUrl(env, {
					redirectUri: `${url.origin}/auth/callback`,
					state,
					codeChallenge,
					provider,
					organizationId,
					screenHint,
				});
				return new Response(null, {
					status: 302,
					headers: {
						Location: authorizeUrl,
						"Set-Cookie": makeShortAuthCookie(WORKOS_OAUTH_STATE_COOKIE, sealedState, secure),
					},
				});
			}
			return new Response(renderLoginPage(undefined, workosEnabled, returnTo), {
				status: 200,
				headers: { "Content-Type": "text/html; charset=utf-8" },
			});
		}

		if (request.method === "POST") {
			const csrfErr = checkSameOrigin(request);
			if (csrfErr) {
				return Response.json({ code: ErrorCode.NotAuthorized, message: csrfErr }, { status: 403 });
			}
			const body = await readBodyParams(request);
			const returnTo = isSafeReturnTo(body.return_to ?? url.searchParams.get("return_to"));
			if (!ownerToken) {
				return new Response(
					renderLoginPage(
						"OWNER_TOKEN is not configured on this controller.",
						workosEnabled,
						returnTo,
					),
					{
						status: 503,
						headers: { "Content-Type": "text/html; charset=utf-8" },
					},
				);
			}
			const submitted = (body.token ?? "").trim();
			if (!submitted || !(await timingSafeStringEqual(submitted, ownerToken))) {
				return new Response(renderLoginPage("Invalid owner token.", workosEnabled, returnTo), {
					status: 401,
					headers: { "Content-Type": "text/html; charset=utf-8" },
				});
			}
			const cookie = await makeSessionCookie(ownerToken, secure);
			return new Response(null, {
				status: 303,
				headers: {
					Location: returnTo,
					"Set-Cookie": cookie,
				},
			});
		}
		return new Response("Method Not Allowed", { status: 405 });
	}

	// 3. GET /auth/cli/authorize — browser entry point for `hotbed login` (RFC 8252 loopback + PKCE)
	if (pathname === "/auth/cli/authorize" && request.method === "GET") {
		if (!workosEnabled) {
			return Response.json(
				{
					code: ErrorCode.InvalidArgs,
					message: "WorkOS is not configured on this controller",
					suggestion: "set HOTBED_TOKEN to your OWNER_TOKEN instead",
				},
				{ status: 400 },
			);
		}
		const redirectUri = (url.searchParams.get("redirect_uri") ?? "").trim();
		const cliState = (url.searchParams.get("state") ?? "").trim();
		const cliCodeChallenge = (url.searchParams.get("code_challenge") ?? "").trim();
		const organizationId = (url.searchParams.get("organization_id") ?? "").trim() || undefined;
		const provider = (url.searchParams.get("provider") ?? "").trim() || undefined;
		if (!isAllowedCliRedirectUri(redirectUri)) {
			return Response.json(
				{
					code: ErrorCode.InvalidArgs,
					message:
						"invalid redirect_uri: must be http://127.0.0.1:<port>/callback or http://[::1]:<port>/callback",
				},
				{ status: 400 },
			);
		}
		if (!cliCodeChallenge) {
			return Response.json(
				{ code: ErrorCode.InvalidArgs, message: "code_challenge is required" },
				{ status: 400 },
			);
		}

		const state = randomHex(16);
		const codeVerifier = randomBase64Url(32);
		const codeChallenge = await pkceS256(codeVerifier);
		const statePayload: OAuthStatePayload = {
			state,
			codeVerifier,
			returnTo: "/",
			cliRedirectUri: redirectUri,
			cliState,
			cliCodeChallenge,
			exp: Date.now() + 10 * 60_000,
		};
		const sealedState = await sealPayload(env, "oauth-state", statePayload);
		const authorizeUrl = buildAuthorizeUrl(env, {
			redirectUri: `${url.origin}/auth/callback`,
			state,
			codeChallenge,
			provider,
			organizationId,
		});
		return new Response(null, {
			status: 302,
			headers: {
				Location: authorizeUrl,
				"Set-Cookie": makeShortAuthCookie(WORKOS_OAUTH_STATE_COOKIE, sealedState, secure),
			},
		});
	}

	// 4. GET /auth/callback — WorkOS OAuth callback
	if (pathname === "/auth/callback" && request.method === "GET") {
		if (!workosEnabled) {
			return new Response("WorkOS is not configured", { status: 404 });
		}
		const rawStateCookie = getCookie(request, WORKOS_OAUTH_STATE_COOKIE);
		const savedState = rawStateCookie
			? await unsealPayload<OAuthStatePayload>(env, "oauth-state", rawStateCookie)
			: null;

		const oauthError = url.searchParams.get("error");
		if (oauthError) {
			const desc = url.searchParams.get("error_description") || oauthError;
			if (
				savedState &&
				savedState.exp >= Date.now() &&
				savedState.cliRedirectUri &&
				isAllowedCliRedirectUri(savedState.cliRedirectUri)
			) {
				const dest = new URL(savedState.cliRedirectUri);
				dest.searchParams.set("error", oauthError);
				dest.searchParams.set("error_description", desc);
				if (savedState.cliState) dest.searchParams.set("state", savedState.cliState);
				return new Response(null, {
					status: 303,
					headers: {
						Location: dest.toString(),
						"Set-Cookie": clearShortAuthCookie(WORKOS_OAUTH_STATE_COOKIE, secure),
					},
				});
			}
			return new Response(renderLoginPage(`Sign-in failed: ${desc}`, true), {
				status: 401,
				headers: {
					"Content-Type": "text/html; charset=utf-8",
					"Set-Cookie": clearShortAuthCookie(WORKOS_OAUTH_STATE_COOKIE, secure),
				},
			});
		}

		const code = (url.searchParams.get("code") ?? "").trim();
		const state = (url.searchParams.get("state") ?? "").trim();

		if (
			!code ||
			!state ||
			!savedState ||
			savedState.exp < Date.now() ||
			!(await timingSafeStringEqual(state, savedState.state))
		) {
			return new Response(
				renderLoginPage("Invalid or expired login state. Please try signing in again.", true),
				{
					status: 400,
					headers: {
						"Content-Type": "text/html; charset=utf-8",
						"Set-Cookie": clearShortAuthCookie(WORKOS_OAUTH_STATE_COOKIE, secure),
					},
				},
			);
		}

		try {
			const auth = await authenticateWithCode(env, {
				code,
				codeVerifier: savedState.codeVerifier,
				ipAddress: clientIp(request),
				userAgent: clientUa(request),
			});
			const clearStateCookie = clearShortAuthCookie(WORKOS_OAUTH_STATE_COOKIE, secure);
			if (auth.kind === "organization_selection_required") {
				const pending: PendingOrgSelectionPayload = {
					pendingAuthenticationToken: auth.pendingAuthenticationToken,
					organizations: auth.organizations,
					returnTo: savedState.returnTo,
					cliRedirectUri: savedState.cliRedirectUri,
					cliState: savedState.cliState,
					cliCodeChallenge: savedState.cliCodeChallenge,
					exp: Date.now() + 10 * 60_000,
				};
				const sealedPending = await sealPayload(env, "pending-org", pending);
				const headers = new Headers({ "Content-Type": "text/html; charset=utf-8" });
				headers.append("Set-Cookie", clearStateCookie);
				headers.append(
					"Set-Cookie",
					makeShortAuthCookie(WORKOS_PENDING_ORG_COOKIE, sealedPending, secure),
				);
				return new Response(
					renderOrgPickerPage({
						email: "",
						organizations: auth.organizations,
						pendingSelection: true,
						returnTo: savedState.returnTo,
					}),
					{ status: 200, headers },
				);
			}
			return finalizeWorkOSLogin(
				request,
				url,
				env,
				auth,
				{
					returnTo: savedState.returnTo,
					cliRedirectUri: savedState.cliRedirectUri,
					cliState: savedState.cliState,
					cliCodeChallenge: savedState.cliCodeChallenge,
				},
				[clearStateCookie],
			);
		} catch (err) {
			const he = fromUnknownError(err);
			return new Response(renderLoginPage(he.message, true), {
				status: 401,
				headers: {
					"Content-Type": "text/html; charset=utf-8",
					"Set-Cookie": clearShortAuthCookie(WORKOS_OAUTH_STATE_COOKIE, secure),
				},
			});
		}
	}

	// 5. POST /auth/org/select-pending — complete WorkOS organization_selection_required flow
	if (pathname === "/auth/org/select-pending" && request.method === "POST") {
		if (!workosEnabled) return new Response("Not Found", { status: 404 });
		const csrfErr = checkSameOrigin(request);
		if (csrfErr) {
			return Response.json({ code: ErrorCode.NotAuthorized, message: csrfErr }, { status: 403 });
		}
		const rawPending = getCookie(request, WORKOS_PENDING_ORG_COOKIE);
		const pending = rawPending
			? await unsealPayload<PendingOrgSelectionPayload>(env, "pending-org", rawPending)
			: null;
		if (!pending || pending.exp < Date.now()) {
			return Response.redirect(new URL("/auth/login", url).toString(), 303);
		}
		const body = await readBodyParams(request);
		const orgId = (body.organization_id || body.organizationId || "").trim();
		if (!orgId || !isValidOrgId(orgId)) {
			return new Response(
				renderOrgPickerPage({
					email: "",
					organizations: pending.organizations,
					pendingSelection: true,
					error: "Please select a valid organization.",
					returnTo: pending.returnTo,
				}),
				{ status: 400, headers: { "Content-Type": "text/html; charset=utf-8" } },
			);
		}
		try {
			const auth = await authenticateWithOrgSelection(env, {
				pendingAuthenticationToken: pending.pendingAuthenticationToken,
				organizationId: orgId,
				ipAddress: clientIp(request),
				userAgent: clientUa(request),
			});
			return finalizeWorkOSLogin(
				request,
				url,
				env,
				auth,
				{
					returnTo: body.return_to || pending.returnTo,
					cliRedirectUri: pending.cliRedirectUri,
					cliState: pending.cliState,
					cliCodeChallenge: pending.cliCodeChallenge,
				},
				[clearShortAuthCookie(WORKOS_PENDING_ORG_COOKIE, secure)],
			);
		} catch (err) {
			const he = fromUnknownError(err);
			return new Response(
				renderOrgPickerPage({
					email: "",
					organizations: pending.organizations,
					pendingSelection: true,
					error: he.message,
					returnTo: pending.returnTo,
				}),
				{ status: 401, headers: { "Content-Type": "text/html; charset=utf-8" } },
			);
		}
	}

	// 6. GET /auth/orgs — Organization picker & creation onboarding page
	if (pathname === "/auth/orgs" && request.method === "GET") {
		if (!workosEnabled) {
			return Response.redirect(new URL("/", url).toString(), 302);
		}
		try {
			const resolved = await resolveSessionCookie(request, env, {
				allowTopLevelNavigate: true,
				allowNoOrg: true,
			});
			if (!resolved) {
				return Response.redirect(new URL("/auth/login", url).toString(), 302);
			}
			const orgs = await listUserOrganizations(env, resolved.session.userId);
			const returnTo = isSafeReturnTo(url.searchParams.get("return_to"));
			const res = new Response(
				renderOrgPickerPage({
					email: resolved.session.email,
					organizations: orgs,
					returnTo,
				}),
				{ status: 200, headers: { "Content-Type": "text/html; charset=utf-8" } },
			);
			return withSetCookie(res, resolved.setCookie);
		} catch {
			return Response.redirect(new URL("/auth/login", url).toString(), 302);
		}
	}

	// 7. POST /auth/cli/exchange — Exchange one-time PKCE-bound CLI handoff code for tokens
	if (pathname === "/auth/cli/exchange" && request.method === "POST") {
		if (!workosEnabled) {
			return Response.json(
				{ code: ErrorCode.InvalidArgs, message: "WorkOS is not configured" },
				{ status: 400 },
			);
		}
		const body = await readBodyParams(request);
		const code = (body.code ?? "").trim();
		const codeVerifier = (body.codeVerifier || body.code_verifier || "").trim();
		const redirectUri = (body.redirectUri || body.redirect_uri || "").trim();
		const handoff = await unsealPayload<CliHandoffPayload>(env, "cli-handoff", code);
		if (!handoff || !handoff.jti || handoff.exp < Date.now()) {
			return Response.json(
				{ code: ErrorCode.NotAuthorized, message: "invalid or expired CLI login code" },
				{ status: 401 },
			);
		}
		if (handoff.redirectUri !== redirectUri) {
			return Response.json(
				{ code: ErrorCode.NotAuthorized, message: "redirect_uri mismatch" },
				{ status: 401 },
			);
		}
		const expectedChallenge = await pkceS256(codeVerifier);
		if (!(await timingSafeStringEqual(expectedChallenge, handoff.codeChallenge))) {
			return Response.json(
				{ code: ErrorCode.NotAuthorized, message: "invalid PKCE code_verifier" },
				{ status: 401 },
			);
		}
		// Enforce single-use redemption across isolates via the default ControllerDO.
		const consumeRes = await env.CONTROLLER.getByName(defaultDoName(request, url, env)).fetch(
			new Request(new URL("/internal/consume-handoff", url).toString(), {
				method: "POST",
				headers: { "Content-Type": "application/json" },
				body: JSON.stringify({ jti: handoff.jti, exp: handoff.exp }),
			}),
		);
		if (!consumeRes.ok) {
			return Response.json(
				{ code: ErrorCode.NotAuthorized, message: "CLI login code has already been used" },
				{ status: 401 },
			);
		}
		let organizations: WorkOSOrganizationSummary[] = [];
		try {
			organizations = await listUserOrganizations(env, handoff.user.id);
		} catch {
			organizations = [];
		}
		return Response.json({
			accessToken: handoff.accessToken,
			refreshToken: handoff.refreshToken,
			expiresAt: handoff.expiresAt,
			user: {
				id: handoff.user.id,
				email: handoff.user.email,
				firstName: handoff.user.firstName,
				lastName: handoff.user.lastName,
				name: formatDisplayName(handoff.user),
			},
			organization: handoff.organizationId
				? {
						id: handoff.organizationId,
						name: handoff.organizationName || handoff.organizationId,
						role: handoff.role || "member",
					}
				: null,
			organizations,
		});
	}

	// 8. POST /auth/cli/refresh — Refresh WorkOS access token (and optionally switch org) for CLI
	if (pathname === "/auth/cli/refresh" && request.method === "POST") {
		if (!workosEnabled) {
			return Response.json(
				{ code: ErrorCode.InvalidArgs, message: "WorkOS is not configured" },
				{ status: 400 },
			);
		}
		const body = await readBodyParams(request);
		const refreshToken = (body.refreshToken || body.refresh_token || "").trim();
		const organizationId = (body.organizationId || body.organization_id || "").trim() || undefined;
		if (!refreshToken) {
			return Response.json(
				{ code: ErrorCode.InvalidArgs, message: "refreshToken is required" },
				{ status: 400 },
			);
		}
		if (organizationId && !isValidOrgId(organizationId)) {
			return Response.json(
				{ code: ErrorCode.InvalidArgs, message: "invalid organizationId" },
				{ status: 400 },
			);
		}
		try {
			const refreshed = await authenticateWithRefreshToken(env, {
				refreshToken,
				organizationId,
				ipAddress: clientIp(request),
				userAgent: clientUa(request),
			});
			let organizations: WorkOSOrganizationSummary[] = [];
			try {
				organizations = await listUserOrganizations(env, refreshed.user.id);
			} catch {
				organizations = [];
			}
			const orgName =
				organizations.find((o) => o.id === refreshed.organizationId)?.name ||
				refreshed.organizationId;
			return Response.json({
				accessToken: refreshed.accessToken,
				refreshToken: refreshed.refreshToken,
				expiresAt: refreshed.expiresAt,
				user: {
					id: refreshed.user.id,
					email: refreshed.user.email,
					firstName: refreshed.user.firstName,
					lastName: refreshed.user.lastName,
					name: formatDisplayName(refreshed.user),
				},
				organization: refreshed.organizationId
					? {
							id: refreshed.organizationId,
							name: orgName,
							role:
								refreshed.role ||
								organizations.find((o) => o.id === refreshed.organizationId)?.role ||
								"member",
						}
					: null,
				organizations,
			});
		} catch (err) {
			const he = fromUnknownError(err);
			return Response.json(he.toJSON(), { status: 401 });
		}
	}

	// 9. POST /auth/cli/device/start & /auth/cli/device/poll — Headless CLI Device Flow
	if (pathname === "/auth/cli/device/start" && request.method === "POST") {
		if (!workosEnabled) {
			return Response.json(
				{ code: ErrorCode.InvalidArgs, message: "WorkOS is not configured" },
				{ status: 400 },
			);
		}
		try {
			const d = await startDeviceAuthorization(env);
			return Response.json(d);
		} catch (err) {
			const he = fromUnknownError(err);
			return Response.json(he.toJSON(), { status: 502 });
		}
	}

	if (pathname === "/auth/cli/device/poll" && request.method === "POST") {
		if (!workosEnabled) {
			return Response.json(
				{ code: ErrorCode.InvalidArgs, message: "WorkOS is not configured" },
				{ status: 400 },
			);
		}
		const body = await readBodyParams(request);
		const deviceCode = (body.deviceCode || body.device_code || "").trim();
		if (!deviceCode) {
			return Response.json(
				{ code: ErrorCode.InvalidArgs, message: "deviceCode is required" },
				{ status: 400 },
			);
		}
		try {
			const polled = await pollDeviceToken(env, deviceCode);
			if (polled.status !== "complete") {
				return Response.json({ status: polled.status });
			}
			let active = polled.auth;
			let organizations: WorkOSOrganizationSummary[] = [];
			try {
				organizations = await listUserOrganizations(env, active.user.id);
			} catch {
				organizations = [];
			}
			if (!active.organizationId && organizations.length === 1 && active.refreshToken) {
				try {
					active = await authenticateWithRefreshToken(env, {
						refreshToken: active.refreshToken,
						organizationId: organizations[0].id,
						ipAddress: clientIp(request),
						userAgent: clientUa(request),
					});
				} catch {
					// leave unselected
				}
			}
			const orgName =
				organizations.find((o) => o.id === active.organizationId)?.name || active.organizationId;
			return Response.json({
				status: "complete",
				accessToken: active.accessToken,
				refreshToken: active.refreshToken,
				expiresAt: active.expiresAt,
				user: {
					id: active.user.id,
					email: active.user.email,
					firstName: active.user.firstName,
					lastName: active.user.lastName,
					name: formatDisplayName(active.user),
				},
				organization: active.organizationId
					? {
							id: active.organizationId,
							name: orgName,
							role:
								active.role ||
								organizations.find((o) => o.id === active.organizationId)?.role ||
								"member",
						}
					: null,
				organizations,
			});
		} catch (err) {
			const he = fromUnknownError(err);
			return Response.json(he.toJSON(), { status: 401 });
		}
	}

	// 10. POST /auth/logout
	if (pathname === "/auth/logout" && request.method === "POST") {
		const csrfErr = checkSameOrigin(request);
		if (csrfErr) {
			return Response.json({ code: ErrorCode.NotAuthorized, message: csrfErr }, { status: 403 });
		}
		let logoutUrl = "/auth/login";
		if (workosEnabled) {
			const rawCookie = getCookie(request, WORKOS_SESSION_COOKIE);
			if (rawCookie) {
				const session = await unsealPayload<WorkOSSessionPayload>(env, "session", rawCookie);
				if (session?.sessionId) {
					logoutUrl = buildLogoutUrl(env, session.sessionId, `${url.origin}/auth/login`);
				}
			}
		}
		const headers = new Headers();
		headers.append("Set-Cookie", clearSessionCookie(secure));
		headers.append("Set-Cookie", clearWorkOSSessionCookie(secure));
		headers.append("Set-Cookie", clearShortAuthCookie(WORKOS_OAUTH_STATE_COOKIE, secure));
		headers.append("Set-Cookie", clearShortAuthCookie(WORKOS_PENDING_ORG_COOKIE, secure));
		if (wantsJson(request)) {
			return Response.json({ ok: true, logoutUrl }, { status: 200, headers });
		}
		headers.set("Location", logoutUrl);
		return new Response(null, { status: 303, headers });
	}

	// 11. GET /auth/me — Current user, active org, and org memberships
	if (pathname === "/auth/me" && request.method === "GET") {
		const bearer = getBearerToken(request);
		const isOwnerBearer = Boolean(
			bearer && ownerToken && (await timingSafeStringEqual(bearer, ownerToken)),
		);
		// Check WorkOS cookie / JWT first (unless bearer matches OWNER_TOKEN)
		if (workosEnabled && !isOwnerBearer) {
			const hasWosCookie = Boolean(getCookie(request, WORKOS_SESSION_COOKIE));
			if (
				(bearer && !parseOrgScopedToken(bearer) && bearer.includes(".")) ||
				(!bearer && hasWosCookie)
			) {
				let rotatedCookie: string | undefined;
				try {
					const ctx = await resolveWorkOSCallerForOrgRoute(request, env, { allowNoOrg: true });
					rotatedCookie = ctx.setCookie;
					let organizations: WorkOSOrganizationSummary[] = [];
					try {
						organizations = await listUserOrganizations(env, ctx.userId);
					} catch {
						organizations = [];
					}
					const orgName =
						ctx.organizationName ||
						organizations.find((o) => o.id === ctx.organizationId)?.name ||
						ctx.organizationId;
					const res = Response.json({
						authenticated: true,
						authMethod: ctx.authMethod,
						workosEnabled: true,
						user: {
							id: ctx.userId,
							email: ctx.email,
							firstName: ctx.firstName,
							lastName: ctx.lastName,
							name: formatDisplayName(ctx),
						},
						organization: ctx.organizationId
							? { id: ctx.organizationId, name: orgName, role: ctx.role }
							: null,
						organizations,
					});
					return withSetCookie(res, rotatedCookie);
				} catch (err) {
					const he = fromUnknownError(err);
					return withSetCookie(Response.json(he.toJSON(), { status: 401 }), rotatedCookie);
				}
			}
		}

		// Org-scoped API token (`hbu_<org>.<secret>`) — validate inside its target DO
		const scoped = bearer && !isOwnerBearer ? parseOrgScopedToken(bearer) : null;
		if (scoped && scoped.kind === "api") {
			const orgId = scoped.orgId ?? "";
			const doName = orgId ? doNameForOrg(orgId, env) : defaultDoName(request, url, env);
			const checkUrl = new URL("/auth/check", url);
			const headers = new Headers();
			headers.set("Authorization", `Bearer ${bearer}`);
			if (orgId) headers.set(INTERNAL_ORG_HEADER, orgId);
			const doRes = await env.CONTROLLER.getByName(doName).fetch(
				new Request(checkUrl.toString(), { method: "GET", headers }),
			);
			if (!doRes.ok) {
				return doRes;
			}
			const info = (await doRes.json()) as {
				login: string;
				userId?: string;
				orgId?: string;
				orgRole?: string;
				authMethod: string;
			};
			let orgName = info.orgId || orgId;
			let organizations: WorkOSOrganizationSummary[] = [];
			if (workosEnabled && orgId) {
				try {
					orgName = (await getOrganization(env, orgId)).name;
				} catch {
					// keep id
				}
				if (info.userId) {
					try {
						organizations = await listUserOrganizations(env, info.userId);
					} catch {
						organizations = [];
					}
				}
			}
			return Response.json({
				authenticated: true,
				authMethod: info.authMethod,
				workosEnabled,
				user: {
					id: info.userId ?? "",
					email: info.login,
					firstName: "",
					lastName: "",
					name: info.login,
				},
				organization: orgId ? { id: orgId, name: orgName, role: info.orgRole || "member" } : null,
				organizations,
			});
		}

		// Legacy OWNER_TOKEN / hotbed_session / CF Access
		try {
			const caller = await authenticateRequest(request, env, NOOP_TOKEN_LOOKUP);
			if (caller.role !== "owner") {
				return new Response("Forbidden", { status: 403 });
			}
			return Response.json({
				authenticated: true,
				authMethod: caller.authMethod,
				workosEnabled,
				user: {
					id: caller.userId,
					email: caller.login,
					firstName: "",
					lastName: "",
					name: caller.login,
				},
				organization: caller.orgId
					? { id: caller.orgId, name: caller.orgId, role: caller.orgRole }
					: null,
				organizations: [],
			});
		} catch (err) {
			const he = fromUnknownError(err);
			return Response.json(he.toJSON(), { status: 401 });
		}
	}

	// 12. GET /auth/check
	if (pathname === "/auth/check" && request.method === "GET") {
		try {
			const routing = await resolveEdgeRouting(request, url, env);
			if (routing.caller) {
				if (routing.caller.role !== "owner") {
					return new Response("Forbidden", { status: 403 });
				}
				const res = Response.json({
					ok: true,
					login: routing.caller.login,
					userId: routing.caller.userId,
					orgId: routing.caller.orgId,
					orgRole: routing.caller.orgRole,
					authMethod: routing.caller.authMethod,
				});
				return withSetCookie(res, routing.setCookie);
			}
			if (routing.needsOrgSelection) {
				return withSetCookie(
					Response.json(
						{ code: ErrorCode.NotAuthorized, message: "no active organization selected" },
						{ status: 401 },
					),
					routing.setCookie,
				);
			}
			const fwd = attachInternalAuth(request, routing);
			const doRes = await env.CONTROLLER.getByName(routing.doName).fetch(fwd);
			return withSetCookie(doRes, routing.setCookie);
		} catch (err) {
			const he = fromUnknownError(err);
			return Response.json(he.toJSON(), { status: 401 });
		}
	}

	// 13. POST /auth/org/switch — Switch active organization
	if (pathname === "/auth/org/switch" && request.method === "POST") {
		if (!workosEnabled) {
			return Response.json(
				{ code: ErrorCode.InvalidArgs, message: "WorkOS is not configured" },
				{ status: 400 },
			);
		}
		let rotatedCookie: string | undefined;
		try {
			const body = await readBodyParams(request);
			const targetOrgId = (body.organizationId || body.organization_id || "").trim();
			const returnTo = isSafeReturnTo(body.return_to ?? null);
			if (!targetOrgId || !isValidOrgId(targetOrgId)) {
				return Response.json(
					{ code: ErrorCode.InvalidArgs, message: "valid organizationId is required" },
					{ status: 400 },
				);
			}

			const ctx = await resolveWorkOSCallerForOrgRoute(request, env, { allowNoOrg: true });
			rotatedCookie = ctx.setCookie;
			const refreshToken = (
				ctx.authMethod === "workos-cookie"
					? ctx.session?.refreshToken || ""
					: body.refreshToken || body.refresh_token || ""
			).trim();
			if (!refreshToken) {
				return withSetCookie(
					Response.json(
						{
							code: ErrorCode.InvalidArgs,
							message: "refreshToken is required to switch organizations",
						},
						{ status: 400 },
					),
					rotatedCookie,
				);
			}

			const refreshed = await authenticateWithRefreshToken(env, {
				refreshToken,
				organizationId: targetOrgId,
				ipAddress: clientIp(request),
				userAgent: clientUa(request),
			});
			const verifiedClaims = await verifyWorkOSAccessToken(refreshed.accessToken, env);
			if (
				!verifiedClaims ||
				verifiedClaims.orgId !== targetOrgId ||
				verifiedClaims.sub !== ctx.userId
			) {
				return withSetCookie(
					Response.json(
						{
							code: ErrorCode.NotAuthorized,
							message: `WorkOS did not issue a valid access token for organization ${targetOrgId}`,
						},
						{ status: 401 },
					),
					rotatedCookie,
				);
			}
			let orgName = targetOrgId;
			try {
				orgName = (await getOrganization(env, targetOrgId)).name;
			} catch {
				// fallback to id
			}

			const nextSession: WorkOSSessionPayload = {
				userId: refreshed.user.id,
				email: refreshed.user.email,
				firstName: refreshed.user.firstName,
				lastName: refreshed.user.lastName,
				organizationId: verifiedClaims.orgId,
				organizationName: orgName,
				role: verifiedClaims.role || "member",
				sessionId: refreshed.sessionId || ctx.session?.sessionId || "",
				accessToken: refreshed.accessToken,
				refreshToken: refreshed.refreshToken,
				expiresAt: refreshed.expiresAt,
			};

			const headers = new Headers();
			if (ctx.authMethod === "workos-cookie") {
				const sealed = await sealPayload(env, "session", nextSession);
				headers.append("Set-Cookie", makeWorkOSSessionCookie(sealed, secure));
			}

			if (wantsJson(request)) {
				const payload: Record<string, unknown> = {
					organization: {
						id: nextSession.organizationId,
						name: nextSession.organizationName,
						role: nextSession.role,
					},
				};
				if (ctx.authMethod === "workos-jwt") {
					payload.accessToken = refreshed.accessToken;
					payload.refreshToken = refreshed.refreshToken;
					payload.expiresAt = refreshed.expiresAt;
				}
				return Response.json(payload, { status: 200, headers });
			}
			headers.set("Location", returnTo);
			return new Response(null, { status: 303, headers });
		} catch (err) {
			const he = fromUnknownError(err);
			return withSetCookie(
				Response.json(he.toJSON(), {
					status: he.code === ErrorCode.NotAuthorized ? 401 : 400,
				}),
				rotatedCookie,
			);
		}
	}

	// 14. POST /auth/org/create — Create a new WorkOS Organization and switch into it
	if (pathname === "/auth/org/create" && request.method === "POST") {
		if (!workosEnabled) {
			return Response.json(
				{ code: ErrorCode.InvalidArgs, message: "WorkOS is not configured" },
				{ status: 400 },
			);
		}
		let rotatedCookie: string | undefined;
		try {
			const ctx = await resolveWorkOSCallerForOrgRoute(request, env, { allowNoOrg: true });
			rotatedCookie = ctx.setCookie;
			const body = await readBodyParams(request);
			const name = (body.name ?? "").trim();
			const returnTo = isSafeReturnTo(body.return_to ?? null);
			if (!name) {
				return withSetCookie(
					Response.json(
						{ code: ErrorCode.InvalidArgs, message: "organization name is required" },
						{ status: 400 },
					),
					rotatedCookie,
				);
			}

			const org = await createOrganization(env, name);
			const membership = await createOrganizationMembership(env, org.id, ctx.userId, "admin");

			const refreshToken = (
				ctx.authMethod === "workos-cookie"
					? ctx.session?.refreshToken || ""
					: body.refreshToken || body.refresh_token || ""
			).trim();
			const headers = new Headers();
			let refreshedTokens:
				| { accessToken: string; refreshToken: string; expiresAt: number }
				| undefined;
			let activeRole = membership.role;

			if (refreshToken) {
				const refreshed = await authenticateWithRefreshToken(env, {
					refreshToken,
					organizationId: org.id,
					ipAddress: clientIp(request),
					userAgent: clientUa(request),
				});
				const verifiedClaims = await verifyWorkOSAccessToken(refreshed.accessToken, env);
				if (
					!verifiedClaims ||
					verifiedClaims.orgId !== org.id ||
					verifiedClaims.sub !== ctx.userId
				) {
					return withSetCookie(
						Response.json(
							{
								code: ErrorCode.NotAuthorized,
								message: `WorkOS did not issue a valid access token for created organization ${org.id}`,
							},
							{ status: 401 },
						),
						rotatedCookie,
					);
				}
				activeRole = verifiedClaims.role || membership.role || "member";
				if (ctx.authMethod === "workos-cookie") {
					const nextSession: WorkOSSessionPayload = {
						userId: refreshed.user.id,
						email: refreshed.user.email,
						firstName: refreshed.user.firstName,
						lastName: refreshed.user.lastName,
						organizationId: verifiedClaims.orgId,
						organizationName: org.name,
						role: activeRole,
						sessionId: refreshed.sessionId || ctx.session?.sessionId || "",
						accessToken: refreshed.accessToken,
						refreshToken: refreshed.refreshToken,
						expiresAt: refreshed.expiresAt,
					};
					const sealed = await sealPayload(env, "session", nextSession);
					headers.append("Set-Cookie", makeWorkOSSessionCookie(sealed, secure));
				} else {
					refreshedTokens = {
						accessToken: refreshed.accessToken,
						refreshToken: refreshed.refreshToken,
						expiresAt: refreshed.expiresAt,
					};
				}
			} else if (rotatedCookie) {
				headers.append("Set-Cookie", rotatedCookie);
			}

			if (wantsJson(request)) {
				return Response.json(
					{
						organization: { id: org.id, name: org.name, role: activeRole },
						...refreshedTokens,
					},
					{ status: 201, headers },
				);
			}
			headers.set("Location", returnTo);
			return new Response(null, { status: 303, headers });
		} catch (err) {
			const he = fromUnknownError(err);
			return withSetCookie(
				Response.json(he.toJSON(), {
					status:
						he.code === ErrorCode.NotAuthorized
							? 401
							: he.code === ErrorCode.InvalidArgs
								? 400
								: 500,
				}),
				rotatedCookie,
			);
		}
	}

	// 15. GET /auth/org/members — List active org's members & pending invitations
	if (pathname === "/auth/org/members" && request.method === "GET") {
		if (!workosEnabled) {
			return Response.json(
				{ code: ErrorCode.InvalidArgs, message: "WorkOS is not configured" },
				{ status: 400 },
			);
		}
		let rotatedCookie: string | undefined;
		try {
			const ctx = await resolveWorkOSCallerForOrgRoute(request, env);
			rotatedCookie = ctx.setCookie;
			const admin = isOrgAdmin(ctx.role);
			const [members, rawInvitations] = await Promise.all([
				listOrganizationMembers(env, ctx.organizationId),
				listOrganizationInvitations(env, ctx.organizationId),
			]);
			// Never expose secret acceptInvitationUrl tokens to non-admin members.
			const invitations = admin
				? rawInvitations
				: rawInvitations.map(({ acceptInvitationUrl: _url, ...rest }) => rest);
			const res = Response.json({
				organization: {
					id: ctx.organizationId,
					name: ctx.organizationName || ctx.organizationId,
					role: ctx.role,
				},
				members,
				invitations,
			});
			return withSetCookie(res, rotatedCookie);
		} catch (err) {
			const he = fromUnknownError(err);
			return withSetCookie(
				Response.json(he.toJSON(), {
					status: he.code === ErrorCode.NotAuthorized ? 401 : 500,
				}),
				rotatedCookie,
			);
		}
	}

	// 16. POST /auth/org/invite — Invite a user to the active organization (org admin only)
	if (pathname === "/auth/org/invite" && request.method === "POST") {
		if (!workosEnabled) {
			return Response.json(
				{ code: ErrorCode.InvalidArgs, message: "WorkOS is not configured" },
				{ status: 400 },
			);
		}
		let rotatedCookie: string | undefined;
		try {
			const ctx = await resolveWorkOSCallerForOrgRoute(request, env);
			rotatedCookie = ctx.setCookie;
			if (!isOrgAdmin(ctx.role)) {
				return withSetCookie(
					Response.json(
						hotbedError(ErrorCode.NotAuthorized, "only organization admins can invite members")
							.withSuggestion("ask an organization admin to send this invitation")
							.toJSON(),
						{ status: 403 },
					),
					rotatedCookie,
				);
			}
			const body = await readBodyParams(request);
			const email = (body.email ?? "").trim();
			const roleSlug = (body.roleSlug || body.role || "").trim() || undefined;
			const invitation = await sendOrganizationInvitation(env, {
				email,
				organizationId: ctx.organizationId,
				roleSlug,
				inviterUserId: ctx.userId,
			});
			return withSetCookie(Response.json({ invitation }, { status: 201 }), rotatedCookie);
		} catch (err) {
			const he = fromUnknownError(err);
			return withSetCookie(
				Response.json(he.toJSON(), {
					status:
						he.code === ErrorCode.NotAuthorized
							? 401
							: he.code === ErrorCode.InvalidArgs
								? 400
								: 500,
				}),
				rotatedCookie,
			);
		}
	}

	// 17. DELETE /auth/org/invitations/:id — Revoke a pending invitation in the active org (org admin only)
	if (pathname.startsWith("/auth/org/invitations/") && request.method === "DELETE") {
		if (!workosEnabled) {
			return Response.json(
				{ code: ErrorCode.InvalidArgs, message: "WorkOS is not configured" },
				{ status: 400 },
			);
		}
		const invitationId = decodePathSegment(pathname.slice("/auth/org/invitations/".length));
		if (!invitationId) {
			return Response.json(
				{ code: ErrorCode.InvalidArgs, message: "valid invitation id is required" },
				{ status: 400 },
			);
		}
		let rotatedCookie: string | undefined;
		try {
			const ctx = await resolveWorkOSCallerForOrgRoute(request, env);
			rotatedCookie = ctx.setCookie;
			if (!isOrgAdmin(ctx.role)) {
				return withSetCookie(
					Response.json(
						hotbedError(
							ErrorCode.NotAuthorized,
							"only organization admins can revoke invitations",
						).toJSON(),
						{ status: 403 },
					),
					rotatedCookie,
				);
			}
			const inv = await getInvitation(env, invitationId);
			if (inv.organizationId !== ctx.organizationId) {
				return withSetCookie(
					Response.json(
						{ code: ErrorCode.NotFound, message: "invitation not found in this organization" },
						{ status: 404 },
					),
					rotatedCookie,
				);
			}
			await revokeOrganizationInvitation(env, invitationId);
			return withSetCookie(Response.json({ ok: true, id: invitationId }), rotatedCookie);
		} catch (err) {
			const he = fromUnknownError(err);
			return withSetCookie(
				Response.json(he.toJSON(), {
					status:
						he.code === ErrorCode.NotAuthorized ? 401 : he.code === ErrorCode.NotFound ? 404 : 500,
				}),
				rotatedCookie,
			);
		}
	}

	// 18. DELETE /auth/org/members/:membershipId — Remove a member from the active org (org admin only)
	if (pathname.startsWith("/auth/org/members/") && request.method === "DELETE") {
		if (!workosEnabled) {
			return Response.json(
				{ code: ErrorCode.InvalidArgs, message: "WorkOS is not configured" },
				{ status: 400 },
			);
		}
		const membershipId = decodePathSegment(pathname.slice("/auth/org/members/".length));
		if (!membershipId) {
			return Response.json(
				{ code: ErrorCode.InvalidArgs, message: "valid membership id is required" },
				{ status: 400 },
			);
		}
		let rotatedCookie: string | undefined;
		try {
			const ctx = await resolveWorkOSCallerForOrgRoute(request, env);
			rotatedCookie = ctx.setCookie;
			if (!isOrgAdmin(ctx.role)) {
				return withSetCookie(
					Response.json(
						hotbedError(
							ErrorCode.NotAuthorized,
							"only organization admins can remove members",
						).toJSON(),
						{ status: 403 },
					),
					rotatedCookie,
				);
			}
			const mem = await getOrganizationMembership(env, membershipId);
			if (mem.organizationId !== ctx.organizationId) {
				return withSetCookie(
					Response.json(
						{ code: ErrorCode.NotFound, message: "member not found in this organization" },
						{ status: 404 },
					),
					rotatedCookie,
				);
			}
			if (mem.userId === ctx.userId) {
				return withSetCookie(
					Response.json(
						{ code: ErrorCode.InvalidArgs, message: "cannot remove your own active membership" },
						{ status: 400 },
					),
					rotatedCookie,
				);
			}
			await removeOrganizationMembership(env, membershipId);
			// Immediately revoke all personal API tokens (hbu_...) held by the removed member in this org's DO.
			const adminCaller: CallerIdentity = {
				role: "owner",
				nodeId: `workos:${ctx.userId}`,
				hostname: ctx.email || ctx.userId,
				login: ctx.email || ctx.userId,
				userId: ctx.userId,
				orgId: ctx.organizationId,
				orgRole: ctx.role,
				addr: "",
				authMethod: ctx.authMethod,
			};
			const revokeRes = await env.CONTROLLER.getByName(doNameForOrg(ctx.organizationId, env)).fetch(
				new Request(new URL("/internal/revoke-user-tokens", url).toString(), {
					method: "POST",
					headers: {
						"Content-Type": "application/json",
						[INTERNAL_ORG_HEADER]: ctx.organizationId,
						[INTERNAL_AUTH_HEADER]: JSON.stringify(adminCaller),
					},
					body: JSON.stringify({ userId: mem.userId }),
				}),
			);
			if (!revokeRes.ok) {
				throw hotbedError(
					ErrorCode.Internal,
					`member removed from WorkOS, but failed to revoke API tokens (HTTP ${revokeRes.status})`,
				);
			}
			return withSetCookie(Response.json({ ok: true, membershipId }), rotatedCookie);
		} catch (err) {
			const he = fromUnknownError(err);
			return withSetCookie(
				Response.json(he.toJSON(), {
					status:
						he.code === ErrorCode.NotAuthorized ? 401 : he.code === ErrorCode.NotFound ? 404 : 500,
				}),
				rotatedCookie,
			);
		}
	}

	return new Response("Not Found", { status: 404 });
}
