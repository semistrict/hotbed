import { env, SELF } from "cloudflare:test";
import { create } from "@bufbuild/protobuf";
import { createClient, type Interceptor } from "@connectrpc/connect";
import { createFetchClient } from "@connectrpc/connect/protocol";
import { createTransport } from "@connectrpc/connect/protocol-connect";
import { describe, expect, it } from "vite-plus/test";
import {
	AdminScopeError,
	resolveScopedInspectableDo,
	type AdminOrgScope,
} from "../src/admin-cf-api.ts";
import type { ControllerEnv } from "../src/controller-do.ts";
import {
	ClusterService,
	DaemonLogEntrySchema,
	DaemonLogLevel,
	EventRefSchema,
	EventSchema,
	NodeUsageSchema,
	ResourcesSchema,
	WorkerEventSchema,
	WorkspaceService,
} from "../src/gen/hotbed/v1/hotbed_pb.ts";
import { KEEP_WORKER_INFO_LOGS_MS, workerDoName } from "../src/worker-do.ts";

const OWNER_TOKEN = "test-owner-secret";
const JOIN_TOKEN = "test-join-secret";

function makeClients(token: string, extraHeaders: Record<string, string> = {}) {
	const authInterceptor: Interceptor = (next) => async (req) => {
		req.header.set("Authorization", `Bearer ${token}`);
		for (const [k, v] of Object.entries(extraHeaders)) req.header.set(k, v);
		return next(req);
	};
	const transport = createTransport({
		httpClient: createFetchClient((input, init) => SELF.fetch(input, init)),
		baseUrl: "https://controller.test",
		useBinaryFormat: true,
		interceptors: [authInterceptor],
		sendCompression: null,
		acceptCompression: [],
		compressMinBytes: 0,
		readMaxBytes: 0xffffffff,
		writeMaxBytes: 0xffffffff,
	});
	return {
		cluster: createClient(ClusterService, transport),
		workspaces: createClient(WorkspaceService, transport),
	};
}

describe("WorkerDO per-org / per-worker SQLite storage", () => {
	it("streams worker slog (INFO/WARN/ERROR), samples, and feed events into WorkerDO without bloating ControllerDO", async () => {
		const testEnv = env as unknown as ControllerEnv;
		const owner = makeClients(OWNER_TOKEN);

		// Join worker "ubu1" with a dedicated node identity.
		const joinClient = makeClients(JOIN_TOKEN, {
			"X-Hotbed-Node-ID": "node-ubu1",
			"X-Hotbed-Host": "ubu1",
		});
		const joined = await joinClient.cluster.join({});
		expect(joined.clusterId).toBeTruthy();
		expect(joined.workerToken).toBeTruthy();

		const worker = makeClients(joined.workerToken, {
			"X-Hotbed-Node-ID": "node-ubu1",
			"X-Hotbed-Host": "ubu1",
		});

		const nowMs = Date.now();

		// 1. Heartbeat records telemetry samples into WorkerDO, NOT ControllerDO.samples.
		await worker.cluster.heartbeat({
			clusterId: joined.clusterId,
			resources: create(ResourcesSchema, { arch: "amd64", cpus: 16, memoryBytes: 32n << 30n }),
			usage: create(NodeUsageSchema, { cpuBusy: 0.42, memoryUsedBytes: 16n << 30n }),
		});

		// 2. PushEvents records feed events (like auto-capture) into WorkerDO, NOT ControllerDO.events.
		await worker.cluster.pushEvents({
			clusterId: joined.clusterId,
			epoch: "e1",
			checkpoint: { seq: 1n, bases: [], workspaces: [], commands: [] },
			events: [
				create(EventSchema, {
					seq: 1n,
					at: BigInt(nowMs),
					feed: create(WorkerEventSchema, {
						at: BigInt(nowMs),
						kind: "base",
						text: "captured hotbed-vm-cap-ws1 ← ws1",
						ref: create(EventRefSchema, { type: "workspace", id: "ws1" }),
					}),
				}),
			],
		});

		// 3. PushDaemonLogs streams INFO, WARN, and ERROR into WorkerDO (idempotent by epoch+seq).
		const entries = [
			create(DaemonLogEntrySchema, {
				at: BigInt(nowMs - 2000),
				level: DaemonLogLevel.INFO,
				msg: "auto-captured workspace as project base",
				attrsJson: '{"workspace":"ws1","project":"hotbed"}',
				sourceLoc: "internal/worker/cladefs_backend.go:1024",
				seq: 1n,
			}),
			create(DaemonLogEntrySchema, {
				at: BigInt(nowMs - 1000),
				level: DaemonLogLevel.WARN,
				msg: "cladefsd cell still busy",
				attrsJson: '{"retryIn":"100ms"}',
				sourceLoc: "internal/worker/cladefs_backend.go:367",
				seq: 2n,
			}),
			create(DaemonLogEntrySchema, {
				at: BigInt(nowMs),
				level: DaemonLogLevel.ERROR,
				msg: "vm guest sync failed",
				attrsJson: '{"vm":"ws1"}',
				sourceLoc: "internal/vm/manager.go:726",
				seq: 3n,
			}),
		];
		const push1 = await worker.cluster.pushDaemonLogs({
			clusterId: joined.clusterId,
			epoch: "ep-1",
			entries,
		});
		expect(push1.ackedSeq).toBe(3n);

		// Retrying the same batch must not duplicate rows.
		const push2 = await worker.cluster.pushDaemonLogs({
			clusterId: joined.clusterId,
			epoch: "ep-1",
			entries,
		});
		expect(push2.ackedSeq).toBe(3n);

		// Verify ControllerDO SQLite stayed completely free of samples and events bloat.
		const ctrlStub = testEnv.CONTROLLER.getByName("default");
		const ctrlSamples = await ctrlStub.adminQuerySql("SELECT COUNT(*) AS c FROM samples");
		const ctrlEvents = await ctrlStub.adminQuerySql("SELECT COUNT(*) AS c FROM events");
		expect(Number(ctrlSamples.rows[0].c)).toBe(0);
		expect(Number(ctrlEvents.rows[0].c)).toBe(0);

		// Verify WorkerDO holds all 3 tables (worker_logs, worker_samples, worker_events).
		const wStub = testEnv.WORKER.getByName(workerDoName(joined.clusterId, "node-ubu1"));
		const wLogs = await wStub.adminQuerySql("SELECT COUNT(*) AS c FROM worker_logs");
		const wSamples = await wStub.adminQuerySql("SELECT COUNT(*) AS c FROM worker_samples");
		const wEvents = await wStub.adminQuerySql("SELECT COUNT(*) AS c FROM worker_events");
		expect(Number(wLogs.rows[0].c)).toBe(3);
		expect(Number(wSamples.rows[0].c)).toBe(1);
		expect(Number(wEvents.rows[0].c)).toBe(1);

		// 4. GetDaemonLogs works directly from WorkerDO even with NO open worker tunnel!
		// Default (UNSPECIFIED, includeInfo=false): returns WARN + ERROR only.
		const warnErr = await owner.workspaces.getDaemonLogs({ source: "ubu1" });
		expect(warnErr.entries.map((e) => e.level)).toEqual([
			DaemonLogLevel.ERROR,
			DaemonLogLevel.WARN,
		]);
		expect(warnErr.sources[0]?.reachable).toBe(true);
		expect(warnErr.sources[0]?.errors24h).toBe(1);
		expect(warnErr.sources[0]?.warns24h).toBe(1);

		// --level info returns INFO entries (e.g. auto-capture).
		const infoOnly = await owner.workspaces.getDaemonLogs({
			source: "ubu1",
			level: DaemonLogLevel.INFO,
			query: "auto-captured",
		});
		expect(infoOnly.entries).toHaveLength(1);
		expect(infoOnly.entries[0].msg).toBe("auto-captured workspace as project base");

		// --include-info returns INFO + WARN + ERROR combined.
		const allLevels = await owner.workspaces.getDaemonLogs({
			source: "ubu1",
			includeInfo: true,
		});
		expect(allLevels.entries).toHaveLength(3);

		// 5. listEvents and getSnapshot aggregate from WorkerDO transparently.
		const evRes = await owner.cluster.listEvents({ node: "ubu1", kinds: ["base"] });
		expect(evRes.events).toHaveLength(1);
		expect(evRes.events[0].text).toContain("hotbed-vm-cap-ws1");

		const snap = await owner.cluster.getSnapshot({});
		expect(snap.events.some((e) => e.text.includes("hotbed-vm-cap-ws1"))).toBe(true);
		expect(snap.series["node-ubu1"]?.samples.length).toBe(1);
		const ubuNode = snap.nodes.find((n) => n.id === "node-ubu1");
		expect(ubuNode?.daemonLogs?.errors24h).toBe(1);
		expect(ubuNode?.daemonLogs?.warns24h).toBe(1);

		// 6. WorkerDO prunes expired INFO logs (>48h) while retaining WARN/ERROR (<14d).
		await wStub.pruneNow(nowMs + KEEP_WORKER_INFO_LOGS_MS + 10_000);
		const afterPrune = await owner.workspaces.getDaemonLogs({
			source: "ubu1",
			includeInfo: true,
		});
		expect(afterPrune.entries.map((e) => e.level)).toEqual([
			DaemonLogLevel.ERROR,
			DaemonLogLevel.WARN,
		]);
	});

	it("enforces strict per-organization scoping on Admin Agent WORKER DO access", async () => {
		const testEnv = env as unknown as ControllerEnv;
		const ctrlStub = testEnv.CONTROLLER.getByName("default");
		const health = await ctrlStub.getHealth();
		const scope: AdminOrgScope = {
			controllerDoName: "default",
			orgId: "",
			isGlobalAdmin: true,
		};

		// Same-org WorkerDO resolves cleanly.
		const ok = await resolveScopedInspectableDo(testEnv, scope, {
			binding: "WORKER",
			name: workerDoName(health.clusterId, "node-ubu1"),
		});
		expect(ok.label).toBe(`WORKER(worker:${health.clusterId}:node-ubu1)`);

		// Cross-org cluster prefix is rejected with 403 AdminScopeError.
		await expect(
			resolveScopedInspectableDo(testEnv, scope, {
				binding: "WORKER",
				name: workerDoName("other-org-cluster", "node-ubu1"),
			}),
		).rejects.toThrow(AdminScopeError);
	});
});
