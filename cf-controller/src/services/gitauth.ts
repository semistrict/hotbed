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
import { getCaller } from "../connect-adapter.ts";
import { REPO_ID_RE, type ControllerContext } from "../context.ts";
import { ErrorCode, hotbedError } from "../errors.ts";
import { parseGitRepo, randomHex } from "../gitops.ts";
import type { StoredGitCredential } from "../store.ts";

const DEFAULT_GITHUB_CLIENT_ID = "Iv23li5qE6N9l4g7P3hW";
const GIT_TOKEN_LEASE_TTL_MS = 15 * 60_000;
const OAUTH_REFRESH_LEAD_MS = 5 * 60_000;
const GIT_HTTPS_USERNAME = "x-access-token";
const MAX_PROXY_RESPONSE_BYTES = 16 * 1024 * 1024;

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
	const pendingLogins = new Map<string, PendingGitHubLogin>();

	function resolveCredential(host: string, owner: string, repo: string): StoredGitCredential {
		const candidates = ctrl.store.gitCredentialsByHost(host);
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

	async function freshToken(
		stored: StoredGitCredential,
	): Promise<{ token: string; expiresAt: number }> {
		const nowMs = ctrl.now();
		if (!stored.secret.refreshToken || !stored.secret.tokenExpiresAt) {
			return { token: stored.secret.token, expiresAt: nowMs + GIT_TOKEN_LEASE_TTL_MS };
		}
		if (stored.secret.tokenExpiresAt > nowMs + OAUTH_REFRESH_LEAD_MS) {
			return { token: stored.secret.token, expiresAt: stored.secret.tokenExpiresAt };
		}
		const oauthBase = githubOAuthBase(stored.cred.host, stored.cred.oauthUrl);
		const clientId = stored.secret.clientId || DEFAULT_GITHUB_CLIENT_ID;
		const form = new URLSearchParams({
			client_id: clientId,
			grant_type: "refresh_token",
			refresh_token: stored.secret.refreshToken,
		});
		const res = await fetch(`${oauthBase}/login/oauth/access_token`, {
			method: "POST",
			headers: {
				Accept: "application/json",
				"Content-Type": "application/x-www-form-urlencoded",
			},
			body: form.toString(),
		});
		const data = (await res.json()) as {
			access_token?: string;
			refresh_token?: string;
			expires_in?: number;
			refresh_token_expires_in?: number;
			error?: string;
			error_description?: string;
		};
		if (!res.ok || data.error || !data.access_token) {
			throw hotbedError(
				ErrorCode.NotAuthorized,
				`refresh GitHub OAuth token for ${stored.cred.name}: ${data.error_description || data.error || `HTTP ${res.status}`}`,
			).withSuggestion("re-authenticate on the controller with: hotbed auth login");
		}
		const exp = data.expires_in ? nowMs + data.expires_in * 1000 : 0;
		const rExp = data.refresh_token_expires_in ? nowMs + data.refresh_token_expires_in * 1000 : 0;
		stored.secret.token = data.access_token;
		stored.secret.refreshToken = data.refresh_token || stored.secret.refreshToken;
		stored.secret.tokenExpiresAt = exp;
		stored.secret.refreshExpiresAt = rExp;
		stored.cred.expiresAt = BigInt(exp);
		stored.cred.refreshable = Boolean(stored.secret.refreshToken);
		stored.cred.updatedAt = BigInt(nowMs);
		ctrl.store.putGitCredential(stored.cred, stored.secret);
		return { token: stored.secret.token, expiresAt: exp || nowMs + GIT_TOKEN_LEASE_TTL_MS };
	}

	return {
		async startGitHubLogin(req) {
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
			const data = (await res.json()) as {
				device_code?: string;
				user_code?: string;
				verification_uri?: string;
				expires_in?: number;
				interval?: number;
				error?: string;
				error_description?: string;
			};
			if (!res.ok || data.error || !data.device_code || !data.user_code) {
				throw hotbedError(
					ErrorCode.Internal,
					`start GitHub OAuth device flow: ${data.error_description || data.error || `HTTP ${res.status}`}`,
				);
			}
			const nowMs = ctrl.now();
			for (const [id, p] of pendingLogins) {
				if (nowMs > p.expiresAt) pendingLogins.delete(id);
			}
			const loginId = randomHex(12);
			const expiresIn = data.expires_in ?? 900;
			const interval = data.interval ?? 5;
			pendingLogins.set(loginId, {
				name,
				host,
				repos,
				clientId,
				deviceCode: data.device_code,
				intervalMs: interval * 1000,
				expiresAt: nowMs + expiresIn * 1000,
				apiUrl,
				oauthUrl,
			});
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
			const loginId = req.loginId.trim();
			if (!loginId) {
				throw hotbedError(ErrorCode.InvalidArgs, "login_id is required").withSuggestion(
					"run `hotbed auth login` or pass --login-id from `hotbed auth login-start`",
				);
			}
			const pending = pendingLogins.get(loginId);
			if (!pending) {
				throw hotbedError(
					ErrorCode.NotFound,
					`no pending GitHub login "${loginId}" (it may have expired or already completed)`,
				).withSuggestion("run `hotbed auth login` to start a new login");
			}
			pendingLogins.delete(loginId);

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
				const data = (await res.json()) as {
					access_token?: string;
					refresh_token?: string;
					expires_in?: number;
					refresh_token_expires_in?: number;
					error?: string;
					error_description?: string;
				};
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
				throw hotbedError(
					ErrorCode.NotAuthorized,
					data.error_description || data.error || "GitHub OAuth device flow failed",
				);
			}

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
			const user = (await userRes.json()) as { login?: string };
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
			ctrl.store.putGitCredential(cred, {
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
			ctrl.store.putGitCredential(cred, { token });
			return cred;
		},

		listGitCredentials(req) {
			const host = req.host.trim().toLowerCase();
			const creds = ctrl.store.listGitCredentials().filter((c) => !host || c.host === host);
			return create(ListGitCredentialsResponseSchema, { credentials: creds });
		},

		deleteGitCredential(req) {
			const name = req.name.trim();
			if (!name) throw hotbedError(ErrorCode.InvalidArgs, "credential name is required");
			if (!ctrl.store.deleteGitCredential(name)) {
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
			const buf = new Uint8Array(await resp.arrayBuffer());
			const capped =
				buf.byteLength > MAX_PROXY_RESPONSE_BYTES ? buf.subarray(0, MAX_PROXY_RESPONSE_BYTES) : buf;
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
