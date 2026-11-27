import type { ServiceImpl } from "@connectrpc/connect";
import { create } from "@bufbuild/protobuf";
import {
	DeleteGitCredentialResponseSchema,
	GitAuthService,
	GitCredentialSchema,
	GitHubLoginChallengeSchema,
	GitTokenSchema,
	ListGitCredentialsResponseSchema,
	ProxyGitHubAPIResponseSchema,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import { isOrgAdminCaller } from "../auth.ts";
import { getCaller } from "../connect-adapter.ts";
import { REPO_ID_RE, type ControllerContext } from "../context.ts";
import { ErrorCode, hotbedError } from "../errors.ts";
import { parseGitRepo, randomHex } from "../gitops.ts";
import type { StoredGitCredential } from "../store/index.ts";

const DEFAULT_GITHUB_CLIENT_ID = "Iv23li5qE6N9l4g7P3hW";
const GIT_TOKEN_LEASE_TTL_MS = 15 * 60_000;
const OAUTH_REFRESH_LEAD_MS = 5 * 60_000;
const GIT_HTTPS_USERNAME = "x-access-token";
const MAX_PROXY_RESPONSE_BYTES = 16 * 1024 * 1024;
const PENDING_LOGINS_META_KEY = "pending_github_logins";

async function readJsonObject<T extends Record<string, unknown>>(
	res: Response,
	label: string,
	errorCode: (typeof ErrorCode)[keyof typeof ErrorCode] = ErrorCode.Internal,
): Promise<T> {
	const text = await res.text();
	try {
		const parsed = JSON.parse(text) as unknown;
		if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
			return parsed as T;
		}
	} catch {
		// fall through to structured error
	}
	const snippet = text.trim().slice(0, 120);
	throw hotbedError(
		errorCode,
		`${label}: unexpected non-JSON response (HTTP ${res.status})${snippet ? `: ${snippet}` : ""}`,
	);
}

async function readBodyCapped(resp: Response, maxBytes: number): Promise<Uint8Array> {
	if (!resp.body) return new Uint8Array(0);
	const reader = resp.body.getReader();
	const chunks: Uint8Array[] = [];
	let total = 0;
	try {
		while (total < maxBytes) {
			const { done, value } = await reader.read();
			if (done) break;
			if (!value || value.byteLength === 0) continue;
			const remaining = maxBytes - total;
			if (value.byteLength >= remaining) {
				chunks.push(value.subarray(0, remaining));
				total += remaining;
				await reader.cancel();
				break;
			}
			chunks.push(value);
			total += value.byteLength;
		}
	} finally {
		reader.releaseLock();
	}
	const out = new Uint8Array(total);
	let offset = 0;
	for (const c of chunks) {
		out.set(c, offset);
		offset += c.byteLength;
	}
	return out;
}

interface PendingGitHubLogin {
	name: string;
	host: string;
	repos: string[];
	clientId: string;
	deviceCode: string;
	intervalMs: number;
	expiresAt: number;
	apiUrl: string;
	oauthUrl: string;
}

function validateGitHost(raw: string): string {
	const host = raw.trim().toLowerCase();
	if (!host) return "github.com";
	if (/[\s/]/.test(host) || host.includes("://")) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`git host "${raw}" must be a hostname (or host:port), not a URL`,
		);
	}
	return host;
}

function validateOptionalURL(raw: string, field: string): string {
	const s = raw.trim().replace(/\/+$/, "");
	if (!s) return "";
	let u: URL;
	try {
		u = new URL(s);
	} catch {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`${field} "${raw}" must be an http:// or https:// URL`,
		);
	}
	if ((u.protocol !== "https:" && u.protocol !== "http:") || !u.host) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`${field} "${raw}" must be an http:// or https:// URL`,
		);
	}
	return s;
}

function normalizeRepoPatterns(patterns: string[]): string[] {
	const out: string[] = [];
	for (const raw of patterns) {
		let p = raw.trim().replace(/^\/+|\/+$/g, "");
		if (p.endsWith(".git")) p = p.slice(0, -4);
		const slash = p.indexOf("/");
		const owner = slash >= 0 ? p.slice(0, slash) : "";
		const repo = slash >= 0 ? p.slice(slash + 1) : "";
		if (
			!owner ||
			!repo ||
			repo.includes("/") ||
			/\s/.test(p) ||
			owner === "*" ||
			owner === "." ||
			owner === ".." ||
			repo === "." ||
			repo === ".."
		) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`repo pattern "${raw}" must be "owner/repo" or "owner/*"`,
			);
		}
		if (!out.some((e) => e.toLowerCase() === p.toLowerCase())) {
			out.push(p);
		}
	}
	return out;
}

function githubAPIBase(host: string, override: string): string {
	if (override) return override.replace(/\/+$/, "");
	return host === "github.com" ? "https://api.github.com" : `https://${host}/api/v3`;
}

function githubOAuthBase(host: string, override: string): string {
	if (override) return override.replace(/\/+$/, "");
	return `https://${host}`;
}

function extractRepoHint(repoHint: string, apiPath: string): { owner: string; repo: string } {
	let s = repoHint.trim().replace(/^\/+|\/+$/g, "");
	if (s) {
		if (s.endsWith(".git")) s = s.slice(0, -4);
		const slash = s.indexOf("/");
		if (slash > 0 && slash < s.length - 1) {
			return { owner: s.slice(0, slash), repo: s.slice(slash + 1) };
		}
	}
	const q = apiPath.indexOf("?");
	const p = (q >= 0 ? apiPath.slice(0, q) : apiPath).replace(/^\/+/, "");
	const parts = p.split("/");
	if (parts.length >= 3 && parts[0] === "repos" && parts[1] && parts[2]) {
		return { owner: parts[1], repo: parts[2] };
	}
	return { owner: "", repo: "" };
}

function matchScore(patterns: string[], owner: string, repo: string): number {
	if (patterns.length === 0) return 1;
	const full = `${owner}/${repo}`.toLowerCase();
	const wild = `${owner}/*`.toLowerCase();
	let best = 0;
	for (const p of patterns) {
		const low = p.toLowerCase();
		if (low === full) return 3;
		if (low === wild && best < 2) best = 2;
	}
	return best;
}

function selectGitCredential(
	candidates: StoredGitCredential[],
	owner: string,
	repo: string,
): StoredGitCredential | null {
	let best: StoredGitCredential | null = null;
	let bestScore = 0;
	for (const c of candidates) {
		const score = matchScore(c.cred.repos, owner, repo);
		if (score === 0) continue;
		if (score > bestScore || (score === bestScore && best && c.cred.name < best.cred.name)) {
			best = c;
			bestScore = score;
		}
	}
	return best;
}

export function createGitAuthService(ctrl: ControllerContext): ServiceImpl<typeof GitAuthService> {
	function loadPendingLogins(nowMs: number): Record<string, PendingGitHubLogin> {
		const raw = ctrl.store.meta.get(PENDING_LOGINS_META_KEY);
		if (!raw) return {};
		try {
			const parsed = JSON.parse(raw) as Record<string, PendingGitHubLogin>;
			const out: Record<string, PendingGitHubLogin> = {};
			for (const [id, p] of Object.entries(parsed)) {
				if (p && typeof p.expiresAt === "number" && nowMs <= p.expiresAt) {
					out[id] = p;
				}
			}
			return out;
		} catch {
			return {};
		}
	}

	function savePendingLogins(map: Record<string, PendingGitHubLogin>): void {
		ctrl.store.meta.set(PENDING_LOGINS_META_KEY, JSON.stringify(map));
	}

	function deletePendingLogin(loginId: string, nowMs: number): void {
		const map = loadPendingLogins(nowMs);
		if (loginId in map) {
			delete map[loginId];
			savePendingLogins(map);
		}
	}

	function resolveCredential(host: string, owner: string, repo: string): StoredGitCredential {
		const candidates = ctrl.store.gitCredentials.byHost(host);
		if (owner && repo) {
			const best = selectGitCredential(candidates, owner, repo);
			if (best) return best;
			throw hotbedError(
				ErrorCode.NotFound,
				`no git credential configured for ${host}/${owner}/${repo}`,
			).withSuggestion(
				"run `hotbed auth login` on the controller (or `gh auth token | hotbed auth set github --token-file -`)",
			);
		}
		for (const c of candidates) {
			if (c.cred.repos.length === 0) return c;
		}
		if (candidates.length > 0) return candidates[0];
		throw hotbedError(
			ErrorCode.NotFound,
			`no git credential configured for ${host}`,
		).withSuggestion(
			"run `hotbed auth login` on the controller (or `gh auth token | hotbed auth set github --token-file -`)",
		);
	}

	/**
	 * The stored token, or null when it must be refreshed first. Read from the
	 * store each time so a token another request refreshed is reused.
	 */
	function usableToken(name: string, host: string): { token: string; expiresAt: number } | null {
		const stored = ctrl.store.gitCredentials.byHost(host).find((c) => c.cred.name === name);
		if (!stored) return null;
		const nowMs = ctrl.now();
		if (!stored.secret.refreshToken || !stored.secret.tokenExpiresAt) {
			return { token: stored.secret.token, expiresAt: nowMs + GIT_TOKEN_LEASE_TTL_MS };
		}
		if (stored.secret.tokenExpiresAt > nowMs + OAUTH_REFRESH_LEAD_MS) {
			return { token: stored.secret.token, expiresAt: stored.secret.tokenExpiresAt };
		}
		return null;
	}

	/**
	 * Refreshes on the calling request: an in-flight fetch shared with
	 * concurrent DO requests makes workerd throw (cf-controller/AGENTS.md).
	 * GitHub refresh tokens are single-use, so when two requests race and this
	 * one loses, the winner's stored token is used instead of the error.
	 */
	async function freshToken(
		stored: StoredGitCredential,
	): Promise<{ token: string; expiresAt: number }> {
		const { name, host } = stored.cred;
		const current = usableToken(name, host);
		if (current) return current;
		try {
			return await refreshOAuthToken(stored);
		} catch (err) {
			const refreshedElsewhere = usableToken(name, host);
			if (refreshedElsewhere) return refreshedElsewhere;
			throw err;
		}
	}

	async function refreshOAuthToken(
		stored: StoredGitCredential,
	): Promise<{ token: string; expiresAt: number }> {
		const refreshToken = stored.secret.refreshToken;
		if (!refreshToken) {
			throw hotbedError(
				ErrorCode.Internal,
				`git credential ${stored.cred.name} has no refresh token`,
			);
		}
		const oauthBase = githubOAuthBase(stored.cred.host, stored.cred.oauthUrl);
		const clientId = stored.secret.clientId || DEFAULT_GITHUB_CLIENT_ID;
		const form = new URLSearchParams({
			client_id: clientId,
			grant_type: "refresh_token",
			refresh_token: refreshToken,
		});
		const res = await fetch(`${oauthBase}/login/oauth/access_token`, {
			method: "POST",
			headers: {
				Accept: "application/json",
				"Content-Type": "application/x-www-form-urlencoded",
			},
			body: form.toString(),
		});
		const data = await readJsonObject<{
			access_token?: string;
			refresh_token?: string;
			expires_in?: number;
			refresh_token_expires_in?: number;
			error?: string;
			error_description?: string;
		}>(res, `refresh GitHub OAuth token for ${stored.cred.name}`, ErrorCode.NotAuthorized);
		if (!res.ok || data.error || !data.access_token) {
			throw hotbedError(
				ErrorCode.NotAuthorized,
				`refresh GitHub OAuth token for ${stored.cred.name}: ${data.error_description || data.error || `HTTP ${res.status}`}`,
			).withSuggestion("re-authenticate on the controller with: hotbed auth login");
		}
		const refreshedNow = ctrl.now();
		const exp = data.expires_in ? refreshedNow + data.expires_in * 1000 : 0;
		const rExp = data.refresh_token_expires_in
			? refreshedNow + data.refresh_token_expires_in * 1000
			: 0;
		stored.secret.token = data.access_token;
		stored.secret.refreshToken = data.refresh_token || refreshToken;
		stored.secret.tokenExpiresAt = exp;
		stored.secret.refreshExpiresAt = rExp;
		stored.cred.expiresAt = BigInt(exp);
		stored.cred.refreshable = Boolean(stored.secret.refreshToken);
		stored.cred.updatedAt = BigInt(refreshedNow);
		ctrl.store.gitCredentials.put(stored.cred, stored.secret);
		return {
			token: stored.secret.token,
			expiresAt: exp || refreshedNow + GIT_TOKEN_LEASE_TTL_MS,
		};
	}

	return {
		async startGitHubLogin(req, ctx) {
			const caller = getCaller(ctx.values);
			if (!isOrgAdminCaller(caller)) {
				throw hotbedError(
					ErrorCode.NotAuthorized,
					"only organization admins may manage organization git credentials",
				);
			}
			const name = req.name.trim() || "github";
			if (!REPO_ID_RE.test(name)) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`credential name "${name}" must be lowercase letters, digits and dashes`,
				);
			}
			const host = validateGitHost(req.host);
			const repos = normalizeRepoPatterns(req.repos);
			const apiUrl = validateOptionalURL(req.apiUrl, "api_url");
			const oauthUrl = validateOptionalURL(req.oauthUrl, "oauth_url");
			const clientId = req.clientId.trim() || DEFAULT_GITHUB_CLIENT_ID;
			const oauthBase = githubOAuthBase(host, oauthUrl);

			const form = new URLSearchParams({ client_id: clientId });
			if (req.scopes.length > 0) form.set("scope", req.scopes.join(" "));
			const res = await fetch(`${oauthBase}/login/device/code`, {
				method: "POST",
				headers: {
					Accept: "application/json",
					"Content-Type": "application/x-www-form-urlencoded",
				},
				body: form.toString(),
			});
			const data = await readJsonObject<{
				device_code?: string;
				user_code?: string;
				verification_uri?: string;
				expires_in?: number;
				interval?: number;
				error?: string;
				error_description?: string;
			}>(res, "start GitHub OAuth device flow");
			if (!res.ok || data.error || !data.device_code || !data.user_code) {
				throw hotbedError(
					ErrorCode.Internal,
					`start GitHub OAuth device flow: ${data.error_description || data.error || `HTTP ${res.status}`}`,
				);
			}
			const nowMs = ctrl.now();
			const pending = loadPendingLogins(nowMs);
			const loginId = randomHex(12);
			const expiresIn = data.expires_in ?? 900;
			const interval = data.interval ?? 5;
			pending[loginId] = {
				name,
				host,
				repos,
				clientId,
				deviceCode: data.device_code,
				intervalMs: interval * 1000,
				expiresAt: nowMs + expiresIn * 1000,
				apiUrl,
				oauthUrl,
			};
			savePendingLogins(pending);
			return create(GitHubLoginChallengeSchema, {
				loginId,
				userCode: data.user_code,
				verificationUri: data.verification_uri ?? `${oauthBase}/login/device`,
				expiresIn,
				interval,
			});
		},

		async completeGitHubLogin(req, ctx) {
			const caller = getCaller(ctx.values);
			if (!isOrgAdminCaller(caller)) {
				throw hotbedError(
					ErrorCode.NotAuthorized,
					"only organization admins may manage organization git credentials",
				);
			}
			const loginId = req.loginId.trim();
			if (!loginId) {
				throw hotbedError(ErrorCode.InvalidArgs, "login_id is required").withSuggestion(
					"run `hotbed auth login` or pass --login-id from `hotbed auth login-start`",
				);
			}
			const pending = loadPendingLogins(ctrl.now())[loginId];
			if (!pending) {
				throw hotbedError(
					ErrorCode.NotFound,
					`no pending GitHub login "${loginId}" (it may have expired or already completed)`,
				).withSuggestion("run `hotbed auth login` to start a new login");
			}

			const oauthBase = githubOAuthBase(pending.host, pending.oauthUrl);
			let intervalMs = pending.intervalMs;
			let tokenData: {
				access_token?: string;
				refresh_token?: string;
				expires_in?: number;
				refresh_token_expires_in?: number;
			} | null = null;

			while (ctrl.now() < pending.expiresAt) {
				if (ctx.signal.aborted) {
					throw hotbedError(ErrorCode.Internal, "login canceled");
				}
				const form = new URLSearchParams({
					client_id: pending.clientId,
					device_code: pending.deviceCode,
					grant_type: "urn:ietf:params:oauth:grant-type:device_code",
				});
				const res = await fetch(`${oauthBase}/login/oauth/access_token`, {
					method: "POST",
					headers: {
						Accept: "application/json",
						"Content-Type": "application/x-www-form-urlencoded",
					},
					body: form.toString(),
				});
				const data = await readJsonObject<{
					access_token?: string;
					refresh_token?: string;
					expires_in?: number;
					refresh_token_expires_in?: number;
					error?: string;
					error_description?: string;
				}>(res, "poll GitHub OAuth device flow", ErrorCode.NotAuthorized);
				if (data.access_token) {
					tokenData = data;
					break;
				}
				if (data.error === "authorization_pending") {
					await new Promise((r) => setTimeout(r, intervalMs));
					continue;
				}
				if (data.error === "slow_down") {
					intervalMs += 5000;
					await new Promise((r) => setTimeout(r, intervalMs));
					continue;
				}
				deletePendingLogin(loginId, ctrl.now());
				throw hotbedError(
					ErrorCode.NotAuthorized,
					data.error_description || data.error || "GitHub OAuth device flow failed",
				);
			}

			deletePendingLogin(loginId, ctrl.now());

			if (!tokenData?.access_token) {
				throw hotbedError(ErrorCode.NotAuthorized, "GitHub OAuth login expired");
			}

			const apiBase = githubAPIBase(pending.host, pending.apiUrl);
			const userRes = await fetch(`${apiBase}/user`, {
				headers: {
					Accept: "application/vnd.github+json",
					Authorization: `Bearer ${tokenData.access_token}`,
					"User-Agent": "hotbed-cf-controller",
				},
			});
			if (!userRes.ok) {
				throw hotbedError(
					ErrorCode.NotAuthorized,
					`authenticated with GitHub, but failed to read user profile from ${apiBase}/user: HTTP ${userRes.status}`,
				);
			}
			const user = await readJsonObject<{ login?: string }>(
				userRes,
				`read GitHub user profile from ${apiBase}/user`,
				ErrorCode.NotAuthorized,
			);
			const nowMs = ctrl.now();
			const exp = tokenData.expires_in ? nowMs + tokenData.expires_in * 1000 : 0;
			const rExp = tokenData.refresh_token_expires_in
				? nowMs + tokenData.refresh_token_expires_in * 1000
				: 0;
			const cred = create(GitCredentialSchema, {
				name: pending.name,
				host: pending.host,
				repos: pending.repos,
				username: user.login ?? "",
				apiUrl: pending.apiUrl,
				oauthUrl: pending.oauthUrl,
				refreshable: Boolean(tokenData.refresh_token),
				expiresAt: BigInt(exp),
				updatedAt: BigInt(nowMs),
				updatedBy: caller.hostname,
			});
			ctrl.store.gitCredentials.put(cred, {
				token: tokenData.access_token,
				refreshToken: tokenData.refresh_token,
				clientId: pending.clientId,
				tokenExpiresAt: exp,
				refreshExpiresAt: rExp,
			});
			return cred;
		},

		putGitCredential(req, ctx) {
			const caller = getCaller(ctx.values);
			if (!isOrgAdminCaller(caller)) {
				throw hotbedError(
					ErrorCode.NotAuthorized,
					"only organization admins may manage organization git credentials",
				);
			}
			const name = req.name.trim();
			if (!REPO_ID_RE.test(name)) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`credential name "${req.name}" must be lowercase letters, digits and dashes`,
				);
			}
			const host = validateGitHost(req.host);
			const repos = normalizeRepoPatterns(req.repos);
			const apiUrl = validateOptionalURL(req.apiUrl, "api_url");
			const token = req.token.trim();
			if (!token) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					"token is required (--token or --token-file -)",
				).withSuggestion(
					"use `hotbed auth login` for interactive OAuth login, or `gh auth token | hotbed auth set github --token-file -`",
				);
			}
			if (/[\r\n\0]/.test(token)) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					"token must be a single line with no control characters",
				);
			}
			const cred = create(GitCredentialSchema, {
				name,
				host,
				repos,
				username: req.username.trim(),
				apiUrl,
				updatedAt: BigInt(ctrl.now()),
				updatedBy: caller.hostname,
			});
			ctrl.store.gitCredentials.put(cred, { token });
			return cred;
		},

		listGitCredentials(req) {
			const host = req.host.trim().toLowerCase();
			const creds = ctrl.store.gitCredentials.list().filter((c) => !host || c.host === host);
			return create(ListGitCredentialsResponseSchema, { credentials: creds });
		},

		deleteGitCredential(req, ctx) {
			const caller = getCaller(ctx.values);
			if (!isOrgAdminCaller(caller)) {
				throw hotbedError(
					ErrorCode.NotAuthorized,
					"only organization admins may manage organization git credentials",
				);
			}
			const name = req.name.trim();
			if (!name) throw hotbedError(ErrorCode.InvalidArgs, "credential name is required");
			if (!ctrl.store.gitCredentials.delete(name)) {
				throw hotbedError(ErrorCode.NotFound, `no git credential "${name}"`).withSuggestion(
					"hotbed auth ls",
				);
			}
			return create(DeleteGitCredentialResponseSchema, { name });
		},

		async mintGitToken(req) {
			const { host, owner, repo } = parseGitRepo(req.repo);
			const best = resolveCredential(host, owner, repo);
			const { token, expiresAt } = await freshToken(best);
			return create(GitTokenSchema, {
				host,
				repository: `${owner}/${repo}`,
				username: GIT_HTTPS_USERNAME,
				token,
				expiresAt: BigInt(expiresAt),
				credential: best.cred.name,
			});
		},

		async proxyGitHubAPI(req) {
			const path = req.path.trim();
			if (!path.startsWith("/") || path.startsWith("//") || path.includes("://")) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`GitHub API path "${req.path}" must start with a single / (e.g. /user, /repos/owner/repo, or /graphql)`,
				);
			}
			const host = validateGitHost(req.host);
			const method = (req.method.trim() || (req.body ? "POST" : "GET")).toUpperCase();
			if (!["GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"].includes(method)) {
				throw hotbedError(ErrorCode.InvalidArgs, `unsupported HTTP method "${method}"`);
			}
			const { owner, repo } = extractRepoHint(req.repo, path);
			const best = resolveCredential(host, owner, repo);
			const { token } = await freshToken(best);

			const target = `${githubAPIBase(best.cred.host, best.cred.apiUrl)}${path}`;
			const headers = new Headers();
			let hasAccept = false;
			let hasContentType = false;
			let hasAPIVersion = false;
			for (const h of req.headers) {
				const colon = h.indexOf(":");
				if (colon < 0) {
					throw hotbedError(ErrorCode.InvalidArgs, `header "${h}" must be "Key: Value"`);
				}
				const k = h.slice(0, colon).trim();
				const v = h.slice(colon + 1).trim();
				const low = k.toLowerCase();
				if (!low || ["authorization", "cookie", "host", "content-length"].includes(low)) continue;
				if (low === "accept") hasAccept = true;
				if (low === "content-type") hasContentType = true;
				if (low === "x-github-api-version") hasAPIVersion = true;
				headers.append(k, v);
			}
			if (!hasAccept) headers.set("Accept", "application/vnd.github+json");
			if (!hasContentType && req.body) headers.set("Content-Type", "application/json");
			if (!hasAPIVersion) headers.set("X-GitHub-Api-Version", "2022-11-28");
			headers.set("Authorization", `Bearer ${token}`);
			headers.set("User-Agent", "hotbed-cf-controller");

			const resp = await fetch(target, {
				method,
				headers,
				body: req.body && method !== "GET" && method !== "HEAD" ? req.body : undefined,
			});
			const capped = await readBodyCapped(resp, MAX_PROXY_RESPONSE_BYTES);
			const outHeaders: string[] = [];
			resp.headers.forEach((v, k) => {
				const low = k.toLowerCase();
				if (low === "set-cookie" || low === "authorization") return;
				outHeaders.push(`${k}: ${v}`);
			});
			outHeaders.sort();
			return create(ProxyGitHubAPIResponseSchema, {
				statusCode: resp.status,
				headers: outHeaders,
				body: new TextDecoder().decode(capped),
			});
		},
	};
}
