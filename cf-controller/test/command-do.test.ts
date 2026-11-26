import { env, runDurableObjectAlarm, runInDurableObject, SELF } from "cloudflare:test";
import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import { createClient, type Interceptor } from "@connectrpc/connect";
import { createFetchClient } from "@connectrpc/connect/protocol";
import { createTransport } from "@connectrpc/connect/protocol-connect";
import { describe, expect, it } from "vite-plus/test";
import { commandDoName } from "../src/command-do.ts";
import type { ControllerEnv } from "../src/controller-do.ts";
import {
	ClusterService,
	CommandDiagnosticsSchema,
	CommandInterventionSchema,
	CommandLogSchema,
	CommandProfileSchema,
	CommandSampleSchema,
	CommandSchema,
	CommandState,
	FinishedCommandSchema,
	InterventionKind,
	ProfileFunctionSchema,
	StampSchema,
	WorkspaceService,
} from "../src/gen/hotbed/v1/hotbed_pb.ts";

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

describe("CommandDO per-command SQLite storage via typed Workers RPC", () => {
	it("isolates per-command profiles, diagnostics, samples, and output logs across CommandDO instances and prunes on alarm", async () => {
		const testEnv = env as unknown as ControllerEnv;
		const cmdA = testEnv.COMMAND.getByName(commandDoName("cluster-test", "cmd-a"));
		const cmdB = testEnv.COMMAND.getByName(commandDoName("cluster-test", "cmd-b"));
		const baseNow = Date.now();

		// 1. Profile stored in cmdA is invisible to cmdB
		const profA = create(CommandProfileSchema, {
			commandId: "cmd-a",
			profiler: "opentelemetry-ebpf-profiler",
			totalSamples: 42n,
			topFunctions: [
				create(ProfileFunctionSchema, { frame: 0, selfSamples: 42n, totalSamples: 42n }),
			],
		});
		await cmdA.saveProfile({
			nodeId: "node-1",
			endedAtMs: baseNow,
			doc: toBinary(CommandProfileSchema, profA),
			nowMs: baseNow,
		});
		const gotProfA = await cmdA.getProfile();
		expect(gotProfA).not.toBeNull();
		expect(fromBinary(CommandProfileSchema, gotProfA!).totalSamples).toBe(42n);
		expect(await cmdB.getProfile()).toBeNull();

		// 2. Diagnostics ordered by seq
		await cmdA.saveDiagnostics({
			nodeId: "node-1",
			seq: 1,
			createdAtMs: baseNow + 1000,
			doc: '{"commandId":"cmd-a","intervention":{"seq":1}}',
			nowMs: baseNow + 1000,
		});
		await cmdA.saveDiagnostics({
			nodeId: "node-1",
			seq: 2,
			createdAtMs: baseNow + 2000,
			doc: '{"commandId":"cmd-a","intervention":{"seq":2}}',
			nowMs: baseNow + 2000,
		});
		expect(await cmdA.hasDiagnostics()).toBe(true);
		expect(await cmdB.hasDiagnostics()).toBe(false);
		expect(await cmdA.getDiagnostic(2)).toContain('"seq":2');
		expect(await cmdA.listDiagnosticsAfter(1)).toHaveLength(1);

		// 3. Samples keep only newest maxSamples
		for (let seq = 1; seq <= 4; seq++) {
			const s = create(CommandSampleSchema, { commandId: "cmd-a", seq, durationSeconds: 3 });
			await cmdA.saveSample({
				nodeId: "node-1",
				seq,
				createdAtMs: baseNow + seq * 1000,
				meta: JSON.stringify({ commandId: "cmd-a", seq }),
				doc: toBinary(CommandSampleSchema, s),
				maxSamples: 2,
				nowMs: baseNow + seq * 1000,
			});
		}
		expect(await cmdA.listSamples()).toHaveLength(2);
		expect(await cmdA.getSample(1)).toBeNull();
		expect(await cmdA.getSample(2)).toBeNull();
		expect(await cmdA.getSample(3)).not.toBeNull();
		expect(await cmdA.getSample(4)).not.toBeNull();

		// 4. Cached output log preserves per-line timestamps
		const log = create(CommandLogSchema, {
			commandId: "cmd-a",
			lines: ["alpha", "beta"],
			lineTimestamps: [BigInt(baseNow + 1234), BigInt(baseNow + 2567)],
			cut: false,
		});
		await cmdA.saveOutputLog({
			tail: 2000,
			cut: false,
			doc: toBinary(CommandLogSchema, log),
			nowMs: baseNow + 3000,
		});
		const gotLog = await cmdA.getOutputLog();
		expect(gotLog).not.toBeNull();
		const decodedLog = fromBinary(CommandLogSchema, gotLog!.doc);
		expect(decodedLog.lines).toEqual(["alpha", "beta"]);
		expect(decodedLog.lineTimestamps).toEqual([BigInt(baseNow + 1234), BigInt(baseNow + 2567)]);

		// 5. Alarm self-prunes expired CommandDO storage
		await cmdA.expireNowForTest();
		await runDurableObjectAlarm(cmdA);
		expect(await cmdA.getProfile()).toBeNull();
	});

	it("routes PushEvents profiles/diagnostics, SaveCommandDiagnostics, SaveCommandSample, and GetProfile through CommandDO", async () => {
		const testEnv = env as unknown as ControllerEnv;
		const owner = makeClients(OWNER_TOKEN);
		const joiner = makeClients(JOIN_TOKEN, {
			"X-Hotbed-Node-Id": "node-cmddo",
			"X-Hotbed-Host": "cmddo-host",
		});

		const joined = await joiner.cluster.join({});
		expect(joined.workerToken).toBeTruthy();
		const worker = makeClients(joined.workerToken, {
			"X-Hotbed-Node-Id": "node-cmddo",
			"X-Hotbed-Host": "cmddo-host",
		});
		const cmdId = "cmd-do-e2e-1";
		const nowMs = BigInt(Date.now());

		const pushRes = await worker.cluster.pushEvents({
			clusterId: joined.clusterId,
			epoch: "epoch-cmddo",
			checkpoint: {
				seq: 1n,
				commands: [
					create(CommandSchema, {
						id: cmdId,
						workspaceId: "ws-1",
						nodeId: "node-cmddo",
						state: CommandState.SUCCEEDED,
						argv: "go test ./...",
						endedAt: nowMs,
					}),
				],
			},
			events: [
				{
					seq: 1n,
					at: nowMs,
					change: {
						case: "command",
						value: {
							what: "finished",
							command: create(CommandSchema, {
								id: cmdId,
								workspaceId: "ws-1",
								nodeId: "node-cmddo",
								state: CommandState.SUCCEEDED,
								argv: "go test ./...",
								endedAt: nowMs,
							}),
							finished: create(FinishedCommandSchema, {
								id: cmdId,
								workspaceId: "ws-1",
								nodeId: "node-cmddo",
								state: CommandState.SUCCEEDED,
								argv: "go test ./...",
								endedAt: nowMs,
								seconds: 1.5,
							}),
							profile: create(CommandProfileSchema, {
								commandId: cmdId,
								profiler: "opentelemetry-ebpf-profiler",
								active: true,
								totalSamples: 99n,
								topFunctions: [
									create(ProfileFunctionSchema, {
										frame: 0,
										selfSamples: 99n,
										totalSamples: 99n,
									}),
								],
							}),
						},
					},
				},
			],
		});
		expect(pushRes.checkpointNeeded).toBe(false);

		// Profile is read back via GetProfile from CommandDO
		const prof = await owner.workspaces.getProfile({
			target: { case: "commandId", value: cmdId },
		});
		expect(prof.totalSamples).toBe(99n);
		expect(prof.live).toBe(false);

		// Verify it physically lives in CommandDO (accessed via typed RPC)
		const directCmdDo = testEnv.COMMAND.getByName(commandDoName(joined.clusterId, cmdId));
		expect(await directCmdDo.getProfile()).not.toBeNull();

		// SaveCommandDiagnostics & GetCommandDiagnostics
		await worker.cluster.saveCommandDiagnostics({
			diagnostics: create(CommandDiagnosticsSchema, {
				commandId: cmdId,
				intervention: create(CommandInterventionSchema, {
					seq: 1,
					kind: InterventionKind.STOP,
					comment: "stuck",
					by: create(StampSchema, { at: nowMs, host: "cmddo-host" }),
				}),
			}),
		});
		const diags = await owner.cluster.getCommandDiagnostics({ id: cmdId });
		expect(diags.diagnostics).toHaveLength(1);
		expect(diags.diagnostics[0].intervention?.comment).toBe("stuck");
		expect(await directCmdDo.hasDiagnostics()).toBe(true);

		// SaveCommandSample, ListCommandSamples, GetCommandSample
		await worker.cluster.saveCommandSample({
			sample: create(CommandSampleSchema, {
				commandId: cmdId,
				seq: 1,
				durationSeconds: 3,
				by: create(StampSchema, { at: nowMs, host: "cmddo-host" }),
			}),
		});
		const listed = await owner.cluster.listCommandSamples({ id: cmdId });
		expect(listed.samples).toHaveLength(1);
		expect(listed.samples[0].seq).toBe(1);
		const fetchedSample = await owner.cluster.getCommandSample({ id: cmdId, seq: 1 });
		expect(fetchedSample.durationSeconds).toBe(3);

		// Cached output log in CommandDO is returned by GetLogLines even when worker is offline
		const cachedLog = create(CommandLogSchema, {
			commandId: cmdId,
			lines: ["line-1", "line-2", "line-3"],
			lineTimestamps: [1_700_000_001_000n, 1_700_000_002_000n, 1_700_000_003_000n],
			cut: false,
		});
		await directCmdDo.saveOutputLog({
			tail: 2000,
			cut: false,
			doc: toBinary(CommandLogSchema, cachedLog),
			nowMs: Number(nowMs),
		});
		const tail2 = await owner.workspaces.getLogLines({ id: cmdId, tail: 2 });
		expect(tail2.lines).toEqual(["line-2", "line-3"]);
		expect(tail2.lineTimestamps).toEqual([1_700_000_002_000n, 1_700_000_003_000n]);
	});

	it("serves ControllerDO via typed Workers RPC and rejects non-WebSocket HTTP fetch() calls", async () => {
		const testEnv = env as unknown as ControllerEnv;
		const stub = testEnv.CONTROLLER.getByName("rpc-only-check");

		// Typed Workers RPC works
		const health = await stub.getHealth();
		expect(health.ok).toBe(true);
		expect(health.clusterId).toBeTruthy();

		// Typed signup / handoff RPCs work
		expect(await stub.checkSignupApproved("user_unapproved")).toBe(false);
		const handoff1 = await stub.consumeCliHandoff("jti-rpc-1", Date.now() + 60_000);
		expect(handoff1.ok).toBe(true);
		const handoff2 = await stub.consumeCliHandoff("jti-rpc-1", Date.now() + 60_000);
		expect(handoff2.ok).toBe(false);

		// Legacy /internal/* HTTP routes no longer exist even via handleHttpRequest
		const internalRes = await stub.handleHttpRequest(
			new Request("https://controller.test/internal/signup/check", {
				method: "POST",
				body: JSON.stringify({ userId: "u1" }),
			}),
		);
		expect(internalRes.status).toBe(404);

		// Direct stub.fetch() refuses non-WebSocket HTTP calls
		const directFetchRes = await stub.fetch(new Request("https://controller.test/healthz"));
		expect(directFetchRes.status).toBe(405);

		// Main ControllerDO SQLite no longer has command_profiles, command_diagnostics, or command_samples tables
		const tables = await runInDurableObject(stub, (_instance, state) =>
			state.storage.sql
				.exec("SELECT name FROM sqlite_master WHERE type = 'table'")
				.toArray()
				.map((r) => (typeof r.name === "string" ? r.name : "")),
		);
		expect(tables).not.toContain("command_profiles");
		expect(tables).not.toContain("command_diagnostics");
		expect(tables).not.toContain("command_samples");
	});
});
