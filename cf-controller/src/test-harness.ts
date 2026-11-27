// The /__test/* control plane: endpoints the local test harnesses
// (cmd/hotbed's cfharness, internal/cfbundle, cmd/dev-cockpit) drive the
// controller with. ControllerDO serves them only when TEST_HARNESS is "1".
import { create, toJson } from "@bufbuild/protobuf";
import type { BuildDistributor, TestBuildConfig } from "./builds.ts";
import type { ControllerContext } from "./context.ts";
import {
	BaseSchema,
	CommandProfileSchema,
	CommandSchema,
	DaemonLogLevel,
	FinishedCommandSchema,
	RunOutcome,
	ScheduledRunSchema,
	SessionEventSchema,
	SessionSchema,
	StampSchema,
	WorkspaceSchema,
} from "./gen/hotbed/v1/hotbed_pb.ts";
import type { QuestionBridgeHub } from "./question-bridge.ts";
import type { BoardServiceBundle } from "./services/board.ts";
import type { ScheduleRunner } from "./services/schedules.ts";
import type { ControllerStore } from "./store/index.ts";
import type { WorkerTunnelHub } from "./tunnel.ts";

/** What /__test/* changed, which ControllerDO keeps across restarts of its runtime. */
export class TestOverrides {
	/** Added to the wall clock: ControllerContext.now. */
	nowOffsetMs = 0;
	/** Caps placement's capacity and upgrade waits (ControllerContext.placementWaitCapMs). */
	placementWaitCapMs?: number;
	allowLocalRepos?: boolean;
	buildConfig?: TestBuildConfig;
	/**
	 * Leaves maintenance (reaping idle sessions, ticking schedules and the
	 * board, settling runs) to /__test/reap-idle and the tick endpoints, so
	 * a test that moves the clock never races the real-time DO alarm.
	 */
	maintenancePaused = false;
}

/** ControllerDO's parts as of now: restart() replaces the runtime ones. */
export interface TestHarnessHost {
	readonly overrides: TestOverrides;
	readonly store: ControllerStore;
	readonly tunnel: WorkerTunnelHub;
	readonly questionBridge: QuestionBridgeHub;
	runtime(): {
		ctrl: ControllerContext;
		builds: BuildDistributor;
		scheduleRunner: ScheduleRunner;
		boardSvc: BoardServiceBundle;
	};
	restart(): void;
}

async function body<T>(request: Request): Promise<T> {
	return (await request.json().catch(() => ({}))) as T;
}

export async function handleTestHarness(
	host: TestHarnessHost,
	request: Request,
	url: URL,
): Promise<Response> {
	const sub = url.pathname.slice("/__test/".length);
	const post = request.method === "POST";
	const { overrides: o, store } = host;
	if (sub === "clock" && post) {
		const b = await body<{ offsetMs?: number; advanceMs?: number; setNowMs?: number }>(request);
		if (typeof b.offsetMs === "number") o.nowOffsetMs = b.offsetMs;
		else if (typeof b.advanceMs === "number") o.nowOffsetMs += b.advanceMs;
		else if (typeof b.setNowMs === "number") o.nowOffsetMs = b.setNowMs - Date.now();
		return Response.json({ nowMs: host.runtime().ctrl.now(), offsetMs: o.nowOffsetMs });
	}
	if (sub === "config" && post) {
		return Response.json(await configure(host, await body<ConfigBody>(request)));
	}
	if (sub === "reap-idle" && post) {
		await host.runtime().ctrl.reapIdle();
		return Response.json({ ok: true });
	}
	if (sub === "tick-schedules" && post) {
		const rt = host.runtime();
		await rt.scheduleRunner.tickSchedules();
		await rt.ctrl.settleRuns();
		return Response.json({ ok: true });
	}
	if (sub === "tick-board" && post) {
		host.runtime().boardSvc.tickBoard();
		return Response.json({ ok: true });
	}
	if (sub === "restart" && post) {
		host.restart();
		return Response.json({ ok: true, clusterId: host.runtime().ctrl.clusterId });
	}
	if (sub === "daemon-log" && post) {
		const b = await body<{
			level?: string;
			msg?: string;
			attrs?: Record<string, unknown>;
			at?: number;
		}>(request);
		const lvl =
			(b.level ?? "").toLowerCase() === "error" ? DaemonLogLevel.ERROR : DaemonLogLevel.WARN;
		store.daemonLogs.log(lvl, b.msg ?? "", b.attrs, b.at ?? host.runtime().ctrl.now());
		return Response.json({ ok: true });
	}
	if (sub === "seed" && post) {
		seed(host, await body<SeedBody>(request));
		return Response.json({ ok: true });
	}
	if (sub === "inspect" && request.method === "GET") {
		return inspect(host, url.searchParams.get("kind") ?? "", url.searchParams.get("id") ?? "");
	}
	return new Response("Not Found", { status: 404 });
}

interface ConfigBody {
	/** Caps placement's capacity and upgrade waits (ControllerContext.placementWaitCapMs). */
	wakeWaitMs?: number;
	allowLocalRepos?: boolean;
	/** false pauses the DO alarm's maintenance (TestOverrides.maintenancePaused). */
	maintenanceAlarm?: boolean;
	updatesRepo?: string;
	updatesApiBase?: string;
	updatesDownloadBase?: string;
	updatesToken?: string;
	controllerBuild?: number;
	controllerCommit?: string;
	controllerSha256Sums?: string;
	mockBuild?: TestBuildConfig["mockBuild"];
	clearBuild?: boolean;
}

const BUILD_KEYS = [
	"updatesRepo",
	"updatesApiBase",
	"updatesDownloadBase",
	"updatesToken",
	"controllerBuild",
	"controllerCommit",
	"controllerSha256Sums",
	"mockBuild",
] as const;

async function configure(host: TestHarnessHost, b: ConfigBody) {
	const o = host.overrides;
	if (typeof b.wakeWaitMs === "number") {
		o.placementWaitCapMs = b.wakeWaitMs;
		host.runtime().ctrl.placementWaitCapMs = b.wakeWaitMs;
	}
	if (typeof b.maintenanceAlarm === "boolean") {
		o.maintenancePaused = !b.maintenanceAlarm;
	}
	if (typeof b.allowLocalRepos === "boolean") {
		o.allowLocalRepos = b.allowLocalRepos;
		host.restart();
	}
	const changed: Partial<TestBuildConfig> = {};
	for (const k of BUILD_KEYS) {
		if (b[k] !== undefined) Object.assign(changed, { [k]: b[k] });
	}
	if (Object.keys(changed).length > 0 || b.clearBuild) {
		o.buildConfig = {
			...o.buildConfig,
			...changed,
			...(b.clearBuild ? { clearBuild: true, mockBuild: undefined } : {}),
		};
		host.runtime().builds.setTestConfig(o.buildConfig);
	}
	return { ok: true, clusterId: host.runtime().ctrl.clusterId };
}

interface SeedBody {
	project?: { name: string; remotes?: string[]; at?: number; reason?: string };
	session?: { id: string; agent: string; project: string; t3codeThreadId?: string; at?: number };
	workspace?: { id: string; sessionId: string; nodeId: string; agent: string; at?: number };
	run?: { schedule: string; sessionId: string; startedAt?: number; outcome?: number };
}

function seed(host: TestHarnessHost, b: SeedBody): void {
	const { store } = host;
	const now = host.runtime().ctrl.now();
	if (b.project) {
		store.projects.add(
			b.project.name,
			b.project.remotes ?? [],
			create(StampSchema, { at: BigInt(b.project.at ?? now), reason: b.project.reason ?? "" }),
		);
	}
	if (b.session) {
		store.sessions.start(
			b.session.id,
			b.session.agent,
			b.session.project,
			b.session.t3codeThreadId ?? "",
			b.session.at ?? now,
		);
	}
	if (b.workspace) {
		store.workspaces.addLive(
			b.workspace.id,
			b.workspace.sessionId,
			b.workspace.nodeId,
			b.workspace.agent,
			b.workspace.at ?? now,
		);
	}
	if (b.run) {
		store.schedules.addRun(
			create(ScheduledRunSchema, {
				schedule: b.run.schedule,
				sessionId: b.run.sessionId,
				startedAt: BigInt(b.run.startedAt ?? now),
				outcome: (b.run.outcome ?? RunOutcome.RUNNING) as RunOutcome,
			}),
		);
	}
}

async function inspect(host: TestHarnessHost, kind: string, id: string): Promise<Response> {
	const { store } = host;
	switch (kind) {
		case "connectedWorkers":
			return Response.json({ nodeIds: [...host.tunnel.connectedNodeIds()] });
		case "connectedQuestionBridges":
			return Response.json({ count: host.questionBridge.connectedCount() });
		case "session": {
			const s = store.sessions.tryGet(id);
			return Response.json({ found: Boolean(s), data: s ? toJson(SessionSchema, s) : null });
		}
		case "sessionEvents": {
			const evs = store.sessions.eventsAfter(id, 0n);
			return Response.json({ data: evs.map((e) => toJson(SessionEventSchema, e)) });
		}
		case "nodeView": {
			const cp = store.nodeState.checkpoint(id);
			return Response.json({
				seq: Number(cp.seq),
				bases: cp.bases.map((b) => toJson(BaseSchema, b)),
				workspaces: cp.workspaces.map((w) => toJson(WorkspaceSchema, w)),
				commands: cp.commands.map((c) => toJson(CommandSchema, c)),
			});
		}
		case "finished": {
			const f = store.commands.finished(id);
			return Response.json({
				found: Boolean(f),
				data: f ? toJson(FinishedCommandSchema, f) : null,
			});
		}
		case "history": {
			const hist = store.commands.historySince(0);
			return Response.json({ data: hist.map((h) => toJson(FinishedCommandSchema, h)) });
		}
		case "commandProfile": {
			const p = await host.runtime().ctrl.commands.getProfile(id);
			return Response.json({ found: Boolean(p), data: p ? toJson(CommandProfileSchema, p) : null });
		}
		case "baseByCommand": {
			const b = store.commands.baseBuiltBy(id);
			return Response.json({
				found: Boolean(b),
				nodeId: b?.nodeId ?? "",
				base: b ? toJson(BaseSchema, b.base) : null,
			});
		}
		case "command": {
			try {
				const route = store.commands.lookup(id);
				return Response.json({ found: true, session: route.session, node: route.node });
			} catch {
				return Response.json({ found: false });
			}
		}
		default:
			return Response.json({ error: `unknown inspect kind ${kind}` }, { status: 400 });
	}
}
