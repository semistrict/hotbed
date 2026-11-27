import { DurableObject } from "cloudflare:workers";
import { create, toJson } from "@bufbuild/protobuf";
import { drizzle, type DrizzleSqliteDODatabase } from "drizzle-orm/durable-sqlite";
import { migrate } from "drizzle-orm/durable-sqlite/migrator";
import migrations from "../drizzle/migrations.js";
import * as dbSchema from "./db/schema.ts";
import {
	BaseSchema,
	BoardService,
	ClusterService,
	CommandProfileSchema,
	CommandSchema,
	DaemonLogLevel,
	FinishedCommandSchema,
	GitAuthService,
	MemoryService,
	QuestionService,
	ReviewService,
	IssueService,
	RunOutcome,
	ScheduledRunSchema,
	ScheduleService,
	SessionEventSchema,
	SessionSchema,
	SessionService,
	StampSchema,
	WorkspaceSchema,
	WorkspaceService,
} from "./gen/hotbed/v1/hotbed_pb.ts";
import {
	authenticateRequest,
	INTERNAL_DEFAULT_DO_HEADER,
	INTERNAL_GLOBAL_ADMIN_HEADER,
	INTERNAL_ORG_HEADER,
	isOrgAdminCaller,
	sha256Hex,
	timingSafeStringEqual,
	type AuthEnv,
	type CallerIdentity,
} from "./auth.ts";
import { formatOrgToken, isWorkOSEnabled } from "./workos.ts";
import { BuildDistributor, type BuildsEnv, type TestBuildConfig } from "./builds.ts";
import { CommandStoreClient, type CommandDO } from "./command-do.ts";
import { createConnectFetchDispatcher, type ConnectFetchDispatcher } from "./connect-adapter.ts";
import { ControllerContext } from "./context.ts";
import { decodePathSegment } from "./encoding.ts";
import { connectErrorResponse, ErrorCode, HotbedError, fromUnknownError } from "./errors.ts";
import { randomHex } from "./gitops.ts";
import { createBoardService, type BoardServiceBundle } from "./services/board.ts";
import { createClusterService } from "./services/cluster.ts";
import { createGitAuthService } from "./services/gitauth.ts";
import { createMemoryService } from "./services/memories.ts";
import { createQuestionService } from "./services/questions.ts";
import { createReviewService } from "./services/reviews.ts";
import { createIssueService } from "./services/issues.ts";
import { createScheduleService, type ScheduleRunner } from "./services/schedules.ts";
import { createSessionService } from "./services/sessions.ts";
import { createWorkspaceService } from "./services/workspaces.ts";
import { ControllerStore } from "./store.ts";
import { QuestionBridgeHub } from "./question-bridge.ts";
import type { TenantRegistryDO } from "./tenant-registry-do.ts";
import { WorkerTunnelHub } from "./tunnel.ts";

export interface ControllerEnv extends AuthEnv, BuildsEnv {
	CONTROLLER: DurableObjectNamespace<ControllerDO>;
	TENANT_REGISTRY?: DurableObjectNamespace<TenantRegistryDO>;
	COMMAND: DurableObjectNamespace<CommandDO>;
	CLUSTER_ID?: string;
	CONTROLLER_NAME?: string;
	ALLOW_LOCAL_REPOS?: string;
	TEST_HARNESS?: string;
	ASSETS?: Fetcher;
	GC_WORKER?: Fetcher;
	GC_WORKER_URL?: string;
	GC_TOKEN?: string;
}

export interface ControllerHealth {
	ok: true;
	clusterId: string;
	orgId?: string;
	cladefsTenantId?: number;
	workosEnabled: boolean;
	version: string;
	build: number;
	commit: string;
	connectedWorkers: number;
}

export type RedeemSignupCodeResult =
	| { ok: true; alreadyApproved?: boolean }
	| { ok: false; code: string; message: string; status: number };

export type ConsumeHandoffResult =
	| { ok: true }
	| { ok: false; code: string; message: string; status: number };

export type RevokeUserApiTokensResult =
	| { ok: true; revoked: number }
	| { ok: false; code: string; message: string; status: number };

export type ApiTokenCheckResult =
	| {
			ok: true;
			login: string;
			userId?: string;
			orgId?: string;
			cladefsTenantId?: number;
			orgRole?: string;
			authMethod: string;
	  }
	| {
			ok: false;
			status: number;
			error: { code: string; message: string; suggestion?: string; retryable?: boolean };
	  };

const ACTIVE_ALARM_MS = 15_000;
const IDLE_ALARM_MS = 60_000;
const CLUSTER_ID_META_KEY = "cluster_id";
const ORG_ID_META_KEY = "org_id";
const CLADEFS_TENANT_ID_META_KEY = "cladefs_tenant_id";
const CLADEFS_TENANT_ORG_KEY_META_KEY = "cladefs_tenant_org_key";
const TENANT_REGISTRY_DO_NAME = "global";

export class ControllerDO extends DurableObject<ControllerEnv> {
	private db!: DrizzleSqliteDODatabase<typeof dbSchema>;
	private store!: ControllerStore;
	private tunnel!: WorkerTunnelHub;
	private questionBridge!: QuestionBridgeHub;
	private builds!: BuildDistributor;
	private ctrl!: ControllerContext;
	private scheduleRunner!: ScheduleRunner;
	private boardSvc!: BoardServiceBundle;
	private dispatcher!: ConnectFetchDispatcher;
	private nowOffsetMs = 0;
	private testWakeWaitMs?: number;
	private testAllowLocalRepos?: boolean;
	private testBuildConfig?: TestBuildConfig;

	constructor(ctx: DurableObjectState, env: ControllerEnv) {
		super(ctx, env);
		void this.ctx.blockConcurrencyWhile(async () => {
			// Auto-run bundled Drizzle Kit migrations before any request or alarm is delivered.
			this.db = drizzle(this.ctx.storage, { schema: dbSchema });
			await migrate(this.db, migrations);

			this.store = new ControllerStore(this.ctx.storage, this.db);
			this.store.autoApproveLegacyProposedMemories();
			this.tunnel = new WorkerTunnelHub(this.ctx);
			this.questionBridge = new QuestionBridgeHub(this.ctx);
			this.initRuntime();
		});
	}

	private initRuntime(): void {
		this.builds = new BuildDistributor(this.env, this.store, (p) => this.ctx.waitUntil(p));
		if (this.testBuildConfig) {
			this.builds.setTestConfig(this.testBuildConfig);
		}

		const orgId = this.store.getMeta(ORG_ID_META_KEY) ?? "";
		const defaultOrgId = (this.env.DEFAULT_ORG_ID ?? "").trim();
		const allowEnvClusterId = !orgId || (defaultOrgId !== "" && orgId === defaultOrgId);
		let clusterId = allowEnvClusterId ? (this.env.CLUSTER_ID ?? "").trim() : "";
		if (!clusterId) {
			clusterId = this.store.getMeta(CLUSTER_ID_META_KEY) ?? "";
			if (!clusterId) {
				clusterId = randomHex(8);
				this.store.setMeta(CLUSTER_ID_META_KEY, clusterId);
			}
		}
		const rawTenant = this.store.getMeta(CLADEFS_TENANT_ID_META_KEY);
		const cladefsTenantId =
			rawTenant !== null && /^\d+$/.test(rawTenant) ? BigInt(rawTenant) : null;
		const controllerName = (this.env.CONTROLLER_NAME ?? "").trim() || "cloudflare-do";
		const allowLocalRepos =
			this.testAllowLocalRepos ?? (this.env.ALLOW_LOCAL_REPOS ?? "").trim() === "1";

		this.ctrl = new ControllerContext({
			store: this.store,
			commands: new CommandStoreClient(this.env.COMMAND, clusterId),
			tunnel: this.tunnel,
			questionBridge: this.questionBridge,
			builds: this.builds,
			clusterId,
			orgId,
			cladefsTenantId,
			controllerName,
			allowLocalRepos,
		});
		this.ctrl.nowFn = () => Date.now() + this.nowOffsetMs;
		const initialSignupCode = (this.env.INITIAL_SIGNUP_CODE ?? "").trim();
		if (initialSignupCode) {
			this.store.seedInitialSignupCode(initialSignupCode, this.ctrl.now());
		}
		if (this.testWakeWaitMs !== undefined) {
			this.ctrl.power.wakeWaitMs = this.testWakeWaitMs;
		}

		const wsSvc = createWorkspaceService(this.ctrl);
		const schedSvc = createScheduleService(this.ctrl, wsSvc.helpers);
		this.scheduleRunner = schedSvc.runner;
		this.boardSvc = createBoardService(this.ctrl, this.ctx.storage);
		this.ctrl.boardCountsFn = () => this.boardSvc.store.projectBoardCounts(this.ctrl.now());
		const clusterSvc = createClusterService(this.ctrl, schedSvc.runner);
		const sessionSvc = createSessionService(this.ctrl);
		const questionSvc = createQuestionService(this.ctrl);
		const memorySvc = createMemoryService(this.ctrl);
		const reviewSvc = createReviewService(this.ctrl);
		const issueSvc = createIssueService(this.ctrl);
		const gitAuthSvc = createGitAuthService(this.ctrl);

		this.dispatcher = createConnectFetchDispatcher((router) => {
			router.service(ClusterService, clusterSvc);
			router.service(SessionService, sessionSvc);
			router.service(WorkspaceService, wsSvc.impl);
			router.service(QuestionService, questionSvc);
			router.service(MemoryService, memorySvc);
			router.service(ReviewService, reviewSvc);
			router.service(IssueService, issueSvc);
			router.service(ScheduleService, schedSvc.impl);
			router.service(GitAuthService, gitAuthSvc);
			router.service(BoardService, this.boardSvc.impl);
		});
	}

	private desiredAlarmDelayMs(): number | null {
		const hasActiveWork =
			this.store.listSchedules("").some((s) => !s.paused) ||
			this.store.liveSessionsByProject().size > 0 ||
			this.boardSvc.store.hasPendingWork(this.ctrl.now());
		if (hasActiveWork) return ACTIVE_ALARM_MS;
		const hasIdleWork = this.store.listNodes().length > 0 || this.builds.needsBackgroundRefresh();
		return hasIdleWork ? IDLE_ALARM_MS : null;
	}

	private async ensureAlarm(): Promise<void> {
		const delay = this.desiredAlarmDelayMs();
		if (delay === null) return;
		const nowWall = Date.now();
		const desiredAt = nowWall + delay;
		const existing = await this.ctx.storage.getAlarm();
		if (existing !== null && existing > nowWall && existing <= desiredAt + 1000) {
			return;
		}
		await this.ctx.storage.setAlarm(desiredAt);
	}

	async runMaintenance(): Promise<void> {
		const steps: Array<{ name: string; level: DaemonLogLevel; fn: () => Promise<void> | void }> = [
			{ name: "reap idle sessions", level: DaemonLogLevel.ERROR, fn: () => this.ctrl.reapIdle() },
			{
				name: "tick schedules",
				level: DaemonLogLevel.ERROR,
				fn: () => this.scheduleRunner.tickSchedules(),
			},
			{
				name: "tick board",
				level: DaemonLogLevel.ERROR,
				fn: () => this.boardSvc.tickBoard(),
			},
			{
				name: "settle scheduled runs",
				level: DaemonLogLevel.ERROR,
				fn: () => this.ctrl.settleRuns(),
			},
			{
				name: "refresh GitHub builds",
				level: DaemonLogLevel.WARN,
				fn: async () => {
					await this.builds.refreshIfStale(this.ctrl.now());
				},
			},
			{
				name: "prune controller store",
				level: DaemonLogLevel.ERROR,
				fn: () => this.ctrl.maybePrune(),
			},
		];
		for (const step of steps) {
			try {
				await step.fn();
			} catch (err) {
				this.store.logDaemon(
					step.level,
					step.name,
					{ err: err instanceof Error ? err.message : String(err) },
					this.ctrl.now(),
				);
			}
		}
	}

	override async alarm(): Promise<void> {
		try {
			await this.runMaintenance();
		} finally {
			const delay = this.desiredAlarmDelayMs();
			if (delay !== null) {
				await this.ctx.storage.setAlarm(Date.now() + delay);
			}
		}
	}

	override webSocketMessage(ws: WebSocket, message: string | ArrayBuffer): void {
		if (this.questionBridge.isBridgeSocket(ws)) {
			this.questionBridge.handleMessage(ws, message, () => this.store.listT3OutboxQuestions());
			return;
		}
		this.tunnel.handleMessage(ws, message);
	}

	override webSocketClose(ws: WebSocket, code: number, reason: string, _wasClean: boolean): void {
		if (this.questionBridge.isBridgeSocket(ws)) {
			this.questionBridge.handleClose(ws, code, reason);
			return;
		}
		this.tunnel.handleClose(ws, code, reason);
	}

	override webSocketError(ws: WebSocket, error: unknown): void {
		const msg = error instanceof Error ? error.message : String(error);
		if (this.questionBridge.isBridgeSocket(ws)) {
			this.questionBridge.handleClose(ws, 1011, msg);
			return;
		}
		this.tunnel.handleClose(ws, 1011, msg);
	}

	private checkOrgMismatch(request: Request): Response | null {
		const reqOrgId = (request.headers.get(INTERNAL_ORG_HEADER) ?? "").trim();
		if (reqOrgId && this.ctrl.orgId && this.ctrl.orgId !== reqOrgId) {
			return Response.json(
				{
					code: ErrorCode.NotAuthorized,
					message: `organization mismatch: this controller DO belongs to ${this.ctrl.orgId}, not ${reqOrgId}`,
				},
				{ status: 403 },
			);
		}
		return null;
	}

	private async ensureCladefsTenantId(isDefaultHint: boolean): Promise<bigint | null> {
		const defaultOrgId = (this.env.DEFAULT_ORG_ID ?? "").trim();
		const isDefault =
			isDefaultHint ||
			(Boolean(defaultOrgId) && this.ctrl.orgId === defaultOrgId) ||
			(!this.ctrl.orgId && !defaultOrgId) ||
			this.store.hasLegacyTenantZeroData();
		const orgKey = this.ctrl.orgId || (isDefault ? "default" : "");
		if (!orgKey) {
			return this.ctrl.cladefsTenantId;
		}
		const recordedOrgKey = this.store.getMeta(CLADEFS_TENANT_ORG_KEY_META_KEY) ?? "";
		if (this.ctrl.cladefsTenantId !== null) {
			if (recordedOrgKey === orgKey || !this.env.TENANT_REGISTRY) {
				return this.ctrl.cladefsTenantId;
			}
			// If we initially claimed tenant 0 under the legacy "default" placeholder
			// before org_id was bound, promote the global registry row to orgId now.
			if (!(recordedOrgKey === "default" && this.ctrl.cladefsTenantId === 0n && isDefault)) {
				return this.ctrl.cladefsTenantId;
			}
		}
		if (!this.env.TENANT_REGISTRY) {
			return this.ctrl.cladefsTenantId;
		}
		// Never share an in-flight DO RPC promise across requests: workerd rejects
		// awaiting I/O created in another request's context. TenantRegistryDO.assignTenant
		// is transactional and idempotent, and once persisted to SQLite meta all
		// subsequent requests return synchronously from memory.
		const registry = this.env.TENANT_REGISTRY.getByName(TENANT_REGISTRY_DO_NAME);
		const assignedStr = await registry.assignTenant(orgKey, { isDefault });
		const assigned = BigInt(assignedStr);
		this.store.setMeta(CLADEFS_TENANT_ID_META_KEY, assignedStr);
		this.store.setMeta(CLADEFS_TENANT_ORG_KEY_META_KEY, orgKey);
		this.ctrl.cladefsTenantId = assigned;
		return assigned;
	}

	private async authRequest(request: Request, opts?: { allowJoinToken?: boolean }) {
		const reqOrgId = (request.headers.get(INTERNAL_ORG_HEADER) ?? "").trim();
		const isDefaultDo = request.headers.get(INTERNAL_DEFAULT_DO_HEADER) === "1";
		const caller = await authenticateRequest(request, this.env, this.store, {
			allowJoinToken: opts?.allowJoinToken,
			trustInternalHeader: true,
			nowMs: this.ctrl.now(),
			orgId: this.ctrl.orgId || reqOrgId,
		});
		// Only persist org_id after authentication succeeds so unauthenticated
		// requests with forged org-scoped tokens cannot poison an unbound DO.
		const effectiveOrgId = reqOrgId || caller.orgId;
		if (effectiveOrgId && !this.ctrl.orgId) {
			this.store.setMeta(ORG_ID_META_KEY, effectiveOrgId);
			this.ctrl.orgId = effectiveOrgId;
		}
		await this.ensureCladefsTenantId(isDefaultDo);
		return caller;
	}

	// ---- Typed Workers RPC methods ----------------------------------------

	getHealth(): ControllerHealth {
		return {
			ok: true,
			clusterId: this.ctrl.clusterId,
			orgId: this.ctrl.orgId || undefined,
			cladefsTenantId:
				this.ctrl.cladefsTenantId !== null ? Number(this.ctrl.cladefsTenantId) : undefined,
			workosEnabled: isWorkOSEnabled(this.env),
			version: this.builds.versionString(),
			build: this.builds.currentBuildNumber(),
			commit: this.builds.controllerCommit(),
			connectedWorkers: this.tunnel.connectedNodeIds().size,
		};
	}

	checkSignupApproved(userIdRaw: string): boolean {
		const userId = userIdRaw.trim();
		if (!userId) return false;
		return this.store.isUserSignupApproved(userId);
	}

	async redeemSignupCode(params: {
		userId: string;
		email: string;
		code: string;
	}): Promise<RedeemSignupCodeResult> {
		const userId = params.userId.trim();
		const email = params.email.trim();
		const submitted = params.code.trim().toUpperCase();
		if (!userId) {
			return { ok: false, code: ErrorCode.InvalidArgs, message: "userId is required", status: 400 };
		}
		if (this.store.isUserSignupApproved(userId)) {
			return { ok: true, alreadyApproved: true };
		}
		if (!submitted) {
			return {
				ok: false,
				code: ErrorCode.NotAuthorized,
				message: "Sign-up code is required.",
				status: 403,
			};
		}
		const active = this.store.listActiveSignupCodes();
		let matchedCode = "";
		for (const candidate of active) {
			if (await timingSafeStringEqual(submitted, candidate.code)) {
				matchedCode = candidate.code;
			}
		}
		if (!matchedCode) {
			return {
				ok: false,
				code: ErrorCode.NotAuthorized,
				message: "Invalid sign-up code.",
				status: 403,
			};
		}
		this.store.recordSignupRedemption(userId, email, matchedCode, this.ctrl.now());
		return { ok: true };
	}

	consumeCliHandoff(jtiRaw: string, expRaw: number): ConsumeHandoffResult {
		const jti = jtiRaw.trim();
		const exp = typeof expRaw === "number" && Number.isFinite(expRaw) ? expRaw : 0;
		if (!jti || jti.length > 128) {
			return { ok: false, code: ErrorCode.InvalidArgs, message: "jti is required", status: 400 };
		}
		const key = `cli_handoff:${jti}`;
		if (this.store.getMeta(key) !== null) {
			return {
				ok: false,
				code: ErrorCode.Conflict,
				message: "CLI handoff code already used",
				status: 409,
			};
		}
		this.store.setMeta(key, String(Math.max(exp, this.ctrl.now() + 120_000)));
		return { ok: true };
	}

	async revokeUserApiTokens(params: {
		caller: CallerIdentity;
		orgId: string;
		userId: string;
	}): Promise<RevokeUserApiTokensResult> {
		const reqOrgId = params.orgId.trim();
		if (reqOrgId && this.ctrl.orgId && this.ctrl.orgId !== reqOrgId) {
			return {
				ok: false,
				code: ErrorCode.NotAuthorized,
				message: `organization mismatch: this controller DO belongs to ${this.ctrl.orgId}, not ${reqOrgId}`,
				status: 403,
			};
		}
		if (!isOrgAdminCaller(params.caller)) {
			return {
				ok: false,
				code: ErrorCode.NotAuthorized,
				message: "organization admin required",
				status: 403,
			};
		}
		const effectiveOrgId = reqOrgId || params.caller.orgId;
		if (effectiveOrgId && !this.ctrl.orgId) {
			this.store.setMeta(ORG_ID_META_KEY, effectiveOrgId);
			this.ctrl.orgId = effectiveOrgId;
		}
		await this.ensureCladefsTenantId(false);
		const userId = params.userId.trim();
		if (!userId) {
			return { ok: false, code: ErrorCode.InvalidArgs, message: "userId is required", status: 400 };
		}
		const revoked = this.store.revokeApiTokensForUser(userId, this.ctrl.now());
		return { ok: true, revoked };
	}

	async checkApiToken(params: {
		bearer: string;
		orgId: string;
		isDefaultDo: boolean;
	}): Promise<ApiTokenCheckResult> {
		const reqOrgId = params.orgId.trim();
		if (reqOrgId && this.ctrl.orgId && this.ctrl.orgId !== reqOrgId) {
			return {
				ok: false,
				status: 403,
				error: {
					code: ErrorCode.NotAuthorized,
					message: `organization mismatch: this controller DO belongs to ${this.ctrl.orgId}, not ${reqOrgId}`,
				},
			};
		}
		try {
			const headers = new Headers({ Authorization: `Bearer ${params.bearer}` });
			if (reqOrgId) headers.set(INTERNAL_ORG_HEADER, reqOrgId);
			if (params.isDefaultDo) headers.set(INTERNAL_DEFAULT_DO_HEADER, "1");
			const caller = await this.authRequest(
				new Request("https://controller.internal/auth/check", { headers }),
			);
			if (caller.role !== "owner") {
				return {
					ok: false,
					status: 403,
					error: { code: ErrorCode.NotAuthorized, message: "Forbidden" },
				};
			}
			return {
				ok: true,
				login: caller.login,
				userId: caller.userId,
				orgId: caller.orgId || this.ctrl.orgId,
				cladefsTenantId:
					this.ctrl.cladefsTenantId !== null ? Number(this.ctrl.cladefsTenantId) : undefined,
				orgRole: caller.orgRole,
				authMethod: caller.authMethod,
			};
		} catch (err) {
			const he = fromUnknownError(err);
			return { ok: false, status: 401, error: he.toJSON() };
		}
	}

	/**
	 * Cloudflare Workers requires WebSocket upgrade responses (101 with webSocket)
	 * to return from DO `fetch()`. All other routes use typed Workers RPC methods
	 * (`getHealth`, `checkSignupApproved`, `redeemSignupCode`, `consumeCliHandoff`,
	 * `revokeUserApiTokens`, `checkApiToken`, `handleHttpRequest`).
	 */
	override async fetch(request: Request): Promise<Response> {
		const url = new URL(request.url);
		if (url.pathname === "/ws/worker" || url.pathname === "/ws/questions") {
			return this.handleWebSocketUpgrade(request, url.pathname);
		}
		if (url.pathname === "/ws/pipe") {
			return this.handlePipeUpgrade(request, url);
		}
		return new Response("Use typed Workers RPC methods on ControllerDO", { status: 405 });
	}

	/**
	 * /ws/pipe?workspace=ID&kind=ssh|port[&port=N]: a client's byte stream to
	 * the workspace's worker, relayed over the worker's tunnel: its shell
	 * server (hotbed shell's ssh; OpenShell's ticket admits it), or a port a
	 * command of the workspace serves on. Binary messages carry the bytes;
	 * one text message, {"type":"opened"}, says the worker's end is open; the
	 * socket closes when either end does, with why if it failed.
	 */
	private async handlePipeUpgrade(request: Request, url: URL): Promise<Response> {
		const mismatch = this.checkOrgMismatch(request);
		if (mismatch) return mismatch;
		if ((request.headers.get("Upgrade") ?? "").toLowerCase() !== "websocket") {
			return new Response("Expected Upgrade: websocket", { status: 426 });
		}
		let caller;
		try {
			caller = await this.authRequest(request);
		} catch (err) {
			return Response.json(fromUnknownError(err).toJSON(), { status: 403 });
		}
		if (caller.role === "worker" || caller.orgRole === "viewer") {
			return Response.json(
				{
					code: ErrorCode.NotAuthorized,
					message: "a worker or a viewer opens no pipes to workspaces",
				},
				{ status: 403 },
			);
		}
		const workspace = url.searchParams.get("workspace") ?? "";
		const kind = url.searchParams.get("kind") ?? "";
		const port = Number(url.searchParams.get("port") ?? "0");
		if ((kind !== "ssh" && kind !== "port") || (kind === "port" && !(port > 0 && port < 65536))) {
			return Response.json(
				{
					code: ErrorCode.InvalidArgs,
					message: "kind is ssh, or port with a port from 1 to 65535",
				},
				{ status: 400 },
			);
		}
		let nodeId: string;
		let host: string;
		try {
			const row = this.store.getWorkspace(workspace);
			if (row.released)
				throw new HotbedError(ErrorCode.NotFound, `workspace ${workspace} was released`);
			nodeId = row.node;
			host = this.store.getNode(nodeId).node.hostname || nodeId;
		} catch (err) {
			return Response.json(fromUnknownError(err).toJSON(), { status: 404 });
		}

		const pair = new WebSocketPair();
		const client = pair[0];
		const server = pair[1];
		server.accept();
		const closeClient = (message: string) => {
			try {
				server.close(message ? 1011 : 1000, message.slice(0, 120));
			} catch {
				// already closed
			}
		};
		let pipe;
		try {
			pipe = await this.tunnel.openPipe(nodeId, host, kind, workspace, port, {
				onOpened: () => server.send(JSON.stringify({ type: "opened" })),
				onData: (bytes) => server.send(bytes),
				onClose: closeClient,
			});
		} catch (err) {
			closeClient(fromUnknownError(err).message);
			return new Response(null, { status: 101, webSocket: client });
		}
		// Sent in order, a message's bytes once the one before's are. A frame
		// that fails to send ends the pipe: the client's byte stream has a hole
		// in it, and the chain must not stay rejected for every later frame.
		let sending: Promise<void> = Promise.resolve();
		server.addEventListener("message", (ev) => {
			const data: unknown = ev.data;
			sending = sending
				.then(async () => {
					if (typeof data === "string") pipe.send(new TextEncoder().encode(data));
					else if (data instanceof ArrayBuffer) pipe.send(new Uint8Array(data));
					else if (ArrayBuffer.isView(data))
						pipe.send(new Uint8Array(data.buffer, data.byteOffset, data.byteLength));
					else if (data instanceof Blob) pipe.send(new Uint8Array(await data.arrayBuffer()));
				})
				.catch((err: unknown) => {
					const message = fromUnknownError(err).message;
					console.error(`pipe to ${host} workspace ${workspace}: send failed: ${message}`);
					pipe.close();
					closeClient(`send failed: ${message}`);
				});
		});
		server.addEventListener("close", () => pipe.close());
		server.addEventListener("error", () => pipe.close());
		return new Response(null, { status: 101, webSocket: client });
	}

	private async handleWebSocketUpgrade(request: Request, pathname: string): Promise<Response> {
		const mismatch = this.checkOrgMismatch(request);
		if (mismatch) {
			return mismatch;
		}
		if (pathname === "/ws/worker") {
			const upgrade = (request.headers.get("Upgrade") ?? "").toLowerCase();
			if (upgrade !== "websocket") {
				return new Response("Expected Upgrade: websocket", { status: 426 });
			}
			let caller;
			try {
				caller = await this.authRequest(request);
			} catch (err) {
				const he = fromUnknownError(err);
				return Response.json(he.toJSON(), { status: 403 });
			}
			if (caller.orgRole === "viewer") {
				return Response.json(
					{
						code: ErrorCode.NotAuthorized,
						message: "viewer role is read-only and cannot connect as a worker",
					},
					{ status: 403 },
				);
			}
			// Pin nodeId to the token's DB-bound identity for worker-token auth so
			// one worker token can never hijack another node's WebSocket tunnel.
			const nodeId =
				caller.role === "worker"
					? caller.nodeId
					: (request.headers.get("X-Hotbed-Node-Id") ?? "").trim() || caller.nodeId;
			const hostname =
				caller.role === "worker"
					? caller.hostname || nodeId
					: (request.headers.get("X-Hotbed-Host") ?? "").trim() || caller.hostname || nodeId;
			const addr = (request.headers.get("X-Hotbed-Addr") ?? "").trim() || caller.addr;
			if (!nodeId) {
				return Response.json(
					{ code: ErrorCode.InvalidArgs, message: "X-Hotbed-Node-Id header is required" },
					{ status: 400 },
				);
			}

			const pair = new WebSocketPair();
			const client = pair[0];
			const server = pair[1];
			this.tunnel.accept(server, {
				nodeId,
				hostname,
				addr,
				connectedAt: this.ctrl.now(),
			});
			this.ctx.waitUntil(this.ensureAlarm());
			return new Response(null, { status: 101, webSocket: client });
		}

		// /ws/questions — outbound T3 Code question bridge
		const upgrade = (request.headers.get("Upgrade") ?? "").toLowerCase();
		if (upgrade !== "websocket") {
			return new Response("Expected Upgrade: websocket", { status: 426 });
		}
		let caller;
		try {
			caller = await this.authRequest(request);
		} catch (err) {
			const he = fromUnknownError(err);
			return Response.json(he.toJSON(), { status: 403 });
		}
		if (caller.role !== "owner") {
			return Response.json(
				{
					code: ErrorCode.NotAuthorized,
					message: "owner or user authentication required for /ws/questions",
				},
				{ status: 403 },
			);
		}

		const pair = new WebSocketPair();
		const client = pair[0];
		const server = pair[1];
		this.questionBridge.accept(
			server,
			{
				kind: "question-bridge",
				hostname: caller.hostname,
				login: caller.login,
				connectedAt: this.ctrl.now(),
			},
			this.store.listT3OutboxQuestions(),
		);
		return new Response(null, { status: 101, webSocket: client });
	}

	async handleHttpRequest(request: Request): Promise<Response> {
		const url = new URL(request.url);
		const pathname = url.pathname;

		// 0. Local test harness control endpoints (never active in production)
		if (this.env.TEST_HARNESS === "1" && pathname.startsWith("/__test/")) {
			return this.handleTestHarness(request, url);
		}

		// 1. Health check (unauthenticated)
		if (pathname === "/healthz" && request.method === "GET") {
			return Response.json(this.getHealth());
		}

		// Defense-in-depth: refuse organization mismatches before handling any route
		// (without mutating SQLite; org_id is only persisted after authRequest succeeds).
		const mismatch = this.checkOrgMismatch(request);
		if (mismatch) {
			return mismatch;
		}

		// 2. Cockpit cookie auth (/auth/login, /auth/logout) is served at the edge by
		// handleAuthRoute; only the session check reaches the DO.
		if (pathname === "/auth/check" && request.method === "GET") {
			try {
				const caller = await this.authRequest(request);
				if (caller.role !== "owner") {
					return new Response("Forbidden", { status: 403 });
				}
				return Response.json({
					ok: true,
					login: caller.login,
					userId: caller.userId,
					orgId: caller.orgId || this.ctrl.orgId,
					cladefsTenantId:
						this.ctrl.cladefsTenantId !== null ? Number(this.ctrl.cladefsTenantId) : undefined,
					orgRole: caller.orgRole,
					authMethod: caller.authMethod,
				});
			} catch (err) {
				const he = fromUnknownError(err);
				return Response.json(he.toJSON(), { status: 401 });
			}
		}

		// 3. Per-worker token minting (owner / org user)
		if (pathname === "/admin/worker-token" && request.method === "POST") {
			try {
				const caller = await this.authRequest(request);
				if (caller.role !== "owner") {
					return Response.json(
						{ code: ErrorCode.NotAuthorized, message: "owner token required" },
						{ status: 403 },
					);
				}
				if (caller.orgRole === "viewer") {
					return Response.json(
						{
							code: ErrorCode.NotAuthorized,
							message: "viewer role is read-only and cannot mint worker tokens",
						},
						{ status: 403 },
					);
				}
				const body = (await request.json().catch(() => ({}))) as {
					nodeId?: string;
					hostname?: string;
				};
				const nodeId = (body.nodeId ?? "").trim();
				const hostname = (body.hostname ?? "").trim();
				if (!nodeId || !hostname) {
					return Response.json(
						{ code: ErrorCode.InvalidArgs, message: "nodeId and hostname are required" },
						{ status: 400 },
					);
				}
				const token = formatOrgToken("hbw", this.ctrl.orgId || caller.orgId);
				const hash = await sha256Hex(token);
				this.store.putWorkerToken(hash, nodeId, hostname, this.ctrl.now());
				return Response.json(
					{ token, nodeId, hostname, orgId: this.ctrl.orgId || undefined },
					{ status: 201 },
				);
			} catch (err) {
				const he = fromUnknownError(err);
				return Response.json(he.toJSON(), {
					status: he.code === ErrorCode.NotAuthorized ? 403 : 500,
				});
			}
		}

		// 3b. Per-organization worker join tokens (GET/POST /admin/join-tokens, DELETE /admin/join-tokens/:id)
		if (pathname === "/admin/join-tokens" || pathname.startsWith("/admin/join-tokens/")) {
			try {
				const caller = await this.authRequest(request);
				if (caller.role !== "owner") {
					return Response.json(
						{
							code: ErrorCode.NotAuthorized,
							message: "organization member authentication required",
						},
						{ status: 403 },
					);
				}
				if (pathname === "/admin/join-tokens" && request.method === "GET") {
					return Response.json({
						orgId: this.ctrl.orgId || undefined,
						tokens: this.store.listJoinTokens(),
					});
				}
				if (pathname === "/admin/join-tokens" && request.method === "POST") {
					if (caller.orgRole === "viewer") {
						return Response.json(
							{
								code: ErrorCode.NotAuthorized,
								message: "viewer role is read-only and cannot mint join tokens",
							},
							{ status: 403 },
						);
					}
					const body = (await request.json().catch(() => ({}))) as {
						label?: string;
						expiresInHours?: number;
					};
					const label = (body.label ?? "").trim().slice(0, 100);
					const nowMs = this.ctrl.now();
					let expiresAt: number | null = null;
					if (
						typeof body.expiresInHours === "number" &&
						Number.isFinite(body.expiresInHours) &&
						body.expiresInHours > 0
					) {
						expiresAt = nowMs + Math.round(body.expiresInHours * 3600_000);
					}
					const id = `jt_${randomHex(8)}`;
					const token = formatOrgToken("hbj", this.ctrl.orgId || caller.orgId);
					const tokenHash = await sha256Hex(token);
					this.store.putJoinToken({
						id,
						tokenHash,
						label,
						createdBy: caller.login,
						createdAt: nowMs,
						expiresAt,
					});
					return Response.json(
						{
							id,
							token,
							label,
							createdBy: caller.login,
							createdAt: nowMs,
							expiresAt,
							orgId: this.ctrl.orgId || undefined,
						},
						{ status: 201 },
					);
				}
				if (pathname.startsWith("/admin/join-tokens/") && request.method === "DELETE") {
					if (caller.orgRole === "viewer") {
						return Response.json(
							{
								code: ErrorCode.NotAuthorized,
								message: "viewer role is read-only and cannot revoke join tokens",
							},
							{ status: 403 },
						);
					}
					const id = decodePathSegment(pathname.slice("/admin/join-tokens/".length));
					if (id === null) {
						return Response.json(
							{ code: ErrorCode.InvalidArgs, message: "invalid join token id encoding" },
							{ status: 400 },
						);
					}
					if (!id || !this.store.revokeJoinToken(id, this.ctrl.now())) {
						return Response.json(
							{ code: ErrorCode.NotFound, message: `no join token ${id}` },
							{ status: 404 },
						);
					}
					return Response.json({ ok: true, id });
				}
				return new Response("Method Not Allowed", { status: 405 });
			} catch (err) {
				const he = fromUnknownError(err);
				return Response.json(he.toJSON(), {
					status: he.code === ErrorCode.NotAuthorized ? 403 : 500,
				});
			}
		}

		// 3c. Per-organization personal API tokens (GET/POST /admin/api-tokens, DELETE /admin/api-tokens/:id)
		if (pathname === "/admin/api-tokens" || pathname.startsWith("/admin/api-tokens/")) {
			try {
				const caller = await this.authRequest(request);
				if (caller.role !== "owner") {
					return Response.json(
						{
							code: ErrorCode.NotAuthorized,
							message: "organization member authentication required",
						},
						{ status: 403 },
					);
				}
				const isAdmin = caller.orgRole === "admin" || caller.orgRole === "owner" || !caller.userId;
				if (pathname === "/admin/api-tokens" && request.method === "GET") {
					const tokens = this.store.listApiTokens(isAdmin ? undefined : caller.userId);
					return Response.json({
						orgId: this.ctrl.orgId || undefined,
						tokens,
					});
				}
				if (pathname === "/admin/api-tokens" && request.method === "POST") {
					const body = (await request.json().catch(() => ({}))) as {
						name?: string;
						expiresInDays?: number;
					};
					const name = (body.name ?? "").trim().slice(0, 100);
					if (!name) {
						return Response.json(
							{ code: ErrorCode.InvalidArgs, message: "token name is required" },
							{ status: 400 },
						);
					}
					const nowMs = this.ctrl.now();
					let expiresAt: number | null = null;
					if (
						typeof body.expiresInDays === "number" &&
						Number.isFinite(body.expiresInDays) &&
						body.expiresInDays > 0
					) {
						expiresAt = nowMs + Math.round(body.expiresInDays * 24 * 3600_000);
					}
					const id = `at_${randomHex(8)}`;
					const token = formatOrgToken("hbu", this.ctrl.orgId || caller.orgId);
					const tokenHash = await sha256Hex(token);
					this.store.putApiToken({
						id,
						tokenHash,
						name,
						userId: caller.userId,
						email: caller.login,
						orgRole: caller.orgRole || "member",
						createdAt: nowMs,
						expiresAt,
					});
					return Response.json(
						{
							id,
							token,
							name,
							userId: caller.userId,
							email: caller.login,
							orgRole: caller.orgRole || "member",
							createdAt: nowMs,
							expiresAt,
							orgId: this.ctrl.orgId || undefined,
						},
						{ status: 201 },
					);
				}
				if (pathname.startsWith("/admin/api-tokens/") && request.method === "DELETE") {
					const id = decodePathSegment(pathname.slice("/admin/api-tokens/".length));
					if (id === null) {
						return Response.json(
							{ code: ErrorCode.InvalidArgs, message: "invalid api token id encoding" },
							{ status: 400 },
						);
					}
					const existing = id ? this.store.getApiToken(id) : null;
					if (!existing) {
						return Response.json(
							{ code: ErrorCode.NotFound, message: `no api token ${id}` },
							{ status: 404 },
						);
					}
					if (!isAdmin && existing.userId !== caller.userId) {
						return Response.json(
							{ code: ErrorCode.NotAuthorized, message: "cannot revoke another user's API token" },
							{ status: 403 },
						);
					}
					this.store.revokeApiToken(id, this.ctrl.now());
					return Response.json({ ok: true, id });
				}
				return new Response("Method Not Allowed", { status: 405 });
			} catch (err) {
				const he = fromUnknownError(err);
				return Response.json(he.toJSON(), {
					status: he.code === ErrorCode.NotAuthorized ? 403 : 500,
				});
			}
		}

		// 3d. Global sign-up codes management (GET/POST /admin/signup-codes, DELETE /admin/signup-codes/:code)
		if (pathname === "/admin/signup-codes" || pathname.startsWith("/admin/signup-codes/")) {
			try {
				const caller = await this.authRequest(request);
				const defaultOrgId = (this.env.DEFAULT_ORG_ID ?? "").trim();
				const edgeGlobalAdmin = request.headers.get(INTERNAL_GLOBAL_ADMIN_HEADER) === "1";
				const isGlobalAdmin =
					caller.role === "owner" &&
					(edgeGlobalAdmin ||
						caller.authMethod === "bearer" ||
						(Boolean(defaultOrgId) &&
							caller.orgId === defaultOrgId &&
							(caller.orgRole === "admin" || caller.orgRole === "owner")));
				if (!isGlobalAdmin) {
					return Response.json(
						{
							code: ErrorCode.NotAuthorized,
							message: "cluster owner or default-organization admin required",
						},
						{ status: 403 },
					);
				}
				// `/auth/signup-code` redeems codes in the default DO only; a code
				// written to an organization's DO (OWNER_TOKEN + X-Hotbed-Org-Id)
				// would be accepted here and never honored.
				const isDefaultDo = request.headers.get(INTERNAL_DEFAULT_DO_HEADER) === "1";
				const boundOrgId =
					this.ctrl.orgId || (request.headers.get(INTERNAL_ORG_HEADER) ?? "").trim();
				if (!isDefaultDo && boundOrgId) {
					return Response.json(
						{
							code: ErrorCode.InvalidArgs,
							message: `sign-up codes live in the default organization, not ${boundOrgId}`,
							suggestion: "omit X-Hotbed-Org-Id when managing sign-up codes",
						},
						{ status: 400 },
					);
				}
				if (pathname === "/admin/signup-codes" && request.method === "GET") {
					return Response.json({
						codes: this.store.listSignupCodes(),
						approvedUsersCount: this.store.countApprovedUsers(),
					});
				}
				if (pathname === "/admin/signup-codes" && request.method === "POST") {
					const body = (await request.json().catch(() => ({}))) as {
						code?: string;
						label?: string;
						maxUses?: number | null;
					};
					const code = (body.code ?? "").trim().toUpperCase();
					if (code.length < 3 || code.length > 64 || /\s/.test(code)) {
						return Response.json(
							{
								code: ErrorCode.InvalidArgs,
								message: "sign-up code must be 3-64 non-whitespace characters",
							},
							{ status: 400 },
						);
					}
					const label = (body.label ?? "").trim().slice(0, 100);
					let maxUses: number | null = null;
					if (body.maxUses !== undefined && body.maxUses !== null) {
						if (
							typeof body.maxUses !== "number" ||
							!Number.isInteger(body.maxUses) ||
							body.maxUses < 1
						) {
							return Response.json(
								{ code: ErrorCode.InvalidArgs, message: "maxUses must be a positive integer" },
								{ status: 400 },
							);
						}
						maxUses = body.maxUses;
					}
					const nowMs = this.ctrl.now();
					this.store.putSignupCode({ code, label, maxUses, createdAt: nowMs });
					return Response.json({ code, label, maxUses, createdAt: nowMs }, { status: 201 });
				}
				if (pathname.startsWith("/admin/signup-codes/") && request.method === "DELETE") {
					const code = decodePathSegment(pathname.slice("/admin/signup-codes/".length));
					if (code === null || !code) {
						return Response.json(
							{ code: ErrorCode.InvalidArgs, message: "valid sign-up code is required" },
							{ status: 400 },
						);
					}
					if (!this.store.revokeSignupCode(code, this.ctrl.now())) {
						return Response.json(
							{ code: ErrorCode.NotFound, message: `no sign-up code ${code}` },
							{ status: 404 },
						);
					}
					return Response.json({ ok: true, code: code.toUpperCase() });
				}
				return new Response("Method Not Allowed", { status: 405 });
			} catch (err) {
				const he = fromUnknownError(err);
				return Response.json(he.toJSON(), {
					status: he.code === ErrorCode.NotAuthorized ? 403 : 500,
				});
			}
		}

		// 5. Binary build downloads (/builds/{build}/{asset})
		if (pathname.startsWith("/builds/") && request.method === "GET") {
			try {
				await this.authRequest(request, { allowJoinToken: true });
			} catch (err) {
				const he = fromUnknownError(err);
				return Response.json(he.toJSON(), { status: 403 });
			}
			const parts = pathname.slice("/builds/".length).split("/");
			if (parts.length !== 2 || !/^\d+$/.test(parts[0]) || !parts[1]) {
				return new Response("Not Found", { status: 404 });
			}
			return this.builds.serveAsset(Number(parts[0]), parts[1]);
		}

		// 5b. Board attachment downloads (/board/attachments/{id})
		if (
			pathname.startsWith("/board/attachments/") &&
			(request.method === "GET" || request.method === "HEAD")
		) {
			try {
				const caller = await this.authRequest(request);
				if (caller.role === "worker") {
					return Response.json(
						{ code: ErrorCode.NotAuthorized, message: "workers cannot download board attachments" },
						{ status: 403 },
					);
				}
				const id = decodePathSegment(pathname.slice("/board/attachments/".length));
				if (!id) {
					return Response.json(
						{ code: ErrorCode.InvalidArgs, message: "attachment ID is required" },
						{ status: 400 },
					);
				}
				const { attachment, data } = this.boardSvc.store.downloadAttachment(id);
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

		// 6. ConnectRPC endpoints (/hotbed.v1.<Service>/<Method>)
		if (this.dispatcher.hasPath(pathname)) {
			let caller;
			try {
				caller = await this.authRequest(request, {
					allowJoinToken: pathname === "/hotbed.v1.ClusterService/Join",
				});
			} catch (err) {
				return connectErrorResponse(err, {
					"Hotbed-Build": String(this.builds.currentBuildNumber()),
				});
			}
			const res = await this.dispatcher.handle(request, caller, this.builds.currentBuildNumber());
			if (request.method === "POST") {
				this.ctx.waitUntil(this.ensureAlarm());
			}
			return res;
		}

		return new Response("Not Found", { status: 404 });
	}

	private async handleTestHarness(request: Request, url: URL): Promise<Response> {
		const sub = url.pathname.slice("/__test/".length);
		if (sub === "clock" && request.method === "POST") {
			const body = (await request.json().catch(() => ({}))) as {
				offsetMs?: number;
				advanceMs?: number;
				setNowMs?: number;
			};
			if (typeof body.offsetMs === "number") {
				this.nowOffsetMs = body.offsetMs;
			} else if (typeof body.advanceMs === "number") {
				this.nowOffsetMs += body.advanceMs;
			} else if (typeof body.setNowMs === "number") {
				this.nowOffsetMs = body.setNowMs - Date.now();
			}
			return Response.json({ nowMs: this.ctrl.now(), offsetMs: this.nowOffsetMs });
		}
		if (sub === "config" && request.method === "POST") {
			const body = (await request.json().catch(() => ({}))) as {
				wakeWaitMs?: number;
				allowLocalRepos?: boolean;
				updatesRepo?: string;
				updatesApiBase?: string;
				updatesDownloadBase?: string;
				updatesToken?: string;
				controllerBuild?: number;
				controllerCommit?: string;
				controllerSha256Sums?: string;
				mockBuild?: TestBuildConfig["mockBuild"];
				clearBuild?: boolean;
			};
			if (typeof body.wakeWaitMs === "number") {
				this.testWakeWaitMs = body.wakeWaitMs;
				this.ctrl.power.wakeWaitMs = body.wakeWaitMs;
			}
			if (typeof body.allowLocalRepos === "boolean") {
				this.testAllowLocalRepos = body.allowLocalRepos;
				this.initRuntime();
			}
			if (
				body.updatesRepo !== undefined ||
				body.updatesApiBase !== undefined ||
				body.updatesDownloadBase !== undefined ||
				body.updatesToken !== undefined ||
				body.controllerBuild !== undefined ||
				body.controllerCommit !== undefined ||
				body.controllerSha256Sums !== undefined ||
				body.mockBuild !== undefined ||
				body.clearBuild
			) {
				const nextCfg: TestBuildConfig = {
					...this.testBuildConfig,
					...(body.updatesRepo !== undefined ? { updatesRepo: body.updatesRepo } : {}),
					...(body.updatesApiBase !== undefined ? { updatesApiBase: body.updatesApiBase } : {}),
					...(body.updatesDownloadBase !== undefined
						? { updatesDownloadBase: body.updatesDownloadBase }
						: {}),
					...(body.updatesToken !== undefined ? { updatesToken: body.updatesToken } : {}),
					...(body.controllerBuild !== undefined ? { controllerBuild: body.controllerBuild } : {}),
					...(body.controllerCommit !== undefined
						? { controllerCommit: body.controllerCommit }
						: {}),
					...(body.controllerSha256Sums !== undefined
						? { controllerSha256Sums: body.controllerSha256Sums }
						: {}),
					...(body.mockBuild !== undefined ? { mockBuild: body.mockBuild } : {}),
					...(body.clearBuild ? { clearBuild: true, mockBuild: undefined } : {}),
				};
				this.testBuildConfig = nextCfg;
				this.builds.setTestConfig(nextCfg);
			}
			return Response.json({ ok: true, clusterId: this.ctrl.clusterId });
		}
		if (sub === "reap-idle" && request.method === "POST") {
			await this.ctrl.reapIdle();
			return Response.json({ ok: true });
		}
		if (sub === "tick-schedules" && request.method === "POST") {
			await this.scheduleRunner.tickSchedules();
			await this.ctrl.settleRuns();
			return Response.json({ ok: true });
		}
		if (sub === "tick-board" && request.method === "POST") {
			this.boardSvc.tickBoard();
			return Response.json({ ok: true });
		}
		if (sub === "restart" && request.method === "POST") {
			this.initRuntime();
			return Response.json({ ok: true, clusterId: this.ctrl.clusterId });
		}
		if (sub === "daemon-log" && request.method === "POST") {
			const body = (await request.json().catch(() => ({}))) as {
				level?: string;
				msg?: string;
				attrs?: Record<string, unknown>;
				at?: number;
			};
			const lvl =
				(body.level ?? "").toLowerCase() === "error" ? DaemonLogLevel.ERROR : DaemonLogLevel.WARN;
			this.store.logDaemon(lvl, body.msg ?? "", body.attrs, body.at ?? this.ctrl.now());
			return Response.json({ ok: true });
		}
		if (sub === "seed" && request.method === "POST") {
			const body = (await request.json().catch(() => ({}))) as {
				project?: { name: string; remotes?: string[]; at?: number; reason?: string };
				session?: {
					id: string;
					agent: string;
					project: string;
					t3codeThreadId?: string;
					at?: number;
				};
				workspace?: { id: string; sessionId: string; nodeId: string; agent: string; at?: number };
				run?: { schedule: string; sessionId: string; startedAt?: number; outcome?: number };
			};
			if (body.project) {
				this.store.addProject(
					body.project.name,
					body.project.remotes ?? [],
					create(StampSchema, {
						at: BigInt(body.project.at ?? this.ctrl.now()),
						reason: body.project.reason ?? "",
					}),
				);
			}
			if (body.session) {
				this.store.startSession(
					body.session.id,
					body.session.agent,
					body.session.project,
					body.session.t3codeThreadId ?? "",
					body.session.at ?? this.ctrl.now(),
				);
			}
			if (body.workspace) {
				this.store.addLiveWorkspace(
					body.workspace.id,
					body.workspace.sessionId,
					body.workspace.nodeId,
					body.workspace.agent,
					body.workspace.at ?? this.ctrl.now(),
				);
			}
			if (body.run) {
				this.store.addScheduleRun(
					create(ScheduledRunSchema, {
						schedule: body.run.schedule,
						sessionId: body.run.sessionId,
						startedAt: BigInt(body.run.startedAt ?? this.ctrl.now()),
						outcome: (body.run.outcome ?? RunOutcome.RUNNING) as RunOutcome,
					}),
				);
			}
			return Response.json({ ok: true });
		}
		if (sub === "inspect" && request.method === "GET") {
			const kind = url.searchParams.get("kind") ?? "";
			const id = url.searchParams.get("id") ?? "";
			switch (kind) {
				case "connectedWorkers":
					return Response.json({ nodeIds: [...this.tunnel.connectedNodeIds()] });
				case "connectedQuestionBridges":
					return Response.json({ count: this.questionBridge.connectedCount() });
				case "session": {
					const s = this.store.tryGetSession(id);
					return Response.json({ found: Boolean(s), data: s ? toJson(SessionSchema, s) : null });
				}
				case "sessionEvents": {
					const evs = this.store.sessionEventsAfter(id, 0n);
					return Response.json({
						data: evs.map((e) => toJson(SessionEventSchema, e)),
					});
				}
				case "nodeView": {
					const cp = this.store.nodeCheckpoint(id);
					return Response.json({
						seq: Number(cp.seq),
						bases: cp.bases.map((b) => toJson(BaseSchema, b)),
						workspaces: cp.workspaces.map((w) => toJson(WorkspaceSchema, w)),
						commands: cp.commands.map((c) => toJson(CommandSchema, c)),
					});
				}
				case "finished": {
					const f = this.store.finishedCommand(id);
					return Response.json({
						found: Boolean(f),
						data: f ? toJson(FinishedCommandSchema, f) : null,
					});
				}
				case "history": {
					const hist = this.store.historySince(0);
					return Response.json({
						data: hist.map((h) => toJson(FinishedCommandSchema, h)),
					});
				}
				case "commandProfile": {
					const p = await this.ctrl.commands.getProfile(id);
					return Response.json({
						found: Boolean(p),
						data: p ? toJson(CommandProfileSchema, p) : null,
					});
				}
				case "baseByCommand": {
					const b = this.store.baseByCommand(id);
					return Response.json({
						found: Boolean(b),
						nodeId: b?.nodeId ?? "",
						base: b ? toJson(BaseSchema, b.base) : null,
					});
				}
				case "command": {
					try {
						const route = this.store.lookupCommand(id);
						return Response.json({ found: true, session: route.session, node: route.node });
					} catch {
						return Response.json({ found: false });
					}
				}
				default:
					return Response.json({ error: `unknown inspect kind ${kind}` }, { status: 400 });
			}
		}
		return new Response("Not Found", { status: 404 });
	}
}
