declare const __HOTBED_BUILD_NUMBER__: number | undefined;
declare const __HOTBED_GIT_COMMIT__: string | undefined;
declare const __HOTBED_BUILD_SHA256SUMS__: string | undefined;

export const COMPILED_BUILD_NUMBER: number =
	typeof __HOTBED_BUILD_NUMBER__ === "number" &&
	Number.isInteger(__HOTBED_BUILD_NUMBER__) &&
	__HOTBED_BUILD_NUMBER__ > 0
		? __HOTBED_BUILD_NUMBER__
		: 0;

export const COMPILED_GIT_COMMIT: string =
	typeof __HOTBED_GIT_COMMIT__ === "string" ? __HOTBED_GIT_COMMIT__.trim() : "";

export const COMPILED_BUILD_SHA256SUMS: string =
	typeof __HOTBED_BUILD_SHA256SUMS__ === "string" ? __HOTBED_BUILD_SHA256SUMS__ : "";

export function parseSha256Sums(text: string): Map<string, string> {
	const sums = new Map<string, string>();
	for (const line of text.split(/\r?\n/)) {
		const trimmed = line.trim();
		if (!trimmed) continue;
		const parts = trimmed.split(/\s+/);
		if (parts.length >= 2) {
			sums.set(parts[1].replace(/^\*/, ""), parts[0].toLowerCase());
		}
	}
	return sums;
}

export function formatControllerVersion(buildNumber: number, commit: string): string {
	const label = buildNumber > 0 ? `build ${buildNumber}` : "dev";
	const shortCommit = commit.trim().slice(0, 12);
	return shortCommit ? `${label} (${shortCommit})` : `${label} (cloudflare-do)`;
}

/** Extracts build number from version string like "build 42 (0123456789ab)"; returns 0 for dev. */
export function parseBuildNumber(version: string): number {
	const m = /^build\s+(\d+)\b/.exec(version.trim());
	if (!m) return 0;
	const n = Number(m[1]);
	return Number.isSafeInteger(n) && n > 0 ? n : 0;
}
