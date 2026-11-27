import { CommandState } from "./gen/hotbed/v1/hotbed_pb.ts";

export const ONLINE_WINDOW_MS = 15_000;

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
