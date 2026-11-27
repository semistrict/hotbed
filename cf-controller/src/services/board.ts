import type { ServiceImpl } from "@connectrpc/connect";
import { create } from "@bufbuild/protobuf";
import {
	BatchCreateBoardTasksResponseSchema,
	BoardActorKind,
	BoardActorSchema,
	BoardAuthorSchema,
	BoardMessageResultSchema,
	BoardScheduleResultSchema,
	BoardService,
	BoardTaskResultSchema,
	CancelBoardScheduleResponseSchema,
	CreateBoardSchemaResponseSchema,
	DownloadBoardAttachmentResponseSchema,
	GetBoardThreadResponseSchema,
	ListBoardActorsResponseSchema,
	ListBoardEventsResponseSchema,
	ListBoardSchedulesResponseSchema,
	ListBoardSchemasResponseSchema,
	ListBoardTasksResponseSchema,
	ListBoardThreadsResponseSchema,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import type { CallerIdentity } from "../auth.ts";
import { BoardStore, validateProfileName, type BoardIdentity } from "../board/store.ts";
import { getCaller } from "../connect-adapter.ts";
import { listedProject } from "../project-scope.ts";
import { MAX_WAIT_MS, type ControllerContext } from "../context.ts";
import { ErrorCode, fromUnknownError, hotbedError } from "../errors.ts";
import { canonicalRemote } from "../gitops.ts";

function sanitizeHandlePart(raw: string): string {
	const cleaned = raw
		.trim()
		.toLowerCase()
		.replace(/@.*$/, "")
		.replace(/[^a-z0-9._-]+/g, "-")
		.replace(/^[-.]+|[-.]+$/g, "");
	return cleaned || "user";
}

function raceAbortAndTimeout(
	promise: Promise<void>,
	timeoutMs: number,
	signal: AbortSignal,
): Promise<void> {
	return new Promise<void>((resolve) => {
		if (signal.aborted) {
			resolve();
			return;
		}
		const done = () => {
			clearTimeout(timer);
			signal.removeEventListener("abort", done);
			resolve();
		};
		const timer = setTimeout(done, timeoutMs);
		signal.addEventListener("abort", done, { once: true });
		void promise.then(done);
	});
}

export interface BoardServiceBundle {
	impl: ServiceImpl<typeof BoardService>;
	store: BoardStore;
	tickBoard: () => void;
}

/**
 * GET or HEAD /board/attachments/{id}, its caller authenticated: the
 * attachment's bytes, shown inline only for types a browser renders safely,
 * and sandboxed either way.
 */
export function serveBoardAttachment(store: BoardStore, request: Request, id: string): Response {
	try {
		const { attachment, data } = store.downloadAttachment(id);
		const ct = attachment.contentType || "application/octet-stream";
		const safeInline =
			ct.startsWith("image/") ||
			ct.startsWith("video/") ||
			ct.startsWith("audio/") ||
			ct === "text/plain" ||
			ct === "application/pdf";
		const safeName = attachment.name.replace(/["\\\r\n]/g, "_");
		const headers = new Headers({
			"Content-Type": ct,
			"Content-Length": String(data.byteLength),
			"Content-Disposition": `${safeInline ? "inline" : "attachment"}; filename="${safeName}"`,
			"Content-Security-Policy": "sandbox",
			"X-Content-Type-Options": "nosniff",
			"Cache-Control": "private, max-age=31536000, immutable",
		});
		return new Response(request.method === "HEAD" ? null : data, { status: 200, headers });
	} catch (err) {
		const he = fromUnknownError(err);
		const status =
			he.code === ErrorCode.NotFound
				? 404
				: he.code === ErrorCode.NotAuthorized
					? 403
					: he.code === ErrorCode.InvalidArgs
						? 400
						: 500;
		return Response.json(he.toJSON(), { status });
	}
}

export function createBoardService(
	ctrl: ControllerContext,
	storage: DurableObjectStorage,
): BoardServiceBundle {
	const store = new BoardStore(storage, ctrl.store.projects);

	function wakeProjects(projects: string[]): void {
		ctrl.boardWakes.wake("*");
		for (const p of new Set(projects)) {
			if (p) ctrl.boardWakes.wake(p);
		}
	}

	function resolveIdentity(
		caller: CallerIdentity,
		opts: {
			sessionId?: string;
			asProfile?: string;
			model?: string;
			t3codeThreadId?: string;
		},
	): { identity: BoardIdentity; sessionProject: string } {
		const isAdmin =
			caller.role === "owner" &&
			(caller.orgRole === "admin" || caller.orgRole === "owner" || !caller.userId);
		const profile = validateProfileName(opts.asProfile ?? "");
		const sessionId = (opts.sessionId ?? "").trim();

		const baseLogin = (caller.login || caller.userId || caller.hostname || "owner").trim();
		const baseHandle = sanitizeHandlePart(baseLogin);
		const principalId = `user:${baseLogin}`;

		if (sessionId) {
			const sess = ctrl.store.sessions.live(sessionId, ctrl.now());
			const effProfile = profile || sess.id;
			const actorId = `${principalId}+${effProfile}`;
			const handle = `${baseHandle}/${effProfile}`;
			const model = (opts.model ?? "").trim() || sess.model || "";
			const t3Thread = (opts.t3codeThreadId ?? "").trim() || sess.t3codeThreadId || "";
			return {
				sessionProject: sess.project,
				identity: {
					isAdmin,
					author: create(BoardAuthorSchema, {
						actor: actorId,
						principal: principalId,
						profile: effProfile,
						sessionId: sess.id,
						model,
						t3codeThreadId: t3Thread,
					}),
					actor: create(BoardActorSchema, {
						id: actorId,
						kind: BoardActorKind.AGENT,
						handle,
						name: effProfile,
						viaName: baseHandle,
						sessionId: sess.id,
						verified: true,
					}),
				},
			};
		}

		if (profile) {
			const actorId = `${principalId}+${profile}`;
			return {
				sessionProject: "",
				identity: {
					isAdmin,
					author: create(BoardAuthorSchema, {
						actor: actorId,
						principal: principalId,
						profile,
					}),
					actor: create(BoardActorSchema, {
						id: actorId,
						kind: BoardActorKind.AGENT,
						handle: `${baseHandle}/${profile}`,
						name: profile,
						viaName: baseHandle,
						verified: true,
					}),
				},
			};
		}

		return {
			sessionProject: "",
			identity: {
				isAdmin,
				author: create(BoardAuthorSchema, {
					actor: principalId,
					principal: principalId,
				}),
				actor: create(BoardActorSchema, {
					id: principalId,
					kind: BoardActorKind.PERSON,
					handle: baseHandle,
					name: baseHandle,
					verified: true,
				}),
			},
		};
	}

	function resolveWriteProject(
		explicitProject: string,
		sessionProject: string,
		remote: string,
	): string {
		if (explicitProject.trim()) {
			ctrl.store.projects.get(explicitProject.trim());
			return explicitProject.trim();
		}
		if (sessionProject) return sessionProject;
		if (remote.trim()) {
			const canonical = canonicalRemote(remote.trim());
			const holding = ctrl.store.projects.holding(canonical);
			if (holding.length === 1) return holding[0].name;
			if (holding.length > 1) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`${canonical} is in projects ${holding.map((p) => p.name).join(", ")}: pass -p PROJECT`,
				).withSuggestion("pass -p PROJECT");
			}
		}
		throw hotbedError(
			ErrorCode.InvalidArgs,
			"say which project to post to: pass -p PROJECT or run inside a session / project checkout",
		).withSuggestion("pass -p PROJECT (hotbed project ls)");
	}

	function resolveScopeProjectId(req: {
		scope:
			| { case: "project"; value: string }
			| { case: "allProjects"; value: boolean }
			| { case: undefined; value?: undefined };
		sessionId: string;
		remote: string;
	}): { projectName: string; projectId: number | null } {
		const name = listedProject(ctrl, req);
		if (!name) return { projectName: "", projectId: null };
		return { projectName: name, projectId: ctrl.store.projects.idByName(name) };
	}

	const tickBoard = () => {
		const nowMs = ctrl.now();
		const woken = [...store.expireDueLeases(nowMs), ...store.tickDueSchedules(nowMs)];
		if (woken.length > 0) wakeProjects(woken);
	};

	const impl: ServiceImpl<typeof BoardService> = {
		postBoardMessage(req, ctx) {
			const caller = getCaller(ctx.values);
			const { identity, sessionProject } = resolveIdentity(caller, {
				sessionId: req.sessionId,
				asProfile: req.asProfile,
				model: req.model,
				t3codeThreadId: req.t3codeThreadId,
			});
			const re = Number(req.re);
			const project = re > 0 ? "" : resolveWriteProject(req.project, sessionProject, req.remote);
			const res = store.postMessage({
				project,
				re,
				text: req.text,
				labels: req.labels,
				attachmentIds: req.attachmentIds,
				idempotencyKey: req.idempotencyKey,
				afterId: Number(req.afterId),
				identity,
				nowMs: ctrl.now(),
			});
			wakeProjects([res.project]);
			return create(BoardMessageResultSchema, {
				message: res.message,
				mentions: res.mentions,
				outcome: res.outcome,
			});
		},

		editBoardMessage(req, ctx) {
			const caller = getCaller(ctx.values);
			const { identity } = resolveIdentity(caller, {
				sessionId: req.sessionId,
				asProfile: req.asProfile,
			});
			const res = store.editMessage({
				id: Number(req.id),
				text: req.text,
				ifRevision: req.ifRevision,
				identity,
				nowMs: ctrl.now(),
			});
			wakeProjects([res.project]);
			return create(BoardMessageResultSchema, {
				message: res.message,
				mentions: res.mentions,
				outcome: res.outcome,
			});
		},

		deleteBoardMessage(req, ctx) {
			const caller = getCaller(ctx.values);
			const { identity } = resolveIdentity(caller, {
				sessionId: req.sessionId,
				asProfile: req.asProfile,
			});
			const res = store.deleteMessage({
				id: Number(req.id),
				ifRevision: req.ifRevision,
				identity,
				nowMs: ctrl.now(),
			});
			wakeProjects([res.project]);
			return res.message;
		},

		reactBoardMessage(req, ctx) {
			const caller = getCaller(ctx.values);
			const { identity } = resolveIdentity(caller, {
				sessionId: req.sessionId,
				asProfile: req.asProfile,
			});
			const res = store.reactMessage({
				id: Number(req.id),
				emoji: req.emoji,
				remove: req.remove,
				identity,
				nowMs: ctrl.now(),
			});
			wakeProjects([res.project]);
			return res.message;
		},

		labelBoardMessage(req, ctx) {
			const caller = getCaller(ctx.values);
			const { identity } = resolveIdentity(caller, {
				sessionId: req.sessionId,
				asProfile: req.asProfile,
			});
			const res = store.labelMessage({
				id: Number(req.id),
				set: req.set,
				unset: req.unset,
				ifEquals: req.ifEquals,
				ifUnset: req.ifUnset,
				identity,
				nowMs: ctrl.now(),
			});
			wakeProjects([res.project]);
			return res.message;
		},

		listBoardThreads(req, ctx) {
			const caller = getCaller(ctx.values);
			const { identity } = resolveIdentity(caller, {
				sessionId: req.sessionId,
				asProfile: req.asProfile,
			});
			const { projectId } = resolveScopeProjectId(req);
			const res = store.listThreads({
				projectId,
				labels: req.labels,
				by: req.by,
				mentions: req.mentions,
				callerActorId: identity.actor.id,
				callerHandle: identity.actor.handle,
				text: req.text,
				after: Number(req.after),
				before: req.before,
				limit: req.limit,
			});
			return create(ListBoardThreadsResponseSchema, {
				threads: res.threads,
				cursor: res.cursor,
				older: res.older,
			});
		},

		getBoardThread(req) {
			const res = store.getThread({
				id: Number(req.id),
				afterId: Number(req.afterId),
				labels: req.labels,
				limit: req.limit,
			});
			return create(GetBoardThreadResponseSchema, {
				message: res.message,
				replies: res.replies,
				schedules: res.schedules,
				cursor: res.cursor,
			});
		},

		async listBoardEvents(req, ctx) {
			const caller = getCaller(ctx.values);
			const { identity } = resolveIdentity(caller, {
				sessionId: req.sessionId,
				asProfile: req.asProfile,
			});
			const { projectName, projectId } = resolveScopeProjectId(req);
			const waitMs = req.timeoutSeconds > 0 ? Math.min(req.timeoutSeconds * 1000, MAX_WAIT_MS) : 0;
			const deadline = Date.now() + waitMs;
			const wakeKey = projectName || "*";

			let after = Number(req.after);
			if (after === 0 && req.last === 0 && waitMs > 0) {
				// Start waiting from current head when neither --after nor --last was given
				after = Number(
					store.listEvents({
						projectId,
						re: Number(req.re),
						labels: req.labels,
						by: req.by,
						mentions: req.mentions,
						callerActorId: identity.actor.id,
						callerHandle: identity.actor.handle,
						text: req.text,
						after: 0,
						last: 0,
						limit: req.limit,
						includeAll: req.includeAll,
					}).cursor,
				);
			}

			while (true) {
				const waiter = waitMs > 0 ? ctrl.boardWakes.wait(wakeKey) : null;
				try {
					const res = store.listEvents({
						projectId,
						re: Number(req.re),
						labels: req.labels,
						by: req.by,
						mentions: req.mentions,
						callerActorId: identity.actor.id,
						callerHandle: identity.actor.handle,
						text: req.text,
						after,
						last: after > 0 ? 0 : req.last,
						limit: req.limit,
						includeAll: req.includeAll,
					});
					if (res.events.length > 0 || waitMs === 0 || ctx.signal.aborted) {
						return create(ListBoardEventsResponseSchema, {
							events: res.events,
							cursor: res.cursor,
						});
					}
					after = Number(res.cursor);
					const remaining = deadline - Date.now();
					if (remaining <= 0) {
						return create(ListBoardEventsResponseSchema, {
							events: [],
							cursor: res.cursor,
						});
					}
					await raceAbortAndTimeout(waiter!.promise, remaining, ctx.signal);
				} finally {
					waiter?.cancel();
				}
			}
		},

		listBoardActors(req) {
			const { projectId } = resolveScopeProjectId(req);
			const res = store.listActors(projectId, req.ids);
			return create(ListBoardActorsResponseSchema, {
				actors: res.actors,
				cursor: res.cursor,
			});
		},

		uploadBoardAttachment(req, ctx) {
			const caller = getCaller(ctx.values);
			const { identity } = resolveIdentity(caller, {
				sessionId: req.sessionId,
				asProfile: req.asProfile,
			});
			return store.uploadAttachment(
				req.name,
				req.contentType,
				req.data,
				identity.actor.id,
				ctrl.now(),
			);
		},

		downloadBoardAttachment(req) {
			const res = store.downloadAttachment(req.id);
			return create(DownloadBoardAttachmentResponseSchema, {
				attachment: res.attachment,
				data: res.data,
			});
		},

		listBoardSchemas() {
			return create(ListBoardSchemasResponseSchema, { schemas: store.listSchemas() });
		},

		getBoardSchema(req) {
			return store.getSchema(req.id);
		},

		createBoardSchema(req, ctx) {
			const caller = getCaller(ctx.values);
			const { identity } = resolveIdentity(caller, {
				sessionId: req.sessionId,
				asProfile: req.asProfile,
			});
			const res = store.createSchema(
				req.name,
				req.version,
				req.description,
				req.schemaJson,
				identity.actor.id,
				ctrl.now(),
			);
			return create(CreateBoardSchemaResponseSchema, {
				schema: res.schema,
				created: res.created,
			});
		},

		scheduleBoardMessage(req, ctx) {
			const caller = getCaller(ctx.values);
			const { identity, sessionProject } = resolveIdentity(caller, {
				sessionId: req.sessionId,
				asProfile: req.asProfile,
				model: req.model,
				t3codeThreadId: req.t3codeThreadId,
			});
			const re = Number(req.re);
			const project = re > 0 ? "" : resolveWriteProject(req.project, sessionProject, req.remote);
			const res = store.createSchedule({
				project,
				re,
				text: req.text,
				labels: req.labels,
				attachmentIds: req.attachmentIds,
				inSeconds: Number(req.inSeconds),
				atMs: Number(req.atMs),
				everySeconds: Number(req.everySeconds),
				untilMs: Number(req.untilMs),
				forSeconds: Number(req.forSeconds),
				times: req.times,
				identity,
				nowMs: ctrl.now(),
			});
			return create(BoardScheduleResultSchema, {
				schedule: res.schedule,
				mentions: res.mentions,
				outcome: "scheduled",
			});
		},

		listBoardSchedules(req) {
			const { projectId } = resolveScopeProjectId(req);
			const schedules = store.listSchedules({
				projectId,
				re: Number(req.re),
				by: req.by,
				includeAll: req.includeAll,
			});
			return create(ListBoardSchedulesResponseSchema, { schedules });
		},

		cancelBoardSchedule(req, ctx) {
			const caller = getCaller(ctx.values);
			const { identity } = resolveIdentity(caller, {
				sessionId: req.sessionId,
				asProfile: req.asProfile,
			});
			const res = store.cancelSchedule(Number(req.id), identity);
			return create(CancelBoardScheduleResponseSchema, {
				schedule: res.schedule,
				canceled: res.canceled,
			});
		},

		createBoardTask(req, ctx) {
			const caller = getCaller(ctx.values);
			const { identity, sessionProject } = resolveIdentity(caller, {
				sessionId: req.sessionId,
				asProfile: req.asProfile,
				model: req.model,
				t3codeThreadId: req.t3codeThreadId,
			});
			const re = Number(req.re);
			const project = re > 0 ? "" : resolveWriteProject(req.project, sessionProject, req.remote);
			const res = store.createTask({
				project,
				re,
				text: req.text,
				labels: req.labels,
				attachmentIds: req.attachmentIds,
				idempotencyKey: req.idempotencyKey,
				needs: req.needs.map((n) => Number(n)),
				forActor: req.forActor,
				claim: req.claim,
				leaseSeconds: req.leaseSeconds,
				identity,
				nowMs: ctrl.now(),
			});
			wakeProjects([res.project]);
			return create(BoardTaskResultSchema, {
				task: res.task,
				claim: res.claim,
				mentions: res.mentions,
				outcome: res.outcome,
			});
		},

		batchCreateBoardTasks(req, ctx) {
			const caller = getCaller(ctx.values);
			const { identity, sessionProject } = resolveIdentity(caller, {
				sessionId: req.sessionId,
				asProfile: req.asProfile,
				model: req.model,
				t3codeThreadId: req.t3codeThreadId,
			});
			const project = resolveWriteProject(req.project, sessionProject, req.remote);
			const res = store.batchCreateTasks({
				project,
				tasks: req.tasks,
				idempotencyKey: req.idempotencyKey,
				identity,
				nowMs: ctrl.now(),
			});
			wakeProjects([res.project]);
			return create(BatchCreateBoardTasksResponseSchema, { tasks: res.tasks });
		},

		listBoardTasks(req) {
			const { projectId } = resolveScopeProjectId(req);
			const res = store.listTasks(
				{
					projectId,
					states: req.states,
					includeAll: req.includeAll,
					labels: req.labels,
					by: req.by,
					owner: req.owner,
					forActor: req.forActor,
					needs: Number(req.needs),
					after: Number(req.after),
					limit: req.limit,
					latest: req.latest,
				},
				ctrl.now(),
			);
			return create(ListBoardTasksResponseSchema, {
				tasks: res.tasks,
				cursor: res.cursor,
			});
		},

		async claimBoardTask(req, ctx) {
			const caller = getCaller(ctx.values);
			const { identity } = resolveIdentity(caller, {
				sessionId: req.sessionId,
				asProfile: req.asProfile,
			});
			if (!req.ready) {
				if (req.id <= 0n) {
					throw hotbedError(
						ErrorCode.InvalidArgs,
						"give a task ID to claim, or pass --ready to claim the next ready task",
					);
				}
				const res = store.claimTaskById({
					id: Number(req.id),
					leaseSeconds: req.leaseSeconds,
					identity,
					nowMs: ctrl.now(),
				});
				wakeProjects([res.project]);
				return create(BoardTaskResultSchema, {
					task: res.task,
					claim: res.claim,
					outcome: "claimed",
				});
			}

			const { projectName, projectId } = resolveScopeProjectId(req);
			const waitMs = req.waitSeconds > 0 ? Math.min(req.waitSeconds * 1000, MAX_WAIT_MS) : 0;
			const deadline = Date.now() + waitMs;
			const wakeKey = projectName || "*";

			while (true) {
				const waiter = waitMs > 0 ? ctrl.boardWakes.wait(wakeKey) : null;
				try {
					const found = store.claimNextReadyTask({
						projectId,
						labels: req.labels,
						fromOwners: req.fromOwners,
						leaseSeconds: req.leaseSeconds,
						identity,
						nowMs: ctrl.now(),
					});
					if (found) {
						wakeProjects([found.project]);
						return create(BoardTaskResultSchema, {
							task: found.task,
							claim: found.claim,
							outcome: "claimed",
						});
					}
					if (waitMs === 0 || ctx.signal.aborted) {
						return create(BoardTaskResultSchema, { outcome: "nothing_ready" });
					}
					const remaining = deadline - Date.now();
					if (remaining <= 0) {
						return create(BoardTaskResultSchema, { outcome: "timeout" });
					}
					await raceAbortAndTimeout(waiter!.promise, Math.min(remaining, 2_000), ctx.signal);
				} finally {
					waiter?.cancel();
				}
			}
		},

		renewBoardTask(req, ctx) {
			const caller = getCaller(ctx.values);
			const { identity } = resolveIdentity(caller, {
				sessionId: req.sessionId,
				asProfile: req.asProfile,
			});
			const res = store.renewTask({
				id: Number(req.id),
				leaseSeconds: req.leaseSeconds,
				identity,
				nowMs: ctrl.now(),
			});
			return create(BoardTaskResultSchema, {
				task: res.task,
				claim: res.claim,
				outcome: "renewed",
			});
		},

		releaseBoardTask(req, ctx) {
			const caller = getCaller(ctx.values);
			const { identity } = resolveIdentity(caller, {
				sessionId: req.sessionId,
				asProfile: req.asProfile,
			});
			const res = store.releaseTask({
				id: Number(req.id),
				identity,
				nowMs: ctrl.now(),
			});
			wakeProjects([res.project]);
			return res.task;
		},

		completeBoardTask(req, ctx) {
			const caller = getCaller(ctx.values);
			const { identity } = resolveIdentity(caller, {
				sessionId: req.sessionId,
				asProfile: req.asProfile,
			});
			const res = store.completeTask({
				id: Number(req.id),
				result: req.result,
				identity,
				nowMs: ctrl.now(),
			});
			wakeProjects(res.projects);
			return res.task;
		},

		failBoardTask(req, ctx) {
			const caller = getCaller(ctx.values);
			const { identity } = resolveIdentity(caller, {
				sessionId: req.sessionId,
				asProfile: req.asProfile,
			});
			const res = store.failTask({
				id: Number(req.id),
				reason: req.reason,
				identity,
				nowMs: ctrl.now(),
			});
			wakeProjects([res.project]);
			return res.task;
		},

		blockBoardTask(req, ctx) {
			const caller = getCaller(ctx.values);
			const { identity } = resolveIdentity(caller, {
				sessionId: req.sessionId,
				asProfile: req.asProfile,
			});
			const res = store.blockTask({
				id: Number(req.id),
				reason: req.reason,
				kind: req.kind,
				identity,
				nowMs: ctrl.now(),
			});
			wakeProjects([res.project]);
			return res.task;
		},

		unblockBoardTask(req, ctx) {
			const caller = getCaller(ctx.values);
			const { identity } = resolveIdentity(caller, {
				sessionId: req.sessionId,
				asProfile: req.asProfile,
			});
			const res = store.unblockTask({
				id: Number(req.id),
				identity,
				nowMs: ctrl.now(),
			});
			wakeProjects([res.project]);
			return res.task;
		},

		reopenBoardTask(req, ctx) {
			const caller = getCaller(ctx.values);
			const { identity } = resolveIdentity(caller, {
				sessionId: req.sessionId,
				asProfile: req.asProfile,
			});
			const res = store.reopenTask({
				id: Number(req.id),
				identity,
				nowMs: ctrl.now(),
			});
			wakeProjects([res.project]);
			return res.task;
		},

		assignBoardTask(req, ctx) {
			const caller = getCaller(ctx.values);
			const { identity } = resolveIdentity(caller, {
				sessionId: req.sessionId,
				asProfile: req.asProfile,
			});
			const res = store.assignTask({
				id: Number(req.id),
				forActor: req.forActor,
				unassign: req.unassign,
				identity,
				nowMs: ctrl.now(),
			});
			wakeProjects([res.project]);
			return res.task;
		},

		setBoardTaskNeeds(req, ctx) {
			const caller = getCaller(ctx.values);
			const { identity } = resolveIdentity(caller, {
				sessionId: req.sessionId,
				asProfile: req.asProfile,
			});
			const res = store.setTaskNeeds({
				id: Number(req.id),
				needs: req.needs.map((n) => Number(n)),
				identity,
				nowMs: ctrl.now(),
			});
			wakeProjects([res.project]);
			return res.task;
		},

		cancelBoardTask(req, ctx) {
			const caller = getCaller(ctx.values);
			const { identity } = resolveIdentity(caller, {
				sessionId: req.sessionId,
				asProfile: req.asProfile,
			});
			const res = store.cancelTask({
				id: Number(req.id),
				reason: req.reason,
				identity,
				nowMs: ctrl.now(),
			});
			wakeProjects([res.project]);
			return res.task;
		},

		importBoard(req, ctx) {
			const caller = getCaller(ctx.values);
			const res = store.importBoard(req, ctrl.now(), caller.login || caller.hostname || "import");
			wakeProjects(["*"]);
			return res;
		},
	};

	return { impl, store, tickBoard };
}
