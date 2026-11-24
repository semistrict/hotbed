import { bytesToHex } from "./gitops.ts";
import { ErrorCode, hotbedError } from "./errors.ts";

export interface CallerIdentity {
	role: "owner" | "worker";
	nodeId: string;
	hostname: string;
	login: string;
	addr: string;
	authMethod: "bearer" | "worker-token" | "cookie" | "cf-access";
}

export interface AuthEnv {
	OWNER_TOKEN?: string;
	JOIN_TOKEN?: string;
	CLUSTER_OWNER?: string;
	CF_ACCESS_AUD?: string;
	CF_ACCESS_TEAM_DOMAIN?: string;
}

export interface WorkerTokenLookup {
	findWorkerToken(tokenHash: string): { nodeId: string; hostname: string } | null;
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
	const attrs = [
		`hotbed_session=${val}`,
		"Path=/",
		"HttpOnly",
		"SameSite=Strict",
		"Max-Age=2592000",
	];
	if (secure) attrs.push("Secure");
	return attrs.join("; ");
}

export function clearSessionCookie(secure: boolean): string {
	const attrs = ["hotbed_session=", "Path=/", "HttpOnly", "SameSite=Strict", "Max-Age=0"];
	if (secure) attrs.push("Secure");
	return attrs.join("; ");
}

function getCookie(req: Request, name: string): string | null {
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

interface JWKSCache {
	domain: string;
	fetchedAt: number;
	keys: Map<string, CryptoKey>;
}

let jwksCache: JWKSCache | null = null;

function base64UrlDecode(s: string): Uint8Array {
	const pad = "=".repeat((4 - (s.length % 4)) % 4);
	const b64 = (s + pad).replace(/-/g, "+").replace(/_/g, "/");
	const bin = atob(b64);
	const out = new Uint8Array(bin.length);
	for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
	return out;
}

async function getAccessKeys(teamDomain: string): Promise<Map<string, CryptoKey>> {
	const now = Date.now();
	if (jwksCache && jwksCache.domain === teamDomain && now - jwksCache.fetchedAt < 5 * 60_000) {
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
		if (!jwk.kid) continue;
		const key = await crypto.subtle.importKey(
			"jwk",
			jwk,
			{ name: "RSASSA-PKCS1-v1_5", hash: "SHA-256" },
			false,
			["verify"],
		);
		keys.set(jwk.kid, key);
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
			exp?: number;
			email?: string;
			sub?: string;
		};
		if (header.alg !== "RS256" || !header.kid) return null;
		const auds = Array.isArray(payload.aud) ? payload.aud : payload.aud ? [payload.aud] : [];
		if (!auds.includes(aud)) return null;
		if (typeof payload.exp !== "number" || payload.exp * 1000 <= Date.now()) return null;
		const keys = await getAccessKeys(teamDomain);
		const key = keys.get(header.kid);
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

function checkSameOrigin(req: Request): string | null {
	const site = (req.headers.get("Sec-Fetch-Site") ?? "").trim().toLowerCase();
	if (site && site !== "same-origin" && site !== "none") {
		return `cross-site browser request (${site}) is not allowed`;
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
 *  1. Authorization: Bearer <token> (or ?token= for WebSocket upgrade)
 *     - matches OWNER_TOKEN -> owner (or worker if X-Hotbed-Node-Id is supplied on worker routes)
 *     - matches JOIN_TOKEN on Join / /ws/worker -> worker
 *     - matches a per-worker token hash in worker_tokens -> worker
 *  2. Cloudflare Access JWT (Cf-Access-Jwt-Assertion) when CF_ACCESS_AUD + CF_ACCESS_TEAM_DOMAIN are set
 *  3. Cookie session (hotbed_session) + same-origin CSRF check
 */
export async function authenticateRequest(
	req: Request,
	env: AuthEnv,
	tokens: WorkerTokenLookup,
	opts?: { allowJoinToken?: boolean },
): Promise<CallerIdentity> {
	const ownerToken = (env.OWNER_TOKEN ?? "").trim();
	const joinToken = (env.JOIN_TOKEN ?? "").trim() || ownerToken;
	const ownerLogin = (env.CLUSTER_OWNER ?? "").trim() || "owner";
	const accessAud = (env.CF_ACCESS_AUD ?? "").trim();
	const accessDomain = (env.CF_ACCESS_TEAM_DOMAIN ?? "").trim();

	if (!ownerToken && !(accessAud && accessDomain)) {
		throw hotbedError(
			ErrorCode.NotAuthorized,
			"this controller has no OWNER_TOKEN or Cloudflare Access configured: refusing all requests",
		).withSuggestion(
			"configure OWNER_TOKEN secret via `npx wrangler secret put OWNER_TOKEN --name hotbed-controller` or `.dev.vars`",
		);
	}

	const headerHost = (req.headers.get("X-Hotbed-Host") ?? "").trim();
	const headerNodeId = (req.headers.get("X-Hotbed-Node-Id") ?? "").trim();
	const headerAddr = (req.headers.get("X-Hotbed-Addr") ?? "").trim();

	// 1. Bearer token
	let bearer = "";
	const authHeader = req.headers.get("Authorization") ?? "";
	if (authHeader.toLowerCase().startsWith("bearer ")) {
		bearer = authHeader.slice(7).trim();
	}
	if (bearer) {
		if (ownerToken && (await timingSafeStringEqual(bearer, ownerToken))) {
			return {
				role: "owner",
				nodeId: headerNodeId || `owner:${headerHost || "client"}`,
				hostname: headerHost || "client",
				login: ownerLogin,
				addr: headerAddr,
				authMethod: "bearer",
			};
		}
		const hash = await sha256Hex(bearer);
		const wt = tokens.findWorkerToken(hash);
		if (wt) {
			return {
				role: "worker",
				nodeId: wt.nodeId,
				hostname: headerHost || wt.hostname,
				login: ownerLogin,
				addr: headerAddr,
				authMethod: "worker-token",
			};
		}
		if (opts?.allowJoinToken && joinToken && (await timingSafeStringEqual(bearer, joinToken))) {
			return {
				role: "worker",
				nodeId: headerNodeId || `worker:${headerHost || "unknown"}`,
				hostname: headerHost || headerNodeId || "worker",
				login: ownerLogin,
				addr: headerAddr,
				authMethod: "worker-token",
			};
		}
		throw hotbedError(ErrorCode.NotAuthorized, "invalid bearer token");
	}

	// 2. Cloudflare Access JWT
	if (accessAud && accessDomain) {
		const jwt = req.headers.get("Cf-Access-Jwt-Assertion") ?? getCookie(req, "CF_Authorization");
		if (jwt) {
			const verified = await verifyCloudflareAccessJWT(jwt, accessAud, accessDomain);
			if (verified) {
				const csrfErr = checkSameOrigin(req);
				if (csrfErr) {
					throw hotbedError(ErrorCode.NotAuthorized, csrfErr);
				}
				return {
					role: "owner",
					nodeId: `access:${verified.email}`,
					hostname: headerHost || verified.email,
					login: verified.email,
					addr: "",
					authMethod: "cf-access",
				};
			}
		}
	}

	// 3. Session cookie (for Cockpit UI)
	if (ownerToken) {
		const cookie = getCookie(req, "hotbed_session");
		if (cookie) {
			const expected = await hmacCookieValue(ownerToken);
			if (await timingSafeStringEqual(cookie, expected)) {
				const csrfErr = checkSameOrigin(req);
				if (csrfErr) {
					throw hotbedError(ErrorCode.NotAuthorized, csrfErr);
				}
				return {
					role: "owner",
					nodeId: "cockpit",
					hostname: headerHost || "cockpit",
					login: ownerLogin,
					addr: "",
					authMethod: "cookie",
				};
			}
		}
	}

	throw hotbedError(ErrorCode.NotAuthorized, "authentication required").withSuggestion(
		"set Authorization: Bearer <OWNER_TOKEN> (or HOTBED_TOKEN in CLI) or sign in at /auth/login",
	);
}

export function renderLoginPage(error?: string): string {
	const errHtml = error
		? `<p style="color:#f87171;margin:0 0 12px;font-size:14px">${escapeHtml(error)}</p>`
		: "";
	return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width,initial-scale=1" />
<title>hotbed cockpit — sign in</title>
<style>
  body{margin:0;min-height:100vh;display:grid;place-items:center;background:#0b0d12;color:#e5e7eb;font-family:system-ui,-apple-system,sans-serif}
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
  <input type="password" name="token" placeholder="Owner token" autocomplete="current-password" autofocus required />
  <button type="submit">Sign in</button>
</form>
</body>
</html>`;
}

function escapeHtml(s: string): string {
	return s
		.replace(/&/g, "&amp;")
		.replace(/</g, "&lt;")
		.replace(/>/g, "&gt;")
		.replace(/"/g, "&quot;");
}
