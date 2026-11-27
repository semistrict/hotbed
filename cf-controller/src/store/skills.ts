import { create, clone } from "@bufbuild/protobuf";
import {
	SkillBriefingSchema,
	SkillChangeSchema,
	SkillDescriptorSchema,
	SkillFileSchema,
	SkillOrder,
	SkillSchema,
	SkillScope,
	SkillState,
	SkillStateSchema,
	type Skill,
	type SkillBriefing,
	type SkillChange,
	type SkillFile,
	type SkillFileMeta,
	type SkillProvenance,
	type SkillSource,
	type Stamp,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import { enumName } from "../enums.ts";
import { ErrorCode, hotbedError } from "../errors.ts";
import { big, num, str } from "../sql-values.ts";
import {
	appendAuditChange,
	auditChanges,
	marshal,
	moveRankedItem,
	topRank,
	unmarshal,
	type RankedList,
	type Row,
	type SqlKernel,
} from "./kernel.ts";
import type { ProjectStore } from "./projects.ts";

const stateName = (s: SkillState): string => enumName(SkillStateSchema, s);

const TRAIL = { table: "skill_changes", key: "skill_id" };
const SKILL_CHUNK_BYTES = 512 * 1024;

const SKILL_SELECT = `SELECT s.id, s.rank, COALESCE(p.name, '') AS project, s.doc
	 FROM skills s LEFT JOIN projects p ON p.id = s.project_id`;

export function skillUri(scope: SkillScope, project: string, name: string): string {
	const prefix = scope === SkillScope.PROJECT && project ? project : "global";
	return `skill://${prefix}/${name}/SKILL.md`;
}

function skillSearchText(item: Skill): string {
	const parts: string[] = [item.name, item.description, ...item.tags];
	if (item.source?.source) parts.push(item.source.source);
	if (item.source?.originalName) parts.push(item.source.originalName);
	for (const f of item.files) {
		parts.push(f.path);
	}
	return parts.filter(Boolean).join("\n").toLowerCase();
}

function compatibilityFromFrontmatter(frontmatterJson: string): string {
	if (!frontmatterJson) return "";
	try {
		const parsed = JSON.parse(frontmatterJson) as Record<string, unknown>;
		return typeof parsed.compatibility === "string" ? parsed.compatibility : "";
	} catch {
		return "";
	}
}

export interface StoredSkillFileInput {
	meta: SkillFileMeta;
	content: Uint8Array;
}

/** Agent Skills stored on the controller: multi-file trees with chunked blobs, SHA-256 manifests, and audit trail. */
export class SkillStore {
	private readonly ranked: RankedList<Skill>;

	constructor(
		private readonly sql: SqlKernel,
		private readonly projects: ProjectStore,
	) {
		this.ranked = {
			table: "skills",
			scope: "state = ?",
			scopeArgs: [stateName(SkillState.ACTIVE)],
			get: (id) => this.get(id),
			topOf: () => {
				const r = this.sql.first(
					"SELECT id FROM skills WHERE state = ? ORDER BY rank ASC, id ASC LIMIT 1",
					stateName(SkillState.ACTIVE),
				);
				return r ? num(r.id) : undefined;
			},
			change: (id, what, by, edit) => this.changeTx(id, what, by, edit),
		};
	}

	private projectIdOf(s: { scope: SkillScope; project: string }): number | null {
		return s.scope === SkillScope.PROJECT ? this.projects.idByName(s.project) : null;
	}

	put(opts: {
		scope: SkillScope;
		project: string;
		repo: string;
		name: string;
		description: string;
		frontmatterJson: string;
		files: StoredSkillFileInput[];
		tags: string[];
		source?: SkillSource;
		provenance?: SkillProvenance;
		ifRevision: bigint;
		by: Stamp;
	}): Skill {
		return this.sql.tx(() => {
			const pid = this.projectIdOf(opts);
			const scopeKey = opts.scope === SkillScope.PROJECT ? opts.project : "";
			const metas = opts.files.map((f) => f.meta);
			let totalBytes = 0n;
			for (const m of metas) totalBytes += m.size;

			const existing = this.findActiveByScopeName(scopeKey, opts.name);
			if (existing) {
				if (opts.ifRevision > 0n && existing.revision !== opts.ifRevision) {
					throw hotbedError(
						ErrorCode.Conflict,
						`skill #${existing.id} (${existing.name}) is at revision ${existing.revision}, not ${opts.ifRevision}`,
					).withSuggestion(`hotbed skills show ${existing.id}`);
				}
				existing.repo = opts.repo;
				existing.description = opts.description;
				existing.frontmatterJson = opts.frontmatterJson;
				existing.files = metas;
				existing.fileCount = metas.length;
				existing.totalBytes = totalBytes;
				existing.uri = skillUri(existing.scope, existing.project, existing.name);
				if (opts.tags.length > 0) existing.tags = opts.tags;
				if (opts.source) existing.source = opts.source;
				if (opts.provenance) existing.provenance = opts.provenance;
				existing.edited = opts.by;
				existing.updatedAt = opts.by.at;
				this.replaceFiles(existing.id, opts.files);
				this.saveRow(existing, pid, "updated", opts.by);
				return existing;
			}

			if (opts.ifRevision > 0n) {
				throw hotbedError(
					ErrorCode.Conflict,
					`no active skill "${opts.name}" exists to match revision ${opts.ifRevision}`,
				);
			}

			const top = topRank(this.sql, this.ranked);
			const item = create(SkillSchema, {
				scope: opts.scope,
				project: scopeKey,
				repo: opts.repo,
				name: opts.name,
				description: opts.description,
				frontmatterJson: opts.frontmatterJson,
				files: metas,
				fileCount: metas.length,
				totalBytes,
				state: SkillState.ACTIVE,
				rank: BigInt(top - 1),
				source: opts.source,
				provenance: opts.provenance,
				created: opts.by,
				updatedAt: opts.by.at,
				uri: skillUri(opts.scope, scopeKey, opts.name),
				tags: opts.tags,
			});

			item.id = this.sql.insert(
				`INSERT INTO skills (project_id, scope_key, repo, name, state, rank, created_by, created_at, updated_at, search_text, doc)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '')`,
				pid,
				scopeKey,
				opts.repo,
				opts.name,
				stateName(SkillState.ACTIVE),
				item.rank,
				opts.by.session,
				opts.by.at,
				opts.by.at,
				skillSearchText(item),
			);
			this.replaceFiles(item.id, opts.files);
			this.saveRow(item, pid, "created", opts.by);
			return item;
		});
	}

	private replaceFiles(skillId: number, files: StoredSkillFileInput[]): void {
		this.sql.exec("DELETE FROM skill_file_chunks WHERE skill_id = ?", skillId);
		this.sql.exec("DELETE FROM skill_files WHERE skill_id = ?", skillId);

		for (const { meta, content } of files) {
			let chunkCount = 0;
			for (let off = 0; off < content.byteLength; off += SKILL_CHUNK_BYTES) {
				const slice = content.subarray(off, Math.min(off + SKILL_CHUNK_BYTES, content.byteLength));
				this.sql.exec(
					`INSERT INTO skill_file_chunks (skill_id, path, chunk_index, data)
					 VALUES (?, ?, ?, ?)`,
					skillId,
					meta.path,
					chunkCount,
					slice,
				);
				chunkCount++;
			}
			this.sql.exec(
				`INSERT INTO skill_files (skill_id, path, size, digest, mime_type, binary, chunk_count)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				skillId,
				meta.path,
				meta.size,
				meta.digest,
				meta.mimeType,
				meta.binary ? 1 : 0,
				chunkCount,
			);
		}
	}

	readFile(skillId: number, path: string): SkillFile | null {
		const metaRow = this.sql.first(
			`SELECT path, size, mime_type, chunk_count FROM skill_files
			 WHERE skill_id = ? AND path = ?`,
			skillId,
			path,
		);
		if (!metaRow) return null;
		const totalSize = num(metaRow.size);
		const mimeType = str(metaRow.mime_type);
		const data = new Uint8Array(totalSize);
		if (totalSize > 0) {
			const chunks = this.sql
				.exec(
					`SELECT data FROM skill_file_chunks
					 WHERE skill_id = ? AND path = ?
					 ORDER BY chunk_index ASC`,
					skillId,
					path,
				)
				.toArray();
			let offset = 0;
			for (const c of chunks) {
				const buf = c.data;
				const bytes =
					buf instanceof ArrayBuffer
						? new Uint8Array(buf)
						: ArrayBuffer.isView(buf)
							? new Uint8Array(buf.buffer, buf.byteOffset, buf.byteLength)
							: new Uint8Array(0);
				data.set(bytes, offset);
				offset += bytes.byteLength;
			}
		}
		return create(SkillFileSchema, { path, content: data, mimeType });
	}

	readAllFiles(skillId: number): SkillFile[] {
		const rows = this.sql
			.exec(
				`SELECT path FROM skill_files
				 WHERE skill_id = ?
				 ORDER BY path ASC`,
				skillId,
			)
			.toArray();
		const out: SkillFile[] = [];
		for (const r of rows) {
			const f = this.readFile(skillId, str(r.path));
			if (f) out.push(f);
		}
		return out;
	}

	get(id: number): Skill {
		const rows = this.sql
			.exec(
				`${SKILL_SELECT}
				 WHERE s.id = ?`,
				id,
			)
			.toArray();
		if (rows.length === 0) {
			throw hotbedError(ErrorCode.NotFound, `no skill #${id}`).withSuggestion("hotbed skills ls");
		}
		return this.decodeRow(rows[0]);
	}

	findActiveByScopeName(scopeKey: string, name: string): Skill | null {
		const rows = this.sql
			.exec(
				`${SKILL_SELECT}
				 WHERE s.scope_key = ? AND s.name = ? AND s.state = ?
				 LIMIT 1`,
				scopeKey,
				name,
				stateName(SkillState.ACTIVE),
			)
			.toArray();
		if (rows.length === 0) return null;
		return this.decodeRow(rows[0]);
	}

	findActiveByNameAnyScope(name: string): Skill[] {
		return this.sql
			.exec(
				`${SKILL_SELECT}
				 WHERE s.name = ? AND s.state = ?
				 ORDER BY s.scope_key, s.id`,
				name,
				stateName(SkillState.ACTIVE),
			)
			.toArray()
			.map((r) => this.decodeRow(r));
	}

	history(id: number): SkillChange[] {
		return auditChanges(this.sql, TRAIL, id).map((c) =>
			create(SkillChangeSchema, {
				seq: c.seq,
				what: c.what,
				by: c.by,
				skill: unmarshal(SkillSchema, c.doc),
			}),
		);
	}

	list(filter: {
		states: SkillState[];
		scope: SkillScope;
		project: string;
		allProjects: boolean;
		tag: string;
		query: string;
		order: SkillOrder;
		limit: number;
	}): { skills: Skill[]; total: number } {
		if (filter.project) {
			this.projects.get(filter.project);
		}
		const states =
			filter.states.length > 0 ? filter.states.map(stateName) : [stateName(SkillState.ACTIVE)];
		const ph = states.map(() => "?").join(",");

		const clauses: string[] = [`s.state IN (${ph})`];
		const args: unknown[] = [...states];

		if (filter.scope === SkillScope.GLOBAL) {
			clauses.push("s.project_id IS NULL");
		} else if (filter.scope === SkillScope.PROJECT) {
			if (filter.allProjects) {
				clauses.push("s.project_id IS NOT NULL");
			} else {
				clauses.push("s.scope_key = ?");
				args.push(filter.project);
			}
		} else if (!filter.allProjects) {
			if (filter.project) {
				clauses.push("(s.scope_key = '' OR s.scope_key = ?)");
				args.push(filter.project);
			} else {
				clauses.push("s.scope_key = ''");
			}
		}

		const q = filter.query.trim().toLowerCase();
		if (q) {
			clauses.push("instr(s.search_text, ?) > 0");
			args.push(q);
		}

		const whereSql = clauses.join(" AND ");
		const effectiveOrder = filter.order !== SkillOrder.UNSPECIFIED ? filter.order : SkillOrder.RANK;
		const orderBy =
			effectiveOrder === SkillOrder.NEWEST
				? "s.created_at DESC, s.id DESC"
				: effectiveOrder === SkillOrder.OLDEST
					? "s.created_at ASC, s.id ASC"
					: effectiveOrder === SkillOrder.UPDATED
						? "s.updated_at DESC, s.id DESC"
						: effectiveOrder === SkillOrder.NAME
							? "s.name ASC, s.id ASC"
							: "s.rank ASC, s.updated_at DESC, s.id DESC";

		const rows = this.sql
			.exec(
				`${SKILL_SELECT}
				 WHERE ${whereSql}
				 ORDER BY ${orderBy}`,
				...args,
			)
			.toArray();

		const tag = filter.tag.trim().toLowerCase();
		const matched: Skill[] = [];
		for (const r of rows) {
			const s = this.decodeRow(r);
			if (tag && !s.tags.includes(tag)) continue;
			matched.push(s);
		}
		const total = matched.length;
		const skills = filter.limit > 0 ? matched.slice(0, filter.limit) : matched;
		return { skills, total };
	}

	/** All active skill descriptors relevant to a session in `project` (global + project). */
	briefing(project: string): SkillBriefing {
		const rows = this.sql
			.exec(
				`${SKILL_SELECT}
				 WHERE s.state = ? AND (s.scope_key = '' OR s.scope_key = ?)
				 ORDER BY s.rank ASC, s.updated_at DESC, s.id DESC`,
				stateName(SkillState.ACTIVE),
				project,
			)
			.toArray();

		const descriptors = rows.map((r) => {
			const s = this.decodeRow(r);
			return create(SkillDescriptorSchema, {
				id: s.id,
				name: s.name,
				scope: s.scope,
				project: s.project,
				repo: s.repo,
				description: s.description,
				uri: s.uri,
				fileCount: s.fileCount,
				totalBytes: s.totalBytes,
				revision: s.revision,
				updatedAt: s.updatedAt,
				compatibility: compatibilityFromFrontmatter(s.frontmatterJson),
				source: s.source?.source ?? "",
			});
		});

		return create(SkillBriefingSchema, {
			skills: descriptors,
			totalActive: descriptors.length,
		});
	}

	change(id: number, what: string, by: Stamp, edit: (s: Skill) => string | void): Skill {
		return this.sql.tx(() => this.changeTx(id, what, by, edit));
	}

	delete(id: number, check: (s: Skill) => void): Skill {
		return this.sql.tx(() => {
			const item = this.get(id);
			check(item);
			this.sql.exec("DELETE FROM skill_file_chunks WHERE skill_id = ?", id);
			this.sql.exec("DELETE FROM skill_files WHERE skill_id = ?", id);
			this.sql.exec("DELETE FROM skill_changes WHERE skill_id = ?", id);
			this.sql.exec("DELETE FROM skills WHERE id = ?", id);
			return item;
		});
	}

	move(
		id: number,
		place: { before?: number; first?: boolean },
		by: Stamp,
		check: (s: Skill, before: Skill | null) => void,
	): Skill {
		return moveRankedItem(this.sql, this.ranked, id, place, by, check);
	}

	private changeTx(id: number, what: string, by: Stamp, edit: (s: Skill) => string | void): Skill {
		const item = this.get(id);
		const label = edit(item) || what;
		const scopeKey = item.scope === SkillScope.PROJECT ? item.project : "";
		const pid = this.projectIdOf(item);
		item.uri = skillUri(item.scope, scopeKey, item.name);
		if (item.state === SkillState.ACTIVE) {
			const conflict = this.findActiveByScopeName(scopeKey, item.name);
			if (conflict && conflict.id !== item.id) {
				const where = scopeKey ? `project ${scopeKey}` : "global";
				throw hotbedError(
					ErrorCode.Conflict,
					`an active skill #${conflict.id} already uses name "${item.name}" in ${where}`,
				).withSuggestion(`remove or archive #${conflict.id} first`);
			}
		}
		if (label !== "moved") item.updatedAt = by.at;
		this.saveRow(item, pid, label, by);
		return item;
	}

	private saveRow(item: Skill, pid: number | null, what: string, by: Stamp): void {
		item.revision += 1n;
		const scopeKey = item.scope === SkillScope.PROJECT ? item.project : "";
		item.uri = skillUri(item.scope, scopeKey, item.name);
		const stored = clone(SkillSchema, item);
		stored.rank = 0n;
		stored.history = [];
		stored.project = "";
		stored.cockpitUrl = "";
		this.sql.exec(
			`UPDATE skills
			 SET project_id = ?, scope_key = ?, repo = ?, name = ?, state = ?, rank = ?, updated_at = ?, search_text = ?, doc = ?
			 WHERE id = ?`,
			pid,
			scopeKey,
			item.repo,
			item.name,
			stateName(item.state),
			item.rank,
			item.updatedAt,
			skillSearchText(item),
			marshal(SkillSchema, stored),
			item.id,
		);
		stored.rank = item.rank;
		stored.project = item.project;
		appendAuditChange(this.sql, TRAIL, item.id, what, by, marshal(SkillSchema, stored));
	}

	private decodeRow(r: Row): Skill {
		const s = unmarshal(SkillSchema, str(r.doc));
		s.id = num(r.id);
		s.rank = big(r.rank);
		s.project = str(r.project);
		s.uri = skillUri(s.scope, s.project, s.name);
		return s;
	}
}
