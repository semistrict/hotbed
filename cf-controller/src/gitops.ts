import { ErrorCode, hotbedError } from "./errors.ts";

/**
 * A `scheme://authority/path` URL as Go's url.Parse reads it, for the parts
 * of it that repo URLs are judged and named by. The standard URL parser
 * (WHATWG's) normalizes what it is given: it punycodes and percent-decodes a
 * host, turns a backslash into a slash, resolves `.` and `..` in a path, and
 * rewrites numeric hosts as IPv4 addresses. Go's does none of that, and the
 * worker judges the URL the user wrote, so the controller must too.
 */
interface ParsedURL {
	/** The user of the userinfo, as written (a percent-escape undecoded), "" if none. */
	username: string;
	/** The authority after its userinfo: its host (in brackets for IPv6) and port. */
	hostPort: string;
	/** The host, without brackets or port. */
	host: string;
	/** The port's digits, "" if none. */
	port: string;
	/** The path, percent-decoded, as Go's URL.Path. */
	path: string;
}

/** What Go's url.Parse accepts in a userinfo. */
const USERINFO = /^[A-Za-z0-9\-._:~!$&'()*+,;=%@]*$/;
/** What Go's url.Parse accepts in a host that is not an IPv6 literal: no ASCII but these. */
const URL_HOST = /^(?:[A-Za-z0-9\-._~!$&'()*+,;=:[\]<>"%]|\P{ASCII})*$/u;
/** A percent sign that is not the start of an escape, which Go refuses. */
const BAD_ESCAPE = /%(?![0-9A-Fa-f]{2})/;

function validOptionalPort(colonPort: string): boolean {
	return colonPort === "" || /^:\d*$/.test(colonPort);
}

/** The percent-escapes of s decoded, those that are not UTF-8 left as written. */
function unescapePath(s: string): string {
	return s.replace(/(?:%[0-9A-Fa-f]{2})+/g, (m) => {
		try {
			return decodeURIComponent(m);
		} catch {
			return m;
		}
	});
}

/** Whether s has a byte Go's url.Parse refuses outright: a C0 control or DEL. */
function hasControlByte(s: string): boolean {
	for (let i = 0; i < s.length; i++) {
		const code = s.charCodeAt(i);
		if (code < 0x20 || code === 0x7f) return true;
	}
	return false;
}

/** The URL s is, or null where Go's url.Parse refuses it. */
function parseURL(s: string): ParsedURL | null {
	const schemeEnd = s.indexOf("://");
	if (schemeEnd < 0 || !/^[A-Za-z][A-Za-z0-9+.-]*$/.test(s.slice(0, schemeEnd))) return null;
	if (hasControlByte(s)) return null;
	let rest = s.slice(schemeEnd + 3);
	const hash = rest.indexOf("#");
	const fragment = hash < 0 ? "" : rest.slice(hash + 1);
	if (hash >= 0) rest = rest.slice(0, hash);
	const query = rest.indexOf("?");
	if (query >= 0) rest = rest.slice(0, query);
	const slash = rest.indexOf("/");
	const authority = slash < 0 ? rest : rest.slice(0, slash);
	const rawPath = slash < 0 ? "" : rest.slice(slash);

	const at = authority.lastIndexOf("@");
	const userinfo = at < 0 ? "" : authority.slice(0, at);
	if (!USERINFO.test(userinfo)) return null;
	const hostPort = authority.slice(at + 1);
	if (BAD_ESCAPE.test(userinfo + rawPath + fragment)) return null;

	let host = hostPort;
	let colonPort = "";
	if (hostPort.startsWith("[")) {
		const close = hostPort.lastIndexOf("]");
		if (close < 0) return null;
		host = hostPort.slice(1, close);
		colonPort = hostPort.slice(close + 1);
		if (!host.includes(":")) return null; // brackets hold an IPv6 address
	} else {
		const colon = hostPort.lastIndexOf(":");
		if (colon >= 0) {
			host = hostPort.slice(0, colon);
			colonPort = hostPort.slice(colon);
		}
		if (host.includes(":") || !URL_HOST.test(host)) return null; // a bare host has no colon
	}
	if (!validOptionalPort(colonPort)) return null;
	return {
		username: userinfo.split(":", 1)[0],
		hostPort,
		host,
		port: colonPort.slice(1),
		path: unescapePath(rawPath),
	};
}

/** An absolute path as Go's filepath.Clean writes it. */
function cleanAbsolute(p: string): string {
	const out: string[] = [];
	for (const seg of p.split("/")) {
		if (seg === "" || seg === ".") continue;
		if (seg === "..") out.pop();
		else out.push(seg);
	}
	return `/${out.join("/")}`;
}

/**
 * The name of a local repo: its path cleaned and without ".git". A relative
 * path the worker makes absolute against its working directory, which the
 * controller has none of, so it is left as written.
 */
function localRemote(p: string): string {
	if (!p.startsWith("/")) return stripDotGit(p.replace(/\/+$/, ""));
	return cleanAbsolute(stripDotGit(cleanAbsolute(p)));
}

const trimSlashes = (s: string) => s.replace(/^\/+|\/+$/g, "");

/**
 * Canonicalizes HTTPS, SSH, SCP-style, and local git URLs so
 * "git@github.com:semistrict/hotbed.git" and "https://github.com/semistrict/hotbed"
 * compare equal. Matches Go's internal/gitops.CanonicalRemote.
 */
export function canonicalRemote(raw: string): string {
	const s = raw.replace(/^\p{White_Space}+|\p{White_Space}+$/gu, "");
	if (!s) return "";
	if (s.startsWith("/") || s.startsWith(".")) return localRemote(s);
	if (s.startsWith("file://")) {
		const u = parseURL(s);
		if (u) return localRemote(u.path);
	}
	if (!s.includes("://")) {
		const colonIdx = s.indexOf(":");
		if (colonIdx >= 0) {
			let hostPart = s.slice(0, colonIdx);
			const pathPart = s.slice(colonIdx + 1);
			if (!hostPart.includes("/")) {
				const atIdx = hostPart.indexOf("@");
				if (atIdx >= 0) {
					hostPart = hostPart.slice(atIdx + 1);
				}
				return `${hostPart.toLowerCase()}/${stripDotGit(trimSlashes(pathPart))}`;
			}
		}
	}
	const u = parseURL(s);
	if (!u || !u.host) return stripDotGit(s.replace(/\/+$/, ""));
	return `${u.host.toLowerCase()}/${stripDotGit(trimSlashes(u.path))}`;
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

function validateStandardRepoURL(raw: string): void {
	const u = parseURL(raw);
	if (!u) {
		throw hotbedError(ErrorCode.InvalidArgs, `invalid repo URL "${raw}"`);
	}
	if (/^(?:-|%2d)/i.test(u.username)) {
		throw hotbedError(ErrorCode.InvalidArgs, `repo URL "${raw}" username must not start with '-'`);
	}
	if (!u.host) {
		throw hotbedError(ErrorCode.InvalidArgs, `repo URL "${raw}" is missing a host`);
	}
	if (u.host.startsWith("-")) {
		throw hotbedError(ErrorCode.InvalidArgs, `repo URL "${raw}" host must not start with '-'`);
	}
	if (!validHost(u.host)) {
		throw hotbedError(ErrorCode.InvalidArgs, `repo URL "${raw}" has invalid host "${u.host}"`);
	}
	if (u.port) {
		const port = Number(u.port);
		if (!(port >= 1 && port <= 65535)) {
			throw hotbedError(ErrorCode.InvalidArgs, `repo URL "${raw}" has invalid port "${u.port}"`);
		}
	}
	if (!trimSlashes(u.path)) {
		throw hotbedError(ErrorCode.InvalidArgs, `repo URL "${raw}" is missing a repository path`);
	}
}

function validateFileRepoURL(raw: string): void {
	const u = parseURL(raw);
	if (!u) {
		throw hotbedError(ErrorCode.InvalidArgs, `invalid file repo URL "${raw}"`);
	}
	if (u.hostPort && u.hostPort.toLowerCase() !== "localhost") {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`file repo URL "${raw}" must not specify remote host "${u.hostPort}"`,
		);
	}
	if (!u.path.startsWith("/") || u.path.length <= 1) {
		throw hotbedError(ErrorCode.InvalidArgs, `file repo URL "${raw}" must have an absolute path`);
	}
}

/**
 * Validates a remote repository URL (matching Go's gitops.ValidateRepoURL).
 */
export function validateRepoURL(raw: string, allowLocal = false): void {
	if (!raw) {
		throw hotbedError(ErrorCode.InvalidArgs, "repo URL must not be empty");
	}
	if (!raw.isWellFormed()) {
		throw hotbedError(ErrorCode.InvalidArgs, "repo URL contains invalid UTF-8");
	}
	// Go's unicode.IsControl and unicode.IsSpace: the C0 and C1 controls, and
	// every space, which the URL parser would strip, drop or encode.
	if (/[\p{Cc}\p{White_Space}]/u.test(raw)) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			"repo URL must not contain whitespace or control characters",
		);
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
			validateStandardRepoURL(raw);
			return;
		}
		if (scheme === "file") {
			if (!allowLocal) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`repo URL "${raw}" uses disallowed file:// scheme`,
				);
			}
			validateFileRepoURL(raw);
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
