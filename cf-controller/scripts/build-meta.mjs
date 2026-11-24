import { execSync } from "node:child_process";

/** @returns {number} */
export function readBuildNumber() {
	const raw = (process.env.HOTBED_BUILD_NUMBER ?? "").trim();
	if (!/^\d+$/.test(raw)) return 0;
	const n = Number(raw);
	return Number.isSafeInteger(n) && n > 0 ? n : 0;
}

/** @returns {string} */
export function readGitCommit() {
	const envCommit = (process.env.HOTBED_GIT_COMMIT ?? "").trim();
	if (envCommit) return envCommit;
	try {
		return execSync("git rev-parse HEAD", {
			encoding: "utf8",
			stdio: ["ignore", "pipe", "ignore"],
		}).trim();
	} catch {
		return "";
	}
}

/** @returns {string} */
export function readBuildSha256Sums() {
	return process.env.HOTBED_BUILD_SHA256SUMS ?? "";
}
