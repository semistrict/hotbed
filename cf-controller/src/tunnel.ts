import { fromBinary, toBinary, type DescMessage, type MessageShape } from "@bufbuild/protobuf";
import { ErrorCode, HotbedError, hotbedError } from "./errors.ts";
import { randomHex } from "./gitops.ts";

/**
 * Attachment persisted across Durable Object WebSocket hibernation.
 */
export interface WorkerSocketAttachment {
	nodeId: string;
	hostname: string;
	addr: string;
	connectedAt: number;
}

/**
 * Wire protocol over the worker <-> controller WebSocket tunnel (/ws/worker).
 *
 * Controller -> Worker:
 *   { type: "rpc_req", id, procedure, body (base64 proto), stream: boolean }
 *   { type: "rpc_cancel", id }
 *
 * Worker -> Controller:
 *   { type: "rpc_msg", id, body (base64 proto) }
 *   { type: "rpc_end", id }
 *   { type: "rpc_err", id, code, message, suggestion?, retryable? }
 */
export type TunnelDownMessage =
	| {
			type: "rpc_req";
			id: string;
			procedure: string;
			body: string;
			stream: boolean;
	  }
	| {
			type: "rpc_cancel";
			id: string;
	  };

export type TunnelUpMessage =
	| {
			type: "rpc_msg";
			id: string;
			body: string;
	  }
	| {
			type: "rpc_end";
			id: string;
	  }
	| {
			type: "rpc_err";
			id: string;
			code: string;
			message: string;
			suggestion?: string;
			retryable?: boolean;
	  };

interface PendingUnary {
	kind: "unary";
	nodeId: string;
	resolve: (bytes: Uint8Array) => void;
	reject: (err: Error) => void;
	timer: ReturnType<typeof setTimeout>;
	cleanup?: () => void;
}

interface PendingStream {
	kind: "stream";
	nodeId: string;
	queue: Uint8Array[];
	queuedBytes: number;
	done: boolean;
	error: Error | null;
	notify: (() => void) | null;
}

const MAX_STREAM_QUEUE_BYTES = 16 * 1024 * 1024;

type PendingCall = PendingUnary | PendingStream;

function encodeBase64(bytes: Uint8Array): string {
	let bin = "";
	const chunk = 0x8000;
	for (let i = 0; i < bytes.length; i += chunk) {
		bin += String.fromCharCode(...bytes.subarray(i, i + chunk));
	}
	return btoa(bin);
}

function decodeBase64(b64: string): Uint8Array {
	if (!b64) return new Uint8Array(0);
	const bin = atob(b64);
	const out = new Uint8Array(bin.length);
	for (let i = 0; i < bin.length; i++) {
		out[i] = bin.charCodeAt(i);
	}
	return out;
}

/**
 * Manages persistent worker WebSocket connections inside ControllerDO
 * using Cloudflare's WebSocket Hibernation API so idle sockets survive
 * DO hibernation and active reverse-RPC calls multiplex cleanly.
 */
export class WorkerTunnelHub {
	private readonly ctx: DurableObjectState;
	private readonly pending = new Map<string, PendingCall>();

	constructor(ctx: DurableObjectState) {
		this.ctx = ctx;
		// Auto-reply to worker keepalive pings without waking the DO from hibernation.
		this.ctx.setWebSocketAutoResponse(new WebSocketRequestResponsePair("ping", "pong"));
	}

	/** Accepts an upgraded WebSocket from a worker, replacing any stale socket for the same node. */
	accept(server: WebSocket, attachment: WorkerSocketAttachment): void {
		// Close any superseded sockets for this nodeId so at most one active tunnel exists per node.
		for (const existing of this.ctx.getWebSockets(attachment.nodeId)) {
			try {
				existing.close(1012, "superseded by newer connection");
			} catch {
				// ignore already-closed socket
			}
			this.failPendingForNode(
				attachment.nodeId,
				hotbedError(ErrorCode.WorkerUnreachable, `${attachment.hostname} reconnected`, {
					retryable: true,
				}),
			);
		}
		this.ctx.acceptWebSocket(server, [attachment.nodeId, `host:${attachment.hostname}`]);
		server.serializeAttachment(attachment);
	}

	private openSocketsFor(nodeId: string): Array<{ ws: WebSocket; meta: WorkerSocketAttachment }> {
		const out: Array<{ ws: WebSocket; meta: WorkerSocketAttachment }> = [];
		for (const ws of this.ctx.getWebSockets(nodeId)) {
			if (ws.readyState !== WebSocket.OPEN) continue;
			const meta = (ws.deserializeAttachment() as WorkerSocketAttachment | null) ?? {
				nodeId,
				hostname: nodeId,
				addr: "",
				connectedAt: 0,
			};
			out.push({ ws, meta });
		}
		out.sort((a, b) => a.meta.connectedAt - b.meta.connectedAt);
		return out;
	}

	/** Returns the active WebSocket for nodeId (works across hibernation via ctx.getWebSockets). */
	private socketFor(nodeId: string): { ws: WebSocket; meta: WorkerSocketAttachment } | null {
		const sockets = this.openSocketsFor(nodeId);
		if (sockets.length === 0) return null;
		return sockets[sockets.length - 1];
	}

	isConnected(nodeId: string): boolean {
		return this.openSocketsFor(nodeId).length > 0;
	}

	connectedNodeIds(): Set<string> {
		const ids = new Set<string>();
		for (const ws of this.ctx.getWebSockets()) {
			if (ws.readyState !== WebSocket.OPEN) continue;
			const meta = ws.deserializeAttachment() as WorkerSocketAttachment | null;
			if (meta?.nodeId) ids.add(meta.nodeId);
		}
		return ids;
	}

	handleMessage(ws: WebSocket, message: string | ArrayBuffer): void {
		const text =
			typeof message === "string" ? message : new TextDecoder().decode(new Uint8Array(message));
		if (text === "ping") {
			ws.send("pong");
			return;
		}
		let msg: TunnelUpMessage;
		try {
			msg = JSON.parse(text) as TunnelUpMessage;
		} catch {
			return;
		}
		const call = this.pending.get(msg.id);
		if (!call) return;
		const sender = ws.deserializeAttachment() as WorkerSocketAttachment | null;
		if (!sender || sender.nodeId !== call.nodeId) return;

		if (msg.type === "rpc_msg") {
			let bytes: Uint8Array;
			try {
				bytes = decodeBase64(msg.body);
			} catch {
				const err = hotbedError(ErrorCode.Internal, "worker tunnel sent invalid base64 payload");
				if (call.kind === "unary") {
					clearTimeout(call.timer);
					call.cleanup?.();
					this.pending.delete(msg.id);
					call.reject(err);
				} else {
					call.error = err;
					call.done = true;
					this.pending.delete(msg.id);
					const notify = call.notify;
					call.notify = null;
					notify?.();
				}
				return;
			}
			if (call.kind === "unary") {
				clearTimeout(call.timer);
				call.cleanup?.();
				this.pending.delete(msg.id);
				call.resolve(bytes);
			} else {
				if (call.queuedBytes + bytes.byteLength > MAX_STREAM_QUEUE_BYTES) {
					call.error = hotbedError(
						ErrorCode.Internal,
						"worker stream exceeded controller buffer limit",
					);
					call.done = true;
					this.pending.delete(msg.id);
					try {
						ws.send(JSON.stringify({ type: "rpc_cancel", id: msg.id } satisfies TunnelDownMessage));
					} catch {
						// ignore
					}
					const notify = call.notify;
					call.notify = null;
					notify?.();
					return;
				}
				call.queue.push(bytes);
				call.queuedBytes += bytes.byteLength;
				const notify = call.notify;
				call.notify = null;
				notify?.();
			}
			return;
		}

		if (msg.type === "rpc_end") {
			if (call.kind === "unary") {
				clearTimeout(call.timer);
				call.cleanup?.();
				this.pending.delete(msg.id);
				call.resolve(new Uint8Array(0));
			} else {
				call.done = true;
				this.pending.delete(msg.id);
				const notify = call.notify;
				call.notify = null;
				notify?.();
			}
			return;
		}

		if (msg.type === "rpc_err") {
			const err = new HotbedError(
				(msg.code as HotbedError["code"]) || ErrorCode.Internal,
				msg.message || "worker RPC failed",
				{ suggestion: msg.suggestion ?? "", retryable: Boolean(msg.retryable) },
			);
			if (call.kind === "unary") {
				clearTimeout(call.timer);
				call.cleanup?.();
				this.pending.delete(msg.id);
				call.reject(err);
			} else {
				call.error = err;
				call.done = true;
				this.pending.delete(msg.id);
				const notify = call.notify;
				call.notify = null;
				notify?.();
			}
		}
	}

	handleClose(ws: WebSocket, _code: number, reason: string): void {
		const meta = ws.deserializeAttachment() as WorkerSocketAttachment | null;
		if (!meta) return;
		// Only fail pending calls if no newer open replacement socket is connected.
		const remaining = this.openSocketsFor(meta.nodeId).filter((s) => s.ws !== ws);
		if (remaining.length === 0) {
			this.failPendingForNode(
				meta.nodeId,
				hotbedError(
					ErrorCode.WorkerUnreachable,
					`${meta.hostname} disconnected${reason ? `: ${reason}` : ""}`,
					{ retryable: true },
				),
			);
		}
	}

	private failPendingForNode(nodeId: string, err: HotbedError): void {
		for (const [id, call] of this.pending) {
			if (call.nodeId !== nodeId) continue;
			this.pending.delete(id);
			if (call.kind === "unary") {
				clearTimeout(call.timer);
				call.cleanup?.();
				call.reject(err);
			} else {
				call.error = err;
				call.done = true;
				const notify = call.notify;
				call.notify = null;
				notify?.();
			}
		}
	}

	/** Invokes a unary WorkspaceService RPC on a connected worker over its WebSocket tunnel. */
	async callUnary<Req extends DescMessage, Res extends DescMessage>(
		nodeId: string,
		host: string,
		procedure: string,
		reqSchema: Req,
		reqMsg: MessageShape<Req>,
		resSchema: Res,
		timeoutMs = 30_000,
		signal?: AbortSignal,
	): Promise<MessageShape<Res>> {
		if (signal?.aborted) {
			throw hotbedError(ErrorCode.Internal, `${host}: ${procedure} canceled`);
		}
		const sock = this.socketFor(nodeId);
		if (!sock) {
			throw hotbedError(
				ErrorCode.WorkerUnreachable,
				`${host} has no active controller WebSocket tunnel`,
				{
					retryable: true,
					suggestion: "check that the worker is running and connected to the controller",
				},
			);
		}
		const id = randomHex(8);
		const body = encodeBase64(toBinary(reqSchema, reqMsg));
		const frame: TunnelDownMessage = {
			type: "rpc_req",
			id,
			procedure,
			body,
			stream: false,
		};

		const bytes = await new Promise<Uint8Array>((resolve, reject) => {
			const sendCancel = () => {
				try {
					sock.ws.send(JSON.stringify({ type: "rpc_cancel", id } satisfies TunnelDownMessage));
				} catch {
					// ignore
				}
			};
			const onAbort = () => {
				clearTimeout(timer);
				this.pending.delete(id);
				sendCancel();
				reject(hotbedError(ErrorCode.Internal, `${host}: ${procedure} canceled`));
			};
			const cleanup = () => {
				signal?.removeEventListener("abort", onAbort);
			};
			const timer = setTimeout(() => {
				cleanup();
				this.pending.delete(id);
				sendCancel();
				reject(
					hotbedError(ErrorCode.WorkerUnreachable, `${host}: ${procedure} timed out`, {
						retryable: true,
					}),
				);
			}, timeoutMs);
			signal?.addEventListener("abort", onAbort, { once: true });
			this.pending.set(id, { kind: "unary", nodeId, resolve, reject, timer, cleanup });
			try {
				sock.ws.send(JSON.stringify(frame));
			} catch (err) {
				clearTimeout(timer);
				cleanup();
				this.pending.delete(id);
				reject(
					hotbedError(
						ErrorCode.WorkerUnreachable,
						`${host}: send over tunnel failed: ${err instanceof Error ? err.message : String(err)}`,
						{ retryable: true },
					),
				);
			}
		});

		return fromBinary(resSchema, bytes);
	}

	/** Invokes a server-streaming WorkspaceService RPC on a connected worker over its WebSocket tunnel. */
	async *callServerStream<Req extends DescMessage, Res extends DescMessage>(
		nodeId: string,
		host: string,
		procedure: string,
		reqSchema: Req,
		reqMsg: MessageShape<Req>,
		resSchema: Res,
		signal?: AbortSignal,
	): AsyncGenerator<MessageShape<Res>> {
		if (signal?.aborted) {
			return;
		}
		const sock = this.socketFor(nodeId);
		if (!sock) {
			throw hotbedError(
				ErrorCode.WorkerUnreachable,
				`${host} has no active controller WebSocket tunnel`,
				{ retryable: true },
			);
		}
		const id = randomHex(8);
		const body = encodeBase64(toBinary(reqSchema, reqMsg));
		const frame: TunnelDownMessage = {
			type: "rpc_req",
			id,
			procedure,
			body,
			stream: true,
		};
		const state: PendingStream = {
			kind: "stream",
			nodeId,
			queue: [],
			queuedBytes: 0,
			done: false,
			error: null,
			notify: null,
		};
		this.pending.set(id, state);

		const onAbort = () => {
			state.done = true;
			this.pending.delete(id);
			try {
				sock.ws.send(JSON.stringify({ type: "rpc_cancel", id } satisfies TunnelDownMessage));
			} catch {
				// ignore
			}
			const notify = state.notify;
			state.notify = null;
			notify?.();
		};
		signal?.addEventListener("abort", onAbort, { once: true });

		try {
			sock.ws.send(JSON.stringify(frame));
		} catch (err) {
			this.pending.delete(id);
			signal?.removeEventListener("abort", onAbort);
			throw hotbedError(
				ErrorCode.WorkerUnreachable,
				`${host}: send over tunnel failed: ${err instanceof Error ? err.message : String(err)}`,
				{ retryable: true },
			);
		}

		try {
			while (true) {
				while (state.queue.length > 0) {
					const bytes = state.queue.shift()!;
					state.queuedBytes = Math.max(0, state.queuedBytes - bytes.byteLength);
					yield fromBinary(resSchema, bytes);
				}
				if (state.error) throw state.error;
				if (state.done) return;
				await new Promise<void>((resolve) => {
					state.notify = resolve;
				});
			}
		} finally {
			signal?.removeEventListener("abort", onAbort);
			if (this.pending.has(id)) {
				this.pending.delete(id);
				try {
					sock.ws.send(JSON.stringify({ type: "rpc_cancel", id } satisfies TunnelDownMessage));
				} catch {
					// ignore
				}
			}
		}
	}
}
