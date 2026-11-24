import { ErrorCode, hotbedError } from "./errors.ts";

export interface ParsedCron {
	minutes: Set<number>;
	hours: Set<number>;
	dom: Set<number>;
	domWild: boolean;
	months: Set<number>;
	dow: Set<number>;
	dowWild: boolean;
	timeZone: string;
	next(afterMs: number): number | null;
}

const MONTH_NAMES: Record<string, number> = {
	jan: 1,
	feb: 2,
	mar: 3,
	apr: 4,
	may: 5,
	jun: 6,
	jul: 7,
	aug: 8,
	sep: 9,
	oct: 10,
	nov: 11,
	dec: 12,
};

const DOW_NAMES: Record<string, number> = {
	sun: 0,
	mon: 1,
	tue: 2,
	wed: 3,
	thu: 4,
	fri: 5,
	sat: 6,
};

function parseField(
	raw: string,
	min: number,
	max: number,
	names?: Record<string, number>,
	allowSevenAsZero = false,
): { values: Set<number>; wild: boolean } {
	const wild = raw === "*" || raw === "?";
	const values = new Set<number>();
	const parts = raw.split(",");
	for (const part of parts) {
		if (!part) throw new Error(`empty token in "${raw}"`);
		const slashIdx = part.indexOf("/");
		let rangeStr = part;
		let step = 1;
		if (slashIdx >= 0) {
			rangeStr = part.slice(0, slashIdx);
			const stepStr = part.slice(slashIdx + 1);
			if (!/^\d+$/.test(stepStr)) throw new Error(`invalid step "${stepStr}"`);
			step = Number(stepStr);
			if (step <= 0) throw new Error(`step must be positive in "${part}"`);
		}
		let start: number;
		let end: number;
		if (rangeStr === "*" || rangeStr === "?") {
			start = min;
			end = max;
		} else if (rangeStr.includes("-")) {
			const [sRaw, eRaw, extra] = rangeStr.split("-");
			if (extra !== undefined || !sRaw || !eRaw) throw new Error(`invalid range "${rangeStr}"`);
			start = parseVal(sRaw, min, max, names, allowSevenAsZero);
			end = parseVal(eRaw, min, max, names, allowSevenAsZero);
			if (allowSevenAsZero && eRaw === "7") end = 7;
			if (start > end) throw new Error(`range ${start}-${end} runs backwards`);
		} else {
			start = parseVal(rangeStr, min, max, names, allowSevenAsZero);
			end = slashIdx >= 0 ? max : start;
		}
		for (let v = start; v <= end; v += step) {
			values.add(allowSevenAsZero && v === 7 ? 0 : v);
		}
	}
	if (values.size === 0) throw new Error(`empty field "${raw}"`);
	return { values, wild };
}

function parseVal(
	s: string,
	min: number,
	max: number,
	names?: Record<string, number>,
	allowSevenAsZero = false,
): number {
	const low = s.toLowerCase();
	if (names && low in names) return names[low];
	if (!/^\d+$/.test(s)) throw new Error(`invalid value "${s}"`);
	const n = Number(s);
	const effectiveMax = allowSevenAsZero ? 7 : max;
	if (n < min || n > effectiveMax) {
		throw new Error(`value ${n} out of range ${min}..${effectiveMax}`);
	}
	return allowSevenAsZero && n === 7 ? 0 : n;
}

interface ZonedParts {
	year: number;
	month: number;
	day: number;
	hour: number;
	minute: number;
	dow: number;
}

function getZonedParts(ms: number, fmt: Intl.DateTimeFormat): ZonedParts {
	const parts = fmt.formatToParts(new Date(ms));
	let year = 0;
	let month = 0;
	let day = 0;
	let hour = 0;
	let minute = 0;
	let dow = 0;
	for (const p of parts) {
		switch (p.type) {
			case "year":
				year = Number(p.value);
				break;
			case "month":
				month = Number(p.value);
				break;
			case "day":
				day = Number(p.value);
				break;
			case "hour":
				hour = Number(p.value) % 24;
				break;
			case "minute":
				minute = Number(p.value);
				break;
			case "weekday":
				dow = DOW_NAMES[p.value.toLowerCase().slice(0, 3)] ?? 0;
				break;
		}
	}
	return { year, month, day, hour, minute, dow };
}

export function formatInTimeZone(ms: number, timeZone: string, style: "hm" | "session"): string {
	const fmt = new Intl.DateTimeFormat("en-US", {
		timeZone,
		year: "numeric",
		month: "2-digit",
		day: "2-digit",
		hour: "2-digit",
		minute: "2-digit",
		hour12: false,
	});
	const p = getZonedParts(ms, fmt);
	const hh = String(p.hour).padStart(2, "0");
	const mm = String(p.minute).padStart(2, "0");
	if (style === "hm") return `${hh}:${mm}`;
	const yyyy = String(p.year).padStart(4, "0");
	const mo = String(p.month).padStart(2, "0");
	const dd = String(p.day).padStart(2, "0");
	return `${yyyy}${mo}${dd}-${hh}${mm}`;
}

/**
 * Parses a 5-field crontab expression with optional leading CRON_TZ=ZONE or TZ=ZONE.
 */
export function parseCron(spec: string, nowMs = Date.now()): ParsedCron {
	let rest = spec.trim();
	let timeZone = "UTC";
	if (rest.startsWith("CRON_TZ=") || rest.startsWith("TZ=")) {
		const sp = rest.search(/\s/);
		if (sp < 0) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`cron schedule "${spec}": missing fields`,
			).withSuggestion(
				`five fields, minute hour day-of-month month day-of-week, such as "0 2 * * *" for 02:00 daily`,
			);
		}
		const eq = rest.indexOf("=");
		timeZone = rest.slice(eq + 1, sp);
		rest = rest.slice(sp).trim();
	}
	let fmt: Intl.DateTimeFormat;
	try {
		fmt = new Intl.DateTimeFormat("en-US", {
			timeZone,
			year: "numeric",
			month: "2-digit",
			day: "2-digit",
			hour: "2-digit",
			minute: "2-digit",
			weekday: "short",
			hour12: false,
		});
	} catch (err) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`cron schedule "${spec}": invalid timezone "${timeZone}": ${err instanceof Error ? err.message : String(err)}`,
		).withSuggestion(
			`five fields, minute hour day-of-month month day-of-week, such as "0 2 * * *" for 02:00 daily`,
		);
	}

	const fields = rest.split(/\s+/);
	if (fields.length !== 5) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`cron schedule "${spec}": expected 5 fields, got ${fields.length}`,
		).withSuggestion(
			`five fields, minute hour day-of-month month day-of-week, such as "0 2 * * *" for 02:00 daily`,
		);
	}

	let minutes: Set<number>;
	let hours: Set<number>;
	let domRes: { values: Set<number>; wild: boolean };
	let months: Set<number>;
	let dowRes: { values: Set<number>; wild: boolean };
	try {
		minutes = parseField(fields[0], 0, 59).values;
		hours = parseField(fields[1], 0, 23).values;
		domRes = parseField(fields[2], 1, 31);
		months = parseField(fields[3], 1, 12, MONTH_NAMES).values;
		dowRes = parseField(fields[4], 0, 6, DOW_NAMES, true);
	} catch (err) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`cron schedule "${spec}": ${err instanceof Error ? err.message : String(err)}`,
		).withSuggestion(
			`five fields, minute hour day-of-month month day-of-week, such as "0 2 * * *" for 02:00 daily`,
		);
	}

	const matchesDay = (p: ZonedParts): boolean => {
		const domMatch = domRes.values.has(p.day);
		const dowMatch = dowRes.values.has(p.dow);
		if (domRes.wild && dowRes.wild) return true;
		if (domRes.wild) return dowMatch;
		if (dowRes.wild) return domMatch;
		return domMatch || dowMatch;
	};

	const next = (afterMs: number): number | null => {
		// Start at the next whole minute strictly after afterMs.
		let t = Math.floor(afterMs / 60_000) * 60_000 + 60_000;
		const limit = t + 5 * 366 * 24 * 60 * 60_000; // 5 years, matching robfig/cron
		while (t < limit) {
			const p = getZonedParts(t, fmt);
			if (!months.has(p.month)) {
				// Advance by 1 day (at most 31 iterations per non-matching month)
				t += 24 * 60 * 60_000;
				// Reset roughly to start of day if we jumped past 00:00
				const p2 = getZonedParts(t, fmt);
				if (p2.hour > 0 || p2.minute > 0) {
					t -= (p2.hour * 60 + p2.minute) * 60_000;
					if (t <= afterMs) t = Math.floor(afterMs / 60_000) * 60_000 + 60_000;
				}
				continue;
			}
			if (!matchesDay(p)) {
				const remMin = (24 - p.hour) * 60 - p.minute;
				t += Math.max(1, remMin) * 60_000;
				continue;
			}
			if (!hours.has(p.hour)) {
				const remMin = 60 - p.minute;
				t += Math.max(1, remMin) * 60_000;
				continue;
			}
			if (!minutes.has(p.minute)) {
				t += 60_000;
				continue;
			}
			return t;
		}
		return null;
	};

	const parsed: ParsedCron = {
		minutes,
		hours,
		dom: domRes.values,
		domWild: domRes.wild,
		months,
		dow: dowRes.values,
		dowWild: dowRes.wild,
		timeZone,
		next,
	};

	if (parsed.next(nowMs) === null) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`cron schedule "${spec}": no such time comes in the next five years`,
		).withSuggestion(
			`five fields, minute hour day-of-month month day-of-week, such as "0 2 * * *" for 02:00 daily`,
		);
	}
	return parsed;
}
