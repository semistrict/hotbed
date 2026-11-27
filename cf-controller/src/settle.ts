// A port of internal/settle (Go): what a program wrote to a terminal, turned
// into the lines a reader would see. Carriage-return redraws (progress bars,
// spinners) and cursor-up redraws (multi-line progress) collapse to their
// final state, erases erase, and colors and other escape sequences go. A
// plain log, one that never redraws, passes through line for line. The
// worker settles live output with the Go package and the controller settles
// stored output with this one, so the two must agree byte for byte.
//
// It is a small terminal emulator over a live window of the newest rows: a
// row that scrolls out of the window can no longer be redrawn, so it is
// settled and emitted. Nothing wraps; a row holds at most MAX_COLS code
// points, past which what is written to it is dropped, so hostile output
// cannot grow one row (and the isolate's memory) without end.

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

/**
 * How many of the newest rows a program can still redraw by moving the
 * cursor up: more than any multi-line progress display.
 */
export const WINDOW = 64;
/**
 * The height of the terminal a command runs in (guestd's): absolute
 * positions (CSI H) count from the top of its last SCREEN_ROWS rows.
 */
export const SCREEN_ROWS = 50;
/**
 * Bounds a row: a cursor moved past it, or text written past it, goes
 * nowhere.
 */
export const MAX_COLS = 1 << 16;

const INT64_MAX = (1n << 63n) - 1n;

const enum EscState {
	Ground,
	Escape, // after ESC
	CSI, // after ESC [
	OSC, // after ESC ]: to BEL or ESC \
	OSCEsc, // ESC inside an OSC, perhaps its ST
	Charset, // after ESC ( and the like: one more character
}

type Row = string[]; // one code point per column

function rowLine(r: Row): string {
	let end = r.length;
	while (end > 0 && r[end - 1] === " ") end--;
	return r.slice(0, end).join("");
}

/** A terminal emulator that emits each row once it settles. */
class Screen {
	private rows: Row[] = [[]];
	private rowTs: number[] = [0]; // receive time (unix ms) of each row, 0 when unknown
	private row = 0; // the cursor's row, an index into rows
	private col = 0;
	private curTs = 0; // the receive time of the write executing

	// An escape sequence in progress across writes.
	private state = EscState.Ground;
	private params = "";

	// The cursor as saved (ESC 7, CSI s), its row an index into rows kept
	// current as rows settle; savedRow < 0 once that row has settled.
	private saved = false;
	private savedRow = 0;
	private savedCol = 0;

	constructor(private readonly emit: (line: string, ts: number) => void) {}

	/** Feeds text the program wrote, received at unix-ms ts (0 when unknown). */
	write(text: string, ts = 0): void {
		const prev = this.curTs;
		this.curTs = ts;
		for (const ch of text) this.char(ch);
		this.curTs = prev;
	}

	private char(ch: string): void {
		switch (this.state) {
			case EscState.Escape:
				this.escapeChar(ch);
				return;
			case EscState.CSI:
				this.csiChar(ch);
				return;
			case EscState.OSC:
				if (ch === "\x07") this.state = EscState.Ground;
				else if (ch === "\x1b") this.state = EscState.OSCEsc;
				return;
			case EscState.OSCEsc:
				// ESC \ ends it; any other ESC starts a new sequence.
				if (ch === "\\") {
					this.state = EscState.Ground;
				} else {
					this.state = EscState.Escape;
					this.escapeChar(ch);
				}
				return;
			case EscState.Charset:
				this.state = EscState.Ground;
				return;
		}
		const code = ch.codePointAt(0)!;
		if (ch === "\x1b") {
			this.state = EscState.Escape;
		} else if (ch === "\n") {
			// A newline starts the next line at its start, as a terminal turns
			// it into \r\n (onlcr) and as a log written to a file means.
			this.lineFeed();
			this.col = 0;
		} else if (ch === "\r") {
			this.col = 0;
		} else if (ch === "\b") {
			if (this.col > 0) this.col--;
		} else if (ch === "\t") {
			this.col = Math.min((Math.floor(this.col / 8) + 1) * 8, MAX_COLS);
		} else if (code < 0x20 || code === 0x7f) {
			// other controls (BEL, SI, SO, NUL) print nothing
		} else {
			this.put(ch);
		}
	}

	private escapeChar(ch: string): void {
		this.state = EscState.Ground;
		switch (ch) {
			case "[":
				this.state = EscState.CSI;
				this.params = "";
				return;
			case "]":
				this.state = EscState.OSC;
				return;
			case "(":
			case ")":
			case "*":
			case "+":
			case "#":
			case "%":
				this.state = EscState.Charset;
				return;
			case "7": // save the cursor
				this.save();
				return;
			case "8": // restore it
				this.restore();
				return;
			case "M": // reverse index: up a row
				this.up(1);
				return;
			case "E": // next line
				this.lineFeed();
				this.col = 0;
				return;
			case "D": // index: down a row
				this.lineFeed();
				return;
		}
		// ESC =, and the rest change nothing seen here
	}

	/** Parameter i of the CSI in progress, as Go's strconv.Atoi reads it. */
	private param(i: number, def: number): number {
		const p = this.params.split(";")[i];
		if (p === undefined || !/^[+-]?\d+$/.test(p)) return def;
		const v = BigInt(p);
		if (v < 0n || v > INT64_MAX) return def;
		if (v === 0n && def > 0) return def;
		return v > BigInt(MAX_COLS) ? MAX_COLS : Number(v);
	}

	private csiChar(ch: string): void {
		const code = ch.codePointAt(0)!;
		if (code >= 0x20 && code <= 0x3f) {
			// parameters and intermediates
			this.params += ch;
			return;
		}
		this.state = EscState.Ground;
		const lead = this.params[0];
		if (lead === "?" || lead === ">" || lead === "=") {
			return; // private modes: cursor visibility, alternate screens and the like
		}
		const n = (i: number, def: number) => this.param(i, def);
		switch (ch) {
			case "A": // cursor up
				this.up(n(0, 1));
				break;
			case "B":
			case "e": // cursor down
				this.down(n(0, 1));
				break;
			case "C":
			case "a": // cursor forward
				this.col = Math.min(this.col + n(0, 1), MAX_COLS);
				break;
			case "D": // cursor back
				this.col = Math.max(0, this.col - n(0, 1));
				break;
			case "E": // next line
				this.down(n(0, 1));
				this.col = 0;
				break;
			case "F": // previous line
				this.up(n(0, 1));
				this.col = 0;
				break;
			case "G":
			case "`": // column
				this.col = n(0, 1) - 1;
				break;
			case "H":
			case "f": {
				// position on the screen: its last SCREEN_ROWS rows
				const top = Math.max(0, this.rows.length - SCREEN_ROWS);
				this.row = top + Math.min(n(0, 1), SCREEN_ROWS) - 1;
				while (this.row >= this.rows.length) {
					this.rows.push([]);
					this.rowTs.push(0);
				}
				this.col = n(1, 1) - 1;
				break;
			}
			case "s":
				this.save();
				break;
			case "u":
				this.restore();
				break;
			case "K": // erase in line
				this.eraseLine(n(0, 0));
				break;
			case "J": // erase in display
				this.eraseDisplay(n(0, 0));
				break;
			case "P": {
				// delete characters
				const r = this.rows[this.row];
				if (this.col < r.length) {
					r.splice(this.col, Math.min(n(0, 1), r.length - this.col));
					this.touchRow(this.row);
				}
				break;
			}
			case "X": {
				// erase characters
				const r = this.rows[this.row];
				const end = Math.min(this.col + n(0, 1), r.length);
				for (let i = this.col; i < end; i++) r[i] = " ";
				this.touchRow(this.row);
				break;
			}
		}
		// m (colors), h and l (modes), r (scroll region), and the rest change
		// nothing seen here.
	}

	private touchRow(row: number): void {
		if (this.curTs > 0 && row >= 0 && row < this.rowTs.length) {
			this.rowTs[row] = this.curTs;
		}
	}

	private save(): void {
		this.saved = true;
		this.savedRow = this.row;
		this.savedCol = this.col;
	}

	private restore(): void {
		if (this.saved && this.savedRow >= 0) {
			this.row = this.savedRow;
			this.col = this.savedCol;
		}
	}

	private put(ch: string): void {
		if (this.col >= MAX_COLS) return;
		const r = this.rows[this.row];
		while (r.length < this.col) r.push(" ");
		r[this.col] = ch;
		this.touchRow(this.row);
		this.col++;
	}

	/**
	 * Moves the cursor down a row, adding one at the bottom, where the oldest
	 * row scrolls out of the window and settles.
	 */
	private lineFeed(): void {
		if (this.curTs > 0 && this.rowTs[this.row] === 0) {
			this.rowTs[this.row] = this.curTs;
		}
		this.row++;
		if (this.row === this.rows.length) {
			this.rows.push([]);
			this.rowTs.push(0);
			if (this.rows.length > WINDOW) {
				this.emit(rowLine(this.rows.shift()!), this.rowTs.shift()!);
				this.row--;
				this.savedRow--;
			}
		}
	}

	private up(k: number): void {
		this.row = Math.max(0, this.row - k);
	}

	private down(k: number): void {
		this.row = Math.min(this.rows.length - 1, this.row + k);
	}

	private eraseLine(mode: number): void {
		const r = this.rows[this.row];
		if (mode === 0) {
			if (this.col < r.length) r.length = this.col;
		} else if (mode === 1) {
			for (let i = 0; i <= this.col && i < r.length; i++) r[i] = " ";
		} else {
			this.rows[this.row] = []; // the cursor stays where it is
		}
		this.touchRow(this.row);
	}

	private eraseDisplay(mode: number): void {
		if (mode === 0) {
			this.eraseLine(0);
			for (let i = this.row + 1; i < this.rows.length; i++) {
				this.rows[i] = [];
				this.rowTs[i] = 0;
			}
		} else if (mode === 1) {
			for (let i = 0; i < this.row; i++) {
				this.rows[i] = [];
				this.rowTs[i] = 0;
			}
			this.eraseLine(1);
		} else {
			// A cleared screen keeps what scrolled before it: drop the window's
			// rows, as a reader of the log would not see them again.
			for (let i = 0; i < this.rows.length; i++) {
				this.rows[i] = [];
				this.rowTs[i] = 0;
			}
		}
	}

	private lastLiveRow(): number {
		let last = this.rows.length - 1;
		while (last > this.row && this.rows[last].length === 0) last--;
		if (last === this.row && this.rows[last].length === 0) last--;
		return last;
	}

	/** Emits the live window's rows, as the output ended: nothing can redraw them now. */
	flush(): void {
		const last = this.lastLiveRow();
		for (let i = 0; i <= last; i++) this.emit(rowLine(this.rows[i]), this.rowTs[i]);
		this.rows = [[]];
		this.rowTs = [0];
		this.row = 0;
		this.col = 0;
	}
}

/**
 * The settled lines of ordered output chunks, each with the receive time of
 * the chunk that last wrote it, keeping at most the last `tail` lines and
 * `maxBytes` bytes of them.
 */
export function extractLogLines(
	chunks: TimedOutputChunk[],
	tail: number,
	maxBytes: number,
	alreadyCut = false,
): ExtractedLogLines {
	const rawLines: string[] = [];
	const rawTimestamps: number[] = [];
	const screen = new Screen((line, ts) => {
		rawLines.push(line);
		rawTimestamps.push(ts);
	});
	// Streaming throughout: a rune split across chunks joins up, and one
	// left incomplete at the end is dropped, as Go's Screen drops it.
	const decoder = new TextDecoder("utf-8");
	for (const c of chunks) {
		screen.write(decoder.decode(c.data, { stream: true }), c.receivedAt);
	}
	screen.flush();

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
