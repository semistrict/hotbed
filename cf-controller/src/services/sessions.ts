import type { ServiceImpl } from "@connectrpc/connect";
import { create } from "@bufbuild/protobuf";
import {
	CommandUpdate_What,
	GetSessionResponseSchema,
	ListSessionsResponseSchema,
	QuestionUpdate_What,
	ReviewUpdate_What,
	SearchSessionsResponseSchema,
	SessionEventsSchema,
	SessionService,
	WorkspaceState,
	WorkspaceUpdate_What,
	type Command,
	type Session,
	type SessionEvent,
	type SessionEvents,
	type Workspace,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import { getCaller } from "../connect-adapter.ts";
import {
	MAX_WAIT_MS,
	MEMORY_FAILURE_REMINDER,
	MEMORY_FEEDBACK_REMINDER,
	MEMORY_SESSION_END_REMINDER,
	SESSION_IDLE_MS,
	sessionStartReminder,
	validateT3CodeThreadId,
	type ControllerContext,
} from "../context.ts";
import { ErrorCode, codeOf, hotbedError } from "../errors.ts";
import { canonicalRemote, generatePetname } from "../gitops.ts";
import { isCommandLive } from "../standby.ts";

const SESSION_NAME_RE = /^[a-z][a-z0-9]*(-[a-z0-9]+)+$/;
const DEFAULT_SESSION_SEARCH_LIMIT = 50;

function pollMemoryReminder(events: SessionEvent[]): string | undefined {
	let sawFailure = false;
	let sawFeedback = false;
	for (const ev of events) {
		switch (ev.event.case) {
			case "command": {
				const c = ev.event.value;
				if (
					(c.what === CommandUpdate_What.EXITED && c.exitCode !== 0) ||
					c.what === CommandUpdate_What.CANCELLED ||
					c.what === CommandUpdate_What.ISOLATION_LOST
				) {
					sawFailure = true;
				}
				break;
			}
			case "workspace":
				if (ev.event.value.what === WorkspaceUpdate_What.FAILED) sawFailure = true;
				break;
			case "question":
				if (ev.event.value.what === QuestionUpdate_What.ANSWERED) sawFeedback = true;
				break;
			case "review": {
				const w = ev.event.value.what;
				if (w === ReviewUpdate_What.FAILED) sawFailure = true;
				else if (w === ReviewUpdate_What.FINISHED || w === ReviewUpdate_What.RESPONDED) {
					sawFeedback = true;
				}
				break;
			}
		}
	}
	if (sawFailure) return MEMORY_FAILURE_REMINDER;
	if (sawFeedback) return MEMORY_FEEDBACK_REMINDER;
	return undefined;
}

export function createSessionService(ctrl: ControllerContext): ServiceImpl<typeof SessionService> {
	async function resumeSession(
		id: string,
		project: string,
		t3codeThreadId: string,
		model: string,
	): Promise<Session> {
		return ctrl.withSessionLock(id, () => {
			const sess = ctrl.store.getSession(id);
			if (project && project !== sess.project) {
				throw hotbedError(
					ErrorCode.Conflict,
					`session ${sess.id} works on project ${sess.project}, not ${project}`,
				).withSuggestion("resume it without naming a project, or start another session");
			}
			if (t3codeThreadId && !sess.t3codeThreadId) {
				ctrl.store.setSessionT3CodeThreadIdIfEmpty(sess.id, t3codeThreadId);
			}
			if (model && !sess.model) {
				ctrl.store.setSessionModelIfEmpty(sess.id, model);
			}
			if (sess.endedAt !== undefined) {
				ctrl.store.resumeSession(sess.id, ctrl.now());
				ctrl.sessionWakes.wake(sess.id);
			}
			return ctrl.store.liveSession(sess.id, ctrl.now());
		});
	}

	function sessionState(id: string, after: bigint): SessionEvents {
		const sess = ctrl.store.getSession(id);
		const events = ctrl.store.sessionEventsAfter(id, after);
		const cursor = events.length > 0 ? events[events.length - 1].seq : after;
		const views = ctrl.store.allViews();
		let live = 0;
		for (const v of views.values()) {
			for (const c of v.commands) {
				if (c.sessionId === id && isCommandLive(c.state)) live++;
			}
		}
		return create(SessionEventsSchema, {
			events,
			cursor,
			live,
			ended: sess.endedAt !== undefined,
			endReason: sess.endReason,
		});
	}

	return {
		async startSession(req, ctx) {
			const caller = getCaller(ctx.values);
			const agent = req.agent || caller.hostname;
			const t3codeThreadId = validateT3CodeThreadId(req.t3codeThreadId);
			const model = req.model.trim();

			if (req.id && (!SESSION_NAME_RE.test(req.id) || req.id.length > 48)) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`session name "${req.id}" must look like adjective-noun: lowercase words joined by dashes`,
				);
			}
			if (req.resume && !req.id) {
				throw hotbedError(ErrorCode.InvalidArgs, "say which session to resume");
			}
			if (req.id && ctrl.store.tryGetSession(req.id)) {
				if (!req.resume) {
					throw hotbedError(ErrorCode.Conflict, `a session called ${req.id} exists`).withSuggestion(
						"pick another name, or leave it out for a random one",
					);
				}
				const resumed = await resumeSession(req.id, req.project, t3codeThreadId, model);
				const proj = ctrl.store.getProject(resumed.project);
				resumed.reminder = sessionStartReminder(proj);
				if (!req.omitMemories) {
					resumed.memories = ctrl.store.memoryBriefing(resumed.project);
				}
				return resumed;
			}

			const { project } = ctrl.sessionProject(caller, req.project, canonicalRemote(req.remote));
			let sess: Session | null = null;
			if (req.id) {
				sess = ctrl.store.startSession(
					req.id,
					agent,
					project.name,
					t3codeThreadId,
					ctrl.now(),
					model,
				);
			} else {
				for (let i = 0; i < 20; i++) {
					try {
						sess = ctrl.store.startSession(
							generatePetname(),
							agent,
							project.name,
							t3codeThreadId,
							ctrl.now(),
							model,
						);
						break;
					} catch (err) {
						if (codeOf(err) !== ErrorCode.Conflict) throw err;
					}
				}
				if (!sess) {
					throw hotbedError(ErrorCode.Conflict, "could not find a free session name");
				}
			}
			sess.reminder = sessionStartReminder(project);
			if (!req.omitMemories) {
				sess.memories = ctrl.store.memoryBriefing(project.name);
			}
			return sess;
		},

		async getSession(req) {
			let id = req.id.trim();
			const tid = validateT3CodeThreadId(req.t3codeThreadId);
			if (id && tid) {
				throw hotbedError(ErrorCode.InvalidArgs, "give either id or t3code_thread_id, not both");
			}
			if (!id && !tid) {
				throw hotbedError(ErrorCode.InvalidArgs, "session id or t3code_thread_id is required");
			}
			let sess: Session;
			if (tid) {
				const { sessions } = ctrl.store.searchSessionsByT3CodeThread(tid, "", false, 1);
				if (sessions.length === 0) {
					throw hotbedError(
						ErrorCode.NotFound,
						`no session for T3 Code thread ${tid}`,
					).withSuggestion("start one with hotbed session start");
				}
				sess = sessions[0];
				id = sess.id;
			} else {
				sess = ctrl.store.getSession(id);
			}

			const nvs = ctrl.views();
			const nodeHosts: Record<string, string> = {};
			const targetNodes = new Set<string>();
			for (const sw of sess.workspaces) targetNodes.add(sw.nodeId);
			for (const nv of nvs) {
				nodeHosts[nv.node.id] = nv.node.hostname;
				for (const ws of nv.workspaces) if (ws.sessionId === id) targetNodes.add(nv.node.id);
				for (const c of nv.commands) if (c.sessionId === id) targetNodes.add(nv.node.id);
			}

			const [liveCmds, liveWSByNode] = await Promise.all([
				ctrl.liveNodeCommands(nvs, targetNodes, id),
				ctrl.liveNodeWorkspaces(nvs, targetNodes),
			]);

			let reconciled = false;
			for (const sw of sess.workspaces) {
				if (sw.releasedAt !== undefined || ctrl.creatingWS.has(sw.id)) continue;
				const nodeWS = liveWSByNode.get(sw.nodeId);
				if (!nodeWS) continue;
				const found = nodeWS.find((ws) => ws.id === sw.id);
				if (!found) {
					ctrl.store.reconcileMissingWorkspace(sw.id, ctrl.now());
					reconciled = true;
				} else if (found.state === WorkspaceState.RELEASED) {
					ctrl.store.releaseWorkspaceRow(sw.id, ctrl.now());
					reconciled = true;
				}
			}
			if (reconciled) {
				sess = ctrl.store.getSession(id);
			}

			const releasedWS = new Set(
				sess.workspaces.filter((sw) => sw.releasedAt !== undefined).map((sw) => sw.id),
			);
			const wsList: Workspace[] = [];
			const cmdList: Command[] = [];
			for (const nv of nvs) {
				const wss = liveWSByNode.get(nv.node.id) ?? nv.workspaces;
				for (const ws of wss) {
					if (
						ws.state !== WorkspaceState.RELEASED &&
						!releasedWS.has(ws.id) &&
						ws.sessionId === id
					) {
						if (!ws.project) ws.project = sess.project;
						wsList.push(ws);
					}
				}
				const cmds = liveCmds.get(nv.node.id) ?? nv.commands;
				for (const c of cmds) {
					if (c.kind !== "freshen" && c.sessionId === id) {
						cmdList.push(c);
					}
				}
			}
			wsList.sort((a, b) => Number(a.createdAt - b.createdAt));
			cmdList.sort((a, b) => {
				if (a.queuedAt !== b.queuedAt) return a.queuedAt < b.queuedAt ? -1 : 1;
				return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
			});

			return create(GetSessionResponseSchema, {
				session: sess,
				workspaces: wsList,
				commands: cmdList,
				now: BigInt(ctrl.now()),
				idleTimeoutSeconds: BigInt(SESSION_IDLE_MS / 1000),
				nodeHosts,
			});
		},

		listSessions(req) {
			const tid = validateT3CodeThreadId(req.t3codeThreadId);
			let project = "";
			if (
				!tid ||
				req.scope.case === "allProjects" ||
				req.scope.case === "project" ||
				req.sessionId ||
				req.remote
			) {
				project = ctrl.listedProject(req);
			}
			const since = req.ended ? ctrl.now() - 24 * 3600_000 : ctrl.now() + 1;
			return create(ListSessionsResponseSchema, {
				sessions: ctrl.store.recentSessions(since, project, tid),
			});
		},

		searchSessions(req) {
			const tid = validateT3CodeThreadId(req.t3codeThreadId);
			if (!tid) {
				throw hotbedError(ErrorCode.InvalidArgs, "t3code_thread_id is required").withSuggestion(
					"pass a T3 Code thread ID: hotbed session search THREAD_ID",
				);
			}
			const project = req.project.trim();
			if (project) ctrl.store.getProject(project);
			let limit = DEFAULT_SESSION_SEARCH_LIMIT;
			if (req.limit !== undefined) {
				if (req.limit < 0) {
					throw hotbedError(ErrorCode.InvalidArgs, "limit must be non-negative");
				}
				limit = req.limit;
			}
			const { sessions, total } = ctrl.store.searchSessionsByT3CodeThread(
				tid,
				project,
				req.liveOnly,
				limit,
			);
			return create(SearchSessionsResponseSchema, {
				sessions,
				more: Math.max(0, total - sessions.length),
			});
		},

		async endSession(req) {
			const sess = await ctrl.endSession(req.id, "ended by its agent", true);
			sess.reminder = MEMORY_SESSION_END_REMINDER;
			return sess;
		},

		async *watchSession(req, ctx) {
			ctrl.store.liveSession(req.sessionId, ctrl.now());
			const waitMs =
				req.timeoutSeconds > 0 && req.timeoutSeconds * 1000 < MAX_WAIT_MS
					? req.timeoutSeconds * 1000
					: MAX_WAIT_MS;
			const deadline = Date.now() + waitMs;
			let after = req.after;
			let first = true;

			while (!ctx.signal.aborted) {
				const waiter = ctrl.sessionWakes.wait(req.sessionId);
				try {
					const res = sessionState(req.sessionId, after);
					if (first || res.events.length > 0 || res.ended) {
						yield res;
						first = false;
						after = res.cursor;
					}
					if (res.ended) return;
					const remaining = deadline - Date.now();
					if (remaining <= 0) return;
					await raceAbortAndTimeout(waiter.promise, remaining, ctx.signal);
				} finally {
					waiter.cancel();
				}
			}
		},

		async poll(req, ctx) {
			if (!req.sessionId) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					"name the session to poll: its events are what happened to its commands, questions and reviews",
				).withSuggestion("pass -s SESSION, set HOTBED_SESSION, or run `hotbed session start`");
			}
			ctrl.store.liveSession(req.sessionId, ctrl.now());
			const waitMs =
				req.timeoutSeconds > 0 && req.timeoutSeconds * 1000 < MAX_WAIT_MS
					? req.timeoutSeconds * 1000
					: MAX_WAIT_MS;
			const deadline = Date.now() + waitMs;

			while (!ctx.signal.aborted) {
				const waiter = ctrl.sessionWakes.wait(req.sessionId);
				try {
					const res = sessionState(req.sessionId, req.after);
					if (res.events.length > 0 || res.ended) {
						res.reminder = pollMemoryReminder(res.events);
						return res;
					}
					const remaining = deadline - Date.now();
					if (remaining <= 0) return res;
					await raceAbortAndTimeout(waiter.promise, remaining, ctx.signal);
				} finally {
					waiter.cancel();
				}
			}
			const finalRes = sessionState(req.sessionId, req.after);
			finalRes.reminder = pollMemoryReminder(finalRes.events);
			return finalRes;
		},
	};
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
