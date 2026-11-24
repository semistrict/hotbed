import {
	BaseState,
	CommandState,
	WorkspaceState,
	type Base,
	type Command,
	type HeartbeatRequest,
	type Node,
	type Workspace,
} from "./gen/hotbed/v1/hotbed_pb.ts";
import type { CallerIdentity } from "./auth.ts";
import { ErrorCode, hotbedError } from "./errors.ts";

export const ONLINE_WINDOW_MS = 15_000;
export const STANDBY_GRACE_MS = 30_000;
export const DEFAULT_WAKE_WAIT_MS = 40_000;
export const BOOT_TIMEOUT_MS = 3 * 60_000;
export const WAKE_FAILED_HOLD_MS = 10 * 60_000;

/**
 * Simple in-DO pub/sub wakeup map keyed by session ID or node hostname.
 * Replaces Go's channel-based wakeups.go inside the single-threaded JS DO.
 */
export class Wakeups {
	private readonly waiters = new Map<string, Set<() => void>>();

	wait(key: string): { promise: Promise<void>; cancel: () => void } {
		let resolveFn!: () => void;
		const promise = new Promise<void>((resolve) => {
			resolveFn = resolve;
		});
		let set = this.waiters.get(key);
		if (!set) {
			set = new Set();
			this.waiters.set(key, set);
		}
		set.add(resolveFn);
		const cancel = () => {
			const s = this.waiters.get(key);
			if (!s) return;
			s.delete(resolveFn);
			if (s.size === 0) this.waiters.delete(key);
		};
		return { promise, cancel };
	}

	wake(key: string): void {
		const set = this.waiters.get(key);
		if (!set) return;
		this.waiters.delete(key);
		for (const fn of set) fn();
	}

	wakeAll(keys: Iterable<string>): void {
		for (const k of keys) this.wake(k);
	}
}

export function isCommandLive(s: CommandState): boolean {
	return (
		s === CommandState.QUEUED ||
		s === CommandState.RUNNING ||
		s === CommandState.PARKED ||
		s === CommandState.PAUSED
	);
}

interface PowerState {
	calls: number;
	endedAt: number;
	running: boolean;
	since: number;
	stopping: boolean;
	sleepAfterMs: number;
	want: boolean;
	worked: number;
	wokeAt: number;
	failedAt: number;
	supervisor: string;
	supervisorHost: string;
}

export interface NodeWork {
	working: boolean;
	busy: boolean;
}

export function workOf(view: {
	online: boolean;
	hb?: HeartbeatRequest;
	bases: Base[];
	workspaces: Workspace[];
	commands: Command[];
}): NodeWork {
	let working = false;
	let busy = false;
	for (const c of view.commands) {
		if (isCommandLive(c.state)) {
			busy = true;
			if (c.sessionId) working = true;
		}
	}
	for (const ws of view.workspaces) {
		if (ws.state === WorkspaceState.PREPARING) {
			busy = true;
			working = true;
		}
	}
	for (const b of view.bases) {
		if (b.state === BaseState.BUILDING) {
			busy = true;
		}
	}
	if (view.online && (view.hb?.activeSessions.length ?? 0) > 0) {
		busy = true;
		working = true;
	}
	return { working, busy };
}

function supervisorKey(caller: CallerIdentity): string {
	if (caller.nodeId) return `id:${caller.nodeId}`;
	if (caller.hostname) return `host:${caller.hostname}`;
	return "";
}

export class StandbyState {
	readonly changes = new Wakeups();
	private readonly nodes = new Map<string, PowerState>();
	wakeWaitMs = DEFAULT_WAKE_WAIT_MS;

	holdBy(
		host: string,
		supervisor: CallerIdentity,
		running: boolean,
		sleepAfterMs: number,
		nowMs: number,
	): () => void {
		let p = this.nodes.get(host);
		if (!p) {
			p = {
				calls: 0,
				endedAt: 0,
				running,
				since: nowMs - ONLINE_WINDOW_MS,
				stopping: false,
				sleepAfterMs,
				want: running,
				worked: nowMs,
				wokeAt: 0,
				failedAt: 0,
				supervisor: "",
				supervisorHost: "",
			};
			this.nodes.set(host, p);
		}
		if (running && !p.running && !p.want) {
			p.want = true;
			p.worked = nowMs;
		}
		if (!running) {
			p.stopping = false;
		}
		if (p.running !== running) {
			p.running = running;
			p.since = nowMs;
		}
		const key = supervisorKey(supervisor);
		if (key) {
			p.supervisor = key;
			p.supervisorHost = supervisor.hostname || supervisor.nodeId;
		}
		p.calls++;
		p.sleepAfterMs = sleepAfterMs;
		this.changes.wake(host);
		return () => {
			p!.calls = Math.max(0, p!.calls - 1);
			p!.endedAt = Date.now();
		};
	}

	authorizeHostReporter(host: string, caller: CallerIdentity, nowMs: number): void {
		const p = this.nodes.get(host);
		if (!p || !this.isSupervised(p, nowMs)) {
			throw hotbedError(ErrorCode.Conflict, `${host} is not supervised by Standby`).withSuggestion(
				"only a VM worker's active host supervisor (hotbed worker supervise) may report host usage",
			);
		}
		const key = supervisorKey(caller);
		if (!p.supervisor || !key || p.supervisor !== key) {
			const who = caller.hostname || caller.nodeId || "unknown peer";
			const sup = p.supervisorHost || p.supervisor || "none";
			throw hotbedError(
				ErrorCode.NotAuthorized,
				`${who} may not report host usage for ${host} (supervised by ${sup})`,
			);
		}
	}

	private isSupervised(p: PowerState, nowMs: number): boolean {
		return p.calls > 0 || (p.endedAt > 0 && nowMs - p.endedAt < STANDBY_GRACE_MS);
	}

	private isAwake(p: PowerState, lastSeen: bigint | undefined): boolean {
		return (
			p.want && p.running && !p.stopping && lastSeen !== undefined && Number(lastSeen) > p.since
		);
	}

	asleep(node: Node, nowMs: number): boolean {
		const p = this.nodes.get(node.hostname);
		return !!p && this.isSupervised(p, nowMs) && !this.isAwake(p, node.lastSeen);
	}

	wakeFailed(node: Node, nowMs: number): boolean {
		const p = this.nodes.get(node.hostname);
		return (
			!!p &&
			p.failedAt > 0 &&
			nowMs - p.failedAt < WAKE_FAILED_HOLD_MS &&
			!this.isAwake(p, node.lastSeen)
		);
	}

	wake(host: string, nowMs: number): void {
		const p = this.nodes.get(host);
		if (p) {
			p.want = true;
			p.worked = nowMs;
			if (p.wokeAt === 0) p.wokeAt = nowMs;
		}
		this.changes.wake(host);
	}

	settleAt(
		host: string,
		lastSeen: bigint | undefined,
		w: NodeWork,
		nowMs: number,
	): { run: boolean; reason: string } {
		const p = this.nodes.get(host);
		if (!p) return { run: false, reason: "unknown" };
		const booting = p.want && !this.isAwake(p, lastSeen);
		if (!booting) {
			p.wokeAt = 0;
		} else if (p.wokeAt === 0) {
			p.wokeAt = nowMs;
		}
		if ((w.working || booting) && nowMs > p.worked) {
			p.worked = nowMs;
		}
		if (booting && nowMs - p.wokeAt >= BOOT_TIMEOUT_MS) {
			p.failedAt = nowMs;
			p.wokeAt = 0;
			if (p.sleepAfterMs > 0) {
				p.want = false;
				p.stopping = p.running;
				return { run: false, reason: `it did not wake within ${BOOT_TIMEOUT_MS / 60_000}m` };
			}
		}
		if (p.sleepAfterMs <= 0) {
			p.want = true;
			return { run: true, reason: "it never sleeps" };
		}
		if (!p.want) {
			return { run: false, reason: "asleep until a run is placed on it" };
		}
		if (w.busy || booting) {
			return { run: true, reason: "busy" };
		}
		if (nowMs - p.worked >= p.sleepAfterMs) {
			p.want = false;
			p.stopping = p.running;
			return { run: false, reason: `no work for ${Math.round(p.sleepAfterMs / 1000)}s` };
		}
		return {
			run: true,
			reason: `last had work ${Math.round((nowMs - p.worked) / 1000)}s ago; sleeps after ${Math.round(p.sleepAfterMs / 1000)}s`,
		};
	}
}
