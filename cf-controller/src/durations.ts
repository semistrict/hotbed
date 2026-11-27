import { ErrorCode, hotbedError } from "./errors.ts";

const UNIT_MS: Record<string, number> = {
	ms: 1,
	s: 1000,
	m: 60_000,
	h: 3_600_000,
	d: 86_400_000,
};

/**
 * A duration such as 500ms, 90s, 15m, 24h or 7d in milliseconds, 0 for an
 * empty one. `what` and `examples` word the error for the flag it came from.
 */
export function parseDurationMs(
	raw: string,
	what = "duration",
	examples = "15m, 1h, 24h, 7d",
): number {
	const s = raw.trim();
	if (!s) return 0;
	const m = /^(\d+)(ms|s|m|h|d)$/.exec(s);
	if (!m) {
		throw hotbedError(ErrorCode.InvalidArgs, `invalid ${what} "${raw}"`).withSuggestion(
			`use e.g. ${examples}`,
		);
	}
	return Number(m[1]) * UNIT_MS[m[2]];
}
