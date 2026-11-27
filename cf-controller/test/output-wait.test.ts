import { SELF } from "cloudflare:test";
import {
	create,
	fromBinary,
	toBinary,
	type DescMessage,
	type MessageShape,
} from "@bufbuild/protobuf";
import { createClient, type Interceptor } from "@connectrpc/connect";
import { createFetchClient } from "@connectrpc/connect/protocol";
import { createTransport } from "@connectrpc/connect/protocol-connect";
import { describe, expect, it } from "vite-plus/test";
import {
	CheckpointSchema,
	ClusterService,
	CommandSchema,
	CommandState,
	OutputLineSchema,
	OutputQuerySchema,
	QueryOutputRequestSchema,
	ResourcesSchema,
	SessionService,
	WorkspaceService,
} from "../src/gen/hotbed/v1/hotbed_pb.ts";
import {
	WorkerTunnelHub,
	type TunnelDownMessage,
	type TunnelUpMessage,
	type WorkerSocketAttachment,
} from "../src/tunnel.ts";

const OWNER_TOKEN = "test-owner-secret";

function encodeBase64(bytes: Uint8Array): string {
	let bin = "";
	for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
	return btoa(bin);
}

function decodeBase64(b64: string): Uint8Array {
	if (!b64) return new Uint8Array(0);
	const bin = atob(b64);
	const out = new Uint8Array(bin.length);
	for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
	return out;
}

function encodeProto<T extends DescMessage>(schema: T, msg: MessageShape<T>): Uint8Array {
	return toBinary(schema, msg);
}

function makeTransport(token?: string, extraHeaders?: Record<string, string>) {
	const authInterceptor: Interceptor = (next) => async (req) => {
		if (token) req.header.set("Authorization", `Bearer ${token}`);
		for (const [k, v] of Object.entries(extraHeaders ?? {})) {
			req.header.set(k, v);
		}
		return next(req);
	};
	return createTransport({
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
}

function ownerClients() {
	const transport = makeTransport(OWNER_TOKEN, { "X-Hotbed-Host": "macbook" });
	return {
		cluster: createClient(ClusterService, transport),
		sessions: createClient(SessionService, transport),
		workspaces: createClient(WorkspaceService, transport),
	};
}

class FakeWebSocket {
	readyState = WebSocket.OPEN;
	sent: string[] = [];
	closed = false;
	private attachment: unknown = null;

	send(data: string): void {
		if (this.readyState !== WebSocket.OPEN) {
			throw new Error("socket closed");
		}
		this.sent.push(data);
	}

	close(): void {
		this.closed = true;
		this.readyState = WebSocket.CLOSED;
	}

	serializeAttachment(value: unknown): void {
		this.attachment = value;
	}

	deserializeAttachment(): unknown {
		return this.attachment;
	}
}

function fakeDurableObjectState() {
	const sockets = new Set<{ ws: FakeWebSocket; tags: string[] }>();
	return {
		state: {
			setWebSocketAutoResponse: () => {},
			acceptWebSocket: (ws: WebSocket, tags?: string[]) => {
				sockets.add({ ws: ws as unknown as FakeWebSocket, tags: tags ?? [] });
			},
			getWebSockets: (tag?: string) => {
				const out: WebSocket[] = [];
				for (const s of sockets) {
					if (tag && !s.tags.includes(tag)) continue;
					out.push(s.ws as unknown as WebSocket);
				}
				return out;
			},
		} as unknown as DurableObjectState,
		removeSocket: (ws: FakeWebSocket) => {
			for (const s of sockets) {
				if (s.ws === ws) {
					sockets.delete(s);
					break;
				}
			}
		},
	};
}

describe("WorkerTunnelHub socket ownership on reconnect", () => {
	it("fails pending calls from a closed socket even when a replacement socket is already open", async () => {
		const { state, removeSocket } = fakeDurableObjectState();
		const hub = new WorkerTunnelHub(state, 50);

		const ws1 = new FakeWebSocket();
		const meta1: WorkerSocketAttachment = {
			nodeId: "node-a",
			hostname: "asahimbp",
			connectedAt: 1000,
		};
		hub.accept(ws1 as unknown as WebSocket, meta1);

		// Start a 10-minute waiting QueryOutput call on ws1.
		const call1 = hub.callUnary(
			"node-a",
			"asahimbp",
			"/hotbed.v1.WorkspaceService/QueryOutput",
			QueryOutputRequestSchema,
			create(QueryOutputRequestSchema, { id: "rw4jqbpo", wait: true, timeoutSeconds: 600 }),
			OutputQuerySchema,
			630_000,
		);
		await Promise.resolve();
		expect(ws1.sent.length).toBe(1);

		// Simulate Cloudflare removing dead ws1 from getWebSockets() BEFORE ws2 connects
		// and BEFORE webSocketClose(ws1) fires: accept(ws2) must still fail call1 immediately
		// rather than letting call1 hang for 630 seconds.
		removeSocket(ws1);
		ws1.readyState = WebSocket.CLOSED;

		const ws2 = new FakeWebSocket();
		hub.accept(ws2 as unknown as WebSocket, { ...meta1, connectedAt: 2000 });

		await expect(call1).rejects.toThrow(/reconnected/);

		// A new call dispatched on ws2 must NOT be broken when stale handleClose(ws1) arrives late.
		const call2 = hub.callUnary(
			"node-a",
			"asahimbp",
			"/hotbed.v1.WorkspaceService/QueryOutput",
			QueryOutputRequestSchema,
			create(QueryOutputRequestSchema, { id: "rw4jqbpo", tail: 40 }),
			OutputQuerySchema,
			10_000,
		);
		await Promise.resolve();
		expect(ws2.sent.length).toBe(1);

		// Late handleClose(ws1) must only fail calls belonging to ws1, never call2 on ws2.
		hub.handleClose(ws1 as unknown as WebSocket, 1006, "eof");

		const frame = JSON.parse(ws2.sent[0]) as TunnelDownMessage;
		expect(frame.type).toBe("rpc_req");
		const reply = encodeProto(
			OutputQuerySchema,
			create(OutputQuerySchema, {
				commandId: "rw4jqbpo",
				state: CommandState.SUCCEEDED,
				exitCode: 0,
				totalLines: 133n,
				waited: "ended",
			}),
		);
		hub.handleMessage(
			ws2 as unknown as WebSocket,
			JSON.stringify({
				type: "rpc_msg",
				id: frame.id,
				body: encodeBase64(reply),
			} satisfies TunnelUpMessage),
		);
		const out = await call2;
		expect(out.state).toBe(CommandState.SUCCEEDED);
		expect(out.totalLines).toBe(133n);
	});
});

describe("WorkspaceService.QueryOutput --wait resilience", () => {
	async function seedRunningCommand(nodeId: string, hostname: string, cmdId: string) {
		const owner = ownerClients();
		const workerTransport = makeTransport(OWNER_TOKEN, {
			"X-Hotbed-Node-Id": nodeId,
			"X-Hotbed-Host": hostname,
		});
		const workerCluster = createClient(ClusterService, workerTransport);
		const joined = await workerCluster.join({});
		await workerCluster.heartbeat({
			clusterId: joined.clusterId,
			resources: create(ResourcesSchema, { arch: "arm64", cpus: 11, memoryBytes: 8n << 30n }),
		});
		await workerCluster.pushEvents({
			clusterId: joined.clusterId,
			checkpoint: create(CheckpointSchema, {
				seq: 1n,
				commands: [
					create(CommandSchema, {
						id: cmdId,
						workspaceId: "ws-wait-1",
						nodeId,
						argv: "bazel test //...",
						kind: "shell",
						state: CommandState.RUNNING,
						queuedAt: 1000n,
						startedAt: 1000n,
					}),
				],
			}),
		});
		return owner;
	}

	async function connectWorker(
		nodeId: string,
		hostname: string,
		onQuery: (
			req: MessageShape<typeof QueryOutputRequestSchema>,
			ws: WebSocket,
			id: string,
		) => void,
	) {
		const res = await SELF.fetch("https://controller.test/ws/worker", {
			headers: {
				Upgrade: "websocket",
				Authorization: `Bearer ${OWNER_TOKEN}`,
				"X-Hotbed-Node-Id": nodeId,
				"X-Hotbed-Host": hostname,
			},
		});
		expect(res.status).toBe(101);
		const ws = res.webSocket!;
		ws.accept();
		ws.addEventListener("message", (ev) => {
			const raw = typeof ev.data === "string" ? ev.data : new TextDecoder().decode(ev.data);
			if (raw === "pong") return;
			const msg = JSON.parse(raw) as TunnelDownMessage;
			if (msg.type !== "rpc_req" || msg.procedure !== "/hotbed.v1.WorkspaceService/QueryOutput") {
				return;
			}
			const req = fromBinary(QueryOutputRequestSchema, decodeBase64(msg.body));
			onQuery(req, ws, msg.id);
		});
		return ws;
	}

	it("slices 600s wait into bounded worker RPCs and rides out a worker reconnect mid-wait", async () => {
		const nodeId = "node-wait-reconnect";
		const hostname = "asahimbp-wait";
		const cmdId = "rw4jqbpo";
		const owner = await seedRunningCommand(nodeId, hostname, cmdId);

		const seenSliceTimeouts: number[] = [];
		let firstCallArrived!: () => void;
		const firstCallPromise = new Promise<void>((r) => {
			firstCallArrived = r;
		});

		// Worker connection #1 receives QueryOutput --wait --timeout 600, records
		// that the controller sliced it to <= 30s, and then never replies (simulating
		// a worker restart/upgrade while the wait is in flight).
		const ws1 = await connectWorker(nodeId, hostname, (req) => {
			seenSliceTimeouts.push(req.timeoutSeconds);
			firstCallArrived();
		});

		const waitPromise = owner.workspaces.queryOutput({
			id: cmdId,
			wait: true,
			timeoutSeconds: 600,
			tail: 120,
		});

		await firstCallPromise;
		expect(seenSliceTimeouts[0]).toBeGreaterThan(0);
		expect(seenSliceTimeouts[0]).toBeLessThanOrEqual(30);

		// Simulate worker restart: old socket closes and new socket connects;
		// slice 1 on ws2 returns timeout, slice 2 returns completed output.
		ws1.close(1012, "restarting for upgrade");
		let ws2Calls = 0;
		const ws2 = await connectWorker(nodeId, hostname, (req, ws, id) => {
			seenSliceTimeouts.push(req.timeoutSeconds);
			ws2Calls++;
			const out =
				ws2Calls === 1
					? create(OutputQuerySchema, {
							commandId: cmdId,
							state: CommandState.RUNNING,
							totalLines: 100n,
							waited: "timeout",
						})
					: create(OutputQuerySchema, {
							commandId: cmdId,
							state: CommandState.SUCCEEDED,
							exitCode: 0,
							totalLines: 133n,
							nextLine: 133n,
							waited: "ended",
							lines: [
								create(OutputLineSchema, {
									number: 133n,
									text: "INFO: Build completed successfully",
								}),
							],
						});
			ws.send(
				JSON.stringify({
					type: "rpc_msg",
					id,
					body: encodeBase64(encodeProto(OutputQuerySchema, out)),
				} satisfies TunnelUpMessage),
			);
		});

		try {
			const res = await waitPromise;
			expect(res.state).toBe(CommandState.SUCCEEDED);
			expect(res.waited).toBe("ended");
			expect(res.totalLines).toBe(133n);
			expect(res.lines[0]?.text).toBe("INFO: Build completed successfully");
			expect(ws2Calls).toBe(2);
			expect(seenSliceTimeouts.every((s) => s >= 1 && s <= 30)).toBe(true);
		} finally {
			ws2.close(1000, "test done");
		}
	});
});
