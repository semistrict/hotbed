import {
	MemoryBriefingSchema,
	MemoryChangeSchema,
	MemoryIndexEntrySchema,
	MemoryMode,
	MemoryModeSchema,
	MemoryOrder,
	MemorySchema,
	MemoryScope,
	MemoryState,
	MemoryStateSchema,
	StampSchema,
	type Memory,
	type MemoryBriefing,
	type MemoryChange,
	type MemoryContent,
	type MemoryIndexEntry,
	type MemoryProvenance,
	type Stamp,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import { enumName } from "../enums.ts";
import { big, num, str } from "../sql-values.ts";
import { ErrorCode, hotbedError } from "../errors.ts";
import { create, clone } from "@bufbuild/protobuf";
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

const stateName = (s: MemoryState): string => enumName(MemoryStateSchema, s);
const modeName = (m: MemoryMode): string => enumName(MemoryModeSchema, m);

const TRAIL = { table: "memory_changes", key: "memory_id" };

const MEMORY_SELECT = `SELECT m.id, m.rank, COALESCE(p.name, '') AS project, m.doc
	 FROM memories m LEFT JOIN projects p ON p.id = m.project_id`;

function memorySearchText(c?: MemoryContent): string {
	if (!c) return "";
	return [c.slug, c.title, c.summary, ...c.tags, c.body].filter(Boolean).join("\n").toLowerCase();
}

/** Cross-agent memories: hand-ranked, slug-unique per scope, with an audit trail. */
export class MemoryStore {
	private readonly ranked: RankedList<Memory>;

	constructor(
		private readonly sql: SqlKernel,
		private readonly projects: ProjectStore,
	) {
		// One ranking across all approved memories, global and per project.
		this.ranked = {
			table: "memories",
			scope: "state = ?",
			scopeArgs: [stateName(MemoryState.APPROVED)],
			get: (id) => this.get(id),
			topOf: () => {
				const r = this.sql.first(
					"SELECT id FROM memories WHERE state = ? ORDER BY rank ASC, id ASC LIMIT 1",
					stateName(MemoryState.APPROVED),
				);
				return r ? num(r.id) : undefined;
			},
			change: (id, what, by, edit) => this.changeTx(id, what, by, edit),
		};
	}

	private projectIdOf(m: { scope: MemoryScope; project: string }): number | null {
		return m.scope === MemoryScope.PROJECT ? this.projects.idByName(m.project) : null;
	}

	static readonly BRIEFING_ALWAYS_MAX_COUNT = 20;
	static readonly BRIEFING_ALWAYS_MAX_BYTES = 4096;
	static readonly BRIEFING_ON_DEMAND_MAX_COUNT = 30;
	static readonly BRIEFING_ON_DEMAND_MAX_BYTES = 4096;

	propose(opts: {
		scope: MemoryScope;
		project: string;
		repo: string;
		content: MemoryContent;
		supersedesIds: number[];
		provenance?: MemoryProvenance;
		by: Stamp;
	}): Memory {
		return this.sql.tx(() => {
			const pid = this.projectIdOf(opts);
			const scopeKey = opts.scope === MemoryScope.PROJECT ? opts.project : "";
			const top = topRank(this.sql, this.ranked);
			const item = create(MemorySchema, {
				scope: opts.scope,
				project: scopeKey,
				repo: opts.repo,
				content: opts.content,
				state: MemoryState.APPROVED,
				rank: BigInt(top - 1),
				supersedesIds: opts.supersedesIds,
				provenance: opts.provenance,
				proposed: opts.by,
				decided: opts.by,
				updatedAt: opts.by.at,
			});

			// Validate supersedes targets exist and are APPROVED.
			const supersededTargets: Memory[] = [];
			for (const tid of opts.supersedesIds) {
				const target = this.get(tid);
				if (target.state !== MemoryState.APPROVED) {
					throw hotbedError(
						ErrorCode.Conflict,
						`cannot supersede memory #${tid}: it is not approved`,
					).withSuggestion(`hotbed memory show ${tid}`);
				}
				supersededTargets.push(target);
			}

			// Guard against duplicate active slugs unless that memory is being superseded.
			const existingSlug = this.findApprovedByScopeSlug(scopeKey, opts.content.slug);
			if (existingSlug && !opts.supersedesIds.includes(existingSlug.id)) {
				const where = scopeKey ? `project ${scopeKey}` : "global";
				throw hotbedError(
					ErrorCode.Conflict,
					`an approved memory #${existingSlug.id} already uses slug "${opts.content.slug}" in ${where}`,
				).withSuggestion(
					`pass --supersede ${existingSlug.id} to update it, or pick a different --slug`,
				);
			}

			// Insert initially in PROPOSED state so we obtain item.id before freeing
			// superseded targets' slugs in the unique approved-slug index.
			item.id = this.sql.insert(
				`INSERT INTO memories (project_id, scope_key, repo, slug, mode, state, rank, proposed_by, proposed_at, updated_at, search_text, doc)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '')`,
				pid,
				scopeKey,
				opts.repo,
				opts.content.slug,
				modeName(opts.content.mode),
				stateName(MemoryState.PROPOSED),
				item.rank,
				opts.by.session,
				opts.by.at,
				opts.by.at,
				memorySearchText(opts.content),
			);

			if (supersededTargets.length > 0) {
				let minRank = supersededTargets[0].rank;
				for (const t of supersededTargets) {
					if (t.rank < minRank) minRank = t.rank;
					t.state = MemoryState.SUPERSEDED;
					t.supersededBy = item.id;
					t.updatedAt = opts.by.at;
					const tpid = this.projectIdOf(t);
					this.saveRow(t, tpid, "superseded", opts.by);
				}
				item.rank = minRank;
			}
			this.saveRow(item, pid, "created", opts.by);
			return item;
		});
	}

	/**
	 * Auto-approves any legacy MEMORY_STATE_PROPOSED rows left from before
	 * memories became automatically approved on creation. Idempotent and fast
	 * when no proposed rows exist.
	 */
	autoApproveLegacyProposed(): number {
		const rows = this.sql
			.exec(
				`${MEMORY_SELECT}
				 WHERE m.state = ?
				 ORDER BY m.id ASC`,
				stateName(MemoryState.PROPOSED),
			)
			.toArray();
		if (rows.length === 0) return 0;

		return this.sql.tx(() => {
			let approved = 0;
			for (const r of rows) {
				const item = this.decodeRow(r);
				const scopeKey = item.scope === MemoryScope.PROJECT ? item.project : "";
				const pid = this.projectIdOf(item);
				const by =
					item.proposed ??
					create(StampSchema, {
						at: item.updatedAt || BigInt(Date.now()),
					});

				// Supersede any targets that are still active.
				let minRank: bigint | null = null;
				for (const tid of item.supersedesIds) {
					if (tid === item.id) continue;
					let target: Memory | null = null;
					try {
						target = this.get(tid);
					} catch {
						continue;
					}
					if (target.state !== MemoryState.APPROVED) continue;
					if (minRank === null || target.rank < minRank) minRank = target.rank;
					target.state = MemoryState.SUPERSEDED;
					target.supersededBy = item.id;
					target.updatedAt = by.at;
					const tpid = this.projectIdOf(target);
					this.saveRow(target, tpid, "superseded", by);
				}
				if (minRank !== null) {
					item.rank = minRank;
				}

				// Disambiguate slug if another active memory already holds it.
				if (item.content) {
					const conflict = this.findApprovedByScopeSlug(scopeKey, item.content.slug);
					if (conflict && conflict.id !== item.id) {
						const suffix = `-${item.id}`;
						const base = item.content.slug
							.slice(0, Math.max(1, 64 - suffix.length))
							.replace(/-+$/, "");
						item.content.slug = `${base}${suffix}`;
					}
				}

				item.state = MemoryState.APPROVED;
				item.decided = by;
				item.updatedAt = by.at;
				this.saveRow(item, pid, "approved", by);
				approved++;
			}
			return approved;
		});
	}

	get(id: number): Memory {
		const rows = this.sql
			.exec(
				`${MEMORY_SELECT}
				 WHERE m.id = ?`,
				id,
			)
			.toArray();
		if (rows.length === 0) {
			throw hotbedError(ErrorCode.NotFound, `no memory #${id}`).withSuggestion("hotbed memory ls");
		}
		return this.decodeRow(rows[0]);
	}

	findApprovedByScopeSlug(scopeKey: string, slug: string): Memory | null {
		const rows = this.sql
			.exec(
				`${MEMORY_SELECT}
				 WHERE m.scope_key = ? AND m.slug = ? AND m.state = ?
				 LIMIT 1`,
				scopeKey,
				slug,
				stateName(MemoryState.APPROVED),
			)
			.toArray();
		if (rows.length === 0) return null;
		return this.decodeRow(rows[0]);
	}

	findApprovedBySlugAnyScope(slug: string): Memory[] {
		return this.sql
			.exec(
				`${MEMORY_SELECT}
				 WHERE m.slug = ? AND m.state = ?
				 ORDER BY m.scope_key, m.id`,
				slug,
				stateName(MemoryState.APPROVED),
			)
			.toArray()
			.map((r) => this.decodeRow(r));
	}

	history(id: number): MemoryChange[] {
		return auditChanges(this.sql, TRAIL, id).map((c) =>
			create(MemoryChangeSchema, {
				seq: c.seq,
				what: c.what,
				by: c.by,
				memory: unmarshal(MemorySchema, c.doc),
			}),
		);
	}

	list(filter: {
		states: MemoryState[];
		scope: MemoryScope;
		project: string;
		allProjects: boolean;
		mode: MemoryMode;
		tag: string;
		query: string;
		order: MemoryOrder;
		limit: number;
		includeBody?: boolean;
	}): { memories: Memory[]; total: number } {
		if (filter.project) {
			this.projects.get(filter.project);
		}
		const states =
			filter.states.length > 0 ? filter.states.map(stateName) : [stateName(MemoryState.APPROVED)];
		const ph = states.map(() => "?").join(",");

		const clauses: string[] = [`m.state IN (${ph})`];
		const args: unknown[] = [...states];

		if (filter.scope === MemoryScope.GLOBAL) {
			clauses.push("m.project_id IS NULL");
		} else if (filter.scope === MemoryScope.PROJECT) {
			if (filter.allProjects) {
				clauses.push("m.project_id IS NOT NULL");
			} else {
				clauses.push("m.scope_key = ?");
				args.push(filter.project);
			}
		} else if (!filter.allProjects) {
			if (filter.project) {
				clauses.push("(m.scope_key = '' OR m.scope_key = ?)");
				args.push(filter.project);
			} else {
				clauses.push("m.scope_key = ''");
			}
		}

		if (filter.mode !== MemoryMode.UNSPECIFIED) {
			clauses.push("m.mode = ?");
			args.push(modeName(filter.mode));
		}

		const q = filter.query.trim().toLowerCase();
		if (q) {
			clauses.push("instr(m.search_text, ?) > 0");
			args.push(q);
		}

		const whereSql = clauses.join(" AND ");
		const isProposalOnly = filter.states.length === 1 && filter.states[0] === MemoryState.PROPOSED;
		const effectiveOrder =
			filter.order !== MemoryOrder.UNSPECIFIED
				? filter.order
				: isProposalOnly
					? MemoryOrder.NEWEST
					: MemoryOrder.RANK;

		const orderBy =
			effectiveOrder === MemoryOrder.NEWEST
				? "m.proposed_at DESC, m.id DESC"
				: effectiveOrder === MemoryOrder.OLDEST
					? "m.proposed_at ASC, m.id ASC"
					: effectiveOrder === MemoryOrder.UPDATED
						? "m.updated_at DESC, m.id DESC"
						: effectiveOrder === MemoryOrder.SLUG
							? "m.slug ASC, m.id ASC"
							: "m.rank ASC, m.updated_at DESC, m.id DESC";

		const rows = this.sql
			.exec(
				`${MEMORY_SELECT}
				 WHERE ${whereSql}
				 ORDER BY ${orderBy}`,
				...args,
			)
			.toArray();

		const tag = filter.tag.trim().toLowerCase();
		const matched: Memory[] = [];
		for (const r of rows) {
			const m = this.decodeRow(r);
			if (tag && !(m.content?.tags ?? []).includes(tag)) continue;
			if (!filter.includeBody && m.content) {
				m.content.body = "";
			}
			matched.push(m);
		}
		const total = matched.length;
		const memories = filter.limit > 0 ? matched.slice(0, filter.limit) : matched;
		return { memories, total };
	}

	briefing(project: string): MemoryBriefing {
		const approvedRows = this.sql
			.exec(
				`${MEMORY_SELECT}
				 WHERE m.state = ? AND (m.scope_key = '' OR m.scope_key = ?)
				 ORDER BY m.rank ASC, m.updated_at DESC, m.id DESC`,
				stateName(MemoryState.APPROVED),
				project,
			)
			.toArray();

		const pendingProposals = num(
			this.sql
				.exec(
					`SELECT COUNT(*) AS cnt FROM memories
					 WHERE state = ? AND (scope_key = '' OR scope_key = ?)`,
					stateName(MemoryState.PROPOSED),
					project,
				)
				.one().cnt,
		);

		const always: MemoryIndexEntry[] = [];
		const onDemand: MemoryIndexEntry[] = [];
		let alwaysBytes = 0;
		let onDemandBytes = 0;
		let omittedAlways = 0;
		let omittedOnDemand = 0;
		const enc = new TextEncoder();

		for (const r of approvedRows) {
			const m = this.decodeRow(r);
			const c = m.content;
			if (!c) continue;
			const entry = create(MemoryIndexEntrySchema, {
				id: m.id,
				slug: c.slug,
				scope: m.scope,
				project: m.project,
				repo: m.repo,
				mode: c.mode,
				summary: c.summary,
				hasBody: c.body.trim().length > 0,
				revision: m.revision,
				updatedAt: m.updatedAt,
			});
			const entryBytes = enc.encode(`${c.slug}: ${c.summary}`).byteLength;
			if (c.mode === MemoryMode.ALWAYS) {
				if (
					always.length < MemoryStore.BRIEFING_ALWAYS_MAX_COUNT &&
					(always.length === 0 || alwaysBytes + entryBytes <= MemoryStore.BRIEFING_ALWAYS_MAX_BYTES)
				) {
					always.push(entry);
					alwaysBytes += entryBytes;
				} else {
					omittedAlways++;
				}
			} else {
				if (
					onDemand.length < MemoryStore.BRIEFING_ON_DEMAND_MAX_COUNT &&
					(onDemand.length === 0 ||
						onDemandBytes + entryBytes <= MemoryStore.BRIEFING_ON_DEMAND_MAX_BYTES)
				) {
					onDemand.push(entry);
					onDemandBytes += entryBytes;
				} else {
					omittedOnDemand++;
				}
			}
		}

		return create(MemoryBriefingSchema, {
			always,
			onDemand,
			omittedAlways,
			omittedOnDemand,
			totalApproved: approvedRows.length,
			pendingProposals,
		});
	}

	pendingCounts(): { total: number; global: number; byProject: Map<string, number> } {
		const rows = this.sql
			.exec(
				`SELECT scope_key, COUNT(*) AS cnt FROM memories WHERE state = ? GROUP BY scope_key`,
				stateName(MemoryState.PROPOSED),
			)
			.toArray();
		let total = 0;
		let global = 0;
		const byProject = new Map<string, number>();
		for (const r of rows) {
			const sk = str(r.scope_key);
			const cnt = num(r.cnt);
			total += cnt;
			if (sk === "") global += cnt;
			else byProject.set(sk, cnt);
		}
		return { total, global, byProject };
	}

	change(id: number, what: string, by: Stamp, edit: (m: Memory) => string | void): Memory {
		return this.sql.tx(() => this.changeTx(id, what, by, edit));
	}

	delete(id: number, check: (m: Memory) => void): Memory {
		return this.sql.tx(() => {
			const item = this.get(id);
			check(item);
			this.sql.exec("DELETE FROM memory_changes WHERE memory_id = ?", id);
			this.sql.exec("DELETE FROM memories WHERE id = ?", id);
			return item;
		});
	}

	move(
		id: number,
		place: { before?: number; first?: boolean },
		by: Stamp,
		check: (m: Memory, before: Memory | null) => void,
	): Memory {
		return moveRankedItem(this.sql, this.ranked, id, place, by, check);
	}

	private changeTx(
		id: number,
		what: string,
		by: Stamp,
		edit: (m: Memory) => string | void,
	): Memory {
		const item = this.get(id);
		const label = edit(item) || what;
		const scopeKey = item.scope === MemoryScope.PROJECT ? item.project : "";
		const pid = this.projectIdOf(item);
		if (item.state === MemoryState.APPROVED) {
			const slug = item.content?.slug ?? "";
			const conflict = this.findApprovedByScopeSlug(scopeKey, slug);
			if (conflict && conflict.id !== item.id) {
				const where = scopeKey ? `project ${scopeKey}` : "global";
				throw hotbedError(
					ErrorCode.Conflict,
					`an approved memory #${conflict.id} already uses slug "${slug}" in ${where}`,
				).withSuggestion(`pick a different slug or remove/archive #${conflict.id} first`);
			}
		}
		if (label !== "moved") item.updatedAt = by.at;
		this.saveRow(item, pid, label, by);
		return item;
	}

	private saveRow(item: Memory, pid: number | null, what: string, by: Stamp): void {
		item.revision += 1n;
		const scopeKey = item.scope === MemoryScope.PROJECT ? item.project : "";
		const stored = clone(MemorySchema, item);
		stored.rank = 0n;
		stored.history = [];
		stored.project = "";
		stored.reminder = undefined;
		stored.cockpitUrl = "";
		this.sql.exec(
			`UPDATE memories
			 SET project_id = ?, scope_key = ?, repo = ?, slug = ?, mode = ?, state = ?, rank = ?, updated_at = ?, search_text = ?, doc = ?
			 WHERE id = ?`,
			pid,
			scopeKey,
			item.repo,
			item.content?.slug ?? "",
			modeName(item.content?.mode ?? MemoryMode.ON_DEMAND),
			stateName(item.state),
			item.rank,
			item.updatedAt,
			memorySearchText(item.content),
			marshal(MemorySchema, stored),
			item.id,
		);
		stored.rank = item.rank;
		stored.project = item.project;
		appendAuditChange(this.sql, TRAIL, item.id, what, by, marshal(MemorySchema, stored));
	}

	private decodeRow(r: Row): Memory {
		const m = unmarshal(MemorySchema, str(r.doc));
		m.id = num(r.id);
		m.rank = big(r.rank);
		m.project = str(r.project);
		return m;
	}
}
