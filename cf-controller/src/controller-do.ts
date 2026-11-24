import { DurableObject } from "cloudflare:workers";
import { create, toBinary } from "@bufbuild/protobuf";
import { drizzle, type DrizzleSqliteDODatabase } from "drizzle-orm/durable-sqlite";
import { migrate } from "drizzle-orm/durable-sqlite/migrator";
import migrations from "../drizzle/migrations.js";
import * as dbSchema from "./db/schema.ts";
import {
	ClusterService,
	DaemonLogLevel,
	ErrorInfoSchema,
	GitAuthService,
	QuestionService,
	ReviewService,
	ScheduleService,
	SessionService,
	WorkspaceService,
} from "./gen/hotbed/v1/hotbed_pb.ts";
import {
	authenticateRequest,
	clearSessionCookie,
	makeSessionCookie,
	renderLoginPage,
	sha256Hex,
	timingSafeStringEqual,
	type AuthEnv,
} from "./auth.ts";
import { BuildDistributor, type BuildsEnv } from "./builds.ts";
import { createConnectFetchDispatcher, type ConnectFetchDispatcher } from "./connect-adapter.ts";
import { ControllerContext } from "./context.ts";
import { ErrorCode, HotbedError, fromUnknownError } from "./errors.ts";
import { randomHex } from "./gitops.ts";
import { createClusterService } from "./services/cluster.ts";
import { createGitAuthService } from "./services/gitauth.ts";
import { createQuestionService } from "./services/questions.ts";
import { createReviewService } from "./services/reviews.ts";
import { createScheduleService, type ScheduleRunner } from "./services/schedules.ts";
import { createSessionService } from "./services/sessions.ts";
import { createWorkspaceService } from "./services/workspaces.ts";
import { ControllerStore } from "./store.ts";
import { WorkerTunnelHub } from "./tunnel.ts";

export interface ControllerEnv extends AuthEnv, BuildsEnv {
	CONTROLLER: DurableObjectNamespace<ControllerDO>;
	CLUSTER_ID?: string;
	CONTROLLER_NAME?: string;
	ALLOW_LOCAL_REPOS?: string;
	ASSETS?: Fetcher;
}

const ACTIVE_ALARM_MS = 15_000;
const IDLE_ALARM_MS = 60_000;
const CLUSTER_ID_META_KEY = "cluster_id";

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

export class ControllerDO extends DurableObject<ControllerEnv> {
	private db!: DrizzleSqliteDODatabase<typeof dbSchema>;
	private store!: ControllerStore;
	private tunnel!: WorkerTunnelHub;
	private builds!: BuildDistributor;
	private ctrl!: ControllerContext;
	private scheduleRunner!: ScheduleRunner;
	private dispatcher!: ConnectFetchDispatcher;

	constructor(ctx: DurableObjectState, env: ControllerEnv) {
		super(ctx, env);
		void this.ctx.blockConcurrencyWhile(async () => {
			// Auto-run bundled Drizzle Kit migrations before any request or alarm is delivered.
			this.db = drizzle(this.ctx.storage, { schema: dbSchema });
			await migrate(this.db, migrations);

			this.store = new ControllerStore(this.ctx.storage, this.db);
			this.tunnel = new WorkerTunnelHub(this.ctx);
			this.builds = new BuildDistributor(this.env, this.store);

			let clusterId = (this.env.CLUSTER_ID ?? "").trim();
			if (!clusterId) {
				clusterId = this.store.getMeta(CLUSTER_ID_META_KEY) ?? "";
				if (!clusterId) {
					clusterId = randomHex(8);
					this.store.setMeta(CLUSTER_ID_META_KEY, clusterId);
				}
			}
			const controllerName = (this.env.CONTROLLER_NAME ?? "").trim() || "cloudflare-do";
			const allowLocalRepos = (this.env.ALLOW_LOCAL_REPOS ?? "").trim() === "1";

			this.ctrl = new ControllerContext({
				store: this.store,
				tunnel: this.tunnel,
				builds: this.builds,
				clusterId,
				controllerName,
				allowLocalRepos,
			});

			const wsSvc = createWorkspaceService(this.ctrl);
			const schedSvc = createScheduleService(this.ctrl, wsSvc.helpers);
			this.scheduleRunner = schedSvc.runner;
			const clusterSvc = createClusterService(this.ctrl, schedSvc.runner);
			const sessionSvc = createSessionService(this.ctrl);
			const questionSvc = createQuestionService(this.ctrl);
			const reviewSvc = createReviewService(this.ctrl);
			const gitAuthSvc = createGitAuthService(this.ctrl);

			this.dispatcher = createConnectFetchDispatcher((router) => {
				router.service(ClusterService, clusterSvc);
				router.service(SessionService, sessionSvc);
				router.service(WorkspaceService, wsSvc.impl);
				router.service(QuestionService, questionSvc);
				router.service(ReviewService, reviewSvc);
				router.service(ScheduleService, schedSvc.impl);
				router.service(GitAuthService, gitAuthSvc);
			});
		});
	}

	private async ensureAlarm(): Promise<void> {
		const existing = await this.ctx.storage.getAlarm();
		if (existing !== null && existing > this.ctrl.now()) {
			return;
		}
		const hasActiveWork =
			this.store.listSchedules("").some((s) => !s.paused) ||
			this.store.liveSessionsByProject().size > 0;
		const hasIdleWork =
			this.store.listNodes().length > 0 || Boolean((this.env.UPDATES_REPO ?? "").trim());
		if (hasActiveWork) {
			await this.ctx.storage.setAlarm(this.ctrl.now() + ACTIVE_ALARM_MS);
		} else if (hasIdleWork) {
			await this.ctx.storage.setAlarm(this.ctrl.now() + IDLE_ALARM_MS);
		}
	}

	async runMaintenance(): Promise<void> {
		await this.ctrl.reapIdle();
		await this.scheduleRunner.tickSchedules();
		await this.ctrl.settleRuns();
		try {
			await this.builds.refreshIfStale(this.ctrl.now());
		} catch (err) {
			this.store.logDaemon(
				DaemonLogLevel.WARN,
				"refresh GitHub builds",
				{ err: err instanceof Error ? err.message : String(err) },
				this.ctrl.now(),
			);
		}
		this.ctrl.maybePrune();
	}

	override async alarm(): Promise<void> {
		await this.runMaintenance();
		const hasActiveWork =
			this.store.listSchedules("").some((s) => !s.paused) ||
			this.store.liveSessionsByProject().size > 0;
		const hasIdleWork =
			this.store.listNodes().length > 0 || Boolean((this.env.UPDATES_REPO ?? "").trim());
		if (hasActiveWork) {
			await this.ctx.storage.setAlarm(this.ctrl.now() + ACTIVE_ALARM_MS);
		} else if (hasIdleWork) {
			await this.ctx.storage.setAlarm(this.ctrl.now() + IDLE_ALARM_MS);
		}
	}

	override webSocketMessage(ws: WebSocket, message: string | ArrayBuffer): void {
		this.tunnel.handleMessage(ws, message);
	}

	override webSocketClose(ws: WebSocket, code: number, reason: string, _wasClean: boolean): void {
		this.tunnel.handleClose(ws, code, reason);
	}

	override webSocketError(ws: WebSocket, error: unknown): void {
		this.tunnel.handleClose(ws, 1011, error instanceof Error ? error.message : String(error));
	}

	override async fetch(request: Request): Promise<Response> {
		const url = new URL(request.url);
		const pathname = url.pathname;

		// 1. Health check (unauthenticated)
		if (pathname === "/healthz" && request.method === "GET") {
			return Response.json({
				ok: true,
				clusterId: this.ctrl.clusterId,
				version: this.builds.versionString(),
				build: this.builds.currentBuildNumber(),
				connectedWorkers: this.tunnel.connectedNodeIds().size,
			});
		}

		// 2. Cockpit cookie auth endpoints
		if (pathname === "/auth/login") {
			if (request.method === "GET") {
				return new Response(renderLoginPage(), {
					status: 200,
					headers: { "Content-Type": "text/html; charset=utf-8" },
				});
			}
			if (request.method === "POST") {
				const ownerToken = (this.env.OWNER_TOKEN ?? "").trim();
				if (!ownerToken) {
					return new Response(
						renderLoginPage("OWNER_TOKEN is not configured on this controller."),
						{
							status: 503,
							headers: { "Content-Type": "text/html; charset=utf-8" },
						},
					);
				}
				let submitted = "";
				const ct = (request.headers.get("Content-Type") ?? "").toLowerCase();
				if (ct.includes("application/json")) {
					const body = (await request.json().catch(() => ({}))) as { token?: string };
					submitted = (body.token ?? "").trim();
				} else {
					const form = await request.formData().catch(() => null);
					const tokenField = form?.get("token");
					submitted = typeof tokenField === "string" ? tokenField.trim() : "";
				}
				if (!submitted || !(await timingSafeStringEqual(submitted, ownerToken))) {
					return new Response(renderLoginPage("Invalid owner token."), {
						status: 401,
						headers: { "Content-Type": "text/html; charset=utf-8" },
					});
				}
				const secure = url.protocol === "https:";
				const cookie = await makeSessionCookie(ownerToken, secure);
				return new Response(null, {
					status: 303,
					headers: {
						Location: "/",
						"Set-Cookie": cookie,
					},
				});
			}
			return new Response("Method Not Allowed", { status: 405 });
		}

		if (pathname === "/auth/logout" && request.method === "POST") {
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
				const caller = await authenticateRequest(request, this.env, this.store);
				if (caller.role !== "owner") {
					return new Response("Forbidden", { status: 403 });
				}
				return Response.json({ ok: true, login: caller.login, authMethod: caller.authMethod });
			} catch (err) {
				const he = fromUnknownError(err);
				return Response.json(he.toJSON(), { status: 401 });
			}
		}

		// 3. Per-worker token minting (owner-only)
		if (pathname === "/admin/worker-token" && request.method === "POST") {
			try {
				const caller = await authenticateRequest(request, this.env, this.store);
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
				const token = `hbw_${randomHex(24)}`;
				const hash = await sha256Hex(token);
				this.store.putWorkerToken(hash, nodeId, hostname, this.ctrl.now());
				return Response.json({ token, nodeId, hostname }, { status: 201 });
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
				caller = await authenticateRequest(request, this.env, this.store, {
					allowJoinToken: true,
				});
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

		// 5. Binary build downloads (/builds/{build}/{asset})
		if (pathname.startsWith("/builds/") && request.method === "GET") {
			try {
				await authenticateRequest(request, this.env, this.store, { allowJoinToken: true });
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
				caller = await authenticateRequest(request, this.env, this.store, {
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
}
