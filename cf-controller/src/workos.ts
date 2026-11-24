import { bytesToHex, randomHex } from "./gitops.ts";
import { ErrorCode, hotbedError } from "./errors.ts";

export interface WorkOSEnv {
	WORKOS_CLIENT_ID?: string;
	WORKOS_API_KEY?: string;
	WORKOS_COOKIE_PASSWORD?: string;
	WORKOS_AUTH_PROVIDER?: string;
	WORKOS_API_BASE?: string;
	WORKOS_JWKS_URL?: string;
	DEFAULT_ORG_ID?: string;
}

export interface WorkOSUser {
	id: string;
	email: string;
	firstName: string;
	lastName: string;
	profilePictureUrl?: string;
}

export interface WorkOSOrganizationSummary {
	id: string;
	name: string;
	role: string;
	membershipId?: string;
}

export interface WorkOSMember {
	membershipId: string;
	userId: string;
	email: string;
	firstName: string;
	lastName: string;
	name: string;
	role: string;
	status: string;
	createdAt: string;
}

export interface WorkOSInvitation {
	id: string;
	email: string;
	state: string;
	role: string;
	expiresAt: string;
	createdAt: string;
	acceptInvitationUrl?: string;
}

export interface WorkOSSessionPayload {
	userId: string;
	email: string;
	firstName: string;
	lastName: string;
	organizationId: string;
	organizationName: string;
	role: string;
	sessionId: string;
	accessToken: string;
	refreshToken: string;
	expiresAt: number; // unix ms
}

export interface OAuthStatePayload {
	state: string;
	codeVerifier: string;
	returnTo: string;
	cliRedirectUri?: string;
	cliState?: string;
	cliCodeChallenge?: string;
	exp: number;
}

export interface PendingOrgSelectionPayload {
	pendingAuthenticationToken: string;
	organizations: Array<{ id: string; name: string }>;
	returnTo: string;
	cliRedirectUri?: string;
	cliState?: string;
	cliCodeChallenge?: string;
	exp: number;
}

export interface CliHandoffPayload {
	accessToken: string;
	refreshToken: string;
	expiresAt: number;
	user: WorkOSUser;
	organizationId: string;
	organizationName: string;
	role: string;
	codeChallenge: string;
	redirectUri: string;
	exp: number;
}

export interface VerifiedWorkOSClaims {
	sub: string;
	orgId: string;
	role: string;
	sid: string;
	email: string;
	exp: number; // unix ms
}

export type AuthenticateResult =
	| {
			kind: "authenticated";
			user: WorkOSUser;
			organizationId: string;
			accessToken: string;
			refreshToken: string;
			sessionId: string;
			role: string;
			expiresAt: number;
	  }
	| {
			kind: "organization_selection_required";
			pendingAuthenticationToken: string;
			organizations: Array<{ id: string; name: string }>;
	  };

export const WORKOS_SESSION_COOKIE = "hotbed_wos_session";
export const WORKOS_OAUTH_STATE_COOKIE = "hotbed_oauth_state";
export const WORKOS_PENDING_ORG_COOKIE = "hotbed_pending_org";

const ORG_ID_RE = /^[A-Za-z0-9_-]{1,128}$/;

export function isValidOrgId(orgId: string): boolean {
	return ORG_ID_RE.test(orgId);
}

export function isWorkOSEnabled(env: WorkOSEnv): boolean {
	return Boolean((env.WORKOS_CLIENT_ID ?? "").trim() && (env.WORKOS_API_KEY ?? "").trim());
}

export function workosClientId(env: WorkOSEnv): string {
	return (env.WORKOS_CLIENT_ID ?? "").trim();
}

export function workosApiKey(env: WorkOSEnv): string {
	return (env.WORKOS_API_KEY ?? "").trim();
}

export function workosApiBase(env: WorkOSEnv): string {
	const raw = (env.WORKOS_API_BASE ?? "").trim();
	return (raw || "https://api.workos.com").replace(/\/+$/, "");
}

export function workosJwksUrl(env: WorkOSEnv): string {
	const override = (env.WORKOS_JWKS_URL ?? "").trim();
	if (override) return override;
	return `${workosApiBase(env)}/sso/jwks/${encodeURIComponent(workosClientId(env))}`;
}

export function workosAuthProvider(env: WorkOSEnv): string {
	const raw = (env.WORKOS_AUTH_PROVIDER ?? "").trim();
	return raw || "authkit";
}

export function doNameForOrg(orgId: string, env: Pick<WorkOSEnv, "DEFAULT_ORG_ID">): string {
	const trimmed = orgId.trim();
	if (!trimmed) return "default";
	const defaultOrg = (env.DEFAULT_ORG_ID ?? "").trim();
	if (defaultOrg && trimmed === defaultOrg) return "default";
	return `org:${trimmed}`;
}

export function formatOrgToken(
	prefix: "hbw" | "hbj" | "hbu",
	orgId: string,
	secret = randomHex(24),
): string {
	const cleanOrg = orgId.trim();
	if (cleanOrg) {
		return `${prefix}_${cleanOrg}.${secret}`;
	}
	return `${prefix}_${secret}`;
}

export function parseOrgScopedToken(
	token: string,
): { kind: "worker" | "join" | "api"; orgId: string | null } | null {
	let kind: "worker" | "join" | "api";
	if (token.startsWith("hbw_")) kind = "worker";
	else if (token.startsWith("hbj_")) kind = "join";
	else if (token.startsWith("hbu_")) kind = "api";
	else return null;

	const rest = token.slice(4);
	const dot = rest.indexOf(".");
	if (dot < 0) {
		return { kind, orgId: null };
	}
	const orgId = rest.slice(0, dot);
	const secret = rest.slice(dot + 1);
	if (!orgId || !secret || !isValidOrgId(orgId)) {
		return null;
	}
	return { kind, orgId };
}

export function base64UrlEncode(bytes: Uint8Array): string {
	let bin = "";
	const chunk = 0x8000;
	for (let i = 0; i < bytes.length; i += chunk) {
		bin += String.fromCharCode(...bytes.subarray(i, i + chunk));
	}
	return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

export function base64UrlDecode(s: string): Uint8Array {
	const pad = "=".repeat((4 - (s.length % 4)) % 4);
	const b64 = (s + pad).replace(/-/g, "+").replace(/_/g, "/");
	const bin = atob(b64);
	const out = new Uint8Array(bin.length);
	for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
	return out;
}

export function randomBase64Url(bytesLen = 32): string {
	const buf = new Uint8Array(bytesLen);
	crypto.getRandomValues(buf);
	return base64UrlEncode(buf);
}

export async function pkceS256(verifier: string): Promise<string> {
	const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier));
	return base64UrlEncode(new Uint8Array(digest));
}

async function deriveSealKey(env: WorkOSEnv): Promise<CryptoKey> {
	const rawSecret = (env.WORKOS_COOKIE_PASSWORD ?? "").trim() || (env.WORKOS_API_KEY ?? "").trim();
	if (!rawSecret) {
		throw hotbedError(
			ErrorCode.NotAuthorized,
			"WorkOS cookie encryption key is not configured",
		).withSuggestion("configure WORKOS_COOKIE_PASSWORD or WORKOS_API_KEY");
	}
	const ikm = await crypto.subtle.importKey(
		"raw",
		new TextEncoder().encode(rawSecret),
		"HKDF",
		false,
		["deriveKey"],
	);
	return crypto.subtle.deriveKey(
		{
			name: "HKDF",
			hash: "SHA-256",
			salt: new TextEncoder().encode("hotbed-workos-seal-salt-v1"),
			info: new TextEncoder().encode("hotbed-workos-cookie-v1"),
		},
		ikm,
		{ name: "AES-GCM", length: 256 },
		false,
		["encrypt", "decrypt"],
	);
}

export async function sealPayload<T>(env: WorkOSEnv, payload: T): Promise<string> {
	const key = await deriveSealKey(env);
	const iv = new Uint8Array(12);
	crypto.getRandomValues(iv);
	const plaintext = new TextEncoder().encode(JSON.stringify(payload));
	const ciphertext = new Uint8Array(
		await crypto.subtle.encrypt({ name: "AES-GCM", iv }, key, plaintext),
	);
	const combined = new Uint8Array(iv.length + ciphertext.length);
	combined.set(iv, 0);
	combined.set(ciphertext, iv.length);
	return `v1.${base64UrlEncode(combined)}`;
}

export async function unsealPayload<T>(env: WorkOSEnv, sealed: string): Promise<T | null> {
	if (!sealed || !sealed.startsWith("v1.")) return null;
	try {
		const raw = base64UrlDecode(sealed.slice(3));
		if (raw.length <= 12) return null;
		const iv = raw.subarray(0, 12);
		const ciphertext = raw.subarray(12);
		const key = await deriveSealKey(env);
		const plaintext = await crypto.subtle.decrypt({ name: "AES-GCM", iv }, key, ciphertext);
		return JSON.parse(new TextDecoder().decode(plaintext)) as T;
	} catch {
		return null;
	}
}

export function makeWorkOSSessionCookie(sealed: string, secure: boolean): string {
	const attrs = [
		`${WORKOS_SESSION_COOKIE}=${sealed}`,
		"Path=/",
		"HttpOnly",
		"SameSite=Lax",
		"Max-Age=2592000",
	];
	if (secure) attrs.push("Secure");
	return attrs.join("; ");
}

export function clearWorkOSSessionCookie(secure: boolean): string {
	const attrs = [`${WORKOS_SESSION_COOKIE}=`, "Path=/", "HttpOnly", "SameSite=Lax", "Max-Age=0"];
	if (secure) attrs.push("Secure");
	return attrs.join("; ");
}

export function makeShortAuthCookie(name: string, sealed: string, secure: boolean): string {
	const attrs = [`${name}=${sealed}`, "Path=/auth", "HttpOnly", "SameSite=Lax", "Max-Age=600"];
	if (secure) attrs.push("Secure");
	return attrs.join("; ");
}

export function clearShortAuthCookie(name: string, secure: boolean): string {
	const attrs = [`${name}=`, "Path=/auth", "HttpOnly", "SameSite=Lax", "Max-Age=0"];
	if (secure) attrs.push("Secure");
	return attrs.join("; ");
}

export function isSafeReturnTo(raw: string | null): string {
	if (!raw) return "/";
	const trimmed = raw.trim();
	if (!trimmed.startsWith("/") || trimmed.startsWith("//") || trimmed.startsWith("/\\")) {
		return "/";
	}
	return trimmed;
}

/**
 * Strictly validates native CLI loopback callback URIs (RFC 8252):
 * only http://127.0.0.1:<port>/callback or http://[::1]:<port>/callback with unprivileged port.
 */
export function isAllowedCliRedirectUri(raw: string): boolean {
	try {
		const u = new URL(raw);
		if (u.protocol !== "http:") return false;
		if (u.hostname !== "127.0.0.1" && u.hostname !== "[::1]") return false;
		if (u.pathname !== "/callback") return false;
		if (u.username || u.password || u.search || u.hash) return false;
		const port = Number(u.port);
		return Number.isInteger(port) && port >= 1024 && port <= 65535;
	} catch {
		return false;
	}
}

// ---- WorkOS JWKS verification -----------------------------------------

interface CachedJWKS {
	url: string;
	fetchedAt: number;
	keys: Map<string, { key: CryptoKey; alg: string }>;
}

const JWKS_CACHE_TTL_MS = 5 * 60_000;
const JWKS_FORCE_REFRESH_COOLDOWN_MS = 10_000;
const JWT_CLOCK_SKEW_MS = 60_000;

let workosJwksCache: CachedJWKS | null = null;

export function resetWorkOSJwksCache(): void {
	workosJwksCache = null;
}

async function importJwkForVerify(
	jwk: JsonWebKey & { kid?: string; alg?: string; kty?: string; crv?: string },
): Promise<{ key: CryptoKey; alg: string } | null> {
	try {
		if (jwk.kty === "RSA") {
			const key = await crypto.subtle.importKey(
				"jwk",
				jwk,
				{ name: "RSASSA-PKCS1-v1_5", hash: "SHA-256" },
				false,
				["verify"],
			);
			return { key, alg: jwk.alg || "RS256" };
		}
		if (jwk.kty === "EC" && jwk.crv === "P-256") {
			const key = await crypto.subtle.importKey(
				"jwk",
				jwk,
				{ name: "ECDSA", namedCurve: "P-256" },
				false,
				["verify"],
			);
			return { key, alg: jwk.alg || "ES256" };
		}
	} catch {
		// Skip unsupported or malformed JWKs so one bad key cannot lock out logins.
	}
	return null;
}

async function fetchWorkOSJwks(
	env: WorkOSEnv,
	forceRefresh = false,
): Promise<Map<string, { key: CryptoKey; alg: string }>> {
	const jwksUrl = workosJwksUrl(env);
	const now = Date.now();
	const freshEnough =
		workosJwksCache &&
		workosJwksCache.url === jwksUrl &&
		now - workosJwksCache.fetchedAt <
			(forceRefresh ? JWKS_FORCE_REFRESH_COOLDOWN_MS : JWKS_CACHE_TTL_MS);
	if (freshEnough && workosJwksCache) {
		return workosJwksCache.keys;
	}
	const res = await fetch(jwksUrl);
	if (!res.ok) {
		throw new Error(`fetch WorkOS JWKS (${jwksUrl}): HTTP ${res.status}`);
	}
	const data = (await res.json()) as {
		keys?: Array<JsonWebKey & { kid?: string; alg?: string; kty?: string; crv?: string }>;
	};
	const keys = new Map<string, { key: CryptoKey; alg: string }>();
	for (const jwk of data.keys ?? []) {
		if (!jwk.kid) continue;
		const imported = await importJwkForVerify(jwk);
		if (imported) keys.set(jwk.kid, imported);
	}
	workosJwksCache = { url: jwksUrl, fetchedAt: now, keys };
	return keys;
}

export function decodeJwtClaimsUnverified(jwt: string): Record<string, unknown> | null {
	const parts = jwt.split(".");
	if (parts.length !== 3) return null;
	try {
		return JSON.parse(new TextDecoder().decode(base64UrlDecode(parts[1]))) as Record<
			string,
			unknown
		>;
	} catch {
		return null;
	}
}

export async function verifyWorkOSAccessToken(
	jwt: string,
	env: WorkOSEnv,
	nowMs = Date.now(),
): Promise<VerifiedWorkOSClaims | null> {
	const parts = jwt.split(".");
	if (parts.length !== 3) return null;
	try {
		const header = JSON.parse(new TextDecoder().decode(base64UrlDecode(parts[0]))) as {
			kid?: string;
			alg?: string;
		};
		const payload = JSON.parse(new TextDecoder().decode(base64UrlDecode(parts[1]))) as {
			sub?: string;
			org_id?: string;
			role?: string;
			sid?: string;
			email?: string;
			exp?: number;
			nbf?: number;
		};
		if (!header.kid || (header.alg !== "RS256" && header.alg !== "ES256")) return null;
		if (typeof payload.exp !== "number" || payload.exp * 1000 <= nowMs) return null;
		if (typeof payload.nbf === "number" && payload.nbf * 1000 > nowMs + JWT_CLOCK_SKEW_MS) {
			return null;
		}
		if (!payload.sub) return null;

		let keys = await fetchWorkOSJwks(env, false);
		let entry = keys.get(header.kid);
		if (!entry) {
			keys = await fetchWorkOSJwks(env, true);
			entry = keys.get(header.kid);
		}
		if (!entry) return null;

		const sig = base64UrlDecode(parts[2]);
		const signed = new TextEncoder().encode(`${parts[0]}.${parts[1]}`);
		const verifyAlgo =
			header.alg === "ES256"
				? ({ name: "ECDSA", hash: "SHA-256" } as const)
				: ({ name: "RSASSA-PKCS1-v1_5" } as const);
		const valid = await crypto.subtle.verify(verifyAlgo, entry.key, sig, signed);
		if (!valid) return null;

		return {
			sub: payload.sub,
			orgId: typeof payload.org_id === "string" ? payload.org_id : "",
			role: typeof payload.role === "string" ? payload.role : "",
			sid: typeof payload.sid === "string" ? payload.sid : "",
			email: typeof payload.email === "string" ? payload.email : "",
			exp: payload.exp * 1000,
		};
	} catch {
		return null;
	}
}

// ---- WorkOS REST API helpers ------------------------------------------

interface RawWorkOSUser {
	id: string;
	email: string;
	first_name?: string | null;
	last_name?: string | null;
	profile_picture_url?: string | null;
}

function mapUser(raw: RawWorkOSUser): WorkOSUser {
	return {
		id: raw.id,
		email: raw.email,
		firstName: raw.first_name ?? "",
		lastName: raw.last_name ?? "",
		profilePictureUrl: raw.profile_picture_url ?? undefined,
	};
}

export function formatDisplayName(
	user: Pick<WorkOSUser, "firstName" | "lastName" | "email">,
): string {
	const full = `${user.firstName} ${user.lastName}`.trim();
	return full || user.email;
}

export function buildAuthorizeUrl(
	env: WorkOSEnv,
	params: {
		redirectUri: string;
		state: string;
		codeChallenge: string;
		organizationId?: string;
		provider?: string;
		screenHint?: string;
	},
): string {
	const u = new URL(`${workosApiBase(env)}/user_management/authorize`);
	u.searchParams.set("client_id", workosClientId(env));
	u.searchParams.set("redirect_uri", params.redirectUri);
	u.searchParams.set("response_type", "code");
	u.searchParams.set("provider", params.provider || workosAuthProvider(env));
	u.searchParams.set("state", params.state);
	u.searchParams.set("code_challenge", params.codeChallenge);
	u.searchParams.set("code_challenge_method", "S256");
	if (params.organizationId) {
		u.searchParams.set("organization_id", params.organizationId);
	}
	if (params.screenHint) {
		u.searchParams.set("screen_hint", params.screenHint);
	}
	return u.toString();
}

export function buildLogoutUrl(env: WorkOSEnv, sessionId: string, returnTo?: string): string {
	const u = new URL(`${workosApiBase(env)}/user_management/sessions/logout`);
	u.searchParams.set("session_id", sessionId);
	if (returnTo) {
		u.searchParams.set("return_to", returnTo);
	}
	return u.toString();
}

function parseAuthSuccess(data: {
	user: RawWorkOSUser;
	organization_id?: string | null;
	access_token: string;
	refresh_token: string;
}): Extract<AuthenticateResult, { kind: "authenticated" }> {
	const claims = decodeJwtClaimsUnverified(data.access_token) ?? {};
	const expSec = typeof claims.exp === "number" ? claims.exp : Math.floor(Date.now() / 1000) + 300;
	const orgId =
		(typeof data.organization_id === "string" && data.organization_id) ||
		(typeof claims.org_id === "string" ? claims.org_id : "");
	const role = typeof claims.role === "string" ? claims.role : "";
	const sid = typeof claims.sid === "string" ? claims.sid : "";
	return {
		kind: "authenticated",
		user: mapUser(data.user),
		organizationId: orgId,
		accessToken: data.access_token,
		refreshToken: data.refresh_token,
		sessionId: sid,
		role,
		expiresAt: expSec * 1000,
	};
}

export async function authenticateWithCode(
	env: WorkOSEnv,
	params: {
		code: string;
		codeVerifier?: string;
		ipAddress?: string;
		userAgent?: string;
	},
): Promise<AuthenticateResult> {
	const body: Record<string, unknown> = {
		client_id: workosClientId(env),
		client_secret: workosApiKey(env),
		grant_type: "authorization_code",
		code: params.code,
	};
	if (params.codeVerifier) body.code_verifier = params.codeVerifier;
	if (params.ipAddress) body.ip_address = params.ipAddress;
	if (params.userAgent) body.user_agent = params.userAgent;

	const res = await fetch(`${workosApiBase(env)}/user_management/authenticate`, {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify(body),
	});
	const data = (await res.json().catch(() => ({}))) as Record<string, unknown>;
	if (
		!res.ok &&
		data.code === "organization_selection_required" &&
		typeof data.pending_authentication_token === "string"
	) {
		const orgs = Array.isArray(data.organizations)
			? (data.organizations as Array<{ id?: string; name?: string }>)
					.filter((o): o is { id: string; name: string } => typeof o.id === "string")
					.map((o) => ({ id: o.id, name: o.name || o.id }))
			: [];
		return {
			kind: "organization_selection_required",
			pendingAuthenticationToken: data.pending_authentication_token,
			organizations: orgs,
		};
	}
	if (!res.ok) {
		const msg =
			(typeof data.error_description === "string" && data.error_description) ||
			(typeof data.message === "string" && data.message) ||
			`WorkOS authentication failed (HTTP ${res.status})`;
		throw hotbedError(ErrorCode.NotAuthorized, msg);
	}
	return parseAuthSuccess(
		data as {
			user: RawWorkOSUser;
			organization_id?: string | null;
			access_token: string;
			refresh_token: string;
		},
	);
}

export async function authenticateWithOrgSelection(
	env: WorkOSEnv,
	params: {
		pendingAuthenticationToken: string;
		organizationId: string;
		ipAddress?: string;
		userAgent?: string;
	},
): Promise<Extract<AuthenticateResult, { kind: "authenticated" }>> {
	const body: Record<string, unknown> = {
		client_id: workosClientId(env),
		client_secret: workosApiKey(env),
		grant_type: "organization_selection",
		pending_authentication_token: params.pendingAuthenticationToken,
		organization_id: params.organizationId,
	};
	if (params.ipAddress) body.ip_address = params.ipAddress;
	if (params.userAgent) body.user_agent = params.userAgent;

	const res = await fetch(`${workosApiBase(env)}/user_management/authenticate`, {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify(body),
	});
	const data = (await res.json().catch(() => ({}))) as Record<string, unknown>;
	if (!res.ok) {
		const msg =
			(typeof data.error_description === "string" && data.error_description) ||
			(typeof data.message === "string" && data.message) ||
			`WorkOS organization selection failed (HTTP ${res.status})`;
		throw hotbedError(ErrorCode.NotAuthorized, msg);
	}
	return parseAuthSuccess(
		data as {
			user: RawWorkOSUser;
			organization_id?: string | null;
			access_token: string;
			refresh_token: string;
		},
	);
}

const inFlightRefreshes = new Map<
	string,
	Promise<Extract<AuthenticateResult, { kind: "authenticated" }>>
>();

async function refreshDedupKey(refreshToken: string, organizationId: string): Promise<string> {
	const digest = await crypto.subtle.digest(
		"SHA-256",
		new TextEncoder().encode(`${refreshToken}\0${organizationId}`),
	);
	return bytesToHex(new Uint8Array(digest));
}

export async function authenticateWithRefreshToken(
	env: WorkOSEnv,
	params: {
		refreshToken: string;
		organizationId?: string;
		ipAddress?: string;
		userAgent?: string;
	},
): Promise<Extract<AuthenticateResult, { kind: "authenticated" }>> {
	const key = await refreshDedupKey(params.refreshToken, params.organizationId ?? "");
	const existing = inFlightRefreshes.get(key);
	if (existing) return existing;

	const promise = (async () => {
		const body: Record<string, unknown> = {
			client_id: workosClientId(env),
			client_secret: workosApiKey(env),
			grant_type: "refresh_token",
			refresh_token: params.refreshToken,
		};
		if (params.organizationId) body.organization_id = params.organizationId;
		if (params.ipAddress) body.ip_address = params.ipAddress;
		if (params.userAgent) body.user_agent = params.userAgent;

		const res = await fetch(`${workosApiBase(env)}/user_management/authenticate`, {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify(body),
		});
		const data = (await res.json().catch(() => ({}))) as Record<string, unknown>;
		if (!res.ok) {
			const msg =
				(typeof data.error_description === "string" && data.error_description) ||
				(typeof data.message === "string" && data.message) ||
				`WorkOS token refresh failed (HTTP ${res.status})`;
			throw hotbedError(ErrorCode.NotAuthorized, msg);
		}
		return parseAuthSuccess(
			data as {
				user: RawWorkOSUser;
				organization_id?: string | null;
				access_token: string;
				refresh_token: string;
			},
		);
	})();

	inFlightRefreshes.set(key, promise);
	try {
		return await promise;
	} finally {
		inFlightRefreshes.delete(key);
	}
}

export interface DeviceAuthStart {
	deviceCode: string;
	userCode: string;
	verificationUri: string;
	verificationUriComplete: string;
	expiresIn: number;
	interval: number;
}

export async function startDeviceAuthorization(env: WorkOSEnv): Promise<DeviceAuthStart> {
	const res = await fetch(`${workosApiBase(env)}/user_management/authorize/device`, {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ client_id: workosClientId(env) }),
	});
	const data = (await res.json().catch(() => ({}))) as Record<string, unknown>;
	if (!res.ok) {
		const msg =
			(typeof data.error_description === "string" && data.error_description) ||
			(typeof data.message === "string" && data.message) ||
			`WorkOS device authorization failed (HTTP ${res.status})`;
		throw hotbedError(ErrorCode.Internal, msg);
	}
	const strField = (v: unknown): string => (typeof v === "string" ? v : "");
	return {
		deviceCode: strField(data.device_code),
		userCode: strField(data.user_code),
		verificationUri: strField(data.verification_uri),
		verificationUriComplete:
			strField(data.verification_uri_complete) || strField(data.verification_uri),
		expiresIn: Number(data.expires_in ?? 300),
		interval: Number(data.interval ?? 5),
	};
}

export type DevicePollResult =
	| { status: "pending" }
	| { status: "slow_down" }
	| {
			status: "complete";
			auth: Extract<AuthenticateResult, { kind: "authenticated" }>;
	  };

export async function pollDeviceToken(
	env: WorkOSEnv,
	deviceCode: string,
): Promise<DevicePollResult> {
	const res = await fetch(`${workosApiBase(env)}/user_management/authenticate`, {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({
			client_id: workosClientId(env),
			client_secret: workosApiKey(env),
			grant_type: "urn:ietf:params:oauth:grant-type:device_code",
			device_code: deviceCode,
		}),
	});
	const data = (await res.json().catch(() => ({}))) as Record<string, unknown>;
	if (!res.ok) {
		const errCode =
			typeof data.error === "string" ? data.error : typeof data.code === "string" ? data.code : "";
		if (errCode === "authorization_pending") {
			return { status: "pending" };
		}
		if (errCode === "slow_down") {
			return { status: "slow_down" };
		}
		const msg =
			(typeof data.error_description === "string" && data.error_description) ||
			(typeof data.message === "string" && data.message) ||
			errCode ||
			`WorkOS device authentication failed (HTTP ${res.status})`;
		throw hotbedError(ErrorCode.NotAuthorized, msg);
	}
	return {
		status: "complete",
		auth: parseAuthSuccess(
			data as {
				user: RawWorkOSUser;
				organization_id?: string | null;
				access_token: string;
				refresh_token: string;
			},
		),
	};
}

async function workosAdminFetch<T>(env: WorkOSEnv, path: string, init?: RequestInit): Promise<T> {
	const headers = new Headers(init?.headers);
	headers.set("Authorization", `Bearer ${workosApiKey(env)}`);
	if (init?.body && !headers.has("Content-Type")) {
		headers.set("Content-Type", "application/json");
	}
	const res = await fetch(`${workosApiBase(env)}${path}`, {
		...init,
		headers,
	});
	if (res.status === 204) {
		return undefined as T;
	}
	const data = (await res.json().catch(() => ({}))) as Record<string, unknown>;
	if (!res.ok) {
		const msg =
			(typeof data.message === "string" && data.message) ||
			(typeof data.error_description === "string" && data.error_description) ||
			`WorkOS API ${init?.method ?? "GET"} ${path} failed (HTTP ${res.status})`;
		const code =
			res.status === 404
				? ErrorCode.NotFound
				: res.status === 409
					? ErrorCode.Conflict
					: res.status === 400 || res.status === 422
						? ErrorCode.InvalidArgs
						: res.status === 401 || res.status === 403
							? ErrorCode.NotAuthorized
							: ErrorCode.Internal;
		throw hotbedError(code, msg);
	}
	return data as T;
}

export async function getUser(env: WorkOSEnv, userId: string): Promise<WorkOSUser> {
	const raw = await workosAdminFetch<RawWorkOSUser>(
		env,
		`/user_management/users/${encodeURIComponent(userId)}`,
	);
	return mapUser(raw);
}

export async function getOrganization(
	env: WorkOSEnv,
	orgId: string,
): Promise<{ id: string; name: string }> {
	const raw = await workosAdminFetch<{ id: string; name: string }>(
		env,
		`/organizations/${encodeURIComponent(orgId)}`,
	);
	return { id: raw.id, name: raw.name };
}

interface RawMembership {
	id: string;
	user_id: string;
	organization_id: string;
	organization_name?: string;
	status: string;
	role?: { slug?: string } | null;
	created_at?: string;
}

export async function listUserOrganizations(
	env: WorkOSEnv,
	userId: string,
): Promise<WorkOSOrganizationSummary[]> {
	const list = await workosAdminFetch<{ data: RawMembership[] }>(
		env,
		`/user_management/organization_memberships?user_id=${encodeURIComponent(userId)}&statuses=active&limit=100`,
	);
	const memberships = list.data ?? [];
	const results = await Promise.all(
		memberships.map(async (m) => {
			let name = m.organization_name ?? "";
			if (!name) {
				try {
					const org = await getOrganization(env, m.organization_id);
					name = org.name;
				} catch {
					name = m.organization_id;
				}
			}
			return {
				id: m.organization_id,
				name,
				role: m.role?.slug || "member",
				membershipId: m.id,
			};
		}),
	);
	return results.sort((a, b) => a.name.localeCompare(b.name));
}

export async function createOrganization(
	env: WorkOSEnv,
	name: string,
): Promise<{ id: string; name: string }> {
	const trimmed = name.trim();
	if (!trimmed || trimmed.length > 100) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			"organization name must be between 1 and 100 characters",
		);
	}
	const raw = await workosAdminFetch<{ id: string; name: string }>(env, "/organizations", {
		method: "POST",
		body: JSON.stringify({ name: trimmed }),
	});
	return { id: raw.id, name: raw.name };
}

export async function createOrganizationMembership(
	env: WorkOSEnv,
	organizationId: string,
	userId: string,
	roleSlug?: string,
): Promise<{ id: string; role: string }> {
	const attempt = async (slug?: string) => {
		const payload: Record<string, unknown> = {
			organization_id: organizationId,
			user_id: userId,
		};
		if (slug) payload.role_slug = slug;
		return workosAdminFetch<RawMembership>(env, "/user_management/organization_memberships", {
			method: "POST",
			body: JSON.stringify(payload),
		});
	};
	try {
		const m = await attempt(roleSlug);
		return { id: m.id, role: m.role?.slug || roleSlug || "member" };
	} catch (err) {
		if (roleSlug) {
			const m = await attempt(undefined);
			return { id: m.id, role: m.role?.slug || "member" };
		}
		throw err;
	}
}

export async function listOrganizationMembers(
	env: WorkOSEnv,
	organizationId: string,
): Promise<WorkOSMember[]> {
	const [memRes, usersRes] = await Promise.all([
		workosAdminFetch<{ data: RawMembership[] }>(
			env,
			`/user_management/organization_memberships?organization_id=${encodeURIComponent(organizationId)}&limit=100`,
		),
		workosAdminFetch<{ data: RawWorkOSUser[] }>(
			env,
			`/user_management/users?organization_id=${encodeURIComponent(organizationId)}&limit=100`,
		),
	]);
	const usersById = new Map<string, WorkOSUser>();
	for (const u of usersRes.data ?? []) {
		usersById.set(u.id, mapUser(u));
	}
	const out: WorkOSMember[] = [];
	for (const m of memRes.data ?? []) {
		let u = usersById.get(m.user_id);
		if (!u) {
			try {
				u = await getUser(env, m.user_id);
			} catch {
				u = { id: m.user_id, email: m.user_id, firstName: "", lastName: "" };
			}
		}
		out.push({
			membershipId: m.id,
			userId: m.user_id,
			email: u.email,
			firstName: u.firstName,
			lastName: u.lastName,
			name: formatDisplayName(u),
			role: m.role?.slug || "member",
			status: m.status,
			createdAt: m.created_at ?? "",
		});
	}
	return out.sort((a, b) => a.email.localeCompare(b.email));
}

export async function getOrganizationMembership(
	env: WorkOSEnv,
	membershipId: string,
): Promise<{ id: string; userId: string; organizationId: string; role: string; status: string }> {
	const m = await workosAdminFetch<RawMembership>(
		env,
		`/user_management/organization_memberships/${encodeURIComponent(membershipId)}`,
	);
	return {
		id: m.id,
		userId: m.user_id,
		organizationId: m.organization_id,
		role: m.role?.slug || "member",
		status: m.status,
	};
}

export async function removeOrganizationMembership(
	env: WorkOSEnv,
	membershipId: string,
): Promise<void> {
	await workosAdminFetch<void>(
		env,
		`/user_management/organization_memberships/${encodeURIComponent(membershipId)}`,
		{ method: "DELETE" },
	);
}

interface RawInvitation {
	id: string;
	email: string;
	state: string;
	organization_id?: string | null;
	role_slug?: string | null;
	expires_at?: string;
	created_at?: string;
	accept_invitation_url?: string;
}

function mapInvitation(raw: RawInvitation): WorkOSInvitation & { organizationId: string } {
	return {
		id: raw.id,
		email: raw.email,
		state: raw.state,
		role: raw.role_slug || "member",
		expiresAt: raw.expires_at ?? "",
		createdAt: raw.created_at ?? "",
		acceptInvitationUrl: raw.accept_invitation_url,
		organizationId: raw.organization_id ?? "",
	};
}

export async function listOrganizationInvitations(
	env: WorkOSEnv,
	organizationId: string,
): Promise<WorkOSInvitation[]> {
	const list = await workosAdminFetch<{ data: RawInvitation[] }>(
		env,
		`/user_management/invitations?organization_id=${encodeURIComponent(organizationId)}&limit=100`,
	);
	return (list.data ?? [])
		.filter((i) => i.state === "pending")
		.map((i) => {
			const { organizationId: _org, ...rest } = mapInvitation(i);
			return rest;
		});
}

export async function getInvitation(
	env: WorkOSEnv,
	invitationId: string,
): Promise<WorkOSInvitation & { organizationId: string }> {
	const raw = await workosAdminFetch<RawInvitation>(
		env,
		`/user_management/invitations/${encodeURIComponent(invitationId)}`,
	);
	return mapInvitation(raw);
}

export async function sendOrganizationInvitation(
	env: WorkOSEnv,
	params: {
		email: string;
		organizationId: string;
		roleSlug?: string;
		inviterUserId?: string;
	},
): Promise<WorkOSInvitation> {
	const email = params.email.trim().toLowerCase();
	if (!email || !email.includes("@")) {
		throw hotbedError(ErrorCode.InvalidArgs, "valid email address is required");
	}
	const body: Record<string, unknown> = {
		email,
		organization_id: params.organizationId,
	};
	if (params.roleSlug) body.role_slug = params.roleSlug;
	if (params.inviterUserId) body.inviter_user_id = params.inviterUserId;

	const raw = await workosAdminFetch<RawInvitation>(env, "/user_management/invitations", {
		method: "POST",
		body: JSON.stringify(body),
	});
	const { organizationId: _org, ...inv } = mapInvitation(raw);
	return inv;
}

export async function revokeOrganizationInvitation(
	env: WorkOSEnv,
	invitationId: string,
): Promise<void> {
	await workosAdminFetch<unknown>(
		env,
		`/user_management/invitations/${encodeURIComponent(invitationId)}/revoke`,
		{ method: "POST" },
	);
}

// ---- HTML Pages for Org Selection & Onboarding ------------------------

function escapeHtml(s: string): string {
	return s
		.replace(/&/g, "&amp;")
		.replace(/</g, "&lt;")
		.replace(/>/g, "&gt;")
		.replace(/"/g, "&quot;");
}

export function renderOrgPickerPage(opts: {
	email: string;
	organizations: Array<{ id: string; name: string; role?: string }>;
	pendingSelection?: boolean;
	error?: string;
	returnTo?: string;
}): string {
	const errHtml = opts.error ? `<p class="err">${escapeHtml(opts.error)}</p>` : "";
	const actionPath = opts.pendingSelection ? "/auth/org/select-pending" : "/auth/org/switch";
	const orgButtons = opts.organizations
		.map(
			(o) => `<form method="POST" action="${actionPath}" class="org-row">
  <input type="hidden" name="organization_id" value="${escapeHtml(o.id)}" />
  <input type="hidden" name="return_to" value="${escapeHtml(opts.returnTo || "/")}" />
  <button type="submit" class="org-btn">
    <span class="org-name">${escapeHtml(o.name)}</span>
    <span class="org-meta">${escapeHtml(o.role || "member")} · ${escapeHtml(o.id)}</span>
  </button>
</form>`,
		)
		.join("\n");

	const createSection = opts.pendingSelection
		? ""
		: `<div class="divider"><span>or create a new organization</span></div>
<form method="POST" action="/auth/org/create" class="create-form">
  <input type="hidden" name="return_to" value="${escapeHtml(opts.returnTo || "/")}" />
  <input type="text" name="name" placeholder="Organization name (e.g. Acme)" maxlength="100" required />
  <button type="submit" class="primary">Create organization</button>
</form>`;

	return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width,initial-scale=1" />
<title>hotbed — select organization</title>
<style>
  body{margin:0;min-height:100vh;display:grid;place-items:center;background:#0b0d12;color:#e5e7eb;font-family:system-ui,-apple-system,sans-serif;padding:24px;box-sizing:border-box}
  .card{width:100%;max-width:440px;padding:28px;border:1px solid #1f2430;border-radius:14px;background:#121620;box-sizing:border-box}
  h1{margin:0 0 6px;font-size:19px;font-weight:600}
  .sub{margin:0 0 20px;color:#9ca3af;font-size:13px}
  .err{color:#f87171;margin:0 0 14px;font-size:13px;padding:10px 12px;border:1px solid #7f1d1d;background:#1f1115;border-radius:8px}
  .org-row{margin-bottom:8px}
  .org-btn{width:100%;text-align:left;padding:12px 14px;border-radius:10px;border:1px solid #262e40;background:#0d1017;color:#f3f4f6;cursor:pointer;display:flex;flex-direction:column;gap:3px;transition:border-color .15s,background .15s}
  .org-btn:hover{border-color:#3b82f6;background:#151b28}
  .org-name{font-size:14px;font-weight:600}
  .org-meta{font-size:11.5px;color:#9ca3af;font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
  .divider{display:flex;align-items:center;gap:10px;margin:20px 0 14px;color:#6b7280;font-size:12px}
  .divider::before,.divider::after{content:"";flex:1;height:1px;background:#1f2430}
  input[type=text]{width:100%;padding:10px 12px;border-radius:8px;border:1px solid #2b3242;background:#0b0d12;color:#f9fafb;font-size:14px;box-sizing:border-box;margin-bottom:10px}
  button.primary{width:100%;padding:10px 12px;border-radius:8px;border:0;background:#3b82f6;color:#fff;font-weight:600;font-size:14px;cursor:pointer}
  button.primary:hover{background:#2563eb}
  .footer{margin-top:18px;display:flex;justify-content:space-between;align-items:center;font-size:12px;color:#6b7280}
  .logout{background:none;border:0;color:#9ca3af;cursor:pointer;font-size:12px;padding:0;text-decoration:underline}
  .logout:hover{color:#e5e7eb}
</style>
</head>
<body>
<div class="card">
  <h1>Choose an organization</h1>
  <p class="sub">${opts.email ? `Signed in as ${escapeHtml(opts.email)}. ` : ""}Each organization runs its own isolated hotbed cluster.</p>
  ${errHtml}
  ${orgButtons || `<p class="sub">You don't belong to an organization yet. Create one below to start your cluster.</p>`}
  ${createSection}
  <div class="footer">
    <span>hotbed cockpit</span>
    <form method="POST" action="/auth/logout"><button type="submit" class="logout">Sign out</button></form>
  </div>
</div>
</body>
</html>`;
}
