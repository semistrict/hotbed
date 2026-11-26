import { create } from "@bufbuild/protobuf";
import {
	ActorKind,
	BaseState,
	CommandState,
	DaemonLogLevel,
	HostedProjectSchema,
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
	type HostedProject,
	type Node,
	type Project,
	type Repo,
	type Session,
	type Stamp,
	type Workspace,
} from "./gen/hotbed/v1/hotbed_pb.ts";
import type { CallerIdentity } from "./auth.ts";
import type { BuildDistributor } from "./builds.ts";
import type { CommandStoreClient } from "./command-do.ts";
import { ErrorCode, codeOf, hotbedError } from "./errors.ts";
import { canonicalRemote, projectNameFor } from "./gitops.ts";
import { HostUsageState, mergeHostAdmission } from "./hostusage.ts";
import type { NodeRow, NodeView } from "./placement.ts";
import type { QuestionBridgeHub } from "./question-bridge.ts";
import { ONLINE_WINDOW_MS, StandbyState, Wakeups, isCommandLive } from "./standby.ts";
import { ControllerStore, LOST_DETAIL, sessionEndedError, type WorkspaceRow } from "./store.ts";
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
	readonly power = new StandbyState(() => this.now());
	readonly hostUsages = new HostUsageState();
	readonly sessionWakes = new Wakeups();
	readonly creatingWS = new Map<string, { promise: Promise<void>; resolve: () => void }>();
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
		const secs = this.store.getWorkerConfig("")?.controller?.sessionIdleTimeoutSeconds;
		if (secs !== undefined && secs >= 60) {
			return secs * 1000;
		}
		return SESSION_IDLE_MS;
	}

	/** Per-org offline node grace period in milliseconds (default 24h). */
	offlineGraceMs(): number {
		const secs = this.store.getWorkerConfig("")?.controller?.offlineGraceSeconds;
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
		n.asleep = this.power.asleep(n, nowMs);
		if (n.online && !n.asleep) {
			const hu = this.hostUsages.get(n.hostname, nowMs);
			if (hu) {
				n.hostUsage = hu;
				n.admission = mergeHostAdmission(n.admission, n.usage, hu, nowMs);
			}
		}
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
			return this.addrOf(this.store.getNode(nodeId));
		} catch {
			return "";
		}
	}

	findNodeByNameOrId(name: string): NodeRow {
		for (const r of this.store.listNodes()) {
			if (r.node.hostname === name || r.node.id === name) {
				return r;
			}
		}
		throw hotbedError(ErrorCode.NotFound, `no node is called ${name}`).withSuggestion(
			"hotbed nodes lists the nodes",
		);
	}

	findNodeByHost(host: string): NodeRow {
		for (const r of this.store.listNodes()) {
			if (r.node.hostname === host) return r;
		}
		throw hotbedError(ErrorCode.NotFound, `no node is called ${host}`).withSuggestion(
			"hotbed nodes lists the nodes; a worker joins with hotbed worker init",
		);
	}

	follows(arch: string): number {
		if (!this.builds.buildFor("linux", arch)) return 0;
		return this.builds.currentBuildNumber();
	}

	nodeServes(row: NodeRow): boolean {
		const node = this.withOnline(row.node);
		if (!node.online || node.asleep) return false;
		const want = this.follows(row.hb?.resources?.arch ?? "");
		if (want === 0) return true;
		const v = row.hb?.resources?.version ?? "";
		const m = /^build\s+(\d+)\b/.exec(v.trim());
		return (m ? Number(m[1]) : 0) >= want;
	}

	views(): NodeView[] {
		const rows = this.store.listNodes();
		const all = this.store.allViews();
		const sessIds: string[] = [];
		for (const v of all.values()) {
			for (const w of v.workspaces) if (w.sessionId) sessIds.push(w.sessionId);
			for (const c of v.commands) if (c.sessionId) sessIds.push(c.sessionId);
		}
		const sessProj = this.store.sessionProjectsFor(sessIds);
		const nowMs = this.now();

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
				wakeFailed: this.power.wakeFailed(node, nowMs),
				bases: v.bases,
				workspaces: v.workspaces,
				commands: v.commands,
			};
		});
	}

	/** Calls online workers with active WebSocket tunnels in parallel for live commands. */
	async liveNodeCommands(
		nvs: NodeView[],
		nodeIds: Set<string> | null,
		sessionId: string,
	): Promise<Map<string, Command[]>> {
		const targets = nvs.filter(
			(nv) =>
				nv.online && this.tunnel.isConnected(nv.node.id) && (!nodeIds || nodeIds.has(nv.node.id)),
		);
		const live = new Map<string, Command[]>();
		if (targets.length === 0) return live;

		await Promise.all(
			targets.map(async (nv) => {
				try {
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
					live.set(nv.node.id, res.commands);
				} catch {
					// Fall back to cached node_state view
				}
			}),
		);

		const sessIds: string[] = [];
		for (const cmds of live.values()) {
			for (const cmd of cmds) if (cmd.sessionId) sessIds.push(cmd.sessionId);
		}
		const sessProj = this.store.sessionProjectsFor(sessIds);
		for (const cmds of live.values()) {
			for (const cmd of cmds) {
				const p = sessProj.get(cmd.sessionId);
				if (p) cmd.project = p;
			}
		}
		return live;
	}

	/** Calls online workers with active WebSocket tunnels in parallel for live workspaces. */
	async liveNodeWorkspaces(
		nvs: NodeView[],
		nodeIds: Set<string> | null,
	): Promise<Map<string, Workspace[]>> {
		const targets = nvs.filter(
			(nv) =>
				nv.online && this.tunnel.isConnected(nv.node.id) && (!nodeIds || nodeIds.has(nv.node.id)),
		);
		const live = new Map<string, Workspace[]>();
		if (targets.length === 0) return live;

		await Promise.all(
			targets.map(async (nv) => {
				try {
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
					live.set(nv.node.id, res.workspaces);
				} catch {
					// Fall back to cached node_state view
				}
			}),
		);

		const sessIds: string[] = [];
		for (const wss of live.values()) {
			for (const ws of wss) if (ws.sessionId) sessIds.push(ws.sessionId);
		}
		const sessProj = this.store.sessionProjectsFor(sessIds);
		for (const wss of live.values()) {
			for (const ws of wss) {
				const p = sessProj.get(ws.sessionId);
				if (p) ws.project = p;
			}
		}
		return live;
	}

	/** Projects enriched with live session, schedule, question, and review counts. */
	projects(): Project[] {
		const ps = this.store.listProjects();
		const live = this.store.liveSessionsByProject();
		const scheds = this.store.schedulesByProject();
		const qps = this.store.questionProjects();
		const qByName = new Map(qps.map((q) => [q.name, q]));
		const { active, toDecide } = this.store.reviewCounts();
		const memCounts = this.store.pendingMemoriesCounts();
		for (const p of ps) {
			p.liveSessions = live.get(p.name) ?? 0;
			p.schedules = scheds.get(p.name) ?? 0;
			p.activeReviews = active.get(p.name) ?? 0;
			p.reviewsToDecide = toDecide.get(p.name) ?? 0;
			p.pendingMemories = memCounts.byProject.get(p.name) ?? 0;
			const qp = qByName.get(p.name);
			if (qp) {
				p.openQuestions = qp.open;
				p.outbox = qp.outbox;
			}
		}
		return ps;
	}

	hostedOf(p: Project, repos: Repo[]): { hosted: HostedProject; missing: string[] } {
		const h = create(HostedProjectSchema, {
			name: p.name,
			steps: p.steps,
			scratchEpoch: p.scratchEpoch,
		});
		const missing: string[] = [];
		for (const remote of p.repos) {
			const match = repos.find((r) => canonicalRemote(r.url) === remote);
			if (!match) missing.push(remote);
			else h.repos.push(match);
		}
		return { hosted: h, missing };
	}

	hosted(p: Project): HostedProject {
		const { hosted, missing } = this.hostedOf(p, this.store.listRepos());
		if (missing.length > 0) {
			throw hotbedError(
				ErrorCode.NotFound,
				`project ${p.name} holds ${missing.join(", ")}, which no repo of the cluster has`,
			).withSuggestion(
				`hotbed repo add NAME ${missing[0]} -p ${p.name} registers it; or hotbed project rm-repo ${p.name} --repo ${missing[0]}`,
			);
		}
		if (hosted.repos.length === 0) {
			throw hotbedError(
				ErrorCode.NotFound,
				`project ${p.name} holds no repos to check out`,
			).withSuggestion(
				`hotbed project add ${p.name} --repo REMOTE, then hotbed repo add NAME REMOTE`,
			);
		}
		return hosted;
	}

	hostingFor(nodeId: string, hostname: string): { projects: HostedProject[]; release: string[] } {
		const projects = this.store.listProjects();
		const repos = this.store.listRepos();
		const out: HostedProject[] = [];
		for (const p of projects) {
			if (p.hosts.length > 0 && !p.hosts.includes(hostname)) continue;
			const { hosted, missing } = this.hostedOf(p, repos);
			if (missing.length === 0 && hosted.repos.length > 0) out.push(hosted);
		}
		return { projects: out, release: this.store.toRelease(nodeId) };
	}

	repoById(id: string): Repo {
		const r = this.store.listRepos().find((x) => x.id === id);
		if (!r) {
			throw hotbedError(ErrorCode.NotFound, `no repo ${id}`).withSuggestion("hotbed repo ls");
		}
		return r;
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
		const sess = this.store.liveSession(sessionId, this.now());
		const project = this.store.getProject(sess.project);
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

	listedProject(req: ScopedRequest): string {
		const sess = req.sessionId ? this.store.getSession(req.sessionId) : null;
		if (req.scope.case === "allProjects" && req.scope.value) return "";
		if (req.scope.case === "project" && req.scope.value) {
			this.store.getProject(req.scope.value);
			return req.scope.value;
		}
		if (!sess && req.remote) {
			const remote = canonicalRemote(req.remote);
			const holding = this.store.projectsHolding(remote);
			if (holding.length === 1) return holding[0].name;
			if (holding.length === 0) {
				throw hotbedError(
					ErrorCode.NotFound,
					`say which project: no project holds ${remote}, this checkout's repo`,
				).withSuggestion("pass -p PROJECT or --all-projects (hotbed project ls)");
			}
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`say which project: ${remote}, this checkout's repo, is in projects ${holding.map((p) => p.name).join(", ")}`,
			).withSuggestion("pass -p PROJECT or --all-projects");
		}
		if (!sess) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				"say which project: name one, all of them, or the session whose project it is",
			).withSuggestion("pass -p PROJECT, --all-projects, or -s SESSION");
		}
		if (!sess.project) {
			throw hotbedError(ErrorCode.InvalidArgs, `session ${sess.id} has no project`).withSuggestion(
				"pass -p PROJECT or --all-projects",
			);
		}
		return sess.project;
	}

	sessionProject(
		caller: CallerIdentity,
		name: string,
		remote: string,
	): { project: Project; changed: boolean } {
		if (name) {
			return { project: this.store.getProject(name), changed: false };
		}
		if (!remote) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				"a session works on a project: outside a git checkout, name it",
			).withSuggestion("pass -p PROJECT or set HOTBED_PROJECT (hotbed project ls)");
		}
		const holding = this.store.projectsHolding(remote);
		if (holding.length === 1) return { project: holding[0], changed: false };
		if (holding.length > 1) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`${remote} is in projects ${holding.map((p) => p.name).join(", ")}: name the session's project`,
			).withSuggestion("pass -p PROJECT or set HOTBED_PROJECT");
		}
		const own = projectNameFor(remote);
		if (!own) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`no project holds ${remote}, and its name makes none`,
			).withSuggestion(`make one: hotbed project add NAME --repo ${remote}`);
		}
		try {
			const existing = this.store.getProject(own);
			if (existing.repos.length > 0) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`no project holds ${remote}, and project ${own} holds other repos (${existing.repos.join(", ")})`,
				).withSuggestion(
					`add it there (hotbed project add ${own} --repo ${remote}), or pass -p PROJECT`,
				);
			}
		} catch (err) {
			if (codeOf(err) !== ErrorCode.NotFound) throw err;
		}
		const created = this.store.addProject(
			own,
			[remote],
			this.madeBy(caller, `for ${remote}, which a session started in and no project held`),
		);
		return { project: created, changed: true };
	}

	ensureRepoProject(caller: CallerIdentity, repoId: string, repoUrl: string): Project | null {
		const remote = canonicalRemote(repoUrl);
		if (!remote) return null;
		const holding = this.store.projectsHolding(remote);
		if (holding.length === 1) return holding[0];
		if (holding.length > 1) return null;

		const candidates: string[] = [];
		const own = projectNameFor(remote);
		if (own) candidates.push(own);
		if (repoId && candidates[0] !== repoId && PROJECT_NAME_RE.test(repoId)) {
			candidates.push(repoId);
		}
		const base = candidates[0] ?? "repo";
		for (let n = 2; n <= 99; n++) {
			const suffix = `-${n}`;
			let prefix = base;
			if (prefix.length + suffix.length > 40) {
				prefix = prefix.slice(0, 40 - suffix.length).replace(/-+$/, "");
			}
			candidates.push(prefix + suffix);
		}
		for (const name of candidates) {
			try {
				const existing = this.store.getProject(name);
				if (existing.repos.length > 0) continue;
			} catch (err) {
				if (codeOf(err) !== ErrorCode.NotFound) throw err;
			}
			return this.store.addProject(
				name,
				[remote],
				this.madeBy(caller, `for registered repo ${repoId}, which no project held`),
			);
		}
		throw hotbedError(
			ErrorCode.Conflict,
			`could not find a free project name for repo ${repoId}`,
		).withSuggestion("pass -p PROJECT to place the repo in an explicit project");
	}

	async releaseWorkspace(id: string): Promise<void> {
		const w = this.store.getWorkspace(id);
		const row = this.store.getNode(w.node);
		const node = this.withOnline(row.node);
		if (node.online && !node.asleep && this.tunnel.isConnected(w.node)) {
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
			throw hotbedError(
				ErrorCode.WorkerUnreachable,
				`${row.node.hostname} is ${node.asleep ? "asleep" : "offline"}`,
				{ retryable: true },
			);
		}
		this.store.releaseWorkspaceRow(id, this.now());
	}

	async endSession(id: string, reason: string, requireLive = false): Promise<Session> {
		return this.withSessionLock(id, async () => {
			const sess = this.store.getSession(id);
			if (requireLive && sess.endedAt !== undefined) {
				throw sessionEndedError(id, sess.endReason);
			}
			const wss = this.store.endSessionAndWorkspaces(id, reason, this.now());
			return this.finishEndSession(id, reason, wss);
		});
	}

	async finishEndSession(id: string, reason: string, wss: WorkspaceRow[]): Promise<Session> {
		const runs = this.store.listLiveScheduleRunsBySession(id);
		const ended = BigInt(this.now());
		for (const run of runs) {
			run.outcome = RunOutcome.CANCELLED;
			run.reason = `its session ended: ${reason}`;
			run.endedAt = ended;
			this.store.updateLiveScheduleRun(run);
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
				this.store.logDaemon(
					DaemonLogLevel.WARN,
					"release workspace on session end",
					{ workspace: ws.id, session: id, err: String(err) },
					this.now(),
				);
			}
		}
		this.sessionWakes.wake(id);
		return this.store.getSession(id);
	}

	loseRuns(commandIds: string[]): void {
		if (commandIds.length === 0) return;
		const set = new Set(commandIds);
		const ended = BigInt(this.now());
		for (const run of this.store.listAllLiveScheduleRuns()) {
			if (!run.commandId || !set.has(run.commandId)) continue;
			run.outcome = RunOutcome.CANCELLED;
			run.reason = LOST_DETAIL;
			run.endedAt = ended;
			this.store.updateLiveScheduleRun(run);
		}
	}

	async settleRuns(): Promise<void> {
		const toEnd: Array<{ id: string; reason: string }> = [];
		for (const run of this.store.listAllLiveScheduleRuns()) {
			if (!run.commandId) continue;
			const f = this.store.finishedCommand(run.commandId);
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
				this.store.touchSession(run.sessionId, this.now());
			}
			const updated = this.store.updateLiveScheduleRun(run);
			if (updated && run.outcome === RunOutcome.SUCCEEDED) {
				toEnd.push({ id: run.sessionId, reason: "its scheduled run succeeded" });
			}
		}
		for (const e of toEnd) {
			try {
				await this.endSession(e.id, e.reason);
			} catch (err) {
				this.store.logDaemon(
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
		this.hostUsages.prune(nowMs);
		const idleMs = this.sessionIdleMs();
		const graceMs = this.offlineGraceMs();
		const cutoff = nowMs - idleMs;
		const ids = this.store.idleSessions(cutoff);
		if (ids.length === 0) return;
		const rows = this.store.listNodes();
		const offline = new Set<string>();
		for (const r of rows) {
			const recent = r.node.lastSeen !== undefined && nowMs - Number(r.node.lastSeen) < graceMs;
			const node = this.withOnline(r.node);
			if (!node.online && !node.asleep && recent) offline.add(node.id);
		}
		const idleSec = Math.round(idleMs / 1000);
		const hrs = Math.floor(idleSec / 3600);
		const mins = Math.floor((idleSec % 3600) / 60);
		const secs = idleSec % 60;
		const idleLabel =
			hrs > 0 ? `${hrs}h${mins}m${secs}s` : mins > 0 ? `${mins}m${secs}s` : `${secs}s`;
		for (const id of ids) {
			await this.withSessionLock(id, async () => {
				const sess = this.store.tryGetSession(id);
				if (!sess || sess.endedAt !== undefined) return;
				if (sess.workspaces.some((w) => w.releasedAt === undefined && offline.has(w.nodeId))) {
					return;
				}
				const reason = `abandoned: nothing called it for ${idleLabel}`;
				const { ended, workspaces } = this.store.endSessionIfIdleAndWorkspaces(
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
			const stored = Number(this.store.getMeta("last_prune_at") ?? "0");
			if (Number.isFinite(stored) && stored > 0) {
				this.lastPruneAt = stored;
			}
		}
		if (nowMs - this.lastPruneAt < 10 * 60_000) return;
		this.lastPruneAt = nowMs;
		this.store.setMeta("last_prune_at", String(nowMs));
		this.store.prune(nowMs);
	}
}

export function setupStepsReminder(p?: Project | null): string {
	const standard =
		"By default, after all setup steps have run, common dev commands (build, test, check, lint, etc.) should run without downloading or compiling anything.";
	if (!p || !p.name) {
		return (
			"Reminder: add setup steps (`hotbed step ls -p PROJECT`, `hotbed step add NAME -- ...`, `hotbed step set -p PROJECT NAME -- ...`; use `--every-pull` for dependency installs and builds) for common things like toolchain installs, dependencies, and pre-builds. " +
			standard
		);
	}
	if (p.steps.length === 0) {
		return `Reminder: project ${p.name} has no setup steps yet. Add setup steps (\`hotbed step add NAME -- ...\` or \`hotbed step set -p ${p.name} NAME -- ...\`; use \`--every-pull\` for dependency installs and builds) for common things like toolchain installs, dependencies, and pre-builds. ${standard}`;
	}
	return `Reminder: make sure project ${p.name} has setup steps (\`hotbed step ls -p ${p.name}\`, \`hotbed step add\`, \`hotbed step set\`; use \`--every-pull\` for dependency installs and builds) for common things like toolchain installs, dependencies, and pre-builds. ${standard}`;
}

// Where a lesson goes. Every always-on memory costs every session's context,
// so the reminders below send a lesson to the place it will be found first
// (its fix, the repo, a skill) and to memory only when it has no such home.
const LESSON_HOMES =
	'fix its cause, or write it where it will be found (the repo\'s AGENTS.md, a comment by the code, the tool\'s skill); save a memory only for what has no such home (a preference, a machine fact, a workaround naming its open bug): `hotbed memory ls -q WORD` for one to update, else `hotbed memory add --slug SLUG --summary "..." --rationale "..."` (MCP: `memory_add`).';

export const MEMORY_FAILURE_REMINDER = `Reminder: if this failure revealed a reusable fix or quirk, ${LESSON_HOMES}`;

export const MEMORY_FEEDBACK_REMINDER = `Reminder: if this user answer or review feedback reflects a standing preference or convention, ${LESSON_HOMES}`;

export const MEMORY_FIX_REMINDER = `Reminder: if this defect exposed a reusable pitfall or invariant, ${LESSON_HOMES}`;

export const MEMORY_SESSION_END_REMINDER = `Before wrapping up: if this session taught something reusable, ${LESSON_HOMES} Archive memories it made obsolete with \`hotbed memory archive ID --reason "..."\`.`;

export function memorySavingReminder(): string {
	return `Lessons: ${LESSON_HOMES} Archive obsolete memories with \`hotbed memory archive ID --reason "..."\`.`;
}

/** How an agent reports a problem with hotbed itself, once, and counts its own hitting of one already reported. */
export const ISSUE_REMINDER =
	"Hotbed itself failing you (a bug, a missing capability, a misleading error)? `hotbed issue ls -q WORD`: +1 one already reported (`hotbed issue upvote ID -m WHAT`), else `hotbed issue report TITLE -m DETAILS`.";

export function sessionStartReminder(p?: Project | null): string {
	return `${setupStepsReminder(p)}\n${memorySavingReminder()}\n${ISSUE_REMINDER}`;
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
