import { escapeHtml } from "./encoding.ts";
import { bytesToHex } from "./gitops.ts";
import { ErrorCode, hotbedError } from "./errors.ts";
import {
	base64UrlDecode,
	isWorkOSEnabled,
	unsealPayload,
	verifyWorkOSAccessToken,
	WORKOS_SESSION_COOKIE,
	type WorkOSEnv,
	type WorkOSSessionPayload,
} from "./workos.ts";

export interface CallerIdentity {
	role: "owner" | "worker";
	nodeId: string;
	hostname: string;
	login: string;
	userId: string;
	orgId: string;
	orgRole: string;
	addr: string;
	authMethod:
		| "bearer"
		| "worker-token"
		| "join-token"
		| "api-token"
		| "cookie"
		| "cf-access"
		| "workos-jwt"
		| "workos-cookie";
}

/**
 * Reports whether caller is authorized to mutate organization-level settings,
 * worker configuration, host upgrades, and git credentials.
 *
 * Workers and non-admin WorkOS org members (`orgRole === "member"`) are denied;
 * org admins/owners and single-tenant owner callers are permitted.
 */
export function isOrgAdminCaller(caller: CallerIdentity): boolean {
	if (caller.role !== "owner") return false;
	return !caller.orgRole || caller.orgRole === "admin" || caller.orgRole === "owner";
}

export interface AuthEnv extends WorkOSEnv {
	OWNER_TOKEN?: string;
	JOIN_TOKEN?: string;
	CLUSTER_OWNER?: string;
	CF_ACCESS_AUD?: string;
	CF_ACCESS_TEAM_DOMAIN?: string;
}

export interface TokenStoreLookup {
	findWorkerToken(tokenHash: string): { nodeId: string; hostname: string } | null;
	findJoinToken?(tokenHash: string, nowMs: number): { id: string; label: string } | null;
	findApiToken?(
		tokenHash: string,
		nowMs: number,
	): { id: string; name: string; userId: string; email: string; orgRole: string } | null;
}

export const NOOP_TOKEN_LOOKUP: TokenStoreLookup = {
	findWorkerToken: () => null,
	findJoinToken: () => null,
	findApiToken: () => null,
};

/**
 * Internal headers injected by the Worker edge (`index.ts`) after cryptographic
 * WorkOS authentication and stripped from all untrusted incoming requests.
 */
export const INTERNAL_AUTH_HEADER = "X-Hotbed-Internal-Auth";
export const INTERNAL_ORG_HEADER = "X-Hotbed-Internal-Org";
export const INTERNAL_GLOBAL_ADMIN_HEADER = "X-Hotbed-Internal-Global-Admin";
export const INTERNAL_DEFAULT_DO_HEADER = "X-Hotbed-Internal-Default-DO";

export function stripInternalAuthHeaders(headers: Headers): void {
	const toDelete: string[] = [];
	for (const key of headers.keys()) {
		if (key.toLowerCase().startsWith("x-hotbed-internal-")) {
			toDelete.push(key);
		}
	}
	for (const key of toDelete) {
		headers.delete(key);
	}
}

export async function sha256Hex(input: string): Promise<string> {
	const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(input));
	return bytesToHex(new Uint8Array(digest));
}

/** Constant-time string comparison via SHA-256 + crypto.subtle.timingSafeEqual. */
export async function timingSafeStringEqual(a: string, b: string): Promise<boolean> {
	const enc = new TextEncoder();
	const [ha, hb] = await Promise.all([
		crypto.subtle.digest("SHA-256", enc.encode(a)),
		crypto.subtle.digest("SHA-256", enc.encode(b)),
	]);
	return crypto.subtle.timingSafeEqual(ha, hb);
}

async function hmacCookieValue(secret: string): Promise<string> {
	const key = await crypto.subtle.importKey(
		"raw",
		new TextEncoder().encode(secret),
		{ name: "HMAC", hash: "SHA-256" },
		false,
		["sign"],
	);
	const sig = await crypto.subtle.sign("HMAC", key, new TextEncoder().encode("hotbed-cockpit-v1"));
	return bytesToHex(new Uint8Array(sig));
}

export async function makeSessionCookie(ownerToken: string, secure: boolean): Promise<string> {
	const val = await hmacCookieValue(ownerToken);
	const attrs = [`hotbed_session=${val}`, "Path=/", "HttpOnly", "SameSite=Lax", "Max-Age=2592000"];
	if (secure) attrs.push("Secure");
	return attrs.join("; ");
}

export function clearSessionCookie(secure: boolean): string {
	const attrs = ["hotbed_session=", "Path=/", "HttpOnly", "SameSite=Lax", "Max-Age=0"];
	if (secure) attrs.push("Secure");
	return attrs.join("; ");
}

export function getCookie(req: Request, name: string): string | null {
	const header = req.headers.get("Cookie");
	if (!header) return null;
	for (const part of header.split(";")) {
		const eq = part.indexOf("=");
		if (eq < 0) continue;
		if (part.slice(0, eq).trim() === name) {
			return part.slice(eq + 1).trim();
		}
	}
	return null;
}

export function getBearerToken(req: Request): string {
	const authHeader = req.headers.get("Authorization") ?? "";
	if (authHeader.toLowerCase().startsWith("bearer ")) {
		return authHeader.slice(7).trim();
	}
	return "";
}

interface JWKSCache {
	domain: string;
	fetchedAt: number;
	keys: Map<string, CryptoKey>;
}

let jwksCache: JWKSCache | null = null;

const JWKS_CACHE_TTL_MS = 5 * 60_000;
const JWKS_FORCE_REFRESH_COOLDOWN_MS = 10_000;
const JWT_CLOCK_SKEW_MS = 60_000;

async function getAccessKeys(
	teamDomain: string,
	forceRefresh = false,
): Promise<Map<string, CryptoKey>> {
	const now = Date.now();
	const freshEnough =
		jwksCache &&
		jwksCache.domain === teamDomain &&
		now - jwksCache.fetchedAt < (forceRefresh ? JWKS_FORCE_REFRESH_COOLDOWN_MS : JWKS_CACHE_TTL_MS);
	if (freshEnough && jwksCache) {
		return jwksCache.keys;
	}
	const domain = teamDomain.replace(/^https?:\/\//, "").replace(/\/+$/, "");
	const res = await fetch(`https://${domain}/cdn-cgi/access/certs`);
	if (!res.ok) {
		throw new Error(`fetch Access JWKS: ${res.status}`);
	}
	const data = (await res.json()) as { keys?: Array<JsonWebKey & { kid?: string }> };
	const keys = new Map<string, CryptoKey>();
	for (const jwk of data.keys ?? []) {
		if (!jwk.kid || jwk.kty !== "RSA") continue;
		try {
			const key = await crypto.subtle.importKey(
				"jwk",
				jwk,
				{ name: "RSASSA-PKCS1-v1_5", hash: "SHA-256" },
				false,
				["verify"],
			);
			keys.set(jwk.kid, key);
		} catch {
			// Skip unsupported or malformed keys so one bad JWK cannot lock out all Access logins.
		}
	}
	jwksCache = { domain: teamDomain, fetchedAt: now, keys };
	return keys;
}

async function verifyCloudflareAccessJWT(
	jwt: string,
	aud: string,
	teamDomain: string,
): Promise<{ email: string } | null> {
	const parts = jwt.split(".");
	if (parts.length !== 3) return null;
	try {
		const header = JSON.parse(new TextDecoder().decode(base64UrlDecode(parts[0]))) as {
			kid?: string;
			alg?: string;
		};
		const payload = JSON.parse(new TextDecoder().decode(base64UrlDecode(parts[1]))) as {
			aud?: string | string[];
			iss?: string;
			exp?: number;
			nbf?: number;
			email?: string;
			sub?: string;
		};
		if (header.alg !== "RS256" || !header.kid) return null;
		const auds = Array.isArray(payload.aud) ? payload.aud : payload.aud ? [payload.aud] : [];
		if (!auds.includes(aud)) return null;
		const domain = teamDomain.replace(/^https?:\/\//, "").replace(/\/+$/, "");
		if (payload.iss && payload.iss.replace(/\/+$/, "") !== `https://${domain}`) return null;
		const now = Date.now();
		if (typeof payload.exp !== "number" || payload.exp * 1000 <= now) return null;
		if (typeof payload.nbf === "number" && payload.nbf * 1000 > now + JWT_CLOCK_SKEW_MS) {
			return null;
		}
		let keys = await getAccessKeys(teamDomain, false);
		let key = keys.get(header.kid);
		if (!key) {
			keys = await getAccessKeys(teamDomain, true);
			key = keys.get(header.kid);
		}
		if (!key) return null;
		const sig = base64UrlDecode(parts[2]);
		const signed = new TextEncoder().encode(`${parts[0]}.${parts[1]}`);
		const valid = await crypto.subtle.verify("RSASSA-PKCS1-v1_5", key, sig, signed);
		if (!valid) return null;
		return { email: payload.email || payload.sub || "access-user" };
	} catch {
		return null;
	}
}

export function checkSameOrigin(
	req: Request,
	opts?: { allowTopLevelNavigate?: boolean },
): string | null {
	const site = (req.headers.get("Sec-Fetch-Site") ?? "").trim().toLowerCase();
	if (site && site !== "same-origin" && site !== "none") {
		const mode = (req.headers.get("Sec-Fetch-Mode") ?? "").trim().toLowerCase();
		const dest = (req.headers.get("Sec-Fetch-Dest") ?? "").trim().toLowerCase();
		const isSafeTopLevelNav =
			Boolean(opts?.allowTopLevelNavigate) &&
			req.method === "GET" &&
			mode === "navigate" &&
			(dest === "document" || dest === "");
		if (!isSafeTopLevelNav) {
			return `cross-site browser request (${site}) is not allowed`;
		}
	}
	const origin = (req.headers.get("Origin") ?? "").trim();
	if (origin) {
		try {
			const ou = new URL(origin);
			const ru = new URL(req.url);
			if (ou.origin !== ru.origin) {
				return `origin "${origin}" does not match host "${ru.host}"`;
			}
		} catch {
			return `invalid origin "${origin}"`;
		}
	}
	return null;
}

/**
 * Authenticates a request fail-closed.
 * Order:
 *  0. Edge-verified internal WorkOS identity header (only trusted inside ControllerDO after index.ts strips external headers)
 *  1. Authorization: Bearer <token>
 *     - matches OWNER_TOKEN -> owner
 *     - matches per-org API token hash in api_tokens -> owner (org user)
 *     - matches per-worker token hash in worker_tokens -> worker (nodeId + hostname DB-bound)
 *     - matches per-org join token hash in join_tokens or JOIN_TOKEN when allowJoinToken is set -> worker
 *     - verified WorkOS JWT when WorkOS is configured -> owner (org user)
 *  2. WorkOS sealed session cookie (hotbed_wos_session) + same-origin CSRF check
 *  3. Cloudflare Access JWT (Cf-Access-Jwt-Assertion) when CF_ACCESS_AUD + CF_ACCESS_TEAM_DOMAIN are set
 *  4. Legacy session cookie (hotbed_session) + same-origin CSRF check
 */
export async function authenticateRequest(
	req: Request,
	env: AuthEnv,
	tokens: TokenStoreLookup,
	opts?: {
		allowJoinToken?: boolean;
		allowTopLevelNavigate?: boolean;
		trustInternalHeader?: boolean;
		nowMs?: number;
		orgId?: string;
	},
): Promise<CallerIdentity> {
	const nowMs = opts?.nowMs ?? Date.now();
	const ownerToken = (env.OWNER_TOKEN ?? "").trim();
	const joinToken = (env.JOIN_TOKEN ?? "").trim() || ownerToken;
	const ownerLogin = (env.CLUSTER_OWNER ?? "").trim() || "owner";
	const accessAud = (env.CF_ACCESS_AUD ?? "").trim();
	const accessDomain = (env.CF_ACCESS_TEAM_DOMAIN ?? "").trim();
	const workosEnabled = isWorkOSEnabled(env);

	if (!ownerToken && !(accessAud && accessDomain) && !workosEnabled) {
		throw hotbedError(
			ErrorCode.NotAuthorized,
			"this controller has no WorkOS, OWNER_TOKEN, or Cloudflare Access configured: refusing all requests",
		).withSuggestion(
			"configure WORKOS_CLIENT_ID + WORKOS_API_KEY or OWNER_TOKEN secret on the hotbed-controller Worker",
		);
	}

	const headerHost = (req.headers.get("X-Hotbed-Host") ?? "").trim();
	const headerNodeId = (req.headers.get("X-Hotbed-Node-Id") ?? "").trim();
	const headerAddr = (req.headers.get("X-Hotbed-Addr") ?? "").trim();
	const expectedOrgId = (opts?.orgId || req.headers.get(INTERNAL_ORG_HEADER) || "").trim();

	// 0. Edge-verified WorkOS identity forwarded from index.ts into ControllerDO
	if (opts?.trustInternalHeader) {
		const rawInternal = req.headers.get(INTERNAL_AUTH_HEADER);
		if (rawInternal) {
			try {
				const parsed = JSON.parse(rawInternal) as CallerIdentity;
				if (
					parsed &&
					parsed.role === "owner" &&
					parsed.login &&
					(!expectedOrgId || !parsed.orgId || parsed.orgId === expectedOrgId)
				) {
					return parsed;
				}
			} catch {
				// Ignore malformed internal header and fail closed below.
			}
		}
	}

	// 1. Bearer token
	const bearer = getBearerToken(req);
	if (bearer) {
		if (ownerToken && (await timingSafeStringEqual(bearer, ownerToken))) {
			return {
				role: "owner",
				nodeId: headerNodeId || `owner:${headerHost || "client"}`,
				hostname: headerHost || "client",
				login: ownerLogin,
				userId: "",
				orgId: expectedOrgId,
				orgRole: "owner",
				addr: headerAddr,
				authMethod: "bearer",
			};
		}

		const hash = await sha256Hex(bearer);

		if (tokens.findApiToken) {
			const at = tokens.findApiToken(hash, nowMs);
			if (at) {
				return {
					role: "owner",
					nodeId: headerNodeId || `user:${at.userId || at.email}`,
					hostname: headerHost || at.email,
					login: at.email,
					userId: at.userId,
					orgId: expectedOrgId,
					orgRole: at.orgRole || "member",
					addr: headerAddr,
					authMethod: "api-token",
				};
			}
		}

		const wt = tokens.findWorkerToken(hash);
		if (wt) {
			return {
				role: "worker",
				nodeId: wt.nodeId,
				hostname: wt.hostname || headerHost,
				login: ownerLogin,
				userId: "",
				orgId: expectedOrgId,
				orgRole: "worker",
				addr: headerAddr,
				authMethod: "worker-token",
			};
		}

		if (opts?.allowJoinToken) {
			if (tokens.findJoinToken) {
				const jt = tokens.findJoinToken(hash, nowMs);
				if (jt) {
					return {
						role: "worker",
						nodeId: headerNodeId || `worker:${headerHost || "unknown"}`,
						hostname: headerHost || headerNodeId || "worker",
						login: ownerLogin,
						userId: "",
						orgId: expectedOrgId,
						orgRole: "worker",
						addr: headerAddr,
						authMethod: "join-token",
					};
				}
			}
			if (joinToken && (await timingSafeStringEqual(bearer, joinToken))) {
				return {
					role: "worker",
					nodeId: headerNodeId || `worker:${headerHost || "unknown"}`,
					hostname: headerHost || headerNodeId || "worker",
					login: ownerLogin,
					userId: "",
					orgId: expectedOrgId,
					orgRole: "worker",
					addr: headerAddr,
					authMethod: "join-token",
				};
			}
		}

		if (workosEnabled && bearer.includes(".")) {
			const claims = await verifyWorkOSAccessToken(bearer, env, nowMs);
			if (claims) {
				if (!claims.orgId) {
					throw hotbedError(
						ErrorCode.NotAuthorized,
						"this WorkOS token has no active organization selected",
					).withSuggestion(
						"select or create an organization with `hotbed org switch` or in the Cockpit",
					);
				}
				if (expectedOrgId && claims.orgId !== expectedOrgId) {
					throw hotbedError(
						ErrorCode.NotAuthorized,
						`organization mismatch: token belongs to ${claims.orgId}, not ${expectedOrgId}`,
					);
				}
				const login = claims.email || claims.sub;
				return {
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
			}
		}

		throw hotbedError(ErrorCode.NotAuthorized, "invalid bearer token");
	}

	// 2. WorkOS sealed session cookie (for Cockpit UI)
	if (workosEnabled) {
		const rawCookie = getCookie(req, WORKOS_SESSION_COOKIE);
		if (rawCookie) {
			const session = await unsealPayload<WorkOSSessionPayload>(env, "session", rawCookie);
			if (session && session.accessToken) {
				const claims = await verifyWorkOSAccessToken(session.accessToken, env, nowMs);
				if (claims) {
					const csrfErr = checkSameOrigin(req, {
						allowTopLevelNavigate: opts?.allowTopLevelNavigate,
					});
					if (csrfErr) {
						throw hotbedError(ErrorCode.NotAuthorized, csrfErr);
					}
					const orgId = claims.orgId;
					if (!orgId) {
						throw hotbedError(
							ErrorCode.NotAuthorized,
							"no active organization selected for this session",
						).withSuggestion("visit /auth/orgs to choose or create an organization");
					}
					if (expectedOrgId && orgId !== expectedOrgId) {
						throw hotbedError(
							ErrorCode.NotAuthorized,
							`organization mismatch: session belongs to ${orgId}, not ${expectedOrgId}`,
						);
					}
					const login = session.email || claims.email || claims.sub;
					return {
						role: "owner",
						nodeId: `workos:${claims.sub}`,
						hostname: headerHost || login,
						login,
						userId: claims.sub,
						orgId,
						orgRole: claims.role || "member",
						addr: "",
						authMethod: "workos-cookie",
					};
				}
			}
		}
	}

	// 3. Cloudflare Access JWT
	if (accessAud && accessDomain) {
		const jwt = req.headers.get("Cf-Access-Jwt-Assertion") ?? getCookie(req, "CF_Authorization");
		if (jwt) {
			const verified = await verifyCloudflareAccessJWT(jwt, accessAud, accessDomain);
			if (verified) {
				const csrfErr = checkSameOrigin(req, {
					allowTopLevelNavigate: opts?.allowTopLevelNavigate,
				});
				if (csrfErr) {
					throw hotbedError(ErrorCode.NotAuthorized, csrfErr);
				}
				return {
					role: "owner",
					nodeId: `access:${verified.email}`,
					hostname: headerHost || verified.email,
					login: verified.email,
					userId: "",
					orgId: expectedOrgId,
					orgRole: "owner",
					addr: "",
					authMethod: "cf-access",
				};
			}
		}
	}

	// 4. Legacy session cookie (for Cockpit UI with OWNER_TOKEN)
	if (ownerToken) {
		const cookie = getCookie(req, "hotbed_session");
		if (cookie) {
			const expected = await hmacCookieValue(ownerToken);
			if (await timingSafeStringEqual(cookie, expected)) {
				const csrfErr = checkSameOrigin(req, {
					allowTopLevelNavigate: opts?.allowTopLevelNavigate,
				});
				if (csrfErr) {
					throw hotbedError(ErrorCode.NotAuthorized, csrfErr);
				}
				return {
					role: "owner",
					nodeId: "cockpit",
					hostname: headerHost || "cockpit",
					login: ownerLogin,
					userId: "",
					orgId: expectedOrgId,
					orgRole: "owner",
					addr: "",
					authMethod: "cookie",
				};
			}
		}
	}

	throw hotbedError(ErrorCode.NotAuthorized, "authentication required").withSuggestion(
		workosEnabled
			? "run `hotbed login` or sign in at /auth/login"
			: "set Authorization: Bearer <OWNER_TOKEN> (or HOTBED_TOKEN in CLI) or sign in at /auth/login",
	);
}

export function renderLoginPage(error?: string, workosEnabled = false, returnTo = "/"): string {
	const errHtml = error
		? `<p style="color:#f87171;margin:0 0 12px;font-size:14px">${escapeHtml(error)}</p>`
		: "";
	if (workosEnabled) {
		const googleHref =
			returnTo && returnTo !== "/"
				? `/auth/login?return_to=${encodeURIComponent(returnTo)}`
				: "/auth/login";
		return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width,initial-scale=1" />
<title>hotbed cockpit — sign in</title>
<style>
  body{margin:0;min-height:100vh;display:grid;place-items:center;background:#0b0d12;color:#e5e7eb;font-family:system-ui,-apple-system,sans-serif;padding:16px;box-sizing:border-box}
  .card{width:100%;max-width:360px;padding:28px;border:1px solid #1f2430;border-radius:12px;background:#121620;box-sizing:border-box}
  h1{margin:0 0 8px;font-size:18px;font-weight:600}
  p.sub{margin:0 0 18px;color:#9ca3af;font-size:13px}
  a.btn{display:block;text-align:center;text-decoration:none;width:100%;padding:10px 12px;border-radius:8px;background:#3b82f6;color:#fff;font-weight:600;font-size:14px;box-sizing:border-box}
  a.btn:hover{background:#2563eb}
</style>
</head>
<body>
<div class="card">
  <h1>hotbed cockpit</h1>
  <p class="sub">Sign in with your Google account to continue.</p>
  ${errHtml}
  <a href="${escapeHtml(googleHref)}" class="btn">Sign in with Google</a>
</div>
</body>
</html>`;
	}
	return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width,initial-scale=1" />
<title>hotbed cockpit — sign in</title>
<style>
  body{margin:0;min-height:100vh;display:grid;place-items:center;background:#0b0d12;color:#e5e7eb;font-family:system-ui,-apple-system,sans-serif;padding:16px;box-sizing:border-box}
  form{width:100%;max-width:360px;padding:28px;border:1px solid #1f2430;border-radius:12px;background:#121620;box-sizing:border-box}
  h1{margin:0 0 8px;font-size:18px;font-weight:600}
  p.sub{margin:0 0 18px;color:#9ca3af;font-size:13px}
  input{width:100%;padding:10px 12px;border-radius:8px;border:1px solid #2b3242;background:#0b0d12;color:#f9fafb;font-size:14px;box-sizing:border-box;margin-bottom:12px}
  button{width:100%;padding:10px 12px;border-radius:8px;border:0;background:#3b82f6;color:#fff;font-weight:600;font-size:14px;cursor:pointer}
  button:hover{background:#2563eb}
</style>
</head>
<body>
<form method="POST" action="/auth/login">
  <h1>hotbed cockpit</h1>
  <p class="sub">Enter your controller owner token to sign in.</p>
  ${errHtml}
  <input type="hidden" name="return_to" value="${escapeHtml(returnTo || "/")}" />
  <input type="password" name="token" placeholder="Owner token" autocomplete="current-password" autofocus required />
  <button type="submit">Sign in</button>
</form>
</body>
</html>`;
}
