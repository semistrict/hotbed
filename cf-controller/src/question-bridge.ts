import { clone, toJson, type JsonValue } from "@bufbuild/protobuf";
import { QuestionSchema, QuestionState, type Question } from "./gen/hotbed/v1/hotbed_pb.ts";

export const QUESTION_BRIDGE_TAG = "question-bridge";

/**
 * Attachment persisted across Durable Object WebSocket hibernation for
 * outbound question-bridge clients (`/ws/questions`).
 */
export interface QuestionBridgeAttachment {
	kind: "question-bridge";
	hostname: string;
	login: string;
	connectedAt: number;
}

export type QuestionBridgeEvent =
	| "answered"
	| "dismissed"
	| "handled"
	| "reopened"
	| "revised"
	| "deleted";

export interface InterventionBridgeNotice {
	commandId: string;
	seq: number;
	kind: "stop" | "pause";
	sessionId: string;
	t3codeThreadId: string;
	argv: string;
	by: string;
	comment: string;
	diagnosed: boolean;
	at: number;
}

export type QuestionBridgeDownMessage =
	| {
			type: "snapshot";
			questions: JsonValue[];
	  }
	| {
			type: "update";
			event: QuestionBridgeEvent;
			questions: JsonValue[];
	  }
	| {
			type: "intervention";
			intervention: InterventionBridgeNotice;
	  };

interface QuestionBridgeUpMessage {
	type?: string;
}

function isOutboxState(state: QuestionState): boolean {
	return state === QuestionState.ANSWERED || state === QuestionState.DISMISSED;
}

function serializeQuestion(q: Question): JsonValue {
	const copy = clone(QuestionSchema, q);
	copy.history = [];
	copy.reminder = undefined;
	return toJson(QuestionSchema, copy);
}

/**
 * Manages persistent outbound bridge WebSocket connections (`/ws/questions`)
 * inside ControllerDO using Cloudflare's WebSocket Hibernation API so idle
 * sockets survive DO hibernation without billing or waking until a question
 * actually mutates.
 */
export class QuestionBridgeHub {
	private readonly ctx: DurableObjectState;

	constructor(ctx: DurableObjectState) {
		this.ctx = ctx;
	}

	/** Accepts an upgraded WebSocket and immediately pushes the current T3 outbox snapshot. */
	accept(ws: WebSocket, attachment: QuestionBridgeAttachment, initialQuestions: Question[]): void {
		this.ctx.acceptWebSocket(ws, [QUESTION_BRIDGE_TAG]);
		ws.serializeAttachment(attachment);
		this.sendSnapshot(ws, initialQuestions);
	}

	/** Reports whether ws belongs to the question bridge rather than the worker tunnel. */
	isBridgeSocket(ws: WebSocket): boolean {
		try {
			if (this.ctx.getTags(ws).includes(QUESTION_BRIDGE_TAG)) {
				return true;
			}
		} catch {
			// Fall back to attachment inspection if tags are unavailable.
		}
		const meta = ws.deserializeAttachment() as { kind?: string } | null;
		return meta?.kind === "question-bridge";
	}

	connectedCount(): number {
		let count = 0;
		for (const ws of this.ctx.getWebSockets(QUESTION_BRIDGE_TAG)) {
			if (ws.readyState === WebSocket.OPEN) {
				count += 1;
			}
		}
		return count;
	}

	sendSnapshot(ws: WebSocket, questions: Question[]): void {
		if (ws.readyState !== WebSocket.OPEN) return;
		const filtered = questions.filter((q) => Boolean(q.t3codeThreadId) && isOutboxState(q.state));
		const frame: QuestionBridgeDownMessage = {
			type: "snapshot",
			questions: filtered.map(serializeQuestion),
		};
		try {
			ws.send(JSON.stringify(frame));
		} catch {
			// Ignore broken socket; close handler will clean up.
		}
	}

	/** Pushes question state mutations for T3-associated questions to all connected bridges. */
	broadcastUpdate(event: QuestionBridgeEvent, questions: Question[]): void {
		const relevant = questions.filter((q) => Boolean(q.t3codeThreadId));
		if (relevant.length === 0) return;
		const sockets = this.ctx.getWebSockets(QUESTION_BRIDGE_TAG);
		if (sockets.length === 0) return;
		const payload = JSON.stringify({
			type: "update",
			event,
			questions: relevant.map(serializeQuestion),
		} satisfies QuestionBridgeDownMessage);
		for (const ws of sockets) {
			if (ws.readyState !== WebSocket.OPEN) continue;
			try {
				ws.send(payload);
			} catch {
				// Ignore broken socket so remaining bridges still receive the update.
			}
		}
	}

	/** Pushes an external command intervention (stop/pause by a user or another session) to all connected bridges. */
	broadcastIntervention(intervention: InterventionBridgeNotice): void {
		if (!intervention.t3codeThreadId) return;
		const sockets = this.ctx.getWebSockets(QUESTION_BRIDGE_TAG);
		if (sockets.length === 0) return;
		const payload = JSON.stringify({
			type: "intervention",
			intervention,
		} satisfies QuestionBridgeDownMessage);
		for (const ws of sockets) {
			if (ws.readyState !== WebSocket.OPEN) continue;
			try {
				ws.send(payload);
			} catch {
				// Ignore broken socket so remaining bridges still receive the update.
			}
		}
	}

	handleMessage(ws: WebSocket, message: string | ArrayBuffer, snapshotFn: () => Question[]): void {
		const text =
			typeof message === "string" ? message : new TextDecoder().decode(new Uint8Array(message));
		if (text === "ping") {
			try {
				ws.send("pong");
			} catch {
				// ignore
			}
			return;
		}
		let msg: QuestionBridgeUpMessage;
		try {
			msg = JSON.parse(text) as QuestionBridgeUpMessage;
		} catch {
			return;
		}
		if (msg.type === "resync") {
			this.sendSnapshot(ws, snapshotFn());
		}
	}

	handleClose(_ws: WebSocket, _code: number, _reason: string): void {
		// Hibernating sockets are tracked by ctx.getWebSockets(QUESTION_BRIDGE_TAG).
	}
}
