import { DurableObject } from "cloudflare:workers";
import { drizzle, type DrizzleSqliteDODatabase } from "drizzle-orm/durable-sqlite";
import { migrate } from "drizzle-orm/durable-sqlite/migrator";
import migrations from "../drizzle/migrations.js";
import { handleAdminRoute } from "./admin-routes.ts";
import type { AdminAgentDO } from "./admin-agent-do.ts";
import {
	authenticateRequest,
	INTERNAL_DEFAULT_DO_HEADER,
	INTERNAL_ORG_HEADER,
	isOrgAdminCaller,
	timingSafeStringEqual,
	type AuthEnv,
	type CallerIdentity,
} from "./auth.ts";
import { BuildDistributor, type BuildsEnv } from "./builds.ts";
import { CommandStoreClient, type CommandDO } from "./command-do.ts";
import { createConnectFetchDispatcher, type ConnectFetchDispatcher } from "./connect-adapter.ts";
import { ControllerContext } from "./context.ts";
import * as dbSchema from "./db/schema.ts";
import {
	executeReadOnlySql,
	inspectDoSchema,
	type DoSchemaInfo,
	type DoSqlResult,
	type SqlParamValue,
} from "./do-sql.ts";
import { decodePathSegment } from "./encoding.ts";
import { connectErrorResponse, ErrorCode, fromUnknownError, HotbedError } from "./errors.ts";
import {
	BoardService,
	ClusterService,
	DaemonLogLevel,
	GitAuthService,
	IssueService,
	MemoryService,
	QuestionService,
	ReviewService,
	ScheduleService,
	SessionService,
	WorkspaceService,
} from "./gen/hotbed/v1/hotbed_pb.ts";
import { randomHex } from "./ids.ts";
import { QuestionBridgeHub } from "./question-bridge.ts";
import {
	createBoardService,
	serveBoardAttachment,
	type BoardServiceBundle,
} from "./services/board.ts";
import { createClusterService } from "./services/cluster.ts";
import { createGitAuthService } from "./services/gitauth.ts";
import { createIssueService } from "./services/issues.ts";
import { createMemoryService } from "./services/memories.ts";
import { createQuestionService } from "./services/questions.ts";
import { createReviewService } from "./services/reviews.ts";
import { createScheduleService, type ScheduleRunner } from "./services/schedules.ts";
import { createSessionService } from "./services/sessions.ts";
import { createWorkspaceService } from "./services/workspaces.ts";
import { ControllerStore } from "./store/index.ts";
import type { TenantRegistryDO } from "./tenant-registry-do.ts";
import { handleTestHarness, TestOverrides } from "./test-harness.ts";
import { WorkerTunnelHub } from "./tunnel.ts";
import { isWorkOSEnabled } from "./workos.ts";

export interface ControllerEnv extends AuthEnv, BuildsEnv {
	CONTROLLER: DurableObjectNamespace<ControllerDO>;
	TENANT_REGISTRY?: DurableObjectNamespace<TenantRegistryDO>;
	COMMAND: DurableObjectNamespace<CommandDO>;
	ADMIN_AGENT?: DurableObjectNamespace<AdminAgentDO>;
	AI?: Ai;
	ANTHROPIC_API_KEY?: string;
	OPENAI_API_KEY?: string;
	ADMIN_AGENT_MODEL?: string;
	CLOUDFLARE_API_TOKEN?: string;
	CLOUDFLARE_ACCOUNT_ID?: string;
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

type Refusal = { ok: false; code: string; message: string; status: number };

export type RedeemSignupCodeResult = { ok: true; alreadyApproved?: boolean } | Refusal;

export type ConsumeHandoffResult = { ok: true } | Refusal;

export type RevokeUserApiTokensResult = { ok: true; revoked: number } | Refusal;

/** Who a session check found: the body of GET /auth/check, and of checkApiToken. */
export interface AuthCheck {
	ok: true;
	login: string;
	userId?: string;
	orgId?: string;
	cladefsTenantId?: number;
	orgRole?: string;
	authMethod: string;
}

export type ApiTokenCheckResult =
	| AuthCheck
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
	/** What /__test/* set: kept across initRuntime. */
	private readonly test = new TestOverrides();

	constructor(ctx: DurableObjectState, env: ControllerEnv) {
		super(ctx, env);
		void this.ctx.blockConcurrencyWhile(async () => {
			// Auto-run bundled Drizzle Kit migrations before any request or alarm is delivered.
			this.db = drizzle(this.ctx.storage, { schema: dbSchema });
			await migrate(this.db, migrations);

			this.store = new ControllerStore(this.ctx.storage, this.db);
			this.store.memories.autoApproveLegacyProposed();
			this.tunnel = new WorkerTunnelHub(this.ctx);
			this.questionBridge = new QuestionBridgeHub(this.ctx);
			this.initRuntime();
		});
	}

	/** (Re)builds what reads configuration: the context, the services and their router. */
	private initRuntime(): void {
		const waitUntil = (p: Promise<unknown>) => this.ctx.waitUntil(p);
		this.builds = new BuildDistributor(this.env, this.store.meta, waitUntil);
		if (this.test.buildConfig) {
			this.builds.setTestConfig(this.test.buildConfig);
		}

		const meta = this.store.meta;
		const orgId = meta.get(ORG_ID_META_KEY) ?? "";
		const defaultOrgId = (this.env.DEFAULT_ORG_ID ?? "").trim();
		const allowEnvClusterId = !orgId || (defaultOrgId !== "" && orgId === defaultOrgId);
		let clusterId = allowEnvClusterId ? (this.env.CLUSTER_ID ?? "").trim() : "";
		if (!clusterId) {
			clusterId = meta.get(CLUSTER_ID_META_KEY) ?? "";
			if (!clusterId) {
				clusterId = randomHex(8);
				meta.set(CLUSTER_ID_META_KEY, clusterId);
			}
		}
		const rawTenant = meta.get(CLADEFS_TENANT_ID_META_KEY);
		const cladefsTenantId =
			rawTenant !== null && /^\d+$/.test(rawTenant) ? BigInt(rawTenant) : null;

		this.ctrl = new ControllerContext({
			store: this.store,
			commands: new CommandStoreClient(this.env.COMMAND, clusterId),
			tunnel: this.tunnel,
			questionBridge: this.questionBridge,
			builds: this.builds,
			waitUntil,
			clusterId,
			orgId,
			cladefsTenantId,
			controllerName: (this.env.CONTROLLER_NAME ?? "").trim() || "cloudflare-do",
			allowLocalRepos:
				this.test.allowLocalRepos ?? (this.env.ALLOW_LOCAL_REPOS ?? "").trim() === "1",
		});
		this.ctrl.nowFn = () => Date.now() + this.test.nowOffsetMs;
		const initialSignupCode = (this.env.INITIAL_SIGNUP_CODE ?? "").trim();
		if (initialSignupCode) {
			this.store.tokens.seedInitialSignupCode(initialSignupCode, this.ctrl.now());
		}
		if (this.test.placementWaitCapMs !== undefined) {
			this.ctrl.placementWaitCapMs = this.test.placementWaitCapMs;
		}

		const wsSvc = createWorkspaceService(this.ctrl);
		const schedSvc = createScheduleService(this.ctrl, wsSvc.helpers);
		this.scheduleRunner = schedSvc.runner;
		this.boardSvc = createBoardService(this.ctrl, this.ctx.storage);
		this.ctrl.boardCountsFn = () => this.boardSvc.store.projectBoardCounts(this.ctrl.now());
		const services = {
			cluster: createClusterService(this.ctrl, schedSvc.runner),
			session: createSessionService(this.ctrl),
			question: createQuestionService(this.ctrl),
			memory: createMemoryService(this.ctrl),
			review: createReviewService(this.ctrl),
			issue: createIssueService(this.ctrl),
			gitAuth: createGitAuthService(this.ctrl),
		};
		this.dispatcher = createConnectFetchDispatcher((router) => {
			router.service(ClusterService, services.cluster);
			router.service(SessionService, services.session);
			router.service(WorkspaceService, wsSvc.impl);
			router.service(QuestionService, services.question);
			router.service(MemoryService, services.memory);
			router.service(ReviewService, services.review);
			router.service(IssueService, services.issue);
			router.service(ScheduleService, schedSvc.impl);
			router.service(GitAuthService, services.gitAuth);
			router.service(BoardService, this.boardSvc.impl);
		});
	}

	// ---- Maintenance alarm --------------------------------------------------

	private desiredAlarmDelayMs(): number | null {
		const hasActiveWork =
			this.store.schedules.list("").some((s) => !s.paused) ||
			this.store.sessions.liveByProject().size > 0 ||
			this.boardSvc.store.hasPendingWork(this.ctrl.now());
		if (hasActiveWork) return ACTIVE_ALARM_MS;
		const hasIdleWork = this.store.fleet.list().length > 0 || this.builds.needsBackgroundRefresh();
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
			{ name: "tick board", level: DaemonLogLevel.ERROR, fn: () => this.boardSvc.tickBoard() },
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
				this.store.daemonLogs.log(
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
			if (!this.test.maintenancePaused) await this.runMaintenance();
		} finally {
			const delay = this.desiredAlarmDelayMs();
			if (delay !== null) {
				await this.ctx.storage.setAlarm(Date.now() + delay);
			}
		}
	}

	// ---- WebSocket hibernation ------------------------------------------------

	override webSocketMessage(ws: WebSocket, message: string | ArrayBuffer): void {
		if (this.questionBridge.isBridgeSocket(ws)) {
			this.questionBridge.handleMessage(ws, message, () => this.store.questions.t3Outbox());
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

	// ---- Organization binding and authentication ------------------------------

	/** Why a request for reqOrgId may not reach this controller, or "" when it may. */
	private orgMismatch(reqOrgId: string): string {
		if (reqOrgId && this.ctrl.orgId && this.ctrl.orgId !== reqOrgId) {
			return `organization mismatch: this controller DO belongs to ${this.ctrl.orgId}, not ${reqOrgId}`;
		}
		return "";
	}

	private checkOrgMismatch(request: Request): Response | null {
		const message = this.orgMismatch((request.headers.get(INTERNAL_ORG_HEADER) ?? "").trim());
		return message
			? Response.json({ code: ErrorCode.NotAuthorized, message }, { status: 403 })
			: null;
	}

	/**
	 * Binds an unbound controller to orgId, once an authenticated caller
	 * names it, so a request with a forged org-scoped token cannot poison it.
	 */
	private bindOrg(orgId: string): void {
		if (orgId && !this.ctrl.orgId) {
			this.store.meta.set(ORG_ID_META_KEY, orgId);
			this.ctrl.orgId = orgId;
		}
	}

	private async ensureCladefsTenantId(isDefaultHint: boolean): Promise<bigint | null> {
		const defaultOrgId = (this.env.DEFAULT_ORG_ID ?? "").trim();
		const isDefault =
			isDefaultHint ||
			(Boolean(defaultOrgId) && this.ctrl.orgId === defaultOrgId) ||
			(!this.ctrl.orgId && !defaultOrgId) ||
			this.store.cladefs.hasLegacyTenantZeroData();
		const orgKey = this.ctrl.orgId || (isDefault ? "default" : "");
		if (!orgKey) {
			return this.ctrl.cladefsTenantId;
		}
		const recordedOrgKey = this.store.meta.get(CLADEFS_TENANT_ORG_KEY_META_KEY) ?? "";
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
		this.store.meta.set(CLADEFS_TENANT_ID_META_KEY, assignedStr);
		this.store.meta.set(CLADEFS_TENANT_ORG_KEY_META_KEY, orgKey);
		this.ctrl.cladefsTenantId = assigned;
		return assigned;
	}

	private async authRequest(
		request: Request,
		opts?: { allowJoinToken?: boolean },
	): Promise<CallerIdentity> {
		const reqOrgId = (request.headers.get(INTERNAL_ORG_HEADER) ?? "").trim();
		const caller = await authenticateRequest(request, this.env, this.store.tokens, {
			allowJoinToken: opts?.allowJoinToken,
			trustInternalHeader: true,
			nowMs: this.ctrl.now(),
			orgId: this.ctrl.orgId || reqOrgId,
		});
		this.bindOrg(reqOrgId || caller.orgId);
		await this.ensureCladefsTenantId(request.headers.get(INTERNAL_DEFAULT_DO_HEADER) === "1");
		return caller;
	}

	/** Authenticates request, or answers status with why it failed. */
	private async authOr(
		request: Request,
		status: number,
		opts?: { allowJoinToken?: boolean },
	): Promise<CallerIdentity | Response> {
		try {
			return await this.authRequest(request, opts);
		} catch (err) {
			return Response.json(fromUnknownError(err).toJSON(), { status });
		}
	}

	private authCheck(caller: CallerIdentity): AuthCheck {
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
		return this.store.tokens.isUserSignupApproved(userId);
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
		const tokens = this.store.tokens;
		if (tokens.isUserSignupApproved(userId)) {
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
		let matchedCode = "";
		for (const candidate of tokens.listActiveSignupCodes()) {
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
		tokens.recordSignupRedemption(userId, email, matchedCode, this.ctrl.now());
		return { ok: true };
	}

	consumeCliHandoff(jtiRaw: string, expRaw: number): ConsumeHandoffResult {
		const jti = jtiRaw.trim();
		const exp = typeof expRaw === "number" && Number.isFinite(expRaw) ? expRaw : 0;
		if (!jti || jti.length > 128) {
			return { ok: false, code: ErrorCode.InvalidArgs, message: "jti is required", status: 400 };
		}
		const key = `cli_handoff:${jti}`;
		if (this.store.meta.get(key) !== null) {
			return {
				ok: false,
				code: ErrorCode.Conflict,
				message: "CLI handoff code already used",
				status: 409,
			};
		}
		this.store.meta.set(key, String(Math.max(exp, this.ctrl.now() + 120_000)));
		return { ok: true };
	}

	startCliDeviceAuth(deviceCodeRaw: string, userCodeRaw: string, expRaw: number): { ok: true } {
		const deviceCode = deviceCodeRaw.trim();
		const userCode = userCodeRaw.trim().toUpperCase();
		const now = this.ctrl.now();
		const exp = typeof expRaw === "number" && Number.isFinite(expRaw) ? expRaw : now + 600_000;
		this.store.meta.prune(now);
		this.store.meta.set(
			`cli_device:${deviceCode}`,
			JSON.stringify({ userCode, exp, status: "pending" }),
		);
		this.store.meta.set(`cli_device_code:${userCode}`, deviceCode);
		return { ok: true };
	}

	checkCliDeviceCode(userCodeRaw: string): { ok: true } | { ok: false; message: string } {
		const userCode = userCodeRaw.trim().toUpperCase();
		const now = this.ctrl.now();
		if (!userCode) {
			return { ok: false, message: "Enter the code shown in your terminal." };
		}
		const deviceCode = this.store.meta.get(`cli_device_code:${userCode}`);
		if (!deviceCode) {
			return { ok: false, message: "Invalid or expired code. Run `hotbed login` again." };
		}
		const raw = this.store.meta.get(`cli_device:${deviceCode}`);
		if (!raw) {
			this.store.meta.delete(`cli_device_code:${userCode}`);
			return { ok: false, message: "Invalid or expired code. Run `hotbed login` again." };
		}
		try {
			const entry = JSON.parse(raw) as { exp?: number; status?: string };
			if (typeof entry.exp !== "number" || entry.exp < now || entry.status !== "pending") {
				this.store.meta.delete(`cli_device:${deviceCode}`);
				this.store.meta.delete(`cli_device_code:${userCode}`);
				return { ok: false, message: "That code has expired. Run `hotbed login` again." };
			}
		} catch {
			return { ok: false, message: "Invalid or expired code. Run `hotbed login` again." };
		}
		return { ok: true };
	}

	approveCliDeviceAuth(
		userCodeRaw: string,
		sealedPayload: string,
	): { ok: true } | { ok: false; message: string } {
		const userCode = userCodeRaw.trim().toUpperCase();
		const now = this.ctrl.now();
		const deviceCode = this.store.meta.get(`cli_device_code:${userCode}`);
		if (!deviceCode) {
			return { ok: false, message: "Invalid or expired code. Run `hotbed login` again." };
		}
		const raw = this.store.meta.get(`cli_device:${deviceCode}`);
		if (!raw) {
			this.store.meta.delete(`cli_device_code:${userCode}`);
			return { ok: false, message: "Invalid or expired code. Run `hotbed login` again." };
		}
		let entry: { userCode: string; exp: number; status: string };
		try {
			entry = JSON.parse(raw) as { userCode: string; exp: number; status: string };
		} catch {
			return { ok: false, message: "Invalid or expired code. Run `hotbed login` again." };
		}
		if (entry.exp < now || entry.status !== "pending") {
			this.store.meta.delete(`cli_device:${deviceCode}`);
			this.store.meta.delete(`cli_device_code:${userCode}`);
			return { ok: false, message: "That code has expired. Run `hotbed login` again." };
		}
		this.store.meta.set(
			`cli_device:${deviceCode}`,
			JSON.stringify({
				userCode,
				exp: Math.max(entry.exp, now + 120_000),
				status: "complete",
				sealedPayload,
			}),
		);
		this.store.meta.delete(`cli_device_code:${userCode}`);
		return { ok: true };
	}

	pollCliDeviceAuth(deviceCodeRaw: string): {
		status: "pending" | "complete" | "expired";
		sealedPayload?: string;
	} {
		const deviceCode = deviceCodeRaw.trim();
		const now = this.ctrl.now();
		if (!deviceCode) {
			return { status: "expired" };
		}
		const key = `cli_device:${deviceCode}`;
		const raw = this.store.meta.get(key);
		if (!raw) {
			return { status: "expired" };
		}
		let entry: { userCode?: string; exp?: number; status?: string; sealedPayload?: string };
		try {
			entry = JSON.parse(raw) as {
				userCode?: string;
				exp?: number;
				status?: string;
				sealedPayload?: string;
			};
		} catch {
			this.store.meta.delete(key);
			return { status: "expired" };
		}
		if (typeof entry.exp !== "number" || entry.exp < now) {
			this.store.meta.delete(key);
			if (entry.userCode) this.store.meta.delete(`cli_device_code:${entry.userCode}`);
			return { status: "expired" };
		}
		if (entry.status === "complete" && entry.sealedPayload) {
			this.store.meta.delete(key);
			if (entry.userCode) this.store.meta.delete(`cli_device_code:${entry.userCode}`);
			return { status: "complete", sealedPayload: entry.sealedPayload };
		}
		return { status: "pending" };
	}

	async revokeUserApiTokens(params: {
		caller: CallerIdentity;
		orgId: string;
		userId: string;
	}): Promise<RevokeUserApiTokensResult> {
		const reqOrgId = params.orgId.trim();
		const mismatch = this.orgMismatch(reqOrgId);
		if (mismatch) {
			return { ok: false, code: ErrorCode.NotAuthorized, message: mismatch, status: 403 };
		}
		if (!isOrgAdminCaller(params.caller)) {
			return {
				ok: false,
				code: ErrorCode.NotAuthorized,
				message: "organization admin required",
				status: 403,
			};
		}
		this.bindOrg(reqOrgId || params.caller.orgId);
		await this.ensureCladefsTenantId(false);
		const userId = params.userId.trim();
		if (!userId) {
			return { ok: false, code: ErrorCode.InvalidArgs, message: "userId is required", status: 400 };
		}
		const revoked = this.store.tokens.revokeApiTokensForUser(userId, this.ctrl.now());
		return { ok: true, revoked };
	}

	async checkApiToken(params: {
		bearer: string;
		orgId: string;
		isDefaultDo: boolean;
	}): Promise<ApiTokenCheckResult> {
		const reqOrgId = params.orgId.trim();
		const mismatch = this.orgMismatch(reqOrgId);
		if (mismatch) {
			return {
				ok: false,
				status: 403,
				error: { code: ErrorCode.NotAuthorized, message: mismatch },
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
			return this.authCheck(caller);
		} catch (err) {
			return { ok: false, status: 401, error: fromUnknownError(err).toJSON() };
		}
	}

	/** Read-only SQL query executed via typed Workers RPC by the Admin Agent. */
	adminQuerySql(
		query: string,
		params: SqlParamValue[] = [],
		opts?: { maxRows?: number },
	): DoSqlResult {
		return executeReadOnlySql(this.ctx.storage.sql, query, params, opts);
	}

	/** Schema & metadata inspection executed via typed Workers RPC by the Admin Agent. */
	async adminInspectSchema(): Promise<
		DoSchemaInfo & { clusterId: string; orgId: string; cladefsTenantId: string | null }
	> {
		const schema = await inspectDoSchema(this.ctx.storage.sql, this.ctx.storage);
		return {
			...schema,
			clusterId: this.ctrl.clusterId,
			orgId: this.ctrl.orgId,
			cladefsTenantId:
				this.ctrl.cladefsTenantId !== null ? this.ctrl.cladefsTenantId.toString() : null,
		};
	}

	// ---- HTTP -------------------------------------------------------------

	/**
	 * Cloudflare Workers requires WebSocket upgrade responses (101 with webSocket)
	 * to return from DO `fetch()`. All other routes use typed Workers RPC methods
	 * (`getHealth`, `checkSignupApproved`, `redeemSignupCode`, `consumeCliHandoff`,
	 * `revokeUserApiTokens`, `checkApiToken`, `handleHttpRequest`).
	 */
	override async fetch(request: Request): Promise<Response> {
		const url = new URL(request.url);
		const path = url.pathname;
		if (path !== "/ws/worker" && path !== "/ws/questions" && path !== "/ws/pipe") {
			return new Response("Use typed Workers RPC methods on ControllerDO", { status: 405 });
		}
		const mismatch = this.checkOrgMismatch(request);
		if (mismatch) return mismatch;
		if (path === "/ws/pipe") {
			return this.tunnel.servePipe(request, url, {
				authorize: async (req) => {
					const caller = await this.authOr(req, 403);
					if (caller instanceof Response) return caller;
					if (caller.role === "worker" || caller.orgRole === "viewer") {
						return Response.json(
							{
								code: ErrorCode.NotAuthorized,
								message: "a worker or a viewer opens no pipes to workspaces",
							},
							{ status: 403 },
						);
					}
					return null;
				},
				locate: (workspace) => {
					const row = this.store.workspaces.get(workspace);
					if (row.released) {
						throw new HotbedError(ErrorCode.NotFound, `workspace ${workspace} was released`);
					}
					const host = this.store.fleet.get(row.node).node.hostname || row.node;
					return { nodeId: row.node, host };
				},
			});
		}
		if ((request.headers.get("Upgrade") ?? "").toLowerCase() !== "websocket") {
			return new Response("Expected Upgrade: websocket", { status: 426 });
		}
		const caller = await this.authOr(request, 403);
		if (caller instanceof Response) return caller;
		return path === "/ws/worker"
			? this.acceptWorker(request, caller)
			: this.acceptQuestionBridge(caller);
	}

	/** /ws/worker: a worker's tunnel. */
	private acceptWorker(request: Request, caller: CallerIdentity): Response {
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
		const header = (name: string) => (request.headers.get(name) ?? "").trim();
		const worker = caller.role === "worker";
		const nodeId = worker ? caller.nodeId : header("X-Hotbed-Node-Id") || caller.nodeId;
		const hostname = worker
			? caller.hostname || nodeId
			: header("X-Hotbed-Host") || caller.hostname || nodeId;
		if (!nodeId) {
			return Response.json(
				{ code: ErrorCode.InvalidArgs, message: "X-Hotbed-Node-Id header is required" },
				{ status: 400 },
			);
		}
		const pair = new WebSocketPair();
		this.tunnel.accept(pair[1], { nodeId, hostname, connectedAt: this.ctrl.now() });
		this.ctx.waitUntil(this.ensureAlarm());
		return new Response(null, { status: 101, webSocket: pair[0] });
	}

	/** /ws/questions: the outbound T3 Code question bridge. */
	private acceptQuestionBridge(caller: CallerIdentity): Response {
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
		this.questionBridge.accept(
			pair[1],
			{
				kind: "question-bridge",
				hostname: caller.hostname,
				login: caller.login,
				connectedAt: this.ctrl.now(),
			},
			this.store.questions.t3Outbox(),
		);
		return new Response(null, { status: 101, webSocket: pair[0] });
	}

	async handleHttpRequest(request: Request): Promise<Response> {
		const url = new URL(request.url);
		const pathname = url.pathname;

		// Local test harness control endpoints (never active in production)
		if (this.env.TEST_HARNESS === "1" && pathname.startsWith("/__test/")) {
			return handleTestHarness(
				{
					overrides: this.test,
					store: this.store,
					tunnel: this.tunnel,
					questionBridge: this.questionBridge,
					runtime: () => ({
						ctrl: this.ctrl,
						builds: this.builds,
						scheduleRunner: this.scheduleRunner,
						boardSvc: this.boardSvc,
					}),
					restart: () => this.initRuntime(),
				},
				request,
				url,
			);
		}

		// Health check (unauthenticated)
		if (pathname === "/healthz" && request.method === "GET") {
			return Response.json(this.getHealth());
		}

		// Defense-in-depth: refuse organization mismatches before handling any route
		// (without mutating SQLite; org_id is only persisted after authRequest succeeds).
		const mismatch = this.checkOrgMismatch(request);
		if (mismatch) return mismatch;

		// Cockpit cookie auth (/auth/login, /auth/logout) is served at the edge by
		// handleAuthRoute; only the session check reaches the DO.
		if (pathname === "/auth/check" && request.method === "GET") {
			const caller = await this.authOr(request, 401);
			if (caller instanceof Response) return caller;
			if (caller.role !== "owner") return new Response("Forbidden", { status: 403 });
			return Response.json(this.authCheck(caller));
		}

		const admin = await handleAdminRoute(request, pathname, {
			tokens: this.store.tokens,
			orgId: () => this.ctrl.orgId,
			defaultOrgId: (this.env.DEFAULT_ORG_ID ?? "").trim(),
			now: () => this.ctrl.now(),
			authenticate: (req) => this.authRequest(req),
		});
		if (admin) return admin;

		// Binary build downloads (/builds/{build}/{asset})
		if (pathname.startsWith("/builds/") && request.method === "GET") {
			const caller = await this.authOr(request, 403, { allowJoinToken: true });
			if (caller instanceof Response) return caller;
			const parts = pathname.slice("/builds/".length).split("/");
			if (parts.length !== 2 || !/^\d+$/.test(parts[0]) || !parts[1]) {
				return new Response("Not Found", { status: 404 });
			}
			return this.builds.serveAsset(Number(parts[0]), parts[1]);
		}

		// Board attachment downloads (/board/attachments/{id})
		if (
			pathname.startsWith("/board/attachments/") &&
			(request.method === "GET" || request.method === "HEAD")
		) {
			const caller = await this.authOr(request, 403);
			if (caller instanceof Response) return caller;
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
			return serveBoardAttachment(this.boardSvc.store, request, id);
		}

		// ConnectRPC endpoints (/hotbed.v1.<Service>/<Method>)
		if (this.dispatcher.hasPath(pathname)) {
			let caller: CallerIdentity;
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
}
