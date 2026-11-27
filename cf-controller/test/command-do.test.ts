import { env, runDurableObjectAlarm, runInDurableObject, SELF } from "cloudflare:test";
import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import { createClient, type Interceptor } from "@connectrpc/connect";
import { createFetchClient } from "@connectrpc/connect/protocol";
import { createTransport } from "@connectrpc/connect/protocol-connect";
import { describe, expect, it, vi } from "vite-plus/test";
import { commandDoName, type CommandDO } from "../src/command-do.ts";
import type { ControllerEnv } from "../src/controller-do.ts";
import {
	CheckpointSchema,
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
	ResourcesSchema,
	StampSchema,
	WorkspaceService,
} from "../src/gen/hotbed/v1/hotbed_pb.ts";
import type { TunnelDownMessage, TunnelUpMessage } from "../src/tunnel.ts";

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

		// Workers push live output chunks to CommandDO as output arrives; GetLogLines
		// and FollowLog serve directly from CommandDO without hitting any worker tunnel.
		const liveCmdId = "c-live-stream";
		const enc = new TextEncoder();
		const b1 = enc.encode("hello\nprogress 10%\rprogress 100%\n");
		const r1 = await worker.cluster.pushCommandOutput({
			commandId: liveCmdId,
			chunks: [
				{
					offset: 0n,
					nextOffset: BigInt(b1.byteLength),
					receivedAt: 1_700_000_100_000n,
					data: b1,
				},
			],
			eof: false,
		});
		expect(r1.ackedOffset).toBe(BigInt(b1.byteLength));

		// Duplicate retry at offset 0 is idempotent.
		const rDup = await worker.cluster.pushCommandOutput({
			commandId: liveCmdId,
			chunks: [
				{
					offset: 0n,
					nextOffset: BigInt(b1.byteLength),
					receivedAt: 1_700_000_100_000n,
					data: b1,
				},
			],
			eof: false,
		});
		expect(rDup.ackedOffset).toBe(BigInt(b1.byteLength));

		// Second chunk arrives with ANSI color + eof=true.
		const b2 = enc.encode("\x1b[32mok\x1b[0m\n");
		const r2 = await worker.cluster.pushCommandOutput({
			commandId: liveCmdId,
			chunks: [
				{
					offset: BigInt(b1.byteLength),
					nextOffset: BigInt(b1.byteLength + b2.byteLength),
					receivedAt: 1_700_000_200_000n,
					data: b2,
				},
			],
			eof: true,
		});
		expect(r2.ackedOffset).toBe(BigInt(b1.byteLength + b2.byteLength));

		// GetLogLines settles CR redraws and strips ANSI escapes with exact per-line timestamps.
		const liveLines = await owner.workspaces.getLogLines({ id: liveCmdId, tail: 10 });
		expect(liveLines.lines).toEqual(["hello", "progress 100%", "ok"]);
		expect(liveLines.lineTimestamps).toEqual([
			1_700_000_100_000n,
			1_700_000_100_000n,
			1_700_000_200_000n,
		]);

		// FollowLog (non-follow) streams raw chunks & timestamps directly from CommandDO,
		// including mid-chunk offset slicing.
		const rawChunks: Array<{ text: string; nextOffset: bigint; receivedAt: bigint }> = [];
		for await (const chunk of owner.workspaces.followLog({
			id: liveCmdId,
			offset: 6n, // after "hello\n"
			follow: false,
			timestamps: true,
		})) {
			rawChunks.push({
				text: new TextDecoder().decode(chunk.data),
				nextOffset: chunk.nextOffset,
				receivedAt: chunk.receivedAt,
			});
		}
		expect(rawChunks).toEqual([
			{
				text: "progress 10%\rprogress 100%\n",
				nextOffset: BigInt(b1.byteLength),
				receivedAt: 1_700_000_100_000n,
			},
			{
				text: "\x1b[32mok\x1b[0m\n",
				nextOffset: BigInt(b1.byteLength + b2.byteLength),
				receivedAt: 1_700_000_200_000n,
			},
		]);
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

	it("reads stored output from the tail only up to its byte budget", async () => {
		const testEnv = env as unknown as ControllerEnv;
		const stub = testEnv.COMMAND.getByName(commandDoName("cluster-test", "cmd-budget"));
		const chunkBytes = 256 << 10;
		const chunkCount = 40;
		const line = `${"x".repeat(1023)}\n`;
		const chunks = Array.from({ length: chunkCount }, (_, i) => ({
			offset: i * chunkBytes,
			nextOffset: (i + 1) * chunkBytes,
			receivedAt: 1_700_000_000_000 + i,
			data: new TextEncoder().encode(line.repeat(chunkBytes / line.length)),
		}));
		for (let i = 0; i < chunkCount; i += 8) {
			await stub.appendOutputChunks({ chunks: chunks.slice(i, i + 8), eof: i + 8 >= chunkCount });
		}

		const { rowsRead, result } = await runInDurableObject(stub, (instance: CommandDO, state) => {
			const cursors: SqlStorageCursor<Record<string, SqlStorageValue>>[] = [];
			const exec = state.storage.sql.exec.bind(state.storage.sql);
			const spy = vi
				.spyOn(state.storage.sql, "exec")
				.mockImplementation((query: string, ...bindings: unknown[]) => {
					const cursor = exec(query, ...bindings);
					if (query.includes("ORDER BY offset DESC")) cursors.push(cursor);
					return cursor;
				});
			try {
				const result = instance.getOutputLines(10, 64 << 10);
				return { rowsRead: cursors.map((c) => c.rowsRead), result };
			} finally {
				spy.mockRestore();
			}
		});
		// The 2 MiB read budget is 8 chunks of 256 KiB; the ninth crosses it, and the cursor has read one row ahead.
		expect(rowsRead).toEqual([10]);
		expect(result.lines).toEqual(Array.from({ length: 10 }, () => "x".repeat(1023)));
		expect(result.eof).toBe(true);
	});

	it("serves writes after clear() wiped the schema on a live instance", async () => {
		const testEnv = env as unknown as ControllerEnv;
		const stub = testEnv.COMMAND.getByName(commandDoName("cluster-test", "cmd-cleared"));
		const data = new TextEncoder().encode("before\n");
		await stub.appendOutputChunks({
			chunks: [{ offset: 0, nextOffset: data.byteLength, receivedAt: 1, data }],
			eof: true,
		});
		await stub.clear();
		expect(await stub.getOutputLines()).toEqual({
			lines: [],
			lineTimestamps: [],
			cut: false,
			eof: false,
			hasOutput: false,
		});
		const after = new TextEncoder().encode("after\n");
		await stub.appendOutputChunks({
			chunks: [{ offset: 0, nextOffset: after.byteLength, receivedAt: 2, data: after }],
			eof: true,
		});
		expect((await stub.getOutputLines()).lines).toEqual(["after"]);
	});

	it("queries the online worker when CommandDO only holds an incomplete prefix, and falls back if offline", async () => {
		const owner = makeClients(OWNER_TOKEN);
		const nodeId = "node-partial-log";
		const hostname = "partial-log-host";
		const joiner = makeClients(JOIN_TOKEN, {
			"X-Hotbed-Node-Id": nodeId,
			"X-Hotbed-Host": hostname,
		});
		const joined = await joiner.cluster.join({});
		const worker = makeClients(joined.workerToken, {
			"X-Hotbed-Node-Id": nodeId,
			"X-Hotbed-Host": hostname,
		});
		await worker.cluster.heartbeat({
			clusterId: joined.clusterId,
			resources: create(ResourcesSchema, { arch: "amd64", cpus: 4, memoryBytes: 4n << 30n }),
		});

		const cmdId = "cmd-partial-prefix";
		await worker.cluster.pushEvents({
			clusterId: joined.clusterId,
			epoch: "epoch-partial",
			checkpoint: create(CheckpointSchema, {
				seq: 1n,
				commands: [
					create(CommandSchema, {
						id: cmdId,
						workspaceId: "ws-partial",
						nodeId,
						state: CommandState.RUNNING,
						argv: "emit-big-log",
					}),
				],
			}),
		});

		// Push only the first prefix chunk with eof=false (more output still on the worker).
		const enc = new TextEncoder();
		const prefix = enc.encode("prefix-only\n");
		await worker.cluster.pushCommandOutput({
			commandId: cmdId,
			chunks: [
				{
					offset: 0n,
					nextOffset: BigInt(prefix.byteLength),
					receivedAt: 1_700_000_001_000n,
					data: prefix,
				},
			],
			eof: false,
		});

		// Connect a live worker tunnel that serves the full log on GetLogLines.
		const wsRes = await SELF.fetch("https://controller.test/ws/worker", {
			headers: {
				Upgrade: "websocket",
				Authorization: `Bearer ${joined.workerToken}`,
				"X-Hotbed-Node-Id": nodeId,
				"X-Hotbed-Host": hostname,
			},
		});
		expect(wsRes.status).toBe(101);
		const ws = wsRes.webSocket!;
		ws.accept();
		let workerCalls = 0;
		let forgetCommand = false;
		ws.addEventListener("message", (ev) => {
			const raw = typeof ev.data === "string" ? ev.data : new TextDecoder().decode(ev.data);
			if (raw === "pong") return;
			const msg = JSON.parse(raw) as TunnelDownMessage;
			if (msg.type !== "rpc_req" || msg.procedure !== "/hotbed.v1.WorkspaceService/GetLogLines") {
				return;
			}
			workerCalls++;
			if (forgetCommand) {
				ws.send(
					JSON.stringify({
						type: "rpc_err",
						id: msg.id,
						code: "not_found",
						message: "command forgotten",
					} satisfies TunnelUpMessage),
				);
				return;
			}
			const body = toBinary(
				CommandLogSchema,
				create(CommandLogSchema, {
					commandId: cmdId,
					lines: ["final-line-from-worker"],
					cut: true,
				}),
			);
			let bin = "";
			for (let i = 0; i < body.length; i++) bin += String.fromCharCode(body[i]);
			ws.send(
				JSON.stringify({
					type: "rpc_msg",
					id: msg.id,
					body: btoa(bin),
				} satisfies TunnelUpMessage),
			);
		});

		try {
			// While eof=false and worker is online, GetLogLines must ask the worker rather than returning the stale prefix.
			const fromWorker = await owner.workspaces.getLogLines({ id: cmdId, tail: 3 });
			expect(workerCalls).toBe(1);
			expect(fromWorker.lines).toEqual(["final-line-from-worker"]);
			expect(fromWorker.cut).toBe(true);

			// If the worker fails (e.g. forgot the command or went offline), GetLogLines falls back to the partial prefix.
			forgetCommand = true;
			const fallback = await owner.workspaces.getLogLines({ id: cmdId, tail: 3 });
			expect(workerCalls).toBe(2);
			expect(fallback.lines).toEqual(["prefix-only"]);

			// Once eof=true arrives in CommandDO, GetLogLines serves directly from CommandDO without hitting the worker.
			const suffix = enc.encode("final-line-in-do\n");
			await worker.cluster.pushCommandOutput({
				commandId: cmdId,
				chunks: [
					{
						offset: BigInt(prefix.byteLength),
						nextOffset: BigInt(prefix.byteLength + suffix.byteLength),
						receivedAt: 1_700_000_002_000n,
						data: suffix,
					},
				],
				eof: true,
			});
			const complete = await owner.workspaces.getLogLines({ id: cmdId, tail: 3 });
			expect(workerCalls).toBe(2);
			expect(complete.lines).toEqual(["prefix-only", "final-line-in-do"]);
		} finally {
			ws.close(1000, "test done");
		}
	});
});
