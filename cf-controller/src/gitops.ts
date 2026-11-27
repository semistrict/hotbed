import { ErrorCode, hotbedError } from "./errors.ts";

/**
 * Canonicalizes HTTPS, SSH, SCP-style, and local git URLs so
 * "git@github.com:semistrict/hotbed.git" and "https://github.com/semistrict/hotbed"
 * compare equal. Matches Go's internal/gitops.CanonicalRemote.
 */
export function canonicalRemote(raw: string): string {
	const s = raw.trim();
	if (!s) return "";
	if (s.startsWith("/") || s.startsWith(".")) {
		return stripDotGit(s.replace(/\/+$/, ""));
	}
	if (s.startsWith("file://")) {
		try {
			const u = new URL(s);
			return stripDotGit(u.pathname.replace(/\/+$/, ""));
		} catch {
			return stripDotGit(s.replace(/\/+$/, ""));
		}
	}
	if (!s.includes("://")) {
		const colonIdx = s.indexOf(":");
		if (colonIdx > 0) {
			let hostPart = s.slice(0, colonIdx);
			let pathPart = s.slice(colonIdx + 1);
			if (!hostPart.includes("/")) {
				const atIdx = hostPart.indexOf("@");
				if (atIdx >= 0) {
					hostPart = hostPart.slice(atIdx + 1);
				}
				pathPart = stripDotGit(pathPart.replace(/^\/+|\/+$/g, ""));
				return `${hostPart.toLowerCase()}/${pathPart}`;
			}
		}
	}
	try {
		const u = new URL(s);
		if (!u.hostname) {
			return stripDotGit(s.replace(/\/+$/, ""));
		}
		const p = stripDotGit(u.pathname.replace(/^\/+|\/+$/g, ""));
		return `${u.hostname.toLowerCase()}/${p}`;
	} catch {
		return stripDotGit(s.replace(/\/+$/, ""));
	}
}

function stripDotGit(s: string): string {
	return s.endsWith(".git") ? s.slice(0, -4) : s;
}

const DISALLOWED_SCP_SCHEMES = new Set([
	"file",
	"ext",
	"fd",
	"remote-ext",
	"http",
	"https",
	"git",
	"ftp",
	"ftps",
	"ssh",
]);

/** Whether s is an IP address with no zone, as Go's netip.ParseAddr reads one. */
function isIPLiteral(s: string): boolean {
	const v4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(s);
	if (v4) {
		return v4.slice(1).every((p) => (p === "0" || !p.startsWith("0")) && Number(p) <= 255);
	}
	// The URL parser's IPv6 grammar refuses ":", "::::", "1::2::3" and zones;
	// the character check keeps anything but an address out of the brackets.
	if (!s.includes(":") || !/^[0-9a-fA-F:.]+$/.test(s)) return false;
	try {
		new URL(`http://[${s}]/`);
		return true;
	} catch {
		return false;
	}
}

function validHost(host: string): boolean {
	if (
		!host ||
		host.startsWith("-") ||
		host.endsWith("-") ||
		host.startsWith(".") ||
		host.endsWith(".") ||
		host.includes("..")
	) {
		return false;
	}
	return isIPLiteral(host) || /^[a-zA-Z0-9.-]+$/.test(host);
}

/**
 * Validates a remote repository URL (matching Go's gitops.ValidateRepoURL).
 */
export function validateRepoURL(raw: string, allowLocal = false): void {
	if (!raw) {
		throw hotbedError(ErrorCode.InvalidArgs, "repo URL must not be empty");
	}
	for (let i = 0; i < raw.length; i++) {
		const code = raw.charCodeAt(i);
		if (code === 0 || code < 0x20 || code === 0x7f || /\s/.test(raw[i])) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				"repo URL must not contain whitespace or control characters",
			);
		}
	}
	if (raw.startsWith("-")) {
		throw hotbedError(ErrorCode.InvalidArgs, `repo URL "${raw}" must not start with '-'`);
	}
	const lower = raw.toLowerCase();
	for (const helper of ["ext::", "fd::", "remote-ext::"]) {
		if (lower.startsWith(helper)) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`repo URL "${raw}" uses disallowed git remote helper`,
			);
		}
	}
	const schemeIdx = raw.indexOf("://");
	if (schemeIdx >= 0) {
		const scheme = raw.slice(0, schemeIdx).toLowerCase();
		if (scheme === "https" || scheme === "http" || scheme === "ssh") {
			// The URL parser reads "https:///org/repo" as host "org" and a
			// backslash as a slash; Go's url.Parse, the worker's, does neither.
			const authority = raw.slice(schemeIdx + 3).split(/[/?#]/)[0];
			if (!authority || authority.includes("\\")) {
				throw hotbedError(ErrorCode.InvalidArgs, `repo URL "${raw}" is missing a host`);
			}
			let u: URL;
			try {
				u = new URL(raw);
			} catch {
				throw hotbedError(ErrorCode.InvalidArgs, `invalid repo URL "${raw}"`);
			}
			if (u.username.startsWith("-")) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`repo URL "${raw}" username must not start with '-'`,
				);
			}
			const host = u.hostname.replace(/^\[|\]$/g, "");
			if (!u.host || !host || host.startsWith("-") || !validHost(host)) {
				throw hotbedError(ErrorCode.InvalidArgs, `repo URL "${raw}" has invalid host "${host}"`);
			}
			if (u.port) {
				const port = Number(u.port);
				if (!Number.isInteger(port) || port < 1 || port > 65535) {
					throw hotbedError(
						ErrorCode.InvalidArgs,
						`repo URL "${raw}" has invalid port "${u.port}"`,
					);
				}
			}
			if (!u.pathname.replace(/^\/+|\/+$/g, "")) {
				throw hotbedError(ErrorCode.InvalidArgs, `repo URL "${raw}" is missing a repository path`);
			}
			return;
		}
		if (scheme === "file") {
			if (!allowLocal) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`repo URL "${raw}" uses disallowed file:// scheme`,
				);
			}
			let u: URL;
			try {
				u = new URL(raw);
			} catch {
				throw hotbedError(ErrorCode.InvalidArgs, `invalid file repo URL "${raw}"`);
			}
			if (u.host && u.host.toLowerCase() !== "localhost") {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`file repo URL "${raw}" must not specify remote host "${u.host}"`,
				);
			}
			if (!u.pathname.startsWith("/") || u.pathname.length <= 1) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`file repo URL "${raw}" must have an absolute path`,
				);
			}
			return;
		}
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`repo URL "${raw}" uses unsupported scheme "${scheme}" (allowed: https, http, ssh, or SCP-style [user@]host:path)`,
		);
	}
	if (raw.startsWith("/")) {
		if (!allowLocal) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`repo URL "${raw}" is a local path, which is not allowed`,
			);
		}
		if (raw.length <= 1) {
			throw hotbedError(ErrorCode.InvalidArgs, `repo URL "${raw}" is not a valid local repo path`);
		}
		return;
	}
	if (raw.startsWith(".")) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`repo URL "${raw}" is a relative local path, which is not allowed`,
		);
	}
	// SCP-style [user@]host:path
	let rest = raw;
	let hasUser = false;
	const colonIdx = raw.indexOf(":");
	const bracketIdx = raw.indexOf("[");
	const atIdx = raw.indexOf("@");
	if (atIdx >= 0 && (colonIdx < 0 || atIdx < colonIdx) && (bracketIdx < 0 || atIdx < bracketIdx)) {
		hasUser = true;
		const user = raw.slice(0, atIdx);
		rest = raw.slice(atIdx + 1);
		if (!user || !rest) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`repo URL "${raw}" must use https://, http://, ssh://, or SCP-style [user@]host:path`,
			);
		}
		if (user.startsWith("-") || !/^[a-zA-Z0-9._+-]+$/.test(user)) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`repo URL "${raw}" has invalid SSH username "${user}"`,
			);
		}
	}
	let host = "";
	let pathPart = "";
	if (rest.startsWith("[")) {
		const closeIdx = rest.indexOf("]:");
		if (closeIdx < 0) {
			throw hotbedError(ErrorCode.InvalidArgs, `repo URL "${raw}" has invalid bracketed host`);
		}
		host = rest.slice(1, closeIdx);
		if (!isIPLiteral(host)) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`repo URL "${raw}" has invalid IP literal "${host}"`,
			);
		}
		pathPart = rest.slice(closeIdx + 2);
	} else {
		const c = rest.indexOf(":");
		if (c < 0) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`repo URL "${raw}" must use https://, http://, ssh://, or SCP-style [user@]host:path`,
			);
		}
		host = rest.slice(0, c);
		pathPart = rest.slice(c + 1);
		if (pathPart.startsWith(":")) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`repo URL "${raw}" uses disallowed git remote helper syntax`,
			);
		}
		if (!validHost(host)) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`repo URL "${raw}" must use https://, http://, ssh://, or SCP-style [user@]host:path`,
			);
		}
		if (!hasUser) {
			if (host.length === 1 && /[a-zA-Z]/.test(host)) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`repo URL "${raw}" looks like a drive-letter path, which is not allowed`,
				);
			}
			if (DISALLOWED_SCP_SCHEMES.has(host.toLowerCase())) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`repo URL "${raw}" uses disallowed scheme prefix "${host}"`,
				);
			}
		}
	}
	if (pathPart.startsWith(":") || !pathPart.replace(/^\/+|\/+$/g, "") || pathPart.startsWith("-")) {
		throw hotbedError(ErrorCode.InvalidArgs, `repo URL "${raw}" has an invalid repository path`);
	}
}

export function parseGitRepo(raw: string): { host: string; owner: string; repo: string } {
	const s = raw.trim();
	if (!s) {
		throw hotbedError(ErrorCode.InvalidArgs, "repository URL or remote is required");
	}
	if (s.startsWith("/") || s.startsWith(".") || s.startsWith("file://")) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`"${raw}" is a local path, not a remote git repository`,
		);
	}
	let host = "";
	let pathPart = "";
	if (s.includes("://")) {
		let u: URL;
		try {
			u = new URL(s);
		} catch {
			throw hotbedError(ErrorCode.InvalidArgs, `unreadable git repository URL "${raw}"`);
		}
		if (!u.host) {
			throw hotbedError(ErrorCode.InvalidArgs, `unreadable git repository URL "${raw}"`);
		}
		host = u.host.toLowerCase();
		const scheme = u.protocol.replace(":", "").toLowerCase();
		if (scheme === "https") host = host.replace(/:443$/, "");
		else if (scheme === "http") host = host.replace(/:80$/, "");
		else if (scheme === "ssh") host = host.replace(/:22$/, "");
		pathPart = u.pathname;
	} else {
		const scp = splitSCP(s);
		if (scp) {
			host = scp.host.toLowerCase();
			pathPart = scp.path;
		} else {
			const slash = s.indexOf("/");
			if (slash <= 0 || slash === s.length - 1) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`repository "${raw}" must be a URL or host/owner/repo`,
				);
			}
			host = s.slice(0, slash).toLowerCase();
			pathPart = s.slice(slash + 1);
		}
	}
	pathPart = stripDotGit(pathPart.replace(/^\/+|\/+$/g, ""));
	const parts = pathPart.split("/");
	if (parts.length < 2 || !parts[0] || !parts[1]) {
		throw hotbedError(ErrorCode.InvalidArgs, `repository "${raw}" must include owner/repo`);
	}
	for (const seg of parts) {
		if (!seg || seg === "." || seg === "..") {
			throw hotbedError(ErrorCode.InvalidArgs, `repository "${raw}" has an invalid path`);
		}
	}
	return { host, owner: parts[0], repo: parts.slice(1).join("/") };
}

function splitSCP(s: string): { host: string; path: string } | null {
	const colon = s.indexOf(":");
	if (colon <= 0) return null;
	let before = s.slice(0, colon);
	const after = s.slice(colon + 1);
	if (before.includes("/") || !after) return null;
	const slash = after.indexOf("/");
	if (slash > 0 && /^\d+$/.test(after.slice(0, slash))) return null;
	const at = before.indexOf("@");
	if (at >= 0) before = before.slice(at + 1);
	if (!before) return null;
	return { host: before, path: after };
}

export function projectNameFor(remote: string): string {
	const lastSlash = remote.lastIndexOf("/");
	const tail = (lastSlash >= 0 ? remote.slice(lastSlash + 1) : remote).toLowerCase();
	let name = tail.replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");
	if (name.length > 40) {
		name = name.slice(0, 40).replace(/-+$/, "");
	}
	return name;
}
