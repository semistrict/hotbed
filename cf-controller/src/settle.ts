export interface TimedOutputChunk {
	offset: number;
	nextOffset: number;
	receivedAt: number;
	data: Uint8Array;
}

export interface ExtractedLogLines {
	lines: string[];
	lineTimestamps: number[];
	cut: boolean;
}

const CR = 13;
const ESC = 27;
const BS = 8;

function hasControlBytes(chunks: TimedOutputChunk[]): boolean {
	for (const c of chunks) {
		const b = c.data;
		for (let i = 0; i < b.length; i++) {
			const v = b[i];
			if (v === ESC || v === CR || v === BS) return true;
		}
	}
	return false;
}

/**
 * Extracts lines and per-line receive timestamps from ordered output chunks.
 *
 * Fast path: chunks without ESC / CR / BS split directly on LF.
 * Settled path: collapses CR redraws, backspaces, and strips ANSI CSI/OSC
 * sequences so progress bars and colored terminal output read cleanly, matching
 * Go's internal/settle semantics.
 */
export function extractLogLines(
	chunks: TimedOutputChunk[],
	tail: number,
	maxBytes: number,
	alreadyCut = false,
): ExtractedLogLines {
	const rawLines: string[] = [];
	const rawTimestamps: number[] = [];

	if (chunks.length > 0) {
		const decoder = new TextDecoder("utf-8");
		if (!hasControlBytes(chunks)) {
			let cur = "";
			let lineTs = 0;
			for (let i = 0; i < chunks.length; i++) {
				const c = chunks[i];
				const text = decoder.decode(c.data, { stream: i < chunks.length - 1 });
				if (!text) continue;
				let start = 0;
				while (start < text.length) {
					const nl = text.indexOf("\n", start);
					if (nl < 0) {
						if (c.receivedAt > 0) lineTs = c.receivedAt;
						cur += text.slice(start);
						break;
					}
					if (c.receivedAt > 0) lineTs = c.receivedAt;
					cur += text.slice(start, nl);
					rawLines.push(cur);
					rawTimestamps.push(lineTs);
					cur = "";
					lineTs = 0;
					start = nl + 1;
				}
			}
			const tailText = decoder.decode();
			if (tailText) cur += tailText;
			if (cur.length > 0) {
				rawLines.push(cur);
				rawTimestamps.push(lineTs);
			}
		} else {
			settleTimedChunks(chunks, decoder, rawLines, rawTimestamps);
		}
	}

	let startIdx = 0;
	if (tail > 0 && rawLines.length > tail) {
		startIdx = rawLines.length - tail;
	}
	let cut = alreadyCut;
	if (maxBytes > 0) {
		const encoder = new TextEncoder();
		let used = 0;
		let keepFrom = rawLines.length;
		for (let i = rawLines.length - 1; i >= startIdx; i--) {
			const sz = encoder.encode(rawLines[i]).byteLength + 1;
			if (used + sz > maxBytes && keepFrom < rawLines.length) {
				cut = true;
				break;
			}
			used += sz;
			keepFrom = i;
		}
		if (keepFrom > startIdx) {
			startIdx = keepFrom;
			cut = true;
		}
	}

	return {
		lines: startIdx > 0 ? rawLines.slice(startIdx) : rawLines,
		lineTimestamps: startIdx > 0 ? rawTimestamps.slice(startIdx) : rawTimestamps,
		cut,
	};
}

const enum EscState {
	Normal = 0,
	Esc = 1,
	CSI = 2,
	OSC = 3,
	OSCEsc = 4,
}

function settleTimedChunks(
	chunks: TimedOutputChunk[],
	decoder: TextDecoder,
	outLines: string[],
	outTimestamps: number[],
): void {
	let buf: string[] = [];
	let col = 0;
	let lineTs = 0;
	let escState: EscState = EscState.Normal;
	let csiBuf = "";

	const emitLine = () => {
		outLines.push(buf.join(""));
		outTimestamps.push(lineTs);
		buf = [];
		col = 0;
		lineTs = 0;
	};

	const feed = (text: string, ts: number) => {
		for (let i = 0; i < text.length; i++) {
			const ch = text[i];
			const code = ch.charCodeAt(0);
			if (escState === EscState.Esc) {
				if (ch === "[") {
					escState = EscState.CSI;
					csiBuf = "";
				} else if (ch === "]") {
					escState = EscState.OSC;
				} else {
					escState = EscState.Normal;
				}
				continue;
			}
			if (escState === EscState.CSI) {
				if (code >= 0x40 && code <= 0x7e) {
					// Handle erase-in-line (K) so overwritten progress lines don't leave trailing chars.
					if (ch === "K") {
						const param = csiBuf === "" ? 0 : Number.parseInt(csiBuf, 10);
						if (param === 0) {
							buf.length = Math.min(buf.length, col);
						} else if (param === 2) {
							buf = [];
							col = 0;
						}
					} else if (ch === "G") {
						const n = Number.parseInt(csiBuf || "1", 10);
						col = Math.max(0, Number.isFinite(n) ? n - 1 : 0);
					}
					escState = EscState.Normal;
					csiBuf = "";
				} else {
					csiBuf += ch;
				}
				continue;
			}
			if (escState === EscState.OSC) {
				if (code === 0x07) {
					escState = EscState.Normal;
				} else if (code === ESC) {
					escState = EscState.OSCEsc;
				}
				continue;
			}
			if (escState === EscState.OSCEsc) {
				escState = EscState.Normal;
				continue;
			}

			if (code === ESC) {
				escState = EscState.Esc;
				continue;
			}
			if (ch === "\r") {
				col = 0;
				continue;
			}
			if (ch === "\b") {
				if (col > 0) col--;
				continue;
			}
			if (ch === "\n") {
				if (ts > 0) lineTs = ts;
				emitLine();
				continue;
			}
			if (ts > 0) lineTs = ts;
			while (buf.length < col) buf.push(" ");
			buf[col] = ch;
			col++;
		}
	};

	for (let i = 0; i < chunks.length; i++) {
		const c = chunks[i];
		const text = decoder.decode(c.data, { stream: i < chunks.length - 1 });
		if (text) feed(text, c.receivedAt);
	}
	const rest = decoder.decode();
	if (rest) feed(rest, 0);
	if (buf.length > 0) {
		emitLine();
	}
}
