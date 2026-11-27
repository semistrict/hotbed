import { SELF } from "cloudflare:test";
import {
	create,
	fromBinary,
	toBinary,
	type DescMessage,
	type MessageShape,
} from "@bufbuild/protobuf";
import { Code, ConnectError, createClient, type Interceptor } from "@connectrpc/connect";
import { createFetchClient } from "@connectrpc/connect/protocol";
import { createTransport } from "@connectrpc/connect/protocol-connect";
import { describe, expect, it } from "vite-plus/test";
import {
	CheckpointSchema,
	ClusterService,
	CommandSchema,
	CommandState,
	ErrorInfoSchema,
	OutputLineSchema,
	OutputQuerySchema,
	QueryOutputRequestSchema,
	ResourcesSchema,
	SessionService,
	WorkspaceService,
} from "../src/gen/hotbed/v1/hotbed_pb.ts";
import { ErrorCode, HotbedError } from "../src/errors.ts";
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

describe("WorkerTunnelHub calls aborted by their caller", () => {
	function hubWithWorker() {
		const { state } = fakeDurableObjectState();
		const hub = new WorkerTunnelHub(state, 50);
		const ws = new FakeWebSocket();
		hub.accept(ws as unknown as WebSocket, {
			nodeId: "node-a",
			hostname: "macbook",
			connectedAt: 1000,
		});
		return { hub, ws };
	}

	function query(hub: WorkerTunnelHub, signal: AbortSignal) {
		return hub.callUnary(
			"node-a",
			"macbook",
			"/hotbed.v1.WorkspaceService/QueryOutput",
			QueryOutputRequestSchema,
			create(QueryOutputRequestSchema, { id: "rw4jqbpo", wait: true, timeoutSeconds: 30 }),
			OutputQuerySchema,
			60_000,
			signal,
		);
	}

	// A caller's deadline (Connect-Timeout-Ms) aborts the handler's signal
	// with DeadlineExceeded while the caller may still be listening: what it
	// hears is the worker not answering in time, which it may retry.
	it("ends a call its caller's deadline aborted as a retryable worker timeout", async () => {
		const { hub, ws } = hubWithWorker();
		const ac = new AbortController();
		const call = query(hub, ac.signal);
		await Promise.resolve();
		expect(ws.sent.length).toBe(1);
		ac.abort(new ConnectError("the operation timed out", Code.DeadlineExceeded));
		const err = await call.then(
			() => null,
			(e: unknown) => e,
		);
		expect(err).toBeInstanceOf(HotbedError);
		expect((err as HotbedError).toJSON()).toEqual({
			code: ErrorCode.WorkerUnreachable,
			message:
				"macbook: /hotbed.v1.WorkspaceService/QueryOutput timed out: its caller's deadline passed first",
			retryable: true,
		});
		expect(JSON.parse(ws.sent[1]!)).toEqual({ type: "rpc_cancel", id: JSON.parse(ws.sent[0]!).id });
	});

	it("ends a call its caller abandoned as canceled", async () => {
		const { hub } = hubWithWorker();
		const ac = new AbortController();
		const call = query(hub, ac.signal);
		await Promise.resolve();
		ac.abort();
		const err = await call.then(
			() => null,
			(e: unknown) => e,
		);
		expect((err as HotbedError).toJSON()).toEqual({
			code: ErrorCode.Internal,
			message: "macbook: /hotbed.v1.WorkspaceService/QueryOutput canceled",
			retryable: false,
		});
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

	function replyTo(ws: WebSocket, id: string, out: MessageShape<typeof OutputQuerySchema>) {
		ws.send(
			JSON.stringify({
				type: "rpc_msg",
				id,
				body: encodeBase64(encodeProto(OutputQuerySchema, out)),
			} satisfies TunnelUpMessage),
		);
	}

	// A caller that sends Connect-Timeout-Ms but does not enforce it itself
	// (or does so a little late, as a loaded client does) hears whatever the
	// controller answers as that deadline passes; the controller must answer
	// first, never let its own abort of the call race the caller.
	function clientsWithDeadline(timeoutMs: number) {
		return createClient(
			WorkspaceService,
			makeTransport(OWNER_TOKEN, {
				"X-Hotbed-Host": "macbook",
				"Connect-Timeout-Ms": String(timeoutMs),
			}),
		);
	}

	it("answers a wait that its caller's deadline cuts short before that deadline", async () => {
		const nodeId = "node-wait-caller-deadline";
		const hostname = "macbook-caller-deadline";
		const cmdId = "cd5wait01";
		await seedRunningCommand(nodeId, hostname, cmdId);
		const slices: number[] = [];
		// The worker waits each slice out, as a real one does, and says it
		// timed out.
		const ws = await connectWorker(nodeId, hostname, (req, ws, id) => {
			slices.push(req.timeoutSeconds);
			setTimeout(() => {
				replyTo(
					ws,
					id,
					create(OutputQuerySchema, {
						commandId: cmdId,
						state: CommandState.RUNNING,
						totalLines: 10n,
						waited: "timeout",
					}),
				);
			}, req.timeoutSeconds * 1000);
		});
		try {
			const started = Date.now();
			const res = await clientsWithDeadline(36_000).queryOutput({
				id: cmdId,
				wait: true,
				timeoutSeconds: 600,
			});
			expect(res.waited).toBe("timeout");
			expect(res.state).toBe(CommandState.RUNNING);
			// 36s less the answer's margin (5s) and the last slice's worker
			// allowance (30s): a single 1s slice.
			expect(slices).toEqual([1]);
			expect(Date.now() - started).toBeLessThan(31_000);
		} finally {
			ws.close(1000, "test done");
		}
	});

	it("tells a caller whose deadline a silent worker outlasts to retry, before that deadline", async () => {
		const nodeId = "node-wait-silent-worker";
		const hostname = "macbook-silent";
		const cmdId = "cd5wait02";
		await seedRunningCommand(nodeId, hostname, cmdId);
		let calls = 0;
		const ws = await connectWorker(nodeId, hostname, () => {
			calls++;
		});
		try {
			const started = Date.now();
			const err = await clientsWithDeadline(4_000)
				.queryOutput({ id: cmdId, wait: true, timeoutSeconds: 600 })
				.then(
					() => null,
					(e: unknown) => e,
				);
			const elapsed = Date.now() - started;
			expect(err).toBeInstanceOf(ConnectError);
			const ce = err as ConnectError;
			expect(ce.code).toBe(Code.Unavailable);
			expect(ce.rawMessage).toBe(`${hostname}: /hotbed.v1.WorkspaceService/QueryOutput timed out`);
			const [info] = ce.findDetails(ErrorInfoSchema);
			expect(info?.code).toBe(ErrorCode.WorkerUnreachable);
			expect(info?.retryable).toBe(true);
			expect(calls).toBe(1);
			// Half of 4s, its margin, for the answer's way back.
			expect(elapsed).toBeGreaterThanOrEqual(2_000);
			expect(elapsed).toBeLessThan(4_000);
		} finally {
			ws.close(1000, "test done");
		}
	});

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

	it("retries a plain (non-waiting) QueryOutput across a worker tunnel reconnect", async () => {
		const nodeId = "node-plain-retry";
		const hostname = "mac-plain";
		const cmdId = "plain12345";
		const owner = await seedRunningCommand(nodeId, hostname, cmdId);

		let firstArrived!: () => void;
		const firstArrivedP = new Promise<void>((r) => {
			firstArrived = r;
		});
		const ws1 = await connectWorker(nodeId, hostname, () => {
			firstArrived();
		});

		const readP = owner.workspaces.queryOutput({ id: cmdId, after: 95n });
		await firstArrivedP;
		ws1.close(1012, "restarting");

		const ws2 = await connectWorker(nodeId, hostname, (_req, ws, id) => {
			const out = create(OutputQuerySchema, {
				commandId: cmdId,
				state: CommandState.RUNNING,
				totalLines: 110n,
				nextLine: 110n,
				lines: [create(OutputLineSchema, { number: 96n, text: "line 96" })],
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
			const res = await readP;
			expect(res.totalLines).toBe(110n);
			expect(res.lines[0]?.text).toBe("line 96");
		} finally {
			ws2.close(1000, "test done");
		}
	});
});
