import { fromBinary, toBinary, type DescMessage, type MessageShape } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { ErrorCode, fromUnknownError, HotbedError, hotbedError } from "./errors.ts";
import { randomHex } from "./ids.ts";

/**
 * Why a tunnel call its caller's signal aborted ends. Its caller's own
 * deadline passing (Connect-Timeout-Ms, which aborts the handler's signal
 * with DeadlineExceeded while the caller may still be listening) is the
 * worker not answering in time, as retryable as a call's own timeout;
 * anything else is the caller gone, which nobody hears.
 */
function abortError(host: string, procedure: string, signal: AbortSignal | undefined): HotbedError {
	const reason: unknown = signal?.reason;
	if (reason instanceof ConnectError && reason.code === Code.DeadlineExceeded) {
		return hotbedError(
			ErrorCode.WorkerUnreachable,
			`${host}: ${procedure} timed out: its caller's deadline passed first`,
			{ retryable: true },
		);
	}
	return hotbedError(ErrorCode.Internal, `${host}: ${procedure} canceled`);
}

/**
 * Attachment persisted across Durable Object WebSocket hibernation.
 */
export interface WorkerSocketAttachment {
	nodeId: string;
	hostname: string;
	connectedAt: number;
}

/**
 * Wire protocol over the worker <-> controller WebSocket tunnel (/ws/worker).
 *
 * Controller -> Worker:
 *   { type: "rpc_req", id, procedure, body (base64 proto), stream: boolean, clientStream?: boolean }
 *   { type: "rpc_send", id, body (base64 proto) }   one request message of a clientStream call
 *   { type: "rpc_close_send", id }                   its request messages end
 *   { type: "rpc_cancel", id }
 *   { type: "pipe_open", id, kind, workspace, port }   a client's byte stream: "ssh" (the worker's shell server) or "port"
 *   { type: "pipe_data", id, body (base64 bytes) }
 *   { type: "pipe_close", id }
 *
 * Worker -> Controller:
 *   { type: "rpc_msg", id, body (base64 proto), headers? }   headers: response headers to pass on, on a call's first message
 *   { type: "rpc_end", id }
 *   { type: "rpc_err", id, code, message, suggestion?, retryable? }
 *   { type: "pipe_opened", id }                        the worker's end of a pipe is open
 *   { type: "pipe_data", id, body (base64 bytes) }
 *   { type: "pipe_close", id, message? }                message: why it failed, if it did
 */
export type TunnelDownMessage =
	| {
			type: "rpc_req";
			id: string;
			procedure: string;
			body: string;
			stream: boolean;
			clientStream?: boolean;
	  }
	| {
			type: "rpc_send";
			id: string;
			body: string;
	  }
	| {
			type: "rpc_close_send";
			id: string;
	  }
	| {
			type: "rpc_cancel";
			id: string;
	  }
	| {
			type: "pipe_open";
			id: string;
			kind: string;
			workspace: string;
			port: number;
	  }
	| {
			type: "pipe_data";
			id: string;
			body: string;
	  }
	| {
			type: "pipe_close";
			id: string;
	  };

export type TunnelUpMessage =
	| {
			type: "rpc_msg";
			id: string;
			body: string;
			headers?: Record<string, string>;
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
	  }
	| {
			type: "pipe_opened";
			id: string;
	  }
	| {
			type: "pipe_data";
			id: string;
			body: string;
	  }
	| {
			type: "pipe_close";
			id: string;
			message?: string;
	  };

/** A client's end of a pipe to a worker, as the controller relays it. */
export interface PipeHandlers {
	onOpened: () => void;
	onData: (bytes: Uint8Array) => void;
	onClose: (message: string) => void;
}

/** What ControllerDO decides for servePipe. */
export interface PipeRequestHooks {
	/** Authenticates the request; a Response refuses the pipe with it. */
	authorize(request: Request): Promise<Response | null>;
	/** The node holding workspace and its hostname; throws for one unknown or released. */
	locate(workspace: string): { nodeId: string; host: string };
}

/** The controller's end of a pipe to a worker. */
export interface PipeEnd {
	send: (bytes: Uint8Array) => void;
	close: () => void;
}

interface PendingUnary {
	kind: "unary";
	nodeId: string;
	ws: WebSocket;
	resolve: (bytes: Uint8Array) => void;
	reject: (err: Error) => void;
	timer: ReturnType<typeof setTimeout>;
	cleanup?: () => void;
}

interface PendingStream {
	kind: "stream";
	nodeId: string;
	ws: WebSocket;
	queue: Uint8Array[];
	queuedBytes: number;
	done: boolean;
	error: Error | null;
	notify: (() => void) | null;
	onHeaders?: (headers: Record<string, string>) => void;
}

const MAX_STREAM_QUEUE_BYTES = 16 * 1024 * 1024;
const DEFAULT_RECONNECT_WAIT_MS = 5_000;

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
	private readonly pipes = new Map<string, PipeHandlers & { nodeId: string; ws: WebSocket }>();
	private readonly connectWaiters = new Map<string, Set<() => void>>();
	private readonly reconnectWaitMs: number;

	constructor(ctx: DurableObjectState, reconnectWaitMs = DEFAULT_RECONNECT_WAIT_MS) {
		this.ctx = ctx;
		this.reconnectWaitMs = reconnectWaitMs;
		// Auto-reply to worker keepalive pings without waking the DO from hibernation.
		this.ctx.setWebSocketAutoResponse(new WebSocketRequestResponsePair("ping", "pong"));
	}

	/** Accepts an upgraded WebSocket from a worker, replacing any stale socket for the same node. */
	accept(server: WebSocket, attachment: WorkerSocketAttachment): void {
		// Close any superseded sockets for this nodeId so at most one active tunnel exists per node.
		for (const existing of this.ctx.getWebSockets(attachment.nodeId)) {
			if (existing === server) continue;
			try {
				existing.close(1012, "superseded by newer connection");
			} catch {
				// ignore already-closed socket
			}
		}
		// Always fail pending calls/pipes from any previous socket for this nodeId,
		// even if the closed socket already left getWebSockets() before webSocketClose fired.
		this.failPendingForNodeExcept(
			attachment.nodeId,
			server,
			hotbedError(ErrorCode.WorkerUnreachable, `${attachment.hostname} reconnected`, {
				retryable: true,
			}),
		);
		this.ctx.acceptWebSocket(server, [attachment.nodeId, `host:${attachment.hostname}`]);
		server.serializeAttachment(attachment);
		this.notifyConnected(attachment.nodeId);
	}

	private notifyConnected(nodeId: string): void {
		const waiters = this.connectWaiters.get(nodeId);
		if (!waiters || waiters.size === 0) return;
		this.connectWaiters.delete(nodeId);
		for (const wake of waiters) {
			wake();
		}
	}

	/**
	 * Returns the active WebSocket for nodeId, waiting up to waitMs for a
	 * briefly disconnected worker to reconnect its /ws/worker tunnel.
	 */
	private async waitForSocket(
		nodeId: string,
		waitMs: number,
		signal?: AbortSignal,
	): Promise<{ ws: WebSocket; meta: WorkerSocketAttachment } | null> {
		const immediate = this.socketFor(nodeId);
		if (immediate) return immediate;
		if (waitMs <= 0 || signal?.aborted) return null;

		await new Promise<void>((resolve) => {
			let settled = false;
			const finish = () => {
				if (settled) return;
				settled = true;
				clearTimeout(timer);
				signal?.removeEventListener("abort", finish);
				const set = this.connectWaiters.get(nodeId);
				if (set) {
					set.delete(finish);
					if (set.size === 0) this.connectWaiters.delete(nodeId);
				}
				resolve();
			};
			let set = this.connectWaiters.get(nodeId);
			if (!set) {
				set = new Set();
				this.connectWaiters.set(nodeId, set);
			}
			set.add(finish);
			const timer = setTimeout(finish, waitMs);
			signal?.addEventListener("abort", finish, { once: true });
			// Re-check in case accept() landed before the waiter was registered.
			if (this.socketFor(nodeId)) {
				finish();
			}
		});
		if (signal?.aborted) return null;
		return this.socketFor(nodeId);
	}

	private openSocketsFor(nodeId: string): Array<{ ws: WebSocket; meta: WorkerSocketAttachment }> {
		const out: Array<{ ws: WebSocket; meta: WorkerSocketAttachment }> = [];
		for (const ws of this.ctx.getWebSockets(nodeId)) {
			if (ws.readyState !== WebSocket.OPEN) continue;
			const meta = (ws.deserializeAttachment() as WorkerSocketAttachment | null) ?? {
				nodeId,
				hostname: nodeId,
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
			const meta = ws.deserializeAttachment() as
				| (Partial<WorkerSocketAttachment> & { kind?: string })
				| null;
			if (meta?.kind === "question-bridge") continue;
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
		if (msg.type === "pipe_opened" || msg.type === "pipe_data" || msg.type === "pipe_close") {
			const pipe = this.pipes.get(msg.id);
			if (!pipe || pipe.ws !== ws) return;
			if (msg.type === "pipe_opened") pipe.onOpened();
			else if (msg.type === "pipe_data") pipe.onData(decodeBase64(msg.body));
			else {
				this.pipes.delete(msg.id);
				pipe.onClose(msg.message ?? "");
			}
			return;
		}
		const call = this.pending.get(msg.id);
		if (!call || call.ws !== ws) return;

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
				if (msg.headers && call.onHeaders) {
					call.onHeaders(msg.headers);
					call.onHeaders = undefined;
				}
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
		const host = meta?.hostname || meta?.nodeId || "worker";
		// Every pending call and pipe dispatched on ws can only ever be answered
		// on ws; fail them unconditionally even if a newer socket has already connected.
		this.failPendingForSocket(
			ws,
			hotbedError(
				ErrorCode.WorkerUnreachable,
				`${host} disconnected${reason ? `: ${reason}` : ""}`,
				{ retryable: true },
			),
		);
	}

	private failPendingForSocket(ws: WebSocket, err: HotbedError): void {
		this.failPendingWhere((entry) => entry.ws === ws, err);
	}

	private failPendingForNodeExcept(nodeId: string, keep: WebSocket, err: HotbedError): void {
		this.failPendingWhere((entry) => entry.nodeId === nodeId && entry.ws !== keep, err);
	}

	private failPendingWhere(
		match: (entry: { nodeId: string; ws: WebSocket }) => boolean,
		err: HotbedError,
	): void {
		for (const [id, pipe] of this.pipes) {
			if (!match(pipe)) continue;
			this.pipes.delete(id);
			pipe.onClose(err.message);
		}
		for (const [id, call] of this.pending) {
			if (!match(call)) continue;
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
			throw abortError(host, procedure, signal);
		}
		const waitMs = Math.min(this.reconnectWaitMs, timeoutMs);
		let sock = await this.waitForSocket(nodeId, waitMs, signal);
		if (signal?.aborted) {
			throw abortError(host, procedure, signal);
		}
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
		const frameJSON = JSON.stringify(frame);

		// Register pending before send so an immediate reply cannot beat registration.
		// If send() throws synchronously on a dying socket, wait once for a replacement
		// socket and retry sending the exact same frame (it never left the DO).
		const sendWithReconnect = async (): Promise<Uint8Array> => {
			for (let attempt = 0; attempt < 2; attempt++) {
				const curSock = sock!;
				let sendErrMsg = "";
				const p = new Promise<Uint8Array>((resolve, reject) => {
					const sendCancel = () => {
						try {
							curSock.ws.send(
								JSON.stringify({ type: "rpc_cancel", id } satisfies TunnelDownMessage),
							);
						} catch {
							// ignore
						}
					};
					const onAbort = () => {
						clearTimeout(timer);
						this.pending.delete(id);
						sendCancel();
						reject(abortError(host, procedure, signal));
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
					this.pending.set(id, {
						kind: "unary",
						nodeId,
						ws: curSock.ws,
						resolve,
						reject,
						timer,
						cleanup,
					});
					try {
						curSock.ws.send(frameJSON);
					} catch (err) {
						clearTimeout(timer);
						cleanup();
						this.pending.delete(id);
						sendErrMsg = err instanceof Error ? err.message : String(err);
						resolve(new Uint8Array(0));
					}
				});
				if (!sendErrMsg) {
					return await p;
				}
				if (attempt === 0) {
					const next = await this.waitForSocket(nodeId, waitMs, signal);
					if (signal?.aborted) {
						throw abortError(host, procedure, signal);
					}
					if (next && next.ws !== curSock.ws) {
						sock = next;
						continue;
					}
				}
				throw hotbedError(
					ErrorCode.WorkerUnreachable,
					`${host}: send over tunnel failed: ${sendErrMsg}`,
					{ retryable: true },
				);
			}
			throw hotbedError(ErrorCode.WorkerUnreachable, `${host}: send over tunnel failed`, {
				retryable: true,
			});
		};

		const bytes = await sendWithReconnect();
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
		onHeaders?: (headers: Record<string, string>) => void,
	): AsyncGenerator<MessageShape<Res>> {
		if (signal?.aborted) {
			return;
		}
		let sock = await this.waitForSocket(nodeId, this.reconnectWaitMs, signal);
		if (signal?.aborted) {
			return;
		}
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
		const frameJSON = JSON.stringify(frame);
		const state: PendingStream = {
			kind: "stream",
			nodeId,
			ws: sock.ws,
			queue: [],
			queuedBytes: 0,
			done: false,
			error: null,
			notify: null,
			onHeaders,
		};
		this.pending.set(id, state);

		const onAbort = () => {
			state.done = true;
			this.pending.delete(id);
			try {
				state.ws.send(JSON.stringify({ type: "rpc_cancel", id } satisfies TunnelDownMessage));
			} catch {
				// ignore
			}
			const notify = state.notify;
			state.notify = null;
			notify?.();
		};
		signal?.addEventListener("abort", onAbort, { once: true });

		let sent = false;
		let lastSendErr: unknown = null;
		for (let attempt = 0; attempt < 2; attempt++) {
			try {
				state.ws = sock.ws;
				sock.ws.send(frameJSON);
				sent = true;
				break;
			} catch (err) {
				lastSendErr = err;
				if (attempt === 0) {
					const prevWs = sock.ws;
					const next = await this.waitForSocket(nodeId, this.reconnectWaitMs, signal);
					if (signal?.aborted) {
						this.pending.delete(id);
						signal.removeEventListener("abort", onAbort);
						return;
					}
					if (next && next.ws !== prevWs) {
						sock = next;
						continue;
					}
				}
			}
		}
		if (!sent) {
			this.pending.delete(id);
			signal?.removeEventListener("abort", onAbort);
			throw hotbedError(
				ErrorCode.WorkerUnreachable,
				`${host}: send over tunnel failed: ${lastSendErr instanceof Error ? lastSendErr.message : String(lastSendErr)}`,
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

	/**
	 * Invokes a client-streaming WorkspaceService RPC (Sync) on a connected
	 * worker over its WebSocket tunnel: each of reqs in an rpc_send frame, in
	 * order, then rpc_close_send; the worker answers with one message.
	 */
	async callClientStream<Req extends DescMessage, Res extends DescMessage>(
		nodeId: string,
		host: string,
		procedure: string,
		reqSchema: Req,
		reqs: AsyncIterable<MessageShape<Req>>,
		resSchema: Res,
		timeoutMs: number,
		signal?: AbortSignal,
	): Promise<MessageShape<Res>> {
		const sock = await this.waitForSocket(nodeId, this.reconnectWaitMs, signal);
		if (signal?.aborted) {
			throw abortError(host, procedure, signal);
		}
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
		const cancel = () => {
			try {
				sock.ws.send(JSON.stringify({ type: "rpc_cancel", id } satisfies TunnelDownMessage));
			} catch {
				// ignore
			}
		};
		let settled = false;
		const result = new Promise<Uint8Array>((resolve, reject) => {
			const onAbort = () => {
				clearTimeout(timer);
				this.pending.delete(id);
				cancel();
				reject(abortError(host, procedure, signal));
			};
			const cleanup = () => {
				settled = true;
				signal?.removeEventListener("abort", onAbort);
			};
			const timer = setTimeout(() => {
				cleanup();
				this.pending.delete(id);
				cancel();
				reject(
					hotbedError(ErrorCode.WorkerUnreachable, `${host}: ${procedure} timed out`, {
						retryable: true,
					}),
				);
			}, timeoutMs);
			signal?.addEventListener("abort", onAbort, { once: true });
			this.pending.set(id, {
				kind: "unary",
				nodeId,
				ws: sock.ws,
				resolve,
				reject,
				timer,
				cleanup,
			});
		});
		// The answer may come (an error) before every message is sent.
		result.catch(() => {});
		try {
			sock.ws.send(
				JSON.stringify({
					type: "rpc_req",
					id,
					procedure,
					body: "",
					stream: false,
					clientStream: true,
				} satisfies TunnelDownMessage),
			);
			for await (const m of reqs) {
				if (settled || !this.pending.has(id)) break;
				sock.ws.send(
					JSON.stringify({
						type: "rpc_send",
						id,
						body: encodeBase64(toBinary(reqSchema, m)),
					} satisfies TunnelDownMessage),
				);
			}
			if (this.pending.has(id)) {
				sock.ws.send(JSON.stringify({ type: "rpc_close_send", id } satisfies TunnelDownMessage));
			}
		} catch (err) {
			const call = this.pending.get(id);
			this.pending.delete(id);
			if (call?.kind === "unary") {
				clearTimeout(call.timer);
				call.cleanup?.();
			}
			cancel();
			if (err instanceof HotbedError) throw err;
			throw hotbedError(
				ErrorCode.WorkerUnreachable,
				`${host}: send over tunnel failed: ${err instanceof Error ? err.message : String(err)}`,
				{ retryable: true },
			);
		}
		return fromBinary(resSchema, await result);
	}

	/**
	 * Opens a pipe to a worker for a client's byte stream: kind "ssh" (its
	 * shell server) or "port" (a port a command of workspace serves on).
	 */
	async openPipe(
		nodeId: string,
		host: string,
		kind: string,
		workspace: string,
		port: number,
		handlers: PipeHandlers,
	): Promise<PipeEnd> {
		const sock = await this.waitForSocket(nodeId, this.reconnectWaitMs);
		if (!sock) {
			throw hotbedError(
				ErrorCode.WorkerUnreachable,
				`${host} has no active controller WebSocket tunnel`,
				{
					retryable: true,
				},
			);
		}
		const id = randomHex(8);
		this.pipes.set(id, { nodeId, ws: sock.ws, ...handlers });
		const sendFrame = (frame: TunnelDownMessage) => sock.ws.send(JSON.stringify(frame));
		try {
			sendFrame({ type: "pipe_open", id, kind, workspace, port });
		} catch (err) {
			this.pipes.delete(id);
			throw hotbedError(
				ErrorCode.WorkerUnreachable,
				`${host}: send over tunnel failed: ${err instanceof Error ? err.message : String(err)}`,
				{ retryable: true },
			);
		}
		return {
			send: (bytes) => {
				if (!this.pipes.has(id)) return;
				try {
					sendFrame({ type: "pipe_data", id, body: encodeBase64(bytes) });
				} catch {
					const pipe = this.pipes.get(id);
					this.pipes.delete(id);
					pipe?.onClose(`${host} went away`);
				}
			},
			close: () => {
				if (!this.pipes.delete(id)) return;
				try {
					sendFrame({ type: "pipe_close", id });
				} catch {
					// ignore
				}
			},
		};
	}

	/**
	 * Serves /ws/pipe?workspace=ID&kind=ssh|port[&port=N]: a client's byte
	 * stream to the workspace's worker, relayed over the worker's tunnel: its
	 * shell server (hotbed shell's ssh; OpenShell's ticket admits it), or a
	 * port a command of the workspace serves on. Binary messages carry the
	 * bytes; one text message, {"type":"opened"}, says the worker's end is
	 * open; the socket closes when either end does, with why if it failed.
	 */
	async servePipe(request: Request, url: URL, hooks: PipeRequestHooks): Promise<Response> {
		if ((request.headers.get("Upgrade") ?? "").toLowerCase() !== "websocket") {
			return new Response("Expected Upgrade: websocket", { status: 426 });
		}
		const refused = await hooks.authorize(request);
		if (refused) return refused;
		const workspace = url.searchParams.get("workspace") ?? "";
		const kind = url.searchParams.get("kind") ?? "";
		const port = Number(url.searchParams.get("port") ?? "0");
		if ((kind !== "ssh" && kind !== "port") || (kind === "port" && !(port > 0 && port < 65536))) {
			return Response.json(
				{
					code: ErrorCode.InvalidArgs,
					message: "kind is ssh, or port with a port from 1 to 65535",
				},
				{ status: 400 },
			);
		}
		let nodeId: string;
		let host: string;
		try {
			({ nodeId, host } = hooks.locate(workspace));
		} catch (err) {
			return Response.json(fromUnknownError(err).toJSON(), { status: 404 });
		}

		const pair = new WebSocketPair();
		const client = pair[0];
		const server = pair[1];
		server.accept();
		const closeClient = (message: string) => {
			try {
				server.close(message ? 1011 : 1000, message.slice(0, 120));
			} catch {
				// already closed
			}
		};
		let pipe: PipeEnd;
		try {
			pipe = await this.openPipe(nodeId, host, kind, workspace, port, {
				onOpened: () => server.send(JSON.stringify({ type: "opened" })),
				onData: (bytes) => server.send(bytes),
				onClose: closeClient,
			});
		} catch (err) {
			closeClient(fromUnknownError(err).message);
			return new Response(null, { status: 101, webSocket: client });
		}
		// Sent in order, a message's bytes once the one before's are. A frame
		// that fails to send ends the pipe: the client's byte stream has a hole
		// in it, and the chain must not stay rejected for every later frame.
		let sending: Promise<void> = Promise.resolve();
		server.addEventListener("message", (ev) => {
			const data: unknown = ev.data;
			sending = sending
				.then(async () => {
					if (typeof data === "string") pipe.send(new TextEncoder().encode(data));
					else if (data instanceof ArrayBuffer) pipe.send(new Uint8Array(data));
					else if (ArrayBuffer.isView(data))
						pipe.send(new Uint8Array(data.buffer, data.byteOffset, data.byteLength));
					else if (data instanceof Blob) pipe.send(new Uint8Array(await data.arrayBuffer()));
				})
				.catch((err: unknown) => {
					const message = fromUnknownError(err).message;
					console.error(`pipe to ${host} workspace ${workspace}: send failed: ${message}`);
					pipe.close();
					closeClient(`send failed: ${message}`);
				});
		});
		server.addEventListener("close", () => pipe.close());
		server.addEventListener("error", () => pipe.close());
		return new Response(null, { status: 101, webSocket: client });
	}
}
