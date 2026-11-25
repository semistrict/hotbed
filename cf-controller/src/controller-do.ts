import { DurableObject } from "cloudflare:workers";
import { create, toBinary, toJson } from "@bufbuild/protobuf";
import { drizzle, type DrizzleSqliteDODatabase } from "drizzle-orm/durable-sqlite";
import { migrate } from "drizzle-orm/durable-sqlite/migrator";
import migrations from "../drizzle/migrations.js";
import * as dbSchema from "./db/schema.ts";
import {
	BaseSchema,
	ClusterService,
	CommandProfileSchema,
	CommandSchema,
	DaemonLogLevel,
	ErrorInfoSchema,
	FinishedCommandSchema,
	GitAuthService,
	MemoryService,
	QuestionService,
	ReviewService,
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
	checkSameOrigin,
	clearSessionCookie,
	INTERNAL_DEFAULT_DO_HEADER,
	INTERNAL_GLOBAL_ADMIN_HEADER,
	INTERNAL_ORG_HEADER,
	makeSessionCookie,
	renderLoginPage,
	sha256Hex,
	timingSafeStringEqual,
	type AuthEnv,
} from "./auth.ts";
import { formatOrgToken, isSafeReturnTo, isWorkOSEnabled } from "./workos.ts";
import { BuildDistributor, type BuildsEnv, type TestBuildConfig } from "./builds.ts";
import { createConnectFetchDispatcher, type ConnectFetchDispatcher } from "./connect-adapter.ts";
import { ControllerContext } from "./context.ts";
import { ErrorCode, HotbedError, fromUnknownError } from "./errors.ts";
import { randomHex } from "./gitops.ts";
import { createClusterService } from "./services/cluster.ts";
import { createGitAuthService } from "./services/gitauth.ts";
import { createMemoryService } from "./services/memories.ts";
import { createQuestionService } from "./services/questions.ts";
import { createReviewService } from "./services/reviews.ts";
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
	CLUSTER_ID?: string;
	CONTROLLER_NAME?: string;
	ALLOW_LOCAL_REPOS?: string;
	TEST_HARNESS?: string;
	ASSETS?: Fetcher;
	GC_WORKER?: Fetcher;
	GC_WORKER_URL?: string;
	GC_TOKEN?: string;
}

const ACTIVE_ALARM_MS = 15_000;
const IDLE_ALARM_MS = 60_000;
const CLUSTER_ID_META_KEY = "cluster_id";
const ORG_ID_META_KEY = "org_id";
const CLADEFS_TENANT_ID_META_KEY = "cladefs_tenant_id";
const CLADEFS_TENANT_ORG_KEY_META_KEY = "cladefs_tenant_org_key";
const TENANT_REGISTRY_DO_NAME = "global";

function encodeBase64(bytes: Uint8Array): string {
	let bin = "";
	const chunk = 0x8000;
	for (let i = 0; i < bytes.length; i += chunk) {
		bin += String.fromCharCode(...bytes.subarray(i, i + chunk));
	}
	return btoa(bin);
}

/**
 * Formats an auth/gate error in Connect JSON wire format with hotbed.v1.ErrorInfo detail
 * so Go clients (`api.FromConnect`) and @connectrpc/connect-web decode the exact HotbedError.
 */
function connectErrorResponse(err: HotbedError, buildNumber: number): Response {
	const info = create(ErrorInfoSchema, {
		code: err.code,
		suggestion: err.suggestion,
		retryable: err.retryable,
	});
	const connectCode =
		err.code === ErrorCode.NotAuthorized
			? "permission_denied"
			: err.code === ErrorCode.InvalidArgs
				? "invalid_argument"
				: "internal";
	const httpStatus =
		err.code === ErrorCode.NotAuthorized ? 403 : err.code === ErrorCode.InvalidArgs ? 400 : 500;
	return new Response(
		JSON.stringify({
			code: connectCode,
			message: err.message,
			details: [
				{
					type: ErrorInfoSchema.typeName,
					value: encodeBase64(toBinary(ErrorInfoSchema, info)),
				},
			],
		}),
		{
			status: httpStatus,
			headers: {
				"Content-Type": "application/json",
				"Hotbed-Build": String(buildNumber),
			},
		},
	);
}

function decodePathSegment(raw: string): string | null {
	try {
		return decodeURIComponent(raw).trim();
	} catch {
		return null;
	}
}

export class ControllerDO extends DurableObject<ControllerEnv> {
	private db!: DrizzleSqliteDODatabase<typeof dbSchema>;
	private store!: ControllerStore;
	private tunnel!: WorkerTunnelHub;
	private questionBridge!: QuestionBridgeHub;
	private builds!: BuildDistributor;
	private ctrl!: ControllerContext;
	private scheduleRunner!: ScheduleRunner;
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
		const clusterSvc = createClusterService(this.ctrl, schedSvc.runner);
		const sessionSvc = createSessionService(this.ctrl);
		const questionSvc = createQuestionService(this.ctrl);
		const memorySvc = createMemoryService(this.ctrl);
		const reviewSvc = createReviewService(this.ctrl);
		const gitAuthSvc = createGitAuthService(this.ctrl);

		this.dispatcher = createConnectFetchDispatcher((router) => {
			router.service(ClusterService, clusterSvc);
			router.service(SessionService, sessionSvc);
			router.service(WorkspaceService, wsSvc.impl);
			router.service(QuestionService, questionSvc);
			router.service(MemoryService, memorySvc);
			router.service(ReviewService, reviewSvc);
			router.service(ScheduleService, schedSvc.impl);
			router.service(GitAuthService, gitAuthSvc);
		});
	}

	private desiredAlarmDelayMs(): number | null {
		const hasActiveWork =
			this.store.listSchedules("").some((s) => !s.paused) ||
			this.store.liveSessionsByProject().size > 0;
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

	override async fetch(request: Request): Promise<Response> {
		const url = new URL(request.url);
		const pathname = url.pathname;

		// 0. Local test harness control endpoints (never active in production)
		if (this.env.TEST_HARNESS === "1" && pathname.startsWith("/__test/")) {
			return this.handleTestHarness(request, url);
		}

		// 1. Health check (unauthenticated)
		if (pathname === "/healthz" && request.method === "GET") {
			return Response.json({
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
			});
		}

		// Defense-in-depth: refuse organization mismatches before handling any route
		// (without mutating SQLite; org_id is only persisted after authRequest succeeds).
		const mismatch = this.checkOrgMismatch(request);
		if (mismatch) {
			return mismatch;
		}

		// 1b. Internal Worker-to-DO control routes (never exposed by index.ts DO_PREFIXES)
		if (pathname === "/internal/revoke-user-tokens" && request.method === "POST") {
			try {
				const caller = await this.authRequest(request);
				const isAdmin =
					caller.role === "owner" && (caller.orgRole === "admin" || caller.orgRole === "owner");
				if (!isAdmin) {
					return Response.json(
						{ code: ErrorCode.NotAuthorized, message: "organization admin required" },
						{ status: 403 },
					);
				}
				const body = (await request.json().catch(() => ({}))) as { userId?: string };
				const userId = (body.userId ?? "").trim();
				if (!userId) {
					return Response.json(
						{ code: ErrorCode.InvalidArgs, message: "userId is required" },
						{ status: 400 },
					);
				}
				const revoked = this.store.revokeApiTokensForUser(userId, this.ctrl.now());
				return Response.json({ ok: true, revoked });
			} catch (err) {
				const he = fromUnknownError(err);
				return Response.json(he.toJSON(), {
					status: he.code === ErrorCode.NotAuthorized ? 403 : 500,
				});
			}
		}

		if (pathname === "/internal/consume-handoff" && request.method === "POST") {
			const body = (await request.json().catch(() => ({}))) as { jti?: string; exp?: number };
			const jti = (body.jti ?? "").trim();
			const exp = typeof body.exp === "number" && Number.isFinite(body.exp) ? body.exp : 0;
			if (!jti || jti.length > 128) {
				return Response.json(
					{ code: ErrorCode.InvalidArgs, message: "jti is required" },
					{ status: 400 },
				);
			}
			const key = `cli_handoff:${jti}`;
			if (this.store.getMeta(key) !== null) {
				return Response.json(
					{ code: ErrorCode.Conflict, message: "CLI handoff code already used" },
					{ status: 409 },
				);
			}
			this.store.setMeta(key, String(Math.max(exp, this.ctrl.now() + 120_000)));
			return Response.json({ ok: true });
		}

		if (pathname === "/internal/signup/check" && request.method === "POST") {
			const body = (await request.json().catch(() => ({}))) as { userId?: string };
			const userId = (body.userId ?? "").trim();
			if (!userId) {
				return Response.json(
					{ code: ErrorCode.InvalidArgs, message: "userId is required" },
					{ status: 400 },
				);
			}
			return Response.json({ approved: this.store.isUserSignupApproved(userId) });
		}

		if (pathname === "/internal/signup/redeem" && request.method === "POST") {
			const body = (await request.json().catch(() => ({}))) as {
				userId?: string;
				email?: string;
				code?: string;
			};
			const userId = (body.userId ?? "").trim();
			const email = (body.email ?? "").trim();
			const submitted = (body.code ?? "").trim().toUpperCase();
			if (!userId) {
				return Response.json(
					{ code: ErrorCode.InvalidArgs, message: "userId is required" },
					{ status: 400 },
				);
			}
			if (this.store.isUserSignupApproved(userId)) {
				return Response.json({ ok: true, alreadyApproved: true });
			}
			if (!submitted) {
				return Response.json(
					{ code: ErrorCode.NotAuthorized, message: "Sign-up code is required." },
					{ status: 403 },
				);
			}
			const active = this.store.listActiveSignupCodes();
			let matchedCode = "";
			for (const candidate of active) {
				if (await timingSafeStringEqual(submitted, candidate.code)) {
					matchedCode = candidate.code;
				}
			}
			if (!matchedCode) {
				return Response.json(
					{ code: ErrorCode.NotAuthorized, message: "Invalid sign-up code." },
					{ status: 403 },
				);
			}
			this.store.recordSignupRedemption(userId, email, matchedCode, this.ctrl.now());
			return Response.json({ ok: true });
		}

		// 2. Cockpit cookie auth endpoints
		if (pathname === "/auth/login") {
			if (request.method === "GET") {
				const returnTo = isSafeReturnTo(url.searchParams.get("return_to"));
				return new Response(renderLoginPage(undefined, false, returnTo), {
					status: 200,
					headers: { "Content-Type": "text/html; charset=utf-8" },
				});
			}
			if (request.method === "POST") {
				const csrfErr = checkSameOrigin(request);
				if (csrfErr) {
					return Response.json(
						{ code: ErrorCode.NotAuthorized, message: csrfErr },
						{ status: 403 },
					);
				}
				let submitted = "";
				let rawReturnTo: string | null = url.searchParams.get("return_to");
				const ct = (request.headers.get("Content-Type") ?? "").toLowerCase();
				if (ct.includes("application/json")) {
					const body = (await request.json().catch(() => ({}))) as {
						token?: string;
						return_to?: string;
					};
					submitted = (body.token ?? "").trim();
					if (typeof body.return_to === "string") rawReturnTo = body.return_to;
				} else {
					const form = await request.formData().catch(() => null);
					const tokenField = form?.get("token");
					submitted = typeof tokenField === "string" ? tokenField.trim() : "";
					const rtField = form?.get("return_to");
					if (typeof rtField === "string") rawReturnTo = rtField;
				}
				const returnTo = isSafeReturnTo(rawReturnTo);
				const ownerToken = (this.env.OWNER_TOKEN ?? "").trim();
				if (!ownerToken) {
					return new Response(
						renderLoginPage("OWNER_TOKEN is not configured on this controller.", false, returnTo),
						{
							status: 503,
							headers: { "Content-Type": "text/html; charset=utf-8" },
						},
					);
				}
				if (!submitted || !(await timingSafeStringEqual(submitted, ownerToken))) {
					return new Response(renderLoginPage("Invalid owner token.", false, returnTo), {
						status: 401,
						headers: { "Content-Type": "text/html; charset=utf-8" },
					});
				}
				const secure = url.protocol === "https:";
				const cookie = await makeSessionCookie(ownerToken, secure);
				return new Response(null, {
					status: 303,
					headers: {
						Location: returnTo,
						"Set-Cookie": cookie,
					},
				});
			}
			return new Response("Method Not Allowed", { status: 405 });
		}

		if (pathname === "/auth/logout" && request.method === "POST") {
			const csrfErr = checkSameOrigin(request);
			if (csrfErr) {
				return Response.json({ code: ErrorCode.NotAuthorized, message: csrfErr }, { status: 403 });
			}
			const secure = url.protocol === "https:";
			return new Response(null, {
				status: 303,
				headers: {
					Location: "/auth/login",
					"Set-Cookie": clearSessionCookie(secure),
				},
			});
		}

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

		// 4. Worker persistent reverse-RPC WebSocket tunnel (/ws/worker)
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

		// 4b. Question outbox push WebSocket for outbound T3 Code bridges (/ws/questions)
		if (pathname === "/ws/questions") {
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

		// 6. ConnectRPC endpoints (/hotbed.v1.<Service>/<Method>)
		if (this.dispatcher.hasPath(pathname)) {
			let caller;
			try {
				caller = await this.authRequest(request, {
					allowJoinToken: pathname === "/hotbed.v1.ClusterService/Join",
				});
			} catch (err) {
				return connectErrorResponse(fromUnknownError(err), this.builds.currentBuildNumber());
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
					const p = this.store.getCommandProfile(id);
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
