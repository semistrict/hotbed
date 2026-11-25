import type { ServiceImpl } from "@connectrpc/connect";
import { clone, create } from "@bufbuild/protobuf";
import {
	ActorKind,
	GetMemoriesResponseSchema,
	ListMemoriesResponseSchema,
	MemoryContentSchema,
	MemoryMode,
	MemoryProvenanceSchema,
	MemoryScope,
	MemoryService,
	MemoryState,
	type Memory,
	type MemoryContent,
	type Project,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import { getCaller } from "../connect-adapter.ts";
import { inProject, type ControllerContext } from "../context.ts";
import { ErrorCode, hotbedError } from "../errors.ts";
import { canonicalRemote } from "../gitops.ts";

const SLUG_RE = /^[a-z0-9][a-z0-9-]{0,63}$/;
const TAG_RE = /^[a-z0-9][a-z0-9-]{0,31}$/;
const MAX_TITLE_RUNES = 120;
const MAX_ALWAYS_SUMMARY_RUNES = 240;
const MAX_ON_DEMAND_SUMMARY_RUNES = 280;
const MAX_BODY_BYTES = 64 * 1024;
const MAX_TAGS = 10;
const DIRTY_KEPT = 20;

function stateWord(st: MemoryState): string {
	switch (st) {
		case MemoryState.PROPOSED:
			return "proposed";
		case MemoryState.APPROVED:
			return "approved";
		case MemoryState.REJECTED:
			return "rejected";
		case MemoryState.SUPERSEDED:
			return "superseded";
		case MemoryState.ARCHIVED:
			return "archived";
		case MemoryState.WITHDRAWN:
			return "withdrawn";
		default:
			return "unspecified";
	}
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

function normalizeSupersedes(ids: number[]): number[] {
	const out: number[] = [];
	const seen = new Set<number>();
	for (const id of ids) {
		if (id <= 0) {
			throw hotbedError(ErrorCode.InvalidArgs, `invalid superseded memory ID ${id}`);
		}
		if (!seen.has(id)) {
			seen.add(id);
			out.push(id);
		}
	}
	return out;
}

function validateContent(c?: MemoryContent): MemoryContent {
	if (!c) {
		throw hotbedError(ErrorCode.InvalidArgs, "memory content is required").withSuggestion(
			"pass --slug SLUG --summary SUMMARY [--body BODY] [--mode always|on_demand]",
		);
	}
	const slug = c.slug.trim().toLowerCase();
	if (!SLUG_RE.test(slug)) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`slug "${c.slug}" must be lowercase kebab-case (letters, digits, dashes; max 64 chars)`,
		).withSuggestion("e.g. --slug subagent-model-neon");
	}
	const title = c.title.trim();
	if (Array.from(title).length > MAX_TITLE_RUNES) {
		throw hotbedError(ErrorCode.InvalidArgs, `title is longer than ${MAX_TITLE_RUNES} characters`);
	}
	const body = c.body.trim();
	const mode =
		c.mode === MemoryMode.UNSPECIFIED ? (body ? MemoryMode.ON_DEMAND : MemoryMode.ALWAYS) : c.mode;
	if (mode !== MemoryMode.ALWAYS && mode !== MemoryMode.ON_DEMAND) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`unknown memory mode ${String(c.mode)}`,
		).withSuggestion("use --mode always or --mode on_demand");
	}
	const summary = c.summary.trim();
	if (!summary) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			"summary is required: the compact rule (for always) or 1-line description/trigger (for on-demand)",
		);
	}
	const maxSummary =
		mode === MemoryMode.ALWAYS ? MAX_ALWAYS_SUMMARY_RUNES : MAX_ON_DEMAND_SUMMARY_RUNES;
	const summaryRunes = Array.from(summary).length;
	if (summaryRunes > maxSummary) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`summary is ${summaryRunes} characters (max ${maxSummary} for ${mode === MemoryMode.ALWAYS ? "always" : "on-demand"} mode)`,
		).withSuggestion(
			mode === MemoryMode.ALWAYS
				? "keep always-on rules crisp (<= 240 chars), or pass --body for an on-demand memory"
				: "keep the index summary to one concise line and put full details in --body",
		);
	}
	if (mode === MemoryMode.ON_DEMAND && !body) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			"on-demand memories require --body (or --body-file) with the full details",
		).withSuggestion(
			"pass --body TEXT, --body-file PATH, or omit --mode / use --mode always for a short inline rule",
		);
	}
	const bodyBytes = new TextEncoder().encode(body).byteLength;
	if (bodyBytes > MAX_BODY_BYTES) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`body is ${bodyBytes} bytes (max ${MAX_BODY_BYTES} bytes)`,
		);
	}
	const tags = normalizeTags(c.tags);
	return create(MemoryContentSchema, {
		slug,
		title,
		summary,
		body,
		mode,
		tags,
	});
}

function mergeContent(
	base: MemoryContent,
	patch?: MemoryContent,
	opts?: {
		replaceContent?: boolean;
		clearTitle?: boolean;
		clearBody?: boolean;
		replaceTags?: boolean;
	},
): MemoryContent {
	if (opts?.replaceContent) {
		return validateContent(patch);
	}
	const out = clone(MemoryContentSchema, base);
	if (patch) {
		if (patch.slug.trim()) out.slug = patch.slug;
		if (patch.title.trim()) out.title = patch.title;
		if (patch.summary.trim()) out.summary = patch.summary;
		if (patch.body.trim()) out.body = patch.body;
		if (patch.mode !== MemoryMode.UNSPECIFIED) out.mode = patch.mode;
		if (patch.tags.length > 0) out.tags = normalizeTags(patch.tags);
	}
	if (opts?.clearTitle) out.title = "";
	if (opts?.clearBody) out.body = "";
	if (opts?.replaceTags) out.tags = normalizeTags(patch?.tags ?? []);
	return validateContent(out);
}

function checkRevision(m: Memory, want: bigint): void {
	if (want > 0n && m.revision !== want) {
		throw hotbedError(
			ErrorCode.Conflict,
			`memory #${m.id} is at revision ${m.revision}, not ${want}`,
		).withSuggestion(`hotbed memory show ${m.id}`);
	}
}

function memoryRepo(project: Project, named: string, fromRemote: string): string {
	if (named) {
		const repo = canonicalRemote(named);
		if (!project.repos.includes(repo)) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`project ${project.name} does not hold ${repo}; it holds ${project.repos.join(", ")}`,
			).withSuggestion(`hotbed project add ${project.name} --repo ${repo}`);
		}
		return repo;
	}
	const from = canonicalRemote(fromRemote);
	if (from && project.repos.includes(from)) return from;
	return "";
}

export function createMemoryService(ctrl: ControllerContext): ServiceImpl<typeof MemoryService> {
	/** Resolve the optional active project from explicit flag, session, or checkout remote. */
	function resolveOptionalProject(req: {
		project: string;
		sessionId: string;
		remote: string;
	}): string {
		if (req.project) {
			ctrl.store.getProject(req.project);
			return req.project;
		}
		if (req.sessionId) {
			const sess = ctrl.store.getSession(req.sessionId);
			if (sess.project) return sess.project;
		}
		if (req.remote) {
			const remote = canonicalRemote(req.remote);
			if (remote) {
				const holding = ctrl.store.projectsHolding(remote);
				if (holding.length === 1) return holding[0].name;
			}
		}
		return "";
	}

	/** Resolve a reference ("12", "#12", "global/slug", "project/slug", or bare "slug"). */
	function resolveRef(rawRef: string, activeProject: string): Memory {
		const ref = rawRef.trim();
		if (!ref) {
			throw hotbedError(ErrorCode.InvalidArgs, "memory reference is empty");
		}
		const bareNum = ref.startsWith("#") ? ref.slice(1) : ref;
		if (/^\d+$/.test(bareNum)) {
			return ctrl.store.getMemory(Number(bareNum));
		}
		const slash = ref.indexOf("/");
		if (slash >= 0) {
			const scopePart = ref.slice(0, slash).trim().toLowerCase();
			const slugPart = ref
				.slice(slash + 1)
				.trim()
				.toLowerCase();
			if (!slugPart) {
				throw hotbedError(ErrorCode.InvalidArgs, `invalid memory reference "${rawRef}"`);
			}
			const scopeKey = scopePart === "global" ? "" : scopePart;
			if (scopeKey) ctrl.store.getProject(scopeKey);
			const found = ctrl.store.findApprovedByScopeSlug(scopeKey, slugPart);
			if (!found) {
				throw hotbedError(
					ErrorCode.NotFound,
					`no active memory "${slugPart}" in ${scopeKey ? `project ${scopeKey}` : "global"}`,
				).withSuggestion("hotbed memory ls");
			}
			return found;
		}
		const slug = ref.toLowerCase();
		if (activeProject) {
			const inProj = ctrl.store.findApprovedByScopeSlug(activeProject, slug);
			if (inProj) return inProj;
		}
		const inGlobal = ctrl.store.findApprovedByScopeSlug("", slug);
		if (inGlobal) return inGlobal;

		const others = ctrl.store.findApprovedBySlugAnyScope(slug);
		if (others.length > 0) {
			const hints = others
				.map((m) => `${m.project || "global"}/${m.content?.slug ?? slug}`)
				.join(", ");
			throw hotbedError(
				ErrorCode.NotFound,
				`no active memory "${slug}" in ${activeProject ? `project ${activeProject} or global` : "global"}`,
			).withSuggestion(`found in another scope: ${hints}`);
		}
		throw hotbedError(ErrorCode.NotFound, `no active memory "${rawRef}"`).withSuggestion(
			"hotbed memory ls",
		);
	}

	return {
		proposeMemory(req, ctx) {
			const caller = getCaller(ctx.values);
			if (req.global && req.project) {
				throw hotbedError(ErrorCode.InvalidArgs, "give either --global or --project, not both");
			}
			if (req.global && req.repo) {
				throw hotbedError(ErrorCode.InvalidArgs, "a global memory cannot be scoped to a repo");
			}
			const rationale = req.rationale.trim();
			const { by, project: sessProject } = ctrl.userStamp(caller, req.sessionId, rationale);
			if (by.actorKind === ActorKind.AGENT && !rationale) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					"give --rationale explaining why you are adding this memory and what triggered it",
				).withSuggestion('pass --rationale "..."');
			}

			let scope = MemoryScope.PROJECT;
			let targetProject: Project | null = null;
			let repo = "";

			if (req.global) {
				scope = MemoryScope.GLOBAL;
			} else if (req.project) {
				targetProject = ctrl.store.getProject(req.project);
				if (sessProject && sessProject.name !== targetProject.name) {
					throw hotbedError(
						ErrorCode.Conflict,
						`session ${by.session} works on project ${sessProject.name}, not ${targetProject.name}`,
					).withSuggestion("save for your session's project or pass --global");
				}
				repo = memoryRepo(targetProject, req.repo, req.from?.remote ?? "");
			} else if (sessProject) {
				targetProject = sessProject;
				repo = memoryRepo(targetProject, req.repo, req.from?.remote ?? "");
			} else {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					"specify --global or -p PROJECT when creating a memory outside a session",
				).withSuggestion("pass --global, -p PROJECT, or -s SESSION");
			}

			const content = validateContent(req.content);
			const supersedesIds = normalizeSupersedes(req.supersedesIds);

			// Agents may only supersede global memories or memories of their own project.
			if (sessProject) {
				for (const tid of supersedesIds) {
					const target = ctrl.store.getMemory(tid);
					if (target.scope === MemoryScope.PROJECT && target.project !== sessProject.name) {
						throw hotbedError(
							ErrorCode.Conflict,
							`cannot supersede memory #${tid} of project ${target.project} from session ${by.session} (${sessProject.name})`,
						);
					}
				}
			}

			const from = req.from;
			if (from && from.dirty.length > DIRTY_KEPT) {
				from.dirty = from.dirty.slice(0, DIRTY_KEPT);
			}
			let t3Thread = req.t3codeThreadId.trim();
			if (!t3Thread && req.sessionId) {
				const sess = ctrl.store.tryGetSession(req.sessionId);
				if (sess?.t3codeThreadId) t3Thread = sess.t3codeThreadId;
			}
			const provenance = create(MemoryProvenanceSchema, {
				from,
				t3codeThreadId: t3Thread,
				model: req.model.trim(),
				rationale: req.rationale.trim(),
			});

			return ctrl.store.proposeMemory({
				scope,
				project: targetProject?.name ?? "",
				repo,
				content,
				supersedesIds,
				provenance,
				by,
			});
		},

		listMemories(req) {
			if (req.limit < 0) {
				throw hotbedError(ErrorCode.InvalidArgs, `limit ${req.limit}: give 0 for all, or how many`);
			}
			const explicitProject = req.projectScope.case === "project" ? req.projectScope.value : "";
			const allProjects = req.projectScope.case === "allProjects" && req.projectScope.value;
			if (req.scope === MemoryScope.GLOBAL && explicitProject) {
				throw hotbedError(ErrorCode.InvalidArgs, "cannot combine --scope global with --project");
			}
			let project = "";
			if (req.scope === MemoryScope.GLOBAL) {
				if (req.sessionId) ctrl.store.getSession(req.sessionId);
			} else {
				project = ctrl.listedProject({
					scope: req.projectScope,
					sessionId: req.sessionId,
					remote: req.remote,
				});
			}
			const { memories, total } = ctrl.store.listMemories({
				states: req.states,
				scope: req.scope,
				project,
				allProjects,
				mode: req.mode,
				tag: req.tag,
				query: req.query,
				order: req.order,
				limit: req.limit,
				includeBody: false,
			});
			return create(ListMemoriesResponseSchema, {
				memories,
				total,
			});
		},

		getMemories(req) {
			if (req.refs.length === 0) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					"name at least one memory ID or slug",
				).withSuggestion("hotbed memory ls");
			}
			const activeProject = resolveOptionalProject(req);
			const memories: Memory[] = [];
			const seen = new Set<number>();
			for (const ref of req.refs) {
				const m = resolveRef(ref, activeProject);
				if (!seen.has(m.id)) {
					seen.add(m.id);
					// Justification / provenance rationale is pure metadata for humans and auditing,
					// not returned in context when reading memories.
					if (m.provenance) m.provenance.rationale = "";
					if (m.proposed) m.proposed.reason = "";
					memories.push(m);
				}
			}
			return create(GetMemoriesResponseSchema, { memories });
		},

		showMemory(req) {
			const activeProject = resolveOptionalProject(req);
			const m = resolveRef(req.ref, activeProject);
			m.history = ctrl.store.memoryHistory(m.id);
			return m;
		},

		reviseMemory(req, ctx) {
			if (req.global && req.project) {
				throw hotbedError(ErrorCode.InvalidArgs, "give either --global or --project, not both");
			}
			if (req.global && req.repo !== undefined && req.repo.trim()) {
				throw hotbedError(ErrorCode.InvalidArgs, "a global memory cannot be scoped to a repo");
			}
			const { by, project } = ctrl.userStamp(getCaller(ctx.values), req.sessionId, "");
			return ctrl.store.changeMemory(req.id, "edited", by, (item) => {
				if (item.scope === MemoryScope.PROJECT) {
					inProject("memory", item.id, item.project, project, by);
				}
				checkRevision(item, req.ifRevision);

				const wasProject = item.scope === MemoryScope.PROJECT;

				if (req.global) {
					item.scope = MemoryScope.GLOBAL;
					item.project = "";
					item.repo = "";
				} else if (req.project) {
					const targetProj = ctrl.store.getProject(req.project);
					if (project && project.name !== targetProj.name) {
						throw hotbedError(
							ErrorCode.Conflict,
							`session ${by.session} works on project ${project.name}, not ${targetProj.name}`,
						).withSuggestion("edit memories in your session's project or promote with --global");
					}
					const projectChanged =
						item.scope !== MemoryScope.PROJECT || item.project !== targetProj.name;
					item.scope = MemoryScope.PROJECT;
					item.project = targetProj.name;
					if (req.repo !== undefined) {
						item.repo = req.repo.trim() ? memoryRepo(targetProj, req.repo.trim(), "") : "";
					} else if (projectChanged && item.repo && !targetProj.repos.includes(item.repo)) {
						item.repo = "";
					}
				} else if (req.repo !== undefined) {
					if (item.scope === MemoryScope.GLOBAL) {
						if (req.repo.trim()) {
							throw hotbedError(
								ErrorCode.InvalidArgs,
								"a global memory cannot be scoped to a repo",
							);
						}
						item.repo = "";
					} else {
						const curProj = ctrl.store.getProject(item.project);
						item.repo = req.repo.trim() ? memoryRepo(curProj, req.repo.trim(), "") : "";
					}
				}

				item.content = mergeContent(item.content!, req.content, {
					replaceContent: req.replaceContent,
					clearTitle: req.clearTitle,
					clearBody: req.clearBody,
					replaceTags: req.replaceTags,
				});

				if (req.rationale !== undefined) {
					if (!item.provenance) item.provenance = create(MemoryProvenanceSchema);
					item.provenance.rationale = req.rationale.trim();
				}

				item.edited = by;
				if (wasProject && item.scope === MemoryScope.GLOBAL) {
					return "promoted";
				}
			});
		},

		archiveMemory(req, ctx) {
			const reason = req.reason.trim();
			const { by, project } = ctrl.userStamp(getCaller(ctx.values), req.sessionId, reason);
			if (by.actorKind === ActorKind.AGENT && !reason) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					"give --reason explaining why this memory is being archived",
				).withSuggestion('pass --reason "..."');
			}
			return ctrl.store.changeMemory(req.id, "archived", by, (item) => {
				if (item.scope === MemoryScope.PROJECT) {
					inProject("memory", item.id, item.project, project, by);
				}
				checkRevision(item, req.ifRevision);
				if (item.state !== MemoryState.APPROVED) {
					throw hotbedError(
						ErrorCode.Conflict,
						`memory #${item.id} is ${stateWord(item.state)}; only an active memory can be archived`,
					);
				}
				item.state = MemoryState.ARCHIVED;
				item.archived = by;
			});
		},

		restoreMemory(req, ctx) {
			const { by, project } = ctrl.userStamp(getCaller(ctx.values), req.sessionId, "");
			return ctrl.store.changeMemory(req.id, "restored", by, (item) => {
				if (item.scope === MemoryScope.PROJECT) {
					inProject("memory", item.id, item.project, project, by);
				}
				checkRevision(item, req.ifRevision);
				if (
					item.state !== MemoryState.ARCHIVED &&
					item.state !== MemoryState.REJECTED &&
					item.state !== MemoryState.WITHDRAWN
				) {
					throw hotbedError(
						ErrorCode.Conflict,
						`memory #${item.id} is ${stateWord(item.state)}; only archived, rejected, or withdrawn memories can be restored`,
					);
				}
				item.state = MemoryState.APPROVED;
				item.archived = undefined;
				item.decided = by;
			});
		},

		deleteMemory(req, ctx) {
			const { by, project } = ctrl.userStamp(getCaller(ctx.values), req.sessionId, "");
			return ctrl.store.deleteMemory(req.id, (item) => {
				if (item.scope === MemoryScope.PROJECT) {
					inProject("memory", item.id, item.project, project, by);
				}
				checkRevision(item, req.ifRevision);
			});
		},

		moveMemory(req, ctx) {
			const { by, project } = ctrl.userStamp(getCaller(ctx.values), req.sessionId, "");
			let place: { before?: number; first?: boolean } = {};
			if (req.place.case === "before") {
				if (req.place.value === req.id) {
					throw hotbedError(ErrorCode.InvalidArgs, `memory #${req.id} cannot go before itself`);
				}
				place = { before: req.place.value };
			} else if (req.place.case === "first") {
				place = { first: req.place.value };
			} else if (req.place.case !== "last" || !req.place.value) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`say where memory #${req.id} goes: before another, first or last`,
				);
			}
			return ctrl.store.moveMemory(req.id, place, by, (m, before) => {
				if (m.scope === MemoryScope.PROJECT) {
					inProject("memory", m.id, m.project, project, by);
				}
				if (m.state !== MemoryState.APPROVED) {
					throw hotbedError(
						ErrorCode.Conflict,
						`memory #${m.id} is ${stateWord(m.state)}: only active memories can be moved`,
					);
				}
				if (before && before.state !== MemoryState.APPROVED) {
					throw hotbedError(
						ErrorCode.Conflict,
						`memory #${before.id} is ${stateWord(before.state)}: only active memories have a place in the order`,
					);
				}
			});
		},
	};
}
