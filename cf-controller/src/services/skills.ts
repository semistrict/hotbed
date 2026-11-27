import type { ServiceImpl } from "@connectrpc/connect";
import { create } from "@bufbuild/protobuf";
import {
	GetSkillResponseSchema,
	ListSkillsResponseSchema,
	SkillFileMetaSchema,
	SkillProvenanceSchema,
	SkillScope,
	SkillService,
	SkillState,
	SkillStateSchema,
	type Project,
	type Skill,
	type SkillFile,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import { getCaller } from "../connect-adapter.ts";
import { inProject, type ControllerContext } from "../context.ts";
import { enumWord } from "../enums.ts";
import { ErrorCode, hotbedError } from "../errors.ts";
import { canonicalRemote } from "../gitops.ts";
import { listedProject } from "../project-scope.ts";
import type { StoredSkillFileInput } from "../store/skills.ts";
import { checkRevision, projectRepo } from "./project-items.ts";

// SEP-2640 interoperability baseline limits.
const MAX_FILES_PER_SKILL = 512;
const MAX_TOTAL_SKILL_BYTES = 16 * 1024 * 1024;
const MAX_DESCRIPTION_RUNES = 1024;
const MAX_COMPATIBILITY_RUNES = 500;
const MAX_TAGS = 10;
const DIRTY_KEPT = 20;
const TAG_RE = /^[a-z0-9][a-z0-9-]{0,31}$/;

/** Agent Skills & SEP-2640 skill name: 1..64 lowercase Unicode letters/digits/non-consecutive hyphens. */
export function isValidSkillName(name: string): boolean {
	const runes = Array.from(name);
	if (runes.length < 1 || runes.length > 64) return false;
	if (name !== name.toLowerCase()) return false;
	if (name.startsWith("-") || name.endsWith("-") || name.includes("--")) return false;
	for (const ch of runes) {
		if (ch !== "-" && !/[\p{L}\p{N}]/u.test(ch)) return false;
	}
	return true;
}

function normalizeTags(raw: string[]): string[] {
	const out: string[] = [];
	const seen = new Set<string>();
	for (const t of raw) {
		const clean = t.trim().toLowerCase();
		if (!clean) continue;
		if (!TAG_RE.test(clean)) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`tag "${t}" must be lowercase letters, digits and dashes (at most 32 characters)`,
			);
		}
		if (!seen.has(clean)) {
			seen.add(clean);
			out.push(clean);
		}
	}
	if (out.length > MAX_TAGS) {
		throw hotbedError(ErrorCode.InvalidArgs, `too many tags (${out.length}); at most ${MAX_TAGS}`);
	}
	return out;
}

function normalizeRelativeFilePath(raw: string): string {
	const p = raw.replace(/\\/g, "/");
	if (!p || p.startsWith("/") || p.endsWith("/") || p.includes("\0") || /^[A-Za-z]:/.test(p)) {
		throw hotbedError(ErrorCode.InvalidArgs, `invalid skill file path "${raw}"`);
	}
	const parts = p.split("/");
	for (const part of parts) {
		if (!part || part === "." || part === "..") {
			throw hotbedError(ErrorCode.InvalidArgs, `invalid skill file path "${raw}"`);
		}
	}
	return parts.join("/");
}

function isBinaryBytes(bytes: Uint8Array): boolean {
	for (let i = 0; i < bytes.byteLength; i++) {
		if (bytes[i] === 0) return true;
	}
	try {
		new TextDecoder("utf-8", { fatal: true, ignoreBOM: false }).decode(bytes);
		return false;
	} catch {
		return true;
	}
}

function inferMimeType(path: string, binary: boolean): string {
	const lower = path.toLowerCase();
	if (lower.endsWith(".md") || lower.endsWith(".markdown") || lower.endsWith(".mdx")) {
		return "text/markdown";
	}
	if (lower.endsWith(".json")) return "application/json";
	if (lower.endsWith(".yaml") || lower.endsWith(".yml")) return "application/yaml";
	if (lower.endsWith(".toml")) return "application/toml";
	if (lower.endsWith(".ts") || lower.endsWith(".tsx")) return "text/typescript";
	if (lower.endsWith(".js") || lower.endsWith(".mjs") || lower.endsWith(".cjs")) {
		return "text/javascript";
	}
	if (lower.endsWith(".py")) return "text/x-python";
	if (lower.endsWith(".sh") || lower.endsWith(".bash") || lower.endsWith(".zsh")) {
		return "text/x-shellscript";
	}
	if (lower.endsWith(".go")) return "text/x-go";
	if (lower.endsWith(".rs")) return "text/x-rust";
	if (lower.endsWith(".html") || lower.endsWith(".htm")) return "text/html";
	if (lower.endsWith(".css")) return "text/css";
	if (lower.endsWith(".xml")) return "application/xml";
	if (lower.endsWith(".svg")) return "image/svg+xml";
	if (lower.endsWith(".png")) return "image/png";
	if (lower.endsWith(".jpg") || lower.endsWith(".jpeg")) return "image/jpeg";
	if (lower.endsWith(".webp")) return "image/webp";
	if (lower.endsWith(".gif")) return "image/gif";
	if (lower.endsWith(".pdf")) return "application/pdf";
	if (lower.endsWith(".txt")) return "text/plain";
	return binary ? "application/octet-stream" : "text/plain";
}

async function sha256Digest(bytes: Uint8Array): Promise<string> {
	const buf = await crypto.subtle.digest("SHA-256", bytes);
	const hex = Array.from(new Uint8Array(buf), (b) => b.toString(16).padStart(2, "0")).join("");
	return `sha256:${hex}`;
}

/**
 * Lightweight fallback YAML frontmatter parser for SKILL.md when the caller
 * did not supply pre-parsed verbatim `frontmatter_json`. Supports standard
 * Agent Skills frontmatter (scalars, quoted strings, folded/literal blocks,
 * and nested string maps like `metadata:`).
 */
function parseSimpleFrontmatter(raw: string): Record<string, unknown> {
	const normalized = raw.replace(/\r\n/g, "\n");
	if (!normalized.startsWith("---\n")) {
		throw hotbedError(ErrorCode.InvalidArgs, "SKILL.md must begin with YAML frontmatter (---)");
	}
	let end = normalized.indexOf("\n---\n", 4);
	if (end < 0 && normalized.endsWith("\n---")) {
		end = normalized.length - 4;
	}
	if (end < 0) {
		throw hotbedError(ErrorCode.InvalidArgs, "SKILL.md frontmatter is missing closing ---");
	}
	const block = normalized.slice(4, end);
	const lines = block.split("\n");
	const out: Record<string, unknown> = {};
	let i = 0;

	const unquote = (val: string): string => {
		const trimmed = val.trim();
		if (
			(trimmed.startsWith('"') && trimmed.endsWith('"') && trimmed.length >= 2) ||
			(trimmed.startsWith("'") && trimmed.endsWith("'") && trimmed.length >= 2)
		) {
			if (trimmed.startsWith('"')) {
				try {
					return JSON.parse(trimmed) as string;
				} catch {
					return trimmed.slice(1, -1);
				}
			}
			return trimmed.slice(1, -1).replace(/''/g, "'");
		}
		return trimmed;
	};

	while (i < lines.length) {
		const line = lines[i];
		if (!line.trim() || line.trim().startsWith("#")) {
			i++;
			continue;
		}
		const match = /^([A-Za-z0-9_.-]+)\s*:\s*(.*)$/.exec(line);
		if (!match) {
			i++;
			continue;
		}
		const key = match[1];
		const rest = match[2].trim();
		i++;
		if (rest === "|" || rest === ">" || rest === "|-" || rest === ">-") {
			const blockLines: string[] = [];
			let indent = -1;
			while (i < lines.length) {
				const l = lines[i];
				if (l.trim() === "") {
					blockLines.push("");
					i++;
					continue;
				}
				const sp = /^(\s+)/.exec(l)?.[1].length ?? 0;
				if (sp === 0) break;
				if (indent < 0) indent = sp;
				blockLines.push(l.slice(Math.min(indent, sp)));
				i++;
			}
			const joined = rest.startsWith(">")
				? blockLines.join(" ").replace(/\s+/g, " ").trim()
				: blockLines.join("\n").replace(/\n+$/, "");
			out[key] = joined;
		} else if (rest === "") {
			const nested: Record<string, string> = {};
			let hasNested = false;
			while (i < lines.length) {
				const l = lines[i];
				if (!l.trim() || l.trim().startsWith("#")) {
					i++;
					continue;
				}
				if (!/^\s+/.test(l)) break;
				const sub = /^\s+([A-Za-z0-9_./-]+)\s*:\s*(.*)$/.exec(l);
				if (sub) {
					nested[sub[1]] = unquote(sub[2]);
					hasNested = true;
				}
				i++;
			}
			if (hasNested) {
				out[key] = nested;
			} else {
				out[key] = "";
			}
		} else {
			out[key] = unquote(rest);
		}
	}
	return out;
}

function validateFrontmatter(
	skillMdBytes: Uint8Array,
	suppliedJson: string,
	explicitName: string,
): { name: string; description: string; frontmatterJson: string } {
	let text: string;
	try {
		text = new TextDecoder("utf-8", { fatal: true, ignoreBOM: false }).decode(skillMdBytes);
	} catch {
		throw hotbedError(ErrorCode.InvalidArgs, "SKILL.md must be valid UTF-8 text");
	}

	const parsedFromMd = parseSimpleFrontmatter(text);
	let fm: Record<string, unknown>;
	if (suppliedJson.trim()) {
		let parsed: unknown;
		try {
			parsed = JSON.parse(suppliedJson);
		} catch (err) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`invalid frontmatter_json: ${err instanceof Error ? err.message : String(err)}`,
			);
		}
		if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
			throw hotbedError(ErrorCode.InvalidArgs, "frontmatter_json must be a JSON object");
		}
		fm = parsed as Record<string, unknown>;
		// Sanity-check that name and description match SKILL.md when present in both.
		if (
			typeof parsedFromMd.name === "string" &&
			typeof fm.name === "string" &&
			parsedFromMd.name !== fm.name
		) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`frontmatter_json name "${fm.name}" does not match SKILL.md name "${parsedFromMd.name}"`,
			);
		}
	} else {
		fm = parsedFromMd;
	}

	const rawName = typeof fm.name === "string" ? fm.name.trim() : "";
	if (!rawName || !isValidSkillName(rawName)) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`SKILL.md frontmatter name "${rawName}" must be 1–64 lowercase letters, digits, or non-consecutive hyphens`,
		);
	}
	if (explicitName.trim() && explicitName.trim() !== rawName) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`name "${explicitName.trim()}" must match SKILL.md frontmatter name "${rawName}"`,
		);
	}

	const description = typeof fm.description === "string" ? fm.description.trim() : "";
	const descLen = Array.from(description).length;
	if (descLen < 1 || descLen > MAX_DESCRIPTION_RUNES) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`SKILL.md frontmatter description must be 1–${MAX_DESCRIPTION_RUNES} characters (got ${descLen})`,
		);
	}

	if (fm.compatibility !== undefined) {
		if (typeof fm.compatibility !== "string") {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				"SKILL.md frontmatter compatibility must be a string",
			);
		}
		const compatLen = Array.from(fm.compatibility).length;
		if (compatLen < 1 || compatLen > MAX_COMPATIBILITY_RUNES) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`SKILL.md frontmatter compatibility must be 1–${MAX_COMPATIBILITY_RUNES} characters`,
			);
		}
	}
	if (fm.license !== undefined && typeof fm.license !== "string") {
		throw hotbedError(ErrorCode.InvalidArgs, "SKILL.md frontmatter license must be a string");
	}
	if (fm["allowed-tools"] !== undefined && typeof fm["allowed-tools"] !== "string") {
		throw hotbedError(ErrorCode.InvalidArgs, "SKILL.md frontmatter allowed-tools must be a string");
	}
	if (fm.metadata !== undefined) {
		if (!fm.metadata || typeof fm.metadata !== "object" || Array.isArray(fm.metadata)) {
			throw hotbedError(ErrorCode.InvalidArgs, "SKILL.md frontmatter metadata must be an object");
		}
		for (const [k, v] of Object.entries(fm.metadata as Record<string, unknown>)) {
			if (typeof v !== "string") {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`SKILL.md frontmatter metadata.${k} must be a string`,
				);
			}
		}
	}

	return {
		name: rawName,
		description,
		frontmatterJson: JSON.stringify(fm),
	};
}

function checkSkillRevision(s: Skill, want: bigint): void {
	checkRevision(s.revision, want, `skill #${s.id}`, `hotbed skills show ${s.id}`);
}

export function createSkillService(ctrl: ControllerContext): ServiceImpl<typeof SkillService> {
	function resolveOptionalProject(req: {
		project: string;
		sessionId: string;
		remote: string;
	}): string {
		if (req.project) {
			ctrl.store.projects.get(req.project);
			return req.project;
		}
		if (req.sessionId) {
			const sess = ctrl.store.sessions.get(req.sessionId);
			if (sess.project) return sess.project;
		}
		if (req.remote) {
			const remote = canonicalRemote(req.remote);
			if (remote) {
				const holding = ctrl.store.projects.holding(remote);
				if (holding.length === 1) return holding[0].name;
			}
		}
		return "";
	}

	/**
	 * Resolve a skill reference:
	 * - "12" or "#12" -> by ID
	 * - "skill://global/<name>/SKILL.md" or "skill://<project>/<name>/SKILL.md" -> by MCP URI
	 * - "global/<name>" or "<project>/<name>" -> scoped lookup
	 * - "<name>" -> active project first, then global
	 */
	function resolveRef(rawRef: string, activeProject: string): Skill {
		const ref = rawRef.trim();
		if (!ref) {
			throw hotbedError(ErrorCode.InvalidArgs, "skill reference is empty");
		}
		const bareNum = ref.startsWith("#") ? ref.slice(1) : ref;
		if (/^\d+$/.test(bareNum)) {
			return ctrl.store.skills.get(Number(bareNum));
		}
		if (ref.startsWith("skill://")) {
			const rest = ref.slice("skill://".length);
			if (!rest.endsWith("/SKILL.md")) {
				throw hotbedError(ErrorCode.InvalidArgs, `invalid skill URI "${rawRef}"`);
			}
			const pathNoFile = rest.slice(0, -"/SKILL.md".length);
			const slash = pathNoFile.indexOf("/");
			if (slash <= 0) {
				throw hotbedError(ErrorCode.NotFound, `no active skill at "${rawRef}"`);
			}
			const scopePart = pathNoFile.slice(0, slash).toLowerCase();
			const namePart = pathNoFile.slice(slash + 1).toLowerCase();
			const scopeKey = scopePart === "global" ? "" : scopePart;
			if (scopeKey) {
				try {
					ctrl.store.projects.get(scopeKey);
				} catch {
					throw hotbedError(ErrorCode.NotFound, `no active skill at "${rawRef}"`);
				}
			}
			const found = ctrl.store.skills.findActiveByScopeName(scopeKey, namePart);
			if (!found) {
				throw hotbedError(ErrorCode.NotFound, `no active skill at "${rawRef}"`);
			}
			return found;
		}
		const slash = ref.indexOf("/");
		if (slash >= 0) {
			const scopePart = ref.slice(0, slash).trim().toLowerCase();
			const namePart = ref
				.slice(slash + 1)
				.trim()
				.toLowerCase();
			if (!namePart) {
				throw hotbedError(ErrorCode.InvalidArgs, `invalid skill reference "${rawRef}"`);
			}
			const scopeKey = scopePart === "global" ? "" : scopePart;
			if (scopeKey) ctrl.store.projects.get(scopeKey);
			const found = ctrl.store.skills.findActiveByScopeName(scopeKey, namePart);
			if (!found) {
				throw hotbedError(
					ErrorCode.NotFound,
					`no active skill "${namePart}" in ${scopeKey ? `project ${scopeKey}` : "global"}`,
				).withSuggestion("hotbed skills ls");
			}
			return found;
		}
		const name = ref.toLowerCase();
		if (activeProject) {
			const inProj = ctrl.store.skills.findActiveByScopeName(activeProject, name);
			if (inProj) return inProj;
		}
		const inGlobal = ctrl.store.skills.findActiveByScopeName("", name);
		if (inGlobal) return inGlobal;

		const others = ctrl.store.skills.findActiveByNameAnyScope(name);
		if (others.length > 0) {
			const hints = others.map((s) => `${s.project || "global"}/${s.name}`).join(", ");
			throw hotbedError(
				ErrorCode.NotFound,
				`no active skill "${name}" in ${activeProject ? `project ${activeProject} or global` : "global"}`,
			).withSuggestion(`found in another scope: ${hints}`);
		}
		throw hotbedError(ErrorCode.NotFound, `no active skill "${rawRef}"`).withSuggestion(
			"hotbed skills ls",
		);
	}

	return {
		async putSkill(req, ctx) {
			const caller = getCaller(ctx.values);
			if (req.global && req.project) {
				throw hotbedError(ErrorCode.InvalidArgs, "give either --global or --project, not both");
			}
			if (req.global && req.repo) {
				throw hotbedError(ErrorCode.InvalidArgs, "a global skill cannot be scoped to a repo");
			}
			if (req.files.length === 0) {
				throw hotbedError(ErrorCode.InvalidArgs, "skill must include at least SKILL.md");
			}
			if (req.files.length > MAX_FILES_PER_SKILL) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`skill has ${req.files.length} files (max ${MAX_FILES_PER_SKILL})`,
				);
			}

			const rationale = req.rationale.trim();
			const { by, project: sessProject } = ctrl.userStamp(caller, req.sessionId, rationale);

			let scope = SkillScope.PROJECT;
			let targetProject: Project | null = null;
			let repo = "";

			if (req.global) {
				scope = SkillScope.GLOBAL;
			} else if (req.project) {
				targetProject = ctrl.store.projects.get(req.project);
				repo = projectRepo(targetProject, req.repo, req.from?.remote ?? "");
			} else if (sessProject) {
				targetProject = sessProject;
				repo = projectRepo(targetProject, req.repo, req.from?.remote ?? "");
			} else {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					"specify --global or -p PROJECT when adding a skill outside a session",
				).withSuggestion("pass --global, -p PROJECT, or -s SESSION");
			}

			// Validate files, enforce size/uniqueness limits, and compute SHA-256 digests before DB tx.
			const seenPaths = new Set<string>();
			let totalBytes = 0;
			let skillMdBytes: Uint8Array | null = null;
			const prepared: StoredSkillFileInput[] = [];

			const sortedInputs = [...req.files].sort((a, b) => a.path.localeCompare(b.path));
			for (const f of sortedInputs) {
				const path = normalizeRelativeFilePath(f.path);
				if (seenPaths.has(path)) {
					throw hotbedError(ErrorCode.InvalidArgs, `duplicate skill file path "${path}"`);
				}
				seenPaths.add(path);
				const bytes = f.content;
				totalBytes += bytes.byteLength;
				if (totalBytes > MAX_TOTAL_SKILL_BYTES) {
					throw hotbedError(
						ErrorCode.InvalidArgs,
						`skill total size exceeds limit of ${MAX_TOTAL_SKILL_BYTES} bytes (16 MiB)`,
					);
				}
				if (path === "SKILL.md") {
					skillMdBytes = bytes;
				}
				const binary = isBinaryBytes(bytes);
				const mimeType = f.mimeType.trim() || inferMimeType(path, binary);
				const digest = await sha256Digest(bytes);
				prepared.push({
					meta: create(SkillFileMetaSchema, {
						path,
						size: BigInt(bytes.byteLength),
						digest,
						mimeType,
						binary,
					}),
					content: bytes,
				});
			}

			if (!skillMdBytes) {
				throw hotbedError(ErrorCode.InvalidArgs, "skill package is missing required SKILL.md");
			}

			const { name, description, frontmatterJson } = validateFrontmatter(
				skillMdBytes,
				req.frontmatterJson,
				req.name,
			);
			const tags = normalizeTags(req.tags);

			const from = req.from;
			if (from && from.dirty.length > DIRTY_KEPT) {
				from.dirty = from.dirty.slice(0, DIRTY_KEPT);
			}
			let t3Thread = req.t3codeThreadId.trim();
			if (!t3Thread && req.sessionId) {
				const sess = ctrl.store.sessions.tryGet(req.sessionId);
				if (sess?.t3codeThreadId) t3Thread = sess.t3codeThreadId;
			}
			const provenance = create(SkillProvenanceSchema, {
				from,
				t3codeThreadId: t3Thread,
				model: req.model.trim(),
				rationale,
			});

			return ctrl.store.skills.put({
				scope,
				project: targetProject?.name ?? "",
				repo,
				name,
				description,
				frontmatterJson,
				files: prepared,
				tags,
				source: req.source,
				provenance,
				ifRevision: req.ifRevision,
				by,
			});
		},

		listSkills(req) {
			if (req.limit < 0) {
				throw hotbedError(ErrorCode.InvalidArgs, `limit ${req.limit}: give 0 for all, or how many`);
			}
			const explicitProject = req.projectScope.case === "project" ? req.projectScope.value : "";
			const allProjects = req.projectScope.case === "allProjects" && req.projectScope.value;
			if (req.scope === SkillScope.GLOBAL && explicitProject) {
				throw hotbedError(ErrorCode.InvalidArgs, "cannot combine --scope global with --project");
			}
			if (req.scope === SkillScope.BUILTIN) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					"built-in skills are compiled into the hotbed CLI, not stored on the controller",
				).withSuggestion("hotbed skills ls lists them, and hotbed skills get NAME reads one");
			}
			let project = "";
			if (req.scope === SkillScope.GLOBAL) {
				if (req.sessionId) ctrl.store.sessions.get(req.sessionId);
			} else {
				project = listedProject(ctrl, {
					scope: req.projectScope,
					sessionId: req.sessionId,
					remote: req.remote,
				});
			}
			const { skills, total } = ctrl.store.skills.list({
				states: req.states,
				scope: req.scope,
				project,
				allProjects,
				tag: req.tag,
				query: req.query,
				order: req.order,
				limit: req.limit,
			});
			return create(ListSkillsResponseSchema, { skills, total });
		},

		getSkill(req) {
			const activeProject = resolveOptionalProject(req);
			const skill = resolveRef(req.ref, activeProject);
			let files: SkillFile[];
			if (req.allFiles) {
				files = ctrl.store.skills.readAllFiles(skill.id);
			} else {
				const targetPath = req.path.trim()
					? normalizeRelativeFilePath(req.path.trim())
					: "SKILL.md";
				const f = ctrl.store.skills.readFile(skill.id, targetPath);
				if (!f) {
					throw hotbedError(
						ErrorCode.NotFound,
						`skill "${skill.name}" has no file "${targetPath}"`,
					).withSuggestion(`hotbed skills show ${skill.name}`);
				}
				files = [f];
			}
			return create(GetSkillResponseSchema, { skill, files });
		},

		showSkill(req) {
			const activeProject = resolveOptionalProject(req);
			const s = resolveRef(req.ref, activeProject);
			s.history = ctrl.store.skills.history(s.id);
			return s;
		},

		reviseSkill(req, ctx) {
			if (req.global && req.project) {
				throw hotbedError(ErrorCode.InvalidArgs, "give either --global or --project, not both");
			}
			if (req.global && req.repo !== undefined && req.repo.trim()) {
				throw hotbedError(ErrorCode.InvalidArgs, "a global skill cannot be scoped to a repo");
			}
			const { by, project } = ctrl.userStamp(getCaller(ctx.values), req.sessionId, "");
			return ctrl.store.skills.change(req.id, "edited", by, (item) => {
				if (item.scope === SkillScope.PROJECT) {
					inProject("skill", item.id, item.project, project, by);
				}
				checkSkillRevision(item, req.ifRevision);

				const wasProject = item.scope === SkillScope.PROJECT;

				if (req.global) {
					item.scope = SkillScope.GLOBAL;
					item.project = "";
					item.repo = "";
				} else if (req.project) {
					const targetProj = ctrl.store.projects.get(req.project);
					const projectChanged =
						item.scope !== SkillScope.PROJECT || item.project !== targetProj.name;
					item.scope = SkillScope.PROJECT;
					item.project = targetProj.name;
					if (req.repo !== undefined) {
						item.repo = req.repo.trim() ? projectRepo(targetProj, req.repo.trim(), "") : "";
					} else if (projectChanged && item.repo && !targetProj.repos.includes(item.repo)) {
						item.repo = "";
					}
				} else if (req.repo !== undefined) {
					if (item.scope === SkillScope.GLOBAL) {
						if (req.repo.trim()) {
							throw hotbedError(ErrorCode.InvalidArgs, "a global skill cannot be scoped to a repo");
						}
						item.repo = "";
					} else {
						const curProj = ctrl.store.projects.get(item.project);
						item.repo = req.repo.trim() ? projectRepo(curProj, req.repo.trim(), "") : "";
					}
				}

				if (req.replaceTags) {
					item.tags = normalizeTags(req.tags);
				} else if (req.tags.length > 0) {
					item.tags = normalizeTags([...item.tags, ...req.tags]);
				}

				if (req.rationale !== undefined) {
					if (!item.provenance) item.provenance = create(SkillProvenanceSchema);
					item.provenance.rationale = req.rationale.trim();
				}

				item.edited = by;
				if (wasProject && item.scope === SkillScope.GLOBAL) {
					return "promoted";
				}
			});
		},

		archiveSkill(req, ctx) {
			const reason = req.reason.trim();
			const { by, project } = ctrl.userStamp(getCaller(ctx.values), req.sessionId, reason);
			return ctrl.store.skills.change(req.id, "archived", by, (item) => {
				if (item.scope === SkillScope.PROJECT) {
					inProject("skill", item.id, item.project, project, by);
				}
				checkSkillRevision(item, req.ifRevision);
				if (item.state !== SkillState.ACTIVE) {
					throw hotbedError(
						ErrorCode.Conflict,
						`skill #${item.id} is ${enumWord(SkillStateSchema, item.state)}; only an active skill can be archived`,
					);
				}
				item.state = SkillState.ARCHIVED;
				item.archived = by;
			});
		},

		restoreSkill(req, ctx) {
			const { by, project } = ctrl.userStamp(getCaller(ctx.values), req.sessionId, "");
			return ctrl.store.skills.change(req.id, "restored", by, (item) => {
				if (item.scope === SkillScope.PROJECT) {
					inProject("skill", item.id, item.project, project, by);
				}
				checkSkillRevision(item, req.ifRevision);
				if (item.state !== SkillState.ARCHIVED) {
					throw hotbedError(
						ErrorCode.Conflict,
						`skill #${item.id} is ${enumWord(SkillStateSchema, item.state)}; only archived skills can be restored`,
					);
				}
				item.state = SkillState.ACTIVE;
				item.archived = undefined;
			});
		},

		deleteSkill(req, ctx) {
			const { by, project } = ctrl.userStamp(getCaller(ctx.values), req.sessionId, "");
			return ctrl.store.skills.delete(req.id, (item) => {
				if (item.scope === SkillScope.PROJECT) {
					inProject("skill", item.id, item.project, project, by);
				}
				checkSkillRevision(item, req.ifRevision);
			});
		},

		moveSkill(req, ctx) {
			const { by, project } = ctrl.userStamp(getCaller(ctx.values), req.sessionId, "");
			let place: { before?: number; first?: boolean } = {};
			if (req.place.case === "before") {
				if (req.place.value === req.id) {
					throw hotbedError(ErrorCode.InvalidArgs, `skill #${req.id} cannot go before itself`);
				}
				place = { before: req.place.value };
			} else if (req.place.case === "first") {
				place = { first: req.place.value };
			} else if (req.place.case !== "last" || !req.place.value) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`say where skill #${req.id} goes: before another, first or last`,
				);
			}
			return ctrl.store.skills.move(req.id, place, by, (s, before) => {
				if (s.scope === SkillScope.PROJECT) {
					inProject("skill", s.id, s.project, project, by);
				}
				if (s.state !== SkillState.ACTIVE) {
					throw hotbedError(
						ErrorCode.Conflict,
						`skill #${s.id} is ${enumWord(SkillStateSchema, s.state)}: only active skills can be moved`,
					);
				}
				if (before && before.state !== SkillState.ACTIVE) {
					throw hotbedError(
						ErrorCode.Conflict,
						`skill #${before.id} is ${enumWord(SkillStateSchema, before.state)}: only active skills have a place in the order`,
					);
				}
			});
		},
	};
}
