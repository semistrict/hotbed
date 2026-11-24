import { create } from "@bufbuild/protobuf";
import { BuildSchema, type Build } from "./gen/hotbed/v1/hotbed_pb.ts";
import { ErrorCode, hotbedError } from "./errors.ts";

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
	GITHUB_TOKEN?: string;
	BUILDS_BUCKET?: R2Bucket;
}

export interface TestBuildConfig {
	updatesRepo?: string;
	updatesApiBase?: string;
	updatesToken?: string;
	mockBuild?: {
		number: number;
		assets: Record<string, { sha256: string; contentBase64: string }>;
	};
	clearBuild?: boolean;
}

export interface MetaKV {
	getMeta(key: string): string | null;
	setMeta(key: string, value: string): void;
}

const RELEASE_CACHE_KEY = "github_latest_release";
const REFRESH_TTL_MS = 2 * 60_000;

export function assetName(os: string, arch: string): string {
	return `hotbed-${os}-${arch}`;
}

export function isSupportedPlatform(os: string, arch: string): boolean {
	return SUPPORTED_PLATFORMS.some((p) => p.os === os && p.arch === arch);
}

export class BuildDistributor {
	private readonly env: BuildsEnv;
	private readonly kv: MetaKV;
	private memCache: CachedRelease | null = null;
	private updatesRepoOverride?: string;
	private updatesApiBaseOverride?: string;
	private updatesTokenOverride?: string;
	private mockAssets = new Map<string, Uint8Array>();

	constructor(env: BuildsEnv, kv: MetaKV) {
		this.env = env;
		this.kv = kv;
	}

	setTestConfig(cfg: TestBuildConfig): void {
		if (cfg.updatesRepo !== undefined) this.updatesRepoOverride = cfg.updatesRepo;
		if (cfg.updatesApiBase !== undefined) this.updatesApiBaseOverride = cfg.updatesApiBase;
		if (cfg.updatesToken !== undefined) this.updatesTokenOverride = cfg.updatesToken;
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
				assets[name] = { sha256: a.sha256, downloadUrl: "", apiUrl: "" };
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

	getCachedRelease(): CachedRelease | null {
		if (this.memCache) return this.memCache;
		const raw = this.kv.getMeta(RELEASE_CACHE_KEY);
		if (!raw) return null;
		try {
			this.memCache = JSON.parse(raw) as CachedRelease;
			return this.memCache;
		} catch {
			return null;
		}
	}

	currentBuildNumber(): number {
		return this.getCachedRelease()?.buildNumber ?? 0;
	}

	versionString(): string {
		const n = this.currentBuildNumber();
		return n > 0 ? `build ${n} (cloudflare-do)` : "dev (cloudflare-do)";
	}

	buildFor(os: string, arch: string): Build | undefined {
		const rel = this.getCachedRelease();
		if (!rel || rel.buildNumber <= 0) return undefined;
		const name = assetName(os, arch);
		const asset = rel.assets[name];
		if (!asset) return undefined;
		return create(BuildSchema, {
			number: BigInt(rel.buildNumber),
			sha256: asset.sha256,
			path: `/builds/${rel.buildNumber}/${name}`,
		});
	}

	async refreshIfStale(nowMs: number, force = false): Promise<CachedRelease | null> {
		const repo = (this.updatesRepoOverride ?? this.env.UPDATES_REPO ?? "").trim();
		if (!repo) return this.getCachedRelease();
		const cached = this.getCachedRelease();
		if (!force && cached && nowMs - cached.fetchedAt < REFRESH_TTL_MS) {
			return cached;
		}
		const headers: Record<string, string> = {
			Accept: "application/vnd.github+json",
			"User-Agent": "hotbed-cf-controller",
			"X-GitHub-Api-Version": "2022-11-28",
		};
		const token = (this.updatesTokenOverride ?? this.env.GITHUB_TOKEN ?? "").trim();
		if (token) headers.Authorization = `Bearer ${token}`;

		const apiBase = (
			this.updatesApiBaseOverride ??
			this.env.UPDATES_API_BASE ??
			"https://api.github.com"
		)
			.trim()
			.replace(/\/+$/, "");
		const res = await fetch(`${apiBase}/repos/${repo}/releases?per_page=10`, {
			headers,
		});
		if (!res.ok) {
			throw hotbedError(
				ErrorCode.Internal,
				`fetch GitHub releases for ${repo}: HTTP ${res.status}`,
			);
		}
		const releases = (await res.json()) as Array<{
			tag_name?: string;
			draft?: boolean;
			assets?: Array<{ name?: string; url?: string; browser_download_url?: string }>;
		}>;

		let bestNum = 0;
		let bestRel: (typeof releases)[number] | null = null;
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
		if (!bestRel || bestNum <= 0) return cached;

		const sumsAsset = (bestRel.assets ?? []).find((a) => a.name === "SHA256SUMS");
		if (!sumsAsset) return cached;

		const sumsHeaders: Record<string, string> = {
			Accept: "application/octet-stream",
			"User-Agent": "hotbed-cf-controller",
		};
		if (token) sumsHeaders.Authorization = `Bearer ${token}`;
		const sumsUrl = token && sumsAsset.url ? sumsAsset.url : (sumsAsset.browser_download_url ?? "");
		const sumsRes = await fetchFollowingRedirectWithoutAuth(sumsUrl, sumsHeaders);
		if (!sumsRes.ok) {
			throw hotbedError(
				ErrorCode.Internal,
				`fetch SHA256SUMS for ${bestRel.tag_name}: HTTP ${sumsRes.status}`,
			);
		}
		const sumsText = await sumsRes.text();
		const sums = new Map<string, string>();
		for (const line of sumsText.split(/\r?\n/)) {
			const trimmed = line.trim();
			if (!trimmed) continue;
			const parts = trimmed.split(/\s+/);
			if (parts.length >= 2) {
				sums.set(parts[1].replace(/^\*/, ""), parts[0].toLowerCase());
			}
		}

		const assets: CachedRelease["assets"] = {};
		for (const a of bestRel.assets ?? []) {
			if (!a.name) continue;
			const sha = sums.get(a.name);
			if (!sha) continue;
			assets[a.name] = {
				sha256: sha,
				downloadUrl: a.browser_download_url ?? "",
				apiUrl: a.url ?? "",
			};
		}

		const next: CachedRelease = {
			buildNumber: bestNum,
			tag: bestRel.tag_name ?? `build-${bestNum}`,
			fetchedAt: nowMs,
			assets,
		};
		this.memCache = next;
		this.kv.setMeta(RELEASE_CACHE_KEY, JSON.stringify(next));
		return next;
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
					"Cache-Control": "public, max-age=31536000, immutable",
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
						"Cache-Control": "public, max-age=31536000, immutable",
						ETag: `"${asset.sha256}"`,
					},
				});
			}
		}

		// 2. Proxy stream from GitHub Releases (and populate R2 asynchronously if bound).
		const token = (this.updatesTokenOverride ?? this.env.GITHUB_TOKEN ?? "").trim();
		const headers: Record<string, string> = {
			Accept: "application/octet-stream",
			"User-Agent": "hotbed-cf-controller",
		};
		if (token) headers.Authorization = `Bearer ${token}`;
		const url = token && asset.apiUrl ? asset.apiUrl : asset.downloadUrl;
		const upstream = await fetchFollowingRedirectWithoutAuth(url, headers);
		if (!upstream.ok || !upstream.body) {
			return new Response(`upstream build asset error: HTTP ${upstream.status}`, { status: 502 });
		}

		if (this.env.BUILDS_BUCKET) {
			const [forClient, forR2] = upstream.body.tee();
			void this.env.BUILDS_BUCKET.put(r2Key, forR2).catch(() => {});
			return new Response(forClient, {
				status: 200,
				headers: {
					"Content-Type": "application/octet-stream",
					"Cache-Control": "public, max-age=31536000, immutable",
					ETag: `"${asset.sha256}"`,
				},
			});
		}

		return new Response(upstream.body, {
			status: 200,
			headers: {
				"Content-Type": "application/octet-stream",
				"Cache-Control": "public, max-age=31536000, immutable",
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
