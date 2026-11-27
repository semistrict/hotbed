import { create } from "@bufbuild/protobuf";
import {
	ActorKind,
	BaseState,
	CommandState,
	DaemonLogLevel,
	ReleaseWorkspaceRequestSchema,
	ReleaseWorkspaceResponseSchema,
	ListCommandsRequestSchema,
	ListCommandsResponseSchema,
	ListWorkspacesRequestSchema,
	ListWorkspacesResponseSchema,
	RunOutcome,
	StampSchema,
	type Base,
	type Command,
	type HeartbeatRequest,
	type Node,
	type Project,
	type Resources,
	type Session,
	type Stamp,
	type Workspace,
} from "./gen/hotbed/v1/hotbed_pb.ts";
import type { CallerIdentity } from "./auth.ts";
import type { BuildDistributor } from "./builds.ts";
import type { CommandStoreClient } from "./command-do.ts";
import { ErrorCode, codeOf, hotbedError } from "./errors.ts";
import { parseBuildNumber } from "./version.ts";
import type { NodeRow, NodeView } from "./placement.ts";
import type { QuestionBridgeHub } from "./question-bridge.ts";
import { ONLINE_WINDOW_MS, Wakeups, isCommandLive } from "./wakeups.ts";
import {
	ControllerStore,
	LOST_DETAIL,
	sessionEndedError,
	type WorkspaceRow,
} from "./store/index.ts";
import type { WorkerTunnelHub } from "./tunnel.ts";

export const SESSION_IDLE_MS = 60 * 60_000;
export const OFFLINE_GRACE_MS = 24 * 60 * 60_000;
export const MAX_WAIT_MS = 2 * 60_000;
export const WORKER_RPC_TIMEOUT_MS = 30_000;
export const LIVE_QUERY_TIMEOUT_MS = 2_000;
export const WORKER_PORT = 7121;
export const PROJECT_NAME_RE = /^[a-z0-9][a-z0-9-]{0,39}$/;
export const REPO_ID_RE = /^[a-z0-9][a-z0-9-]{0,39}$/;
export const STEP_NAME_RE = /^[a-z0-9]+(-[a-z0-9]+)*$/;
export const MAX_T3CODE_THREAD_ID_LEN = 256;

export function validateT3CodeThreadId(raw: string): string {
	const tid = raw.trim();
	if (tid.length > MAX_T3CODE_THREAD_ID_LEN) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`t3code_thread_id is too long (max ${MAX_T3CODE_THREAD_ID_LEN} bytes)`,
		);
	}
	if (/[\r\n\0]/.test(tid)) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			"t3code_thread_id must not contain control characters",
		);
	}
	return tid;
}

export interface ScopedRequest {
	scope:
		| { case: "project"; value: string }
		| { case: "allProjects"; value: boolean }
		| { case: undefined; value?: undefined };
	sessionId: string;
	remote: string;
}

export class ControllerContext {
	readonly store: ControllerStore;
	readonly commands: CommandStoreClient;
	readonly tunnel: WorkerTunnelHub;
	readonly questionBridge?: QuestionBridgeHub;
	readonly builds: BuildDistributor;
	/**
	 * Keeps the controller alive until p settles, past the request that
	 * started it: background work a response does not wait for
	 * (DurableObjectState.waitUntil).
	 */
	readonly waitUntil: (p: Promise<unknown>) => void;
	/** Woken, by hostname, whenever a node's heartbeat changes what it can serve. */
	readonly nodeChanges = new Wakeups();
	readonly capacityChanges = new Wakeups();
	/**
	 * Caps how long placement waits for capacity or an upgrade; 0 leaves the
	 * defaults. Only tests set it (POST /__test/config wakeWaitMs).
	 */
	placementWaitCapMs = 0;
	readonly sessionWakes = new Wakeups();
	readonly boardWakes = new Wakeups();
	boardCountsFn?: () => Map<
		string,
		{ threads: number; openTasks: number; readyTasks: number; blockedTasks: number }
	>;
	readonly creatingWS = new Map<string, { promise: Promise<void>; resolve: () => void }>();
	readonly sessionRaces = new Map<string, Promise<void>>();
	readonly startingRuns = new Map<
		string,
		{ schedule: string; scheduled: boolean; stopWhy: string; abort: AbortController }
	>();
	clusterId: string;
	orgId: string;
	cladefsTenantId: bigint | null;
	readonly controllerName: string;
	readonly allowLocalRepos: boolean;
	nowFn: () => number = () => Date.now();

	private placingChain: Promise<void> = Promise.resolve();
	private sessionChains = new Map<string, Promise<void>>();
	private lastPruneAt = 0;

	constructor(opts: {
		store: ControllerStore;
		commands: CommandStoreClient;
		tunnel: WorkerTunnelHub;
		questionBridge?: QuestionBridgeHub;
		builds: BuildDistributor;
		waitUntil: (p: Promise<unknown>) => void;
		clusterId: string;
		orgId?: string;
		cladefsTenantId?: bigint | null;
		controllerName: string;
		allowLocalRepos?: boolean;
	}) {
		this.store = opts.store;
		this.commands = opts.commands;
		this.tunnel = opts.tunnel;
		this.questionBridge = opts.questionBridge;
		this.builds = opts.builds;
		this.waitUntil = opts.waitUntil;
		this.clusterId = opts.clusterId;
		this.orgId = opts.orgId ?? "";
		this.cladefsTenantId = opts.cladefsTenantId ?? null;
		this.controllerName = opts.controllerName;
		this.allowLocalRepos = opts.allowLocalRepos ?? false;
	}

	now(): number {
		return this.nowFn();
	}

	/** Per-org session idle timeout in milliseconds (default 1h). */
	sessionIdleMs(): number {
		const secs = this.store.workerConfigs.get("")?.controller?.sessionIdleTimeoutSeconds;
		if (secs !== undefined && secs >= 60) {
			return secs * 1000;
		}
		return SESSION_IDLE_MS;
	}

	/** Per-org offline node grace period in milliseconds (default 24h). */
	offlineGraceMs(): number {
		const secs = this.store.workerConfigs.get("")?.controller?.offlineGraceSeconds;
		if (secs !== undefined && secs >= 0) {
			return secs * 1000;
		}
		return OFFLINE_GRACE_MS;
	}

	/** Async mutex for placement operations. */
	async withPlacementLock<T>(fn: () => Promise<T> | T): Promise<T> {
		let release!: () => void;
		const next = new Promise<void>((r) => {
			release = r;
		});
		const prev = this.placingChain;
		this.placingChain = prev.then(() => next);
		await prev;
		try {
			return await fn();
		} finally {
			release();
		}
	}

	/** Per-session async mutex matching Go's lockSession. */
	async withSessionLock<T>(id: string, fn: () => Promise<T> | T): Promise<T> {
		let release!: () => void;
		const next = new Promise<void>((r) => {
			release = r;
		});
		const prev = this.sessionChains.get(id) ?? Promise.resolve();
		const tail = prev.then(() => next);
		this.sessionChains.set(id, tail);
		await prev;
		try {
			return await fn();
		} finally {
			release();
			if (this.sessionChains.get(id) === tail) {
				this.sessionChains.delete(id);
			}
		}
	}

	markCreatingWS(id: string): void {
		let resolve!: () => void;
		const promise = new Promise<void>((r) => {
			resolve = r;
		});
		this.creatingWS.set(id, { promise, resolve });
	}

	unmarkCreatingWS(id: string): void {
		const entry = this.creatingWS.get(id);
		if (entry) {
			this.creatingWS.delete(id);
			entry.resolve();
		}
	}

	creatingWorkspacesSet(): Set<string> {
		return new Set(this.creatingWS.keys());
	}

	withOnline(n: Node): Node {
		const nowMs = this.now();
		n.online =
			n.lastSeen !== undefined && nowMs - Number(n.lastSeen) <= ONLINE_WINDOW_MS && !n.storageError;
		return n;
	}

	addrOf(n: NodeRow): string {
		const target = n.addr || n.node.hostname;
		if (!target) return "";
		if (target.includes("://")) return target;
		// Check if port already present (IPv6 bracketed or host:port)
		if (target.startsWith("[")) {
			return target.includes("]:") ? target : `${target}:${WORKER_PORT}`;
		}
		const colons = target.split(":").length - 1;
		if (colons === 1) return target;
		if (colons > 1) return `[${target}]:${WORKER_PORT}`;
		return `${target}:${WORKER_PORT}`;
	}

	nodeAddr(nodeId: string): string {
		try {
			return this.addrOf(this.store.fleet.get(nodeId));
		} catch {
			return "";
		}
	}

	findNodeByNameOrId(name: string): NodeRow {
		for (const r of this.store.fleet.list()) {
			if (r.node.hostname === name || r.node.id === name) {
				return r;
			}
		}
		throw hotbedError(ErrorCode.NotFound, `no node is called ${name}`).withSuggestion(
			"hotbed nodes lists the nodes",
		);
	}

	findNodeByHost(host: string): NodeRow {
		for (const r of this.store.fleet.list()) {
			if (r.node.hostname === host) return r;
		}
		throw hotbedError(ErrorCode.NotFound, `no node is called ${host}`).withSuggestion(
			"hotbed nodes lists the nodes; a worker joins with hotbed worker init",
		);
	}

	follows(res: Resources | undefined): number {
		if (!this.builds.buildForNode(res)) return 0;
		return this.builds.currentBuildNumber();
	}

	nodeServes(row: NodeRow): boolean {
		const node = this.withOnline(row.node);
		if (!node.online) return false;
		const want = this.follows(row.hb?.resources);
		if (want === 0) return true;
		return parseBuildNumber(row.hb?.resources?.version ?? "") >= want;
	}

	views(): NodeView[] {
		const rows = this.store.fleet.list();
		const all = this.store.nodeState.allViews();
		const sessIds: string[] = [];
		for (const v of all.values()) {
			for (const w of v.workspaces) if (w.sessionId) sessIds.push(w.sessionId);
			for (const c of v.commands) if (c.sessionId) sessIds.push(c.sessionId);
		}
		const sessProj = this.store.sessions.projectsFor(sessIds);

		return rows.map((n) => {
			const v = all.get(n.node.id) ?? { bases: [], workspaces: [], commands: [] };
			const node = this.withOnline(n.node);
			const addr = this.addrOf(n);
			for (const w of v.workspaces) {
				w.nodeAddr = addr;
				w.node = n.node.hostname;
				const p = sessProj.get(w.sessionId);
				if (p) w.project = p;
			}
			for (const c of v.commands) {
				c.nodeAddr = addr;
				const p = sessProj.get(c.sessionId);
				if (p) c.project = p;
			}
			if (node.online && n.hb) {
				overlayHeartbeat(v, n.hb);
			}
			return {
				...n,
				node,
				online: node.online,
				tunnelConnected: this.tunnel.isConnected(n.node.id),
				bases: v.bases,
				workspaces: v.workspaces,
				commands: v.commands,
			};
		});
	}

	/** Live commands of a session (or all, for ""), by node. See liveFromNodes. */
	liveNodeCommands(
		nvs: NodeView[],
		nodeIds: Set<string> | null,
		sessionId: string,
	): Promise<Map<string, Command[]>> {
		return this.liveFromNodes(nvs, nodeIds, async (nv) => {
			const res = await this.tunnel.callUnary(
				nv.node.id,
				nv.node.hostname,
				"/hotbed.v1.WorkspaceService/ListCommands",
				ListCommandsRequestSchema,
				create(ListCommandsRequestSchema, { sessionId }),
				ListCommandsResponseSchema,
				LIVE_QUERY_TIMEOUT_MS,
			);
			const addr = this.addrOf(nv);
			for (const cmd of res.commands) cmd.nodeAddr = addr;
			return res.commands;
		});
	}

	/** Live workspaces, by node. See liveFromNodes. */
	liveNodeWorkspaces(
		nvs: NodeView[],
		nodeIds: Set<string> | null,
	): Promise<Map<string, Workspace[]>> {
		return this.liveFromNodes(nvs, nodeIds, async (nv) => {
			const res = await this.tunnel.callUnary(
				nv.node.id,
				nv.node.hostname,
				"/hotbed.v1.WorkspaceService/ListWorkspaces",
				ListWorkspacesRequestSchema,
				create(ListWorkspacesRequestSchema, {}),
				ListWorkspacesResponseSchema,
				LIVE_QUERY_TIMEOUT_MS,
			);
			const addr = this.addrOf(nv);
			for (const ws of res.workspaces) {
				ws.nodeAddr = addr;
				ws.node = nv.node.hostname;
			}
			return res.workspaces;
		});
	}

	/**
	 * Asks each online node with a connected tunnel (of nodeIds, or all) for
	 * its live items in parallel, and files each item under its session's
	 * project. A node that does not answer is left out of the map: its caller
	 * falls back to the cached node_state view.
	 */
	private async liveFromNodes<T extends { sessionId: string; project: string }>(
		nvs: NodeView[],
		nodeIds: Set<string> | null,
		ask: (nv: NodeView) => Promise<T[]>,
	): Promise<Map<string, T[]>> {
		const targets = nvs.filter(
			(nv) =>
				nv.online && this.tunnel.isConnected(nv.node.id) && (!nodeIds || nodeIds.has(nv.node.id)),
		);
		const live = new Map<string, T[]>();
		if (targets.length === 0) return live;

		await Promise.all(
			targets.map(async (nv) => {
				try {
					live.set(nv.node.id, await ask(nv));
				} catch {
					// Fall back to cached node_state view
				}
			}),
		);

		const sessIds: string[] = [];
		for (const items of live.values()) {
			for (const it of items) if (it.sessionId) sessIds.push(it.sessionId);
		}
		const sessProj = this.store.sessions.projectsFor(sessIds);
		for (const items of live.values()) {
			for (const it of items) {
				const p = sessProj.get(it.sessionId);
				if (p) it.project = p;
			}
		}
		return live;
	}

	madeBy(caller: CallerIdentity, reason: string): Stamp {
		return create(StampSchema, {
			at: BigInt(this.now()),
			host: caller.hostname || this.controllerName,
			reason,
		});
	}

	makeStamp(
		caller: CallerIdentity,
		opts: {
			sessionId?: string;
			actorKind?: ActorKind;
			reason?: string;
			model?: string;
			subagent?: string;
			t3codeThreadId?: string;
			fallbackModel?: string;
		},
	): { by: Stamp; project: Project | null; session: Session | null } {
		const sessionId = (opts.sessionId ?? "").trim();
		const reason = opts.reason ?? "";
		const reqKind = opts.actorKind ?? ActorKind.UNSPECIFIED;
		if (reqKind === ActorKind.USER && sessionId) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				"a user action cannot also name an agent session",
			).withSuggestion("omit -s/--session when passing --as-user");
		}
		if (reqKind === ActorKind.AGENT && !sessionId) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				"name the session making the change: questions and reviews record which session did what",
			).withSuggestion("pass -s SESSION, set HOTBED_SESSION, or run `hotbed session start`");
		}
		if (!sessionId) {
			const by = create(StampSchema, {
				at: BigInt(this.now()),
				actorKind: ActorKind.USER,
				session: "",
				host: caller.hostname,
				reason,
				user: caller.login || caller.userId || "",
			});
			return { by, project: null, session: null };
		}
		const sess = this.store.sessions.live(sessionId, this.now());
		const project = this.store.projects.get(sess.project);
		const model = (opts.model ?? "").trim() || sess.model || (opts.fallbackModel ?? "").trim();
		const subagent = (opts.subagent ?? "").trim();
		const t3codeThreadId =
			validateT3CodeThreadId(opts.t3codeThreadId ?? "") || sess.t3codeThreadId || "";
		const by = create(StampSchema, {
			at: BigInt(this.now()),
			actorKind: ActorKind.AGENT,
			session: sessionId,
			host: caller.hostname,
			reason,
			model,
			subagent,
			t3codeThreadId,
		});
		return { by, project, session: sess };
	}

	userStamp(
		caller: CallerIdentity,
		sessionId: string,
		reason: string,
	): { by: Stamp; project: Project | null } {
		const { by, project } = this.makeStamp(caller, { sessionId, reason });
		return { by, project };
	}

	agentStamp(
		caller: CallerIdentity,
		sessionId: string,
		reason: string,
		extra?: { model?: string; subagent?: string; t3codeThreadId?: string; fallbackModel?: string },
	): { by: Stamp; project: Project; session: Session } {
		const res = this.makeStamp(caller, {
			sessionId,
			actorKind: ActorKind.AGENT,
			reason,
			...extra,
		});
		return { by: res.by, project: res.project!, session: res.session! };
	}

	async releaseWorkspace(id: string): Promise<void> {
		const w = this.store.workspaces.get(id);
		const row = this.store.fleet.get(w.node);
		const node = this.withOnline(row.node);
		if (node.online && this.tunnel.isConnected(w.node)) {
			try {
				await this.tunnel.callUnary(
					w.node,
					row.node.hostname,
					"/hotbed.v1.WorkspaceService/ReleaseWorkspace",
					ReleaseWorkspaceRequestSchema,
					create(ReleaseWorkspaceRequestSchema, { id }),
					ReleaseWorkspaceResponseSchema,
					WORKER_RPC_TIMEOUT_MS,
				);
			} catch (err) {
				const c = codeOf(err);
				if (c !== ErrorCode.NotFound) {
					throw err;
				}
			}
		} else {
			throw hotbedError(ErrorCode.WorkerUnreachable, `${row.node.hostname} is offline`, {
				retryable: true,
			});
		}
		this.store.workspaces.release(id, this.now());
	}

	async endSession(id: string, reason: string, requireLive = false): Promise<Session> {
		return this.withSessionLock(id, async () => {
			const sess = this.store.sessions.get(id);
			if (requireLive && sess.endedAt !== undefined) {
				throw sessionEndedError(id, sess.endReason);
			}
			const wss = this.store.sessions.endAndWorkspaces(id, reason, this.now());
			return this.finishEndSession(id, reason, wss);
		});
	}

	async finishEndSession(id: string, reason: string, wss: WorkspaceRow[]): Promise<Session> {
		const runs = this.store.schedules.liveRunsOfSession(id);
		const ended = BigInt(this.now());
		for (const run of runs) {
			run.outcome = RunOutcome.CANCELLED;
			run.reason = `its session ended: ${reason}`;
			run.endedAt = ended;
			this.store.schedules.updateLiveRun(run);
		}
		for (const ws of wss) {
			try {
				await this.releaseWorkspace(ws.id);
			} catch (err) {
				const c = codeOf(err);
				if (c === ErrorCode.NotFound) continue;
				if (c === ErrorCode.WorkerUnreachable) {
					// Worker's next heartbeat releases it.
					continue;
				}
				this.store.daemonLogs.log(
					DaemonLogLevel.WARN,
					"release workspace on session end",
					{ workspace: ws.id, session: id, err: String(err) },
					this.now(),
				);
			}
		}
		this.sessionWakes.wake(id);
		return this.store.sessions.get(id);
	}

	loseRuns(commandIds: string[]): void {
		if (commandIds.length === 0) return;
		const set = new Set(commandIds);
		const ended = BigInt(this.now());
		for (const run of this.store.schedules.allLiveRuns()) {
			if (!run.commandId || !set.has(run.commandId)) continue;
			run.outcome = RunOutcome.CANCELLED;
			run.reason = LOST_DETAIL;
			run.endedAt = ended;
			this.store.schedules.updateLiveRun(run);
		}
	}

	async settleRuns(): Promise<void> {
		const toEnd: Array<{ id: string; reason: string }> = [];
		for (const run of this.store.schedules.allLiveRuns()) {
			if (!run.commandId) continue;
			const f = this.store.commands.finished(run.commandId);
			if (!f) continue;
			run.endedAt = f.endedAt;
			run.exitCode = f.exitCode;
			run.reason = f.reason;
			if (f.state === CommandState.SUCCEEDED) {
				run.outcome = RunOutcome.SUCCEEDED;
			} else if (f.state === CommandState.CANCELLED) {
				run.outcome = RunOutcome.CANCELLED;
			} else {
				run.outcome = RunOutcome.FAILED;
				if (!run.reason && f.exitCode !== undefined) {
					run.reason = `exited ${f.exitCode}`;
				}
			}
			if (run.outcome !== RunOutcome.SUCCEEDED) {
				this.store.sessions.touch(run.sessionId, this.now());
			}
			const updated = this.store.schedules.updateLiveRun(run);
			if (updated && run.outcome === RunOutcome.SUCCEEDED) {
				toEnd.push({ id: run.sessionId, reason: "its scheduled run succeeded" });
			}
		}
		for (const e of toEnd) {
			try {
				await this.endSession(e.id, e.reason);
			} catch (err) {
				this.store.daemonLogs.log(
					DaemonLogLevel.ERROR,
					"end session of scheduled run",
					{ session: e.id, err: String(err) },
					this.now(),
				);
			}
		}
	}

	async reapIdle(): Promise<void> {
		const nowMs = this.now();
		const idleMs = this.sessionIdleMs();
		const graceMs = this.offlineGraceMs();
		const cutoff = nowMs - idleMs;
		const ids = this.store.sessions.idle(cutoff);
		if (ids.length === 0) return;
		const rows = this.store.fleet.list();
		const offline = new Set<string>();
		for (const r of rows) {
			const recent = r.node.lastSeen !== undefined && nowMs - Number(r.node.lastSeen) < graceMs;
			const node = this.withOnline(r.node);
			if (!node.online && recent) offline.add(node.id);
		}
		const idleSec = Math.round(idleMs / 1000);
		const hrs = Math.floor(idleSec / 3600);
		const mins = Math.floor((idleSec % 3600) / 60);
		const secs = idleSec % 60;
		const idleLabel =
			hrs > 0 ? `${hrs}h${mins}m${secs}s` : mins > 0 ? `${mins}m${secs}s` : `${secs}s`;
		for (const id of ids) {
			await this.withSessionLock(id, async () => {
				const sess = this.store.sessions.tryGet(id);
				if (!sess || sess.endedAt !== undefined) return;
				if (sess.workspaces.some((w) => w.releasedAt === undefined && offline.has(w.nodeId))) {
					return;
				}
				const reason = `abandoned: nothing called it for ${idleLabel}`;
				const { ended, workspaces } = this.store.sessions.endIfIdleAndWorkspaces(
					id,
					reason,
					this.now(),
					cutoff,
				);
				if (!ended) return;
				await this.finishEndSession(id, reason, workspaces);
			});
		}
	}

	maybePrune(): void {
		const nowMs = this.now();
		if (this.lastPruneAt === 0) {
			const stored = Number(this.store.meta.get("last_prune_at") ?? "0");
			if (Number.isFinite(stored) && stored > 0) {
				this.lastPruneAt = stored;
			}
		}
		if (nowMs - this.lastPruneAt < 10 * 60_000) return;
		this.lastPruneAt = nowMs;
		this.store.meta.set("last_prune_at", String(nowMs));
		this.store.prune(nowMs);
	}
}

export function inProject(
	what: string,
	id: number,
	ofProject: string,
	project: Project | null,
	by: Stamp,
): void {
	if (!project || ofProject === project.name) return;
	throw hotbedError(
		ErrorCode.Conflict,
		`${what} ${id} is project ${ofProject}'s; session ${by.session} works on ${project.name}`,
	).withSuggestion(`leave it to a session of ${ofProject}`);
}

export function stamped(st?: Stamp): string {
	let who = "by the user";
	if (st?.session) who = `by ${st.session}`;
	if (st?.host) who += ` on ${st.host}`;
	const d = new Date(Number(st?.at ?? 0n));
	return `${who} at ${d.toISOString().replace("T", " ").slice(0, 19)}`;
}

function overlayHeartbeat(
	v: { bases: Base[]; workspaces: Workspace[]; commands: Command[] },
	h: HeartbeatRequest,
): void {
	const cmds = new Map(h.commands.map((u) => [u.id, u]));
	for (const c of v.commands) {
		const u = cmds.get(c.id);
		if (u && isCommandLive(c.state)) {
			c.cpuCores = u.cpuCores;
			c.memoryBytes = u.memoryBytes;
			c.swapBytes = u.swapBytes;
			c.droppedOutputBytes = u.droppedOutputBytes;
			if (c.state === CommandState.QUEUED && u.reason) c.reason = u.reason;
		}
	}
	const bases = new Map(h.bases.map((u) => [u.id, u]));
	for (const b of v.bases) {
		const u = bases.get(b.id);
		if (u) {
			b.sizeBytes = u.sizeBytes;
			if (b.state === BaseState.BUILDING) b.progress = u.progress;
		}
	}
	const wss = new Map(h.workspaces.map((u) => [u.id, u]));
	for (const w of v.workspaces) {
		const u = wss.get(w.id);
		if (u) {
			w.exclusiveBytes = u.exclusiveBytes;
			w.sharedBytes = u.sharedBytes;
		}
	}
}
