/**
 * Resolves once p settles, timeoutMs passes, or signal aborts, whichever is
 * first; it never rejects, so a caller re-checks the state it waits on.
 */
export function waitAtMost(
	p: Promise<void>,
	timeoutMs: number,
	signal?: AbortSignal,
): Promise<void> {
	return new Promise<void>((resolve) => {
		if (signal?.aborted) {
			resolve();
			return;
		}
		const done = () => {
			clearTimeout(timer);
			signal?.removeEventListener("abort", done);
			resolve();
		};
		const timer = setTimeout(done, timeoutMs);
		signal?.addEventListener("abort", done, { once: true });
		void p.then(done, done);
	});
}
