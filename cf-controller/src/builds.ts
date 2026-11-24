import { create } from "@bufbuild/protobuf";
import { BuildSchema, type Build } from "./gen/hotbed/v1/hotbed_pb.ts";
import { ErrorCode, hotbedError } from "./errors.ts";
import {
	COMPILED_BUILD_NUMBER,
	COMPILED_BUILD_SHA256SUMS,
	COMPILED_GIT_COMMIT,
	formatControllerVersion,
	parseSha256Sums,
} from "./version.ts";

export const SUPPORTED_PLATFORMS = [
	{ os: "linux", arch: "amd64" },
	{ os: "linux", arch: "arm64" },
	{ os: "darwin", arch: "arm64" },
] as const;

export interface CachedRelease {
	buildNumber: number;
	tag: string;
	fetchedAt: number;
	assets: Record<string, { sha256: string; downloadUrl: string; apiUrl: string }>;
}

export interface BuildsEnv {
	UPDATES_REPO?: string;
	UPDATES_API_BASE?: string;
	UPDATES_DOWNLOAD_BASE?: string;
	GITHUB_TOKEN?: string;
	BUILDS_BUCKET?: R2Bucket;
	CONTROLLER_BUILD_NUMBER?: string;
	CONTROLLER_GIT_COMMIT?: string;
	CONTROLLER_BUILD_SHA256SUMS?: string;
}

export interface TestBuildConfig {
	updatesRepo?: string;
	updatesApiBase?: string;
	updatesDownloadBase?: string;
	updatesToken?: string;
	controllerBuild?: number;
	controllerCommit?: string;
	controllerSha256Sums?: string;
	mockBuild?: {
		number: number;
		assets: Record<string, { sha256: string; contentBase64: string; downloadUrl?: string }>;
	};
	clearBuild?: boolean;
}

export interface MetaKV {
	getMeta(key: string): string | null;
	setMeta(key: string, value: string): void;
}

const RELEASE_CACHE_KEY = "github_latest_release";
const REFRESH_TTL_MS = 2 * 60_000;
const FAILURE_COOLDOWN_MS = 15_000;
const DEFAULT_API_BASE = "https://api.github.com";
const DEFAULT_DOWNLOAD_BASE = "https://github.com";

export function assetName(os: string, arch: string): string {
	return `hotbed-${os}-${arch}`;
}

export function isSupportedPlatform(os: string, arch: string): boolean {
	return SUPPORTED_PLATFORMS.some((p) => p.os === os && p.arch === arch);
}

function parsePositiveBuild(raw: string | undefined): number {
	const trimmed = (raw ?? "").trim();
	if (!/^\d+$/.test(trimmed)) return 0;
	const n = Number(trimmed);
	return Number.isSafeInteger(n) && n > 0 ? n : 0;
}

interface GitHubRelease {
	tag_name?: string;
	draft?: boolean;
	assets?: Array<{ name?: string; url?: string; browser_download_url?: string }>;
}

export class BuildDistributor {
	private readonly env: BuildsEnv;
	private readonly kv: MetaKV;
	private readonly waitUntil?: (promise: Promise<unknown>) => void;
	private memCache: CachedRelease | null = null;
	private updatesRepoOverride?: string;
	private updatesApiBaseOverride?: string;
	private updatesDownloadBaseOverride?: string;
	private updatesTokenOverride?: string;
	private controllerBuildOverride?: number;
	private controllerCommitOverride?: string;
	private controllerSha256SumsOverride?: string;
	private mockAssets = new Map<string, Uint8Array>();
	private inflightRefresh: Promise<CachedRelease | null> | null = null;
	private lastFailedAt = 0;

	constructor(env: BuildsEnv, kv: MetaKV, waitUntil?: (promise: Promise<unknown>) => void) {
		this.env = env;
		this.kv = kv;
		this.waitUntil = waitUntil;
	}

	setTestConfig(cfg: TestBuildConfig): void {
		if (cfg.updatesRepo !== undefined) this.updatesRepoOverride = cfg.updatesRepo;
		if (cfg.updatesApiBase !== undefined) this.updatesApiBaseOverride = cfg.updatesApiBase;
		if (cfg.updatesDownloadBase !== undefined) {
			this.updatesDownloadBaseOverride = cfg.updatesDownloadBase;
		}
		if (cfg.updatesToken !== undefined) this.updatesTokenOverride = cfg.updatesToken;
		if (cfg.controllerBuild !== undefined) this.controllerBuildOverride = cfg.controllerBuild;
		if (cfg.controllerCommit !== undefined) this.controllerCommitOverride = cfg.controllerCommit;
		if (cfg.controllerSha256Sums !== undefined) {
			this.controllerSha256SumsOverride = cfg.controllerSha256Sums;
		}
		this.lastFailedAt = 0;
		if (cfg.clearBuild) {
			this.memCache = null;
			this.mockAssets.clear();
			this.kv.setMeta(RELEASE_CACHE_KEY, "");
		}
		if (cfg.mockBuild) {
			this.mockAssets.clear();
			const assets: CachedRelease["assets"] = {};
			for (const [name, a] of Object.entries(cfg.mockBuild.assets)) {
				const bin = atob(a.contentBase64);
				const bytes = new Uint8Array(bin.length);
				for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
				this.mockAssets.set(`${cfg.mockBuild.number}/${name}`, bytes);
				assets[name] = { sha256: a.sha256, downloadUrl: a.downloadUrl ?? "", apiUrl: "" };
			}
			const rel: CachedRelease = {
				buildNumber: cfg.mockBuild.number,
				tag: `build-${cfg.mockBuild.number}`,
				fetchedAt: Date.now(),
				assets,
			};
			this.memCache = rel;
			this.kv.setMeta(RELEASE_CACHE_KEY, JSON.stringify(rel));
		}
	}

	/** Build number compiled into or configured on this controller deployment (0 for dev). */
	pinnedBuildNumber(): number {
		if (this.controllerBuildOverride !== undefined) {
			return this.controllerBuildOverride > 0 ? this.controllerBuildOverride : 0;
		}
		const fromEnv = parsePositiveBuild(this.env.CONTROLLER_BUILD_NUMBER);
		if (fromEnv > 0) return fromEnv;
		return COMPILED_BUILD_NUMBER;
	}

	/** Git commit SHA compiled into or configured on this controller deployment. */
	controllerCommit(): string {
		if (this.controllerCommitOverride !== undefined) {
			return this.controllerCommitOverride.trim();
		}
		if (this.env.CONTROLLER_GIT_COMMIT !== undefined) {
			return this.env.CONTROLLER_GIT_COMMIT.trim();
		}
		return COMPILED_GIT_COMMIT;
	}

	private bakedSha256Sums(): string {
		if (this.controllerSha256SumsOverride !== undefined) {
			return this.controllerSha256SumsOverride;
		}
		if (this.env.CONTROLLER_BUILD_SHA256SUMS !== undefined) {
			return this.env.CONTROLLER_BUILD_SHA256SUMS;
		}
		return COMPILED_BUILD_SHA256SUMS;
	}

	private updatesRepo(): string {
		return (this.updatesRepoOverride ?? this.env.UPDATES_REPO ?? "").trim();
	}

	private updatesApiBase(): string {
		return (this.updatesApiBaseOverride ?? this.env.UPDATES_API_BASE ?? DEFAULT_API_BASE)
			.trim()
			.replace(/\/+$/, "");
	}

	private updatesDownloadBase(): string {
		return (
			this.updatesDownloadBaseOverride ??
			this.env.UPDATES_DOWNLOAD_BASE ??
			DEFAULT_DOWNLOAD_BASE
		)
			.trim()
			.replace(/\/+$/, "");
	}

	private updatesToken(): string {
		return (this.updatesTokenOverride ?? this.env.GITHUB_TOKEN ?? "").trim();
	}

	private directAssetUrl(repo: string, buildNum: number, name: string): string {
		if (!repo) return "";
		return `${this.updatesDownloadBase()}/${repo}/releases/download/build-${buildNum}/${name}`;
	}

	/** Release constructed directly from build-time stamped SHA256SUMS (zero network calls). */
	private bakedRelease(): CachedRelease | null {
		const pinned = this.pinnedBuildNumber();
		if (pinned <= 0) return null;
		const sumsText = this.bakedSha256Sums();
		if (!sumsText.trim()) return null;
		const sums = parseSha256Sums(sumsText);
		const repo = this.updatesRepo();
		const assets: CachedRelease["assets"] = {};
		for (const p of SUPPORTED_PLATFORMS) {
			const name = assetName(p.os, p.arch);
			const sha = sums.get(name);
			if (!sha) continue;
			assets[name] = {
				sha256: sha,
				downloadUrl: this.directAssetUrl(repo, pinned, name),
				apiUrl: "",
			};
		}
		if (Object.keys(assets).length === 0) return null;
		return {
			buildNumber: pinned,
			tag: `build-${pinned}`,
			fetchedAt: 0,
			assets,
		};
	}

	getCachedRelease(): CachedRelease | null {
		const pinned = this.pinnedBuildNumber();
		if (pinned > 0) {
			const baked = this.bakedRelease();
			if (baked) return baked;
		}
		let rel = this.memCache;
		if (!rel) {
			const raw = this.kv.getMeta(RELEASE_CACHE_KEY);
			if (raw) {
				try {
					rel = JSON.parse(raw) as CachedRelease;
					this.memCache = rel;
				} catch {
					rel = null;
				}
			}
		}
		if (pinned > 0 && rel && rel.buildNumber !== pinned) {
			return null;
		}
		return rel;
	}

	currentBuildNumber(): number {
		const pinned = this.pinnedBuildNumber();
		if (pinned > 0) return pinned;
		return this.getCachedRelease()?.buildNumber ?? 0;
	}

	versionString(): string {
		return formatControllerVersion(this.currentBuildNumber(), this.controllerCommit());
	}

	buildFor(os: string, arch: string): Build | undefined {
		const rel = this.getCachedRelease();
		if (!rel || rel.buildNumber <= 0) return undefined;
		const name = assetName(os, arch);
		const asset = rel.assets[name];
		if (!asset) return undefined;
		// Private repos (configured with GITHUB_TOKEN) require authenticated /builds/ proxying.
		const downloadUrl = this.updatesToken() ? "" : asset.downloadUrl || "";
		return create(BuildSchema, {
			number: BigInt(rel.buildNumber),
			sha256: asset.sha256,
			path: `/builds/${rel.buildNumber}/${name}`,
			downloadUrl,
		});
	}

	needsImmediateRefresh(nowMs: number): boolean {
		if (this.pinnedBuildNumber() <= 0) return false;
		if (this.getCachedRelease() !== null) return false;
		if (!this.updatesRepo()) return false;
		if (this.lastFailedAt > 0 && nowMs - this.lastFailedAt < FAILURE_COOLDOWN_MS) {
			return false;
		}
		return true;
	}

	needsBackgroundRefresh(): boolean {
		if (!this.updatesRepo()) return false;
		const pinned = this.pinnedBuildNumber();
		if (pinned > 0) return this.getCachedRelease() === null;
		return true;
	}

	async ensureLoaded(nowMs: number): Promise<CachedRelease | null> {
		if (!this.needsImmediateRefresh(nowMs)) {
			return this.getCachedRelease();
		}
		return this.refreshIfStale(nowMs, false);
	}

	async refreshIfStale(nowMs: number, force = false): Promise<CachedRelease | null> {
		if (this.inflightRefresh) {
			return this.inflightRefresh;
		}
		const p = this.doRefresh(nowMs, force)
			.then((rel) => {
				this.lastFailedAt = 0;
				return rel;
			})
			.catch((err) => {
				this.lastFailedAt = nowMs;
				throw err;
			})
			.finally(() => {
				if (this.inflightRefresh === p) {
					this.inflightRefresh = null;
				}
			});
		this.inflightRefresh = p;
		return p;
	}

	private async doRefresh(nowMs: number, force: boolean): Promise<CachedRelease | null> {
		const pinned = this.pinnedBuildNumber();
		const repo = this.updatesRepo();

		if (pinned > 0) {
			const baked = this.bakedRelease();
			if (baked) return baked;
			const cached = this.getCachedRelease();
			if (!force && cached && cached.buildNumber === pinned) return cached;
			if (!repo) return cached;

			const rel = await this.fetchPinnedRelease(repo, pinned, nowMs);
			if (rel) {
				this.memCache = rel;
				this.kv.setMeta(RELEASE_CACHE_KEY, JSON.stringify(rel));
				return rel;
			}
			return cached;
		}

		if (!repo) return this.getCachedRelease();
		const cached = this.getCachedRelease();
		if (!force && cached && nowMs - cached.fetchedAt < REFRESH_TTL_MS) {
			return cached;
		}
		const next = await this.fetchLatestRelease(repo, nowMs);
		if (!next) return cached;
		this.memCache = next;
		this.kv.setMeta(RELEASE_CACHE_KEY, JSON.stringify(next));
		return next;
	}

	private async fetchPinnedRelease(
		repo: string,
		buildNum: number,
		nowMs: number,
	): Promise<CachedRelease | null> {
		const token = this.updatesToken();
		const apiBase = this.updatesApiBase();
		if (!token && apiBase === DEFAULT_API_BASE) {
			// Public GitHub release: fetch SHA256SUMS directly without hitting the rate-limited REST API.
			const sumsUrl = this.directAssetUrl(repo, buildNum, "SHA256SUMS");
			const sumsRes = await fetchFollowingRedirectWithoutAuth(sumsUrl, {
				Accept: "application/octet-stream",
				"User-Agent": "hotbed-cf-controller",
			});
			if (!sumsRes.ok) {
				throw hotbedError(
					ErrorCode.Internal,
					`fetch SHA256SUMS for build-${buildNum}: HTTP ${sumsRes.status}`,
				);
			}
			const sums = parseSha256Sums(await sumsRes.text());
			const assets: CachedRelease["assets"] = {};
			for (const p of SUPPORTED_PLATFORMS) {
				const name = assetName(p.os, p.arch);
				const sha = sums.get(name);
				if (!sha) continue;
				assets[name] = {
					sha256: sha,
					downloadUrl: this.directAssetUrl(repo, buildNum, name),
					apiUrl: "",
				};
			}
			if (Object.keys(assets).length === 0) return null;
			return {
				buildNumber: buildNum,
				tag: `build-${buildNum}`,
				fetchedAt: nowMs,
				assets,
			};
		}

		const headers = this.apiHeaders(token);
		const res = await fetch(`${apiBase}/repos/${repo}/releases/tags/build-${buildNum}`, {
			headers,
		});
		if (!res.ok) {
			throw hotbedError(
				ErrorCode.Internal,
				`fetch GitHub release build-${buildNum} for ${repo}: HTTP ${res.status}`,
			);
		}
		const rel = (await res.json()) as GitHubRelease;
		return this.parseGitHubRelease(rel, buildNum, token, nowMs);
	}

	private async fetchLatestRelease(repo: string, nowMs: number): Promise<CachedRelease | null> {
		const token = this.updatesToken();
		const apiBase = this.updatesApiBase();
		const headers = this.apiHeaders(token);
		const res = await fetch(`${apiBase}/repos/${repo}/releases?per_page=10`, {
			headers,
		});
		if (!res.ok) {
			throw hotbedError(
				ErrorCode.Internal,
				`fetch GitHub releases for ${repo}: HTTP ${res.status}`,
			);
		}
		const releases = (await res.json()) as GitHubRelease[];

		let bestNum = 0;
		let bestRel: GitHubRelease | null = null;
		for (const r of releases) {
			if (r.draft || !r.tag_name) continue;
			const m = /^build-(\d+)$/.exec(r.tag_name);
			if (!m) continue;
			const num = Number(m[1]);
			if (num > bestNum) {
				bestNum = num;
				bestRel = r;
			}
		}
		if (!bestRel || bestNum <= 0) return null;
		return this.parseGitHubRelease(bestRel, bestNum, token, nowMs);
	}

	private apiHeaders(token: string): Record<string, string> {
		const headers: Record<string, string> = {
			Accept: "application/vnd.github+json",
			"User-Agent": "hotbed-cf-controller",
			"X-GitHub-Api-Version": "2022-11-28",
		};
		if (token) headers.Authorization = `Bearer ${token}`;
		return headers;
	}

	private async parseGitHubRelease(
		rel: GitHubRelease,
		buildNum: number,
		token: string,
		nowMs: number,
	): Promise<CachedRelease | null> {
		const sumsAsset = (rel.assets ?? []).find((a) => a.name === "SHA256SUMS");
		if (!sumsAsset) return null;

		const sumsHeaders: Record<string, string> = {
			Accept: "application/octet-stream",
			"User-Agent": "hotbed-cf-controller",
		};
		const useApi = Boolean(token && sumsAsset.url);
		if (useApi) sumsHeaders.Authorization = `Bearer ${token}`;
		const sumsUrl = useApi ? (sumsAsset.url ?? "") : (sumsAsset.browser_download_url ?? "");
		const sumsRes = await fetchFollowingRedirectWithoutAuth(sumsUrl, sumsHeaders);
		if (!sumsRes.ok) {
			throw hotbedError(
				ErrorCode.Internal,
				`fetch SHA256SUMS for ${rel.tag_name ?? `build-${buildNum}`}: HTTP ${sumsRes.status}`,
			);
		}
		const sums = parseSha256Sums(await sumsRes.text());

		const assets: CachedRelease["assets"] = {};
		for (const a of rel.assets ?? []) {
			if (!a.name) continue;
			const sha = sums.get(a.name);
			if (!sha) continue;
			assets[a.name] = {
				sha256: sha,
				downloadUrl: a.browser_download_url ?? "",
				apiUrl: a.url ?? "",
			};
		}

		return {
			buildNumber: buildNum,
			tag: rel.tag_name ?? `build-${buildNum}`,
			fetchedAt: nowMs,
			assets,
		};
	}

	async serveAsset(buildNum: number, name: string): Promise<Response> {
		const rel = this.getCachedRelease();
		if (!rel || rel.buildNumber !== buildNum || !rel.assets[name]) {
			return new Response(
				`the controller hands out only its own build's binaries; it runs ${this.versionString()}`,
				{ status: 404 },
			);
		}
		const asset = rel.assets[name];
		const mockBytes = this.mockAssets.get(`${buildNum}/${name}`);
		if (mockBytes) {
			return new Response(mockBytes, {
				status: 200,
				headers: {
					"Content-Type": "application/octet-stream",
					"Cache-Control": "private, max-age=31536000, immutable",
					ETag: `"${asset.sha256}"`,
				},
			});
		}
		const r2Key = `builds/${buildNum}/${name}`;

		// 1. Serve from R2 cache if configured.
		if (this.env.BUILDS_BUCKET) {
			const obj = await this.env.BUILDS_BUCKET.get(r2Key);
			if (obj) {
				return new Response(obj.body, {
					status: 200,
					headers: {
						"Content-Type": "application/octet-stream",
						"Cache-Control": "private, max-age=31536000, immutable",
						ETag: `"${asset.sha256}"`,
					},
				});
			}
		}

		// 2. Proxy stream from GitHub Releases (and populate R2 asynchronously if bound and sized).
		const token = this.updatesToken();
		const useApi = Boolean(token && asset.apiUrl);
		const headers: Record<string, string> = {
			Accept: "application/octet-stream",
			"User-Agent": "hotbed-cf-controller",
		};
		if (useApi) headers.Authorization = `Bearer ${token}`;
		const url = useApi ? asset.apiUrl : asset.downloadUrl;
		if (!url) {
			return new Response(`no upstream URL for build ${buildNum} asset ${name}`, { status: 404 });
		}
		const upstream = await fetchFollowingRedirectWithoutAuth(url, headers);
		if (!upstream.ok || !upstream.body) {
			return new Response(`upstream build asset error: HTTP ${upstream.status}`, { status: 502 });
		}

		const contentLength = Number(upstream.headers.get("Content-Length") ?? "");
		if (this.env.BUILDS_BUCKET && Number.isSafeInteger(contentLength) && contentLength > 0) {
			const [forClient, forR2] = upstream.body.tee();
			const fixed = new FixedLengthStream(contentLength);
			const putTask = Promise.all([
				forR2.pipeTo(fixed.writable),
				this.env.BUILDS_BUCKET.put(r2Key, fixed.readable),
			]).catch(() => {});
			if (this.waitUntil) {
				this.waitUntil(putTask);
			} else {
				void putTask;
			}
			return new Response(forClient, {
				status: 200,
				headers: {
					"Content-Type": "application/octet-stream",
					"Cache-Control": "private, max-age=31536000, immutable",
					ETag: `"${asset.sha256}"`,
				},
			});
		}

		return new Response(upstream.body, {
			status: 200,
			headers: {
				"Content-Type": "application/octet-stream",
				"Cache-Control": "private, max-age=31536000, immutable",
				ETag: `"${asset.sha256}"`,
			},
		});
	}
}

async function fetchFollowingRedirectWithoutAuth(
	url: string,
	headers: Record<string, string>,
): Promise<Response> {
	let currentUrl = url;
	let currentHeaders = headers;
	for (let hops = 0; hops < 5; hops++) {
		const res = await fetch(currentUrl, { headers: currentHeaders, redirect: "manual" });
		if (res.status >= 300 && res.status < 400) {
			const loc = res.headers.get("Location");
			if (!loc) return res;
			await res.body?.cancel();
			currentUrl = new URL(loc, currentUrl).toString();
			const stripped: Record<string, string> = {};
			for (const [k, v] of Object.entries(currentHeaders)) {
				if (k.toLowerCase() !== "authorization") stripped[k] = v;
			}
			currentHeaders = stripped;
			continue;
		}
		return res;
	}
	return fetch(currentUrl, { headers: currentHeaders, redirect: "follow" });
}
